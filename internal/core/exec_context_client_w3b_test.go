package core

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W3b final repair round 4 (the navigation reviewer's reproduction
// TestReviewClosedClientCannotCountAsReachedCleanup; orchestrator rule: a
// frame record is forgotten only after an attempt that reached the
// current target): an owner-change cleanup through a page given by an
// explicit WebSocket URL (webkit.Client) counts as reached only when the
// Client wrote its message. A Client closed before the write keeps the
// record; a Client closed after the write and before the reply is an
// attempt that got no answer, so the record is forgotten.

// closingClient closes its webkit.Client at the next Send, exactly after
// pageConn picked the connection and before Client.Send runs (before), or
// the page closes it when the call arrives, before it answers (after the
// write, see newClientCleanupRig).
type closingClient struct {
	*webkit.Client
	closeNext atomic.Bool
}

func (c *closingClient) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closeNext.Swap(false) {
		_ = c.Close()
	}
	return c.Client.Send(ctx, method, params)
}

// clientCleanupRig is a scheduler on a pageConn over a real webkit.Client
// whose page reported the frame "kid" in context 17, recorded for lease
// epoch 1, and an operation of epoch 2 (the next owner).
type clientCleanupRig struct {
	f  *fakewebkit.Server
	c  *closingClient
	s  *Scheduler
	op *Op
}

func newClientCleanupRig(t *testing.T, closeOnEvaluate bool) *clientCleanupRig {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	r := &clientCleanupRig{f: f}
	f.Handle("Runtime.enable", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		f.EmitFrom(call.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 17, "frameId": "kid", "type": "normal"}})
		return map[string]any{}, nil
	})
	if closeOnEvaluate {
		// The page receives the release and the connection closes before
		// the reply.
		f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			_ = r.c.Close()
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		})
	}
	ctx := ctxFor(t, 5*time.Second)
	raw, err := webkit.NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	r.c = &closingClient{Client: raw}
	if _, err := r.c.Send(ctx, "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	pc := newPageConn(r.c, func() {}, func(func(context.Context)) bool { return false }, t.Logf)
	r.s = NewScheduler(pc, SchedulerOptions{})
	t.Cleanup(r.s.Close)
	r.s.SetLease("s1", 2)
	ec := r.s.FrameContext("kid")
	if ec.ID != 17 {
		t.Fatalf("frame context = %d, want 17", ec.ID)
	}
	r.s.installContext(1, ec)
	r.op, err = r.s.Begin(ctx, OpSpec{Session: "s1", Epoch: 2, Name: "get", Class: ClassRead})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.op.End)
	return r
}

// cleanup runs the owner-change cleanup of epoch 1 as commands
// releaseEarlierFrames does: the release, then
// ForgetFailedExecutionContexts after a failure.
func (r *clientCleanupRig) cleanup(t *testing.T) error {
	t.Helper()
	err := r.s.ReleaseExecutionContexts(ctxFor(t, time.Second), 1, r.op.Caller(), func(ctx context.Context, c webkit.Caller) error {
		_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
		return err
	})
	if err == nil {
		t.Fatal("the release through a closed Client succeeded")
	}
	r.s.ForgetFailedExecutionContexts(1)
	return err
}

func TestClientClosedBeforeTheWriteKeepsTheFrameRecord(t *testing.T) {
	r := newClientCleanupRig(t, false)
	r.c.closeNext.Store(true)
	err := r.cleanup(t)
	if n := len(r.f.ReceivedMethod("Runtime.evaluate")); n != 0 {
		t.Fatalf("the page received %d releases, want 0", n)
	}
	if left := r.s.ContextEpochsBefore(2); len(left) != 1 {
		t.Fatalf("the Client closed before the write (err %v); epochs left %v, want [1]", err, left)
	}
}

func TestClientClosedAfterTheWriteForgetsTheFrameRecord(t *testing.T) {
	r := newClientCleanupRig(t, true)
	err := r.cleanup(t)
	if n := len(r.f.ReceivedMethod("Runtime.evaluate")); n != 1 {
		t.Fatalf("the page received %d releases, want 1", n)
	}
	if left := r.s.ContextEpochsBefore(2); len(left) != 0 {
		t.Fatalf("the Client closed after the write (err %v); epochs left %v, want none (one attempt)", err, left)
	}
}
