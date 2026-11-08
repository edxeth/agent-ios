package cdp

// The reproductions of the W5b-1 round-1 closure review
// (/home/devkit/.pi/artifacts/reviewer/w5b1-r1-20261108-090627.md,
// findings F1, F2 and F5), kept as regression tests.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReviewR1MissingWIPWorldStillRunsInNewDocument(t *testing.T) {
	for _, method := range []string{"Runtime.evaluate", "Runtime.callFunctionOn"} {
		t.Run(method, func(t *testing.T) {
			r, fp, c, sid, target := attachPage(t)
			call(t, c, sid, "Page.enable", nil)
			call(t, c, sid, "Runtime.enable", nil)
			alias := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"})).ExecutionContextID
			_, s := r.pageSession(t, sid)
			s.page.mu.Lock()
			id := s.page.mainWorldLocked(s.page.main.wip.ID)
			s.page.mu.Unlock()
			if id != 0 {
				t.Fatalf("fixture expected no WIP replay, got %d", id)
			}
			var once sync.Once
			fp.mu.Lock()
			fp.before = func(expr string) {
				if strings.Contains(expr, "__raced") {
					once.Do(func() { fp.navigate("https://one.example/newdoc") })
				}
			}
			fp.eval = func(expr string) (any, bool) {
				if strings.HasPrefix(expr, "(function") {
					return map[string]any{"result": map[string]any{"type": "function", "objectId": "fn-newdoc"}}, true
				}
				return nil, false
			}
			fp.mu.Unlock()
			r.fake["One"].Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "undefined"}})
			p := map[string]any{"expression": "window.__raced = true", "contextId": alias}
			if method == "Runtime.callFunctionOn" {
				p = map[string]any{"functionDeclaration": "function(){window.__raced = true}", "executionContextId": alias}
			}
			_, err := c.Call(ctxFor(t, 5*time.Second), sid, method, p)
			if err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
				t.Fatalf("%s with no reported WIP world ran after navigation: err=%v, expressions=%v", method, err, fp.expressions())
			}
		})
	}
}

func TestReviewR1PromiseContinuationKeepsReportingTarget(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	fp.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
	_, s := r.pageSession(t, sid)
	eventually(t, time.Second, "main world known", func() bool {
		s.page.mu.Lock()
		defer s.page.mu.Unlock()
		return s.page.mainWorldLocked(s.page.main.wip.ID) == 1
	})
	fp.mu.Lock()
	fp.eval = func(expr string) (any, bool) {
		if expr == "Promise.resolve('old')" {
			return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "old-promise"}}, true
		}
		return nil, false
	}
	fp.mu.Unlock()
	main := contextsOf(t, c, 0, sid)[0].ID
	params, _ := json.Marshal(map[string]any{"expression": "Promise.resolve('old')", "contextId": main, "awaitPromise": true, "returnByValue": true})
	_, later, err := runtimeEvaluate(ctxFor(t, 5*time.Second), &Call{Method: "Runtime.evaluate", Params: params, session: s})
	if err != nil || later == nil {
		t.Fatalf("fixture: later=%v err=%v", later != nil, err)
	}
	f := r.fake["One"]
	waitResumed(t, f.Provisional("next"))
	f.Commit("next")
	eventually(t, time.Second, "commit applied", func() bool { return activeTarget(s.pg) == "next" })
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "wrong target"}})
	_, err = later(ctxFor(t, 5*time.Second))
	calls := f.ReceivedMethod("Runtime.awaitPromise")
	if len(calls) != 0 && calls[len(calls)-1].Target == "next" {
		t.Fatalf("old promise was sent to new target: %s %s, error=%v", calls[len(calls)-1].Target, calls[len(calls)-1].Params, err)
	}
}

func TestReviewR1UnknownProvisionalWithQueuedCommit(t *testing.T) {
	r := newRig(t)
	fp := newFakePage(r.fake["One"])
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	att := eventsOf(c, "Target.attachedToTarget")
	tab := decode[attached](t, att[len(att)-1].Params).SessionID
	eventually(t, time.Second, "connection open", func() bool { return f.ConnectionCount() > 0 })
	waitResumed(t, f.Provisional("early"))
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att = eventsOf(c, "Target.attachedToTarget")
	sid := decode[attached](t, att[len(att)-1].Params).SessionID
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	_, s := r.pageSession(t, sid)
	oldTarget := f.ActiveTarget()
	s.page.mu.Lock()
	f.EmitFrom("early", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.2", "url": "https://early.example/", "securityOrigin": "https://early.example"}})
	// An active document's already-known frame can be reported again while the provisional one is loading.
	f.EmitFrom(oldTarget, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.1", "url": "https://one.example/", "securityOrigin": "https://one.example"}})
	fp.mu.Lock()
	fp.loader = 2
	fp.url = "https://early.example/"
	fp.mu.Unlock()
	f.Commit("early")
	eventually(t, time.Second, "connection applied commit ahead of subscriber", func() bool { return activeTarget(s.pg) == "early" })
	s.page.mu.Unlock()
	seq := c.Seq()
	syncConsole(t, r, c, seq, sid)
	got := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	if got.URL != "https://early.example/" {
		t.Fatalf("late-attached subscriber lost committed document: active=%s got URL=%s", f.ActiveTarget(), got.URL)
	}
}

func TestReviewR1LoadSourceCannotEmitOldLifecycle(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	seq := c.Seq()
	call(t, c, sid, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	idle := waitEvent(t, c, seq, sid, "Page.lifecycleEvent", func(p json.RawMessage) bool { return strings.Contains(string(p), `"name":"networkIdle"`) })
	old := decode[struct{ LoaderID string }](t, idle.Params).LoaderID
	waitResumed(t, f.Provisional("next"))
	f.Commit("next")
	_, s := r.pageSession(t, sid)
	eventually(t, time.Second, "commit applied", func() bool { return activeTarget(s.pg) == "next" })
	seq = c.Seq()
	f.EmitFrom("next", "Page.loadEventFired", map[string]any{"timestamp": 2})
	syncConsole(t, r, c, seq, sid)
	// The commit may reseed the new target's document (round-2 review:
	// a resynchronization may report its lifecycle); no lifecycle event
	// may carry the old document's loader.
	for _, e := range sessionEvents(c, seq, sid, "Page.lifecycleEvent") {
		if decode[struct{ LoaderID string }](t, e.Params).LoaderID == old {
			t.Fatalf("new target's load emitted old-document lifecycle: %s", e.Params)
		}
	}
}

func TestReviewR1StagingGrowth(t *testing.T) {
	ps := newPageModel("PAGE", func(string, any) {})
	defer ps.close()
	ps.dispatch("", "Target.targetCreated", json.RawMessage(`{"targetInfo":{"targetId":"pending","isProvisional":true}}`))
	payload := json.RawMessage(`{"message":{"source":"console-api","text":"still provisional"}}`)
	for batch := 1; batch <= 4; batch++ {
		for i := 0; i < 4096; i++ {
			ps.dispatch("pending", "Console.messageAdded", payload)
		}
		ps.mu.Lock()
		n := len(ps.staged["pending"])
		ps.mu.Unlock()
		t.Logf("after %d events, pending target retains %d staged payloads", batch*4096, n)
		// F5: the staged events are bounded (staging.go).
		if n > maxStagedEvents {
			t.Fatalf("after %d events the pending target retains %d staged payloads, more than the bound %d", batch*4096, n, maxStagedEvents)
		}
	}
}
