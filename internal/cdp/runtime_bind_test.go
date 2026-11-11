package cdp

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// Context-bound Runtime calls (design 5.3; W5b-1 review F2) and the reset
// that replaces an incomplete provisional document (F5), on the real
// bridge and core with fake WebKit.

// attachWithPriorContexts opens page One's connection, lets WebKit report
// the main world (id 1) and, with child, the child frame c1's main world
// (id 7) before the page attachment exists (as when another owner enabled
// Runtime before), then attaches the page and enables Page and Runtime;
// the attachment's own events never report these worlds.
func attachWithPriorContexts(t *testing.T, child bool) (*rig, *fakePage, *cdptest.Client, string, *session) {
	t.Helper()
	r := newRig(t)
	fp := newFakePage(r.fake["One"])
	f := r.fake["One"]
	if child {
		fp.children = []wipTree{{Frame: wipFrame{ID: "c1", ParentID: "main", LoaderID: "c.1", URL: "https://c.example/", SecurityOrigin: "https://c.example"}}}
	}
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	eventually(t, time.Second, "connection open", func() bool { return f.ConnectionCount() > 0 })
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
	if child {
		f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "c1"}})
	}
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att := eventsOf(c, "Target.attachedToTarget")
	sid := decode[attached](t, att[len(att)-1].Params).SessionID
	_, s := r.pageSession(t, sid)
	eventually(t, time.Second, "WebKit's contexts recorded", func() bool {
		return s.pg.PS.Sched.FrameContext("main").ID == 1 && (!child || s.pg.PS.Sched.FrameContext("c1").ID == 7)
	})
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	return r, fp, c, sid, s
}

// contextOf returns the CDP default context of the CDP frame id.
func contextOf(t *testing.T, c *cdptest.Client, sid, frame string) int {
	t.Helper()
	for _, x := range contextsOf(t, c, 0, sid) {
		if x.AuxData.FrameID == frame && x.AuxData.IsDefault {
			return x.ID
		}
	}
	t.Fatalf("no default context of frame %s", frame)
	return 0
}

// evaluateErr runs Runtime.evaluate in the context id and returns its error.
func evaluateErr(t *testing.T, c *cdptest.Client, sid, expr string, id int) error {
	t.Helper()
	_, err := c.Call(ctxFor(t, 5*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": expr, "contextId": id})
	return err
}

func isNoContext(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Cannot find context with specified id")
}

// A context whose WIP world the attachment never saw is proven by the
// connection's record of the document's contexts: the call carries that
// world's id to its target, so a navigation between the proof and the
// call makes WebKit refuse it instead of running it in the new document.
func TestContextProvenByConnectionRecord(t *testing.T) {
	r, fp, c, sid, s := attachWithPriorContexts(t, false)
	f := r.fake["One"]
	s.page.mu.Lock()
	unseen := s.page.mainWorldLocked("main")
	s.page.mu.Unlock()
	if unseen != 0 {
		t.Fatalf("fixture: the attachment saw the main world %d", unseen)
	}
	main := contextOf(t, c, sid, s.page.target)
	if err := evaluateErr(t, c, sid, "window.__proven = 1", main); err != nil {
		t.Fatalf("evaluate in the proven main context: %v", err)
	}
	ev := f.ReceivedMethod("Runtime.evaluate")
	last := ev[len(ev)-1]
	if !strings.Contains(string(last.Params), `"contextId":1`) || last.Target != f.ActiveTarget() {
		t.Fatalf("evaluate sent %s to %s, want contextId 1 on the active target %s", last.Params, last.Target, f.ActiveTarget())
	}
	fp.mu.Lock()
	fp.before = func(expr string) {
		if strings.Contains(expr, "__raced") {
			fp.mu.Lock()
			fp.before = nil
			fp.mu.Unlock()
			fp.navigate("https://one.example/next")
		}
	}
	fp.mu.Unlock()
	if err := evaluateErr(t, c, sid, "window.__raced = 1", main); !isNoContext(err) {
		t.Fatalf("evaluate raced by a navigation: %v, want the unknown-context error", err)
	}
	for _, e := range fp.expressions() {
		if strings.Contains(e, "__raced") {
			t.Fatalf("the raced expression ran: %v", fp.expressions())
		}
	}
}

// A candidate context of another connection or of another world than the
// one the attachment saw is not proven: nothing runs.
func TestContextProofRefusesOtherSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(ps *pageState) func()
	}{
		{"other connection", func(ps *pageState) func() {
			c := ps.conn
			ps.conn = &otherConn{n: 9}
			return func() { ps.conn = c }
		}},
		{"other world", func(ps *pageState) func() {
			for _, x := range ps.ctxs {
				if x.wipFrame == "main" && x.world == "" {
					x.wipID = 99
				}
			}
			return func() {}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fp, c, sid, s := attachWithPriorContexts(t, false)
			main := contextOf(t, c, sid, s.page.target)
			s.page.mu.Lock()
			undo := tc.edit(s.page)
			s.page.mu.Unlock()
			if err := evaluateErr(t, c, sid, "window.__unproven = 1", main); !isNoContext(err) {
				t.Fatalf("evaluate: %v, want the unknown-context error", err)
			}
			s.page.mu.Lock()
			undo()
			s.page.mu.Unlock()
			for _, e := range fp.expressions() {
				if strings.Contains(e, "__unproven") {
					t.Fatalf("the unproven expression ran: %v", fp.expressions())
				}
			}
		})
	}
}

// The proof waits until the attachment has handled every event that the
// connection delivered before the candidate's read: a child frame that
// navigated while the attachment lagged has no proven context, even when
// the connection already recorded the new document's world.
func TestContextProofWaitsForTheAttachment(t *testing.T) {
	r, fp, c, sid, s := attachWithPriorContexts(t, true)
	f := r.fake["One"]
	child := contextOf(t, c, sid, "c1")
	release := make(chan struct{})
	s.page.mu.Lock()
	s.page.testApply = func(ev wipEvent) {
		if ev.method == "Page.frameNavigated" {
			<-release
		}
	}
	s.page.mu.Unlock()
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "c1", "parentId": "main", "loaderId": "c.2", "url": "https://c.example/2", "securityOrigin": "https://c.example"}})
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 8, "type": "normal", "frameId": "c1"}})
	eventually(t, time.Second, "the new child world recorded", func() bool { return s.pg.PS.Sched.FrameContext("c1").ID == 8 })
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	if err := evaluateErr(t, c, sid, "window.__oldChild = 1", child); !isNoContext(err) {
		t.Fatalf("evaluate in the navigated child's old context: %v, want the unknown-context error", err)
	}
	for _, e := range fp.expressions() {
		if strings.Contains(e, "__oldChild") {
			t.Fatalf("the old child context's expression ran in its new document: %v", fp.expressions())
		}
	}
	// The new document's context, which the attachment announced, runs.
	var newChild int
	eventually(t, time.Second, "the new child context", func() bool {
		for _, x := range contextsOf(t, c, 0, sid) {
			if x.AuxData.FrameID == "c1" && x.AuxData.IsDefault && x.ID != child {
				newChild = x.ID
			}
		}
		return newChild != 0
	})
	if err := evaluateErr(t, c, sid, "window.__newChild = 1", newChild); err != nil {
		t.Fatalf("evaluate in the new child context: %v", err)
	}
}

// Every send of an operation keeps the binding of its first one: after a
// commit replaced the target, the function call, the read by value and
// the releases go nowhere instead of to the new target, with a context
// and without one.
func TestBoundOperationKeepsItsTarget(t *testing.T) {
	for _, withContext := range []bool{true, false} {
		name := "no context"
		if withContext {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			r, _, c, sid, s := attachWithPriorContexts(t, false)
			f := r.fake["One"]
			id := 0
			if withContext {
				id = contextOf(t, c, sid, s.page.target)
			}
			mo, end, err := s.begin(ctxFor(t, 5*time.Second), "Runtime.evaluate", core.ClassMutate)
			if err != nil {
				t.Fatal(err)
			}
			defer end()
			b, err := s.bind(mo, id, "")
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if _, err := b.call("Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
				t.Fatalf("first call: %v", err)
			}
			waitResumed(t, f.Provisional("next"))
			f.Commit("next")
			eventually(t, time.Second, "commit applied", func() bool { return activeTarget(s.pg) == "next" })
			if _, err := b.call("Runtime.callFunctionOn", map[string]any{"objectId": "obj-old", "functionDeclaration": "function() {}"}); err == nil {
				t.Fatal("the function call after the commit succeeded")
			}
			if _, err := b.valueOf("obj-old"); err == nil {
				t.Fatal("the read by value after the commit succeeded")
			}
			b.release("obj-old", "a test object")
			for _, m := range []string{"Runtime.callFunctionOn", "Runtime.releaseObject", "Runtime.evaluate"} {
				for _, rec := range f.ReceivedMethod(m) {
					if rec.Target == "next" {
						t.Fatalf("%s %s reached the new target", m, rec.Params)
					}
				}
			}
		})
	}
}

// A provisional target that exceeded the staging bounds publishes a reset
// at its commit: the contexts are cleared, then the page's frame tree is
// read and announced as a new document with its default context; none of
// its staged events is sent (F5).
func TestOverflowResetPublishesFreshTree(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	waitResumed(t, f.Provisional("big"))
	f.EmitFrom("big", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.9", "url": "https://big.example/", "securityOrigin": "https://big.example"}})
	for range maxStagedEvents {
		f.EmitFrom("big", "Console.messageAdded", consoleFrom("staged"))
	}
	fp.mu.Lock()
	fp.loader, fp.url = 9, "https://big.example/"
	fp.mu.Unlock()
	seq := c.Seq()
	f.Commit("big")
	nav := waitEvent(t, c, seq, sid, "Page.frameNavigated", nil)
	if u := decode[navFrame](t, nav.Params).Frame.URL; u != "https://big.example/" {
		t.Fatalf("frameNavigated of %s, want the fresh tree's document", u)
	}
	cleared := sessionEvents(c, seq, sid, "Runtime.executionContextsCleared")
	if len(cleared) == 0 || cleared[0].Seq > nav.Seq {
		t.Fatalf("contexts cleared %v, want before the new document", cleared)
	}
	created := waitEvent(t, c, nav.Seq, sid, "Runtime.executionContextCreated", nil)
	if x := decode[struct{ Context cdpContext }](t, created.Params).Context; !x.AuxData.IsDefault {
		t.Fatalf("context after the reset %+v, want the default context", x)
	}
	for _, e := range sessionEvents(c, seq, sid, "Runtime.consoleAPICalled") {
		if strings.Contains(string(e.Params), "staged") {
			t.Fatalf("a staged event of the overflowed target was sent: %s", e.Params)
		}
	}
	got := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	if got.URL != "https://big.example/" {
		t.Fatalf("frame tree after the reset %+v", got)
	}
}

// A provisional target whose navigation the attachment never saw (it came
// before the attachment) commits without staged events: the old
// document's context is not proven for the committed target's recorded
// world (the reporting target differs, and the commit resets the
// document, F1), and nothing runs.
func TestContextProofRefusesAnotherTargetsWorld(t *testing.T) {
	r := newRig(t)
	fp := newFakePage(r.fake["One"])
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	eventually(t, time.Second, "connection open", func() bool { return f.ConnectionCount() > 0 })
	waitResumed(t, f.Provisional("early"))
	f.EmitFrom("early", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.2", "url": "https://early.example/", "securityOrigin": "https://early.example"}})
	f.EmitFrom("early", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 3, "type": "normal", "frameId": "main"}})
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att := eventsOf(c, "Target.attachedToTarget")
	sid := decode[attached](t, att[len(att)-1].Params).SessionID
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	_, s := r.pageSession(t, sid)
	main := contextOf(t, c, sid, s.page.target)
	f.Commit("early")
	eventually(t, time.Second, "the committed target's world recorded", func() bool {
		return activeTarget(s.pg) == "early" && s.pg.PS.Sched.FrameContext("main").ID == 3
	})
	if err := evaluateErr(t, c, sid, "window.__otherTarget = 1", main); !isNoContext(err) {
		t.Fatalf("evaluate: %v, want the unknown-context error", err)
	}
	for _, e := range fp.expressions() {
		if strings.Contains(e, "__otherTarget") {
			t.Fatalf("the expression ran in the committed target's document: %v", fp.expressions())
		}
	}
}
