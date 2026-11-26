package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

const testSession = "s1"

type harness struct {
	f     *fakewebkit.Server
	rt    *fakewebkit.PageRuntime
	sess  *webkit.Session
	s     *Scheduler
	mu    sync.Mutex
	sends []AttemptEvent
	hosts []time.Time // host time of each attempt send
}

func newHarness(t *testing.T, o SchedulerOptions) *harness {
	t.Helper()
	h := &harness{f: fakewebkit.New(fakewebkit.Options{}), rt: fakewebkit.NewPageRuntime()}
	t.Cleanup(h.f.Close)
	h.f.SetRuntime(h.rt.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, h.f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	h.sess = sess
	user := o.OnAttempt
	o.OnAttempt = func(e AttemptEvent) {
		h.mu.Lock()
		h.sends = append(h.sends, e)
		h.hosts = append(h.hosts, time.Now())
		h.mu.Unlock()
		if user != nil {
			user(e)
		}
	}
	h.s = NewScheduler(sess, o)
	t.Cleanup(h.s.Close)
	h.s.SetLease(testSession, 1)
	return h
}

func (h *harness) attempts() ([]AttemptEvent, []time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]AttemptEvent(nil), h.sends...), append([]time.Time(nil), h.hosts...)
}

func (h *harness) begin(t *testing.T, ctx context.Context, class Class) *Op {
	t.Helper()
	op, err := h.s.Begin(ctx, OpSpec{Name: "test", Class: class, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return op
}

func timeoutCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx, cancel
}

func (h *harness) queueLen() int {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return len(h.s.queue)
}

// hasWaiterAt reports whether c holds a timer that fires at exactly at.
// Test-only: it reads the waiters under the clock's lock.
func (c *ManualClock) hasWaiterAt(at time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if w.at == at {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSchedulerAdmission(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	r1 := h.begin(t, ctx, ClassRead)
	r2 := h.begin(t, ctx, ClassRead) // reads run in parallel
	mutated := make(chan *Op, 1)
	go func() {
		op, _ := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		mutated <- op
	}()
	waitUntil(t, "the mutation to queue", func() bool { return h.queueLen() == 1 })
	readAfter := make(chan *Op, 1)
	go func() {
		op, _ := h.s.Begin(ctx, OpSpec{Class: ClassRead, Session: testSession, Epoch: 1})
		readAfter <- op
	}()
	waitUntil(t, "the read to queue behind the mutation", func() bool { return h.queueLen() == 2 })
	r1.End()
	select {
	case <-mutated:
		t.Fatal("mutation admitted while a read runs")
	case <-time.After(30 * time.Millisecond):
	}
	r2.End()
	m := <-mutated
	select {
	case <-readAfter:
		t.Fatal("read admitted while a mutation runs (FIFO)")
	case <-time.After(30 * time.Millisecond):
	}
	m.End()
	(<-readAfter).End()

	if _, err := h.s.Begin(ctx, OpSpec{Class: ClassRead, Session: testSession, Epoch: 7}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old epoch Begin err = %v, want ErrStaleEpoch", err)
	}
	if _, err := h.s.Begin(context.Background(), OpSpec{Class: ClassRead, Session: testSession, Epoch: 1}); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Begin without deadline err = %v, want ErrNoDeadline", err)
	}
}

func TestOpCallerFencesSends(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassMutate)
	c := op.Caller()
	if _, err := c.Send(ctx, "Test.one", nil); err != nil {
		t.Fatal(err)
	}
	h.s.SetLease(testSession, 2) // the lease was reacquired: epoch 2
	if _, err := c.Send(ctx, "Test.two", nil); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("send under an old epoch err = %v, want ErrStaleEpoch", err)
	}
	op.End()
	if _, err := c.Send(ctx, "Test.three", nil); err == nil {
		t.Fatal("send after End was accepted")
	}
	if n := len(h.f.ReceivedMethod("Test.two")) + len(h.f.ReceivedMethod("Test.three")); n != 0 {
		t.Fatalf("fake received %d fenced sends", n)
	}
}

func TestReadCallsInFlightAreBounded(t *testing.T) {
	h := newHarness(t, SchedulerOptions{MaxInFlight: 2})
	h.f.Stall("Test.slow")
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassRead)
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < 3; i++ {
		go func() { _, _ = op.Caller().Send(callCtx, "Test.slow", nil) }()
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(h.f.ReceivedMethod("Test.slow")); n != 2 {
		t.Fatalf("calls in flight = %d, want the limit 2", n)
	}
	cancel()
	op.End()
}

// A detached operation (Op.Detach: CDP's awaited Runtime.awaitPromise)
// no longer holds the page: a queued mutation is admitted while its
// waiting call is in flight, and that call stays fenced; the End of a
// detached operation frees nothing that a later operation holds; a
// release cancels a detached operation and waits for its end.
func TestDetachedOperationAdmitsTheQueue(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.f.Stall("Test.await")
	ctx, _ := timeoutCtx(t, 5*time.Second)
	beginAsync := func() <-chan *Op {
		ch := make(chan *Op, 1)
		go func() {
			op, err := h.s.Begin(ctx, OpSpec{Name: "next", Class: ClassMutate, Session: testSession, Epoch: 1})
			if err != nil {
				t.Errorf("Begin: %v", err)
			}
			ch <- op
		}()
		return ch
	}
	admittedWithin := func(ch <-chan *Op, d time.Duration) *Op {
		select {
		case op := <-ch:
			return op
		case <-time.After(d):
			return nil
		}
	}

	// a waits in a call, detached; b is admitted meanwhile.
	a := h.begin(t, ctx, ClassMutate)
	awaited := make(chan error, 1)
	go func() {
		_, err := a.Caller().Send(ctx, "Test.await", nil)
		awaited <- err
		a.End()
	}()
	waitUntil(t, "the waiting call to reach the page", func() bool { return len(h.f.ReceivedMethod("Test.await")) == 1 })
	a.Detach()
	b := admittedWithin(beginAsync(), 2*time.Second)
	if b == nil {
		t.Fatal("a mutation queued behind a detached operation was not admitted")
	}
	if _, err := b.Caller().Send(ctx, "Test.next", nil); err != nil {
		t.Fatalf("send of the admitted operation: %v", err)
	}

	// e is detached and ends while b runs: c stays queued behind b.
	eCh := beginAsync()
	waitUntil(t, "e to queue behind b", func() bool { return h.queueLen() == 1 })
	b.End()
	e := admittedWithin(eCh, 2*time.Second)
	if e == nil {
		t.Fatal("e was not admitted after b ended")
	}
	e.Detach()
	c := admittedWithin(beginAsync(), 2*time.Second)
	if c == nil {
		t.Fatal("c was not admitted after e detached")
	}
	dCh := beginAsync()
	waitUntil(t, "d to queue behind c", func() bool { return h.queueLen() == 1 })
	e.End()
	if d := admittedWithin(dCh, 50*time.Millisecond); d != nil {
		t.Fatal("the End of a detached operation admitted d while c runs")
	}
	c.End()
	d := admittedWithin(dCh, 2*time.Second)
	if d == nil {
		t.Fatal("d was not admitted after c ended")
	}
	d.End()

	// The release cancels the detached a and returns after its End.
	if err := h.s.Release(ctx, nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case err := <-awaited:
		if !errors.Is(err, ErrLeaseEnded) {
			t.Fatalf("the detached call after the release: %v, want ErrLeaseEnded", err)
		}
	default:
		t.Fatal("Release returned before the detached operation's call ended")
	}
}

// 7.3 test 4a: cancel while queued.
func TestCancelWhileQueuedSendsNothing(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	a := h.begin(t, ctx, ClassMutate)
	bCtx, cancelB := context.WithCancel(ctx)
	bErr := make(chan error, 1)
	go func() {
		op, err := h.s.Begin(bCtx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		if err == nil {
			_, _ = op.Caller().Send(ctx, "Test.b", nil)
			op.End()
		}
		bErr <- err
	}()
	waitUntil(t, "b to queue", func() bool { return h.queueLen() == 1 })
	cDone := make(chan struct{})
	go func() {
		op, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		if err == nil {
			_, _ = op.Caller().Send(ctx, "Test.c", nil)
			op.End()
		}
		close(cDone)
	}()
	waitUntil(t, "c to queue", func() bool { return h.queueLen() == 2 })
	cancelB() // the client of b disconnects
	if err := <-bErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("b err = %v, want canceled", err)
	}
	if _, err := a.Caller().Send(ctx, "Test.a", nil); err != nil {
		t.Fatal(err)
	}
	a.End()
	<-cDone
	var got []string
	for _, r := range h.f.Received() {
		if strings.HasPrefix(r.Method, "Test.") {
			got = append(got, r.Method)
		}
	}
	if strings.Join(got, ",") != "Test.a,Test.c" {
		t.Fatalf("fake received %v, want [Test.a Test.c]", got)
	}
}

func attemptFn(h *harness) int { return len(h.rt.CallsOf("act")) }

// 7.3 test 4b: lost cancel during a retry loop.
func TestLostCancelDuringRetryLoop(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	h.rt.Ready = func(string, int) (bool, string) { return false, "covered" }
	h.rt.IgnoreCancel = true // the __iwdp.cancel message is lost
	ctx, cancel := timeoutCtx(t, 3*time.Second)
	op, err := h.s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1}); errc <- err }()
	time.Sleep(500 * time.Millisecond)
	cancelAt := h.s.o.Clock.Now()
	cancelHost := time.Now()
	cancel()
	err = <-errc
	op.End()
	if err == nil {
		t.Fatal("RunSteps succeeded after cancel")
	}
	time.Sleep(time.Second)
	sends, _ := h.attempts()
	if len(sends) < 2 {
		t.Fatalf("only %d attempts before the cancel; the retry loop did not run", len(sends))
	}
	for _, s := range sends {
		if s.SentAt >= cancelAt {
			t.Fatalf("attempt sent at %s, after the cancel at %s", s.SentAt, cancelAt)
		}
	}
	if got := attemptFn(h); got != len(sends) {
		t.Fatalf("fake received %d attempts, the daemon sent %d", got, len(sends))
	}
	cancels := h.rt.CallsOf("cancel")
	if len(cancels) != 1 || cancels[0].At.Sub(cancelHost) > time.Second {
		t.Fatalf("cancel calls = %+v, want one within 1 s of the cancel", cancels)
	}
	if d := h.rt.Dispatched(); len(d) != 0 {
		t.Fatalf("page dispatched %+v", d)
	}
}

// 7.3 test 4c: attempt in flight at the cancel.
func TestCancelWithAttemptInFlightHoldsSlot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropReply bool
	}{{"reply arrives", false}, {"reply lost", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// Design 2.6: a cancel is answered at once, and the slot stays
			// held until the attempt's reply or its slot-release boundary
			// H = min(s + 250 ms, D), whichever comes first. The test orders
			// both events itself, so a late test goroutine cannot reorder
			// them: the fake holds the attempt's reply until the test sends
			// releaseReply, and the scheduler and the page run on a manual
			// clock that reaches H only when the test sets it. The long reply
			// grace keeps the real-time limit of the reply wait (H - s plus
			// the grace) from ending the attempt during the test.
			const releaseReply = "Test.releaseAttemptReply"
			clock := NewManualClock(time.Second)
			sent := make(chan AttemptEvent, 1)
			var h *harness
			h = newHarness(t, SchedulerOptions{Clock: clock, ReplyGrace: time.Minute, OnAttempt: func(e AttemptEvent) {
				// OnAttempt runs before the attempt is written, so the
				// attempt is the next Runtime.evaluate the fake receives.
				if !tc.dropReply {
					h.f.HoldUntil("Runtime.evaluate", releaseReply)
				}
				sent <- e
			}})
			h.rt.Now = clock.Now
			if tc.dropReply {
				h.rt.DropReplyOfStep = 1
			}
			h.f.SetRuntime(h.rt.Handler()) // publishes the settings above under the fake's lock
			ctx, _ := timeoutCtx(t, 5*time.Second)
			opCtx, cancel := context.WithCancel(ctx)
			op, err := h.s.Begin(opCtx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			errc := make(chan error, 1)
			go func() { _, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1}); errc <- err }()
			attempt := <-sent
			// The page received the attempt (and its reply is held or
			// dropped), so the cancel's runtime call cannot take the hold.
			waitUntil(t, "the attempt to reach the page", func() bool { return attemptFn(h) == 1 })
			cancelHost := time.Now()
			cancel()
			err = <-errc
			if el := time.Since(cancelHost); el > 30*time.Millisecond {
				t.Fatalf("canceled client answered after %s, want at once", el)
			}
			if !errors.Is(err, ErrOutcomeUnknown) || !strings.Contains(err.Error(), "canceled; outcome unknown") {
				t.Fatalf("err = %v, want canceled; outcome unknown", err)
			}
			op.End()
			admitted := make(chan *Op, 1)
			go func() {
				succ, err := h.s.Begin(ctx, OpSpec{Name: "successor", Class: ClassMutate, Session: testSession, Epoch: 1})
				if err != nil {
					t.Errorf("successor Begin: %v", err)
				}
				admitted <- succ
			}()
			// A slot released at End would admit the successor at once.
			waitUntil(t, "the successor to queue behind the held slot", func() bool { return h.queueLen() == 1 })
			boundary := attempt.SentAt + DefaultAttemptBudget // D is 5 s away
			// End registers its boundary timer in its own goroutine, as an
			// offset from the clock's time then. A clock moved before that
			// would shift the timer past H, so the test moves it only after
			// the timer at H exists.
			waitUntil(t, "End's timer at the slot-release boundary", func() bool { return clock.hasWaiterAt(boundary) })
			if tc.dropReply {
				clock.Set(boundary - time.Microsecond)
			}
			select {
			case <-admitted:
				t.Fatal("successor admitted while the attempt's reply and its slot-release boundary were both pending")
			case <-time.After(30 * time.Millisecond):
			}
			if tc.dropReply {
				clock.Set(boundary)
			} else if _, err := h.sess.Send(ctx, releaseReply, nil); err != nil {
				t.Fatal(err)
			}
			var succ *Op
			select {
			case succ = <-admitted:
			case <-ctx.Done():
				t.Fatal("successor not admitted after the attempt's reply or its slot-release boundary")
			}
			if succ == nil {
				t.FailNow() // Begin failed; the goroutine reported it
			}
			if _, err := succ.Caller().Send(ctx, "Test.successor", nil); err != nil {
				t.Fatal(err)
			}
			succ.End()
			if tc.dropReply {
				if succ.AdmittedAt < boundary {
					t.Fatalf("successor admitted at %s, before the slot-release boundary %s", succ.AdmittedAt, boundary)
				}
				return
			}
			// The successor reached the page after the message that released
			// the attempt's reply.
			var order []string
			for _, r := range h.f.Received() {
				if strings.HasPrefix(r.Method, "Test.") {
					order = append(order, r.Method)
				}
			}
			if want := releaseReply + ",Test.successor"; strings.Join(order, ",") != want {
				t.Fatalf("fake received %v, want %s", order, want)
			}
		})
	}
}

// 7.3 test 4d and 4g: a lost reply stops the sequence; the step is not resent.
func TestLostStepReplyStopsSequence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		steps, drop int
	}{{"single action", 1, 1}, {"type abc, step 2", 3, 2}, {"drag, step 4", 10, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{ReplyGrace: 300 * time.Millisecond})
			h.rt.DropReplyOfStep = tc.drop
			ctx, _ := timeoutCtx(t, 5*time.Second)
			op := h.begin(t, ctx, ClassMutate)
			defer op.End()
			res, err := op.RunSteps(Action{Name: "type", Fn: "act", Steps: tc.steps})
			var se *StepError
			if !errors.As(err, &se) || !se.MayHaveRun || se.Step != tc.drop {
				t.Fatalf("err = %v, want a StepError for step %d that may have run", err, tc.drop)
			}
			want := "the page may have run step " + itoa(tc.drop) + " of " + itoa(tc.steps)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %q, want it to contain %q", err, want)
			}
			if res.Acked != tc.drop-1 {
				t.Fatalf("acked = %d, want %d", res.Acked, tc.drop-1)
			}
			steps := map[int]int{}
			for _, d := range h.rt.Dispatched() {
				steps[d.Step]++
			}
			for s := 1; s <= tc.steps; s++ {
				want := 0
				if s <= tc.drop {
					want = 1
				}
				if steps[s] != want {
					t.Fatalf("step %d dispatched %d times, want %d (dispatches %v)", s, steps[s], want, steps)
				}
			}
			if n := attemptFn(h); n != tc.drop {
				t.Fatalf("fake received %d attempts, want %d: the ambiguous step must not be resent", n, tc.drop)
			}
		})
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// 7.3 test 4e: a multi-step action across several attempt windows.
func TestMultiStepActionAdvancesOnlyOnAck(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	const replyDelay = 50 * time.Millisecond
	h.f.DelayReplies("runtime:act", replyDelay)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassMutate)
	defer op.End()
	res, err := op.RunSteps(Action{Name: "type", Fn: "act", Steps: 3, Delay: 300 * time.Millisecond})
	if err != nil || res.Acked != 3 {
		t.Fatalf("RunSteps = %+v, %v", res, err)
	}
	d := h.rt.Dispatched()
	if len(d) != 3 || d[0].Step != 1 || d[1].Step != 2 || d[2].Step != 3 {
		t.Fatalf("dispatches = %+v, want steps 1, 2, 3 once each in order", d)
	}
	for i := 1; i < 3; i++ {
		if gap := d[i].At.Sub(d[i-1].At); gap < replyDelay+300*time.Millisecond-5*time.Millisecond {
			t.Fatalf("step %d arrived %s after step %d, before the reply and the delay", i+1, gap, i)
		}
	}
	if n := attemptFn(h); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

// 7.3 test 4f: cancel between steps.
func TestCancelBetweenStepsSendsNoLaterStep(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	opCtx, cancel := context.WithCancel(ctx)
	op, err := h.s.Begin(opCtx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	acked := make(chan StepsResult, 1)
	go func() {
		res, err := op.RunSteps(Action{Name: "type", Fn: "act", Steps: 3, Delay: 300 * time.Millisecond})
		acked <- res
		errc <- err
	}()
	waitUntil(t, "step 1", func() bool { return len(h.rt.Dispatched()) == 1 })
	time.Sleep(50 * time.Millisecond) // the reply of step 1 has arrived; step 2 waits for the delay
	cancel()
	err = <-errc
	op.End()
	if res := <-acked; res.Acked != 1 || !strings.Contains(err.Error(), "stopped before step 2 of 3") {
		t.Fatalf("result %+v, err %v; want step 1 acknowledged and a stop before step 2", res, err)
	}
	time.Sleep(400 * time.Millisecond)
	for _, c := range h.rt.CallsOf("act") {
		var a AttemptArgs
		_ = json.Unmarshal(c.Args, &a)
		if a.Step != 1 {
			t.Fatalf("attempt for step %d sent after the cancel", a.Step)
		}
	}
}
