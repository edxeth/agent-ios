package core

import (
	"context"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// ReleaseOwnerChange releases owner as one change of the page's desired
// state through c (DesiredState.Change), with the pending-cleanup
// transaction of a lease release (release_lifecycle.go, releaseOwners):
//
//   - Its sends are cleanup sends (asCleanup): a forwarded one that fails
//     stays pending for its target (cleanupFailedLocked).
//   - A header name that lost its last owner while Network stays owned runs
//     the Network reinitialization barrier (ReleaseResult.NeedsNetworkReset).
//   - Each domain whose cleanup send failed (ReleaseResult.Failed, and
//     Network for a failed Network reset) becomes unknown with a new mark,
//     inside the change. State.Release removes owner and its entries before
//     it sends, so the mark is what keeps that cleanup: the next change
//     that uses the domain resets it first (ResetUnknownAgent, legacyCall),
//     and so do a new connection's setup and a lease release's background
//     reset. Only a reset that succeeds removes the mark.
//
// It is the release of an owner that lives inside a lease, for example a
// W3b command's frame or hover owner. ran reports whether the change ran.
// When it did not (the change lock was not free before ctx ended), owner
// is still in the model with its state, and the caller decides: try again
// later, or remove it without the lock (forgetOwners).
func (ps *PageSession) ReleaseOwnerChange(ctx context.Context, c webkit.Caller, owner Owner) (ran bool, err error) {
	ctx = asCleanup(ctx) // a forwarded send that fails stays pending too
	err = ps.State.Change(ctx, c, func(c webkit.Caller) error {
		ran = true
		res, err := ps.State.Release(ctx, owner, c)
		failed := res.Failed
		if err == nil && res.NeedsNetworkReset {
			if err = NetworkReset(ctx, ps.Sched, c, ps.State); err != nil {
				failed = append(failed, "Network")
			}
		}
		ps.State.markFailedCleanup(failed)
		return err
	})
	return ran, err
}

// ResetPendingRuntime services a Runtime cleanup that a release left
// pending (a Runtime.disable that failed after the last owner left) before
// an acquisition of Runtime, inside its change: while no owner holds
// Runtime, it resets the agent (ResetUnknownAgent, a Runtime.disable).
// While an owner holds Runtime, it sends nothing: that owner's
// Runtime.enable came after the failed disable, Runtime carries no other
// state, and design 2.4 sends Runtime.disable only when the last owner
// leaves. The mark then stays for the next reset.
func ResetPendingRuntime(ctx context.Context, ps *PageSession, c Sender) error {
	if ps.State.Needs("Runtime") {
		return nil
	}
	return ResetUnknownAgent(ctx, ps, c, "Runtime")
}

// NoEffect reports a failed call that provably changed nothing on the
// page (noEffect): it was not sent (webkit.ErrNotSent), or WebKit
// answered it with a protocol error. Any other failure, such as a reply
// lost after the write, leaves the outcome unknown, so a cleanup recorded
// for the call must stay.
func NoEffect(err error) bool { return noEffect(err) }

// markFailedCleanup makes each domain of failed unknown with a new mark
// (release_lifecycle.go, "a cleanup send that fails marks its domain").
func (d *DesiredState) markFailedCleanup(failed []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dom := range failed {
		d.markUnknownLocked(dom)
	}
}
