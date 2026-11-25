package cdp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Network domain tests (design 5.2 "Navigation identity", 5.3; 7.8 items 1
// and 5), on the fake page of page_harness_test.go.

// wipRequest is a WIP Network.requestWillBeSent.
func wipRequest(id, frame, loader, typ, url string, extra map[string]any) map[string]any {
	p := map[string]any{
		"requestId": id, "frameId": frame, "loaderId": loader, "documentURL": url, "type": typ,
		"request":   map[string]any{"url": url, "method": "GET", "headers": map[string]string{"Accept": "*/*"}},
		"timestamp": 100.0, "walltime": 1.7e9, "initiator": map[string]any{"type": "other"},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

type cdpNetEvent struct {
	RequestID string  `json:"requestId"`
	LoaderID  string  `json:"loaderId"`
	FrameID   string  `json:"frameId"`
	Type      string  `json:"type"`
	Timestamp float64 `json:"timestamp"`
	ErrorText string  `json:"errorText"`
	Canceled  bool    `json:"canceled"`
	Request   struct {
		URL         string `json:"url"`
		Method      string `json:"method"`
		PostData    string `json:"postData"`
		HasPostData bool   `json:"hasPostData"`
	} `json:"request"`
	Response struct {
		Status int            `json:"status"`
		Timing map[string]any `json:"timing"`
	} `json:"response"`
}

func netEvents(t *testing.T, c *cdptest.Client, after int, sid, method string) []cdpNetEvent {
	t.Helper()
	var out []cdpNetEvent
	for _, e := range sessionEvents(c, after, sid, method) {
		out = append(out, decode[cdpNetEvent](t, e.Params))
	}
	return out
}

// callErr calls method and returns its CDP error.
func callErr(t *testing.T, c *cdptest.Client, sid, method string, params any) *cdptest.Error {
	t.Helper()
	_, err := c.Call(testCtx(t), sid, method, params)
	ce, ok := err.(*cdptest.Error)
	if !ok {
		t.Fatalf("%s: got %v, want a CDP error", method, err)
	}
	return ce
}

// Item 5, the part of W5c: Page.navigate returns at the main-frame
// document request; its loaderId equals that request's requestId and
// loaderId and frameNavigated's loaderId.
func TestDocumentRequestIDIsLoaderID(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Network.enable", nil)
	fp.before = func(expr string) {
		if strings.HasPrefix(expr, "location.href") {
			f.Emit("Network.requestWillBeSent", wipRequest("0.9", "main", "0.2", "Document", "https://one.example/two", nil))
		}
	}
	seq := c.Seq()
	nav := decode[struct{ FrameID, LoaderID string }](t, call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/two"}))
	reqs := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	if len(reqs) != 1 {
		t.Fatalf("requestWillBeSent before the navigate reply: %d, want 1", len(reqs))
	}
	fn := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params).Frame
	q := reqs[0]
	if nav.LoaderID == "" || q.RequestID != nav.LoaderID || q.LoaderID != nav.LoaderID || fn.LoaderID != nav.LoaderID {
		t.Fatalf("navigate loader %q, request %q/%q, frameNavigated %q: want one id", nav.LoaderID, q.RequestID, q.LoaderID, fn.LoaderID)
	}
	if q.FrameID != target || q.Type != "Document" {
		t.Fatalf("document request frame %q type %q, want the page target %q and Document", q.FrameID, q.Type, target)
	}
}

// Every Network event: CDP ids, frame and loader ids, the clock, timing,
// post data and bodies.
func TestNetworkEventsAndBodies(t *testing.T) {
	r, _, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	tree := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	call(t, c, sid, "Network.enable", nil)
	f.Handle("Network.getResponseBody", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ RequestID string }
		_ = json.Unmarshal(call.Params, &p)
		switch p.RequestID {
		case "0.3":
			return map[string]any{"body": "aGk=", "base64Encoded": true}, nil
		case "0.4":
			return nil, &fakewebkit.Error{Code: -32000, Message: "Missing content of resource for given requestId"}
		}
		return nil, &fakewebkit.Error{Code: -32000, Message: "Missing resource for given requestId"}
	})
	seq := c.Seq()
	post := "a=1&b=2"
	f.Emit("Network.requestWillBeSent", wipRequest("0.3", "main", "0.1", "Fetch", "https://one.example/api", map[string]any{
		"request": map[string]any{"url": "https://one.example/api", "method": "POST", "headers": map[string]string{}, "postData": post},
	}))
	f.Emit("Network.responseReceived", map[string]any{
		"requestId": "0.3", "frameId": "main", "loaderId": "0.1", "timestamp": 100.25, "type": "Fetch",
		"response": map[string]any{
			"url": "https://one.example/api", "status": 200, "statusText": "OK", "headers": map[string]string{"Content-Type": "text/plain"}, "mimeType": "text/plain", "source": "network",
			"timing": map[string]any{"fetchStart": 100.01, "domainLookupStart": 0, "domainLookupEnd": 0, "connectStart": 1.5, "connectEnd": 3, "secureConnectionStart": 0, "requestStart": 3.5, "responseStart": 200},
		},
	})
	f.Emit("Network.dataReceived", map[string]any{"requestId": "0.3", "timestamp": 100.3, "dataLength": 2, "encodedDataLength": 2})
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "0.3", "timestamp": 100.5, "metrics": map[string]any{"responseBodyBytesReceived": 2}})
	f.Emit("Network.requestWillBeSent", wipRequest("0.4", "main", "0.1", "Image", "https://one.example/a.png", nil))
	f.Emit("Network.loadingFailed", map[string]any{"requestId": "0.4", "timestamp": 100.6, "errorText": "Cancelled load", "canceled": true})
	waitEvent(t, c, seq, sid, "Network.loadingFailed", nil)

	reqs := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	if len(reqs) != 2 || reqs[0].RequestID == reqs[1].RequestID || reqs[0].RequestID == "0.3" {
		t.Fatalf("request ids %+v, want two bridge ids", reqs)
	}
	api, img := reqs[0], reqs[1]
	if api.FrameID != target || api.LoaderID != tree.LoaderID || api.Request.PostData != post || !api.Request.HasPostData || api.Type != "Fetch" {
		t.Fatalf("requestWillBeSent %+v, want frame %s, loader %s and the post data", api, target, tree.LoaderID)
	}
	resp := netEvents(t, c, seq, sid, "Network.responseReceived")[0]
	tm := resp.Response.Timing
	if resp.RequestID != api.RequestID || resp.FrameID != target || resp.LoaderID != tree.LoaderID || resp.Response.Status != 200 {
		t.Fatalf("responseReceived %+v, want the request's ids", resp)
	}
	// One clock: requestTime is fetchStart 10 ms after the request's 100.0.
	if d := tm["requestTime"].(float64) - api.Timestamp; d < 0.0099 || d > 0.0101 || tm["dnsStart"].(float64) != -1 || tm["connectStart"].(float64) != 1.5 || tm["sslStart"].(float64) != -1 || tm["sendStart"].(float64) != 3.5 || tm["receiveHeadersEnd"].(float64) != 200 {
		t.Fatalf("timing %v (request at %v), want WIP's converted to CDP", tm, api.Timestamp)
	}
	if d := resp.Timestamp - api.Timestamp; d < 0.2499 || d > 0.2501 {
		t.Fatalf("response %v after the request %v, want 0.25 s", resp.Timestamp, api.Timestamp)
	}
	fin := netEvents(t, c, seq, sid, "Network.loadingFinished")
	data := netEvents(t, c, seq, sid, "Network.dataReceived")
	failed := netEvents(t, c, seq, sid, "Network.loadingFailed")
	if len(fin) != 1 || fin[0].RequestID != api.RequestID || len(data) != 1 || data[0].RequestID != api.RequestID ||
		len(failed) != 1 || failed[0].RequestID != img.RequestID || failed[0].ErrorText != "Cancelled load" || !failed[0].Canceled || failed[0].Type != "Image" {
		t.Fatalf("finished %+v, data %+v, failed %+v", fin, data, failed)
	}

	body := decode[struct {
		Body          string
		Base64Encoded bool
	}](t, call(t, c, sid, "Network.getResponseBody", map[string]any{"requestId": api.RequestID}))
	if body.Body != "aGk=" || !body.Base64Encoded {
		t.Fatalf("getResponseBody = %+v", body)
	}
	if got := f.ReceivedMethod("Network.getResponseBody"); len(got) != 1 || !strings.Contains(string(got[0].Params), `"0.3"`) {
		t.Fatalf("WIP getResponseBody calls %v, want WebKit's id 0.3", got)
	}
	if e := callErr(t, c, sid, "Network.getResponseBody", map[string]any{"requestId": img.RequestID}); e.Message != "No data found for resource with given identifier" {
		t.Fatalf("body without content: %v", e)
	}
	if e := callErr(t, c, sid, "Network.getResponseBody", map[string]any{"requestId": "nope"}); e.Message != "No resource with given identifier found" || e.Code != -32000 {
		t.Fatalf("body of an unknown request: %v", e)
	}
	pd := decode[struct{ PostData string }](t, call(t, c, sid, "Network.getRequestPostData", map[string]any{"requestId": api.RequestID}))
	if pd.PostData != post {
		t.Fatalf("getRequestPostData = %q", pd.PostData)
	}
	if e := callErr(t, c, sid, "Network.getRequestPostData", map[string]any{"requestId": img.RequestID}); e.Message != "No post data available for the request" {
		t.Fatalf("post data of a GET: %v", e)
	}
	if e := callErr(t, c, sid, "Network.getRequestPostData", map[string]any{"requestId": "nope"}); e.Message != "No resource with given id was found" {
		t.Fatalf("post data of an unknown request: %v", e)
	}

	// A memory-cache hit: Chrome's four events.
	seq = c.Seq()
	f.Emit("Network.requestServedFromMemoryCache", map[string]any{
		"requestId": "0.5", "frameId": "main", "loaderId": "0.1", "documentURL": "https://one.example/", "timestamp": 101.0,
		"resource": map[string]any{
			"url": "https://one.example/c.css", "type": "StyleSheet", "bodySize": 9,
			"response": map[string]any{"url": "https://one.example/c.css", "status": 200, "statusText": "OK", "headers": map[string]string{}, "mimeType": "text/css", "source": "memory-cache"},
		},
	})
	waitEvent(t, c, seq, sid, "Network.loadingFinished", nil)
	var order []string
	for _, e := range c.Events() {
		if e.Seq > seq && e.SessionID == sid && strings.HasPrefix(e.Method, "Network.") {
			order = append(order, e.Method)
		}
	}
	if strings.Join(order, ",") != "Network.requestWillBeSent,Network.requestServedFromCache,Network.responseReceived,Network.loadingFinished" {
		t.Fatalf("memory-cache hit events %v", order)
	}
	if q := netEvents(t, c, seq, sid, "Network.requestWillBeSent")[0]; q.Type != "Stylesheet" || q.FrameID != target {
		t.Fatalf("memory-cache request %+v", q)
	}
}

// Network.disable stops the events; the owner's state is released, and
// the other methods map to their WIP calls.
func TestNetworkMethodsMapToWIP(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Network.setCacheDisabled", map[string]any{"cacheDisabled": true})
	call(t, c, sid, "Network.setUserAgentOverride", map[string]any{"userAgent": "UA/1"})
	call(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": 1000, "uploadThroughput": -1})
	if e := callErr(t, c, sid, "Network.emulateNetworkConditions", map[string]any{"offline": true}); !strings.Contains(e.Message, "offline") {
		t.Fatalf("offline: %v", e)
	}
	for _, m := range []string{"Network.enable", "Network.setResourceCachingDisabled", "Page.overrideUserAgent", "Network.setEmulatedConditions"} {
		if len(f.ReceivedMethod(m)) == 0 {
			t.Fatalf("WIP %s not sent; received %v", m, received(f))
		}
	}
	call(t, c, sid, "Network.disable", nil)
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.7", "main", "0.1", "Script", "https://one.example/x.js", nil))
	call(t, c, sid, "Page.getFrameTree", nil) // a round trip after the event
	time.Sleep(50 * time.Millisecond)
	if got := sessionEvents(c, seq, sid, "Network.requestWillBeSent"); len(got) != 0 {
		t.Fatalf("events after Network.disable: %v", got)
	}
	if len(f.ReceivedMethod("Network.disable")) == 0 {
		t.Fatalf("WIP Network.disable not sent after the last owner left; received %v", received(f))
	}
}

// Request ids are unique across targets: WIP numbers requests per
// process, so a provisional target repeats the active target's ids. The
// provisional target's document request is published at its commit, with
// requestId == loaderId == frameNavigated's loaderId.
func TestNetworkIDsAcrossSwap(t *testing.T) {
	r, _, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.1", "main", "0.1", "Script", "https://one.example/a.js", nil))
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil)
	waitResumed(t, f.Provisional("next"))
	f.EmitFrom("next", "Network.requestWillBeSent", wipRequest("0.1", "9.1", "0.7", "Document", "https://two.example/", nil))
	f.EmitFrom("next", "Network.responseReceived", map[string]any{
		"requestId": "0.1", "frameId": "9.1", "loaderId": "0.7", "timestamp": 1.0, "type": "Document",
		"response": map[string]any{"url": "https://two.example/", "status": 200, "statusText": "OK", "headers": map[string]string{}, "mimeType": "text/html"},
	})
	f.EmitFrom("next", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "9.1", "loaderId": "0.7", "url": "https://two.example/", "securityOrigin": "https://two.example"}})
	call(t, c, sid, "Page.getFrameTree", nil)
	if got := netEvents(t, c, seq, sid, "Network.requestWillBeSent"); len(got) != 1 {
		t.Fatalf("requests before the commit: %+v, want only the active target's", got)
	}
	f.Commit("next")
	fn := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params).Frame
	reqs := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	failed := netEvents(t, c, seq, sid, "Network.loadingFailed")
	if len(reqs) != 2 || reqs[0].RequestID == reqs[1].RequestID {
		t.Fatalf("requests %+v, want two distinct ids", reqs)
	}
	doc := reqs[1]
	if doc.RequestID != doc.LoaderID || doc.LoaderID != fn.LoaderID || doc.FrameID != target || fn.ID != target {
		t.Fatalf("document request %+v, frameNavigated %+v: want requestId == loaderId == frameNavigated's loader on frame %s", doc, fn, target)
	}
	resp := netEvents(t, c, seq, sid, "Network.responseReceived")
	if len(resp) != 1 || resp[0].RequestID != doc.RequestID || resp[0].FrameID != target {
		t.Fatalf("response of the document %+v", resp)
	}
	// The replaced target's script request never ends: it is aborted.
	if len(failed) != 1 || failed[0].RequestID != reqs[0].RequestID || failed[0].ErrorText != "net::ERR_ABORTED" || !failed[0].Canceled {
		t.Fatalf("loadingFailed %+v, want the replaced target's request aborted", failed)
	}
}

// A provisional target destroyed without a commit takes its staged
// Network events with it.
func TestNetworkStagedEventsDroppedAtAbort(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	waitResumed(t, f.Provisional("gone"))
	f.EmitFrom("gone", "Network.requestWillBeSent", wipRequest("0.1", "9.1", "0.7", "Document", "https://two.example/", nil))
	f.DestroyTarget("gone")
	f.Emit("Network.requestWillBeSent", wipRequest("0.2", "main", "0.1", "Script", "https://one.example/b.js", nil))
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil)
	if got := netEvents(t, c, seq, sid, "Network.requestWillBeSent"); len(got) != 1 || got[0].Request.URL != "https://one.example/b.js" {
		t.Fatalf("requests %+v, want only the active target's", got)
	}
	if got := sessionEvents(c, seq, sid, "Network.loadingFailed"); len(got) != 0 {
		t.Fatalf("loadingFailed for a request the client never saw: %v", got)
	}
}

// A reset (staging.go) ends the requests in flight that the client saw
// start with loadingFailed; their later events are dropped. A request
// that the attachment holds paused stays.
func TestNetworkResetEndsRequestsInFlight(t *testing.T) {
	var got []string
	ps := newPageModel("T", func(method string, params any) {
		b, _ := json.Marshal(params)
		got = append(got, method+" "+string(b))
	})
	ps.setEnabled(domNetwork, true)
	ps.dispatch("t1", "Page.frameNavigated", mustRaw(map[string]any{"frame": map[string]any{"id": "main", "loaderId": "L1", "url": "https://x/"}}))
	ps.dispatch("t1", "Network.requestWillBeSent", mustRaw(wipRequest("1", "main", "L1", "Script", "https://x/a.js", nil)))
	ps.dispatch("t1", "Network.requestWillBeSent", mustRaw(wipRequest("2", "main", "L1", "Script", "https://x/b.js", nil)))
	ps.mu.Lock()
	held := ps.net.live[netKey{"t1", "2"}]
	ps.fetch.paused["job"] = pausedReq{key: netKey{"t1", "2"}, stage: stageRequest}
	first := ps.net.live[netKey{"t1", "1"}].id
	got = nil
	ps.resetLocked()
	ps.mu.Unlock()
	var failed []string
	for _, g := range got {
		if strings.HasPrefix(g, "Network.loadingFailed") {
			failed = append(failed, g)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], `"requestId":"`+first+`"`) || !strings.Contains(failed[0], `"canceled":true`) {
		t.Fatalf("loadingFailed at the reset %v, want the in-flight request %s only", failed, first)
	}
	got = nil
	ps.dispatch("t1", "Network.loadingFinished", mustRaw(map[string]any{"requestId": "1", "timestamp": 2.0}))
	ps.dispatch("t1", "Network.loadingFinished", mustRaw(map[string]any{"requestId": "2", "timestamp": 2.0}))
	if len(got) != 1 || !strings.Contains(got[0], `"requestId":"`+held.id+`"`) {
		t.Fatalf("events after the reset %v, want only the held request's end", got)
	}
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// The record bound (maxNetRecords): the oldest ended request makes room
// and its id then gets Chrome's eviction error; with only requests in
// flight, the oldest of them ends for the client with loadingFailed.
func TestNetworkRecordBoundEvicts(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.1", "main", "0.1", "Fetch", "https://one.example/first", map[string]any{
		"request": map[string]any{"url": "https://one.example/first", "method": "POST", "headers": map[string]string{}, "postData": "p"},
	}))
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "0.1", "timestamp": 100.1})
	first := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.loadingFinished", nil).Params).RequestID
	for i := 1; i <= maxNetRecords; i++ {
		id := fmt.Sprintf("1.%d", i)
		f.Emit("Network.requestWillBeSent", wipRequest(id, "main", "0.1", "Script", "https://one.example/"+id, nil))
	}
	last := fmt.Sprintf("/1.%d\"", maxNetRecords)
	// 2000 events take a while under the race detector.
	eventually(t, time.Minute, "the last request's requestWillBeSent", func() bool {
		return slices.ContainsFunc(sessionEvents(c, seq, sid, "Network.requestWillBeSent"), func(e cdptest.Event) bool {
			return strings.Contains(string(e.Params), last)
		})
	})
	for _, m := range []string{"Network.getResponseBody", "Network.getRequestPostData"} {
		if e := callErr(t, c, sid, m, map[string]any{"requestId": first}); e.Message != "Request content was evicted from inspector cache" || e.Code != -32000 {
			t.Fatalf("%s of the evicted request: %v, want Chrome's eviction error", m, e)
		}
	}
	if got := sessionEvents(c, seq, sid, "Network.loadingFailed"); len(got) != 0 {
		t.Fatalf("an ended request made room, but loadingFailed was sent: %v", got)
	}
	oldest := netEvents(t, c, seq, sid, "Network.requestWillBeSent")[1]
	f.Emit("Network.requestWillBeSent", wipRequest("2.1", "main", "0.1", "Script", "https://one.example/2.1", nil))
	failed := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.loadingFailed", nil).Params)
	if failed.RequestID != oldest.RequestID || !failed.Canceled || failed.ErrorText != "net::ERR_ABORTED" {
		t.Fatalf("loadingFailed %+v, want the oldest request in flight %s aborted", failed, oldest.RequestID)
	}
}

// Post data bounds: post data above postDataInEvent is left out of
// requestWillBeSent and read with getRequestPostData; above maxPostData it
// is not kept (eviction error).
func TestNetworkPostDataBounds(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	big, huge := strings.Repeat("a", postDataInEvent+1), strings.Repeat("b", maxPostData+1)
	seq := c.Seq()
	for _, q := range []struct{ id, post string }{{"0.1", big}, {"0.2", huge}} {
		f.Emit("Network.requestWillBeSent", wipRequest(q.id, "main", "0.1", "Fetch", "https://one.example/"+q.id, map[string]any{
			"request": map[string]any{"url": "https://one.example/" + q.id, "method": "POST", "headers": map[string]string{}, "postData": q.post},
		}))
	}
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", func(p json.RawMessage) bool { return strings.Contains(string(p), "/0.2") })
	reqs := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	if reqs[0].Request.PostData != "" || !reqs[0].Request.HasPostData {
		t.Fatalf("requestWillBeSent with %d bytes of post data carried %d of them, want none and hasPostData", len(big), len(reqs[0].Request.PostData))
	}
	if pd := decode[struct{ PostData string }](t, call(t, c, sid, "Network.getRequestPostData", map[string]any{"requestId": reqs[0].RequestID})); pd.PostData != big {
		t.Fatalf("getRequestPostData returned %d bytes, want %d", len(pd.PostData), len(big))
	}
	if e := callErr(t, c, sid, "Network.getRequestPostData", map[string]any{"requestId": reqs[1].RequestID}); e.Message != "Request content was evicted from inspector cache" {
		t.Fatalf("post data above the bound: %v", e)
	}
}

// All kept post data together stays within maxPostTotal: the oldest goes
// first (getRequestPostData then gets the eviction error).
func TestNetworkPostDataTotalBound(t *testing.T) {
	ps := newPageModel("T", func(string, any) {})
	ps.setEnabled(domNetwork, true)
	post := strings.Repeat("p", maxPostData)
	n := maxPostTotal/maxPostData + 1
	for i := range n {
		ps.dispatch("t1", "Network.requestWillBeSent", mustRaw(wipRequest(fmt.Sprint(i), "main", "L1", "Fetch", "https://x/", map[string]any{
			"request": map[string]any{"url": "https://x/", "method": "POST", "headers": map[string]string{}, "postData": post},
		})))
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	first, last := ps.net.live[netKey{"t1", "0"}], ps.net.live[netKey{"t1", fmt.Sprint(n - 1)}]
	if ps.net.postBytes > maxPostTotal || !first.postGone || first.post != "" || last.post != post {
		t.Fatalf("kept %d bytes (bound %d); oldest gone %v, newest kept %v", ps.net.postBytes, maxPostTotal, first.postGone, last.post == post)
	}
}

// A cached response gets requestServedFromCache before responseReceived;
// a TLS connection keeps its ssl phase; an event without a timestamp
// takes its arrival time; a WIP id reported again without a redirect is
// a new request with a new CDP id.
func TestNetworkCacheTLSAndRepeatedID(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.3", "main", "0.1", "Script", "https://one.example/s.js", nil))
	f.Emit("Network.responseReceived", map[string]any{
		"requestId": "0.3", "frameId": "main", "loaderId": "0.1", "timestamp": 100.2, "type": "Script",
		"response": map[string]any{
			"url": "https://one.example/s.js", "status": 200, "statusText": "OK", "headers": map[string]string{}, "mimeType": "text/javascript", "source": "disk-cache",
			"timing": map[string]any{"fetchStart": 100.01, "domainLookupStart": 0.5, "domainLookupEnd": 1, "connectStart": 1.5, "connectEnd": 4, "secureConnectionStart": 2, "requestStart": 4.5, "responseStart": 9},
		},
	})
	f.Emit("Network.dataReceived", map[string]any{"requestId": "0.3", "dataLength": 2, "encodedDataLength": 2})
	f.Emit("Network.requestWillBeSent", wipRequest("0.3", "main", "0.1", "Script", "https://one.example/again.js", nil))
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", func(p json.RawMessage) bool { return strings.Contains(string(p), "again.js") })
	var order []string
	for _, e := range c.Events() {
		if e.Seq > seq && e.SessionID == sid && strings.HasPrefix(e.Method, "Network.") {
			order = append(order, e.Method)
		}
	}
	if strings.Join(order, ",") != "Network.requestWillBeSent,Network.requestServedFromCache,Network.responseReceived,Network.dataReceived,Network.requestWillBeSent" {
		t.Fatalf("events %v", order)
	}
	tm := netEvents(t, c, seq, sid, "Network.responseReceived")[0].Response.Timing
	if tm["sslStart"].(float64) != 2 || tm["sslEnd"].(float64) != 4 || tm["dnsStart"].(float64) != 0.5 {
		t.Fatalf("timing %v, want ssl 2 to 4 and dns from 0.5", tm)
	}
	reqs := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	data := netEvents(t, c, seq, sid, "Network.dataReceived")[0]
	if reqs[0].RequestID == reqs[1].RequestID || data.Timestamp < reqs[0].Timestamp {
		t.Fatalf("requests %+v and data %+v, want two ids and an arrival time for the data", reqs, data)
	}
}
