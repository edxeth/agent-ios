package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 round 7, review finding netlog.go:542 (P1): WebKit's request ids are
// unique per page target only, so the netlog gives each request an id that
// names its target (Request.ID), keeps WebKit's id (Request.RequestID) and
// the target, and reads a body from the request's own target. These tests
// are the reviewer's reproductions
// (/home/devkit/.pi/artifacts/reviewer/w4-r56-evidence/netlog_repro_test.go)
// on the public id.

// startNetlogOf starts the network log of session s1 on r's page.
func startNetlogOf(t *testing.T, r *rig) *Netlog {
	t.Helper()
	var n *Netlog
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		n, _, err = p.StartNetlog(ctx, c, "s1")
		return err
	})
	return n
}

// requestTo returns the log's request for url once it is in state.
func requestTo(t *testing.T, n *Netlog, url, state string) Request {
	t.Helper()
	var q Request
	eventually(t, url+" "+state, func() bool {
		for _, x := range n.Snapshot(Filter{}).Requests {
			if x.URL == url && x.State == state {
				q = x
				return true
			}
		}
		return false
	})
	return q
}

// bodyOf reads the body of request id inside an operation.
func bodyOf(t *testing.T, r *rig, n *Netlog, id string) (string, error) {
	t.Helper()
	var body []byte
	var berr error
	r.must(t, func(ctx context.Context, _ *Page, c webkit.Caller) error {
		body, _, berr = n.Body(ctx, c, id)
		return nil
	})
	return string(body), berr
}

// bodyOfTarget answers Network.getResponseBody with "<target>-body".
func bodyOfTarget(c fakewebkit.Call) (any, *fakewebkit.Error) {
	return map[string]any{"body": c.Target + "-body", "base64Encoded": false}, nil
}

// bodyCalls lists the getResponseBody calls as "<target> <requestId>".
func bodyCalls(f *fakewebkit.Server, method string) []string {
	var out []string
	for _, rec := range received(f, method) {
		out = append(out, rec.Target+" "+string(rec.Params))
	}
	return out
}

func finishedOn(f *fakewebkit.Server, target, id, url string) {
	f.EmitFrom(target, "Network.requestWillBeSent", reqEvent(id, url, 1))
	f.EmitFrom(target, "Network.loadingFinished", map[string]any{"requestId": id, "timestamp": 2})
}

// A body is read from the target that loaded the request, with WebKit's
// request id, also while that target is provisional.
func TestNetlogReadsABodyFromTheRequestsTarget(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	<-r.f.Provisional("prov")
	r.f.Handle("Network.getResponseBody", bodyOfTarget)
	finishedOn(r.f, "prov", "prov.unique", "https://fixture.example/body")
	q := requestTo(t, n, "https://fixture.example/body", StateFinished)
	body, err := bodyOf(t, r, n, q.ID)
	calls := bodyCalls(r.f, "Network.getResponseBody")
	if err != nil || body != "prov-body" || len(calls) != 1 || calls[0] != `prov {"requestId":"prov.unique"}` {
		t.Fatalf("body of %s, recorded on target prov = %q, %v; getResponseBody calls %q; want prov-body from one call to prov with WebKit's id", q.ID, body, err, calls)
	}
}

// Two targets' requests with the same WebKit id have different ids in the
// log; Get and Body of one id refer to the same request, and a cached
// body of one target is never the body of the other.
func TestNetlogKeepsSameIDRequestsOfTwoTargetsApart(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	<-r.f.Provisional("prov")
	old := r.f.ActiveTarget()
	r.f.Handle("Network.getResponseBody", bodyOfTarget)
	finishedOn(r.f, old, "same", "https://fixture.example/old")
	r.f.EmitFrom("prov", "Network.requestWillBeSent", reqEvent("same", "https://fixture.example/new", 3))
	oldReq := requestTo(t, n, "https://fixture.example/old", StateFinished)
	newReq := requestTo(t, n, "https://fixture.example/new", StatePending)
	if oldReq.ID == newReq.ID {
		t.Fatalf("the requests of %s and prov with WebKit id \"same\" share the log id %q", old, oldReq.ID)
	}
	for _, want := range []Request{oldReq, newReq} {
		if got, ok := n.Get(want.ID); !ok || got.URL != want.URL || got.State != want.State {
			t.Fatalf("Get(%q) = %s %s (found %v), want %s %s", want.ID, got.URL, got.State, ok, want.URL, want.State)
		}
	}
	if body, err := bodyOf(t, r, n, oldReq.ID); err != nil || body != old+"-body" {
		t.Fatalf("body of the finished request %s = %q, %v; want %s-body", oldReq.ID, body, err, old)
	}
	if _, err := bodyOf(t, r, n, newReq.ID); err == nil || !strings.Contains(err.Error(), "still in flight") {
		t.Fatalf("body of the pending request %s: %v; want 'still in flight'", newReq.ID, err)
	}
	r.f.EmitFrom("prov", "Network.loadingFinished", map[string]any{"requestId": "same", "timestamp": 4})
	requestTo(t, n, "https://fixture.example/new", StateFinished)
	if body, err := bodyOf(t, r, n, newReq.ID); err != nil || body != "prov-body" {
		t.Fatalf("body of %s after the old target's body was cached = %q, %v; want prov-body", newReq.ID, body, err)
	}
	want := []string{old + ` {"requestId":"same"}`, `prov {"requestId":"same"}`}
	if got := bodyCalls(r.f, "Network.getResponseBody"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("getResponseBody calls = %q, want %q", got, want)
	}
}

// A HAR with content "all" carries each request's own body and its log id
// (_requestId), for two targets' requests with the same WebKit id.
func TestHARKeepsTheBodiesOfEachTarget(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	<-r.f.Provisional("prov")
	old := r.f.ActiveTarget()
	if err := n.StartHAR("all"); err != nil {
		t.Fatal(err)
	}
	r.f.Handle("Network.getResponseBody", bodyOfTarget)
	finishedOn(r.f, old, "same", "https://fixture.example/"+old)
	finishedOn(r.f, "prov", "same", "https://fixture.example/prov")
	ids := []string{
		requestTo(t, n, "https://fixture.example/"+old, StateFinished).ID,
		requestTo(t, n, "https://fixture.example/prov", StateFinished).ID,
	}
	var h HAR
	r.must(t, func(ctx context.Context, _ *Page, c webkit.Caller) error {
		var err error
		h, err = n.StopHAR(ctx, c, HAROptions{ShowValues: true})
		return err
	})
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var entries struct {
		Log struct {
			Entries []struct {
				RequestID string `json:"_requestId"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{old + "-body", "prov-body"} {
		e := h.Log.Entries[i]
		if e.Response.Content.Text != want || entries.Log.Entries[i].RequestID != ids[i] {
			t.Fatalf("HAR entry %s: body %q, _requestId %q; want %q and %q; getResponseBody calls %q",
				e.Request.URL, e.Response.Content.Text, entries.Log.Entries[i].RequestID, want, ids[i], bodyCalls(r.f, "Network.getResponseBody"))
		}
	}
}

// When getResponseBody fails, Page.getResourceContent of the request's
// target may supply the body; a reply without content is no body, and the
// error names WebKit's error.
func TestNetlogBodyFallbackNeedsContent(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	<-r.f.Provisional("prov")
	r.f.Handle("Network.getResponseBody", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "No resource with given identifier found"}
	})
	r.f.Handle("Page.getResourceContent", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ URL string }
		_ = json.Unmarshal(c.Params, &p)
		if strings.HasSuffix(p.URL, "/kept") {
			return map[string]any{"content": c.Target + "-resource", "base64Encoded": false}, nil
		}
		return map[string]any{"content": "", "base64Encoded": false}, nil
	})
	finishedOn(r.f, "prov", "1.1", "https://fixture.example/kept")
	finishedOn(r.f, "prov", "1.2", "https://fixture.example/empty")
	kept := requestTo(t, n, "https://fixture.example/kept", StateFinished)
	empty := requestTo(t, n, "https://fixture.example/empty", StateFinished)
	if body, err := bodyOf(t, r, n, kept.ID); err != nil || body != "prov-resource" {
		t.Fatalf("body of %s from Page.getResourceContent = %q, %v; want prov-resource", kept.ID, body, err)
	}
	body, err := bodyOf(t, r, n, empty.ID)
	if err == nil || !strings.Contains(err.Error(), "No resource with given identifier found") {
		t.Fatalf("body of %s, which WebKit does not have = %q, %v; want an error with WebKit's text", empty.ID, body, err)
	}
	for _, rec := range append(received(r.f, "Network.getResponseBody"), received(r.f, "Page.getResourceContent")...) {
		if rec.Target != "prov" {
			t.Fatalf("%s %s went to %s, want prov, the requests' target", rec.Method, rec.Params, rec.Target)
		}
	}
}

// The body of a request of a target that a commit replaced is not read
// from the committed target: the request's target is gone, so its body is
// reported as unavailable.
func TestNetlogBodyOfAReplacedTargetIsUnavailable(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	old := r.f.ActiveTarget()
	r.f.Handle("Network.getResponseBody", bodyOfTarget)
	finishedOn(r.f, old, "1.1", "https://fixture.example/old")
	q := requestTo(t, n, "https://fixture.example/old", StateFinished)
	<-r.f.Provisional("prov")
	r.f.Commit("prov")
	eventually(t, "the page connection commits prov", func() bool { return activeTargetOf(r.page(t).ps.Caller) == "prov" })
	body, err := bodyOf(t, r, n, q.ID)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("body of %s after its target %s was replaced = %q, %v; want it reported as unavailable", q.ID, old, body, err)
	}
	if calls := bodyCalls(r.f, "Network.getResponseBody"); len(calls) != 0 {
		t.Fatalf("getResponseBody calls %q; want none (the request's target is gone)", calls)
	}
}

// A target that reuses a WebKit id for a new request (not a redirect)
// gets a new log id: every id of the log names one request.
func TestNetlogIDsStayUniqueWhenATargetReusesAnID(t *testing.T) {
	r := newRig(t)
	n := startNetlogOf(t, r)
	target := r.f.ActiveTarget()
	finishedOn(r.f, target, "1.1", "https://fixture.example/first")
	finishedOn(r.f, target, "1.1", "https://fixture.example/second")
	first := requestTo(t, n, "https://fixture.example/first", StateFinished)
	second := requestTo(t, n, "https://fixture.example/second", StateFinished)
	if first.ID == second.ID {
		t.Fatalf("two requests share the log id %q", first.ID)
	}
	for _, want := range []Request{first, second} {
		if got, _ := n.Get(want.ID); got.URL != want.URL {
			t.Fatalf("Get(%q) = %s, want %s", want.ID, got.URL, want.URL)
		}
	}
}

// The CDP events of SubscribeCDP carry their source target and the log id
// of their request chain: two targets' requests with the same WebKit id
// get different ids, every event of one chain (also a redirect hop) gets
// the same id, and that id is the log's id of the chain's last hop.
func TestCDPEventsCarryTheTargetAndTheLogID(t *testing.T) {
	r := newPausingRig(t)
	n := startNetlogOf(t, r)
	<-r.f.Provisional("prov")
	old := r.f.ActiveTarget()
	var mu sync.Mutex
	var got []string
	unsub := n.SubscribeCDP(func(target, method string, p json.RawMessage) {
		var e struct {
			ID string `json:"requestId"`
		}
		if err := json.Unmarshal(p, &e); err != nil {
			t.Errorf("CDP %s params %s: %v", method, p, err)
		}
		mu.Lock()
		got = append(got, target+" "+method+" "+e.ID)
		mu.Unlock()
	})
	defer unsub()
	hop := reqEvent("same", "https://fixture.example/old-final", 2)
	hop["redirectResponse"] = map[string]any{"url": "https://fixture.example/old", "status": 302, "statusText": "Found", "headers": map[string]string{}, "mimeType": "text/plain"}
	r.f.EmitFrom(old, "Network.requestWillBeSent", reqEvent("same", "https://fixture.example/old", 1))
	r.f.EmitFrom(old, "Network.requestWillBeSent", hop)
	r.f.EmitFrom("prov", "Network.requestWillBeSent", reqEvent("same", "https://fixture.example/new", 3))
	r.f.EmitFrom(old, "Network.loadingFinished", map[string]any{"requestId": "same", "timestamp": 4})
	r.f.EmitFrom("prov", "Network.loadingFinished", map[string]any{"requestId": "same", "timestamp": 5})
	eventually(t, "five CDP events", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 5 })
	oldID := requestTo(t, n, "https://fixture.example/old-final", StateFinished).ID
	newID := requestTo(t, n, "https://fixture.example/new", StateFinished).ID
	want := []string{
		old + " Network.requestWillBeSent " + oldID,
		old + " Network.requestWillBeSent " + oldID,
		"prov Network.requestWillBeSent " + newID,
		old + " Network.loadingFinished " + oldID,
		"prov Network.loadingFinished " + newID,
	}
	mu.Lock()
	defer mu.Unlock()
	if oldID == newID || !slices.Equal(got, want) {
		t.Fatalf("CDP events (target method requestId) = %q, want %q with distinct ids", got, want)
	}
}

// netOp is one step of TestNetlogIDsProperty: a Network event of one of
// three targets about one of three WebKit ids, or a log operation.
type netOp struct{ Target, ID, Kind uint8 }

// Property: whatever the order of events, redirects, id reuse, trims and
// Clear, every request of the log has its own id, Get(id) returns that
// request, the current hop of each (target, WebKit id) records both, and
// each CDP event carries the id of its chain's current hop (the same id
// for every hop of a redirect chain).
func TestNetlogIDsProperty(t *testing.T) {
	methods := []string{
		"Network.requestWillBeSent", "Network.requestWillBeSent", "Network.requestServedFromMemoryCache",
		"Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed",
	}
	check := func(ops []netOp) bool {
		n := newNetlog(nil, "s1")
		n.SetLimits(4, DefaultMaxBodyBytes)
		var cdpID string
		n.SubscribeCDP(func(_, _ string, p json.RawMessage) {
			var e struct {
				ID string `json:"requestId"`
			}
			_ = json.Unmarshal(p, &e)
			cdpID = e.ID
		})
		for i, op := range ops {
			kind := int(op.Kind) % (len(methods) + 1)
			if kind == len(methods) {
				n.Clear()
				continue
			}
			target, id := []string{"", "t1", "t2"}[op.Target%3], fmt.Sprintf("0.%d", op.ID%3)
			e := reqEvent(id, fmt.Sprintf("https://fixture.example/%d", i), float64(i))
			switch kind {
			case 1:
				e["redirectResponse"] = map[string]any{"url": "https://fixture.example/r", "status": 302, "headers": map[string]string{}}
			case 2:
				e["resource"] = map[string]any{"url": "https://fixture.example/c", "type": "Image", "bodySize": 1}
			case 3:
				e["response"] = map[string]any{"url": "https://fixture.example/doc", "status": 200, "headers": map[string]string{}}
			}
			raw, _ := json.Marshal(e)
			n.mu.Lock()
			prev := slotCur(n, reqKey{target, id})
			n.mu.Unlock()
			cdpID = ""
			n.event(target, methods[kind], raw)
			n.mu.Lock()
			cur := slotCur(n, reqKey{target, id})
			n.mu.Unlock()
			if kind == 1 && prev != nil && cur != nil && cur.ID != logID(target, id) && !strings.HasPrefix(cur.ID, logID(target, id)+"#") {
				t.Logf("step %d: chain id %q does not name %s and %s", i, cur.ID, target, id)
				return false
			}
			if kind != 2 && cur != nil && cdpID != cur.ID {
				t.Logf("step %d: CDP id %q, current hop %q", i, cdpID, cur.ID)
				return false
			}
			if kind == 1 && prev != nil && cur != nil && cur != prev && n.ids[prev.ID] == prev && prev.State == StateRedirected && !strings.HasPrefix(prev.ID, cur.ID+":redirect") {
				t.Logf("step %d: redirect hop %q is not of chain %q", i, prev.ID, cur.ID)
				return false
			}
			s := n.Snapshot(Filter{})
			seen := map[string]bool{}
			for _, q := range s.Requests {
				if seen[q.ID] {
					t.Logf("step %d: id %q twice in %+v", i, q.ID, s.Requests)
					return false
				}
				seen[q.ID] = true
				if g, ok := n.Get(q.ID); !ok || g.Seq != q.Seq {
					t.Logf("step %d: Get(%q) = seq %d (%v), want seq %d", i, q.ID, g.Seq, ok, q.Seq)
					return false
				}
			}
			n.mu.Lock()
			stale := len(n.ids) != len(n.reqs)
			for k, sl := range n.slots {
				r := sl.cur
				stale = stale || r != nil && (r.Target != k.target || r.RequestID != k.id || r.ID != sl.id)
			}
			n.mu.Unlock()
			if stale {
				t.Logf("step %d: the id index does not match the log", i)
				return false
			}
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// slotCur is the current hop of the slot of k, or nil (call with n.mu held).
func slotCur(n *Netlog, k reqKey) *Request {
	if s := n.slots[k]; s != nil {
		return s.cur
	}
	return nil
}
