package core

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Regression tests for the W1b page-owner delta (docs/notes/w1b.md,
// "Review repairs, page owner"): the bootstrap parts are a requirement of
// Page like an owner, and a page target restored from the back/forward
// cache gets the desired state on the backend it kept.

// leaveFor sets up target as a provisional target and commits it, the way
// a cross-site navigation does. The old active target's backend leaves the
// live set but keeps its state (WebKit's back/forward cache); it is
// returned for restore.
func leaveFor(t *testing.T, p *wkPage, d *DesiredState, target string) *wkTarget {
	t.Helper()
	if p.live[target] == nil {
		p.live[target] = &wkTarget{bps: map[string]string{}}
	}
	delete(p.gone, target)
	if err := d.Apply(context.Background(), wkTargetCaller{p, target}); err != nil {
		t.Fatalf("setup of page target %s: %v", target, err)
	}
	old := p.active
	kept := p.live[old]
	delete(p.live, old)
	p.gone[old] = webkit.ErrTargetSwapped
	p.active = target
	d.commitTarget(target)
	return kept
}

// Review finding (W1b closure, tree dce71d4d): Apply sent Page.enable for
// the bootstrap parts but no Page reference counted it, so the first Page
// override after a commit sent a second Page.enable, which WebKit refuses
// with "Page domain already enabled" (InspectorPageAgent::enable). The
// parts are now a Page requirement like an owner: the first part enables
// Page on the active target too (the script runs only while the Page agent
// is enabled), an owner's Page.disable or release keeps Page for the
// parts, and the last requirement's end disables it.
func TestBootstrapPartsAreAPageRequirement(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Page.setBootstrapScript", `{"source":"globalThis.fixture = 1;"}`)
	if tg := p.live["page-0"]; !tg.page || !strings.Contains(tg.script, "globalThis.fixture = 1;") {
		t.Fatalf("page-0 after the first part: Page enabled %v, script %q; want Page enabled for the script", tg.page, tg.script)
	}
	leaveFor(t, p, ps.State, "page-1")
	// The reviewer's counterexample: a Page override after the commit.
	ownerCall(t, ps, c, "s2", "Page.overrideUserAgent", `{"value":"custom"}`)
	tg := p.live["page-1"]
	if !tg.page || tg.overrides["Page.overrideUserAgent"] != `{"value":"custom"}` || !strings.Contains(tg.script, "fixture") {
		t.Fatalf("page-1 after the override: %s, script %q", pageState(tg), tg.script)
	}
	// s2's own Page.disable ends its override; the part keeps Page.
	ownerCall(t, ps, c, "s2", "Page.disable", "")
	if !tg.page || len(tg.overrides) != 0 || !strings.Contains(tg.script, "fixture") {
		t.Fatalf("page-1 after s2's Page.disable: %s, script %q; want Page enabled for the part, no override", pageState(tg), tg.script)
	}
	from := len(p.wire)
	// A later owner's Page.enable is not sent again.
	ownerCall(t, ps, c, "s3", "Page.enable", "")
	if sent := sentSince(p, from, "Page.enable"); len(sent) != 0 {
		t.Fatalf("s3's Page.enable sent %q while the part holds Page", sent)
	}
	ownerCall(t, ps, c, "s3", "Page.disable", "")
	// Removing the last part ends the last requirement.
	ownerCall(t, ps, c, "s1", "Page.setBootstrapScript", `{"source":""}`)
	if tg.page || tg.script != "" {
		t.Fatalf("page-1 after the last part was removed: Page enabled %v, script %q; want Page disabled, no script", tg.page, tg.script)
	}
	if left := ps.State.AllOwners(); len(left) != 0 {
		t.Fatalf("owners left: %v", left)
	}
}

// The release path of the same rule: an owner's release keeps Page while
// another owner's part needs it, and the release of the part's owner
// disables Page when nothing else needs it.
func TestReleaseKeepsPageWhileAPartNeedsIt(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Page.setBootstrapScript", `{"source":"globalThis.part = 1;"}`)
	ownerCall(t, ps, c, "s2", "Page.overrideUserAgent", `{"value":"ua-2"}`)
	tg := p.live["page-0"]
	if _, err := releaseOwners(context.Background(), ps, "s2", c); err != nil {
		t.Fatal(err)
	}
	if !tg.page || len(tg.overrides) != 0 || tg.script == "" {
		t.Fatalf("page-0 after s2's release: %s, script %q; want Page kept for s1's part", pageState(tg), tg.script)
	}
	if _, err := releaseOwners(context.Background(), ps, "s1", c); err != nil {
		t.Fatal(err)
	}
	if tg.page || tg.script != "" {
		t.Fatalf("page-0 after s1's release: Page enabled %v, script %q; want Page disabled", tg.page, tg.script)
	}
}

// W3b finding (finding-back-across-sites.txt): "back" across two sites
// returns to the earlier page target from WebKit's back/forward cache. The
// restored target has the old target id and the old backend, whose agents
// stayed enabled with their state (the commit away never disconnected
// them; WebKitGTK 2.54 probe and source, docs/notes/w1b.md). The setup sent
// Page.enable to it, WebKit refused it as already enabled, and every later
// call was refused with the setup failure. The setup now resets what the
// backend kept and applies the current desired state, so the restored
// backend has exactly that state, also after changes made while it was
// away; forward does the same for the other target.
func TestRestoredTargetGetsTheDesiredState(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Page.setBootstrapScript", `{"source":"globalThis.boot = 1;"}`)
	ownerCall(t, ps, c, "s1", "Page.overrideUserAgent", `{"value":"ua-1"}`)
	ownerCall(t, ps, c, "s1", "Debugger.enable", "")
	ownerCall(t, ps, c, "s1", "Debugger.setBreakpointByUrl", `{"url":"https://a.example/a.js","lineNumber":1}`)
	ownerCall(t, ps, c, "s1", "Network.setExtraHTTPHeaders", `{"headers":{"X-A":"1"}}`)
	ownerCall(t, ps, c, "s2", "Network.setExtraHTTPHeaders", `{"headers":{"X-B":"1"}}`)

	page0 := leaveFor(t, p, ps.State, "page-1")
	// Changes while page-0 is in the back/forward cache.
	ownerCall(t, ps, c, "s1", "Debugger.removeBreakpoint", `{"breakpointId":"https://a.example/a.js:1:0"}`)
	ownerCall(t, ps, c, "s1", "Debugger.setBreakpointByUrl", `{"url":"https://b.example/b.js","lineNumber":1}`)
	ownerCall(t, ps, c, "s1", "Page.overrideUserAgent", `{"value":"ua-2"}`)
	ownerCall(t, ps, c, "s1", "Page.setBootstrapScript", `{"source":""}`)
	if _, err := releaseOwners(context.Background(), ps, "s2", c); err != nil {
		t.Fatal(err)
	}

	check := func(id string, tg *wkTarget) {
		t.Helper()
		ua := `{"value":"ua-2"}`
		if !tg.enabled || !maps.Equal(tg.bps, map[string]string{"https://b.example/b.js:1:0": "https://b.example/b.js"}) {
			t.Fatalf("%s: Debugger enabled %v breakpoints %v; want enabled with only b.js", id, tg.enabled, tg.bps)
		}
		if !tg.page || len(tg.overrides) != 1 || tg.overrides["Page.overrideUserAgent"] != ua || tg.script != "" {
			t.Fatalf("%s: Page %s script %q; want enabled, user agent ua-2, no script", id, pageState(tg), tg.script)
		}
		if !tg.net || !maps.Equal(tg.headers, map[string]string{"X-A": "1"}) {
			t.Fatalf("%s: Network %s; want enabled with X-A only", id, networkState(tg))
		}
	}
	// Back: page-0 returns with the backend it kept.
	p.live["page-0"] = page0
	page1 := leaveFor(t, p, ps.State, "page-0")
	check("page-0 after back", page0)
	// Forward: page-1 returns too.
	p.live["page-1"] = page1
	leaveFor(t, p, ps.State, "page-1")
	check("page-1 after forward", page1)
	// The restored target takes later changes as the active target.
	ownerCall(t, ps, c, "s1", "Page.overrideUserAgent", `{"value":"ua-3"}`)
	if _, err := releaseOwners(context.Background(), ps, "s1", c); err != nil {
		t.Fatal(err)
	}
	if page1.enabled || page1.page || page1.net || len(page1.bps) != 0 || len(page1.overrides) != 0 {
		t.Fatalf("page-1 after every release: Debugger %v Page %s Network %s; want nothing", page1.enabled, pageState(page1), networkState(page1))
	}
}

// resetCalls's other rules, on a restored backend: an agent that holds
// no desired state besides its enablement (Console here) and is still
// needed is kept, so its enable is not sent again (WebKit refuses a second
// enable on most agents); one that is no longer needed is disabled. A
// reset that fails fails the setup, naming the agent.
func TestRestoredBackendKeepsOrDisablesOtherAgents(t *testing.T) {
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s1", "Console.enable", "")
	ownerCall(t, ps, c, "s1", "Inspector.enable", "")
	ownerCall(t, ps, c, "s1", "Debugger.enable", "")
	page0 := leaveFor(t, p, ps.State, "page-1")
	ownerCall(t, ps, c, "s1", "Inspector.disable", "")
	p.live["page-0"] = page0
	from := len(p.wire)
	leaveFor(t, p, ps.State, "page-0")
	var got []string
	for _, w := range p.wire[from:] {
		if strings.HasPrefix(w, "page-0 ") && (strings.Contains(w, ".enable") || strings.Contains(w, ".disable")) {
			got = append(got, strings.TrimPrefix(w, "page-0 "))
		}
	}
	want := []string{"Debugger.disablenull", "Inspector.disablenull", "Debugger.enablenull"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("enables and disables sent to the restored page-0: %q, want %q (Console kept)", got, want)
	}

	// A failing reset fails the setup of the restored target.
	leaveFor(t, p, ps.State, "page-2")
	p.live["page-0"] = page0
	delete(p.gone, "page-0")
	p.fail = &wkFail{target: "page-0", token: `"none"`} // Debugger.setPauseOnExceptions {"state":"none"}
	err := ps.State.Apply(context.Background(), wkTargetCaller{p, "page-0"})
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "resetting the page target's state (Debugger.setPauseOnExceptions)") {
		t.Fatalf("setup of page-0 with a failing reset: %v, want the reset failure", err)
	}
}

// refusingConn answers method with a protocol error, the page's refusal,
// which provably changed nothing (noEffect).
type refusingConn struct {
	wkConn
	method string
	err    error
}

func (c refusingConn) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == c.method {
		return nil, c.err
	}
	return c.wkConn.Send(ctx, method, params)
}

// The first bootstrap part sends Page.enable. When the page refuses it
// (no effect), the part is not kept, so nothing needs Page; when the
// outcome is unknown, the part and its Page requirement stay until the
// owner's release, which sends Page.disable (review finding R6-6 rule).
func TestFirstPartWhosePageEnableFails(t *testing.T) {
	p, c, ps := ownersPage(t)
	refused := refusingConn{c, "Page.enable", &webkit.ErrorData{Code: -32000, Message: "refused by the test"}}
	_, err := legacyCall(context.Background(), ps, SessionOwner("legacy", "s1"), refused, "Page.setBootstrapScript", json.RawMessage(`{"source":"x = 1;"}`))
	if err == nil || ps.State.Needs("Page") || !ps.State.Boot.Empty() || len(ps.State.AllOwners()) != 0 {
		t.Fatalf("refused Page.enable: err %v, Page needed %v, parts %v, owners %v; want the error and nothing kept",
			err, ps.State.Needs("Page"), ps.State.Boot.Names(), ps.State.AllOwners())
	}
	lost := refusingConn{c, "Page.enable", errors.New("reply lost by the test")}
	if _, err := legacyCall(context.Background(), ps, SessionOwner("legacy", "s1"), lost, "Page.setBootstrapScript", json.RawMessage(`{"source":"x = 1;"}`)); err == nil {
		t.Fatal("Page.enable with a lost reply: no error")
	}
	if !ps.State.Needs("Page") {
		t.Fatal("after a Page.enable with an unknown outcome the part no longer needs Page; its release would not disable Page")
	}
	p.live["page-0"].page = true // the enable may have taken effect
	if _, err := releaseOwners(context.Background(), ps, "s1", c); err != nil {
		t.Fatal(err)
	}
	if p.live["page-0"].page || ps.State.Needs("Page") {
		t.Fatal("the release of the part's owner did not disable Page")
	}
}
