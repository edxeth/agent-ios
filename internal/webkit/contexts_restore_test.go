package webkit

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Finding 1 of the core-events review (docs/notes/w1b.md, "core-events r1:
// contexts after a back/forward cache restore"): a document restored from
// the back/forward cache comes back with Page.frameNavigated and without
// Runtime.executionContextCreated, and the ids recorded for it before are
// invalid. The inventory was empty after the restore. Now the session
// replays the contexts with Runtime.disable and Runtime.enable when a
// main-frame navigation reported no context, and only then.
//
// Finding 1 of core-events review round 2 (docs/notes/w1b.md, "core-events
// r2: the context replay and Runtime's owners"): the replay must not undo
// an owner's Runtime.disable. It runs through SessionOptions.ContextReplay,
// which core serializes with the changes of Runtime's owners
// (core.DesiredState.replayContexts); runtimeOwners models that here.

// runtimeOwners models core's owners of the Runtime domain: an owner's
// Runtime.enable or Runtime.disable and the context replay run one at a
// time (mu), and the replay runs only while an owner holds Runtime.
type runtimeOwners struct {
	mu        sync.Mutex
	owned     bool
	waiting   atomic.Int32 // owner changes waiting for mu
	replaying atomic.Bool  // the replay holds mu
}

func (o *runtimeOwners) options() SessionOptions {
	return SessionOptions{ContextReplay: o.replay}
}

func (o *runtimeOwners) replay(ctx context.Context, run func(context.Context) error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.owned {
		return nil
	}
	o.replaying.Store(true)
	defer o.replaying.Store(false)
	return run(ctx)
}

// set acquires (on) or releases Runtime for the one owner and sends its
// Runtime.enable or Runtime.disable.
func (o *runtimeOwners) set(ctx context.Context, s *Session, on bool) error {
	o.waiting.Add(1)
	o.mu.Lock()
	o.waiting.Add(-1)
	defer o.mu.Unlock()
	o.owned = on
	method := "Runtime.disable"
	if on {
		method = "Runtime.enable"
	}
	_, err := s.Send(ctx, method, nil)
	return err
}

// pageRuntime models WebKit's page runtime agent (PageRuntimeAgent,
// InjectedScriptManager) for the main frame of each target: context ids
// count up from 1 and are never reused; a new document reports its context
// right after its frameNavigated while the agent is enabled; a restored
// document reports nothing and has no valid id until something uses it;
// Runtime.enable reports the current document's context, assigning an id,
// only when the agent was disabled; Runtime.disable only switches it off.
type pageRuntime struct {
	f       *fakewebkit.Server
	mu      sync.Mutex
	next    int
	enabled map[string]bool
	doc     map[string]int // target -> id of its document's context, 0: none
}

const mainFrame = "0.1"

func newPageRuntime(f *fakewebkit.Server) *pageRuntime {
	m := &pageRuntime{f: f, enabled: map[string]bool{}, doc: map[string]int{}}
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.enabled[c.Target] {
			return nil, nil
		}
		m.enabled[c.Target] = true
		if m.doc[c.Target] == 0 {
			m.next++
			m.doc[c.Target] = m.next
		}
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", contextEvent(m.doc[c.Target], mainFrame))
		return nil, nil
	})
	f.Handle("Runtime.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.enabled[c.Target] = false
		return nil, nil
	})
	// The barrier: WebKit runs a command after the commit's task, which
	// reports the new document's context right after its frameNavigated.
	// m.mu holds the command until navigate has sent both events.
	f.Handle("Runtime.releaseObjectGroup", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return nil, nil
	})
	return m
}

func navigated(target, loader string) map[string]any {
	return map[string]any{"frame": map[string]any{"id": mainFrame, "loaderId": loader, "url": "https://fixture.example/"}}
}

// navigate loads a new document in target's main frame.
func (m *pageRuntime) navigate(target, loader string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.doc[target] = m.next
	m.f.EmitFrom(target, "Page.frameNavigated", navigated(target, loader))
	if m.enabled[target] {
		m.f.EmitFrom(target, "Runtime.executionContextCreated", contextEvent(m.doc[target], mainFrame))
	}
}

// restore brings a cached document back into target's main frame.
func (m *pageRuntime) restore(target, loader string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doc[target] = 0
	m.f.EmitFrom(target, "Page.frameNavigated", navigated(target, loader))
}

func (m *pageRuntime) current(target string) []ExecutionContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.doc[target] == 0 {
		return nil
	}
	return []ExecutionContext{{ID: m.doc[target], FrameID: mainFrame, Type: "normal"}}
}

func receivedOn(f *fakewebkit.Server, method, target string) int {
	n := 0
	for _, r := range f.ReceivedMethod(method) {
		if r.Target == target {
			n++
		}
	}
	return n
}

func waitContexts(t *testing.T, s *Session, target string, want []ExecutionContext) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for {
		got := s.ExecutionContexts(target)
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("ExecutionContexts(%s) = %+v, want %+v", target, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRestoredDocumentGetsValidContextsAgain(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	m := newPageRuntime(f)
	var owners runtimeOwners
	s := newFakeSession(t, f, owners.options())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := owners.set(ctx, s, true); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	waitContexts(t, s, "page-1", m.current("page-1"))
	resets := receivedOn(f, "Runtime.disable", "page-1") // the first enable's agent reset

	// A new document reports its context: no replay.
	m.navigate("page-1", "L2")
	waitFor(t, func() bool { return receivedOn(f, "Runtime.releaseObjectGroup", "page-1") == 1 })
	syncOn(t, s, "page-1")
	if n := receivedOn(f, "Runtime.disable", "page-1") - resets; n != 0 {
		t.Fatalf("%d Runtime.disable after a new document that reported its context, want 0", n)
	}
	waitContexts(t, s, "page-1", m.current("page-1"))

	// A restored document reports nothing: the session replays it.
	m.restore("page-1", "L1")
	waitFor(t, func() bool { return m.current("page-1") != nil })
	waitContexts(t, s, "page-1", m.current("page-1"))
	// The replayed context arrives before the enable's reply, which the
	// record follows.
	waitFor(t, func() bool { return s.AgentStates("page-1")["Runtime"] == AgentEnabled })
	if n := receivedOn(f, "Runtime.disable", "page-1") - resets; n != 1 {
		t.Fatalf("%d Runtime.disable, want 1 (one replay)", n)
	}
}

// Back across sites: the cached page's target is announced again with its
// old id, commits, and its document is restored. The replaced target has
// no contexts once it is destroyed, also while the restored target is
// provisional, and the restored document gets valid contexts.
func TestRestoredTargetGetsValidContextsAgain(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	m := newPageRuntime(f)
	var owners runtimeOwners
	s := newFakeSession(t, f, owners.options())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := owners.set(ctx, s, true); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	waitContexts(t, s, "page-1", m.current("page-1"))
	f.Provisional("page-2")
	f.Commit("page-2")
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	if got := s.ExecutionContexts("page-1"); len(got) != 0 {
		t.Fatalf("ExecutionContexts of the replaced target = %+v, want none", got)
	}
	f.Provisional("page-1")
	waitFor(t, func() bool { _, err := s.SendControl(WithTarget(ctx, "page-1"), "Test.sync", nil); return err == nil })
	if got := s.ExecutionContexts("page-1"); len(got) != 0 {
		t.Fatalf("ExecutionContexts of the restored provisional target before its document = %+v, want none", got)
	}
	f.Commit("page-1")
	m.restore("page-1", "L1") // the backend kept its enabled Runtime agent
	waitFor(t, func() bool { return m.current("page-1") != nil })
	waitContexts(t, s, "page-1", m.current("page-1"))
}

// Without an enabled Runtime agent no context is reported, and the
// session sends nothing after a navigation. A replay is skipped when the
// target navigated again before the barrier's reply: only the replay of
// the last navigation runs.
func TestContextReplayOnlyForTheLastNavigationWithRuntime(t *testing.T) {
	t.Run("Runtime not enabled", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		m := newPageRuntime(f)
		var owners runtimeOwners
		s := newFakeSession(t, f, owners.options())
		m.restore("page-1", "L1")
		syncOn(t, s, "page-1")
		time.Sleep(50 * time.Millisecond)
		if n := len(f.ReceivedMethod("Runtime.releaseObjectGroup")) + len(f.ReceivedMethod("Runtime.disable")); n != 0 {
			t.Fatalf("%d calls after a navigation without Runtime, want 0", n)
		}
	})
	t.Run("no owner model", func(t *testing.T) {
		// Without SessionOptions.ContextReplay the session cannot know
		// whether Runtime is still needed, so it replays nothing.
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		m := newPageRuntime(f)
		s := newFakeSession(t, f, SessionOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.Send(ctx, "Runtime.enable", nil); err != nil {
			t.Fatalf("Runtime.enable: %v", err)
		}
		resets := len(f.ReceivedMethod("Runtime.disable"))
		m.restore("page-1", "L1")
		syncOn(t, s, "page-1")
		time.Sleep(50 * time.Millisecond)
		if n := len(f.ReceivedMethod("Runtime.releaseObjectGroup")) + len(f.ReceivedMethod("Runtime.disable")) - resets; n != 0 {
			t.Fatalf("%d calls after a navigation without ContextReplay, want 0", n)
		}
	})
	t.Run("target replaced before the barrier's reply", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		m := newPageRuntime(f)
		var owners runtimeOwners
		s := newFakeSession(t, f, owners.options())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owners.set(ctx, s, true); err != nil {
			t.Fatalf("Runtime.enable: %v", err)
		}
		resets := len(f.ReceivedMethod("Runtime.disable"))
		f.DelayReplies("Runtime.releaseObjectGroup", 300*time.Millisecond)
		m.restore("page-1", "L1")
		waitFor(t, func() bool { return len(f.ReceivedMethod("Runtime.releaseObjectGroup")) == 1 })
		f.Provisional("page-2")
		f.Commit("page-2") // page-1 never answers the barrier now
		waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
		time.Sleep(400 * time.Millisecond) // past the barrier's delayed reply
		if n := len(f.ReceivedMethod("Runtime.disable")) - resets; n != 0 {
			t.Fatalf("%d Runtime.disable after the target was replaced, want 0", n)
		}
	})
	t.Run("navigated again before the barrier's reply", func(t *testing.T) {
		f := fakewebkit.New(fakewebkit.Options{})
		defer f.Close()
		m := newPageRuntime(f)
		var owners runtimeOwners
		s := newFakeSession(t, f, owners.options())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := owners.set(ctx, s, true); err != nil {
			t.Fatalf("Runtime.enable: %v", err)
		}
		resets := len(f.ReceivedMethod("Runtime.disable"))
		// Both barrier replies arrive after both restores, in order.
		f.DelayReplies("Runtime.releaseObjectGroup", 300*time.Millisecond)
		m.restore("page-1", "L1")
		m.restore("page-1", "L2")
		waitFor(t, func() bool { return len(f.ReceivedMethod("Runtime.releaseObjectGroup")) == 2 })
		waitFor(t, func() bool { return m.current("page-1") != nil })
		waitContexts(t, s, "page-1", m.current("page-1"))
		time.Sleep(100 * time.Millisecond) // a second replay would follow at once
		syncOn(t, s, "page-1")
		if n := len(f.ReceivedMethod("Runtime.disable")) - resets; n != 1 {
			t.Fatalf("%d Runtime.disable, want 1: the first navigation's replay is stale", n)
		}
	})
}

// blockingConn holds the write of the first Runtime.disable after armed
// until proceed is closed (the reviewer's reproduction of core-events r1).
type blockingConn struct {
	transport.MessageConn
	armed   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
}

func (c *blockingConn) Write(ctx context.Context, data []byte) error {
	var outer struct {
		Params struct {
			Message string `json:"message"`
		} `json:"params"`
	}
	var inner struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(data, &outer) == nil && json.Unmarshal([]byte(outer.Params.Message), &inner) == nil &&
		inner.Method == "Runtime.disable" && c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		select {
		case <-c.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.MessageConn.Write(ctx, data)
}

// The reviewer's reproduction: the last owner releases Runtime while the
// replay's Runtime.disable is being written. Before, the owner's disable
// completed first and the replay's Runtime.enable then enabled Runtime
// with no owner. Now the owner's change waits for the replay, which
// enables Runtime again in the same step, and its disable comes last.
func TestContextReplayDoesNotUndoTheLastOwnersDisable(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	m := newPageRuntime(f)
	var owners runtimeOwners
	var conn *blockingConn
	s := newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
		c, err := DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		if err != nil {
			return nil, err
		}
		conn = &blockingConn{MessageConn: c, entered: make(chan struct{}), proceed: make(chan struct{})}
		return conn, nil
	}, owners.options())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := owners.set(ctx, s, true); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	conn.armed.Store(true)
	m.restore("page-1", "L-restore")
	select {
	case <-conn.entered:
	case <-ctx.Done():
		t.Fatal("the replay did not reach its Runtime.disable")
	}
	released := make(chan error, 1)
	go func() { released <- owners.set(ctx, s, false) }()
	// The owner's change either waits for the replay or, without the
	// serialization, completes.
	waitFor(t, func() bool { return len(released) == 1 || owners.waiting.Load() == 1 && owners.replaying.Load() })
	close(conn.proceed)
	if err := <-released; err != nil {
		t.Fatalf("the owner's Runtime.disable: %v", err)
	}
	waitFor(t, func() bool { return receivedOn(f, "Runtime.enable", "page-1") == 2 }) // the owner's and the replay's
	syncOn(t, s, "page-1")
	m.mu.Lock()
	enabled := m.enabled["page-1"]
	m.mu.Unlock()
	if enabled {
		t.Fatal("the context replay enabled Runtime after its last owner's disable completed")
	}
	if n := receivedOn(f, "Runtime.disable", "page-1"); n != 3 { // the first enable's reset, the replay's, the owner's
		t.Fatalf("%d Runtime.disable, want 3", n)
	}
}

// The replay checks again inside ContextReplay, after it waited for the
// owners: a navigation while it waited makes it stale, and it sends
// nothing.
func TestContextReplayChecksAgainAfterWaitingForTheOwners(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	m := newPageRuntime(f)
	var owners runtimeOwners
	entered, proceed := make(chan struct{}), make(chan struct{})
	o := owners.options()
	o.ContextReplay = func(ctx context.Context, run func(context.Context) error) error {
		close(entered)
		<-proceed
		return owners.replay(ctx, run)
	}
	s := newFakeSession(t, f, o)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := owners.set(ctx, s, true); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	resets := receivedOn(f, "Runtime.disable", "page-1")
	m.restore("page-1", "L1")
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the replay did not start")
	}
	m.navigate("page-1", "L2") // a new document that reports its context
	waitFor(t, func() bool { return receivedOn(f, "Runtime.releaseObjectGroup", "page-1") == 2 })
	waitContexts(t, s, "page-1", m.current("page-1"))
	close(proceed)
	time.Sleep(100 * time.Millisecond) // the stale replay would send at once
	syncOn(t, s, "page-1")
	if n := receivedOn(f, "Runtime.disable", "page-1") - resets; n != 0 {
		t.Fatalf("%d Runtime.disable from a replay that went stale while it waited, want 0", n)
	}
}
