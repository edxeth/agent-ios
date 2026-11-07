package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
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
}

// queued is one forwarded send of a change, queued for one target. Its
// fields after send are guarded by DesiredState.mu.
type queued struct {
	send      func(context.Context, Sender) error
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
// empties it: the target is gone or gets the whole state again. The caller
// holds d.mu.
func dropQueueLocked(ts *targetSync) {
	for _, q := range ts.queue {
		q.completeLocked(nil)
	}
	ts.queue = nil
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
		dropQueueLocked(old)
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
// the change's. The caller holds d.mu.
func (c *changeSender) enqueueLocked(v uint64, f func(context.Context, Sender) error) {
	targets := make([]string, 0, len(c.d.syncs))
	for t := range c.d.syncs {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		ts := c.d.syncs[t]
		if t != c.target && !ts.failed && ts.snapV < v {
			q := &queued{send: f, done: make(chan struct{})}
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
		d.mu.Unlock()
		err := q.send(ctx, ts.via)
		d.mu.Lock()
		switch {
		case err == nil:
		case targetGone(err):
			err = nil
			dropQueueLocked(ts)
		default:
			err = fmt.Errorf("page target %s: %w", target, err)
			errs = append(errs, err)
		}
		q.completeLocked(err)
	}
	if d.syncs[target] != ts {
		dropQueueLocked(ts) // the record was dropped while an item was sent
	}
	ts.busy = false
	close(ts.idle)
	d.mu.Unlock()
	return errors.Join(errs...)
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
	dropQueueLocked(ts)
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
}

type noForwardKey struct{}

// withoutForward marks a call whose forwarding the caller does itself
// (changeSender.put), together with the record it makes.
func withoutForward(ctx context.Context) context.Context {
	return context.WithValue(ctx, noForwardKey{}, true)
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
func (c *changeSender) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	sctx := ctx
	if c.target != "" {
		sctx = webkit.WithTarget(ctx, c.target)
	}
	res, err := c.Caller.Send(sctx, method, params)
	if err != nil {
		if !errors.Is(err, webkit.ErrTargetSwapped) {
			return res, err
		}
		if needsReply[method] {
			if !errors.Is(err, webkit.ErrNotSent) || !c.retarget(ctx) {
				return res, err
			}
			return c.Send(ctx, method, params)
		}
		res = json.RawMessage("{}")
	}
	if forwarded(method) && ctx.Value(noForwardKey{}) == nil {
		call := forwardCall(method, params)
		d := c.d
		d.mu.Lock()
		if method == "Page.setBootstrapScript" {
			// The bootstrap parts change outside d.mu; the call carries the
			// whole script, so a repeat is harmless.
			d.ver++
		}
		c.enqueueLocked(d.ver, call)
		d.mu.Unlock()
	}
	return res, nil
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
	d.ver++
	d.entries[e.Key] = e
	if f != nil {
		c.enqueueLocked(d.ver, f)
	}
}

func forwardCall(method string, params any) func(context.Context, Sender) error {
	return func(ctx context.Context, s Sender) error {
		_, err := s.Send(ctx, method, params)
		return err
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
	d.mu.Unlock()
	cs := &changeSender{Caller: c, d: d, target: target}
	end := func(err error) error {
		<-d.chg
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forwardBudget)
		defer cancel()
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
