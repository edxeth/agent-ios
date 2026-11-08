package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// schemaTypes returns the JSON types that a schema allows.
func schemaTypes(s *jsonschema.Schema) []string {
	if s.Type != "" {
		return []string{s.Type}
	}
	return s.Types
}

// resolve resolves a schema for validation.
func resolve(t *testing.T, s *jsonschema.Schema) *jsonschema.Resolved {
	t.Helper()
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Design 4.1: an MCP tool that a registry command takes over keeps its
// parameters. For each such tool of the W4 commands, the table holds the
// legacy input type of this file (the W0 handler) and calls that were
// valid for it. The schema that the server advertises for the registry
// tool keeps every legacy property with its JSON type, and accepts each
// call; the registry handler then runs it (the commands package tests the
// effect on the page).
func TestRegistryToolsKeepTheLegacySchema(t *testing.T) {
	cases := []struct {
		tool   string
		legacy func() (*jsonschema.Schema, error)
		calls  []map[string]any
	}{
		{"set_extra_headers", func() (*jsonschema.Schema, error) { return jsonschema.For[SetExtraHeadersInput](nil) }, []map[string]any{
			{"headers": map[string]any{"X-A": "1"}},
			{"headers": map[string]any{}},
		}},
		{"set_cookie", func() (*jsonschema.Schema, error) { return jsonschema.For[SetCookieInput](nil) }, []map[string]any{
			{"name": "sid", "value": "v", "expires": 1893456000},
			{"name": "sid", "value": "v", "expires": 0},
			{"name": "sid", "value": "v", "domain": "a.test", "path": "/", "expires": 1893456000.5, "secure": true, "http_only": true, "same_site": "Lax"},
		}},
	}
	// The test binary registers every tool (TestMain: IWDP_MCP_TOOLS=all),
	// so the server lists the registry tools of these names.
	mock := testutil.NewMockServer()
	defer mock.Close()
	cs := connectMCP(t, mock)
	advertised := map[string]*jsonschema.Schema{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		advertised[tool.Name] = &s
	}
	for _, c := range cases {
		legacy, err := c.legacy()
		if err != nil {
			t.Fatal(err)
		}
		adv := advertised[c.tool]
		if adv == nil {
			t.Fatalf("the server does not list %s", c.tool)
		}
		for name, lp := range legacy.Properties {
			ap := adv.Properties[name]
			if ap == nil {
				t.Errorf("%s: the advertised schema lacks the legacy parameter %q", c.tool, name)
				continue
			}
			for _, typ := range schemaTypes(lp) {
				// JSON Schema: every integer is a number.
				kept := slices.Contains(schemaTypes(ap), typ) || (typ == "integer" && slices.Contains(schemaTypes(ap), "number"))
				if !kept {
					t.Errorf("%s.%s: advertised types %v, want them to include the legacy type %q", c.tool, name, schemaTypes(ap), typ)
				}
			}
		}
		lr, ar := resolve(t, legacy), resolve(t, adv)
		for _, call := range c.calls {
			if err := lr.Validate(call); err != nil {
				t.Fatalf("%s %v is not a legacy call: %v", c.tool, call, err)
			}
			if err := ar.Validate(call); err != nil {
				t.Errorf("%s: the advertised schema refuses the legacy call %v: %v", c.tool, call, err)
			}
		}
	}
}
