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
	"strconv"
	"strings"
	"time"
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
	bin, err := exec.LookPath("lsof")
	if err != nil {
		bin = lsofPath
	}
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	// -w: no warnings; -n -P: numeric addresses and ports; -F pgtn: the
	// process id, process group, address family and local address.
	cmd := exec.CommandContext(ctx, bin, "-w", "-n", "-P", "-iTCP:"+strconv.Itoa(int(ep.Port())), "-sTCP:LISTEN", "-F", "pgtn")
	cmd.WaitDelay = lsofWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("lsof for the listener on port %d: %w", ep.Port(), ctx.Err())
	}
	// lsof exits 1 without a message when no file matches.
	var exit *exec.ExitError
	noMatch := errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0
	if err != nil && !noMatch {
		return 0, fmt.Errorf("lsof for the listener on port %d: %v %s", ep.Port(), err, bytes.TrimSpace(stderr.Bytes()))
	}
	socks, err := lsofSockets(out, pgid)
	if err != nil {
		return 0, fmt.Errorf("lsof for the listener on port %d: %w", ep.Port(), err)
	}
	return classify(socks, ep.Addr()), nil
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
