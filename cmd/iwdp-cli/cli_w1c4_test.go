package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	reg "github.com/nnemirovsky/iwdp-cli/internal/commands"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// L01 (w1c backlog): the element screenshot, which reads the document
// root, exits 1 with an error that names DOM.getDocument when its reply
// has no "root", instead of a Go panic or a "null" result.
func TestCLIDocumentWithoutRootIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"screenshot", "--selector", "#card", "-o", "card.png"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			mock := testutil.NewMockServer()
			defer mock.Close()
			mock.HandleFunc("DOM.getDocument", map[string]any{})
			res := runCLI(t, "", nil, append(args, "--page", mock.URL)...)
			if res.code != 1 || !strings.Contains(res.stderr, "Error: ") || !strings.Contains(res.stderr, "DOM.getDocument") {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 and an error naming DOM.getDocument", res.code, res.stdout, res.stderr)
			}
			if strings.Contains(res.stderr, "panic") || strings.Contains(res.stderr, "goroutine ") {
				t.Fatalf("the command panicked: %s", res.stderr)
			}
			if res.stdout != "" {
				t.Fatalf("stdout %q, want nothing", res.stdout)
			}
		})
	}
}

// L03 (w1c backlog, decision L03): a command that selects its page by
// discovery prints no "Connecting to: <title>" line; the page title never
// reaches stderr, and the result is unchanged.
func TestCLIDiscoveredPagePrintsNoTitle(t *testing.T) {
	env, _, _, pg := physicalPage(t, false, fakewebkit.Options{})
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	res := runCLI(t, "", env, "eval", "1+1")
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("eval: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if strings.Contains(res.stderr, "Connecting to") || strings.Contains(res.stderr, pg.Title) {
		t.Fatalf("stderr %q names the page title %q", res.stderr, pg.Title)
	}
}

// L05 (w1c backlog): a registry command of IWDP_NO_DAEMON=1 ended by
// SIGINT exits 130 after its cleanup and prints no "Error: <command>: ..."
// line for the call that the signal cancelled.
func TestCLIRegistrySignalPrintsNoCancelError(t *testing.T) {
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
		t.Fatalf("exit %d after SIGINT, want %d; stderr %q", code, exitInterrupted, p.stderr.String())
	}
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Errorf("Console.disable sent %d times before the exit, want 1", n)
	}
	if s := p.stderr.String(); strings.Contains(s, "Error:") {
		t.Fatalf("stderr %q has an error line for the signal's own cancellation", s)
	}
}

// L05: the signal handler cancels the running registry command's context
// with commands.ErrInterrupted before the cleanup starts, then exits 130.
func TestSignalCancelsTheRegistryCommandWithErrInterrupted(t *testing.T) {
	f := newFakeExit(t, time.Second, time.Second, false)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.cancelRun = cancel
	go f.signalled()
	<-f.started
	if cause := context.Cause(ctx); !errors.Is(cause, reg.ErrInterrupted) {
		t.Fatalf("cause when the cleanup started: %v, want commands.ErrInterrupted", cause)
	}
	close(f.release)
	if code := f.exitCode(t); code != exitInterrupted {
		t.Fatalf("exit %d, want %d", code, exitInterrupted)
	}
}

// L05: runRegistry runs the command under the context that exits.cancelRun
// cancels, so a signal's cancellation reaches the command.
func TestRunRegistryCommandContextIsTheSignalsContext(t *testing.T) {
	var cause error
	c := &reg.Command{
		Name: "test w1c4 signal context", Summary: "Test fixture: cancel as the signal handler does.", Local: true,
		Run: func(ctx context.Context, _ reg.Env, _ reg.Input) (reg.Result, error) {
			exits.cancelRun(reg.ErrInterrupted)
			cause = context.Cause(ctx)
			return reg.Result{}, ctx.Err()
		},
	}
	reg.Register(c)
	saved := exits.cancelRun
	t.Cleanup(func() { reg.Unregister(c); exits.cancelRun = saved })
	if code := runRegistry(strings.Fields(c.Name)); code != 1 {
		t.Fatalf("exit %d, want 1 for the canceled command (main exits 130 for the signal)", code)
	}
	if !errors.Is(cause, reg.ErrInterrupted) {
		t.Fatalf("the command's context cause %v, want commands.ErrInterrupted", cause)
	}
}
