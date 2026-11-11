package devtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// rig is one fake iPhone page (iOS 16.7.10) behind a fake iwdp listing,
// with a device reservation and a daemon manager, as in internal/core.
type rig struct {
	f   *fakewebkit.Server
	m   *core.Manager
	req core.PageRequest
	ao  core.AcquireOptions
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigWith(t, webkit.SessionOptions{TargetWait: time.Second})
}

// newPausingRig is newRig with pause on start: a provisional target is
// resumed after its setup, so a test that waits for the fake's
// Provisional channel finds the setup done (its agent resets, such as
// Network.disable, which continues the target's paused requests, sent).
func newPausingRig(t *testing.T) *rig {
	t.Helper()
	return newRigWith(t, webkit.SessionOptions{TargetWait: time.Second, PauseOnStart: true})
}

func newRigWith(t *testing.T, so webkit.SessionOptions) *rig {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(dev.ID, "agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := core.NewManager(core.ManagerOptions{
		Reservations: res, Session: so,
		Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
			return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return &rig{f: f, m: m, req: core.PageRequest{Transport: "iwdp", Addr: l.Addr()}, ao: core.AcquireOptions{Session: "s1", Reservation: "agent"}}
}

// do runs fn as one mutating operation of session s1.
func (r *rig) do(t *testing.T, fn func(ctx context.Context, p *Page, c webkit.Caller) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.m.Do(ctx, r.req, r.ao, "test", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
		p, err := For(mo.Page.PS)
		if err != nil {
			return err
		}
		return fn(ctx, p, mo.Op.Caller())
	})
}

func (r *rig) must(t *testing.T, fn func(ctx context.Context, p *Page, c webkit.Caller) error) {
	t.Helper()
	if err := r.do(t, fn); err != nil {
		t.Fatal(err)
	}
}

// page returns the page's DevTools state outside an operation.
func (r *rig) page(t *testing.T) *Page {
	var out *Page
	r.must(t, func(_ context.Context, p *Page, _ webkit.Caller) error { out = p; return nil })
	return out
}

func (r *rig) release(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.m.ReleaseSession(ctx, "s1"); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, 3*time.Second, what, cond)
}

// throughputBound bounds the waits of tests that check an outcome of much
// work (many messages, megabytes of data), not its speed: it only turns a
// hang into a failure. Under -race and a host load of 40 to 70 such work
// took several times its unloaded duration (W4 round 6 and 7 race runs).
const throughputBound = 2 * time.Minute

// eventuallyWithin is eventually with the bound d.
func eventuallyWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%s: not within %v", what, d)
		}
	}
}

func received(f *fakewebkit.Server, method string) []fakewebkit.Record {
	return f.ReceivedMethod(method)
}

// lastSeq is the sequence number of the fake's newest log record.
func lastSeq(f *fakewebkit.Server) int {
	log := f.Log()
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].Seq
}

// receivedAfter returns the calls of method that the fake received after
// the log record mark. A test that counts what a release sends takes the
// mark just before the release: a connection's setup also sends a
// <Domain>.disable before the first <Domain>.enable on a target, because
// core resets every agent whose state the connection does not know
// (docs/notes/w1b.md, "Review repairs, unknown backends").
func receivedAfter(f *fakewebkit.Server, method string, mark int) []fakewebkit.Record {
	var out []fakewebkit.Record
	for _, rec := range f.ReceivedMethod(method) {
		if rec.Seq > mark {
			out = append(out, rec)
		}
	}
	return out
}

func reqEvent(id, url string, ts float64) map[string]any {
	return map[string]any{
		"requestId": id, "frameId": "f1", "loaderId": "l1", "documentURL": url, "timestamp": ts, "walltime": 1.7e9,
		"initiator": map[string]any{"type": "other"}, "type": "Fetch",
		"request": map[string]any{"url": url, "method": "GET", "headers": map[string]string{"Accept": "*/*"}},
	}
}

func finish(f *fakewebkit.Server, id string, ts float64) {
	f.Emit("Network.responseReceived", map[string]any{
		"requestId": id, "frameId": "f1", "loaderId": "l1", "timestamp": ts, "type": "Fetch",
		"response": map[string]any{"url": "u", "status": 200, "statusText": "OK", "headers": map[string]string{"Content-Type": "text/plain"}, "mimeType": "text/plain", "source": "network"},
	})
	f.Emit("Network.loadingFinished", map[string]any{"requestId": id, "timestamp": ts + 0.01})
}

// 7.7 test 1: the log is bounded by count (the oldest go first and are
// counted) and returns a body for a request seen while it ran, and
// WebKit's error text otherwise.
func TestNetlogBoundsAndBodies(t *testing.T) {
	r := newRig(t)
	r.f.Handle("Network.getResponseBody", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ RequestID string }
		_ = json.Unmarshal(c.Params, &p)
		if p.RequestID == "r3" {
			return map[string]any{"body": "hello", "base64Encoded": false}, nil
		}
		return nil, &fakewebkit.Error{Code: -32000, Message: "No resource with given identifier found"}
	})
	r.f.Handle("Page.getResourceContent", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "No resource with given URL found"}
	})
	var n *Netlog
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		n, _, err = p.StartNetlog(ctx, c, "s1")
		return err
	})
	if got := len(received(r.f, "Network.enable")); got != 1 {
		t.Fatalf("Network.enable sent %d times, want 1", got)
	}
	n.SetLimits(2, DefaultMaxBodyBytes)
	for i, id := range []string{"r1", "r2", "r3"} {
		r.f.Emit("Network.requestWillBeSent", reqEvent(id, "https://fixture.example/"+id, float64(i)))
		finish(r.f, id, float64(i)+0.5)
	}
	eventually(t, "three requests seen", func() bool {
		s := n.Snapshot(Filter{})
		return s.Dropped == 1 && len(s.Requests) == 2 && s.Requests[1].State == StateFinished
	})
	s := n.Snapshot(Filter{})
	if s.Requests[0].ID != "page-1:r2" || s.Requests[1].ID != "page-1:r3" || s.Requests[1].State != StateFinished || s.Requests[1].Status != 200 {
		t.Fatalf("log = %+v, want r2 and r3 (r1 dropped as the oldest), r3 finished with 200", s.Requests)
	}
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		body, b64, err := n.Body(ctx, c, "page-1:r3")
		if err != nil || string(body) != "hello" || b64 {
			t.Errorf("body of r3 = %q, %v, %v; want hello", body, b64, err)
		}
		_, _, err = n.Body(ctx, c, "page-1:r2")
		if err == nil || !strings.Contains(err.Error(), "No resource with given identifier found") {
			t.Errorf("body of r2: %v; want WebKit's error text", err)
		}
		return nil
	})
}

// 7.7 test 1: the capture survives a provisional-target swap; Network is
// enabled on the provisional target before commit (design 2.7), and
// requests of the new document are logged. The target is paused on start,
// and the test commits it only after page-2 got Network.enable: under
// -race the setup's Network.enable took 65 to 170 ms, and a fixed 100 ms
// commit delay failed about 1 run in 10 (W4 round 6).
func TestNetlogSurvivesASwap(t *testing.T) {
	r := newPausingRig(t)
	var n *Netlog
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		n, _, err = p.StartNetlog(ctx, c, "s1")
		return err
	})
	<-r.f.Provisional("page-2")
	eventually(t, "Network.enable sent to the provisional target page-2", func() bool {
		return slices.ContainsFunc(received(r.f, "Network.enable"), func(rec fakewebkit.Record) bool { return rec.Target == "page-2" })
	})
	r.f.Commit("page-2")
	eventually(t, "the page connection commits page-2", func() bool { return activeTargetOf(r.page(t).ps.Caller) == "page-2" })
	r.f.EmitFrom("page-2", "Network.requestWillBeSent", reqEvent("n1", "https://other.example/x", 1))
	eventually(t, "request of the new document", func() bool { _, ok := n.Get("page-2:n1"); return ok })

	// WebKitGTK 2.54 starts the document load of a cross-process
	// navigation before the provisional target has Network enabled: the
	// new target sends responseReceived and loadingFinished for the
	// document without a requestWillBeSent (seen in the W4 WebKitGTK
	// suite, TestNetlogAcrossACrossProcessSwap). The log still lists it.
	r.f.EmitFrom("page-2", "Network.responseReceived", map[string]any{
		"requestId": "0.9", "frameId": "0.1", "loaderId": "0.2", "timestamp": 2, "type": "Document",
		"response": map[string]any{"url": "https://other.example/doc.html", "status": 200, "statusText": "OK", "headers": map[string]string{"Content-Type": "text/html"}, "mimeType": "text/html", "source": "network"},
	})
	r.f.EmitFrom("page-2", "Network.loadingFinished", map[string]any{"requestId": "0.9", "timestamp": 2.1})
	eventually(t, "the document of the new target", func() bool { d, ok := n.Get("page-2:0.9"); return ok && d.State == StateFinished })
	d, _ := n.Get("page-2:0.9")
	if d.URL != "https://other.example/doc.html" || d.Status != 200 || d.Type != "Document" || d.Method != "" {
		t.Fatalf("document seen only by its response = %+v; want its URL, 200, type Document and no method (WebKit did not send it)", d)
	}
}

func pending(n *Netlog) int { return n.InFlight() }

// 7.7 test 8 and 7.3 test 17 at the W4 level: with a netlog capture and a
// route active, headers {"X-A":"1"} then {} run the Network reset: the
// fake gets disable, enable, then the route's interception again, and
// never X-A with an empty value; the log gets a gap, the request in flight
// is capture_reset, and an earlier body is reported as gone. The same with
// the legacy Network.setExtraHTTPHeaders as header owner.
func TestHeaderRemovalWithOtherNetworkOwners(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "set headers", true: "legacy set_extra_headers"}[legacy], func(t *testing.T) {
			r := newRig(t)
			var n *Netlog
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				var err error
				if n, _, err = p.StartNetlog(ctx, c, "s1"); err != nil {
					return err
				}
				_, err = p.AddRoute(ctx, c, "s1", Route{Pattern: "**/api/*", Abort: true})
				return err
			})
			r.f.Emit("Network.requestWillBeSent", reqEvent("early", "https://fixture.example/early", 1))
			finish(r.f, "early", 1.5)
			r.f.Emit("Network.requestWillBeSent", reqEvent("inflight", "https://fixture.example/slow", 2))
			eventually(t, "requests seen", func() bool { return pending(n) == 1 })
			setHeaders := func(h map[string]string) {
				if !legacy {
					r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
						_, err := p.SetHeaders(ctx, c, "s1", h)
						return err
					})
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				mo, err := r.m.Begin(ctx, r.req, r.ao, "legacy", core.ClassExclusive)
				if err != nil {
					t.Fatal(err)
				}
				defer r.m.End(mo.ID)
				raw, _ := json.Marshal(map[string]any{"headers": h})
				if _, err := r.m.RawAs(ctx, mo, core.SessionOwner("legacy", mo.Session), "Network.setExtraHTTPHeaders", raw); err != nil {
					t.Fatal(err)
				}
			}
			setHeaders(map[string]string{"X-A": "1"})
			mark := len(r.f.Received())
			setHeaders(map[string]string{})
			var after []string
			for _, rec := range r.f.Received()[mark:] {
				if strings.HasPrefix(rec.Method, "Network.") {
					after = append(after, rec.Method)
				}
				if rec.Method == "Network.setExtraHTTPHeaders" && strings.Contains(string(rec.Params), "X-A") {
					t.Fatalf("X-A was sent again after its removal: %s", rec.Params)
				}
			}
			want := []string{"Network.disable", "Network.enable", "Network.setInterceptionEnabled", "Network.addInterception"}
			if strings.Join(after, " ") != strings.Join(want, " ") {
				t.Fatalf("Network calls after the removal = %v, want %v", after, want)
			}
			s := n.Snapshot(Filter{})
			if len(s.Gaps) != 1 || len(s.Gaps[0].Closed) != 1 || s.Gaps[0].Closed[0] != "page-1:inflight" {
				t.Fatalf("gaps = %+v, want one gap that closed the request in flight", s.Gaps)
			}
			if got, _ := n.Get("page-1:inflight"); got.State != StateCaptureReset {
				t.Fatalf("request in flight is %q, want capture_reset", got.State)
			}
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				if _, _, err := n.Body(ctx, c, "page-1:early"); err == nil || !strings.Contains(err.Error(), "no longer available") {
					t.Errorf("body of an earlier request after the reset: %v; want 'no longer available'", err)
				}
				return nil
			})
		})
	}
}

func interceptParams(f *fakewebkit.Server, method string) []map[string]any {
	var out []map[string]any
	for _, rec := range received(f, method) {
		var p map[string]any
		_ = json.Unmarshal(rec.Params, &p)
		out = append(out, p)
	}
	return out
}

func intercept(f *fakewebkit.Server, id, url string) {
	f.Emit("Network.requestIntercepted", map[string]any{"requestId": id, "request": map[string]any{"url": url, "method": "GET", "headers": map[string]string{"Accept": "*/*"}}})
}

// Every intercepted request that a route matches gets its completion.
// The scheduler records an intercepted request in the read loop and
// refuses a completion for a request it has not recorded; the interceptor
// must not send its completion before that record exists. Before the fix,
// the interceptor was a read-loop subscriber next to the scheduler's (in
// random order) and started the completion at once, so about 1 request in
// 30 under -race was refused and stayed held (TestNetworkRouteCommands).
func TestEveryInterceptedRequestIsCompleted(t *testing.T) {
	r := newRig(t)
	body := "ok"
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/fulfill/*", Body: &body})
		return err
	})
	const n = 300
	for i := 0; i < n; i++ {
		intercept(r.f, fmt.Sprintf("r%d", i), fmt.Sprintf("https://fixture.example/fulfill/%d", i))
	}
	eventually(t, "every completion", func() bool { return len(received(r.f, "Network.interceptRequestWithResponse")) == n })
}

// 7.7 test 3 (WIP mode): fulfill with body and status, abort, delay;
// unroute continues a held request at once; release turns interception
// off (the fake sees setInterceptionEnabled false).
func TestRoutes(t *testing.T) {
	r := newRig(t)
	body := "ok"
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		for _, rt := range []Route{
			{Pattern: "**/fulfill", Status: 201, Body: &body, ContentType: "text/plain"},
			{Pattern: "**/abort", Abort: true},
			{Pattern: "**/slow", Delay: 300 * time.Millisecond},
			{Pattern: "**/held", Delay: time.Hour},
		} {
			if _, err := p.AddRoute(ctx, c, "s1", rt); err != nil {
				return err
			}
		}
		return nil
	})
	if got := interceptParams(r.f, "Network.addInterception"); len(got) != 4 || got[0]["url"] != `^.*/fulfill$` || got[0]["isRegex"] != true || got[0]["stage"] != "request" {
		t.Fatalf("addInterception = %v, want 4 regex rules for the request stage", got)
	}
	// Request z is another owner's: a legacy command's rule pauses it, so
	// that owner answers it (the safety net continues only requests that
	// no owner claims, TestRemovalRefusedByAProvisionalTarget).
	other := core.InterceptRule{URL: "https://fixture.example/not-mine", Stage: "request", CaseSensitive: true}
	r.page(t).ps.State.AddInterception("legacy:other", other)
	intercept(r.f, "a", "https://fixture.example/x/fulfill")
	intercept(r.f, "b", "https://fixture.example/abort")
	start := time.Now()
	intercept(r.f, "c", "https://fixture.example/slow")
	intercept(r.f, "z", "https://fixture.example/not-mine")
	eventually(t, "fulfill", func() bool { return len(received(r.f, "Network.interceptRequestWithResponse")) == 1 })
	f := interceptParams(r.f, "Network.interceptRequestWithResponse")[0]
	dec, _ := base64.StdEncoding.DecodeString(f["content"].(string))
	if f["requestId"] != "a" || f["status"] != float64(201) || string(dec) != "ok" || f["base64Encoded"] != true {
		t.Fatalf("fulfill = %v, want request a, status 201, body ok", f)
	}
	eventually(t, "abort", func() bool { return len(received(r.f, "Network.interceptRequestWithError")) == 1 })
	if e := interceptParams(r.f, "Network.interceptRequestWithError")[0]; e["requestId"] != "b" {
		t.Fatalf("abort = %v, want request b", e)
	}
	eventually(t, "delayed continue", func() bool { return len(received(r.f, "Network.interceptContinue")) == 1 })
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("delayed request continued after %v, want at least 300 ms", el)
	}
	intercept(r.f, "d", "https://fixture.example/held")
	time.Sleep(50 * time.Millisecond)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		n, err := p.RemoveRoutes(ctx, c, "s1", "**/held")
		if n != 1 {
			t.Errorf("unroute removed %d routes, want 1", n)
		}
		return err
	})
	eventually(t, "unroute continues the held request", func() bool { return len(received(r.f, "Network.interceptContinue")) == 2 })
	for _, p := range interceptParams(r.f, "Network.interceptContinue") {
		if p["requestId"] == "z" {
			t.Fatal("a request that another owner's rule paused was answered")
		}
	}
	r.page(t).ps.State.RemoveInterception("legacy:other", other)
	r.release(t)
	off := false
	for _, p := range interceptParams(r.f, "Network.setInterceptionEnabled") {
		off = off || p["enabled"] == false
	}
	if !off {
		t.Fatal("release did not send Network.setInterceptionEnabled false")
	}
	if got := r.page(t).Routes("s1"); len(got) != 0 {
		t.Fatalf("routes after release = %v, want none", got)
	}
}

func consoleMsg(text, level string, ts float64) map[string]any {
	return map[string]any{"message": map[string]any{"source": "console-api", "level": level, "text": text, "type": "log", "timestamp": ts}}
}

// 7.7 test 4: messages replayed by Console.enable (a re-enable or
// reconnect) are not stored twice; the drop count is reported; errors
// include uncaught errors and unhandled rejections with stack, without the
// duplicate WebKit message of the same error.
func TestConsoleReplayAndErrors(t *testing.T) {
	r := newRig(t)
	var k *Console
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		k, err = p.StartErrors(ctx, c, "s1")
		return err
	})
	for i, txt := range []string{"one", "two", "three"} {
		r.f.Emit("Console.messageAdded", consoleMsg(txt, "log", float64(i+1)))
	}
	// The replay of a re-enable: the same three events again.
	for i, txt := range []string{"one", "two", "three"} {
		r.f.Emit("Console.messageAdded", consoleMsg(txt, "log", float64(i+1)))
	}
	r.f.Emit("Console.messageAdded", consoleMsg("one", "log", 9)) // a new message with the same text
	hook, _ := json.Marshal(map[string]any{"kind": "unhandledrejection", "message": "boom", "stack": "f@https://fixture.example/a.js:1:2"})
	r.f.Emit("Console.messageAdded", consoleMsg(errorMarker+string(hook), "debug", 10))
	r.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"source": "javascript", "level": "error", "text": "Unhandled Promise Rejection: Error: boom", "timestamp": 11.0}})
	r.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "javascript", "level": "error", "text": "TypeError: x is undefined", "timestamp": 12.0,
		"stackTrace": map[string]any{"callFrames": []any{map[string]any{"functionName": "g", "url": "https://fixture.example/b.js", "lineNumber": 3, "columnNumber": 4}}},
	}})
	eventually(t, "messages", func() bool { m, _ := k.Messages(ConsoleFilter{}); return len(m) == 6 })
	msgs, dropped := k.Messages(ConsoleFilter{})
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Text)
	}
	if strings.Join(texts, "|") != "one|two|three|one|Unhandled Promise Rejection: Error: boom|TypeError: x is undefined" || dropped != 0 {
		t.Fatalf("messages = %q (dropped %d), want each original once plus the new ones", texts, dropped)
	}
	errs := k.Errors()
	if len(errs) != 2 || errs[0].Kind != "unhandledrejection" || errs[0].Stack == "" || errs[1].Kind != "console" || !strings.Contains(errs[1].Stack, "b.js:3:4") {
		t.Fatalf("errors = %+v, want the rejection with its stack and the TypeError with its stack", errs)
	}
	if got := received(r.f, "Page.setBootstrapScript"); !strings.Contains(string(got[len(got)-1].Params), "iwdp bootstrap part: errors:s1") {
		t.Fatal("the error hooks are not a bootstrap part")
	}
	k.SetMax(2)
	if _, dropped := k.Messages(ConsoleFilter{}); dropped != 4 {
		t.Fatalf("dropped = %d after shrinking the buffer to 2, want 4", dropped)
	}
	if m, _ := k.Messages(ConsoleFilter{Level: "error"}); len(m) != 2 {
		t.Fatalf("--level error = %d messages, want 2", len(m))
	}
}

// 7.7 test 6 (fake part): set media dark sends the preference override,
// and a takeover reconnect re-applies it on the new connection; release
// resets it.
func TestMediaReappliedAfterReconnect(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "dark"})
	})
	dark := func() int {
		k := 0
		for _, p := range interceptParams(r.f, "Page.overrideUserPreference") {
			if p["name"] == "PrefersColorScheme" && p["value"] == "Dark" {
				k++
			}
		}
		return k
	}
	if dark() != 1 {
		t.Fatalf("overrideUserPreference Dark sent %d times, want 1", dark())
	}
	r.f.CloseConnections()
	time.Sleep(200 * time.Millisecond)
	r.ao.Takeover = true
	r.must(t, func(context.Context, *Page, webkit.Caller) error { return nil })
	r.ao.Takeover = false
	if dark() != 2 {
		t.Fatalf("overrideUserPreference Dark sent %d times after the reconnect, want 2", dark())
	}
	r.release(t)
	reset := false
	for _, p := range interceptParams(r.f, "Page.overrideUserPreference") {
		_, has := p["value"]
		reset = reset || (p["name"] == "PrefersColorScheme" && !has)
	}
	if !reset {
		t.Fatal("release did not reset the color scheme preference")
	}
}

// 7.7 tests 1 and 4 across a lost connection: the takeover reattaches
// the same page session with its desired state, so the captures that the
// desired state owns keep their contents and keep capturing. Before, the
// DevTools state was forgotten when the connection was lost, and the
// next command got an empty log while the old entries still held the
// Network and Console domains (seen on WebKitGTK in
// TestConsoleReplayIsStoredOnce).
func TestCapturesSurviveATakeover(t *testing.T) {
	r := newRig(t)
	var n *Netlog
	var k *Console
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		if n, _, err = p.StartNetlog(ctx, c, "s1"); err != nil {
			return err
		}
		k, err = p.StartConsole(ctx, c, "s1")
		return err
	})
	r.f.Emit("Network.requestWillBeSent", reqEvent("before", "https://fixture.example/before", 1))
	r.f.Emit("Console.messageAdded", consoleMsg("before the loss", "log", 1))
	eventually(t, "the request and message before the loss", func() bool {
		_, ok := n.Get("page-1:before")
		m, _ := k.Messages(ConsoleFilter{})
		return ok && len(m) == 1
	})
	r.f.CloseConnections()
	time.Sleep(200 * time.Millisecond)
	r.ao.Takeover = true
	var after *Page
	r.must(t, func(_ context.Context, p *Page, _ webkit.Caller) error { after = p; return nil })
	r.ao.Takeover = false
	if got := after.Netlog("s1"); got != n {
		t.Fatalf("netlog after the takeover = %p, want the capture from before the loss (%p)", got, n)
	}
	if got := after.Console("s1"); got != k {
		t.Fatalf("console capture after the takeover = %p, want the capture from before the loss (%p)", got, k)
	}
	r.f.Emit("Network.requestWillBeSent", reqEvent("after", "https://fixture.example/after", 2))
	eventually(t, "a request after the takeover", func() bool { _, ok := n.Get("page-1:after"); return ok })
	if _, ok := n.Get("page-1:before"); !ok {
		t.Fatal("the request from before the loss left the log")
	}
}

// 7.7 test 7 (fake part): credentials go only to requests of their
// origin; the command is unsupported where the origin scope is not
// verified; the password is in no error.
func TestCredentialsAreOriginScoped(t *testing.T) {
	r := newRig(t)
	const secret = "s3cr3t-pw"
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "user", secret)
		if !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), secret) {
			t.Errorf("credentials on an unverified transport: %v; want unsupported without the password", err)
		}
		return nil
	})
	credentialsVerified[transport.KindIWDP] = true
	t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		if _, err := p.SetCredentials(ctx, c, "s1", "https://a.example/path", "user", secret); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("an origin with a path: %v; want an error without the password", err)
		}
		_, err := p.SetCredentials(ctx, c, "s1", "https://A.example", "user", secret)
		return err
	})
	if got := interceptParams(r.f, "Network.addInterception"); len(got) != 1 || got[0]["url"] != `^https://a\.example/` {
		t.Fatalf("addInterception = %v, want one rule for https://a.example/", got)
	}
	intercept(r.f, "b1", "https://b.example/img.png")
	intercept(r.f, "a1", "https://a.example/api")
	eventually(t, "credentials", func() bool { return len(received(r.f, "Network.interceptWithRequest")) == 1 })
	time.Sleep(100 * time.Millisecond)
	got := interceptParams(r.f, "Network.interceptWithRequest")
	h, _ := got[0]["headers"].(map[string]any)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+secret))
	if len(got) != 1 || got[0]["requestId"] != "a1" || h["Authorization"] != want || h["Accept"] != "*/*" {
		t.Fatalf("interceptWithRequest = %v, want only a1 with the Authorization header added", got)
	}
}

// W4 review finding (intercept.go): a burst of intercepted requests larger
// than a subscriber queue (1024 events) while the interceptor cannot run
// must still give every request its completion. A dropped event leaves
// the request held by WebKit until the release, so an awaited fetch that
// an automatic route matches stalls.
func TestInterceptBurstIsLossless(t *testing.T) {
	r := newRig(t)
	var ic *interceptor
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/burst/*", Abort: true})
		ic = p.icpt
		return err
	})
	// The handler cannot run while the read loop receives the burst.
	ic.mu.Lock()
	const count = 1100
	for i := range count {
		intercept(r.f, fmt.Sprintf("burst-%d", i), fmt.Sprintf("https://fixture.example/burst/%d", i))
	}
	// A reply after the burst: the read loop has read every event of it.
	r.must(t, func(ctx context.Context, _ *Page, c webkit.Caller) error {
		_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1"})
		return err
	})
	ic.mu.Unlock()
	// The test checks that no completion is lost, not their speed: under
	// -race and host load 1100 completions took more than 3 s (W4 round 6).
	eventuallyWithin(t, throughputBound, "a completion for each of the 1100 intercepted requests", func() bool {
		return len(received(r.f, "Network.interceptRequestWithError")) >= count
	})
	seen := map[string]bool{}
	for _, p := range interceptParams(r.f, "Network.interceptRequestWithError") {
		id, _ := p["requestId"].(string)
		if seen[id] {
			t.Fatalf("request %s completed twice", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("%d distinct requests completed, want %d", len(seen), count)
	}
}

// The interceptor handles an intercepted request only after the
// scheduler has recorded it; the scheduler refuses the completion of a
// request it has not recorded, which leaves the request held. A slow
// read-loop subscriber of the same method runs in random order with the
// scheduler's, so an interceptor that is a subscriber of the method too
// would often run before the record (and before the slow subscriber).
func TestInterceptorRunsAfterTheSchedulerRecord(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/order/*", Abort: true})
		if err != nil {
			return err
		}
		ss, ok := p.ps.Caller.(interface {
			SubscribeSync(string, webkit.EventHandler) func()
		})
		if !ok {
			return fmt.Errorf("the page caller %T has no read-loop subscribers", p.ps.Caller)
		}
		unsub := ss.SubscribeSync("Network.requestIntercepted", func(string, json.RawMessage) { time.Sleep(5 * time.Millisecond) })
		t.Cleanup(unsub)
		return nil
	})
	const count = 40
	for i := range count {
		intercept(r.f, fmt.Sprintf("order-%d", i), fmt.Sprintf("https://fixture.example/order/%d", i))
	}
	eventually(t, "a completion for each intercepted request", func() bool {
		return len(received(r.f, "Network.interceptRequestWithError")) == count
	})
}

// W4 review finding (netlog.go): the network-idle tracker counts a request
// in flight until it finishes, also after the bounded history dropped it
// and after Clear; a network reset closes it.
func TestIdleTrackerOutlivesTheHistory(t *testing.T) {
	r := newRig(t)
	var n *Netlog
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		var err error
		n, _, err = p.StartNetlog(ctx, c, "s1")
		return err
	})
	n.SetLimits(1, DefaultMaxBodyBytes)
	r.f.Emit("Network.requestWillBeSent", reqEvent("long", "https://fixture.example/slow", 1))
	r.f.Emit("Network.requestWillBeSent", reqEvent("short", "https://fixture.example/fast", 2))
	finish(r.f, "short", 3)
	eventually(t, "short request finished", func() bool { q, ok := n.Get("page-1:short"); return ok && q.State == StateFinished })
	if _, ok := n.Get("page-1:long"); ok {
		t.Fatal("the history of one entry still holds the older request")
	}
	notIdle := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		res, err := n.WaitIdle(ctx, time.Millisecond)
		if err == nil || res.InFlight != 1 {
			t.Fatalf("%s: WaitIdle = %+v, %v; want a timeout with 1 request in flight", what, res, err)
		}
	}
	notIdle("after the eviction")
	n.Clear()
	notIdle("after Clear")
	finish(r.f, "long", 4)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := n.WaitIdle(ctx, time.Millisecond); err != nil {
		t.Fatalf("WaitIdle after the evicted request finished: %v", err)
	}
	// A network reset closes the requests in flight, held or not.
	r.f.Emit("Network.requestWillBeSent", reqEvent("cut", "https://fixture.example/cut", 5))
	r.f.Emit("Network.requestWillBeSent", reqEvent("cut2", "https://fixture.example/cut2", 6))
	eventually(t, "two requests in flight", func() bool { return n.InFlight() == 2 })
	n.onGap(core.NetworkGap{Start: time.Now(), End: time.Now()})
	if got := n.InFlight(); got != 0 {
		t.Fatalf("in flight after a network reset: %d, want 0", got)
	}
}

// W4 review finding (overrides.go putOverride): an override whose second
// send fails has applied its first one (Dark). That part is undone, so
// the page ends without Dark: by the undo at once, or at the latest by the
// release when the undo fails too.
func TestFailedMediaOverrideIsUndone(t *testing.T) {
	for _, undoFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("undo fails %v", undoFails), func(t *testing.T) {
			r := newRig(t)
			var failing atomic.Bool
			failing.Store(true)
			r.f.Handle("Page.overrideUserPreference", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				p := string(c.Params)
				if failing.Load() && strings.Contains(p, "PrefersReducedMotion") && strings.Contains(p, "Reduce") {
					return nil, &fakewebkit.Error{Code: -32000, Message: "override failed"}
				}
				if failing.Load() && undoFails && !strings.Contains(p, "\"value\"") {
					return nil, &fakewebkit.Error{Code: -32000, Message: "reset failed"}
				}
				return nil, nil
			})
			err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "dark", ReducedMotion: true})
			})
			if err == nil || !strings.Contains(err.Error(), "override failed") {
				t.Fatalf("SetMedia: %v, want the reduced-motion failure", err)
			}
			failing.Store(false)
			r.release(t)
			last := ""
			for _, rec := range received(r.f, "Page.overrideUserPreference") {
				if strings.Contains(string(rec.Params), "PrefersColorScheme") {
					last = string(rec.Params)
				}
			}
			if strings.Contains(last, "\"value\"") {
				t.Fatalf("the last color-scheme override is %s; want a reset after the failed override and the release", last)
			}
		})
	}
}

// W4 review finding (intercept.go SetOffline): a failed rule install does
// not leave the session marked offline, so a retry installs the rule and
// requests then fail.
func TestOfflineRetryAfterRuleFailure(t *testing.T) {
	r := newRig(t)
	var fail atomic.Bool
	fail.Store(true)
	r.f.Handle("Network.addInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if fail.CompareAndSwap(true, false) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "rule rejected"}
		}
		return nil, nil
	})
	if err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", true) }); err == nil {
		t.Fatal("SetOffline: no error, want the add-rule failure")
	}
	before := len(received(r.f, "Network.addInterception"))
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", true) })
	if got := len(received(r.f, "Network.addInterception")); got != before+1 {
		t.Fatalf("the retry sent %d Network.addInterception, want 1", got-before)
	}
	intercept(r.f, "off-1", "https://fixture.example/any")
	eventually(t, "the offline failure of a request", func() bool {
		return len(received(r.f, "Network.interceptRequestWithError")) == 1
	})
	// A failed rule removal keeps the session offline, so that the rule
	// that can still be in place does not hold requests without an
	// answer, and a retry removes the rule again.
	var failRemove atomic.Bool
	failRemove.Store(true)
	r.f.Handle("Network.removeInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if failRemove.CompareAndSwap(true, false) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "rule removal rejected"}
		}
		return nil, nil
	})
	if err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", false) }); err == nil {
		t.Fatal("SetOffline(false): no error, want the remove-rule failure")
	}
	intercept(r.f, "off-2", "https://fixture.example/any")
	eventually(t, "the offline failure of a request after the failed removal", func() bool {
		return len(received(r.f, "Network.interceptRequestWithError")) == 2
	})
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", false) })
	if got := len(received(r.f, "Network.removeInterception")); got != 2 {
		t.Fatalf("Network.removeInterception sent %d times, want 2 (the failure and the retry)", got)
	}
}

// W4 review finding (overrides.go putOverride): when the reset of the
// earlier override fails, the earlier override stays owned, so the release
// resets it.
func TestFailedMediaResetKeepsTheOldOverride(t *testing.T) {
	r := newRig(t)
	var failReset atomic.Bool
	r.f.Handle("Page.overrideUserPreference", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if !strings.Contains(string(c.Params), "\"value\"") && failReset.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "reset failed"}
		}
		return nil, nil
	})
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "dark"})
	})
	failReset.Store(true)
	if err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "light"})
	}); err == nil || !strings.Contains(err.Error(), "reset failed") {
		t.Fatalf("SetMedia(light): %v, want the reset failure", err)
	}
	failReset.Store(false)
	mark := len(received(r.f, "Page.overrideUserPreference"))
	r.release(t)
	for _, rec := range received(r.f, "Page.overrideUserPreference")[mark:] {
		if p := string(rec.Params); strings.Contains(p, "PrefersColorScheme") && !strings.Contains(p, "\"value\"") {
			return
		}
	}
	t.Fatal("the release sent no color-scheme reset: the Dark override lost its owner when its reset failed")
}
