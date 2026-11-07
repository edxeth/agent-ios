//go:build webkitgtk

// Package webkitgtk is the local end-to-end suite on WebKitGTK (design
// docs/design/agentic-parity.md, sections 7.3 item 14 and 8.2). It needs
// broadwayd and the WebKitGTK MiniBrowser:
//
//	go test -tags=webkitgtk ./e2e/webkitgtk/ -v -count=1
package webkitgtk

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// Latency target of design 8.5: p50 of 50 warm CLI evals at or below
// 50 ms on WebKitGTK.
const (
	warmEvals = 50
	p50Target = 50 * time.Millisecond
)

// runtimeDir is the private XDG_RUNTIME_DIR and XDG_STATE_HOME of the
// CLI runs; cliBin is the iwdp-cli binary under test.
var runtimeDir, userRuntimeDir, cliBin string

// setupCLISuite prepares the CLI and daemon runs of the suite (called by
// TestMain in runtime_harness_test.go before the shared browser starts):
// a private runtime directory, the fake iwdp first on PATH and the iwdp-cli
// binary. The returned cleanup stops the suite's daemon and removes the
// directory.
func setupCLISuite() (func(), error) {
	userRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	dir, err := os.MkdirTemp("", "iwdp-gtk-")
	if err != nil {
		return nil, err
	}
	runtimeDir = dir
	cleanup := func() {
		_ = exec.Command(cliBin, "daemon", "stop").Run()
		_ = os.RemoveAll(dir)
	}
	_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	_ = os.Setenv("IWDP_DAEMON_IDLE", "30s")
	for _, k := range []string{"IWDP_SESSION", "IWDP_DEVICE_RESERVATION", "IWDP_TRANSPORT", "IWDP_DAEMON_DIR", "IWDP_WS_URL", "IWDP_TIMEOUT"} {
		_ = os.Unsetenv(k)
	}
	fakeIWDPBin = filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeIWDPBin, 0o700); err != nil {
		cleanup()
		return nil, err
	}
	// The suite's CLI runs and daemons never run the real iwdp.
	if err := testutil.InstallFakeIWDP(fakeIWDPBin); err != nil {
		cleanup()
		return nil, err
	}
	cliBin = filepath.Join(dir, "iwdp-cli")
	build := exec.Command("go", "build", "-o", cliBin, "../../cmd/iwdp-cli")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("building iwdp-cli: %w", err)
	}
	return cleanup, nil
}

// fakeIWDPBin holds the fake iwdp executables (testutil.InstallFakeIWDP).
var fakeIWDPBin string

// No test of this suite can start, stop or restart the real iwdp.
func TestWebKitGTKSuiteNeverRunsTheRealIWDP(t *testing.T) {
	if err := testutil.CheckFakeIWDP(fakeIWDPBin); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	stdout, stderr string
	code           int
	elapsed        time.Duration
}

func cli(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(cliBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String(), elapsed: time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running iwdp-cli %v: %v", args, err)
	}
	return r
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startProcess starts a command in its own process group and kills the
// group at cleanup, so no MiniBrowser or broadwayd survives the test.
func startProcess(t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
}

// browser starts broadwayd and MiniBrowser on a fixture page and returns
// the inspector address once it lists the page.
func browser(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(miniBrowser); err != nil {
		t.Fatalf("WebKitGTK MiniBrowser not found at %s: %v", miniBrowser, err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>Fixture</title><p>fixture</p>"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	display := fmt.Sprintf(":%d", 40+time.Now().Nanosecond()%50)
	startProcess(t, nil, "broadwayd", display)
	time.Sleep(500 * time.Millisecond)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr},
		miniBrowser, "http://"+l.Addr().String()+"/")
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 {
			return addr
		}
	}
	t.Fatalf("MiniBrowser did not list a page at %s within 15 s", addr)
	return ""
}

// Task item 4: the daemon of this suite lives in the private runtime
// directory, never the user's.
func TestWebKitGTKSuiteUsesAPrivateRuntimeDir(t *testing.T) {
	sock := daemon.SocketDir()
	if !strings.HasPrefix(sock, runtimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is not below the private dir %s", sock, runtimeDir)
	}
	if userRuntimeDir != "" && strings.HasPrefix(sock, userRuntimeDir+string(filepath.Separator)) {
		t.Fatalf("daemon socket dir %s is in the user's runtime dir %s", sock, userRuntimeDir)
	}
}

// Design 7.3 item 14: "eval 1+1" through the CLI and the daemon on
// WebKitGTK prints 2, and the p50 of 50 warm CLI evals is at or below
// 50 ms on this host.
func TestEvalThroughDaemonAndWarmLatency(t *testing.T) {
	addr := browser(t)
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	r := cli(t, "eval", "1+1")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "2" {
		t.Fatalf("eval 1+1: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	st := cli(t, "daemon", "status")
	if st.code != 0 || !strings.Contains(st.stdout, "Pages: 1") || !strings.Contains(st.stdout, "webkitgtk|webkitgtk-"+addr) || !strings.Contains(st.stdout, "connection attached") {
		t.Fatalf("daemon status after eval: exit %d, stdout %q; want the WebKitGTK page attached in the daemon", st.code, st.stdout)
	}

	times := make([]time.Duration, 0, warmEvals)
	for i := 0; i < warmEvals; i++ {
		r := cli(t, "eval", fmt.Sprintf("%d+1", i))
		if r.code != 0 || strings.TrimSpace(r.stdout) != fmt.Sprint(i+1) {
			t.Fatalf("warm eval %d: exit %d, stdout %q, stderr %q", i, r.code, r.stdout, r.stderr)
		}
		times = append(times, r.elapsed)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p50 := times[warmEvals/2-1]
	t.Logf("warm CLI evals (n=%d): min %v, p50 %v, p90 %v, max %v", warmEvals, times[0], p50, times[warmEvals*9/10-1], times[warmEvals-1])
	if p50 > p50Target {
		t.Fatalf("p50 of %d warm CLI evals is %v, want at most %v", warmEvals, p50, p50Target)
	}
}
