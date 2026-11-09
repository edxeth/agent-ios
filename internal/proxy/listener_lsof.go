package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/logcause"
)

const (
	// lsofPath is lsof's place on macOS when it is not on PATH.
	lsofPath = "/usr/sbin/lsof"
	// lsofTimeout bounds one lsof run; a stalled lsof fails the check.
	lsofTimeout = 2 * time.Second
	// lsofWaitDelay bounds the wait for lsof's output after it was killed.
	lsofWaitDelay = 500 * time.Millisecond
)

// lsofGroupListens (darwin) reports who receives a connection to the
// endpoint ep (classify). lsof lists the TCP sockets in LISTEN state on
// ep's port with their process group and local address; a socket is the
// group's when its process group is pgid. lsof without root lists only
// this user's processes, so a listener of another user is not seen: then
// it reports listenerNone, and the start waits on until it fails. lsof
// runs under ctx and at most lsofTimeout; it is killed and reaped when
// either ends, and the error then wraps the context's error.
func lsofGroupListens(ctx context.Context, pgid int, ep netip.AddrPort) (listener, error) {
	// -F pgtn: the process id, process group, address family and local
	// address.
	out, err := lsofListening(ctx, ep.Port(), "-F", "pgtn")
	if err != nil {
		return 0, err
	}
	socks, err := lsofSockets(out, pgid)
	if err != nil {
		return 0, fmt.Errorf("lsof for the listener on port %d: %w", ep.Port(), err)
	}
	return classify(socks, ep.Addr()), nil
}

// lsofListenerPIDs (darwin) returns the pids of the processes that listen
// on the TCP port, sorted; lsof (-t: pids only) runs as in
// lsofGroupListens.
func lsofListenerPIDs(ctx context.Context, port uint16) ([]int, error) {
	out, err := lsofListening(ctx, port, "-t")
	if err != nil {
		return nil, err
	}
	return lsofPIDs(out)
}

// lsofPIDs parses lsof -t output, one pid per line, into sorted pids.
func lsofPIDs(out []byte) ([]int, error) {
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("lsof -t printed %q, not a pid", f)
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids, nil
}

// lsofListening runs lsof for the TCP sockets in LISTEN state on port with
// the output options format, under ctx and at most lsofTimeout, and
// returns its output; no matching socket is an empty output. lsof is
// killed and reaped when either ends, and the error then wraps the
// context's error.
func lsofListening(ctx context.Context, port uint16, format ...string) ([]byte, error) {
	bin, err := exec.LookPath("lsof")
	if err != nil {
		bin = lsofPath
	}
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	// -w: no warnings; -n -P: numeric addresses and ports.
	args := append([]string{"-w", "-n", "-P", "-iTCP:" + strconv.Itoa(int(port)), "-sTCP:LISTEN"}, format...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = lsofWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("lsof for the listener on port %d: %w", port, ctx.Err())
	}
	// lsof exits 1 without a message when no file matches.
	var exit *exec.ExitError
	noMatch := errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0
	if err != nil && !noMatch {
		return nil, logcause.Errorf("lsof for the listener on port %d: %v %s", port, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// lsofSockets parses lsof -F pgtn output: per process a p and a g line,
// then per socket a t (IPv4 or IPv6) and an n line ("127.0.0.1:9221",
// "[::1]:9221", or "*:9221" for the wildcard address).
func lsofSockets(out []byte, pgid int) ([]sock, error) {
	var socks []sock
	var group, family string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			group, family = "", ""
		case 'g':
			group = v
		case 't':
			family = v
		case 'n':
			host, _, err := net.SplitHostPort(v)
			if err != nil {
				return nil, fmt.Errorf("local address %q: %w", v, err)
			}
			var addr netip.Addr
			switch {
			case host == "*" && family == "IPv4":
				addr = netip.IPv4Unspecified()
			case host == "*" && family == "IPv6":
				addr = netip.IPv6Unspecified()
			default:
				if addr, err = netip.ParseAddr(strings.Trim(host, "[]")); err != nil {
					return nil, fmt.Errorf("local address %q (%s): %w", v, family, err)
				}
			}
			socks = append(socks, sock{addr: addr.Unmap(), own: group == strconv.Itoa(pgid)})
		}
	}
	return socks, sc.Err()
}
