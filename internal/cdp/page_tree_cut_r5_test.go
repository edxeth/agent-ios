package cdp

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// W5b-1 repair round 5 (review /home/devkit/.pi/artifacts/reviewer/w5b1-r4-20261108-122300.md,
// F10): a tree read is applied at the place of its reply in the event
// stream (staging.go, "Tree reads"), not at a document generation guessed
// from the tree's document identity, which a back/forward restore repeats.

// positionsOf returns the position of the seq-th message that conn read
// while target was its active target.
func positionsOf(conn webkit.Caller, active string) func(seq uint64) core.EventPosition {
	return func(seq uint64) core.EventPosition {
		return core.EventPosition{Conn: conn, ActiveTarget: active, Seq: seq}
	}
}

// replyAt queues the reply of rd with tree from target at pos, as the read
// loop does when the connection reads it (treeRead.replied).
func replyAt(t *testing.T, rd *treeRead, pos core.EventPosition, target string, tree wipTree) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"frameTree": tree})
	if err != nil {
		t.Fatal(err)
	}
	rd.replied(pos, target, raw)
}

// replyQueued reports whether a tree read's reply waits in the attachment's
// queue.
func replyQueued(ps *pageState) bool {
	ps.q.mu.Lock()
	defer ps.q.mu.Unlock()
	for _, ev := range ps.q.items {
		if ev.tree != nil {
			return true
		}
	}
	return false
}

// pauseAt makes the attachment's goroutine stop before it handles the
// event of position seq, until the returned release is called; paused is
// closed when it stopped there.
func pauseAt(t *testing.T, ps *pageState, seq uint64) (paused <-chan struct{}, release func()) {
	t.Helper()
	p, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.seq == seq {
			once.Do(func() { close(p); <-r })
		}
	}
	ps.mu.Unlock()
	var released sync.Once
	stop := func() { released.Do(func() { close(r) }) }
	t.Cleanup(stop)
	return p, stop
}

// frameState returns the attachment's main-frame WIP loader ("" without a
// document) and its child frames, sorted.
func frameState(ps *pageState) (string, string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	main := ""
	if ps.main != nil {
		main = ps.main.wip.LoaderID
	}
	kids := ps.framesLocked()
	if ps.main != nil {
		kids = kids[1:]
	}
	return main, strings.Join(kids, ",")
}

func treeWithChild(loader, child string) wipTree {
	tree := wipTree{Frame: wipFrame{ID: "main", LoaderID: loader, URL: "https://" + loader + ".example/"}}
	if child != "" {
		tree.ChildFrames = []wipTree{{Frame: wipFrame{ID: child, ParentID: "main", LoaderID: child + ".1", URL: "https://" + child + ".example/"}}}
	}
	return tree
}

// The reply of a tree read is applied at its place among the events: an
// event read just before the reply is applied before the tree, one read
// just after it after the tree. The goroutine lags behind both, so the
// order comes from the stream, not from the time the read returns.
func TestTreeReplyCut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		after      bool   // a removal of the tree's child frame k is read just after the reply
		wantFrames string // the attachment's child frames at the end
	}{
		// The new document 0.7 is read just before the reply, so the tree
		// of 0.7 is applied to it and adds k (a tree applied before that
		// event would find document 0.1 and add nothing).
		{"event just before the reply", false, "k"},
		// k is removed just after the reply, so it stays removed (a tree
		// applied after that event would bring k back).
		{"event just after the reply", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, _ := startModel(t, &otherConn{n: 1})
			at := positionsOf(ps.conn, "wk-0")
			paused, release := pauseAt(t, ps, 1)
			rd := ps.startTreeRead()
			ps.onWire(at(1), "wk-0", "Page.frameNavigated", navigatedTo("0.7"))
			<-paused
			replyAt(t, rd, at(2), "wk-0", treeWithChild("0.7", "k"))
			if tc.after {
				ps.onWire(at(3), "wk-0", "Page.frameDetached", json.RawMessage(`{"frameId":"k"}`))
			}
			release()
			if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
				t.Fatal(err)
			}
			settle(t, ps)
			if main, kids := frameState(ps); main != "0.7" || kids != tc.wantFrames {
				t.Fatalf("document %q with child frames [%s], want 0.7 with [%s]", main, kids, tc.wantFrames)
			}
		})
	}
}

// Repeated identities: during one read the page goes A, B, A, B, A (back
// and forward through its back/forward cache, which restores a document
// with its target, its loader and its child frames, and reports only the
// main frame's frameNavigated). The place of the reply orders the tree,
// wherever the reply falls among the repeated documents.
func TestTreeReplyRepeatedIdentities(t *testing.T) {
	const docA, docB = "0.1", "0.2" // the model starts on A, of target wk-0
	t.Run("reply after the last restore", func(t *testing.T) {
		ps, _ := startModel(t, &otherConn{n: 1})
		at := positionsOf(ps.conn, "wk-0")
		paused, release := pauseAt(t, ps, 1)
		rd := ps.startTreeRead()
		for i, l := range []string{docB, docA, docB, docA} {
			ps.onWire(at(uint64(i+1)), "wk-0", "Page.frameNavigated", navigatedTo(l))
		}
		<-paused
		replyAt(t, rd, at(5), "wk-0", treeWithChild(docA, "c1"))
		release()
		if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		settle(t, ps)
		if main, kids := frameState(ps); main != docA || kids != "c1" {
			t.Fatalf("document %q with child frames [%s], want the restored A with c1", main, kids)
		}
	})
	t.Run("reply between the restores", func(t *testing.T) {
		ps, _ := startModel(t, &otherConn{n: 1})
		at := positionsOf(ps.conn, "wk-0")
		paused, release := pauseAt(t, ps, 1)
		var atNext struct{ main, kids string } // the attachment when it gets to the event after the reply
		ps.mu.Lock()
		pause := ps.testApply
		ps.testApply = func(ev wipEvent) {
			pause(ev)
			if ev.seq == 4 {
				atNext.main, atNext.kids = frameState(ps)
			}
		}
		ps.mu.Unlock()
		rd := ps.startTreeRead()
		ps.onWire(at(1), "wk-0", "Page.frameNavigated", navigatedTo(docB))
		ps.onWire(at(2), "wk-0", "Page.frameNavigated", navigatedTo(docA))
		<-paused
		replyAt(t, rd, at(3), "wk-0", treeWithChild(docA, "c1"))
		ps.onWire(at(4), "wk-0", "Page.frameNavigated", navigatedTo(docB))
		ps.onWire(at(5), "wk-0", "Page.frameNavigated", navigatedTo(docA))
		release()
		if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		settle(t, ps)
		if atNext.main != docA || atNext.kids != "c1" {
			t.Fatalf("at the event after the reply the attachment had %q with [%s], want the tree's A with c1", atNext.main, atNext.kids)
		}
		if main, _ := frameState(ps); main != docA {
			t.Fatalf("document %q at the end, want the last restored A", main)
		}
	})
}

// The reply of a target that a commit replaced before the connection read
// the reply is not applied, also when the attachment has no document: the
// replaced target's document is not the page's (F1).
func TestTreeReplyOfReplacedTargetNotApplied(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	ps.mu.Lock()
	ps.resetLocked()
	ps.mu.Unlock()
	rd := ps.startTreeRead()
	replyAt(t, rd, positionsOf(ps.conn, "next")(1), "wk-0", treeWithChild("0.1", ""))
	if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if main, _ := frameState(ps); main != "" {
		t.Fatalf("the replaced target's tree was applied: document %q", main)
	}
}

// A tree reply larger than the queue's byte bound is still queued and
// applied: its bytes are the read's own reply, which the read holds
// anyway, so they do not count against the bound of queued events.
func TestLargeTreeReplyApplied(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	tree := treeWithChild("0.1", "k")
	tree.Resources = []json.RawMessage{json.RawMessage(`"` + strings.Repeat("x", maxQueuedBytes) + `"`)}
	rd := ps.startTreeRead()
	replyAt(t, rd, positionsOf(ps.conn, "wk-0")(1), "wk-0", tree)
	if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if main, kids := frameState(ps); main != "0.1" || kids != "k" {
		t.Fatalf("document %q with child frames [%s], want 0.1 with the tree's k", main, kids)
	}
}

// The reply of a read on a new connection is the first message of that
// connection that reaches the attachment: like an event, it resets the
// document (the page may have navigated while no connection delivered its
// events), and its tree, read before that reset, is not applied; the
// reset's own read seeds the document.
func TestTreeReplyOfNewConnectionResets(t *testing.T) {
	ps, sent := startModel(t, &otherConn{n: 1})
	before := len(sent())
	rd := ps.startTreeRead()
	replyAt(t, rd, positionsOf(&otherConn{n: 2}, "wk-0")(1), "wk-0", treeWithChild("0.1", "k"))
	if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if main, _ := frameState(ps); main != "" {
		t.Fatalf("document %q after a new connection's reply, want none until the reset's read", main)
	}
	if got := sent()[before:]; len(got) != 1 || got[0] != "Runtime.executionContextsCleared" {
		t.Fatalf("sent %v, want the reset's executionContextsCleared", got)
	}
}
