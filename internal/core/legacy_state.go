package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Network settings and owned page state that legacy calls set (design 2.4
// and 2.10). Network.disable clears the Network settings in WebKit, so the
// Network reinitialization barrier applies them again (ApplyNetwork).

type condEntry struct {
	seq    uint64
	params json.RawMessage
}

// interceptOnLocked: interception is on while any owner enabled it or has a
// rule (a rule works only with interception on).
func (d *DesiredState) interceptOnLocked() bool {
	if len(d.intercept) > 0 {
		return true
	}
	for _, rs := range d.rules {
		if len(rs) > 0 {
			return true
		}
	}
	return false
}

// SetInterception records whether owner needs interception enabled. It
// returns the value to send when the effective enablement changed, else
// nil: a "disable" is not sent while another owner needs interception.
func (d *DesiredState) SetInterception(owner Owner, on bool) *bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return nil
	}
	d.ver++
	before := d.interceptOnLocked()
	if on {
		d.intercept[owner] = true
	} else {
		delete(d.intercept, owner)
	}
	if after := d.interceptOnLocked(); after != before {
		return &after
	}
	return nil
}

// missingInterceptRuleError is WebKit's error text for
// Network.removeInterception of a rule the target does not hold
// (InspectorNetworkAgent::removeInterception; internal/devtools
// tombstone.go missingRuleError has the sources).
const missingInterceptRuleError = "Missing intercept for given url, given isRegex, and given stage"

// MissingInterceptRule reports whether err is WebKit's refusal of a
// Network.removeInterception because the target does not hold the rule:
// the rule is absent, which is what the removal wants.
func MissingInterceptRule(err error) bool {
	var pe *webkit.ErrorData
	return errors.As(err, &pe) && pe.Message == missingInterceptRuleError
}

// SetCachingDisabled records whether owner needs the resource cache
// disabled (Network.setResourceCachingDisabled). It returns the value to
// send when the effective setting changed, else nil.
func (d *DesiredState) SetCachingDisabled(owner Owner, disabled bool) *bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return nil
	}
	d.ver++
	before := len(d.caching) > 0
	if disabled {
		d.caching[owner] = true
	} else {
		delete(d.caching, owner)
	}
	if after := len(d.caching) > 0; after != before {
		return &after
	}
	return nil
}

// conditionKeys are the parameters of WIP Network.setEmulatedConditions
// that set a condition: bytesPerSecondLimit (iOS 16.4 to 17.x), bandwidth
// and latency (iOS 27.3 and WebKit main; WebKit commit c4a7ebcc295f).
var conditionKeys = []string{"bytesPerSecondLimit", "bandwidth", "latency"}

// limitsConditions reports whether Network.setEmulatedConditions params p
// set a condition.
func limitsConditions[V any](p map[string]V) bool {
	for _, k := range conditionKeys {
		if _, ok := p[k]; ok {
			return true
		}
	}
	return false
}

// SetEmulatedConditions records owner's Network.setEmulatedConditions
// parameters; parameters without a condition (conditionKeys) remove owner's
// conditions. The latest owner's conditions are effective. It returns the
// parameters to send when the effective conditions changed ({} for none),
// or when an earlier send had an unknown outcome (condsUnsure), and undo,
// which puts owner's record back as it was: the caller runs it when WebKit
// refused the send (noEffect), so that the record holds only what WebKit
// holds and a retry sends the call again (parity-045 review F4).
func (d *DesiredState) SetEmulatedConditions(owner Owner, params json.RawMessage) (send json.RawMessage, undo func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return nil, func() {}
	}
	d.ver++
	before := d.effectiveCondsLocked()
	prev, had := d.conds[owner]
	var v map[string]json.RawMessage
	if json.Unmarshal(params, &v) == nil && limitsConditions(v) {
		d.condSeq++
		d.conds[owner] = condEntry{seq: d.condSeq, params: append(json.RawMessage(nil), params...)}
	} else {
		delete(d.conds, owner)
	}
	undo = func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.ver++
		if had {
			d.conds[owner] = prev
		} else {
			delete(d.conds, owner)
		}
	}
	if after := d.effectiveCondsLocked(); string(after) != string(before) || d.condsUnsure {
		if after == nil {
			return json.RawMessage("{}"), undo
		}
		return after, undo
	}
	return nil, undo
}

// SentEmulatedConditions records the outcome err of a send that
// SetEmulatedConditions asked for, with that call's undo: a refusal
// (noEffect) puts owner's record back and changes nothing else, so an
// earlier unknown outcome stays unknown; after an unknown outcome the next
// SetEmulatedConditions sends the effective conditions even when they did
// not change; only an applied send makes the record certain.
func (d *DesiredState) SentEmulatedConditions(err error, undo func()) {
	refused := err != nil && noEffect(err)
	if refused {
		undo()
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.condsUnsure = err != nil
}

func (d *DesiredState) effectiveCondsLocked() json.RawMessage {
	var best condEntry
	for _, c := range d.conds {
		if c.seq > best.seq {
			best = c
		}
	}
	return best.params
}

// networkSettings is the Network state besides headers and rules.
type networkSettings struct {
	intercept bool
	caching   bool
	conds     json.RawMessage
}

func (d *DesiredState) networkSettingsLocked() networkSettings {
	return networkSettings{intercept: d.interceptOnLocked(), caching: len(d.caching) > 0, conds: d.effectiveCondsLocked()}
}

// applyNetworkSettings sends the caching override and the emulated
// conditions when they are set (interception is sent with its rules).
func applyNetworkSettings(ctx context.Context, s Sender, ns networkSettings) error {
	if ns.caching {
		if _, err := s.Send(ctx, "Network.setResourceCachingDisabled", map[string]bool{"disabled": true}); err != nil {
			return fmt.Errorf("resource caching override: %w", err)
		}
	}
	if ns.conds != nil {
		if _, err := s.Send(ctx, "Network.setEmulatedConditions", ns.conds); err != nil {
			return fmt.Errorf("emulated conditions: %w", err)
		}
	}
	return nil
}

// releaseNetworkSettings sends what changed between before and after a
// release: interception off, caching on again, the remaining conditions.
func releaseNetworkSettings(ctx context.Context, s Sender, before, after networkSettings, note func(error)) {
	if before.intercept && !after.intercept {
		_, err := s.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": false})
		note(err)
	}
	if before.caching && !after.caching {
		_, err := s.Send(ctx, "Network.setResourceCachingDisabled", map[string]bool{"disabled": false})
		note(err)
	}
	if string(before.conds) != string(after.conds) {
		p := after.conds
		if p == nil {
			p = json.RawMessage("{}")
		}
		_, err := s.Send(ctx, "Network.setEmulatedConditions", p)
		note(err)
	}
}

// Ownership of URL breakpoints (design 2.4 and 2.7). Each target that
// holds a copy of a client's breakpoint is recorded with the id it gave the
// copy: the active target in bpIDs, a provisional target in staged (with
// the sender that reaches it in stagedVia) until it commits or is
// destroyed. The client's entry stays the single authority: a copy is
// recorded only while the entry exists, and removing or releasing the
// entry takes every recorded copy and removes it on its own target. A
// replay that completes after its entry ended removes its copy itself.

// breakpointKey is the generic-entry key of the breakpoint a client knows
// as id.
func breakpointKey(id string) string {
	return entryKey("Debugger.breakpoint", param(map[string]any{"breakpointId": id}, "breakpointId"))
}

// setBreakpointID records that the URL breakpoint a client knows as id
// has the id cur on target ("" = the connection's active target), which
// via reaches. The id of a provisional target is staged until that target
// commits. It records nothing and returns false when the client's entry no
// longer exists (removed or released while the replay was in flight); the
// caller then removes cur from target.
func (d *DesiredState) setBreakpointID(target, id, cur string, via Sender) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[breakpointKey(id)]; !ok {
		return false
	}
	if target != "" && target != d.active {
		if d.staged == nil {
			d.staged = map[string]map[string]string{}
			d.stagedVia = map[string]Sender{}
		}
		if d.staged[target] == nil {
			d.staged[target] = map[string]string{}
		}
		d.staged[target][id] = cur
		d.stagedVia[target] = via
		return true
	}
	if d.bpIDs == nil {
		d.bpIDs = map[string]string{}
	}
	d.bpIDs[id] = cur
	return true
}

// bpCopy is a copy of a client's URL breakpoint on a provisional target:
// the id that target gave it and the sender that reaches the target.
type bpCopy struct {
	target, id string
	via        Sender
}

// takeBreakpointID forgets every copy of the breakpoint a client knows as
// id. It returns the id to send to sentTo, the target a change addresses
// ("" = the active target): the active target's id, or id itself when it
// was never replayed. The copies that other targets hold come back for the
// caller to remove on their own target (removeBreakpointCopies). When a
// commit replaced sentTo after the change began, the active target's id is
// such a copy, on the committed target.
func (d *DesiredState) takeBreakpointID(id, sentTo string) (string, []bpCopy) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.bpIDs[id]
	delete(d.bpIDs, id)
	var copies []bpCopy
	if ok && sentTo != "" && sentTo != d.active {
		if ts := d.syncs[d.active]; ts != nil {
			copies = append(copies, bpCopy{target: d.active, id: cur, via: ts.via})
		}
		cur, ok = "", false
	}
	for target, ids := range d.staged {
		if c, ok := ids[id]; ok {
			copies = append(copies, bpCopy{target: target, id: c, via: d.stagedVia[target]})
			delete(ids, id)
		}
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].target < copies[j].target })
	if !ok {
		return id, copies
	}
	return cur, copies
}

// changeTargetOf is the target that a change's sender addresses ("" when s
// is not a change's sender).
func changeTargetOf(s Sender) string {
	if cs, ok := s.(interface{ changeTarget() string }); ok {
		return cs.changeTarget()
	}
	return ""
}

// removeBreakpointOn removes the breakpoint with id cur from the target s
// reaches. A target that was destroyed meanwhile (abandoned, or replaced by
// a commit) took its breakpoints with it, so its send error is no failure.
func removeBreakpointOn(ctx context.Context, s Sender, cur string) error {
	_, err := s.Send(ctx, "Debugger.removeBreakpoint", map[string]string{"breakpointId": cur})
	if errors.Is(err, webkit.ErrTargetAbandoned) || errors.Is(err, webkit.ErrTargetSwapped) {
		return nil
	}
	return err
}

// removeBreakpointCopies removes each copy on its own target.
func removeBreakpointCopies(ctx context.Context, copies []bpCopy) error {
	var errs []error
	for _, c := range copies {
		if err := removeBreakpointOn(ctx, c.via, c.id); err != nil {
			errs = append(errs, fmt.Errorf("removing breakpoint %s from page target %s: %w", c.id, c.target, err))
		}
	}
	return errors.Join(errs...)
}

// withCopiesRemoved returns err, or the copies' removal error when err is
// nil, or both.
func withCopiesRemoved(ctx context.Context, err error, copies []bpCopy) error {
	cerr := removeBreakpointCopies(ctx, copies)
	if cerr == nil {
		return err
	}
	return errors.Join(err, cerr)
}

// Remove deletes the generic entry key and returns it.
func (d *DesiredState) Remove(key string) (Entry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ver++
	e, ok := d.entries[key]
	delete(d.entries, key)
	return e, ok
}

// stateRule maps a legacy call that sets page state onto a generic entry
// (design 2.4): key names the state the call sets (one entry per key; the
// latest owner owns it; a shared value keeps every owner's latest value,
// domain_owners.go), clear reports a call that ends the state instead (for
// a shared value: sets WebKit's baseline),
// replay re-sends the call on a new target (state that WebKit resets on
// disconnect and that names no node id), scoped marks state that names a
// node id of the document (Entry.Scoped: dropped on a new target), and
// reset is the call that undoes it when its owner releases.
type stateRule struct {
	key    func(p map[string]any) string
	clear  func(p map[string]any) bool
	replay bool
	scoped bool
	reset  func(p map[string]any) (string, any)
	rank   int // Entry.Rank
	// beforeEnable marks a shared value that a target gets before its
	// <Domain>.enable (Entry.BeforeEnable), also when it is the baseline:
	// the pause settings that live in JSC::Debugger, so that a page never
	// runs with Debugger enabled and a pause setting it does not want.
	beforeEnable bool
}

func param(p map[string]any, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "|%v", p[n])
	}
	return b.String()
}

func keyBy(names ...string) func(map[string]any) string {
	return func(p map[string]any) string { return param(p, names...) }
}

func resetWith(method string, keep []string, set map[string]any) func(map[string]any) (string, any) {
	return func(p map[string]any) (string, any) {
		out := map[string]any{}
		for _, k := range keep {
			if v, ok := p[k]; ok {
				out[k] = v
			}
		}
		for k, v := range set {
			out[k] = v
		}
		return method, out
	}
}

func missing(name string) func(map[string]any) bool {
	return func(p map[string]any) bool { _, ok := p[name]; return !ok }
}

// missingOrEmpty is the clear of Page.overrideUserAgent: WebKit applies
// the user-agent override only when it is not empty (InspectorPageAgent),
// so an empty value is no override, as internal/devtools treats it.
func missingOrEmpty(name string) func(map[string]any) bool {
	return func(p map[string]any) bool { v, ok := p[name]; return !ok || v == "" }
}

func isFalse(name string) func(map[string]any) bool {
	return func(p map[string]any) bool { b, _ := p[name].(bool); return !b }
}

var hideHighlight = resetWith("DOM.hideHighlight", nil, nil)

// stateRules are the state-setting calls of the legacy tools and of the
// design 2.4 table (forced pseudo state, overlays, debugger settings and
// breakpoints, user-agent, media and preference overrides).
var stateRules = map[string]stateRule{
	"CSS.forcePseudoState": {key: keyBy("nodeId"), scoped: true, clear: func(p map[string]any) bool {
		l, _ := p["forcedPseudoClasses"].([]any)
		return len(l) == 0
	}, reset: resetWith("CSS.forcePseudoState", []string{"nodeId"}, map[string]any{"forcedPseudoClasses": []string{}})},
	"DOM.highlightNode":         {key: keyBy(), reset: hideHighlight},
	"DOM.highlightNodeList":     {key: keyBy(), reset: hideHighlight},
	"DOM.highlightRect":         {key: keyBy(), reset: hideHighlight},
	"DOM.highlightQuad":         {key: keyBy(), reset: hideHighlight},
	"DOM.highlightSelector":     {key: keyBy(), reset: hideHighlight},
	"DOM.highlightFrame":        {key: keyBy(), reset: hideHighlight},
	"DOM.setInspectModeEnabled": {key: keyBy(), clear: isFalse("enabled"), reset: resetWith("DOM.setInspectModeEnabled", nil, map[string]any{"enabled": false})},
	// The pause on exceptions and on debugger statements live in
	// JSC::Debugger: they survive Debugger.disable and a new inspector
	// connection, and they act as soon as Debugger.enable attaches the
	// debugger. Their baseline is that of a new JSC::Debugger, which pauses
	// on neither (Debugger.h: m_pauseOnAllExceptionsBreakpoint,
	// m_pauseOnUncaughtExceptionsBreakpoint and
	// m_pauseOnDebuggerStatementsBreakpoint start null; Web Inspector's
	// frontend turns the debugger statements on itself). WebKit has no
	// getter, so a page's earlier value cannot be read; agent-ios never
	// turns a pause on by itself. Both go to a target before its
	// Debugger.enable (beforeEnable).
	"Debugger.setPauseOnExceptions": {
		key: keyBy(), replay: true, beforeEnable: true, clear: func(p map[string]any) bool { return p["state"] == "none" },
		reset: resetWith("Debugger.setPauseOnExceptions", nil, map[string]any{"state": "none"}),
	},
	"Debugger.setPauseOnDebuggerStatements": {key: keyBy(), replay: true, beforeEnable: true, clear: isFalse("enabled"), reset: resetWith("Debugger.setPauseOnDebuggerStatements", nil, map[string]any{"enabled": false})},
	"Debugger.setPauseOnAssertions":         {key: keyBy(), replay: true, clear: isFalse("enabled"), reset: resetWith("Debugger.setPauseOnAssertions", nil, map[string]any{"enabled": false})},
	"Debugger.setPauseOnMicrotasks":         {key: keyBy(), replay: true, clear: isFalse("enabled"), reset: resetWith("Debugger.setPauseOnMicrotasks", nil, map[string]any{"enabled": false})},
	// A new backend starts with breakpoints inactive (WebKit Debugger.cpp,
	// m_breakpointsActivated(false)); Debugger.enable does not change that.
	// The activation is a shared value, replayed after Debugger.enable and
	// before the breakpoints, as Web Inspector's
	// DebuggerManager.initializeTarget does; with no owner left it returns
	// to the inactive baseline.
	"Debugger.setBreakpointsActive": {
		key: keyBy(), replay: true, rank: -1, clear: isFalse("active"),
		reset: resetWith("Debugger.setBreakpointsActive", nil, map[string]any{"active": false}),
	},
	"DOMDebugger.setDOMBreakpoint":   {key: keyBy("nodeId", "type"), scoped: true, reset: resetWith("DOMDebugger.removeDOMBreakpoint", []string{"nodeId", "type"}, nil)},
	"DOMDebugger.setEventBreakpoint": {key: keyBy("breakpointType", "eventName"), replay: true, reset: resetWith("DOMDebugger.removeEventBreakpoint", []string{"breakpointType", "eventName"}, nil)},
	"DOMDebugger.setURLBreakpoint":   {key: keyBy("url"), replay: true, reset: resetWith("DOMDebugger.removeURLBreakpoint", []string{"url", "isRegex"}, nil)},
	"Page.overrideUserAgent":         {key: keyBy(), replay: true, clear: missingOrEmpty("value"), reset: resetWith("Page.overrideUserAgent", nil, nil)},
	"Page.overrideSetting":           {key: keyBy("setting"), replay: true, clear: missing("value"), reset: resetWith("Page.overrideSetting", []string{"setting"}, nil)},
	"Page.overrideUserPreference":    {key: keyBy("name"), replay: true, clear: missing("value"), reset: resetWith("Page.overrideUserPreference", []string{"name"}, nil)},
	"Page.setEmulatedMedia": {
		key: keyBy(), replay: true, clear: func(p map[string]any) bool { s, _ := p["media"].(string); return s == "" },
		reset: resetWith("Page.setEmulatedMedia", nil, map[string]any{"media": ""}),
	},
	"Page.setShowPaintRects": {key: keyBy(), replay: true, clear: isFalse("result"), reset: resetWith("Page.setShowPaintRects", nil, map[string]any{"result": false})},
	// iOS 16.0 only (internal/wip); without appearance nothing is forced.
	"Page.setForcedAppearance": {
		key: keyBy(), replay: true, clear: func(p map[string]any) bool { s, _ := p["appearance"].(string); return s == "" },
		reset: resetWith("Page.setForcedAppearance", nil, nil),
	},
}

// stateClears are calls that end the state of another method's entry.
var stateClears = map[string]struct {
	of  string
	key func(p map[string]any) string
}{
	"DOM.hideHighlight":                 {"DOM.highlightNode", keyBy()},
	"DOMDebugger.removeDOMBreakpoint":   {"DOMDebugger.setDOMBreakpoint", keyBy("nodeId", "type")},
	"DOMDebugger.removeEventBreakpoint": {"DOMDebugger.setEventBreakpoint", keyBy("breakpointType", "eventName")},
	"DOMDebugger.removeURLBreakpoint":   {"DOMDebugger.setURLBreakpoint", keyBy("url")},
}

// entryKey is the generic-entry key of a rule's state. The DOM highlight
// methods share one overlay.
func entryKey(method, key string) string {
	if strings.HasPrefix(method, "DOM.highlight") {
		method = "DOM.highlightNode"
	}
	return "legacy:" + method + key
}

// legacyStateCall handles the state-setting calls that legacy commands send
// besides domains, headers, interception rules and the bootstrap script:
// Network settings, the stateRules and stateClears, and JavaScript
// breakpoints. handled is false for any other method.
func legacyStateCall(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (res json.RawMessage, handled bool, err error) {
	empty := json.RawMessage("{}")
	var p map[string]any
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, true, fmt.Errorf("%s: bad params: %w", method, err)
		}
	}
	if p == nil {
		p = map[string]any{}
	}
	send := func(v any) (json.RawMessage, bool, error) {
		r, err := c.Send(ctx, method, v)
		return r, true, err
	}
	switch method {
	case "Network.setInterceptionEnabled":
		on, _ := p["enabled"].(bool)
		if err := acquireNetworkFor(ctx, ps, owner, c, on); err != nil {
			return nil, true, err
		}
		if v := ps.State.SetInterception(owner, on); v != nil {
			r, err := c.Send(ctx, method, map[string]bool{"enabled": *v})
			if err != nil && noEffect(err) {
				// WebKit kept its setting: so does owner's record, and a
				// retry sends the call again (W5c-1a review F9).
				ps.State.SetInterception(owner, !on)
			}
			return r, true, err
		}
		return empty, true, nil
	case "Network.setResourceCachingDisabled":
		off, _ := p["disabled"].(bool)
		if err := acquireNetworkFor(ctx, ps, owner, c, off); err != nil {
			return nil, true, err
		}
		if v := ps.State.SetCachingDisabled(owner, off); v != nil {
			return send(map[string]bool{"disabled": *v})
		}
		return empty, true, nil
	case "Network.setEmulatedConditions":
		limited := limitsConditions(p)
		if err := acquireNetworkFor(ctx, ps, owner, c, limited); err != nil {
			return nil, true, err
		}
		v, undo := ps.State.SetEmulatedConditions(owner, params)
		if v == nil {
			return empty, true, nil
		}
		r, err := c.Send(ctx, method, v)
		ps.State.SentEmulatedConditions(err, undo)
		return r, true, err
	case "Debugger.setBreakpoint", "Debugger.setBreakpointByUrl":
		if err := acquireFor(ctx, ps.State, owner, c, ruleDomain(method)); err != nil {
			return nil, true, err
		}
		r, err := c.Send(ctx, method, params)
		var v struct {
			BreakpointID string `json:"breakpointId"`
		}
		if err == nil && json.Unmarshal(r, &v) == nil && v.BreakpointID != "" {
			e := breakpointEntry(ps.State, owner, method, params, v.BreakpointID)
			var replay func(context.Context, Sender) error
			if !e.Scoped {
				replay = e.Apply // provisional targets get it as a setup would
			}
			putState(ps.State, c, e, replay)
		}
		return r, true, err
	case "Debugger.removeBreakpoint":
		// The client names the id it got; after a replay the current
		// target knows the breakpoint by another one. Provisional targets
		// that replayed it lose their copies too, so a later commit cannot
		// bring it back (design 2.7).
		id, _ := p["breakpointId"].(string)
		ps.State.mu.Lock()
		holder, held := ps.State.breakpointHolderLocked(id)
		ps.State.mu.Unlock()
		if held && holder != owner {
			// One owner never removes another's breakpoint (design 2.4).
			return nil, true, notSent{fmt.Errorf("%s: breakpoint %s is not one this client set, so it was not removed", method, id)}
		}
		ps.State.Remove(entryKey("Debugger.breakpoint", param(p, "breakpointId")))
		cur, copies := ps.State.takeBreakpointID(id, changeTargetOf(c))
		p["breakpointId"] = cur
		r, err := c.Send(ctx, method, p)
		return r, true, withCopiesRemoved(ctx, err, copies)
	}
	if cl, ok := stateClears[method]; ok {
		key := entryKey(cl.of, cl.key(p))
		if sharedRule(stateRules[cl.of]) {
			return setSharedValue(ctx, ps, owner, c, key, cl.of, method, p, true)
		}
		ps.State.Remove(key)
		return send(params)
	}
	rule, ok := stateRules[method]
	if !ok {
		return nil, false, nil
	}
	key := entryKey(method, rule.key(p))
	base := rule.clear != nil && rule.clear(p)
	if sharedRule(rule) {
		return setSharedValue(ctx, ps, owner, c, key, method, method, p, base)
	}
	if base {
		ps.State.Remove(key)
		return send(params)
	}
	if err := acquireFor(ctx, ps.State, owner, c, ruleDomain(method)); err != nil {
		return nil, true, err
	}
	r, err := c.Send(withoutForward(ctx), method, params)
	if err != nil {
		return r, true, err
	}
	call := append(json.RawMessage(nil), params...)
	e := Entry{
		Key: key, Rank: rule.rank, Owner: owner, Domain: ruleDomain(method), Scoped: rule.scoped, Apply: func(context.Context, Sender) error { return nil },
		Release: func(ctx context.Context, s Sender) error {
			m, rp := rule.reset(p)
			_, err := s.Send(ctx, m, rp)
			return err
		},
	}
	var replay func(context.Context, Sender) error
	if rule.replay {
		e.Apply = func(ctx context.Context, s Sender) error {
			_, err := s.Send(ctx, method, call)
			return err
		}
		replay = e.Apply
	}
	putState(ps.State, c, e, replay)
	return r, true, nil
}

// acquireNetworkFor acquires Network for owner before it sets Network state
// other than the baseline (set): headers, interception, the caching
// override or emulated conditions (orchestrator rule 1, design 2.4). The
// state then reaches new targets (Apply sends it while Network is owned),
// another owner's last Network.disable does not clear it, and it ends only
// with its owner's own Network.disable or release.
func acquireNetworkFor(ctx context.Context, ps *PageSession, owner Owner, c Sender, set bool) error {
	if !set {
		return nil
	}
	return acquireFor(ctx, ps.State, owner, c, "Network")
}

// setSharedValue sends a call that sets owner's value of the shared key of
// rule (base: the call sets WebKit's baseline) and records the value. A
// value other than the baseline acquires the rule's domain first.
func setSharedValue(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, key, rule, method string, p map[string]any, base bool) (json.RawMessage, bool, error) {
	if !base {
		if err := acquireFor(ctx, ps.State, owner, c, ruleDomain(rule)); err != nil {
			return nil, true, err
		}
	}
	r, err := c.Send(withoutForward(ctx), method, p)
	if err != nil {
		return r, true, err
	}
	ps.State.setShared(c, owner, key, rule, method, p, base)
	return r, true, nil
}

// notForwarded are the calls that a change does not repeat on provisional
// targets: state that names node or script ids of the active document or
// that a new target does not get (rules without replay, Scoped rules, their
// resets and clears), and JavaScript breakpoints, whose copies have their
// own ids (breakpointEntry, takeBreakpointID).
var notForwarded = func() map[string]bool {
	m := map[string]bool{"Debugger.setBreakpoint": true, "Debugger.setBreakpointByUrl": true, "Debugger.removeBreakpoint": true}
	for method, rule := range stateRules {
		if !rule.replay || rule.scoped {
			m[method] = true
			reset, _ := rule.reset(map[string]any{})
			m[reset] = true
		}
	}
	for method, cl := range stateClears {
		if m[cl.of] {
			m[method] = true
		}
	}
	return m
}()

// forwarded reports whether a change repeats a successful call on the
// provisional targets.
func forwarded(method string) bool { return !notForwarded[method] }

// desiredStateMethod reports whether a legacy call changes the desired
// state (legacyCall), so that it runs as one change (beginChange).
func desiredStateMethod(method string) bool {
	_, verb, _ := strings.Cut(method, ".")
	switch method {
	case "Network.setExtraHTTPHeaders", "Network.addInterception", "Network.removeInterception", "Page.setBootstrapScript",
		"Network.setInterceptionEnabled", "Network.setResourceCachingDisabled", "Network.setEmulatedConditions",
		"Debugger.setBreakpoint", "Debugger.setBreakpointByUrl", "Debugger.removeBreakpoint":
		return true
	}
	_, clear := stateClears[method]
	_, rule := stateRules[method]
	return verb == "enable" || verb == "disable" || clear || rule || scriptProfilerMethod(method)
}

// scriptProfilerMethod reports a legacy call that starts or stops the
// page's ScriptProfiler run (scriptprofiler.go).
func scriptProfilerMethod(method string) bool {
	return method == "ScriptProfiler.startTracking" || method == "ScriptProfiler.stopTracking"
}

// breakpointEntry is the owned entry of a JavaScript breakpoint that the
// client knows as id. A URL breakpoint (Debugger.setBreakpointByUrl) is a
// stable specification: a new target gets it again, and the id that target
// gives it replaces id for removal and release. A breakpoint at a script
// location names a script id of the old backend, so it is Scoped.
func breakpointEntry(d *DesiredState, owner Owner, method string, params json.RawMessage, id string) Entry {
	e := Entry{
		Key: breakpointKey(id), Owner: owner,
		Scoped: method == "Debugger.setBreakpoint", Domain: ruleDomain(method),
		Apply: func(context.Context, Sender) error { return nil },
		Release: func(ctx context.Context, s Sender) error {
			cur, copies := d.takeBreakpointID(id, changeTargetOf(s))
			_, err := s.Send(ctx, "Debugger.removeBreakpoint", map[string]string{"breakpointId": cur})
			return withCopiesRemoved(ctx, err, copies)
		},
	}
	if method == "Debugger.setBreakpointByUrl" {
		spec := append(json.RawMessage(nil), params...)
		e.Apply = func(ctx context.Context, s Sender) error {
			r, err := s.Send(ctx, method, spec)
			if err != nil {
				return err
			}
			var v struct {
				BreakpointID string `json:"breakpointId"`
			}
			if err := json.Unmarshal(r, &v); err != nil || v.BreakpointID == "" {
				return fmt.Errorf("%s: no breakpoint id in the reply %s", method, r)
			}
			target := ""
			if tc, ok := s.(interface{ TargetID() string }); ok {
				target = tc.TargetID()
			}
			if !d.setBreakpointID(target, id, v.BreakpointID, s) {
				// The client removed the breakpoint, or its owner released
				// it, while this replay was in flight.
				if err := removeBreakpointOn(ctx, s, v.BreakpointID); err != nil {
					return fmt.Errorf("removing breakpoint %s, which ended while it was replayed: %w", v.BreakpointID, err)
				}
			}
			return nil
		}
	}
	return e
}
