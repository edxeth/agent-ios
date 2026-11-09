package cdp

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"
)

// Repair round 1, findings F1 to F3 (review
// /home/devkit/.pi/artifacts/reviewer/w5c1a-review-20261108-170530.md):
// the reviewer's reproductions, kept as contract tests.

// F1: a pattern's resourceType applies to the document request of a
// provisional target too: its type comes from the staged
// requestWillBeSent, so a Document pattern reports it (design 5.3).
func TestReviewProvisionalDocumentTypeFilter(t *testing.T) {
	n := newNetRun(t)
	n.w.ps.fetch.patterns[0].typ = "Document"
	n.w.provisional("next", false)
	u, id := n.request("next", "Document", "new")
	n.w.deliver("next", evRequestIntercepted, mustRaw(map[string]any{"requestId": id, "request": map[string]any{"url": u, "method": "GET", "headers": map[string]string{}}}))
	var paused []pausedEvent
	var sent []string
	for _, e := range n.w.events {
		switch e.method {
		case "Fetch.requestPaused":
			var p pausedEvent
			if err := json.Unmarshal(e.params, &p); err != nil {
				t.Fatal(err)
			}
			paused = append(paused, p)
		case "Network.requestWillBeSent":
			var p cdpNetEvent
			if err := json.Unmarshal(e.params, &p); err != nil {
				t.Fatal(err)
			}
			sent = append(sent, p.RequestID)
		}
	}
	if len(paused) != 1 || paused[0].ResourceType != "Document" || !slices.Equal(sent, []string{paused[0].NetworkID}) {
		t.Fatalf("paused %+v after requests %v, want the Document request paused for the client after its requestWillBeSent", paused, sent)
	}
}

// F1: two attachments of one logical session (design 5.2) with the same
// URL pattern and other resource types: a request that neither type
// matches is continued, not left to the other attachment.
func TestReviewUnmatchedTypeFiltersAcrossAttachments(t *testing.T) {
	_, _, c, sid, target, f := enableFetch(t)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Script"}}})
	second := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, second, "Network.enable", nil)
	call(t, c, second, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Document"}}})
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("fetch", "main", "0.1", "Fetch", "https://one.example/fetch", nil))
	f.InterceptEvent("wk-One", "fetch", "request", map[string]any{"request": map[string]any{"url": "https://one.example/fetch", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "fetch", "Network.interceptContinue")
	if a, b := sessionEvents(c, seq, sid, "Fetch.requestPaused"), sessionEvents(c, seq, second, "Fetch.requestPaused"); len(a)+len(b) != 0 {
		t.Fatalf("a request of type Fetch was reported paused: first %v, second %v", a, b)
	}
}

// F1: the attachment whose pattern claims the request reports it, and the
// other attachment, whose rule pauses it too, leaves it to that one: the
// client's answer is the only one.
func TestReviewTypeFilterClaimantAnswersAlone(t *testing.T) {
	_, _, c, sid, target, f := enableFetch(t)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Script"}}})
	second := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, second, "Network.enable", nil)
	call(t, c, second, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Document"}}})
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("doc", "main", "0.2", "Document", "https://one.example/doc", nil))
	f.InterceptEvent("wk-One", "doc", "request", map[string]any{"request": map[string]any{"url": "https://one.example/doc", "method": "GET", "headers": map[string]string{}}})
	p := decode[pausedEvent](t, waitEvent(t, c, seq, second, "Fetch.requestPaused", nil).Params)
	call(t, c, sid, "Page.getFrameTree", nil) // the first attachment handled the event too
	call(t, c, second, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200, "body": ""})
	var got []string
	for _, a := range f.Answers() {
		if a.RequestID == "doc" {
			got = append(got, a.Method)
		}
	}
	if !slices.Equal(got, []string{"Network.interceptRequestWithResponse"}) || len(sessionEvents(c, seq, sid, "Fetch.requestPaused")) != 0 {
		t.Fatalf("answers %v, first attachment's pauses %d; want only the second client's fulfill", got, len(sessionEvents(c, seq, sid, "Fetch.requestPaused")))
	}
}

// F3: across a reset, a paused document request that survives keeps its
// navigation identity (design 5.2): the document announced for it at the
// commit has its loader id (requestId == loaderId).
func TestReviewPausedDocumentResetIdentity(t *testing.T) {
	n := newNetRun(t)
	n.step(netStep{Op: nsReset})
	n.step(netStep{Op: nsPausedSwap, A: 0})
	var req, frame string
	for _, e := range n.w.events {
		var p struct {
			RequestID, LoaderID, Type string
			Frame                     struct{ LoaderID string }
		}
		if err := json.Unmarshal(e.params, &p); err != nil {
			t.Fatal(err)
		}
		if e.method == "Network.requestWillBeSent" && p.Type == "Document" {
			req = p.RequestID
		}
		if e.method == "Page.frameNavigated" {
			frame = p.Frame.LoaderID
		}
	}
	if req == "" || frame == "" || req != frame {
		t.Fatalf("paused document request id %s, last announced frame loader %s, want equal", req, frame)
	}
}

// F2: a Network reset applies at its place in the event stream: a
// request whose requestWillBeSent the connection delivered before core's
// report, but which the attachment had not handled yet, is a request from
// before the reset and ends too.
func TestReviewNetworkResetAppliesAtItsPlace(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	_, s := r.pageSession(t, sid)
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Review": "1"}})
	ps := s.page
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if ev.method == "Network.requestWillBeSent" {
			once.Do(func() { close(held); <-release })
		}
	}
	ps.mu.Unlock()
	t.Cleanup(func() {
		ps.mu.Lock()
		ps.testApply = nil
		ps.mu.Unlock()
	})
	seq := c.Seq()
	r.fake["One"].Emit("Network.requestWillBeSent", wipRequest("queued", "main", "0.1", "Fetch", "https://one.example/queued", nil))
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the attachment did not reach the request")
	}
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	close(release)
	sent := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil).Params)
	failed := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.loadingFailed", nil).Params)
	if failed.RequestID != sent.RequestID || !failed.Canceled {
		t.Fatalf("loadingFailed %+v, want the request %s from before the reset", failed, sent.RequestID)
	}
}

// F3 keeps F7: a paused document request of the document that the
// attachment announced already keeps no loader id across a reset, so the
// reset still announces that document with a new loader id.
func TestReviewResetRenewsAnnouncedPausedLoader(t *testing.T) {
	n := newNetRun(t)
	w := n.w
	announced := ""
	for _, e := range w.events {
		if e.method == "Page.frameNavigated" {
			announced = decode[struct{ Frame struct{ LoaderID string } }](t, e.params).Frame.LoaderID
		}
	}
	u, id := n.request(w.target, "Document", w.main) // the shown document's loader
	w.deliver(w.target, evRequestIntercepted, mustRaw(map[string]any{"requestId": id, "request": map[string]any{"url": u, "method": "GET", "headers": map[string]string{}}}))
	at := len(w.events)
	n.step(netStep{Op: nsReset})
	var req, renewed string
	for _, e := range w.events {
		var p struct {
			RequestID, Type string
			Frame           struct{ LoaderID string }
		}
		if err := json.Unmarshal(e.params, &p); err != nil {
			t.Fatal(err)
		}
		if e.method == "Network.requestWillBeSent" && p.Type == "Document" {
			req = p.RequestID
		}
	}
	for _, e := range w.events[at:] {
		if e.method == "Page.frameNavigated" {
			renewed = decode[struct{ Frame struct{ LoaderID string } }](t, e.params).Frame.LoaderID
		}
	}
	if announced == "" || req != announced || renewed == "" || renewed == announced {
		t.Fatalf("document announced with %q, its paused request %q, after the reset %q; want a new loader id after the reset", announced, req, renewed)
	}
}

// F2: core's Network reset (design 2.4; here the barrier of removing an
// extra header) reaches the CDP Network owner: the requests in flight
// across it end with loadingFailed, the client gets a Log warning, a
// paused request that the barrier continued is no longer held, earlier
// bodies are gone, the page's document stays, and a request after the
// reset is followed as usual.
func TestReviewHeaderResetEndsInflight(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Network.enable", nil) // one watch per attachment: one warning per reset
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/held"}}})
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Review": "1"}})
	f := r.fake["One"]
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("done", "main", "0.1", "Fetch", "https://one.example/done", nil))
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "done", "timestamp": 101.0})
	f.Emit("Network.requestWillBeSent", wipRequest("inflight", "main", "0.1", "Fetch", "https://one.example/pending", nil))
	f.Emit("Network.requestWillBeSent", wipRequest("held", "main", "0.1", "Fetch", "https://one.example/held", nil))
	held := interceptReq(t, c, f, sid, "wk-One", "held", "https://one.example/held")
	sent := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	if len(sent) != 3 {
		t.Fatalf("requests %+v, want 3", sent)
	}
	before := len(f.ReceivedMethod("Network.disable"))
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	if len(f.ReceivedMethod("Network.disable")) <= before {
		t.Fatal("removing the header did not reset Network")
	}
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil)
	eventually(t, 5*time.Second, "loadingFailed of both requests in flight", func() bool {
		return len(sessionEvents(c, seq, sid, "Network.loadingFailed")) >= 2
	})
	var failed []string
	for _, e := range netEvents(t, c, seq, sid, "Network.loadingFailed") {
		if !e.Canceled {
			t.Fatalf("loadingFailed %+v, want canceled", e)
		}
		failed = append(failed, e.RequestID)
	}
	slices.Sort(failed)
	want := []string{sent[1].RequestID, sent[2].RequestID}
	slices.Sort(want)
	if !slices.Equal(failed, want) {
		t.Fatalf("loadingFailed for %v, want the requests in flight %v", failed, want)
	}
	var warn []struct {
		Entry struct{ Source, Level, Text string }
	}
	for _, e := range sessionEvents(c, seq, sid, "Log.entryAdded") {
		warn = append(warn, decode[struct {
			Entry struct{ Source, Level, Text string }
		}](t, e.Params))
	}
	if len(warn) != 1 || warn[0].Entry.Level != "warning" || warn[0].Entry.Source != "network" {
		t.Fatalf("Log entries %+v, want one network warning", warn)
	}
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": held.RequestID}); e == nil || e.Message != "Invalid InterceptionId." {
		t.Fatalf("answer for a request the reset continued: %v", e)
	}
	if e := callErr(t, c, sid, "Network.getResponseBody", map[string]any{"requestId": sent[0].RequestID}); e == nil || e.Message != errEvicted.Message {
		t.Fatalf("getResponseBody of a request before the reset: %v, want %q", e, errEvicted.Message)
	}
	for _, m := range []string{"Page.frameNavigated", "Runtime.executionContextsCleared"} {
		if got := sessionEvents(c, seq, sid, m); len(got) != 0 {
			t.Fatalf("the Network reset reset the document: %s %v", m, got)
		}
	}
	after := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("next", "main", "0.1", "Fetch", "https://one.example/next", nil))
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "next", "timestamp": 102.0})
	waitEvent(t, c, after, sid, "Network.loadingFinished", nil)
	if got := sessionEvents(c, after, sid, "Network.loadingFailed"); len(got) != 0 {
		t.Fatalf("a request after the reset failed: %v", got)
	}
}
