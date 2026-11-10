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
// was never reset. Every exit path (main, the signal handler and the
// watchdog) waits for the one cleanup; a signal makes the exit code 130, an
// expired deadline 124 (processExit in daemon_cli.go). Since WS2 round-1
// review S1 F2, the command's deadline bounds that cleanup (processExit.window):
// a cleanup still running at the deadline plus deadlineGrace is cut off.

// cleanupPastDeadline holds a page reply longer than shortDeadline plus
// watchdogGrace, and less than daemon.ShutdownBudget. A reply of the
// cleanup held this long is past the cleanup's budget, which the
// command's deadline bounds (processExit.window).
const cleanupPastDeadline = 3500 * time.Millisecond

// shortDeadline is the --timeout of two tests below. In
// TestCLINoDaemonDeadlineCutsAHeldCleanup, the command must connect and
// enable its domains before it, or no state exists for the cleanup to
// reset; 500 ms was too short on a loaded host. In
// TestCLINoDaemonTimedOutCommandStillSendsItsCleanup, the idle command
// waits for its deadline, so its cleanup starts at shortDeadline. In both,
// the page holds a reply of the cleanup for cleanupPastDeadline, so the
// deadline cuts the cleanup off.
const shortDeadline = "1s"

// startHeldCleanup starts setRegistryCommand under IWDP_NO_DAEMON=1 with
// --timeout timeout and waits until main's cleanup sent Console.disable,
// whose reply the page holds for hold; Debugger.disable comes after it.
func startHeldCleanup(t *testing.T, timeout string, hold time.Duration) (*fakewebkit.Server, *runningCLI) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.DelayReplies("Console.disable", hold)
	p := startCLI(t, []string{"IWDP_NO_DAEMON=1"}, append(strings.Fields(setRegistryCommand), "--page", f.URL, "--timeout", timeout)...)
	eventually(t, cleanupLimit, "main's cleanup to disable Console", func() bool {
		return len(f.ReceivedMethod("Console.disable")) == 1
	})
	return f, p
}

// signalDeadline and signalHold: a deadline that leaves main's cleanup
// time for the held Console.disable, so that the signal arrives while the
// cleanup runs within its budget.
const (
	signalDeadline = "10s"
	signalHold     = 1500 * time.Millisecond
)

// Reviewer's counterexample: SIGINT during main's cleanup. The exit path of
// the signal must wait for the running cleanup, which then sends
// Debugger.disable, and the signal makes the exit code 130. (Before WS2
// round-1 review S1 F2 the cleanup ran past the deadline; now the deadline
// bounds it, so the cleanup here fits in a longer deadline.)
func TestCLINoDaemonSignalDuringAHeldCleanupWaitsForIt(t *testing.T) {
	f, p := startHeldCleanup(t, signalDeadline, signalHold)
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

// Deadline only: a held cleanup past the deadline without a signal. WS2
// round-1 review S1 F2: the deadline bounds the cleanup, so the process
// does not wait for the held Console.disable; it exits with the command's
// own status (0: the command succeeded) and a warning that names the reset
// that did not finish, before the watchdog, and sends no reset after the
// cut. The bound itself is checked by TestNoDaemonCleanupStaysWithinTheDeadline.
func TestCLINoDaemonDeadlineCutsAHeldCleanup(t *testing.T) {
	f, p := startHeldCleanup(t, shortDeadline, cleanupPastDeadline)
	if code := p.wait(t, cleanupLimit); code != 0 {
		t.Errorf("exit %d after the deadline cut the cleanup, want the command's own 0; stderr %q", code, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Debugger.disable")); n != 0 {
		t.Errorf("Debugger.disable sent %d times, want 0: no reset after the cleanup's budget; stderr %q", n, p.stderr.String())
	}
	if s := p.stderr.String(); !strings.Contains(s, "warning: cleanup of the page state") || !strings.Contains(s, "Console.disable") || strings.Contains(s, errWatchdog.Error()) {
		t.Errorf("stderr %q: want the warning that names Console.disable, and no watchdog error", s)
	}
}

// heldRegistryCommand is a registry command for the tests below: it
// acquires Console for its session's owner, enables it and then evaluates,
// as an ordinary registry command that sets page state does.
// idleRegistryCommand enables Console the same way, prints idleLine and
// waits for its deadline without a call in flight. setRegistryCommand enables Console
// and Debugger and succeeds; the cleanup disables them in that order
// (sorted by domain). failRegistryCommand enables Console and fails.
const (
	heldRegistryCommand = "test hold console"
	idleRegistryCommand = "test idle console"
	setRegistryCommand  = "test set console debugger"
	failRegistryCommand = "test fail console"
)

// idleLine is what idleRegistryCommand prints when it starts to wait.
const idleLine = "idle after Console.enable"

// errFixtureFailure is the error of failRegistryCommand.
var errFixtureFailure = errors.New("test fixture failure after Console.enable")

func init() {
	for _, name := range []string{heldRegistryCommand, idleRegistryCommand, setRegistryCommand, failRegistryCommand} {
		reg.Register(&reg.Command{
			Name: name, Summary: "Test fixture: enable Console, then evaluate, wait, enable Debugger or fail.", Page: reg.AnyPage, Mutates: true,
			Run: func(ctx context.Context, env reg.Env, in reg.Input) (reg.Result, error) {
				ps, err := env.Page(ctx)
				if err != nil {
					return reg.Result{}, err
				}
				owner := core.SessionOwner("test", in.Global.Session)
				ps.State.Acquire(owner, "Console")
				c := env.(interface{ Op() *core.Op }).Op().Caller()
				if _, err := c.Send(ctx, "Console.enable", nil); err != nil {
					return reg.Result{}, err
				}
				switch name {
				case idleRegistryCommand:
					env.Emit(idleLine)
					<-ctx.Done()
					return reg.Result{}, ctx.Err()
				case setRegistryCommand:
					ps.State.Acquire(owner, "Debugger")
					_, err = c.Send(ctx, "Debugger.enable", nil)
					return reg.Result{Text: "set"}, err
				case failRegistryCommand:
					return reg.Result{}, errFixtureFailure
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
// stayed enabled. They get the signal cleanup: exit 130 after
// Console.disable.
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

// A registry command of IWDP_NO_DAEMON=1 that ends at its deadline,
// shortDeadline (exit 124 from the command; no call is in flight then, so
// no liveness probe can mark the page stalled before the cleanup), still
// gets its cleanup: main's cleanup sends Console.disable within the first
// part of deadlineGrace. WS2 round-1 review S1 F2: the page holds the
// reply past that budget, so the cleanup is cut off and the process exits
// 124 with the command's deadline error and a warning that names
// Console.disable, before the watchdog.
func TestCLINoDaemonTimedOutCommandStillSendsItsCleanup(t *testing.T) {
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
	if s := p.stderr.String(); strings.Contains(s, errWatchdog.Error()) || !strings.Contains(s, "iwdp-cli "+idleRegistryCommand+" timed out") || !strings.Contains(s, "Console.disable") {
		t.Errorf("stderr %q: want the command's deadline error and the warning that names Console.disable, without the watchdog", s)
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
