package daemon

import (
	"context"
	"errors"
	"os"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// The daemon's part of the sim commands (internal/commands/sim.go): the
// forward's record goes in the daemon's private directory, and a restart of
// iwdp counts the daemon's active leases first.
func init() {
	commands.SimStateDir = simDir
	commands.SimReservations = func() (*core.Reservations, error) {
		dir, err := simDir()
		if err != nil {
			return nil, err
		}
		return &core.Reservations{Dir: ReservationsDir(dir)}, nil
	}
	commands.ActiveLeases = func(ctx context.Context, env commands.Env) (int, error) {
		c, err := connectRunning(ctx, env)
		if errors.Is(err, ErrNotRunning) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		defer c.Close()
		st, err := c.Status(ctx)
		if notRunning(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		return activeLeases(st), nil
	}
}

// activeLeases counts the pages of st whose lease is active: on every
// device, since a restart of iwdp ends every page connection.
func activeLeases(st Status) int {
	n := 0
	for _, p := range st.Pages {
		if p.Lease == "active" {
			n++
		}
	}
	return n
}

// simDir is the daemon's directory, as the daemon picks it (RunOptions):
// the forward's record and the reservations live there, so the sim
// commands and the daemon's reservation checks share them.
func simDir() (string, error) {
	dir := os.Getenv(dirEnv)
	if dir == "" {
		dir = SocketDir()
	}
	if err := EnsureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}
