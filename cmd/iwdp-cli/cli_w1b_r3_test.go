package main

import (
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

// R3-3: a command that fails after it set owned state (failRegistryCommand
// enabled Console, then fails) exits 1 with its error, and the cleanup
// disables Console before the process exits.
func TestCLINoDaemonFailedCommandResetsItsState(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	res := runCLI(t, "", []string{"IWDP_NO_DAEMON=1"}, append(strings.Fields(failRegistryCommand), "--page", f.URL)...)
	if res.code != 1 || !strings.Contains(res.stderr, errFixtureFailure.Error()) {
		t.Fatalf("%s: exit %d, stderr %q; want exit 1 with the command's error", failRegistryCommand, res.code, res.stderr)
	}
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Fatalf("Console.disable sent %d times before the exit, want 1 (the reset of the command's state)", n)
	}
}

// startNoDaemon starts the registry command (its words) of IWDP_NO_DAEMON=1
// on f and waits until the page received method (owned state of the
// command).
func startNoDaemon(t *testing.T, f *fakewebkit.Server, method, command string, args ...string) *runningCLI {
	t.Helper()
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, append(append(strings.Fields(command), "--page", f.URL), args...)...)
	eventually(t, cleanupLimit, method+" on the page", func() bool {
		return len(f.ReceivedMethod(method)) > 0
	})
	return p
}

// R3-3: SIGINT or SIGTERM while a command of IWDP_NO_DAEMON=1 runs (here
// idleRegistryCommand, which waits for its deadline without a call in
// flight) runs the cleanup of the in-process core (cleanupOnSignal):
// Console, which the command enabled, is disabled before the process exits
// 130.
func TestCLINoDaemonSignalResetsStateBeforeExit(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			p := startNoDaemon(t, f, "Console.enable", idleRegistryCommand, "--timeout", "60s")
			// The signal must reach the command while it waits: the page
			// receiving Console.enable does not mean that its reply arrived
			// (TestCLINoDaemonSignalDuringEnableResetsTheDomain covers a
			// signal while that call is in flight).
			eventually(t, cleanupLimit, "the command waits", func() bool {
				return strings.Contains(p.stdout.String(), idleLine)
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
