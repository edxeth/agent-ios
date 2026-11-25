package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// inventoryCaller is a connection with an execution-context inventory and
// agent states (contextSource) that the test sets, as webkit.Session
// keeps them.
type inventoryCaller struct {
	nopCaller
	done    chan struct{}
	mu      sync.Mutex
	target  string
	ctxs    []webkit.ExecutionContext
	runtime webkit.AgentState // Runtime's state on target; 0: unknown
	// onAgentStates, when set, runs once, at the next AgentStates call,
	// before the states are read (a change between two reads of a waiter).
	onAgentStates func()
}

func newInventoryCaller(t *testing.T) *inventoryCaller {
	c := &inventoryCaller{done: make(chan struct{})}
	t.Cleanup(func() { close(c.done) })
	return c
}

func (c *inventoryCaller) Done() <-chan struct{} { return c.done }

// set makes target the active target with the contexts list and Runtime's
// agent state.
func (c *inventoryCaller) set(target string, runtime webkit.AgentState, list ...webkit.ExecutionContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.target, c.runtime, c.ctxs = target, runtime, list
}

func (c *inventoryCaller) ActiveTarget() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

func (c *inventoryCaller) ExecutionContexts(target string) []webkit.ExecutionContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	if target != c.target {
		return nil
	}
	return slices.Clone(c.ctxs)
}

func (c *inventoryCaller) AgentStates(target string) map[string]webkit.AgentState {
	c.mu.Lock()
	hook := c.onAgentStates
	c.onAgentStates = nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if target != c.target || c.runtime == 0 {
		return nil
	}
	return map[string]webkit.AgentState{"Runtime": c.runtime}
}

func normalCtx(id int, frame string) webkit.ExecutionContext {
	return webkit.ExecutionContext{ID: id, FrameID: frame, Type: "normal"}
}

// boundID returns the context id bound as FrameContext binds a context:
// with its frame (when the inventory lists it), the active target and the
// connection of the scheduler's current inventory.
func boundID(s *Scheduler, id int64) ExecContext {
	inv := s.inventory()
	ec := ExecContext{ID: id, target: inv.target, conn: inv.conn}
	for _, c := range inv.contexts {
		if mainWorld(c) && int64(c.ID) == id {
			ec.Frame = c.FrameID
		}
	}
	return ec
}

// W3b final round, finding (a) of the round-4 review (design 2.4, 3.8):
// the scheduler reads the frame contexts from its connection's inventory
// (webkit.Session.ExecutionContexts, W1c): the newest main-world context
// of the frame on the active target; isolated worlds and other targets do
// not count. A connection without an inventory names no frame context.
func TestFrameContextReadsTheConnectionsInventory(t *testing.T) {
	c := newInventoryCaller(t)
	s := NewScheduler(c, SchedulerOptions{})
	t.Cleanup(s.Close)
	c.set("page-1", webkit.AgentEnabled, normalCtx(1, "main"), normalCtx(7, "kid"),
		webkit.ExecutionContext{ID: 8, FrameID: "kid", Type: "user"}, normalCtx(12, "kid"),
		webkit.ExecutionContext{ID: 13, FrameID: "ext", Type: "user"})
	got := []int64{s.FrameContext("main").ID, s.FrameContext("kid").ID, s.FrameContext("ext").ID, s.FrameContext("none").ID}
	if want := []int64{1, 12, 0, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frame contexts = %v, want %v", got, want)
	}
	c.set("page-2", webkit.AgentEnabled, normalCtx(30, "other"))
	if id := s.FrameContext("kid").ID; id != 0 {
		t.Fatalf("frame context of another target's frame = %d, want 0", id)
	}
	plain := NewScheduler(nopCaller{}, SchedulerOptions{})
	t.Cleanup(plain.Close)
	if id := plain.FrameContext("kid").ID; id != 0 {
		t.Fatalf("frame context on a connection without an inventory = %d, want 0", id)
	}
}

// The inventory rules of a reconnect (contextsMoved): the recorded context
// ids are void on the new connection and never sent there; a recorded
// context becomes its frame, which the release finds in the newest
// main-world context of the current connection; a frame without a context
// there stays recorded with an error until it has one; a recorded context
// whose frame the inventory never knew is dropped.
func TestContextsMovedResolvesFramesOnTheCurrentConnection(t *testing.T) {
	c := newInventoryCaller(t)
	s := NewScheduler(c, SchedulerOptions{})
	t.Cleanup(s.Close)
	c.set("page-1", webkit.AgentEnabled, normalCtx(7, "kid"), normalCtx(8, "ad"))
	s.installContext(1, boundID(s, 7))
	s.installContext(1, boundID(s, 8))
	s.installContext(2, boundID(s, 7))
	s.installContext(3, boundID(s, 99)) // a context the inventory never listed

	s.contextsMoved()
	c.set("page-1", 0, normalCtx(17, "kid"), normalCtx(18, "kid"))
	if e := s.ContextEpochsBefore(10); !reflect.DeepEqual(e, []uint64{1, 2}) {
		t.Fatalf("epochs with recorded contexts = %v, want [1 2] (epoch 3's context had no frame)", e)
	}
	release := func(epoch uint64) ([]int64, error) {
		var ids []int64
		err := s.ReleaseExecutionContexts(ctxFor(t, time.Second), epoch, nopCaller{}, func(_ context.Context, c webkit.Caller) error {
			ids = append(ids, c.(contextCaller).ec.ID)
			return nil
		})
		return ids, err
	}
	ids, err := release(1)
	if !reflect.DeepEqual(ids, []int64{18}) || !errors.Is(err, errContextNotReported) || !strings.Contains(err.Error(), "frame ad") {
		t.Fatalf("release of epoch 1: contexts %v, err %v; want [18] and frame ad not reported", ids, err)
	}
	if ids, err := release(2); !reflect.DeepEqual(ids, []int64{18}) || err != nil {
		t.Fatalf("release of epoch 2: contexts %v, err %v; want [18]", ids, err)
	}
	if e := s.ContextEpochsBefore(10); !reflect.DeepEqual(e, []uint64{1}) {
		t.Fatalf("epochs with recorded contexts after the releases = %v, want [1] (frame ad)", e)
	}
	// The frame without a context has one later (Runtime enabled again).
	c.set("page-1", webkit.AgentEnabled, normalCtx(18, "kid"), normalCtx(21, "ad"))
	if ids, err := release(1); !reflect.DeepEqual(ids, []int64{21}) || err != nil {
		t.Fatalf("release of epoch 1 after frame ad reported: contexts %v, err %v; want [21]", ids, err)
	}
	if e := s.ContextEpochsBefore(10); len(e) != 0 {
		t.Fatalf("epochs with recorded contexts at the end = %v", e)
	}
}

// Finding (b) of the round-4 review (design 2.6: an operation ends at its
// deadline): a wait for frame contexts whose context has ended, already
// at its start or during the wait, drops nothing, also when the inventory
// is complete; whether the frame lives is not decided. Only a live wait
// on a complete inventory counts a missing frame as gone. On an
// incomplete inventory the wait keeps the frame and takes at most half of
// the time left.
func TestFrameReportWaitDecidesNothingAfterItsContextEnds(t *testing.T) {
	setup := func(t *testing.T, runtime webkit.AgentState) (*Scheduler, *inventoryCaller) {
		c := newInventoryCaller(t)
		s := NewScheduler(c, SchedulerOptions{})
		t.Cleanup(s.Close)
		c.set("page-1", webkit.AgentEnabled, normalCtx(7, "kid"))
		s.installContext(1, boundID(s, 7))
		s.contextsMoved()
		c.set("page-1", runtime)
		return s, c
	}
	kept := func(t *testing.T, s *Scheduler, gone []string) {
		t.Helper()
		if gone != nil {
			t.Fatalf("frames counted as gone = %q, want none", gone)
		}
		if got := s.unreportedFrames(1); !reflect.DeepEqual(got, []string{"kid"}) {
			t.Fatalf("frames recorded after the wait = %q, want [kid]", got)
		}
	}
	t.Run("expired at the start", func(t *testing.T) {
		s, _ := setup(t, webkit.AgentEnabled)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		kept(t, s, s.awaitReports(ctx, 1))
	})
	t.Run("canceled during the wait", func(t *testing.T) {
		s, c := setup(t, webkit.AgentMaybeEnabled)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, func() {
			cancel()
			c.set("page-1", webkit.AgentEnabled) // complete only after the cancel
		})
		kept(t, s, s.awaitReports(ctx, 1))
	})
	// The cancel lands after the wait checked ctx and before it reads the
	// inventory, which is complete from then on (seen once under -race:
	// the frame was counted as gone).
	t.Run("canceled before the inventory read", func(t *testing.T) {
		s, c := setup(t, webkit.AgentMaybeEnabled)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.mu.Lock()
		c.onAgentStates = func() {
			cancel()
			c.set("page-1", webkit.AgentEnabled)
		}
		c.mu.Unlock()
		kept(t, s, s.awaitReports(ctx, 1))
	})
	t.Run("incomplete inventory", func(t *testing.T) {
		s, _ := setup(t, webkit.AgentMaybeEnabled)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		gone := s.awaitReports(ctx, 1)
		// Half of the 200 ms left, plus one poll interval.
		if took := time.Since(start); took > 100*time.Millisecond+frameReportPoll+50*time.Millisecond {
			t.Fatalf("the wait took %v, want at most half the time left", took)
		}
		kept(t, s, gone)
	})
	t.Run("complete inventory", func(t *testing.T) {
		s, _ := setup(t, webkit.AgentEnabled)
		if gone := s.awaitReports(ctxFor(t, time.Second), 1); !reflect.DeepEqual(gone, []string{"kid"}) {
			t.Fatalf("frames counted as gone = %q, want [kid]", gone)
		}
		if e := s.ContextEpochsBefore(2); len(e) != 0 {
			t.Fatalf("epochs with recorded contexts = %v, want none", e)
		}
	})
	// The frame's report and the enable's reply land between the read of
	// the frame's context and the read of the agent state (webkit.Session
	// records the report before the reply; seen once under -race in
	// "reported" below): the frame lives and is not counted as gone.
	t.Run("reported between the reads", func(t *testing.T) {
		s, c := setup(t, webkit.AgentMaybeEnabled)
		c.mu.Lock()
		c.onAgentStates = func() { c.set("page-1", webkit.AgentEnabled, normalCtx(17, "kid")) }
		c.mu.Unlock()
		if gone := s.awaitReports(ctxFor(t, time.Second), 1); gone != nil {
			t.Fatalf("frames counted as gone = %q, want none", gone)
		}
		if got := s.unreportedFrames(1); len(got) != 0 {
			t.Fatalf("frames without a context = %q, want none", got)
		}
	})
	t.Run("reported", func(t *testing.T) {
		s, c := setup(t, webkit.AgentMaybeEnabled)
		time.AfterFunc(20*time.Millisecond, func() { c.set("page-1", webkit.AgentEnabled, normalCtx(17, "kid")) })
		if gone := s.awaitReports(ctxFor(t, time.Second), 1); gone != nil {
			t.Fatalf("frames counted as gone = %q, want none", gone)
		}
		if got := s.unreportedFrames(1); len(got) != 0 {
			t.Fatalf("frames without a context = %q, want none", got)
		}
	})
}

// Finding (a) of the round-4 review, the reviewer's reproduction
// TestReviewProvisionalSetupBeforeCapture (design 2.7 provisional setup,
// 3.8): during a reconnect, a provisional target's setup enables Runtime,
// and the frame reports its context, before Manager.openConn returns;
// the re-apply that follows sends no second Runtime.enable. The new
// connection's inventory (webkit.Session, from its start) keeps that
// report, so the release finds the live frame in its new context and
// never counts it as gone.
func TestProvisionalSetupBeforeTheReconnectKeepsTheFrame(t *testing.T) {
	ctx := ctxFor(t, 5*time.Second)
	old := newInventoryCaller(t)
	old.set("page-1", webkit.AgentEnabled, normalCtx(7, "kid"))
	pc := newPageConn(old, func() {}, func(func(context.Context)) bool { return false }, t.Logf)
	t.Cleanup(pc.close) // before the callers' cleanup: no watch log after the test
	sched := NewScheduler(pc, SchedulerOptions{})
	t.Cleanup(sched.Close)
	sched.installContext(1, boundID(sched, 7))
	state := NewDesiredState()
	frameOwner := Owner("frame:s1")
	state.Acquire(frameOwner, "Runtime")
	ps := &PageSession{State: state, Sched: sched}
	pg := &Page{PS: ps}

	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	var mu sync.Mutex
	enabled := map[string]bool{}
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		mu.Lock()
		first := !enabled[c.Target]
		enabled[c.Target] = true
		mu.Unlock()
		if first {
			f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 17, "type": "normal", "frameId": "kid"}})
		}
		return map[string]any{}, nil
	})
	target := pageTarget{title: "fixture", open: func(ctx context.Context, state *DesiredState) (webkit.Caller, error) {
		setup := func(ctx context.Context, tc webkit.TargetCaller) error { return state.Apply(ctx, tc) }
		sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
			return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		}, webkit.SessionOptions{Setup: setup, PauseOnStart: true})
		if err != nil {
			return nil, err
		}
		f.Swap("page-2", 0)
		eventually(t, time.Second, "the early provisional target to commit", func() bool { return sess.ActiveTarget() == "page-2" })
		if err := sess.ActiveSetup(ctx); err != nil {
			return nil, err
		}
		return sess, nil
	}}
	m := &Manager{}
	c, _, err := m.openConn(ctx, pg, target, state, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeCaller(c) })
	if err := pc.swap(c); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReceivedMethod("Runtime.enable")); n != 1 {
		t.Fatalf("Runtime.enable sent %d times, want once (the provisional setup's)", n)
	}
	var released []int64
	err = ps.ReleaseFrameContexts(ctx, 1, "s1", pc, func(_ context.Context, c webkit.Caller) error {
		released = append(released, c.(contextCaller).ec.ID)
		return nil
	})
	if err != nil || !reflect.DeepEqual(released, []int64{17}) {
		t.Fatalf("frame release: contexts %v, err %v; want the live frame's new context [17]", released, err)
	}
	if e := sched.ContextEpochsBefore(2); len(e) != 0 {
		t.Fatalf("epochs with recorded contexts after the release = %v, want none", e)
	}
	if o := state.AllOwners(); !reflect.DeepEqual(o, []Owner{frameOwner}) {
		t.Fatalf("owners after the release = %q, want only %s", o, frameOwner)
	}
}
