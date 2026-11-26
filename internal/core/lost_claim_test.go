package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// iwdp ends the connection of a page that another inspector claimed with
// close code 1001 and this reason (iPhone X, iOS 16.7; parity-045 check
// B3x).
const claimedReason = "Page 9223/1 claimed by remote"

// claimOnEval makes the fake close every connection with iwdp's claim
// close when it receives a Runtime.evaluate of expr, after delay: the
// claim as the first command after it finds it (delay 0), or during the
// command, while its reply is outstanding.
func claimOnEval(f *fakewebkit.Server, expr string, delay time.Duration) {
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if p.Expression == expr {
			time.Sleep(delay)
			f.CloseConnectionsWith(1001, claimedReason)
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
}

// docs/sessions.md, "Lost pages and takeover": the first command after
// another inspector claimed the page fails with the lost-page error (exit
// 4, "ownership is unknown", --takeover), also when the session used the
// page less than the idle-probe time (20 s) before, so that no liveness
// probe runs first; the page is lost when the command returns, and
// --takeover reattaches it.
func TestFirstCommandAfterAClaimReportsTheLostPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"claim found by the command", 0},
		{"claim during the command", 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
				t.Fatal(err)
			}
			pg := r.m.FindAttached(r.f.URL)
			claimOnEval(r.f, "2", tc.delay)
			err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
			if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || !strings.Contains(err.Error(), "ownership is unknown") || !strings.Contains(err.Error(), "--takeover") {
				t.Fatalf("first command after the claim = %v (exit %d), want the lost-page error with the --takeover guidance (exit %d)", err, exitCode(err), CodeBusy)
			}
			// The command was written: the page may have run it.
			if errors.Is(err, webkit.ErrNotSent) || !strings.Contains(err.Error(), "may have run") {
				t.Errorf("error = %v, want it to say that the page may have run the written command", err)
			}
			if !strings.Contains(err.Error(), claimedReason) {
				t.Errorf("error = %v, want the close reason %q", err, claimedReason)
			}
			if st := pg.ConnState(); st != ConnLost {
				t.Errorf("state after the command = %v, want lost", st)
			}
			r.f.Handle("Runtime.evaluate", nil)
			ao := agent
			ao.Takeover = true
			if err := evalOnce(ctxFor(t, 5*time.Second), r, ao); err != nil {
				t.Fatalf("--takeover after the claim: %v", err)
			}
		})
	}
}

// claimedCaller is a connection that iwdp's claim close has ended before a
// call was written: the call is refused before the write (webkit.ErrNotSent).
type claimedCaller struct{ endedCaller }

func (c claimedCaller) Send(_ context.Context, method string, _ any) (json.RawMessage, error) {
	return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, c.err)
}

// A call that the claim close refused before the write fails with the
// lost-page error and says that it was not sent, also before watch has
// seen the end of the connection (here it never runs).
func TestCallRefusedByAClaimedConnectionIsLostAndNotSent(t *testing.T) {
	done := make(chan struct{})
	close(done)
	closed := fmt.Errorf("%w: websocket: close 1001 (going away): %s", webkit.ErrSessionClosed, claimedReason)
	c := claimedCaller{endedCaller{done: done, err: closed}}
	p := newPageConn(c, func() { t.Error("recovery started for a claimed page") }, func(func(context.Context)) bool { return false }, func(string, ...any) {})
	_, err := p.Send(ctxFor(t, 2*time.Second), "Runtime.evaluate", nil)
	if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || !errors.Is(err, webkit.ErrNotSent) || !strings.Contains(err.Error(), "not sent") || strings.Contains(err.Error(), "may have run") {
		t.Fatalf("call = %v (exit %d), want the lost-page error that says it was not sent (exit %d)", err, exitCode(err), CodeBusy)
	}
	if st := p.State(); st != ConnLost {
		t.Errorf("state = %v, want lost", st)
	}
}
