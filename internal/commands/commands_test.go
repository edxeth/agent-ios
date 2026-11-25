package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
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
		{
			Name: "fixture click", Aliases: []string{"fixture tap"},
			Summary: "Click an element.", Page: LeasedPage, Mutates: true,
			Args:  []Arg{{Name: "target", Help: "ref or selector", Required: true}},
			Flags: []Flag{{Name: "button", Type: FlagEnum, Enum: []string{"left", "right"}, Default: "left", Help: "mouse button"}, {Name: "force", Type: FlagBool, Help: "skip checks"}},
			Run:   run,
		},
		{
			Name: "fixture type", Summary: "Type text.", Mutates: true,
			Args:  []Arg{{Name: "target", Help: "ref or selector", Required: true}, {Name: "text", Help: "text to type", Variadic: true}},
			Flags: []Flag{{Name: "delay", Short: "d", Type: FlagDuration, Help: "pause between characters"}, {Name: "count", Type: FlagInt, Help: "repeat count"}},
			Run:   run,
		},
		{Name: "fixture", Summary: "No page.", Run: run},
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
	code = RunCLIStarted(context.Background(), args, &out, &errb, nil, func(ctx context.Context, _ *Invocation) (Env, error) {
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
			if code != ExitOK || env != 0 || !strings.Contains(out, "Usage: agent-ios "+c.Name) {
				t.Fatalf("%v: code %d, env calls %d, output %q", args, code, env, out)
			}
		}
		// Help never hides an unknown flag: the line is a usage error
		// (criterion 3; WS2 round-1 review S1 F1 and S3 F2).
		if code, _, _, env := runCLI(append(strings.Fields(c.Name), "--no-such-flag", "-h")...); code != ExitUsage || env != 0 {
			t.Fatalf("%s: help with a bad flag gave code %d, env calls %d; want exit 2 without a connection", c.Name, code, env)
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
	env := func(k string) string { return map[string]string{"AGENT_IOS_SESSION": "agent-1"}[k] }
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
		"flag without help":   {Name: "other", Summary: "x", Run: run, Flags: []Flag{{Name: "f"}}},
		"shadows global flag": {Name: "other", Summary: "x", Run: run, Flags: []Flag{{Name: "json", Help: "h"}}},
		"arg without help":    {Name: "other", Summary: "x", Run: run, Args: []Arg{{Name: "a"}}},
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
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if seen[n] {
				t.Fatalf("name %q is used twice", n)
			}
			seen[n] = true
		}
	}
}

// Design 2.6: one absolute deadline per command. The CLI binding applies
// --timeout, AGENT_IOS_TIMEOUT or the command's own timeout before it builds the
// environment, and a command that waits for its context ends at that
// deadline with exit code 3.
func TestRunCLIAppliesCommandDeadline(t *testing.T) {
	waitCmd := func(name string, timeout time.Duration) *Command {
		return &Command{
			Name: name, Summary: "Wait until the deadline.", Timeout: timeout,
			Run: func(ctx context.Context, env Env, in Input) (Result, error) {
				select {
				case <-ctx.Done():
					return Result{}, ctx.Err()
				case <-time.After(3 * time.Second):
					return Result{}, errors.New("no deadline reached the command")
				}
			},
		}
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
		{"AGENT_IOS_TIMEOUT", "80ms", []string{"fixture", "wait"}},
		{"command default", "", []string{"fixture", "slow"}},
		{"flag beats command default", "", []string{"fixture", "long", "--timeout=40ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_IOS_TIMEOUT", tc.env)
			start := time.Now()
			code, _, stderr, _ := runCLI(tc.args...)
			if el := time.Since(start); code != ExitTimeout || el > time.Second {
				t.Fatalf("exit %d after %s (%s); want exit 124 at the deadline", code, el, stderr)
			}
		})
	}
	t.Setenv("AGENT_IOS_TIMEOUT", "soon")
	if code, _, _, env := runCLI("fixture", "deadline"); code != ExitUsage || env != 0 {
		t.Fatalf("AGENT_IOS_TIMEOUT=soon: exit %d, env calls %d; want exit 2 before any connection", code, env)
	}
	t.Setenv("AGENT_IOS_TIMEOUT", "")
	if code, _, stderr, _ := runCLI("fixture", "deadline"); code != ExitOK || seen < 29*time.Second || seen > DefaultTimeout {
		t.Fatalf("exit %d (%s), time left %s; want the default deadline of 30 s", code, stderr, seen)
	}
}
