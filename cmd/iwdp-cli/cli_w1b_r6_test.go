package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the sixth W1b review (R6-5): both final checks of
// round 5 failed. Each test names the failure it pins.

// enableReplyDelay holds the page's reply to a call, so the signal below
// arrives while that call is in flight.
const enableReplyDelay = 2 * time.Second

// R6-5 and R6-6: SIGINT while the enable call itself is in flight (the page
// received Debugger.enable of setRegistryCommand, which acquired Debugger
// first; its reply is delayed). The page applies the enable whatever
// happens to the reply, so the outcome is unknown and the release must
// still disable Debugger before the process exits 130, and no error line
// reports the call that the signal ended. (A signal during a later call is
// TestCLINoDaemonRegistrySignalRunsCleanup and
// TestCLIRegistrySignalPrintsNoCancelError.)
func TestCLINoDaemonSignalDuringEnableResetsTheDomain(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Debugger.enable", enableReplyDelay)
	p := startNoDaemon(t, f, "Debugger.enable", setRegistryCommand, "--timeout", "60s")
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, cleanupLimit); code != exitInterrupted {
		t.Fatalf("exit %d after SIGINT during Console.enable, want %d; stderr %q", code, exitInterrupted, p.stderr.String())
	}
	if s := p.stderr.String(); strings.Contains(s, "Error:") {
		t.Errorf("stderr %q: the call that the signal's cleanup ended is reported as the command's failure", s)
	}
	if n := len(f.ReceivedMethod("Debugger.disable")); n != 1 {
		t.Fatalf("Debugger.disable sent %d times before exit, want 1: the page received Debugger.enable", n)
	}
}

// Review finding (round-6 review, P1): SIGINT while main already runs the
// in-process cleanup. The command succeeded (setRegistryCommand), and
// main's cleanup disables Console, whose reply the page holds, before it
// disables Debugger. The signal handler's own cleanup found the core
// already taken and exited 130 at once, before main's cleanup sent
// Debugger.disable. Whoever starts the cleanup (main, fatal or the signal
// handler), every other path waits for it to end before it exits, and a
// signal makes the exit code 130.
func TestCLINoDaemonSignalDuringMainCleanupWaitsForIt(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Console.disable", enableReplyDelay)
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, append(strings.Fields(setRegistryCommand), "--page", f.URL)...)
	eventually(t, cleanupLimit, "main's cleanup to disable Console", func() bool {
		return len(f.ReceivedMethod("Console.disable")) == 1
	})
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, cleanupLimit); code != exitInterrupted {
		t.Fatalf("exit %d after SIGINT during main's cleanup, want %d; stderr %q", code, exitInterrupted, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Debugger.disable")); n != 1 {
		t.Fatalf("Debugger.disable sent %d times before the exit, want 1: the exit did not wait for main's cleanup; stderr %q", n, p.stderr.String())
	}
}
