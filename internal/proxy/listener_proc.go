package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// socketTables are the kernel's TCP socket tables. A kernel without IPv6
// has no tcp6 table, and then no IPv6 listener exists.
var socketTables = struct{ tcp, tcp6 string }{"/proc/net/tcp", "/proc/net/tcp6"}

// procFS is where procListenerPIDs and procIdentity read the processes;
// tests point it elsewhere to inject failures.
var procFS = "/proc"

// procGroupListens (Linux) reports who receives a connection to the
// endpoint ep (classify): a process of the process group pgid, another
// process or nobody. It takes the listening sockets on ep's port, with
// their local addresses and inodes, from the kernel's socket tables, and
// counts a socket as the group's when its inode is among the socket links
// in /proc/<pid>/fd of a process of the group.
func procGroupListens(ctx context.Context, pgid int, ep netip.AddrPort) (listener, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	found, err := procListenSockets(ep.Port())
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return listenerNone, nil
	}
	links, err := groupSocketLinks(pgid)
	if err != nil {
		return 0, err
	}
	socks := make([]sock, len(found))
	for i, e := range found {
		socks[i] = sock{addr: e.addr, own: links[e.link]}
	}
	return classify(socks, ep.Addr()), nil
}

// procSock is a listening TCP socket of the kernel's socket tables.
type procSock struct {
	addr netip.Addr
	link string // "socket:[inode]", as fd links name sockets
}

// procListenSockets returns the listening TCP sockets on port from the
// kernel's socket tables (socketTables).
func procListenSockets(port uint16) ([]procSock, error) {
	var found []procSock
	for _, table := range []string{socketTables.tcp, socketTables.tcp6} {
		b, err := os.ReadFile(table)
		if table == socketTables.tcp6 && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading the socket table: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for _, line := range lines[1:] { // the first line names the columns
			// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
			f := strings.Fields(line)
			if len(f) < 10 {
				return nil, fmt.Errorf("%s has a short line %q", table, line)
			}
			hexAddr, hexPort, ok := strings.Cut(f[1], ":")
			p, err := strconv.ParseUint(hexPort, 16, 16)
			addr, addrOK := procAddr(hexAddr)
			if !ok || err != nil || !addrOK {
				return nil, fmt.Errorf("%s has the local address %q", table, f[1])
			}
			const tcpListen = "0A" // TCP_LISTEN in the st column
			if uint16(p) == port && f[3] == tcpListen {
				found = append(found, procSock{addr, "socket:[" + f[9] + "]"})
			}
		}
	}
	return found, nil
}

// procListenerPIDs (Linux) returns the pids of the processes that hold a
// listening TCP socket on port, sorted: the sockets come from the socket
// tables, their holders from the socket links in /proc/<pid>/fd. A process
// whose fds this user cannot read (another user's) is not listed.
func procListenerPIDs(port uint16) ([]int, error) {
	found, err := procListenSockets(port)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	links := map[string]bool{}
	for _, s := range found {
		links[s.link] = true
	}
	entries, err := os.ReadDir(procFS)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", procFS, err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process
		}
		fdDir := filepath.Join(procFS, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // exited since ReadDir, or another user's process
		}
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && links[link] {
				pids = append(pids, pid)
				break
			}
		}
	}
	slices.Sort(pids)
	return pids, nil
}

// procAddr decodes a local address of a socket table: 8 or 32 hex digits,
// each group of 8 a 32-bit word of the address that the kernel prints as
// a number in host byte order.
func procAddr(h string) (netip.Addr, bool) {
	if len(h) != 8 && len(h) != 32 {
		return netip.Addr{}, false
	}
	b := make([]byte, len(h)/2)
	for i := 0; i < len(h); i += 8 {
		w, err := strconv.ParseUint(h[i:i+8], 16, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		binary.NativeEndian.PutUint32(b[i/2:], uint32(w))
	}
	a, _ := netip.AddrFromSlice(b) // 4 or 16 bytes
	return a.Unmap(), true
}

// groupSocketLinks returns the socket links of the fds of every process of
// process group pgid (the pgrp field of /proc/<pid>/stat).
func groupSocketLinks(pgid int) (map[string]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	links := map[string]bool{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue // exited since ReadDir
		}
		// pid (comm) state ppid pgrp ...; comm can hold spaces and parentheses.
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 3 || f[2] != strconv.Itoa(pgid) {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(fdDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue // exited since the scan
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", fdDir, err)
		}
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && strings.HasPrefix(link, "socket:") {
				links[link] = true
			}
		}
	}
	return links, nil
}
