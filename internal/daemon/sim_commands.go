package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/edxeth/agent-ios/internal/commands"
	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
)

// The daemon's part of the sim commands (internal/commands/sim.go): the
// forward's record goes in the daemon's private directory, and a restart of
// iwdp counts the daemon's active leases first, and sim up --url prints
// the tab id that the daemon gives its new tab.
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
	commands.SimulatorTab = simulatorTab
}

// simulatorTab is commands.SimulatorTab: it runs tab list --device
// SIMULATOR in the daemon, for in's session and reservation owner, and
// returns the id of the tab whose page key is the iwdp page pageID of the
// simulator ("" when the listing does not have it). It starts the daemon
// when none runs, as the tab command that selects the tab would: the
// daemon keeps the ids for its life. Under AGENT_IOS_NO_DAEMON=1 it gives no
// id ("") and starts nothing: the in-process core ends with this command,
// and the next command's core numbers the tabs of another listing, so its
// "tab <id>" could select another device's page (final review F1).
func simulatorTab(ctx context.Context, env commands.Env, in commands.Input, pageID string) (string, error) {
	if NoDaemon() {
		return "", nil
	}
	h, err := hostOf(env)
	if err != nil {
		return "", err
	}
	c, err := Connect(ctx, h.ClientOptions())
	if err != nil {
		return "", err
	}
	defer c.Close()
	cwd, _ := os.Getwd()
	g := in.Global
	res, err := c.Run(ctx, RunRequest{
		Argv: []string{"tab", "list", "--device", core.SimulatorDevice}, Cwd: cwd, JSON: true,
		PageReq: PageReq{Transport: g.Transport}, // as the CLI's tab list sends it
		Owner:   Owner{Session: g.Session, Reservation: g.Reservation},
	}, nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Tabs []core.Tab `json:"tabs"`
	}
	if err := json.Unmarshal(res.Data, &list); err != nil {
		return "", fmt.Errorf("reading the daemon's tab list: %w", err)
	}
	// A page key is "<kind>|<device id>|<page id>" (core's findListed).
	key := string(transport.KindIWDP) + "|" + core.SimulatorDevice + "|" + pageID
	for _, t := range list.Tabs {
		if t.Key == key {
			return t.ID, nil
		}
	}
	return "", nil
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
