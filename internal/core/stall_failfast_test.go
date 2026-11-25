package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Core-events round 5 (the orchestrator's decision): once the liveness
// probe declares a connection stalled, every call still waiting for a reply
// on it fails at once with the stall classification (exit 4,
// ErrSessionStalled), not at its own deadline. Here an operation send and a
// control-lane send wait on a connection that stopped answering; a probe
// with a short budget declares the stall.
func TestStallDeclaredEndsWaitingSendsAtOnce(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "waiting send", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	pg := mo.Page
	r.f.StallConnection(r.f.ConnectionCount())
	const deadline = 8 * time.Second
	type result struct {
		err  error
		took time.Duration
	}
	opDone, ctlDone := make(chan result, 1), make(chan result, 1)
	start := time.Now()
	go func() {
		c, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		_, err := r.m.Raw(c, mo.ID, "DOM.getDocument", json.RawMessage("{}"))
		opDone <- result{err, time.Since(start)}
	}()
	go func() {
		c, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		_, err := pg.conn.SendControl(c, "Network.interceptContinue", map[string]any{"requestId": "x", "stage": "request"})
		ctlDone <- result{err, time.Since(start)}
	}()
	eventually(t, 3*time.Second, "both sends written", func() bool {
		return len(r.f.ReceivedMethod("DOM.getDocument")) > 0 && len(r.f.ReceivedMethod("Network.interceptContinue")) > 0
	})
	pctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := pg.conn.probeNow(pctx); err == nil {
		t.Fatal("fixture: the probe of a stalled connection succeeded")
	}
	if st := pg.ConnState(); st != ConnStalled {
		t.Fatalf("fixture: state after the failed probe = %s, want stalled", st)
	}
	for name, ch := range map[string]chan result{"operation send": opDone, "control-lane send": ctlDone} {
		res := <-ch
		if res.took > 3*time.Second {
			t.Errorf("%s returned after %v, want right after the probe declared the stall (deadline %v)", name, res.took.Round(time.Millisecond), deadline)
		}
		if !errors.Is(res.err, webkit.ErrSessionStalled) || exitCode(res.err) != CodeBusy {
			t.Errorf("%s = %v (exit %d), want ErrSessionStalled with exit %d", name, res.err, exitCode(res.err), CodeBusy)
		}
	}
}
