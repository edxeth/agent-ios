package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Final review F2: the sweep fences every lease that it ends before it runs
// any page's cleanup, and one page's slow cleanup does not delay another
// page's lease end. fenceRig has two phone pages leased under the phone's
// reservation and a simulator page leased by "old"; the phone pages stall
// the runtime release (a cleanup that waits its whole 1 s budget), and
// their keys sort before the simulator's, so a serial sweep reaches the
// simulator page only after both stalled cleanups.
type fenceRig struct {
	*rig
	phones []*Page
	sim    *Page
}

func newFenceRig(t *testing.T, mo ManagerOptions) *fenceRig {
	t.Helper()
	r := &fenceRig{rig: newRig(t, fakewebkit.Options{}, false, mo)}
	ctx := ctxFor(t, 30*time.Second)
	r.reserve(t, "phone")
	fakes := []*fakewebkit.Server{r.f}
	for i := 1; i < 2; i++ {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		r.dev.AddPage(fmt.Sprintf("phone%d", i), "http://phone.test/", f.URL)
		fakes = append(fakes, f)
	}
	for _, f := range fakes {
		req := r.req
		req.Page = f.URL
		pg, _, err := r.m.Touch(ctx, req, AcquireOptions{Session: "phone", Reservation: "phone"})
		if err != nil {
			t.Fatal(err)
		}
		r.phones = append(r.phones, pg)
	}
	sf := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(sf.Close)
	r.l.AddDevice(SimulatorDevice, "Simulator", "").AddPage("Sim", "http://sim.test/", sf.URL)
	sr := r.req
	sr.Page = sf.URL
	sp, _, err := r.m.Touch(ctx, sr, AcquireOptions{Session: "old"})
	if err != nil {
		t.Fatal(err)
	}
	r.sim = sp
	for _, pg := range r.phones {
		if pg.Key >= sp.Key {
			t.Fatalf("phone page %q does not sort before the simulator page %q", pg.Key, sp.Key)
		}
	}
	for _, f := range fakes {
		f.Stall("Runtime.evaluate")
	}
	return r
}

// phonesHeld counts the phone pages whose lease is still held.
func (r *fenceRig) phonesHeld() int {
	n := 0
	for _, pg := range r.phones {
		if _, _, ok := r.m.LeaseOf(pg.Key); ok {
			n++
		}
	}
	return n
}

// waitSimFenced waits, until the test's deadline, for the simulator page's
// lease to end, and returns how many phone leases were still held then.
func (r *fenceRig) waitSimFenced(t *testing.T) int {
	t.Helper()
	for {
		held := r.phonesHeld()
		if _, _, ok := r.m.LeaseOf(r.sim.Key); !ok {
			return held
		}
		if dl, ok := t.Deadline(); ok && time.Until(dl) < time.Second {
			t.Fatal("the simulator page's lease never ended")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// One Sweep: the simulator lease ends while the phones' stalled cleanups
// still run, instead of after them.
func TestSweepFencesEveryPageBeforeSlowCleanups(t *testing.T) {
	r := newFenceRig(t, ManagerOptions{SweepEvery: time.Hour})
	if err := r.res.Release(r.dev.ID, "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.res.Reserve(SimulatorDevice, "new", time.Hour); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.m.Sweep(time.Now()); close(done) }()
	if held := r.waitSimFenced(t); held == 0 {
		t.Fatal("the simulator lease ended only after the phones' stalled cleanups: the sweep fenced it behind them")
	}
	<-done
	if held := r.phonesHeld(); held != 0 {
		t.Fatalf("after the sweep %d phone leases are still held", held)
	}
}

// The periodic sweep: a simulator turn that changes while an earlier
// sweep's stalled cleanup runs is fenced at the next tick, not after that
// cleanup ends.
func TestSweeperFencesWhileAnotherCleanupStalls(t *testing.T) {
	r := newFenceRig(t, ManagerOptions{SweepEvery: 50 * time.Millisecond})
	if err := r.res.Release(r.dev.ID, "phone"); err != nil {
		t.Fatal(err)
	}
	// Wait until the sweeper releases the phone pages.
	for {
		r.phones[0].mu.Lock()
		releasing := r.phones[0].releasing
		r.phones[0].mu.Unlock()
		if releasing {
			break
		}
		if dl, ok := t.Deadline(); ok && time.Until(dl) < time.Second {
			t.Fatal("the sweeper never released the phone page")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := r.res.Reserve(SimulatorDevice, "new", time.Hour); err != nil {
		t.Fatal(err)
	}
	if held := r.waitSimFenced(t); held == 0 {
		t.Fatal("the simulator lease ended only after the phones' stalled cleanups: the sweeper waited for them before its next sweep")
	}
}
