//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// procGroupListens reads the socket tables strictly: a missing IPv4 table,
// a short line and a bad local address are errors; a missing tcp6 table
// means no IPv6 listener.
func TestProcGroupListensReadsTheTablesStrictly(t *testing.T) {
	dir := t.TempDir()
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := socketTables
	t.Cleanup(func() { socketTables = old })
	missing, missing6 := filepath.Join(dir, "missing"), filepath.Join(dir, "missing6")
	cases := []struct {
		name, tcp, tcp6 string
		wantErr         bool
	}{
		{"no IPv4 table", missing, missing6, true},
		{"no IPv6 table", write("empty", header), missing6, false},
		{"short line", write("short", header+"   0: 0100007F:1F90 00000000:0000 0A\n"), missing6, true},
		{"bad address", write("bad", header+"   0: 0100007F 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 123 1\n"), missing6, true},
		{"bad address digits", write("baddigits", header+"   0: 0100007G:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 123 1\n"), missing6, true},
	}
	ep := netip.MustParseAddrPort("127.0.0.1:8080")
	for _, c := range cases {
		socketTables.tcp, socketTables.tcp6 = c.tcp, c.tcp6
		got, err := procGroupListens(context.Background(), os.Getpid(), ep)
		if c.wantErr != (err != nil) || (!c.wantErr && got != listenerNone) {
			t.Errorf("%s: procGroupListens = %v, %v; want error %v", c.name, got, err, c.wantErr)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := procGroupListens(ctx, os.Getpid(), ep); err == nil {
		t.Error("procGroupListens with an ended context = nil error")
	}
}

// procAddr decodes the kernel's addresses: IPv4, IPv6 and IPv4-mapped.
func TestProcAddrDecodesTheSocketTables(t *testing.T) {
	for h, want := range map[string]string{
		"0100007F":                         "127.0.0.1",
		"00000000":                         "0.0.0.0",
		"00000000000000000000000001000000": "::1",
		"0000000000000000FFFF00000100007F": "127.0.0.1",
	} {
		got, ok := procAddr(h)
		if !ok || got != netip.MustParseAddr(want) {
			t.Errorf("procAddr(%s) = %v, %v; want %s", h, got, ok, want)
		}
	}
	for _, bad := range []string{"0100007", "zz00007F"} {
		if _, ok := procAddr(bad); ok {
			t.Errorf("procAddr(%q) passed", bad)
		}
	}
}

// Review round 2, F2: a group that listens on 127.0.0.2:P does not own
// 127.0.0.1:P, where another process listens: ready refuses it (both
// checks; Linux only, where all of 127/8 is local).
func TestReadinessMatchesTheAddress(t *testing.T) {
	addr := freeAddr(t)
	port := portOf(t, addr)
	child := startServer(t, net.JoinHostPort("127.0.0.2", strconv.Itoa(port)))
	serveListing(t, addr) // another process (this test) on the listing address
	for name, f := range inspectors() {
		s := testStarter(addr, time.Second)
		s.listens = f
		st, err := s.ready(context.Background(), time.Now().Add(5*time.Second), child)
		if st != startNotListening || err == nil || !errors.Is(err, errPortTaken) {
			t.Errorf("%s: ready with the group on 127.0.0.2 and another process on %s = %v, %v; want errPortTaken", name, addr, st, err)
		}
	}
}
