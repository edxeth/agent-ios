package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// e2eRuntimeDir is the private XDG_RUNTIME_DIR and XDG_STATE_HOME of the
// binaries these tests start: a daemon that one of them starts lives
// there, never in the user's runtime directory, and exits when TestMain
// removes the directory (the daemon exits when its socket goes away).
// userRuntimeDir is the directory the tests started with.
var e2eRuntimeDir, userRuntimeDir string

// e2eDaemonIdle ends a daemon that a test started soon after its use.
const e2eDaemonIdle = "10s"

func init() {
	userRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	dir, err := os.MkdirTemp("", "iwdp-e2e-run-*")
	if err != nil {
		panic(err)
	}
	trackTempDir(dir)
	e2eRuntimeDir = dir
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	_ = os.Setenv("IWDP_DAEMON_IDLE", e2eDaemonIdle)
	for _, k := range []string{"IWDP_SESSION", "IWDP_DEVICE_RESERVATION", "IWDP_TRANSPORT", "IWDP_DAEMON_DIR"} {
		_ = os.Unsetenv(k)
	}
	// devices, pages and status start iwdp when it does not
	// answer, and a daemon started by a test may too (D10): every binary
	// these tests start finds fake ios_webkit_debug_proxy and pkill first.
	e2eFakeBin = filepath.Join(dir, "bin")
	if err := os.Mkdir(e2eFakeBin, 0o700); err != nil {
		panic(err)
	}
	if err := testutil.InstallFakeIWDP(e2eFakeBin); err != nil {
		panic(err)
	}
}

// e2eFakeBin holds the fake iwdp executables (testutil.InstallFakeIWDP).
var e2eFakeBin string

// No e2e path can start, stop or restart the real ios_webkit_debug_proxy.
func TestE2ENeverRunsTheRealIWDP(t *testing.T) {
	if err := testutil.CheckFakeIWDP(e2eFakeBin); err != nil {
		t.Fatal(err)
	}
}

// Task item 4: every binary these tests start reaches a daemon in the
// private runtime directory, never the user's.
func TestE2EDaemonsUseAPrivateRuntimeDir(t *testing.T) {
	sock := daemon.SocketDir()
	if !strings.HasPrefix(sock, e2eRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is not below the private e2e dir %s", sock, e2eRuntimeDir)
	}
	if userRuntimeDir != "" && strings.HasPrefix(sock, userRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is in the user's runtime dir %s", sock, userRuntimeDir)
	}
	if p, err := daemon.LogPath(); err != nil || !strings.HasPrefix(p, e2eRuntimeDir) {
		t.Fatalf("daemon log %s (%v) is not below %s", p, err, e2eRuntimeDir)
	}
	for _, k := range []string{"IWDP_SESSION", "IWDP_DEVICE_RESERVATION", "IWDP_DAEMON_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s=%q leaks into the tested binaries", k, v)
		}
	}
}
