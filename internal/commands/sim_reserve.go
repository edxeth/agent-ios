package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// Taking turns on the simulator. Several agents share one simulator, and
// Safari answers only for its visible tab, so they use it one at a time:
// sim reserve takes the simulator's reservation for an owner (waiting
// while another owner holds it), sim release closes the session and gives
// it back (in the daemon, which holds the page leases). While an owner
// holds it, other owners' page commands on the simulator are refused, its
// tabs are hidden from them, and their sim commands that act on the screen
// or the simulator are refused. Without any reservation the simulator is
// free for all, as before.

// SimReservations returns the reservations store; the daemon package sets
// it to the daemon's reservations directory.
var SimReservations = func() (*core.Reservations, error) {
	return nil, errors.New("sim: no reservations store in this build")
}

// simReservePoll is the interval between attempts of a waiting sim reserve.
var simReservePoll = 2 * time.Second

func init() {
	owner := Flag{Name: "owner", Type: FlagString, Help: "reservation owner; default --reservation (IWDP_DEVICE_RESERVATION), else the session"}
	Register(&Command{
		Name: "sim reserve", Local: true, Timeout: 11 * time.Minute,
		Summary: "Reserve the simulator for an owner, waiting up to --wait while another owner holds it; renews the owner's own reservation. " +
			"While an owner holds it, other owners' page commands on the simulator fail (exit 6), its tabs are hidden from them, and their sim commands that act on it fail. " +
			"Exit 3 when the wait ends first: the simulator is still another owner's; run the same command again (do not use the simulator meanwhile). " +
			"The reservation ends with sim release, or --ttl (2m) after the owner's last use (sim commands renew it, and an open page lease keeps renewing it). The owner must be your own name: the default session is refused.",
		Flags: []Flag{
			owner,
			{Name: "ttl", Type: FlagDuration, Default: "2m", Help: "lifetime after the owner's last use (sim commands renew it, and an open page lease keeps renewing it)"},
			{Name: "wait", Type: FlagDuration, Default: "50s", Help: "how long to wait for another owner's reservation to end, at most 10m (keep it under your tool's timeout)"},
		},
		Check: func(in Input) error {
			if w, _ := in.Flags["wait"].(time.Duration); w < 0 || w > 10*time.Minute {
				return usagef("sim reserve: --wait %s must be between 0 and 10m", w)
			}
			if ttl, _ := in.Flags["ttl"].(time.Duration); ttl <= 0 {
				return usagef("sim reserve: --ttl must be positive")
			}
			return nil
		},
		Run: runSimReserve,
	})
	Register(&Command{
		Name: "sim release", Timeout: 30 * time.Second,
		Summary: "End your turn on the simulator: close this session (release its page leases, as close does) and release the owner's reservation of the simulator. " +
			"Close your own tab first (tab close). Releasing no reservation is not an error; another owner's is refused.",
		Flags: []Flag{owner},
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			rs, err := reservationsOf(env)
			if err != nil {
				return Result{}, err
			}
			m, err := managerOf(env)
			if err != nil {
				return Result{}, err
			}
			o := reservationOwner(in)
			mine, other, err := rs.Check(core.SimulatorDevice, o)
			if err != nil {
				return Result{}, err
			}
			if mine == nil && other != nil {
				return Result{}, &core.CodedError{Code: ExitReservation, Err: fmt.Errorf("%w: the simulator is reserved by %q, not %q", core.ErrReserved, other.Owner, o)}
			}
			revokeCDP(env, in.Global.Session)
			n, err := m.ReleaseSession(ctx, in.Global.Session)
			if err != nil {
				return Result{}, fmt.Errorf("closing session %q: %w", in.Global.Session, err)
			}
			if err := rs.Release(core.SimulatorDevice, o); err != nil {
				return Result{}, err
			}
			return Result{
				Text: fmt.Sprintf("Simulator reservation of %q released; session %q closed (%d page lease(s) released).", o, in.Global.Session, n),
				Data: map[string]any{"released": true, "leases": n},
			}, nil
		},
	})
}

// simReservation is the data of sim reserve.
type simReservation struct {
	Reserved bool      `json:"reserved"`
	Owner    string    `json:"owner"`
	Expires  time.Time `json:"expires"`
	Waited   string    `json:"waited"`
}

func runSimReserve(ctx context.Context, env Env, in Input) (Result, error) {
	rs, err := SimReservations()
	if err != nil {
		return Result{}, err
	}
	o := reservationOwner(in)
	if o == "" || o == "default" {
		// Every agent that sets no session is "default": two of them would
		// share one turn, and one that forgets its release holds the
		// simulator for the others (live run, 2026-11-24).
		return Result{}, usagef("sim reserve: set IWDP_SESSION (or --owner) to a name of your own; the default session is every agent's")
	}
	ttl := in.Flags["ttl"].(time.Duration)
	wait := in.Flags["wait"].(time.Duration)
	start := time.Now()
	deadline := start.Add(wait)
	said := false
	for {
		r, err := rs.Reserve(core.SimulatorDevice, o, ttl)
		if err == nil {
			waited := time.Since(start).Round(time.Second)
			return Result{
				Text: fmt.Sprintf("Simulator reserved by %q until %s (waited %s). Release it with iwdp-cli sim release when your run ends.", o, r.Expires.Format(time.RFC3339), waited),
				Data: simReservation{Reserved: true, Owner: o, Expires: r.Expires, Waited: waited.String()},
			}, nil
		}
		if !errors.Is(err, core.ErrReserved) {
			return Result{}, err
		}
		if !said && env != nil {
			// At once, on stdout: an agent whose tool call ends before the
			// wait still reads why (live run, 2026-11-24: a 10 s tool
			// timeout cut the command, and the agent stopped). The line
			// names no owner, so it is safe in private mode.
			env.Emit(fmt.Sprintf("waiting: the simulator is in another agent's turn; this waits up to %s. If it is cut off (a tool timeout), run 'iwdp-cli sim reserve' again: waiting is normal", wait))
			said = true
		}
		if !time.Now().Before(deadline) {
			_, other, _ := rs.Check(core.SimulatorDevice, o)
			holder := "another owner"
			if other != nil {
				holder = fmt.Sprintf("%q (last used %s ago; it ends by %s at the latest)", other.Owner, time.Since(other.Expires.Add(-other.TTL)).Round(time.Second), other.Expires.Format(time.RFC3339))
			}
			return Result{}, &core.CodedError{Code: ExitWaitTimeout, Err: fmt.Errorf(
				"the simulator is still reserved by %s after waiting %s: another agent is using it. Run 'iwdp-cli sim reserve' again to keep waiting, and do not use the simulator or its pages meanwhile",
				holder, time.Since(start).Round(time.Second))}
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(min(simReservePoll, time.Until(deadline)+time.Millisecond)):
		}
	}
}

// simGate admits a sim command that acts on the simulator: it fails (exit
// 6) while another owner reserves the simulator, and renews the owner's
// own reservation.
func simGate(in Input) error {
	rs, err := SimReservations()
	if err != nil {
		return err
	}
	o := reservationOwner(in)
	mine, other, err := rs.Check(core.SimulatorDevice, o)
	if err != nil {
		return &core.CodedError{Code: ExitReservation, Err: err}
	}
	if mine != nil {
		_, err := rs.Reserve(core.SimulatorDevice, o, mine.TTL)
		return err
	}
	if other != nil {
		return &core.CodedError{Code: ExitReservation, Err: fmt.Errorf(
			"%w: the simulator is reserved by %q until %s at the latest: another agent is using it. Run 'iwdp-cli sim reserve' (it waits), then this command",
			core.ErrReserved, other.Owner, other.Expires.Format(time.RFC3339))}
	}
	return nil
}
