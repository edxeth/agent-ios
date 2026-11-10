package core

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

const ms = time.Millisecond

func TestClockMapBoundsOfOneSample(t *testing.T) {
	var m ClockMap
	m.Observe(ClockSample{H0: 999 * ms, H1: 1000 * ms, P: 1000 * ms})
	// Design 2.6: pageMin(T) = p + (1 - rho)(T - h1); rho = 100 ppm.
	if got, want := m.PageMin(1250*ms), 1249975*time.Microsecond; got != want {
		t.Fatalf("PageMin(1250 ms) = %s, want %s", got, want)
	}
	if got, want := m.PageMin(1100*ms), 1099990*time.Microsecond; got != want {
		t.Fatalf("PageMin(1100 ms) = %s, want %s", got, want)
	}
	// Uncertainty: (h1 - h0) + rho(2T - h0 - h1) = 1 ms + 0.0501 ms at T = 1250 ms.
	if got, want := m.Uncertainty(1250*ms), 1050100*time.Nanosecond; got != want {
		t.Fatalf("Uncertainty(1250 ms) = %s, want %s", got, want)
	}
}

func TestClockMapUsesTightestSamples(t *testing.T) {
	var m ClockMap
	m.Observe(ClockSample{H0: 0, H1: 80 * ms, P: 5040 * ms})         // wide
	m.Observe(ClockSample{H0: 100 * ms, H1: 102 * ms, P: 5101 * ms}) // narrow
	m.Observe(ClockSample{H0: 200 * ms, H1: 260 * ms, P: 5230 * ms}) // wide again
	lo, hi := m.PageMin(300*ms), m.PageMax(300*ms)
	if want := 5101*ms + 198*ms - drift(198*ms); lo != want {
		t.Fatalf("PageMin = %s, want the bound of the narrow sample %s", lo, want)
	}
	if want := 5101*ms + 200*ms + drift(200*ms); hi != want {
		t.Fatalf("PageMax = %s, want the bound of the narrow sample %s", hi, want)
	}
}

// clockWorld is a generated true page clock and measurements of it: the
// page clock reads offset + rate*daemon, with rate within rho of 1, and each
// sample reads it at some daemon instant between its h0 and h1.
type clockWorld struct {
	Offset  time.Duration
	Rate    float64
	Samples []ClockSample
	Later   time.Duration // a daemon instant at or after every sample
}

func (w clockWorld) page(d time.Duration) time.Duration {
	return w.Offset + time.Duration(float64(d)*w.Rate)
}

func (clockWorld) Generate(r *rand.Rand, _ int) reflect.Value {
	w := clockWorld{
		Offset: time.Duration(r.Int63n(int64(10*time.Hour))) - 5*time.Hour,
		Rate:   1 + (2*r.Float64()-1)*0.99e-4, // strictly inside rho
	}
	h := time.Duration(r.Int63n(int64(time.Hour)))
	for i := 0; i < 1+r.Intn(5); i++ {
		h += time.Duration(r.Int63n(int64(time.Minute)))
		h0 := h
		h += time.Duration(r.Int63n(int64(200 * time.Millisecond)))
		read := h0 + time.Duration(r.Int63n(int64(h-h0)+1))
		w.Samples = append(w.Samples, ClockSample{H0: h0, H1: h, P: w.page(read)})
	}
	w.Later = h + time.Duration(r.Int63n(int64(time.Hour)))
	return reflect.ValueOf(w)
}

// Design 2.6, clock bounds: for every page clock whose rate is within rho
// and every set of measurements of it, the page value at a later daemon
// instant lies between PageMin and PageMax, so attemptEnd = PageMin(H) is
// reached no later than H. One nanosecond of slack covers the rounding of
// the generated page reads.
func TestClockMapBoundsHoldForEveryClockWithinRho(t *testing.T) {
	prop := func(w clockWorld) bool {
		var m ClockMap
		for _, s := range w.Samples {
			m.Observe(s)
		}
		truth := w.page(w.Later)
		return m.PageMin(w.Later) <= truth+1 && truth <= m.PageMax(w.Later)+1
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatal(err)
	}
}

type nopCaller struct{}

func (nopCaller) Send(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (nopCaller) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (nopCaller) IsTargetRouted() bool                         { return true }
func (nopCaller) Done() <-chan struct{}                        { return nil }

// simPage simulates the page and the network on the manual clock: the
// page reads its clock readAfter after a measurement is sent, measurement
// replies arrive rtt after the send, attempts arrive delivery late, and
// attempt replies can be lost.
type simPage struct {
	clock     *ManualClock
	rt        *fakewebkit.PageRuntime
	page      func(daemon time.Duration) time.Duration
	rtts      []time.Duration // per measurement; the last one repeats
	readAfter func(rtt time.Duration) time.Duration
	delivery  time.Duration
	drop      bool

	mu       sync.Mutex
	measures int
	attempts []AttemptArgs
}

func (p *simPage) value(v any) (json.RawMessage, error) {
	env := v.(map[string]any)
	return json.Marshal(env["value"])
}

func (p *simPage) call(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
	// The write of the runtime call: the caller's fence decides whether it
	// happens.
	if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(args)
	switch fn {
	case ClockFn:
		p.mu.Lock()
		rtt := p.rtts[min(p.measures, len(p.rtts)-1)]
		p.measures++
		p.mu.Unlock()
		h0 := p.clock.Now()
		v := p.rt.HandleAt(p.page(h0+p.readAfter(rtt)), fn, raw)
		p.clock.Set(h0 + rtt)
		return p.value(v)
	case "cancel":
		return p.value(p.rt.HandleAt(p.page(p.clock.Now()+p.delivery), fn, raw))
	}
	var a AttemptArgs
	_ = json.Unmarshal(raw, &a)
	p.mu.Lock()
	p.attempts = append(p.attempts, a)
	p.mu.Unlock()
	v := p.rt.HandleAt(p.page(p.clock.Now()+p.delivery), fn, raw)
	if p.drop {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.value(v)
}

func (p *simPage) attemptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.attempts)
}

type clockRun struct {
	args       AttemptArgs
	sentAt     time.Duration
	admittedAt time.Duration
	events     []time.Duration // daemon times at which predecessor events ran
	dispatches []time.Duration // page times of the page's dispatches
}

// runFenced runs one attempt whose reply and cancel are lost, cancels it at
// cancelAt (0: let the deadline expire), queues a successor, and moves the
// clock to the boundary. It returns what the daemon sent and when the
// successor was admitted.
func runFenced(t *testing.T, clock *ManualClock, p *simPage, deadline, cancelAt, boundary time.Duration) clockRun {
	t.Helper()
	p.rt.IgnoreCancel = true
	p.rt.KeepOpen = true // the predecessor's call never settles: only the clock bounds stop its events
	p.drop = true
	var run clockRun
	s := NewScheduler(nopCaller{}, SchedulerOptions{Clock: clock, Runtime: p.call, OnAttempt: func(e AttemptEvent) { run.sentAt = e.SentAt }})
	defer s.Close()
	s.SetLease(testSession, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := a.RunSteps(Action{Name: "click", Fn: "act", Steps: 1}); errc <- err }()
	waitUntil(t, "the attempt", func() bool { return p.attemptCount() == 1 })
	run.args = p.attempts[0]
	admitted := make(chan *Op, 1)
	go func() {
		op, err := s.Begin(context.Background(), OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: deadline + time.Hour})
		if err != nil {
			t.Error(err)
		}
		admitted <- op
	}()
	end := func() {
		if err := <-errc; !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("RunSteps err = %v, want canceled; outcome unknown", err)
		}
		a.End()
	}
	if cancelAt > 0 {
		clock.Set(cancelAt)
		a.Cancel(errors.New("client went away"))
		end()
	}
	clock.Set(boundary - time.Microsecond)
	select {
	case <-admitted:
		t.Fatalf("successor admitted before the slot-release boundary %s", boundary)
	case <-time.After(30 * time.Millisecond):
	}
	clock.Set(boundary)
	if cancelAt == 0 {
		end() // the deadline expired at the boundary
	}
	succ := <-admitted
	run.admittedAt = succ.AdmittedAt
	succ.End()
	// Later events of the predecessor's step, tried every 10 ms of daemon time.
	for d := run.sentAt; d <= boundary+100*ms; d += 10 * ms {
		if p.rt.TryEvent(a.ID, p.page(d)) {
			run.events = append(run.events, d)
		}
	}
	for _, d := range p.rt.Dispatched() {
		run.dispatches = append(run.dispatches, d.PageNow)
	}
	return run
}

// 7.3 test 4h: page clock with a 5 s offset, 80 ms round trip, attempts
// delivered 200 ms late, reply and cancel lost, successor waiting.
func TestFencingWithOffsetClockAndLateDelivery(t *testing.T) {
	scenario := func(wall time.Time) clockRun {
		clock := NewManualClock(0)
		rt := fakewebkit.NewPageRuntime()
		rt.WallClock = wall
		p := &simPage{
			clock: clock, rt: rt, delivery: 200 * ms,
			page:      func(d time.Duration) time.Duration { return d + 5*time.Second },
			rtts:      []time.Duration{80 * ms},
			readAfter: func(rtt time.Duration) time.Duration { return rtt / 2 },
		}
		run := runFenced(t, clock, p, 10*time.Second, 150*ms, 80*ms+DefaultAttemptBudget)
		if len(rt.Dispatched()) == 0 || len(run.events) == 0 {
			t.Fatal("the late attempt or its later events did not run; the scenario does not exercise the fence")
		}
		return run
	}
	run := scenario(time.Unix(0, 0))
	boundary := run.sentAt + DefaultAttemptBudget
	if run.sentAt != 80*ms {
		t.Fatalf("attempt sent at %s, want 80 ms (after the measurement)", run.sentAt)
	}
	// Worst-case offsets of the measurement (p read at h0 or at h1) and the
	// slowest rate: the page reaches attemptEnd no later than the boundary.
	p, h0, h1 := 5040.0, 0.0, 80.0 // ms
	for _, read := range []float64{h0, h1} {
		// Slowest page that read p at daemon time read: page(T) = p + (1-rho)(T - read).
		reach := read + (run.args.AttemptEnd-p)/(1-1e-4)
		if reach > toMS(boundary)+1e-9 {
			t.Fatalf("attemptEnd %v is reached at daemon %v ms when p was read at %v ms, after the boundary %s", run.args.AttemptEnd, reach, read, boundary)
		}
	}
	if run.admittedAt != boundary {
		t.Fatalf("successor admitted at %s, want the boundary %s", run.admittedAt, boundary)
	}
	for _, e := range run.events {
		if e >= run.admittedAt {
			t.Fatalf("predecessor event ran at daemon %s, after the successor's admission at %s", e, run.admittedAt)
		}
	}
	// Moving the page's wall clock by one hour changes nothing.
	if moved := scenario(time.Unix(3600, 0)); !reflect.DeepEqual(moved, run) {
		t.Fatalf("wall clock change altered the run:\n%+v\n%+v", moved, run)
	}
}

// 7.3 test 4h: worst-rate slower clock; attemptEnd and deadline projected
// through their own instants.
func TestFencingWithWorstRateSlowerClock(t *testing.T) {
	const rho = 1e-4
	pageAt := func(d time.Duration) time.Duration {
		// The page clock runs at exactly 1 - rho of the daemon rate and reads 1000 ms at daemon 1000 ms.
		return 1000*ms + time.Duration(float64(d-1000*ms)*(1-rho))
	}
	newSim := func(clock *ManualClock) *simPage {
		return &simPage{
			clock: clock, rt: fakewebkit.NewPageRuntime(), page: pageAt,
			rtts:      []time.Duration{1 * ms},
			readAfter: func(rtt time.Duration) time.Duration { return rtt }, // p read at h1
		}
	}

	clock := NewManualClock(999 * ms)
	p := newSim(clock)
	run := runFenced(t, clock, p, 10*time.Second, 1100*ms, 1250*ms)
	if run.sentAt != 1000*ms {
		t.Fatalf("attempt sent at %s, want s = h1 = 1000 ms", run.sentAt)
	}
	if run.args.AttemptEnd != 1249.975 {
		t.Fatalf("attemptEnd = %v, want pageMin(1250 ms) = 1249.975", run.args.AttemptEnd)
	}
	if run.admittedAt != 1250*ms {
		t.Fatalf("successor admitted at %s, want H = 1250 ms", run.admittedAt)
	}
	for _, d := range []time.Duration{1250 * ms, 1250*ms + time.Microsecond} {
		if p.rt.TryEvent("op-1", pageAt(d)) {
			t.Fatalf("predecessor event at daemon %s ran; want it refused as expired", d)
		}
	}
	for _, e := range run.events {
		if e >= run.admittedAt {
			t.Fatalf("predecessor event at daemon %s, after the successor's admission", e)
		}
	}

	// Second run: the deadline D = 1100 ms comes before s + 250 ms, so H = D.
	clock = NewManualClock(999 * ms)
	p = newSim(clock)
	run = runFenced(t, clock, p, 1100*ms, 0, 1100*ms)
	if run.args.Deadline != 1099.99 || run.args.AttemptEnd != 1099.99 {
		t.Fatalf("deadline %v, attemptEnd %v; want both pageMin(1100 ms) = 1099.99", run.args.Deadline, run.args.AttemptEnd)
	}
	if run.admittedAt != 1100*ms {
		t.Fatalf("successor admitted at %s, want D = 1100 ms", run.admittedAt)
	}
	if p.rt.TryEvent("op-1", pageAt(1100*ms)) {
		t.Fatal("the page ran a predecessor event at D")
	}
}

// 7.3 test 4h: refresh or fail closed.
func TestClockUncertaintyRefreshThenFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rtts     []time.Duration
		attempts int
	}{
		{"still uncertain", []time.Duration{150 * ms}, 0},
		{"refresh helps", []time.Duration{150 * ms, 20 * ms}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := NewManualClock(0)
			p := &simPage{
				clock: clock, rt: fakewebkit.NewPageRuntime(), rtts: tc.rtts,
				page:      func(d time.Duration) time.Duration { return d },
				readAfter: func(rtt time.Duration) time.Duration { return rtt / 2 },
			}
			s := NewScheduler(nopCaller{}, SchedulerOptions{Clock: clock, Runtime: p.call})
			defer s.Close()
			s.SetLease(testSession, 1)
			op, err := s.Begin(context.Background(), OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer op.End()
			_, err = op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
			if p.measures != 2 {
				t.Fatalf("measurements = %d, want 2 (the first and one refresh)", p.measures)
			}
			if p.attemptCount() != tc.attempts {
				t.Fatalf("attempts = %d, want %d", p.attemptCount(), tc.attempts)
			}
			if tc.attempts == 0 {
				var ce *ClockUncertaintyError
				if !errors.As(err, &ce) || !errors.Is(err, ErrClockUncertain) {
					t.Fatalf("err = %v, want the clock uncertainty error", err)
				}
				if want := "page clock uncertainty is 150 ms; the page is too slow to fence actions safely"; ce.Error() != want {
					t.Fatalf("message = %q, want %q", ce.Error(), want)
				}
			} else if err != nil {
				t.Fatalf("RunSteps: %v", err)
			}
		})
	}
}
