package daemon

import (
	"context"
	"errors"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
)

// simulatorLabel is the device label of iwdp's SIMULATOR device in the
// daemon's page status (core.PageStatus.Device).
var simulatorLabel = iwdp.Label("SIMULATOR", "SIMULATOR")

// The daemon's part of the sim commands (internal/commands/sim.go): the
// forward's record goes in the daemon's private directory, and a restart of
// iwdp counts the daemon's active leases on other devices first.
func init() {
	commands.SimStateDir = func() (string, error) {
		dir := SocketDir()
		if err := EnsureDir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	commands.ActiveLeasesElsewhere = func(ctx context.Context, env commands.Env) (int, error) {
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
		n := 0
		for _, p := range st.Pages {
			if p.Lease == "active" && p.Device != simulatorLabel {
				n++
			}
		}
		return n, nil
	}
}
