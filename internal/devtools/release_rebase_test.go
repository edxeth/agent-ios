package devtools

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W4 rebase onto core-events (docs/notes/w1b.md "core-events r3"): a lease
// release removes the session's owners from the desired state also when
// their cleanup cannot run, without the entries' Release: the connection is
// not attached, or a change holds the change lock past the release
// deadline. The DevTools state of the session in memory (captures, routes,
// offline mode, credentials, recordings, tombstones) must go with its
// entries, so that the next lease of the same session starts new captures
// that own their domains and finds no handler or recording of the old
// lease.
func TestReleaseWithoutCleanupLeavesNoDevtoolsStateForTheNextLease(t *testing.T) {
	for _, path := range []string{"lost connection", "change lock held past the release deadline"} {
		t.Run(path, func(t *testing.T) {
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			r := newRig(t)
			p := r.registryOp(t)
			oldN, oldK := p.Netlog("s1"), p.Console("s1")
			if oldN == nil || oldK == nil {
				t.Fatal("setup: the lease start did not start the captures")
			}
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				if _, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/blocked/*", Abort: true}); err != nil {
					return err
				}
				if err := p.SetOffline(ctx, c, "s1", true); err != nil {
					return err
				}
				if _, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "u", "pw"); err != nil {
					return err
				}
				if err := p.StartTrace(ctx, c, "s1", false); err != nil {
					return err
				}
				return p.StartProfiler(ctx, c, "s1")
			})
			r.f.Emit("Network.requestWillBeSent", reqEvent("before", "https://fixture.example/before", 1))
			eventually(t, "the request of the first lease", func() bool { _, ok := oldN.Get("page-1:before"); return ok })
			// A tombstone whose entry the release removed.
			ic := p.icpt
			ic.mu.Lock()
			ic.tombs["devtools:tombstone:route:s1:99"] = wipRule("^stale$")
			// A route of another session: the lease start of s1 leaves it.
			ic.routes = append(ic.routes, &Route{ID: 99, Pattern: "**/other/*", Mode: ModeWIP, session: "s2"})
			ic.mu.Unlock()

			switch path {
			case "lost connection":
				r.f.CloseConnections()
				eventually(t, "the lost page", func() bool { return pageConnState(p) == core.ConnLost })
				r.release(t)
				r.ao.Takeover = true
			case "change lock held past the release deadline":
				inside, unblock, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					held <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error {
						close(inside)
						<-unblock
						return nil
					})
				}()
				<-inside
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				_, err := r.m.ReleaseSession(ctx, "s1")
				cancel()
				if err == nil {
					t.Fatal("setup: the release got the change lock")
				}
				close(unblock)
				if err := <-held; err != nil {
					t.Fatal(err)
				}
			}
			for _, o := range p.ps.State.AllOwners() {
				if strings.HasSuffix(string(o), ":s1") {
					t.Fatalf("setup: owner %s of s1 after the release", o)
				}
			}

			p = r.registryOp(t) // the next lease of s1
			n, k := p.Netlog("s1"), p.Console("s1")
			if n == nil || n == oldN || k == nil || k == oldK {
				t.Fatalf("captures of the next lease: netlog %p (old %p), console %p (old %p); want new ones", n, oldN, k, oldK)
			}
			if !p.ps.State.Needs("Network") || !p.ps.State.Needs("Console") {
				t.Fatal("the captures of the next lease do not own Network and Console")
			}
			r.f.Emit("Network.requestWillBeSent", reqEvent("after", "https://fixture.example/after", 2))
			eventually(t, "the request of the next lease", func() bool { _, ok := n.Get("page-1:after"); return ok })
			if _, ok := n.Get("page-1:before"); ok {
				t.Fatal("the next lease's log holds a request of the first lease")
			}
			if rs := p.Routes("s1"); len(rs) != 0 {
				t.Fatalf("routes of the first lease in the next lease: %+v", rs)
			}
			if rs := p.Routes("s2"); len(rs) != 1 {
				t.Fatalf("routes of session s2 after the lease start of s1: %+v, want the one route", rs)
			}
			ic.mu.Lock()
			offline, creds, tombs := ic.offline["s1"], len(ic.creds["s1"]), len(ic.tombs)
			ic.mu.Unlock()
			if offline || creds != 0 || tombs != 0 {
				t.Fatalf("interceptor state of the first lease in the next lease: offline %v, %d credentials, %d tombstones", offline, creds, tombs)
			}
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				if err := p.StartTrace(ctx, c, "s1", false); err != nil {
					return err
				}
				return p.StartProfiler(ctx, c, "s1")
			})
		})
	}
}

// The page-side overrides of a session whose release could not send their
// reset (a change held the change lock past the release deadline) are reset
// later by core: the release marks the agents that the removed owners
// touched as unknown, and the background reset sends their disable, which
// resets WebKit's overrides of that agent (core-events r3). The user agent
// and media overrides belong to the Page agent, so their release marks
// Page.
func TestOverridesOfAReleaseWithoutCleanupAreResetLater(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(ctx context.Context, p *Page, c webkit.Caller) error
	}{
		{"user agent", func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetUserAgent(ctx, c, "s1", "UA-test")
		}},
		{"media", func(ctx context.Context, p *Page, c webkit.Caller) error {
			return p.SetMedia(ctx, c, "s1", Media{ColorScheme: "dark", Type: "print"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.must(t, tc.set)
			p := r.page(t)
			inside, unblock, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				held <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error {
					close(inside)
					<-unblock
					return nil
				})
			}()
			<-inside
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_, err := r.m.ReleaseSession(ctx, "s1")
			cancel()
			if err == nil {
				t.Fatal("setup: the release got the change lock")
			}
			mark := lastSeq(r.f)
			close(unblock)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			eventually(t, "the background reset of the Page agent", func() bool { return len(receivedAfter(r.f, "Page.disable", mark)) > 0 })
		})
	}
}

// The lease start's captures run before the first command's operation
// (core.OnLeaseStart). When the setup of the page's active target failed
// (a provisional target whose setup failed and then committed), core
// applies the page state again before an operation is queued
// (Manager.recoverSetup); the lease start does so too, so that its
// captures are not refused by the failed setup and start with the lease.
func TestLeaseCapturesStartAfterAFailedSetupIsRecovered(t *testing.T) {
	r := newRig(t)
	p := r.page(t) // lease s1 without the lease start
	var fail atomic.Bool
	fail.Store(true)
	r.f.Handle("DOM.hideHighlight", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if fail.Load() && c.Target == "page-2" {
			return nil, &fakewebkit.Error{Code: -32000, Message: "setup failure (test)"}
		}
		return map[string]any{}, nil
	})
	<-r.f.Provisional("page-2")
	r.f.Commit("page-2")
	eventually(t, "the page connection commits page-2", func() bool { return activeTargetOf(p.ps.Caller) == "page-2" })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := p.ps.Caller.(interface{ ActiveSetup(context.Context) error }).ActiveSetup(ctx)
	cancel()
	if !errors.As(err, new(*webkit.SetupError)) {
		t.Fatalf("setup: page-2's setup must have failed, got %v", err)
	}
	fail.Store(false)
	mark := lastSeq(r.f)
	p = r.registryOp(t)
	if p.Netlog("s1") == nil || p.Console("s1") == nil {
		t.Fatal("the lease start did not start the captures after the failed setup")
	}
	for _, m := range []string{"Network.enable", "Console.enable"} {
		sent := false
		for _, rec := range receivedAfter(r.f, m, mark) {
			sent = sent || rec.Target == "page-2"
		}
		if !sent {
			t.Fatalf("%s was not sent to page-2 at the lease start", m)
		}
	}
}

// A capture start whose operation ended records nothing in the desired
// state (core-events r3: a change of an ended operation is refused). The
// capture must not stay registered without its entry: the next start in a
// live operation starts a capture that owns its domain.
func TestCaptureStartOfAnEndedOperationLeavesNoCapture(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		start        func(ctx context.Context, p *Page, c webkit.Caller) (any, error)
		running      func(p *Page) bool
	}{
		{
			"netlog", "Network",
			func(ctx context.Context, p *Page, c webkit.Caller) (any, error) {
				n, _, err := p.StartNetlog(ctx, c, "s1")
				return n, err
			},
			func(p *Page) bool { return p.Netlog("s1") != nil },
		},
		{
			"console", "Console",
			func(ctx context.Context, p *Page, c webkit.Caller) (any, error) { return p.StartConsole(ctx, c, "s1") },
			func(p *Page) bool { return p.Console("s1") != nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := r.m.Do(ctx, r.req, r.ao, "test", core.ClassMutate, false, func(_ context.Context, mo *core.ManagedOp) error {
				p, err := For(mo.Page.PS)
				if err != nil {
					return err
				}
				mo.Op.End()
				if _, err := tc.start(ctx, p, mo.Op.Caller()); err == nil {
					t.Error("a capture start in an ended operation succeeded")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			p := r.page(t)
			if tc.running(p) {
				t.Fatalf("a %s capture runs after the start of an ended operation", tc.name)
			}
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := tc.start(ctx, p, c)
				return err
			})
			if !p.ps.State.Needs(tc.domain) || len(received(r.f, tc.domain+".enable")) == 0 {
				t.Fatalf("the %s capture of a live operation does not own %s", tc.name, tc.domain)
			}
		})
	}
}
