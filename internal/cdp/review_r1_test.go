package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Repair round 1 (review /home/devkit/.pi/artifacts/reviewer/w5c1a-review-20261108-170530.md):
// the reviewer's reproductions, kept as contract tests.

// F4: post data above 64 KiB is left out of the event, also above the
// stored-data bound (1 MiB).
func TestReviewOversizedPostOmitted(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	post := strings.Repeat("x", maxPostData+1)
	r.fake["One"].Emit("Network.requestWillBeSent", wipRequest("post", "main", "0.1", "Fetch", "https://one.example/post", map[string]any{
		"request": map[string]any{"url": "https://one.example/post", "method": "POST", "headers": map[string]string{}, "postData": post},
	}))
	p := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil).Params)
	if p.Request.PostData != "" || !p.Request.HasPostData {
		t.Fatalf("event contains %d bytes of post data (hasPostData %v), want none above 64 KiB", len(p.Request.PostData), p.Request.HasPostData)
	}
}

// F7: frameId and loaderId on every Network event (docs/notes/w5.md),
// also the synthetic abort and the memory-cache sequence.
func TestReviewNetworkEventIdentityFields(t *testing.T) {
	var events []sentEvent
	ps := newPageModel("PAGE", func(method string, params any) {
		b, _ := json.Marshal(params)
		events = append(events, sentEvent{method, b})
	})
	ps.setEnabled(domNetwork, true)
	ps.dispatch("active", "Page.frameNavigated", mustRaw(map[string]any{"frame": map[string]any{"id": "main", "loaderId": "L", "url": "https://one.example/"}}))
	ps.dispatch("active", "Network.requestWillBeSent", mustRaw(wipRequest("r1", "main", "L", "Fetch", "https://one.example/resource", nil)))
	ps.dispatch("active", "Network.dataReceived", mustRaw(map[string]any{"requestId": "r1", "timestamp": 101, "dataLength": 1, "encodedDataLength": 1}))
	ps.dispatch("active", "Network.loadingFinished", mustRaw(map[string]any{"requestId": "r1", "timestamp": 102}))
	ps.dispatch("active", "Network.requestWillBeSent", mustRaw(wipRequest("r2", "main", "L", "Fetch", "https://one.example/fail", nil)))
	ps.dispatch("active", "Network.loadingFailed", mustRaw(map[string]any{"requestId": "r2", "timestamp": 103, "errorText": "Failed", "canceled": false}))
	ps.dispatch("active", "Network.requestServedFromMemoryCache", mustRaw(map[string]any{
		"requestId": "r3", "frameId": "main", "loaderId": "L", "timestamp": 104, "resource": map[string]any{"url": "https://one.example/cache", "type": "Script", "bodySize": 1},
	}))
	ps.dispatch("active", "Network.requestWillBeSent", mustRaw(wipRequest("r4", "main", "L", "Fetch", "https://one.example/cut", nil)))
	ps.mu.Lock()
	ps.resetLocked() // the synthetic loadingFailed of r4
	ps.mu.Unlock()
	kinds := map[string]bool{}
	for _, e := range events {
		if !strings.HasPrefix(e.method, "Network.") {
			continue
		}
		var p struct{ FrameID, LoaderID string }
		if err := json.Unmarshal(e.params, &p); err != nil {
			t.Fatal(err)
		}
		if p.FrameID != "PAGE" || p.LoaderID == "" {
			t.Errorf("%s lacks frameId/loaderId: %s", e.method, e.params)
		}
		kinds[e.method] = true
	}
	for _, m := range []string{"Network.dataReceived", "Network.loadingFinished", "Network.loadingFailed", "Network.requestServedFromCache"} {
		if !kinds[m] {
			t.Errorf("no %s was sent", m)
		}
	}
}

// F5: a positive latency or upload limit cannot be emulated and is
// refused before any change; the unthrottled values that clients send
// (latency 0, throughput -1) are accepted.
func TestReviewUnsupportedConditionsRejected(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	for _, params := range []map[string]any{
		{"offline": false, "latency": 100, "downloadThroughput": -1, "uploadThroughput": -1},
		{"offline": false, "latency": 0, "downloadThroughput": 1000, "uploadThroughput": 100},
	} {
		if _, err := c.Call(ctxFor(t, 2*time.Second), sid, "Network.emulateNetworkConditions", params); err == nil {
			t.Errorf("unsupported emulation %v returned success", params)
		}
	}
	if got := f.ReceivedMethod("Network.setEmulatedConditions"); len(got) != 0 {
		t.Fatalf("a refused call changed the conditions: %v", got)
	}
	call(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	for _, got := range f.ReceivedMethod("Network.setEmulatedConditions") {
		if strings.Contains(string(got.Params), "bytesPerSecondLimit") {
			t.Fatalf("unthrottled values set a limit: %s", got.Params)
		}
	}
}

// F6: at the response stage, fulfillRequest without body (CDP: keep the
// original body) is refused, since WIP would replace the body; an
// explicit empty body is the empty-body operation.
func TestReviewResponseFulfillOmittedBody(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "requestStage": "Response"}}})
	seq := c.Seq()
	f.InterceptEvent("wk-One", "7.1", "response", map[string]any{"response": map[string]any{"url": "https://one.example/r", "status": 200, "statusText": "OK", "headers": map[string]string{}}})
	id := decode[pausedEvent](t, waitEvent(t, c, seq, sid, "Fetch.requestPaused", nil).Params).RequestID
	if e := callErr(t, c, sid, "Fetch.fulfillRequest", map[string]any{"requestId": id, "responseCode": 200}); e.Code != -32000 || !strings.Contains(e.Message, "without body") {
		t.Fatalf("omitted body at the response stage: %v, want a refusal", e)
	}
	if got := f.ReceivedMethod("Network.interceptWithResponse"); len(got) != 0 {
		t.Fatalf("the refused fulfill was sent: %v", got)
	}
	call(t, c, sid, "Fetch.fulfillRequest", map[string]any{"requestId": id, "responseCode": 200, "body": ""})
	answeredWith(t, f, "7.1", "Network.interceptWithResponse")
	if got := string(f.ReceivedMethod("Network.interceptWithResponse")[0].Params); !strings.Contains(got, `"content":""`) {
		t.Fatalf("empty body sent as %s", got)
	}
}
