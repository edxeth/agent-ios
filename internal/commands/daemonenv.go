package commands

import (
	"errors"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// errNeedsDaemon: a session, lease or device command ran outside the daemon.
var errNeedsDaemon = errors.New("this command runs in the iwdp daemon")

// managerOf returns the daemon's manager from env (the daemon's Env offers
// Manager; Env itself stays small for the W1a fakes).
func managerOf(env Env) (*core.Manager, error) {
	if me, ok := env.(interface{ Manager() *core.Manager }); ok && me.Manager() != nil {
		return me.Manager(), nil
	}
	return nil, errNeedsDaemon
}

// reservationsOf returns the daemon's device reservations from env.
func reservationsOf(env Env) (*core.Reservations, error) {
	if re, ok := env.(interface{ Reservations() *core.Reservations }); ok && re.Reservations() != nil {
		return re.Reservations(), nil
	}
	return nil, errNeedsDaemon
}

// sessionPages are the status rows of the pages that session leases.
func sessionPages(m *core.Manager, session string) []core.PageStatus {
	var out []core.PageStatus
	for _, p := range m.Status() {
		if p.Session == session {
			out = append(out, p)
		}
	}
	return out
}
