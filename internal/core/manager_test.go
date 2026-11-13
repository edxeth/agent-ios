package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// rig is one fake device behind a fake iwdp listing, a reservation
// directory and a manager.
type rig struct {
	f   *fakewebkit.Server
	l   *fakewebkit.Listing
	dev *fakewebkit.ListedDevice
	pg  *fakewebkit.ListedPage
	res *Reservations
	m   *Manager
	req PageRequest
}

func newRig(t *testing.T, fo fakewebkit.Options, frontend bool, mo ManagerOptions) *rig {
	t.Helper()
	r := &rig{f: fakewebkit.New(fo), l: fakewebkit.NewListing(frontend)}
	t.Cleanup(r.f.Close)
	t.Cleanup(r.l.Close)
	r.dev = r.l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	r.pg = r.dev.AddPage("Fixture", "https://fixture.example/?token=secret", r.f.URL)
	r.res = &Reservations{Dir: t.TempDir()}
	mo.Reservations = r.res
	if mo.Transport == nil {
		mo.Transport = func(_ context.Context, kind, addr string) (transport.Transport, error) {
			return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
		}
	}
	if mo.Session.TargetWait == 0 {
		mo.Session.TargetWait = time.Second
	}
	r.m = NewManager(mo)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.m.Close(ctx)
	})
	r.req = PageRequest{Transport: "iwdp", Addr: r.l.Addr()}
	return r
}

func (r *rig) reserve(t *testing.T, owner string) {
	t.Helper()
	if _, err := r.res.Reserve(r.dev.ID, owner, time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func exitCode(err error) int {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

func eventually(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("%s: not within %v", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func evalsOf(f *fakewebkit.Server, expr string) int {
	n := 0
	for _, rec := range f.ReceivedMethod("Runtime.evaluate") {
		var p struct{ Expression string }
		_ = json.Unmarshal(rec.Params, &p)
		if p.Expression == expr {
			n++
		}
	}
	return n
}

var agent = AcquireOptions{Session: "s1", Reservation: "agent"}

// 7.3 test 3 [W1b]: a stalled call with a dead probe returns at the
// command deadline; the page becomes stalled; the call was sent once; the
// background recovery reconnects once while the lease is valid.
func TestDeadProbeStallsThenRecoversOnce(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	r.f.StallConnection(1)
	start := time.Now()
	_, err = r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 1200*time.Millisecond {
		t.Fatalf("Raw = %v after %v, want the deadline within 1.2 s", err, time.Since(start))
	}
	r.m.End(mo.ID)
	pg := mo.Page
	eventually(t, 3*time.Second, "page stalled", func() bool { return pg.ConnState() == ConnStalled || r.f.ConnectionCount() == 2 })
	eventually(t, 15*time.Second, "background recovery", func() bool { return pg.ConnState() == ConnOK && r.f.ConnectionCount() == 2 })
	if n := evalsOf(r.f, "document.title"); n != 1 {
		t.Errorf("the fake received %d evaluations of the command, want exactly 1 (no retry)", n)
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.f.ConnectionCount(); n != 2 {
		t.Errorf("connections = %d, want 2 (one reconnect)", n)
	}
}

// 7.3 test 3 [W1b]: with a live probe the call ends with ErrCallTimeout at
// its budget, and the page stays attached.
func TestLiveProbeGivesCallTimeout(t *testing.T) {
	// The session keeps its default call budget, so the attach and its
	// replay of the desired state are bounded only by the deadline. Only
	// the stalled call gets the 300 ms budget (webkit.WithCallBudget); its
	// probe inherits it (Session.Probe).
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	r.f.Stall("DOM.getDocument")
	ctx := ctxFor(t, 3*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "read", ClassRead)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer r.m.End(mo.ID)
	start := time.Now()
	_, err = mo.Op.Caller().Send(webkit.WithCallBudget(ctx, 300*time.Millisecond), "DOM.getDocument", nil)
	if !errors.Is(err, webkit.ErrCallTimeout) || errors.Is(err, webkit.ErrSessionStalled) {
		t.Fatalf("Send = %v, want ErrCallTimeout", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Errorf("returned after %v, want about the 300 ms call budget", d)
	}
	if st := mo.Page.ConnState(); st != ConnOK {
		t.Errorf("state = %v, want attached", st)
	}
}

// 7.3 test 3 [W1b]: a RetrySafe read is retried once after the reconnect,
// inside its deadline; a read that is not retry-safe is not.
func TestRetrySafeReadRetriedOnceAfterReconnect(t *testing.T) {
	for _, retrySafe := range []bool{true, false} {
		// The session keeps its default call budget, so the reconnect's
		// replay of the desired state and the retried read are bounded only
		// by the 8 s deadline. Only the read on the stalled connection gets
		// a short budget (webkit.WithCallBudget), so that it fails with
		// ErrCallTimeout and its probe fails fast.
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
		r.reserve(t, "agent")
		ctx := ctxFor(t, 8*time.Second)
		if err := r.m.Do(ctx, r.req, agent, "attach", ClassRead, false, func(context.Context, *ManagedOp) error { return nil }); err != nil {
			t.Fatalf("attach: %v", err)
		}
		r.f.StallConnection(1)
		runs := 0
		err := r.m.Do(ctx, r.req, agent, "get title", ClassRead, retrySafe, func(ctx context.Context, mo *ManagedOp) error {
			if runs++; runs == 1 {
				ctx = webkit.WithCallBudget(ctx, 300*time.Millisecond)
			}
			_, err := mo.Op.Caller().Send(ctx, "DOM.getDocument", nil)
			return err
		})
		calls := len(r.f.ReceivedMethod("DOM.getDocument"))
		if retrySafe {
			if err != nil || calls != 2 {
				t.Fatalf("retry-safe read: err %v, %d calls; want success after exactly one retry", err, calls)
			}
		} else if !errors.Is(err, webkit.ErrSessionStalled) || calls != 1 {
			t.Fatalf("plain read: err %v, %d calls; want ErrSessionStalled and no retry", err, calls)
		}
	}
}

// stealPage opens a second inspector connection, as another client would.
func stealPage(t *testing.T, f *fakewebkit.Server) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(f.URL, nil)
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
}

// 7.3 test 7 (d) [W1b] and test 3: a page taken by another client moves
// to lost, is never reconnected automatically, refuses the next command
// with exit 4, and --takeover reattaches only with the reservation and
// re-applies the desired state.
func TestStealLostAndTakeover(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lease lost", func() bool { return pg.ConnState() == ConnLost })
	time.Sleep(300 * time.Millisecond)
	if n := r.f.ConnectionCount(); n != 2 {
		t.Fatalf("connections = %d, want 2 (no automatic reconnect)", n)
	}
	if _, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive); exitCode(err) != CodeBusy || !strings.Contains(err.Error(), "ownership is unknown") {
		t.Fatalf("command after the steal: %v (exit %d), want exit 4 'ownership is unknown'", err, exitCode(err))
	}
	other := AcquireOptions{Session: "s1", Reservation: "nobody", Takeover: true}
	if _, err := r.m.Begin(ctx, r.req, other, "eval", ClassExclusive); exitCode(err) != CodeReservation {
		t.Fatalf("--takeover without the reservation: %v (exit %d), want exit 6", err, exitCode(err))
	}
	before := len(r.f.ReceivedMethod("Network.enable"))
	take := agent
	take.Takeover = true
	mo2, err := r.m.Begin(ctx, r.req, take, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("--takeover: %v", err)
	}
	r.m.End(mo2.ID)
	if mo2.Epoch <= mo.Epoch {
		t.Errorf("epoch after takeover = %d, want above %d", mo2.Epoch, mo.Epoch)
	}
	if n := r.f.ConnectionCount(); n != 3 {
		t.Errorf("connections = %d, want 3", n)
	}
	if after := len(r.f.ReceivedMethod("Network.enable")); after != before+1 {
		t.Errorf("Network.enable sent %d times after takeover, want the desired state re-applied once", after-before)
	}
}

// 7.3 test 7 (a)-(c) [W1b]: the ownership preflight before the first
// attach. Expected codes come from design 2.5 and 2.10.
func TestPreflight(t *testing.T) {
	t.Run("busy hint refuses with exit 4 and no dial", func(t *testing.T) {
		r := newRig(t, fakewebkit.Options{}, true, ManagerOptions{})
		r.reserve(t, "agent")
		r.dev.AddPage("Other", "https://other.example/", "ws://127.0.0.1:1/x") // keeps the frontend hint on
		r.dev.SetBusy(r.pg, true)
		_, err := r.m.Begin(ctxFor(t, 3*time.Second), PageRequest{Transport: "iwdp", Addr: r.l.Addr(), Page: "Fixture"}, agent, "eval", ClassExclusive)
		if exitCode(err) != CodeBusy || !errors.Is(err, ErrPageBusy) {
			t.Fatalf("busy page: %v (exit %d), want exit 4", err, exitCode(err))
		}
		if n := r.f.ConnectionCount(); n != 0 {
			t.Fatalf("connections = %d, want no WebSocket dial", n)
		}
	})
	t.Run("another owner's reservation refuses with exit 6", func(t *testing.T) {
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
		r.reserve(t, "someone-else")
		_, err := r.m.Begin(ctxFor(t, 3*time.Second), r.req, agent, "eval", ClassExclusive)
		if exitCode(err) != CodeReservation || !strings.Contains(err.Error(), "someone-else") {
			t.Fatalf("reserved by another owner: %v (exit %d), want exit 6", err, exitCode(err))
		}
		if n := r.f.ConnectionCount(); n != 0 {
			t.Fatalf("connections = %d, want no dial", n)
		}
	})
	t.Run("unknown ownership needs a reservation", func(t *testing.T) {
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
		if _, err := r.m.Begin(ctxFor(t, 3*time.Second), r.req, agent, "eval", ClassExclusive); exitCode(err) != CodeReservation {
			t.Fatalf("no reservation: %v (exit %d), want exit 6", err, exitCode(err))
		}
		r.reserve(t, "agent")
		mo, err := r.m.Begin(ctxFor(t, 3*time.Second), r.req, agent, "eval", ClassExclusive)
		if err != nil {
			t.Fatalf("with the reservation: %v", err)
		}
		r.m.End(mo.ID)
	})
}

// 7.3 test 8 [W1b]: leases between sessions.
func TestLeases(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	s2 := AcquireOptions{Session: "s2", Reservation: "agent"}
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("s1: %v", err)
	}
	r.m.End(mo.ID)
	start := time.Now()
	_, err = r.m.Begin(ctx, r.req, s2, "eval", ClassExclusive)
	if exitCode(err) != CodeBusy || !strings.Contains(err.Error(), `is leased by session "s1"`) || !strings.Contains(err.Error(), "lease break") || time.Since(start) > time.Second {
		t.Fatalf("second session: %v (exit %d) after %v, want the lease error at once", err, exitCode(err), time.Since(start))
	}
	// close in s2 does not touch s1's lease.
	if err := r.m.Release(ctx, "s2", mo.Page.Key); err != nil {
		t.Fatalf("Release s2: %v", err)
	}
	if st := r.m.Status(); len(st) != 1 || st[0].Session != "s1" || st[0].Lease != "active" {
		t.Fatalf("status after s2's close = %+v, want s1's lease unchanged", st)
	}
	// --wait-lease longer than the command deadline ends at the deadline (exit 124, D9).
	short := ctxFor(t, 300*time.Millisecond)
	wl := s2
	wl.WaitLease = 5 * time.Second
	start = time.Now()
	if _, err := r.m.Begin(short, r.req, wl, "eval", ClassExclusive); exitCode(err) != CodeTimeout || time.Since(start) > time.Second {
		t.Fatalf("wait past the deadline: %v (exit %d) after %v, want exit 124 at the deadline", err, exitCode(err), time.Since(start))
	}
	// --wait-lease 2s succeeds after the release.
	wl.WaitLease = 2 * time.Second
	got := make(chan error, 1)
	go func() {
		mo, err := r.m.Begin(ctx, r.req, wl, "eval", ClassExclusive)
		if err == nil {
			r.m.End(mo.ID)
		}
		got <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := r.m.Release(ctx, "s1", mo.Page.Key); err != nil {
		t.Fatalf("Release s1: %v", err)
	}
	if err := <-got; err != nil {
		t.Fatalf("--wait-lease after the release: %v", err)
	}
}

// 7.3 test 8 [W1b]: idle TTL expiry releases the lease and runs cleanup.
func TestLeaseIdleTTLRunsCleanup(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{LeaseTTL: 300 * time.Millisecond, SweepEvery: 50 * time.Millisecond})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "route", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, c := range []struct{ m, p string }{
		{"Network.enable", ""},
		{"Network.setInterceptionEnabled", `{"enabled":true}`},
		{"Network.addInterception", `{"url":"api","stage":"request","caseSensitive":false,"isRegex":false}`},
		{"Debugger.enable", ""},
	} {
		if _, err := r.m.Raw(ctx, mo.ID, c.m, json.RawMessage(c.p)); err != nil {
			t.Fatalf("%s: %v", c.m, err)
		}
	}
	r.m.End(mo.ID)
	r.f.Emit("Debugger.paused", map[string]any{"callFrames": []any{}, "reason": "other"})
	eventually(t, 2*time.Second, "pause recorded", func() bool { return mo.Page.PS.Sched.pauseOpen() })
	eventually(t, 3*time.Second, "lease expiry", func() bool { st := r.m.Status(); return len(st) == 1 && st[0].Lease == "none" })
	for _, want := range []string{"Network.setInterceptionEnabled", "Debugger.resume", "Network.disable", "Debugger.disable"} {
		recs := r.f.ReceivedMethod(want)
		if len(recs) == 0 {
			t.Errorf("cleanup did not send %s", want)
		}
	}
	var last struct{ Enabled bool }
	recs := r.f.ReceivedMethod("Network.setInterceptionEnabled")
	_ = json.Unmarshal(recs[len(recs)-1].Params, &last)
	if last.Enabled {
		t.Error("the last Network.setInterceptionEnabled did not disable interception")
	}
}

// 7.3 test 9 [W1b]: mixed entry points on one page.
func TestMixedEntryPoints(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ps := mo.Page.PS
	capture := SessionOwner("netlog", "s1-capture")
	if ps.State.Acquire(capture, "Network") {
		if _, err := mo.Op.Caller().Send(ctx, "Network.enable", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatalf("legacy Network.enable: %v", err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.disable", nil); err != nil {
		t.Fatalf("legacy Network.disable: %v", err)
	}
	if n := len(r.f.ReceivedMethod("Network.enable")); n != 1 {
		t.Errorf("Network.enable sent %d times, want 1 (the capture's)", n)
	}
	if n := len(methodsAfter(r.f, "Network.enable", "Network.disable")); n != 0 {
		t.Fatalf("Network.disable sent while the capture owns Network")
	}
	// Refused before sending: not in the iOS 16.7 list, and the CSS hang methods.
	for _, m := range []string{"DOM.getMediaStats", "CSS.getAllStyleSheets", "CSS.enable", "CSS.getStyleSheetText"} {
		if _, err := r.m.Raw(ctx, mo.ID, m, nil); err == nil {
			t.Errorf("%s was not refused", m)
		}
		if n := len(r.f.ReceivedMethod(m)); n != 0 {
			t.Errorf("the fake received %s", m)
		}
	}
	// A legacy operation is exclusive: a concurrent read waits for it.
	admitted := make(chan time.Time, 1)
	go func() {
		rd, err := r.m.Begin(ctx, r.req, agent, "read", ClassRead)
		if err == nil {
			admitted <- time.Now()
			r.m.End(rd.ID)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	ended := time.Now()
	r.m.End(mo.ID)
	select {
	case at := <-admitted:
		if at.Before(ended) {
			t.Error("a read ran while the legacy operation held the page")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read was never admitted")
	}
	// The last owner's release sends Network.disable.
	if _, err := ps.State.Release(ctx, capture, ps.Caller); err != nil {
		t.Fatal(err)
	}
	if n := len(methodsAfter(r.f, "Network.enable", "Network.disable")); n != 1 {
		t.Errorf("Network.disable sent %d times after the last owner's release, want 1", n)
	}
	// Release and reacquire while an old operation runs: its later sends
	// are rejected by epoch and never reach the fake.
	old, err := r.m.Begin(ctx, r.req, agent, "old", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.m.Break(ctx, old.Page.Key); err != nil {
			t.Errorf("Break: %v", err)
		}
	}()
	wg.Wait()
	fresh, err := r.m.Begin(ctx, r.req, agent, "new", ClassExclusive)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	defer r.m.End(fresh.ID)
	if fresh.Epoch == old.Epoch {
		t.Fatal("the epoch did not change")
	}
	if _, err := r.m.Raw(ctx, old.ID, "Page.reload", nil); err == nil {
		t.Fatal("the old operation could still send")
	}
	r.m.End(old.ID)
	if n := len(r.f.ReceivedMethod("Page.reload")); n != 0 {
		t.Errorf("the fake received %d sends of the old operation", n)
	}
}

// 7.3 test 17 [W1b]: the Network reinitialization barrier (design 2.4).
func TestNetworkReinitializationBarrier(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	a := AcquireOptions{Session: "a", Reservation: "agent"}
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, a, "headers", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ps := mo.Page.PS
	c := mo.Op.Caller()
	// Owner B: a capture with one header and one interception rule.
	b := SessionOwner("netlog", "b")
	rule := InterceptRule{URL: "api", Stage: "request"}
	var gaps []NetworkGap
	var gmu sync.Mutex
	ps.State.WatchNetworkReset(b, func(g NetworkGap) { gmu.Lock(); gaps = append(gaps, g); gmu.Unlock() })
	ps.State.Acquire(b, "Network")
	ps.State.SetHeaders(b, map[string]string{"X-B": "b"})
	ps.State.AddInterception(b, rule)
	for _, s := range []struct {
		m string
		p any
	}{{"Network.enable", nil}, {"Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-B": "b"}}}, {"Network.setInterceptionEnabled", map[string]bool{"enabled": true}}, {"Network.addInterception", rule}} {
		if _, err := c.Send(ctx, s.m, s.p); err != nil {
			t.Fatal(err)
		}
	}
	// Owner A: the legacy command of session a.
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-A":"a"}}`)); err != nil {
		t.Fatal(err)
	}
	r.f.Emit("Network.requestIntercepted", map[string]any{"requestId": "r1"})
	eventually(t, 2*time.Second, "held request", func() bool { return len(ps.Sched.PendingRequests()) == 1 })
	mark := len(r.f.Received())
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{}}`)); err != nil {
		t.Fatalf("A removes its headers: %v", err)
	}
	var seq []string
	for _, rec := range r.f.Received()[mark:] {
		seq = append(seq, rec.Method+" "+string(rec.Params))
	}
	want := []string{
		"Network.interceptContinue",
		"Network.disable",
		"Network.enable",
		`Network.setExtraHTTPHeaders {"headers":{"X-B":"b"}}`,
		"Network.setInterceptionEnabled",
		"Network.addInterception",
	}
	if len(seq) != len(want) {
		t.Fatalf("barrier sequence = %q, want %q", seq, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(seq[i], w) {
			t.Fatalf("barrier message %d = %q, want %q (sequence %q)", i, seq[i], w, seq)
		}
	}
	if !strings.Contains(seq[0], `"requestId":"r1"`) {
		t.Errorf("the continue was not for r1: %s", seq[0])
	}
	for _, rec := range r.f.ReceivedMethod("Network.setExtraHTTPHeaders") {
		if strings.Contains(string(rec.Params), `"X-A":""`) {
			t.Fatal("the fake received X-A with an empty value")
		}
	}
	gmu.Lock()
	if len(gaps) != 1 || len(gaps[0].Requests) != 1 || gaps[0].End.Before(gaps[0].Start) {
		t.Errorf("gap markers = %+v, want one for B with r1", gaps)
	}
	gmu.Unlock()
	_, err = ps.Sched.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: "a", Epoch: mo.Epoch, RequestID: "r1", Method: "Network.interceptContinue"})
	if err == nil || !strings.Contains(err.Error(), "request was released by a network reset") {
		t.Fatalf("late completion = %v, want 'request was released by a network reset'", err)
	}
	// A new header name, and a changed value, go out without the barrier.
	mark = len(r.f.Received())
	for _, h := range []string{`{"headers":{"X-C":"c"}}`, `{"headers":{"X-C":"d"}}`} {
		if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(h)); err != nil {
			t.Fatal(err)
		}
	}
	for _, rec := range r.f.Received()[mark:] {
		if rec.Method != "Network.setExtraHTTPHeaders" {
			t.Errorf("unexpected %s for a new or changed header", rec.Method)
		}
	}
	r.m.End(mo.ID)
}
