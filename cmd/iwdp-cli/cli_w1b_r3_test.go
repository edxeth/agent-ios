package main

import (
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the third W1b review (R3-3): the in-process core of
// IWDP_NO_DAEMON=1 resets the page state that a command set before the
// process exits, also when the command fails or a signal ends it (design
// 2.4 and INV A10; R2-6 in docs/notes/w1b.md). The page is a private fake.

// cleanupLimit bounds the exit of a command after its end or a signal:
// the cleanup has daemon.ShutdownBudget (5 s), plus process start.
const cleanupLimit = 10 * time.Second

// R3-3: a command that fails after it set owned state (the page set the
// URL breakpoint, but its reply cannot be decoded) exits through fatal,
// which removes that breakpoint before the process exits.
func TestCLINoDaemonFailedCommandResetsItsState(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Result("Debugger.setBreakpointByUrl", map[string]any{"breakpointId": "bp-1", "locations": "not a list"})
	res := runCLI(t, "", []string{"IWDP_NO_DAEMON=1"}, "set-breakpoint", "https://fixture.example/app.js", "3", f.URL)
	if res.code != 1 || !strings.Contains(res.stderr, "decoding setBreakpointByUrl") {
		t.Fatalf("set-breakpoint with an undecodable reply: exit %d, stderr %q; want exit 1 with the decoding error", res.code, res.stderr)
	}
	var removed []string
	for _, r := range f.ReceivedMethod("Debugger.removeBreakpoint") {
		var p struct {
			ID string `json:"breakpointId"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("Debugger.removeBreakpoint params %s: %v", r.Params, err)
		}
		removed = append(removed, p.ID)
	}
	if len(removed) != 1 || removed[0] != "bp-1" {
		t.Fatalf("breakpoints removed before exit: %v, want [bp-1]", removed)
	}
}

// startNoDaemon starts an IWDP_NO_DAEMON=1 command on f and waits until
// the page received Console.enable (owned state of the command).
func startNoDaemon(t *testing.T, f *fakewebkit.Server, args ...string) *runningCLI {
	t.Helper()
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, args...)
	eventually(t, cleanupLimit, "Console.enable on the page", func() bool {
		return len(f.ReceivedMethod("Console.enable")) == 1
	})
	return p
}

// R3-3: SIGINT or SIGTERM while a command of IWDP_NO_DAEMON=1 runs (here
// console-messages, which sleeps for its duration) runs the cleanup of the
// in-process core (cleanupOnSignal): Console, which the command enabled, is
// disabled before the process exits 130.
func TestCLINoDaemonSignalResetsStateBeforeExit(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			p := startNoDaemon(t, f, "console-messages", "-d", "60s", f.URL)
			// The signal must reach the command while it sleeps: the page
			// receiving Console.enable does not mean that its reply arrived
			// (TestCLINoDaemonSignalDuringCallExitsInterrupted covers a
			// signal while a call is in flight).
			eventually(t, cleanupLimit, "the command sleeps", func() bool {
				return strings.Contains(p.stderr.String(), "Collecting console messages")
			})
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := p.wait(t, cleanupLimit); code != exitInterrupted {
				t.Fatalf("exit %d after %v, want %d; stderr %q", code, sig, exitInterrupted, p.stderr.String())
			}
			if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
				t.Fatalf("Console.disable sent %d times before exit, want 1 (the release of the command's state)", n)
			}
		})
	}
}

// streamWaitSettle is the time between the "Listening" message of a
// stream and the signal: waitForStream registers its signal handler right
// after that message, and a signal before it ends the command as any other
// command (exit 130).
const streamWaitSettle = 300 * time.Millisecond

// R3-3: a streaming command that waits for its end (console --follow)
// handles the signal itself: it ends the stream with its own Console.disable
// (the page answers after a second, so a cleanup of the in-process core that
// ran meanwhile would fail that call), and the process exits normally after
// the cleanup of the in-process core.
func TestCLINoDaemonSignalDuringStreamResetsState(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Console.disable", time.Second)
	p := startNoDaemon(t, f, "console", "--follow", f.URL)
	eventually(t, cleanupLimit, "the stream waits", func() bool {
		return strings.Contains(p.stderr.String(), "Listening for console messages")
	})
	time.Sleep(streamWaitSettle)
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, cleanupLimit); code != 0 {
		t.Fatalf("exit %d after SIGINT, want 0; stderr %q", code, p.stderr.String())
	}
	if s := p.stderr.String(); !strings.Contains(s, "(interrupted)") || strings.Contains(s, "warning:") {
		t.Fatalf("stderr %q: want the interrupted stream ended by its own Console.disable, without warnings", s)
	}
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Fatalf("Console.disable sent %d times before exit, want 1", n)
	}
}
