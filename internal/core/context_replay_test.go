package core

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Finding 1 of core-events review round 2 (docs/notes/w1b.md, "core-events
// r2: the context replay and Runtime's owners"): the session replays the
// contexts of a document restored from the back/forward cache with
// Runtime.disable and Runtime.enable. The replay ran outside the
// desired-state changes, so when the last Runtime owner's Runtime.disable
// completed between the replay's check and its calls, the replay enabled
// Runtime again with no owner. Now the replay is one desired-state change
// (DesiredState.replayContexts): it runs only while an owner needs Runtime,
// and no owner's change runs between its check and its calls.

// runtimeAgent models WebKit's page runtime agent for the main frame of
// each target (PageRuntimeAgent): context ids count up and are never
// reused; Runtime.enable reports the current document's context only when
// the agent was disabled; a restored document reports nothing.
type runtimeAgent struct {
	f       *fakewebkit.Server
	mu      sync.Mutex
	next    int
	enabled map[string]bool
	doc     map[string]int // target -> id of its document's context, 0: none
}

const replayFrame = "0.1"

func newRuntimeAgent(f *fakewebkit.Server) *runtimeAgent {
	a := &runtimeAgent{f: f, enabled: map[string]bool{}, doc: map[string]int{}}
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.enabled[c.Target] {
			return nil, nil
		}
		a.enabled[c.Target] = true
		if a.doc[c.Target] == 0 {
			a.next++
			a.doc[c.Target] = a.next
		}
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": a.doc[c.Target], "frameId": replayFrame, "name": "", "type": "normal"}})
		return nil, nil
	})
	f.Handle("Runtime.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.enabled[c.Target] = false
		return nil, nil
	})
	return a
}

// restore brings a cached document back into target's main frame.
func (a *runtimeAgent) restore(target, loader string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.doc[target] = 0
	a.f.EmitFrom(target, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": replayFrame, "loaderId": loader, "url": "https://fixture.example/"}})
}

func (a *runtimeAgent) isEnabled(target string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enabled[target]
}

func (a *runtimeAgent) current(target string) []webkit.ExecutionContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc[target] == 0 {
		return nil
	}
	return []webkit.ExecutionContext{{ID: a.doc[target], FrameID: replayFrame, Type: "normal"}}
}

func countOn(f *fakewebkit.Server, method, target string) int {
	n := 0
	for _, r := range f.ReceivedMethod(method) {
		if r.Target == target {
			n++
		}
	}
	return n
}

// While a legacy owner holds Runtime, the restored document's contexts are
// replayed through the daemon's page connection.
func TestRestoredDocumentContextsReplayWhileRuntimeIsOwned(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	a := newRuntimeAgent(r.f)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "runtime", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(ctx, mo.ID, "Runtime.enable", nil); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	pc := mo.Page.PS.Caller.(*pageConn)
	disables := countOn(r.f, "Runtime.disable", "page-1")
	a.restore("page-1", "L-restore")
	eventually(t, 3*time.Second, "the restored document's context", func() bool {
		want := a.current("page-1")
		return want != nil && reflect.DeepEqual(pc.ExecutionContexts("page-1"), want)
	})
	if n := countOn(r.f, "Runtime.disable", "page-1") - disables; n != 1 {
		t.Fatalf("%d Runtime.disable for the replay, want 1", n)
	}
	if !a.isEnabled("page-1") {
		t.Fatal("Runtime is disabled after the replay while its owner holds it")
	}
}

// The last owner's Runtime.disable is written and its reply is pending
// when the restore's barrier answers, so the session's record still says
// enabled. The replay must wait for that change and then find no owner:
// it sends nothing, and Runtime stays disabled.
func TestContextReplayDoesNotEnableRuntimeAfterItsLastOwner(t *testing.T) {
	const disableReply = 400 * time.Millisecond
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	a := newRuntimeAgent(r.f)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "runtime", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(ctx, mo.ID, "Runtime.enable", nil); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	enables := countOn(r.f, "Runtime.enable", "page-1")
	disables := countOn(r.f, "Runtime.disable", "page-1")
	r.f.DelayReplies("Runtime.disable", disableReply)
	released := make(chan error, 1)
	go func() {
		_, err := r.m.Raw(ctx, mo.ID, "Runtime.disable", nil)
		released <- err
	}()
	eventually(t, 3*time.Second, "the owner's Runtime.disable", func() bool { return countOn(r.f, "Runtime.disable", "page-1") == disables+1 })
	a.restore("page-1", "L-restore")
	eventually(t, 3*time.Second, "the replay's barrier", func() bool { return countOn(r.f, "Runtime.releaseObjectGroup", "page-1") == 1 })
	if err := <-released; err != nil {
		t.Fatalf("the owner's Runtime.disable: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // a replay would follow at once
	if a.isEnabled("page-1") {
		t.Fatal("the context replay enabled Runtime after its last owner disabled it")
	}
	if n := countOn(r.f, "Runtime.enable", "page-1") - enables; n != 0 {
		t.Fatalf("%d Runtime.enable after the last owner's disable, want 0", n)
	}
	pc := mo.Page.PS.Caller.(*pageConn)
	pc.mu.Lock()
	s := pc.cur.(*webkit.Session)
	pc.mu.Unlock()
	if st := s.AgentStates("page-1")["Runtime"]; st != webkit.AgentDisabled {
		t.Fatalf("the Runtime record = %v, want AgentDisabled", st)
	}
}

// replayContexts calls the replay only while an owner needs Runtime, never
// while another change runs, and returns the replay's error.
func TestReplayContextsRunsOnlyForAnOwnerBetweenChanges(t *testing.T) {
	d := NewDesiredState()
	ctx := ctxFor(t, 5*time.Second)
	called := 0
	replay := func(context.Context) error { called++; return nil }
	if err := d.replayContexts(ctx, replay); err != nil || called != 0 {
		t.Fatalf("without an owner: err %v, %d replays, want none", err, called)
	}
	d.Acquire("o", "Runtime")
	if err := d.replayContexts(ctx, replay); err != nil || called != 1 {
		t.Fatalf("with an owner: err %v, %d replays, want 1", err, called)
	}

	// A change that releases the last owner runs: the replay waits for it
	// and then finds no owner.
	_, end, err := d.beginChange(ctx, nil)
	if err != nil {
		t.Fatalf("beginChange: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- d.replayContexts(ctx, replay) }()
	select {
	case err := <-done:
		t.Fatalf("the replay returned (%v) while a change ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	d.ReleaseDomain("o", "Runtime")
	if err := end(nil); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := <-done; err != nil || called != 1 {
		t.Fatalf("after the last owner's release: err %v, %d replays, want 1 in all", err, called)
	}

	d.Acquire("o", "Runtime")
	boom := errors.New("boom")
	if err := d.replayContexts(ctx, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("the replay's error: got %v, want %v", err, boom)
	}
	_, end, err = d.beginChange(ctx, nil)
	if err != nil {
		t.Fatalf("beginChange: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := d.replayContexts(short, replay); !errors.Is(err, context.DeadlineExceeded) || called != 1 {
		t.Fatalf("waiting past its deadline: err %v, %d replays; want the deadline and no replay", err, called)
	}
	if err := end(nil); err != nil {
		t.Fatalf("end: %v", err)
	}
}
