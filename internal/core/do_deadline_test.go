package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// timerNotFired is a caller's context at the instant when its deadline
// has passed but its own deadline timer has not run yet: Deadline reports
// dl, and Done never closes. The scheduler's expiry of the operation
// (Op.watch) and the timer of the caller's context fire at the same
// instant in either order; this context fixes the order in which the
// operation's expiry runs first, without load.
type timerNotFired struct {
	context.Context
	dl time.Time
}

func (c timerNotFired) Deadline() (time.Time, bool) { return c.dl, true }

// opDeadline is the deadline of the operation in the tests below. It only
// has to cover the admission of an operation on an attached page.
const opDeadline = 500 * time.Millisecond

// A function that returns its context's error when the operation's own
// deadline ended it fails as a deadline (exit 124, decision D9), also when
// the caller's deadline timer has not fired: the scheduler cancels the
// operation with the cause ErrDeadline, so ctx.Err() is context.Canceled.
// Regression of the CLI test TestCLINoDaemonTimedOutCommandStillSendsItsCleanup,
// which exited 1 with "context canceled" on a loaded host.
func TestDoReportsTheOperationDeadlineAsADeadline(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := r.m.Do(ctxFor(t, 8*time.Second), r.req, agent, "attach", ClassRead, false, func(context.Context, *ManagedOp) error { return nil }); err != nil {
		t.Fatalf("attach: %v", err)
	}
	ctx := timerNotFired{Context: context.Background(), dl: time.Now().Add(opDeadline)}
	err := r.m.Do(ctx, r.req, agent, "wait", ClassRead, false, func(ctx context.Context, _ *ManagedOp) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrDeadline) {
		t.Fatalf("Do = %v, want a deadline error (context.DeadlineExceeded and ErrDeadline) after the operation's deadline", err)
	}
}

// A cancellation of the caller's context (a signal, a client that went
// away) stays a cancellation: only the operation's deadline makes a
// deadline error.
func TestDoKeepsACancellationACancellation(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx, cancel := context.WithCancel(ctxFor(t, 8*time.Second))
	defer cancel()
	err := r.m.Do(ctx, r.req, agent, "wait", ClassRead, false, func(ctx context.Context, _ *ManagedOp) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do = %v, want context.Canceled and no deadline error after the caller canceled", err)
	}
}
