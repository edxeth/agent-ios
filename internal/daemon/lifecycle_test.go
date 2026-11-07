package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// TestMain makes the test binary act as "<exe> daemon run", so the
// auto-start tests start real daemon processes of this code.
func TestMain(m *testing.M) {
	// Every daemon of these tests resolves page URLs against a private
	// dead listing, never the device list of the user's iwdp.
	iwdp.DefaultListingAddr = testutil.DeadIWDPListing()
	if len(os.Args) >= 3 && os.Args[1] == "daemon" && os.Args[2] == "run" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		err := Run(ctx, RunOptions{Version: "test"})
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "daemon:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Every default path (SocketDir, LogPath) points into a private
	// directory, so no test reaches the user's daemon (task item 4).
	userRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	dir, err := os.MkdirTemp("", "iwdpd-run")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testRuntimeDir = dir
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	_ = os.Unsetenv("IWDP_DAEMON_DIR")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testRuntimeDir is the private XDG_RUNTIME_DIR of these tests;
// userRuntimeDir is the one they started with.
var testRuntimeDir, userRuntimeDir string

func TestDaemonTestsNeverReadTheRealIWDPListing(t *testing.T) {
	if a := iwdp.DefaultListingAddr; a == "127.0.0.1:9221" || !strings.HasPrefix(a, "127.0.0.1:") {
		t.Fatalf("iwdp's default listing is %s, want the private dead listing", a)
	}
	// The dead listing answers like an iwdp that is not running.
	if listingAnswers(ctxFor(t, 5*time.Second), iwdp.DefaultListingAddr) {
		t.Fatalf("the private listing %s answers", iwdp.DefaultListingAddr)
	}
}

func TestDaemonTestsUseAPrivateRuntimeDir(t *testing.T) {
	sock := SocketDir()
	if !strings.HasPrefix(sock, testRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("SocketDir %s is not below the private test dir %s", sock, testRuntimeDir)
	}
	if userRuntimeDir != "" && strings.HasPrefix(sock, userRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("SocketDir %s is in the user's runtime dir %s", sock, userRuntimeDir)
	}
}

// shortDir is a fresh directory with a short path: a Unix socket path must
// stay under 108 bytes, and t.TempDir paths carry the test name.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "iwdpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// spawnOptions are client options that auto-start this test binary as the
// daemon in a private socket directory, with the log in that directory.
func spawnOptions(t *testing.T, env ...string) Options {
	t.Helper()
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	o := Options{Dir: filepath.Join(base, "iwdp"), Exe: os.Args[0], Env: env, Version: "test"}
	t.Cleanup(func() { stopDaemon(t, o) })
	return o
}

func stopDaemon(t *testing.T, o Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	o.NoStart = true
	c, err := Connect(ctx, o)
	if err != nil {
		return
	}
	defer c.Close()
	pid := c.Hello.PID
	_ = c.Shutdown(ctx, false)
	waitGone(t, pid, 8*time.Second)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for alive(pid) {
		if time.Since(start) > limit {
			t.Fatalf("daemon pid %d still running after %v", pid, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func readLog(t *testing.T) string {
	t.Helper()
	p, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	return string(data)
}

// 7.3 item 11: ten clients that find no daemon at the same moment start
// exactly one (flock on daemon.lock, design 2.3).
func TestAutoStartConcurrentClientsStartOneDaemon(t *testing.T) {
	o := spawnOptions(t, "IWDP_DAEMON_IDLE=1m")
	const n = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	pids := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := Connect(ctxFor(t, 10*time.Second), o)
			if err != nil {
				errs[i] = err
				return
			}
			defer c.Close()
			pids[i] = c.Hello.PID
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("client %d: %v", i, errs[i])
		}
		if pids[i] != pids[0] || pids[i] == os.Getpid() {
			t.Fatalf("clients reached daemons %v, want one daemon process", pids)
		}
	}
	log := readLog(t)
	if got := strings.Count(log, "listening on"); got != 1 || strings.Contains(log, "already running") {
		t.Fatalf("daemon log shows %d starts (want 1):\n%s", got, log)
	}
	fi, err := os.Stat(SocketPath(o.Dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %s: %v, mode %v; want 0600", SocketPath(o.Dir), err, fi.Mode())
	}
}

// fakeOldDaemon answers /v1/hello with another protocol number, as an
// older or newer daemon on the same socket would.
type fakeOldDaemon struct {
	mu       sync.Mutex
	leases   int
	shutdown int
	l        net.Listener
}

func startFakeOldDaemon(t *testing.T, dir string, leases int) *fakeOldDaemon {
	t.Helper()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOldDaemon{leases: leases, l: l}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(Hello{Version: "0.4.0", Protocol: Protocol + 1, PID: 1, Leases: f.leases})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.shutdown++
		f.mu.Unlock()
		_, _ = w.Write([]byte("{\"ok\":true}\n"))
		go func() { _ = l.Close() }() // removes the socket file
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// 7.3 item 11 and design 2.2: a client of another protocol asks an idle
// daemon to exit and starts its own; with active leases it fails with the
// documented message and leaves the daemon alone.
func TestProtocolMismatch(t *testing.T) {
	o := spawnOptions(t, "IWDP_DAEMON_IDLE=1m")
	f := startFakeOldDaemon(t, o.Dir, 2)
	_, err := Connect(ctxFor(t, 5*time.Second), o)
	want := fmt.Sprintf("daemon version 0.4.0 (protocol %d) is running with active sessions; run 'iwdp-cli daemon stop' when they finish", Protocol+1)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Connect with a busy old daemon: %v; want %q", err, want)
	}
	f.mu.Lock()
	f.leases = 0
	n := f.shutdown
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("client asked a daemon with leases to exit")
	}
	c, err := Connect(ctxFor(t, 8*time.Second), o)
	if err != nil {
		t.Fatalf("Connect with an idle old daemon: %v", err)
	}
	defer c.Close()
	f.mu.Lock()
	n = f.shutdown
	f.mu.Unlock()
	if n != 1 || c.Hello.Protocol != Protocol || c.Hello.PID == 1 {
		t.Fatalf("shutdown requests %d, hello %+v; want one request and a daemon of protocol %d", n, c.Hello, Protocol)
	}
}

// openLeasedPage leases a fake page for session s1 through the daemon and
// ends the operation; the lease stays.
func openLeasedPage(t *testing.T, c *Client, f *fakewebkit.Server, sends ...string) {
	t.Helper()
	pc, err := c.OpenPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}, Name: "test"})
	if err != nil {
		t.Fatalf("OpenPage: %v", err)
	}
	for _, m := range sends {
		if _, err := pc.Send(ctxFor(t, 5*time.Second), m, nil); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// 7.3 item 11 and design 2.3: the daemon exits after IWDP_DAEMON_IDLE with
// no requests and no leases; an active lease keeps it running.
func TestIdleExitWaitsForLeases(t *testing.T) {
	o := spawnOptions(t, "IWDP_DAEMON_IDLE=300ms")
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	c, err := Connect(ctxFor(t, 5*time.Second), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid := c.Hello.PID
	openLeasedPage(t, c, f)
	time.Sleep(1200 * time.Millisecond)
	if !alive(pid) {
		t.Fatalf("daemon exited while session s1 held a lease")
	}
	if _, err := c.Lease(ctxFor(t, 5*time.Second), LeaseRequest{Op: "release", Owner: Owner{Session: "s1"}}); err != nil {
		t.Fatalf("lease release: %v", err)
	}
	waitGone(t, pid, 5*time.Second)
	if _, err := os.Lstat(SocketPath(o.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind after idle exit: %v", err)
	}
}

// 7.3 item 11 and design 2.3: SIGTERM runs the cleanup of every page within
// 5 s and removes the socket.
func TestSIGTERMRunsCleanupWithin5s(t *testing.T) {
	o := spawnOptions(t, "IWDP_DAEMON_IDLE=1m")
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	c, err := Connect(ctxFor(t, 5*time.Second), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid := c.Hello.PID
	openLeasedPage(t, c, f, "Network.enable")
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if took := waitGone(t, pid, 5*time.Second); took > 5*time.Second {
		t.Fatalf("SIGTERM shutdown took %v", took)
	}
	if len(f.ReceivedMethod("Network.disable")) != 1 {
		t.Fatalf("cleanup did not release Network; the fake received %v", f.Received())
	}
	if _, err := os.Lstat(SocketPath(o.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind after SIGTERM: %v", err)
	}
}

// 7.3 item 11: a socket directory that is a symlink or has a mode other
// than 0700 makes the daemon refuse to start, and auto-start refuses it
// too. (Another owner's directory is covered by TestCheckDir with a uid.)
func TestUnsafeSocketDirRefused(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	for _, dir := range []string{link, open} {
		if err := Run(ctxFor(t, 5*time.Second), RunOptions{Dir: dir, Version: "test"}); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("Run in %s: %v, want ErrUnsafeDir", dir, err)
		}
		if _, err := Connect(ctxFor(t, 5*time.Second), Options{Dir: dir, Exe: os.Args[0]}); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("Connect in %s: %v, want ErrUnsafeDir", dir, err)
		}
		if _, err := os.Lstat(SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a socket was created in %s", dir)
		}
	}
}

// A second daemon on the same directory refuses to start and leaves the
// first one's socket in place.
func TestSecondDaemonRefused(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	err := Run(ctxFor(t, 5*time.Second), RunOptions{Dir: o.Dir, Version: "test"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Run: %v, want already running", err)
	}
	c, err := Connect(ctxFor(t, 2*time.Second), o)
	if err != nil {
		t.Fatalf("first daemon unreachable after the refused second start: %v", err)
	}
	c.Close()
}
