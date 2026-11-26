package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// untilTestDeadline polls cond until it holds and fails the test, naming
// what, shortly before the test's own deadline (go test -timeout).
func untilTestDeadline(t *testing.T, what string, cond func() bool) {
	t.Helper()
	dl, ok := t.Deadline()
	for !cond() {
		if ok && time.Until(dl) < time.Second {
			t.Fatalf("%s did not happen before the test deadline", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// detachedAwaits starts n operations that each wait in a stalled
// Runtime.awaitPromise and detach, as CDP's awaited evaluations do
// (FIX-L); it returns a channel that gets each await's error.
func detachedAwaits(t *testing.T, h *harness, ctx context.Context, n int) <-chan error {
	t.Helper()
	errs := make(chan error, n)
	before := len(h.f.ReceivedMethod("Runtime.awaitPromise"))
	for i := 0; i < n; i++ {
		op := h.begin(t, ctx, ClassMutate)
		go func() {
			_, err := op.Caller().Send(ctx, "Runtime.awaitPromise", map[string]any{"promiseObjectId": "p"})
			errs <- err
			op.End()
		}()
		want := before + i + 1
		untilTestDeadline(t, "the detached await to reach the page", func() bool { return len(h.f.ReceivedMethod("Runtime.awaitPromise")) == want })
		op.Detach()
	}
	return errs
}

// runtimeCallsSent counts the call expressions of runtime function fn
// that the page received.
func runtimeCallsSent(h *harness, fn string) int {
	n := 0
	for _, c := range h.f.ReceivedMethod("Runtime.evaluate") {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil && m[1] == fn {
			n++
		}
	}
	return n
}

// FIX-M F1 (review FIX-K x FIX-L): an armed async call through an
// operation's caller (a wait slice) holds no send slot while its armed
// await waits, so its call is sent although every other slot is held by
// a pending await: before, the await held the last slot and the call that
// settles it was never sent.
func TestArmedAwaitHoldsNoSendSlot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inFlight int
		detached int
	}{
		{"seven detached CDP awaits and one wait", 0, DefaultMaxInFlight - 1},
		{"one slot", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{MaxInFlight: tc.inFlight})
			h.f.Stall("Runtime.awaitPromise")
			ctx := testDeadlineCtx(t)
			detachedAwaits(t, h, ctx, tc.detached)
			op := h.begin(t, ctx, ClassRead)
			defer op.End()
			wctx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				var v any
				done <- runtime.New(nil, nil).Via(op.Caller()).CallAsync(wctx, "wait", map[string]any{"k": "text", "text": "x", "slice": 1000}, &v)
			}()
			untilTestDeadline(t, "the wait's call expression", func() bool { return runtimeCallsSent(h, "wait") > 0 })
			stop()
			if err := <-done; err == nil {
				t.Log("the canceled wait answered") // either outcome is fine here
			}
		})
	}
}

// FIX-M F1 (review FIX-L): eight detached awaits, as one chrome-devtools-mcp
// wait_for makes for 2 locators x texts x frames, leave the page's send
// slots free: an admitted operation sends at once (the calls that answer
// Puppeteer's binding and settle the awaits). The awaits stay fenced: the
// release cancels them.
func TestDetachedAwaitsLeaveTheSendSlots(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.Stall("Runtime.awaitPromise")
	ctx := testDeadlineCtx(t)
	errs := detachedAwaits(t, h, ctx, DefaultMaxInFlight)
	op := h.begin(t, ctx, ClassMutate) // admitted: every earlier operation is detached
	sent := make(chan error, 1)
	go func() {
		_, err := op.Caller().Send(ctx, "Test.next", nil)
		sent <- err
	}()
	untilTestDeadline(t, "the admitted operation's send", func() bool { return len(h.f.ReceivedMethod("Test.next")) == 1 })
	if err := <-sent; err != nil {
		t.Fatalf("send of the admitted operation: %v", err)
	}
	op.End()
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for i := 0; i < DefaultMaxInFlight; i++ {
		if err := <-errs; err == nil || !errors.Is(err, ErrLeaseEnded) {
			t.Fatalf("detached await after the release: %v, want the lease end", err)
		}
	}
}

// testDeadlineCtx is a context that ends at the test's deadline (an
// hour from now without one); the operations of a test need a deadline.
// Its calls have no call budget of their own, so a stalled await ends
// only with the test: no budget frees a send slot meanwhile.
func testDeadlineCtx(t *testing.T) context.Context {
	dl, ok := t.Deadline()
	if !ok {
		dl = time.Now().Add(time.Hour)
	}
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	t.Cleanup(cancel)
	return webkit.WithCallBudget(ctx, time.Until(dl))
}
