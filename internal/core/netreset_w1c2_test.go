package core

import (
	"errors"
	"testing"
	"time"
)

// W1c-2 (backlog C04): the Network reinitialization barrier records a held
// request as reset in the same critical section in which its continue
// closes the hold. Before, resetHolds recorded it in a later critical
// section, and a client completion in between was refused as "no open
// intercepted request" instead of with ErrRequestReset (design 2.4 step 2:
// "a later completion for such a request fails with 'request was released
// by a network reset'").
func TestCompletionRightAfterAResetContinueGetsErrRequestReset(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	waitUntil(t, "the held request", func() bool { return len(h.s.PendingRequests()) == 1 })
	var completion error
	called := false
	// The first moment after the reset's continue closed the hold.
	h.s.holdClosed = func(k holdKey) {
		if k == (holdKey{"page-1", "r1"}) && !called {
			called = true
			_, completion = h.s.Control(ctx, completeOn("page-1", "r1"))
		}
	}
	ids, err := h.s.ResetHeldRequests(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("ResetHeldRequests = %v, %v; want [r1]", ids, err)
	}
	if !called {
		t.Fatal("the reset's continue did not close the hold")
	}
	if !errors.Is(completion, ErrRequestReset) {
		t.Fatalf("completion right after the reset's continue: err = %v, want ErrRequestReset", completion)
	}
	if n := completionsSent(h.f); n != 1 {
		t.Fatalf("%d completions sent, want only the reset's continue", n)
	}
}

// The release barrier is not a Network reset: a completion right after its
// continue is refused without ErrRequestReset, and nothing is recorded.
func TestCompletionRightAfterAReleaseContinueIsNotAReset(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	waitUntil(t, "the held request", func() bool { return len(h.s.PendingRequests()) == 1 })
	var completion error
	called := false
	h.s.holdClosed = func(k holdKey) {
		if !called {
			called = true
			_, completion = h.s.Control(ctx, completeOn("page-1", "r1"))
		}
	}
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !called || !errors.Is(completion, ErrControlRefused) || errors.Is(completion, ErrRequestReset) {
		t.Fatalf("completion right after the release's continue (called %v): err = %v, want a refusal without ErrRequestReset", called, completion)
	}
	h.s.mu.Lock()
	reset := len(h.s.resetReqs)
	h.s.mu.Unlock()
	if reset != 0 {
		t.Fatalf("%d holds recorded as reset by the release barrier, want none", reset)
	}
}
