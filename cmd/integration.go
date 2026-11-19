/*
 * Copyright 2026 Conductor Authors.
 * <p>
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 * <p>
 * http://www.apache.org/licenses/LICENSE-2.0
 * <p>
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package cmd

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/conductor-oss/conductor-cli/internal"
	"github.com/conductor-oss/conductor-cli/internal/transport"
	"github.com/spf13/cobra"
)

type connectionSummary struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Category    string `json:"category"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description,omitempty"`
}
type catalogOperation struct {
	API         string `json:"api"`
	Description string `json:"description"`
	Input       *struct {
		Data json.RawMessage `json:"data"`
	} `json:"inputSchema,omitempty"`
	Output *struct {
		Data json.RawMessage `json:"data"`
	} `json:"outputSchema,omitempty"`
}
type providerDefinition struct {
	connectionSummary
	Configuration json.RawMessage    `json:"configuration"`
	APIs          []catalogOperation `json:"apis"`
}

func integrationPath(name string) string { return "/integrations/provider/" + neturl.PathEscape(name) }

func init() { rootCmd.AddCommand(newIntegrationCommand()) }

func newIntegrationCommand() *cobra.Command {
	group := &cobra.Command{Use: "integration", Short: "Discover and manage Orkes Conductor integrations", GroupID: "conductor"}
	add := func(use, help string, args cobra.PositionalArgs, run func(*cobra.Command, []string) (any, error)) *cobra.Command {
		c := &cobra.Command{Use: use, Short: help, Args: args, SilenceUsage: true, RunE: func(c *cobra.Command, a []string) error {
			if !isEnterpriseServer() {
				return errors.New("Connected Apps are not supported in OSS Conductor. If this profile points to Orkes Conductor, use --server-type Enterprise or set CONDUCTOR_SERVER_TYPE=Enterprise.")
			}
			v, err := run(c, a)
			if err != nil {
				return err
			}
			if v == nil {
				return nil
			}
			if rows, ok := v.([]connectionSummary); ok {
				return writeConnectionSummaries(c, rows)
			}
			enc := json.NewEncoder(c.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}}
		group.AddCommand(c)
		return c
	}
	types := add("types", "List enabled integration types", cobra.NoArgs, func(c *cobra.Command, _ []string) (any, error) {
		var rows []connectionSummary
		err := internal.Transport().DoJSON(c.Context(), "GET", "/integrations/def", nil, &rows)
		cat, _ := c.Flags().GetString("category")
		out := []connectionSummary{}
		for _, r := range rows {
			if r.Enabled && (cat == "" || r.Category == cat) {
				out = append(out, r)
			}
		}
		return out, err
	})
	AddOutputFlags(types)
	types.Flags().String("category", "", "Filter by category, e.g. MCP")
	list := add("list", "List visible saved connections", cobra.NoArgs, func(c *cobra.Command, _ []string) (any, error) {
		var rows []connectionSummary
		err := internal.Transport().DoJSON(c.Context(), "GET", "/integrations/provider?activeOnly=false", nil, &rows)
		t, _ := c.Flags().GetString("type")
		out := []connectionSummary{}
		for _, r := range rows {
			if t == "" || r.Type == t {
				out = append(out, r)
			}
		}
		return out, err
	})
	AddOutputFlags(list)
	list.Flags().String("type", "", "Filter by provider type")
	add("type-config <type>", "Show the provider's configuration fields", cobra.ExactArgs(1), func(c *cobra.Command, a []string) (any, error) {
		var d providerDefinition
		err := internal.Transport().DoJSON(c.Context(), "GET", "/integrations/def/"+neturl.PathEscape(a[0]), nil, &d)
		return d.Configuration, err
	})
	add("get <name>", "Show saved connection metadata and configured field names", cobra.ExactArgs(1), func(c *cobra.Command, a []string) (any, error) {
		var r struct {
			connectionSummary
			Configuration map[string]json.RawMessage
		}
		err := internal.Transport().DoJSON(c.Context(), "GET", integrationPath(a[0]), nil, &r)
		keys := []string{}
		for k, v := range r.Configuration {
			if string(v) != "null" && string(v) != "\"\"" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return struct {
			connectionSummary
			Configured []string `json:"configuredFields"`
		}{r.connectionSummary, keys}, err
	})
	ops := add("operations [connection]", "List operations or inspect an operation schema", cobra.MaximumNArgs(1), func(c *cobra.Command, a []string) (any, error) {
		t, _ := c.Flags().GetString("type")
		if (t == "") == (len(a) == 0) {
			return nil, fmt.Errorf("specify one connection or --type")
		}
		var rows []catalogOperation
		var err error
		if t != "" {
			var d providerDefinition
			err = internal.Transport().DoJSON(c.Context(), "GET", "/integrations/def/"+neturl.PathEscape(t), nil, &d)
			rows = d.APIs
		} else {
			err = internal.Transport().DoJSON(c.Context(), "GET", "/integrations/"+neturl.PathEscape(a[0])+"/def/apis", nil, &rows)
		}
		if err != nil {
			return nil, err
		}
		name, _ := c.Flags().GetString("name")
		for i, r := range rows {
			if name != "" && r.API == name {
				return r, nil
			}
			rows[i].Input = nil
			rows[i].Output = nil
		}
		if name != "" {
			return nil, fmt.Errorf("unknown operation %q", name)
		}
		return rows, nil
	})
	ops.Long = "List operations for a provider type (--type) or for a saved connection (positional argument). Supply exactly one. Add --name with an exact operation name to show its input and output schemas."
	ops.Example = `  conductor integration operations --type stripe
  conductor integration operations payments-test
  conductor integration operations payments-test --name "List Charges"`
	ops.Flags().String("type", "", "Discover by provider type")
	ops.Flags().String("name", "", "Inspect an exact operation name")
	create := add("create [json_payload]", "Create a saved connection from JSON, --file or stdin", cobra.MaximumNArgs(1), createConnection)
	create.Long = `Create a saved connection from a JSON argument, --file, or stdin. Supply the connection name and provider type. If category is omitted or empty, the CLI uses the registered provider category. Run conductor integration type-config <type> for provider-specific configuration fields.

Use $.env.VAR as the entire string value of a configuration field to read it from the CLI environment. Other fields, including description, remain literal.`
	create.Example = `  conductor integration create '{"name":"payments-test","type":"stripe","category":"MCP","enabled":true,"description":"Test payments","configuration":{"api_key":"$.env.STRIPE_TEST_KEY"}}'
  conductor integration create --file connection.json`
	create.Flags().String("file", "", "Read a JSON object from a file")
	update := add("update <name> [json_payload]", "Update a saved connection from JSON, --file or stdin", cobra.RangeArgs(1, 2), updateConnection)
	update.Long = `Update a saved connection from a JSON patch supplied as an argument, via --file, or on stdin. Only description, enabled, and configuration can be updated. Each supplied configuration field replaces its saved value. Omitted fields keep their values.

Use $.env.VAR as the entire string value of a configuration field to read it from the CLI environment. The description remains literal.`
	update.Example = `  conductor integration update payments-test '{"configuration":{"api_key":"$.env.STRIPE_TEST_KEY"}}'
  conductor integration update payments-test --file patch.json`
	update.Flags().String("file", "", "Read a JSON patch object from a file")
	add("delete <name>", "Delete a saved connection", cobra.ExactArgs(1), func(c *cobra.Command, a []string) (any, error) {
		if !confirmDeletion("integration", a[0]) {
			_, err := fmt.Fprintln(c.OutOrStdout(), "Deletion cancelled")
			return nil, err
		}
		if err := internal.Transport().DoJSON(c.Context(), "DELETE", integrationPath(a[0]), nil, nil); err != nil {
			return nil, err
		}
		_, err := fmt.Fprintf(c.OutOrStdout(), "Deleted connection %s\n", a[0])
		return nil, err
	})
	return group
}

func writeConnectionSummaries(c *cobra.Command, rows []connectionSummary) error {
	format, err := GetOutputFormat(c)
	if err != nil {
		return err
	}
	out := c.OutOrStdout()
	if format == OutputFormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	records := [][]string{{"NAME", "TYPE", "CATEGORY", "ENABLED", "DESCRIPTION"}}
	for _, row := range rows {
		records = append(records, []string{row.Name, row.Type, row.Category, strconv.FormatBool(row.Enabled), row.Description})
	}
	if format == OutputFormatCSV {
		return csv.NewWriter(out).WriteAll(records)
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	for _, record := range records {
		if _, err := fmt.Fprintln(w, strings.Join(record, "\t")); err != nil {
			return err
		}
	}
	return w.Flush()
}

func connectionInput(c *cobra.Command, a []string, body *map[string]json.RawMessage) error {
	var input io.Reader = c.InOrStdin()
	file, _ := c.Flags().GetString("file")
	if len(a) > 0 && file != "" {
		return fmt.Errorf("use either JSON or --file")
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	} else if len(a) > 0 {
		input = strings.NewReader(a[0])
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, body) != nil || *body == nil {
		return fmt.Errorf("expected a JSON object")
	}
	raw, ok := (*body)["configuration"]
	if !ok {
		return nil
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return errors.New("configuration must be an object")
	}
	if _, err := resolveIntegrationEnv(fields, "configuration"); err != nil {
		return err
	}
	(*body)["configuration"], err = json.Marshal(fields)
	return err
}

var integrationEnvRef = regexp.MustCompile(`^\$\.env\.([A-Za-z_][A-Za-z0-9_]*)$`)

// Windows environment lookups are case-insensitive. Unix names are case-sensitive.
func reservedIntegrationEnv(name string) bool {
	if runtime.GOOS == "windows" {
		name = strings.ToUpper(name)
	}
	switch name {
	case "CONDUCTOR_KEY", "CONDUCTOR_AUTH_KEY", "CONDUCTOR_AUTH_SECRET", "CONDUCTOR_AUTH_TOKEN",
		"CONDUCTOR_ACCESS_KEY_ID", "CONDUCTOR_ACCESS_KEY_SECRET":
		return true
	}
	return false
}

// Resolve whole string values in the caller's configuration only, before merging an update.
func resolveIntegrationEnv(value any, path string) (any, error) {
	switch v := value.(type) {
	case string:
		if match := integrationEnvRef.FindStringSubmatch(v); match != nil {
			name := match[1]
			if reservedIntegrationEnv(name) {
				return nil, fmt.Errorf("%s: %s is reserved for Conductor authentication; use a provider credential variable", path, name)
			}
			resolved := os.Getenv(name)
			if resolved == "" {
				return nil, fmt.Errorf("%s: environment variable %s is unset or empty", path, name)
			}
			return resolved, nil
		}
	case map[string]any:
		for k, child := range v {
			resolved, err := resolveIntegrationEnv(child, path+"."+k)
			if err != nil {
				return nil, err
			}
			v[k] = resolved
		}
	case []any:
		for i, child := range v {
			resolved, err := resolveIntegrationEnv(child, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			v[i] = resolved
		}
	}
	return value, nil
}

func createConnection(c *cobra.Command, a []string) (any, error) {
	var body map[string]json.RawMessage
	if err := connectionInput(c, a, &body); err != nil {
		return nil, err
	}
	var name, t string
	if json.Unmarshal(body["name"], &name) != nil || json.Unmarshal(body["type"], &t) != nil ||
		strings.TrimSpace(name) == "" || strings.TrimSpace(t) == "" {
		return nil, fmt.Errorf("name and type required")
	}
	client := internal.Transport()
	path := integrationPath(name)
	err := client.DoJSON(c.Context(), "GET", path, nil, nil)
	if err == nil {
		return nil, fmt.Errorf("integration already exists")
	}
	var apiErr *transport.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		return nil, integrationRequestError("creation", t, err)
	}
	var category string
	if raw := body["category"]; raw != nil && json.Unmarshal(raw, &category) != nil {
		return nil, errors.New("category must be a string")
	}
	if strings.TrimSpace(category) == "" {
		var definition connectionSummary
		if err := client.DoJSON(c.Context(), "GET", "/integrations/def/"+neturl.PathEscape(t), nil, &definition); err != nil {
			return nil, integrationRequestError("definition lookup", t, err)
		}
		if strings.TrimSpace(definition.Category) == "" {
			return nil, fmt.Errorf("registered definition for %q has no category", t)
		}
		body["category"], err = json.Marshal(definition.Category)
		if err != nil {
			return nil, err
		}
	}
	if err := client.DoJSON(c.Context(), "POST", path, body, nil); err != nil {
		return nil, integrationRequestError("creation", t, err)
	}
	return map[string]string{"name": name, "type": t}, nil
}

// The provider endpoint replaces these fields on POST. Keep its GET response in
// memory: configuration values can contain resolved credentials.
type savedConnection struct {
	Type          string                     `json:"type"`
	Category      string                     `json:"category"`
	Description   *string                    `json:"description"`
	Enabled       bool                       `json:"enabled"`
	Configuration map[string]json.RawMessage `json:"configuration"`
}

func updateConnection(c *cobra.Command, args []string) (any, error) {
	var patch map[string]json.RawMessage
	if err := connectionInput(c, args[1:], &patch); err != nil {
		return nil, err
	}
	name := args[0]
	path := integrationPath(name)
	client := internal.Transport()
	var saved savedConnection
	if err := client.DoJSON(c.Context(), "GET", path, nil, &saved); err != nil {
		return nil, integrationRequestError("update", "", err)
	}
	if saved.Type == "" || saved.Category == "" {
		return nil, fmt.Errorf("incomplete saved integration")
	}
	if err := saved.applyPatch(patch); err != nil {
		return nil, err
	}
	if err := client.DoJSON(c.Context(), "POST", path, saved, nil); err != nil {
		return nil, integrationRequestError("update", saved.Type, err)
	}
	return map[string]string{"name": name, "type": saved.Type}, nil
}

// A server error may echo request or stored credentials. Only expose its status.
func integrationRequestError(action, providerType string, err error) error {
	var apiErr *transport.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("integration %s request failed", action)
	}
	if apiErr.Status == 400 && providerType != "" {
		return fmt.Errorf("integration %s failed (HTTP 400). Check the accepted fields with conductor integration type-config %s", action, providerType)
	}
	return fmt.Errorf("integration %s failed (HTTP %d)", action, apiErr.Status)
}

func (saved *savedConnection) applyPatch(patch map[string]json.RawMessage) error {
	if len(patch) == 0 {
		return fmt.Errorf("no integration changes supplied")
	}
	for field, value := range patch {
		switch field {
		case "description":
			if string(value) == "null" || json.Unmarshal(value, &saved.Description) != nil {
				return fmt.Errorf("description must be a string")
			}
		case "enabled":
			if string(value) == "null" || json.Unmarshal(value, &saved.Enabled) != nil {
				return fmt.Errorf("enabled must be a boolean")
			}
		case "configuration":
			var fields map[string]json.RawMessage
			if json.Unmarshal(value, &fields) != nil || len(fields) == 0 {
				return fmt.Errorf("configuration must contain fields to update")
			}
			if saved.Configuration == nil {
				saved.Configuration = make(map[string]json.RawMessage)
			}
			for key, replacement := range fields {
				saved.Configuration[key] = replacement
			}
		default:
			return fmt.Errorf("only description, enabled and configuration can be updated")
		}
	}
	return nil
}
