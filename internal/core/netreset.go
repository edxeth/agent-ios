package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// ErrRequestReset: a completion arrived for a held request that the
// Network reinitialization barrier already continued unmodified.
var ErrRequestReset = errors.New("request was released by a network reset")

// ResetHeldRequests continues every open held request unmodified, at the
// stage of its hold and on the target that holds it, through the control
// lane (step 2 of the Network reinitialization barrier, design 2.4) and
// records each one per target, so that a later completion for it fails
// with ErrRequestReset. It returns the continued request ids. A hold that
// it could not close (answerHold failed) stays open for the release
// barrier and is not recorded.
func (s *Scheduler) ResetHeldRequests(ctx context.Context) ([]string, error) {
	return s.resetHolds(ctx, func(holdKey) bool { return true })
}

// resetTargetHolds is ResetHeldRequests for the holds of target only
// (setupHolds).
func (s *Scheduler) resetTargetHolds(ctx context.Context, target string) ([]string, error) {
	return s.resetHolds(ctx, func(k holdKey) bool { return k.target == target })
}

// resetHolds is ResetHeldRequests for the holds that match.
func (s *Scheduler) resetHolds(ctx context.Context, match func(holdKey) bool) ([]string, error) {
	s.mu.Lock()
	keys := make([]holdKey, 0, len(s.intercepted))
	for k := range s.intercepted {
		if match(k) {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	var firstErr error
	var done []string
	for _, k := range keys {
		// answerHold records k as reset in the critical section that
		// closes the hold, so no completion sees it closed but not reset.
		if err := s.answerHold(ctx, k, true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		done = append(done, k.id)
	}
	return done, firstErr
}

// forgetHeldRequests records every held request as continued by a Network
// reset, without sending anything: a later completion for one fails with
// ErrRequestReset. A new connection (recovery, reopen, takeover) calls it
// after its setup, because its backend holds none of them. Apply reset the
// Network agent (resetCalls: an agent the connection does not know), and
// the old connection's close disconnected its frontend; WebKit's
// Network.disable, which a frontend disconnect also runs, continues every
// pending intercept (InspectorNetworkAgent::disable and
// continuePendingRequests/continuePendingResponses, webkitgtk-2.54.2
// InspectorNetworkAgent.cpp:848-867, 898-910). A continue for one of them
// would get "Missing pending intercept ..." and fail the release barrier.
// Every target's holds are forgotten, each recorded for its own target. It
// returns the forgotten request ids.
func (s *Scheduler) forgetHeldRequests() []string {
	return s.forgetHolds(func(holdKey) bool { return true })
}

// forgetTargetHolds is forgetHeldRequests for the holds of target only
// (setupHolds): a setup's Network.disable on target continued them.
func (s *Scheduler) forgetTargetHolds(target string) []string {
	return s.forgetHolds(func(k holdKey) bool { return k.target == target })
}

// disabledOn records the holds of target as reset after c's target replied
// to Network.disable (DesiredState.networkDisabled), when c is the
// connection the scheduler sends through (pageConn); a page connection
// that cannot tell counts as that connection.
func (s *Scheduler) disabledOn(c webkit.Caller, target string) {
	if pc, ok := s.c.(interface{ installed(webkit.Caller) bool }); ok && !pc.installed(c) {
		return
	}
	s.forgetTargetHolds(target)
}

// forgetHolds is forgetHeldRequests for the holds that match. A hold whose
// completion is in flight is recorded too: that completion gets WebKit's
// "Missing pending intercept ..." (requestGone), or completed it first.
func (s *Scheduler) forgetHolds(match func(holdKey) bool) []string {
	s.mu.Lock()
	var ids []string
	var forgot []holdKey
	for k := range s.intercepted {
		if !match(k) {
			continue
		}
		if s.resetReqs == nil {
			s.resetReqs = map[holdKey]bool{}
		}
		s.resetReqs[k] = true
		delete(s.intercepted, k)
		ids = append(ids, k.id)
		forgot = append(forgot, k)
	}
	s.mu.Unlock()
	s.reportForgotten(forgot)
	slices.Sort(ids)
	return ids
}

// holdAdmission closes admission until reopen, without waiting for the
// running operations (setupHolds; Barrier waits for them).
func (s *Scheduler) holdAdmission() (reopen func()) {
	s.mu.Lock()
	s.barriers++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.barriers--
			s.pumpLocked()
			s.mu.Unlock()
		})
	}
}

// newConnectionReset is the Network reset that a new connection of the page
// (recovery, reopen, takeover) implies: the old connection's close and the
// new connection's Apply both disable the backend's Network agent, which
// clears its state and continues every held request. Like NetworkReset it
// reports the gap to every remaining Network owner (design 2.4), so a
// capture marks the discontinuity and closes the requests in flight across
// it as capture_reset; the held requests are forgotten (forgetHeldRequests).
// start is when the daemon retired the old connection; a stalled connection
// may have delivered no events for some time before. It returns the gap's
// End.
func newConnectionReset(sched *Scheduler, state *DesiredState, start time.Time) time.Time {
	ids := sched.forgetHeldRequests()
	end := time.Now()
	state.notifyNetworkReset(NetworkGap{Start: start, End: end, Requests: ids})
	return end
}

// networkResetWritten marks a failed DesiredState.Apply that may have
// written a Network.disable: the backend's Network agent may be reset, and
// its held requests continued, although the setup failed. A new
// connection's setup that fails so still reports the gap
// (networkDisableWritten, Page.reportNetworkGap). The message is that of
// the wrapped error.
type networkResetWritten struct{ error }

func (e networkResetWritten) Unwrap() error { return e.error }

// networkDisableWritten reports whether err comes from a setup that may
// have written a Network.disable (networkResetWritten).
func networkDisableWritten(err error) bool {
	var w networkResetWritten
	return errors.As(err, &w)
}

// networkDisableIn reports whether calls hold a Network.disable that may
// have been written: errs are the calls' errors, and only webkit.ErrNotSent
// proves that a call never reached the page.
func networkDisableIn(calls []webkit.Call, errs []error) bool {
	for i, c := range calls {
		if c.Method == "Network.disable" && !errors.Is(errs[i], webkit.ErrNotSent) {
			return true
		}
	}
	return false
}

// NetworkReset runs the Network reinitialization barrier of design 2.4 on
// s, the sender of the operation or release that already holds the page
// (admission is closed: an exclusive operation, or the release barrier):
// continue held requests, Network.disable, Network.enable, apply the
// complete remaining Network state, then report the gap to every remaining
// Network owner. WIP merges extra-header maps and never removes a name, so
// this is the only way to remove a header while Network stays enabled.
func NetworkReset(ctx context.Context, sched *Scheduler, s Sender, state *DesiredState) error {
	start := time.Now()
	ids, err := sched.ResetHeldRequests(ctx)
	if err != nil {
		return fmt.Errorf("network reset: continuing held requests: %w", err)
	}
	for _, m := range []string{"Network.disable", "Network.enable"} {
		if _, err := s.Send(ctx, m, nil); err != nil {
			return fmt.Errorf("network reset: %s: %w", m, err)
		}
	}
	if err := state.ApplyNetwork(ctx, s); err != nil {
		return err
	}
	state.notifyNetworkReset(NetworkGap{Start: start, End: time.Now(), Requests: ids})
	return nil
}
