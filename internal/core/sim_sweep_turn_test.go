package core

import (
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Review F14: the sweep fences a simulator lease against the reservation
// that holds the simulator when it decides, not against one read at the
// start of the sweep. Here A hands the simulator to B (A's lease released,
// A's reservation released, B reserves and takes a lease) after the sweep
// began and before it reaches the simulator's page: B's lease stays.
func TestSweepFencesAgainstTheCurrentSimulatorHolder(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{SweepEvery: time.Hour})
	ctx := ctxFor(t, 10*time.Second)
	sf := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(sf.Close)
	sd := r.l.AddDevice(SimulatorDevice, "Simulator", "")
	sd.AddPage("simpage", "http://sim.test/", sf.URL)
	if _, err := r.res.Reserve(SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	sr := r.req
	sr.Page = "simpage"
	sp, _, err := r.m.Touch(ctx, sr, AcquireOptions{Session: "a", Reservation: "a"})
	if err != nil {
		t.Fatal(err)
	}
	sr.Page = sp.Key
	handed := false
	r.m.sweepPageHook = func(pg *Page) {
		if pg.Key != sp.Key || handed {
			return
		}
		handed = true
		if err := r.m.Release(ctx, "a", sp.Key); err != nil {
			t.Errorf("release a's lease: %v", err)
		}
		if err := r.res.Release(SimulatorDevice, "a"); err != nil {
			t.Errorf("release a's reservation: %v", err)
		}
		if _, err := r.res.Reserve(SimulatorDevice, "b", time.Hour); err != nil {
			t.Errorf("reserve for b: %v", err)
		}
		if _, _, err := r.m.Touch(ctx, sr, AcquireOptions{Session: "b", Reservation: "b"}); err != nil {
			t.Errorf("b's touch: %v", err)
		}
	}
	r.m.Sweep(time.Now())
	if !handed {
		t.Fatal("the sweep never reached the simulator's page")
	}
	if sess, _, ok := r.m.LeaseOf(sp.Key); !ok || sess != "b" {
		t.Fatalf("after the sweep the simulator page's lease is session %q (held %t), want b's: the sweep fenced the new turn against the old holder", sess, ok)
	}
}
