package core

import (
	"context"
	"maps"
	"strings"
	"testing"
)

// W1b closure review (tree ef4d5221): a page cached while the connection
// that enabled its agents closed comes back under the next connection with
// those agents and their state (WebKit never disconnects a cached page's
// backend; researcher reports webkit-bfcache-target-state-20261107 and
// webkit-bfcache-highlight-inspect-pseudo-state-20261107-063807). The new
// connection has no record of it, and its setup sent Page.enable as to a
// fresh backend ("Page domain already enabled"). Now every agent the
// connection does not know is reset before its first enable
// (agentResetCalls, sent by webkit.Session and modeled by the fake), and
// the DOM overlay of every new target is reset, so the restored target has
// exactly the second connection's desired state: no breakpoint, script,
// pause on exceptions, header, interception, highlight or inspect mode of
// the first connection.
func TestRestoredTargetOfAnEarlierConnection(t *testing.T) {
	p, c, ps := ownersPage(t)
	for _, call := range [][2]string{
		{"Page.setBootstrapScript", `{"source":"globalThis.old = 1;"}`},
		{"Page.overrideUserAgent", `{"value":"ua-old"}`},
		{"Debugger.enable", ""},
		{"Debugger.setBreakpointByUrl", `{"url":"https://a.example/a.js","lineNumber":1}`},
		{"Debugger.setPauseOnExceptions", `{"state":"all"}`},
		{"Network.setExtraHTTPHeaders", `{"headers":{"X-Old":"1"}}`},
		{"Network.setInterceptionEnabled", `{"enabled":true}`},
		{"Network.addInterception", `{"url":"https://a.example/","stage":"request"}`},
		{"DOM.highlightRect", `{"x":1,"y":1,"width":5,"height":5}`},
		{"DOM.setInspectModeEnabled", `{"enabled":true}`},
	} {
		ownerCall(t, ps, c, "s1", call[0], call[1])
	}
	page0 := leaveFor(t, p, ps.State, "page-1")
	if !page0.page || !page0.enabled || !page0.net || !page0.inspect || page0.pauseExc != "all" {
		t.Fatalf("page-0 before it was cached: Debugger %v Page %s Network %s inspect %v; want the first connection's state", page0.enabled, pageState(page0), networkState(page0), page0.inspect)
	}

	// The first connection ends: WebKit cleans the active target only, and
	// the next connection's Session starts without a record.
	p.live["page-1"] = &wkTarget{bps: map[string]string{}}
	p.rec = nil
	d := NewDesiredState()
	if err := d.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ps = &PageSession{State: d, Sched: NewScheduler(c, SchedulerOptions{})}
	ownerCall(t, ps, c, "s9", "Page.overrideUserAgent", `{"value":"ua-new"}`)
	ownerCall(t, ps, c, "s9", "Debugger.enable", "")
	ownerCall(t, ps, c, "s9", "Network.setExtraHTTPHeaders", `{"headers":{"X-New":"1"}}`)

	// Back: page-0 returns with the first connection's backend.
	p.live["page-0"] = page0
	leaveFor(t, p, d, "page-0")
	if !page0.enabled || len(page0.bps) != 0 || page0.pauseExc != "none" {
		t.Fatalf("restored page-0: Debugger enabled %v breakpoints %v pause on exceptions %q; want enabled, none, none", page0.enabled, page0.bps, page0.pauseExc)
	}
	if !page0.page || !maps.Equal(page0.overrides, map[string]string{"Page.overrideUserAgent": `{"value":"ua-new"}`}) || page0.script != "" {
		t.Fatalf("restored page-0: Page %s script %q; want enabled, user agent ua-new, no script", pageState(page0), page0.script)
	}
	if !page0.net || !maps.Equal(page0.headers, map[string]string{"X-New": "1"}) || page0.intercept || len(page0.rules) != 0 {
		t.Fatalf("restored page-0: Network %s; want enabled with X-New only", networkState(page0))
	}
	if page0.highlight || page0.inspect {
		t.Fatalf("restored page-0: highlight %v inspect mode %v; want neither", page0.highlight, page0.inspect)
	}
}

// Closure review P2 (researcher report
// webkit-bfcache-highlight-inspect-pseudo-state-20261107-063807): DOM
// highlights and inspect mode survive a back/forward cache restore; only
// DOM.hideHighlight, DOM.setInspectModeEnabled(false) or a backend
// disconnect clears them, and no record says whether a backend has them.
// Every new target, restored or not, and the target of a new connection
// now start without them. The desired state does not replay them (a fresh
// target never got them), and their owner's release still sends the
// resets.
func TestEveryNewTargetStartsWithoutTheOverlay(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "DOM.highlightRect", `{"x":1,"y":1,"width":5,"height":5}`)
	ownerCall(t, ps, c, "s1", "DOM.setInspectModeEnabled", `{"enabled":true}`)
	page0 := p.live["page-0"]
	if !page0.highlight || !page0.inspect {
		t.Fatalf("page-0: highlight %v inspect mode %v; want both", page0.highlight, page0.inspect)
	}
	from := len(p.wire)
	page0 = leaveFor(t, p, ps.State, "page-1")
	if got := sentSince(p, from, "DOM."); strings.Join(got, " ") != `page-1 DOM.hideHighlightnull page-1 DOM.setInspectModeEnabled{"enabled":false}` {
		t.Fatalf("the setup of the fresh page-1 sent %q, want the overlay reset only", got)
	}
	// Back: page-0 kept its overlay in the back/forward cache.
	p.live["page-0"] = page0
	leaveFor(t, p, ps.State, "page-0")
	if page0.highlight || page0.inspect {
		t.Fatalf("restored page-0: highlight %v inspect mode %v; want neither", page0.highlight, page0.inspect)
	}
	// A new connection's target (WebKit cleans it on disconnect; the
	// connection cannot know that it did).
	page0.highlight, page0.inspect = true, true
	if err := ps.State.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if page0.highlight || page0.inspect {
		t.Fatalf("page-0 after a new connection's Apply: highlight %v inspect mode %v; want neither", page0.highlight, page0.inspect)
	}
	from = len(p.wire)
	if err := releaseOwners(context.Background(), ps, "s1", c); err != nil {
		t.Fatal(err)
	}
	if got := sentSince(p, from, "DOM."); len(got) != 2 {
		t.Fatalf("s1's release sent %q, want its two overlay resets", got)
	}
}
