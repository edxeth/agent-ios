package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/proxy"
)

// The serve fake iwdp of these tests is that of the internal/proxy tests
// (start_test.go: installFakeIWDP with fakeServe, and TestFakeIWDPProcess):
// a script named ios_webkit_debug_proxy, first on PATH, that runs this
// test binary as TestFakeIWDPServe. It serves an empty device list on the
// private address of fakeIWDPAddrEnv and ends when its parent, the test
// process of fakeIWDPParentEnv, is gone.
const (
	fakeIWDPAddrEnv   = "AGENT_IOS_COMMANDS_TEST_FAKE_IWDP_ADDR"
	fakeIWDPParentEnv = "AGENT_IOS_COMMANDS_TEST_FAKE_IWDP_PARENT"
)

// TestFakeIWDPServe is the body of the serve fake iwdp; without
// fakeIWDPAddrEnv it returns at once.
func TestFakeIWDPServe(t *testing.T) {
	addr := os.Getenv(fakeIWDPAddrEnv)
	if addr == "" {
		return
	}
	parent, err := strconv.Atoi(os.Getenv(fakeIWDPParentEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake iwdp: no parent pid:", err)
		os.Exit(3)
	}
	go func() {
		for os.Getppid() == parent {
			time.Sleep(100 * time.Millisecond)
		}
		os.Exit(3)
	}()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake iwdp:", err)
		os.Exit(3)
	}
	err = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	fmt.Fprintln(os.Stderr, "fake iwdp:", err)
	os.Exit(3)
}

// installServeFakeIWDP puts the serve fake iwdp for addr, and an
// idevice_id that exits 1 (no UDID is pinned, usbmuxd is never reached),
// first on PATH, with private runtime and state directories for the iwdp
// records and log. It returns the file in which each start of the fake
// appends its pid; the cleanup kills those pids.
func installServeFakeIWDP(t *testing.T, addr string) string {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	starts := filepath.Join(dir, "starts.log")
	script := "#!/bin/sh\necho $$ >> " + q(starts) + "\nexec env " + fakeIWDPParentEnv + "=" + strconv.Itoa(os.Getpid()) + " " + fakeIWDPAddrEnv + "=" + q(addr) + " " + q(exe) + " -test.run='^TestFakeIWDPServe$'\n"
	if err := os.WriteFile(filepath.Join(dir, "ios_webkit_debug_proxy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "idevice_id"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		for _, pid := range fakeStarts(t, starts) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // by its recorded pid, never by name
		}
	})
	return starts
}

// fakeStarts returns the pids that the starts of the fake recorded.
func fakeStarts(t *testing.T, starts string) []int {
	t.Helper()
	data, err := os.ReadFile(starts)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("starts.log holds %q", f)
		}
		pids = append(pids, pid)
	}
	return pids
}

// freeLoopbackAddr returns a loopback address that nothing listens on.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// WS2 round-2 review: the successful result of restart-iwdp had no test.
// Its output is that of the legacy restart-iwdp: the text line
// "ios_webkit_debug_proxy restarted", and with --json {"restarted":true}.
// With no iwdp on the address it starts one (the help: "When no iwdp
// runs, it starts one"); then that recorded iwdp is stopped and a new one
// started. The fake iwdp serves a private address, never iwdp's.
func TestRestartIWDPSucceeds(t *testing.T) {
	addr := freeLoopbackAddr(t)
	starts := installServeFakeIWDP(t, addr)
	saved := restartIWDP
	restartIWDP = proxy.NewStarter(addr).Restart
	t.Cleanup(func() { restartIWDP = saved })

	code, out, stderr, _ := runCLI("restart-iwdp")
	if code != ExitOK || out != "ios_webkit_debug_proxy restarted\n" {
		t.Fatalf("restart-iwdp with no iwdp: exit %d, stdout %q, stderr %q; want exit 0 and the restarted line", code, out, stderr)
	}
	if err := proxy.CheckListing(t.Context(), addr); err != nil {
		t.Fatalf("after restart-iwdp the listing %s: %v; want the started fake to answer", addr, err)
	}
	code, out, stderr, _ = runCLI("--json", "restart-iwdp")
	var data map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &data) != nil || len(data) != 1 || data["restarted"] != true {
		t.Fatalf("--json restart-iwdp of the recorded iwdp: exit %d, stdout %q, stderr %q; want exit 0 and {\"restarted\":true}", code, out, stderr)
	}
	pids := fakeStarts(t, starts)
	if len(pids) != 2 {
		t.Fatalf("the fake iwdp started %d time(s) (pids %v); want 2: the start and the restart", len(pids), pids)
	}
	// The restart stopped the first iwdp (its process group).
	if err := syscall.Kill(pids[0], 0); !errors.Is(err, syscall.ESRCH) && !zombie(pids[0]) {
		t.Fatalf("the first fake iwdp, pid %d, still runs after the restart (kill 0: %v)", pids[0], err)
	}
	if err := proxy.CheckListing(t.Context(), addr); err != nil {
		t.Fatalf("after the restart the listing %s: %v; want the new fake to answer", addr, err)
	}
}

// zombie reports whether pid has ended but is not reaped yet (Linux
// /proc state Z); on other systems it reports false.
func zombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(data), ')')
	return i >= 0 && i+2 < len(data) && data[i+2] == 'Z'
}
