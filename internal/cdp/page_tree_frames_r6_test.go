package cdp

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// A tree read is the truth for its document's frames at its place in the
// event stream (events.go seedLocked), and the client hears of every change
// that it makes, as Chrome's model requires (a CDP client reads the frame
// tree only when it attaches):
//   - a child frame that a tree read adds to the current document (WebKit
//     sends no frameAttached for the retained child frames of a document
//     that the back/forward cache restores) is announced with
//     frameAttached and frameNavigated, and its default context with
//     executionContextCreated when the page has one; a later read of the
//     same tree announces nothing again;
//   - a child frame that a tree read lacks is removed with frameDetached,
//     after the end of its contexts.
func TestTreeReadAnnouncesFrameChanges(t *testing.T) {
	r, fp, c, sid, s := attachWithPriorContexts(t, false)
	f := r.fake["One"]
	target := s.page.target
	// c1 appears without a frameAttached; its main world is reported while
	// the attachment does not know the frame yet.
	fp.mu.Lock()
	fp.children = []wipTree{{Frame: wipFrame{ID: "c1", ParentID: "main", LoaderID: "c.1", URL: "https://c.example/", SecurityOrigin: "https://c.example"}}}
	fp.mu.Unlock()
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "c1"}})
	eventually(t, time.Second, "c1's context recorded", func() bool { return s.pg.PS.Sched.FrameContext("c1").ID == 7 })
	isChild := func(p json.RawMessage) bool { return decode[navFrame](t, p).Frame.ID == "c1" }

	seq := c.Seq()
	call(t, c, sid, "Page.getFrameTree", nil)
	childNav := waitEvent(t, c, seq, sid, "Page.frameNavigated", isChild)
	ctxEv := waitEvent(t, c, seq, sid, "Runtime.executionContextCreated", func(p json.RawMessage) bool {
		x := decode[struct{ Context cdpContext }](t, p).Context
		return x.AuxData.FrameID == "c1" && x.AuxData.IsDefault
	})
	attached := sessionEvents(c, seq, sid, "Page.frameAttached")
	if len(attached) != 1 {
		t.Fatalf("frameAttached %v after the tree that adds c1, want one", attached)
	}
	att := decode[struct{ FrameID, ParentFrameID string }](t, attached[0].Params)
	nf := decode[navFrame](t, childNav.Params).Frame
	if att.FrameID != "c1" || att.ParentFrameID != target || nf.ParentID != target || nf.LoaderID == "" || nf.URL != "https://c.example/" {
		t.Fatalf("frameAttached %+v, child frameNavigated %+v; want c1 under %s at https://c.example/", att, nf, target)
	}
	if order := []int{attached[0].Seq, childNav.Seq, ctxEv.Seq}; !slices.IsSorted(order) {
		t.Fatalf("order frameAttached, frameNavigated, context = %v", order)
	}
	ctxID := decode[struct{ Context cdpContext }](t, ctxEv.Params).Context.ID

	seq = c.Seq()
	call(t, c, sid, "Page.getFrameTree", nil)
	if got := append(sessionEvents(c, seq, sid, "Page.frameAttached"), sessionEvents(c, seq, sid, "Page.frameNavigated")...); len(got) != 0 {
		t.Fatalf("the same tree again sent %v, want nothing", methodsOf(got))
	}

	fp.mu.Lock()
	fp.children = nil
	fp.mu.Unlock()
	seq = c.Seq()
	call(t, c, sid, "Page.getFrameTree", nil)
	detached := waitEvent(t, c, seq, sid, "Page.frameDetached", nil)
	if d := decode[struct{ FrameID, Reason string }](t, detached.Params); d.FrameID != "c1" || d.Reason != "remove" {
		t.Fatalf("frameDetached %+v, want c1 removed", d)
	}
	destroyed := sessionEvents(c, seq, sid, "Runtime.executionContextDestroyed")
	if len(destroyed) != 1 || decode[struct{ ExecutionContextID int }](t, destroyed[0].Params).ExecutionContextID != ctxID || destroyed[0].Seq > detached.Seq {
		t.Fatalf("executionContextDestroyed %v, frameDetached at %d; want context %d destroyed first", destroyed, detached.Seq, ctxID)
	}
}

// A child frame's first event can be its frameNavigated (WebKit sends no
// frameAttached for the retained child frames of a document restored from
// the back/forward cache): the client gets frameAttached first, once.
func TestUnknownChildNavigationAnnouncesFrame(t *testing.T) {
	ps, sent := recordingModel(t)
	before := len(sent())
	nav := json.RawMessage(`{"frame":{"id":"c1","parentId":"main","loaderId":"c.1","url":"https://c.example/"}}`)
	ps.dispatch("wk-0", "Page.frameNavigated", nav)
	ps.dispatch("wk-0", "Page.frameNavigated", nav)
	if got := sent()[before:]; !slices.Equal(got, []string{"Page.frameAttached", "Page.frameNavigated", "Page.frameNavigated"}) {
		t.Fatalf("sent %v, want frameAttached once, before the first frameNavigated", got)
	}
}

// At its place, a tree is the truth for the data of its document's child
// frames too (seedLocked): a frame that it keeps takes its loader and URL,
// with a new bridge loader id for a new WIP loader.
func TestTreeReadRefreshesKeptFrames(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	ps.onWire(at(1), "wk-0", "Page.frameAttached", json.RawMessage(`{"frameId":"c1","parentFrameId":"main"}`))
	ps.onWire(at(2), "wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"c1","parentId":"main","loaderId":"child.old","url":"https://old.example/"}}`))
	settle(t, ps)
	ps.mu.Lock()
	oldLoader := ps.frames["c1"].loader
	ps.mu.Unlock()
	tree := treeWithChild("0.1", "c1")
	tree.ChildFrames[0].Frame.LoaderID = "child.new"
	tree.ChildFrames[0].Frame.URL = "https://new.example/"
	replyAt(t, ps.startTreeRead(), at(3), "wk-0", tree)
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	f := ps.frames["c1"]
	if f == nil || f.wip.LoaderID != "child.new" || f.wip.URL != "https://new.example/" || f.loader == oldLoader || f.loader == "" {
		t.Fatalf("frame %+v (bridge loader before %s), want the tree's child.new at https://new.example/ with a new bridge loader", f, oldLoader)
	}
}

// A tree of another document than the attachment's is older or newer than
// the event stream, which brings or brought that document's own
// frameNavigated, so it is not applied (seedLocked): the document and its
// child frames stay, and nothing is sent.
func TestTreeOfOtherDocumentNotApplied(t *testing.T) {
	ps, sent := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	ps.onWire(at(1), "wk-0", "Page.frameAttached", json.RawMessage(`{"frameId":"c1","parentFrameId":"main"}`))
	settle(t, ps)
	before := len(sent())
	replyAt(t, ps.startTreeRead(), at(2), "wk-0", treeWithChild("0.6", "k"))
	settle(t, ps)
	if main, kids := frameState(ps); main != "0.1" || kids != "c1" {
		t.Fatalf("document %s with child frames [%s] after a tree of document 0.6, want 0.1 with [c1]", main, kids)
	}
	if got := sent()[before:]; len(got) != 0 {
		t.Fatalf("sent %v for a tree of another document, want nothing", got)
	}
}
