package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the W1b review (docs/notes/w1b.md, "Review repairs").

// urlReq selects the rig's page by its webSocketDebuggerUrl.
func (r *rig) urlReq() PageRequest {
	return PageRequest{Transport: r.req.Transport, Addr: r.req.Addr, Page: r.f.URL}
}

// Design 2.5: one page has one record, one connection and one lease,
// whether it was selected by discovery, by title or by its listed URL.
func TestURLAndDiscoverySelectOnePage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second func(r *rig) PageRequest
	}{
		{"auto then URL", func(r *rig) PageRequest { return r.req }, (*rig).urlReq},
		{"URL then title", (*rig).urlReq, func(r *rig) PageRequest { q := r.req; q.Page = "Fixture"; return q }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			ctx := ctxFor(t, 5*time.Second)
			a, err := r.m.Begin(ctx, tc.first(r), AcquireOptions{Session: "s1", Reservation: "agent"}, "a", ClassRead)
			if err != nil {
				t.Fatalf("first Begin: %v", err)
			}
			r.m.End(a.ID)
			_, err = r.m.Begin(ctx, tc.second(r), AcquireOptions{Session: "s2", Reservation: "agent"}, "b", ClassRead)
			if !errors.Is(err, ErrLeased) || exitCode(err) != CodeBusy {
				t.Fatalf("second session: %v (exit %d), want the lease error of s1 (exit 4)", err, exitCode(err))
			}
			b, err := r.m.Begin(ctx, tc.second(r), AcquireOptions{Session: "s1", Reservation: "agent"}, "c", ClassRead)
			if err != nil {
				t.Fatalf("same session, other selector: %v", err)
			}
			r.m.End(b.ID)
			if b.Page != a.Page {
				t.Errorf("two page records (%s, %s) for one page", a.Page.Key, b.Page.Key)
			}
			if n := r.f.ConnectionCount(); n != 1 {
				t.Errorf("connections = %d, want 1", n)
			}
		})
	}
}

// Design 2.5 rule 2: a listed URL keeps the device's busy hint.
func TestURLSelectionKeepsTheBusyHint(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, true, ManagerOptions{})
	r.reserve(t, "agent")
	r.dev.AddPage("Other", "https://other.example/", "ws://127.0.0.1:1/x") // keeps the frontend hint on
	r.dev.SetBusy(r.pg, true)
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), r.urlReq(), agent, "a", ClassRead)
	if !errors.Is(err, ErrPageBusy) || exitCode(err) != CodeBusy {
		t.Fatalf("Begin on a busy page by URL: %v (exit %d), want ErrPageBusy (exit 4)", err, exitCode(err))
	}
	if n := r.f.ConnectionCount(); n != 0 {
		t.Errorf("connections = %d, want 0", n)
	}
}

// Design 2.5 rule 1: a listed URL needs a reservation of its own device.
func TestURLSelectionNeedsTheDevicesReservation(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	if _, err := r.res.Reserve("00008020-0000000000000999", "agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), r.urlReq(), agent, "a", ClassRead)
	if !errors.Is(err, ErrNoReservation) || exitCode(err) != CodeReservation {
		t.Fatalf("Begin with a reservation of another device: %v (exit %d), want ErrNoReservation (exit 6)", err, exitCode(err))
	}
	if n := r.f.ConnectionCount(); n != 0 {
		t.Errorf("connections = %d, want 0", n)
	}
}

// An iwdp device-port URL that iwdp does not list is no page; it is never
// attached as a page of an unknown device.
func TestUnlistedIWDPPortURLRefused(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	q := r.req
	q.Page = "ws://127.0.0.1:9230/devtools/page/77"
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "a", ClassRead)
	if !errors.Is(err, ErrNoPage) || exitCode(err) != CodeNoPage {
		t.Fatalf("Begin on an unlisted iwdp URL: %v (exit %d), want ErrNoPage (exit 5)", err, exitCode(err))
	}
}

// Design 2.6 step 4: after IdleProbe without commands, the probe runs
// before the next command; a failed probe refuses the command unsent.
func TestIdleProbeRunsBeforeTheNextCommand(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{IdleProbe: 100 * time.Millisecond})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "a", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	probes := evalsOf(r.f, "1")
	time.Sleep(250 * time.Millisecond)
	r.f.StallAll()
	_, err = r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "b", ClassExclusive)
	if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy {
		t.Fatalf("Begin after idle with a dead probe: %v (exit %d), want ErrSessionStalled (exit 4)", err, exitCode(err))
	}
	if n := evalsOf(r.f, "1"); n != probes+1 {
		t.Errorf("probes after the idle interval = %d, want 1", n-probes)
	}
}

// Design 2.5: a sweep that expires a lease claims it atomically. A command
// of the same session that arrives while the expiry's cleanup runs waits
// for it and then gets a new lease; it is never admitted under the old one
// and then ended by the release.
func TestSweepDoesNotEndARenewedCommand(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{SweepEvery: time.Hour})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "a", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	r.f.DelayReplies("Network.disable", 700*time.Millisecond)
	// The first Network.enable went out after the agent's reset, a
	// Network.disable; the expiry cleanup sends the next one.
	disables := len(r.f.ReceivedMethod("Network.disable"))
	swept := make(chan struct{})
	go func() { defer close(swept); r.m.Sweep(time.Now().Add(DefaultLeaseTTL + time.Second)) }()
	eventually(t, 3*time.Second, "the expiry cleanup to start", func() bool { return len(r.f.ReceivedMethod("Network.disable")) == disables+1 })
	next, err := r.m.Begin(ctx, r.req, agent, "b", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin during the expiry: %v", err)
	}
	defer r.m.End(next.ID)
	if next.Epoch == mo.Epoch {
		t.Errorf("the command runs under the expired epoch %d", mo.Epoch)
	}
	if _, err := r.m.Raw(ctx, next.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"2"}`)); err != nil {
		t.Fatalf("Raw in the new lease: %v", err)
	}
	<-swept
}

// Design 2.6 step 3 and 2.10: recovery on a single-inspector endpoint (a
// new connection closes the old one) retires the old connection itself,
// so the page is recovered, not lost.
func TestRecoveryOnASingleInspectorEndpoint(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.f.StallConnection(1)
	_, _ = r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
	r.m.End(mo.ID)
	pg := mo.Page
	eventually(t, 15*time.Second, "the recovery to end", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return r.f.ConnectionCount() == 2 && !pg.recovering
	})
	if st := pg.ConnState(); st != ConnOK {
		t.Fatalf("page state after recovery = %s, want attached", st)
	}
	st := r.m.Status()
	if len(st) != 1 || st[0].Session != "s1" || st[0].Lease != "active" {
		t.Fatalf("status %+v, want s1's lease active", st)
	}
}

// Design 2.10: a lease released while recovery dials is not given the new
// connection; the recovery closes it.
func TestReleaseDuringRecoveryDropsTheNewConnection(t *testing.T) {
	r := newRig(t, fakewebkit.Options{TargetDelay: 600 * time.Millisecond}, false, ManagerOptions{Session: webkit.SessionOptions{TargetWait: 2 * time.Second}})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 3*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.f.StallConnection(1)
	_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
	r.m.End(mo.ID)
	pg := mo.Page
	eventually(t, 10*time.Second, "the recovery to dial", func() bool { return r.f.ConnectionCount() == 2 })
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", pg.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	eventually(t, 10*time.Second, "the recovery to end", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return !pg.recovering
	})
	if st := pg.ConnState(); st == ConnOK {
		t.Fatal("the recovery installed its connection after the lease was released")
	}
}

// Design 2.10: protocol retirement and idle exit see every lease, also
// the lease of a lost page.
func TestActiveLeasesCountsLostLeases(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "a", ClassRead)
	if err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	r.f.CloseConnections()
	eventually(t, 3*time.Second, "page lost", func() bool { return mo.Page.ConnState() == ConnLost })
	if n := r.m.ActiveLeases(); n != 1 {
		t.Fatalf("ActiveLeases = %d with a lost lease, want 1", n)
	}
}

// rawSeq runs legacy calls in one operation of session s1.
func rawSeq(t *testing.T, r *rig, ctx context.Context, calls ...[2]string) *ManagedOp {
	t.Helper()
	mo, err := r.m.Begin(ctx, r.req, agent, "legacy", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		var p json.RawMessage
		if c[1] != "" {
			p = json.RawMessage(c[1])
		}
		if _, err := r.m.Raw(ctx, mo.ID, c[0], p); err != nil {
			t.Fatalf("%s: %v", c[0], err)
		}
	}
	return mo
}

func lastParams(t *testing.T, f *fakewebkit.Server, method string) string {
	t.Helper()
	recs := f.ReceivedMethod(method)
	if len(recs) == 0 {
		t.Fatalf("the fake never received %s", method)
	}
	return string(recs[len(recs)-1].Params)
}

// methodsAfter returns the calls of method that f received after the
// first call of first. A call of method before it is the reset that the
// session sends before the first enable of an agent it does not know
// (webkit.SessionOptions.AgentReset).
func methodsAfter(f *fakewebkit.Server, first, method string) []fakewebkit.Record {
	var out []fakewebkit.Record
	seen := false
	for _, rec := range f.Received() {
		seen = seen || rec.Method == first
		if seen && rec.Method == method {
			out = append(out, rec)
		}
	}
	return out
}

// Design 2.4: a forced pseudo state is undone when its owner releases.
func TestReleaseClearsForcedPseudoState(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo := rawSeq(t, r, ctx, [2]string{"CSS.forcePseudoState", `{"nodeId":5,"forcedPseudoClasses":["hover"]}`})
	r.m.End(mo.ID)
	if err := r.m.Release(ctx, "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	recs := r.f.ReceivedMethod("CSS.forcePseudoState")
	if len(recs) != 2 {
		t.Fatalf("CSS.forcePseudoState sent %d times, want 2 (set, then clear on release)", len(recs))
	}
	var p struct {
		NodeID  int      `json:"nodeId"`
		Classes []string `json:"forcedPseudoClasses"`
	}
	if err := json.Unmarshal(recs[1].Params, &p); err != nil || p.NodeID != 5 || p.Classes == nil || len(p.Classes) != 0 {
		t.Fatalf("release sent %s, want nodeId 5 with an empty list", recs[1].Params)
	}
}

// Design 2.4: interception stays enabled while another owner needs it; a
// legacy "disable" is not forwarded then.
func TestInterceptionDisableKeptForAnotherOwner(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo := rawSeq(t, r, ctx)
	ps := mo.Page.PS
	route := SessionOwner("route", "s1")
	ps.State.Acquire(route, "Network")
	ps.State.AddInterception(route, InterceptRule{URL: "api", Stage: "request"})
	for _, c := range [][2]string{{"Network.setInterceptionEnabled", `{"enabled":true}`}, {"Network.setInterceptionEnabled", `{"enabled":false}`}} {
		if _, err := r.m.Raw(ctx, mo.ID, c[0], json.RawMessage(c[1])); err != nil {
			t.Fatal(err)
		}
	}
	r.m.End(mo.ID)
	for _, rec := range r.f.ReceivedMethod("Network.setInterceptionEnabled") {
		if strings.Contains(string(rec.Params), "false") {
			t.Fatalf("interception was disabled while the route owner needs it: %s", rec.Params)
		}
	}
}

// Design 2.4 barrier step 4: the reset re-applies the resource-caching
// override, and the last owner's release resets it.
func TestNetworkResetReappliesCachingAndRelease(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo := rawSeq(t, r, ctx,
		[2]string{"Network.enable", ""},
		[2]string{"Network.setResourceCachingDisabled", `{"disabled":true}`},
		[2]string{"Network.setExtraHTTPHeaders", `{"headers":{"X-A":"a"}}`})
	ps := mo.Page.PS
	capture := SessionOwner("netlog", "other")
	ps.State.Acquire(capture, "Network")
	mark := len(r.f.Received())
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{}}`)); err != nil {
		t.Fatal(err)
	}
	var after []string
	for _, rec := range r.f.Received()[mark:] {
		after = append(after, rec.Method+" "+string(rec.Params))
	}
	joined := strings.Join(after, "\n")
	if !strings.Contains(joined, "Network.enable") || !strings.Contains(joined, `Network.setResourceCachingDisabled {"disabled":true}`) {
		t.Fatalf("the reset did not re-apply the caching override:\n%s", joined)
	}
	r.m.End(mo.ID)
	if err := r.m.Release(ctx, "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if got := lastParams(t, r.f, "Network.setResourceCachingDisabled"); got != `{"disabled":false}` {
		t.Fatalf("release left the caching override: last params %s", got)
	}
	if n := len(methodsAfter(r.f, "Network.enable", "Network.disable")); n != 1 {
		t.Errorf("Network.disable sent %d times, want 1 (the reset's; the capture keeps Network)", n)
	}
}

// Design 2.4 and 2.9: a legacy bootstrap script is one part of the joined
// script, and its owner's release re-sends the script without it.
func TestRawBootstrapScriptIsAPart(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo := rawSeq(t, r, ctx)
	ps := mo.Page.PS
	if err := ps.State.SetPart(SessionOwner("runtime", "other"), "runtime", RankRuntime, "/*runtime*/"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Page.setBootstrapScript", json.RawMessage(`{"source":"/*legacy*/"}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	got := lastParams(t, r.f, "Page.setBootstrapScript")
	if !strings.Contains(got, "/*runtime*/") || !strings.Contains(got, "/*legacy*/") {
		t.Fatalf("the sent script %s does not join both parts", got)
	}
	if err := r.m.Release(ctx, "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	got = lastParams(t, r.f, "Page.setBootstrapScript")
	if !strings.Contains(got, "/*runtime*/") || strings.Contains(got, "/*legacy*/") {
		t.Fatalf("after the release the script is %s, want only the runtime part", got)
	}
}
