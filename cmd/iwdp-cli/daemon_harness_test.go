package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/daemon"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil"
)

// realIWDPListing is the device list of a real iwdp, which no test reads.
const realIWDPListing = "127.0.0.1:9221"

// testDaemonIdle ends a test daemon soon after the tests stop using it.
const testDaemonIdle = "30s"

// testRuntimeDir is this test binary's private XDG_RUNTIME_DIR and
// XDG_STATE_HOME: CLI tests reach a daemon of this code there and never
// the user's daemon, iwdp or device.
var testRuntimeDir string

// userRuntimeDir is the XDG_RUNTIME_DIR the tests started with.
var userRuntimeDir string

func init() {
	// Every process of these tests (the tests, and the CLI and daemon runs
	// of the test binary) resolves page URLs against a private dead
	// listing, never the device list of the user's iwdp.
	iwdp.DefaultListingAddr = testutil.DeadIWDPListing()
	if os.Getenv(runMainEnv) == "1" {
		// A CLI or daemon process of a test (the test binary running
		// main): it inherits the private directories. It never starts the
		// real ios_webkit_debug_proxy.
		daemonStartIWDP = nil
		if os.Getenv("IWDP_DAEMON_IDLE") == "" {
			_ = os.Setenv("IWDP_DAEMON_IDLE", testDaemonIdle)
		}
		return
	}
	userRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	dir, err := os.MkdirTemp("", "iwdp-cli-test")
	if err != nil {
		panic(err)
	}
	testRuntimeDir = dir
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	// connectToPage called by in-process tests starts this test binary as
	// the daemon; it runs main() only with runMainEnv.
	daemonSpawnEnv = []string{runMainEnv + "=1", "IWDP_DAEMON_IDLE=" + testDaemonIdle}
	// Commands such as restart-iwdp run ios_webkit_debug_proxy and pkill
	// by name; the CLI runs of the tests find fakes first.
	fakeIWDPBin = filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeIWDPBin, 0o700); err != nil {
		panic(err)
	}
	if err := testutil.InstallFakeIWDP(fakeIWDPBin); err != nil {
		panic(err)
	}
}

// testDaemons maps each test to its own XDG_RUNTIME_DIR below
// testRuntimeDir (testRuntime). A daemon shared by the tests keeps the
// record of a page whose mock a test closed, as lost, and refuses the next
// mock that gets the same loopback URL with the lost-lease error (review
// finding R5-4); a daemon per test has no record of another test's pages.
var testDaemons sync.Map // *testing.T -> string

// testDaemonStops tracks the stops of the per-test daemons. They run in the
// background, because a daemon built with -race sleeps a second before it
// exits; stopTestDaemons waits for them, so no daemon outlives the tests.
var testDaemonStops sync.WaitGroup

// testRuntime returns t's own XDG_RUNTIME_DIR, created on first use. Every
// CLI run of t (cliCommand) and its in-process daemon calls (t.Setenv) use
// it; its daemon is stopped when t ends.
func testRuntime(t *testing.T) string {
	t.Helper()
	if dir, ok := testDaemons.Load(t); ok {
		return dir.(string)
	}
	dir, err := os.MkdirTemp(testRuntimeDir, "t")
	if err != nil {
		t.Fatal(err)
	}
	testDaemons.Store(t, dir)
	t.Cleanup(func() {
		testDaemons.Delete(t)
		testDaemonStops.Add(1)
		go func() {
			defer testDaemonStops.Done()
			stopDaemonIn(filepath.Join(dir, "iwdp"))
		}()
	})
	return dir
}

// fakeIWDPBin holds the fake iwdp executables (testutil.InstallFakeIWDP).
var fakeIWDPBin string

// No CLI test can start, stop or restart the real ios_webkit_debug_proxy,
// and no core of these tests reads its device list.
func TestCLITestsNeverRunTheRealIWDP(t *testing.T) {
	if err := testutil.CheckFakeIWDP(fakeIWDPBin); err != nil {
		t.Fatal(err)
	}
	if a := iwdp.DefaultListingAddr; a == realIWDPListing || !strings.HasPrefix(a, "127.0.0.1:") {
		t.Fatalf("iwdp's default listing is %s, want the private dead listing", a)
	}
}

// Task item 4: daemons that the CLI tests start live in the private
// runtime directory, never the user's, and never start the real iwdp (a
// daemon started with daemonSpawnEnv runs main with runMainEnv, which
// clears daemonStartIWDP).
func TestCLITestsUseAPrivateRuntimeDir(t *testing.T) {
	sock := daemon.SocketDir()
	if !strings.HasPrefix(sock, testRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is not below the private test dir %s", sock, testRuntimeDir)
	}
	if userRuntimeDir != "" && strings.HasPrefix(sock, userRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is in the user's runtime dir %s", sock, userRuntimeDir)
	}
	if p, err := daemon.LogPath(); err != nil || !strings.HasPrefix(p, testRuntimeDir) {
		t.Fatalf("daemon log %s (%v) is not below %s", p, err, testRuntimeDir)
	}
	if !slices.Contains(daemonSpawnEnv, runMainEnv+"=1") {
		t.Fatalf("daemonSpawnEnv %v lacks %s=1", daemonSpawnEnv, runMainEnv)
	}
	// cliCommand gives each CLI run the test's own directory below the
	// private dir; the last value is the one the process gets.
	var runtimeDir string
	for _, kv := range cliCommand(t, "", nil, "session").Env {
		if v, ok := strings.CutPrefix(kv, "XDG_RUNTIME_DIR="); ok {
			runtimeDir = v
		}
	}
	if runtimeDir != testRuntime(t) || !strings.HasPrefix(runtimeDir, testRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("CLI runs get XDG_RUNTIME_DIR %s, want the test's own dir %s below %s", runtimeDir, testRuntime(t), testRuntimeDir)
	}
}

// stopTestDaemons stops the daemons that the tests started under
// testRuntimeDir (also per-test directories below it) and removes it.
func stopTestDaemons() {
	if testRuntimeDir == "" {
		return
	}
	testDaemonStops.Wait()
	_ = filepath.WalkDir(testRuntimeDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "daemon.sock" {
			stopDaemonIn(filepath.Dir(p))
		}
		return nil
	})
	_ = os.RemoveAll(testRuntimeDir)
}

func stopDaemonIn(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: dir, NoStart: true})
	if err != nil {
		return
	}
	defer c.Close()
	pid := c.Hello.PID
	if c.Shutdown(ctx, false) == nil {
		_ = c.WaitGone(ctx, 7*time.Second)
	}
	// The socket goes before the process ends; wait for the process too,
	// so no daemon outlives the test binary.
	for end := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
}
