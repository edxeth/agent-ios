package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP profiles (decision D2): "core" is the default; "all" selects every
// registry tool.
const (
	ProfileCore     = "core"
	ProfileDevtools = "devtools"
	ProfileLegacy   = "legacy"
	ProfileAll      = "all"
)

// SelectedProfile returns the MCP profile chosen by IWDP_MCP_TOOLS:
// "all" selects every tool; anything else selects the default "core" (D2).
func SelectedProfile(getenv func(string) string) string {
	if getenv("IWDP_MCP_TOOLS") == ProfileAll {
		return ProfileAll
	}
	return ProfileCore
}

// InProfile reports whether c is an MCP tool of the selected profile.
func InProfile(c *Command, profile string) bool {
	return c.MCPName != "" && (profile == ProfileAll || c.Profile == profile)
}

// InputSchema returns the MCP input schema of c: one property per argument
// (a variadic argument is an array of strings) and per flag.
func InputSchema(c *Command) map[string]any {
	props := map[string]any{}
	var required []string
	for _, a := range c.Args {
		if a.Variadic {
			props[a.Name] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": a.Help}
		} else {
			props[a.Name] = map[string]any{"type": "string", "description": a.Help}
		}
		if a.Required {
			required = append(required, a.Name)
		}
	}
	for _, f := range c.Flags {
		p := map[string]any{"description": f.Help}
		switch f.Type {
		case FlagInt:
			p["type"] = "integer"
		case FlagBool:
			p["type"] = "boolean"
		case FlagDuration:
			p["type"] = "string"
			p["description"] = f.Help + " (a duration such as 300ms or 5s)"
		case FlagEnum:
			p["type"] = "string"
			enum := make([]any, len(f.Enum))
			for i, e := range f.Enum {
				enum[i] = e
			}
			p["enum"] = enum
		default:
			p["type"] = "string"
		}
		props[f.Name] = p
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return CodexCompatibleInputSchema(c.MCPName, s)
}

// CodexCompatibleInputSchema returns schema as a JSON object in which every
// object has "properties" and every array has "items", as Codex requires.
func CodexCompatibleInputSchema(toolName string, schema any) map[string]any {
	data, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("AddTool: tool %q: marshal input schema: %v", toolName, err))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("AddTool: tool %q: unmarshal input schema: %v", toolName, err))
	}
	normalizeSchema(out)
	return out
}

func normalizeSchema(schema any) {
	switch s := schema.(type) {
	case map[string]any:
		for _, value := range s {
			normalizeSchema(value)
		}
		if SchemaTypeIncludes(s["type"], "object") {
			if _, ok := s["properties"].(map[string]any); !ok {
				s["properties"] = map[string]any{}
			}
		}
		if SchemaTypeIncludes(s["type"], "array") {
			if _, ok := s["items"].(map[string]any); !ok {
				s["items"] = map[string]any{}
			}
		}
	case []any:
		for _, value := range s {
			normalizeSchema(value)
		}
	}
}

// SchemaTypeIncludes reports whether a JSON schema "type" value names typ.
func SchemaTypeIncludes(value any, typ string) bool {
	switch v := value.(type) {
	case string:
		return v == typ
	case []any:
		for _, item := range v {
			if item == typ {
				return true
			}
		}
	}
	return false
}

// ManifestEntry is one MCP tool to register: a registry command, or the
// legacy handler of that name when Cmd is nil.
type ManifestEntry struct {
	Name string
	Cmd  *Command
}

// Manifest is the deduplicated registration manifest (design 2.10): every
// registry tool of the profile, plus every legacy tool whose name no
// registry tool reuses. A duplicate name in legacy is an error.
func Manifest(profile string, legacy []string) ([]ManifestEntry, error) {
	byName := map[string]ManifestEntry{}
	for _, c := range All() {
		if InProfile(c, profile) {
			byName[c.MCPName] = ManifestEntry{Name: c.MCPName, Cmd: c}
		}
	}
	seen := map[string]bool{}
	for _, n := range legacy {
		if seen[n] {
			return nil, fmt.Errorf("legacy MCP tool %q is registered twice", n)
		}
		seen[n] = true
		if _, ok := byName[n]; !ok {
			byName[n] = ManifestEntry{Name: n}
		}
	}
	out := make([]ManifestEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ProvidesMCPTool reports whether a registry command of profile registers
// the MCP tool name; the legacy handler of that name is then skipped.
func ProvidesMCPTool(profile, name string) bool {
	for _, c := range All() {
		if InProfile(c, profile) && c.MCPName == name {
			return true
		}
	}
	return false
}

// RegisterMCP registers every registry tool of profile on s. Each call
// runs under the command's timeout (DefaultTimeout when it has none), which
// covers building its Env with newEnv.
func RegisterMCP(s *mcp.Server, profile string, newEnv func(ctx context.Context, g Globals) (Env, error)) {
	for _, c := range All() {
		if !InProfile(c, profile) {
			continue
		}
		cmd := c
		s.AddTool(&mcp.Tool{Name: cmd.MCPName, Description: cmd.Summary, InputSchema: InputSchema(cmd)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				in, err := mcpInput(cmd, req.Params.Arguments)
				if err != nil {
					return toolError(err), nil
				}
				ctx, cancel := context.WithTimeout(ctx, commandTimeout(cmd, in.Global))
				defer cancel()
				env, err := newEnv(ctx, in.Global)
				if err != nil {
					return toolError(err), nil
				}
				res, err := run(ctx, env, cmd, in)
				if err != nil {
					return toolError(err), nil
				}
				text := res.Text
				if res.Data != nil {
					data, err := json.Marshal(res.Data)
					if err != nil {
						return toolError(err), nil
					}
					text = string(data)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
	}
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// mcpInput turns MCP arguments into an Input with the same checks as the CLI.
func mcpInput(c *Command, raw json.RawMessage) (Input, error) {
	in := Input{Flags: map[string]any{}, Global: Globals{Session: "default", Transport: "auto"}}
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return in, fmt.Errorf("%s: arguments are not an object: %w", c.MCPName, err)
		}
	}
	for _, a := range c.Args {
		v, ok := args[a.Name]
		delete(args, a.Name)
		if !ok {
			if a.Required {
				return in, fmt.Errorf("%s: missing argument %q", c.MCPName, a.Name)
			}
			continue
		}
		if a.Variadic {
			list, ok := v.([]any)
			if !ok {
				return in, fmt.Errorf("%s: argument %q must be an array of strings", c.MCPName, a.Name)
			}
			for _, x := range list {
				in.Args = append(in.Args, fmt.Sprint(x))
			}
			continue
		}
		in.Args = append(in.Args, fmt.Sprint(v))
	}
	for _, f := range c.Flags {
		v, ok := args[f.Name]
		delete(args, f.Name)
		s := f.Default
		if ok {
			switch x := v.(type) {
			case float64:
				s = fmt.Sprint(int64(x))
			default:
				s = fmt.Sprint(x)
			}
		}
		if s == "" {
			continue
		}
		val, err := parseFlagValue(f, s)
		if err != nil {
			return in, fmt.Errorf("%s: %w", c.MCPName, err)
		}
		in.Flags[f.Name] = val
	}
	if len(args) > 0 {
		var extra []string
		for k := range args {
			extra = append(extra, k)
		}
		sort.Strings(extra)
		return in, fmt.Errorf("%s: unknown argument %s", c.MCPName, strings.Join(extra, ", "))
	}
	return in, nil
}
