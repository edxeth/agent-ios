package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for a new connection whose setup fails after its
// Network.disable was written (docs/notes/w1b.md, "Review repairs, closure:
// failed setup and the reconnect gap"). WebKit's Network.disable continues
// every pending intercept (interceptBackend), so the remaining Network
// owners must get the gap even though the setup fails, and a later new
// connection must continue it without a hole and without the same
// requests again. The gap starts when the daemon retired the old
// connection.

// gapRecorder records the gaps reported to one owner.
type gapRecorder struct {
	mu   sync.Mutex
	gaps []NetworkGap
}

func watchGaps(t *testing.T, pg *Page, owner Owner) *gapRecorder {
	t.Helper()
	g := &gapRecorder{}
	t.Cleanup(pg.PS.State.WatchNetworkReset(owner, func(x NetworkGap) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.gaps = append(g.gaps, x)
	}))
	return g
}

func (g *gapRecorder) get() []NetworkGap {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]NetworkGap(nil), g.gaps...)
}

// capturePage enables Network as the legacy owner of agent's session and
// holds r1 in the backend and the scheduler.
func capturePage(t *testing.T, r *rig, ctx context.Context) (*Page, *interceptBackend, *gapRecorder) {
	t.Helper()
	b := newInterceptBackend(r.f)
	mo, err := r.m.Begin(ctx, r.req, agent, "capture", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	pg := mo.Page
	r.m.End(mo.ID)
	g := watchGaps(t, pg, SessionOwner("legacy", agent.Session))
	b.hold(r.f, "r1")
	eventually(t, 3*time.Second, "held r1", func() bool { return len(pg.PS.Sched.PendingRequests()) == 1 })
	return pg, b, g
}

// failBootstrapScript makes the page refuse Page.setBootstrapScript while
// fail is set. Apply sends it in the pipelined setup batch after the
// Network.disable of the Network reset (resetCalls).
func failBootstrapScript(f *fakewebkit.Server) *atomic.Bool {
	fail := &atomic.Bool{}
	fail.Store(true)
	f.Handle("Page.setBootstrapScript", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if fail.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "setup failure after Network.disable (test)"}
		}
		return nil, nil
	})
	return fail
}

// checkAdjacent checks the gaps of a failed setup and its successful retry:
// the first holds r1 and starts at or after before, the second starts where
// the first ends and repeats no request.
func checkAdjacent(t *testing.T, gaps []NetworkGap, before, failedEnd time.Time) {
	t.Helper()
	if len(gaps) != 2 {
		t.Fatalf("gaps after the failed setup and the retry: %+v, want two", gaps)
	}
	if g := gaps[0]; !reflect.DeepEqual(g.Requests, []string{"r1"}) || g.Start.Before(before) || g.End.Before(g.Start) || g.End.After(failedEnd) {
		t.Errorf("gap of the failed setup %+v, want requests [r1] and %v <= start <= end <= %v", g, before, failedEnd)
	}
	if g := gaps[1]; !g.Start.Equal(gaps[0].End) || g.End.Before(g.Start) || len(g.Requests) != 0 {
		t.Errorf("gap of the retry %+v, want start %v (the end of the first gap) and no requests", g, gaps[0].End)
	}
}

// The closure reviewer's reproduction (w1b-closure4, failure-gap.go): a
// takeover whose setup fails after the pipelined Network.disable reports
// the gap with the held request; the successful retry continues it.
func TestFailedTakeoverSetupReportsTheNetworkGap(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, b, g := capturePage(t, r, ctx)
	fail := failBootstrapScript(r.f)
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lost page", func() bool { return pg.ConnState() == ConnLost })
	take := agent
	take.Takeover = true
	before := time.Now()
	if _, err := r.m.Begin(ctx, r.req, take, "takeover with a failed setup", ClassExclusive); err == nil {
		t.Fatal("takeover succeeded, want the setup failure")
	}
	failedEnd := time.Now()
	if n := b.held(); n != 0 {
		t.Fatalf("the backend still holds %d requests: the setup did not reset Network", n)
	}
	if p := pg.PS.Sched.PendingRequests(); len(p) != 0 {
		t.Errorf("held requests after the backend's Network reset: %v, want none", p)
	}
	if gaps := g.get(); len(gaps) != 1 {
		t.Fatalf("gaps after the failed setup: %+v, want one", gaps)
	}
	fail.Store(false)
	mo, err := r.m.Begin(ctx, r.req, take, "takeover retry", ClassExclusive)
	if err != nil {
		t.Fatalf("takeover retry: %v", err)
	}
	r.m.End(mo.ID)
	checkAdjacent(t, g.get(), before, failedEnd)
}

// Background recovery whose setup fails after the Network.disable reports
// the gap; the reopen that follows continues it.
func TestFailedRecoverySetupReportsTheNetworkGap(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 20*time.Second)
	pg, b, g := capturePage(t, r, ctx)
	fail := failBootstrapScript(r.f)
	mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	r.f.StallConnection(1)
	before := time.Now()
	_, _ = r.m.Raw(ctxFor(t, time.Second), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
	r.m.End(mo.ID)
	eventually(t, 15*time.Second, "the failed recovery", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return r.f.ConnectionCount() == 2 && !pg.recovering
	})
	failedEnd := time.Now()
	if st := pg.ConnState(); st != ConnStalled {
		t.Fatalf("page state after the failed recovery: %v, want stalled", st)
	}
	if n := b.held(); n != 0 {
		t.Fatalf("the backend still holds %d requests: the setup did not reset Network", n)
	}
	if gaps := g.get(); len(gaps) != 1 {
		t.Fatalf("gaps after the failed recovery: %+v, want one", gaps)
	}
	fail.Store(false)
	mo, err = r.m.Begin(ctx, r.req, agent, "reopen", ClassExclusive)
	if err != nil {
		t.Fatalf("reopen after the failed recovery: %v", err)
	}
	r.m.End(mo.ID)
	checkAdjacent(t, g.get(), before, failedEnd)
}

// A setup that fails before it writes the Network.disable (here the
// liveness probe of a physical page, which runs before Apply) reports no
// gap and keeps the held request; the successful retry reports one gap
// from the first retirement.
func TestSetupWithoutNetworkDisableReportsNoGap(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, b, g := capturePage(t, r, ctx)
	refuseProbe := &atomic.Bool{}
	refuseProbe.Store(true)
	r.f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if refuseProbe.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "probe refused (test)"}
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lost page", func() bool { return pg.ConnState() == ConnLost })
	disables := len(r.f.ReceivedMethod("Network.disable"))
	take := agent
	take.Takeover = true
	before := time.Now()
	if _, err := r.m.Begin(ctx, r.req, take, "takeover with a failed probe", ClassExclusive); err == nil {
		t.Fatal("takeover succeeded, want the probe failure")
	}
	failedEnd := time.Now()
	if n := len(r.f.ReceivedMethod("Network.disable")); n != disables {
		t.Fatalf("the failed setup sent %d Network.disable, want none", n-disables)
	}
	if gaps := g.get(); len(gaps) != 0 {
		t.Errorf("gaps after a setup that never sent Network.disable: %+v, want none", gaps)
	}
	if p := pg.PS.Sched.PendingRequests(); !reflect.DeepEqual(p, []string{"r1"}) || b.held() != 1 {
		t.Errorf("held requests after the failed probe: scheduler %v, backend %d; want r1 in both", p, b.held())
	}
	refuseProbe.Store(false)
	mo, err := r.m.Begin(ctx, r.req, take, "takeover retry", ClassExclusive)
	if err != nil {
		t.Fatalf("takeover retry: %v", err)
	}
	r.m.End(mo.ID)
	gaps := g.get()
	if len(gaps) != 1 {
		t.Fatalf("gaps after the retry: %+v, want one", gaps)
	}
	if x := gaps[0]; !reflect.DeepEqual(x.Requests, []string{"r1"}) || x.Start.Before(before) || x.Start.After(failedEnd) || x.End.Before(failedEnd) {
		t.Errorf("gap %+v, want requests [r1], start in [%v, %v] (the first retirement) and end after it", x, before, failedEnd)
	}
}

// scriptedConn answers the methods in errs with that error and sends the
// others to the page.
type scriptedConn struct {
	wkConn
	errs map[string]error
}

func (c scriptedConn) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err, ok := c.errs[method]; ok {
		return nil, err
	}
	return c.wkConn.Send(ctx, method, params)
}

// A failed Apply says whether its Network.disable may have been written
// (networkDisableWritten): only an error that proves the call was never
// sent (webkit.ErrNotSent) means the Network agent was not reset. The mark
// keeps the failure's chain, which callers read (canceledByOwner, noEffect).
func TestApplyErrorSaysWhetherNetworkDisableWasWritten(t *testing.T) {
	refused := &webkit.ErrorData{Code: -32000, Message: "refused by the test"}
	notSent := errors.Join(webkit.ErrNotSent, errors.New("connection closed before the write (test)"))
	for _, tc := range []struct {
		name    string
		errs    map[string]error
		cause   error // the failure that Apply's error must still carry
		written bool
	}{
		{"disable answered, later call refused", map[string]error{"Page.setBootstrapScript": refused}, refused, true},
		{"disable refused as not enabled, later call refused", map[string]error{"Network.disable": &webkit.ErrorData{Code: -32000, Message: "Network domain must be enabled"}, "Page.setBootstrapScript": refused}, refused, true},
		{"disable without an answer", map[string]error{"Network.disable": context.DeadlineExceeded}, context.DeadlineExceeded, true},
		{"disable never sent", map[string]error{"Network.disable": notSent, "Page.disable": notSent, "Page.setBootstrapScript": notSent, "Network.enable": notSent}, webkit.ErrNotSent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A backend without a record: Apply resets Network and Page.
			c := wkConn{&wkPage{active: "page-0", live: map[string]*wkTarget{"page-0": {bps: map[string]string{}}}, gone: map[string]error{}}}
			d := NewDesiredState()
			d.Acquire("o", "Network")
			err := d.Apply(context.Background(), scriptedConn{c, tc.errs})
			if err == nil {
				t.Fatal("Apply succeeded, want its failure")
			}
			if got := networkDisableWritten(err); got != tc.written {
				t.Errorf("networkDisableWritten(%v) = %v, want %v", err, got, tc.written)
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("Apply's error %v lost its cause %v", err, tc.cause)
			}
			// openConn's wrapping keeps the answer.
			wrapped := &CodedError{Code: CodeBusy, Err: fmt.Errorf("re-applying the page state: %w", err)}
			if got := networkDisableWritten(wrapped); got != tc.written {
				t.Errorf("networkDisableWritten(%v) = %v, want %v", wrapped, got, tc.written)
			}
		})
	}
}
