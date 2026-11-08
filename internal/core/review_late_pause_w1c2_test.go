package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Contract: W1c-2 R1 brief, pauses that arrive during release are drained.
func TestReviewReleaseDoesNotLoseAPauseWakeup(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 3*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	defer op.End()
	injected := false
	afterPausePending = func() {
		if !injected {
			injected = true
			m.pause("page-1")
			syncPage(t, h.sess, "page-1")
		}
	}
	defer func() { afterPausePending = nil }()
	rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// A paused running evaluation cannot finish until the barrier resumes it.
	// End the operation when the backend receives its resume.
	h.f.Handle("Debugger.resume", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		delete(m.paused, c.Target)
		m.resumes = append(m.resumes, c.Target)
		m.mu.Unlock()
		op.End()
		return nil, nil
	})
	err := h.s.Release(rctx, nil)
	paused, _, _, _ := m.state()
	if err != nil || len(paused) != 0 {
		t.Fatalf("pause arrived after pending() but before changed snapshot: Release = %v, backend paused = %v; want successful resume", err, paused)
	}
}

// Contract: the lease has not ended while its cleanup callback runs.
func TestReviewReleaseDrainsAPauseDuringCleanup(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 3*time.Second)
	err := h.s.Release(ctx, func(context.Context, webkit.Caller) error {
		m.pause("page-1")
		syncPage(t, h.sess, "page-1")
		return nil
	})
	paused, _, _, calls := m.state()
	if err != nil || len(paused) != 0 {
		t.Fatalf("Release = %v, backend paused = %v, resume writes = %d; want no live pause before lease ends", err, paused, calls)
	}
}

// Contract: the release error names every target still paused at the bound,
// even if a held request has already failed earlier in the same release.
func TestReviewReleaseNamesPausedTargetsAfterAnEarlierHoldError(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 3*time.Second)
	h.f.Intercept("page-1", "r1", "request")
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	h.f.Handle("Network.interceptContinue", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32602, Message: "Invalid stage"}
	})
	h.s.c = &faultyUnsent{Caller: h.s.c, unsent: maxBarrierContinues}
	err := h.s.Release(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), `"page-2"`) {
		t.Fatalf("Release = %v; want paused page-2 named (preserving the earlier hold failure is also valid)", err)
	}
}

// Contract: a target at the unsent bound remains named when an operation
// also fails to finish before the release deadline.
func TestReviewReleaseNamesPausedTargetsWhenOperationWaitExpires(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 3*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	defer op.End()
	m.pause("page-1")
	syncPage(t, h.sess, "page-1")
	h.s.c = &faultyUnsent{Caller: h.s.c, unsent: maxBarrierContinues}
	rctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err := h.s.Release(rctx, nil)
	if err == nil || !strings.Contains(err.Error(), `"page-1"`) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Release = %v; want deadline error naming paused page-1", err)
	}
}
