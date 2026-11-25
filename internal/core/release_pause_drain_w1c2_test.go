package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W1c-2 repair round 1 (review F3): the release barrier drains the Debugger
// pauses. The reviewer's two schedules are kept in
// review_pause_release_w1c2_test.go; these tests pin the other rules of
// the drain (pauseDrain).

// The bound: a pause whose resume is not sent maxBarrierContinues times
// stays paused, and the release reports it by name instead of ending the
// lease silently.
func TestReleaseNamesAPauseItCouldNotResume(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	h.s.c = &faultyUnsent{Caller: h.s.c, unsent: maxBarrierContinues}
	err := h.s.Release(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), `"page-2"`) || !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("Release: err = %v, want an error that names page-2 with the unsent resume", err)
	}
	if _, _, _, calls := m.state(); calls != 0 {
		t.Fatalf("%d Debugger.resume written, want none", calls)
	}
}

// Fewer unsent resumes than the bound: the barrier sends the resume again
// and the release succeeds.
func TestReleaseSendsAnUnsentResumeAgain(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	h.s.c = &faultyUnsent{Caller: h.s.c, unsent: maxBarrierContinues - 1}
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	checkPauses(t, m, nil, []string{"page-2"})
}

// A resume that may have been written (here: an error reply) is never sent
// again: a second resume could resume a later pause that nobody saw. The
// release reports the target as still paused.
func TestReleaseDoesNotRepeatAResumeWithAnUnknownOutcome(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	calls := 0
	h.f.Handle("Debugger.resume", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		calls++
		if calls == 1 {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Internal error"}
		}
		delete(m.paused, c.Target)
		return nil, nil
	})
	err := h.s.Release(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), `"page-2"`) {
		t.Fatalf("Release: err = %v, want an error that names page-2", err)
	}
	m.mu.Lock()
	n := calls
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d Debugger.resume sent, want 1", n)
	}
}

// A pause that arrives while the release waits for a running operation (an
// evaluation that hits a breakpoint) is resumed too.
func TestReleaseResumesAPauseWhileItWaitsForOperations(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	released := make(chan error, 1)
	go func() { released <- h.s.Release(ctx, nil) }()
	waitUntil(t, "the release to cancel the operation", func() bool { return op.Context().Err() != nil })
	m.pause("page-1")
	waitUntil(t, "the resume of the new pause", func() bool { _, resumes, _, _ := m.state(); return len(resumes) == 1 })
	select {
	case err := <-released:
		t.Fatalf("Release ended (%v) while the operation runs", err)
	default:
	}
	op.End()
	if err := <-released; err != nil {
		t.Fatalf("Release: %v", err)
	}
	checkPauses(t, m, nil, []string{"page-1"})
}

// The context bounds the drain: when the release's context ends while a
// client resume is still in flight, the release fails and names the target
// that may still be paused.
func TestReleaseBoundsTheWaitForAResumeInFlight(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	c := &reviewUnwrittenResume{Caller: h.s.c, entered: make(chan struct{}), finish: make(chan struct{})}
	h.s.c = c
	client := make(chan error, 1)
	go func() { _, err := h.s.Control(ctx, resumeOn("page-2")); client <- err }()
	<-c.entered
	rctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err := h.s.Release(rctx, nil)
	close(c.finish)
	if cerr := <-client; !errors.Is(cerr, webkit.ErrNotSent) {
		t.Fatalf("client resume = %v, want ErrNotSent", cerr)
	}
	if err == nil || !strings.Contains(err.Error(), `"page-2"`) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Release: err = %v, want the deadline, naming page-2", err)
	}
}

// A client resume with an unknown outcome (an error reply) ends that pause
// as an item, so the barrier sends nothing for it; the target may still be
// paused, and the release names it with errResumeUnknown.
func TestReleaseNamesAPauseWhoseClientResumeHasAnUnknownOutcome(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	m := newPauseModel(h.f)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	h.f.Provisional("page-2")
	m.pause("page-2")
	syncPage(t, h.sess, "page-1")
	calls := 0
	h.f.Handle("Debugger.resume", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		calls++
		return nil, &fakewebkit.Error{Code: -32000, Message: "Internal error"}
	})
	if _, err := h.s.Control(ctx, resumeOn("page-2")); err == nil {
		t.Fatal("client resume succeeded; the fake answers it with an error")
	}
	err := h.s.Release(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), `"page-2"`) || !errors.Is(err, errResumeUnknown) {
		t.Fatalf("Release: err = %v, want errResumeUnknown naming page-2", err)
	}
	m.mu.Lock()
	n := calls
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d Debugger.resume sent, want only the client's", n)
	}
}

// faultyUnsent fails the first unsent Debugger.resume calls before they
// are written.
type faultyUnsent struct {
	webkit.Caller
	unsent int
}

func (c *faultyUnsent) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "Debugger.resume" && c.unsent > 0 {
		c.unsent--
		return nil, fmt.Errorf("%w: test: Debugger.resume was not written", webkit.ErrNotSent)
	}
	if next, ok := c.Caller.(interface {
		SendControl(context.Context, string, any) (json.RawMessage, error)
	}); ok {
		return next.SendControl(ctx, method, params)
	}
	return c.Caller.Send(ctx, method, params)
}
