package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// errPortTaken marks a start that found the listing address owned by
// another process after its first check: right before the spawn, or as
// the listener at readiness. It does not start the back-off.
var errPortTaken = errors.New("another process took the iwdp listing port")

// portFree returns nil when every address of the listing address addr
// (host:port; a host name counts with every address it resolves to) can be
// bound now, and otherwise the error of the first bind that fails. Only a
// successful bind proves that no process owns the address: a socket that
// is bound but does not listen refuses connections like a free port.
func portFree(ctx context.Context, addr string) error {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listing address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("listing address %q: bad port %q", addr, ps)
	}
	var ips []net.IP
	switch ip := net.ParseIP(host); {
	case host == "":
		ips = []net.IP{net.IPv4zero}
	case ip != nil:
		ips = []net.IP{ip}
	default:
		found, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return fmt.Errorf("resolving the listing address %q: %w", addr, err)
		}
		for _, a := range found {
			ips = append(ips, a.IP)
		}
	}
	// Each address, and the wildcard address of its family: on darwin
	// SO_REUSEADDR lets a specific address bind over a wildcard listener
	// (iwdp listens on 0.0.0.0), but two binds of the same wildcard
	// address conflict.
	probed := map[string]bool{}
	for _, ip := range ips {
		wild := net.IPv6unspecified
		if ip.To4() != nil {
			wild = net.IPv4zero
		}
		for _, p := range []net.IP{ip, wild} {
			if probed[p.String()] {
				continue
			}
			probed[p.String()] = true
			if err := bindProbe(p, port); err != nil {
				return fmt.Errorf("%s: %w", net.JoinHostPort(p.String(), ps), err)
			}
		}
	}
	return nil
}

// bindProbe binds a TCP socket to ip:port and closes it at once; it never
// listens, so no client can connect to it. SO_REUSEADDR lets the bind pass
// over connections in TIME_WAIT that a stopped iwdp left behind; a
// listening socket, and on Linux a bound socket without SO_REUSEADDR,
// still make it fail with EADDRINUSE.
func bindProbe(ip net.IP, port int) error {
	family := syscall.AF_INET6
	var sa syscall.Sockaddr
	if v4 := ip.To4(); v4 != nil {
		family = syscall.AF_INET
		sa = &syscall.SockaddrInet4{Port: port, Addr: [4]byte(v4)}
	} else {
		sa = &syscall.SockaddrInet6{Port: port, Addr: [16]byte(ip.To16())}
	}
	// Close-on-exec under ForkLock, as package net does: a process started
	// meanwhile must not inherit the probe and own the port.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return os.NewSyscallError("socket", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return os.NewSyscallError("setsockopt SO_REUSEADDR", err)
	}
	return os.NewSyscallError("bind", syscall.Bind(fd, sa))
}

// Who receives a connection to the listing endpoint, as groupListens
// reports it.
type listener int

const (
	listenerNone    listener = iota // nobody listens there (yet)
	listenerOwn                     // a process of the started group
	listenerForeign                 // another process
)

// sock is a listening TCP socket on the probed port.
type sock struct {
	addr netip.Addr // local address, IPv4-mapped addresses unmapped
	own  bool       // held by a process of the started group
}

// classify reports who receives a connection to address ep among the
// listening sockets socks on its port. The most specific bind receives it:
// sockets on ep itself first, else those on the wildcard address of ep's
// family, and for IPv4 last the IPv6 wildcard (a dual-stack socket, as Go
// creates for 0.0.0.0; it can hold the port only when no IPv4 socket
// does). A socket of another process in the deciding set makes it
// foreign. Sockets on other addresses never count.
func classify(socks []sock, ep netip.Addr) listener {
	ep = ep.Unmap()
	order := []netip.Addr{ep, netip.IPv6Unspecified()}
	if ep.Is4() {
		order = []netip.Addr{ep, netip.IPv4Unspecified(), netip.IPv6Unspecified()}
	}
	for _, want := range order {
		found := false
		for _, s := range socks {
			if s.addr != want {
				continue
			}
			if !s.own {
				return listenerForeign
			}
			found = true
		}
		if found {
			return listenerOwn
		}
	}
	return listenerNone
}

// groupListens reports who receives a connection to the TCP endpoint ep:
// a process of the started process group pgid, another process or nobody.
// Linux reads /proc (procGroupListens); darwin asks lsof under ctx
// (lsofGroupListens). Elsewhere no started iwdp counts as ready.
func groupListens(ctx context.Context, pgid int, ep netip.AddrPort) (listener, error) {
	switch runtime.GOOS {
	case "linux":
		return procGroupListens(ctx, pgid, ep)
	case "darwin":
		return lsofGroupListens(ctx, pgid, ep)
	}
	return 0, fmt.Errorf("cannot tell which process listens on %s on %s", ep, runtime.GOOS)
}

// listenerPIDs returns the pids of the processes that listen on the TCP
// port, on any address, sorted, for a message that names a port's owner.
// Linux reads /proc (procListenerPIDs); darwin asks lsof under ctx
// (lsofListenerPIDs). A process whose sockets this user cannot see is
// missing from the list.
func listenerPIDs(ctx context.Context, port uint16) ([]int, error) {
	switch runtime.GOOS {
	case "linux":
		return procListenerPIDs(port)
	case "darwin":
		return lsofListenerPIDs(ctx, port)
	}
	return nil, fmt.Errorf("cannot tell which process listens on port %d on %s", port, runtime.GOOS)
}

// startLockPoll is the interval of the attempts to take the start lock.
const startLockPoll = 50 * time.Millisecond

// runtimeName is the name of this user's file of kind for the listing
// address addr, iwdp-mcp-<kind>-<uid>-<address>.<ext>: it carries the user
// id, so in a shared directory one user's file never blocks another user.
func runtimeName(kind, addr, ext string) string {
	name := strings.NewReplacer(":", "_", "/", "_", "[", "", "]", "", "%", "_").Replace(addr)
	return fmt.Sprintf("iwdp-mcp-%s-%d-%s.%s", kind, os.Getuid(), name, ext)
}

// lockStart takes the host-wide start lock of the listing address addr, so
// that cooperating processes (the daemon, the legacy MCP server, the CLI)
// start iwdp one at a time. The lock is a flock on a file in
// XDG_RUNTIME_DIR, else the temporary directory (shared /tmp on Linux). The
// file is opened with O_NOFOLLOW and close-on-exec, so the started iwdp
// never holds it.
func lockStart(ctx context.Context, addr string) (unlock func(), err error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, runtimeName("start", addr, "lock"))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the iwdp start lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("taking the iwdp start lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for another process's iwdp start (lock %s): %w", path, ctx.Err())
		case <-time.After(startLockPoll):
		}
	}
}
