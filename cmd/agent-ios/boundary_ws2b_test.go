package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	reg "github.com/nnemirovsky/agent-ios/internal/commands"
	"github.com/nnemirovsky/agent-ios/internal/daemon"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// wantFullDeadline fails unless res is the exit 124 of command name with
// the complete deadline message: the command, the page line, the elapsed
// time, the last wait and the likely causes.
func wantFullDeadline(t *testing.T, res cliResult, name string) {
	t.Helper()
	for _, want := range []string{"Error: agent-ios " + name + " timed out after ", "\nPage: ", "\nLast wait: ", "\nLikely causes:\n"} {
		if res.code != exitTimeout || !strings.Contains(res.stderr, want) {
			t.Fatalf("%s: exit %d, stderr %q; want exit %d and the full deadline message with %q", name, res.code, res.stderr, exitTimeout, want)
		}
	}
}

// runtimeDirOf is the XDG_RUNTIME_DIR of env.
func runtimeDirOf(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "XDG_RUNTIME_DIR="); ok {
			return v
		}
	}
	t.Fatalf("no XDG_RUNTIME_DIR in %v", env)
	return ""
}

// delayedDaemon returns the environment of CLI runs that reach the daemon
// of env through a relay socket. The relay forwards every request, and
// holds each POST /v1/run for hold after it arrived; it forwards the
// request even when the CLI has given up, as a slow handoff would.
// relayed receives the outcome of the first run's relay when it ended: nil
// after the daemon's whole answer, else the relay's error.
func delayedDaemon(t *testing.T, env []string, hold time.Duration) (relayEnv []string, relayed <-chan error) {
	t.Helper()
	target := daemon.SocketPath(filepath.Join(runtimeDirOf(t, env), "agent-ios"))
	dir, err := os.MkdirTemp(testRuntimeDir, "r")
	if err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(dir, "agent-ios")
	if err := daemon.EnsureDir(sockDir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", daemon.SocketPath(sockDir))
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", target)
	}}
	done := make(chan error, 1)
	var once sync.Once
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run := r.URL.Path == "/v1/run"
		var err error
		if run {
			// Every end of the run's relay reports, so the test never
			// waits for a relay that failed.
			defer func() { once.Do(func() { done <- err }) }()
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if run {
			time.Sleep(hold) // the late handoff is the behavior under test
		}
		out, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()), r.Method, "http://iwdp"+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := tr.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		f, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n]) // the CLI may have gone; the answer is still read to its end
				if f != nil {
					f.Flush()
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					err = rerr
				}
				break
			}
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); tr.CloseIdleConnections() })
	return []string{"XDG_RUNTIME_DIR=" + dir}, done
}

// WS2b-1b-a item 1 (review ws2a-r3-c F9): the daemon runs the operation
// under the command's absolute deadline. A request that reaches the daemon
// after it does no page work, and the CLI exits 124 with the full
// deadline message, though the daemon still answers within the CLI's
// delivery grace.
func TestLateDeliveryDoesNoPageWorkAndExits124(t *testing.T) {
	env, f := reservedPage(t)
	const budget = time.Second
	late, relayed := delayedDaemon(t, env, budget+100*time.Millisecond)
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	res := runCLI(t, "", late, "eval", "1", "--timeout", budget.String())
	select {
	case err := <-relayed:
		if err != nil {
			t.Fatalf("relaying the run to the daemon: %v", err)
		}
	case <-time.After(time.Until(testDeadline(t))):
		t.Fatal("the relay did not hand the run to the daemon")
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")) - before; n != 0 {
		t.Fatalf("eval reached the page %d time(s) after its deadline (exit %d, elapsed %v, stdout %q, stderr %q); want no page work", n, res.code, res.elapsed, res.stdout, res.stderr)
	}
	wantFullDeadline(t, res, "eval")
}

// testDeadline is the test's deadline, or a minute from now without one.
func testDeadline(t *testing.T) time.Time {
	if dl, ok := t.Deadline(); ok {
		return dl
	}
	return time.Now().Add(time.Minute)
}

// WS2b-1b-a item 2 (review ws2a-r3-c F11): every Local registry command
// gets the CLI's deadline message (a reporter) except "daemon run", which
// has no command deadline. Only the iwdp commands arm the watchdog, which
// would end this test process if any other command armed it (as the
// in-process fixture of TestRunRegistryCommandContextIsTheSignalsContext
// did in the first race run).
func TestEveryLocalCommandButDaemonRunReportsItsDeadline(t *testing.T) {
	saved := armWatchdog
	t.Cleanup(func() { armWatchdog = saved })
	var armed bool
	armWatchdog = func(time.Duration, *processExit) *time.Timer {
		armed = true
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	seen := map[string]bool{}
	for _, c := range reg.All() {
		if !c.Local {
			continue
		}
		seen[c.Name] = true
		armed = false
		got := startRegistry(ctx, &reg.Invocation{Cmd: c})
		if (got == nil) != (c.Name == daemonRun) {
			t.Errorf("startRegistry(%s) returned reporter %v; want one for every Local command but %s", c.Name, got, daemonRun)
		}
		if armed != iwdpCommands[c.Name] {
			t.Errorf("startRegistry(%s) armed the watchdog: %v; want it armed only for the iwdp commands %v", c.Name, armed, iwdpCommands)
		}
	}
	// devices runs in the daemon since 2026-11-12 (internal/commands/iwdp.go).
	for _, name := range []string{"daemon run", "daemon start", "daemon stop", "daemon status", "daemon logs", "status", "restart-iwdp"} {
		if !seen[name] {
			t.Errorf("%s is not a Local registry command", name)
		}
	}
}

// A command without a deadline is not recorded for the deadline message
// (commandRun.started), so it arms no watchdog.
func TestCommandWithoutADeadlineIsNotRecorded(t *testing.T) {
	r := &commandRun{}
	if budget, ok := r.started("daemon status", context.Background()); ok || budget != 0 || r.name != "" {
		t.Fatalf("started without a deadline = %v, %v, recorded %q; want 0, false and nothing recorded", budget, ok, r.name)
	}
}

// WS2b-1b-a item 2 (review ws2a-r3-c F11): "daemon start" that waits for
// the daemon start lock until its deadline prints the full message.
func TestDaemonStartUnderAHeldLockPrintsTheFullDeadline(t *testing.T) {
	env := privateDaemon(t)
	dir := filepath.Join(runtimeDirOf(t, env), "agent-ios")
	if err := daemon.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	wantFullDeadline(t, runCLI(t, "", env, "daemon", "start", "--timeout", "300ms"), "daemon start")
}

// silentDaemon returns the environment of CLI runs whose daemon socket
// accepts connections and never answers.
func silentDaemon(t *testing.T) []string {
	t.Helper()
	dir, err := os.MkdirTemp(testRuntimeDir, "s")
	if err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(dir, "agent-ios")
	if err := daemon.EnsureDir(sockDir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", daemon.SocketPath(sockDir))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return []string{"XDG_RUNTIME_DIR=" + dir}
}

// WS2b-1b-a item 2 (review ws2a-r3-c F11): "daemon status" and "daemon
// stop" on a daemon that accepts and never answers print the full message.
func TestDaemonStatusAndStopOnASilentDaemonPrintTheFullDeadline(t *testing.T) {
	env := silentDaemon(t)
	wantFullDeadline(t, runCLI(t, "", env, "daemon", "status", "--timeout", "300ms"), "daemon status")
	wantFullDeadline(t, runCLI(t, "", env, "daemon", "stop", "--timeout", "300ms"), "daemon stop")
}

// copyTestBinary copies this test binary into a fresh directory as
// agent-ios, with extra appended: an ELF loader ignores trailing bytes, so
// the copy runs as this binary does, and its content differs when extra
// is not empty.
func copyTestBinary(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent-ios")
	writeTestBinary(t, p, extra)
	return p
}

// writeTestBinary replaces the file at path with a copy of this test
// binary followed by extra (copyTestBinary): a new file renamed over the
// old one, as a build or an install replaces a binary.
func writeTestBinary(t *testing.T, path, extra string) {
	t.Helper()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, append(data, extra...), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// runBinary is runCLI with exe, a copy of the test binary, as agent-ios.
func runBinary(t *testing.T, exe string, env []string, args ...string) cliResult {
	t.Helper()
	cmd := cliCommand(t, "", env, args...)
	cmd.Path, cmd.Args[0] = exe, exe
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := cliResult{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s %v: %v", exe, args, err)
	}
	return res
}

// daemonPID runs "daemon status --json" with exe and returns the PID and
// the lease count of the daemon it reaches.
func daemonPID(t *testing.T, exe string, env []string) (pid, leases int) {
	t.Helper()
	res := runBinary(t, exe, env, "daemon", "status", "--json")
	var st daemon.Status
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &st) != nil || st.Daemon.PID == 0 {
		t.Fatalf("%s daemon status --json: exit %d, stdout %q, stderr %q", exe, res.code, res.stdout, res.stderr)
	}
	return st.Daemon.PID, st.Daemon.Leases
}

// startedPID runs "daemon start --json" with exe and returns the PID of
// the daemon it reports.
func startedPID(t *testing.T, exe string, env []string, args ...string) int {
	t.Helper()
	res := runBinary(t, exe, env, append([]string{"daemon", "start", "--json"}, args...)...)
	var h daemon.Hello
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &h) != nil || h.PID == 0 {
		t.Fatalf("%s daemon start --json: exit %d, stdout %q, stderr %q", exe, res.code, res.stdout, res.stderr)
	}
	return h.PID
}

// processEnded reports whether process pid has ended. A daemon started by
// a CLI run outlives that run, so a child subreaper (the quality gate's
// bounded.py) or init, not this test, reaps it; until then it is a zombie,
// which signal 0 still reaches.
func processEnded(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return syscall.Kill(pid, 0) != nil // gone, or a system without /proc
	}
	// The state follows the command name in parentheses, which can hold
	// any character.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}
	state := data[i+2]
	return state == 'Z' || state == 'X'
}

// waitExited waits until process pid has ended, at most until the test's
// deadline.
func waitExited(t *testing.T, pid int) {
	t.Helper()
	for !processEnded(pid) {
		if time.Now().After(testDeadline(t)) {
			t.Fatalf("daemon pid %d still runs", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WS2b-1b-a item 3 (review ws2a-r3-c F10): two binaries of different
// content (here with the same build information and commands) do not
// share a daemon: the second replaces the idle daemon of the first.
func TestAnotherBinaryReplacesAnIdleDaemon(t *testing.T) {
	env := privateDaemon(t)
	one, two := copyTestBinary(t, "build one"), copyTestBinary(t, "build two")
	first := startedPID(t, one, env)
	second := startedPID(t, two, env)
	if second == first {
		t.Fatalf("the second binary reused the idle daemon (pid %d) of the first", first)
	}
	waitExited(t, first)
	if pid, _ := daemonPID(t, two, env); pid != second {
		t.Fatalf("daemon status reaches pid %d, want the second binary's daemon %d", pid, second)
	}
}

// fakePhysicalPage is one fake page of a physical device behind a fake
// iwdp listing (physicalPage without a daemon); it returns the listing's
// address.
func fakePhysicalPage(t *testing.T) (*fakewebkit.Server, string) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10").AddPage("Fixture", "https://fixture.example/", f.URL)
	return f, l.Addr()
}

// leasedBy starts the daemon with exe on a fake page, reserves the device
// and runs eval, so that the default session holds a lease (the daemon is
// busy); it returns the daemon's PID.
func leasedBy(t *testing.T, exe string, env []string) (*fakewebkit.Server, int) {
	t.Helper()
	f, addr := fakePhysicalPage(t)
	pid := startedPID(t, exe, env, "--transport", "iwdp", "--addr", addr)
	if res := runBinary(t, exe, env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	if res := runBinary(t, exe, env, "eval", "1"); res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("eval 1: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if got, leases := daemonPID(t, exe, env); got != pid || leases == 0 {
		t.Fatalf("daemon pid %d with %d lease(s), want pid %d with a lease", got, leases, pid)
	}
	return f, pid
}

// WS2b-1b-a item 3: identical copies of one binary at different paths
// share one daemon, also while it holds leases.
func TestIdenticalCopiesShareABusyDaemon(t *testing.T) {
	env := privateDaemon(t)
	_, pid := leasedBy(t, os.Args[0], env)
	other := copyTestBinary(t, "")
	if got := startedPID(t, other, env); got != pid {
		t.Fatalf("an identical copy at another path started daemon %d, want the busy daemon %d", got, pid)
	}
	if res := runBinary(t, other, env, "eval", "1"); res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("eval 1 from the copy: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if got, _ := daemonPID(t, other, env); got != pid {
		t.Fatalf("daemon pid %d, want %d", got, pid)
	}
}

// WS2b-1b-a item 3 (review ws2a-r3-c F10): after the binary at a path is
// replaced by other content, a run of the new binary finds the busy daemon
// of the old one: it fails with the advice to run 'agent-ios daemon stop'
// and does no page work, while 'daemon status' and 'daemon stop' still
// reach that daemon.
func TestReplacedBinaryGetsTheAdviceForABusyDaemon(t *testing.T) {
	env := privateDaemon(t)
	exe := copyTestBinary(t, "old build")
	f, pid := leasedBy(t, exe, env)
	writeTestBinary(t, exe, "new build")
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	res := runBinary(t, exe, env, "eval", "1")
	if res.code == 0 || !strings.Contains(res.stderr, "is running with active sessions; run 'agent-ios daemon stop' when they finish") {
		t.Fatalf("eval 1 from the replaced binary: exit %d, stdout %q, stderr %q; want the daemon stop advice", res.code, res.stdout, res.stderr)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")) - before; n != 0 {
		t.Fatalf("the replaced binary evaluated %d time(s) through the old daemon", n)
	}
	if got, _ := daemonPID(t, exe, env); got != pid {
		t.Fatalf("daemon status from the replaced binary reaches pid %d, want the busy daemon %d", got, pid)
	}
	res = runBinary(t, exe, env, "daemon", "stop")
	if res.code != 0 || !strings.Contains(res.stdout, "Stopped the agent-ios daemon") {
		t.Fatalf("daemon stop from the replaced binary: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	waitExited(t, pid)
}
