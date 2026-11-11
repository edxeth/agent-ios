package cdp

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Repair round 1, F8: contract tests for the lifecycle, failure and
// capability branches of W5c-1a that had no retained behavior test
// (/home/devkit/.pi/artifacts/reviewer/w5c1a/coverage-judgment.md).

// holdApply makes the attachment's event goroutine stop before the n-th
// requestWillBeSent that it handles, until release is closed; reached is
// closed when it stops there.
func holdApply(t *testing.T, ps *pageState, n int) (reached, release chan struct{}) {
	t.Helper()
	reached, release = make(chan struct{}), make(chan struct{})
	seen := 0
	var once sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.method != "Network.requestWillBeSent" {
			return
		}
		if seen++; seen == n {
			once.Do(func() { close(reached); <-release })
		}
	}
	ps.mu.Unlock()
	t.Cleanup(func() {
		ps.mu.Lock()
		ps.testApply = nil
		ps.mu.Unlock()
	})
	return reached, release
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-testCtx(t).Done():
		t.Fatalf("%s: not before the test's deadline", what)
	}
}

// pushedAtLeast waits until the attachment's connections delivered n
// events.
func pushedAtLeast(t *testing.T, ps *pageState, n uint64) {
	t.Helper()
	waitUntil(t, "the events queued", func() bool { return ps.q.pushed() >= n })
}

// events.go close: an interception event that the attachment queued and
// will not translate is continued at the detach (fetch_domain.go "No
// request stays paused").
func TestFetchCloseContinuesQueuedInterception(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	ps := s.page
	reached, release := holdApply(t, ps, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.Emit("Network.requestWillBeSent", wipRequest("q.0", "main", "0.1", "Fetch", "https://one.example/first", nil))
	waitClosed(t, reached, "the attachment at its first request")
	n := ps.q.pushed()
	f.InterceptEvent("wk-One", "q.1", "request", map[string]any{"request": map[string]any{"url": "https://one.example/queued", "method": "GET", "headers": map[string]string{}}})
	pushedAtLeast(t, ps, n+1)
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	answeredWith(t, f, "q.1", "Network.interceptContinue")
}

// staging.go applyLocked: an interception event in a batch that the
// event goroutine took before the detach, and handles after it, is
// continued. A second attachment keeps Network enabled, so the detach
// does not disable Network (which would continue every hold of the
// page).
func TestFetchBatchTakenBeforeCloseContinued(t *testing.T) {
	r, _, c, sid, target, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	other := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, other, "Network.enable", nil)
	ps := s.page
	first, releaseFirst := holdApply(t, ps, 1)
	f.Emit("Network.requestWillBeSent", wipRequest("b.0", "main", "0.1", "Fetch", "https://one.example/first", nil))
	waitClosed(t, first, "the attachment at its first request")
	n := ps.q.pushed()
	f.Emit("Network.requestWillBeSent", wipRequest("b.1", "main", "0.1", "Fetch", "https://one.example/second", nil))
	f.InterceptEvent("wk-One", "b.2", "request", map[string]any{"request": map[string]any{"url": "https://one.example/batched", "method": "GET", "headers": map[string]string{}}})
	pushedAtLeast(t, ps, n+2)
	second, releaseSecond := holdApply(t, ps, 1) // the next requestWillBeSent: b.1, in the batch with b.2
	close(releaseFirst)
	waitClosed(t, second, "the attachment at the batch")
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	close(releaseSecond)
	answeredWith(t, f, "b.2", "Network.interceptContinue")
}

// fetch_domain.go continueUnreported: a request paused at the response
// stage while the detach removes the rules is continued at the response
// stage.
func TestFetchDetachContinuesResponseStage(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "requestStage": "Response"}}})
	var late sync.Once
	f.Handle("Network.removeInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		late.Do(func() {
			f.InterceptEvent("wk-One", "r.1", "response", map[string]any{"response": map[string]any{"url": "https://one.example/late", "status": 200, "statusText": "OK", "headers": map[string]string{}}})
		})
		return nil, nil
	})
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	answeredWith(t, f, "r.1", "Network.interceptContinue")
	for _, a := range f.Answers() {
		if a.RequestID == "r.1" && a.Stage != "response" {
			t.Fatalf("answer %+v, want the response stage", a)
		}
	}
}

// fetch_domain.go fetchEnable: WebKit refuses setInterceptionEnabled.
// The enable fails, the attachment reports nothing and continues what its
// owner's rules may pause, and Fetch.disable releases the owner.
func TestFetchEnableRefusedSetInterception(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	_, s := r.pageSession(t, sid)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	f.Unsupported("Network.setInterceptionEnabled")
	if e := callErr(t, c, sid, "Fetch.enable", nil); e == nil {
		t.Fatal("Fetch.enable succeeded although WebKit refused setInterceptionEnabled")
	}
	seq := c.Seq()
	f.InterceptEvent("wk-One", "e.1", "request", map[string]any{"request": map[string]any{"url": "https://one.example/a", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "e.1", "Network.interceptContinue")
	if got := sessionEvents(c, seq, sid, "Fetch.requestPaused"); len(got) != 0 {
		t.Fatalf("a failed Fetch.enable reported %v", got)
	}
	f.Handle("Network.setInterceptionEnabled", nil)
	call(t, c, sid, "Fetch.disable", nil)
	if slices.Contains(s.pg.PS.State.AllOwners(), s.fetchOwner()) {
		t.Fatal("the Fetch owner stays in the desired state after Fetch.disable")
	}
}

// fetch_domain.go fetchEnable: WebKit refuses the removal of a replaced
// rule. The enable fails, the paused request is continued, and
// Fetch.disable removes both rules once WebKit accepts the removal.
func TestFetchEnableRefusedRemoval(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	interceptReq(t, c, f, sid, "wk-One", "x.1", "https://one.example/a")
	f.Unsupported("Network.removeInterception")
	if e := callErr(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/b"}}}); e == nil {
		t.Fatal("Fetch.enable succeeded although WebKit refused the removal")
	}
	answeredWith(t, f, "x.1", "Network.interceptContinue")
	f.Handle("Network.removeInterception", nil)
	call(t, c, sid, "Fetch.disable", nil)
	var removed []string
	for _, rec := range f.ReceivedMethod("Network.removeInterception") {
		removed = append(removed, string(rec.Params))
	}
	all := strings.Join(removed, " ")
	if !strings.Contains(all, `"url":"^.*(?:`) || !strings.Contains(all, `"url":"^.*/b(?:`) {
		t.Fatalf("removals %v, want both rules removed", removed)
	}
	// The refused removal left the old rule in WebKit, so the release sends
	// its removal again (W5c-1a review F9: core keeps the rule's record).
	if n := strings.Count(all, `"url":"^.*(?:`); n < 2 {
		t.Fatalf("removals %v, want the refused removal of the old rule sent again at Fetch.disable", removed)
	}
}

// fetch_domain.go fetchAnswer: a hold that went away while the attachment
// still remembers it (another owner continued it) gets Chrome's error,
// and the attachment forgets it.
func TestFetchAnswerAfterHoldVanished(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	p := interceptReq(t, c, f, sid, "wk-One", "v.1", "https://one.example/a")
	if err := s.pg.PS.Sched.ContinueUnclaimed(ctxFor(t, 5*time.Second), "wk-One", "v.1", stageRequest); err != nil {
		t.Fatal(err)
	}
	if e := callErr(t, c, sid, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200, "body": ""}); e.Message != "Invalid InterceptionId." {
		t.Fatalf("answer for a vanished hold: %v", e)
	}
	s.page.mu.Lock()
	left := len(s.page.fetch.paused)
	s.page.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d paused requests remembered, want the vanished one forgotten", left)
	}
}

// fetch_domain.go control: after the detach the attachment's control
// steps are refused without running.
func TestFetchControlFencedAfterDetach(t *testing.T) {
	r, _, c, sid, _, _ := enableFetch(t)
	_, s := r.pageSession(t, sid)
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	ran := false
	if err := s.control(func() error { ran = true; return nil }); !errors.Is(err, errDetached) || ran {
		t.Fatalf("control after the detach: %v, ran %v; want errDetached without running", err, ran)
	}
}

// network_domain.go networkEnable: a refused Network.enable leaves the
// attachment's Network events off.
func TestNetworkEnableRefusedRollsBack(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	f.Unsupported("Network.enable")
	if e := callErr(t, c, sid, "Network.enable", nil); e == nil {
		t.Fatal("Network.enable succeeded although WebKit refused it")
	}
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("n.1", "main", "0.1", "Fetch", "https://one.example/a", nil))
	f.Emit("Page.loadEventFired", map[string]any{"timestamp": 1.0})
	waitEvent(t, c, seq, sid, "Page.loadEventFired", nil)
	if got := sessionEvents(c, seq, sid, "Network.requestWillBeSent"); len(got) != 0 {
		t.Fatalf("Network events after a refused Network.enable: %v", got)
	}
}

// network_domain.go getResponseBody: the body of a request whose target a
// commit replaced is gone: Chrome's eviction text.
func TestNetworkBodyOfSwappedTarget(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("s.1", "main", "0.1", "Fetch", "https://one.example/a", nil))
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "s.1", "timestamp": 2.0})
	done := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.loadingFinished", nil).Params)
	waitResumed(t, f.Provisional("next"))
	f.EmitFrom("next", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "9.1", "loaderId": "0.7", "url": "https://two.example/", "securityOrigin": "https://two.example"}})
	f.Commit("next")
	waitEvent(t, c, seq, sid, "Page.frameNavigated", nil)
	if e := callErr(t, c, sid, "Network.getResponseBody", map[string]any{"requestId": done.RequestID}); e.Message != errEvicted.Message {
		t.Fatalf("body of a replaced target's request: %v, want %q", e, errEvicted.Message)
	}
}

// network_domain.go isMainFrameLocked: in a provisional target, the frame
// of its first document request is its main frame, so a later document
// request of another frame of that target is a child frame's: it gets a
// counter id, not its loader id, and keeps its own frame id (5.2).
func TestNetworkProvisionalChildDocumentRequest(t *testing.T) {
	var events []sentEvent
	ps := newPageModel("PAGE", func(method string, params any) {
		b, _ := json.Marshal(params)
		events = append(events, sentEvent{method, b})
	})
	ps.setEnabled(domNetwork, true)
	ps.dispatch("active", "Page.frameNavigated", mustRaw(map[string]any{"frame": map[string]any{"id": "main", "loaderId": "L0", "url": "https://one.example/"}}))
	ps.dispatch("", evTargetCreated, mustRaw(map[string]any{"targetInfo": map[string]any{"targetId": "next", "type": "page", "isProvisional": true}}))
	ps.dispatch("next", "Network.requestWillBeSent", mustRaw(wipRequest("1", "9.1", "L1", "Document", "https://two.example/", nil)))
	ps.dispatch("next", "Network.requestWillBeSent", mustRaw(wipRequest("2", "9.2", "L2", "Document", "https://kid.example/", nil)))
	ps.dispatch("next", "Page.frameNavigated", mustRaw(map[string]any{"frame": map[string]any{"id": "9.1", "loaderId": "L1", "url": "https://two.example/"}}))
	ps.dispatch("", evTargetCommitted, mustRaw(map[string]any{"oldTargetId": "active", "newTargetId": "next"}))
	var reqs []cdpNetEvent
	for _, e := range events {
		if e.method == "Network.requestWillBeSent" {
			reqs = append(reqs, decode[cdpNetEvent](t, e.params))
		}
	}
	if len(reqs) != 2 {
		t.Fatalf("requests %+v, want 2", reqs)
	}
	if doc, kid := reqs[0], reqs[1]; doc.RequestID != doc.LoaderID || doc.FrameID != "PAGE" || kid.RequestID == kid.LoaderID || kid.FrameID != "9.2" {
		t.Fatalf("main document request %+v and child frame's %+v: want requestId == loaderId only for the main frame's", doc, kid)
	}
}

// network_domain.go emulateNetworkConditions: an iOS version whose
// command list lacks Network.setEmulatedConditions (16.0) gets the
// bridge's error, and nothing is sent.
func TestNetworkConditionsCommandList(t *testing.T) {
	r := newRig(t)
	dev := r.l.AddDevice("00008020-00000000000000C3", "iPhone C", "16.0")
	if _, err := r.res.Reserve(dev.ID, "o1", time.Hour); err != nil {
		t.Fatal(err)
	}
	f := r.addPage(t, dev, "Old")
	c := dial(t, r.issue(t, "s1", "Old", ScopePage).WS)
	_, sid := puppeteerAttach(t, c)
	_, err := c.Call(ctxFor(t, 5*time.Second), sid, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": 1000, "uploadThroughput": -1})
	var ce *cdptest.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "cannot be emulated on iOS 16.0") {
		t.Fatalf("emulateNetworkConditions on iOS 16.0: %v, want the command-list error", err)
	}
	if got := f.ReceivedMethod("Network.setEmulatedConditions"); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}
