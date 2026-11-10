package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Regression tests for the fifth W1b review: R5-1 (a change after a
// provisional setup reaches that target), R5-2 (breakpoint activation is
// owned state) and R5-3 (a removal that crosses a commit). Every page
// endpoint is a private fake.

func waitStaged(t *testing.T, st *DesiredState) {
	t.Helper()
	eventually(t, time.Second, "staged initial breakpoint", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.staged["page-2"]["bp-1"] == "bp-2"
	})
}

func waitCommitted(t *testing.T, st *DesiredState) {
	t.Helper()
	eventually(t, time.Second, "commit reaches the desired state", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.active == "page-2"
	})
}

// R5-1 (reviewer counterexample), design 2.4 and 2.7: a URL breakpoint set
// after page-2's setup, while page-1 is still active, is installed on
// page-2 before it commits.
func TestSetAfterProvisionalSetupReachesTheCommittedTarget(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", 700*time.Millisecond); close(swapped) }()
	defer func() { <-swapped }()
	waitStaged(t, mo.Page.PS.State)
	b := rawSeq(t, r, ctxFor(t, 2*time.Second), [2]string{"Debugger.setBreakpointByUrl", `{"url":"https://fixture.example/late.js","lineNumber":1}`})
	r.m.End(b.ID)
	<-swapped
	waitCommitted(t, mo.Page.PS.State)
	for _, rec := range r.f.ReceivedMethod("Debugger.setBreakpointByUrl") {
		if rec.Target == "page-2" && strings.Contains(string(rec.Params), "late.js") {
			return
		}
	}
	t.Fatal("successful breakpoint set after provisional setup was never installed on the committed page-2")
}

// R5-2 (reviewer counterexample) and the user's decision: an explicit
// active:true is owned state; a fresh target gets Debugger.enable, then the
// activation, then the breakpoints (WebKit DebuggerManager.initializeTarget
// order), and the release restores the inactive baseline.
func TestActivationIsReplayedBeforeTheBreakpoints(t *testing.T) {
	r := breakpointRig(t)
	mo := rawSeq(t, r, ctxFor(t, 3*time.Second), [2]string{"Debugger.enable", ""},
		[2]string{"Debugger.setBreakpointsActive", `{"active":true}`},
		[2]string{"Debugger.setBreakpointByUrl", bpByURL})
	r.m.End(mo.ID)
	r.f.Swap("page-2", 200*time.Millisecond)
	waitCommitted(t, mo.Page.PS.State)
	// WebKit can commit before the setup ends; an operation then waits for
	// the setup (Scheduler.Begin, ActiveSetup; design 2.7), so the page
	// state is read when the setup has ended, not at a fixed time
	// (docs/notes/w1b.md, "core-events r2: two swap tests read too early").
	if err := mo.Page.PS.Caller.(*pageConn).ActiveSetup(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatalf("page-2's setup: %v", err)
	}
	var order []string
	for _, rec := range r.f.Received() {
		if rec.Target != "page-2" || !strings.HasPrefix(rec.Method, "Debugger.") {
			continue
		}
		order = append(order, rec.Method+string(rec.Params))
	}
	// The connection does not know page-2's Debugger agent, so its enable
	// follows the agent's reset (agentResetCalls).
	want := []string{`Debugger.setPauseOnExceptions{"state":"none"}`, `Debugger.setPauseOnDebuggerStatements{"enabled":true}`, "Debugger.disable", "Debugger.enable", `Debugger.setBreakpointsActive{"active":true}`, "Debugger.setBreakpointByUrl" + bpByURL}
	if !slices.Equal(order, want) {
		t.Fatalf("page-2 setup sent %q, want %q", order, want)
	}
	if err := r.m.Release(ctxFor(t, 2*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	var restored bool
	for _, rec := range r.f.ReceivedMethod("Debugger.setBreakpointsActive") {
		if rec.Target == "page-2" && string(rec.Params) == `{"active":false}` {
			restored = true
		}
	}
	if !restored {
		t.Fatal("release did not restore the inactive baseline on page-2")
	}
}

// R5-3 (reviewer counterexample): page-1's removal reply is held across
// the commit, so page-1 is destroyed with its breakpoint. The removal
// succeeds and page-2's copy is removed.
func TestRemovalAcrossCommitDoesNotFailForTheOldTarget(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	r.f.DelayReplies("Debugger.removeBreakpoint", time.Second)
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", 500*time.Millisecond); close(swapped) }()
	defer func() { <-swapped }()
	waitStaged(t, mo.Page.PS.State)
	rm, err := r.m.Begin(ctxFor(t, 3*time.Second), r.req, agent, "remove", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.m.Raw(ctxFor(t, 3*time.Second), rm.ID, "Debugger.removeBreakpoint", json.RawMessage(`{"breakpointId":"bp-1"}`))
	r.m.End(rm.ID)
	<-swapped
	waitCommitted(t, mo.Page.PS.State)
	if got := removalsOn(r.f, "page-2"); len(got) != 1 || got[0] != "bp-2" {
		t.Fatalf("new target removal %v", got)
	}
	if err != nil {
		t.Fatalf("all live copies removed, but the removal failed: %v", err)
	}
}

// wkTarget is one WebKit backend as far as these tests need: the Debugger
// domain, the activation flag (inactive on a new backend) and URL
// breakpoints with WebKit's deterministic ids (<url>:<line>:<column>).
// Debugger.enable on an enabled backend and a second breakpoint at one
// location fail, as in WebKit, so a repeated send shows up as an error.
//
// The Network part follows InspectorNetworkAgent (design 2.4): extra
// headers are merged by name and never removed, and Network.disable clears
// the headers, the interception enablement and rules, the caching override
// and the emulated conditions.
//
// The Page part follows InspectorPageAgent: Page.enable on an enabled
// backend fails, the overrides (user agent, emulated media, user
// preferences, settings, paint rects, forced appearance) are kept by key
// and a call with the baseline value removes one, and Page.disable resets
// every override but keeps the bootstrap script.
type wkTarget struct {
	enabled, active bool
	bps             map[string]string // id -> url
	net             bool
	headers         map[string]string
	intercept       bool
	rules           map[string]bool // interception rule URLs
	caching         bool            // the resource cache is disabled
	conds           string          // emulated conditions, "" for none
	page            bool
	overrides       map[string]string // Page override key -> its call's parameters
	script          string            // Page.setBootstrapScript source
	highlight       bool              // the DOM overlay shows a highlight
	inspect         bool              // DOM inspect mode
	pauseExc        string            // Debugger.setPauseOnExceptions state; Debugger.disable keeps it (JSC::Debugger)
}

// pageOverride returns the key of a Page override call and whether the
// call sets WebKit's baseline (removes the override). ok is false for
// another method.
func pageOverride(method string, p map[string]any) (key string, base, ok bool) {
	str := func(n string) string { s, _ := p[n].(string); return s }
	_, has := p["value"]
	switch method {
	case "Page.overrideUserAgent":
		return method, str("value") == "", true
	case "Page.overrideSetting":
		return method + "|" + str("setting"), !has, true
	case "Page.overrideUserPreference":
		return method + "|" + str("name"), !has, true
	case "Page.setEmulatedMedia":
		return method, str("media") == "", true
	case "Page.setShowPaintRects":
		on, _ := p["result"].(bool)
		return method, !on, true
	case "Page.setForcedAppearance":
		return method, str("appearance") == "", true
	}
	return "", false, false
}

// wkBlock holds the first send to target, other than a breakpoint
// removal, until release is closed; hit is closed when that send starts.
type wkBlock struct {
	target       string
	hit, release chan struct{}
}

// wkFail rejects every send to target whose parameters contain token.
type wkFail struct {
	target, token string
	fired         bool
}

// errInjected is the failure of a send that wkFail rejects.
var errInjected = errors.New("rejected by the test")

type wkPage struct {
	mu     sync.Mutex // sends of concurrent changes; hooks run outside it
	active string
	live   map[string]*wkTarget
	gone   map[string]error
	sends  int
	hookAt int // the send after which hook runs once (0: none)
	hook   func()
	block  *wkBlock
	fail   *wkFail
	trace  []string
	wire   []string // "<target> <method><params>" of every call a live target took
	// rec is the webkit.Session record of the agents each target's backend
	// has (webkit.AgentState), kept as Session.noteAgent keeps it: across
	// commits, so that a restored backend reports what it kept.
	// A first enable of an agent without an entry goes out after the
	// agent's reset (agentResetCalls), as webkit.Session sends it
	// (SessionOptions.AgentReset).
	rec map[string]map[string]webkit.AgentState
}

// noteAgentLocked updates p.rec after a send of method to target ended
// with err, as webkit.Session does for replies (the fake has no lost
// replies).
func (p *wkPage) noteAgentLocked(target, method string, err error) {
	dom, verb, _ := strings.Cut(method, ".")
	already := err != nil && strings.Contains(err.Error(), "already enabled")
	if verb != "enable" && verb != "disable" || err != nil && !already {
		return
	}
	if p.rec == nil {
		p.rec = map[string]map[string]webkit.AgentState{}
	}
	if p.rec[target] == nil {
		p.rec[target] = map[string]webkit.AgentState{}
	}
	if verb == "enable" {
		p.rec[target][dom] = webkit.AgentEnabled
	} else {
		p.rec[target][dom] = webkit.AgentDisabled
	}
}

// send sends to target ("" = the active target).
func (p *wkPage) send(target, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	p.mu.Lock()
	if target == "" {
		target = p.active
	}
	if b := p.block; b != nil && b.target == target && method != "Debugger.removeBreakpoint" {
		p.block = nil
		close(b.hit)
		p.mu.Unlock()
		<-b.release
		p.mu.Lock()
	}
	dom, verb, _ := strings.Cut(method, ".")
	if _, known := p.rec[target][dom]; p.live[target] != nil && verb == "enable" && !known {
		for _, c := range agentResetCalls(dom) {
			craw, _ := json.Marshal(c.Params)
			_, cerr := p.applyLocked(target, c.Method, craw)
			p.noteAgentLocked(target, c.Method, cerr)
			if cerr == nil {
				p.wire = append(p.wire, target+" "+c.Method+string(craw))
			}
		}
	}
	res, err := p.applyLocked(target, method, raw)
	if p.live[target] != nil {
		p.noteAgentLocked(target, method, err)
	}
	var hook func()
	if err == nil {
		p.sends++
		p.wire = append(p.wire, target+" "+method+string(raw))
		if p.hook != nil && p.sends >= p.hookAt {
			hook = p.hook
			p.hook = nil
		}
	}
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return res, err
}

func (p *wkPage) applyLocked(target, method string, raw json.RawMessage) (json.RawMessage, error) {
	t := p.live[target]
	if t == nil {
		if err := p.gone[target]; err != nil {
			// Nothing is written to a destroyed target (webkit.Session).
			return nil, fmt.Errorf("%s to %s: %w: %w", method, target, err, webkit.ErrNotSent)
		}
		return nil, fmt.Errorf("%s: unknown target %q", method, target)
	}
	if f := p.fail; f != nil && f.target == target && method != "Debugger.removeBreakpoint" && strings.Contains(string(raw), f.token) {
		f.fired = true
		return nil, fmt.Errorf("%s on %s: %w", method, target, errInjected)
	}
	var v struct {
		URL      string            `json:"url"`
		ID       string            `json:"breakpointId"`
		Active   bool              `json:"active"`
		Enabled  bool              `json:"enabled"`
		Disabled bool              `json:"disabled"`
		Headers  map[string]string `json:"headers"`
		Limit    *json.Number      `json:"bytesPerSecondLimit"`
		State    string            `json:"state"`
	}
	_ = json.Unmarshal(raw, &v)
	res := json.RawMessage("{}")
	switch method {
	case "Debugger.enable":
		if t.enabled {
			return nil, fmt.Errorf("%s on %s: Debugger domain already enabled", method, target)
		}
		t.enabled = true
	case "Debugger.disable":
		t.enabled, t.active, t.bps = false, false, map[string]string{}
	case "Debugger.setBreakpointsActive":
		t.active = v.Active
	case "Debugger.setPauseOnExceptions":
		t.pauseExc = v.State
	case "Debugger.setBreakpointByUrl":
		id := v.URL + ":1:0"
		if _, dup := t.bps[id]; dup {
			return nil, fmt.Errorf("%s on %s: Breakpoint for given location already exists", method, target)
		}
		t.bps[id] = v.URL
		res, _ = json.Marshal(map[string]any{"breakpointId": id, "locations": []any{}})
	case "Debugger.removeBreakpoint":
		delete(t.bps, v.ID)
	case "Page.enable":
		if t.page {
			return nil, fmt.Errorf("%s on %s: Page domain already enabled", method, target)
		}
		t.page = true
	case "Page.disable":
		t.page, t.overrides = false, nil
	case "Page.setBootstrapScript":
		var s struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(raw, &s)
		t.script = s.Source
	case "DOM.hideHighlight":
		t.highlight = false
	case "DOM.setInspectModeEnabled":
		if t.inspect && !v.Enabled {
			t.highlight = false // InspectorDOMAgent::setSearchingForNode(false) hides it
		}
		t.inspect = v.Enabled
	case "Network.enable":
		t.net = true
	case "Network.disable":
		t.net, t.headers, t.intercept, t.rules, t.caching, t.conds = false, nil, false, nil, false, ""
	case "Network.setExtraHTTPHeaders":
		if t.headers == nil {
			t.headers = map[string]string{}
		}
		maps.Copy(t.headers, v.Headers)
	case "Network.setInterceptionEnabled":
		t.intercept = v.Enabled
	case "Network.addInterception":
		if t.rules == nil {
			t.rules = map[string]bool{}
		}
		t.rules[v.URL] = true
	case "Network.removeInterception":
		delete(t.rules, v.URL)
	case "Network.setResourceCachingDisabled":
		t.caching = v.Disabled
	case "Network.setEmulatedConditions":
		t.conds = ""
		if v.Limit != nil {
			t.conds = string(raw)
		}
	default:
		if strings.HasPrefix(method, "DOM.highlight") {
			t.highlight = true
		}
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		if key, base, ok := pageOverride(method, p); ok {
			if t.overrides == nil {
				t.overrides = map[string]string{}
			}
			if base {
				delete(t.overrides, key)
			} else {
				t.overrides[key] = string(raw)
			}
		}
	}
	return res, nil
}

// wkTargetCaller sends to one target, as webkit.TargetCaller does.
type wkTargetCaller struct {
	p  *wkPage
	id string
}

func (c wkTargetCaller) TargetID() string { return c.id }

// AgentStates is the Session's record of the target's backend.
func (c wkTargetCaller) AgentStates() map[string]webkit.AgentState {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return maps.Clone(c.p.rec[c.id])
}

func (c wkTargetCaller) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	return c.p.send(c.id, method, params)
}

// wkConn is the connection: the active target, or the one webkit.WithTarget names.
type wkConn struct{ p *wkPage }

func (c wkConn) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	target, _ := webkit.TargetOf(ctx)
	return c.p.send(target, method, params)
}

func (c wkConn) ActiveTarget() string {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return c.p.active
}

// AgentStates is the Session's record of target's backend.
func (c wkConn) AgentStates(target string) map[string]webkit.AgentState {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return maps.Clone(c.p.rec[target])
}
func (wkConn) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (wkConn) IsTargetRouted() bool                         { return true }
func (wkConn) Done() <-chan struct{}                        { return nil }

// parkedChange runs change in its own goroutine and returns once the
// change ended, or once it queued items for target and let the next change
// begin (it then waits for those items, which target's setup sends). The
// returned channel is closed when the change ends.
func parkedChange(d *DesiredState, target string, change func()) <-chan struct{} {
	done := make(chan struct{})
	before := queuedFor(d, target)
	go func() {
		defer close(done)
		change()
	}()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Microsecond) {
		select {
		case <-done:
			return done
		default:
		}
		if queuedFor(d, target) > before && len(d.chg) == 0 {
			return done
		}
	}
	return done
}

// R5-1 and R5-2, design 2.4 (reference-counted domain ownership) and 2.7,
// as a property over any interleaving of breakpoint sets and removals,
// activation changes, Debugger enables and disables, lease releases,
// provisional targets, their setups, commits and destruction, where a
// change can land during a setup's replay and a setup or a commit during a
// change's send. The oracle is the ownership model of round 5 (docs/notes/
// w1b.md): after every step, every live target (active or provisional) has
// exactly the desired state, which is Debugger enabled while any owner
// holds it (an explicit enable, or implicitly a breakpoint or an activation
// other than the baseline), the most recent activation of a remaining owner
// (inactive without one) and the breakpoints of the remaining owners. No
// call fails, so no call was repeated on a target that already had its
// effect. After every owner released, no target has anything left.
func TestEveryTargetHasTheDesiredState(t *testing.T) {
	owners := []string{"s1", "s2"}
	prop := func(ops []uint16) bool {
		ctx := context.Background()
		p := &wkPage{active: "page-0", live: map[string]*wkTarget{"page-0": {bps: map[string]string{}}}, gone: map[string]error{}}
		conn := wkConn{p}
		d := NewDesiredState()
		if err := d.Apply(ctx, conn); err != nil {
			t.Log(err)
			return false
		}
		ps := &PageSession{State: d}
		type bp struct{ id, owner string }
		type act struct {
			on  bool
			seq int
		}
		desired := map[string]bp{}     // url -> breakpoint
		domain := map[string]bool{}    // Debugger owners
		activation := map[string]act{} // owner -> its latest activation
		seq := 0
		forget := func(owner string) {
			for u, b := range desired {
				if b.owner == owner {
					delete(desired, u)
				}
			}
			delete(activation, owner)
			delete(domain, owner)
		}
		fail := func(f string, a ...any) bool {
			t.Logf("after %s: "+f, append([]any{strings.Join(p.trace, "; ")}, a...)...)
			return false
		}
		var callErr error
		call := func(owner, method, params string) json.RawMessage {
			p.trace = append(p.trace, owner+" "+method+params)
			res, err := legacyCall(ctx, ps, SessionOwner("legacy", owner), conn, method, json.RawMessage(params))
			if err != nil && callErr == nil {
				callErr = err
			}
			return res
		}
		release := func(owner string) {
			p.trace = append(p.trace, "release "+owner)
			if _, err := releaseOwners(ctx, ps, owner, conn); err != nil && callErr == nil {
				callErr = err
			}
			forget(owner)
		}
		n := 0
		change := func(arg int) {
			owner := owners[arg%2]
			switch (arg / 2) % 6 {
			case 0, 1:
				n++
				url := fmt.Sprintf("https://fixture.example/%d.js", n)
				res := call(owner, "Debugger.setBreakpointByUrl", fmt.Sprintf(`{"url":%q,"lineNumber":1}`, url))
				var v struct {
					ID string `json:"breakpointId"`
				}
				if json.Unmarshal(res, &v) == nil && v.ID != "" {
					desired[url] = bp{v.ID, owner}
					domain[owner] = true
				}
			case 2:
				if us := slices.Sorted(maps.Keys(desired)); len(us) > 0 {
					u := us[arg%len(us)]
					call(desired[u].owner, "Debugger.removeBreakpoint", fmt.Sprintf(`{"breakpointId":%q}`, desired[u].id))
					delete(desired, u)
				}
			case 3:
				on := arg%3 != 0
				call(owner, "Debugger.setBreakpointsActive", fmt.Sprintf(`{"active":%v}`, on))
				seq++
				activation[owner] = act{on, seq}
				if on {
					domain[owner] = true
				}
			case 4:
				release(owner)
			case 5:
				// An owner's disable drops its Debugger state whether or not
				// it holds the domain, for example a baseline active:false
				// (review finding R6-3).
				call(owner, "Debugger.disable", "")
				forget(owner)
			}
		}
		check := func() bool {
			wantURLs := slices.Sorted(maps.Keys(desired))
			var eff act
			for _, a := range activation {
				if a.seq > eff.seq {
					eff = a
				}
			}
			for id, tg := range p.live {
				var urls []string
				for _, u := range tg.bps {
					urls = append(urls, u)
				}
				sort.Strings(urls)
				if tg.enabled != (len(domain) > 0) || tg.active != eff.on || !slices.Equal(urls, wantURLs) {
					return fail("%s: enabled %v active %v breakpoints %v; want enabled %v active %v breakpoints %v", id, tg.enabled, tg.active, urls, len(domain) > 0, eff.on, wantURLs)
				}
			}
			return true
		}
		provisional := func() []string {
			var out []string
			for id := range p.live {
				if id != p.active {
					out = append(out, id)
				}
			}
			sort.Strings(out)
			return out
		}
		commit := func(target string) {
			p.trace = append(p.trace, "commit "+target)
			delete(p.live, p.active)
			p.gone[p.active] = webkit.ErrTargetSwapped
			p.active = target
			d.commitTarget(target)
		}
		abandon := func(target string) {
			p.trace = append(p.trace, "destroy "+target)
			delete(p.live, target)
			p.gone[target] = webkit.ErrTargetAbandoned
			d.abandonTarget(target)
		}
		setup := func(i int) {
			target := fmt.Sprintf("page-%d", i+1)
			p.live[target] = &wkTarget{bps: map[string]string{}}
			p.trace = append(p.trace, "setup "+target)
			_ = d.Apply(ctx, wkTargetCaller{p, target}) // fails when the target is destroyed during it
		}
		for i, op := range ops {
			arg := int(op >> 4)
			switch op % 6 {
			case 0, 1: // a change; a setup or a commit can land during its send
				if (op>>3)%2 == 1 {
					p.hookAt, p.hook = p.sends+1, func() {
						if ts := provisional(); len(ts) > 0 && arg%2 == 0 {
							commit(ts[0])
						} else {
							setup(i + 1000)
						}
					}
				}
				change(arg)
			case 2: // a setup; a change, its own commit or destruction can land during it
				target := fmt.Sprintf("page-%d", i+1)
				p.hookAt = p.sends + 1 + arg%4
				var parked <-chan struct{}
				switch (arg / 4) % 4 {
				case 0:
					// A change runs in its operation's goroutine, not in the
					// setup's; it waits for the setup to send its items.
					p.hook = func() { parked = parkedChange(d, target, func() { change(arg / 16) }) }
				case 1:
					p.hook = func() { commit(target) }
				case 2:
					p.hook = func() { abandon(target) }
				default:
					p.hook = nil
				}
				setup(i)
				if parked != nil {
					select {
					case <-parked:
					case <-time.After(10 * time.Second):
						return fail("the change made during the setup of %s never ended", target)
					}
				}
			case 3:
				if ts := provisional(); len(ts) > 0 {
					commit(ts[arg%len(ts)])
				}
			case 4:
				if ts := provisional(); len(ts) > 0 {
					abandon(ts[arg%len(ts)])
				}
			case 5:
				owner := owners[arg%2]
				call(owner, "Debugger.enable", "")
				domain[owner] = true
			}
			p.hook = nil
			if callErr != nil {
				return fail("a call failed: %v", callErr)
			}
			if !check() {
				return false
			}
		}
		for _, owner := range owners {
			release(owner)
		}
		if callErr != nil {
			return fail("a release failed: %v", callErr)
		}
		if left := d.AllOwners(); len(left) > 0 {
			return fail("owners left after every release: %v", left)
		}
		return check()
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
}
