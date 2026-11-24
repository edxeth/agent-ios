package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
)

// While an owner reserves the simulator, its pages are that owner's run:
// another owner's tab list hides them and its page commands fail with exit
// 6; the owner itself, and everyone once the reservation ends, use them.
// A reservation of every physical device (*) does not take the simulator.
func TestSimulatorPagesBelongToTheReservingOwner(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, core.SimulatorDevice
	f.add("Shop", "http://10.0.2.2:18080/")
	if _, err := res.Reserve(core.AnyDevice, "phone-agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	if out := mustIn(t, m, "b", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("a reservation of every physical device hid the simulator:\n%s", out)
	}
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	r, err := runIn(t, m, "b", "tab", "list")
	if err != nil || strings.Contains(r.Text, "Shop") || r.Data.(map[string]any)["hidden"] != 1 {
		t.Fatalf("another owner's tab list: %q, %v, %v", r.Text, r.Data, err)
	}
	if _, err := runIn(t, m, "b", "eval", "--page", "t1", "1+1"); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
		t.Fatalf("another owner's page command: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
	if out := mustIn(t, m, "a", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("the owner's tab list:\n%s", out)
	}
	if _, err := runIn(t, m, "a", "eval", "--page", "t1", "1+1"); err != nil {
		t.Fatalf("the owner's page command: %v", err)
	}
	if err := res.Release(core.SimulatorDevice, "a"); err != nil {
		t.Fatal(err)
	}
	if out := mustIn(t, m, "b", "tab", "list"); !strings.Contains(out, "t1") {
		t.Fatalf("after the release:\n%s", out)
	}
}

func withSimReservations(t *testing.T) *core.Reservations {
	t.Helper()
	res := &core.Reservations{Dir: t.TempDir()}
	old, oldPoll := SimReservations, simReservePoll
	SimReservations = func() (*core.Reservations, error) { return res, nil }
	simReservePoll = 10 * time.Millisecond
	t.Cleanup(func() { SimReservations, simReservePoll = old, oldPoll })
	return res
}

func simInput(owner string, wait time.Duration) Input {
	return Input{Flags: map[string]any{"owner": owner, "ttl": 30 * time.Minute, "wait": wait}}
}

// sim reserve takes a free simulator at once, waits while another owner
// holds it and takes it when that owner releases it, and after --wait
// fails with exit 3 and a message that says to run it again.
func TestSimReserveWaitsForTheOtherOwner(t *testing.T) {
	res := withSimReservations(t)
	ctx := context.Background()
	if _, err := runSimReserve(ctx, nil, simInput("a", 0)); err != nil {
		t.Fatal(err)
	}
	_, err := runSimReserve(ctx, nil, simInput("b", 50*time.Millisecond))
	if ExitCodeOf(err) != ExitWaitTimeout || !strings.Contains(err.Error(), "'iwdp-cli sim reserve' again") {
		t.Fatalf("a held simulator: %v (exit %d), want exit 3 that says to run it again", err, ExitCodeOf(err))
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = res.Release(core.SimulatorDevice, "a")
	}()
	r, err := runSimReserve(ctx, nil, simInput("b", 5*time.Second))
	if err != nil || !r.Data.(simReservation).Reserved {
		t.Fatalf("after a's release: %+v, %v", r, err)
	}
	if mine, _, _ := res.Check(core.SimulatorDevice, "b"); mine == nil {
		t.Fatal("b holds no reservation after sim reserve")
	}
}

// simGate refuses another owner's sim command while the simulator is
// reserved, renews the owner's own reservation, and admits everyone on a
// free simulator.
func TestSimGate(t *testing.T) {
	res := withSimReservations(t)
	if err := simGate(simInput("x", 0)); err != nil {
		t.Fatalf("free simulator: %v", err)
	}
	r, err := res.Reserve(core.SimulatorDevice, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := simGate(simInput("b", 0)); ExitCodeOf(err) != ExitReservation {
		t.Fatalf("another owner: %v, want exit 6", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := simGate(simInput("a", 0)); err != nil {
		t.Fatal(err)
	}
	mine, _, _ := res.Check(core.SimulatorDevice, "a")
	if mine == nil || !mine.Expires.After(r.Expires) {
		t.Fatalf("the owner's sim command did not renew its reservation: %+v (was until %s)", mine, r.Expires)
	}
}

// In private mode the turn-taking errors still tell the agent what to do.
func TestPrivateSimReservationLines(t *testing.T) {
	if l := privateErrorLine("sim reserve", ExitWaitTimeout); !strings.Contains(l, "run 'iwdp-cli sim reserve' again") {
		t.Errorf("sim reserve exit 3: %q", l)
	}
	if l := privateErrorLine("sim tap", ExitReservation); !strings.Contains(l, "reserved by another agent") {
		t.Errorf("sim tap exit 6: %q", l)
	}
}
