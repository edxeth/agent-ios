package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// The release and attach lifecycle (design 2.4, 2.5 and 2.10; core-events
// round 3).
//
// A release removes the released session's owners from the desired state.
// Every setup sends a snapshot of that state: a new connection (takeover,
// reopen, background recovery), a provisional target, and the context
// replay, which reads Needs("Runtime"). Three rules keep a released
// session's state off every target installed after its release began:
//
//   - The removal never waits for the change lock (forgetSession). It runs
//     under d.mu only, at the end of every release (Manager.releasePage),
//     whatever happened before it: a change that holds the lock past the
//     release deadline, a lost connection, a failed cleanup. So no release
//     ends while the model holds an owner of its session.
//   - A change made inside an operation adds nothing to the model once that
//     operation ended (refusedLocked). A release ends every operation of the
//     page before the removal, so a change of the released session, running
//     or still waiting for the lock, cannot add its owners back. Its caller
//     sends nothing either (opCaller).
//   - The removal increases relGen in the same critical section. A setup
//     takes relGen with its snapshot (applyConn). A new connection is
//     installed only while no release runs and relGen is unchanged
//     (Manager.reattach, Manager.recover); otherwise it is closed and the
//     page is set up again with the current state. A provisional target's
//     setup runs again (Apply). The context replay runs as a change and
//     checks Needs then, so it sees the removal.
//
// A removal on a live connection whose cleanup was not sent (the change
// lock stayed taken until the release deadline), and a cleanup send that
// failed (releaseOwners), leave the page-side state of the agents they
// touched unknown (d.unknown). Design 2.4 keeps such a cleanup pending
// and never reconnects only to clean up; the agents are reset by the next
// change that uses them (legacyCall, resetAgent), by a new connection's
// setup, and by the release's background reset
// (Manager.resetUnknownAgents) once the lock is free. A provisional setup
// needs no mark: it resets every agent of the target's record that holds
// state or is not needed (resetCalls), and a needed agent without state
// that it keeps has nothing of the released session.
//
// The pending cleanup is a transaction (core-events round 4):
//
//   - Each mark carries the sequence number of the release that set it
//     (markSeq). A reset reads the mark before it takes the state it
//     sends and removes the mark only when it is unchanged, so a release
//     that marks the domain again while the reset runs keeps its own
//     obligation; that release's background reset services it.
//   - A cleanup send that fails marks its domain, also after the owner left
//     the model, and the release reports the failure; only a reset that
//     succeeds removes the mark. A cleanup send (of a release or of a
//     reset) forwarded to a provisional target that fails is kept with
//     that target and marks its domain when the target commits, or at once
//     when it committed already (cleanupFailedLocked); a target destroyed
//     before commit takes the state with it. A cleanup send that a commit
//     cut off (ErrTargetSwapped), or that its target answered before the
//     commit but forwarded after it, marks its domain until its forwarded
//     send to the target that is active now succeeds (enqueueLocked,
//     markActiveCleanupLocked, core-events round 9a).
//   - A forwarded cleanup send that is still on its way to a provisional
//     target (in flight or queued) when that target commits marks its
//     domain until it succeeds (markUnsentCleanupLocked, core-events round
//     8): the release or reset that queued it may have ended, and removed
//     its own mark, because its own target was cleaned. The commit first
//     ends the marks of the replaced target's unsent sends, whose backend
//     goes with it. A cleanup send or disable that the active target
//     refuses as not enabled is still forwarded (changeSender.Send).
//   - A setup of the active target (Session.ReapplySetup, the recovery of a
//     failed provisional setup) and a setup that runs again reset the
//     target's Network agent while it may hold requests: an active Network
//     reset (applyResetting).

// errChangeRefused: the operation of a desired-state change ended (a
// release, lease break, deadline or End), so the change recorded nothing
// more.
var errChangeRefused = errors.New("the operation ended, so its page-state change was not recorded")

// ReleaseGen returns the release generation (relGen).
func (d *DesiredState) ReleaseGen() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.relGen
}

// refusedLocked reports whether the change in progress must not add owned
// state because its operation ended, and notes the refusal for the change's
// end (errChangeRefused). Changes run one at a time, so every addition
// while d.cur is set is that change's. The caller holds d.mu.
func (d *DesiredState) refusedLocked() bool {
	if d.cur == nil || d.cur.ended == nil || !d.cur.ended() {
		return false
	}
	d.cur.refused = true
	return true
}

// forgetSession removes every owner of session from the model and
// increases relGen, in one critical section, without the change lock and
// without sending anything. When the connection is live, the domains the
// removed owners touched (their cleanup was not sent) and the domains of
// failed, whose cleanup send failed, become unknown with a new mark. It
// reports whether a cleanup is pending on the live connection after this
// release: one of its own marks, or a mark that a forwarded cleanup send
// of the release set when it failed on the committed target
// (cleanupFailedLocked).
func (d *DesiredState) forgetSession(session string, live bool, failed []string) (pending bool) {
	return d.forgetOwners(func(o Owner) bool { return strings.HasSuffix(string(o), ":"+session) }, live, failed)
}

// forgetOwners is forgetSession for the owners that match selects, for
// example one CDP attachment's owner (Manager.ReleaseOwner).
func (d *DesiredState) forgetOwners(match func(Owner) bool, live bool, failed []string) (pending bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.markSeq++
	mark := func(dom string) {
		if !live || dom == "" {
			return
		}
		if d.unknown == nil {
			d.unknown = map[string]uint64{}
		}
		d.unknown[dom] = d.markSeq
		pending = true
	}
	for _, o := range d.allOwnersLocked() {
		if !match(o) {
			continue
		}
		od := d.dropOwnerLocked(o, "")
		for dom := range od.touched {
			mark(dom)
		}
	}
	for _, dom := range failed {
		mark(dom)
	}
	d.relGen++
	return pending || (live && len(d.unknown) > 0)
}

// markUnknownLocked makes dom unknown with a new mark. The caller holds
// d.mu.
func (d *DesiredState) markUnknownLocked(dom string) {
	d.markSeq++
	if d.unknown == nil {
		d.unknown = map[string]uint64{}
	}
	d.unknown[dom] = d.markSeq
}

// unknownDomains returns the unknown domains, sorted.
func (d *DesiredState) unknownDomains() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(maps.Keys(d.unknown))
}

// isUnknown reports whether dom is unknown.
func (d *DesiredState) isUnknown(dom string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unknown[dom] != 0
}

// isActive reports whether target is the active target of the model
// (Apply on a new connection sets it; a commit moves it, followTargets).
func (d *DesiredState) isActive(target string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return target != "" && target == d.active
}

// setupHolds is what an active Network reset of a setup, and the record of
// a Network.disable, need from the page's scheduler (Scheduler).
type setupHolds interface {
	// holdAdmission closes admission without waiting for the running
	// operations, until reopen.
	holdAdmission() (reopen func())
	// resetTargetHolds continues every held request of target through the
	// control lane and records each as reset (ResetHeldRequests).
	resetTargetHolds(ctx context.Context, target string) ([]string, error)
	// forgetTargetHolds records every held request of target as reset
	// without sending (forgetHeldRequests).
	forgetTargetHolds(target string) []string
	// disabledOn is forgetTargetHolds for a Network.disable that c's
	// target replied to, when c is the page's installed connection.
	disabledOn(c webkit.Caller, target string)
}

// useHolds records the page's scheduler for the active Network resets of
// later setups (applyResetting) and for networkDisabled.
func (d *DesiredState) useHolds(h setupHolds) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holds = h
}

// networkDisabled records that target's Network.disable succeeded
// (webkit.SessionOptions.NetworkDisabled; it runs in the session's read
// loop): WebKit continued every request the target held, so each hold the
// scheduler lists for it is recorded as reset, and a later completion
// fails with ErrRequestReset instead of WebKit's "Missing pending
// intercept ...". Every path that disables Network gets this record: a
// setup's reset batch, the Network reinitialization barrier, the reset of
// an unknown agent, the release's disable of the last owner, a forwarded
// change and the session's agent reset. Holds that arrive after the reply
// are new holds and stay open. A connection that is not installed (the
// setup of a new connection) is left out: newConnectionReset records the
// holds when it is installed and reports them in its gap.
func (d *DesiredState) networkDisabled(c webkit.Caller, target string) {
	d.mu.Lock()
	h := d.holds
	d.mu.Unlock()
	if h != nil {
		h.disabledOn(c, target)
	}
}

// applyResetting is one Apply on target, a target that can hold requests
// the scheduler tracks: the active target, or a provisional target whose
// setup runs again after the target ran (paused-on-start targets are
// resumed after ResumeAfter even when their setup has not ended). Its
// resetCalls send Network.disable, which continues every held request of
// the target (InspectorNetworkAgent::disable), so it runs as the Network
// reinitialization barrier does (design 2.4, 2.10):
//
//  1. Admission is closed (holdAdmission). It does not wait for running
//     operations: the session's setup gate refuses or holds every ordinary
//     send to the target until the setup ends, and an operation waiting
//     at that gate would never end before it.
//  2. The held requests of the target are continued through the control
//     lane and recorded as reset (resetTargetHolds); a later completion
//     fails with ErrRequestReset.
//  3. The setup sends its reset batch. The reply to its Network.disable
//     records every hold of the target that arrived meanwhile as reset
//     too (networkDisabled).
//  4. The gap is reported to the Network owners with the requests that
//     step 2 continued, and admission reopens.
//
// Holds of other targets are not touched: WebKit holds a request in the
// backend of the target that reported it.
func (d *DesiredState) applyResetting(ctx context.Context, s Sender, target string) (uint64, error) {
	d.mu.Lock()
	h := d.holds
	d.mu.Unlock()
	if h == nil {
		return d.applyConn(ctx, s)
	}
	start := time.Now()
	reopen := h.holdAdmission()
	defer reopen()
	ids, err := h.resetTargetHolds(ctx, target)
	if err != nil {
		return 0, fmt.Errorf("applying desired state: continuing the held requests of page target %s before its Network reset: %w", target, err)
	}
	gen, err := d.applyConn(ctx, s)
	if err == nil || networkDisableWritten(err) {
		slices.Sort(ids)
		d.notifyNetworkReset(NetworkGap{Start: start, End: time.Now(), Requests: ids})
	}
	return gen, err
}

// resetAgent brings the agent of an unknown domain dom on the change's
// target c back to the desired state, then marks it known: its reset
// (agentResetCalls; a refusal because it is not enabled is no failure)
// and, while dom is needed, its enable and its state. Network runs the
// reinitialization barrier (NetworkReset) while it is needed, and else
// continues the held requests before its disable; the caller holds the
// page (an operation, or a barrier). It runs inside a change.
//
// It removes only the mark it read before it took the state it sends: a
// release that marked dom again meanwhile removed more state, which this
// reset may have sent, so that mark stays for the next reset. A forwarded
// send of the reset that is still on its way to a target that committed
// meanwhile keeps the cleanup pending instead (endMarkLocked). Its sends
// are cleanup sends (asCleanup): a forwarded one that fails keeps the
// cleanup pending for its target.
func resetAgent(ctx context.Context, ps *PageSession, c Sender, dom string) error {
	ctx = asCleanup(ctx)
	d := ps.State
	d.mu.Lock()
	mark := d.unknown[dom]
	d.mu.Unlock()
	var err error
	switch {
	case dom == "Network" && d.Needs("Network"):
		err = NetworkReset(ctx, ps.Sched, c, d)
	default:
		if dom == "Network" {
			if _, err := ps.Sched.ResetHeldRequests(ctx); err != nil {
				return fmt.Errorf("resetting Network after a release without cleanup: %w", err)
			}
		}
		err = resetAndApplyDomain(ctx, d, c, dom)
	}
	if err != nil {
		return fmt.Errorf("resetting %s after a release without cleanup: %w", dom, err)
	}
	d.mu.Lock()
	d.endMarkLocked(dom, mark)
	d.mu.Unlock()
	return nil
}

// ResetUnknownAgent services the pending cleanup of dom on the change's
// caller c: when a release left the agent's page-side state unknown (its
// cleanup was not sent or failed), it resets the agent first (resetAgent).
// It runs inside a change (DesiredState.Change) of an operation. Code
// outside core that starts state of an agent that a release can leave
// behind, for example a ScriptProfiler run or a Timeline recording, which
// survive a same-target navigation, calls it in its change before the
// start, so that the reset comes before the new state and never after it
// (core's background reset waits for the change lock, Manager.resetUnknownAgents).
func ResetUnknownAgent(ctx context.Context, ps *PageSession, c Sender, dom string) error {
	if !ps.State.isUnknown(dom) {
		return nil
	}
	return resetAgent(ctx, ps, c, dom)
}

// resetAndApplyDomain sends the reset of dom and, while dom is needed, its
// enable and the parts of the desired state that belong to it: for Page
// the bootstrap script, and every generic or shared entry of dom.
func resetAndApplyDomain(ctx context.Context, d *DesiredState, c Sender, dom string) error {
	if dom == overlayDomain {
		// The overlay has no agent to disable, and its entries are never
		// replayed (resetCalls resets it on every setup too).
		for _, call := range overlayResetCalls {
			if _, err := c.Send(ctx, call.Method, call.Params); err != nil {
				return err
			}
		}
		return nil
	}
	for _, call := range agentResetCalls(dom) {
		if _, err := c.Send(ctx, call.Method, call.Params); err != nil && !notEnabled(err) {
			return err
		}
	}
	d.mu.Lock()
	needed := d.needsLocked(dom)
	var entries []Entry
	for k, e := range d.entries {
		switch {
		case e.Domain != dom:
		case e.Scoped:
			// The reset removed it (its ids named the agent's old
			// state), and it is never sent again.
			d.ver++
			delete(d.entries, k)
		default:
			entries = append(entries, e)
		}
	}
	for _, e := range d.sharedEntriesLocked() {
		if e.Domain == dom {
			entries = append(entries, e)
		}
	}
	d.mu.Unlock()
	if !needed {
		return nil
	}
	if _, err := c.Send(ctx, dom+".enable", nil); err != nil {
		return err
	}
	if dom == "Page" {
		if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": d.Boot.Script()}); err != nil {
			return err
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if a.Rank != b.Rank {
			return a.Rank - b.Rank
		}
		return strings.Compare(a.Key, b.Key)
	})
	for _, e := range entries {
		if err := e.Apply(ctx, c); err != nil {
			return fmt.Errorf("applying desired state %q: %w", e.Key, err)
		}
	}
	return nil
}
