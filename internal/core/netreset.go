package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// ErrRequestReset: a completion arrived for a held request that the
// Network reinitialization barrier already continued unmodified.
var ErrRequestReset = errors.New("request was released by a network reset")

// ResetHeldRequests continues every open held request unmodified, at the
// stage of its hold, through the control lane (step 2 of the Network
// reinitialization barrier, design 2.4) and records each id, so that a later
// completion for it fails with ErrRequestReset. It returns the continued ids.
// A hold that it could not close (answerHold failed) stays open for the
// release barrier and is not recorded.
func (s *Scheduler) ResetHeldRequests(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.intercepted))
	for id := range s.intercepted {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var firstErr error
	var done []string
	for _, id := range ids {
		if err := s.answerHold(ctx, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.mu.Lock()
		if s.resetReqs == nil {
			s.resetReqs = map[string]bool{}
		}
		s.resetReqs[id] = true
		s.mu.Unlock()
		done = append(done, id)
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
// It returns the forgotten ids.
func (s *Scheduler) forgetHeldRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.intercepted {
		if s.resetReqs == nil {
			s.resetReqs = map[string]bool{}
		}
		s.resetReqs[id] = true
		delete(s.intercepted, id)
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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
