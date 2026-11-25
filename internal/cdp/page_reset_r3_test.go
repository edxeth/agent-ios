package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W5b-1 repair round 3 (review /home/devkit/.pi/artifacts/reviewer/w5b1-r2-20261108-102557.md):
// commits that do not give the committed target's document (F1), bounded
// loss bookkeeping (F5), no document after a reset (F6), the reset's
// child-frame announcements and the first-send retry (F8), and handler
// panics.

// A commit whose staged events give no document of the committed target
// resets the document (F1): none were staged, or only events that are no
// main-frame document.
func TestCommitWithoutDocumentResets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		staged []string
	}{
		{"nothing staged", nil},
		{"no main-frame document staged", []string{"Console.messageAdded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, sent := recordingModel(t)
			announce(ps, "next")
			for _, m := range tc.staged {
				ps.dispatch("next", m, json.RawMessage(`{"message":{"source":"console-api","text":"x"}}`))
			}
			before := len(sent())
			ps.dispatch("", evTargetCommitted, commitOf("wk-0", "next"))
			got := sent()[before:]
			ps.mu.Lock()
			defer ps.mu.Unlock()
			if len(got) == 0 || got[len(got)-1] != "Runtime.executionContextsCleared" || ps.main != nil || !ps.reseed {
				t.Fatalf("commit sent %v, main %v, reseed %v; want a reset", got, ps.main, ps.reseed)
			}
		})
	}
}

// The reset at a commit without the target's document keeps the loader
// ids: the committed target's documents have their own keys, and an id
// that a translator of the target's events gave the document (W5c's
// document request, events.go) stays the document's loader id when its
// frameNavigated comes after the commit (5.2: requestId == loaderId).
func TestCommitResetKeepsLoaderIdentity(t *testing.T) {
	ps, _ := recordingModel(t)
	announce(ps, "next")
	ps.mu.Lock()
	id := ps.loaderFor("next", "0.2")
	ps.mu.Unlock()
	ps.dispatch("", evTargetCommitted, commitOf("wk-0", "next"))
	ps.dispatch("next", "Page.frameNavigated", navigatedTo("0.2"))
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main == nil || ps.main.target != "next" || ps.main.loader != id {
		t.Fatalf("document after the commit %+v, want target next with loader %s", ps.main, id)
	}
}

// When the ring of lost targets overwrites a target, whose commit could
// still come, the next commit resets even when the attachment already
// has the committed target's document (a tree read after the commit
// seeded it); without the overwrite it does not (F5).
func TestLostRingOverwriteResetsNextCommit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		evictions int
		reset     bool
	}{
		{"ring exact", lostMemory, false},
		{"ring overwrote", lostMemory + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, sent := recordingModel(t)
			for i := range maxStagedTargets + tc.evictions {
				id := fmt.Sprintf("p%d", i)
				announce(ps, id)
				ps.dispatch(id, "Page.frameNavigated", navigatedTo(id))
			}
			ps.mu.Lock()
			ps.resetLocked()
			ps.seedLocked("seen", wipTree{Frame: wipFrame{ID: "main", LoaderID: "0.5", URL: "https://seen.example/"}})
			ps.mu.Unlock()
			before := len(sent())
			ps.dispatch("", evTargetCommitted, commitOf("wk-0", "seen"))
			if got := sent()[before:]; resetSent(ps, got) != tc.reset {
				t.Fatalf("commit sent %v (main %v), want a reset: %v", got, ps.main, tc.reset)
			}
		})
	}
}

// Overflow marks belong to staged targets: when overflowing targets push
// each other out, at most maxStagedTargets marks stay, and a pushed-out
// target's commit still resets (F5).
func TestOverflowMarksStayBounded(t *testing.T) {
	ps, sent := recordingModel(t)
	payload := json.RawMessage(`{"message":{"source":"console-api","text":"` + strings.Repeat("x", maxStagedBytes) + `"}}`)
	for i := range 3 * maxStagedTargets {
		id := fmt.Sprintf("big%d", i)
		announce(ps, id)
		ps.dispatch(id, "Console.messageAdded", payload)
	}
	ps.mu.Lock()
	marks, staged := len(ps.overflowed), len(ps.staged)
	ps.mu.Unlock()
	if marks > maxStagedTargets || staged > maxStagedTargets {
		t.Fatalf("%d overflow marks and %d staged targets, want at most %d", marks, staged, maxStagedTargets)
	}
	before := len(sent())
	ps.dispatch("", evTargetCommitted, commitOf("wk-0", "big0"))
	if got := sent()[before:]; !resetSent(ps, got) {
		t.Fatalf("commit of a pushed-out overflowed target sent %v, want a reset", got)
	}
}

// At most maxLiveTargets targets are remembered as no provisional ones:
// a new one pushes out the oldest, and a destruction forgets its target
// (F5).
func TestLiveTargetsBounded(t *testing.T) {
	ps, _ := recordingModel(t)
	for i := range maxLiveTargets + 1 {
		ps.dispatch("", evTargetCreated, json.RawMessage(fmt.Sprintf(`{"targetInfo":{"targetId":"w%d","type":"worker"}}`, i)))
	}
	ps.dispatch("", evTargetDestroyed, json.RawMessage(`{"targetId":"w1"}`))
	ps.mu.Lock()
	defer ps.mu.Unlock()
	newest := fmt.Sprintf("w%d", maxLiveTargets)
	if len(ps.live) != maxLiveTargets-1 || len(ps.liveOrder) != len(ps.live) || ps.live["w0"] || ps.live["w1"] || !ps.live[newest] {
		t.Fatalf("live %d (order %d), oldest kept %v, destroyed kept %v, newest kept %v", len(ps.live), len(ps.liveOrder), ps.live["w0"], ps.live["w1"], ps.live[newest])
	}
}

// After a reset the attachment has no document until a tree read seeds
// the new one. A handler that needs it fails with a transient error that
// tells the client to retry, runs nothing in the page and leaves the
// attachment unlocked (F6).
func TestNoDocumentFailsTransiently(t *testing.T) {
	handlers := map[string]Handler{
		"Page.getFrameTree": getFrameTree, "Page.getResourceTree": getResourceTree,
		"Page.navigate": pageNavigate, "Page.createIsolatedWorld": createIsolatedWorld,
	}
	for method, h := range handlers {
		t.Run(method, func(t *testing.T) {
			r, fp, c, sid, target := attachPage(t)
			call(t, c, sid, "Page.getFrameTree", nil)
			_, s := r.pageSession(t, sid)
			ps := s.page
			ps.mu.Lock()
			ps.main = nil
			ps.mu.Unlock()
			r.fake["One"].Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				ps.mu.Lock()
				ps.connSeq++ // a reset during the read: it is not seeded
				ps.mu.Unlock()
				fp.mu.Lock()
				defer fp.mu.Unlock()
				return map[string]any{"frameTree": map[string]any{"frame": fp.mainFrame(), "resources": []any{}}}, nil
			})
			params, _ := json.Marshal(map[string]any{"url": "https://one.example/next", "frameId": target, "worldName": "utility"})
			_, later, err := h(ctxFor(t, 5*time.Second), &Call{Method: method, Params: params, session: s})
			if err == nil || later != nil || !strings.Contains(err.Error(), method) || !strings.Contains(err.Error(), "retry the command") {
				t.Fatalf("%s without a document: later %v, err %v; want a transient error", method, later != nil, err)
			}
			if !ps.mu.TryLock() {
				t.Fatalf("%s left the attachment locked", method)
			}
			ps.mu.Unlock()
			for _, e := range fp.expressions() {
				if strings.Contains(e, "location.href") {
					t.Fatalf("%s navigated without a document: %q", method, e)
				}
			}
		})
	}
}

// A tree read that a reset overtakes is not seeded: the reset's own read
// seeds the document, and the overtaken read cannot replace it with the
// older tree.
func TestTreeReadAcrossResetIsNotSeeded(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	_, s := r.pageSession(t, sid)
	call(t, c, sid, "Page.getFrameTree", nil)
	ps := s.page
	var mu sync.Mutex
	reads := 0
	firstDone := make(chan struct{})
	r.fake["One"].Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		frame := fp.mainFrame()
		if n == 1 {
			ps.mu.Lock()
			ps.resetLocked() // events were lost during the read
			ps.mu.Unlock()
		} else {
			<-firstDone
			frame["loaderId"], frame["url"] = "0.7", "https://one.example/after"
		}
		return map[string]any{"frameTree": map[string]any{"frame": frame, "resources": []any{}}}, nil
	})
	mo, end, err := s.begin(ctxFor(t, 5*time.Second), "Page.getResourceTree", core.ClassRead)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ps.readTree(mo.Op.Context(), mo)
	close(firstDone)
	end()
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	eventually(t, 5*time.Second, "the reset's read seeded", func() bool {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return ps.main != nil
	})
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main.wip.URL != "https://one.example/after" {
		t.Fatalf("document after the reset %+v, want the reset's own read", ps.main.wip)
	}
}

// swapHandler replaces the handler of the method name for the test.
func swapHandler(t *testing.T, name string, h Handler) {
	tableMu.Lock()
	m := table[name]
	old := m.Handle
	m.Handle = h
	table[name] = m
	tableMu.Unlock()
	t.Cleanup(func() {
		tableMu.Lock()
		m := table[name]
		m.Handle = old
		table[name] = m
		tableMu.Unlock()
	})
}

// A panic in a handler or in its Later is the command's error, with the
// stack in the daemon log; the daemon and the CDP session go on.
func TestHandlerPanicBecomesError(t *testing.T) {
	const method = "Browser.getWindowForTarget"
	for _, tc := range []struct {
		name string
		h    Handler
	}{
		{"handler", func(context.Context, *Call) (any, Later, error) { panic("handler bug") }},
		{"later", func(context.Context, *Call) (any, Later, error) {
			return nil, func(context.Context) (any, error) { panic("later bug") }, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, c, sid, _ := attachPage(t)
			swapHandler(t, method, tc.h)
			_, err := c.Call(ctxFor(t, 5*time.Second), sid, method, nil)
			if err == nil || !strings.Contains(err.Error(), method) || !strings.Contains(err.Error(), tc.name+" bug") {
				t.Fatalf("%s reply: %v, want the internal error", method, err)
			}
			// The log names the panic by the type of its value, never by
			// the value, which can hold page text (logsafe.Panic, review
			// devfix-R2 F5); the stack locates it.
			if log := r.log.text(); !strings.Contains(log, method+" panicked: a panic value of type string") || !strings.Contains(log, "goroutine ") || strings.Contains(log, tc.name+" bug") {
				t.Fatalf("daemon log without the panic and its stack, or with the panic's message:\n%s", log)
			}
			call(t, c, sid, "Page.getFrameTree", nil)
		})
	}
}

// A reset announces the child frames of the fresh tree on the page
// session: frameAttached with the page's frame id as the parent, then
// frameNavigated with a new loader id, after the main frame's new
// document; with Runtime enabled the child frame's default context comes
// again after the contexts were cleared (F8, events.go seedLocked).
func TestResetAnnouncesChildFrames(t *testing.T) {
	_, _, c, sid, s := attachWithPriorContexts(t, true)
	target := s.page.target
	tree := decode[struct {
		FrameTree struct {
			ChildFrames []navFrame `json:"childFrames"`
		}
	}](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree
	if len(tree.ChildFrames) != 1 || tree.ChildFrames[0].Frame.ID != "c1" {
		t.Fatalf("fixture: child frames %+v", tree.ChildFrames)
	}
	oldLoader := tree.ChildFrames[0].Frame.LoaderID
	seq := c.Seq()
	s.page.mu.Lock()
	s.page.eventsLostLocked()
	s.page.mu.Unlock()
	isChild := func(p json.RawMessage) bool { return decode[navFrame](t, p).Frame.ID == "c1" }
	childNav := waitEvent(t, c, seq, sid, "Page.frameNavigated", isChild)
	nf := decode[navFrame](t, childNav.Params).Frame
	if nf.ParentID != target || nf.LoaderID == "" || nf.LoaderID == oldLoader {
		t.Fatalf("child frameNavigated %+v, want parent %s and a loader other than %s", nf, target, oldLoader)
	}
	ctxEv := waitEvent(t, c, seq, sid, "Runtime.executionContextCreated", func(p json.RawMessage) bool {
		x := decode[struct{ Context cdpContext }](t, p).Context
		return x.AuxData.FrameID == "c1" && x.AuxData.IsDefault
	})
	attachedEvs := sessionEvents(c, seq, sid, "Page.frameAttached")
	cleared := sessionEvents(c, seq, sid, "Runtime.executionContextsCleared")
	mainNav := waitEvent(t, c, seq, sid, "Page.frameNavigated", func(p json.RawMessage) bool { return decode[navFrame](t, p).Frame.ID == target })
	if len(attachedEvs) != 1 || len(cleared) != 1 {
		t.Fatalf("frameAttached %v, executionContextsCleared %v; want one each", attachedEvs, cleared)
	}
	att := decode[struct{ FrameID, ParentFrameID string }](t, attachedEvs[0].Params)
	if att.FrameID != "c1" || att.ParentFrameID != target {
		t.Fatalf("frameAttached %+v, want c1 under %s", att, target)
	}
	if order := []int{cleared[0].Seq, mainNav.Seq, attachedEvs[0].Seq, childNav.Seq}; !slices.IsSorted(order) || ctxEv.Seq < cleared[0].Seq {
		t.Fatalf("order cleared, main frameNavigated, frameAttached, child frameNavigated = %v; child context at %d", order, ctxEv.Seq)
	}
	for _, e := range c.Events() {
		if e.Seq > seq && e.SessionID != sid && (strings.HasPrefix(e.Method, "Page.") || strings.HasPrefix(e.Method, "Runtime.")) {
			t.Fatalf("reset event %s on session %q, want only the page session %q", e.Method, e.SessionID, sid)
		}
	}
}

// Without a context, an operation's first send goes to the target active
// at the send: when a commit replaced the target read at the bind before
// anything was sent, the send goes once to the new active target. After
// a send, a commit fails the operation's later sends instead (F8,
// runtime_domain.go boundOp.send).
func TestBoundOperationRetriesUnsentFirstSend(t *testing.T) {
	r, _, _, _, s := attachWithPriorContexts(t, false)
	f := r.fake["One"]
	mo, end, err := s.begin(ctxFor(t, 5*time.Second), "Runtime.evaluate", core.ClassMutate)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	b, err := s.bind(mo, 0, "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	waitResumed(t, f.Provisional("next"))
	f.Commit("next")
	eventually(t, time.Second, "commit applied", func() bool { return activeTarget(s.pg) == "next" })
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	if _, err := b.call("Runtime.evaluate", map[string]any{"expression": "'first'"}); err != nil {
		t.Fatalf("first send after the commit: %v", err)
	}
	if got := f.ReceivedMethod("Runtime.evaluate")[before:]; len(got) != 1 || got[0].Target != "next" {
		t.Fatalf("first send reached %+v, want one evaluate on the new target", got)
	}
	waitResumed(t, f.Provisional("third"))
	f.Commit("third")
	eventually(t, time.Second, "second commit applied", func() bool { return activeTarget(s.pg) == "third" })
	if _, err := b.call("Runtime.evaluate", map[string]any{"expression": "'second'"}); err == nil {
		t.Fatal("a send after a commit that replaced the operation's target succeeded")
	}
	for _, rec := range f.ReceivedMethod("Runtime.evaluate") {
		if rec.Target == "third" {
			t.Fatalf("a send after the first one followed the commit to %s: %s", rec.Target, rec.Params)
		}
	}
}
