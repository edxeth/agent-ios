package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Sender sends one protocol command. webkit.Caller and webkit.TargetCaller
// (a provisional target during setup) both satisfy it.
type Sender interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Owner names who holds a desired-state entry: a session, a capture, the
// dialog bridge or a CDP attachment (design 2.4).
type Owner string

// InterceptRule is one WIP Network.addInterception rule.
type InterceptRule struct {
	URL           string `json:"url"`
	Stage         string `json:"stage"` // "request" or "response"
	CaseSensitive bool   `json:"caseSensitive"`
	IsRegex       bool   `json:"isRegex"`
}

// Entry is an owned piece of page state that is not one of the built-in
// kinds below, for example a user-agent override or a forced pseudo state.
// Apply sends it to a target (a new connection or a provisional target);
// Release undoes it when its owner releases it.
type Entry struct {
	Key     string // unique; entries apply in (Rank, Key) order
	Rank    int    // lower ranks apply first
	Owner   Owner
	Domain  string // needed by the entry; its owner holds it (domain_owners.go)
	Apply   func(ctx context.Context, s Sender) error
	Release func(ctx context.Context, s Sender) error
	// Scoped state names node or script ids of the document and backend it
	// was set on. A new target (new connection or process) has none of
	// them, so the entry is never sent to one. It is dropped when the new
	// target replaces the old one (Apply on a new connection, or the commit
	// of a provisional target), so its Release never sends a stale id; a
	// provisional target that is abandoned leaves it in place.
	Scoped bool
	// BeforeEnable entries are sent to a new or provisional target after
	// the agent resets and before the domain enables (the Debugger pause
	// settings, stateRule.beforeEnable).
	BeforeEnable bool
}

// DesiredState is the per-page model of state that agent-ios set in WebKit
// (design 2.4). It is not a history of sends: Apply sends the complete state
// to a new or provisional target, and Release removes one owner's entries
// and sends what that changes.
type DesiredState struct {
	mu        sync.Mutex
	domains   map[string]map[Owner]bool
	headers   map[Owner]map[string]string
	rules     map[Owner][]InterceptRule
	intercept map[Owner]bool // owners that enabled interception
	caching   map[Owner]bool // owners that disabled the resource cache
	conds     map[Owner]condEntry
	condSeq   uint64
	// condsUnsure: the last send of the effective conditions had an
	// unknown outcome (SentEmulatedConditions).
	condsUnsure bool
	entries     map[string]Entry
	bpIDs       map[string]string // URL breakpoint id a client got -> the active target's id
	// active is the target that bpIDs and the Scoped entries belong to (""
	// when the connection does not name it). staged holds, per provisional
	// target, the breakpoint ids its setup got, and stagedVia the sender
	// that reaches that target; removing or releasing a breakpoint removes
	// its staged copies there (takeBreakpointID). The ids replace bpIDs
	// only when that target commits (commitTarget) and are dropped when it
	// is destroyed first (abandonTarget).
	active    string
	staged    map[string]map[string]string
	stagedVia map[string]Sender
	// ver counts model mutations, chg serializes desired-state changes and
	// syncs holds the provisional targets whose setup took its snapshot
	// (desired_sync.go). shared holds every owner's latest value of each
	// shared key, in sequence order (domain_owners.go).
	ver       uint64
	chg       chan struct{}
	syncs     map[string]*targetSync
	shared    map[string][]sharedVal
	sharedSeq uint64
	partOwner map[string]Owner
	gapWatch  map[Owner][]*func(NetworkGap) // WatchNetworkReset
	Boot      *BootstrapParts
	// Releases (release_lifecycle.go). relGen counts the releases that
	// removed a session's owners (forgetSession); a setup snapshot is taken
	// with its value, so an installer can tell whether a release ran after
	// the snapshot. cur is the change that holds chg (nil between changes).
	// unknown holds the domains whose page-side state on the live
	// connection is unknown, because owners were removed from the model
	// without their page-side cleanup or a cleanup send failed: each
	// with the mark (markSeq) of the last release that left it so. A
	// reset completes only the mark it read before it took its snapshot
	// (resetAgent).
	relGen  uint64
	cur     *changeSender
	unknown map[string]uint64
	markSeq uint64
	// holds is the page's scheduler (setupHolds), set once it exists
	// (useHolds); nil before the first connection is installed, when no
	// request can be held.
	holds setupHolds
}

// NewDesiredState returns an empty model with its bootstrap parts.
func NewDesiredState() *DesiredState {
	return &DesiredState{
		domains:   make(map[string]map[Owner]bool),
		headers:   make(map[Owner]map[string]string),
		rules:     make(map[Owner][]InterceptRule),
		intercept: make(map[Owner]bool),
		caching:   make(map[Owner]bool),
		conds:     make(map[Owner]condEntry),
		entries:   make(map[string]Entry),
		partOwner: make(map[string]Owner),
		chg:       make(chan struct{}, 1),
		Boot:      NewBootstrapParts(),
	}
}

// Acquire records that owner needs domain enabled. It reports whether
// nothing needed domain before (Needs), that is, whether domain.enable must
// be sent now.
func (d *DesiredState) Acquire(owner Owner, domain string) (first bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return false
	}
	if d.domains[domain] == nil {
		d.domains[domain] = make(map[Owner]bool)
	}
	first = !d.needsLocked(domain)
	d.domains[domain][owner] = true
	d.ver++
	return first
}

// Needs reports whether domain must be enabled: while it has an owner and,
// for Page, while there is a bootstrap part. The parts are a requirement of
// Page like an owner, because WebKit runs the bootstrap script only while
// the Page agent is enabled (InspectorPageAgent::didClearWindowObjectInWorld
// checks the enabled agent). A domain's enable is sent when its first
// requirement begins and its disable when its last one ends.
func (d *DesiredState) Needs(domain string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.needsLocked(domain)
}

func (d *DesiredState) needsLocked(domain string) bool {
	return len(d.domains[domain]) > 0 || domain == "Page" && !d.Boot.Empty()
}

// HeaderChange says what a SetHeaders call requires on the wire.
type HeaderChange struct {
	// Send is the complete effective header map to send with
	// Network.setExtraHTTPHeaders, or nil when nothing new must be sent.
	Send map[string]string
	// NeedsNetworkReset is true when a header name lost its last owner
	// while Network stays owned: WIP merges header maps and never removes a
	// name, so only the Network reinitialization barrier (W1b) removes it.
	NeedsNetworkReset bool
}

// SetHeaders replaces owner's extra HTTP headers. The effective map is the
// union of every owner's map.
func (d *DesiredState) SetHeaders(owner Owner, h map[string]string) HeaderChange {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return HeaderChange{}
	}
	d.ver++
	before := d.effectiveHeadersLocked()
	if len(h) == 0 {
		delete(d.headers, owner)
	} else {
		cp := make(map[string]string, len(h))
		for k, v := range h {
			cp[k] = v
		}
		d.headers[owner] = cp
	}
	return d.headerChangeLocked(before)
}

func (d *DesiredState) headerChangeLocked(before map[string]string) HeaderChange {
	after := d.effectiveHeadersLocked()
	var ch HeaderChange
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			ch.Send = after
			break
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok && len(d.domains["Network"]) > 0 {
			ch.NeedsNetworkReset = true
		}
	}
	return ch
}

// EffectiveHeaders returns the union of every owner's headers. When two
// owners set the same name, the owner that sorts first wins.
func (d *DesiredState) EffectiveHeaders() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.effectiveHeadersLocked()
}

func (d *DesiredState) effectiveHeadersLocked() map[string]string {
	owners := make([]Owner, 0, len(d.headers))
	for o := range d.headers {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] > owners[j] })
	out := make(map[string]string)
	for _, o := range owners {
		for k, v := range d.headers[o] {
			out[k] = v
		}
	}
	return out
}

// AddInterception adds an interception rule for owner.
func (d *DesiredState) AddInterception(owner Owner, r InterceptRule) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return
	}
	d.ver++
	d.rules[owner] = append(d.rules[owner], r)
}

// Put adds or replaces a generic entry.
func (d *DesiredState) Put(e Entry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return
	}
	d.ver++
	d.entries[e.Key] = e
}

// SetPart adds or replaces a bootstrap part owned by owner. The first part
// is a new requirement of Page (Needs): a caller that adds it on a live page
// sends Page.enable when Needs("Page") was false before, as legacyCall does.
func (d *DesiredState) SetPart(owner Owner, name string, rank int, source string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return nil
	}
	if err := d.Boot.Set(name, rank, source); err != nil {
		return err
	}
	d.ver++
	d.partOwner[name] = owner
	return nil
}

// SetLatestPart is SetPart for a part whose replacement must become the
// latest (BootstrapParts.SetLatest), as a geolocation override: the last
// set wins, also when it replaces an earlier one of the same owner.
func (d *DesiredState) SetLatestPart(owner Owner, name string, rank int, source string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return nil
	}
	if err := d.Boot.SetLatest(name, rank, source); err != nil {
		return err
	}
	d.ver++
	d.partOwner[name] = owner
	return nil
}

// RemovePart removes the bootstrap part name. It reports whether that ended
// the last requirement of Page (Needs), that is, whether Page.disable must
// be sent.
func (d *DesiredState) RemovePart(name string) (last bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.Boot.Remove(name) {
		return false
	}
	d.ver++
	delete(d.partOwner, name)
	return !d.needsLocked("Page")
}

// Apply sends the complete desired state to s, which is a new connection's
// target or a provisional target before commit (design 2.7). The target's
// DOM overlay and every agent that the connection does not know to be at
// baseline (a page restored from the back/forward cache, a repeated setup,
// or any target the connection never reset) are reset first (resetCalls).
// Then: Page.enable,
// Page.setBootstrapScript with all parts, every owned domain, the effective
// extra headers, interception enablement and rules, the resource-caching
// override and emulated conditions, then the generic entries. Scoped
// entries are never sent. A new connection replaces the old backend, so
// Apply drops them and its target becomes the active one; a setup of one
// target (a webkit.TargetCaller) leaves the active target's state alone.
//
// A setup of one target runs again when a release removed owners after
// its snapshot (relGen): that snapshot may have sent the released
// session's state, and the new setup resets what the target's record
// shows enabled (resetCalls) before it sends the state that remains. The
// installer of a new connection makes the same check (applyConn).
//
// A setup of one target that is the active target (Session.ReapplySetup,
// or a provisional target that committed before its snapshot), and every
// setup that runs again, is an active Network reset (applyResetting).
func (d *DesiredState) Apply(ctx context.Context, s Sender) error {
	tc, oneTarget := s.(interface{ TargetID() string })
	for pass := 0; ; pass++ {
		var gen uint64
		var err error
		if oneTarget && (pass > 0 || d.isActive(tc.TargetID())) {
			gen, err = d.applyResetting(ctx, s, tc.TargetID())
		} else {
			gen, err = d.applyConn(ctx, s)
		}
		if err != nil || !oneTarget || d.ReleaseGen() == gen {
			return err
		}
	}
}

// applyConn is one Apply. It returns the release generation (relGen) of
// the snapshot it sent: a new connection is installed only while no
// release removed owners since (Manager.reattach, Manager.recover).
func (d *DesiredState) applyConn(ctx context.Context, s Sender) (uint64, error) {
	tc, oneTarget := s.(interface{ TargetID() string })
	d.mu.Lock()
	gen := d.relGen
	var ts *targetSync
	if oneTarget && tc.TargetID() != d.active {
		// A provisional target: changes after this snapshot are queued
		// for it (desired_sync.go).
		ts = d.registerLocked(tc.TargetID(), s)
	}
	if !oneTarget {
		// A new connection replaces every target, and its queue with it.
		for _, old := range d.syncs {
			d.dropQueueLocked(old)
		}
		d.syncs = nil
		d.active = ""
		if a, ok := s.(interface{ ActiveTarget() string }); ok {
			d.active = a.ActiveTarget()
		}
		d.staged, d.stagedVia = nil, nil
		// The new connection's setup resets every agent it does not know.
		d.unknown = nil
	}
	script := d.Boot.Script()
	var domains []string
	for dom, owners := range d.domains {
		if len(owners) > 0 && dom != "Page" {
			domains = append(domains, dom)
		}
	}
	sort.Strings(domains)
	page := d.needsLocked("Page")
	network := len(d.domains["Network"]) > 0
	headers := d.effectiveHeadersLocked()
	var rules []InterceptRule
	var ruleOwners []Owner
	for o := range d.rules {
		ruleOwners = append(ruleOwners, o)
	}
	sort.Slice(ruleOwners, func(i, j int) bool { return ruleOwners[i] < ruleOwners[j] })
	for _, o := range ruleOwners {
		rules = append(rules, d.rules[o]...)
	}
	ns := d.networkSettingsLocked()
	var entries []Entry
	for k, e := range d.entries {
		if e.Scoped {
			if !oneTarget {
				delete(d.entries, k)
			}
			continue
		}
		entries = append(entries, e)
	}
	entries = append(entries, d.sharedEntriesLocked()...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Rank != entries[j].Rank {
			return entries[i].Rank < entries[j].Rank
		}
		return entries[i].Key < entries[j].Key
	})
	d.mu.Unlock()
	prior, recorded := agentRecord(s, oneTarget)
	err := d.applySnapshot(ctx, s, prior, recorded, page, script, domains, network, headers, ns, rules, entries)
	if ts != nil {
		return gen, d.finishSetup(ctx, tc.TargetID(), ts, err)
	}
	return gen, err
}

// agentRecord returns the connection's record of the agents of s's
// backend (webkit.Session.AgentStates): that of the setup's target, or of
// the connection's active target. recorded is false for a sender without
// a record (webkit.Client, the connection of explicit ws:// URLs).
func agentRecord(s Sender, oneTarget bool) (prior map[string]webkit.AgentState, recorded bool) {
	if ar, ok := s.(interface {
		AgentStates() map[string]webkit.AgentState
	}); ok && oneTarget {
		return ar.AgentStates(), true
	}
	if ar, ok := s.(interface {
		ActiveTarget() string
		AgentStates(target string) map[string]webkit.AgentState
	}); ok && !oneTarget {
		return ar.AgentStates(ar.ActiveTarget()), true
	}
	return nil, false
}

// applySnapshot sends a snapshot that Apply took to s, whose backend has
// the agents prior says (recorded: s has a record).
func (d *DesiredState) applySnapshot(ctx context.Context, s Sender, prior map[string]webkit.AgentState, recorded, page bool, script string, domains []string, network bool, headers map[string]string, ns networkSettings, rules []InterceptRule, entries []Entry) (rerr error) {
	send := func(method string, params any) error {
		if _, err := s.Send(ctx, method, params); err != nil {
			return fmt.Errorf("applying desired state: %w", err)
		}
		return nil
	}
	needed := map[string]bool{"Page": page}
	for _, dom := range domains {
		needed[dom] = true
	}
	// The resets, Page.enable, the bootstrap script and the domain enables
	// go out in one pipelined batch (one round trip; WebKit runs them in
	// order). Only a reset may be refused because its agent is not enabled.
	calls, kept := resetCalls(prior, recorded, needed)
	resets := len(calls)
	if page {
		calls = append(calls, webkit.Call{Method: "Page.enable"})
	}
	if script != "" {
		calls = append(calls, webkit.Call{Method: "Page.setBootstrapScript", Params: map[string]string{"source": script}})
	}
	for _, dom := range domains {
		if !kept[dom] { // a kept agent is enabled on this backend already
			calls = append(calls, webkit.Call{Method: dom + ".enable"})
		}
	}
	// The entries that must precede the enables go between the resets and
	// the rest of the batch.
	var pre []Entry
	entries = slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool {
		if e.BeforeEnable {
			pre = append(pre, e)
		}
		return e.BeforeEnable
	})
	var errs []error
	if len(pre) == 0 {
		errs = sendPipelined(ctx, s, calls)
	} else {
		errs = sendPipelined(ctx, s, calls[:resets])
		for _, e := range pre {
			if err := e.Apply(ctx, s); err != nil {
				err = fmt.Errorf("applying desired state %q before the domain enables: %w", e.Key, err)
				if networkDisableIn(calls[:resets], errs) {
					err = networkResetWritten{err}
				}
				return err
			}
		}
		errs = append(errs, sendPipelined(ctx, s, calls[resets:])...)
	}
	if networkDisableIn(calls, errs) {
		// The Network agent may be reset whatever fails below
		// (networkResetWritten).
		defer func() {
			if rerr != nil {
				rerr = networkResetWritten{rerr}
			}
		}()
	}
	for i, err := range errs {
		switch {
		case err == nil || i < resets && notEnabled(err):
		case i < resets:
			return fmt.Errorf("applying desired state: resetting the page target's state (%s): %w", calls[i].Method, err)
		default:
			return fmt.Errorf("applying desired state: %w", err)
		}
	}
	if network && len(headers) > 0 {
		if err := send("Network.setExtraHTTPHeaders", map[string]any{"headers": headers}); err != nil {
			return err
		}
	}
	if network && ns.intercept {
		if err := send("Network.setInterceptionEnabled", map[string]bool{"enabled": true}); err != nil {
			return err
		}
		for _, r := range rules {
			if err := send("Network.addInterception", r); err != nil {
				return err
			}
		}
	}
	if err := applyNetworkSettings(ctx, s, ns); err != nil {
		return fmt.Errorf("applying desired state: %w", err)
	}
	for _, e := range entries {
		if err := e.Apply(ctx, s); err != nil {
			return fmt.Errorf("applying desired state %q: %w", e.Key, err)
		}
	}
	return nil
}

// resetCalls returns the calls that bring a backend to WebKit's baseline
// before Apply sends the desired state, and the domains it keeps enabled.
// applySnapshot sends them pipelined with the enables that follow.
//
// The DOM overlay is reset on every target: DOM.hideHighlight and
// DOM.setInspectModeEnabled(false). Highlights and inspect mode survive a
// back/forward cache restore and a commit away (only an explicit false or
// a backend disconnect clears them), and no record says whether a backend
// has them (DOM has no enable). The desired state does not replay them on a
// new target (stateRules: no replay), so every target starts without them;
// a replayed inspect mode on a target that still has it would keep the old
// highlight configuration, because WebKit ignores an unchanged enabled
// flag (InspectorDOMAgent::setSearchingForNode).
//
// The agents are handled by the connection's record of s's backend
// (prior, webkit.AgentState). A backend with enabled agents in the record
// is a page target restored from the back/forward cache, which keeps
// every agent with the state it had when the page was left, or a target
// whose setup runs again. Its state can differ from the desired state
// (changes made while it was away), and a second <Domain>.enable fails on
// most agents ("Page domain already enabled"), so:
//   - A domain whose agent holds desired state besides its enablement
//     (stateDomain) is reset (agentResetCalls), and Apply enables it again
//     when it is needed.
//   - Another domain that is needed and certainly enabled
//     (webkit.AgentEnabled) is kept, so its enable is not sent again.
//   - A domain that the record says is disabled is left alone.
//   - Any other domain in the record is reset.
//
// The connection knows nothing about an agent without a record entry.
// That backend can be fresh, restored after an earlier connection enabled
// its agents, or older than the connection: they all arrive alike, so an
// empty record never means a fresh backend. With a record (recorded),
// every unknown agent that the desired state needs is reset, and so is
// every unknown state-domain agent (Page, Debugger, Network: their state,
// for example interception that holds requests, acts on the page even
// while the desired state does not need the domain). Later first enables
// of unknown agents get their reset from webkit.Session
// (SessionOptions.AgentReset, agentResetCalls). A sender without a record
// (webkit.Client) gets no agent reset.
//
// A reset call that WebKit refuses because the agent is not enabled is no
// failure: the record can say AgentMaybeEnabled.
func resetCalls(prior map[string]webkit.AgentState, recorded bool, needed map[string]bool) (calls []webkit.Call, kept map[string]bool) {
	kept = map[string]bool{}
	calls = slices.Clone(overlayResetCalls)
	doms := slices.Collect(maps.Keys(prior))
	if recorded {
		for dom := range maps.Keys(needed) {
			if _, ok := prior[dom]; !ok && needed[dom] && !stateDomain(dom) {
				doms = append(doms, dom)
			}
		}
		for _, dom := range []string{"Debugger", "Network", "Page"} {
			if _, ok := prior[dom]; !ok {
				doms = append(doms, dom)
			}
		}
	}
	sort.Strings(doms)
	for _, dom := range doms {
		st := prior[dom]
		switch {
		case st == webkit.AgentDisabled:
			continue
		case st == webkit.AgentEnabled && needed[dom] && !stateDomain(dom):
			kept[dom] = true
			continue
		}
		calls = append(calls, agentResetCalls(dom)...)
	}
	return calls, kept
}

// overlayDomain names the DOM overlay (highlights and inspect mode) in
// d.unknown. Its entries need no domain (Entry.Domain is ""; DOM has no
// enable or disable in WebKit), but a cleanup of one that failed or was not
// sent stays pending under this name (forgetSession), as legacyCall and
// agentDomain name it too; resetAndApplyDomain resets it with
// overlayResetCalls.
const overlayDomain = "DOM"

// overlayResetCalls bring the DOM overlay to WebKit's baseline. Every setup
// sends them (resetCalls): overlay entries are never replayed.
var overlayResetCalls = []webkit.Call{{Method: "DOM.hideHighlight"}, {Method: "DOM.setInspectModeEnabled", Params: map[string]any{"enabled": false}}}

// agentResetCalls returns the calls that bring the agent of dom to WebKit's
// baseline (webkit.SessionOptions.AgentReset, resetCalls): its disable,
// which resets the agent's state; for Debugger, first the baseline of the
// pause-on-exceptions and debugger-statement settings, which survive
// Debugger.disable in JSC::Debugger; for Page, then an empty bootstrap
// script, which Page.disable keeps (Apply sends the desired script after
// Page.enable). ScriptProfiler has no enable or disable; its only state is
// a tracking run, which survives a same-target navigation and which
// ScriptProfiler.stopTracking ends (a no-op while it does not track;
// InspectorScriptProfilerAgent, W4 rebase review finding F2).
func agentResetCalls(dom string) []webkit.Call {
	if dom == "ScriptProfiler" {
		return []webkit.Call{{Method: "ScriptProfiler.stopTracking"}}
	}
	var calls []webkit.Call
	if dom == "Debugger" {
		for _, rule := range []string{"Debugger.setPauseOnExceptions", "Debugger.setPauseOnDebuggerStatements"} {
			m, p := stateRules[rule].reset(map[string]any{})
			calls = append(calls, webkit.Call{Method: m, Params: p})
		}
	}
	calls = append(calls, webkit.Call{Method: dom + ".disable"})
	if dom == "Page" {
		calls = append(calls, webkit.Call{Method: "Page.setBootstrapScript", Params: map[string]string{"source": ""}})
	}
	return calls
}

// sendPipelined sends calls to s in order and returns each call's error.
// A sender that can pipeline (webkit.Session, a webkit.Session target)
// writes them all before it reads a reply, in one round trip; another
// sends them one after the other.
func sendPipelined(ctx context.Context, s Sender, calls []webkit.Call) []error {
	if ps, ok := s.(interface {
		SendPipelined(ctx context.Context, calls []webkit.Call) []error
	}); ok {
		return ps.SendPipelined(ctx, calls)
	}
	errs := make([]error, len(calls))
	for i, c := range calls {
		_, errs[i] = s.Send(ctx, c.Method, c.Params)
	}
	return errs
}

// stateDomain reports whether the agent of dom holds desired state besides
// its enablement: the Page overrides and bootstrap script, the Debugger
// breakpoints and settings, the Network settings (ruleDomain, design 2.4).
// Its disable resets that state in WebKit.
func stateDomain(dom string) bool {
	switch dom {
	case "Page", "Debugger", "Network":
		return true
	}
	return false
}

// ReleaseResult reports what a Release could not do itself.
type ReleaseResult struct {
	// NeedsNetworkReset: a header name lost its last owner while Network
	// stays owned; the caller must run the Network reinitialization barrier,
	// which applies the complete remaining header map.
	NeedsNetworkReset bool
	// Failed lists the domains, sorted, whose cleanup send failed: their
	// agents' page-side state is unknown (release_lifecycle.go).
	Failed []string
}

// Release removes every entry of owner and sends what that changes: the
// owner's contributions to every domain and its bootstrap parts are dropped
// (dropOwner), and each domain whose last owner it was is disabled. Paused
// requests must be continued through the control lane before Release
// (Scheduler.Release does that).
func (d *DesiredState) Release(ctx context.Context, owner Owner, s Sender) (ReleaseResult, error) {
	res, disable, err := d.dropOwner(ctx, owner, "", s)
	errs := []error{err}
	for _, dom := range disable {
		if err := sendDisable(ctx, s, dom+".disable", nil); err != nil {
			errs = append(errs, err)
			res.Failed = append(res.Failed, dom)
		}
	}
	slices.Sort(res.Failed)
	res.Failed = slices.Compact(res.Failed)
	if err := errors.Join(errs...); err != nil {
		return res, fmt.Errorf("releasing desired state of %s: %w", owner, err)
	}
	return res, nil
}

// dropOwner removes owner's contributions to the domain dom, or to every
// domain and the bootstrap parts when dom is "", and sends what that
// changes through s (design 2.4; one cleanup path for an owner's own
// <Domain>.disable and for its release): its generic entries of the domain
// are released, its shared values fall back to the remaining owners'
// values or the baseline (domain_owners.go), and for Network its
// interception rules are removed (interception is disabled when no rule
// remains), its caching override and emulated conditions give way to the
// remaining owners', and a header value that changed while every name
// keeps an owner is sent as the complete effective map; a header name that
// lost its last owner while Network stays owned is reported in
// res.NeedsNetworkReset. Without dom, the bootstrap script is re-sent
// without owner's parts. owner's references to the domains are dropped;
// the domains left without an owner are returned, sorted, and not
// disabled here. res.Failed lists the domains of the sends that failed.
func (d *DesiredState) dropOwner(ctx context.Context, owner Owner, dom string, s Sender) (res ReleaseResult, disable []string, err error) {
	d.mu.Lock()
	od := d.dropOwnerLocked(owner, dom)
	d.mu.Unlock()
	res = od.res
	res.Failed, err = d.sendDrop(ctx, od, s)
	return res, od.disable, err
}

// ownerDrop is what dropOwnerLocked removed and what its removal requires
// on the wire (sendDrop). touched lists the domains whose page-side state
// the removed contributions were part of.
type ownerDrop struct {
	res                           ReleaseResult
	disable                       []string
	touched                       map[string]bool
	entries                       []Entry
	shared                        []sharedSend
	rules                         []InterceptRule
	settingsBefore, settingsAfter networkSettings
	parts                         bool
	headerSend                    map[string]string
}

// dropOwnerLocked is the model part of dropOwner. The caller holds d.mu.
func (d *DesiredState) dropOwnerLocked(owner Owner, dom string) (od ownerDrop) {
	in := func(x string) bool { return dom == "" || x == dom }
	network := in("Network")
	od.touched = map[string]bool{}
	var disable []string
	d.ver++
	var entries []Entry
	for k, e := range d.entries {
		if e.Owner == owner && in(e.Domain) {
			entries = append(entries, e)
			delete(d.entries, k)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	for _, e := range entries {
		od.touched[cmp.Or(e.Domain, overlayDomain)] = true
	}
	shared := d.takeSharedLocked(owner, func(rule string) bool { return in(ruleDomain(rule)) })
	for _, x := range shared {
		od.touched[ruleDomain(x.method)] = true
	}
	var settingsBefore, settingsAfter networkSettings
	var rules []InterceptRule
	if network {
		_, conds := d.conds[owner]
		if d.rules[owner] != nil || d.intercept[owner] || d.caching[owner] || conds || d.headers[owner] != nil {
			od.touched["Network"] = true
		}
		settingsBefore = d.networkSettingsLocked()
		rules = d.rules[owner]
		delete(d.rules, owner)
		delete(d.intercept, owner)
		delete(d.caching, owner)
		delete(d.conds, owner)
		settingsAfter = d.networkSettingsLocked()
	}
	pageBefore := d.needsLocked("Page")
	var parts []string
	if dom == "" {
		for name, o := range d.partOwner {
			if o == owner {
				parts = append(parts, name)
				delete(d.partOwner, name)
				d.Boot.Remove(name)
				od.touched["Page"] = true
			}
		}
	}
	headersBefore, hadHeaders := d.effectiveHeadersLocked(), network && d.headers[owner] != nil
	if network {
		delete(d.headers, owner)
	}
	for dm, owners := range d.domains {
		if in(dm) && owners[owner] {
			od.touched[dm] = true
			delete(owners, owner)
			if !d.needsLocked(dm) {
				disable = append(disable, dm)
			}
		}
	}
	if pageBefore && !d.needsLocked("Page") && !slices.Contains(disable, "Page") {
		disable = append(disable, "Page") // owner's parts were the last requirement
	}
	// With no Network owner left, Network.disable alone clears the map.
	var headerSend map[string]string
	if hadHeaders && len(d.domains["Network"]) > 0 {
		ch := d.headerChangeLocked(headersBefore)
		od.res.NeedsNetworkReset = ch.NeedsNetworkReset
		if !ch.NeedsNetworkReset {
			headerSend = ch.Send
		}
	}
	sort.Strings(disable)
	delete(od.touched, "")
	od.disable, od.entries, od.shared, od.rules = disable, entries, shared, rules
	od.settingsBefore, od.settingsAfter = settingsBefore, settingsAfter
	od.parts, od.headerSend = len(parts) > 0, headerSend
	return od
}

// sendDrop sends what the removal od requires through s, except the
// disables of od.disable, and returns the failures with the domains, sorted,
// whose agent each failed send addressed.
func (d *DesiredState) sendDrop(ctx context.Context, od ownerDrop, s Sender) (failed []string, err error) {
	var errs []error
	note := func(dom string, err error) {
		if err != nil {
			errs = append(errs, err)
			if dom != "" {
				failed = append(failed, dom)
			}
		}
	}
	for _, e := range od.entries {
		if e.Release != nil {
			note(cmp.Or(e.Domain, overlayDomain), e.Release(ctx, s))
		}
	}
	for _, x := range od.shared {
		sendShared(ctx, s, []sharedSend{x}, func(err error) { note(ruleDomain(x.method), err) })
	}
	for _, r := range od.rules {
		_, err := s.Send(ctx, "Network.removeInterception", r)
		note("Network", err)
	}
	releaseNetworkSettings(ctx, s, od.settingsBefore, od.settingsAfter, func(err error) { note("Network", err) })
	if od.parts {
		_, err := s.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": d.Boot.Script()})
		note("Page", err)
	}
	if od.headerSend != nil {
		_, err := s.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": od.headerSend})
		note("Network", err)
	}
	slices.Sort(failed)
	return slices.Compact(failed), errors.Join(errs...)
}
