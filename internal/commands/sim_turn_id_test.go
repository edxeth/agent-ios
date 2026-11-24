package commands

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// gateWith points simGate at res and sets its hold interval for the test.
func gateWith(t *testing.T, res *core.Reservations, every time.Duration) {
	t.Helper()
	oldR, oldH := SimReservations, simHoldEvery
	SimReservations = func() (*core.Reservations, error) { return res, nil }
	simHoldEvery = every
	t.Cleanup(func() { SimReservations, simHoldEvery = oldR, oldH })
}

// waitDone waits for ctx to end, at most until the test's deadline.
func waitDone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	waitUntil(t, what, func() bool { return ctx.Err() != nil })
}

// Review F1: a running sim command holds the turn it started under, not
// its owner's next one. When the owner releases the simulator and reserves
// it again while the command runs, the command stops (exit 6) and does not
// renew the new reservation.
func TestSimCommandDoesNotHoldItsOwnersNextTurn(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	gateWith(t, res, 200*time.Millisecond)
	ctx, end, err := simGate(context.Background(), simInput("a", 0))
	if err != nil {
		t.Fatal(err)
	}
	// Both before the command's first renewal, 200 ms on.
	if err := res.Release(core.SimulatorDevice, "a"); err != nil {
		t.Fatal(err)
	}
	next, err := res.Reserve(core.SimulatorDevice, "a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	renewed := func() bool {
		mine, _, err := res.Check(core.SimulatorDevice, "a")
		return err == nil && mine != nil && mine.Expires.After(next.Expires)
	}
	waitUntil(t, "the command stops or renews the new turn", func() bool { return ctx.Err() != nil || renewed() })
	var result error
	end(&result)
	if renewed() {
		t.Fatal("the command started in a's earlier turn renewed a's new reservation")
	}
	if ExitCodeOf(result) != ExitReservation || !errors.Is(result, errSimTurnLost) {
		t.Fatalf("command whose turn ended: %v (exit %d), want exit 6", result, ExitCodeOf(result))
	}
}

// Review F1: when the owner's reservation ends between the admission's
// check and its first renewal, the command is refused (exit 6) instead of
// running without a turn.
func TestSimGateFailsWhenItsFirstRenewalFindsNoTurn(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(core.SimulatorDevice, "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	var calls atomic.Int32
	// The admission's check reads the clock once and sees the turn; every
	// later read is past its end.
	res.Now = func() time.Time {
		if calls.Add(1) == 1 {
			return base
		}
		return base.Add(2 * time.Hour)
	}
	gateWith(t, res, time.Hour)
	_, end, err := simGate(context.Background(), simInput("a", 0))
	if err == nil {
		end(&err)
		t.Fatal("a sim command was admitted although its reservation ended before the first renewal")
	}
	if ExitCodeOf(err) != ExitReservation {
		t.Fatalf("first renewal found no turn: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
}

// Review F1: a command whose turn was lost while it ran fails with exit 6
// also when its own result is nil: it may have stopped early because its
// context ended.
func TestSimCommandWithLostTurnFailsEvenOnNilResult(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	gateWith(t, res, time.Millisecond)
	ctx, end, err := simGate(context.Background(), simInput("a", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve(core.SimulatorDevice, "b", time.Hour); err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, "the command's context ends after b reserved")
	var result error
	end(&result)
	if ExitCodeOf(result) != ExitReservation || !errors.Is(result, errSimTurnLost) {
		t.Fatalf("command that lost its turn and returned nil ended with %v (exit %d), want exit 6", result, ExitCodeOf(result))
	}
}

// Final review F4: a command that returns before its first renewal still
// checks its turn at completion. Its owner released the simulator and
// reserved it again (a new turn), or another owner reserved the free
// simulator: the command fails with exit 6, and the owner's new turn is
// not renewed.
func TestSimCommandChecksItsTurnAtCompletion(t *testing.T) {
	t.Run("own turn replaced", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		gateWith(t, res, time.Hour)
		_, end, err := simGate(context.Background(), simInput("a", 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Release(core.SimulatorDevice, "a"); err != nil {
			t.Fatal(err)
		}
		next, err := res.Reserve(core.SimulatorDevice, "a", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var result error
		end(&result)
		if ExitCodeOf(result) != ExitReservation || !errors.Is(result, errSimTurnLost) {
			t.Fatalf("command whose turn was replaced before it returned: %v (exit %d), want exit 6", result, ExitCodeOf(result))
		}
		if mine, _, err := res.Check(core.SimulatorDevice, "a"); err != nil || mine == nil || mine.ID != next.ID || !mine.Expires.Equal(next.Expires) {
			t.Fatalf("a's new turn after the command: %+v, %v; want it unchanged (%+v)", mine, err, next)
		}
	})
	t.Run("free simulator taken", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		gateWith(t, res, time.Hour)
		_, end, err := simGate(context.Background(), simInput("a", 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := res.Reserve(core.SimulatorDevice, "b", time.Hour); err != nil {
			t.Fatal(err)
		}
		var result error
		end(&result)
		if ExitCodeOf(result) != ExitReservation || !errors.Is(result, errSimTurnLost) {
			t.Fatalf("command on a free simulator that b reserved before it returned: %v (exit %d), want exit 6", result, ExitCodeOf(result))
		}
	})
	t.Run("turn held", func(t *testing.T) {
		res := &core.Reservations{Dir: t.TempDir()}
		if _, err := res.Reserve(core.SimulatorDevice, "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		gateWith(t, res, time.Hour)
		_, end, err := simGate(context.Background(), simInput("a", 0))
		if err != nil {
			t.Fatal(err)
		}
		var result error
		end(&result)
		if result != nil {
			t.Fatalf("command that kept its turn: %v, want success", result)
		}
	})
}
