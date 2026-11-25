package core

import (
	"context"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// W3b final repair round 3 (orchestrator ruling): an owner-change cleanup
// forgets a frame record only after an attempt whose send reached the
// current target and failed or got no answer. A temporary Runtime.enable
// for a frame without a context on the new connection, which the page
// receives and never answers, is such an attempt: the frame is forgotten
// (ForgetFailedExecutionContexts), so it does not cost every later
// command its budget. The same cleanup with a budget that ended before
// the enable sends nothing and keeps the frame.
func TestOwnerChangeFrameForgetFollowsTheTemporaryEnable(t *testing.T) {
	release := func(ctx context.Context, c webkit.Caller) error {
		_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
		return err
	}
	for _, ended := range []bool{false, true} {
		name := map[bool]string{false: "unanswered enable", true: "budget ended first"}[ended]
		t.Run(name, func(t *testing.T) {
			fr := newFrameReleaseRig(t, 17, "")
			fr.f.Stall("Runtime.enable")
			mo, err := fr.m.Begin(ctxFor(t, 5*time.Second), fr.req, agent, "eval", ClassMutate)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer fr.m.End(mo.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			if ended {
				cancel()
			}
			err = fr.ps.ReleaseFrameContexts(ctx, fr.epoch, agent.Session, mo.Op.Caller(), release)
			cancel()
			if err == nil {
				t.Fatal("the cleanup returned nil without a frame context")
			}
			fr.ps.Sched.ForgetFailedExecutionContexts(fr.epoch)
			enables, left := fr.onSecond("Runtime.enable"), fr.ps.Sched.ContextEpochsBefore(fr.epoch+1)
			if ended {
				if enables != 0 || len(left) != 1 {
					t.Fatalf("Runtime.enable on the second connection %d, epochs left %v; want 0 and the frame kept", enables, left)
				}
				return
			}
			if enables != 1 || len(left) != 0 {
				t.Fatalf("Runtime.enable on the second connection %d, epochs left %v; want 1 and the frame forgotten", enables, left)
			}
		})
	}
}
