package devtools

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W4 round 6 (brief W4-R6): every interception answer goes to the target
// that holds the request. WebKit names the sender of each event in
// Target.dispatchMessageFromTarget, and a provisional target keeps its id
// when it commits (docs/notes/w1b.md, "W1c"). A completion sent to another
// target gets "Missing pending intercept ..." and leaves the request
// paused, so the new page can hang.

// interceptOn makes target hold request id for rawURL at the request stage
// and emits Network.requestIntercepted from target, with the request that
// the interceptor matches.
func interceptOn(f *fakewebkit.Server, target, id, rawURL string) {
	f.InterceptEvent(target, id, "request", map[string]any{
		"request": map[string]any{"url": rawURL, "method": "GET", "headers": map[string]string{"Accept": "*/*"}},
	})
}

// answersFor returns the completions of request id that took a hold of the
// fake (successful completions), in order.
func answersFor(f *fakewebkit.Server, id string) []fakewebkit.Answer {
	var out []fakewebkit.Answer
	for _, a := range f.Answers() {
		if a.RequestID == id {
			out = append(out, a)
		}
	}
	return out
}

// answeredOnceOn waits for the answer of request id and checks that id got
// exactly one completion, that it took the hold of target, and that it is
// method.
func answeredOnceOn(t *testing.T, f *fakewebkit.Server, id, target, method string) {
	t.Helper()
	eventually(t, "a successful completion of "+id+" on "+target, func() bool { return len(answersFor(f, id)) > 0 })
	time.Sleep(100 * time.Millisecond) // a second completion would arrive by now
	want := []fakewebkit.Answer{{Target: target, RequestID: id, Stage: "request", Method: method}}
	if got := answersFor(f, id); !slices.Equal(got, want) {
		t.Fatalf("completions that took a hold of %s: %v, want %v", id, got, want)
	}
	if n := len(answers(f, id)); n != 1 {
		t.Fatalf("request %s got %d completion commands, want 1 (no completion sent to another target)", id, n)
	}
	if held := f.HeldRequests(target); slices.Contains(held, id) {
		t.Fatalf("target %s still holds %s", target, id)
	}
}

// heldBy reports whether the scheduler records request id as held by
// target.
func heldBy(ps *core.PageSession, target, id string) bool {
	return slices.Contains(ps.Sched.PendingHolds(), core.HeldRequest{Target: target, RequestID: id})
}

// handlerKind is one way the interceptor answers a request: a handler of
// the lease holder, or the safety net when none claims it.
type handlerKind struct {
	name, url, method string
	add               func(ctx context.Context, p *Page, c webkit.Caller) error
}

func handlerKinds() []handlerKind {
	body := "routed"
	return []handlerKind{
		{
			name: "route", url: "https://fixture.example/routed/1", method: "Network.interceptRequestWithResponse",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/routed/*", Body: &body})
				return err
			},
		},
		{
			name: "credentials", url: "https://a.example/private", method: "Network.interceptWithRequest",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "u", "pw")
				return err
			},
		},
		{
			name: "offline", url: "https://fixture.example/any", method: "Network.interceptRequestWithError",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", true) },
		},
		{
			name: "safety net", url: "https://fixture.example/unclaimed", method: "Network.interceptContinue",
			add: func(context.Context, *Page, webkit.Caller) error { return nil },
		},
	}
}

// The W4 review P1: a request that a provisional target pauses is answered
// on that target, by each handler and by the safety net:
//   - before commit: the provisional target holds it while the old target
//     is still active;
//   - after commit: the interceptor handles it only after the commit, so
//     the answer goes to the committed target, which kept its id;
//   - on abort: the provisional target is destroyed with its paused
//     request before the interceptor handles it; nothing is sent, neither
//     to the destroyed target nor to the active one.
func TestProvisionalPauseIsAnsweredOnItsTarget(t *testing.T) {
	for _, k := range handlerKinds() {
		t.Run(k.name, func(t *testing.T) {
			setup := func(t *testing.T) (*rig, *Page) {
				r := newPausingRig(t)
				credentialsVerified[transport.KindIWDP] = true
				t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
				r.must(t, k.add)
				return r, r.page(t)
			}
			t.Run("before commit", func(t *testing.T) {
				r, _ := setup(t)
				old := r.f.ActiveTarget()
				<-r.f.Provisional("prov")
				interceptOn(r.f, "prov", "r6-before", k.url)
				answeredOnceOn(t, r.f, "r6-before", "prov", k.method)
				if r.f.ActiveTarget() != old {
					t.Fatal("setup: the provisional target committed")
				}
			})
			t.Run("after commit", func(t *testing.T) {
				r, p := setup(t)
				<-r.f.Provisional("prov")
				// The interceptor's drain waits for ic.mu, so it handles
				// the request only after the commit.
				p.icpt.mu.Lock()
				interceptOn(r.f, "prov", "r6-after", k.url)
				eventually(t, "the scheduler records the hold on prov", func() bool { return heldBy(p.ps, "prov", "r6-after") })
				r.f.Commit("prov")
				eventually(t, "the commit reaches the page connection", func() bool { return activeTargetOf(p.ps.Caller) == "prov" })
				p.icpt.mu.Unlock()
				answeredOnceOn(t, r.f, "r6-after", "prov", k.method)
			})
			t.Run("abort", func(t *testing.T) {
				r, p := setup(t)
				<-r.f.Provisional("prov")
				p.icpt.mu.Lock()
				interceptOn(r.f, "prov", "r6-abort", k.url)
				eventually(t, "the scheduler records the hold on prov", func() bool { return heldBy(p.ps, "prov", "r6-abort") })
				r.f.DestroyTarget("prov")
				eventually(t, "the scheduler drops the hold of the destroyed target", func() bool { return !heldBy(p.ps, "prov", "r6-abort") })
				p.icpt.mu.Unlock()
				time.Sleep(200 * time.Millisecond) // a completion would be sent by now
				if got := answers(r.f, "r6-abort"); len(got) != 0 {
					t.Fatalf("a request of the destroyed target got completions: %v", got)
				}
			})
		})
	}
}

// A route's delay that spans the commit: the request is paused on the
// provisional target, which commits while the route waits; the answer goes
// to the committed target. "network unroute" continues a request that a
// provisional target holds on that target.
func TestDelayedRouteAnswersOnTheTargetThatHoldsTheRequest(t *testing.T) {
	t.Run("delay across the commit", func(t *testing.T) {
		r := newPausingRig(t)
		body := "late"
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/slow/*", Body: &body, Delay: 300 * time.Millisecond})
			return err
		})
		<-r.f.Provisional("prov")
		interceptOn(r.f, "prov", "r6-slow", "https://fixture.example/slow/1")
		r.f.Commit("prov")
		answeredOnceOn(t, r.f, "r6-slow", "prov", "Network.interceptRequestWithResponse")
	})
	t.Run("unroute during the delay", func(t *testing.T) {
		r := newPausingRig(t)
		body := "late"
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/slow/*", Body: &body, Delay: time.Hour})
			return err
		})
		p := r.page(t)
		<-r.f.Provisional("prov")
		interceptOn(r.f, "prov", "r6-unroute", "https://fixture.example/slow/1")
		eventually(t, "the route holds the request", func() bool {
			p.icpt.mu.Lock()
			defer p.icpt.mu.Unlock()
			return len(p.icpt.held) == 1
		})
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
			_, err := p.RemoveRoutes(ctx, c, "s1", "")
			return err
		})
		answeredOnceOn(t, r.f, "r6-unroute", "prov", "Network.interceptContinue")
	})
}

// netTarget returns the source target that the network log records for r,
// read from its JSON form.
func netTarget(t *testing.T, r Request) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Target
}

// The network log keeps the requests of a provisional target apart from
// the active target's: each request records its source target, and the
// events of one target never change a request of another, also when both
// use the same request id. Equal ids are rare on a device, where WebKit
// prefixes ids with its process id, but the log must not depend on that.
// A redirect and a memory-cache hit of the provisional target are logged
// on it.
func TestNetlogKeepsTheRequestsOfEachTargetApart(t *testing.T) {
	r := newRig(t)
	var n *Netlog
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		n, _, err = p.StartNetlog(ctx, c, "s1")
		return err
	})
	old := r.f.ActiveTarget()
	<-r.f.Provisional("prov")
	redirect := map[string]any{"url": "https://fixture.example/x", "status": 302, "statusText": "Found", "headers": map[string]string{}, "mimeType": "text/plain"}
	hop := func(id, url string) map[string]any {
		e := reqEvent(id, url, 2)
		e["redirectResponse"] = redirect
		return e
	}
	r.f.EmitFrom(old, "Network.requestWillBeSent", reqEvent("1.1", "https://fixture.example/old", 1))
	r.f.EmitFrom(old, "Network.requestWillBeSent", reqEvent("1.2", "https://fixture.example/old2", 1))
	r.f.EmitFrom("prov", "Network.requestWillBeSent", reqEvent("1.1", "https://fixture.example/new", 2))
	// prov has no request 1.2: its first hop is a new request, and the old
	// target's 1.2 is no redirect. Its second hop redirects prov's 1.2.
	r.f.EmitFrom("prov", "Network.requestWillBeSent", hop("1.2", "https://fixture.example/new2"))
	r.f.EmitFrom("prov", "Network.requestWillBeSent", hop("1.2", "https://fixture.example/new3"))
	r.f.EmitFrom("prov", "Network.requestServedFromMemoryCache", map[string]any{
		"requestId": "1.5", "frameId": "f1", "loaderId": "l1", "timestamp": 2,
		"resource": map[string]any{"url": "https://fixture.example/new4", "type": "Image", "bodySize": 3},
	})
	r.f.EmitFrom("prov", "Network.loadingFinished", map[string]any{"requestId": "1.1", "timestamp": 3})
	r.f.EmitFrom("prov", "Network.loadingFinished", map[string]any{"requestId": "1.2", "timestamp": 3})
	eventually(t, "six requests", func() bool { return len(n.Snapshot(Filter{}).Requests) == 6 })
	eventually(t, "the provisional requests finish", func() bool { return n.InFlight() == 2 })
	time.Sleep(50 * time.Millisecond)
	// Log ids name the target (W4 round 7); WebKit's id stays in RequestID.
	type row struct{ id, requestID, url, target, state string }
	var got []row
	for _, q := range n.Snapshot(Filter{}).Requests {
		got = append(got, row{q.ID, q.RequestID, q.URL, netTarget(t, q), q.State})
	}
	want := []row{
		{old + ":1.1", "1.1", "https://fixture.example/old", old, StatePending},
		{old + ":1.2", "1.2", "https://fixture.example/old2", old, StatePending},
		{"prov:1.1", "1.1", "https://fixture.example/new", "prov", StateFinished},
		{"prov:1.2:redirect1", "1.2", "https://fixture.example/new2", "prov", StateRedirected},
		{"prov:1.2", "1.2", "https://fixture.example/new3", "prov", StateFinished},
		{"prov:1.5", "1.5", "https://fixture.example/new4", "prov", StateFinished},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("network log = %v, want %v", got, want)
	}
	if got := n.InFlight(); got != 2 {
		t.Fatalf("%d requests in flight, want the 2 of the old target", got)
	}
}

// A tombstone's removal goes to the target that the commit event names,
// and only while that target is the page's active target. Here the
// tombstone is applied for p1 while p1 is still provisional (as when a
// later commit replaced p1 before the tombstones ran): nothing is sent to
// any target and the tombstone stays. At p1's real commit the removal goes
// to p1.
func TestTombstonesGoToTheCommittedTarget(t *testing.T) {
	r := newPausingRig(t)
	m := newWKRules(r.f)
	k := ruleKinds()[0]
	refusedRemoval(t, r, m, k, "p1")
	p := r.page(t)
	old := r.f.ActiveTarget()
	oldBefore, p1Before := m.removalCount(old), m.removalCount("p1")
	p.icpt.applyTombstones("p1")
	if m.removalCount(old) != oldBefore || m.removalCount("p1") != p1Before || tombstones(p) != 1 {
		t.Fatalf("a tombstone applied for a target that is not active sent %d removals to %s and %d to p1, and left %d tombstones; want none sent and the tombstone kept",
			m.removalCount(old)-oldBefore, old, m.removalCount("p1")-p1Before, tombstones(p))
	}
	m.set(m.refuse, "p1", false)
	r.f.Commit("p1")
	eventually(t, "the tombstone removes the rule from p1", func() bool { return !m.holds("p1", k.rule) })
	eventually(t, "the tombstone goes", func() bool { return tombstones(p) == 0 })
	if n := m.removalCount("p1") - p1Before; n != 1 {
		t.Fatalf("p1 got %d removals at its commit, want 1", n)
	}
}

// queuedOnly is a page connection with only the webkit.Caller methods: no
// read-loop subscribers, no event sources and no active target, as a
// legacy webkit.Client.
type queuedOnly struct{ webkit.Caller }

// A connection that does not name its targets: the network log and the
// interceptor fall back to queued subscribers, which report "" as the
// target, and "" names the connection's own target. Here the scheduler
// runs on the session itself, so it records each hold in the read loop
// before a queued subscriber sees the event. Such a connection reports no
// commits, so a tombstone applied for a target sends nothing.
func TestConnectionWithoutTargetsFallsBackToQueuedSubscribers(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	sched := core.NewScheduler(sess, core.SchedulerOptions{})
	t.Cleanup(sched.Close)
	sched.SetLease("s1", 1)
	c := queuedOnly{sess}
	p, err := For(&core.PageSession{Caller: c, Sched: sched, State: core.NewDesiredState()})
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := p.StartNetlog(ctx, c, "s1")
	if err != nil {
		t.Fatal(err)
	}
	f.Emit("Network.requestWillBeSent", reqEvent("q.1", "https://fixture.example/q", 1))
	eventually(t, "the request in the log", func() bool { _, ok := n.Get("q.1"); return ok })
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "q.1", "timestamp": 2})
	eventually(t, "the request finishes", func() bool { q, _ := n.Get("q.1"); return q.State == StateFinished && n.InFlight() == 0 })
	if q, _ := n.Get("q.1"); netTarget(t, q) != "" {
		t.Fatalf("the request records target %q on a connection without event sources, want \"\"", netTarget(t, q))
	}

	interceptOn(f, f.ActiveTarget(), "q.2", "https://fixture.example/q2")
	answeredOnceOn(t, f, "q.2", f.ActiveTarget(), "Network.interceptContinue")

	p.icpt.mu.Lock()
	p.icpt.tombs["t"] = core.InterceptRule{URL: "/t/", Stage: "request", CaseSensitive: true, IsRegex: true}
	p.icpt.mu.Unlock()
	before := len(received(f, "Network.removeInterception"))
	p.icpt.applyTombstones(f.ActiveTarget())
	if n := len(received(f, "Network.removeInterception")) - before; n != 0 || tombstones(p) != 1 {
		t.Fatalf("a tombstone on a connection without targets sent %d removals and left %d tombstones, want none sent and the tombstone kept", n, tombstones(p))
	}
}

// A completion that WebKit refuses with an error is logged with the
// request and the target it went to (the scheduler's refusals are not:
// they mean that someone else answered).
func TestFailedAnswersAreLoggedWithTheirTarget(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	r := newPausingRig(t)
	body := "x"
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/routed/*", Body: &body})
		return err
	})
	for _, m := range []string{"Network.interceptRequestWithResponse", "Network.interceptContinue"} {
		r.f.Handle(m, func(fakewebkit.Call) (any, *fakewebkit.Error) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "refused by the test"}
		})
	}
	<-r.f.Provisional("prov")
	interceptOn(r.f, "prov", "r6-fail-route", "https://fixture.example/routed/1")
	interceptOn(r.f, "prov", "r6-fail-net", "https://fixture.example/other")
	for _, want := range []string{
		`completing intercepted request r6-fail-route of page target "prov" with Network.interceptRequestWithResponse`,
		`continuing unclaimed intercepted request r6-fail-net of page target "prov" (request stage)`,
	} {
		eventually(t, "the log line "+want, func() bool { return strings.Contains(buf.String(), want) })
	}
}
