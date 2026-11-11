package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Design 3.1 install path 2: the runtime part of the bootstrap script
// "needs Page.enable in the session, which the daemon keeps", and design
// 2.4 counts domain owners: "the disable command is sent only when the
// count reaches zero". The runtime's bootstrap part is a requirement of
// Page like an owner (DesiredState.Needs), so a legacy Page.disable, a
// legacy owner's own enable and disable, and the release of a session
// whose legacy client enabled Page never disable Page.
func TestPageRuntimeKeepsThePageDomainEnabled(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "legacy", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ps := mo.Page.PS
	// Count from here: the attach's setup resets the Page agent with a
	// Page.disable before it enables Page (resetCalls).
	mark := lastSeq(r.f)
	// A legacy client that never enabled Page disables it.
	if _, err := r.m.Raw(ctx, mo.ID, "Page.disable", nil); err != nil {
		t.Fatalf("raw Page.disable: %v", err)
	}
	// A legacy client enables Page and disables it again.
	for _, m := range []string{"Page.enable", "Page.disable", "Page.enable"} {
		if _, err := r.m.Raw(ctx, mo.ID, m, nil); err != nil {
			t.Fatalf("raw %s: %v", m, err)
		}
	}
	r.m.End(mo.ID)
	// The session holding the legacy Page owner releases the page.
	beforeRelease := lastSeq(r.f)
	if err := r.m.Release(ctx, agent.Session, mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n := receivedAfter(r.f, "Page.disable", mark); n != 0 {
		t.Fatalf("the page received %d Page.disable while the runtime bootstrap part is installed, want 0", n)
	}
	// The release keeps the runtime part on the page: it sends no
	// bootstrap script, also no empty one that clears the part.
	if n := receivedAfter(r.f, "Page.setBootstrapScript", beforeRelease); n != 0 {
		t.Fatalf("the release sent %d Page.setBootstrapScript (any source), want 0", n)
	}
	if !ps.State.Needs("Page") {
		t.Fatal("Page is not needed after the release, want it needed by the runtime part")
	}
	if got, want := ps.State.AllOwners(), []Owner{Owner(RuntimePart)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("owners after the release = %q, want only the runtime part's %q", got, want)
	}
	if got := ps.Boot.Names(); len(got) != 1 || got[0] != RuntimePart {
		t.Fatalf("bootstrap parts after the release = %q, want [%s]", got, RuntimePart)
	}
}

// releaseRecorder answers the page runtime's release calls and records
// their epochs.
type releaseRecorder struct {
	mu     sync.Mutex
	epochs []float64
	reply  any // the envelope; nil: ok
}

func (p *releaseRecorder) handler(_ fakewebkit.Call, fn string, args json.RawMessage) any {
	if fn != "release" {
		return map[string]any{"ok": true, "value": true}
	}
	var a struct{ Epoch float64 }
	_ = json.Unmarshal(args, &a)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epochs = append(p.epochs, a.Epoch)
	if p.reply != nil {
		return p.reply
	}
	return map[string]any{"ok": true, "value": true}
}

func (p *releaseRecorder) got() []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]float64(nil), p.epochs...)
}

// Design 2.4, row "Page-side runtime state": the owner's page-side state
// is released with runtime __iwdp.release(epoch) when its lease ends,
// fenced by the epoch of that lease. Every way a lease ends runs it: an
// explicit release, the idle expiry and the daemon shutdown.
func TestLeaseEndReleasesThePageRuntime(t *testing.T) {
	ends := map[string]func(r *rig, mo *ManagedOp) error{
		"release": func(r *rig, mo *ManagedOp) error {
			return r.m.Release(ctxFor(t, 5*time.Second), agent.Session, mo.Page.Key)
		},
		"idle expiry": func(r *rig, _ *ManagedOp) error {
			r.m.Sweep(time.Now().Add(DefaultLeaseTTL + time.Minute))
			return nil
		},
		"shutdown": func(r *rig, _ *ManagedOp) error {
			return r.m.Close(ctxFor(t, 5*time.Second))
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
			r.reserve(t, "agent")
			page := &releaseRecorder{}
			r.f.SetRuntime(page.handler)
			mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "get", ClassRead)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			r.m.End(mo.ID)
			if got := page.got(); len(got) != 0 {
				t.Fatalf("release calls before the lease ended: %v", got)
			}
			if err := end(r, mo); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := page.got(); len(got) != 1 || got[0] != float64(mo.Epoch) {
				t.Fatalf("runtime release calls = %v, want one with the lease epoch %d", got, mo.Epoch)
			}
		})
	}
}

// Design 2.4: release cleanup is best effort on the page side. A page
// runtime release that fails or gets no reply does not stop the release:
// the lease ends, the session's desired state is still cleaned up, and the
// release returns within a bounded time. The next owner's first call
// resets the page maps anyway (design 3.1 owner epoch).
func TestFailedPageRuntimeReleaseDoesNotBlockTheRelease(t *testing.T) {
	replies := map[string]any{
		"error envelope": map[string]any{"ok": false, "error": map[string]string{"code": "internal", "message": "boom"}},
		"lost reply":     fakewebkit.NoReply,
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
			r.reserve(t, "agent")
			page := &releaseRecorder{reply: reply}
			r.f.SetRuntime(page.handler)
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			r.m.End(mo.ID)
			// Count from here: the first Network.enable on the target
			// follows the session's reset of the Network agent, a
			// Network.disable (webkit.SessionOptions.AgentReset).
			mark := lastSeq(r.f)
			start := time.Now()
			if err := r.m.Release(ctx, agent.Session, mo.Page.Key); err != nil {
				t.Fatalf("Release with a failing page release: %v, want success (the page release is best effort)", err)
			}
			if d := time.Since(start); d > 4*time.Second {
				t.Fatalf("Release took %v with a failing page release, want it bounded", d)
			}
			if got := page.got(); len(got) != 1 {
				t.Fatalf("runtime release calls = %v, want one", got)
			}
			if n := receivedAfter(r.f, "Network.disable", mark); n != 1 {
				t.Fatalf("Network.disable sent %d times, want 1: the desired state of the session was not cleaned up", n)
			}
			mo.Page.mu.Lock()
			l := mo.Page.lease
			mo.Page.mu.Unlock()
			if l != nil {
				t.Fatalf("the lease of %q stayed after the release", l.session)
			}
			// The page stays usable for the next session.
			next := AcquireOptions{Session: "s2", Reservation: "agent"}
			mo2, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, next, "get", ClassRead)
			if err != nil {
				t.Fatalf("Begin of the next session: %v", err)
			}
			r.m.End(mo2.ID)
		})
	}
}

// Design 2.4: the session's desired-state cleanup is required, the
// page-side runtime release is best effort. A release with a deadline
// shorter than runtimeReleaseBudget cleans up first and does not wait for
// the runtime release beyond its share of the remaining time, so it
// succeeds within its deadline even when the page never answers the
// runtime release. With a responsive runtime the runtime release is sent
// after the cleanup. With very little time left it is skipped.
func TestShortReleaseCleansUpBeforeThePageRuntimeRelease(t *testing.T) {
	cases := []struct {
		name        string
		deadline    time.Duration
		reply       any
		wantRelease bool
	}{
		{"silent runtime", 300 * time.Millisecond, fakewebkit.NoReply, true},
		{"responsive runtime", 300 * time.Millisecond, nil, true},
		// Less than runtimeReleaseReserve + runtimeReleaseMin is left
		// after the cleanup: the runtime release is skipped.
		{"no time for the runtime release", runtimeReleaseReserve + runtimeReleaseMin/2, fakewebkit.NoReply, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
			r.reserve(t, "agent")
			page := &releaseRecorder{reply: tc.reply}
			r.f.SetRuntime(page.handler)
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			r.m.End(mo.ID)
			// Count the cleanup from here, after the reset that preceded
			// the first Network.enable (webkit.SessionOptions.AgentReset).
			mark := lastSeq(r.f)
			rctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
			defer cancel()
			start := time.Now()
			if err := r.m.Release(rctx, agent.Session, mo.Page.Key); err != nil {
				t.Fatalf("Release with a %v deadline: %v, want success", tc.deadline, err)
			}
			if d := time.Since(start); d >= tc.deadline {
				t.Fatalf("Release returned after %v, want within its %v deadline", d, tc.deadline)
			}
			var disables []fakewebkit.Record
			for _, rec := range r.f.ReceivedMethod("Network.disable") {
				if rec.Seq > mark {
					disables = append(disables, rec)
				}
			}
			if len(disables) != 1 {
				t.Fatalf("Network.disable sent %d times, want 1: the session's cleanup did not reach the page", len(disables))
			}
			releases := runtimeReleases(r.f)
			if got := page.got(); (len(got) == 1) != tc.wantRelease || len(got) != len(releases) {
				t.Fatalf("runtime release calls = %v, want sent: %v", got, tc.wantRelease)
			}
			if tc.wantRelease && releases[0].Seq < disables[0].Seq {
				t.Fatalf("the runtime release (seq %d) was sent before the cleanup's Network.disable (seq %d)", releases[0].Seq, disables[0].Seq)
			}
		})
	}
}

// receivedAfter counts the method calls the fake received after the log
// record mark (lastSeq).
func receivedAfter(f *fakewebkit.Server, method string, mark int) int {
	n := 0
	for _, rec := range f.ReceivedMethod(method) {
		if rec.Seq > mark {
			n++
		}
	}
	return n
}

// runtimeReleases returns the runtime release calls the fake received.
func runtimeReleases(f *fakewebkit.Server) []fakewebkit.Record {
	var out []fakewebkit.Record
	for _, rec := range f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(rec.Params), `__iwdp.call(\"release\"`) {
			out = append(out, rec)
		}
	}
	return out
}

// Design 2.6 and the W1b close-probe rule (docs/notes/w1b.md, "a canceled
// probe is no stall"; canceledByOwner): a cancellation by the owner says
// nothing about the page. When the owner of a release cancels it while the
// page-side runtime release waits for the page's answer, the release still
// ends the lease after the session's cleanup, it returns at once and not
// at the runtime release budget, and the page that answers is not
// stalled: no recovery reconnects it, and the next session gets it.
func TestOwnerCanceledRuntimeReleaseIsNoStall(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		if fn == "release" {
			cancel() // the release's owner gives up while the page holds the answer
			return fakewebkit.NoReply
		}
		return map[string]any{"ok": true, "value": true}
	})
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	r.m.End(mo.ID)
	mark := lastSeq(r.f)
	probes := evalsOf(r.f, "1")
	start := time.Now()
	if err := r.m.Release(rctx, agent.Session, mo.Page.Key); err != nil {
		t.Fatalf("Release canceled during the runtime release: %v, want success (the cleanup was done)", err)
	}
	if d := time.Since(start); d >= runtimeReleaseLimit {
		t.Fatalf("Release returned after %v, want at the owner's cancellation, before the %v budget", d, runtimeReleaseLimit)
	}
	if len(runtimeReleases(r.f)) != 1 {
		t.Fatal("the runtime release was not sent")
	}
	if n := receivedAfter(r.f, "Network.disable", mark); n != 1 {
		t.Fatalf("Network.disable sent %d times, want 1: the session's cleanup did not reach the page", n)
	}
	pg := mo.Page
	pg.mu.Lock()
	l := pg.lease
	pg.mu.Unlock()
	if l != nil {
		t.Fatalf("the lease of %q stayed after the release", l.session)
	}
	// The unanswered call starts a liveness probe in the background; the
	// page answers it. (A page marked stalled gets no probe.)
	eventually(t, 5*time.Second, "the liveness probe after the unanswered call", func() bool {
		return evalsOf(r.f, "1") > probes || pg.ConnState() != ConnOK
	})
	eventually(t, 5*time.Second, "the liveness probe to end", func() bool {
		pg.conn.mu.Lock()
		defer pg.conn.mu.Unlock()
		return !pg.conn.probing
	})
	if st := pg.ConnState(); st != ConnOK {
		t.Fatalf("page state after the owner's cancellation = %v, want ok", st)
	}
	if n := r.f.ConnectionCount(); n != 1 {
		t.Fatalf("the page got %d connections, want 1 (no recovery)", n)
	}
	next := AcquireOptions{Session: "s2", Reservation: "agent"}
	mo2, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, next, "get", ClassRead)
	if err != nil {
		t.Fatalf("Begin of the next session: %v", err)
	}
	r.m.End(mo2.ID)
}
