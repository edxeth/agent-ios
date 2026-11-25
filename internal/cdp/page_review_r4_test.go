package cdp

// The reproduction of the W5b-1 round-3 closure review
// (/home/devkit/.pi/artifacts/reviewer/w5b1-r3-20261108-113000.md,
// finding F9), kept as a regression test.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// A commit reseed may read ahead of the attachment's queued events.
// Events from a previous document of the SAME target must not be applied
// to that fresh tree (design 2.7 document generation; 5.2 frame identity).
func TestReviewR3ResetTreeDoesNotResurrectOldChild(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	_, s := r.pageSession(t, sid)
	ps := s.page
	f := r.fake["One"]
	readStarted, allowTree := make(chan struct{}), make(chan struct{})
	paused, release := make(chan struct{}), make(chan struct{})
	var readOnce, pauseOnce sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Page.frameAttached" {
			pauseOnce.Do(func() { close(paused); <-release })
		}
	}
	ps.mu.Unlock()
	defer func() { ps.mu.Lock(); ps.testApply = nil; ps.mu.Unlock() }()
	f.Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		readOnce.Do(func() { close(readStarted); <-allowTree })
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return map[string]any{"frameTree": map[string]any{"frame": fp.mainFrame(), "resources": []any{}}}, nil
	})
	waitResumed(t, f.Provisional("next"))
	f.Commit("next") // no staged document, starts the reseed
	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not read")
	}
	// After the commit, document 0.2 creates a child; it navigates to 0.3
	// before the tree read returns. Both events are on the same target.
	f.EmitFrom("next", "Page.frameAttached", map[string]any{"frameId": "ghost", "parentFrameId": "main"})
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("child event not queued")
	}
	f.EmitFrom("next", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "ghost", "parentId": "main", "loaderId": "child.old", "url": "https://old-child.example/"}})
	fp.mu.Lock()
	fp.loader = 3
	fp.url = "https://one.example/new-document"
	frame := fp.mainFrame()
	fp.mu.Unlock()
	f.EmitFrom("next", "Page.frameNavigated", map[string]any{"frame": frame})
	f.EmitFrom("next", "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 3, "type": "normal", "frameId": "main"}})
	close(allowTree)
	// Round 5 applies the tree at the place of its reply, behind the
	// queued events, so the race is set up by the reply waiting in the
	// queue while the old child's event is held (the reviewer's version
	// waited for the tree to be seeded ahead of that event).
	eventually(t, 5*time.Second, "fresh tree's reply queued before old child is applied", func() bool { return replyQueued(ps) })
	close(release)
	if err := ps.caughtUp(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := call(t, c, sid, "Page.getFrameTree", nil)
	var tree struct {
		FrameTree struct{ ChildFrames []navFrame }
	}
	if err := json.Unmarshal(got, &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.FrameTree.ChildFrames) != 0 {
		t.Fatalf("reset resurrected old-document child in current tree: %s", got)
	}
}
