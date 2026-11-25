package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// runBinIn runs bin with args with a private state and runtime directory
// and returns its stdout, its stderr and its exit code.
func runBinIn(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	return runBinEnv(t, []string{"XDG_STATE_HOME=" + t.TempDir()}, bin, args...)
}

// runBinEnv is runBinIn with the environment additions env (the state
// directory among them) and a private runtime directory.
func runBinEnv(t *testing.T, env []string, bin string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir()), env...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	t.Fatalf("running %s %v: %v", bin, args, err)
	return "", "", 0
}

// WS2a brief, section 3: status, devices and restart-iwdp are registry
// commands that need no page, so they take the registry's global flags:
// --json prints their data.
func TestIWDPCommandsPrintJSON(t *testing.T) {
	listing, device := fakeDeviceListing()
	bin := buildCLIFor(t, listing.Addr())
	stdout, stderr, code := runBinIn(t, bin, "--json", "status")
	var st struct {
		Running *bool  `json:"running"`
		Log     string `json:"log"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &st) != nil || st.Running == nil || !*st.Running || !strings.HasSuffix(st.Log, "iwdp.log") {
		t.Errorf("--json status on a healthy listing: exit %d, stdout %q, stderr %q; want running true and the log path", code, stdout, stderr)
	}
	stdout, stderr, code = runBinIn(t, bin, "devices", "--json")
	var devs []map[string]string
	if code != 0 || json.Unmarshal([]byte(stdout), &devs) != nil || len(devs) != 1 ||
		devs[0]["deviceName"] != e2eDeviceName || devs[0]["deviceId"] != e2eDeviceID || devs[0]["url"] != device.Addr() {
		t.Errorf("devices --json: exit %d, stdout %q, stderr %q; want the one fake device", code, stdout, stderr)
	}
	for _, name := range []string{"status", "devices", "restart-iwdp"} {
		stdout, _, code := runBinIn(t, bin, name, "--bogus")
		if code != 2 {
			t.Errorf("%s --bogus: exit %d, stdout %q; want the registry usage error (exit 2)", name, code, stdout)
		}
	}
}

// A listing without devices: devices prints the legacy line (exit 0), and
// --json an empty list.
func TestDevicesWithoutDevices(t *testing.T) {
	bin := buildCLIFor(t, newFakeListing([]any{}).Addr())
	if stdout, stderr, code := runBinIn(t, bin, "devices"); code != 0 || stdout != "No devices connected.\n" {
		t.Errorf("devices on an empty listing: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stdout, stderr, code := runBinIn(t, bin, "--json", "devices"); code != 0 || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("--json devices on an empty listing: exit %d, stdout %q, stderr %q; want []", code, stdout, stderr)
	}
}

// Device check 3, P3-D: status names why the iwdp log's path is unknown;
// --json carries that reason as logError.
func TestStatusJSONNamesAnUnknownLogPath(t *testing.T) {
	listing, _ := fakeDeviceListing()
	stdout, stderr, code := runBinEnv(t, []string{"XDG_STATE_HOME=", "HOME="}, buildCLIFor(t, listing.Addr()), "status", "--json")
	var st struct {
		Running  bool   `json:"running"`
		Log      string `json:"log"`
		LogError string `json:"logError"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &st) != nil || !st.Running || st.Log != "" || !strings.Contains(st.LogError, "no XDG_STATE_HOME and no home directory") {
		t.Errorf("status --json without a state or home directory: exit %d, stdout %q, stderr %q; want running, no log and the reason", code, stdout, stderr)
	}
}

// The not-running answer of status keeps the legacy streams: what it found
// and the iwdp log on stdout, the start advice of this system on stderr,
// exit 1. With --json, stdout holds the data instead.
func TestStatusNotRunningStreams(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	bin := buildCLIFor(t, addr)
	start := "Start it with: ios_webkit_debug_proxy --no-frontend"
	if runtime.GOOS == "linux" {
		start += " -s 127.0.0.1:27753"
	}
	stdout, stderr, code := runBinIn(t, bin, "status")
	if code != 1 || !strings.HasPrefix(stdout, "ios_webkit_debug_proxy is NOT running\niwdp log of an iwdp that agent-ios starts: ") || !strings.Contains(stderr, start+"\n") {
		t.Errorf("status without iwdp: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, _, code = runBinIn(t, bin, "status", "--json")
	var st struct {
		Running *bool  `json:"running"`
		Log     string `json:"log"`
	}
	if code != 1 || json.Unmarshal([]byte(stdout), &st) != nil || st.Running == nil || *st.Running || st.Log == "" {
		t.Errorf("status --json without iwdp: exit %d, stdout %q; want running false with the log path, exit 1", code, stdout)
	}
}

// restart-iwdp refuses an iwdp listing address that a process owns but
// that no agent-ios start recorded (decision L02): it stops nothing, exits
// 1 and names the address and the manual restart command. The owner here
// is this test's private dead listing, never iwdp's default address.
func TestRestartIWDPRefusesAnUnrecordedOwner(t *testing.T) {
	addr := deadCLIListing().Addr()
	stdout, stderr, code := runBinIn(t, buildCLI(t), "restart-iwdp")
	if code != 1 || !strings.Contains(stderr, "restart refused: "+addr) || !strings.Contains(stderr, "ios_webkit_debug_proxy --no-frontend") || stdout != "" {
		t.Errorf("restart-iwdp on an unrecorded owner: exit %d, stdout %q, stderr %q; want the refusal with exit 1", code, stdout, stderr)
	}
}

// stalledListing is an iwdp listing whose GET /json answers the first
// answered requests with an empty device list and holds every later one
// until the client goes or the test ends: a listing that passes the
// command's check and then stalls its read.
func stalledListing(t *testing.T, answered int64) string {
	t.Helper()
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > answered {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close, which waits for the handlers
	return strings.TrimPrefix(srv.URL, "http://")
}

// wantDeadlineMessage checks the CLI's deadline error of command (exit
// 124, cmd/agent-ios/deadline.go timeoutMessage) and that the command
// itself ended at its deadline: the watchdog, which ends a command that
// ignores its deadline 2 s later, did not.
func wantDeadlineMessage(t *testing.T, command, stdout, stderr string, code int) {
	t.Helper()
	if code != 124 || !strings.Contains(stderr, "Error: agent-ios "+command+" timed out after") || !strings.Contains(stderr, "--timeout <duration>") || strings.Contains(stderr, "watchdog") {
		t.Errorf("%s past its deadline: exit %d, stdout %q, stderr %q; want exit 124 with the CLI's deadline message, ended by the command's deadline", command, code, stdout, stderr)
	}
}

// Review ws2a-a F3 (WS2a round 2): devices reads the device list under
// the command deadline. The listing answers the check, then stalls the
// read past --timeout: exit 124 with the CLI's deadline message.
func TestDevicesHonorsTheDeadline(t *testing.T) {
	bin := buildCLIFor(t, stalledListing(t, 1))
	stdout, stderr, code := runBinIn(t, bin, "devices", "--timeout", "100ms")
	wantDeadlineMessage(t, "devices", stdout, stderr, code)
	// Review ws2a-r2-a F7: the last wait names the command deadline that
	// ended the read, not the read's own 5 s limit.
	if strings.Contains(stderr, "within 5s") || !strings.Contains(stderr, "before the command deadline") {
		t.Errorf("devices --timeout 100ms: stderr %q; want the last wait to name the command deadline, not 5s", stderr)
	}
}

// WS2a round 2: status checks the listing under the command deadline; a
// listing that never answers ends it at --timeout with the CLI's deadline
// message (exit 124).
func TestStatusHonorsTheDeadline(t *testing.T) {
	bin := buildCLIFor(t, stalledListing(t, 0))
	stdout, stderr, code := runBinIn(t, bin, "status", "--timeout", "200ms")
	wantDeadlineMessage(t, "status", stdout, stderr, code)
}

// WS2a round 2: restart-iwdp waits for another process's iwdp start (the
// start lock, held here by the test) under the command deadline, and ends
// at --timeout with the CLI's deadline message (exit 124). It starts and
// stops nothing: the lock is never free.
func TestRestartIWDPHonorsTheDeadline(t *testing.T) {
	addr := deadCLIListing().Addr()
	dir := t.TempDir()
	name := fmt.Sprintf("agent-ios-start-%d-%s.lock", os.Getuid(), strings.ReplaceAll(addr, ":", "_")) // proxy.runtimeName
	lock, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("taking the start lock: %v", err)
	}
	stdout, stderr, code := runBinEnv(t, []string{"XDG_STATE_HOME=" + t.TempDir(), "XDG_RUNTIME_DIR=" + dir}, buildCLI(t), "restart-iwdp", "--timeout", "300ms")
	wantDeadlineMessage(t, "restart-iwdp", stdout, stderr, code)
	if !strings.Contains(stderr, "waiting for another process's iwdp start") {
		t.Errorf("restart-iwdp stderr %q; want the wait for the start lock as the last wait", stderr)
	}
}
