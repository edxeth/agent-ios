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
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Keeping provisional targets in step with the desired state (design 2.4
// and 2.7; review finding R5-1).
//
// A provisional target's setup sends a snapshot of the desired state. Every
// later change must reach that target too, or a commit would make it active
// without the change. Web Inspector sends each change to every target; this
// model does the same with an ordered queue per target:
//
//   - Every model mutation increases d.ver under d.mu. A setup takes its
//     snapshot and registers its target (targetSync, snapV = d.ver) in the
//     same critical section.
//   - A change (beginChange) sends to the active target it began with,
//     addressed by id, and queues the same send for every registered target
//     whose snapshot predates the mutation (snapV < the version of the send).
//     A change that sends first and records afterwards (a new entry) queues
//     its send together with the record (changeSender.put).
//   - A target's queue is sent in order (FIFO) after its snapshot, one item
//     at a time, by whoever holds the target's sending slot (ts.busy): its
//     setup, which sends the whole queue and returns only when it is empty,
//     or a change that waits for its own items. Each item is completed with
//     the result of its own send, whoever sends it, and the change that
//     queued it returns that result (review finding R6-2). A change sends
//     only up to its own last item and then frees the slot, so a later
//     change's failure, or a later budget, never lands on it.
//   - An item whose target is gone (destroyed, replaced by a commit or by a
//     new connection) or whose setup failed completes without error: the
//     target took its state with it, or ReapplySetup sends the whole state.
//   - A cleanup send (asCleanup) that fails on a target keeps that cleanup
//     pending for the target (cleanupFailedLocked): its domain becomes
//     unknown (d.unknown) once the target is the active one. A cleanup send
//     that is still in flight or queued when its target commits keeps its
//     domain unknown until it succeeds (markUnsentCleanupLocked), and so
//     does one queued for a target that committed already
//     (markActiveCleanupLocked).
//   - Changes run one at a time (d.chg) but wait for their items after
//     releasing d.chg; setups never wait for a change, because a change can
//     wait for a setup (the gate of webkit.Session).

// forwardBudget bounds the sending of queued changes after the command that
// made them returned.
const forwardBudget = 10 * time.Second

// targetSync is a provisional target whose setup took its snapshot.
type targetSync struct {
	via       Sender
	snapV     uint64
	queue     []*queued
	busy      bool          // the setup or a change sends the queue
	idle      chan struct{} // closed when the current sending ends
	committed bool          // became the active target; dropped by the next change
	failed    bool          // its setup failed; ReapplySetup sends the full state
	sending   *queued       // the item pump sends now, taken from queue
	// unclean holds the domains of the cleanup sends that failed on the
	// target while it was provisional; they become unknown when it commits
	// (commitTarget).
	unclean map[string]bool
}

// queued is one forwarded send of a change, queued for one target. Its
// fields after send are guarded by DesiredState.mu.
type queued struct {
	send func(context.Context, Sender) error
	// cleanup is the domain of a cleanup send (asCleanup; "" for other
	// sends): its failure keeps that cleanup pending (cleanupFailedLocked).
	// mark is the unknown mark of cleanup that the item's success removes
	// (0 for none; markActiveCleanupLocked, markUnsentCleanupLocked,
	// endMarkLocked).
	cleanup   string
	mark      uint64
	completed bool
	err       error
	done      chan struct{} // closed on completion
}

// completeLocked records q's result. An item is completed once: by the
// sender that took it from the queue, or while it is still queued by
// dropQueueLocked. The caller holds d.mu.
func (q *queued) completeLocked(err error) {
	q.completed, q.err = true, err
	close(q.done)
}

// dropQueueLocked completes every item of ts's queue without error and
// empties it: the target is gone or gets the whole state again, so the
// marks the items carry end too (settleLocked). The caller holds d.mu.
func (d *DesiredState) dropQueueLocked(ts *targetSync) {
	for _, q := range ts.queue {
		d.settleLocked(q)
		q.completeLocked(nil)
	}
	ts.queue = nil
}

// settleLocked ends the unknown mark that q carries, unless a later mark
// replaced it: q was sent, or its target no longer needs it. The caller
// holds d.mu.
func (d *DesiredState) settleLocked(q *queued) {
	if q.mark != 0 && d.unknown[q.cleanup] == q.mark {
		delete(d.unknown, q.cleanup)
	}
	q.mark = 0
}

// markUnsentCleanupLocked keeps the cleanup sends that are still on their
// way to ts's target, in flight or queued, pending when that target
// becomes the active one (commitTarget). Until such a send succeeded, the
// target's backend may hold the state it removes (a provisional target's
// setup snapshot from before a release), and the send is the only record
// of that obligation: the release or reset that queued it may have ended,
// its marks removed, because its own target was cleaned. For each domain
// the last of these sends carries a new mark, which its success removes
// (pump): the queue is sent in order, so the earlier ones were sent by
// then, and a failure of any of them marks the domain again
// (cleanupFailedLocked). A domain that is unknown already keeps its mark:
// that obligation may be older than these sends, so their success does
// not end it. When the reset that serves that mark ends it first, the mark
// passes to the last of these sends (endMarkLocked). The caller holds
// d.mu.
func (d *DesiredState) markUnsentCleanupLocked(ts *targetSync) {
	last := map[string]*queued{}
	for _, q := range append([]*queued{ts.sending}, ts.queue...) {
		if q != nil && q.cleanup != "" {
			last[q.cleanup] = q
		}
	}
	for _, dom := range slices.Sorted(maps.Keys(last)) {
		if d.unknown[dom] == 0 {
			d.markUnknownLocked(dom)
			last[dom].mark = d.markSeq
		}
	}
}

// markActiveCleanupLocked applies the rule of markUnsentCleanupLocked to
// q, a cleanup send queued for ts, the record of the target that is active
// already. A change forwards a call after its own target answered, outside
// d.mu, so that target can commit before the enqueue: after a successful
// reply, after a refusal because the agent is not enabled, or when the
// commit cut the call off (ErrTargetSwapped; changeSender.Send). The
// commit then saw nothing on its way, and q is the only record of the
// cleanup the target is owed. q is the last cleanup send of its domain in
// ts's queue, so it carries a new mark, which its success removes (pump),
// unless the domain is unknown with a mark that none of ts's unsent sends
// carries: that obligation may be older than q, so q's success does not
// end it. The caller holds d.mu and appends q to ts.queue.
func (d *DesiredState) markActiveCleanupLocked(ts *targetSync, q *queued) {
	cur := d.unknown[q.cleanup]
	if cur != 0 && !slices.ContainsFunc(append([]*queued{ts.sending}, ts.queue...), func(p *queued) bool {
		return p != nil && p.cleanup == q.cleanup && p.mark == cur
	}) {
		return
	}
	d.markUnknownLocked(q.cleanup)
	q.mark = d.markSeq
}

// endMarkLocked removes dom's unknown mark when it is still mark, the mark
// that a reset read before it took the state it sent (resetAgent). A
// cleanup send of dom that is still on its way to the active target, in
// flight or queued, keeps dom pending: that send's target may have
// committed after the reset's own target answered, and it may hold the
// state the send removes (markUnsentCleanupLocked and
// markActiveCleanupLocked leave such a send without a mark while the
// reset's mark exists). The last of these sends carries a new mark, which
// its success removes (pump). The caller holds d.mu.
func (d *DesiredState) endMarkLocked(dom string, mark uint64) {
	if mark == 0 || d.unknown[dom] != mark {
		return
	}
	delete(d.unknown, dom)
	ts := d.syncs[d.active]
	if ts == nil {
		return
	}
	var last *queued
	for _, q := range append([]*queued{ts.sending}, ts.queue...) {
		if q != nil && q.cleanup == dom {
			last = q
		}
	}
	if last != nil {
		d.markUnknownLocked(dom)
		last.mark = d.markSeq
	}
}

// registerLocked records target's setup snapshot. The caller holds d.mu and
// takes the snapshot in the same critical section. A record it replaces
// gets nothing more: the new snapshot has the whole state.
func (d *DesiredState) registerLocked(target string, via Sender) *targetSync {
	ts := &targetSync{via: via, snapV: d.ver, busy: true, idle: make(chan struct{})}
	if d.syncs == nil {
		d.syncs = map[string]*targetSync{}
	}
	if old := d.syncs[target]; old != nil {
		d.dropQueueLocked(old)
	}
	d.syncs[target] = ts
	return ts
}

// pendingItem is an item a change queued, with its target.
type pendingItem struct {
	target string
	ts     *targetSync
	q      *queued
}

// enqueueLocked queues f for every registered target other than the
// change's own whose snapshot predates version v, and records the items as
// the change's. cleanup is the domain of a cleanup send ("" for other
// sends).
//
// An item for the target that is active now, a target that committed
// after the change began, is that target's cleanup, so it keeps the
// cleanup pending until it succeeds (markActiveCleanupLocked), whether the
// change's own target answered the call or the commit cut it off. The
// release therefore reports the cleanup pending while the item is
// unanswered, and a reset that sent the call (resetAgent) passes its mark
// on to it (endMarkLocked); a failure marks the domain again
// (cleanupFailedLocked). No item for the active target means its setup
// snapshot is newer than the change, or its setup failed and ReapplySetup
// sends the whole state. The caller holds d.mu.
func (c *changeSender) enqueueLocked(v uint64, cleanup string, f func(context.Context, Sender) error) {
	targets := make([]string, 0, len(c.d.syncs))
	for t := range c.d.syncs {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		ts := c.d.syncs[t]
		if t != c.target && !ts.failed && ts.snapV < v {
			q := &queued{send: f, cleanup: cleanup, done: make(chan struct{})}
			if cleanup != "" && t == c.d.active {
				c.d.markActiveCleanupLocked(ts, q)
			}
			ts.queue = append(ts.queue, q)
			c.pending = append(c.pending, pendingItem{t, ts, q})
		}
	}
}

// targetGone reports a send error that means the target no longer exists:
// a provisional target destroyed before commit, or a target that a commit
// replaced. Its state went with it.
func targetGone(err error) bool {
	return errors.Is(err, webkit.ErrTargetAbandoned) || errors.Is(err, webkit.ErrTargetSwapped)
}

// pump sends ts's queue in order, one item at a time, completing each item
// with the result of its send, until the queue is empty, stop (nil for
// none) reports true, or ctx ends. The caller set ts.busy; pump clears it.
// It returns the failures of the items it sent, and ctx's error when ctx
// ended before the queue did.
func (d *DesiredState) pump(ctx context.Context, target string, ts *targetSync, stop func() bool) error {
	var errs []error
	d.mu.Lock()
	for d.syncs[target] == ts && len(ts.queue) > 0 && (stop == nil || !stop()) {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("page target %s: sending earlier page-state changes: %w", target, ctx.Err()))
			break
		}
		q := ts.queue[0]
		ts.queue = ts.queue[1:]
		ts.sending = q
		d.mu.Unlock()
		err := q.send(ctx, ts.via)
		d.mu.Lock()
		ts.sending = nil
		switch {
		case err == nil:
			d.settleLocked(q)
		case targetGone(err):
			err = nil
			d.dropQueueLocked(ts)
		default:
			err = fmt.Errorf("page target %s: %w", target, err)
			errs = append(errs, err)
			if q.cleanup != "" {
				d.cleanupFailedLocked(target, ts, q.cleanup)
			}
		}
		q.completeLocked(err)
	}
	if d.syncs[target] != ts {
		d.dropQueueLocked(ts) // the record was dropped while an item was sent
	}
	ts.busy = false
	close(ts.idle)
	d.mu.Unlock()
	return errors.Join(errs...)
}

// cleanupFailedLocked keeps the cleanup of the agent of domain dom pending
// on target, whose record is ts, after a cleanup send to it failed (a
// refusal, a stalled connection, the deadline): the target's backend may
// keep the released state. The obligation stays with the target that
// holds that state:
//
//   - A committed target is the active one: dom becomes unknown at once
//     with a new mark, and a reset of the active target completes it
//     (resetAgent, a new connection's setup).
//   - A provisional target keeps dom in ts.unclean; its commit makes dom
//     unknown (commitTarget), before any change can address it as the
//     active target. A reset of the old active target in the meantime does
//     not complete it.
//   - A record that is no longer target's keeps nothing: a provisional
//     target destroyed before commit took its backend and the released
//     state with it (abandonTarget drops the record), and a target whose
//     setup runs again is reset by that setup (resetCalls), or by
//     ReapplySetup when it fails.
//
// The caller holds d.mu.
func (d *DesiredState) cleanupFailedLocked(target string, ts *targetSync, dom string) {
	switch {
	case d.syncs[target] != ts:
	case ts.committed:
		d.markUnknownLocked(dom)
	default:
		if ts.unclean == nil {
			ts.unclean = map[string]bool{}
		}
		ts.unclean[dom] = true
	}
}

// agentDomain is the domain whose agent state method changes, as d.unknown
// names it.
func agentDomain(method string) string {
	dom, _, _ := strings.Cut(method, ".")
	return cmp.Or(ruleDomain(method), dom)
}

// drain sends target's whole queue unless its setup or another change
// does, and waits until it is empty. The items' failures go to the changes
// that queued them; drain fails only when ctx ends first.
func (d *DesiredState) drain(ctx context.Context, target string) error {
	for {
		d.mu.Lock()
		ts := d.syncs[target]
		if ts == nil || (!ts.busy && len(ts.queue) == 0) {
			d.mu.Unlock()
			return nil
		}
		if ctx.Err() != nil {
			d.mu.Unlock()
			return fmt.Errorf("waiting for page target %s to receive earlier page-state changes: %w", target, ctx.Err())
		}
		if ts.busy {
			idle := ts.idle
			d.mu.Unlock()
			select {
			case <-idle:
			case <-ctx.Done():
			}
			continue
		}
		ts.busy, ts.idle = true, make(chan struct{})
		d.mu.Unlock()
		_ = d.pump(ctx, target, ts, nil) // failures belong to the items' changes
	}
}

// await waits until the item it.q is sent and returns the item's result.
// While nobody else sends the target's queue, await sends it, up to and
// including the item.
func (d *DesiredState) await(ctx context.Context, it pendingItem) error {
	for {
		d.mu.Lock()
		if it.q.completed {
			err := it.q.err
			d.mu.Unlock()
			return err
		}
		if ctx.Err() != nil {
			d.mu.Unlock()
			return fmt.Errorf("waiting for page target %s to receive this page-state change: %w", it.target, ctx.Err())
		}
		// Not completed: the item is still in it.ts's queue, because every
		// path that drops a record completes the items it holds.
		if it.ts.busy {
			idle := it.ts.idle
			d.mu.Unlock()
			select {
			case <-it.q.done:
			case <-idle:
			case <-ctx.Done():
			}
			continue
		}
		it.ts.busy, it.ts.idle = true, make(chan struct{})
		d.mu.Unlock()
		// The item's result is read above; the failures of earlier items
		// belong to their own changes.
		_ = d.pump(ctx, it.target, it.ts, func() bool { return it.q.completed })
	}
}

// finishSetup ends the setup of target: on success it sends the changes
// queued since the snapshot and returns the failures of those it sent (the
// target lacks that state; the changes that queued them get them too). On
// failure it drops them (the target lacks part of the state until
// ReapplySetup sends all of it).
func (d *DesiredState) finishSetup(ctx context.Context, target string, ts *targetSync, err error) error {
	if err == nil {
		return d.pump(ctx, target, ts, nil)
	}
	d.mu.Lock()
	ts.failed, ts.busy = true, false
	d.dropQueueLocked(ts)
	close(ts.idle)
	d.mu.Unlock()
	return err
}

// changeSender sends the calls of one desired-state change. It addresses
// the target that was active when the change began and queues each
// successful call for the registered provisional targets.
type changeSender struct {
	webkit.Caller
	d       *DesiredState
	target  string        // "" when the connection does not name its targets
	pending []pendingItem // the items it queued, in order; guarded by d.mu
	// ended reports that the operation the change runs in ended (nil: the
	// change is no operation's); refused, under d.mu, that the model
	// refused one of its additions for that reason (refusedLocked).
	ended   func() bool
	refused bool
}

type noForwardKey struct{}

// withoutForward marks a call whose forwarding the caller does itself
// (changeSender.put), together with the record it makes.
func withoutForward(ctx context.Context) context.Context {
	return context.WithValue(ctx, noForwardKey{}, true)
}

type cleanupKey struct{}

// asCleanup marks the sends of a cleanup: a release's (releaseOwners) and
// the reset of an unknown agent (resetAgent). A forwarded cleanup send
// that fails keeps the cleanup pending (cleanupFailedLocked).
func asCleanup(ctx context.Context) context.Context {
	return context.WithValue(ctx, cleanupKey{}, true)
}

// needsReply lists calls whose reply the change uses; for them the loss of
// the target is a failure.
var needsReply = map[string]bool{"Debugger.setBreakpointByUrl": true, "Debugger.setBreakpoint": true}

// Send sends the call to the change's target. When a commit replaced that
// target (ErrTargetSwapped), the call counts as done: the old target is gone
// with its state, and the committed target gets the change from its queue
// or from its setup snapshot (R5-3). A call that needs the reply goes to
// the committed target instead when it never reached the old one; when it
// may have reached it, it fails, because its reply is lost.
//
// A cleanup send (asCleanup) that a commit cut off this way did not clean
// the target that is active now, which may hold the released state; its
// forwarded send to that target is the cleanup (enqueueLocked). So is the
// forwarded send of a cleanup send that the old target answered when the
// commit came before the enqueue below.
//
// A cleanup send or a disable that the change's target refuses because
// its agent is not enabled (webkit.NotEnabled) is forwarded too, and the
// refusal is returned: the caller counts it as done (sendDisable,
// resetAndApplyDomain, disableDomain), because that target is in the
// state the call wants, but a provisional target's setup may have enabled
// the agent with the state the call removes. The same refusal by a
// provisional target counts as done there too (doneIfNotEnabled).
func (c *changeSender) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cleanup := ""
	if ctx.Value(cleanupKey{}) != nil {
		cleanup = agentDomain(method)
	}
	removes := cleanup != "" || strings.HasSuffix(method, ".disable")
	sctx := ctx
	if c.target != "" {
		sctx = webkit.WithTarget(ctx, c.target)
	}
	res, err := c.Caller.Send(sctx, method, params)
	switch {
	case err == nil:
	case errors.Is(err, webkit.ErrTargetSwapped):
		if needsReply[method] {
			if !errors.Is(err, webkit.ErrNotSent) || !c.retarget(ctx) {
				return res, err
			}
			return c.Send(ctx, method, params)
		}
		res, err = json.RawMessage("{}"), nil
	case notEnabled(err) && removes:
	default:
		return res, err
	}
	if forwarded(method) && ctx.Value(noForwardKey{}) == nil {
		call := forwardCall(method, params)
		if removes {
			call = doneIfNotEnabled(call)
		}
		d := c.d
		d.mu.Lock()
		if method == "Page.setBootstrapScript" {
			// The bootstrap parts change outside d.mu; the call carries the
			// whole script, so a repeat is harmless.
			d.ver++
		}
		c.enqueueLocked(d.ver, cleanup, call)
		d.mu.Unlock()
	}
	return res, err
}

// retarget makes the change address the target that replaced its target,
// after the changes queued for that target. It reports false when the
// commit has not reached the desired state yet or ctx ended before that
// target's queue was sent (the queued changes' failures are their own).
func (c *changeSender) retarget(ctx context.Context) bool {
	d := c.d
	d.mu.Lock()
	next := d.active
	d.mu.Unlock()
	if next == c.target || d.drain(ctx, next) != nil {
		return false
	}
	c.target = next
	return true
}

// changeTarget is the target the change addresses.
func (c *changeSender) changeTarget() string { return c.target }

// put records e and queues f (its send to another target, nil for none) in
// one critical section, so that a setup snapshot has either both or neither.
func (c *changeSender) put(e Entry, f func(context.Context, Sender) error) {
	d := c.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refusedLocked() {
		return
	}
	d.ver++
	d.entries[e.Key] = e
	if f != nil {
		c.enqueueLocked(d.ver, "", f)
	}
}

func forwardCall(method string, params any) func(context.Context, Sender) error {
	return func(ctx context.Context, s Sender) error {
		_, err := s.Send(ctx, method, params)
		return err
	}
}

// doneIfNotEnabled makes f, the forwarded send of a cleanup send or a
// disable, succeed when its target refuses it because the agent is not
// enabled: that agent is in the state the call wants (changeSender.Send).
func doneIfNotEnabled(f func(context.Context, Sender) error) func(context.Context, Sender) error {
	return func(ctx context.Context, s Sender) error {
		if err := f(ctx, s); err != nil && !notEnabled(err) {
			return err
		}
		return nil
	}
}

// putState records e for a change made through c and queues f for the
// provisional targets; without a changeSender it only records e.
func putState(d *DesiredState, c Sender, e Entry, f func(context.Context, Sender) error) {
	if cs, ok := c.(*changeSender); ok {
		cs.put(e, f)
		return
	}
	d.Put(e)
}

// replayContexts runs replay, a session's replay of the execution contexts
// of a document restored from the back/forward cache
// (webkit.SessionOptions.ContextReplay), which sends Runtime.disable and
// Runtime.enable to that document's target. It runs as a desired-state
// change (beginChange), so no owner's Runtime.enable or Runtime.disable
// runs between its check and its calls, and it calls replay only while an
// owner needs Runtime (Needs): the replay never enables an unowned agent,
// and an owned agent is enabled again in the same change. replay changes
// nothing in the model, so it queues nothing for provisional targets.
func (d *DesiredState) replayContexts(ctx context.Context, replay func(context.Context) error) error {
	_, end, err := d.beginChange(ctx, nil) // replay sends through the session itself
	if err != nil {
		return err
	}
	if !d.Needs("Runtime") {
		return end(nil)
	}
	return end(replay(ctx))
}

// beginChange starts a desired-state change made through c and returns its
// sender and the function that ends it. end lets the next change begin,
// waits until every item the change queued was sent (sending the queues
// that nobody sends) and returns err joined with those items' failures.
func (d *DesiredState) beginChange(ctx context.Context, c webkit.Caller) (*changeSender, func(err error) error, error) {
	select {
	case d.chg <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("waiting for another page-state change: %w", ctx.Err())
	}
	d.mu.Lock()
	target := d.active
	d.mu.Unlock()
	// Earlier changes queued for the active target (a provisional target
	// that committed) go first.
	if err := d.drain(ctx, target); err != nil {
		<-d.chg
		return nil, nil, err
	}
	d.mu.Lock()
	for t, ts := range d.syncs {
		if ts.committed && t == target && !ts.busy && len(ts.queue) == 0 {
			delete(d.syncs, t)
		}
	}
	cs := &changeSender{Caller: c, d: d, target: target}
	if oe, ok := c.(interface{ opEnded() bool }); ok {
		cs.ended = oe.opEnded
	}
	d.cur = cs
	d.mu.Unlock()
	end := func(err error) error {
		d.mu.Lock()
		d.cur = nil
		refused := cs.refused
		d.mu.Unlock()
		if refused {
			err = errors.Join(err, errChangeRefused)
		}
		<-d.chg
		// The wait outlives a cancel of ctx (an operation that a release
		// ended) but not its deadline (design 2.6): a provisional target
		// that does not answer must not hold the command past it. Items
		// still queued then are sent later by whoever holds the target's
		// sending slot (its setup, a later change, drain at the commit).
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forwardBudget)
		defer cancel()
		if dl, ok := ctx.Deadline(); ok {
			var cancelDL context.CancelFunc
			fctx, cancelDL = context.WithDeadline(fctx, dl)
			defer cancelDL()
		}
		d.mu.Lock()
		items := cs.pending
		d.mu.Unlock()
		var errs []error
		for _, it := range items {
			if ferr := d.await(fctx, it); ferr != nil {
				errs = append(errs, ferr)
			}
		}
		if len(errs) == 0 {
			return err
		}
		return errors.Join(append([]error{err}, errs...)...)
	}
	return cs, end, nil
}
