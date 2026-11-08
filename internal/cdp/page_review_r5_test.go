package cdp

// The reproduction of the W5b-1 round-4 closure review
// (/home/devkit/.pi/artifacts/reviewer/w5b1-r4-20261108-122300.md, finding
// F10; /home/devkit/.pi/artifacts/reviewer/w5b1-r4/review_r4_extra_test.go),
// kept as a regression test. Round 5 applies a tree at the place of its
// reply in the event stream, so the reproduction queues the reply there
// (replyAt) instead of handing the tree to the removed applyTree, and
// checks the attachment once the read returned; the page's history, the
// lagging goroutine and the expected state are the reviewer's.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// A tree from the restored occurrence of A must not be assigned the
// first A generation and then lose its children to queued B/A events.
func TestReviewR4RestoredTreeKeepsChildren(t *testing.T) {
	conn := &otherConn{n: 1}
	ps, _ := startModel(t, conn)
	at := positionsOf(conn, "wk-0")
	a := json.RawMessage(`{"frame":{"id":"main","loaderId":"0.1","url":"https://a.example/"}}`)
	ps.onWire(at(1), "wk-0", "Page.frameNavigated", a)
	settle(t, ps) // A is the document at the start of the read.
	ps.mu.Lock()
	ps.resetLocked()
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Console.messageAdded" {
			once.Do(func() { close(paused); <-release })
		}
	}
	ps.mu.Unlock()
	rd := ps.startTreeRead()
	ps.onWire(at(2), "wk-0", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","level":"log","text":"A before leaving"}}`))
	<-paused
	ps.onWire(at(3), "wk-0", "Page.frameNavigated", navigatedTo("0.2")) // B
	ps.onWire(at(4), "wk-0", "Page.frameNavigated", a)                  // restored A
	tree := wipTree{Frame: wipFrame{ID: "main", LoaderID: "0.1", URL: "https://a.example/"}, ChildFrames: []wipTree{
		{Frame: wipFrame{ID: "c1", ParentID: "main", LoaderID: "child.a", URL: "https://child.example/"}},
	}}
	replyAt(t, rd, at(5), "wk-0", tree)
	close(release)
	if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main == nil || ps.main.wip.LoaderID != "0.1" || ps.frames["c1"] == nil {
		t.Fatalf("restored A tree lost its child to older queued events: main=%+v, frames=%v; want A with c1", ps.main, ps.frames)
	}
}
