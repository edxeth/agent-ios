package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// followCommits keeps the target-specific state (Scoped entries and
// breakpoint ids) with the target it belongs to, from the Target events of
// a connection (design 2.7): a commit moves it to the new target, and a
// provisional target destroyed before commit takes only its own staged ids
// with it. The handlers run in the connection's read loop.
//
// pending, when not nil, is called after a commit that
// left a cleanup pending on the committed target (commitTarget): the
// committed target may hold released state until its agents are reset
// (Manager.commitLeftCleanup). pending runs from a queued event
// subscription (Subscribe; on a webkit.Session it runs outside the read
// loop), after the commit's bookkeeping in the read loop; several commits
// before it runs give one call.
func (d *DesiredState) followCommits(c interface {
	SubscribeSync(method string, h webkit.EventHandler) func()
}, pending func(),
) {
	const commit = "Target.didCommitProvisionalTarget"
	var marked atomic.Bool
	c.SubscribeSync(commit, func(_ string, params json.RawMessage) {
		var p struct {
			NewTargetID string `json:"newTargetId"`
		}
		if json.Unmarshal(params, &p) == nil && p.NewTargetID != "" && d.commitTarget(p.NewTargetID) {
			marked.Store(true)
		}
	})
	if as, ok := c.(interface {
		Subscribe(method string, h webkit.EventHandler) func()
	}); ok && pending != nil {
		as.Subscribe(commit, func(string, json.RawMessage) {
			if marked.Swap(false) {
				pending()
			}
		})
	}
	c.SubscribeSync("Target.targetDestroyed", func(_ string, params json.RawMessage) {
		var p struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(params, &p) == nil && p.TargetID != "" {
			d.abandonTarget(p.TargetID)
		}
	})
}

// commitTarget makes target the active target: the old document's Scoped
// entries are dropped without a reset (their ids do not exist on target),
// and the breakpoint ids that target's setup got replace the old ones. It
// reports whether the commit marked a domain unknown: a cleanup is pending
// on target then.
func (d *DesiredState) commitTarget(target string) (marked bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	seq := d.markSeq
	defer func() { marked = d.markSeq != seq }()
	if old := d.syncs[d.active]; old != nil && d.active != target {
		// The commit replaces the old active target, and its backend with
		// the state that its unsent cleanup sends were to remove, so the
		// marks they carry end (settleLocked) before target's own are set
		// below. A mark that stayed would make target's unsent cleanup
		// look pending already (markUnsentCleanupLocked), and then end with
		// the old target's record (abandonTarget).
		for _, q := range append([]*queued{old.sending}, old.queue...) {
			if q != nil {
				d.settleLocked(q)
			}
		}
	}
	for k, e := range d.entries {
		if e.Scoped {
			delete(d.entries, k)
		}
	}
	if ids := d.staged[target]; len(ids) > 0 {
		if d.bpIDs == nil {
			d.bpIDs = map[string]string{}
		}
		for id, cur := range ids {
			d.bpIDs[id] = cur
		}
	}
	delete(d.staged, target)
	delete(d.stagedVia, target)
	if ts := d.syncs[target]; ts != nil {
		// Changes queued before the commit still go to it; the next
		// change drops the record once the queue is empty.
		ts.committed = true
		// The cleanup sends that failed on it are pending on the active
		// target now (cleanupFailedLocked), and so are those still on
		// their way to it (markUnsentCleanupLocked).
		for dom := range ts.unclean {
			d.markUnknownLocked(dom)
		}
		ts.unclean = nil
		d.markUnsentCleanupLocked(ts)
	}
	d.active = target
	return // marked: set by the deferred comparison of markSeq
}

// abandonTarget drops the staged breakpoint ids of a destroyed target. The
// active target's state is unchanged.
func (d *DesiredState) abandonTarget(target string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.staged, target)
	delete(d.stagedVia, target)
	if ts := d.syncs[target]; ts != nil {
		d.dropQueueLocked(ts) // the target took its state with it
		delete(d.syncs, target)
	}
}

// ReleaseDomain removes owner from domain's owners. It reports whether
// domain has no owner left, that is, whether domain.disable must be sent.
// A domain that owner did not hold is reported as not last.
func (d *DesiredState) ReleaseDomain(owner Owner, domain string) (last bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	owners := d.domains[domain]
	if !owners[owner] {
		return false
	}
	d.ver++
	delete(owners, owner)
	return len(owners) == 0
}

// RemoveInterception removes one of owner's interception rules and reports
// whether any rule of any owner is left.
func (d *DesiredState) RemoveInterception(owner Owner, r InterceptRule) (rulesLeft bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ver++
	rules := d.rules[owner]
	for i, x := range rules {
		if x == r {
			d.rules[owner] = append(rules[:i:i], rules[i+1:]...)
			break
		}
	}
	if len(d.rules[owner]) == 0 {
		delete(d.rules, owner)
	}
	return len(d.rules) > 0
}

// AllOwners returns every owner that holds any entry.
func (d *DesiredState) AllOwners() []Owner {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.allOwnersLocked()
}

func (d *DesiredState) allOwnersLocked() []Owner {
	set := map[Owner]bool{}
	for _, owners := range d.domains {
		for o := range owners {
			set[o] = true
		}
	}
	for o := range d.headers {
		set[o] = true
	}
	for o := range d.rules {
		set[o] = true
	}
	for _, m := range []map[Owner]bool{d.intercept, d.caching} {
		for o := range m {
			set[o] = true
		}
	}
	for o := range d.conds {
		set[o] = true
	}
	for _, e := range d.entries {
		set[e.Owner] = true
	}
	for _, vals := range d.shared {
		for _, v := range vals {
			set[v.owner] = true
		}
	}
	for _, o := range d.partOwner {
		set[o] = true
	}
	out := make([]Owner, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ApplyNetwork sends the complete Network state after Network.enable
// (step 4 of the Network reinitialization barrier, design 2.4): the
// effective extra headers, interception enablement and every rule, the
// resource-caching override and the emulated conditions.
func (d *DesiredState) ApplyNetwork(ctx context.Context, s Sender) error {
	d.mu.Lock()
	headers := d.effectiveHeadersLocked()
	var owners []Owner
	for o := range d.rules {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })
	var rules []InterceptRule
	for _, o := range owners {
		rules = append(rules, d.rules[o]...)
	}
	ns := d.networkSettingsLocked()
	d.mu.Unlock()
	if len(headers) > 0 {
		if _, err := s.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": headers}); err != nil {
			return fmt.Errorf("network reset: headers: %w", err)
		}
	}
	if ns.intercept {
		if _, err := s.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": true}); err != nil {
			return fmt.Errorf("network reset: interception: %w", err)
		}
		for _, r := range rules {
			if _, err := s.Send(ctx, "Network.addInterception", r); err != nil {
				return fmt.Errorf("network reset: rule %q: %w", r.URL, err)
			}
		}
	}
	if err := applyNetworkSettings(ctx, s, ns); err != nil {
		return fmt.Errorf("network reset: %w", err)
	}
	return nil
}

// NetworkGap is the discontinuity that a Network reinitialization barrier
// reports to every remaining Network owner (design 2.4): requests in flight
// across it are closed as capture_reset, and earlier response bodies can no
// longer be read.
type NetworkGap struct {
	Start, End time.Time
	Requests   []string // held requests continued unmodified by the barrier
}

// WatchNetworkReset registers fn for owner; fn runs after each barrier
// while owner owns Network. It returns a function that removes fn.
func (d *DesiredState) WatchNetworkReset(owner Owner, fn func(NetworkGap)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gapWatch == nil {
		d.gapWatch = map[Owner][]*func(NetworkGap){}
	}
	p := &fn
	d.gapWatch[owner] = append(d.gapWatch[owner], p)
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		ws := d.gapWatch[owner]
		for i, x := range ws {
			if x == p {
				d.gapWatch[owner] = append(ws[:i:i], ws[i+1:]...)
				break
			}
		}
	}
}

// notifyNetworkReset calls the watchers of the current Network owners.
func (d *DesiredState) notifyNetworkReset(g NetworkGap) {
	d.mu.Lock()
	var fns []func(NetworkGap)
	for o := range d.domains["Network"] {
		for _, p := range d.gapWatch[o] {
			fns = append(fns, *p)
		}
	}
	d.mu.Unlock()
	for _, fn := range fns {
		fn(g)
	}
}
