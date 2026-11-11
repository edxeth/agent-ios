package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil"
)

// guardBin holds the package-wide fakes of the iwdp and device commands
// (testutil.InstallFakeIWDP): a test path without its own fake reaches
// them, never the real ios_webkit_debug_proxy or idevice_id.
var guardBin string

// TestMain gives the start lock (lockStart) a private XDG_RUNTIME_DIR and
// puts the fake iwdp and device commands first on PATH.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iwdp-proxy-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	guardBin = filepath.Join(dir, "bin")
	err = os.Mkdir(guardBin, 0o700)
	if err == nil {
		err = testutil.InstallFakeIWDP(guardBin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake iwdp commands:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// Review round 2, F8: idevice_id, ios_webkit_debug_proxy and pkill
// resolve to the fakes first on PATH in this package's tests, and each
// test fake set (installFakeIWDP) fakes idevice_id too.
func TestProxyTestsNeverRunTheRealIWDPOrIdeviceID(t *testing.T) {
	if err := testutil.CheckFakeIWDP(guardBin); err != nil {
		t.Fatal(err)
	}
	if p, err := exec.LookPath("idevice_id"); err != nil || p != filepath.Join(guardBin, "idevice_id") {
		t.Errorf("idevice_id resolves to %q (%v), not the package fake in %s", p, err, guardBin)
	}
	f := installFakeIWDP(t, fakeFail, freeAddr(t))
	for _, name := range []string{"idevice_id", "ios_webkit_debug_proxy"} {
		if p, err := exec.LookPath(name); err != nil || p != filepath.Join(f.dir, name) {
			t.Errorf("%s resolves to %q (%v), not the test fake in %s", name, p, err, f.dir)
		}
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// A wildcard listener owns every local address of its port (iwdp listens
// on 0.0.0.0): the specific loopback address is not free.
func TestPortFreeSeesAWildcardListener(t *testing.T) {
	port := portOf(t, freeAddr(t))
	l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := portFree(context.Background(), fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		t.Fatalf("127.0.0.1:%d counts as free while 0.0.0.0:%d listens", port, port)
	}
}

// Connections in TIME_WAIT that a stopped iwdp leaves on its port do not
// own it: the port is free once the listener is gone.
func TestPortFreeIgnoresTimeWait(t *testing.T) {
	addr := freeAddr(t)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The server side closes first, so its socket on the listening port
	// enters TIME_WAIT.
	_ = s.Close()
	_, _ = io.Copy(io.Discard, c)
	_ = c.Close()
	_ = l.Close()
	if err := portFree(context.Background(), addr); err != nil {
		t.Fatalf("%s with only a TIME_WAIT connection left: %v", addr, err)
	}
}

// startServer runs the serve fake (TestFakeIWDPProcess) on addr in its own
// process group and waits until it listens; it returns the fake's pid. The
// fake ends with this test process (exitWithParent).
func startServer(t *testing.T, addr string) int {
	t.Helper()
	return startServerOn(t, "tcp", addr)
}

// startServerOn is startServer with the listener's network (net.Listen).
func startServerOn(t *testing.T, network, addr string) int {
	t.Helper()
	s := spawnServer(t, network, addr)
	if s.listenErr != "" {
		t.Fatalf("the serve fake did not listen on %s %s: %s", network, addr, s.listenErr)
	}
	return s.pid
}

// fakeStartGuard bounds the wait for the serve fake's listen report: the
// fake is a separate process that can hang before it reports.
const fakeStartGuard = 10 * time.Second

// served is the serve fake's own report of its listen call (fakeReadyEnv).
type served struct {
	pid       int
	local     netip.AddrPort // the listener's local address, when it listens
	listenErr string         // the listen error, when it does not
	noFamily  bool           // the error is EAFNOSUPPORT (fakeNoFamily)
}

// spawnServer runs the serve fake as startServerOn does and returns its
// report of the listen call. The fake reports on a pipe after net.Listen
// returns, so readiness never depends on a connection to the address: a
// dial to [::1] fails on a host whose loopback has no ::1, although the
// fake listens on [::]. A fake that exits before it reports fails the
// test.
func spawnServer(t *testing.T, network, addr string) served {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestFakeIWDPProcess$")
	// ExtraFiles[0] is file descriptor 3 in the fake.
	cmd.ExtraFiles = []*os.File{pw}
	cmd.Env = append(os.Environ(), fakeServeEnv+"="+addr, fakeNetworkEnv+"="+network, fakeReadyEnv+"=3", fakeParent())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	// The fake holds its own copy; without this one, the read below ends
	// when the fake exits.
	_ = pw.Close()
	if err != nil {
		_ = pr.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = pr.Close()
	})
	report := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(pr).ReadString('\n')
		report <- line
	}()
	var line string
	select {
	case line = <-report:
	case <-time.After(fakeStartGuard):
		t.Fatalf("the serve fake did not report its listen on %s %s within %s", network, addr, fakeStartGuard)
	}
	s := served{pid: cmd.Process.Pid}
	line, complete := strings.CutSuffix(line, "\n")
	if local, ok := strings.CutPrefix(line, fakeListening); ok && complete {
		if s.local, err = netip.ParseAddrPort(local); err != nil {
			t.Fatalf("the serve fake on %s %s reported the local address %q: %v", network, addr, local, err)
		}
		return s
	}
	if msg, ok := strings.CutPrefix(line, fakeListenError); ok && complete {
		s.listenErr = msg
		_, s.noFamily = strings.CutPrefix(msg, fakeNoFamily)
		return s
	}
	t.Fatalf("the serve fake on %s %s ended without a listen report (it wrote %q)", network, addr, line)
	return s
}

// inspector tells who receives a connection to an endpoint.
type inspector = func(context.Context, int, netip.AddrPort) (listener, error)

// inspectors are the ownership checks that run on this system: lsof
// everywhere (darwin's check), /proc on Linux.
func inspectors() map[string]inspector {
	m := map[string]inspector{"lsof": lsofGroupListens}
	if runtime.GOOS == "linux" {
		m["proc"] = procGroupListens
	}
	return m
}

// endpoint is the AddrPort of a host:port address.
func endpoint(t *testing.T, addr string) netip.AddrPort {
	t.Helper()
	ep, err := netip.ParseAddrPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// The ownership checks tell the started group's listener from another
// process's listener, and report nobody on a free port.
func TestGroupListensTellsOwnFromForeign(t *testing.T) {
	addr := freeAddr(t)
	child := startServer(t, addr)
	foreign := freeAddr(t)
	serveListing(t, foreign) // a listener of this test process
	self := syscall.Getpgrp()
	ctx := context.Background()
	for name, f := range inspectors() {
		if got, err := f(ctx, child, endpoint(t, addr)); err != nil || got != listenerOwn {
			t.Errorf("%s(child group, its endpoint) = %v, %v; want listenerOwn", name, got, err)
		}
		if got, err := f(ctx, child, endpoint(t, foreign)); err != nil || got != listenerForeign {
			t.Errorf("%s(child group, another process's endpoint) = %v, %v; want listenerForeign", name, got, err)
		}
		if got, err := f(ctx, self, endpoint(t, addr)); err != nil || got != listenerForeign {
			t.Errorf("%s(this group, the child's endpoint) = %v, %v; want listenerForeign", name, got, err)
		}
		if got, err := f(ctx, child, endpoint(t, freeAddr(t))); err != nil || got != listenerNone {
			t.Errorf("%s on a port where nobody listens = %v, %v; want listenerNone", name, got, err)
		}
	}
}

// Review round 2, F2: an IPv4 wildcard listener of the started group (the
// socket iwdp opens on 0.0.0.0) receives a connection to the loopback
// address, so it is the group's (both checks).
func TestGroupListensAcceptsTheGroupsWildcardListener(t *testing.T) {
	addr := freeAddr(t)
	child := startServerOn(t, "tcp4", "0.0.0.0:"+strconv.Itoa(portOf(t, addr)))
	for name, f := range inspectors() {
		if got, err := f(context.Background(), child, endpoint(t, addr)); err != nil || got != listenerOwn {
			t.Errorf("%s(child group on 0.0.0.0, probe %s) = %v, %v; want listenerOwn", name, addr, got, err)
		}
	}
}

// WS3 round 4 (reviewer repro ipv6only-listener.py): an IPv6 wildcard
// socket serves IPv4 only when it is dual-stack, and neither the socket
// tables nor lsof show IPV6_V6ONLY. So the group's only listener on [::]
// never proves that it receives connections to 127.0.0.1: both checks
// report nobody, also for Go's dual-stack socket, and a restart or a
// start's readiness check fails closed.
//
// The real sockets need IPv6 sockets, not an IPv6 loopback address: the
// fake reports its listen itself (spawnServer). On a kernel without IPv6
// (socket(AF_INET6) fails with EAFNOSUPPORT, and Go's "tcp" listener on
// 0.0.0.0 is then an IPv4 socket) no real IPv6 wildcard socket can exist;
// the socket-table fixtures below cover the rule on every host.
func TestGroupListensDoesNotTrustAnIPv6WildcardForIPv4(t *testing.T) {
	ipv6 := true // the system can open IPv6 sockets
	for _, l := range []struct{ name, network, host string }{
		{"IPV6_V6ONLY socket on [::]", "tcp6", "[::]"},
		{"Go's dual-stack socket for 0.0.0.0", "tcp", "0.0.0.0"},
	} {
		addr := freeAddr(t)
		s := spawnServer(t, l.network, l.host+":"+strconv.Itoa(portOf(t, addr)))
		switch {
		case s.noFamily && l.network == "tcp6":
			ipv6 = false
			t.Logf("no IPv6 sockets on this system (%s): the fixtures cover the %s", s.listenErr, l.name)
			continue
		case s.listenErr != "":
			t.Fatalf("the serve fake for a %s did not listen: %s", l.name, s.listenErr)
		case s.local.Addr() != netip.IPv6Unspecified():
			// Go listens on the IPv4 wildcard for "tcp" only without IPv6.
			if ipv6 || l.network != "tcp" {
				t.Fatalf("the serve fake for a %s listens on %s, not on the IPv6 wildcard", l.name, s.local)
			}
			t.Logf("Go opened an IPv4 socket (%s) without IPv6: the fixtures cover the %s", s.local, l.name)
			continue
		}
		for name, f := range inspectors() {
			if got, err := f(context.Background(), s.pid, endpoint(t, addr)); err != nil || got != listenerNone {
				t.Errorf("%s(child group with a %s, probe %s) = %v, %v; want listenerNone", name, l.name, addr, got, err)
			}
		}
	}
	t.Run("socket-table fixtures", testOwnIPv6WildcardFixtures)
}

// fixturePort is the port of the socket-table fixtures; nothing listens on
// it, the inspectors only read the fixtures.
const fixturePort = 9221

// testOwnIPv6WildcardFixtures runs each inspector on fixed socket tables
// where the group's only listener on fixturePort is the IPv6 wildcard: an
// IPv4 probe gets listenerNone, while an IPv6 probe on the same tables
// gets listenerOwn, which shows that the inspector read the listener as
// the group's.
func testOwnIPv6WildcardFixtures(t *testing.T) {
	v4 := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), fixturePort)
	v6 := netip.AddrPortFrom(netip.IPv6Loopback(), fixturePort)
	for name, f := range inspectors() {
		pgid := syscall.Getpgrp()
		switch name {
		case "proc":
			procIPv6WildcardFixture(t)
		case "lsof":
			lsofIPv6WildcardFixture(t, pgid)
		default:
			t.Fatalf("no IPv6 wildcard fixture for the %s inspector", name)
		}
		if got, err := f(context.Background(), pgid, v4); err != nil || got != listenerNone {
			t.Errorf("%s(group whose only listener is on [::]:%d, probe %s) = %v, %v; want listenerNone", name, fixturePort, v4, got, err)
		}
		if got, err := f(context.Background(), pgid, v6); err != nil || got != listenerOwn {
			t.Errorf("%s(group whose only listener is on [::]:%d, probe %s) = %v, %v; want listenerOwn", name, fixturePort, v6, got, err)
		}
	}
}

// procIPv6WildcardFixture points socketTables, for this test, at an empty
// tcp table and a tcp6 table whose only row is a listener on
// [::]:fixturePort. The row's inode is that of a socket of this test
// process, so groupSocketLinks finds it in this process group's fds.
func procIPv6WildcardFixture(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	rc, err := l.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var link string
	var linkErr error
	if err := rc.Control(func(fd uintptr) { link, linkErr = os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)) }); err != nil {
		t.Fatal(err)
	}
	inode, ok := strings.CutPrefix(link, "socket:[")
	inode, closed := strings.CutSuffix(inode, "]")
	if linkErr != nil || !ok || !closed {
		t.Fatalf("the socket link of this test's listener = %q, %v", link, linkErr)
	}
	const header = "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// The IPv6 wildcard as the kernel prints it: 32 hex digits, all zero;
	// 0A is TCP_LISTEN.
	row := fmt.Sprintf("   0: %032X:%04X %032X:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 %s 1 0000000000000000 100 0 0 10 0\n", 0, fixturePort, 0, inode)
	dir := t.TempDir()
	tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcp, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte(header+row), 0o600); err != nil {
		t.Fatal(err)
	}
	old := socketTables
	t.Cleanup(func() { socketTables = old })
	socketTables.tcp, socketTables.tcp6 = tcp, tcp6
}

// lsofIPv6WildcardFixture puts first on PATH, for this test, an lsof that
// answers the inspector's query for the listeners on fixturePort with one
// socket of process group pgid on the IPv6 wildcard, in the -F pgtn format
// (lsofSockets). Any other query fails.
func lsofIPv6WildcardFixture(t *testing.T, pgid int) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "lsof-output")
	if err := os.WriteFile(out, fmt.Appendf(nil, "p%d\ng%d\ntIPv6\nn*:%d\n", os.Getpid(), pgid, fixturePort), 0o600); err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	query := fmt.Sprintf("-w -n -P -iTCP:%d -sTCP:LISTEN -F pgtn", fixturePort)
	script := "#!/bin/sh\nif [ \"$*\" != " + q(query) + " ]; then echo \"fixture lsof: unexpected query $*\" >&2; exit 2; fi\nexec cat " + q(out) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// F2: who receives a connection is decided by address and port. The most
// specific bind wins: the exact address, else the family's wildcard.
func TestClassifyMatchesTheEndpointAddress(t *testing.T) {
	lo, lo2 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")
	any4, any6, lo6 := netip.IPv4Unspecified(), netip.IPv6Unspecified(), netip.MustParseAddr("::1")
	cases := []struct {
		name  string
		socks []sock
		ep    netip.Addr
		want  listener
	}{
		{"own exact", []sock{{lo, true}}, lo, listenerOwn},
		{"own on another address only", []sock{{lo2, true}}, lo, listenerNone},
		{"own on another address, foreign exact", []sock{{lo2, true}, {lo, false}}, lo, listenerForeign},
		{"own wildcard", []sock{{any4, true}}, lo, listenerOwn},
		{"own wildcard, foreign exact", []sock{{any4, true}, {lo, false}}, lo, listenerForeign},
		{"own exact, foreign wildcard", []sock{{lo, true}, {any4, false}}, lo, listenerOwn},
		{"foreign wildcard", []sock{{any4, false}}, lo, listenerForeign},
		{"own IPv6 wildcard does not prove IPv4 (V6ONLY unknown)", []sock{{any6, true}}, lo, listenerNone},
		{"foreign IPv6 wildcard may serve IPv4", []sock{{any6, false}}, lo, listenerForeign},
		{"own dual-stack wildcard, foreign IPv4 wildcard", []sock{{any6, true}, {any4, false}}, lo, listenerForeign},
		{"own IPv4 wildcard, foreign dual-stack wildcard", []sock{{any4, true}, {any6, false}}, lo, listenerOwn},
		{"IPv4 wildcard does not cover IPv6", []sock{{any4, true}}, lo6, listenerNone},
		{"IPv6 exact", []sock{{lo6, true}}, lo6, listenerOwn},
		{"IPv6 own wildcard", []sock{{any6, true}}, lo6, listenerOwn},
		{"IPv4-mapped probe", []sock{{lo, true}}, netip.MustParseAddr("::ffff:127.0.0.1"), listenerOwn},
		{"nobody", nil, lo, listenerNone},
	}
	for _, c := range cases {
		if got := classify(c.socks, c.ep); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

// lsofSockets reads lsof -F pgtn output; an address it cannot parse is an
// error.
func TestLsofSocketsParsesTheFieldOutput(t *testing.T) {
	out := "p10\ng10\ntIPv4\nn*:9\ntIPv6\nn*:9\np11\ng7\ntIPv4\nn127.0.0.1:9\ntIPv6\nn[::1]:9\n"
	got, err := lsofSockets([]byte(out), 10)
	want := []sock{
		{netip.IPv4Unspecified(), true},
		{netip.IPv6Unspecified(), true},
		{netip.MustParseAddr("127.0.0.1"), false},
		{netip.MustParseAddr("::1"), false},
	}
	if err != nil || len(got) != len(want) {
		t.Fatalf("lsofSockets = %v, %v; want %v", got, err, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("socket %d = %v, want %v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"p1\ng1\ntIPv4\nnno-port\n", "p1\ng1\nn*:9\n", "p1\ng1\ntIPv4\nnbad:9\n"} {
		if _, err := lsofSockets([]byte(bad), 1); err == nil {
			t.Errorf("lsofSockets(%q) = nil error, want one", bad)
		}
	}
}

// Review round 1, F2 across processes: a start of another process (here a
// second open of the lock file, which flock treats alike) holds the start
// lock; Start waits for it and starts nothing meanwhile.
func TestStartWaitsForAnotherProcessesStart(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	unlock, err := lockStart(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = testStarter(addr, time.Second).Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "waiting for another process's iwdp start") {
		t.Fatalf("Start while another start holds the lock = %v, want the lock wait error", err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times while another start held the lock", n)
	}
	unlock()
	if err := testStarter(addr, time.Second).Start(context.Background()); err == nil || strings.Contains(err.Error(), "waiting") {
		t.Fatalf("Start after the lock was released = %v, want the fake's exit error", err)
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times after the lock was released, want 1", n)
	}
}

// A listing address that cannot be parsed is never free.
func TestPortFreeRejectsBadAddresses(t *testing.T) {
	if err := portFree(context.Background(), "127.0.0.1"); err == nil {
		t.Error("portFree of an address without a port = nil, want an address error")
	}
	// These fail before any connection: no packet leaves this process.
	for _, addr := range []string{"127.0.0.1:x", "127.0.0.1:70000"} {
		if err := portFree(context.Background(), addr); err == nil {
			t.Errorf("portFree(%q) = nil, want an address error", addr)
		}
		if err := CheckListing(context.Background(), addr); err == nil || errors.Is(err, ErrNotListening) {
			t.Errorf("CheckListing(%q) = %v, want an error other than ErrNotListening", addr, err)
		}
	}
}

// An address without a host is the IPv4 wildcard address, probed once.
func TestPortFreeWithoutAHostProbesTheWildcard(t *testing.T) {
	port := portOf(t, freeAddr(t))
	if err := portFree(context.Background(), fmt.Sprintf(":%d", port)); err != nil {
		t.Fatalf("portFree(:%d) of a free port: %v", port, err)
	}
	l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := portFree(context.Background(), fmt.Sprintf(":%d", port)); err == nil {
		t.Fatalf("portFree(:%d) = nil while 0.0.0.0:%d listens", port, port)
	}
}

// A start lock that cannot be opened stops the start before anything runs.
func TestStartFailsWithoutAStartLock(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "missing"))
	err := testStarter(addr, time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "opening the iwdp start lock") {
		t.Fatalf("Start without a usable lock directory = %v, want the lock error", err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times without the start lock", n)
	}
}

// When the ownership of the listener cannot be checked, the started iwdp
// is not taken as ready: Start stops it and fails (on every system).
func TestStartStopsItsProcessWhenOwnershipCannotBeChecked(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	s.listens = func(context.Context, int, netip.AddrPort) (listener, error) {
		return 0, errors.New("socket tables unreadable")
	}
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checking which process listens") {
		t.Fatalf("Start with a failing ownership check = %v, want the ownership check error", err)
	}
	assertGone(t, f)
}

// assertGone fails for every recorded process of f that still runs.
func assertGone(t *testing.T, f *fakeIWDP) {
	t.Helper()
	for _, pid := range f.pids() {
		if alive(pid) {
			t.Errorf("process %d is still running after Start returned", pid)
		}
	}
}

// fakeLsof installs into f's directory (first on PATH) an lsof that writes
// entered and then waits until release exists. It returns both paths.
func fakeLsof(t *testing.T, f *fakeIWDP) (entered, release string) {
	t.Helper()
	entered, release = filepath.Join(f.dir, "lsof-entered"), filepath.Join(f.dir, "lsof-release")
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\necho entered > " + q(entered) + "\nwhile [ ! -f " + q(release) + " ]; do sleep 0.01; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.dir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("go"), 0o600) })
	return entered, release
}

// Review round 2, F5: a stalled lsof (darwin's check) does not hold Start
// past the caller's context: Start returns the context's error and its
// started group is gone.
//
// The context ends once lsof runs, not after a fixed time: the spawn and
// the readiness wait before the check take a time that depends on the
// host's load, and a context that ends before lsof runs tests nothing of
// the check.
//
// The caller's cancellation, not lsof's own bound (lsofTimeout), must end
// lsof: the check's error wraps context.Canceled, never the
// context.DeadlineExceeded of that bound. Start returns the caller's
// context error in both cases, and the wait for Start's return leaves
// room for the stop of the group (stopGrace + stopReap), which is longer
// than lsofTimeout, so neither Start's error nor its duration shows an
// lsof that ignores the caller's context.
func TestStartHonorsTheContextDuringTheOwnershipCheck(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	entered, _ := fakeLsof(t, f)
	s := testStarter(addr, 20*time.Second)
	checks := make(chan error, 1)
	s.listens = func(ctx context.Context, pgid int, ep netip.AddrPort) (listener, error) {
		l, err := lsofGroupListens(ctx, pgid, ep)
		select {
		case checks <- err:
		default:
			t.Errorf("a second ownership check ran (%v); the stalled lsof blocks the first one until the context ends", err)
		}
		return l, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	waitFile(t, entered)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start after its context ended = %v, want context.Canceled", err)
		}
	case <-time.After(stopGrace + stopReap + time.Second):
		t.Fatal("Start is still blocked in lsof after its context ended")
	}
	assertGone(t, f)
	select {
	case err := <-checks:
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the ownership check ended with %v; want the caller's context.Canceled, not lsof's own %s bound", err, lsofTimeout)
		}
	default:
		t.Fatal("Start returned before its ownership check did")
	}
}

// F5: lsof itself is bounded: a stalled lsof fails the check after
// lsofTimeout, and Start stops its group and fails.
func TestStartFailsWhenLsofStalls(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	fakeLsof(t, f)
	s := testStarter(addr, 20*time.Second)
	s.listens = lsofGroupListens
	begin := time.Now()
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checking which process listens") {
		t.Fatalf("Start with a stalled lsof = %v, want the ownership check error", err)
	}
	if d := time.Since(begin); d > lsofTimeout+stopGrace+stopReap+5*time.Second {
		t.Errorf("Start took %s with a stalled lsof", d)
	}
	assertGone(t, f)
}

// lsof that cannot run is an error, not a foreign listener.
func TestLsofGroupListensReportsAFailingLsof(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte("#!/bin/sh\necho 'lsof: broken' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ep := netip.MustParseAddrPort("127.0.0.1:8080")
	if got, err := lsofGroupListens(context.Background(), os.Getpid(), ep); err == nil {
		t.Fatalf("lsofGroupListens with a failing lsof = %v, nil; want an error", got)
	}
	// Without lsof on PATH, the macOS place is used.
	t.Setenv("PATH", t.TempDir())
	_, err := lsofGroupListens(context.Background(), os.Getpid(), ep)
	if _, statErr := os.Stat(lsofPath); statErr != nil && err == nil {
		t.Fatalf("lsofGroupListens without lsof = nil error, want one (no %s here)", lsofPath)
	}
}

// Review round 2, F7: without XDG_RUNTIME_DIR the start lock lives in the
// temporary directory, which can be the shared /tmp, so its name carries
// the user id: another user's lock file for the same address does not
// block this user.
func TestLockStartFallsBackToAPerUserFileInTheTemporaryDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", tmp)
	// Another user's file, as this user cannot open it.
	other := filepath.Join(tmp, fmt.Sprintf("iwdp-cli-start-%d-127.0.0.1_1.lock", os.Getuid()+1))
	if err := os.WriteFile(other, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(tmp, "iwdp-cli-start-127.0.0.1_1.lock")
	if err := os.WriteFile(shared, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockStart(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("lockStart with other users' files in TMPDIR: %v", err)
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(tmp, fmt.Sprintf("iwdp-cli-start-%d-127.0.0.1_1.lock", os.Getuid()))); err != nil {
		t.Fatalf("the start lock is not this user's file in TMPDIR: %v", err)
	}
}
