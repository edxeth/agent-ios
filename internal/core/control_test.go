package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// evalInOp runs a held Runtime.evaluate inside op and reports when it ends.
func evalInOp(ctx context.Context, op *Op) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := op.Caller().Send(ctx, "Runtime.evaluate", map[string]string{"expression": "await fetch('/x')"})
		done <- err
	}()
	return done
}

func seqIn(f *fakewebkit.Server, method string) int {
	for _, r := range f.Received() {
		if r.Method == method {
			return r.Seq
		}
	}
	return 0
}

// 7.3 test 16a-c: a held request is completed through the control lane
// while the operation that waits for it holds the page.
func TestControlLaneCompletesHeldRequest(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.HoldUntil("Runtime.evaluate", "Network.interceptContinue", "Network.requestIntercepted", map[string]string{"requestId": "r1"})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	start := time.Now()
	evalDone := evalInOp(ctx, op)
	waitUntil(t, "the intercepted request", func() bool { return len(h.s.PendingRequests()) == 1 })

	// (c) an ordinary mutation submitted during the wait queues.
	succSent := make(chan time.Time, 1)
	go func() {
		s, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = s.Caller().Send(ctx, "Test.successor", nil)
		succSent <- time.Now()
		s.End()
	}()
	waitUntil(t, "the successor to queue", func() bool { return h.queueLen() == 1 })

	ctlStart := time.Now()
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}); err != nil {
		t.Fatalf("continue r1: %v", err)
	}
	if el := time.Since(ctlStart); el > 100*time.Millisecond {
		t.Fatalf("control message took %s; it must not wait behind the operation", el)
	}
	if err := <-evalDone; err != nil {
		t.Fatalf("evaluation: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("evaluation completed after %s, want within 1 s", el)
	}
	if n := len(h.f.ReceivedMethod("Test.successor")); n != 0 {
		t.Fatal("the queued mutation was sent while the evaluation held the page")
	}
	opEnd := time.Now()
	op.End()
	if at := <-succSent; at.Before(opEnd) {
		t.Fatal("successor ran before the waiting operation ended")
	}

	// (b) refused control messages are never sent. r2 is open, so the
	// epoch, session and method checks are what refuse those messages.
	h.f.Emit("Network.requestIntercepted", map[string]string{"requestId": "r2"})
	waitUntil(t, "r2", func() bool { return len(h.s.PendingRequests()) == 1 })
	refused := []ControlMsg{
		{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}, // second completion
		{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r9", Method: "Network.interceptContinue"}, // unknown id
		{Kind: ControlCompleteRequest, Session: testSession, Epoch: 0, RequestID: "r2", Method: "Network.interceptContinue"}, // old epoch
		{Kind: ControlCompleteRequest, Session: "other", Epoch: 1, RequestID: "r2", Method: "Network.interceptContinue"},     // other session
		{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r2", Method: "Runtime.evaluate"},          // not a completion
		{Kind: ControlDebuggerResume, Session: testSession, Epoch: 1},                                                        // not paused
	}
	for i, m := range refused {
		if _, err := h.s.Control(ctx, m); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("message %d err = %v, want ErrControlRefused", i, err)
		}
	}
	if n := len(h.f.ReceivedMethod("Network.interceptContinue")); n != 1 {
		t.Fatalf("fake received %d interceptContinue, want exactly 1", n)
	}
	if n := len(h.f.ReceivedMethod("Debugger.resume")); n != 0 {
		t.Fatal("a refused Debugger.resume was sent")
	}
	if p := h.s.PendingRequests(); len(p) != 1 || p[0] != "r2" {
		t.Fatalf("pending = %v; refused messages must not complete r2", p)
	}
}

// 7.3 test 16d: an evaluation paused in the debugger completes after a
// resume sent through the control lane.
func TestControlLaneResumesPausedDebugger(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.HoldUntil("Runtime.evaluate", "Debugger.resume", "Debugger.paused", map[string]any{"reason": "Breakpoint", "callFrames": []any{}})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	defer op.End()
	evalDone := evalInOp(ctx, op)
	waitUntil(t, "the pause", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.openPausesLocked()) > 0
	})
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlDebuggerResume, Session: testSession, Epoch: 1}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case err := <-evalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("evaluation did not complete after the resume")
	}
}

// 7.3 test 16e: canceling a waiting operation sends __iwdp.cancel within 1 s
// while the operation still holds its slot.
func TestCancelBehindWaitSendsPageCancel(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.HoldUntil("Runtime.evaluate", "Network.interceptContinue")
	ctx, _ := timeoutCtx(t, 10*time.Second)
	opCtx, cancel := context.WithCancel(ctx)
	op, err := h.s.Begin(opCtx, OpSpec{Class: ClassExclusive, Session: testSession, Epoch: 1, PageWork: true})
	if err != nil {
		t.Fatal(err)
	}
	evalDone := evalInOp(ctx, op)
	waitUntil(t, "the evaluation", func() bool { return len(h.f.ReceivedMethod("Runtime.evaluate")) == 1 })
	admitted := make(chan struct{})
	go func() {
		s, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		if err == nil {
			close(admitted)
			s.End()
		}
	}()
	waitUntil(t, "the successor to queue", func() bool { return h.queueLen() == 1 })
	cancelAt := time.Now()
	cancel()
	<-evalDone
	waitUntil(t, "the page cancel", func() bool { return len(h.rt.CallsOf("cancel")) == 1 })
	if at := h.rt.CallsOf("cancel")[0].At; at.Sub(cancelAt) > time.Second {
		t.Fatalf("cancel reached the page %s after the cancel, want within 1 s", at.Sub(cancelAt))
	}
	select {
	case <-admitted:
		t.Fatal("successor admitted before the canceled operation ended")
	default:
	}
	op.End()
	<-admitted
}

// 7.3 test 16f: a release during the wait closes admission, completes the
// pending items through the control lane, and only then runs its cleanup.
func TestReleaseBarrierCompletesPendingItemsBeforeCleanup(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.HoldUntil("Runtime.evaluate", "Network.interceptContinue",
		"Network.requestIntercepted", map[string]string{"requestId": "r1"},
		"Debugger.paused", map[string]any{"reason": "Breakpoint", "callFrames": []any{}})
	ctx, _ := timeoutCtx(t, 10*time.Second)
	op := h.begin(t, ctx, ClassExclusive)
	evalDone := evalInOp(ctx, op)
	go func() { <-evalDone; op.End() }()
	waitUntil(t, "the pending items", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.openPausesLocked()) > 0 && h.s.intercepted[holdKey{"page-1", "r1"}] != nil
	})
	queued := make(chan error, 1)
	go func() {
		_, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		queued <- err
	}()
	waitUntil(t, "an operation to queue", func() bool { return h.queueLen() == 1 })

	err := h.s.Release(ctx, func(ctx context.Context, c webkit.Caller) error {
		_, err := c.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": false})
		return err
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	cont, resume, cleanup := seqIn(h.f, "Network.interceptContinue"), seqIn(h.f, "Debugger.resume"), seqIn(h.f, "Network.setInterceptionEnabled")
	if cont == 0 || resume == 0 || cleanup == 0 || cont > cleanup || resume > cleanup {
		t.Fatalf("seq continue %d, resume %d, cleanup %d: the barrier must continue and resume before its cleanup", cont, resume, cleanup)
	}
	if err := <-queued; !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("queued operation err = %v, want ErrLeaseEnded", err)
	}
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("control after release err = %v, want refused", err)
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Design 2.10: each pending item is completed at most once. A control
// message that was never written does not complete its item: a later valid
// message completes it, and the item stays visible to Release meanwhile.
func TestUnsentControlMessageKeepsPendingItem(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	cont := ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}
	h.f.Emit("Network.requestIntercepted", map[string]string{"requestId": "r1"})
	waitUntil(t, "r1", func() bool { return len(h.s.PendingRequests()) == 1 })
	if _, err := h.s.Control(canceledCtx(), cont); err == nil {
		t.Fatal("continue with a canceled context succeeded")
	}
	if p := h.s.PendingRequests(); len(p) != 1 || p[0] != "r1" {
		t.Fatalf("pending = %v after an unsent continue, want [r1]", p)
	}
	if _, err := h.s.Control(ctx, cont); err != nil {
		t.Fatalf("valid continue after an unsent one: %v", err)
	}
	if _, err := h.s.Control(ctx, cont); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("second completion err = %v, want refused", err)
	}
	if n := len(h.f.ReceivedMethod("Network.interceptContinue")); n != 1 {
		t.Fatalf("fake received %d interceptContinue, want exactly 1", n)
	}

	resume := ControlMsg{Kind: ControlDebuggerResume, Session: testSession, Epoch: 1}
	h.f.Emit("Debugger.paused", map[string]any{"reason": "Breakpoint", "callFrames": []any{}})
	waitUntil(t, "the pause", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.openPausesLocked()) > 0
	})
	if _, err := h.s.Control(canceledCtx(), resume); err == nil {
		t.Fatal("resume with a canceled context succeeded")
	}
	if _, err := h.s.Control(ctx, resume); err != nil {
		t.Fatalf("valid resume after an unsent one: %v", err)
	}
	if _, err := h.s.Control(ctx, resume); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("second resume of one pause err = %v, want refused", err)
	}
	if n := len(h.f.ReceivedMethod("Debugger.resume")); n != 1 {
		t.Fatalf("fake received %d Debugger.resume, want exactly 1", n)
	}
}

// Design 2.10, control lane item 1: cancellation of a running operation
// through the control lane cancels it, sends __iwdp.cancel for page work,
// and is refused for an operation that is not running.
func TestControlLaneCancelsRunningOperation(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, PageWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlCancel, Session: testSession, Epoch: 1, OpID: "op-99"}); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("cancel of an unknown operation err = %v, want refused", err)
	}
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlCancel, Session: testSession, Epoch: 1, OpID: op.ID}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-op.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("the control-lane cancel did not cancel the operation")
	}
	waitUntil(t, "the page cancel", func() bool { return len(h.rt.CallsOf("cancel")) == 1 })
	op.End()
	if _, err := h.s.Control(ctx, ControlMsg{Kind: ControlCancel, Session: testSession, Epoch: 1, OpID: op.ID}); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("cancel of an ended operation err = %v, want refused", err)
	}
}

// Design 2.10, control lane item 3, and the release barrier: Target.resume
// for a target paused on start passes once, and Release resumes a target
// that is still paused before it runs its cleanup.
func TestControlLaneResumesTargetPausedOnStart(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, _ := timeoutCtx(t, 10*time.Second)
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{PauseOnStart: true, ResumeAfter: time.Minute, Setup: func(ctx context.Context, _ webkit.TargetCaller) error {
		<-ctx.Done() // the setup never finishes, so only the control lane resumes
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	s := NewScheduler(sess, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)

	go f.Swap("page-2", 0)
	waitUntil(t, "page-2 paused on start", func() bool { return sess.IsPaused("page-2") })
	msg := ControlMsg{Kind: ControlTargetResume, Session: testSession, Epoch: 1, TargetID: "page-2"}
	if _, err := s.Control(ctx, msg); err != nil {
		t.Fatalf("resume page-2: %v", err)
	}
	if _, err := s.Control(ctx, msg); !errors.Is(err, ErrControlRefused) {
		t.Fatalf("second resume err = %v, want refused", err)
	}
	if n := len(f.ReceivedMethod("Target.resume")); n != 1 {
		t.Fatalf("fake received %d Target.resume, want exactly 1", n)
	}

	waitUntil(t, "the commit of page-2", func() bool { return sess.ActiveTarget() == "page-2" })
	go f.Swap("page-3", time.Second) // the commit comes well after the barrier's cleanup
	waitUntil(t, "page-3 paused on start", func() bool { return sess.IsPaused("page-3") })
	if err := s.Release(ctx, func(ctx context.Context, c webkit.Caller) error {
		_, err := c.Send(ctx, "Test.cleanup", nil)
		return err
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	resumes := f.ReceivedMethod("Target.resume")
	if len(resumes) != 2 || !strings.Contains(string(resumes[1].Params), "page-3") {
		t.Fatalf("Target.resume records = %+v, want a second one for page-3", resumes)
	}
	if cleanup := seqIn(f, "Test.cleanup"); cleanup == 0 || resumes[1].Seq > cleanup {
		t.Fatalf("resume of page-3 at seq %d, cleanup at %d: the barrier resumes before its cleanup", resumes[1].Seq, cleanup)
	}
}
