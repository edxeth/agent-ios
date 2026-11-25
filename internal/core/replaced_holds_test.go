package core

import (
	"reflect"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Finding 3 of the core-events review (docs/notes/w1b.md, "core-events r1:
// holds of a replaced target"): the scheduler drops the holds of the
// target that a commit replaced (didCommitProvisionalTarget oldTargetId).
// WebKit main disconnects that target's backend at the commit, which
// continues its pending intercepts. iOS 16 and WebKitGTK 2.54 do not: a
// cached page keeps its backend, and its holds stay pending in WebKit
// (fakewebkit Options.SuspendOnCommit). Dropping them is still right
// there: no completion can reach a destroyed target (the session refuses
// to send to it), and when the page is restored its target's setup resets
// the Network agent, whose Network.disable continues every pending
// intercept before the page runs again. So no hold of the replaced target
// is ever answerable through the daemon, in either model, and the release
// barrier must not wait for one.
func TestReplacedTargetHoldsStayDroppedAcrossARestore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		suspend bool
	}{{"WebKit main: disconnect at commit", false}, {"iOS 16 and WebKitGTK 2.54: suspended backend", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{SuspendOnCommit: tc.suspend}, false, ManagerOptions{})
			r.reserve(t, "agent")
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "capture", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			pg := mo.Page
			r.m.End(mo.ID)
			r.f.Intercept("page-1", "r1", "request")
			eventually(t, 3*time.Second, "held r1", func() bool { return len(pg.PS.Sched.PendingHolds()) == 1 })

			// Cross-site navigation: page-2 commits and replaces page-1.
			r.f.Provisional("page-2")
			eventually(t, 3*time.Second, "page-2's setup", func() bool { return len(r.f.ReceivedMethod("Network.enable")) == 2 })
			r.f.Commit("page-2")
			eventually(t, 3*time.Second, "the commit", func() bool { return pg.PS.Caller.(interface{ ActiveTarget() string }).ActiveTarget() == "page-2" })
			eventually(t, 3*time.Second, "the drop", func() bool { return len(pg.PS.Sched.PendingHolds()) == 0 })
			var want []string
			if tc.suspend {
				want = []string{"r1"} // pending in the cached page's backend
			}
			if got := r.f.HeldRequests("page-1"); !reflect.DeepEqual(got, want) {
				t.Fatalf("page-1's backend holds %v after the commit, want %v", got, want)
			}

			// Back: the cached page-1 is restored with its target id.
			r.f.Provisional("page-1")
			eventually(t, 3*time.Second, "page-1's setup", func() bool { return len(r.f.ReceivedMethod("Network.enable")) == 3 })
			if got := r.f.HeldRequests("page-1"); len(got) != 0 {
				t.Fatalf("page-1's backend holds %v after the restore's setup, want none: its Network reset continues them", got)
			}
			r.f.Commit("page-1")
			eventually(t, 3*time.Second, "the restore", func() bool { return pg.PS.Caller.(interface{ ActiveTarget() string }).ActiveTarget() == "page-1" })
			if p := pg.PS.Sched.PendingHolds(); len(p) != 0 {
				t.Fatalf("pending holds after the restore = %v, want none", p)
			}
			if err := pg.PS.Sched.Release(ctx, nil); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if a := r.f.Answers(); len(a) != 0 {
				t.Fatalf("completions that took a hold: %+v, want none", a)
			}
		})
	}
}
