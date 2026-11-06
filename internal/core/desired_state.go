package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Sender sends one protocol command. webkit.Caller and webkit.TargetCaller
// (a provisional target during setup) both satisfy it.
type Sender interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Owner names who holds a desired-state entry: a session, a capture, a
// legacy operation, the dialog bridge or a CDP attachment (design 2.4).
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
	Key     string // unique; entries apply in key order
	Owner   Owner
	Apply   func(ctx context.Context, s Sender) error
	Release func(ctx context.Context, s Sender) error
}

// DesiredState is the per-page model of state that iwdp-mcp set in WebKit
// (design 2.4). It is not a history of sends: Apply sends the complete state
// to a new or provisional target, and Release removes one owner's entries
// and sends what that changes.
type DesiredState struct {
	mu        sync.Mutex
	domains   map[string]map[Owner]bool
	headers   map[Owner]map[string]string
	rules     map[Owner][]InterceptRule
	entries   map[string]Entry
	partOwner map[string]Owner
	Boot      *BootstrapParts
}

// NewDesiredState returns an empty model with its bootstrap parts.
func NewDesiredState() *DesiredState {
	return &DesiredState{
		domains:   make(map[string]map[Owner]bool),
		headers:   make(map[Owner]map[string]string),
		rules:     make(map[Owner][]InterceptRule),
		entries:   make(map[string]Entry),
		partOwner: make(map[string]Owner),
		Boot:      NewBootstrapParts(),
	}
}

// Acquire records that owner needs domain enabled. It reports whether this
// is the first owner, that is, whether domain.enable must be sent now.
func (d *DesiredState) Acquire(owner Owner, domain string) (first bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.domains[domain] == nil {
		d.domains[domain] = make(map[Owner]bool)
	}
	first = len(d.domains[domain]) == 0
	d.domains[domain][owner] = true
	return first
}

// Owners returns the owners of domain.
func (d *DesiredState) Owners(domain string) []Owner {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Owner
	for o := range d.domains[domain] {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
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
	d.rules[owner] = append(d.rules[owner], r)
}

// Put adds or replaces a generic entry.
func (d *DesiredState) Put(e Entry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[e.Key] = e
}

// SetPart adds or replaces a bootstrap part owned by owner.
func (d *DesiredState) SetPart(owner Owner, name string, rank int, source string) error {
	if err := d.Boot.Set(name, rank, source); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.partOwner[name] = owner
	return nil
}

// Apply sends the complete desired state to s, which is a new connection's
// target or a provisional target before commit (design 2.7): Page.enable,
// Page.setBootstrapScript with all parts, every owned domain, the effective
// extra headers, interception rules, then the generic entries.
func (d *DesiredState) Apply(ctx context.Context, s Sender) error {
	d.mu.Lock()
	script := d.Boot.Script()
	var domains []string
	for dom, owners := range d.domains {
		if len(owners) > 0 && dom != "Page" {
			domains = append(domains, dom)
		}
	}
	sort.Strings(domains)
	page := len(d.domains["Page"]) > 0 || script != ""
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
	var entries []Entry
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	d.mu.Unlock()

	send := func(method string, params any) error {
		if _, err := s.Send(ctx, method, params); err != nil {
			return fmt.Errorf("applying desired state: %w", err)
		}
		return nil
	}
	if page {
		if err := send("Page.enable", nil); err != nil {
			return err
		}
	}
	if script != "" {
		if err := send("Page.setBootstrapScript", map[string]string{"source": script}); err != nil {
			return err
		}
	}
	for _, dom := range domains {
		if err := send(dom+".enable", nil); err != nil {
			return err
		}
	}
	if network && len(headers) > 0 {
		if err := send("Network.setExtraHTTPHeaders", map[string]any{"headers": headers}); err != nil {
			return err
		}
	}
	if network && len(rules) > 0 {
		if err := send("Network.setInterceptionEnabled", map[string]bool{"enabled": true}); err != nil {
			return err
		}
		for _, r := range rules {
			if err := send("Network.addInterception", r); err != nil {
				return err
			}
		}
	}
	for _, e := range entries {
		if err := e.Apply(ctx, s); err != nil {
			return fmt.Errorf("applying desired state %q: %w", e.Key, err)
		}
	}
	return nil
}

// ReleaseResult reports what a Release could not do itself.
type ReleaseResult struct {
	// NeedsNetworkReset: a header name lost its last owner while Network
	// stays owned; the caller must run the Network reinitialization barrier,
	// which applies the complete remaining header map.
	NeedsNetworkReset bool
}

// Release removes every entry of owner and sends what that changes: the
// owner's generic entries are released, its interception rules removed
// (interception is disabled when no rule remains), the bootstrap script is
// re-sent without its parts, a header value that changed while every name
// keeps an owner is sent as the complete effective map, and each domain
// whose last owner it was is disabled. Paused requests must be continued
// through the control lane before Release (Scheduler.Release does that).
func (d *DesiredState) Release(ctx context.Context, owner Owner, s Sender) (ReleaseResult, error) {
	d.mu.Lock()
	var res ReleaseResult
	var entries []Entry
	for k, e := range d.entries {
		if e.Owner == owner {
			entries = append(entries, e)
			delete(d.entries, k)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	rules := d.rules[owner]
	delete(d.rules, owner)
	rulesLeft := 0
	for _, rs := range d.rules {
		rulesLeft += len(rs)
	}
	var parts []string
	for name, o := range d.partOwner {
		if o == owner {
			parts = append(parts, name)
			delete(d.partOwner, name)
		}
	}
	headersBefore, hadHeaders := d.effectiveHeadersLocked(), d.headers[owner] != nil
	delete(d.headers, owner)
	var disable []string
	for dom, owners := range d.domains {
		if owners[owner] {
			delete(owners, owner)
			if len(owners) == 0 {
				disable = append(disable, dom)
			}
		}
	}
	// With no Network owner left, Network.disable alone clears the map.
	var headerSend map[string]string
	if hadHeaders && len(d.domains["Network"]) > 0 {
		ch := d.headerChangeLocked(headersBefore)
		res.NeedsNetworkReset = ch.NeedsNetworkReset
		if !ch.NeedsNetworkReset {
			headerSend = ch.Send
		}
	}
	d.mu.Unlock()
	sort.Strings(disable)

	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range entries {
		if e.Release != nil {
			note(e.Release(ctx, s))
		}
	}
	for _, r := range rules {
		_, err := s.Send(ctx, "Network.removeInterception", r)
		note(err)
	}
	if len(rules) > 0 && rulesLeft == 0 {
		_, err := s.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": false})
		note(err)
	}
	if len(parts) > 0 {
		for _, p := range parts {
			d.Boot.Remove(p)
		}
		_, err := s.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": d.Boot.Script()})
		note(err)
	}
	if headerSend != nil {
		_, err := s.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": headerSend})
		note(err)
	}
	for _, dom := range disable {
		_, err := s.Send(ctx, dom+".disable", nil)
		note(err)
	}
	if firstErr != nil {
		return res, fmt.Errorf("releasing desired state of %s: %w", owner, firstErr)
	}
	return res, nil
}
