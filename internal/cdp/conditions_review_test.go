package cdp

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Regression tests of the bridge review (parity-045 review-bridge.md,
// findings F1 to F6 and F9), ported from the reviewer's reproductions.

// sessionOf returns the page attachment sid of the rig.
func sessionOf(t *testing.T, r *rig, sid string) *session {
	t.Helper()
	var s *session
	r.srv.mu.Lock()
	for conn := range r.srv.conns {
		conn.mu.Lock()
		if conn.sessions[sid] != nil {
			s = conn.sessions[sid]
		}
		conn.mu.Unlock()
	}
	r.srv.mu.Unlock()
	if s == nil {
		t.Fatalf("no attachment %s", sid)
	}
	return s
}

// secondFetchClient attaches a second client to page One with Network and
// Fetch enabled for pats (nil: Fetch.enable without patterns).
func secondFetchClient(t *testing.T, r *rig, pats []any) (*session, string, func(method string, params any)) {
	t.Helper()
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, s2 := puppeteerAttach(t, c2)
	call(t, c2, s2, "Network.enable", nil)
	fp := map[string]any{}
	if pats != nil {
		fp["patterns"] = pats
	}
	call(t, c2, s2, "Fetch.enable", fp)
	return sessionOf(t, r, s2), s2, func(method string, params any) { call(t, c2, s2, method, params) }
}

// F3: an offline setup that WebKit refuses leaves no offline state: a
// request that a Fetch client pauses afterwards is reported to it and not
// failed.
func TestOfflineRefusedSetupFailsNothing(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	f.Handle("Network.addInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
	})
	if err := pptrConditions(t, c, sid, true, 0, 0, 0); err == nil {
		t.Fatal("offline install should fail")
	}
	p := interceptReq(t, c, f, sid, "wk-One", "r-review", "https://one.example/item")
	if n := len(f.ReceivedMethod("Network.interceptRequestWithError")); n != 0 {
		t.Fatalf("after a refused offline setup a request was failed offline (%d)", n)
	}
	call(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
	answeredWith(t, f, "r-review", "Network.interceptContinue")
}

// F4: a throttle that WebKit refuses is not recorded, so a clear and an
// identical retry reach WebKit again (core's condition owners hold only
// what WebKit holds).
func TestThrottleRefusedThenRetried(t *testing.T) {
	c, sid, f := versionPage(t, "27.3")
	f.Handle("Network.setEmulatedConditions", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
	})
	if err := pptrConditions(t, c, sid, false, 0, 50000, -1); err == nil {
		t.Fatal("first throttle should fail")
	}
	f.Handle("Network.setEmulatedConditions", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatal(err)
	}
	if err := pptrConditions(t, c, sid, false, 0, 50000, -1); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := strings.Join(conditionsSent(f), " "); got != `{"bandwidth":50000} {"bandwidth":50000}` {
		t.Fatalf("WIP received %s, want the refused throttle and its retry only", got)
	}
	if err := pptrConditions(t, c, sid, false, 0, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got := conditionsSent(f); got[len(got)-1] != "{}" {
		t.Fatalf("the clear after the retry sent %v, want {} last", got)
	}
}

// F1: a request that the offline rule paused while offline was on is
// answered even when offline is turned off before the attachment
// translated its event.
func TestOfflineOffAnswersQueuedHold(t *testing.T) {
	r, _, c1, s1, _ := attachPage(t)
	f := r.fake["One"]
	secondFetchClient(t, r, []any{map[string]any{"urlPattern": "https://one.example/other"}})
	if err := pptrConditions(t, c1, s1, true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	sess := sessionOf(t, r, s1)
	entered, unblock := make(chan struct{}), make(chan struct{})
	sess.page.mu.Lock()
	sess.page.testApply = func(ev wipEvent) {
		if ev.method == evRequestIntercepted {
			close(entered)
			<-unblock
		}
	}
	sess.page.mu.Unlock()
	f.InterceptEvent("wk-One", "off-review", "request", map[string]any{"request": map[string]any{"url": "https://one.example/item", "method": "GET"}})
	<-entered
	if err := pptrConditions(t, c1, s1, false, 0, -1, -1); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	if err := sess.page.caughtUp(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the held request is answered", func() bool { return len(f.HeldRequests("wk-One")) == 0 })
}

// F1: a request that the offline rule pauses while its attachment detaches
// (after the fence, before the release removed the rule) is answered,
// although another client's Fetch keeps interception enabled.
func TestOfflineDetachAnswersPausedRequest(t *testing.T) {
	r, _, c1, s1, _ := attachPage(t)
	f := r.fake["One"]
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, s2 := puppeteerAttach(t, c2)
	call(t, c2, s2, "Network.enable", nil)
	call(t, c2, s2, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "https://one.example/other"}}})
	if err := pptrConditions(t, c1, s1, true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	sess := sessionOf(t, r, s1)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.Handle("Network.setResourceCachingDisabled", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		once.Do(func() {
			close(entered)
			<-unblock
		})
		return map[string]any{}, nil
	})
	doneOp := make(chan error, 1)
	ctx := testCtx(t)
	go func() {
		_, err := c2.Call(ctx, s2, "Network.setCacheDisabled", map[string]any{"cacheDisabled": true})
		doneOp <- err
	}()
	<-entered
	result := make(chan error, 1)
	go func() {
		_, err := c1.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": s1})
		result <- err
	}()
	waitUntil(t, "offline attachment closed", func() bool {
		sess.page.mu.Lock()
		defer sess.page.mu.Unlock()
		return sess.page.closed
	})
	f.InterceptEvent("wk-One", "detach-review", "request", map[string]any{"request": map[string]any{"url": "https://one.example/item", "method": "GET"}})
	seq := c2.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("marker", "main", "0.1", "Fetch", "https://one.example/marker", nil))
	waitEvent(t, c2, seq, s2, "Network.requestWillBeSent", nil)
	close(unblock) // the fake page answers one call at a time
	if err := <-doneOp; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the request paused during the detach is answered", func() bool { return len(f.HeldRequests("wk-One")) == 0 })
}

// F2: offline wins over a Fetch client of another attachment for the
// requests it pauses while it is on: the request fails, the Fetch client
// never sees it, independent of the order in which the attachments
// translate the event.
func TestOfflineWinsOverOtherFetch(t *testing.T) {
	r, _, c1, s1, _ := attachPage(t)
	f := r.fake["One"]
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, s2 := puppeteerAttach(t, c2)
	call(t, c2, s2, "Fetch.enable", nil)
	if err := pptrConditions(t, c1, s1, true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	sess := sessionOf(t, r, s1)
	entered, unblock := make(chan struct{}), make(chan struct{})
	sess.page.mu.Lock()
	sess.page.testApply = func(ev wipEvent) {
		if ev.method == evRequestIntercepted {
			close(entered)
			<-unblock
		}
	}
	sess.page.mu.Unlock()
	f.InterceptEvent("wk-One", "bypass-review", "request", map[string]any{"request": map[string]any{"url": "https://one.example/item", "method": "GET"}})
	<-entered
	answeredWith(t, f, "bypass-review", "Network.interceptRequestWithError")
	close(unblock)
	if err := sess.page.caughtUp(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	s2s := sessionOf(t, r, s2)
	if err := s2s.page.caughtUp(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	for _, e := range c2.Events() {
		if e.Method == "Fetch.requestPaused" {
			t.Fatalf("the Fetch client got %s for a request that offline failed", e.Params)
		}
	}
	if n := len(f.Answers()); n != 1 {
		t.Fatalf("answers %v, want the offline failure only", f.Answers())
	}
}

// F6: a clear whose current-document restore WebKit refused keeps the
// obligation: the next clear tries the restore again.
func TestGeoClearRetriesFailedRestore(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Emulation.setGeolocationOverride", map[string]any{"latitude": 12, "longitude": 34, "accuracy": 1})
	var refuse atomic.Bool
	refuse.Store(true)
	f.Handle("Runtime.evaluate", func(k fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuse.Load() && strings.Contains(string(k.Params), "__iwdpGeo") {
			return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
		}
		return value(true), nil
	})
	if e := callErr(t, c, sid, "Emulation.clearGeolocationOverride", nil); !strings.Contains(e.Message, "injected refusal") {
		t.Fatalf("clear: %v, want the refused restore", e)
	}
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	if e := callErr(t, c, sid, "Emulation.clearGeolocationOverride", nil); !strings.Contains(e.Message, "injected refusal") {
		t.Fatalf("clear: %v, want the refused restore", e)
	}
	if after := len(f.ReceivedMethod("Runtime.evaluate")); after == before {
		t.Fatal("the second clear did not retry the failed current-document restore")
	}
	refuse.Store(false)
	call(t, c, sid, "Emulation.clearGeolocationOverride", nil)
	before = len(f.ReceivedMethod("Runtime.evaluate"))
	call(t, c, sid, "Emulation.clearGeolocationOverride", nil)
	if after := len(f.ReceivedMethod("Runtime.evaluate")); after != before {
		t.Fatal("a clear after a successful restore sent a restore again")
	}
}

// F6: the detach restores the current document when a clear's restore
// failed.
func TestGeoDetachRetriesFailedRestore(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Emulation.setGeolocationOverride", map[string]any{"latitude": 12, "longitude": 34, "accuracy": 1})
	var refuse atomic.Bool
	refuse.Store(true)
	f.Handle("Runtime.evaluate", func(k fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuse.Load() && strings.Contains(string(k.Params), "__iwdpGeo") {
			return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
		}
		return value(true), nil
	})
	if e := callErr(t, c, sid, "Emulation.clearGeolocationOverride", nil); !strings.Contains(e.Message, "injected refusal") {
		t.Fatalf("clear: %v, want the refused restore", e)
	}
	refuse.Store(false)
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	waitUntil(t, "the detach restores the current document", func() bool {
		for _, m := range f.ReceivedMethod("Runtime.evaluate")[before:] {
			if strings.Contains(string(m.Params), "__iwdpGeo") {
				return true
			}
		}
		return false
	})
}

// F9: CDP applies the first matching rule; an unthrottled rule for some
// URLs before a global throttle exempts those URLs, which WIP's global
// throttle cannot express: refused before any change.
func TestConditionsByRuleRefusesURLException(t *testing.T) {
	c, sid, f := versionPage(t, "27.3")
	_, err := c.Call(testCtx(t), sid, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
		map[string]any{"urlPattern": "https://one.example/*", "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1},
		map[string]any{"urlPattern": "", "latency": 0, "downloadThroughput": 50000, "uploadThroughput": -1},
	}})
	if err == nil || !strings.Contains(err.Error(), "some URLs only") {
		t.Fatalf("an unthrottled URL exception before a global throttle: %v, want the refusal", err)
	}
	if got := conditionsSent(f); len(got) != 0 {
		t.Fatalf("the refused rules sent %v", got)
	}
	// Rules that give every URL the same conditions are expressible: a
	// URL rule equal to the global one, and rules after the first global
	// one (never reached).
	call(t, c, sid, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
		map[string]any{"urlPattern": "https://one.example/*", "latency": 0, "downloadThroughput": 50000, "uploadThroughput": -1},
		map[string]any{"urlPattern": "", "latency": 0, "downloadThroughput": 50000, "uploadThroughput": -1},
		map[string]any{"urlPattern": "https://two.example/*", "offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1},
	}})
	if got := strings.Join(conditionsSent(f), " "); got != `{"bandwidth":50000}` {
		t.Fatalf("WIP received %s", got)
	}
}

// F5: last set wins: replacing an override makes it the latest, so
// clearing another attachment's override installs the latest surviving
// one in the current document (and in new documents, by script order).
func TestGeoClearKeepsLatestSurvivor(t *testing.T) {
	r, _, c, a, target := attachPage(t)
	f := r.fake["One"]
	attach := func() string {
		return decode[struct {
			SessionID string `json:"sessionId"`
		}](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	}
	b, cc := attach(), attach()
	set := func(sid string, lat float64) {
		call(t, c, sid, "Emulation.setGeolocationOverride", map[string]any{"latitude": lat, "longitude": 0, "accuracy": 1})
	}
	set(a, 1)
	set(b, 2)
	set(cc, 3)
	set(b, 4)
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	call(t, c, a, "Emulation.clearGeolocationOverride", nil)
	var evals []string
	for _, m := range f.ReceivedMethod("Runtime.evaluate")[before:] {
		var p struct{ Expression string }
		_ = json.Unmarshal(m.Params, &p)
		evals = append(evals, p.Expression)
	}
	if len(evals) != 2 || evals[0] != devtools.GeoRestore || evals[1] != devtools.GeoShim(4, 0, 1) {
		t.Fatalf("the clear evaluated:\n%s\nwant the restore, then the latest survivor (latitude 4)", strings.Join(evals, "\n---\n"))
	}
}

// Closure review rev2-cdp-logpoint F5: a clear whose Page.setBootstrapScript
// WebKit refused leaves the override where it was in the script order; it
// does not come back as the latest and so win over an override set after it.
func TestGeoFailedClearKeepsScriptOrder(t *testing.T) {
	r, _, c, a, target := attachPage(t)
	f := r.fake["One"]
	b := decode[struct {
		SessionID string `json:"sessionId"`
	}](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, a, "Emulation.setGeolocationOverride", map[string]any{"latitude": 1, "longitude": 0, "accuracy": 1})
	call(t, c, b, "Emulation.setGeolocationOverride", map[string]any{"latitude": 2, "longitude": 0, "accuracy": 1})
	var refuse atomic.Bool
	refuse.Store(true)
	f.Handle("Page.setBootstrapScript", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuse.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
		}
		return map[string]any{}, nil
	})
	if e := callErr(t, c, a, "Emulation.clearGeolocationOverride", nil); !strings.Contains(e.Message, "injected refusal") {
		t.Fatalf("clear: %v, want the refused bootstrap script", e)
	}
	refuse.Store(false)
	// A third override sends the whole script again without moving the
	// first two.
	cc := decode[struct {
		SessionID string `json:"sessionId"`
	}](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, cc, "Emulation.setGeolocationOverride", map[string]any{"latitude": 3, "longitude": 0, "accuracy": 1})
	sent := f.ReceivedMethod("Page.setBootstrapScript")
	var p struct{ Source string }
	_ = json.Unmarshal(sent[len(sent)-1].Params, &p)
	one, two := strings.Index(p.Source, devtools.GeoShim(1, 0, 1)), strings.Index(p.Source, devtools.GeoShim(2, 0, 1))
	if one < 0 || two < 0 || one > two {
		t.Fatalf("after the failed clear the script runs latitude 1 at %d and latitude 2 at %d; want 1 before 2 (2 was set last)", one, two)
	}
}
