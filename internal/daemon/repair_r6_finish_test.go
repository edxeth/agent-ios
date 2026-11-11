package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// deadlineFixture registers a registry command that needs a page and runs
// call with the operation's caller.
func deadlineFixture(t *testing.T, name string, call func(ctx context.Context, c webkit.Caller) error) {
	t.Helper()
	cmd := &commands.Command{
		Name: name, Summary: "Deadline fixture.", Page: commands.AnyPage,
		Run: func(ctx context.Context, env commands.Env, _ commands.Input) (commands.Result, error) {
			op := env.(interface{ Op() *core.Op }).Op()
			return commands.Result{}, call(ctx, op.Caller())
		},
	}
	commands.Register(cmd)
	t.Cleanup(func() { commands.Unregister(cmd) })
}

// Decision D9 (exit 124 for every expired command deadline) and design
// 2.6: a command whose own deadline expires in the daemon fails as a
// deadline (exit 124, errors.Is context.DeadlineExceeded), whatever kind of
// command it is and whichever timer fires first: the request context's or
// the operation's (core.Scheduler cancels the operation with the cause
// ErrDeadline, and the WebKit client saw only context.Canceled). The
// client's own context outlives the command deadline here, so only the
// daemon's handling decides. A cancellation by the client is no deadline.
func TestEveryExpiredCommandDeadlineIsATimeout(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	const deadline = 300 * time.Millisecond
	deadlineFixture(t, "fixture deadline call", func(ctx context.Context, c webkit.Caller) error {
		_, err := c.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"})
		return err
	})
	deadlineFixture(t, "fixture deadline stream", func(ctx context.Context, c webkit.Caller) error {
		unsub := c.Subscribe("Console.messageAdded", func(string, json.RawMessage) {})
		defer unsub()
		_, err := c.Send(ctx, "Console.enable", nil)
		return err
	})
	deadlineFixture(t, "fixture deadline reply", func(ctx context.Context, c webkit.Caller) error {
		// Only the operation's deadline ends this wait for the reply.
		_, err := c.Send(context.WithoutCancel(ctx), "Runtime.evaluate", map[string]string{"expression": "2"})
		return err
	})
	for _, tc := range []struct {
		name    string
		run     func(ctx context.Context, page string) error
		timeout bool
	}{
		{"registry call", func(ctx context.Context, page string) error {
			_, err := c.Run(ctx, RunRequest{Argv: []string{"fixture", "deadline", "call"}, Deadline: time.Now().Add(deadline), PageReq: PageReq{Page: page}, Owner: Owner{Session: "registry"}}, nil)
			return err
		}, true},
		{"stream setup", func(ctx context.Context, page string) error {
			_, err := c.Run(ctx, RunRequest{Argv: []string{"fixture", "deadline", "stream"}, Deadline: time.Now().Add(deadline), PageReq: PageReq{Page: page}, Owner: Owner{Session: "stream"}}, nil)
			return err
		}, true},
		{"call waiting for its reply", func(ctx context.Context, page string) error {
			_, err := c.Run(ctx, RunRequest{Argv: []string{"fixture", "deadline", "reply"}, Deadline: time.Now().Add(deadline), PageReq: PageReq{Page: page}, Owner: Owner{Session: "reply"}}, nil)
			return err
		}, true},
		{"client cancellation", func(ctx context.Context, page string) error {
			ctx, cancel := context.WithCancel(ctx)
			time.AfterFunc(deadline, cancel)
			_, err := c.Run(ctx, RunRequest{Argv: []string{"fixture", "deadline", "call"}, Deadline: time.Now().Add(5 * time.Second), PageReq: PageReq{Page: page}, Owner: Owner{Session: "cancel"}}, nil)
			return err
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, fakewebkit.Options{})
			f.Stall("Runtime.evaluate")
			f.Stall("Console.enable")
			start := time.Now()
			err := tc.run(ctxFor(t, 5*time.Second), f.URL)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("the stalled call succeeded")
			}
			code, isDeadline := commands.ExitCodeOf(err), errors.Is(err, context.DeadlineExceeded)
			if tc.timeout && (code != commands.ExitTimeout || !isDeadline) {
				t.Fatalf("exit %d (deadline %v) after %v: %v; want exit %d and a deadline error", code, isDeadline, elapsed, err, commands.ExitTimeout)
			}
			if !tc.timeout && (code == commands.ExitTimeout || isDeadline) {
				t.Fatalf("exit %d (deadline %v) after a client cancellation: %v; want no deadline error", code, isDeadline, err)
			}
			if elapsed > 3*time.Second {
				t.Errorf("the command ended after %v with a %v deadline", elapsed, deadline)
			}
		})
	}
}
