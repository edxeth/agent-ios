package cdp

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Provisional targets and document binding (design 2.7, 5.2; W5b-1
// review round 1, F1 and F2): the events of a provisional target are
// staged until its commit, which publishes its document once, and dropped
// when it is abandoned; frame, context and lifecycle events count only
// from the target that reported the frame; a call on an execution context
// is bound to that context's document and target.

// waitResumed waits until a provisional target of the fake runs.
func waitResumed(t *testing.T, resumed <-chan struct{}) {
	t.Helper()
	select {
	case <-resumed:
	case <-testCtx(t).Done():
		t.Fatal("the provisional target was not resumed")
	}
}

// consoleFrom is a console.log message event with text.
func consoleFrom(text string) map[string]any {
	return map[string]any{"message": map[string]any{
		"source": "console-api", "level": "log", "type": "log", "text": text,
		"parameters": []any{map[string]any{"type": "string", "value": text}},
	}}
}

func TestProvisionalDocumentPublishedAtCommit(t *testing.T) {
	r, _, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	old := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	waitResumed(t, f.Provisional("next"))
	seq := c.Seq()
	f.EmitFrom("next", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.1", "url": "https://two.example/", "securityOrigin": "https://two.example"}})
	f.EmitFrom("next", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
	f.EmitFrom("next", "Console.messageAdded", consoleFrom("from-next"))
	syncConsole(t, r, c, seq, sid)
	for _, m := range []string{"Page.frameNavigated", "Runtime.executionContextsCleared", "Runtime.executionContextCreated"} {
		if got := sessionEvents(c, seq, sid, m); len(got) != 0 {
			t.Fatalf("%s of the provisional target before its commit: %v", m, got)
		}
	}
	if got := sessionEvents(c, seq, sid, "Runtime.consoleAPICalled"); len(got) != 1 {
		t.Fatalf("console messages before the commit %v, want only the active target's", got)
	}

	seq = c.Seq()
	f.Commit("next")
	waitEvent(t, c, seq, sid, "Runtime.consoleAPICalled", func(p json.RawMessage) bool { return strings.Contains(string(p), "from-next") })
	var order []string
	for _, e := range c.Events() {
		if e.Seq > seq && e.SessionID == sid && e.Method != "Target.targetInfoChanged" {
			order = append(order, e.Method)
		}
	}
	want := []string{"Runtime.executionContextsCleared", "Page.frameNavigated", "Runtime.executionContextCreated", "Runtime.consoleAPICalled"}
	if !slices.Equal(order, want) {
		t.Fatalf("events at the commit %v, want %v", order, want)
	}
	nav := decode[navFrame](t, sessionEvents(c, seq, sid, "Page.frameNavigated")[0].Params).Frame
	if nav.URL != "https://two.example/" || nav.ID != target || nav.LoaderID == "" || nav.LoaderID == old.LoaderID {
		t.Fatalf("frameNavigated at the commit %+v, want the committed document of frame %s with a new loader (old %s)", nav, target, old.LoaderID)
	}
	if ctx := contextsOf(t, c, seq, sid); len(ctx) != 1 || ctx[0].AuxData.FrameID != target || !ctx[0].AuxData.IsDefault {
		t.Fatalf("contexts at the commit %+v, want the new document's default context", ctx)
	}
}

func TestProvisionalAbortDropsStagedEvents(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	waitResumed(t, f.Provisional("gone"))
	seq := c.Seq()
	f.EmitFrom("gone", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.7", "url": "https://gone.example/", "securityOrigin": "https://gone.example"}})
	f.EmitFrom("gone", "Console.messageAdded", consoleFrom("from-gone"))
	f.DestroyTarget("gone")
	syncConsole(t, r, c, seq, sid)
	for _, m := range []string{"Page.frameNavigated", "Runtime.executionContextsCleared", "Runtime.executionContextCreated"} {
		if got := sessionEvents(c, seq, sid, m); len(got) != 0 {
			t.Fatalf("%s of the abandoned target: %v", m, got)
		}
	}
	if got := sessionEvents(c, seq, sid, "Runtime.consoleAPICalled"); len(got) != 1 {
		t.Fatalf("console messages %v, want only the active target's", got)
	}
	_, s := r.pageSession(t, sid)
	s.page.mu.Lock()
	_, staged := s.page.staged["gone"]
	s.page.mu.Unlock()
	if staged {
		t.Fatal("the abandoned target's staged events were kept")
	}
}

// The active target's events that precede a commit keep their target's
// standing even when the attachment translates them after the connection
// applied the commit: the active target at attach time is live.
func TestEventsBeforeCommitKeepTheirTarget(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Runtime.enable", nil)
	waitResumed(t, f.Provisional("later"))
	_, s := r.pageSession(t, sid)
	seq := c.Seq()
	s.page.mu.Lock() // the attachment translates nothing until the commit is applied
	f.Emit("Console.messageAdded", consoleFrom("from-old"))
	f.Commit("later")
	eventually(t, 5*time.Second, "the connection applied the commit", func() bool { return activeTarget(s.pg) == "later" })
	s.page.mu.Unlock()
	waitEvent(t, c, seq, sid, "Runtime.consoleAPICalled", func(p json.RawMessage) bool { return strings.Contains(string(p), "from-old") })
}

// A provisional target that WebKit announced before the attachment (no
// targetCreated reaches its subscription) is staged too: it is not the
// connection's active target.
func TestProvisionalBeforeAttach(t *testing.T) {
	r := newRig(t)
	newFakePage(r.fake["One"])
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	att := eventsOf(c, "Target.attachedToTarget")
	tab := decode[attached](t, att[len(att)-1].Params).SessionID
	eventually(t, 5*time.Second, "the page connection is open", func() bool { return f.ConnectionCount() > 0 })
	waitResumed(t, f.Provisional("early"))
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att = eventsOf(c, "Target.attachedToTarget")
	sid := decode[attached](t, att[len(att)-1].Params).SessionID
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)

	seq := c.Seq()
	f.EmitFrom("early", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.3", "url": "https://early.example/", "securityOrigin": "https://early.example"}})
	syncConsole(t, r, c, seq, sid)
	if got := sessionEvents(c, seq, sid, "Page.frameNavigated"); len(got) != 0 {
		t.Fatalf("frameNavigated of a provisional target before its commit: %v", got)
	}
	f.Commit("early")
	nav := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params).Frame
	if nav.URL != "https://early.example/" {
		t.Fatalf("frameNavigated at the commit %+v, want the early target's document", nav)
	}
}

// After a commit whose target has not reported its document yet, the new
// target's frame ids name its own frames, not the old document's: its
// frame, context and lifecycle events are not applied to the old
// document. A call on an old context is bound to the old target and
// fails; a call without a context runs in the new target.
func TestEventSourceAfterCommit(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	fp.mu.Lock()
	fp.children = []wipTree{{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/child"}}}
	fp.mu.Unlock()
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	main := contextsOf(t, c, 0, sid)[0].ID
	waitResumed(t, f.Provisional("p2"))
	f.Commit("p2")

	seq := c.Seq()
	f.EmitFrom("p2", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 5, "type": "normal", "frameId": "child"}})
	f.EmitFrom("p2", "Page.frameStartedLoading", map[string]any{"frameId": "child"})
	f.EmitFrom("p2", "Page.frameStoppedLoading", map[string]any{"frameId": "main"})
	f.EmitFrom("p2", "Page.frameAttached", map[string]any{"frameId": "grandchild", "parentFrameId": "child"})
	f.EmitFrom("p2", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "child", "parentId": "main", "loaderId": "0.4", "url": "https://p2.example/child"}})
	f.EmitFrom("p2", "Page.domContentEventFired", map[string]any{"timestamp": 1})
	f.EmitFrom("p2", "Page.loadEventFired", map[string]any{"timestamp": 2})
	fp.consoleDebug(navMarker + `{"u":"https://one.example/#moved","t":"fragment"}`)
	f.EmitFrom("p2", "Page.frameDetached", map[string]any{"frameId": "child"})
	syncConsole(t, r, c, seq, sid)
	for _, m := range []string{
		"Runtime.executionContextCreated", "Runtime.executionContextDestroyed", "Page.frameStartedLoading", "Page.frameStoppedLoading",
		"Page.frameAttached", "Page.frameNavigated", "Page.domContentEventFired", "Page.loadEventFired", "Page.navigatedWithinDocument", "Page.frameDetached",
	} {
		if got := sessionEvents(c, seq, sid, m); len(got) != 0 {
			t.Fatalf("%s from the new target applied to the old document: %v", m, got)
		}
	}

	if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": "window.__old = 1", "contextId": main}); err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
		t.Fatalf("evaluate on the old document's context after the commit: %v, want Chrome's unknown-context error", err)
	}
	if slices.Contains(fp.expressions(), "window.__old = 1") {
		t.Fatal("the old document's call ran in the committed target")
	}
	call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "window.__any = 1"})
	if got := f.ReceivedMethod("Runtime.evaluate"); got[len(got)-1].Target != "p2" {
		t.Fatalf("evaluate without a context went to %q, want the active target p2", got[len(got)-1].Target)
	}
}

// A navigation that commits in the same target after the call was
// checked: the call carries the document's WIP main world, which WebKit
// no longer knows, so it does not run and the client gets Chrome's
// unknown-context error.
func TestContextBoundToDocument(t *testing.T) {
	for _, method := range []string{"Runtime.evaluate", "Runtime.callFunctionOn"} {
		t.Run(method, func(t *testing.T) {
			r, fp, c, sid, target := attachPage(t)
			f := r.fake["One"]
			call(t, c, sid, "Page.enable", nil)
			call(t, c, sid, "Runtime.enable", nil)
			f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
			_, s := r.pageSession(t, sid)
			eventually(t, 5*time.Second, "the main world's WIP id is known", func() bool {
				s.page.mu.Lock()
				defer s.page.mu.Unlock()
				return s.page.main != nil && s.page.mainWorldLocked(s.page.main.wip.ID) == 1
			})
			alias := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"})).ExecutionContextID
			var once sync.Once
			fp.mu.Lock()
			fp.before = func(expr string) {
				if strings.Contains(expr, "__raced") {
					once.Do(func() { fp.navigate("https://one.example/raced") })
				}
			}
			fp.eval = func(expr string) (any, bool) {
				if strings.HasPrefix(expr, "(function") {
					return map[string]any{"result": map[string]any{"type": "function", "objectId": "fn-raced"}}, true
				}
				return nil, false
			}
			fp.mu.Unlock()
			params := map[string]any{"expression": "window.__raced = 1", "contextId": alias}
			if method == "Runtime.callFunctionOn" {
				params = map[string]any{"functionDeclaration": "function() { window.__raced = 1; }", "executionContextId": alias}
			}
			_, err := c.Call(ctxFor(t, 5*time.Second), sid, method, params)
			if err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
				t.Fatalf("%s on a context whose document was replaced before it ran: %v, want Chrome's unknown-context error", method, err)
			}
			for _, e := range fp.expressions() {
				if strings.Contains(e, "__raced") {
					t.Fatalf("%s ran in the new document: %q", method, e)
				}
			}
			if got := f.ReceivedMethod("Runtime.callFunctionOn"); len(got) != 0 {
				t.Fatalf("callFunctionOn sent after the failed binding: %v", got)
			}
		})
	}
}
