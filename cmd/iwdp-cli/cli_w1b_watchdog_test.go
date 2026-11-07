package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the W1b closure review of tree dce71d4d (P1): under
// IWDP_NO_DAEMON=1 the deadline watchdog ended the process with exit 124
// while the in-process cleanup still ran, so the page state the command set
// was never reset. Every exit path (main, fatal, the signal handler and the
// watchdog) waits for the one cleanup; a signal makes the exit code 130, an
// expired deadline 124 (processExit in daemon_cli.go).

// cleanupPastDeadline holds the page's reply to the cleanup's first reset
// longer than the command's deadline plus watchdogGrace, and less than
// daemon.ShutdownBudget, so the watchdog fires while the cleanup runs and
// the cleanup still ends within its budget.
const cleanupPastDeadline = 3500 * time.Millisecond

// shortDeadline is the --timeout of the tests below: the watchdog fires at
// shortDeadline + watchdogGrace, inside the held cleanup.
const shortDeadline = "500ms"

// startHeldCleanup starts set-breakpoint under IWDP_NO_DAEMON=1 with
// --timeout shortDeadline and waits until main's cleanup sent
// Debugger.removeBreakpoint, whose reply the page holds for
// cleanupPastDeadline.
func startHeldCleanup(t *testing.T) (*fakewebkit.Server, *runningCLI) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Result("Debugger.setBreakpointByUrl", map[string]any{"breakpointId": "bp-1", "locations": []any{}})
	f.DelayReplies("Debugger.removeBreakpoint", cleanupPastDeadline)
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, "set-breakpoint", "https://fixture.example/app.js", "3", f.URL, "--timeout", shortDeadline)
	eventually(t, cleanupLimit, "main's cleanup to remove the breakpoint", func() bool {
		return len(f.ReceivedMethod("Debugger.removeBreakpoint")) == 1
	})
	return f, p
}

// Reviewer's counterexample: SIGINT during main's cleanup, which runs past
// the deadline. The watchdog exited 124 at 2.5 s, before the cleanup sent
// Debugger.disable. The watchdog must wait for the running cleanup, and the
// signal makes the exit code 130.
func TestCLINoDaemonSignalDuringCleanupPastTheDeadline(t *testing.T) {
	f, p := startHeldCleanup(t)
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, cleanupLimit); code != exitInterrupted {
		t.Errorf("exit %d after SIGINT during the cleanup, want %d; stderr %q", code, exitInterrupted, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Debugger.disable")); n != 1 {
		t.Errorf("Debugger.disable sent %d times before the exit, want 1: an exit path preempted the cleanup; stderr %q", n, p.stderr.String())
	}
}

// Deadline only: the same held cleanup without a signal. The deadline
// expired while the cleanup ran, so the exit code is 124, but only after
// the cleanup reset the page state.
func TestCLINoDaemonDeadlineDuringCleanupWaitsForIt(t *testing.T) {
	f, p := startHeldCleanup(t)
	if code := p.wait(t, cleanupLimit); code != exitTimeout {
		t.Errorf("exit %d after the deadline expired during the cleanup, want %d; stderr %q", code, exitTimeout, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Debugger.disable")); n != 1 {
		t.Errorf("Debugger.disable sent %d times before the exit, want 1: the watchdog preempted the cleanup; stderr %q", n, p.stderr.String())
	}
	if s := p.stderr.String(); !strings.Contains(s, "iwdp-cli set-breakpoint timed out") {
		t.Errorf("stderr %q lacks the deadline error", s)
	}
}

// heldRegistryCommand is a registry command for the tests below: it
// acquires Console for its session's owner, enables it and then evaluates,
// as an ordinary registry command that sets page state does.
// idleRegistryCommand enables Console the same way, then waits for its
// deadline without a call in flight.
const (
	heldRegistryCommand = "test hold console"
	idleRegistryCommand = "test idle console"
)

func init() {
	for _, name := range []string{heldRegistryCommand, idleRegistryCommand} {
		reg.Register(&reg.Command{
			Name: name, Summary: "Test fixture: enable Console, then evaluate or wait.", Page: reg.AnyPage, Mutates: true,
			Run: func(ctx context.Context, env reg.Env, in reg.Input) (reg.Result, error) {
				ps, err := env.Page(ctx)
				if err != nil {
					return reg.Result{}, err
				}
				ps.State.Acquire(core.SessionOwner("test", in.Global.Session), "Console")
				c := env.(interface{ Op() *core.Op }).Op().Caller()
				if _, err := c.Send(ctx, "Console.enable", nil); err != nil {
					return reg.Result{}, err
				}
				if name == idleRegistryCommand {
					<-ctx.Done()
					return reg.Result{}, ctx.Err()
				}
				_, err = c.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"})
				return reg.Result{}, err
			},
		})
	}
}

// Reviewer's counterexample (registry part): SIGINT while a registry
// command of IWDP_NO_DAEMON=1 waits for its evaluate. Registry commands had
// no signal cleanup, so the signal ended the process at once and Console
// stayed enabled. They get the same cleanup as legacy commands: exit 130
// after Console.disable.
func TestCLINoDaemonRegistrySignalRunsCleanup(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Runtime.evaluate", cleanupPastDeadline)
	words := strings.Fields(heldRegistryCommand)
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, append(words, "--page", f.URL)...)
	eventually(t, cleanupLimit, "the registry command's Runtime.evaluate", func() bool {
		select {
		case <-p.done:
			t.Fatalf("the registry command exited %d before the signal; stderr %q", p.code, p.stderr.String())
		default:
		}
		return len(f.ReceivedMethod("Runtime.evaluate")) == 1
	})
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, cleanupLimit); code != exitInterrupted {
		t.Errorf("exit %d after SIGINT during a registry command, want %d; stderr %q", code, exitInterrupted, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Errorf("Console.disable sent %d times before the exit, want 1; stderr %q", n, p.stderr.String())
	}
}

// A registry command of IWDP_NO_DAEMON=1 gets the watchdog of legacy
// commands: the command ends at its 500 ms deadline (exit 124 from the
// command; no call is in flight then, so no liveness probe can mark the
// page stalled before the cleanup), main's cleanup then waits for the held Console.disable past the
// deadline plus watchdogGrace, and the watchdog fires during it. The
// watchdog waits for the cleanup and reports the expired deadline.
func TestCLINoDaemonRegistryWatchdogWaitsForTheCleanup(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Console.disable", cleanupPastDeadline)
	words := strings.Fields(idleRegistryCommand)
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, append(words, "--page", f.URL, "--timeout", shortDeadline)...)
	if code := p.wait(t, cleanupLimit); code != exitTimeout {
		t.Errorf("exit %d, want %d; stderr %q", code, exitTimeout, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Errorf("Console.disable sent %d times before the exit, want 1; stderr %q", n, p.stderr.String())
	}
	if s := p.stderr.String(); !strings.Contains(s, errWatchdog.Error()) || !strings.Contains(s, "iwdp-cli "+idleRegistryCommand+" timed out") {
		t.Errorf("stderr %q lacks the watchdog's deadline error for the registry command", s)
	}
}

// Unit tests of processExit with a fake cleanup and exit: the order of the
// cleanup and the exit, and the exit code, for the watchdog, a signal and
// main (the contract in the processExit comment).

// fakeExit is a processExit whose cleanup the test holds and whose exits
// the test records.
type fakeExit struct {
	*processExit
	started chan struct{} // closed when the cleanup started
	release chan struct{} // closing it ends the held cleanup
	runs    atomic.Int32
	codes   chan int
	stderr  *syncBuffer
}

// newFakeExit returns a processExit whose cleanup runs until release is
// closed, or ignores its context and never ends when hang is set.
func newFakeExit(t *testing.T, budget, grace time.Duration, hang bool) *fakeExit {
	t.Helper()
	saved := current
	current = &commandRun{name: "eval", page: "ws://page", start: time.Now(), budget: time.Second}
	t.Cleanup(func() { current = saved })
	f := &fakeExit{started: make(chan struct{}), release: make(chan struct{}), codes: make(chan int, 4), stderr: &syncBuffer{}}
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	f.processExit = &processExit{
		budget: budget, grace: grace, stderr: f.stderr, exit: func(code int) { f.codes <- code },
		cleanup: func(ctx context.Context) error {
			f.runs.Add(1)
			close(f.started)
			if hang {
				<-f.release // ignores ctx
				return nil
			}
			select {
			case <-f.release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	return f
}

// noExitWhile fails the test when the process exits within d.
func (f *fakeExit) noExitWhile(t *testing.T, d time.Duration, what string) {
	t.Helper()
	select {
	case code := <-f.codes:
		t.Fatalf("exit %d %s", code, what)
	case <-time.After(d):
	}
}

// exitCode returns the one exit code and fails on a second exit.
func (f *fakeExit) exitCode(t *testing.T) int {
	t.Helper()
	var code int
	select {
	case code = <-f.codes:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit")
	}
	select {
	case again := <-f.codes:
		t.Fatalf("second exit %d after exit %d", again, code)
	case <-time.After(50 * time.Millisecond):
	}
	return code
}

// The deadline expires and no cleanup started (a command that ignores its
// context): the watchdog starts the cleanup, waits for it, then exits 124
// with the deadline error.
func TestWatchdogStartsTheCleanupAndExitsAfterIt(t *testing.T) {
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	armWatchdog(10*time.Millisecond, f.processExit)
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog did not start the cleanup")
	}
	f.noExitWhile(t, 200*time.Millisecond, "while the cleanup runs")
	close(f.release)
	if code := f.exitCode(t); code != exitTimeout {
		t.Errorf("exit %d after the watchdog's cleanup, want %d", code, exitTimeout)
	}
	if s := f.stderr.String(); !strings.Contains(s, "iwdp-cli eval timed out") || !strings.Contains(s, "watchdog") {
		t.Errorf("stderr %q lacks the deadline error", s)
	}
	if n := f.runs.Load(); n != 1 {
		t.Errorf("cleanup ran %d times, want 1", n)
	}
}

// main started the cleanup, and the watchdog fires during it: the watchdog
// waits, and the exit after the cleanup is 124 (the deadline expired),
// whichever path exits.
func TestWatchdogWaitsForMainsCleanup(t *testing.T) {
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	go f.finish(func() int { return 0 })
	<-f.started
	armWatchdog(time.Millisecond, f.processExit)
	f.noExitWhile(t, 200*time.Millisecond, "while main's cleanup runs")
	close(f.release)
	if code := f.exitCode(t); code != exitTimeout {
		t.Errorf("exit %d, want %d", code, exitTimeout)
	}
	if n := f.runs.Load(); n != 1 {
		t.Errorf("cleanup ran %d times, want 1", n)
	}
}

// A signal during the watchdog's cleanup: the exit waits for the cleanup
// and is 130.
func TestSignalDuringTheWatchdogsCleanupExitsInterrupted(t *testing.T) {
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	armWatchdog(time.Millisecond, f.processExit)
	<-f.started
	go f.signalled()
	f.noExitWhile(t, 200*time.Millisecond, "while the watchdog's cleanup runs")
	close(f.release)
	if code := f.exitCode(t); code != exitInterrupted {
		t.Errorf("exit %d, want %d", code, exitInterrupted)
	}
	if s := f.stderr.String(); strings.Contains(s, "timed out") {
		t.Errorf("stderr %q: a signal's exit reports a deadline error", s)
	}
}

// Without a signal or the watchdog, the exit code is the path's own, after
// the cleanup.
func TestFinishExitsWithItsCodeAfterTheCleanup(t *testing.T) {
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	go f.finish(func() int { return 4 })
	<-f.started
	f.noExitWhile(t, 100*time.Millisecond, "while the cleanup runs")
	close(f.release)
	if code := f.exitCode(t); code != 4 {
		t.Errorf("exit %d, want 4", code)
	}
}

// A cleanup that fails is reported as a warning; the exit still waits for
// it and keeps the path's own code.
func TestFailedCleanupIsAWarning(t *testing.T) {
	f := newFakeExit(t, 5*time.Second, time.Second, false)
	f.cleanup = func(context.Context) error { return errors.New("Console.disable: not sent") }
	go f.finish(func() int { return 0 })
	if code := f.exitCode(t); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if s := f.stderr.String(); !strings.Contains(s, "warning: cleanup of the page state: Console.disable: not sent") {
		t.Errorf("stderr %q lacks the cleanup warning", s)
	}
}

// A cleanup that ignores its context is given up budget+grace after it
// started; the exit code still states what happened, and stderr says that
// the cleanup did not end.
func TestHangingCleanupIsBounded(t *testing.T) {
	const budget, grace = 100 * time.Millisecond, 100 * time.Millisecond
	for name, tc := range map[string]struct {
		end  func(f *fakeExit)
		want int
	}{
		"watchdog": {func(f *fakeExit) { armWatchdog(time.Millisecond, f.processExit) }, exitTimeout},
		"signal":   {func(f *fakeExit) { go f.signalled() }, exitInterrupted},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeExit(t, budget, grace, true)
			start := time.Now()
			tc.end(f)
			if code := f.exitCode(t); code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
			if elapsed := time.Since(start); elapsed < budget+grace {
				t.Errorf("exit after %v, before the bounded wait of %v", elapsed, budget+grace)
			}
			if s := f.stderr.String(); !strings.Contains(s, "did not end within 200ms") {
				t.Errorf("stderr %q does not say that the cleanup did not end", s)
			}
		})
	}
}
