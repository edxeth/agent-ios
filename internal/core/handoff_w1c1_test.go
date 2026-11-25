package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// Helpers of attach_recovery_w1c1_test.go that use the W1c-1 internals,
// and unit tests of the event handoff of a new connection (pageConn.prepare,
// swap, discard).

// holdCommitReset holds the background reset that a commit with a pending
// cleanup starts (Manager.commitLeftCleanup) until its budget or the
// manager's Close ends it. The tests that pin the mark of such a commit and
// its end by the next change use it; the reset itself is pinned by
// TestCommitLeavingCleanupResetsInTheBackground.
func holdCommitReset(r *rig) {
	r.m.mu.Lock()
	r.m.commitResetHold = func(ctx context.Context) { <-ctx.Done() }
	r.m.mu.Unlock()
}

// allowCommitReset lets a commit with a pending cleanup start its
// background reset again (queuedReleaseCleanup holds it).
func allowCommitReset(r *rig) {
	r.m.mu.Lock()
	r.m.commitResetHold = nil
	r.m.mu.Unlock()
}

// fakeRecoveryStart marks pg stalled with a background recovery that has
// started and not yet taken attachMu, as Manager.recover does before its
// lock. finish ends it: with failure nil the page is attached again on its
// connection, else it stays stalled with failure as the recovery's error.
func fakeRecoveryStart(pg *Page) (finish func(failure error)) {
	pg.conn.mu.Lock()
	pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
	pg.conn.mu.Unlock()
	done := make(chan struct{})
	pg.mu.Lock()
	pg.recovering = true
	pg.recoveryRuns++
	pg.recoveryDone = done
	pg.mu.Unlock()
	return func(failure error) {
		pg.conn.mu.Lock()
		if failure == nil {
			pg.conn.state, pg.conn.stallErr = ConnOK, nil
		} else {
			pg.conn.stallErr = failure
		}
		pg.conn.mu.Unlock()
		pg.mu.Lock()
		pg.recovering = false
		close(done)
		pg.mu.Unlock()
	}
}

// emitCaller is a connection whose events the test emits (emit): handlers
// of SubscribeTargetSync run in emit, as in a read loop, and those of
// SubscribeTarget too (emit stands for the subscriber's own goroutine).
type emitCaller struct {
	webkit.Caller
	done chan struct{}

	mu   sync.Mutex
	next int
	subs map[int]webkit.TargetEventHandler
}

func newEmitCaller() *emitCaller {
	return &emitCaller{done: make(chan struct{}), subs: map[int]webkit.TargetEventHandler{}}
}

func (c *emitCaller) Done() <-chan struct{} { return c.done }
func (c *emitCaller) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

func (c *emitCaller) add(h webkit.TargetEventHandler) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.subs[id] = h
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.subs, id)
	}
}

func (c *emitCaller) SubscribeTarget(_ string, h webkit.TargetEventHandler) func() { return c.add(h) }

func (c *emitCaller) SubscribeTargetSync(_ string, h webkit.TargetEventHandler) func() {
	return c.add(h)
}

func (c *emitCaller) subscribers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}

func (c *emitCaller) emit(text string) {
	c.mu.Lock()
	hs := make([]webkit.TargetEventHandler, 0, len(c.subs))
	for _, h := range c.subs {
		hs = append(hs, h)
	}
	c.mu.Unlock()
	for _, h := range hs {
		h("page-1", "Console.messageAdded", json.RawMessage(`"`+text+`"`))
	}
}

// handoffRig is a page connection on a first connection with one
// read-loop and one queued subscriber that record what they get.
type handoffRig struct {
	p      *pageConn
	first  *emitCaller
	mu     sync.Mutex
	got    map[string][]string
	unsubs []func()
}

func newHandoffRig(t *testing.T, background func(func(context.Context)) bool) *handoffRig {
	h := &handoffRig{first: newEmitCaller(), got: map[string][]string{}}
	t.Cleanup(func() { _ = h.first.Close() })
	h.p = newPageConn(h.first, nil, background, t.Logf)
	for _, kind := range []string{"sync", "async"} {
		rec := func(_, _ string, params json.RawMessage) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.got[kind] = append(h.got[kind], string(params))
		}
		if kind == "sync" {
			h.unsubs = append(h.unsubs, h.p.SubscribeTargetSync("Console.messageAdded", rec))
		} else {
			h.unsubs = append(h.unsubs, h.p.SubscribeTarget("Console.messageAdded", rec))
		}
	}
	return h
}

func (h *handoffRig) events(kind string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.got[kind]...)
}

// trackedBackground is a page connection's background (Manager.goBackground)
// that the test owns: when the test ends, it refuses new goroutines,
// cancels the context of those it started and waits for them, so that no
// watch outlives the test (they log through t.Logf).
func trackedBackground(t *testing.T) func(func(context.Context)) bool {
	ctx, cancel := context.WithCancel(context.Background())
	var (
		mu     sync.Mutex
		closed bool
		wg     sync.WaitGroup
	)
	t.Cleanup(func() {
		mu.Lock()
		closed = true
		mu.Unlock()
		cancel()
		wg.Wait()
	})
	return func(fn func(context.Context)) bool {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return false
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
		return true
	}
}

// newEmitCallerFor is newEmitCaller, closed when the test ends.
func newEmitCallerFor(t *testing.T) *emitCaller {
	c := newEmitCaller()
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// await waits until kind's subscriber got n events.
func (h *handoffRig) await(t *testing.T, kind string, n int) {
	t.Helper()
	eventually(t, 2*time.Second, kind+" events", func() bool { return len(h.events(kind)) >= n })
}

// The events of a prepared connection are held until swap, then reach
// every subscriber in their order, before the connection's later events.
func TestPreparedConnectionEventsAreDeliveredAtSwap(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	next.emit("a")
	next.emit("b")
	time.Sleep(50 * time.Millisecond)
	for _, kind := range []string{"sync", "async"} {
		if got := h.events(kind); len(got) != 0 {
			t.Fatalf("%s subscriber got %v before the swap, want nothing", kind, got)
		}
	}
	mustSwap(t, h.p, next)
	if got := h.events("sync"); !equalStrings(got, []string{`"a"`, `"b"`}) {
		t.Errorf("read-loop subscriber got %v when swap returned, want [a b]", got)
	}
	next.emit("c")
	h.first.emit("old")
	h.await(t, "async", 3)
	for _, kind := range []string{"sync", "async"} {
		if got, want := h.events(kind), []string{`"a"`, `"b"`, `"c"`}; !equalStrings(got, want) {
			t.Errorf("%s subscriber got %v, want %v", kind, got, want)
		}
	}
	if n := next.subscribers(); n != 2 {
		t.Errorf("subscriptions on the installed connection = %d, want 2 (no second set)", n)
	}
}

// A prepared connection that is not installed delivers nothing, and its
// subscriptions are removed.
func TestDiscardedConnectionEventsAreDropped(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	next.emit("a")
	h.p.discard(next)
	next.emit("b")
	time.Sleep(50 * time.Millisecond)
	for _, kind := range []string{"sync", "async"} {
		if got := h.events(kind); len(got) != 0 {
			t.Errorf("%s subscriber got %v from a connection that was not installed, want nothing", kind, got)
		}
	}
	if n := next.subscribers(); n != 0 {
		t.Errorf("subscriptions left on the discarded connection = %d, want 0", n)
	}
	// A later swap to another connection subscribes it normally.
	other := newEmitCallerFor(t)
	mustSwap(t, h.p, other)
	other.emit("c")
	h.await(t, "async", 1)
	for _, kind := range []string{"sync", "async"} {
		if got := h.events(kind); !equalStrings(got, []string{`"c"`}) {
			t.Errorf("%s subscriber got %v after the swap to another connection, want [\"c\"]", kind, got)
		}
	}
}

// A subscription made while a connection is prepared gets its events; one
// removed meanwhile gets none of the held events.
func TestSubscriptionsChangedWhilePrepared(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	var mu sync.Mutex
	var late []string
	defer h.p.Subscribe("Console.messageAdded", func(_ string, params json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		late = append(late, string(params))
	})()
	next.emit("a")
	h.unsubs[0]() // the read-loop subscriber leaves before the swap
	mustSwap(t, h.p, next)
	h.await(t, "async", 1)
	eventually(t, 2*time.Second, "the late subscriber's event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(late) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if got := h.events("sync"); len(got) != 0 {
		t.Errorf("removed subscriber got %v, want nothing", got)
	}
	if got := h.events("async"); !equalStrings(got, []string{`"a"`}) {
		t.Errorf("queued subscriber got %v, want [\"a\"]", got)
	}
	if !equalStrings(late, []string{`"a"`}) {
		t.Errorf("subscriber made while the connection was prepared got %v, want [\"a\"]", late)
	}
}

// When the manager closes before the swap, the queued subscribers' held
// events are dropped instead of being delivered by an untracked goroutine.
func TestSwapAfterCloseDropsQueuedHeldEvents(t *testing.T) {
	h := newHandoffRig(t, func(func(context.Context)) bool { return false })
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	next.emit("a")
	mustSwap(t, h.p, next)
	if got := h.events("async"); len(got) != 0 {
		t.Errorf("queued subscriber got %v after a swap without background, want nothing", got)
	}
	if got := h.events("sync"); !equalStrings(got, []string{`"a"`}) {
		t.Errorf("read-loop subscriber got %v, want [\"a\"]", got)
	}
}

// mustSwap installs c on p (pageConn.swap) and fails the test when swap
// refuses it.
func mustSwap(t *testing.T, p *pageConn, c webkit.Caller) {
	t.Helper()
	if err := p.swap(c); err != nil {
		t.Fatalf("swap: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
