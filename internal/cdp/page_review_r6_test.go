package cdp

// The reproductions of the W5b-1 round-5 closure review
// (/home/devkit/.pi/artifacts/reviewer/w5b1-r5-20261108-134700.md, finding
// F11; /home/devkit/.pi/artifacts/reviewer/w5b1-r5/review_r5_extra_test.go),
// kept unchanged as regression tests. Each queues a tree read's reply
// before an event that the connection read earlier, as round 5's hand-over
// could; round 6 keeps that order in core (pageConn.replyRead) and, as these
// tests check, discards no such event and makes the tree the truth for its
// document's frames.

import (
	"encoding/json"
	"testing"
)

// A reply that overtakes held events cannot replace transitions which
// seedLocked does not reconcile, nor records absent from a frame tree.
func TestReviewR5LateRemovalNotReflectedByTreeMerge(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	ps.onWire(at(1), "wk-0", "Page.frameAttached", json.RawMessage(`{"frameId":"c1","parentFrameId":"main"}`))
	ps.onWire(at(2), "wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"c1","parentId":"main","loaderId":"child.1","url":"https://child.example/"}}`))
	settle(t, ps)
	rd := ps.startTreeRead()
	// The backend removed c1 at 3, but handover delivers that event only
	// after the tree's reply at 5.
	replyAt(t, rd, at(5), "wk-0", treeWithChild("0.1", ""))
	ps.onWire(at(3), "wk-0", "Page.frameDetached", json.RawMessage(`{"frameId":"c1"}`))
	settle(t, ps)
	if main, kids := frameState(ps); main != "0.1" || kids != "" {
		t.Fatalf("tree without c1 plus its late removal retained child: main=%s children=%s; want 0.1 without children", main, kids)
	}
}

func TestReviewR5LateContextNotReflectedByTree(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	rd := ps.startTreeRead()
	replyAt(t, rd, at(5), "wk-0", treeWithChild("0.1", "c1"))
	ps.onWire(at(3), "wk-0", "Runtime.executionContextCreated", json.RawMessage(`{"context":{"id":7,"type":"normal","frameId":"c1"}}`))
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, c := range ps.ctxs {
		if c.wipFrame == "c1" && c.world == "" {
			return
		}
	}
	t.Fatalf("late current-document child context disappeared after a tree which carries no contexts; contexts=%+v", ps.ctxs)
}

func TestReviewR5LateConsoleNotReflectedByTree(t *testing.T) {
	ps, sent := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	before := len(sent())
	rd := ps.startTreeRead()
	replyAt(t, rd, at(5), "wk-0", treeWithChild("0.1", ""))
	ps.onWire(at(3), "wk-0", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","level":"log","text":"current document"}}`))
	settle(t, ps)
	count := 0
	for _, m := range sent()[before:] {
		if m == "Runtime.consoleAPICalled" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("tree erased current-document console record: got %d events, want 1", count)
	}
}

func TestReviewR5LateChildNavigationNotReflectedByTreeMerge(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	ps.onWire(at(1), "wk-0", "Page.frameAttached", json.RawMessage(`{"frameId":"c1","parentFrameId":"main"}`))
	ps.onWire(at(2), "wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"c1","parentId":"main","loaderId":"child.old","url":"https://old.example/"}}`))
	settle(t, ps)
	rd := ps.startTreeRead()
	tree := treeWithChild("0.1", "c1")
	tree.ChildFrames[0].Frame.LoaderID = "child.new"
	tree.ChildFrames[0].Frame.URL = "https://new.example/"
	replyAt(t, rd, at(5), "wk-0", tree)
	ps.onWire(at(3), "wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"c1","parentId":"main","loaderId":"child.new","url":"https://new.example/"}}`))
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	f := ps.frames["c1"]
	if f == nil || f.wip.LoaderID != "child.new" || f.wip.URL != "https://new.example/" {
		t.Fatalf("late current-document child navigation lost despite newer tree: frame=%+v; want child.new at new.example", f)
	}
}
