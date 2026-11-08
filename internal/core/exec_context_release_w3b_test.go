package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W3b-R2 finding 1 (design 2.4, row "Page-side runtime state"; 3.7:
// "Release restores the original functions"): the release of a lease runs
// the page-side runtime release in the main context and in every frame
// context that operations of the lease selected. A context that no longer
// exists counts as released; a context whose release failed stays
// recorded for the next owner.
func TestLeaseReleaseRunsThePageRuntimeReleaseInEveryContext(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	var mu sync.Mutex
	var released []string // "<context>:<epoch>", main is 0
	r.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string
			ContextID  int64 `json:"contextId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression)
		if m == nil || m[1] != "release" {
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": true}}}, nil
		}
		switch p.ContextID {
		case 9:
			return nil, &fakewebkit.Error{Code: -32000, Message: "Missing injected script for given executionContextId"}
		case 11:
			return nil, &fakewebkit.Error{Code: -32000, Message: "the frame is busy"}
		}
		var a struct{ Epoch int64 }
		_ = json.Unmarshal([]byte(m[2]), &a)
		mu.Lock()
		released = append(released, fmtCtx(p.ContextID, a.Epoch))
		mu.Unlock()
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": true}}}, nil
	})
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "frame", ClassMutate)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, id := range []int64{11, 7, 9} {
		mo.Op.SetExecutionContext(boundID(mo.Page.PS.Sched, id))
	}
	mo.Op.SetExecutionContext(ExecContext{})
	r.m.End(mo.ID)
	if err := r.m.Release(ctxFor(t, 5*time.Second), agent.Session, mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), released...)
	mu.Unlock()
	if want := []string{fmtCtx(0, int64(mo.Epoch)), fmtCtx(7, int64(mo.Epoch))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("release calls %q, want %q (main, then frame context 7; 9 is gone, 11 failed)", got, want)
	}
	s := mo.Page.PS.Sched
	if e := s.ContextEpochsBefore(mo.Epoch + 1); !reflect.DeepEqual(e, []uint64{mo.Epoch}) {
		t.Fatalf("epochs with contexts left = %v, want [%d] (context 11 failed)", e, mo.Epoch)
	}
	// The failed context is tried again by the next release of the epoch,
	// and forgotten once it is released.
	var ids []int64
	if err := s.ReleaseExecutionContexts(ctxFor(t, time.Second), mo.Epoch, nopCaller{}, func(_ context.Context, c webkit.Caller) error {
		ids = append(ids, c.(contextCaller).ec.ID)
		return nil
	}); err != nil || !reflect.DeepEqual(ids, []int64{11}) {
		t.Fatalf("second release: contexts %v, err %v; want [11]", ids, err)
	}
	if e := s.ContextEpochsBefore(mo.Epoch + 1); len(e) != 0 {
		t.Fatalf("epochs with contexts left after the second release = %v", e)
	}
}

func fmtCtx(id, epoch int64) string {
	b, _ := json.Marshal([]int64{id, epoch})
	return string(b)
}

// W3b-R2 finding 3 (design 2.10): a cleanup registered with Finally runs
// when the operation ends and before a queued successor is admitted, with
// a caller fenced by the operation's lease epoch, not by its ended
// context; Finally after End is refused.
func TestOpFinallyRunsBeforeTheSuccessor(t *testing.T) {
	c := &paramsCaller{}
	s := NewScheduler(c, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease("a", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.Begin(ctx, OpSpec{Name: "shot", Class: ClassMutate, Session: "a", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	var omu sync.Mutex
	note := func(e string) { omu.Lock(); order = append(order, e); omu.Unlock() }
	op.Cancel(ErrDeadline) // the operation's own caller sends nothing more
	if _, err := op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": "x"}); err == nil {
		t.Fatal("the canceled operation's caller sent")
	}
	release := make(chan struct{})
	if err := op.Finally(time.Second, func(ctx context.Context, fc webkit.Caller) {
		<-release
		_, err := fc.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "clear"})
		note("cleanup:" + errString(err))
	}); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan *Op)
	go func() {
		next, err := s.Begin(ctx, OpSpec{Name: "next", Class: ClassRead, Session: "a", Epoch: 1})
		if err != nil {
			t.Error(err)
		}
		note("successor")
		admitted <- next
	}()
	op.End()
	select {
	case <-admitted:
		t.Fatal("the successor was admitted before the cleanup ran")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	next := <-admitted
	next.End()
	if want := []string{"cleanup:", "successor"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %q, want %q", order, want)
	}
	if !errors.Is(op.Finally(time.Second, func(context.Context, webkit.Caller) {}), ErrOpEnded) {
		t.Fatal("Finally after End was accepted")
	}

	// Another lease epoch: the cleanup sends nothing.
	op2, err := s.Begin(ctx, OpSpec{Name: "shot", Class: ClassMutate, Session: "a", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	_ = op2.Finally(time.Second, func(ctx context.Context, fc webkit.Caller) {
		_, err := fc.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "clear"})
		got <- err
	})
	c.mu.Lock()
	sent := len(c.sent)
	c.mu.Unlock()
	s.SetLease("b", 2)
	op2.End()
	if err := <-got; !errors.Is(err, ErrStaleEpoch) || !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("cleanup under another epoch: %v, want not sent with ErrStaleEpoch", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) != sent {
		t.Fatalf("the cleanup under another epoch sent %v", c.sent[sent:])
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// capacityWaitContext reports, through reached, that leaseCaller.Send
// evaluated Done in its wait for protocol capacity: the send passed the
// lease check before the wait. With hideDone, Done never fires, so the
// wait ends only with the permit; a select whose two cases are both ready
// picks either one, and this fixes the order that the check after the
// wait must handle.
type capacityWaitContext struct {
	context.Context
	reached  chan struct{}
	once     *sync.Once
	hideDone bool
}

func (c capacityWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	if c.hideDone {
		return nil
	}
	return c.Context.Done()
}

// W3b-R3 finding 2, from the navigation review's reproduction
// TestReviewFinallyRechecksEpochAfterCapacityWait (design 2.10,
// Scheduler.SetLease: running operations of an old lease send nothing
// more): a Finally cleanup that waits for protocol capacity checks the
// lease again, and its own budget, after the wait and before the send.
// The lease moves to another epoch, ends, or the cleanup's budget ends
// while the cleanup waits; nothing reaches the page.
func TestOpFinallyRechecksTheLeaseAfterTheCapacityWait(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hideDone bool
		change   func(s *Scheduler, cancel context.CancelFunc)
		want     error
	}{
		{"epoch", false, func(s *Scheduler, _ context.CancelFunc) { s.SetLease("b", 2) }, ErrStaleEpoch},
		{"release", false, func(s *Scheduler, _ context.CancelFunc) {
			s.mu.Lock()
			s.released = true
			s.mu.Unlock()
		}, ErrLeaseEnded},
		{"budget", true, func(_ *Scheduler, cancel context.CancelFunc) { cancel() }, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &paramsCaller{}
			s := NewScheduler(c, SchedulerOptions{MaxInFlight: 1})
			t.Cleanup(s.Close)
			s.SetLease("a", 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			op, err := s.Begin(ctx, OpSpec{Name: "capture", Class: ClassMutate, Session: "a", Epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			s.sem <- struct{}{} // all protocol capacity is in use
			reached := make(chan struct{})
			result := make(chan error, 1)
			var cleanupCancel context.CancelFunc
			if err := op.Finally(time.Second, func(ctx context.Context, fc webkit.Caller) {
				ctx, cancel := context.WithCancel(ctx)
				cleanupCancel = cancel
				defer cancel()
				wait := capacityWaitContext{Context: ctx, reached: reached, once: &sync.Once{}, hideDone: tc.hideDone}
				_, err := fc.Send(wait, "Runtime.evaluate", map[string]any{"expression": "clear"})
				result <- err
			}); err != nil {
				t.Fatal(err)
			}
			op.End()
			<-reached // the send passed the lease check and waits for capacity
			tc.change(s, cleanupCancel)
			<-s.sem
			err = <-result
			if !errors.Is(err, tc.want) || !errors.Is(err, webkit.ErrNotSent) {
				t.Fatalf("cleanup send after the change during the capacity wait: %v; want not sent with %v", err, tc.want)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.sent) != 0 {
				t.Fatalf("the cleanup was sent: %+v", c.sent)
			}
		})
	}
}

// W3b-R3 finding 3 (design 2.4, 3.8; background recovery, design 2.6 step
// 3): a frame-scoped lease survives a reconnect of its page. The new
// connection reports the frame's context under a new id while the
// recovery re-applies the desired state (the Runtime.enable of the frame
// owner), before it replaces the old connection. The inventory then names
// the frame's new context, and the lease release reaches it there, never
// the old id, which the new connection does not know. When the frame
// reports no context on the new connection, the inventory names none (the
// frame command reports the frame as gone), and the release does not call
// the old id: the frame reported nothing while Runtime was enabled, so it
// is gone and counts as released (W3b-R4, PageSession.ReleaseFrameContexts).
func TestFrameContextsAcrossARecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		newID int64 // the frame's context on the second connection; 0: none
	}{
		{"reported", 17},
		{"gone", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
			r.reserve(t, "agent")
			var mu sync.Mutex
			var released []string // "[context,epoch]", main is 0
			r.f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				id := int64(7)
				if c.Conn != 1 {
					id = tc.newID
				}
				if id != 0 {
					r.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "type": "normal", "frameId": "kid"}})
				}
				return map[string]any{}, nil
			})
			r.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				var p struct {
					Expression string
					ContextID  int64 `json:"contextId"`
				}
				_ = json.Unmarshal(c.Params, &p)
				if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil && m[1] == "release" {
					var a struct{ Epoch int64 }
					_ = json.Unmarshal([]byte(m[2]), &a)
					mu.Lock()
					released = append(released, fmtCtx(p.ContextID, a.Epoch))
					mu.Unlock()
				}
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": true}}}, nil
			})
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "frame", ClassMutate)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			ps, pg := mo.Page.PS, mo.Page
			ps.State.Acquire(SessionOwner("frame", agent.Session), "Runtime")
			if _, err := mo.Op.Caller().Send(ctx, "Runtime.enable", nil); err != nil {
				t.Fatalf("Runtime.enable: %v", err)
			}
			if id := ps.Sched.FrameContext("kid").ID; id != 7 {
				t.Fatalf("frame context on the first connection = %d, want 7", id)
			}
			mo.Op.SetExecutionContext(ps.Sched.FrameContext("kid"))
			r.m.End(mo.ID)

			// The first connection stops answering; the recovery reconnects.
			r.f.StallConnection(1)
			mo2, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctxFor(t, time.Second), mo2.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err == nil {
				t.Fatal("Raw on the stalled connection succeeded")
			}
			r.m.End(mo2.ID)
			eventually(t, 15*time.Second, "the recovery to reconnect", func() bool {
				return r.f.ConnectionCount() == 2 && pg.ConnState() == ConnOK
			})
			if n := len(r.f.ReceivedMethod("Runtime.enable")); n != 2 {
				t.Fatalf("Runtime.enable sent %d times, want once per connection", n)
			}
			if id := ps.Sched.FrameContext("kid").ID; id != tc.newID {
				t.Fatalf("frame context after the reconnect = %d, want %d (the second connection's report)", id, tc.newID)
			}

			if err := r.m.Release(ctx, agent.Session, pg.Key); err != nil {
				t.Fatalf("Release: %v", err)
			}
			mu.Lock()
			got := append([]string(nil), released...)
			mu.Unlock()
			want := []string{fmtCtx(0, int64(mo.Epoch))}
			if tc.newID != 0 {
				want = append(want, fmtCtx(tc.newID, int64(mo.Epoch)))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("release calls %q, want %q (never the old context 7)", got, want)
			}
			if left := ps.Sched.ContextEpochsBefore(mo.Epoch + 1); len(left) != 0 {
				t.Fatalf("epochs with recorded contexts after the release = %v, want none", left)
			}
		})
	}
}
