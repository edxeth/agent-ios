package main

import (
	"context"
	"strings"
	"testing"
	"time"

	reg "github.com/edxeth/agent-ios/internal/commands"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// WS2 round-1 review S1 F1 and S3 F2, with the orchestrator's rule
// (criterion 3): an unknown long option exits 2 also with --help or -h, on
// command, group and command-less lines; an unknown command exits 2 with
// or without help; a line without a command exits 2 unless it is "help",
// "--help" or "-h" alone. Ported from TestReviewCliUsageBoundaries and
// TestReviewUnknownLongOptionWithHelp.
func TestUsageLinesExitTwoEvenWithHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // in stderr
	}{
		{[]string{"help", "not-a-command"}, `unknown command "not-a-command"`},
		{[]string{"not-a-command", "--help"}, `unknown command "not-a-command"`},
		{[]string{"-h", "not-a-command"}, `unknown command "not-a-command"`},
		{[]string{"click", "body", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"eval", "1", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"network", "--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"--bogus", "--help"}, "unknown flag --bogus"},
		{[]string{"help", "click", "--bogus"}, "unknown flag --bogus"},
		{[]string{"--json", "--help"}, "no command"},
		{nil, "Usage:"},
	} {
		r := runCLI(t, "", nil, tc.args...)
		if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%q: exit %d, %d bytes of stdout, stderr %q; want exit 2 naming %q", tc.args, r.code, len(r.stdout), r.stderr, tc.want)
		}
	}
}

// "help", "--help" and "-h" alone print the overview with the global
// options; help for a valid command or group exits 0 without the
// command's arguments.
func TestHelpAloneAndValidHelpExitZero(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // in stdout
	}{
		{[]string{"help"}, "Usage: agent-ios <command> [options]"},
		{[]string{"--help"}, "Global options"},
		{[]string{"-h"}, "Global options"},
		{[]string{"click", "--help"}, "Usage: agent-ios click <target>"},
		{[]string{"--help", "click"}, "Usage: agent-ios click <target>"},
		{[]string{"help", "click"}, "Usage: agent-ios click <target>"},
		{[]string{"help", "network"}, `Commands of "network"`},
		{[]string{"help", "eval", "--json"}, "Usage: agent-ios eval <expression>"},
	} {
		r := runCLI(t, "", nil, tc.args...)
		if r.code != 0 || r.stderr != "" || !strings.Contains(r.stdout, tc.want) {
			t.Errorf("%q: exit %d, stderr %q, stdout %q; want exit 0 and %q", tc.args, r.code, r.stderr, r.stdout, tc.want)
		}
	}
}

// Criterion 3a' (round-3 review part 1, TestReviewR3HelpOnlyCombinations):
// a line of only "help", "--help" and "-h", in any number and order,
// prints the overview, the same as "--help" alone, and exits 0. Before,
// "help help" and "--help help" were an unknown command "help" (exit 2).
func TestHelpOnlyLinesPrintTheOverview(t *testing.T) {
	overview := runCLI(t, "", nil, "--help")
	if overview.code != 0 || !strings.Contains(overview.stdout, "Global options") {
		t.Fatalf("--help: exit %d, stdout %q; want the overview", overview.code, overview.stdout)
	}
	for _, args := range [][]string{
		{"help", "help"},
		{"help", "help", "--help"},
		{"--help", "help"},
		{"-h", "help"},
		{"help", "-h", "help"},
		{"--help", "-h"},
		{"-h", "-h", "-h"},
	} {
		r := runCLI(t, "", nil, args...)
		if r.code != 0 || r.stderr != "" || r.stdout != overview.stdout {
			t.Errorf("%q: exit %d, stderr %q, stdout %.60q; want exit 0 and the overview of --help", args, r.code, r.stderr, r.stdout)
		}
	}
	// Any other word keeps the other help rules.
	for _, args := range [][]string{{"help", "help", "bogus"}, {"--help", "help", "--bogus"}} {
		if r := runCLI(t, "", nil, args...); r.code != 2 {
			t.Errorf("%q: exit %d, stdout %.60q; want the usage error (exit 2)", args, r.code, r.stdout)
		}
	}
}

// After "--", --help is an argument: "eval -- --help" evaluates the text
// "--help".
func TestEvalAfterDoubleDashEvaluatesHelpText(t *testing.T) {
	env, f := reservedPage(t)
	echoEval(f)
	r := runCLI(t, "", env, "eval", "--", "--help")
	if r.code != 0 || r.stdout != "evaluated:--help\n" {
		t.Fatalf("eval -- --help: exit %d, stdout %q, stderr %q; want the evaluation of the text --help", r.code, r.stdout, r.stderr)
	}
}

// WS2 round-1 review S3 F1, for every boolean flag of every registered
// command: "--flag" is true, "--flag=<value>" takes exactly the values of
// strconv.ParseBool, and any other value is a usage error (exit 2) that
// names the flag and the value. The global booleans are covered by
// TestGlobalBooleanValues (internal/commands).
func TestEveryBooleanFlagParsesItsValueStrictly(t *testing.T) {
	n := 0
	for _, c := range reg.All() {
		line := strings.Fields(c.Name)
		for _, a := range c.Args {
			if a.Required {
				line = append(line, "x")
			}
		}
		for _, f := range c.Flags {
			if f.Type != reg.FlagBool {
				continue
			}
			n++
			for v, want := range map[string]bool{"": true, "=true": true, "=1": true, "=T": true, "=false": false, "=0": false, "=FALSE": false} {
				inv, err := reg.Parse(append(append([]string{}, line...), "--"+f.Name+v), func(string) string { return "" })
				if err != nil || inv.Input.Flags[f.Name] != want {
					t.Errorf("%s --%s%s: %v; want %v", c.Name, f.Name, v, err, want)
				}
			}
			for _, v := range []string{"banana", "yes", ""} {
				_, err := reg.Parse(append(append([]string{}, line...), "--"+f.Name+"="+v), func(string) string { return "" })
				if reg.ExitCodeOf(err) != reg.ExitUsage || !strings.Contains(err.Error(), "--"+f.Name) || !strings.Contains(err.Error(), `"`+v+`"`) {
					t.Errorf("%s --%s=%q: %v; want a usage error naming the flag and the value", c.Name, f.Name, v, err)
				}
			}
		}
	}
	if n < 10 {
		t.Fatalf("found %d boolean flags in the registry; want the registry's boolean flags", n)
	}
	r := runCLI(t, "", nil, "eval", "1", "--takeover=banana")
	if r.code != 2 || !strings.Contains(r.stderr, `--takeover "banana"`) {
		t.Errorf("eval 1 --takeover=banana: exit %d, stderr %q; want exit 2 naming the option and the value", r.code, r.stderr)
	}
}

// exitAfterDeadline is the contract for the end of a command: the process
// ends at most 200 ms after the command's deadline (docs/guide.md,
// "Deadlines and exit codes": "the command can end up to 200 ms after its
// deadline"; under AGENT_IOS_NO_DAEMON=1 "the process ends at most 200 ms after
// the deadline"). It is the tests' own value, not the product's
// deadlineGrace, so that a change of that constant fails them.
const exitAfterDeadline = 200 * time.Millisecond

// WS2 round-1 review S1 F2 (ported from TestReviewNoDaemonDeadlineBound):
// under AGENT_IOS_NO_DAEMON=1 the page-state cleanup runs within the command's
// remaining budget; a cleanup that the page delays past it is cut off, the
// page connection closes, the process says which cleanup did not finish
// and exits with the command's own status, within the deadline plus the
// 200 ms grace.
//
// The bound is measured inside the CLI process (runCLITimed), from the
// deadline of the command's context to its call of os.Exit, as in
// TestCLIEvalDeadlineWithDeadProbe: the deadline counts from the start of
// the command, after the program has started (docs/guide.md, "Deadlines
// and exit codes"). Process creation, the Go runtime start and the
// process teardown are not part of it; under -race and a loaded host they
// took up to 0.3 s and 0.1 s.
func TestNoDaemonCleanupStaysWithinTheDeadline(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Console.disable", 2*time.Second)
	// GORACE: a binary built with -race sleeps 1 s at exit by default
	// (atexit_sleep_ms); the bound is the command's, not the detector's.
	r, times := runCLITimed(t, []string{"AGENT_IOS_NO_DAEMON=1", "GORACE=atexit_sleep_ms=0"}, "console", "--page", f.URL, "--timeout", "1s")
	if len(f.ReceivedMethod("Console.disable")) != 1 {
		t.Fatalf("cleanup not reached: exit %d stderr=%q", r.code, r.stderr)
	}
	t.Logf("console --timeout 1s: exit call %v after the command's deadline, %v from package initialization to the end of the process, %v including the process start", times.pastDeadline, times.fromInit, r.elapsed)
	if times.pastDeadline > exitAfterDeadline {
		t.Errorf("console --timeout 1s exited %v after its deadline, want at most %v (exitAfterDeadline); exit %d stderr=%q", times.pastDeadline, exitAfterDeadline, r.code, strings.TrimSpace(r.stderr))
	}
	if r.code != 0 || !strings.Contains(r.stderr, "Console.disable") || !strings.Contains(r.stderr, "did not finish") {
		t.Errorf("console --timeout 1s: exit %d, stderr %q; want exit 0 (the command's result) and a warning that names the unfinished Console.disable", r.code, r.stderr)
	}
}

// With a command deadline (bound), the cleanup's context ends closeWait
// before the deadline plus exitAfterDeadline (the contract's 200 ms), a
// cleanup that ignores its context is given up at the deadline plus
// exitAfterDeadline, and the exit keeps the path's own code. After that
// point (the watchdog), the wait is only closeWait.
func TestBoundCleanupEndsByTheDeadlinePlusGrace(t *testing.T) {
	const ahead = 300 * time.Millisecond
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	limit := time.Now().Add(ahead)
	got := make(chan time.Time, 1)
	f.cleanup = func(ctx context.Context) error {
		dl, _ := ctx.Deadline()
		got <- dl
		return nil
	}
	f.bound(limit)
	go f.finish(reg.Outcome{Code: 3})
	if code := f.exitCode(t); code != 3 {
		t.Errorf("exit %d, want the path's own 3", code)
	}
	if dl, want := <-got, limit.Add(exitAfterDeadline-closeWait); !dl.Equal(want) {
		t.Errorf("cleanup context deadline %v, want %v (the command deadline %v plus exitAfterDeadline less closeWait)", dl, want, limit)
	}

	for name, tc := range map[string]struct {
		limit time.Duration // from now
		end   func(f *fakeExit)
		code  int
		least time.Duration
	}{
		"main":     {ahead, func(f *fakeExit) { go f.finish(reg.Outcome{Code: 0}) }, 0, ahead + exitAfterDeadline},
		"watchdog": {-watchdogGrace, func(f *fakeExit) { go f.deadlineExpired() }, exitTimeout, closeWait},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeExit(t, 5*time.Second, time.Second, true)
			start := time.Now()
			f.bound(start.Add(tc.limit))
			tc.end(f)
			code := f.exitCode(t)
			elapsed := time.Since(start)
			if code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			// exitCode waits 50 ms for a second exit; the unbounded wait
			// would be 6 s.
			if elapsed < tc.least || elapsed > tc.least+time.Second {
				t.Errorf("exit after %v; want it at %v", elapsed, tc.least)
			}
			if s := f.stderr.String(); !strings.Contains(s, "did not end within") {
				t.Errorf("stderr %q does not say that the cleanup did not end", s)
			}
		})
	}
}
