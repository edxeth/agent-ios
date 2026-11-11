package cdp

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W5b-1 repair round 4 (review /home/devkit/.pi/artifacts/reviewer/w5b1-r3-20261108-113000.md,
// F9): every tree read is ordered with the attachment's event stream
// (staging.go, "Tree reads"), on every path that seeds a document.

// treeOrderRun is a page attachment whose next tree read waits until the
// test lets it answer, and whose goroutine stops before it handles the
// first Page.frameAttached.
type treeOrderRun struct {
	r         *rig
	fp        *fakePage
	f         *fakewebkit.Server
	c         *cdptest.Client
	sid       string
	s         *session
	read      chan struct{} // closed when the tree read reached the page
	answer    chan struct{} // close to let the tree read answer
	paused    chan struct{} // closed when the goroutine stopped at the frameAttached
	release   chan struct{} // close to let the goroutine go on
	readOnce  sync.Once
	pauseOnce sync.Once
}

func newTreeOrderRun(t *testing.T) *treeOrderRun {
	r, fp, c, sid, _ := attachPage(t)
	_, s := r.pageSession(t, sid)
	return &treeOrderRun{r: r, fp: fp, f: r.fake["One"], c: c, sid: sid, s: s, read: make(chan struct{}), answer: make(chan struct{}), paused: make(chan struct{}), release: make(chan struct{})}
}

// arm makes the next tree read wait and the goroutine stop at the first
// frameAttached.
func (w *treeOrderRun) arm(t *testing.T) {
	ps := w.s.page
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Page.frameAttached" {
			w.pauseOnce.Do(func() { close(w.paused); <-w.release })
		}
	}
	ps.mu.Unlock()
	t.Cleanup(func() {
		ps.mu.Lock()
		ps.testApply = nil
		ps.mu.Unlock()
	})
	w.f.Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		w.readOnce.Do(func() { close(w.read); <-w.answer })
		w.fp.mu.Lock()
		defer w.fp.mu.Unlock()
		return map[string]any{"frameTree": map[string]any{"frame": w.fp.mainFrame(), "resources": []any{}}}, nil
	})
}

// raceRead runs while the tree read waits: the current document attaches
// a child frame (its handling stops) and then the page loads a new
// document without it, which the read then reports; once the reply waits
// behind these events in the attachment's queue, the test lets the
// goroutine go on and checks that the child frame does not come back.
func (w *treeOrderRun) raceRead(t *testing.T) {
	t.Helper()
	ctx := testCtx(t)
	select {
	case <-w.read:
	case <-ctx.Done():
		t.Fatal("no tree read")
	}
	w.f.Emit("Page.frameAttached", map[string]any{"frameId": "ghost", "parentFrameId": "main"})
	select {
	case <-w.paused:
	case <-ctx.Done():
		t.Fatal("the child frame's event did not reach the attachment")
	}
	w.f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "ghost", "parentId": "main", "loaderId": "child.old", "url": "https://old-child.example/"}})
	w.fp.mu.Lock()
	w.fp.loader, w.fp.url = 7, "https://one.example/new-document"
	frame := w.fp.mainFrame()
	w.fp.mu.Unlock()
	w.f.Emit("Page.frameNavigated", map[string]any{"frame": frame})
	close(w.answer)
	ps := w.s.page
	waitUntil(t, "the tree read's reply queued while the goroutine lags", func() bool { return replyQueued(ps) })
	close(w.release)
	if err := ps.caughtUp(ctx); err != nil {
		t.Fatal(err)
	}
}

// noGhost checks that the attachment and Page.getFrameTree have no child
// frame.
func (w *treeOrderRun) noGhost(t *testing.T) {
	t.Helper()
	ps := w.s.page
	ps.mu.Lock()
	n := len(ps.frames)
	ps.mu.Unlock()
	got := call(t, w.c, w.sid, "Page.getFrameTree", nil)
	var tree struct {
		FrameTree struct{ ChildFrames []navFrame }
	}
	if err := json.Unmarshal(got, &tree); err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(tree.FrameTree.ChildFrames) != 0 {
		t.Fatalf("a child frame of the replaced document came back (%d frames): %s", n, got)
	}
}

// The first seed of an attachment (Page.enable, Runtime.enable or a Page
// handler on an attachment without a document) applies its tree at the
// place of its reply, after the events of the replaced document that the
// page sent before it.
func TestFirstSeedOrderedWithEvents(t *testing.T) {
	for _, method := range []string{"Page.enable", "Runtime.enable", "Page.getFrameTree"} {
		t.Run(method, func(t *testing.T) {
			w := newTreeOrderRun(t)
			w.arm(t)
			done := make(chan error, 1)
			go func() {
				_, err := w.c.Call(ctxFor(t, 10*time.Second), w.sid, method, nil)
				done <- err
			}()
			w.raceRead(t)
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			w.noGhost(t)
		})
	}
}

// The read after a reset for dropped events is ordered the same way
// (the reset at a commit without a document:
// TestReviewR3ResetTreeDoesNotResurrectOldChild; at a new connection:
// TestNewConnectionReadOrderedWithEvents).
func TestResetReadOrderedWithEvents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(t *testing.T, w *treeOrderRun)
	}{
		{"events lost", func(t *testing.T, w *treeOrderRun) {
			ps := w.s.page
			ps.mu.Lock()
			ps.eventsLostLocked()
			ps.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTreeOrderRun(t)
			call(t, w.c, w.sid, "Page.enable", nil)
			call(t, w.c, w.sid, "Runtime.enable", nil)
			w.arm(t)
			tc.trigger(t, w)
			w.raceRead(t)
			w.noGhost(t)
		})
	}
}

// The read after the reset at a new connection's first event: the tree
// is applied at the place of its reply, after the events read before it,
// so a child frame that the replaced document attached while the read ran
// stays gone. (A model with the attachment's goroutine: the fake page
// cannot hand its page over to a new connection without a lease takeover.)
func TestNewConnectionReadOrderedWithEvents(t *testing.T) {
	first, second := &otherConn{n: 1}, &otherConn{n: 2}
	ps, _ := startModel(t, first)
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Page.frameAttached" {
			once.Do(func() { close(paused); <-release })
		}
	}
	ps.mu.Unlock()
	at := positionsOf(second, "wk-0")
	ps.onWire(at(1), "wk-0", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","text":"first"}}`))
	settle(t, ps)
	rd := ps.startTreeRead() // the reset's read
	ps.onWire(at(2), "wk-0", "Page.frameAttached", json.RawMessage(`{"frameId":"ghost","parentFrameId":"main"}`))
	<-paused
	ps.onWire(at(3), "wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"ghost","parentId":"main","loaderId":"child.old"}}`))
	ps.onWire(at(4), "wk-0", "Page.frameNavigated", navigatedTo("0.9"))
	replyAt(t, rd, at(5), "wk-0", wipTree{Frame: wipFrame{ID: "main", LoaderID: "0.9", URL: "https://0.9.example/"}})
	close(release)
	if err := rd.applied(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main == nil || ps.main.wip.LoaderID != "0.9" || len(ps.frames) != 0 {
		t.Fatalf("after the new connection's reset: main %+v, frames %v; want document 0.9 without child frames", ps.main, ps.frames)
	}
}

// A tree is applied only after the events read before its reply. Here the
// stale main-frame frameNavigated of an older document, read before the
// reply, must not replace the reply's newer tree after it was applied;
// the read does not return while that event waits.
func TestTreeAppliedAfterEarlierEvents(t *testing.T) {
	ps, _ := startModel(t, &otherConn{n: 1})
	at := positionsOf(ps.conn, "wk-0")
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.mu.Lock()
	ps.resetLocked()
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Page.frameNavigated" {
			once.Do(func() { close(paused); <-release })
		}
	}
	ps.mu.Unlock()
	ps.onWire(at(1), "wk-0", "Page.frameNavigated", navigatedTo("0.5"))
	<-paused
	rd := ps.startTreeRead()
	ps.onWire(at(2), "wk-0", "Page.frameNavigated", navigatedTo("0.6"))
	replyAt(t, rd, at(3), "wk-0", wipTree{Frame: wipFrame{ID: "main", LoaderID: "0.6", URL: "https://0.6.example/"}})
	applied := make(chan error, 1)
	go func() { applied <- rd.applied(ctxFor(t, 5*time.Second)) }()
	select {
	case err := <-applied:
		t.Fatalf("the read returned (%v) before an event read before its reply was handled", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main == nil || ps.main.wip.LoaderID != "0.6" {
		t.Fatalf("document %+v, want the read's 0.6", ps.main)
	}
}
