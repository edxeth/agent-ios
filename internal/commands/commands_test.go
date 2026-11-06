package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// fixture commands cover the spec features; production commands registered
// by other files are covered by the same table tests through All().
var runs int

func fixtures() []*Command {
	run := func(ctx context.Context, env Env, in Input) (Result, error) {
		runs++
		return Result{Text: "ran " + strings.Join(in.Args, ","), Data: map[string]any{"args": in.Args, "flags": in.Flags}}, nil
	}
	return []*Command{
		{Name: "fixture click", Aliases: []string{"fixture tap"}, MCPName: "fixture_click", Profile: ProfileCore,
			Summary: "Click an element.", Page: LeasedPage, Mutates: true,
			Args:  []Arg{{Name: "target", Help: "ref or selector", Required: true}},
			Flags: []Flag{{Name: "button", Type: FlagEnum, Enum: []string{"left", "right"}, Default: "left", Help: "mouse button"}, {Name: "force", Type: FlagBool, Help: "skip checks"}},
			Run:   run},
		{Name: "fixture type", MCPName: "fixture_type", Profile: ProfileDevtools, Summary: "Type text.", Mutates: true,
			Args:  []Arg{{Name: "target", Help: "ref or selector", Required: true}, {Name: "text", Help: "text to type", Variadic: true}},
			Flags: []Flag{{Name: "delay", Short: "d", Type: FlagDuration, Help: "pause between characters"}, {Name: "count", Type: FlagInt, Help: "repeat count"}},
			Run:   run},
		{Name: "fixture", Summary: "No MCP tool.", Run: run},
	}
}

func withFixtures(t *testing.T) {
	t.Helper()
	fs := fixtures()
	for _, c := range fs {
		Register(c)
	}
	runs = 0
	t.Cleanup(func() {
		for _, c := range fs {
			Unregister(c)
		}
	})
}

type fakeEnv struct{}

func (fakeEnv) Page(context.Context) (*core.PageSession, error) { return nil, errors.New("no page") }
func (fakeEnv) Emit(string)                                     {}
func (fakeEnv) ResolvePath(p string) string                     { return p }

func runCLI(args ...string) (code int, stdout, stderr string, envCalls int) {
	var out, errb bytes.Buffer
	code = RunCLI(context.Background(), args, &out, &errb, func(ctx context.Context, _ *Invocation) (Env, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("newEnv got a context without the command deadline")
		}
		envCalls++
		return fakeEnv{}, nil
	})
	return code, out.String(), errb.String(), envCalls
}

// 7.3 test 12: help for every command prints without calling Run.
func TestHelpNeverRunsACommand(t *testing.T) {
	withFixtures(t)
	all := All()
	if len(all) < 3 {
		t.Fatalf("registry has %d commands", len(all))
	}
	for _, c := range all {
		for _, form := range [][]string{{"--help"}, {"-h"}, {"--json", "--help"}} {
			args := append(strings.Fields(c.Name), form...)
			code, out, _, env := runCLI(args...)
			if code != ExitOK || env != 0 || !strings.Contains(out, "Usage: iwdp-cli "+c.Name) {
				t.Fatalf("%v: code %d, env calls %d, output %q", args, code, env, out)
			}
		}
		// Help also wins over a command line that would fail to parse.
		if code, _, _, env := runCLI(append(strings.Fields(c.Name), "--no-such-flag", "-h")...); code != ExitOK || env != 0 {
			t.Fatalf("%s: help with a bad flag gave code %d, env calls %d", c.Name, code, env)
		}
	}
	if runs != 0 {
		t.Fatalf("Run was called %d times by help", runs)
	}
}

func TestEveryFlagAndArgumentHasHelpText(t *testing.T) {
	withFixtures(t)
	for _, c := range All() {
		if err := validate(c); err != nil {
			t.Fatal(err)
		}
		help := Help(c)
		for _, f := range c.Flags {
			if !strings.Contains(help, "--"+f.Name) || !strings.Contains(help, f.Help) {
				t.Fatalf("%s help lacks --%s and its text", c.Name, f.Name)
			}
		}
	}
}

func TestUsageErrorsExitTwoBeforeAnyConnection(t *testing.T) {
	withFixtures(t)
	for _, args := range [][]string{
		{"fixture", "click", "@e1", "--all"},         // unknown flag
		{"fixture", "click"},                         // missing argument
		{"fixture", "click", "@e1", "extra"},         // too many arguments
		{"fixture", "click", "@e1", "--button", "x"}, // bad enum
		{"fixture", "type", "@e1", "-d", "soon"},     // bad duration
		{"fixture", "type", "@e1", "--count"},        // missing value
		{"no-such-command"},                          // unknown command
		{"--timeout", "-1s", "fixture"},              // bad global
	} {
		code, _, stderr, env := runCLI(args...)
		if code != ExitUsage || env != 0 {
			t.Fatalf("%v: code %d, env calls %d (%s); want exit 2 before any connection", args, code, env, stderr)
		}
	}
	if runs != 0 {
		t.Fatalf("Run called %d times", runs)
	}
}

func TestParseFlagsArgsGlobalsAndAliases(t *testing.T) {
	withFixtures(t)
	env := func(k string) string { return map[string]string{"IWDP_SESSION": "agent-1"}[k] }
	inv, err := Parse([]string{"--json", "fixture", "type", "@e3", "-d", "300ms", "--timeout=5s", "a", "--", "--b"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Cmd.Name != "fixture type" || strings.Join(inv.Input.Args, " ") != "@e3 a --b" {
		t.Fatalf("parsed %q %v", inv.Cmd.Name, inv.Input.Args)
	}
	if inv.Input.Flags["delay"] != 300*time.Millisecond || !inv.Input.Global.JSON || inv.Input.Global.Timeout != 5*time.Second || inv.Input.Global.Session != "agent-1" {
		t.Fatalf("flags %v, globals %+v", inv.Input.Flags, inv.Input.Global)
	}
	inv, err = Parse([]string{"fixture", "tap", "@e1", "--force", "--session", "s2"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Cmd.Name != "fixture click" || inv.Input.Flags["force"] != true || inv.Input.Flags["button"] != "left" || inv.Input.Global.Session != "s2" {
		t.Fatalf("alias parse: %q %v %+v", inv.Cmd.Name, inv.Input.Flags, inv.Input.Global)
	}
	code, out, _, envCalls := runCLI("fixture", "click", "@e9", "--max-output", "5")
	if code != ExitOK || envCalls != 1 || !strings.HasPrefix(out, "ran @\n[output truncated: 5 of 7 bytes shown]") {
		t.Fatalf("run: code %d, env %d, out %q", code, envCalls, out)
	}
}

func TestRegisterRejectsDuplicatesAndMissingHelp(t *testing.T) {
	withFixtures(t)
	run := func(context.Context, Env, Input) (Result, error) { return Result{}, nil }
	for name, c := range map[string]*Command{
		"duplicate name":      {Name: "fixture", Summary: "x", Run: run},
		"alias takes a name":  {Name: "other", Aliases: []string{"fixture click"}, Summary: "x", Run: run},
		"duplicate MCP name":  {Name: "other", MCPName: "fixture_click", Profile: ProfileCore, Summary: "x", Run: run},
		"flag without help":   {Name: "other", Summary: "x", Run: run, Flags: []Flag{{Name: "f"}}},
		"shadows global flag": {Name: "other", Summary: "x", Run: run, Flags: []Flag{{Name: "json", Help: "h"}}},
		"arg without help":    {Name: "other", Summary: "x", Run: run, Args: []Arg{{Name: "a"}}},
		"MCP without profile": {Name: "other", MCPName: "other", Summary: "x", Run: run},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: Register accepted it", name)
				}
			}()
			Register(c)
		}()
	}
	seen := map[string]bool{}
	for _, c := range All() {
		for _, n := range append([]string{c.Name, "mcp:" + c.MCPName}, c.Aliases...) {
			if n == "mcp:" {
				continue
			}
			if seen[n] {
				t.Fatalf("name %q is used twice", n)
			}
			seen[n] = true
		}
	}
}

// walkSchema checks the Codex rules on every object and array.
func walkSchema(t *testing.T, tool string, v any) {
	switch s := v.(type) {
	case map[string]any:
		if SchemaTypeIncludes(s["type"], "object") {
			if _, ok := s["properties"].(map[string]any); !ok {
				t.Fatalf("%s: object without properties: %v", tool, s)
			}
		}
		if SchemaTypeIncludes(s["type"], "array") {
			if _, ok := s["items"].(map[string]any); !ok {
				t.Fatalf("%s: array without items: %v", tool, s)
			}
		}
		for _, x := range s {
			walkSchema(t, tool, x)
		}
	case []any:
		for _, x := range s {
			walkSchema(t, tool, x)
		}
	}
}

func TestMCPSchemasAreCodexCompatible(t *testing.T) {
	withFixtures(t)
	for _, c := range All() {
		if c.MCPName == "" {
			continue
		}
		s := InputSchema(c)
		walkSchema(t, c.MCPName, s)
		again := CodexCompatibleInputSchema(c.MCPName, s)
		a, _ := json.Marshal(s)
		b, _ := json.Marshal(again)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: schema changes under codexCompatibleInputSchema:\n%s\n%s", c.MCPName, a, b)
		}
	}
}

func TestManifestDeduplicatesAndRejectsDuplicates(t *testing.T) {
	// Production commands (other files of this package) are in the
	// manifest too; the fixtures add exactly their own entries.
	production, err := Manifest(ProfileCore, nil)
	if err != nil {
		t.Fatal(err)
	}
	withFixtures(t)
	m, err := Manifest(ProfileCore, []string{"fixture_click", "eval_js", "list_pages"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*Command{}
	for _, e := range m {
		if _, dup := names[e.Name]; dup {
			t.Fatalf("manifest lists %q twice", e.Name)
		}
		names[e.Name] = e.Cmd
	}
	if names["fixture_click"] == nil || names["eval_js"] != nil || len(names) != len(production)+3 {
		t.Fatalf("manifest = %+v; want the registry tool to replace the legacy one", m)
	}
	if _, ok := names["fixture_type"]; ok {
		t.Fatal("a devtools tool is in the core profile")
	}
	if !ProvidesMCPTool(ProfileCore, "fixture_click") || ProvidesMCPTool(ProfileCore, "fixture_type") || !ProvidesMCPTool(ProfileAll, "fixture_type") {
		t.Fatal("ProvidesMCPTool does not follow the profile")
	}
	if _, err := Manifest(ProfileCore, []string{"a", "a"}); err == nil {
		t.Fatal("a duplicate legacy name was accepted")
	}
	if SelectedProfile(func(string) string { return "" }) != ProfileCore || SelectedProfile(func(string) string { return "all" }) != ProfileAll {
		t.Fatal("SelectedProfile does not follow IWDP_MCP_TOOLS (D2)")
	}
}

func TestRegisterMCPRunsCommands(t *testing.T) {
	production, err := Manifest(ProfileAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	withFixtures(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	RegisterMCP(server, ProfileAll, func(context.Context, Globals) (Env, error) { return fakeEnv{}, nil })
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(production)+2 {
		t.Fatalf("tools = %d, want the %d production tools and the 2 fixture MCP tools", len(tools.Tools), len(production))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture_type", Arguments: map[string]any{"target": "@e1", "text": []any{"a", "b"}, "delay": "10ms", "count": 2}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"args":["@e1","a","b"]`) || !strings.Contains(text, `"count":2`) || !strings.Contains(text, `"delay":10000000`) {
		t.Fatalf("result %s", text)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture_click", Arguments: map[string]any{"target": "@e1", "all": true}})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("unknown argument accepted: %+v", res)
	}
}

// Design 2.6: one absolute deadline per command. The CLI binding applies
// --timeout, IWDP_TIMEOUT or the command's own timeout before it builds the
// environment, and a command that waits for its context ends at that
// deadline with exit code 3.
func TestRunCLIAppliesCommandDeadline(t *testing.T) {
	waitCmd := func(name string, timeout time.Duration) *Command {
		return &Command{Name: name, Summary: "Wait until the deadline.", Timeout: timeout,
			Run: func(ctx context.Context, env Env, in Input) (Result, error) {
				select {
				case <-ctx.Done():
					return Result{}, ctx.Err()
				case <-time.After(3 * time.Second):
					return Result{}, errors.New("no deadline reached the command")
				}
			}}
	}
	var seen time.Duration // time left on the command's context when Run started; -1: no deadline
	probe := &Command{Name: "fixture deadline", Summary: "Report the deadline.", Run: func(ctx context.Context, env Env, in Input) (Result, error) {
		seen = -1
		if dl, ok := ctx.Deadline(); ok {
			seen = time.Until(dl)
		}
		return Result{}, nil
	}}
	cmds := []*Command{waitCmd("fixture wait", 0), waitCmd("fixture slow", 80*time.Millisecond), waitCmd("fixture long", 2*time.Second), probe}
	for _, c := range cmds {
		Register(c)
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			Unregister(c)
		}
	})
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{"flag", "", []string{"--timeout", "80ms", "fixture", "wait"}},
		{"IWDP_TIMEOUT", "80ms", []string{"fixture", "wait"}},
		{"command default", "", []string{"fixture", "slow"}},
		{"flag beats command default", "", []string{"fixture", "long", "--timeout=40ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IWDP_TIMEOUT", tc.env)
			start := time.Now()
			code, _, stderr, _ := runCLI(tc.args...)
			if el := time.Since(start); code != ExitTimeout || el > time.Second {
				t.Fatalf("exit %d after %s (%s); want exit 3 at the deadline", code, el, stderr)
			}
		})
	}
	t.Setenv("IWDP_TIMEOUT", "soon")
	if code, _, _, env := runCLI("fixture", "deadline"); code != ExitUsage || env != 0 {
		t.Fatalf("IWDP_TIMEOUT=soon: exit %d, env calls %d; want exit 2 before any connection", code, env)
	}
	t.Setenv("IWDP_TIMEOUT", "")
	if code, _, stderr, _ := runCLI("fixture", "deadline"); code != ExitOK || seen < 29*time.Second || seen > DefaultTimeout {
		t.Fatalf("exit %d (%s), time left %s; want the default deadline of 30 s", code, stderr, seen)
	}
}
