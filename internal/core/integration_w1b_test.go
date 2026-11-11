package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Regression tests for interactions between the W1b repairs
// (docs/notes/w1b.md, "Review repairs, integration").

// interceptBackend models the pending intercepts of WebKit's
// InspectorNetworkAgent (webkitgtk-2.54.2, InspectorNetworkAgent.cpp):
// a completion takes its hold from the pending requests or responses and
// fails with "Missing pending intercept ..." when there is none
// (interceptContinue, :1180-1198), and Network.disable continues every
// pending intercept (disable, :848-867; continuePendingRequests and
// continuePendingResponses, :898-910). A frontend disconnect runs the same
// disable (willDestroyFrontendAndBackend, :139-142).
type interceptBackend struct {
	mu      sync.Mutex
	pending map[string]bool
}

func newInterceptBackend(f *fakewebkit.Server) *interceptBackend {
	b := &interceptBackend{pending: map[string]bool{}}
	f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.pending = map[string]bool{}
		return nil, nil
	})
	f.Handle("Network.interceptContinue", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.pending[p.RequestID] {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Missing pending intercept request for given requestId"}
		}
		delete(b.pending, p.RequestID)
		return nil, nil
	})
	return b
}

// hold makes the backend hold id at the request stage and tells the page.
func (b *interceptBackend) hold(f *fakewebkit.Server, id string) {
	b.mu.Lock()
	b.pending[id] = true
	b.mu.Unlock()
	f.Emit("Network.requestIntercepted", map[string]string{"requestId": id})
}

func (b *interceptBackend) held() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// A new connection's backend holds none of the requests that the scheduler
// recorded on the connection it replaces: Apply resets the Network agent
// of every target the connection does not know (resetCalls), and WebKit's
// Network.disable continues every pending intercept. The scheduler forgets
// those holds as a Network reset does (ErrRequestReset), so a client's
// completion (r1) gets ErrRequestReset instead of WebKit's error, and the
// release barrier does not send a continue (r2) that WebKit refuses
// ("Missing pending intercept ...") and that fails the release. A request
// held on the new connection (r3) is still continued by the barrier.
func TestNewConnectionForgetsHoldsTheNetworkResetReleased(t *testing.T) {
	for _, path := range []string{"recovery", "takeover"} {
		t.Run(path, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{Steal: path == "takeover"}, false, ManagerOptions{})
			r.reserve(t, "agent")
			b := newInterceptBackend(r.f)
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "intercept", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			r.m.End(mo.ID)
			pg := mo.Page
			b.hold(r.f, "r1")
			b.hold(r.f, "r2")
			eventually(t, 5*time.Second, "the held requests", func() bool { return len(pg.PS.Sched.PendingRequests()) == 2 })

			switch path {
			case "recovery":
				mo, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				r.f.StallConnection(1)
				_, _ = r.m.Raw(ctxFor(t, time.Second), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
				r.m.End(mo.ID)
				eventually(t, 15*time.Second, "background recovery", func() bool { return pg.ConnState() == ConnOK && r.f.ConnectionCount() == 2 })
			case "takeover":
				stealPage(t, r.f)
				eventually(t, 3*time.Second, "lease lost", func() bool { return pg.ConnState() == ConnLost })
				take := agent
				take.Takeover = true
				mo, err := r.m.Begin(ctx, r.req, take, "eval", ClassExclusive)
				if err != nil {
					t.Fatalf("--takeover: %v", err)
				}
				r.m.End(mo.ID)
			}
			if n := b.held(); n != 0 {
				t.Fatalf("the backend still holds %d requests after the new connection's setup; want its Network reset to continue them", n)
			}
			if p := pg.PS.Sched.PendingRequests(); len(p) != 0 {
				t.Errorf("pending after the new connection = %v, want none: the backend continued them", p)
			}
			session, epoch := pg.PS.Sched.Lease()
			_, err = pg.PS.Sched.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: session, Epoch: epoch, RequestID: "r1", Method: "Network.interceptContinue"})
			if !errors.Is(err, ErrRequestReset) {
				t.Errorf("completion of r1 after the new connection: %v, want ErrRequestReset", err)
			}
			b.hold(r.f, "r3")
			eventually(t, 5*time.Second, "the request held on the new connection", func() bool { return slices.Contains(pg.PS.Sched.PendingRequests(), "r3") })
			if err := r.m.Close(ctxFor(t, 5*time.Second)); err != nil {
				t.Errorf("Close: %v, want the release without a refused continue", err)
			}
			var ids []string
			for _, p := range continues(t, r.f) {
				ids = append(ids, fmt.Sprint(p["requestId"]))
			}
			if len(ids) != 1 || ids[0] != "r3" || b.held() != 0 {
				t.Errorf("continued %v (backend still holds %d), want only r3, the request held on the new connection", ids, b.held())
			}
		})
	}
}

// The first <Domain>.enable of an agent that the session does not know
// goes out after that agent's reset, in one pipelined batch
// (webkit.Session.sendWithReset). Hang detection and the owner's
// cancellation (design 2.6 step 2, pageConn.send) read the call's error:
// a reset without a reply within the call budget is ErrCallTimeout, so the
// page is probed and marked stalled when the probe fails too; a live probe
// leaves the page attached; a reset that the operation's cancellation ends
// is that cancellation, not a stall. Here Console is unknown: the first
// Console.enable goes out after Console.disable.
func TestFirstEnableBatchKeepsHangDetection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deadProbe bool // the page answers no probe either
		cancel    bool // the operation's owner cancels while the batch waits
	}{
		{name: "dead probe", deadProbe: true},
		{name: "live probe"},
		{name: "owner cancels", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The session keeps its default call budget, so the attach and its
			// replay of the desired state are bounded only by the deadline.
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			r.m.recoveryHold = func(ctx context.Context) { <-ctx.Done() } // keep a stalled page stalled
			ctx := ctxFor(t, 5*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "console", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer r.m.End(mo.ID)
			r.f.Stall("Console.disable")
			r.f.Stall("Console.enable")
			if tc.deadProbe {
				r.f.Stall("Runtime.evaluate")
			}
			if tc.cancel {
				go func() {
					for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && len(r.f.ReceivedMethod("Console.enable")) == 0; {
						time.Sleep(10 * time.Millisecond)
					}
					mo.Op.Cancel(errors.New("the client went away"))
				}()
			}
			sendCtx := ctx
			if !tc.cancel {
				// Only the batch whose budget must expire gets the 300 ms
				// budget; its probe inherits it (Session.Probe). The owner's
				// cancellation must end the batch, so that case keeps the
				// default budget and has no race against it.
				sendCtx = webkit.WithCallBudget(ctx, 300*time.Millisecond)
			}
			_, err = mo.Op.Caller().Send(sendCtx, "Console.enable", nil)
			if got := len(r.f.ReceivedMethod("Console.disable")); got != 1 {
				t.Fatalf("Console.disable sent %d times, want 1: the reset of the unknown agent", got)
			}
			st := mo.Page.ConnState()
			switch {
			case tc.deadProbe:
				if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy || st != ConnStalled {
					t.Errorf("first enable on a dead page: %v (exit %d), state %s; want ErrSessionStalled (exit 4) and stalled", err, exitCode(err), st)
				}
			case tc.cancel:
				if !errors.Is(err, context.Canceled) || errors.Is(err, webkit.ErrSessionStalled) || st != ConnOK {
					t.Errorf("first enable canceled by its owner: %v, state %s; want the cancellation and attached", err, st)
				}
			default:
				if !errors.Is(err, webkit.ErrCallTimeout) || errors.Is(err, webkit.ErrSessionStalled) || st != ConnOK {
					t.Errorf("first enable without a reply on a live page: %v, state %s; want ErrCallTimeout and attached", err, st)
				}
			}
			var pe *webkit.ErrorData
			if errors.As(err, &pe) || errors.Is(err, webkit.ErrNotSent) {
				t.Errorf("first enable: %v, want neither a protocol error nor ErrNotSent: the reset reached the page", err)
			}
		})
	}
}
