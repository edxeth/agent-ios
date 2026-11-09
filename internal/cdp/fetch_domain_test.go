package cdp

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Fetch domain tests (design 2.10, 5.3; 7.8 item 12), on the fake page.

type pausedEvent struct {
	RequestID    string `json:"requestId"`
	NetworkID    string `json:"networkId"`
	FrameID      string `json:"frameId"`
	ResourceType string `json:"resourceType"`
	Request      struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"request"`
	ResponseStatusCode int `json:"responseStatusCode"`
}

// interceptReq makes the fake page pause request id of target for u and
// returns the requestPaused the client got for it.
func interceptReq(t *testing.T, c *cdptest.Client, f *fakewebkit.Server, sid, target, id, u string) pausedEvent {
	t.Helper()
	seq := c.Seq()
	f.InterceptEvent(target, id, "request", map[string]any{"request": map[string]any{"url": u, "method": "GET", "headers": map[string]string{"Accept": "*/*"}}})
	return decode[pausedEvent](t, waitEvent(t, c, seq, sid, "Fetch.requestPaused", func(p json.RawMessage) bool {
		return strings.Contains(string(p), u)
	}).Params)
}

// answeredWith waits until the fake page took the hold of id with method.
func answeredWith(t *testing.T, f *fakewebkit.Server, id, method string) {
	t.Helper()
	eventually(t, 5*time.Second, "an answer "+method+" of "+id, func() bool {
		return slices.ContainsFunc(f.Answers(), func(a fakewebkit.Answer) bool { return a.RequestID == id && a.Method == method })
	})
}

func enableFetch(t *testing.T) (*rig, *fakePage, *cdptest.Client, string, string, *fakewebkit.Server) {
	t.Helper()
	r, fp, c, sid, target := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Fetch.enable", map[string]any{"handleAuthRequests": true, "patterns": []any{map[string]any{"urlPattern": "*"}}})
	return r, fp, c, sid, target, r.fake["One"]
}

func TestFetchPausedRequestsAndAnswers(t *testing.T) {
	_, _, c, sid, target, f := enableFetch(t)
	rules := f.ReceivedMethod("Network.addInterception")
	if len(f.ReceivedMethod("Network.setInterceptionEnabled")) != 1 || len(rules) != 1 ||
		!strings.Contains(string(rules[0].Params), `"url":"^.*(?:\\x00`) || !strings.Contains(string(rules[0].Params), `"isRegex":true`) {
		t.Fatalf("WIP rules %v, want interception on and one anchored regex rule", rules)
	}
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.5", "main", "0.1", "Fetch", "https://one.example/resource", nil))
	rq := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil).Params)
	p := interceptReq(t, c, f, sid, "wk-One", "0.5", "https://one.example/resource")
	if p.NetworkID != rq.RequestID || p.FrameID != target || p.ResourceType != "Fetch" || p.Request.Method != "GET" || !strings.HasPrefix(p.RequestID, "interception-job-") {
		t.Fatalf("requestPaused %+v, want networkId %s, frame %s, type Fetch", p, rq.RequestID, target)
	}
	call(t, c, sid, "Fetch.fulfillRequest", map[string]any{
		"requestId": p.RequestID, "responseCode": 200,
		"responseHeaders": []any{map[string]any{"name": "Content-Type", "value": "text/plain; charset=utf-8"}}, "body": "b2s=",
	})
	answeredWith(t, f, "0.5", "Network.interceptRequestWithResponse")
	sent := f.ReceivedMethod("Network.interceptRequestWithResponse")
	for _, want := range []string{`"content":"b2s="`, `"base64Encoded":true`, `"status":200`, `"statusText":"OK"`, `"mimeType":"text/plain"`} {
		if !strings.Contains(string(sent[0].Params), want) {
			t.Fatalf("interceptRequestWithResponse %s lacks %s", sent[0].Params, want)
		}
	}
	if e := callErr(t, c, sid, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200}); e.Message != "Invalid InterceptionId." || e.Code != -32602 {
		t.Fatalf("second answer: %v", e)
	}

	p = interceptReq(t, c, f, sid, "wk-One", "0.6", "https://one.example/c")
	call(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
	answeredWith(t, f, "0.6", "Network.interceptContinue")
	p = interceptReq(t, c, f, sid, "wk-One", "0.7", "https://one.example/f")
	call(t, c, sid, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "Aborted"})
	answeredWith(t, f, "0.7", "Network.interceptRequestWithError")
	if got := f.ReceivedMethod("Network.interceptRequestWithError"); !strings.Contains(string(got[0].Params), `"errorType":"Cancellation"`) {
		t.Fatalf("interceptRequestWithError %s", got[0].Params)
	}
	p = interceptReq(t, c, f, sid, "wk-One", "0.8", "https://one.example/m")
	call(t, c, sid, "Fetch.continueRequest", map[string]any{
		"requestId": p.RequestID, "method": "POST",
		"headers": []any{map[string]any{"name": "X-A", "value": "1"}},
	})
	answeredWith(t, f, "0.8", "Network.interceptWithRequest")
	if got := f.ReceivedMethod("Network.interceptWithRequest"); !strings.Contains(string(got[0].Params), `"headers":{"X-A":"1"}`) {
		t.Fatalf("interceptWithRequest %s", got[0].Params)
	}
	if e := callErr(t, c, sid, "Fetch.continueWithAuth", map[string]any{"requestId": "x"}); e.Message != "authChallengeResponse not expected." || e.Code != -32602 {
		t.Fatalf("continueWithAuth: %v", e)
	}
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": "interception-job-99.0"}); e.Message != "Invalid InterceptionId." {
		t.Fatalf("unknown id: %v", e)
	}
}

// The control lane (design 2.10, item 12): an answer to a paused request
// is not queued behind the evaluation whose promise waits for it.
func TestFetchAnswerNotQueuedBehindAwaitedEvaluate(t *testing.T) {
	_, fp, c, sid, _, f := enableFetch(t)
	fp.eval = func(expr string) (any, bool) {
		if strings.Contains(expr, "fetch(") {
			return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}}, true
		}
		return nil, false
	}
	// As in WebKit, the promise settles only when the paused fetch is
	// answered: the fake holds the awaitPromise reply until the page gets
	// the fulfillment. A handler that blocked instead would stop the fake's
	// only read loop, and no answer could arrive at all.
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "ok"}})
	f.HoldUntil("Runtime.awaitPromise", "Network.interceptRequestWithResponse")
	done := make(chan json.RawMessage, 1)
	go func() {
		res, _ := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.evaluate", map[string]any{
			"expression": "fetch('/resource').then(r => r.text())", "awaitPromise": true, "returnByValue": true,
		})
		done <- res
	}()
	eventually(t, 5*time.Second, "the awaited evaluation", func() bool { return len(f.ReceivedMethod("Runtime.awaitPromise")) > 0 })
	p := interceptReq(t, c, f, sid, "wk-One", "0.9", "https://one.example/resource")
	start := time.Now()
	if _, err := c.Call(ctxFor(t, 2*time.Second), sid, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200, "body": "b2s="}); err != nil {
		t.Fatalf("fulfillRequest while the evaluation waits: %v", err)
	}
	select {
	case res := <-done:
		if !strings.Contains(string(res), `"value":"ok"`) || time.Since(start) > 2*time.Second {
			t.Fatalf("evaluate = %s after %v, want ok within 2 s", res, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the awaited evaluation did not finish after the answer")
	}
}

// No request stays paused: Fetch.disable continues the paused requests
// and releases the rules; a request that the old rule pauses later is
// continued too.
func TestFetchDisableContinuesPaused(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	interceptReq(t, c, f, sid, "wk-One", "1.1", "https://one.example/a")
	call(t, c, sid, "Fetch.disable", nil)
	answeredWith(t, f, "1.1", "Network.interceptContinue")
	if len(f.ReceivedMethod("Network.removeInterception")) == 0 {
		t.Fatalf("the rule was not removed; received %v", received(f))
	}
	seq := c.Seq()
	f.InterceptEvent("wk-One", "1.2", "request", map[string]any{"request": map[string]any{"url": "https://one.example/late", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "1.2", "Network.interceptContinue")
	if got := sessionEvents(c, seq, sid, "Fetch.requestPaused"); len(got) != 0 {
		t.Fatalf("requestPaused after Fetch.disable: %v", got)
	}
}

// The detach continues the reported paused request, and also a request
// that the rule paused before its removal took effect: WebKit sends that
// requestIntercepted before its reply to Network.removeInterception, after
// the attachment stopped reporting.
func TestFetchDetachContinuesPaused(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	interceptReq(t, c, f, sid, "wk-One", "2.1", "https://one.example/a")
	var late sync.Once
	f.Handle("Network.removeInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		late.Do(func() {
			f.InterceptEvent("wk-One", "2.2", "request", map[string]any{"request": map[string]any{"url": "https://one.example/late", "method": "GET", "headers": map[string]string{}}})
		})
		return nil, nil
	})
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	answeredWith(t, f, "2.1", "Network.interceptContinue")
	if len(f.ReceivedMethod("Network.removeInterception")) == 0 {
		t.Fatalf("the detach did not remove the rule; received %v", received(f))
	}
	answeredWith(t, f, "2.2", "Network.interceptContinue")
}

func TestFetchConnectionLossContinuesPaused(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	interceptReq(t, c, f, sid, "wk-One", "3.1", "https://one.example/a")
	c.Close()
	answeredWith(t, f, "3.1", "Network.interceptContinue")
}

func TestFetchLeaseEndContinuesPaused(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	interceptReq(t, c, f, sid, "wk-One", "4.1", "https://one.example/a")
	seq := c.Seq()
	if _, err := r.m.ReleaseSession(ctxFor(t, 5*time.Second), "s1"); err != nil {
		t.Fatal(err)
	}
	answeredWith(t, f, "4.1", "Network.interceptContinue")
	waitEvent(t, c, seq, "", "Target.detachedFromTarget", nil)
}

// A provisional target's paused request is reported at once with its
// requestWillBeSent (the staging rule of the document request); when the
// target is abandoned, the request ends as aborted.
func TestFetchProvisionalRequestPublishedEarly(t *testing.T) {
	_, _, c, sid, target, f := enableFetch(t)
	seq := c.Seq()
	waitResumed(t, f.Provisional("next"))
	f.EmitFrom("next", "Network.requestWillBeSent", wipRequest("0.1", "9.1", "0.7", "Document", "https://two.example/", nil))
	p := interceptReq(t, c, f, sid, "next", "0.1", "https://two.example/")
	rq := netEvents(t, c, seq, sid, "Network.requestWillBeSent")
	if len(rq) != 1 || rq[0].RequestID != p.NetworkID || rq[0].RequestID != rq[0].LoaderID || p.FrameID != target {
		t.Fatalf("requests %+v and paused %+v, want the document request (requestId == loaderId) before its pause", rq, p)
	}
	f.DestroyTarget("next")
	failed := decode[cdpNetEvent](t, waitEvent(t, c, seq, sid, "Network.loadingFailed", nil).Params)
	if failed.RequestID != p.NetworkID || !failed.Canceled {
		t.Fatalf("loadingFailed %+v, want the abandoned document request", failed)
	}
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID}); e.Message != "Invalid InterceptionId." {
		t.Fatalf("answer for a destroyed target's request: %v", e)
	}
}

// CLI and CDP interception on one page (decision, fetch_domain.go): a
// request that the session's CLI route matches is the route's and is not
// reported; a request that only the CDP rule matches is the client's and
// the CLI safety net leaves it; neither strands the other's.
func TestFetchAndCLIRouteOnOnePage(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "test route", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	dp, err := devtools.For(s.pg.PS)
	if err != nil {
		t.Fatal(err)
	}
	body := "cli"
	if _, err := dp.AddRoute(mo.Op.Context(), mo.Op.Caller(), "s1", devtools.Route{Pattern: "**/cli*", Body: &body, Mode: devtools.ModeWIP}); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)

	seq := c.Seq()
	f.InterceptEvent("wk-One", "5.1", "request", map[string]any{"request": map[string]any{"url": "https://one.example/cli1", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "5.1", "Network.interceptRequestWithResponse")
	p := interceptReq(t, c, f, sid, "wk-One", "5.2", "https://one.example/cdp")
	for _, e := range sessionEvents(c, seq, sid, "Fetch.requestPaused") {
		if strings.Contains(string(e.Params), "/cli1") {
			t.Fatalf("the CLI route's request was reported to the CDP client: %s", e.Params)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if slices.ContainsFunc(f.Answers(), func(a fakewebkit.Answer) bool { return a.RequestID == "5.2" }) {
		t.Fatal("the CLI side answered the CDP client's request")
	}
	call(t, c, sid, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": 200, "body": "b2s="})
	answeredWith(t, f, "5.2", "Network.interceptRequestWithResponse")
	call(t, c, sid, "Fetch.disable", nil)
	f.InterceptEvent("wk-One", "5.3", "request", map[string]any{"request": map[string]any{"url": "https://one.example/cli2", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "5.3", "Network.interceptRequestWithResponse")
}

// globRegex: '*' any characters, '?' one, a backslash escapes, anchored;
// the per-attachment suffix matches no URL character.
func TestFetchGlobRegex(t *testing.T) {
	re := regexp.MustCompile(globRegex(`*/a?c\*.js`, "tag"))
	for u, want := range map[string]bool{
		"https://x/abc*.js": true, "https://x/a.c*.js": true, "https://x/abc.js": false, "https://x/abcc*.js": false,
		"https://x/abc*.jsx": false, "https://x/abc*.js\x00tag": true,
	} {
		if re.MatchString(u) != want {
			t.Fatalf("%s on %q = %v, want %v", re, u, !want, want)
		}
	}
}

// Patterns with a resource type and the response stage, the answers'
// parameter checks and conversions (Chrome's error texts for bad
// parameters), and the rule changes of a later enable.
func TestFetchPatternsStagesAndParams(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	if e := callErr(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"requestStage": "Later"}}}); e.Code != -32602 {
		t.Fatalf("unknown requestStage: %v", e)
	}
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{
		map[string]any{"urlPattern": "*/s*", "resourceType": "Script"},
		map[string]any{"urlPattern": "*/resp", "requestStage": "Response"},
	}})
	if rules := f.ReceivedMethod("Network.addInterception"); len(rules) != 2 || !strings.Contains(string(rules[1].Params), `"stage":"response"`) {
		t.Fatalf("rules %v, want a request rule and a response rule", rules)
	}

	// A request of another resource type is continued, not reported.
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("0.1", "main", "0.1", "Fetch", "https://one.example/sx", nil))
	f.InterceptEvent("wk-One", "0.1", "request", map[string]any{"request": map[string]any{"url": "https://one.example/sx", "method": "GET", "headers": map[string]string{}}})
	answeredWith(t, f, "0.1", "Network.interceptContinue")
	if got := sessionEvents(c, seq, sid, "Fetch.requestPaused"); len(got) != 0 {
		t.Fatalf("a Fetch request was reported for a Script pattern: %v", got)
	}

	// A Script request with post data is reported with it.
	f.Emit("Network.requestWillBeSent", wipRequest("0.2", "main", "0.1", "Script", "https://one.example/s.js", nil))
	seq = c.Seq()
	f.InterceptEvent("wk-One", "0.2", "request", map[string]any{"request": map[string]any{"url": "https://one.example/s.js", "method": "POST", "headers": map[string]string{}, "postData": "eA=="}})
	p := decode[struct {
		RequestID string
		Request   struct {
			PostData    string
			HasPostData bool
		}
	}](t, waitEvent(t, c, seq, sid, "Fetch.requestPaused", nil).Params)
	if p.Request.PostData != "eA==" || !p.Request.HasPostData {
		t.Fatalf("requestPaused %+v, want its post data", p)
	}
	for _, bad := range []struct {
		method string
		params map[string]any
	}{
		{"Fetch.fulfillRequest", map[string]any{"responseCode": 42}},
		{"Fetch.fulfillRequest", map[string]any{"responseCode": 200, "body": "!!"}},
		{"Fetch.fulfillRequest", map[string]any{"responseCode": 200, "binaryResponseHeaders": "!!"}},
		{"Fetch.failRequest", map[string]any{"errorReason": "Nope"}},
		{"Fetch.failRequest", map[string]any{"errorReason": 7}},
		{"Fetch.continueRequest", map[string]any{"url": 5}},
		{"Fetch.fulfillRequest", map[string]any{"responseCode": "200"}},
	} {
		bad.params["requestId"] = p.RequestID
		if e := callErr(t, c, sid, bad.method, bad.params); e.Code != -32602 || !strings.HasPrefix(e.Message, "Invalid parameters") {
			t.Fatalf("%s %v: %v, want invalid parameters", bad.method, bad.params, e)
		}
	}
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID, "interceptResponse": true}); e.Code != -32000 {
		t.Fatalf("interceptResponse: %v", e)
	}
	call(t, c, sid, "Fetch.continueRequest", map[string]any{
		"requestId": p.RequestID, "url": "https://one.example/t.js", "postData": "eQ==",
		"headers": []any{map[string]any{"name": "X", "value": "a"}, map[string]any{"name": "X", "value": "b"}},
	})
	answeredWith(t, f, "0.2", "Network.interceptWithRequest")
	got := string(f.ReceivedMethod("Network.interceptWithRequest")[0].Params)
	for _, want := range []string{`"url":"https://one.example/t.js"`, `"postData":"eQ=="`, `"X":"a, b"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("interceptWithRequest %s lacks %s", got, want)
		}
	}

	// The response stage: its status and headers; only fulfill and an
	// unchanged continue apply.
	seq = c.Seq()
	f.InterceptEvent("wk-One", "0.3", "response", map[string]any{"response": map[string]any{"url": "https://one.example/resp", "status": 404, "statusText": "Not Found", "headers": map[string]string{"B": "2", "A": "1"}}})
	rp := decode[struct {
		RequestID          string
		ResponseStatusCode int
		ResponseHeaders    []map[string]string
	}](t, waitEvent(t, c, seq, sid, "Fetch.requestPaused", nil).Params)
	if rp.ResponseStatusCode != 404 || len(rp.ResponseHeaders) != 2 || rp.ResponseHeaders[0]["name"] != "A" {
		t.Fatalf("response-stage requestPaused %+v, want status 404 and the headers sorted", rp)
	}
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": rp.RequestID, "url": "https://x/"}); e.Code != -32000 {
		t.Fatalf("a changed continue at the response stage: %v", e)
	}
	if e := callErr(t, c, sid, "Fetch.failRequest", map[string]any{"requestId": rp.RequestID, "errorReason": "Failed"}); e.Code != -32000 {
		t.Fatalf("failRequest at the response stage: %v", e)
	}
	call(t, c, sid, "Fetch.fulfillRequest", map[string]any{
		"requestId": rp.RequestID, "responseCode": 200, "body": "PGI+", // an omitted body is refused at this stage (TestReviewResponseFulfillOmittedBody)
		"binaryResponseHeaders": base64.StdEncoding.EncodeToString([]byte("Content-Type: text/html\x00X-Y: 1")),
	})
	answeredWith(t, f, "0.3", "Network.interceptWithResponse")
	got = string(f.ReceivedMethod("Network.interceptWithResponse")[0].Params)
	for _, want := range []string{`"mimeType":"text/html"`, `"X-Y":"1"`, `"status":200`} {
		if !strings.Contains(got, want) {
			t.Fatalf("interceptWithResponse %s lacks %s", got, want)
		}
	}

	// A later enable replaces the rules: without patterns one rule for
	// every URL, with an empty list none (interception off).
	call(t, c, sid, "Fetch.enable", nil)
	if n := len(f.ReceivedMethod("Network.removeInterception")); n != 2 || !strings.Contains(string(f.ReceivedMethod("Network.addInterception")[2].Params), `"url":"^.*(?:`) {
		t.Fatalf("removed %d rules, added %v; want both old rules removed and a rule for every URL", n, f.ReceivedMethod("Network.addInterception"))
	}
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{}})
	if on := f.ReceivedMethod("Network.setInterceptionEnabled"); len(on) != 2 || !strings.Contains(string(on[1].Params), `"enabled":false`) {
		t.Fatalf("setInterceptionEnabled %v, want it turned off without rules", on)
	}
}

// Fetch.disable of an attachment that never enabled Fetch sends nothing;
// a failed enable continues the requests it held paused.
func TestFetchDisableWithoutRulesAndFailedEnable(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Fetch.disable", nil)
	if got := f.ReceivedMethod("Network.removeInterception"); len(got) != 0 {
		t.Fatalf("Fetch.disable without rules sent %v", got)
	}
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Fetch.enable", nil)
	interceptReq(t, c, f, sid, "wk-One", "6.1", "https://one.example/a")
	f.Unsupported("Network.addInterception")
	if e := callErr(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/b"}}}); e == nil {
		t.Fatal("Fetch.enable succeeded although WebKit refused the rule")
	}
	answeredWith(t, f, "6.1", "Network.interceptContinue")
}
