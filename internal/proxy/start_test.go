package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Hermetic tests of the iwdp start (design decision D10): every Starter
// here uses a private loopback address and a fake ios_webkit_debug_proxy on
// PATH, never the real iwdp, the device or port 9221 of the host.

// fakeServeEnv makes TestFakeIWDPProcess serve an iwdp device list on the
// address it names.
const fakeServeEnv = "IWDP_PROXY_TEST_FAKE_SERVE"

// fakeParentEnv is the pid of the test process that starts a fake that
// runs the test binary (fakeParent); the fake ends when that process ends
// (exitWithParent).
const fakeParentEnv = "IWDP_PROXY_TEST_FAKE_PARENT"

// fakeParent is the fakeParentEnv entry of this test process.
func fakeParent() string { return fakeParentEnv + "=" + strconv.Itoa(os.Getpid()) }

// fakeParentPoll is how often a fake checks that its test process runs.
const fakeParentPoll = 100 * time.Millisecond

// exitWithParent ends this fake process, after stop, once its parent is no
// longer the test process of fakeParentEnv: the fake runs in its own
// process group, so a killed test binary does not take it along, and an
// orphan gets another parent (init or a subreaper), on Linux and macOS
// alike. The fake's parent is that test process: it runs the fake with
// exec.Command, or a Starter runs the fake's script, which execs the test
// binary. Without the variable the fake keeps running.
func exitWithParent(stop func()) {
	want, err := strconv.Atoi(os.Getenv(fakeParentEnv))
	if err != nil {
		return
	}
	go func() {
		for os.Getppid() == want {
			time.Sleep(fakeParentPoll)
		}
		stop()
		fmt.Fprintln(os.Stderr, "fake iwdp: its test process ended")
		os.Exit(3)
	}()
}

// TestFakeIWDPProcess is the body of the "serve" fake iwdp, which runs the
// test binary with -test.run of this name: it serves an empty device list
// on the address of fakeServeEnv until it is stopped. Without the variable
// it returns at once.
func TestFakeIWDPProcess(t *testing.T) {
	addr := os.Getenv(fakeServeEnv)
	if addr == "" {
		return
	}
	exitWithParent(func() {})
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

// fakeIWDP is a fake ios_webkit_debug_proxy installed first on PATH.
type fakeIWDP struct {
	t   *testing.T
	dir string
}

// Fake modes. Each fake appends "<pid> <args>" to starts.log first.
const (
	fakeServe    = "serve"    // serves the device list on the Starter's address
	fakeFail     = "fail"     // exits 1
	fakeHang     = "hang"     // starts a child, writes its pid to child.pid, never serves
	fakeStubborn = "stubborn" // like hang, but it and its child ignore SIGTERM
	// fakeChatty writes "fake-stderr-line" to stderr and, through sed (a
	// stdio program, block-buffered when its stdout is a file, as iwdp's
	// stdout is), "fake-stdout-line" to stdout, then serves as fakeServe.
	// sed ends when the fake has exited.
	fakeChatty = "chatty"
)

// shellExitWithParent is exitWithParent for a shell fake whose child's pid
// is in $c: instead of waiting for the child, the shell polls its parent
// pid (ps, on Linux and macOS) and, once it is no longer this test
// process, kills the child and exits. A SIGKILL of the fake's process
// group still ends both at once.
func shellExitWithParent() string {
	return "while [ \"$(ps -o ppid= -p $$ | tr -d ' ')\" = " + strconv.Itoa(os.Getpid()) + " ]; do sleep 0.1; done\nkill -9 $c 2>/dev/null\nexit 3\n"
}

// installFakeIWDP writes the fake iwdp of mode for the listing addr and an
// idevice_id that exits 1 (DeviceUDIDs must not reach usbmuxd), and puts
// them first on PATH for this test.
//
// It also gives the test its own XDG_STATE_HOME, so a started fake writes
// its log (LogPath) there and never in the user's state directory, and its
// own XDG_RUNTIME_DIR, so the iwdp records and start locks of this test are
// its own. Records are named by the listing address, and addresses repeat
// between the tests of one package run (freeAddr picks at random), so a
// shared directory hands one test the record that another test left: for
// example the group-writable record of
// TestRestartRefusesAnUnsafeRecordStore, which os.WriteFile keeps
// group-writable when a later test rewrites it.
func installFakeIWDP(t *testing.T, mode, addr string) *fakeIWDP {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	var body string
	switch mode {
	case fakeServe:
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		body = "exec env " + fakeParent() + " " + fakeServeEnv + "=" + q(addr) + " " + q(exe) + " -test.run='^TestFakeIWDPProcess$'\n"
	case fakeChatty:
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		body = "printf 'fake-stderr-line\\n' >&2\nme=$$\n" +
			"(printf 'fake-stdout-line\\n'; while kill -0 $me 2>/dev/null; do sleep 0.2; done) | sed -n p &\n" +
			"exec env " + fakeParent() + " " + fakeServeEnv + "=" + q(addr) + " " + q(exe) + " -test.run='^TestFakeIWDPProcess$'\n"
	case fakeFail:
		body = "exit 1\n"
	case fakeHang:
		body = "sleep 300 &\nc=$!\necho $c > " + q(filepath.Join(dir, "child.pid")) + "\n" + shellExitWithParent()
	case fakeStubborn:
		body = "trap '' TERM\nsleep 300 &\nc=$!\necho $c > " + q(filepath.Join(dir, "child.pid")) + "\n" + shellExitWithParent()
	default:
		t.Fatalf("unknown fake mode %q", mode)
	}
	script := "#!/bin/sh\necho \"$$ $*\" >> " + q(filepath.Join(dir, "starts.log")) + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "ios_webkit_debug_proxy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "idevice_id"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &fakeIWDP{t: t, dir: dir}
	// A started fake that a test leaves running (a red run) is stopped by
	// its recorded pid, never by name.
	t.Cleanup(func() {
		for _, pid := range f.pids() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return f
}

// starts returns the recorded starts, one per line.
func (f *fakeIWDP) starts() []string {
	data, err := os.ReadFile(filepath.Join(f.dir, "starts.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// pids returns the pids of every started fake and of the child of a hang
// or stubborn fake.
func (f *fakeIWDP) pids() []int {
	var pids []int
	lines := f.starts()
	if data, err := os.ReadFile(filepath.Join(f.dir, "child.pid")); err == nil {
		lines = append(lines, strings.TrimSpace(string(data)))
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// alive reports whether pid is a live process. A zombie (state Z in
// /proc on Linux) has exited and counts as gone.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state follows the parenthesized command name.
	s := string(data)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

// childPIDs returns the live child processes of this test process (Linux;
// nil where /proc has no children list).
func childPIDs(t *testing.T) []int {
	t.Helper()
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", os.Getpid()))
	var pids []int
	for _, p := range tasks {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(f); err == nil && alive(pid) {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// Ports of freeAddr: below the ephemeral ranges of Linux (32768-60999) and
// macOS (49152-65535), so a listener on port 0 of a concurrent test or
// package cannot take the address and answer for an iwdp that never does.
const (
	freePortMin   = 20000
	freePortCount = 12000
)

// freeAddr is a loopback address where nothing listens.
func freeAddr(t *testing.T) string {
	t.Helper()
	for range 100 {
		addr := fmt.Sprintf("127.0.0.1:%d", freePortMin+rand.IntN(freePortCount))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = l.Close()
		return addr
	}
	t.Fatalf("no free loopback port in %d-%d", freePortMin, freePortMin+freePortCount-1)
	return ""
}

// silentListing accepts connections on a loopback address and never
// answers, like an iwdp whose event loop is stuck.
func silentListing(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
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
	return l.Addr().String()
}

// testStarter is a Starter for addr with a short readiness wait.
func testStarter(addr string, readyWait time.Duration) *Starter {
	s := NewStarter(addr)
	s.readyWait = readyWait
	return s
}

// Requirement 1: while a process owns the listing port and does not
// answer, Start starts nothing and says why.
func TestStartNeverStartsWhileTheListingPortIsOwned(t *testing.T) {
	addr := silentListing(t)
	f := installFakeIWDP(t, fakeFail, addr)
	begin := time.Now()
	err := testStarter(addr, time.Second).Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded against a listing that never answers")
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("Start ran ios_webkit_debug_proxy %d times while %s was owned, want 0", n, addr)
	}
	for _, want := range []string{addr, "does not answer", "busy or stuck", "no second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	// Requirement 2: the probe waits the whole shared budget.
	if d := time.Since(begin); d < ProbeBudget {
		t.Errorf("Start gave up after %s, before the %s probe budget", d, ProbeBudget)
	}
}

// Requirement 2: a listing that answers after 3 s (past the old 2 s probe
// of the daemon, within the 5 s budget) counts as running.
func TestCheckListingWaitsForTheSharedBudget(t *testing.T) {
	if ProbeBudget != 5*time.Second {
		t.Fatalf("ProbeBudget is %s, want 5s (the budget ProbeBudget documents)", ProbeBudget)
	}
	srv := slowListing(t, 3*time.Second)
	if err := CheckListing(context.Background(), srv); err != nil {
		t.Fatalf("a listing answering after 3s: %v", err)
	}
}

// slowListing serves the device list after delay.
func slowListing(t *testing.T, delay time.Duration) string {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-time.After(delay):
		case <-done:
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return strings.TrimPrefix(srv.URL, "http://")
}

// A caller whose context ends during the check gets that error, not the
// busy-iwdp error.
func TestCheckListingReturnsTheCallersContextError(t *testing.T) {
	addr := silentListing(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := CheckListing(ctx, addr)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "busy") {
		t.Fatalf("CheckListing with an ending context = %v, want context.DeadlineExceeded", err)
	}
}

// Start uses the Starter of the default listing address (std; a private
// running listing here).
func TestStartUsesTheDefaultStarter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	f := installFakeIWDP(t, fakeFail, addr)
	old := std
	t.Cleanup(func() { std = old })
	std = NewStarter(addr)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start with iwdp running: %v", err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times with iwdp running, want 0", n)
	}
}

// A free port is free: CheckListing says so at once, so a dead iwdp costs
// no probe budget.
func TestCheckListingReportsAFreePortAtOnce(t *testing.T) {
	addr := freeAddr(t)
	begin := time.Now()
	err := CheckListing(context.Background(), addr)
	if !errors.Is(err, ErrNotListening) {
		t.Fatalf("CheckListing(%s) = %v, want ErrNotListening", addr, err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Errorf("CheckListing of a free port took %s", d)
	}
}

// Another process on the port that answers, but not with 200, is not iwdp
// and is never displaced.
func TestStartNeverStartsWhileAnotherServerOwnsThePort(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	f := installFakeIWDP(t, fakeFail, addr)
	err := testStarter(addr, time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), addr) {
		t.Fatalf("Start against an HTTP 404 listing = %v, want an error naming %s and HTTP 404", err, addr)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("Start ran ios_webkit_debug_proxy %d times, want 0", n)
	}
}

// A running iwdp is left alone: Start returns nil without a start.
func TestStartLeavesARunningIWDPAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	f := installFakeIWDP(t, fakeFail, addr)
	if err := testStarter(addr, time.Second).Start(context.Background()); err != nil {
		t.Fatalf("Start with a running iwdp: %v", err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("Start ran ios_webkit_debug_proxy %d times with iwdp running, want 0", n)
	}
}

// Requirement 4: on a free port, concurrent calls start exactly one iwdp,
// and all of them succeed once it answers.
func TestStartStartsOnceForConcurrentCalls(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Start(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Start: %v", err)
		}
	}
	if got := f.starts(); len(got) != 1 {
		t.Fatalf("%d concurrent Start calls ran ios_webkit_debug_proxy %d times (%q), want 1", callers, len(got), got)
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Fatalf("the started fake iwdp does not answer: %v", err)
	}
}

// Requirement 3: a started iwdp that never answers is stopped with its
// process group before Start returns; no process of it is left.
func TestStartStopsAnIWDPThatNeverAnswers(t *testing.T) {
	for _, mode := range []string{fakeHang, fakeStubborn} {
		t.Run(mode, func(t *testing.T) {
			addr := freeAddr(t)
			f := installFakeIWDP(t, mode, addr)
			// The readiness wait leaves the fake time to start its child.
			err := testStarter(addr, 2*time.Second).Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), "did not answer") {
				t.Fatalf("Start of a fake that never answers = %v, want a readiness error", err)
			}
			if !strings.Contains(err.Error(), "run '"+wantManualIWDP()+"' to see why") {
				t.Errorf("readiness error %q does not name %q", err, wantManualIWDP())
			}
			pids := f.pids()
			if len(pids) != 2 {
				t.Fatalf("recorded pids %v, want the fake and its child", pids)
			}
			for _, pid := range pids {
				if alive(pid) {
					t.Errorf("process %d of the failed start is still running after Start returned", pid)
				}
			}
			if left := childPIDs(t); len(left) != 0 {
				t.Errorf("child processes %v are left after Start returned", left)
			}
		})
	}
}

// Requirement 3 for a caller that gives up: the started process is
// stopped, and the cancellation does not start the back-off.
func TestStartStopsTheProcessWhenTheCallerGivesUp(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeHang, addr)
	s := testStarter(addr, 30*time.Second)
	for i := 1; i <= 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := s.Start(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start %d with an ending context = %v, want context.DeadlineExceeded", i, err)
		}
		for _, pid := range f.pids() {
			if alive(pid) {
				t.Errorf("process %d is still running after Start %d returned", pid, i)
			}
		}
	}
	if n := len(f.starts()); n != 2 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 2 (a cancelled start does not back off)", n)
	}
}

// Requirement 4: after a failed start, later calls fail fast with that
// error and start nothing until the back-off ends.
func TestStartBacksOffAfterAFailedStart(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	s := testStarter(addr, 5*time.Second)
	first := s.Start(context.Background())
	if first == nil || !strings.Contains(first.Error(), "exited") {
		t.Fatalf("Start of a fake that exits 1 = %v, want an exit error", first)
	}
	if !strings.Contains(first.Error(), "run '"+wantManualIWDP()+"' to see why") {
		t.Errorf("exit error %q does not name %q", first, wantManualIWDP())
	}
	const quick = 5
	begin := time.Now()
	for i := range quick {
		err := s.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), first.Error()) || !strings.Contains(err.Error(), "not trying again") {
			t.Fatalf("call %d in the back-off = %v, want the first failure %q and the back-off", i, err, first)
		}
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("%d calls in the back-off took %s, want a fast failure", quick, d)
	}
	if got := f.starts(); len(got) != 1 {
		t.Fatalf("a failed start and %d calls ran ios_webkit_debug_proxy %d times, want 1", quick, len(got))
	}
	// The back-off ends after StartBackoff (moved back here instead of
	// waiting 30 s).
	s.mu.Lock()
	s.failedAt = s.failedAt.Add(-StartBackoff)
	s.mu.Unlock()
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("the fake that exits 1 started after the back-off")
	}
	if got := f.starts(); len(got) != 2 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times after the back-off ended, want 2", len(got))
	}
}

// A missing ios_webkit_debug_proxy is a failed start too: the install hint,
// then the back-off.
func TestStartReportsAMissingBinaryAndBacksOff(t *testing.T) {
	addr := freeAddr(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "idevice_id"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	s := testStarter(addr, time.Second)
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "brew install ios-webkit-debug-proxy") {
		t.Fatalf("Start without the binary = %v, want the install hint", err)
	}
	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "not trying again") {
		t.Fatalf("the next Start = %v, want the back-off", err)
	}
}

// boundAddr binds a private loopback address without listening on it, as
// a process does between bind(2) and listen(2). Connections to it are
// refused, yet the address is owned.
//
// The bind itself picks the address: a port in use is skipped. A port that
// freeAddr reports free can still refuse this bind, because freeAddr's
// listener sets SO_REUSEADDR and this socket does not: a TIME_WAIT
// connection that an earlier test left on the port (for example
// TestPortFreeIgnoresTimeWait, or a stopped fake iwdp) blocks only this
// bind, for up to a minute.
func boundAddr(t *testing.T) string {
	t.Helper()
	for range 100 {
		port := freePortMin + rand.IntN(freePortCount)
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}, Port: port})
		if errors.Is(err, syscall.EADDRINUSE) {
			_ = syscall.Close(fd)
			continue
		}
		if err != nil {
			_ = syscall.Close(fd)
			t.Fatalf("binding 127.0.0.1:%d: %v", port, err)
		}
		t.Cleanup(func() { _ = syscall.Close(fd) })
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	t.Fatalf("no unbound loopback port in %d-%d", freePortMin, freePortMin+freePortCount-1)
	return ""
}

// Review round 1, F1: a socket that is bound but does not listen owns the
// address although it refuses connections; Start starts nothing.
func TestStartNeverStartsWhileTheListingAddressIsBound(t *testing.T) {
	addr := boundAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	err := testStarter(addr, time.Second).Start(context.Background())
	if err == nil || errors.Is(err, ErrNotListening) || !strings.Contains(err.Error(), addr) {
		t.Fatalf("Start on a bound address = %v, want an ownership error naming %s", err, addr)
	}
	if got := f.starts(); len(got) != 0 {
		t.Fatalf("Start ran ios_webkit_debug_proxy %d times (%q) while %s was bound", len(got), got, addr)
	}
}

// F1 for a host name: every address it resolves to must be free. A refused
// ::1 does not hide a bound 127.0.0.1.
func TestCheckListingNeedsEveryResolvedAddressFree(t *testing.T) {
	addr := boundAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	err := CheckListing(context.Background(), net.JoinHostPort("localhost", port))
	if err == nil || errors.Is(err, ErrNotListening) {
		t.Fatalf("CheckListing(localhost:%s) with 127.0.0.1:%s bound = %v, want an ownership error", port, port, err)
	}
}

// Review round 1, F2: a listener that appears while device discovery runs
// (after the first check, before the spawn) owns the port; Start starts
// nothing and leaves no process.
func TestStartRechecksThePortAfterDeviceDiscovery(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeHang, addr)
	entered, release := filepath.Join(f.dir, "entered"), filepath.Join(f.dir, "release")
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\necho ready > " + q(entered) + "\nwhile [ ! -f " + q(release) + " ]; do sleep 0.01; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.dir, "idevice_id"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("go"), 0o600) })
	result := make(chan error, 1)
	go func() { result <- testStarter(addr, 5*time.Second).Start(context.Background()) }()
	waitFile(t, entered)
	// Another owner binds and serves before the spawn.
	serveListing(t, addr)
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return")
	}
	if got := f.starts(); len(got) != 0 {
		t.Errorf("Start ran ios_webkit_debug_proxy %d times (%q) after another listener took %s", len(got), got, addr)
	}
	for _, pid := range f.pids() {
		if alive(pid) {
			t.Errorf("process %d is still running after Start returned", pid)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("Start = %v, want an error that another process took the port", err)
	}
}

// F2, readiness: a listener of another process that appears after the
// spawn is not the started iwdp. Start stops its own process group and
// says another process took the port.
func TestStartStopsItsProcessWhenAnotherTakesThePort(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeHang, addr)
	result := make(chan error, 1)
	go func() { result <- testStarter(addr, 20*time.Second).Start(context.Background()) }()
	waitFile(t, filepath.Join(f.dir, "child.pid"))
	serveListing(t, addr)
	var err error
	select {
	case err = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return")
	}
	_, port, _ := net.SplitHostPort(addr)
	if err == nil || !strings.Contains(err.Error(), "another process took port "+port) {
		t.Fatalf("Start = %v, want \"another process took port %s\"", err, port)
	}
	pids := f.pids()
	if len(pids) != 2 {
		t.Fatalf("recorded pids %v, want the fake and its child", pids)
	}
	for _, pid := range pids {
		if alive(pid) {
			t.Errorf("process %d of the start is still running after Start returned", pid)
		}
	}
	if got := f.starts(); len(got) != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1 and no other", len(got))
	}
}

// waitFile waits until path exists.
func waitFile(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear", path)
}

// serveListing serves an empty device list on addr from this test process.
func serveListing(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}
