package devtools

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 round 5 (brief W4-R5 item 3): a new page connection resets the
// backend's Network agent: the old connection's close and the new
// connection's setup both disable it, which drops WebKit's stored bodies
// and every request it was tracking. Core reports that reset as a
// NetworkGap to the remaining Network owners (docs/notes/w1b.md, "held
// requests and the reconnect gap" and "failed setup and the reconnect
// gap"). The network log treats it as an explicit Network reset (design
// 2.4): it records the gap, a request in flight across it ends as
// capture_reset, the bodies of earlier requests are gone, the idle tracker
// forgets the request, and the capture goes on over the new connection.
// The paths: a takeover after a lost connection, a takeover whose first
// setup fails after its Network.disable (two adjacent gaps; only the first
// closes the request), and the background recovery of a stalled
// connection.
func TestNetlogMarksTheGapOfANewConnection(t *testing.T) {
	for _, path := range []string{"takeover", "failed takeover setup", "recovery"} {
		t.Run(path, func(t *testing.T) {
			r := newRig(t)
			var n *Netlog
			var pg *Page
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				var err error
				pg = p
				n, _, err = p.StartNetlog(ctx, c, "s1")
				return err
			})
			r.f.Emit("Network.requestWillBeSent", reqEvent("done", "https://fixture.example/done", 1))
			finish(r.f, "done", 1.5)
			r.f.Emit("Network.requestWillBeSent", reqEvent("inflight", "https://fixture.example/slow", 2))
			eventually(t, "both requests in the log", func() bool {
				d, _ := n.Get("page-1:done")
				return d.State == StateFinished && pending(n) == 1
			})
			if s := n.Snapshot(Filter{}); len(s.Gaps) != 0 {
				t.Fatalf("gaps before the new connection: %+v", s.Gaps)
			}
			before := time.Now()
			var failedEnd time.Time
			switch path {
			case "takeover", "failed takeover setup":
				r.f.CloseConnections()
				eventually(t, "the lost page", func() bool { return pageConnState(pg) == core.ConnLost })
				r.ao.Takeover = true
				if path == "failed takeover setup" {
					fail := failBootstrapScript(r.f)
					if err := r.do(t, func(context.Context, *Page, webkit.Caller) error { return nil }); err == nil {
						t.Fatal("the takeover succeeded, want its setup failure")
					}
					failedEnd = time.Now()
					if s := n.Snapshot(Filter{}); len(s.Gaps) != 1 {
						t.Fatalf("gaps after the failed setup: %+v, want one", s.Gaps)
					}
					fail.Store(false)
				}
				r.must(t, func(context.Context, *Page, webkit.Caller) error { return nil })
				r.ao.Takeover = false
			case "recovery":
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				mo, err := r.m.Begin(ctx, r.req, r.ao, "eval", core.ClassExclusive)
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				r.f.StallConnection(1)
				callCtx, callCancel := context.WithTimeout(ctx, time.Second)
				_, _ = r.m.RawAs(callCtx, mo, core.SessionOwner("legacy", mo.Session), "Runtime.evaluate", json.RawMessage(`{"expression":"document.title"}`))
				callCancel()
				r.m.End(mo.ID)
				deadline := time.Now().Add(15 * time.Second)
				for pageConnState(pg) != core.ConnOK || r.f.ConnectionCount() != 2 {
					if time.Now().After(deadline) {
						t.Fatalf("no background recovery: state %v, %d connections", pageConnState(pg), r.f.ConnectionCount())
					}
					time.Sleep(20 * time.Millisecond)
				}
			}

			if got := r.page(t).Netlog("s1"); got != n {
				t.Fatalf("netlog after the new connection = %p, want the capture from before (%p)", got, n)
			}
			s := n.Snapshot(Filter{})
			want := 1
			if path == "failed takeover setup" {
				want = 2
			}
			if len(s.Gaps) != want {
				t.Fatalf("gaps = %+v, want %d", s.Gaps, want)
			}
			g := s.Gaps[0]
			if !slices.Equal(g.Closed, []string{"page-1:inflight"}) || g.Start.Before(before) || g.End.Before(g.Start) {
				t.Fatalf("gap %+v, want it to close [page-1:inflight] and %v <= start <= end", g, before)
			}
			if path == "failed takeover setup" {
				if g.End.After(failedEnd) {
					t.Fatalf("first gap ends at %v, after the failed setup returned (%v)", g.End, failedEnd)
				}
				if g2 := s.Gaps[1]; !g2.Start.Equal(g.End) || len(g2.Closed) != 0 {
					t.Fatalf("second gap %+v, want it to start at %v and close nothing", g2, g.End)
				}
			}
			got, _ := n.Get("page-1:inflight")
			if got.State != StateCaptureReset {
				t.Fatalf("request in flight across the new connection is %q, want %q", got.State, StateCaptureReset)
			}
			if d, _ := n.Get("page-1:done"); d.State != StateFinished || d.BodyGone == "" {
				t.Fatalf("finished request after the gap: %+v, want finished with its body reported gone", d)
			}
			if k := pending(n); k != 0 {
				t.Fatalf("%d requests in flight after the gap, want 0", k)
			}
			// The capture goes on over the new connection, after the gap.
			r.f.Emit("Network.requestWillBeSent", reqEvent("after", "https://fixture.example/after", 3))
			eventually(t, "a request after the new connection", func() bool { _, ok := n.Get("page-1:after"); return ok })
			if a, _ := n.Get("page-1:after"); a.State != StatePending || a.Seq <= s.Gaps[len(s.Gaps)-1].AfterSeq {
				t.Fatalf("request after the new connection: %+v, want pending and after the gap (afterSeq %d)", a, s.Gaps[len(s.Gaps)-1].AfterSeq)
			}
		})
	}
}

// pageConnState is the state of p's page connection.
func pageConnState(p *Page) core.ConnState { return p.ps.Caller.(connState).State() }

// failBootstrapScript makes the page refuse Page.setBootstrapScript while
// the returned flag is set. A new connection's setup sends it in its
// pipelined batch after the Network.disable that resets the backend's
// Network agent (core resetCalls), so the setup fails after that reset.
func failBootstrapScript(f *fakewebkit.Server) *atomic.Bool {
	fail := &atomic.Bool{}
	fail.Store(true)
	f.Handle("Page.setBootstrapScript", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if fail.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "setup failure after Network.disable (test)"}
		}
		return map[string]any{}, nil
	})
	return fail
}
