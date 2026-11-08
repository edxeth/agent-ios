package cdp

// The reproductions of the W5b-1 round-2 closure review
// (/home/devkit/.pi/artifacts/reviewer/w5b1-r2-20261108-102557.md,
// findings F1, F5, F6 and F7), kept as regression tests.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

func TestReviewR2CommitAfterPreAttachmentFrameReseeds(t *testing.T) {
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
	fp.mu.Lock()
	fp.url = "https://early.example/"
	fp.loader = 2
	fp.mu.Unlock()
	f.Commit("early")
	seq := c.Seq()
	syncConsole(t, r, c, seq, sid)
	got := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	if got.URL != "https://early.example/" {
		t.Fatalf("committed page never reseeded: active=%s treeURL=%s", f.ActiveTarget(), got.URL)
	}
}

func TestReviewR2EvictedTargetStillResetsBeyondLostMemory(t *testing.T) {
	ps, sent := recordingModel(t)
	for i := 0; i < maxStagedTargets+lostMemory+2; i++ {
		id := fmt.Sprintf("p%d", i)
		announce(ps, id)
		ps.dispatch(id, "Page.frameNavigated", navigatedTo(id))
	}
	before := len(sent())
	ps.dispatch("", evTargetCommitted, commitOf("wk-0", "p0"))
	if got := sent()[before:]; !resetSent(ps, got) {
		t.Fatalf("commit of evicted target p0 forgot the loss: sent=%v, main=%v, reseed=%v", got, ps.main, ps.reseed)
	}
}

func TestReviewR2OverflowMetadataIsBounded(t *testing.T) {
	ps, _ := recordingModel(t)
	payload := make(json.RawMessage, maxStagedBytes+1)
	for i := 0; i < 64; i++ {
		ps.mu.Lock()
		ps.applyLocked(wipEvent{target: fmt.Sprintf("p%d", i), method: "Console.messageAdded", params: payload, provisional: true, conn: &otherConn{n: i + 1}})
		ps.mu.Unlock()
	}
	ps.mu.Lock()
	n := len(ps.overflowed)
	targets := len(ps.staged)
	ps.mu.Unlock()
	t.Logf("64 oversized provisional sources retained %d overflow markers and %d staged target buffers", n, targets)
	if n > maxStagedTargets+lostMemory {
		t.Fatalf("overflow bookkeeping exceeded all target budgets: %d retained markers", n)
	}
}

func TestReviewR2SkippedTreeCannotPanic(t *testing.T) {
	for _, method := range []string{"Page.getFrameTree", "Page.getResourceTree", "Page.navigate", "Page.createIsolatedWorld"} {
		t.Run(method, func(t *testing.T) {
			r, fp, c, sid, target := attachPage(t)
			call(t, c, sid, "Page.getFrameTree", nil)
			_, s := r.pageSession(t, sid)
			s.page.mu.Lock()
			s.page.main = nil
			s.page.mu.Unlock()
			r.fake["One"].Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				s.page.mu.Lock()
				s.page.connSeq++
				s.page.mu.Unlock()
				fp.mu.Lock()
				defer fp.mu.Unlock()
				return map[string]any{"frameTree": map[string]any{"frame": fp.mainFrame(), "resources": []any{}}}, nil
			})
			handlers := map[string]func(context.Context, *Call) (any, Later, error){
				"Page.getFrameTree": getFrameTree, "Page.getResourceTree": getResourceTree,
				"Page.navigate": pageNavigate, "Page.createIsolatedWorld": createIsolatedWorld,
			}
			params, _ := json.Marshal(map[string]any{"url": "https://one.example/next", "frameId": target, "worldName": "utility"})
			defer func() {
				if p := recover(); p != nil {
					if method == "Page.navigate" {
						s.page.mu.Unlock()
					}
					t.Errorf("%s panicked after readTree legitimately skipped a stale connection: %v", method, p)
				}
			}()
			_, _, _ = handlers[method](ctxFor(t, 5*time.Second), &Call{Method: method, Params: params, session: s})
		})
	}
}

func TestReviewR2ResetAnnouncesNewLoader(t *testing.T) {
	ps, _ := recordingModel(t)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	frame := ps.main.wip
	old := ps.main.loader
	ps.resetLocked()
	ps.seedLocked("wk-0", wipTree{Frame: frame})
	if ps.main.loader == old {
		t.Fatalf("reset advertised a new document generation but reused loader %s", old)
	}
}
