package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP profiles (decision D2, design 4.1): "core" is the default;
// IWDP_MCP_TOOLS (or iwdp-mcp --tools) selects any comma-separated subset
// of core, devtools and legacy, or "all" for every tool.
const (
	ProfileCore     = "core"
	ProfileDevtools = "devtools"
	ProfileLegacy   = "legacy"
	ProfileAll      = "all"
)

// profileOrder is the canonical order of the named profiles.
var profileOrder = []string{ProfileCore, ProfileDevtools, ProfileLegacy}

// SelectedProfile returns the MCP profile chosen by IWDP_MCP_TOOLS (design
// 4.1, D2): unset or empty selects "core"; "all" (also inside a list)
// selects every tool; otherwise a comma-separated subset of core, devtools
// and legacy, returned in the order "core,devtools,legacy". An unknown or
// empty name is an error that names the value.
func SelectedProfile(getenv func(string) string) (string, error) {
	v := getenv("IWDP_MCP_TOOLS")
	if strings.TrimSpace(v) == "" {
		return ProfileCore, nil
	}
	chosen := map[string]bool{}
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == ProfileAll:
			chosen[ProfileAll] = true
		case slices.Contains(profileOrder, name):
			chosen[name] = true
		default:
			return "", fmt.Errorf("IWDP_MCP_TOOLS (or --tools) %q: unknown profile %q; want all, or a comma-separated list of %s", v, name, strings.Join(profileOrder, ", "))
		}
	}
	if chosen[ProfileAll] {
		return ProfileAll, nil
	}
	var out []string
	for _, name := range profileOrder {
		if chosen[name] {
			out = append(out, name)
		}
	}
	return strings.Join(out, ","), nil
}

// ProfileIncludes reports whether the selected profile (SelectedProfile)
// includes the named profile.
func ProfileIncludes(profile, name string) bool {
	return profile == ProfileAll || slices.Contains(strings.Split(profile, ","), name)
}

// InProfile reports whether c is an MCP tool of the selected profile.
func InProfile(c *Command, profile string) bool {
	return c.MCPName != "" && ProfileIncludes(profile, c.Profile)
}

// InputSchema returns the MCP input schema of c: one property per argument
// (a variadic argument is an array of strings) and per flag.
func InputSchema(c *Command) map[string]any {
	props := map[string]any{}
	var required []string
	aliased := map[string]bool{}
	for _, to := range c.MCPAliases {
		aliased[to] = true
	}
	for _, a := range c.Args {
		if a.Variadic {
			props[a.Name] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": a.Help}
		} else {
			props[a.Name] = map[string]any{"type": "string", "description": a.Help}
		}
		if a.Required && !aliased[a.Name] {
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
	for from, to := range c.MCPAliases {
		p := map[string]any{"description": "legacy name of " + to + "; give one of the two"}
		for k, v := range props[to].(map[string]any) {
			if k != "description" {
				p[k] = v
			}
		}
		props[from] = p
	}
	for name, legacy := range c.MCPSchema {
		p, ok := props[name].(map[string]any)
		if !ok {
			panic(fmt.Sprintf("command %q: MCPSchema names %q, which is no argument, flag or MCP alias", c.Name, name))
		}
		desc := p["description"]
		clear(p)
		maps.Copy(p, legacy)
		p["description"] = desc
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
				out, err := mcpResult(cmd, res)
				if err != nil {
					return toolError(err), nil
				}
				return out, nil
			})
	}
}

// mcpText is the command-line text of an MCP argument value: a string as
// it is, a number in decimal notation without exponent (1893456000.5, not
// 1.8934560005e+09), an object or an array as its JSON, and true or false.
func mcpText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case map[string]any, []any:
		data, err := json.Marshal(x)
		if err != nil {
			return "", fmt.Errorf("encoding %T as JSON: %w", x, err)
		}
		return string(data), nil
	}
	return fmt.Sprint(v), nil
}

// mcpImageLimit is the largest PNG that a tool result carries as image
// content. MCP clients pass images to models with size limits (the
// Anthropic API takes at most 5 MB per image, which base64 makes from
// about 3.75 MB), so a larger capture is only named by its file.
const mcpImageLimit = 3_750_000

// pngSignature starts every PNG file.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// mcpResult is the MCP form of a command result: its data as JSON text, or
// its text when it has no data. A file command whose data names a PNG
// file in "path" (screenshot) returns its text, the image and its data as
// structured content, with "file_path" as the legacy take_screenshot tool
// had it.
func mcpResult(c *Command, res Result) (*mcp.CallToolResult, error) {
	if res.Data == nil {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text}}}, nil
	}
	data, err := json.Marshal(res.Data)
	if err != nil {
		return nil, err
	}
	var file struct {
		Path string `json:"path"`
	}
	if c.Output != OutputFile || json.Unmarshal(data, &file) != nil || !strings.HasSuffix(file.Path, ".png") {
		out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}
		// MCP structured content must be a JSON object; a tool that takes
		// over a legacy tool keeps the legacy structured output this way
		// (set_cookie's {ok, cookie}).
		if len(data) > 0 && data[0] == '{' {
			out.StructuredContent = json.RawMessage(data)
		}
		return out, nil
	}
	var structured map[string]any
	if err := json.Unmarshal(data, &structured); err != nil {
		return nil, fmt.Errorf("%s: result data is not an object: %w", c.MCPName, err)
	}
	structured["file_path"] = file.Path
	img, err := os.ReadFile(file.Path)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the capture for the MCP result: %w", c.MCPName, err)
	}
	content := []mcp.Content{&mcp.TextContent{Text: res.Text}}
	switch {
	case !bytes.HasPrefix(img, pngSignature):
		content[0] = &mcp.TextContent{Text: res.Text + "\nimage not attached: the file is not a PNG"}
	case len(img) > mcpImageLimit:
		content[0] = &mcp.TextContent{Text: fmt.Sprintf("%s\nimage not attached: %d bytes is more than the %d bytes an MCP image may have; read the file", res.Text, len(img), mcpImageLimit)}
	default:
		content = append(content, &mcp.ImageContent{Data: img, MIMEType: "image/png"})
	}
	return &mcp.CallToolResult{Content: content, StructuredContent: structured}, nil
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
	for from, to := range c.MCPAliases {
		v, ok := args[from]
		if !ok {
			continue
		}
		if _, both := args[to]; both {
			return in, fmt.Errorf("%s: give %q or its legacy name %q, not both", c.MCPName, to, from)
		}
		delete(args, from)
		args[to] = v
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
				s, err := mcpText(x)
				if err != nil {
					return in, fmt.Errorf("%s: argument %q: %w", c.MCPName, a.Name, err)
				}
				in.Args = append(in.Args, s)
			}
			continue
		}
		s, err := mcpText(v)
		if err != nil {
			return in, fmt.Errorf("%s: argument %q: %w", c.MCPName, a.Name, err)
		}
		in.Args = append(in.Args, s)
	}
	for _, f := range c.Flags {
		v, ok := args[f.Name]
		delete(args, f.Name)
		s := f.Default
		if ok {
			var err error
			if x, isNumber := v.(float64); isNumber && f.Type == FlagInt {
				s = fmt.Sprint(int64(x))
			} else if s, err = mcpText(v); err != nil {
				return in, fmt.Errorf("%s: flag %q: %w", c.MCPName, f.Name, err)
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
	if c.MCPCheck != nil {
		if err := c.MCPCheck(in); err != nil {
			return in, fmt.Errorf("%s: %w", c.MCPName, err)
		}
	}
	return in, nil
}
