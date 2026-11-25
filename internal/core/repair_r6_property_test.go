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
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// The ownership contract of design 2.4 with the round-6 rules (review
// findings R6-2 to R6-4 and the orchestrator's one-cleanup-path rule), as
// a property over the Debugger, Network and Page domains and three owners:
// any interleaving of explicit enables and disables, breakpoint sets and
// removals, activations (including the baseline active:false, which does
// not acquire Debugger), extra headers (which acquire Network for their
// owner, with or without its Network.enable), user-agent overrides (which
// acquire Page; the baseline does not), bootstrap parts (a requirement of
// Page like an owner; an owner's Page.disable keeps its part, its release
// removes it), lease releases, provisional setups
// (with a change landing during one), commits, destruction, reconnects,
// and two concurrent changes where the first is held while it sends a
// provisional target's queue and the second, queued behind it, may be
// rejected there, and restores: a target that a commit replaced comes back
// with the backend and agents it kept (WebKit's back/forward cache), gets
// a setup, and may commit again. A reconnect keeps the back/forward cache
// or evicts it: a target restored after a reconnect comes back with agents
// that the new connection never enabled and has no record of.
//
// Oracle (independent of the implementation): after every operation,
// every live target has exactly the effective state of the remaining
// owners: Debugger enabled while any owner holds it, the most recent
// activation of a remaining owner (inactive without one), the remaining
// breakpoints, Network enabled while any owner holds it, the union of
// the remaining owners' headers, where the owner that sorts first wins a
// name (DesiredState.EffectiveHeaders), Page enabled while any owner holds
// it or a bootstrap part exists, the most recent user agent of a remaining
// owner (none without one), and a bootstrap script with exactly the
// remaining parts. An owner's <Domain>.disable drops
// all of its state of that domain, whether or not it held the domain; a
// release drops all of it. A change's result reflects its own sends: no
// change fails except one whose own send was rejected, and that one fails
// with the rejection naming the target, which is then destroyed. After
// every owner released, nothing is left anywhere.
func TestEveryLiveTargetHasTheRemainingOwnersState(t *testing.T) {
	owners := []string{"s1", "s2", "s3"}
	names := []string{"X-A", "X-B", "X-C"}
	fresh := func() *wkTarget { return &wkTarget{bps: map[string]string{}} }
	// Generator reach: B queued behind a held A, and B rejected by then.
	var queuedBehind, rejectedBehind int
	// Generator reach: headers and user agents set by an owner that did not
	// hold Network or Page before.
	var implicitNetwork, implicitPage int
	// Generator reach: a first part set while no owner held Page, and a
	// restored backend that kept enabled agents.
	var partsWithoutPage, restoredWithAgents int
	// Generator reach: a target restored under a new connection whose
	// backend kept agents that this connection never enabled.
	var restoredUnknown int
	prop := func(ops []uint32) bool {
		ctx := context.Background()
		p := &wkPage{active: "page-0", live: map[string]*wkTarget{"page-0": fresh()}, gone: map[string]error{}}
		conn := wkConn{p}
		d := NewDesiredState()
		if err := d.Apply(ctx, conn); err != nil {
			t.Log(err)
			return false
		}
		ps := &PageSession{State: d, Sched: NewScheduler(conn, SchedulerOptions{})}

		type bp struct{ id, owner string }
		type act struct {
			on  bool
			seq int
		}
		dbg, net, pg := map[string]bool{}, map[string]bool{}, map[string]bool{}
		type ua struct {
			value string
			seq   int
		}
		uas := map[string]ua{}
		parts := map[string]string{} // owner -> its bootstrap part's source
		cache := map[string]*wkTarget{}
		bps := map[string]bp{} // url -> breakpoint
		acts := map[string]act{}
		hdrs := map[string]map[string]string{}
		seq, n := 0, 0
		forgetDebugger := func(o string) {
			for u, b := range bps {
				if b.owner == o {
					delete(bps, u)
				}
			}
			delete(acts, o)
			delete(dbg, o)
		}
		forgetNetwork := func(o string) {
			delete(hdrs, o)
			delete(net, o)
		}
		forgetPage := func(o string) {
			delete(uas, o)
			delete(pg, o)
		}
		fail := func(f string, a ...any) bool {
			t.Logf("after %s: "+f, append([]any{strings.Join(p.trace, "; ")}, a...)...)
			return false
		}
		call := func(owner, method, params string) (json.RawMessage, error) {
			p.mu.Lock()
			p.trace = append(p.trace, owner+" "+method+params)
			p.mu.Unlock()
			return legacyCall(ctx, ps, SessionOwner("legacy", owner), conn, method, json.RawMessage(params))
		}
		// change makes one change (one legacy call) of owner for arg. It
		// returns the change's result and apply, which records its effect
		// in the model; the page accepted it even when a provisional target
		// rejected it. A tagged
		// change (tag != nil) is a breakpoint set or a header set, and
		// passes tag a token that only its own sends carry before it sends.
		change := func(owner string, arg int, tag func(string)) (func(), error) {
			kind := (arg / 3) % 13
			if tag != nil {
				kind = 0
				if arg%2 == 0 {
					kind = 7
				}
			}
			switch kind {
			case 0:
				n++
				url := fmt.Sprintf("https://fixture.example/%d.js", n)
				if tag != nil {
					tag(fmt.Sprintf("/%d.js\"", n))
				}
				res, err := call(owner, "Debugger.setBreakpointByUrl", fmt.Sprintf(`{"url":%q,"lineNumber":1}`, url))
				var v struct {
					ID string `json:"breakpointId"`
				}
				return func() {
					if json.Unmarshal(res, &v) == nil && v.ID != "" {
						bps[url] = bp{v.ID, owner}
						dbg[owner] = true
					}
				}, err
			case 1:
				us := slices.Sorted(maps.Keys(bps))
				if len(us) == 0 {
					return func() {}, nil
				}
				u := us[arg%len(us)]
				_, err := call(bps[u].owner, "Debugger.removeBreakpoint", fmt.Sprintf(`{"breakpointId":%q}`, bps[u].id))
				return func() { delete(bps, u) }, err
			case 2:
				on := (arg/27)%3 != 0
				_, err := call(owner, "Debugger.setBreakpointsActive", fmt.Sprintf(`{"active":%v}`, on))
				return func() {
					seq++
					acts[owner] = act{on, seq}
					if on {
						dbg[owner] = true
					}
				}, err
			case 3:
				_, err := call(owner, "Debugger.enable", "")
				return func() { dbg[owner] = true }, err
			case 4:
				_, err := call(owner, "Debugger.disable", "")
				return func() { forgetDebugger(owner) }, err
			case 5:
				_, err := call(owner, "Network.enable", "")
				return func() { net[owner] = true }, err
			case 6:
				_, err := call(owner, "Network.disable", "")
				return func() { forgetNetwork(owner) }, err
			case 7:
				h := map[string]string{}
				for i, name := range names {
					if (arg>>(5+i))%2 == 1 {
						n++
						h[name] = fmt.Sprintf("v%d", n)
					}
				}
				if tag != nil {
					n++
					h[names[arg%len(names)]] = fmt.Sprintf("v%d", n)
					tag(fmt.Sprintf("\"v%d\"", n))
				}
				if len(h) > 0 && !net[owner] {
					implicitNetwork++
				}
				raw, _ := json.Marshal(map[string]any{"headers": h})
				_, err := call(owner, "Network.setExtraHTTPHeaders", string(raw))
				return func() {
					if len(h) == 0 {
						delete(hdrs, owner)
					} else {
						hdrs[owner] = h
						net[owner] = true // implicit acquisition
					}
				}, err
			case 8:
				_, err := call(owner, "Page.enable", "")
				return func() { pg[owner] = true }, err
			case 9:
				_, err := call(owner, "Page.disable", "")
				return func() { forgetPage(owner) }, err
			case 10:
				v, params := "", "{}" // no value: the baseline (the rule's clear)
				if (arg/27)%3 != 0 {
					n++
					v = fmt.Sprintf("ua%d", n)
					if !pg[owner] {
						implicitPage++
					}
					params = fmt.Sprintf(`{"value":%q}`, v)
				}
				_, err := call(owner, "Page.overrideUserAgent", params)
				return func() {
					seq++
					uas[owner] = ua{v, seq}
					if v != "" {
						pg[owner] = true // implicit acquisition; the baseline acquires nothing
					}
				}, err
			case 11:
				src := "" // the part's removal
				if (arg/27)%3 != 0 {
					n++
					src = fmt.Sprintf("globalThis.part%d = 1;", n)
					if len(pg) == 0 && len(parts) == 0 {
						partsWithoutPage++
					}
				}
				raw, _ := json.Marshal(map[string]string{"source": src})
				_, err := call(owner, "Page.setBootstrapScript", string(raw))
				return func() {
					if src == "" {
						delete(parts, owner)
					} else {
						parts[owner] = src
					}
				}, err
			default:
				p.mu.Lock()
				p.trace = append(p.trace, "release "+owner)
				p.mu.Unlock()
				_, err := releaseOwners(ctx, ps, owner, conn)
				return func() {
					forgetDebugger(owner)
					forgetNetwork(owner)
					forgetPage(owner)
					delete(parts, owner)
				}, err
			}
		}
		check := func() bool {
			var eff act
			for _, a := range acts {
				if a.seq > eff.seq {
					eff = a
				}
			}
			var effUA ua
			for _, u := range uas {
				if u.seq > effUA.seq {
					effUA = u
				}
			}
			wantURLs := slices.Sorted(maps.Keys(bps))
			wantHeaders := map[string]string{}
			if len(net) > 0 {
				os := slices.Sorted(maps.Keys(hdrs))
				for i := len(os) - 1; i >= 0; i-- {
					maps.Copy(wantHeaders, hdrs[os[i]])
				}
			}
			for id, tg := range p.live {
				urls := slices.Sorted(maps.Values(tg.bps))
				if tg.enabled != (len(dbg) > 0) || tg.active != eff.on || !slices.Equal(urls, wantURLs) {
					return fail("%s: Debugger enabled %v active %v breakpoints %v; want enabled %v active %v breakpoints %v",
						id, tg.enabled, tg.active, urls, len(dbg) > 0, eff.on, wantURLs)
				}
				if tg.net != (len(net) > 0) || !maps.Equal(tg.headers, wantHeaders) || tg.intercept || len(tg.rules) > 0 || tg.caching || tg.conds != "" {
					return fail("%s: Network %s; want enabled %v headers %v", id, networkState(tg), len(net) > 0, wantHeaders)
				}
				var got struct {
					Value string `json:"value"`
				}
				_ = json.Unmarshal([]byte(tg.overrides["Page.overrideUserAgent"]), &got)
				if tg.page != (len(pg) > 0 || len(parts) > 0) || got.Value != effUA.value || len(tg.overrides) > 1 {
					return fail("%s: Page %s; want enabled %v user agent %q", id, pageState(tg), len(pg) > 0 || len(parts) > 0, effUA.value)
				}
				if strings.Count(tg.script, "// iwdp bootstrap part: ") != len(parts) {
					return fail("%s: bootstrap script %q; want the parts %v", id, tg.script, parts)
				}
				for _, src := range parts {
					if !strings.Contains(tg.script, src) {
						return fail("%s: bootstrap script %q; want the parts %v", id, tg.script, parts)
					}
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
			old := p.active
			cache[old] = p.live[old] // the back/forward cache keeps its backend
			delete(p.live, old)
			p.gone[old] = webkit.ErrTargetSwapped
			p.active = target
			d.commitTarget(target)
			d.abandonTarget(old) // Target.targetDestroyed of the old target
		}
		abandon := func(target string) {
			p.trace = append(p.trace, "destroy "+target)
			delete(p.live, target)
			p.gone[target] = webkit.ErrTargetAbandoned
			d.abandonTarget(target)
		}
		setup := func(target string) {
			p.live[target] = fresh()
			p.trace = append(p.trace, "setup "+target)
			_ = d.Apply(ctx, wkTargetCaller{p, target}) // fails when the target is destroyed during it
		}
		var opErr error
		note := func(apply func(), err error) {
			apply()
			if err != nil && opErr == nil {
				opErr = err
			}
		}
		for i, op := range ops {
			arg := int(op >> 4)
			owner := owners[arg%len(owners)]
			target := fmt.Sprintf("page-%d", i+1)
			switch op % 9 {
			case 0, 1: // a change; a setup or a commit can land during its send
				if (op>>3)%2 == 1 {
					p.hookAt, p.hook = p.sends+1, func() {
						if ts := provisional(); len(ts) > 0 && arg%2 == 0 {
							commit(ts[0])
						} else {
							setup(target)
						}
					}
				}
				note(change(owner, arg/3, nil))
			case 2: // a setup; a change, its commit or its destruction can land during it
				p.hookAt = p.sends + 1 + arg%4
				var parked <-chan struct{}
				switch (arg / 4) % 4 {
				case 0:
					p.hook = func() { parked = parkedChange(d, target, func() { note(change(owner, arg/16, nil)) }) }
				case 1:
					p.hook = func() { commit(target) }
				case 2:
					p.hook = func() { abandon(target) }
				}
				setup(target)
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
			case 5: // reconnect: a new connection replaces every target
				p.trace = append(p.trace, "reconnect "+target)
				for id := range p.live {
					p.gone[id] = webkit.ErrTargetSwapped
				}
				p.live, p.active = map[string]*wkTarget{target: fresh()}, target
				p.rec = nil // a new Session has no record
				if arg%2 == 1 {
					cache = map[string]*wkTarget{} // the cached pages were evicted
				} // else the back/forward cache outlives the connection, with the agents it enabled
				note(func() {}, d.Apply(ctx, conn))
			case 8: // back or forward: a replaced target returns with its backend
				ids := slices.Sorted(maps.Keys(cache))
				if len(ids) == 0 {
					break
				}
				id := ids[arg%len(ids)]
				p.trace = append(p.trace, "restore "+id)
				if len(p.rec[id]) > 0 {
					restoredWithAgents++
				}
				if b := cache[id]; len(p.rec[id]) == 0 && (b.page || b.enabled || b.net) {
					restoredUnknown++ // new connection, restored target
				}
				p.live[id] = cache[id]
				delete(cache, id)
				delete(p.gone, id)
				if err := d.Apply(ctx, wkTargetCaller{p, id}); err != nil {
					return fail("setup of the restored %s: %v", id, err)
				}
				if arg%2 == 0 {
					commit(id)
				}
			default:
				// Change A (owner) is held at its first send to a
				// provisional target tg; change B (another owner, a
				// breakpoint or header set) queues behind it and, for odd
				// arg/2, is rejected by tg.
				ts := provisional()
				if len(ts) == 0 {
					setup(target)
					if ts = provisional(); len(ts) == 0 {
						break
					}
				}
				tg := ts[arg%len(ts)]
				block := &wkBlock{target: tg, hit: make(chan struct{}), release: make(chan struct{})}
				p.block = block
				p.trace = append(p.trace, "hold the first send to "+tg)
				type result struct {
					apply func()
					err   error
				}
				first := make(chan result, 1)
				go func() {
					apply, err := change(owner, arg/4, nil)
					first <- result{apply, err}
				}()
				var a result
				held := false
				select {
				case <-block.hit:
					held = true
				case a = <-first:
				case <-time.After(10 * time.Second):
					return fail("change A neither sent to %s nor ended", tg)
				}
				p.mu.Lock()
				p.block = nil
				p.mu.Unlock()
				var reject *wkFail
				mark := func(string) {}
				if (arg/2)%2 == 1 {
					reject = &wkFail{target: tg}
					mark = func(token string) {
						p.mu.Lock()
						reject.token, p.fail = token, reject
						p.mu.Unlock()
					}
				}
				var b result
				ownerB := owners[(arg+1)%len(owners)]
				second := func() { b.apply, b.err = change(ownerB, arg/64, mark) }
				if held {
					before := queuedFor(d, tg)
					parked := parkedChange(d, tg, second)
					if queuedFor(d, tg) > before {
						queuedBehind++
						if reject != nil {
							rejectedBehind++
						}
					}
					close(block.release)
					select {
					case a = <-first:
					case <-time.After(10 * time.Second):
						return fail("the held change A never ended")
					}
					select {
					case <-parked:
					case <-time.After(10 * time.Second):
						return fail("the queued change B never ended")
					}
				} else {
					second()
				}
				p.mu.Lock()
				p.fail = nil
				fired := reject != nil && reject.fired
				p.mu.Unlock()
				a.apply()
				b.apply()
				if a.err != nil {
					return fail("change A failed: %v (B rejected by %s: %v)", a.err, tg, fired)
				}
				if fired != (b.err != nil) || fired && (!errors.Is(b.err, errInjected) || !strings.Contains(b.err.Error(), tg)) {
					return fail("change B returned %v; %s rejected its send: %v", b.err, tg, fired)
				}
				if fired {
					abandon(tg) // tg lacks B's state
				}
			}
			p.hook = nil
			if opErr != nil {
				return fail("an operation failed: %v", opErr)
			}
			if !check() {
				return false
			}
		}
		for _, owner := range owners {
			p.trace = append(p.trace, "release "+owner)
			_, err := releaseOwners(ctx, ps, owner, conn)
			note(func() {
				forgetDebugger(owner)
				forgetNetwork(owner)
				forgetPage(owner)
				delete(parts, owner)
			}, err)
		}
		if opErr != nil {
			return fail("a release failed: %v", opErr)
		}
		if left := d.AllOwners(); len(left) > 0 {
			return fail("owners left after every release: %v", left)
		}
		return check()
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 1500}); err != nil {
		t.Fatal(err)
	}
	t.Logf("B queued behind a held A %d times, %d of them rejected", queuedBehind, rejectedBehind)
	if queuedBehind == 0 || rejectedBehind == 0 {
		t.Fatal("the generator never queued a change behind a held one, or never rejected it")
	}
	t.Logf("headers set without Network %d times, user agents set without Page %d times", implicitNetwork, implicitPage)
	if implicitNetwork == 0 || implicitPage == 0 {
		t.Fatal("the generator never set headers without Network or a user agent without Page")
	}
	t.Logf("first parts without a Page owner %d times, restored backends with enabled agents %d times", partsWithoutPage, restoredWithAgents)
	if partsWithoutPage == 0 || restoredWithAgents == 0 {
		t.Fatal("the generator never set a first part without a Page owner, or never restored a backend with enabled agents")
	}
	t.Logf("targets restored under a new connection with agents it never enabled %d times", restoredUnknown)
	if restoredUnknown == 0 {
		t.Fatal("the generator never restored a target under a new connection with agents enabled by the earlier one")
	}
}
