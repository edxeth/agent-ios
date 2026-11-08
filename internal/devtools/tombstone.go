package devtools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Tombstones (W4 repair round 4). A removal of an interception rule
// ("network unroute", clearing credentials, "set offline off") takes the
// rule out of the desired state and sends Network.removeInterception in a
// change, which repeats it on every provisional target (core Change). A
// provisional target that refuses the repeat keeps the rule; when it
// commits, the page holds a rule that no model entry describes, so neither
// a later change nor the release would remove it. The rule's requests get
// their answer from the interceptor's safety net (intercepted); the
// tombstone removes the rule itself:
//
//   - removeRule records a tombstone before its sends: a desired-state
//     entry owned by the rule's owner (so by the session), with no Apply
//     (a new target or connection never holds the rule: WebKit clears the
//     rules of a disconnected frontend, InspectorNetworkAgent::disable),
//     and the rule in interceptor.tombs. The removal drops it again when
//     its change succeeded, that is, when every target took the removal.
//   - At each commit of a provisional target, applyTombstones sends the
//     removal of every tombstone's rule to the committed target, once: the
//     target that the commit event names (newTargetId), and only while it
//     is the page's active target.
//   - The lease release runs the tombstones of the session (the entry's
//     Release), once each, on the active target.
//
// Each attempt goes to one target only (core.Unforwarded): the other
// targets do not hold the rule. A rule that an owner holds again is not
// removed (WebKit keeps one copy of equal rules), and its tombstone goes.
// An attempt that succeeds, or that WebKit answers with its error for an
// unknown rule (missingRule), drops the tombstone; another failure keeps
// it for the next commit or the release. Nothing retries in a loop.

// missingRuleError is WebKit's error text for Network.removeInterception of
// a rule the target does not hold: InspectorNetworkAgent::removeInterception
// in WebKit 7615.3.12.11.2 (iOS 16.7), WebKitGTK 2.54.2 and main 45208912
// (Source/WebCore/inspector/agents/InspectorNetworkAgent.cpp).
const missingRuleError = "Missing intercept for given url, given isRegex, and given stage"

// missingRule reports whether err is WebKit's answer for a rule the target
// does not hold. It is the only failure a tombstone's removal tolerates.
func missingRule(err error) bool {
	var pe *webkit.ErrorData
	return errors.As(err, &pe) && pe.Message == missingRuleError
}

// activeTargetOf returns the active page target of c, or "" when c does not
// name its targets. Only connections that name their targets (the daemon's
// page connection) report commits of provisional targets.
func activeTargetOf(c webkit.Caller) string {
	if at, ok := c.(interface{ ActiveTarget() string }); ok {
		return at.ActiveTarget()
	}
	return ""
}

// tombstoneBudget bounds the change that applies the tombstones to a
// committed target.
const tombstoneBudget = 5 * time.Second

// putTombstone records a tombstone of o's rule r and returns its entry key.
// It runs inside the removal's change, before the removal is sent.
func (ic *interceptor) putTombstone(o core.Owner, r core.InterceptRule) string {
	ic.mu.Lock()
	ic.nextTomb++
	key := fmt.Sprintf("devtools:tombstone:%s:%d", o, ic.nextTomb)
	ic.tombs[key] = r
	ic.mu.Unlock()
	ic.p.ps.State.Put(core.Entry{
		Key: key, Owner: o, Apply: noop,
		Release: func(ctx context.Context, s core.Sender) error {
			defer ic.forgetTombstone(key)
			if err := ic.removeTombstoneRule(ctx, s, r); err != nil {
				return fmt.Errorf("lease release: %w", err)
			}
			return nil
		},
	})
	return key
}

// forgetTombstone removes the tombstone key from the interceptor.
func (ic *interceptor) forgetTombstone(key string) {
	ic.mu.Lock()
	delete(ic.tombs, key)
	ic.mu.Unlock()
}

// dropTombstones removes the tombstones keys and their entries: their
// removal reached every target.
func (ic *interceptor) dropTombstones(keys []string) {
	for _, k := range keys {
		ic.forgetTombstone(k)
		ic.p.ps.State.Remove(k)
	}
}

// dropReleased is Page.dropReleased for the interceptor: the routes,
// offline mode and credentials of session whose entries are gone (the
// requests that the routes' delays hold are continued, as the routes'
// Release does), and every tombstone whose entry is gone. has reports
// whether the desired state holds an entry; it is called without ic.mu.
func (ic *interceptor) dropReleased(session string, has func(key string) bool) {
	routes := !has("devtools:route:" + session)
	offline := !has("devtools:offline:" + session)
	creds := !has("devtools:credentials:" + session)
	ic.mu.Lock()
	tombs := make([]string, 0, len(ic.tombs))
	for k := range ic.tombs {
		tombs = append(tombs, k)
	}
	ic.mu.Unlock()
	var gone []string
	for _, k := range tombs {
		if !has(k) {
			gone = append(gone, k)
		}
	}
	ic.mu.Lock()
	if routes {
		var keep []*Route
		for _, r := range ic.routes {
			if r.session != session {
				keep = append(keep, r)
			}
		}
		ic.routes = keep
	}
	if offline {
		delete(ic.offline, session)
	}
	if creds {
		delete(ic.creds, session)
	}
	for _, k := range gone {
		delete(ic.tombs, k)
	}
	ic.mu.Unlock()
	if routes {
		ic.continueHeld(session)
	}
}

// removeTombstoneRule makes one attempt to remove the tombstone rule r from
// the target that s addresses. It sends nothing when an owner holds r
// again. It succeeds when the target removed r or never held it.
func (ic *interceptor) removeTombstoneRule(ctx context.Context, s core.Sender, r core.InterceptRule) error {
	for _, rules := range ic.p.ps.State.InterceptRules() {
		if slices.Contains(rules, r) {
			return nil
		}
	}
	if _, err := s.Send(core.Unforwarded(ctx), "Network.removeInterception", r); err != nil && !missingRule(err) {
		return fmt.Errorf("removing the %s that a page target kept after a refused removal: %w", ic.ruleName(r), err)
	}
	return nil
}

// applyTombstones makes one attempt per tombstone to remove its rule from
// target, the provisional target that just committed. It runs in its own
// goroutine after the commit event. The change sends to the desired
// state's active target when it begins, which is target unless a later
// commit replaced target; then target is gone with its rules, nothing is
// sent, and the later commit applies the tombstones to its own target.
func (ic *interceptor) applyTombstones(target string) {
	ic.mu.Lock()
	keys := make([]string, 0, len(ic.tombs))
	for k := range ic.tombs {
		keys = append(keys, k)
	}
	ic.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	slices.Sort(keys)
	ctx, cancel := context.WithTimeout(context.Background(), tombstoneBudget)
	defer cancel()
	err := ic.p.ps.State.Change(ctx, ic.p.ps.Caller, func(c webkit.Caller) error {
		if activeTargetOf(ic.p.ps.Caller) != target {
			return nil
		}
		for _, k := range keys {
			ic.mu.Lock()
			r, ok := ic.tombs[k]
			ic.mu.Unlock()
			if !ok {
				continue // dropped meanwhile: its removal succeeded, or the lease ended
			}
			if err := ic.removeTombstoneRule(ctx, c, r); err != nil {
				ic.p.logf("commit of a provisional target: %v; the lease release tries again", err)
				continue
			}
			ic.dropTombstones([]string{k})
		}
		return nil
	})
	if err != nil {
		ic.p.logf("commit of a provisional target: applying the interception tombstones: %v", err)
	}
}
