package core

import (
	"context"
	"slices"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Change runs fn as one change of the page's desired state (design 2.4 and
// 2.7). It is the change path that core's own changes use (legacy calls,
// legacyCall), open to code outside core, so that state set there reaches
// provisional targets too:
//
//   - fn gets a caller in place of c. It sends to the page target that was
//     active when the change began, and repeats each successful send on
//     every live provisional target whose setup snapshot predates the
//     latest model change, so that a later commit keeps the change. A
//     target gets its repeated sends in order (FIFO), after its snapshot.
//     The calls that a new target must not get are not repeated: legacy
//     state rules without replay or with node or script ids, and
//     JavaScript breakpoints (notForwarded in legacy_state.go).
//   - Changes of the page run one at a time. A provisional target's setup
//     never waits for a change.
//   - Change returns fn's error joined with the failures of this change's
//     own repeated sends, after those sends ended (at most forwardBudget
//     after fn returned). A target that is gone (destroyed, or replaced by
//     a commit) is no failure: its state went with it.
//
// fn must record each model change (Put, Remove, SetPart, Acquire,
// SetHeaders, AddInterception and the like) before the sends that carry
// it to the page: a snapshot taken after the record already holds the
// state, so the caller repeats a send only for targets whose snapshot is
// older than the latest record. A send made before its record misses the
// targets whose setup runs between the two.
//
// A Change on a caller that a Change of the same page gave runs fn inside
// that change. So code that runs inside a change (an Entry's Release
// during a lease release, or one W4 command that calls another) can use
// Change too.
func (d *DesiredState) Change(ctx context.Context, c webkit.Caller, fn func(c webkit.Caller) error) (err error) {
	if cs, ok := c.(*changeSender); ok && cs.d == d {
		return fn(cs)
	}
	cs, end, err := d.beginChange(ctx, c)
	if err != nil {
		return err
	}
	defer func() { err = end(err) }()
	return fn(cs)
}

// InterceptRules returns a copy of every owner's interception rules. The
// interceptor of internal/devtools uses it to decide whether an owner
// outside devtools (a legacy command, a CDP client) answers an intercepted
// request.
func (d *DesiredState) InterceptRules() map[Owner][]InterceptRule {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[Owner][]InterceptRule, len(d.rules))
	for o, rs := range d.rules {
		out[o] = slices.Clone(rs)
	}
	return out
}

// HasEntry reports whether the model holds the entry key. A release
// removes a session's entries without the change lock and without their
// Release when its cleanup cannot run (forgetSession), and a change whose
// operation ended records no entry (errChangeRefused); internal/devtools
// reads it to find in-memory state whose entry is gone.
func (d *DesiredState) HasEntry(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.entries[key]
	return ok
}

// Unforwarded marks ctx so that a send of a change through it goes to the
// change's target only and is not repeated on provisional targets. It is
// for a send that concerns one target alone, for example the removal of a
// rule that only that target may still hold; the other targets would
// answer it with an error.
func Unforwarded(ctx context.Context) context.Context { return withoutForward(ctx) }

// ContinueUnclaimed continues the request requestID that target holds
// unmodified at stage ("request" or "response") through the control lane.
// target is the source target of the interception event
// (webkit.TargetEventHandler; "" names the active target), so a request
// that a provisional target paused is continued on that target. It is the
// daemon's own continue step, as the barriers' are (design 2.10, control
// lane item 4): it needs no lease, so it also answers a request paused
// after a release. Like every completion it completes the hold at most
// once; a hold that is gone (also with its destroyed target) or already
// being completed is refused with ErrControlRefused and nothing is sent.
func (s *Scheduler) ContinueUnclaimed(ctx context.Context, target, requestID, stage string) error {
	_, err := s.control(ctx, ControlMsg{
		Kind: ControlCompleteRequest, RequestID: requestID, TargetID: target,
		Method: "Network.interceptContinue", Params: map[string]any{"stage": stage},
	})
	return err
}
