package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// The core-events reviewer's round-4 reproductions
// (/home/devkit/.pi/artifacts/reviewer/core-events-r4-repro/), kept as
// regression tests in round 5.

// Design 2.4 and acceptance 4: every failed cleanup send leaves a pending obligation.
func TestReviewFailedDOMCleanupStaysPending(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "inspect mode", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	inspect := false
	refuse := false
	r.f.Handle("DOM.setInspectModeEnabled", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.Unmarshal(c.Params, &p)
		mu.Lock()
		defer mu.Unlock()
		if !p.Enabled && refuse {
			return nil, &fakewebkit.Error{Code: -32000, Message: "cleanup refused"}
		}
		inspect = p.Enabled
		return nil, nil
	})
	if _, err := r.m.Raw(ctx, mo.ID, "DOM.setInspectModeEnabled", json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	mu.Lock()
	refuse = true
	mu.Unlock()
	if err := r.m.Break(ctx, mo.Page.Key); err == nil {
		t.Fatal("fixture: expected refused cleanup")
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	if err := resetUnknown(ctx, mo.Page); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	leaked := inspect
	mu.Unlock()
	t.Logf("owners=%v unknown=%v", mo.Page.PS.State.AllOwners(), mo.Page.PS.State.unknownDomains())
	if leaked {
		t.Fatal("failed DOM cleanup lost its obligation: inspect mode is still enabled after resetUnknown, with no released owner")
	}
}

// Acceptance 3: a successful disable records the holds it continued even when its caller stopped waiting.
func TestReviewLateNetworkDisableReplyRecordsHolds(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "monitor", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	r.f.Intercept("page-1", "late", "request")
	eventually(t, 3*time.Second, "held request", func() bool { return len(mo.Page.PS.Sched.PendingRequests()) == 1 })
	r.f.DelayReplies("Network.disable", 300*time.Millisecond)
	short, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	_, err = r.m.Raw(short, mo.ID, "Network.disable", nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture: disable err=%v, want caller deadline", err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := mo.Page.conn.SendControl(ctx, "Test.sync", nil); err != nil {
		t.Fatal(err)
	}
	if got := r.f.HeldRequests("page-1"); len(got) > 0 {
		t.Fatalf("fixture: backend holds %v", got)
	}
	t.Logf("after delayed successful disable reply: pending=%v unknown=%v", mo.Page.PS.Sched.PendingRequests(), mo.Page.PS.State.unknownDomains())
	_, err = mo.Page.PS.Sched.control(ctx, ControlMsg{Kind: ControlCompleteRequest, TargetID: "page-1", RequestID: "late", Method: "Network.interceptContinue"})
	if !errors.Is(err, ErrRequestReset) {
		t.Fatalf("late successful Network.disable did not record its reset: completion err=%v, want ErrRequestReset", err)
	}
}
