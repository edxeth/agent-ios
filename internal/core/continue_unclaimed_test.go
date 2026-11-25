package core

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Scheduler.ContinueUnclaimed (change.go) is the daemon's own unmodified
// continue of an intercepted request that no handler claims (the safety
// net of internal/devtools). It goes through the scheduler's control path,
// so the hold rules of design 2.10 and docs/notes/w1b.md ("held requests
// and the reconnect gap") apply to it: it needs no lease, it sends the
// continue only at the stage of the hold, with the stage parameter that
// WIP requires, and it never answers a hold twice: a hold whose completion
// is in flight, or that is gone, is refused without a send.
func TestContinueUnclaimedFollowsTheHoldRules(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// No lease: the continue goes out at the hold's stage.
	h.f.Emit("Network.responseIntercepted", map[string]string{"requestId": "r1"})
	waitUntil(t, "the response hold", func() bool { return len(h.s.PendingRequests()) == 1 })
	if err := h.s.ContinueUnclaimed(ctx, "", "r1", "request"); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("continue at the request stage of a response hold: %v, want ErrControlRefused", err)
	}
	if got := continues(t, h.f); len(got) != 0 {
		t.Fatalf("a continue at the wrong stage was sent: %v", got)
	}
	if err := h.s.ContinueUnclaimed(ctx, "", "r1", "response"); err != nil {
		t.Fatalf("continue after the release: %v", err)
	}
	want := []map[string]any{{"requestId": "r1", "stage": "response"}}
	if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("interceptContinue params = %v, want %v", got, want)
	}
	if p := h.s.PendingRequests(); len(p) != 0 {
		t.Fatalf("pending after the continue = %v, want none", p)
	}
	// The hold is gone: a second continue is refused and not sent.
	if err := h.s.ContinueUnclaimed(ctx, "", "r1", "response"); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("second continue of r1: %v, want ErrControlRefused", err)
	}

	// A completion in flight: a second one is refused while it runs.
	h.f.DelayReplies("Network.interceptContinue", 300*time.Millisecond)
	h.f.Emit("Network.requestIntercepted", map[string]string{"requestId": "r2"})
	waitUntil(t, "the request hold", func() bool { return len(h.s.PendingRequests()) == 1 })
	first := make(chan error, 1)
	go func() { first <- h.s.ContinueUnclaimed(ctx, "", "r2", "request") }()
	waitUntil(t, "the first continue of r2 on the wire", func() bool { return len(continues(t, h.f)) == 2 })
	if err := h.s.ContinueUnclaimed(ctx, "", "r2", "request"); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("continue of r2 while one is in flight: %v, want ErrControlRefused", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first continue of r2: %v", err)
	}
	want = append(want, map[string]any{"requestId": "r2", "stage": "request"})
	if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("interceptContinue params = %v, want %v (one continue per hold)", got, want)
	}
}

// W4 round 6: ContinueUnclaimed continues the hold of the target that the
// interception event came from, so a request that a provisional target
// paused is continued on that target while the old target is still
// active. "" names the active target, which does not hold the request:
// refused, nothing sent.
func TestContinueUnclaimedContinuesOnTheHoldsTarget(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Provisional("page-2")
	h.f.Intercept("page-2", "r1", "request")
	waitUntil(t, "the provisional hold", func() bool { return len(h.s.PendingHolds()) == 1 })
	if err := h.s.ContinueUnclaimed(ctx, "", "r1", "request"); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("continue of page-2's request on the active target: %v, want ErrControlRefused", err)
	}
	if err := h.s.ContinueUnclaimed(ctx, "page-2", "r1", "request"); err != nil {
		t.Fatalf("continue on page-2: %v", err)
	}
	want := []fakewebkit.Answer{{Target: "page-2", RequestID: "r1", Stage: "request", Method: "Network.interceptContinue"}}
	if got := h.f.Answers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("answers = %+v, want %+v", got, want)
	}
	if n := len(continues(t, h.f)); n != 1 {
		t.Fatalf("%d continues sent, want 1", n)
	}
}
