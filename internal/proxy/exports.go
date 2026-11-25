package proxy

import (
	"context"
	"fmt"
	"net/netip"
)

// The exports of this file let another owner of processes and ports (the
// ssh forward of internal/sim) use the identity and listener checks that
// restart-iwdp relies on, and let sim up tell whether the iwdp on the
// listing address is the one that agent-ios started.

// StartTokenEnv is the environment variable whose value marks a process
// that agent-ios started (ProcessIdentity).
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

// EndpointOwner reports who receives a connection to the TCP endpoint ep:
// own when the most specific listening socket that receives it belongs to
// process group pgid, foreign when one of another process does (the checks
// of an iwdp start's readiness, classify). Neither means that nobody
// listens there. A pgid of 0 or less names no group, so any listener is
// foreign. (Process group 0 is the kernel threads' on Linux, whose fds this
// user cannot read: it must not be scanned.)
func EndpointOwner(ctx context.Context, pgid int, ep netip.AddrPort) (own, foreign bool, err error) {
	if pgid <= 0 {
		pgid = -1 // no process has it
	}
	l, err := groupListens(ctx, pgid, ep)
	if err != nil {
		return false, false, err
	}
	return l == listenerOwn, l == listenerForeign, nil
}

// OwnsListing reports whether something listens on the default listing
// address and it is the iwdp that agent-ios started and recorded: the
// checks of a restart (stopOwn), without a stop. agent-ios starts iwdp with
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
