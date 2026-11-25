package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// Domain ownership of owned state (design 2.4: domains are reference
// counted, so a domain stays enabled while anything still needs it).
//
//   - Implicit ownership: a legacy call that sets state which needs a
//     domain acquires that domain for its owner when the owner does not hold
//     it yet (acquireFor): Debugger state, Page overrides and the Network
//     state (headers, interception, the caching override and emulated
//     conditions). The first owner's acquisition sends
//     <Domain>.enable, so the domain is enabled exactly while it is needed
//     (DesiredState.Needs: it has owners or, for Page, a bootstrap part),
//     on the active target and on every new or provisional target (Apply).
//     A target restored with the agents it kept, or any agent the
//     connection does not know, is reset first, so no backend gets a
//     second enable (resetCalls, webkit.SessionOptions.AgentReset).
//   - Explicit disable: an owner's own <Domain>.disable drops every
//     contribution of that owner to the domain, whether or not it holds a
//     reference (a baseline value does not acquire the domain): its entries,
//     shared values and, for Network, its headers, interception, caching
//     override and emulated conditions, sent one by one to every target
//     that holds them (dropOwner, the same path as a lease release), then
//     its reference. The disable reaches the page only when no owner
//     remains (disableDomain).
//     The resets are sent before a last disable too, because Debugger.disable
//     keeps the pause on exceptions and on debugger statements (they live in
//     JSC::Debugger, not in the agent state that disable clears).
//   - Lease release (DesiredState.Release) does the same for every domain,
//     entry and shared value of the session's owners.
//
// Shared values: a stateRule that is replayed and names no node or script
// id sets one value per key, for example the breakpoint activation or the
// pause-on-exceptions state. Each owner's latest value of a key is kept with
// a sequence number, and the most recent value of any owner is effective.
// When an owner releases, the key falls back to the most recent value of a
// remaining owner, or to WebKit's baseline (the rule's reset) when none
// remains. A call that sets the baseline (the rule's clear, or a stateClear)
// is a value too: it stays effective until its owner releases.

// ruleDomain is the domain that state set by method belongs to ("" for
// none): an owner's own <Domain>.disable drops that owner's state of the
// domain (disableDomain), and the state acquires the domain for its owner
// when acquiresImplicitly allows it. Every stateRule maps to the domain
// whose disable clears it in WebKit:
//   - Debugger: Debugger.disable clears breakpoints, the activation and the
//     assertion and microtask pauses, and the DOMDebugger breakpoints with
//     them (WebKit-7615.3.1 InspectorDebuggerAgent.cpp L271-287 and
//     L1787-1800, InspectorDOMDebuggerAgent.cpp L76-100,
//     PageDOMDebuggerAgent.cpp L58-69).
//   - Page: Page.disable resets the user-agent, media, appearance, setting
//     and preference overrides and the paint rects (InspectorPageAgent
//     disable).
//   - CSS: CSS.disable resets the forced pseudo states (InspectorCSSAgent).
//   - DOM has no enable or disable in WebKit, so the highlight and inspect
//     mode state belongs to no domain; it ends with its owner's release.
func ruleDomain(method string) string {
	dom, _, _ := strings.Cut(method, ".")
	switch dom {
	case "Debugger", "DOMDebugger":
		return "Debugger"
	case "Page", "CSS":
		return dom
	}
	return ""
}

// acquiresImplicitly reports whether state of dom acquires dom for its
// owner (implicit ownership). CSS does not: CSS.enable hangs through iwdp
// Target routing (CLAUDE.md, Known Limitations) and CSS.forcePseudoState
// works without it, so a forced pseudo state only belongs to CSS for the
// owner's own CSS.disable.
func acquiresImplicitly(dom string) bool { return dom != "" && dom != "CSS" }

// sharedRule reports whether rule sets a shared value (see above).
func sharedRule(rule stateRule) bool { return rule.replay && !rule.scoped }

// sharedVal is one owner's latest value of a shared key.
type sharedVal struct {
	owner  Owner
	seq    uint64
	rule   string // the stateRules method of the key
	method string // the call that set the value
	params string // its parameters, canonical JSON
	base   bool   // the call sets WebKit's baseline
}

// acquireFor gives owner the domain dom that state it sets needs (implicit
// ownership). The first owner's acquisition sends <dom>.enable through c.
func acquireFor(ctx context.Context, d *DesiredState, owner Owner, c Sender, dom string) error {
	if !acquiresImplicitly(dom) || !d.Acquire(owner, dom) {
		return nil
	}
	if _, err := c.Send(ctx, dom+".enable", nil); err != nil {
		releaseFailedAcquire(d, owner, dom, err)
		return err
	}
	return nil
}

// releaseFailedAcquire undoes owner's acquisition of dom after its enable
// failed, but only when the enable provably had no effect (noEffect). When
// the outcome is unknown, for example the enable was written and its reply
// lost to a cancellation, owner keeps dom, so that its release or its own
// disable sends <dom>.disable (review finding R6-6).
func releaseFailedAcquire(d *DesiredState, owner Owner, dom string, err error) {
	if noEffect(err) {
		d.ReleaseDomain(owner, dom)
	}
}

// setShared records owner's value of the shared key and, for a change made
// through c, queues its call for the provisional targets in the same
// critical section, so that a setup snapshot has either both or neither.
func (d *DesiredState) setShared(c Sender, owner Owner, key, rule, method string, params map[string]any, base bool) {
	raw, _ := json.Marshal(params) // decoded from JSON, so it encodes
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return
	}
	d.ver++
	d.sharedSeq++
	if d.shared == nil {
		d.shared = map[string][]sharedVal{}
	}
	vals := slices.DeleteFunc(d.shared[key], func(v sharedVal) bool { return v.owner == owner })
	d.shared[key] = append(vals, sharedVal{owner: owner, seq: d.sharedSeq, rule: rule, method: method, params: string(raw), base: base})
	if cs, ok := c.(*changeSender); ok {
		cs.enqueueLocked(d.ver, "", forwardCall(method, json.RawMessage(raw)))
	}
}

// sharedSend is a call that makes a shared key's effective value current.
type sharedSend struct {
	method string
	params any
}

// takeSharedLocked removes owner's values of the keys whose rule matches
// and returns, for each key whose effective value changed, the call that
// sets the new one: the most recent remaining value, or the rule's reset
// when none remains (nothing when the removed value was the baseline). The
// caller holds d.mu.
func (d *DesiredState) takeSharedLocked(owner Owner, match func(rule string) bool) []sharedSend {
	keys := make([]string, 0, len(d.shared))
	for k := range d.shared {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []sharedSend
	for _, k := range keys {
		vals := d.shared[k]
		before := vals[len(vals)-1]
		if !match(before.rule) || !slices.ContainsFunc(vals, func(v sharedVal) bool { return v.owner == owner }) {
			continue
		}
		d.ver++
		vals = slices.DeleteFunc(vals, func(v sharedVal) bool { return v.owner == owner })
		if len(vals) == 0 {
			delete(d.shared, k)
			if !before.base {
				var p map[string]any
				_ = json.Unmarshal([]byte(before.params), &p) // recorded by setShared
				m, rp := stateRules[before.rule].reset(p)
				out = append(out, sharedSend{m, rp})
			}
			continue
		}
		d.shared[k] = vals
		if after := vals[len(vals)-1]; after.method != before.method || after.params != before.params {
			out = append(out, sharedSend{after.method, json.RawMessage(after.params)})
		}
	}
	return out
}

// sharedEntriesLocked returns the effective shared values that a new or
// provisional target needs, as entries for Apply. The caller holds d.mu.
func (d *DesiredState) sharedEntriesLocked() []Entry {
	var out []Entry
	for k, vals := range d.shared {
		eff := vals[len(vals)-1]
		if eff.base {
			continue // a new backend starts at the baseline
		}
		m, p := eff.method, json.RawMessage(eff.params)
		out = append(out, Entry{
			Key: k, Rank: stateRules[eff.rule].rank, Owner: eff.owner, Domain: ruleDomain(eff.rule),
			Apply: func(ctx context.Context, s Sender) error {
				_, err := s.Send(ctx, m, p)
				return err
			},
		})
	}
	return out
}

// sendShared sends the calls of takeSharedLocked and reports each failure.
func sendShared(ctx context.Context, s Sender, sends []sharedSend, note func(error)) {
	for _, x := range sends {
		_, err := s.Send(ctx, x.method, x.params)
		note(err)
	}
}

// disableDomain handles owner's own <dom>.disable (method, with params p)
// in the change c: owner's contributions to dom are dropped whether or not
// owner holds a reference to dom (dropOwner; for Network with the Network
// reinitialization barrier when a header name lost its last owner), and
// the disable reaches the page only when no owner of dom remains.
func disableDomain(ctx context.Context, ps *PageSession, owner Owner, c Sender, dom, method string, p any) (json.RawMessage, error) {
	d := ps.State
	res, _, rerr := d.dropOwner(ctx, owner, dom, c)
	if rerr == nil && res.NeedsNetworkReset {
		rerr = NetworkReset(ctx, ps.Sched, c, d)
	}
	if rerr != nil {
		rerr = fmt.Errorf("%s: releasing the %s state of %s: %w", method, dom, owner, rerr)
	}
	if d.Needs(dom) {
		return json.RawMessage("{}"), rerr // another owner or a bootstrap part still needs the domain
	}
	out, err := c.Send(ctx, method, p)
	if err != nil && notEnabled(err) {
		out, err = json.RawMessage("{}"), nil
	}
	if err != nil || rerr != nil {
		return out, errors.Join(rerr, err)
	}
	return out, nil
}

// noEffect reports a failed call that provably changed nothing on the
// page: it was never written (webkit.ErrNotSent), or the page answered it
// with a protocol error. Any other failure (a lost reply, a cancellation
// after the write) leaves the outcome unknown.
func noEffect(err error) bool {
	var pe *webkit.ErrorData
	return errors.Is(err, webkit.ErrNotSent) || errors.As(err, &pe)
}

// notEnabled reports the page's refusal of a disable because the domain
// is not enabled ("... not enabled", "... must be enabled", "... already
// disabled"). Its state is
// the one the disable wants, for example after an enable whose outcome was
// unknown and that never took effect.
func notEnabled(err error) bool { return webkit.NotEnabled(err) }

// sendDisable sends a disable whose domain has no owner left; a refusal
// because the domain is not enabled is no failure (notEnabled).
func sendDisable(ctx context.Context, s Sender, method string, p any) error {
	if _, err := s.Send(ctx, method, p); err != nil && !notEnabled(err) {
		return err
	}
	return nil
}
