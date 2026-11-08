package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Unit tests of staging.go on a pageState model: the bounds of the staged
// events and of the queue, and the reset that replaces an incomplete
// document (W5b-1 review F5), and the reset at a new connection.

// recordingModel is a pageState model with Page and Runtime events on that
// records the methods it sends, with the main document of target "wk-0"
// (WIP loader 0.1) and its main world.
func recordingModel(t *testing.T) (*pageState, func() []string) {
	t.Helper()
	var sent []string
	ps := newPageModel("PAGE", func(method string, _ any) { sent = append(sent, method) })
	t.Cleanup(ps.close)
	ps.setEnabled(domPage, true)
	ps.setEnabled(domRuntime, true)
	ps.dispatch("wk-0", "Page.frameNavigated", json.RawMessage(`{"frame":{"id":"main","loaderId":"0.1","url":"https://one.example/"}}`))
	ps.dispatch("wk-0", "Runtime.executionContextCreated", json.RawMessage(`{"context":{"id":1,"type":"normal","frameId":"main"}}`))
	return ps, func() []string {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// navigatedTo is a WIP main-frame frameNavigated with loader l.
func navigatedTo(l string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"frame":{"id":"main","loaderId":%q,"url":"https://%s.example/"}}`, l, l))
}

func commitOf(old, target string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"oldTargetId":%q,"newTargetId":%q}`, old, target))
}

func announce(ps *pageState, id string) {
	ps.dispatch("", evTargetCreated, json.RawMessage(fmt.Sprintf(`{"targetInfo":{"targetId":%q,"type":"page","isProvisional":true}}`, id)))
}

// resetSent reports whether sent, the events of one step, are a reset:
// the contexts cleared and no new document published (a model has no page
// to read).
func resetSent(ps *pageState, sent []string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(sent) == 1 && sent[0] == "Runtime.executionContextsCleared" && ps.main == nil && ps.reseed
}

// A provisional target that exceeds the staging bounds keeps nothing
// staged, and its commit resets the document instead of publishing a part
// of it (F5).
func TestStagingOverflowResetsAtCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload json.RawMessage
		n       int
	}{
		{"events", json.RawMessage(`{"message":{"source":"console-api","text":"x"}}`), maxStagedEvents + 1},
		{"bytes", json.RawMessage(`{"message":{"source":"console-api","text":"` + strings.Repeat("x", 64<<10) + `"}}`), maxStagedBytes/(64<<10) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, sent := recordingModel(t)
			announce(ps, "big")
			ps.dispatch("big", "Page.frameNavigated", navigatedTo("big"))
			for range tc.n {
				ps.dispatch("big", "Console.messageAdded", tc.payload)
			}
			ps.mu.Lock()
			events, bytes, staged := ps.stagedEvents, ps.stagedBytes, len(ps.staged["big"])
			ps.mu.Unlock()
			if events != 0 || bytes != 0 || staged != 0 {
				t.Fatalf("after the overflow %d events (%d bytes) stay staged, %d of the target; want none", events, bytes, staged)
			}
			before := len(sent())
			ps.dispatch("", evTargetCommitted, commitOf("wk-0", "big"))
			if got := sent()[before:]; !resetSent(ps, got) {
				t.Fatalf("commit of an overflowed target sent %v, want a reset", got)
			}
		})
	}
}

// Within the bounds the staged events are published at the commit, and
// the bookkeeping returns to zero.
func TestStagingWithinBoundsPublishesAtCommit(t *testing.T) {
	ps, sent := recordingModel(t)
	announce(ps, "next")
	ps.dispatch("next", "Page.frameNavigated", navigatedTo("next"))
	before := len(sent())
	ps.dispatch("", evTargetCommitted, commitOf("wk-0", "next"))
	got := strings.Join(sent()[before:], " ")
	if !strings.Contains(got, "Page.frameNavigated") {
		t.Fatalf("commit sent %s, want the staged document", got)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.stagedEvents != 0 || ps.stagedBytes != 0 || len(ps.stagedOrder) != 0 || ps.main.target != "next" {
		t.Fatalf("after the commit: %d events, %d bytes, order %v, main of %q", ps.stagedEvents, ps.stagedBytes, ps.stagedOrder, ps.main.target)
	}
}

// At most maxStagedTargets targets are staged: a new one pushes out the
// oldest, whose commit then resets the document (F5).
func TestStagingEvictionResetsAtCommit(t *testing.T) {
	ps, sent := recordingModel(t)
	for i := range maxStagedTargets + 1 {
		id := fmt.Sprintf("p%d", i)
		announce(ps, id)
		ps.dispatch(id, "Page.frameNavigated", navigatedTo(id))
	}
	ps.mu.Lock()
	_, oldest := ps.staged["p0"]
	n := len(ps.staged)
	ps.mu.Unlock()
	if oldest || n != maxStagedTargets {
		t.Fatalf("staged targets %d (oldest kept: %v), want %d without the oldest", n, oldest, maxStagedTargets)
	}
	before := len(sent())
	ps.dispatch("", evTargetCommitted, commitOf("wk-0", "p0"))
	if got := sent()[before:]; !resetSent(ps, got) {
		t.Fatalf("commit of a pushed-out target sent %v, want a reset", got)
	}
}

// startModel runs the attachment's goroutine on a model, as newPageState
// does, with conn as the attachment's connection.
func startModel(t *testing.T, conn webkit.Caller) (*pageState, func() []string) {
	ps, sent := recordingModel(t)
	ps.conn = conn
	go ps.run()
	return ps, sent
}

// otherConn is a distinct connection value.
type otherConn struct {
	webkit.Caller
	n int
}

// settle waits until the goroutine handled every pushed event.
func settle(t *testing.T, ps *pageState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ps.caughtUp(ctx); err != nil {
		t.Fatalf("waiting for the attachment's events: %v", err)
	}
}

// When the read loop finds the queue full, the events after it are
// dropped; the goroutine then resets the document, forgets the staged
// targets, and the next commit resets again (F5).
func TestQueueOverflowResets(t *testing.T) {
	conn := &otherConn{n: 1}
	ps, sent := startModel(t, conn)
	pos := core.EventPosition{Conn: conn, ActiveTarget: "wk-0"}
	ps.onWire(pos, "next", "Page.frameNavigated", navigatedTo("next"))
	settle(t, ps)
	ps.mu.Lock() // the goroutine waits, after it took at most one batch: the queue fills
	for range 2 * maxQueuedEvents {
		ps.onWire(pos, "wk-0", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","text":"x"}}`))
	}
	ps.q.mu.Lock()
	queued, dropped := len(ps.q.items), ps.q.dropped
	ps.q.mu.Unlock()
	ps.mu.Unlock()
	if queued != maxQueuedEvents || dropped == 0 {
		t.Fatalf("queue holds %d events and dropped %d, want %d and some dropped", queued, dropped, maxQueuedEvents)
	}
	settle(t, ps)
	ps.mu.Lock()
	reset, staged, again := ps.main == nil && ps.reseed, len(ps.staged), ps.resetOnCommit
	ps.mu.Unlock()
	if !reset || staged != 0 || !again {
		t.Fatalf("after dropped events: reset %v, staged targets %d, next commit resets %v; want a reset, none, true", reset, staged, again)
	}
	if got := sent(); !strings.Contains(strings.Join(got, " "), "Runtime.executionContextsCleared") {
		t.Fatalf("sent %v, want the contexts cleared", got)
	}
	// The next commit resets too: a commit may have been among the dropped.
	ps.mu.Lock()
	ps.reseed = false
	ps.mu.Unlock()
	ps.onWire(pos, "later", "Page.frameNavigated", navigatedTo("later"))
	ps.onWire(core.EventPosition{Conn: conn, ActiveTarget: "later"}, "", evTargetCommitted, commitOf("wk-0", "later"))
	settle(t, ps)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main != nil || !ps.reseed || ps.resetOnCommit {
		t.Fatalf("the commit after dropped events published a document (main %v)", ps.main)
	}
}

// The queue also bounds the payload.
func TestQueueBoundsPayload(t *testing.T) {
	var q eventQueue
	q.wake = make(chan struct{}, 1)
	q.push(wipEvent{params: make(json.RawMessage, maxQueuedBytes)})
	q.push(wipEvent{params: json.RawMessage(`{}`)})
	items, dropped := q.take()
	if len(items) != 1 || dropped != 1 {
		t.Fatalf("queued %d, dropped %d; want the first kept and the one beyond the payload bound dropped", len(items), dropped)
	}
}

// An event classified at its position: an event of a target that is not
// active there is staged although no targetCreated announced the target
// (it was created before the attachment), and published at its commit;
// a target announced as no provisional one (a worker) is translated at
// once (F1).
func TestPositionClassifiesUnannouncedTargets(t *testing.T) {
	conn := &otherConn{n: 1}
	ps, sent := startModel(t, conn)
	before := len(sent())
	ps.onWire(core.EventPosition{Conn: conn, ActiveTarget: "wk-0"}, "early", "Page.frameNavigated", navigatedTo("early"))
	settle(t, ps)
	if got := sent()[before:]; len(got) != 0 {
		t.Fatalf("a provisional target's event sent %v before its commit", got)
	}
	ps.onWire(core.EventPosition{Conn: conn, ActiveTarget: "early"}, "", evTargetCommitted, commitOf("wk-0", "early"))
	settle(t, ps)
	ps.mu.Lock()
	main := ps.main.target
	ps.mu.Unlock()
	if main != "early" {
		t.Fatalf("main document of %q after the commit, want the committed target's", main)
	}
	ps.onWire(core.EventPosition{Conn: conn, ActiveTarget: "early"}, "", evTargetCreated, json.RawMessage(`{"targetInfo":{"targetId":"worker-1","type":"worker","isProvisional":false}}`))
	before = len(sent())
	ps.onWire(core.EventPosition{Conn: conn, ActiveTarget: "early"}, "worker-1", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","text":"w"}}`))
	settle(t, ps)
	if got := sent()[before:]; !strings.Contains(strings.Join(got, " "), "Runtime.consoleAPICalled") {
		t.Fatalf("an announced worker's event sent %v, want it at once", got)
	}
}

// The first event of another connection (a reconnect) resets the
// document and forgets the staged targets: the page may have navigated
// while no connection delivered events.
func TestNewConnectionResetsDocument(t *testing.T) {
	first, second := &otherConn{n: 1}, &otherConn{n: 2}
	ps, sent := startModel(t, first)
	ps.onWire(core.EventPosition{Conn: first, ActiveTarget: "wk-0"}, "next", "Page.frameNavigated", navigatedTo("next"))
	settle(t, ps)
	before := len(sent())
	ps.onWire(core.EventPosition{Conn: second, ActiveTarget: "wk-0"}, "wk-0", "Console.messageAdded", json.RawMessage(`{"message":{"source":"console-api","text":"x"}}`))
	settle(t, ps)
	got := sent()[before:]
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(got) == 0 || got[0] != "Runtime.executionContextsCleared" || ps.main != nil || ps.conn != second || ps.connSeq != 1 || len(ps.staged) != 0 || !ps.wasLostLocked("next") {
		t.Fatalf("after the new connection's event: sent %v, main %v, conn %v, seq %d, staged %v", got, ps.main, ps.conn, ps.connSeq, ps.staged)
	}
}

// A frame tree read while the attachment changed its connection is not
// seeded: it may come from either connection, and the change's reset
// reads its own.
func TestTreeReadAcrossConnectionChangeIsNotSeeded(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	_, s := r.pageSession(t, sid)
	call(t, c, sid, "Page.getFrameTree", nil)
	ps := s.page
	ps.mu.Lock()
	ps.main = nil // as after a reset
	ps.mu.Unlock()
	r.fake["One"].Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		ps.mu.Lock()
		ps.connSeq++ // the attachment changes its connection during the read
		ps.mu.Unlock()
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return map[string]any{"frameTree": map[string]any{"frame": fp.mainFrame(), "resources": []any{}}}, nil
	})
	mo, end, err := s.begin(ctxFor(t, 5*time.Second), "Page.getResourceTree", core.ClassRead)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ps.readTree(mo.Op.Context(), mo)
	end()
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.main != nil {
		t.Fatalf("a tree read across a connection change was seeded: %+v", ps.main.wip)
	}
}
