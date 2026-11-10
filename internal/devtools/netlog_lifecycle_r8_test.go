package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W4 round 8, review findings netlog.go:620 (P1), :464 and :364 (P2):
// the life of a log id. The first three tests are the reviewer's
// reproductions (/home/devkit/.pi/artifacts/reviewer/w4-r7-evidence/additional-repros_test.go);
// TestNetlogIDLifecycleProperty checks the id semantics of Request and
// docs/notes/w4.md over random event sequences.

// netEvent applies one Network event of target to n, as the read loop does.
func netEvent(n *Netlog, target, method string, e map[string]any) {
	raw, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	n.event(target, method, raw)
}

// WebKit keeps a body only for the latest request with a WebKit id. Once a
// later request of the target reuses the id, the earlier request's log id
// gives the body read before the reuse, or "unavailable"; it never reads
// WebKit, which would answer with the later request's body.
func TestBodyOfARequestWhoseWebKitIDWasReused(t *testing.T) {
	r := newRig(t)
	n := startNetlogOf(t, r)
	target := r.f.ActiveTarget()
	body := "first-body"
	r.f.Handle("Network.getResponseBody", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"body": body, "base64Encoded": false}, nil
	})
	finishedOn(r.f, target, "1.1", "https://fixture.example/first")
	finishedOn(r.f, target, "1.2", "https://fixture.example/cached")
	first := requestTo(t, n, "https://fixture.example/first", StateFinished)
	cached := requestTo(t, n, "https://fixture.example/cached", StateFinished)
	if got, err := bodyOf(t, r, n, cached.ID); err != nil || got != "first-body" {
		t.Fatalf("setup: body of %s = %q, %v", cached.ID, got, err)
	}
	body = "second-body" // WebKit's slots of 1.1 and 1.2 now hold the later requests
	finishedOn(r.f, target, "1.1", "https://fixture.example/second")
	finishedOn(r.f, target, "1.2", "https://fixture.example/cached-second")
	second := requestTo(t, n, "https://fixture.example/second", StateFinished)
	requestTo(t, n, "https://fixture.example/cached-second", StateFinished)
	if first.ID == second.ID {
		t.Fatal("setup: log ids not distinct")
	}
	calls := len(received(r.f, "Network.getResponseBody"))
	if got, err := bodyOf(t, r, n, first.ID); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) after %s reused its WebKit id = %q, %v; want unavailable", first.ID, second.ID, got, err)
	}
	if got, err := bodyOf(t, r, n, cached.ID); err != nil || got != "first-body" {
		t.Fatalf("Body(%q), read before the reuse = %q, %v; want the cached first-body", cached.ID, got, err)
	}
	if got := len(received(r.f, "Network.getResponseBody")); got != calls {
		t.Fatalf("%d getResponseBody calls for requests whose WebKit id was reused, want none", got-calls)
	}
	if got, err := bodyOf(t, r, n, second.ID); err != nil || got != "second-body" {
		t.Fatalf("Body(%q) = %q, %v; want second-body", second.ID, got, err)
	}
}

// After Clear, a later request can get the same log id, and another read
// can cache its body under that id while a read of the cleared request
// waits. The waiting read returns neither that body nor its own reply.
func TestBodyAfterClearDoesNotReturnTheNextRequestsCachedBody(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	k := reqKey{"t", "1"}
	q := finishedLife(n, wk, k, "https://fixture.example/first", 1)
	h := readHook{wk, func() {
		n.Clear()
		next := finishedLife(n, wk, k, "https://fixture.example/second", 3)
		if next.ID != q.ID {
			t.Errorf("setup: the next request has id %q, want the freed %q", next.ID, q.ID)
		}
		if data, _, err := n.Body(context.Background(), wk, next.ID); err != nil || string(data) != "body of "+next.URL {
			t.Errorf("setup: the other reader got %q, %v", data, err)
		}
	}}
	if got, _, err := n.Body(context.Background(), h, q.ID); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) of %s across Clear = %q, %v; want unavailable", q.ID, q.URL, got, err)
	}
}

// A body read before its WebKit id was reused stays readable from the
// cache; when the cache bound drops it later, the request's body is
// unavailable: WebKit would answer with the later request's body.
func TestBodyOfAReusedIDAfterTheCacheDropsIt(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	k := reqKey{"t", "1"}
	for i, url := range []string{"https://fixture.example/first", "https://fixture.example/second"} {
		netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, url, float64(2*i)))
		netEvent(n, k.target, "Network.loadingFinished", map[string]any{"requestId": k.id, "timestamp": float64(2*i + 1)})
		wk.latest[k] = url
		if i == 0 {
			first := n.Snapshot(Filter{}).Requests[0]
			if got, _, err := n.Body(context.Background(), wk, first.ID); err != nil || string(got) != "body of "+url {
				t.Fatalf("setup: Body(%q) = %q, %v", first.ID, got, err)
			}
		}
	}
	first, ok := recordOf(n, "https://fixture.example/first")
	if !ok || first.BodyGone != "" {
		t.Fatalf("setup: first request %+v (%v); want its cached body readable", first, ok)
	}
	n.SetLimits(DefaultMaxEntries, 0) // the cache bound drops the first body
	calls := wk.calls
	got, _, err := n.Body(context.Background(), wk, first.ID)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) after the cache dropped it = %q, %v; want unavailable", first.ID, got, err)
	}
	if wk.calls != calls {
		t.Fatalf("Body(%q) read WebKit, which has the later request's body", first.ID)
	}
	if q, _ := n.Get(first.ID); !strings.Contains(q.BodyGone, "reused") {
		t.Fatalf("BodyGone of %q = %q, want the reuse", first.ID, q.BodyGone)
	}
}

// A network reset clears WebKit's bodies: a body read before it stays
// readable from the cache, and once the cache bound drops it, Body says
// so without asking WebKit.
func TestBodyReadBeforeAResetAfterTheCacheDropsIt(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	url := "https://fixture.example/first"
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("1", url, 1))
	netEvent(n, "t", "Network.loadingFinished", map[string]any{"requestId": "1", "timestamp": 2})
	wk.latest[reqKey{"t", "1"}] = url
	first, _ := recordOf(n, url)
	if got, _, err := n.Body(context.Background(), wk, first.ID); err != nil || string(got) != "body of "+url {
		t.Fatalf("setup: Body(%q) = %q, %v", first.ID, got, err)
	}
	now := time.Now()
	n.onGap(core.NetworkGap{Start: now, End: now})
	clear(wk.latest)
	if got, _, err := n.Body(context.Background(), wk, first.ID); err != nil || string(got) != "body of "+url {
		t.Fatalf("Body(%q) read before the reset = %q, %v; want the cached body", first.ID, got, err)
	}
	n.SetLimits(DefaultMaxEntries, 0)
	calls := wk.calls
	if got, _, err := n.Body(context.Background(), wk, first.ID); err == nil || !strings.Contains(err.Error(), "network reset") {
		t.Fatalf("Body(%q) after the cache dropped it = %q, %v; want the network reset", first.ID, got, err)
	}
	if wk.calls != calls {
		t.Fatalf("Body(%q) asked WebKit after the reset cleared its bodies", first.ID)
	}
}

// readHook is a lifeWebKit that calls during before it answers a command.
type readHook struct {
	*lifeWebKit
	during func()
}

func (h readHook) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	h.during()
	return h.lifeWebKit.Send(ctx, method, params)
}

// A later request that reuses the WebKit id while Body reads the body:
// WebKit's reply can be that request's body, so Body says unavailable and
// caches nothing.
func TestBodyOfARequestWhoseIDIsReusedDuringTheRead(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	k := reqKey{"t", "1"}
	first, second := "https://fixture.example/first", "https://fixture.example/second"
	netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, first, 1))
	netEvent(n, k.target, "Network.loadingFinished", map[string]any{"requestId": k.id, "timestamp": 2})
	wk.latest[k] = first
	q, _ := recordOf(n, first)
	reuse := readHook{wk, func() {
		netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, second, 3))
		wk.latest[k] = second
	}}
	if got, _, err := n.Body(context.Background(), reuse, q.ID); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) with a reuse during the read = %q, %v; want unavailable", q.ID, got, err)
	}
	if got, _, err := n.Body(context.Background(), wk, q.ID); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) after the reuse = %q, %v; want unavailable (nothing cached)", q.ID, got, err)
	}
}

// W4 round 9, review finding netlog.go:758 (P1): a reply that arrives after
// the log dropped the record (Clear) and WebKit reused the id is discarded.
// The reviewer's reproduction
// (/home/devkit/.pi/artifacts/reviewer/w4-r8-evidence/during-read-repros_test.go).
func TestBodyDiscardsAReplyAfterClearAndReuseDuringTheRead(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	k := reqKey{"t", "1"}
	first, second := "https://fixture.example/first", "https://fixture.example/second"
	netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, first, 1))
	netEvent(n, k.target, "Network.loadingFinished", map[string]any{"requestId": k.id, "timestamp": 2})
	wk.latest[k] = first
	q, _ := recordOf(n, first)
	h := readHook{wk, func() {
		n.Clear()
		netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, second, 3))
		wk.latest[k] = second
	}}
	got, _, err := n.Body(context.Background(), h, q.ID)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Body(%q) of %s after Clear and a reuse during the read = %q, %v; want unavailable", q.ID, first, got, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if b, ok := n.bodies[q.ID]; ok {
		t.Fatalf("the cache holds %q under %q, now the id of %s", b.data, q.ID, second)
	}
}

// A reply that arrives after another reader cached the request's body and
// WebKit reused the id neither returns nor replaces the cached body. The
// reviewer's second reproduction.
func TestBodyKeepsTheCachedOriginalWhenAReusedReplyArrivesLate(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	k := reqKey{"t", "1"}
	first, second := "https://fixture.example/first", "https://fixture.example/second"
	netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, first, 1))
	netEvent(n, k.target, "Network.loadingFinished", map[string]any{"requestId": k.id, "timestamp": 2})
	wk.latest[k] = first
	q, _ := recordOf(n, first)
	h := readHook{wk, func() {
		if data, _, err := n.Body(context.Background(), wk, q.ID); err != nil || string(data) != "body of "+first {
			t.Errorf("setup: the other reader got %q, %v", data, err)
		}
		netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, second, 3))
		wk.latest[k] = second
	}}
	got, _, err := n.Body(context.Background(), h, q.ID)
	if err == nil && string(got) != "body of "+first || err != nil && !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("late Body(%q) = %q, %v; want the cached %s body or unavailable", q.ID, got, err, first)
	}
	if cached, _, err := n.Body(context.Background(), wk, q.ID); err != nil || string(cached) != "body of "+first {
		t.Fatalf("Body(%q) after the late reply = %q, %v; want the cached original", q.ID, cached, err)
	}
}

// finishedLife logs a finished request to url with WebKit id k on n, whose
// body wk then holds.
func finishedLife(n *Netlog, wk *lifeWebKit, k reqKey, url string, ts float64) Request {
	netEvent(n, k.target, "Network.requestWillBeSent", reqEvent(k.id, url, ts))
	netEvent(n, k.target, "Network.loadingFinished", map[string]any{"requestId": k.id, "timestamp": ts + 1})
	wk.latest[k] = url
	q, _ := recordOf(n, url)
	return q
}

// Two reads of one body at once: the later reply does not cache the body a
// second time. Before round 9 it did, which left the id twice in bodyOrder
// and bodyBytes counting both, and the next trim of the cache looped
// forever with n.mu held (found by the round-9 property).
func TestConcurrentBodyReadsCacheTheBodyOnce(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	q := finishedLife(n, wk, reqKey{"t", "1"}, "https://fixture.example/first", 1)
	h := readHook{wk, func() {
		if data, _, err := n.Body(context.Background(), wk, q.ID); err != nil || string(data) != "body of "+q.URL {
			t.Errorf("setup: the other reader got %q, %v", data, err)
		}
	}}
	if data, _, err := n.Body(context.Background(), h, q.ID); err != nil || string(data) != "body of "+q.URL {
		t.Fatalf("Body(%q) = %q, %v; want its body", q.ID, data, err)
	}
	n.mu.Lock()
	ok := cacheBooksAgree(t, n)
	n.mu.Unlock()
	if !ok {
		t.Fatal("the body cache's books disagree after two reads of one body")
	}
}

// A reply that crosses a network reset is discarded, also when WebKit sent
// it before the reset: Body says the reset cleared the body.
func TestBodyDiscardsAReplyThatCrossesANetworkReset(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	q := finishedLife(n, wk, reqKey{"t", "1"}, "https://fixture.example/first", 1)
	h := readHook{wk, func() {
		now := time.Now()
		n.onGap(core.NetworkGap{Start: now, End: now})
	}}
	if got, _, err := n.Body(context.Background(), h, q.ID); err == nil || !strings.Contains(err.Error(), "network reset") {
		t.Fatalf("Body(%q) across a network reset = %q, %v; want the reset", q.ID, got, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.bodies[q.ID]; ok {
		t.Fatalf("the cache holds a body of %q read across a network reset", q.ID)
	}
}

// A reply that arrives after Clear dropped the request is discarded: the
// log no longer holds the request, so nothing ties the reply to its id.
func TestBodyDiscardsAReplyAfterClearDuringTheRead(t *testing.T) {
	n := newNetlog(nil, "s1")
	wk := &lifeWebKit{latest: map[reqKey]string{}}
	q := finishedLife(n, wk, reqKey{"t", "1"}, "https://fixture.example/first", 1)
	h := readHook{wk, n.Clear}
	if got, _, err := n.Body(context.Background(), h, q.ID); err == nil || !strings.Contains(err.Error(), "dropped the request") {
		t.Fatalf("Body(%q) across Clear = %q, %v; want unavailable: the log dropped the request", q.ID, got, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.bodies) != 0 {
		t.Fatalf("the cache holds %d bodies after Clear", len(n.bodies))
	}
}

// The history bound can drop a request's record while the request runs;
// its later events keep the id of its first event.
func TestCDPIDOfARequestSurvivesHistoryEviction(t *testing.T) {
	n := newNetlog(nil, "s1")
	n.SetLimits(1, DefaultMaxBodyBytes)
	var ids []string
	n.SubscribeCDP(func(_, _ string, p json.RawMessage) {
		var e struct {
			ID string `json:"requestId"`
		}
		_ = json.Unmarshal(p, &e)
		ids = append(ids, e.ID)
	})
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("1.1", "https://fixture.example/first", 1))
	netEvent(n, "t", "Network.loadingFinished", map[string]any{"requestId": "1.1", "timestamp": 2})
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("1.1", "https://fixture.example/reuse", 3))
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("1.2", "https://fixture.example/other", 4))
	netEvent(n, "t", "Network.responseReceived", map[string]any{
		"requestId": "1.1", "timestamp": 5, "type": "Fetch",
		"response": map[string]any{"url": "https://fixture.example/reuse", "status": 200, "headers": map[string]string{}},
	})
	netEvent(n, "t", "Network.loadingFinished", map[string]any{"requestId": "1.1", "timestamp": 6})
	if len(ids) != 6 {
		t.Fatalf("setup: CDP ids %v", ids)
	}
	if ids[0] == ids[2] || ids[2] != ids[4] || ids[2] != ids[5] {
		t.Fatalf("CDP ids %v: the reused request (events 3, 5, 6) must keep one id, other than the first request's", ids)
	}
	if s := n.Snapshot(Filter{}); len(s.Requests) != 1 || s.Requests[0].URL != "https://fixture.example/other" {
		t.Fatalf("the response of an evicted request made a new record: %+v", s.Requests)
	}
}

// A WebKit id is an opaque string: a redirect hop's id never equals the
// id of a request whose WebKit id looks like a hop id.
func TestRedirectIDsDoNotOverwriteOtherRequests(t *testing.T) {
	n := newNetlog(nil, "s1")
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("x:redirect1", "https://fixture.example/separate", 1))
	netEvent(n, "t", "Network.requestWillBeSent", reqEvent("x", "https://fixture.example/original", 2))
	hop := reqEvent("x", "https://fixture.example/redirected", 3)
	hop["redirectResponse"] = map[string]any{"url": "https://fixture.example/original", "status": 302, "headers": map[string]string{}}
	netEvent(n, "t", "Network.requestWillBeSent", hop)
	reqs := n.Snapshot(Filter{}).Requests
	seen := map[string]bool{}
	for _, q := range reqs {
		if seen[q.ID] {
			t.Fatalf("two requests have the id %q: %+v", q.ID, reqs)
		}
		seen[q.ID] = true
		if got, ok := n.Get(q.ID); !ok || got.URL != q.URL {
			t.Fatalf("Get(%q) = %s (%v), want %s", q.ID, got.URL, ok, q.URL)
		}
	}
	if len(reqs) != 3 {
		t.Fatalf("want 3 requests, got %+v", reqs)
	}
}

// A log id is "<target>:<WebKit id>" with '%', ':' and '#' in either part
// escaped as %25, %3A and %23 (Request; docs/notes/w4.md, Netlog), so a
// WebKit id never reads as a hop id or a "#<seq>" suffix.
func TestNetlogIDsEscapeTheSeparators(t *testing.T) {
	n := newNetlog(nil, "s1")
	for i, c := range []struct{ target, id, want string }{
		{"t:1", "1:redirect1", "t%3A1:1%3Aredirect1"},
		{"t", "50%", "t:50%25"},
		{"t", "1#2", "t:1%232"},
		{"t", "x%3Aredirect1", "t:x%253Aredirect1"},
		{"", "a:b", "a%3Ab"},
		{"t", "x", "t:x"},
	} {
		url := fmt.Sprintf("https://fixture.example/%d", i)
		netEvent(n, c.target, "Network.requestWillBeSent", reqEvent(c.id, url, float64(i)))
		if q, _ := recordOf(n, url); q.ID != c.want {
			t.Errorf("id of (%q, %q) = %q, want %q", c.target, c.id, q.ID, c.want)
		}
	}
	hop := reqEvent("x", "https://fixture.example/hop", 9)
	hop["redirectResponse"] = map[string]any{"url": "https://fixture.example/5", "status": 302, "headers": map[string]string{}}
	netEvent(n, "t", "Network.requestWillBeSent", hop)
	for url, want := range map[string]string{"https://fixture.example/5": "t:x:redirect1", "https://fixture.example/hop": "t:x"} {
		if q, _ := recordOf(n, url); q.ID != want {
			t.Errorf("id of %s after the redirect = %q, want %q", url, q.ID, want)
		}
	}
}

// lifeKeys are the (target, WebKit id) pairs of TestNetlogIDLifecycleProperty.
// Without the escaping, pairs of them would give equal ids: the hop id
// "1:redirect1" of ("", "1") is the id of ("1", "redirect1"); the hop id
// of ("t:1", "1") reads "t:1:1:redirect1", as ("t:1", "1:redirect1") does;
// ("t", "1#2") reads like the second chain of ("t", "1"); and
// ("t", "1%3Aredirect1") reads like ("t", "1:redirect1") escaped.
var lifeKeys = []reqKey{
	{"", "1"},
	{"1", "redirect1"},
	{"t:1", "1"},
	{"t:1", "1:redirect1"},
	{"t", "1"},
	{"t", "1#2"},
	{"t", "1:redirect1"},
	{"t", "1%3Aredirect1"},
}

// lifeWebKit is the backend of TestNetlogIDLifecycleProperty. As WebKit, it
// keeps the body of the latest request with a WebKit id per target only,
// and a network reset drops every body. A request's body is "body of <url>".
type lifeWebKit struct {
	latest map[reqKey]string // the URL of the latest request of each id
	calls  int               // commands received
}

func (w *lifeWebKit) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	w.calls++
	if method != "Network.getResponseBody" {
		return nil, fmt.Errorf("%s: No resource with given URL found", method)
	}
	target, _ := webkit.TargetOf(ctx)
	if url, ok := w.latest[reqKey{target, params.(map[string]string)["requestId"]}]; ok {
		return json.Marshal(map[string]any{"body": "body of " + url, "base64Encoded": false})
	}
	return nil, errors.New("No resource with given identifier found")
}
func (*lifeWebKit) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (*lifeWebKit) IsTargetRouted() bool                         { return true }
func (*lifeWebKit) Done() <-chan struct{}                        { return nil }

// lifeChain is a running request chain of TestNetlogIDLifecycleProperty.
type lifeChain struct {
	id   string // its log id, from its first CDP event
	hops int    // its redirects so far
	cur  string // the URL of its current hop
}

// lifeRun is one run of TestNetlogIDLifecycleProperty: Keys picks three
// pairs of lifeKeys, so that the steps of a run often meet on one id.
type lifeRun struct {
	Keys [3]uint8
	Ops  []lifeOp
}

// lifeOp is one step of a lifeRun: Kind picks the step, Key one of the
// run's pairs, and Arg the bounds of an eviction or the request whose body
// is read.
type lifeOp struct{ Kind, Key, Arg uint8 }

// hasSeqSuffix reports whether id is base with a "#<seq>" suffix.
func hasSeqSuffix(id, base string) bool {
	seq, ok := strings.CutPrefix(id, base+"#")
	return ok && seq != "" && strings.Trim(seq, "0123456789") == ""
}

// Property (the netlog id lifecycle, W4 rounds 8 and 9): whatever the order
// of requests, redirect chains, WebKit id reuse, evictions at random points
// (of the history and of the body cache), Clear, network resets and body
// reads, also body reads whose reply waits while later steps run (round 9:
// the backend answers with what it holds when it replies), with ids that
// contain ':', '%', '#' and "redirect":
//   - the ids of the log's requests and of its running chains are unique;
//   - a chain's id is fixed from its first event to its end, in every CDP
//     event and in its current hop's record, and it names its target and
//     WebKit id (with a "#<seq>" suffix when it is taken);
//   - a redirected hop's id changes once, at its redirect, to the chain's
//     id with ":redirect<n>", and no record's id changes otherwise;
//   - Body(id) returns the request's own body or an error, never another
//     request's body, and a read without a wait returns the body while
//     WebKit has it and does not ask WebKit when it has not;
//   - the body cache never holds another request's body (lifeInvariants).
func TestNetlogIDLifecycleProperty(t *testing.T) {
	check := func(run lifeRun) bool {
		n := newNetlog(nil, "s1")
		n.SetLimits(4, DefaultMaxBodyBytes)
		wk := &lifeWebKit{latest: map[reqKey]string{}}
		var cdp []string
		n.SubscribeCDP(func(_, _ string, p json.RawMessage) {
			var e struct {
				ID string `json:"requestId"`
			}
			_ = json.Unmarshal(p, &e)
			cdp = append(cdp, e.ID)
		})
		live := map[reqKey]*lifeChain{}
		seen := map[reqKey]bool{}
		want := map[string]string{} // the id of each request, by its URL
		next := 0                   // the index of the next step in run.Ops
		var step func() bool
		step = func() bool {
			i, op := next, run.Ops[next]
			next++
			fail := func(format string, args ...any) bool {
				t.Logf("step %d (%+v): %s", i, op, fmt.Sprintf(format, args...))
				return false
			}
			k := lifeKeys[int(run.Keys[op.Key%3])%len(lifeKeys)]
			url := fmt.Sprintf("https://fixture.example/%d", i)
			c := live[k]
			cdp = nil
			// sameChain checks the CDP event of a step of c.
			sameChain := func() bool {
				return len(cdp) == 1 && cdp[0] == c.id
			}
			// newChain records the chain that the step started with id.
			newChain := func(id string) bool {
				if base := logID(k.target, k.id); id != base && !hasSeqSuffix(id, base) {
					return fail("chain id %q does not name %q and %q (%q)", id, k.target, k.id, base)
				}
				want[url], seen[k], wk.latest[k] = id, true, url
				return true
			}
			switch op.Kind % 11 {
			case 0, 1: // a request; kind 1 is a redirect, also of a request the log did not see start
				e := reqEvent(k.id, url, float64(i))
				if op.Kind%11 == 1 {
					from := url + "/from"
					if c != nil {
						from = c.cur
					}
					e["redirectResponse"] = map[string]any{"url": from, "status": 302, "headers": map[string]string{}}
				} else if c != nil {
					return true // WebKit does not reuse the id of a running request
				}
				netEvent(n, k.target, "Network.requestWillBeSent", e)
				if c == nil {
					if len(cdp) != 1 {
						return fail("CDP events %q", cdp)
					}
					live[k] = &lifeChain{id: cdp[0], cur: url}
					if !newChain(cdp[0]) {
						return false
					}
					break
				}
				if !sameChain() {
					return fail("redirect of chain %q has CDP ids %q", c.id, cdp)
				}
				c.hops++
				hop := fmt.Sprintf("%s:redirect%d", c.id, c.hops)
				if q, ok := recordOf(n, c.cur); ok {
					if q.ID != hop && !hasSeqSuffix(q.ID, hop) {
						return fail("redirected hop %s has id %q, want %q", c.cur, q.ID, hop)
					}
					want[c.cur] = q.ID
				}
				c.cur, want[url], wk.latest[k] = url, c.id, url
			case 2: // a memory-cache hit
				if c != nil {
					return true
				}
				netEvent(n, k.target, "Network.requestServedFromMemoryCache", map[string]any{
					"requestId": k.id, "frameId": "f1", "loaderId": "l1", "timestamp": float64(i),
					"resource": map[string]any{"url": url, "type": "Image", "bodySize": 1},
				})
				q, ok := recordOf(n, url)
				if !ok {
					return fail("no record of the memory-cache hit %s", url)
				}
				if !newChain(q.ID) {
					return false
				}
			case 3: // a response; without a request event only on an id the log never saw
				resp := url
				if c != nil {
					resp = c.cur
				} else if seen[k] {
					return true
				}
				netEvent(n, k.target, "Network.responseReceived", map[string]any{
					"requestId": k.id, "frameId": "f1", "loaderId": "l1", "timestamp": float64(i), "type": "Document",
					"response": map[string]any{"url": resp, "status": 200, "headers": map[string]string{}},
				})
				if c != nil {
					if !sameChain() {
						return fail("response of chain %q has CDP ids %q", c.id, cdp)
					}
					break
				}
				if len(cdp) != 1 {
					return fail("CDP events %q", cdp)
				}
				live[k] = &lifeChain{id: cdp[0], cur: url}
				if !newChain(cdp[0]) {
					return false
				}
			case 4, 5, 6: // data, the end of a chain, or its failure
				if c == nil {
					return true
				}
				e := map[string]any{"requestId": k.id, "timestamp": float64(i), "dataLength": 3, "encodedDataLength": 3, "errorText": "boom"}
				method := []string{"Network.dataReceived", "Network.loadingFinished", "Network.loadingFailed"}[op.Kind%11-4]
				netEvent(n, k.target, method, e)
				if !sameChain() {
					return fail("%s of chain %q has CDP ids %q", method, c.id, cdp)
				}
				if op.Kind%11 != 4 {
					delete(live, k)
				}
			case 7:
				n.Clear()
			case 8: // a network reset drops WebKit's bodies and ends every chain
				now := time.Now()
				n.onGap(core.NetworkGap{Start: now, End: now})
				clear(live)
				clear(wk.latest)
			case 9: // eviction: of the history, and of cached bodies
				n.SetLimits(1+int(op.Arg)%5, []int64{DefaultMaxBodyBytes, 40, 0}[int(op.Arg)/5%3])
			case 10: // a body read; with Arg's high bit, its reply waits while the next steps run
				reqs := n.Snapshot(Filter{}).Requests
				if len(reqs) == 0 {
					return true
				}
				q := reqs[int(op.Arg)%len(reqs)]
				var c webkit.Caller = wk
				delayed, ok := op.Arg&0x80 != 0, true
				if delayed {
					m, started := 1+int(op.Arg)%3, false
					c = readHook{wk, func() {
						if started {
							return
						}
						started = true
						for j := 0; j < m && ok && next < len(run.Ops); j++ {
							ok = step()
						}
					}}
				}
				calls := wk.calls
				has := wk.latest[reqKey{q.Target, q.RequestID}] == q.URL
				data, _, err := n.Body(context.Background(), c, q.ID)
				switch {
				case !ok:
					return false
				case err == nil && string(data) != "body of "+q.URL:
					return fail("Body(%q) of %s = %q, another request's body", q.ID, q.URL, data)
				case delayed:
				case err != nil && q.State == StateFinished && has:
					return fail("Body(%q) = %v, while WebKit has its body", q.ID, err)
				case wk.calls != calls && !has:
					return fail("Body(%q) asked WebKit, which no longer has the body of %s", q.ID, q.URL)
				}
			}
			if !lifeInvariants(t, n, live, want) {
				return fail("invariant broken (see above)")
			}
			return true
		}
		for next < len(run.Ops) {
			if !step() {
				return false
			}
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 4000}); err != nil {
		t.Fatal(err)
	}
}

// recordOf returns the log's record of the request to url.
func recordOf(n *Netlog, url string) (Request, bool) {
	for _, q := range n.Snapshot(Filter{}).Requests {
		if q.URL == url {
			return q, true
		}
	}
	return Request{}, false
}

// lifeInvariants checks the log after a step of TestNetlogIDLifecycleProperty:
// one record per request, each with the id it got (want); Get finds each
// by its id; no running chain shares an id with a request other than
// its current hop or with another running chain; and the body cache holds
// only the bodies of the log's requests, each under its own id.
func lifeInvariants(t *testing.T, n *Netlog, live map[reqKey]*lifeChain, want map[string]string) bool {
	t.Helper()
	owner := map[string]string{} // id -> URL of its record or chain
	for _, q := range n.Snapshot(Filter{}).Requests {
		w, ok := want[q.URL]
		switch {
		case !ok:
			t.Logf("record %q of %s does not belong to a request of the test", q.ID, q.URL)
		case q.ID != w:
			t.Logf("the id of %s changed from %q to %q", q.URL, w, q.ID)
		case owner[q.ID] != "":
			t.Logf("id %q names %s and %s", q.ID, owner[q.ID], q.URL)
		default:
			owner[q.ID] = q.URL
			if g, ok := n.Get(q.ID); !ok || g.URL != q.URL {
				t.Logf("Get(%q) = %s (%v), want %s", q.ID, g.URL, ok, q.URL)
				return false
			}
			continue
		}
		return false
	}
	for _, c := range live {
		if u := owner[c.id]; u != "" && u != c.cur {
			t.Logf("running chain %q (at %s) shares its id with %s", c.id, c.cur, u)
			return false
		}
		owner[c.id] = c.cur
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, b := range n.bodies {
		if r := n.ids[id]; r == nil || string(b.data) != "body of "+r.URL {
			t.Logf("the cache holds %q under %q, which names %+v", b.data, id, r)
			return false
		}
	}
	return cacheBooksAgree(t, n)
}

// cacheBooksAgree reports whether the body cache's order and byte count
// match its bodies: each cached id once in bodyOrder, nothing else, and
// bodyBytes their sum (trimLocked loops on bodyOrder until bodyBytes fits).
// Call it with n.mu held.
func cacheBooksAgree(t *testing.T, n *Netlog) bool {
	t.Helper()
	var size int64
	for _, b := range n.bodies {
		size += int64(len(b.data))
	}
	seen := map[string]bool{}
	for _, id := range n.bodyOrder {
		if _, ok := n.bodies[id]; !ok || seen[id] {
			t.Logf("bodyOrder %q does not match the cached ids", n.bodyOrder)
			return false
		}
		seen[id] = true
	}
	if len(seen) != len(n.bodies) || size != n.bodyBytes {
		t.Logf("cache: %d bodies of %d bytes, bodyOrder %q, bodyBytes %d", len(n.bodies), size, n.bodyOrder, n.bodyBytes)
		return false
	}
	return true
}
