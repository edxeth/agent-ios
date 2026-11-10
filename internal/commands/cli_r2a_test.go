package commands

import (
	"context"
	"strings"
	"testing"
)

// WS2 round-1 review S1 F1 and S3 F2, with the orchestrator's rule
// (criterion 3): an unknown long option exits 2 also with --help or -h, on
// command, group and command-less lines; an unknown command exits 2 with
// or without help; a line without a command exits 2 unless it is only
// --help or -h. None of them builds the environment (no connection).
func TestUsageErrorsWinOverHelp(t *testing.T) {
	withFixtures(t)
	for _, tc := range []struct {
		args []string
		want string // in stderr
	}{
		{[]string{"--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"--bogus", "-h"}, "unknown flag --bogus"},
		{[]string{"-h", "--bogus=1"}, "unknown flag --bogus"},
		{[]string{"eval", "1", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"click", "body", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"fixture", "click", "--no-such-flag", "-h"}, "unknown flag --no-such-flag"},
		{[]string{"network", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"network", "-d", "1", "--bogus", "-h"}, "unknown flag --bogus"},
		{[]string{"not-a-command", "--help"}, "unknown command"},
		{[]string{"--json", "--help"}, "no command"},
		{[]string{"-h", "--json"}, "no command"},
		{[]string{"--page", "x", "-h"}, "no command"},
	} {
		code, out, stderr, calls := runCLI(tc.args...)
		if code != ExitUsage || calls != 0 || out != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, env calls %d, %d bytes of stdout, stderr %q; want exit 2 naming %q before any connection", tc.args, code, calls, len(out), stderr, tc.want)
		}
	}
}

// Help for a valid command or group still exits 0 without the command's
// required arguments and makes no connection; "--help" or "-h" alone is
// the overview.
func TestValidHelpNeedsNoArgumentsAndNoConnection(t *testing.T) {
	withFixtures(t)
	for _, tc := range []struct {
		args []string
		want string // stdout prefix
	}{
		{[]string{"click", "--help"}, "Usage: iwdp-cli click"},
		{[]string{"fixture", "click", "--force", "-h"}, "Usage: iwdp-cli fixture click"},
		{[]string{"fixture", "click", "--button=right", "--json", "--help"}, "Usage: iwdp-cli fixture click"},
		{[]string{"network", "--help"}, "Usage: iwdp-cli network <command>"},
		{[]string{"network", "-d", "1", "--help"}, "Usage: iwdp-cli network <command>"},
		{[]string{"--help"}, "Commands:"},
		{[]string{"-h"}, "Commands:"},
		{[]string{"--help", "-h"}, "Commands:"},
	} {
		code, out, stderr, calls := runCLI(tc.args...)
		if code != ExitOK || calls != 0 || !strings.HasPrefix(out, tc.want) {
			t.Errorf("%q: exit %d, env calls %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, calls, out, stderr, tc.want)
		}
	}
	// After "--", --help is an argument.
	inv, err := Parse([]string{"eval", "--", "--help"}, noEnv)
	if err != nil || inv.Help || strings.Join(inv.Input.Args, " ") != "--help" {
		t.Errorf("Parse(eval -- --help) = %+v, %v; want eval of the text --help", inv, err)
	}
}

// parseBoolValues are the values that strconv.ParseBool accepts, with the
// result (Go documentation of strconv.ParseBool).
var parseBoolValues = map[string]bool{
	"1": true, "t": true, "T": true, "TRUE": true, "true": true, "True": true,
	"0": false, "f": false, "F": false, "FALSE": false, "false": false, "False": false,
}

// rejectedBoolValues are values that strconv.ParseBool refuses.
var rejectedBoolValues = []string{"banana", "", "yes", "no", "on", "off", "tRUE", " true", "2"}

// WS2 round-1 review S3 F1: a global boolean option takes "--flag" as
// true and "--flag=<value>" with exactly the values of strconv.ParseBool;
// any other value is a usage error that names the option and the value.
// --takeover=false never authorizes a takeover.
func TestGlobalBooleanValues(t *testing.T) {
	get := map[string]func(Globals) bool{
		"--json":               func(g Globals) bool { return g.JSON },
		"--content-boundaries": func(g Globals) bool { return g.ContentBoundaries },
		"--takeover":           func(g Globals) bool { return g.Takeover },
	}
	for name, value := range get {
		inv, err := Parse([]string{"eval", "1", name}, noEnv)
		if err != nil || !value(inv.Input.Global) {
			t.Errorf("%s: %+v, %v; want true", name, inv, err)
		}
		for v, want := range parseBoolValues {
			inv, err := Parse([]string{"eval", "1", name + "=" + v}, noEnv)
			if err != nil || value(inv.Input.Global) != want {
				t.Errorf("%s=%s: %v; want %v", name, v, err, want)
			}
		}
		for _, v := range rejectedBoolValues {
			_, err := Parse([]string{"eval", "1", name + "=" + v}, noEnv)
			if ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), `"`+v+`"`) {
				t.Errorf("%s=%q: %v; want a usage error naming the option and the value", name, v, err)
			}
		}
	}
}

// takeoverEnv is a RemoteEnv that records the takeover of the input it
// would send to the daemon.
type takeoverEnv struct {
	fakeEnv
	takeover *[]bool
}

func (e takeoverEnv) RunRemote(_ context.Context, _ *Command, in Input) (Result, error) {
	*e.takeover = append(*e.takeover, in.Global.Takeover)
	return Result{Text: "ok"}, nil
}

// --takeover=false reaches the daemon request as no takeover, and
// --takeover=banana stops before the environment is built.
func TestTakeoverFalseIsSentAsNoTakeover(t *testing.T) {
	var seen []bool
	var built int
	newEnv := func(context.Context, *Invocation) (Env, error) { built++; return takeoverEnv{takeover: &seen}, nil }
	var out, errb strings.Builder
	for _, args := range [][]string{{"eval", "1", "--takeover=false"}, {"eval", "1", "--takeover"}, {"eval", "1", "--takeover=0"}} {
		if code := RunCLIStarted(context.Background(), args, &out, &errb, nil, newEnv); code != ExitOK {
			t.Fatalf("%q: exit %d, stderr %q", args, code, errb.String())
		}
	}
	if len(seen) != 3 || seen[0] || !seen[1] || seen[2] {
		t.Fatalf("takeover sent for =false, bare, =0: %v; want [false true false]", seen)
	}
	errb.Reset()
	if code := RunCLIStarted(context.Background(), []string{"eval", "1", "--takeover=banana"}, &out, &errb, nil, newEnv); code != ExitUsage || built != 3 || !strings.Contains(errb.String(), `--takeover "banana"`) {
		t.Fatalf("--takeover=banana: exit %d, env built %d times, stderr %q; want exit 2 before the environment", code, built, errb.String())
	}
}
