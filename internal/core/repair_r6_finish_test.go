package core

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the W1b round-6 finishing delta (docs/notes/w1b.md,
// "Review repairs, round 6, finishing delta").

// Manager.Close cancels every background goroutine it started and waits
// for it: after Close returns, a background recovery that was running
// touches neither the page nor the reservations directory. Before, Close
// returned while the recovery ran, and the recovery's preflight created the
// reservation lock file afterwards (TestIdleProbeRunsBeforeTheNextCommand
// failed at times with "TempDir RemoveAll cleanup: directory not empty").
func TestCloseWaitsForTheBackgroundRecovery(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	held, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	r.m.recoveryHold = func(ctx context.Context) {
		close(held)
		select {
		case <-ctx.Done():
		case <-release:
		}
	}
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.f.StallConnection(1) // the call and the liveness probe get no reply
	_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
	r.m.End(mo.ID)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("no background recovery started after the failed probe")
	}
	pg := mo.Page
	recovering := func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return pg.recovering
	}
	if err := r.m.Close(ctxFor(t, 5*time.Second)); err != nil {
		t.Logf("Close: %v", err)
	}
	if recovering() {
		t.Error("Close returned while the background recovery still runs")
	}
	// Anything that still runs now works after Close: remove the
	// reservations directory and let a held recovery go on.
	if err := os.RemoveAll(r.res.Dir); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, 10*time.Second, "the recovery to end", func() bool { return !recovering() })
	if _, err := os.Stat(r.res.Dir); !os.IsNotExist(err) {
		t.Errorf("the reservations directory was used after Close returned (stat: %v)", err)
	}
	if n := r.f.ConnectionCount(); n != 1 {
		t.Errorf("connections = %d after Close, want 1: a recovery dialed after Close", n)
	}
}

// pageState is a target's Page state as the oracle compares it.
func pageState(tg *wkTarget) string {
	overrides := tg.overrides
	if len(overrides) == 0 {
		overrides = nil
	}
	b, _ := json.Marshal(map[string]any{"enabled": tg.page, "overrides": overrides})
	return string(b)
}

// Orchestrator rule 1 and design 2.4 for Network: headers, interception,
// the caching override and emulated conditions set by an owner that never
// enabled Network acquire Network for that owner. So they reach new
// targets, another owner's last Network.disable does not clear them (the
// disable reaches the page only when no owner remains), and they are
// cleared, model included, when their owner disables Network or releases.
// Before, such state was sent without Network: new targets did not get
// headers or interception, and another owner's Network.disable cleared all
// of it on the page while the model kept it.
func TestNetworkStateAcquiresNetworkForItsOwner(t *testing.T) {
	for _, kind := range []struct {
		name  string
		calls [][2]string
		want  wkTarget
	}{
		{
			"headers",
			[][2]string{{"Network.setExtraHTTPHeaders", `{"headers":{"X-One":"1"}}`}},
			wkTarget{net: true, headers: map[string]string{"X-One": "1"}},
		},
		{
			"interception",
			[][2]string{{"Network.setInterceptionEnabled", `{"enabled":true}`}, {"Network.addInterception", `{"url":"https://fixture.example/api","stage":"request"}`}},
			wkTarget{net: true, intercept: true, rules: map[string]bool{"https://fixture.example/api": true}},
		},
		{
			"caching",
			[][2]string{{"Network.setResourceCachingDisabled", `{"disabled":true}`}},
			wkTarget{net: true, caching: true},
		},
		{
			"conditions",
			[][2]string{{"Network.setEmulatedConditions", `{"bytesPerSecondLimit":1000}`}},
			wkTarget{net: true, conds: `{"bytesPerSecondLimit":1000}`},
		},
	} {
		for _, end := range []string{"own disable", "release"} {
			t.Run(kind.name+"/"+end, func(t *testing.T) {
				p, c, ps := ownersPage(t)
				ctx := context.Background()
				for _, call := range kind.calls {
					ownerCall(t, ps, c, "s1", call[0], call[1])
				}
				want := networkState(&kind.want)
				if got := networkState(p.live["page-0"]); got != want {
					t.Fatalf("page-0 after s1's calls without Network.enable: %s, want %s", got, want)
				}
				ownerCall(t, ps, c, "s2", "Network.enable", "")
				from := len(p.wire)
				ownerCall(t, ps, c, "s2", "Network.disable", "")
				if got := sentSince(p, from, "Network.disable"); len(got) != 0 {
					t.Errorf("s2's Network.disable reached the page while s1 needs Network: %q", got)
				}
				p.live["page-1"] = &wkTarget{bps: map[string]string{}}
				if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"page-0", "page-1"} {
					if got := networkState(p.live[id]); got != want {
						t.Errorf("%s after s2's Network.disable: %s, want s1's state %s", id, got, want)
					}
				}
				from = len(p.wire)
				if end == "release" {
					if _, err := releaseOwners(ctx, ps, "s1", c); err != nil {
						t.Fatal(err)
					}
				} else {
					ownerCall(t, ps, c, "s1", "Network.disable", "")
				}
				clean := networkState(&wkTarget{})
				for _, id := range []string{"page-0", "page-1"} {
					if got := networkState(p.live[id]); got != clean {
						t.Errorf("%s after s1's %s: %s, want %s", id, end, got, clean)
					}
				}
				if got := sentSince(p, from, "Network.disable"); len(got) != 2 {
					t.Errorf("Network.disable after the last owner's %s: %q, want one per target", end, got)
				}
				if left := ps.State.AllOwners(); len(left) != 0 {
					t.Errorf("owners after s1's %s: %v, want none", end, left)
				}
				p.live["page-2"] = &wkTarget{bps: map[string]string{}}
				if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-2"}); err != nil {
					t.Fatal(err)
				}
				if got := networkState(p.live["page-2"]); got != clean {
					t.Errorf("a later target got %s, want %s", got, clean)
				}
			})
		}
	}
}

// Review finding (round-6 review, R6-3 incomplete) and the orchestrator's
// decision that the one-cleanup-path rule covers every domain: Page
// overrides belong to Page. An owner's own Page.disable drops its
// overrides and resets each to the remaining owners' value or the
// baseline, on the active and the provisional targets, also while Page
// stays enabled for another owner (for example the runtime); a Page
// override acquires Page for its owner, so another owner's last
// Page.disable does not reset it. Before, ruleDomain knew only Debugger:
// the owner's override stayed in the model and on the page.
func TestPageOverridesBelongToPage(t *testing.T) {
	for _, tc := range []struct{ method, set, other string }{
		{"Page.overrideUserAgent", `{"value":"custom"}`, `{"value":"other"}`},
		{"Page.setEmulatedMedia", `{"media":"print"}`, `{"media":"screen"}`},
		{"Page.overrideUserPreference", `{"name":"PrefersReducedMotion","value":"Reduce"}`, `{"name":"PrefersReducedMotion","value":"NoPreference"}`},
		{"Page.overrideSetting", `{"setting":"ScriptEnabled","value":false}`, `{"setting":"ScriptEnabled","value":true}`},
		{"Page.setShowPaintRects", `{"result":true}`, ""},
		{"Page.setForcedAppearance", `{"appearance":"Dark"}`, `{"appearance":"Light"}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			ctx := context.Background()
			t.Run("own disable while another owner keeps Page", func(t *testing.T) {
				p, c, ps := ownersPage(t)
				ownerCall(t, ps, c, "s1", "Page.enable", "")
				ownerCall(t, ps, c, "s2", tc.method, tc.set)
				p.live["page-1"] = &wkTarget{bps: map[string]string{}}
				if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
					t.Fatal(err)
				}
				if len(p.live["page-1"].overrides) != 1 {
					t.Fatalf("page-1's setup did not get s2's override (%s); the test needs it there", pageState(p.live["page-1"]))
				}
				from := len(p.wire)
				ownerCall(t, ps, c, "s2", "Page.disable", "")
				want := pageState(&wkTarget{page: true})
				for _, id := range []string{"page-0", "page-1"} {
					if got := pageState(p.live[id]); got != want {
						t.Errorf("%s after s2's Page.disable: %s, want %s (sent %q)", id, got, want, p.wire[from:])
					}
				}
				if left := ps.State.AllOwners(); !slices.Equal(left, []Owner{SessionOwner("legacy", "s1")}) {
					t.Errorf("owners after s2's Page.disable: %v, want only s1", left)
				}
			})
			t.Run("another owner's last disable keeps it", func(t *testing.T) {
				p, c, ps := ownersPage(t)
				ownerCall(t, ps, c, "s1", tc.method, tc.set)
				want := pageState(p.live["page-0"])
				if !p.live["page-0"].page || len(p.live["page-0"].overrides) != 1 {
					t.Fatalf("page-0 after s1's override: %s, want Page enabled with the override", want)
				}
				ownerCall(t, ps, c, "s2", "Page.enable", "")
				ownerCall(t, ps, c, "s2", "Page.disable", "")
				p.live["page-1"] = &wkTarget{bps: map[string]string{}}
				if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"page-0", "page-1"} {
					if got := pageState(p.live[id]); got != want {
						t.Errorf("%s after s2's Page.disable: %s, want s1's %s", id, got, want)
					}
				}
				if _, err := releaseOwners(ctx, ps, "s1", c); err != nil {
					t.Fatal(err)
				}
				clean := pageState(&wkTarget{})
				for _, id := range []string{"page-0", "page-1"} {
					if got := pageState(p.live[id]); got != clean {
						t.Errorf("%s after s1's release: %s, want %s", id, got, clean)
					}
				}
				if left := ps.State.AllOwners(); len(left) != 0 {
					t.Errorf("owners after s1's release: %v, want none", left)
				}
			})
			if tc.other == "" {
				return
			}
			t.Run("own disable falls back to the other owner's value", func(t *testing.T) {
				p, c, ps := ownersPage(t)
				ownerCall(t, ps, c, "s1", tc.method, tc.other)
				ownerCall(t, ps, c, "s2", tc.method, tc.set)
				ownerCall(t, ps, c, "s2", "Page.disable", "")
				var other map[string]any
				_ = json.Unmarshal([]byte(tc.other), &other)
				key, _, _ := pageOverride(tc.method, other)
				got := p.live["page-0"]
				var have map[string]any
				_ = json.Unmarshal([]byte(got.overrides[key]), &have)
				if !got.page || !maps.Equal(have, other) {
					t.Errorf("page-0 after s2's Page.disable: %s, want Page enabled with s1's %s", pageState(got), tc.other)
				}
			})
		})
	}
}

// The same rule for CSS: a forced pseudo state belongs to CSS, so the
// owner's own CSS.disable resets it. It does not acquire CSS (CSS.enable
// hangs through iwdp Target routing, docs/webkit-protocol.md), so the disable reaches the
// page as before.
func TestForcedPseudoStateBelongsToCSS(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "CSS.forcePseudoState", `{"nodeId":7,"forcedPseudoClasses":["hover"]}`)
	if got := sentSince(p, 0, "CSS.enable"); len(got) != 0 {
		t.Fatalf("CSS.forcePseudoState sent %q; CSS.enable hangs through iwdp", got)
	}
	from := len(p.wire)
	ownerCall(t, ps, c, "s1", "CSS.disable", "")
	got := p.wire[from:]
	if len(got) != 2 || !strings.Contains(got[0], `CSS.forcePseudoState{"forcedPseudoClasses":[],"nodeId":7}`) || !strings.HasPrefix(got[1], "page-0 CSS.disable") {
		t.Errorf("s1's CSS.disable sent %q, want the reset of its forced state, then CSS.disable", got)
	}
	if left := ps.State.AllOwners(); len(left) != 0 {
		t.Errorf("owners after s1's CSS.disable: %v, want none", left)
	}
}
