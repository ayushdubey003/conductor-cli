package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/conductor-oss/conductor-cli/internal"
	"github.com/conductor-oss/conductor-cli/internal/transport"
)

func TestSavedConnectionPatchPreservesCredentials(t *testing.T) {
	saved := savedConnection{Type: "aws-lambda", Category: "MCP", Enabled: true,
		Configuration: map[string]json.RawMessage{
			"api_key":      json.RawMessage(`"original"`),
			"sessionToken": json.RawMessage(`"unrelated"`),
		},
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"configuration":{"api_key":"replacement"},"description":"test"}`), &patch); err != nil {
		t.Fatal(err)
	}
	if err := saved.applyPatch(patch); err != nil {
		t.Fatal(err)
	}
	if string(saved.Configuration["api_key"]) != `"replacement"` ||
		string(saved.Configuration["sessionToken"]) != `"unrelated"` ||
		saved.Description == nil || *saved.Description != "test" || !saved.Enabled {
		t.Fatal("update did not preserve the intended fields")
	}
}

func TestSavedConnectionRejectsInvalidPatch(t *testing.T) {
	for _, input := range []string{`{}`, `{"category":"CLOUD"}`, `{"name":"renamed"}`,
		`{"type":"different"}`, `{"enabled":null}`, `{"configuration":[]}`, `{"description":false}`} {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input), &patch); err != nil {
			t.Fatal(err)
		}
		if err := new(savedConnection).applyPatch(patch); err == nil {
			t.Errorf("accepted invalid patch %s", input)
		}
	}
}

func TestIntegrationCommandsRequireEnterprise(t *testing.T) {
	previous := serverType
	t.Cleanup(func() { serverType = previous })
	serverType = "OSS"
	group, _, err := rootCmd.Find([]string{"integration"})
	if err != nil || group.Name() != "integration" {
		t.Fatal("integration command missing")
	}
	for _, command := range group.Commands() {
		t.Run(command.Name(), func(t *testing.T) {
			if err := command.RunE(command, nil); err == nil || err.Error() != "Connected Apps are not supported in OSS Conductor. If this profile points to Orkes Conductor, use --server-type Enterprise or set CONDUCTOR_SERVER_TYPE=Enterprise." {
				t.Fatalf("expected Enterprise guard, got %v", err)
			}
		})
	}
}

func TestIntegrationEnvReferences(t *testing.T) {
	const secret = "synthetic-\"key\"\n$.env.KEEP_LITERAL"
	const missing = "CONDUCTOR_CLI_TEST_UNSET_130"
	t.Setenv(missing, "")
	if err := os.Unsetenv(missing); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_INTEGRATED_APPS_TEST_KEY", secret)
	t.Setenv("TEST_CONN_EMPTY", "")
	previousType, previousTransport := serverType, internal.Transport()
	serverType = "Enterprise"
	t.Cleanup(func() { serverType = previousType; internal.SetTransport(previousTransport) })

	for _, operation := range []string{"create", "update"} {
		for _, scenario := range []string{"success", "missing", "empty", "server rejection"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				ref := "CONDUCTOR_INTEGRATED_APPS_TEST_KEY"
				switch scenario {
				case "missing":
					ref = missing
				case "empty":
					ref = "TEST_CONN_EMPTY"
				}
				fields := fmt.Sprintf(`"configuration":{"key":"$.env.CONDUCTOR_INTEGRATED_APPS_TEST_KEY","nested":["$.env.%s",{"literal":"prefix $.env.CONDUCTOR_INTEGRATED_APPS_TEST_KEY","number":9007199254740993}]}`, ref)
				body := `{"description":"$.env.CONDUCTOR_INTEGRATED_APPS_TEST_KEY",` + fields + `}`
				if operation == "create" {
					body = `{"name":"example","type":"test","category":"MCP",` + body[1:]
				}

				var calls []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.Method)
					if r.URL.Path != "/integrations/provider/example" {
						t.Error("unexpected integration path")
					}
					if r.Method == http.MethodGet {
						if operation == "create" {
							w.WriteHeader(http.StatusNotFound)
						} else {
							fmt.Fprint(w, `{"type":"test","category":"MCP","configuration":{"existing":"$.env.TEST_CONN_EMPTY"}}`)
						}
						return
					}
					var sent struct {
						Description   string
						Configuration struct {
							Key, Existing string
							Nested        []json.RawMessage
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Error("invalid JSON sent")
						return
					}
					cfg := sent.Configuration
					if sent.Description != "$.env.CONDUCTOR_INTEGRATED_APPS_TEST_KEY" || cfg.Key != secret || len(cfg.Nested) != 2 {
						t.Error("unexpected resolved configuration")
						return
					}
					var first string
					var second struct {
						Literal string
						Number  int64
					}
					if json.Unmarshal(cfg.Nested[0], &first) != nil || first != secret ||
						json.Unmarshal(cfg.Nested[1], &second) != nil || second.Literal != "prefix $.env.CONDUCTOR_INTEGRATED_APPS_TEST_KEY" || second.Number != 9007199254740993 {
						t.Error("nested reference, embedded literal or numeric precision changed")
					}
					if operation == "update" && cfg.Existing != "$.env.TEST_CONN_EMPTY" {
						t.Error("stored configuration was reinterpreted")
					}
					if scenario == "server rejection" {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"message": secret})
					}
				}))
				defer server.Close()
				internal.SetTransport(transport.Config{BaseURL: server.URL})
				cmd, _, err := rootCmd.Find([]string{"integration", operation})
				if err != nil {
					t.Fatal(err)
				}
				cmd.SetContext(context.Background())
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				t.Cleanup(func() { cmd.SetOut(nil); cmd.SetErr(nil); cmd.SetIn(nil); _ = cmd.Flags().Set("file", "") })
				var args []string
				if operation == "update" {
					args = []string{"example"}
					cmd.SetIn(strings.NewReader(body))
				} else if scenario == "success" {
					file := filepath.Join(t.TempDir(), "create.json")
					if err := os.WriteFile(file, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					if err := cmd.Flags().Set("file", file); err != nil {
						t.Fatal(err)
					}
				} else {
					args = []string{body}
				}
				err = cmd.RunE(cmd, args)
				if strings.Contains(output.String()+fmt.Sprint(err), "synthetic-") {
					t.Fatal("credential exposed in output or error")
				}
				if scenario == "server rejection" && (err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "conductor integration type-config test")) {
					t.Fatal("expected safe type-config hint")
				}
				switch scenario {
				case "missing", "empty":
					if err == nil || !strings.Contains(err.Error(), ref) || !strings.Contains(err.Error(), "configuration.nested[0]") || len(calls) != 0 {
						t.Fatal("expected field and variable error before any integration request")
					}
				default:
					if strings.Join(calls, ",") != "GET,POST" || (err == nil) != (scenario == "success") {
						t.Fatal("unexpected requests or command outcome")
					}
				}
			})
		}
	}
}

func integrationTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	previousType, previousTransport := serverType, internal.Transport()
	server := httptest.NewServer(handler)
	serverType = "Enterprise"
	internal.SetTransport(transport.Config{BaseURL: server.URL})
	t.Cleanup(func() { server.Close(); serverType = previousType; internal.SetTransport(previousTransport) })
}

func runIntegration(input string, args ...string) (string, error) {
	c := newIntegrationCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetIn(strings.NewReader(input))
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func TestIntegrationDiscoveryFormats(t *testing.T) {
	const rows = `[{"name":"Alpha","type":"app","category":"MCP","enabled":true,"description":"comma, here"},{"name":"Beta","type":"other","category":"EMAIL","enabled":true},{"name":"Disabled","type":"app","category":"MCP","enabled":false}]`
	for _, command := range []string{"types", "list"} {
		t.Run(command, func(t *testing.T) {
			wantURL := "/integrations/def"
			filter := []string{"--category", "MCP"}
			wantNames := []string{"Alpha"}
			if command == "list" {
				wantURL = "/integrations/provider?activeOnly=false"
				filter = []string{"--type", "app"}
				wantNames = append(wantNames, "Disabled")
			}
			integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.RequestURI() != wantURL {
					t.Errorf("unexpected %s %s", r.Method, r.URL)
				}
				fmt.Fprint(w, rows)
			})
			for _, format := range []string{"table", "json", "csv"} {
				args := append([]string{command}, filter...)
				if format != "table" {
					args = append(args, "--"+format)
				}
				out, err := runIntegration("", args...)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				switch format {
				case "json":
					var got []connectionSummary
					if err := json.Unmarshal([]byte(out), &got); err != nil {
						t.Fatal(err)
					}
					for _, v := range got {
						names = append(names, v.Name)
					}
				case "csv":
					records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
					if err != nil || len(records) < 2 || records[0][0] != "NAME" || records[1][4] != "comma, here" {
						t.Fatalf("invalid CSV: %q", out)
					}
					for _, record := range records[1:] {
						names = append(names, record[0])
					}
				case "table":
					lines := strings.Split(strings.TrimSpace(out), "\n")
					if strings.Fields(lines[0])[0] != "NAME" {
						t.Fatalf("invalid table: %q", out)
					}
					for _, line := range lines[1:] {
						names = append(names, strings.Fields(line)[0])
					}
				}
				if !reflect.DeepEqual(names, wantNames) {
					t.Fatalf("%s got %v want %v", format, names, wantNames)
				}
			}
			if _, err := runIntegration("", command, "--json", "--csv"); err == nil {
				t.Fatal("accepted conflicting formats")
			}
		})
	}
}

func TestIntegrationOutputFlagScope(t *testing.T) {
	group := newIntegrationCommand()
	for _, cmd := range group.Commands() {
		want := cmd.Name() == "list" || cmd.Name() == "types"
		for _, flag := range []string{"json", "csv"} {
			if got := cmd.Flag(flag) != nil; got != want {
				t.Errorf("%s --%s available = %v, want %v", cmd.Name(), flag, got, want)
			}
		}
	}
}

func TestIntegrationGetAndOperationSchemas(t *testing.T) {
	const ops = `[{"api":"Invoke","inputSchema":{"data":{"type":"object"}},"outputSchema":{"data":{"type":"string"}}}]`
	calls := 0
	integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Error(r.Method)
		}
		switch r.URL.Path {
		case "/integrations/provider/sample":
			fmt.Fprint(w, `{"configuration":{"api_key":"private-marker","z":"present","empty":"","unset":null}}`)
		case "/integrations/def/test":
			fmt.Fprintf(w, `{"configuration":[{"fieldName":"key","dependsOn":[{"fieldName":"mode"}]}],"apis":%s}`, ops)
		case "/integrations/sample/def/apis":
			fmt.Fprint(w, `[{"api":"Saved-only"}]`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	})
	out, err := runIntegration("", "get", "sample")
	var metadata struct{ ConfiguredFields []string }
	if err != nil || json.Unmarshal([]byte(out), &metadata) != nil || !reflect.DeepEqual(metadata.ConfiguredFields, []string{"api_key", "z"}) || strings.Contains(out, "private-marker") {
		t.Fatal("unexpected saved connection output")
	}
	out, err = runIntegration("", "type-config", "test")
	var schema []struct{ DependsOn []struct{ FieldName string } }
	if err != nil || json.Unmarshal([]byte(out), &schema) != nil || len(schema) != 1 || len(schema[0].DependsOn) != 1 || schema[0].DependsOn[0].FieldName != "mode" {
		t.Fatal("lost nested schema")
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--type", "test"}, "Invoke"}, {[]string{"sample"}, "Saved-only"},
	} {
		out, err = runIntegration("", append([]string{"operations"}, test.args...)...)
		var summary []catalogOperation
		if err != nil || json.Unmarshal([]byte(out), &summary) != nil || len(summary) != 1 || summary[0].API != test.want || summary[0].Input != nil || summary[0].Output != nil {
			t.Fatal("incorrect operation list", out, err)
		}
	}
	out, err = runIntegration("", "operations", "--type", "test", "--name", "Invoke")
	var op struct {
		Input  struct{ Data struct{ Type string } } `json:"inputSchema"`
		Output struct{ Data struct{ Type string } } `json:"outputSchema"`
	}
	if err != nil || json.Unmarshal([]byte(out), &op) != nil || op.Input.Data.Type != "object" || op.Output.Data.Type != "string" || !strings.Contains(out, "\n  \"inputSchema\":") {
		t.Fatal("lost operation schema")
	}
	if _, err = runIntegration("", "operations", "sample", "--name", "Missing"); err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Fatal("missing operation accepted")
	}
	before := calls
	for _, args := range [][]string{{"operations"}, {"operations", "sample", "--type", "test"}} {
		if _, err := runIntegration("", args...); err == nil || calls != before {
			t.Fatal("expected local argument rejection")
		}
	}
}

func TestIntegrationDelete(t *testing.T) {
	deleted := 0
	integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && r.URL.Path == "/integrations/provider/missing" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "DELETE" || r.URL.Path != "/integrations/provider/sample" {
			t.Error("unexpected delete target")
		}
		deleted++
	})
	oldYes := yes
	t.Cleanup(func() { yes = oldYes })
	for _, reply := range []string{"n\n", "\n"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, reply)
		w.Close()
		stdin := os.Stdin
		os.Stdin = r
		yes = false
		out, err := runIntegration("", "delete", "sample")
		os.Stdin = stdin
		r.Close()
		if err != nil || !strings.Contains(out, "Deletion cancelled") || deleted != 0 {
			t.Fatal("decline did not cancel")
		}
	}
	yes = true
	if out, err := runIntegration("", "delete", "missing"); err == nil || strings.Contains(out, "Deleted connection") {
		t.Fatal("failed delete must not print a success message")
	}
	if out, err := runIntegration("", "delete", "sample"); err != nil || out != "Deleted connection sample\n" || deleted != 1 {
		t.Fatal("expected one confirmed delete")
	}
}

func TestIntegrationCreateCategoryAndRotation(t *testing.T) {
	for _, tc := range []struct {
		label, category, definition, want string
		status                            int
	}{
		{"omitted", "", "EMAIL", "EMAIL", 200}, {"empty", `"category":"",`, "CLOUD", "CLOUD", 200}, {"explicit", `"category":"MCP",`, "", "MCP", 200},
		{"unknown", "", "", "", 404}, {"no category", "", "", "", 200},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var saved json.RawMessage
			lookups := 0
			integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/integrations/def/test" && r.Method == "GET":
					lookups++
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"category":%q,"message":"private-marker"}`, tc.definition)
				case r.URL.Path != "/integrations/provider/sample":
					t.Error(r.URL.Path)
				case r.Method == "POST":
					var err error
					saved, err = io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
				case r.Method == "GET":
					if saved == nil {
						w.WriteHeader(404)
					} else {
						w.Write(saved)
					}
				default:
					t.Error(r.Method)
				}
			})
			_, err := runIntegration("", "create", `{"name":"sample","type":"test",`+tc.category+`"configuration":{"key":"first"}}`)
			if (tc.label == "explicit" && lookups != 0) || (tc.label != "explicit" && lookups != 1) {
				t.Fatal("incorrect catalog lookup count", lookups)
			}
			if tc.want == "" {
				if err == nil || saved != nil || strings.Contains(err.Error(), "private-marker") {
					t.Fatal("failed lookup must prevent write")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runIntegration("", "update", "sample", `{"configuration":{"key":"second"}}`); err != nil {
				t.Fatal(err)
			}
			var got savedConnection
			if json.Unmarshal(saved, &got) != nil || got.Category != tc.want || string(got.Configuration["key"]) != `"second"` {
				t.Fatal("wrong category or credential after rotation")
			}
		})
	}
}

func TestIntegrationUpdateRejectsConflictingInput(t *testing.T) {
	integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input should be rejected before any request")
	})
	_, err := runIntegration("", "update", "sample", `{"enabled":true}`, "--file", "patch.json")
	if err == nil || !strings.Contains(err.Error(), "use either JSON or --file") {
		t.Fatalf("expected conflicting input error, got %v", err)
	}
}

func TestIntegrationLookupErrorHidesServerBody(t *testing.T) {
	integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("failed lookup must prevent a write")
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"private-marker"}`)
	})
	for _, args := range [][]string{{"create", `{"type":"test","name":"sample"}`}, {"update", "sample"}} {
		out, err := runIntegration(`{"enabled":true}`, args...)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(out+err.Error(), "private-marker") {
			t.Fatal("expected safe lookup error")
		}
	}
}

func TestIntegrationRejectsReservedEnvReferences(t *testing.T) {
	integrationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected integration request: %s", r.Method)
	})
	for _, name := range []string{"CONDUCTOR_KEY", "CONDUCTOR_AUTH_KEY", "CONDUCTOR_AUTH_SECRET", "CONDUCTOR_AUTH_TOKEN", "CONDUCTOR_ACCESS_KEY_ID", "CONDUCTOR_ACCESS_KEY_SECRET"} {
		t.Run(name, func(t *testing.T) {
			const value = "synthetic-reserved-marker"
			t.Setenv(name, value)
			config := fmt.Sprintf(`"configuration":{"nested":[{"key":"$.env.%s"}]}`, name)
			for _, args := range [][]string{{"create", `{"name":"sample","type":"test",` + config + `}`}, {"update", "sample"}} {
				out, err := runIntegration("{"+config+"}", args...)
				if err == nil || !strings.Contains(err.Error(), name+" is reserved") || strings.Contains(out+err.Error(), value) {
					t.Fatal("expected safe rejection for", args[0])
				}
			}
		})
	}
}
