package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// probePage is a fake page for actions with a probe (Action.Probe): its
// clock is the time since start, the probe answers ready after probeDelay,
// and the probed call records how much of its attempt window was left
// when it arrived.
type probePage struct {
	t0         time.Time
	probeDelay time.Duration
	mu         sync.Mutex
	calls      []string
	leftAtCall []time.Duration // AttemptEnd minus the page clock at receipt
}

func (p *probePage) now() float64 { return toMS(time.Since(p.t0)) }

func (p *probePage) runtime(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
	if fn == ClockFn {
		return json.Marshal(map[string]any{"now": p.now(), "doc": "d1"})
	}
	a, ok := args.(AttemptArgs)
	if !ok {
		return json.RawMessage("null"), nil // __iwdp.cancel
	}
	if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, fn)
	p.mu.Unlock()
	if fn == "probe" {
		time.Sleep(p.probeDelay)
		return json.Marshal(AttemptReply{Status: StatusRetry, Reason: ProbeReady, Now: p.now(), Doc: a.Doc})
	}
	p.mu.Lock()
	p.leftAtCall = append(p.leftAtCall, fromMS(a.AttemptEnd-p.now()))
	p.mu.Unlock()
	return json.Marshal(AttemptReply{Status: StatusDispatched, Now: p.now(), Doc: a.Doc})
}

func runProbed(t *testing.T, p *probePage, deadline time.Duration) error {
	t.Helper()
	s := NewScheduler(nopCaller{}, SchedulerOptions{Runtime: p.runtime})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	t.Cleanup(cancel)
	op, err := s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	_, err = op.RunSteps(Action{Name: "click --new-tab", Fn: "gesture", Probe: "probe", Steps: 1})
	return err
}

// W3b-R2 finding 6 (a), orchestrator decision: the call after a ready probe
// (the user-gesture call of click --new-tab) gets an attempt window of at
// least 3 times the measured round trip, here the probe's 150 ms, so a
// call delivered as slowly as the probe still lands well inside it. The
// plain 250 ms window would leave at most 250 ms.
func TestProbedCallWindowCoversThreeRoundTrips(t *testing.T) {
	const probeDelay = 150 * time.Millisecond
	p := &probePage{t0: time.Now(), probeDelay: probeDelay}
	if err := runProbed(t, p, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 2 || p.calls[0] != "probe" || p.calls[1] != "gesture" {
		t.Fatalf("calls %v, want the probe, then the gesture call", p.calls)
	}
	// The page reads its clock a moment after the send, and the window's
	// page end is projected conservatively; 3 round trips of at least
	// 150 ms leave well over 300 ms.
	if left := p.leftAtCall[0]; left < 3*probeDelay-150*time.Millisecond || left <= DefaultAttemptBudget {
		t.Fatalf("the gesture call arrived with %v of its window left, want about 3 x %v, more than the plain %v", left, probeDelay, DefaultAttemptBudget)
	}
}

// W3b-R2 finding 6 (b): when the time left cannot cover that window, the
// call is not sent; the action fails at once with the reason, and nothing
// is ambiguous.
func TestProbedCallIsNotSentWithoutTimeForItsWindow(t *testing.T) {
	p := &probePage{t0: time.Now(), probeDelay: 250 * time.Millisecond} // window 750 ms
	start := time.Now()
	err := runProbed(t, p, 700*time.Millisecond)
	var we *ProbedWindowError
	var se *StepError
	if !errors.As(err, &we) || !errors.Is(err, ErrActionTimeout) || !errors.As(err, &se) || se.MayHaveRun {
		t.Fatalf("err = %v, want a ProbedWindowError before dispatch", err)
	}
	if we.Window < 3*250*time.Millisecond || we.Left >= we.Window || time.Since(start) > 600*time.Millisecond {
		t.Fatalf("window %v, left %v after %v; want a window of 3 round trips, more than the time left, failing at once", we.Window, we.Left, time.Since(start))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		if c == "gesture" {
			t.Fatalf("the gesture call was sent: %v", p.calls)
		}
	}
}

// refreshPage is a fake page on a ManualClock for the clock refresh of a
// probed call (W3b-R3 finding 1, from the actions review's reproduction
// window_refresh_test.go): the probe takes 110 ms, so the gesture window
// starts at 3 x 110 ms; the projected uncertainty of that window is above
// the 100 ms limit, so the attempt measures the page clock again, and that
// measurement takes 150 ms, so the window must grow to 3 x 150 ms.
type refreshPage struct {
	clock        *ManualClock
	cm           *ClockMap
	onMeasure    func(h0, h1 time.Duration) time.Duration // page clock of the refresh; nil: 300 ms
	measures     int
	gestures     int
	rttAtGesture time.Duration
}

const (
	refreshProbeRTT   = 110 * time.Millisecond
	refreshMeasureRTT = 150 * time.Millisecond
)

func (p *refreshPage) runtime(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
	if fn == ClockFn {
		p.measures++
		h0 := p.clock.Now()
		h1 := h0 + refreshMeasureRTT
		pageNow := 300 * time.Millisecond
		if p.onMeasure != nil {
			pageNow = p.onMeasure(h0, h1)
		}
		p.clock.Set(h1)
		return json.Marshal(map[string]any{"now": toMS(pageNow), "doc": "d"})
	}
	a, ok := args.(AttemptArgs)
	if !ok {
		return json.RawMessage("null"), nil // __iwdp.cancel
	}
	if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
		return nil, err
	}
	if fn == "probe" {
		readAt := p.clock.Now()
		p.clock.Set(readAt + refreshProbeRTT)
		return json.Marshal(AttemptReply{Status: StatusRetry, Reason: ProbeReady, Now: toMS(readAt), Doc: a.Doc})
	}
	p.gestures++
	p.rttAtGesture = p.cm.RoundTrip()
	return json.Marshal(AttemptReply{Status: StatusDispatched, Now: toMS(p.clock.Now()), Doc: a.Doc})
}

// runRefresh runs one click --new-tab action on p whose operation has
// deadline left on p's clock. The mapping starts with one sample of a
// round trip just under 100 ms, so the probe needs no refresh and the
// gesture call does.
func runRefresh(t *testing.T, p *refreshPage, deadline time.Duration) (*Op, error) {
	t.Helper()
	const initial = 99930 * time.Microsecond
	p.clock = NewManualClock(initial)
	s := NewScheduler(nopCaller{}, SchedulerOptions{Clock: p.clock, Runtime: p.runtime})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)
	p.cm = s.clockMap(clockKey{gen: 1})
	p.cm.BindDoc("d")
	p.cm.Observe(ClockSample{H0: 0, H1: initial, P: 0})
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	t.Cleanup(cancel)
	op, err := s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	_, err = op.RunSteps(Action{Name: "click --new-tab", Fn: "gesture", Probe: "probe", Steps: 1})
	if p.measures != 1 {
		t.Fatalf("the page clock was measured %d times, want 1 refresh for the gesture call", p.measures)
	}
	return op, err
}

// W3b-R3 finding 1, deadline fit (orchestrator decision for the gesture
// window: at least 3 times the latest measured round trip, and nothing is
// sent when the time left cannot cover it): after the refresh the window
// is 450 ms and about 340 ms are left, so the gesture call is not sent.
func TestProbedCallWindowFollowsTheRefreshedRoundTrip(t *testing.T) {
	p := &refreshPage{}
	op, err := runRefresh(t, p, 600*time.Millisecond)
	var we *ProbedWindowError
	if p.gestures != 0 || !errors.As(err, &we) {
		t.Fatalf("sent %d gesture call(s) with RTT %v, err = %v; the refreshed round trip of %v needs a %v window, but only %v remained",
			p.gestures, p.rttAtGesture, err, refreshMeasureRTT, 3*refreshMeasureRTT, op.deadline-p.clock.Now())
	}
	if we.RoundTrip != refreshMeasureRTT || we.Window != 3*refreshMeasureRTT || we.Left >= we.Window {
		t.Fatalf("window error %+v, want the window of the refreshed round trip %v", we, refreshMeasureRTT)
	}
}

// W3b-R3 finding 1, projected uncertainty (design 2.6: an attempt is sent
// only while the clock uncertainty at its boundary is within the limit):
// the refresh leaves an uncertainty within the limit at the end of the old
// 330 ms window but above it at the end of the 450 ms window that the
// refreshed round trip requires, so the gesture call is not sent. The page
// clock of the refresh is chosen for that, on a copy of the mapping.
func TestProbedCallUncertaintyIsCheckedForTheRefreshedWindow(t *testing.T) {
	p := &refreshPage{}
	var found bool
	p.onMeasure = func(h0, h1 time.Duration) time.Duration {
		p.cm.mu.Lock()
		prior := slices.Clone(p.cm.samples)
		p.cm.mu.Unlock()
		oldEnd, newEnd := h1+probedWindowFactor*refreshProbeRTT, h1+probedWindowFactor*refreshMeasureRTT
		for pageNow := h0 - 300*time.Millisecond; pageNow < h1+300*time.Millisecond; pageNow += time.Microsecond {
			m := &ClockMap{samples: slices.Clone(prior)}
			m.Observe(ClockSample{H0: h0, H1: h1, P: pageNow})
			if m.Uncertainty(oldEnd) <= DefaultMaxUncertainty && m.Uncertainty(newEnd) > DefaultMaxUncertainty {
				found = true
				return pageNow
			}
		}
		return 300 * time.Millisecond
	}
	_, err := runRefresh(t, p, 2*time.Second)
	if !found {
		t.Fatal("no page clock value separates the uncertainty of the two windows; the test setup is wrong")
	}
	var ue *ClockUncertaintyError
	if p.gestures != 0 || !errors.As(err, &ue) || ue.Uncertainty <= DefaultMaxUncertainty {
		t.Fatalf("sent %d gesture call(s), err = %v; want a clock uncertainty error for the %v window and no send",
			p.gestures, err, probedWindowFactor*refreshMeasureRTT)
	}
}
