//go:build cdpcompat

package cdpcompat

// The WebKitGTK side of the client scenarios (design 7.8 tests 8, 11 and
// 12): broadwayd, MiniBrowser on a fixture page served by the test, and the
// in-process daemon on the webkitgtk transport. The broadway helpers come
// from the P2 proof harness (e2e/proofs, which this suite replaces) and are
// the same as e2e/webkitgtk's.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/e2e/internal/broadwayshm"
	"github.com/edxeth/agent-ios/internal/transport/webkitgtk"
)

const miniBrowser = "/usr/lib/webkit2gtk-4.1/MiniBrowser"

// startProcess starts a command in its own process group (ownedProcAttr)
// and kills the group at cleanup, so no MiniBrowser survives the test, also
// when the test process ends without its cleanup. A MiniBrowser killed
// with SIGKILL leaves its GTK Broadway buffers in /dev/shm until a reboot,
// so the group runs with the recorder of broadwayshm, and the cleanup
// removes the buffers that it created once every process that recorded
// them ended.
func startProcess(t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	rec, err := broadwayshm.NewRecorder(os.TempDir())
	if err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	cmd := exec.Command(name, args...)
	cmd.Env = rec.Env(append(os.Environ(), env...))
	cmd.SysProcAttr = ownedProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if err := broadwayshm.Keep(rec.Dir, broadwayshm.End(rec.Log)); err != nil {
			t.Errorf("removing the Broadway buffers of %s: %v", name, err)
		}
	})
}

// broadwayd serves display :N on TCP port broadwayPortBase+N on every
// address, and a broadwayd whose port is taken exits, so MiniBrowser never
// lists a page. Several test processes run on one host at once, and
// outgoing connections take ports from the kernel's ephemeral range. So
// broadway picks N at random among the displays from broadwayMinDisplay
// (above the usual X displays) whose port lies below that range, checks
// that the port is free, and tries another N when broadwayd does not come
// up: when it exits, when another process took the port after the check,
// or when it does not listen in time.
const (
	broadwayPortBase   = 8080
	broadwayMinDisplay = 100
	broadwayTries      = 8
	broadwayListenWait = 5 * time.Second // a started broadwayd listens within this
	ephemeralRangeFile = "/proc/sys/net/ipv4/ip_local_port_range"
)

// broadway starts broadwayd on a free display and returns the display
// (":N"). The process group is killed at cleanup.
func broadway(t *testing.T) string {
	t.Helper()
	display, skipped, stop, err := startBroadway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if len(skipped) > 0 {
		t.Logf("broadwayd runs on %s after skipping %s", display, strings.Join(skipped, "; "))
	}
	return display
}

// startBroadway starts broadwayd as broadway describes. It returns the
// display, the displays it skipped with the reasons, and a function that
// kills the process group.
func startBroadway() (string, []string, func(), error) {
	low, err := ephemeralPortLow()
	if err != nil {
		return "", nil, nil, err
	}
	n := low - broadwayPortBase - broadwayMinDisplay // displays whose port is below low
	if n <= 0 {
		return "", nil, nil, fmt.Errorf("choosing a broadway display: no display from :%d has its port below the ephemeral range, which starts at %d", broadwayMinDisplay, low)
	}
	var skipped []string
	for range broadwayTries {
		d := broadwayMinDisplay + rand.IntN(n)
		port := broadwayPortBase + d
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			skipped = append(skipped, fmt.Sprintf(":%d (port busy: %v)", d, err))
			continue
		}
		_ = l.Close()
		display := fmt.Sprintf(":%d", d)
		stop, out, err := runBroadwayd(display, port)
		if err == nil {
			return display, skipped, stop, nil
		}
		if errors.Is(err, errSocketTables) {
			return "", nil, nil, fmt.Errorf("starting broadwayd on %s: %w; broadwayd output %q", display, err, out)
		}
		skipped = append(skipped, fmt.Sprintf("%s (%v; broadwayd output %q)", display, err, out))
	}
	return "", nil, nil, fmt.Errorf("broadwayd did not start on any of %d displays: %s", broadwayTries, strings.Join(skipped, "; "))
}

// runBroadwayd starts broadwayd on display and waits until it listens on
// port itself. A listener of another process on port means that this
// broadwayd cannot bind it. On failure it stops broadwayd and returns its
// output.
func runBroadwayd(display string, port int) (func(), string, error) {
	var out bytes.Buffer // read only after Wait returned
	cmd := exec.Command("broadwayd", display)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = ownedProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting broadwayd: %w", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		if afterBroadwaydWait != nil {
			afterBroadwaydWait()
		}
		close(done)
	}()
	stop := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	exited := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	for end := time.Now().Add(broadwayListenWait); ; time.Sleep(50 * time.Millisecond) {
		if exited() {
			return nil, out.String(), fmt.Errorf("exited before it listened: %v", waitErr)
		}
		owner, err := scanListener(cmd.Process.Pid, port)
		switch {
		case err != nil && exited(): // its open files went away with it
			return nil, out.String(), fmt.Errorf("exited before it listened: %v", waitErr)
		case errors.Is(err, errProcessGone): // reaped, and done is not closed yet
			stop()
			return nil, out.String(), fmt.Errorf("exited before it listened: %v", waitErr)
		case err != nil:
			stop()
			return nil, out.String(), err
		case owner == listenerOwn:
			return stop, "", nil
		case owner == listenerForeign:
			stop()
			return nil, out.String(), fmt.Errorf("another process listens on port %d", port)
		}
		if time.Now().After(end) {
			stop()
			return nil, out.String(), fmt.Errorf("not listening on port %d within %v", port, broadwayListenWait)
		}
	}
}

// Who listens on a TCP port, as portListener reports it.
const (
	listenerNone = iota
	listenerOwn
	listenerForeign
)

// errSocketTables marks a failure to read who listens on a port; another
// display does not help then.
var errSocketTables = errors.New("cannot tell who listens on the broadway port")

// errProcessGone: the process whose sockets portListener looks for has
// exited (its /proc entry is gone).
var errProcessGone = errors.New("the process has exited")

// afterBroadwaydWait, when set, runs in runBroadwayd after Wait reaped
// broadwayd and before runBroadwayd sees the exit. scanListener is the
// ownership scan of runBroadwayd. Tests replace both to order a scan
// between the two.
var (
	afterBroadwaydWait func()
	scanListener       = portListener
)

// socketTables are the kernel's TCP socket tables that portListener reads
// (a variable so that tests can point them elsewhere). A kernel without
// IPv6 has no tcp6 table, and then no IPv6 listener exists; the IPv4 table
// always exists.
var socketTables = struct{ tcp, tcp6 string }{"/proc/net/tcp", "/proc/net/tcp6"}

// portListener reports whether process pid, another process or nobody
// listens on TCP port. It takes the inodes of the listening sockets on
// port from the kernel's socket tables and looks for them among the socket
// links in /proc/<pid>/fd.
func portListener(pid, port int) (int, error) {
	inodes := map[string]bool{} // "socket:[inode]", as /proc/<pid>/fd links name sockets
	for _, table := range []string{socketTables.tcp, socketTables.tcp6} {
		b, err := os.ReadFile(table)
		if table == socketTables.tcp6 && errors.Is(err, fs.ErrNotExist) {
			continue // no IPv6 on this host
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %w", errSocketTables, err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for _, line := range lines[1:] { // the first line names the columns
			// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode ...
			f := strings.Fields(line)
			if len(f) < 10 {
				return 0, fmt.Errorf("%w: %s has a short line %q", errSocketTables, table, line)
			}
			_, hexPort, ok := strings.Cut(f[1], ":")
			p, err := strconv.ParseUint(hexPort, 16, 16)
			if !ok || err != nil {
				return 0, fmt.Errorf("%w: %s has the local address %q", errSocketTables, table, f[1])
			}
			const tcpListen = "0A" // TCP_LISTEN in the st column
			if int(p) == port && f[3] == tcpListen {
				inodes["socket:["+f[9]+"]"] = true
			}
		}
	}
	if len(inodes) == 0 {
		return listenerNone, nil
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: %w", errProcessGone, err)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errSocketTables, err)
	}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // closed since ReadDir
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %w", errSocketTables, err)
		}
		if inodes[link] {
			return listenerOwn, nil
		}
	}
	return listenerForeign, nil
}

// ephemeralPortLow returns the first port of the kernel's ephemeral range.
func ephemeralPortLow() (int, error) {
	b, err := os.ReadFile(ephemeralRangeFile)
	if err != nil {
		return 0, fmt.Errorf("choosing a broadway display: reading the ephemeral port range: %w", err)
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, fmt.Errorf("choosing a broadway display: %s holds %q, want two port numbers", ephemeralRangeFile, b)
	}
	low, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, fmt.Errorf("choosing a broadway display: %s holds %q: %w", ephemeralRangeFile, b, err)
	}
	return low, nil
}

// runBroadwayd counts only a listener of its own broadwayd. Another process
// can take the port between startBroadway's free-port check and this
// broadwayd's bind; then runBroadwayd stops its broadwayd and reports the
// port taken, so startBroadway tries another display. The fake broadwayd
// here starts and never listens.
func TestBroadwayIgnoresAForeignListener(t *testing.T) {
	foreign, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	port := foreign.Addr().(*net.TCPAddr).Port
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "broadwayd"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stop, out, err := runBroadwayd(fmt.Sprintf(":%d", port-broadwayPortBase), port)
	if err == nil {
		stop()
		t.Fatalf("runBroadwayd reported its broadwayd ready on port %d, which another process holds", port)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("another process listens on port %d", port)) {
		t.Fatalf("runBroadwayd err = %v (output %q), want another process on port %d", err, out, port)
	}
}

// A broadwayd that exits, because another process holds its port, is
// retried with another display, also when runBroadwayd scans the
// broadwayd's open files after Wait reaped it and before it saw the exit:
// the missing /proc entry means that broadwayd exited, not that the socket
// tables cannot be read. The hooks fix that order: the scan waits until
// Wait reaped the child, and runBroadwayd sees the exit only after the
// scan.
func TestBroadwaydExitIsRetryable(t *testing.T) {
	foreign, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	port := foreign.Addr().(*net.TCPAddr).Port
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "broadwayd"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	reaped, scanned := make(chan struct{}), make(chan struct{})
	var scans int
	var scanErr error
	afterBroadwaydWait = func() {
		close(reaped)
		<-scanned
	}
	scanListener = func(pid, port int) (int, error) {
		<-reaped
		owner, err := portListener(pid, port)
		if scans++; scans == 1 {
			scanErr = err
			close(scanned)
		}
		return owner, err
	}
	t.Cleanup(func() { afterBroadwaydWait, scanListener = nil, portListener })
	stop, out, err := runBroadwayd(fmt.Sprintf(":%d", port-broadwayPortBase), port)
	if err == nil {
		stop()
		t.Fatalf("runBroadwayd reported a broadwayd that exited ready on port %d", port)
	}
	if !errors.Is(scanErr, errProcessGone) {
		t.Fatalf("the scan after the reap returned %v, want errProcessGone", scanErr)
	}
	if !strings.Contains(err.Error(), "exited before it listened") {
		t.Fatalf("runBroadwayd err = %v (output %q), want a retryable exit before listening", err, out)
	}
}

// portListener finds a listener in the IPv4 table on a host without IPv6,
// where the tcp6 table does not exist. Every other problem with the tables
// still fails: a tcp6 table that exists but cannot be read, a missing IPv4
// table, and malformed data. The test process is the listener's owner.
func TestPortListenerTables(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte("header\n0: not-an-address 00000000:0000 0A 0 0 0 0 0 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := socketTables
	t.Cleanup(func() { socketTables = saved })
	absent := filepath.Join(dir, "absent")
	t.Run("absent tcp6", func(t *testing.T) {
		socketTables.tcp, socketTables.tcp6 = saved.tcp, absent
		if owner, err := portListener(os.Getpid(), port); err != nil || owner != listenerOwn {
			t.Fatalf("portListener = %d, %v; want listenerOwn (%d) from the IPv4 table", owner, err, listenerOwn)
		}
	})
	for _, tc := range []struct{ name, tcp, tcp6 string }{
		{"unreadable tcp6", saved.tcp, dir}, // a directory: it exists, and reading it fails
		{"absent tcp", absent, saved.tcp6},
		{"malformed tcp", malformed, saved.tcp6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketTables.tcp, socketTables.tcp6 = tc.tcp, tc.tcp6
			if _, err := portListener(os.Getpid(), port); !errors.Is(err, errSocketTables) {
				t.Fatalf("portListener err = %v, want errSocketTables", err)
			}
		})
	}
}

// browserStart bounds how long a test waits for MiniBrowser to list its
// page with a title. No product requirement bounds how long MiniBrowser
// takes to start; on a loaded host it took more than 15 s. This bound only
// guards against a hang of the external process.
const browserStart = 60 * time.Second

// dpiShim fixes the page geometry of WebKitGTK 2.54 under broadway (the
// same shim as e2e/webkitgtk's runtime harness; docs/notes/w3a.md): WebKit
// reads the page zoom from gdk_screen_get_resolution, which broadway
// leaves at -1, so every box would be empty and clicks and screenshots
// would find nothing. Preloading this function gives 96 DPI.
const dpiShim = "#include <gdk/gdk.h>\ngdouble gdk_screen_get_resolution(GdkScreen *s) { (void)s; return 96.0; }\n"

// buildDPIShim compiles dpiShim into dir and returns the library path.
func buildDPIShim(dir string) (string, error) {
	src, lib := filepath.Join(dir, "dpi.c"), filepath.Join(dir, "libdpi.so")
	if err := os.WriteFile(src, []byte(dpiShim), 0o600); err != nil {
		return "", err
	}
	flags, err := runBounded(buildTimeout, "", nil, "pkg-config", "--cflags", "gdk-3.0")
	if err != nil {
		return "", fmt.Errorf("pkg-config --cflags gdk-3.0 (needed for the WebKitGTK DPI shim): %w", err)
	}
	args := append([]string{"-shared", "-fPIC", "-o", lib, src}, strings.Fields(string(flags))...)
	if _, err := runBounded(buildTimeout, "", nil, "gcc", args...); err != nil {
		return "", fmt.Errorf("compiling the WebKitGTK DPI shim with gcc: %w", err)
	}
	return lib, nil
}

// fixtureDir holds the scenario pages (README.md, "Fixtures").
const fixtureDir = "fixtures"

// site is the fixture server of one test: the pages of fixtureDir, and
// /resource, which answers "from the server" and counts its requests. A
// client that intercepts /resource answers it itself, so the count shows
// whether a request reached the network.
type site struct {
	origin string

	mu        sync.Mutex
	resources int
}

// resourceBody is what /resource answers when no client intercepts it.
const resourceBody = "from the server"

// serveSite starts the fixture server on a free local port; it stops at
// cleanup.
func serveSite(t *testing.T) *site {
	t.Helper()
	s := &site{}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(fixtureDir)))
	mux.HandleFunc("/resource", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.resources++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(resourceBody))
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.origin = "http://" + l.Addr().String()
	return s
}

// resourceRequests returns how many requests /resource got.
func (s *site) resourceRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resources
}

// gtk is the WebKitGTK setup of one scenario test: the fixture site,
// MiniBrowser on its start page, and the in-process daemon on the
// webkitgtk transport with the CDP endpoint.
type gtk struct {
	*endpoint
	site *site
}

// startGTK starts the fixture server, broadwayd, MiniBrowser (with the DPI
// shim) on start (a path of the site) and the in-process daemon on the
// webkitgtk transport. Every process ends at cleanup. A fresh browser per
// test: WebKitGTK refuses quick reconnects to a page (spike A step B).
func startGTK(t *testing.T, start string) *gtk {
	t.Helper()
	s, addr := startMiniBrowser(t, start)
	return &gtk{endpoint: startDaemonOn(t, "webkitgtk", addr), site: s}
}

// startMiniBrowser starts the fixture server, broadwayd and MiniBrowser
// (with the DPI shim) on start, and waits until MiniBrowser lists the page
// with a title. It returns the site and MiniBrowser's inspector address.
func startMiniBrowser(t *testing.T, start string) (*site, string) {
	t.Helper()
	if _, err := os.Stat(miniBrowser); err != nil {
		t.Fatalf("WebKitGTK MiniBrowser not found at %s: %v; the client scenarios need MiniBrowser, broadwayd, gcc and pkg-config gdk-3.0 (README.md)", miniBrowser, err)
	}
	s := serveSite(t)
	shim, err := buildDPIShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	display := broadway(t)
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "LD_PRELOAD=" + shim},
		miniBrowser, s.origin+start)
	tr := webkitgtk.New(addr)
	listed := false
	for end := time.Now().Add(browserStart); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 && pages[0].Title != "" {
			listed = true
			break
		}
	}
	if !listed {
		t.Fatalf("MiniBrowser did not list the page %s with a title at %s within %v", start, addr, browserStart)
	}
	return s, addr
}
