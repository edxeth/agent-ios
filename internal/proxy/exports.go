package proxy

import (
	"context"
	"fmt"
	"net/netip"
)

// The exports of this file let another owner of processes and ports (the
// ssh forward of internal/sim) use the identity and listener checks that
// restart-iwdp relies on, and let sim up tell whether the iwdp on the
// listing address is the one that iwdp-cli started.

// StartTokenEnv is the environment variable whose value marks a process
// that iwdp-cli started (ProcessIdentity).
const StartTokenEnv = tokenEnv

// NewStartToken returns a fresh random start token.
func NewStartToken() string { return newToken() }

// ProcessIdentity returns the start time of process pid and the value of
// StartTokenEnv in its environment ("" without one). A start time and a
// token, both recorded when the process started, tell that process from a
// later one with the same pid. Any failed read is an error.
func ProcessIdentity(pid int) (start, token string, err error) {
	id, err := readIdentity(pid)
	if err != nil {
		return "", "", err
	}
	return id.start, id.token, nil
}

// ListenerPIDs returns the pids of the processes of this user that listen
// on the TCP port, on any address, sorted.
func ListenerPIDs(ctx context.Context, port uint16) ([]int, error) {
	return listenerPIDs(ctx, port)
}

// OwnsListing reports whether something listens on the default listing
// address and it is the iwdp that iwdp-cli started and recorded: the
// checks of a restart (stopOwn), without a stop. iwdp-cli starts iwdp with
// the simulator address 127.0.0.1:27753 on Linux and iwdp's default
// (localhost:27753) on macOS (simulatorArgs).
func OwnsListing(ctx context.Context) (listening, own bool, err error) {
	return std.ownsListing(ctx)
}

func (s *Starter) ownsListing(ctx context.Context) (listening, own bool, err error) {
	ep, err := netip.ParseAddrPort(s.addr)
	if err != nil {
		return false, false, fmt.Errorf("checking the owner of ios_webkit_debug_proxy on %s: %w", s.addr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeBudget)
	defer cancel()
	if portFree(ctx, s.addr) == nil {
		return false, false, nil
	}
	rec, err := s.loadRecord()
	if err != nil || rec == nil || rec.problem(s.addr) != "" {
		return true, false, nil
	}
	return true, s.owns(ctx, rec, ep) == nil, nil
}
