package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// connectResult is the outcome of one Connect.
type connectResult struct {
	hello Hello
	err   error
}

// connectAsync runs Connect with o in a goroutine and returns its result.
func connectAsync(t *testing.T, o Options) <-chan connectResult {
	t.Helper()
	ctx := ctxFor(t, 8*time.Second)
	ch := make(chan connectResult, 1)
	go func() {
		c, err := Connect(ctx, o)
		var h Hello
		if c != nil {
			h = c.Hello
			c.Close()
		}
		ch <- connectResult{h, err}
	}()
	return ch
}

// WS2 round-2 review part 2 F3, ported from the reviewer's
// TestReviewConcurrentIdleReplacement: client A asks the idle daemon of
// another protocol to exit and is held when the daemon has replied.
// Client B, of the same executable, must wait for the startup lock and
// then reuse the daemon that A starts. Before, A retired the daemon
// before it took the lock: B found the socket gone, started the new
// daemon, and A then waited for that new daemon to exit until its
// deadline.
func TestConcurrentClientsReplaceAnIdleDaemonOnce(t *testing.T) {
	o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
	f := startFakeDaemonOf(t, o.Dir, previousProtocol, 0)
	replied, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	saved := retireReplied
	retireReplied = func() {
		if held.CompareAndSwap(false, true) { // only the first client
			close(replied)
			<-release
		}
	}
	t.Cleanup(func() { retireReplied = saved })
	waiting := lockWaiter(t)
	a := connectAsync(t, o)
	select {
	case <-replied:
	case r := <-a:
		t.Fatalf("client A ended before the old daemon replied: %+v", r)
	case <-untilTestDeadline(t):
		t.Fatal("client A did not ask the old daemon to exit")
	}
	b := connectAsync(t, o)
	var rb connectResult
	bWaited := false
	select {
	case <-waiting:
		bWaited = true
	case rb = <-b:
	case <-untilTestDeadline(t):
		t.Fatal("client B neither waited for the startup lock nor ended")
	}
	close(release)
	ra := <-a
	if bWaited {
		rb = <-b
	}
	f.mu.Lock()
	n := f.shutdown
	f.mu.Unlock()
	if ra.err != nil || rb.err != nil {
		t.Fatalf("client A: %v; client B (waited for the lock: %v): %v; want both to reach the replacement daemon", ra.err, bWaited, rb.err)
	}
	if !bWaited || n != 1 || ra.hello.PID != rb.hello.PID || ra.hello.PID == 1 || ra.hello.Protocol != Protocol {
		t.Fatalf("B waited for the lock: %v, shutdown requests %d, A's daemon %+v, B's daemon %+v; want B to wait, one request and one shared daemon of protocol %d", bWaited, n, ra.hello, rb.hello, Protocol)
	}
}

// WS2 round-2 race check (cmd/agent-ios TestAnotherBinaryReplacesAnIdleDaemon):
// a daemon closes its listener before it releases run.lock (Run), and a
// daemon started before that release exits at once. So WaitGone waits
// until run.lock is free too, not only until the socket stops answering.
func TestWaitGoneWaitsForTheRunLock(t *testing.T) {
	dir := filepath.Join(shortDir(t), "agent-ios")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	c := newClient(dir) // no socket: no daemon answers
	if err := c.WaitGone(ctxFor(t, 5*time.Second), time.Second); err != nil {
		t.Fatalf("WaitGone without a socket or run.lock: %v; want nil", err)
	}
	l, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// The limit is the behavior under test: the held lock keeps WaitGone
	// waiting until it.
	const limit = 200 * time.Millisecond
	err = c.WaitGone(ctxFor(t, 5*time.Second), limit)
	if err == nil || !strings.Contains(err.Error(), "still holding "+filepath.Join(dir, "run.lock")) {
		t.Fatalf("WaitGone while a daemon holds run.lock: %v; want the error that names the held lock", err)
	}
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitGone(ctxFor(t, 5*time.Second), time.Second); err != nil {
		t.Fatalf("WaitGone after run.lock was released: %v; want nil", err)
	}
}

// WS2 round-2 race check, TestAnotherBinaryReplacesAnIdleDaemon (exit 1:
// "the agent-ios daemon exited at start"), with a real daemon (Run): a daemon
// that exits closes its listener first and then waits up to streamDrain
// for open connections before it releases run.lock. A connection that
// sent no request yet (held here) makes that wait last. A new daemon
// started in that window exits at once ("already running"), so after
// WaitGone a new daemon must start.
func TestDaemonStartsAfterWaitGone(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	idle, err := net.Dial("unix", SocketPath(o.Dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Close() }()
	c := connect(t, o)
	if err := c.Shutdown(ctxFor(t, 5*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitGone(ctxFor(t, 10*time.Second), stopWait); err != nil {
		t.Fatalf("WaitGone after the shutdown: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, stopped := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(stopped)
		done <- Run(ctx, RunOptions{Dir: o.Dir, Version: "test"})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	for {
		nc := newClient(o.Dir)
		_, err := nc.hello(ctxFor(t, time.Second))
		nc.Close()
		if err == nil {
			return // the new daemon serves
		}
		select {
		case err := <-done:
			t.Fatalf("the daemon started after WaitGone ended at start: %v", err)
		case <-untilTestDeadline(t):
			t.Fatal("the daemon started after WaitGone did not answer before the test's deadline")
		case <-time.After(startPoll):
		}
	}
}

// Round-3 review part 2 (ported from TestReviewConnectDuringExitDrain): a
// daemon that exits by itself (idle exit) or by another client's 'daemon
// stop' closes its listener before it releases run.lock. A Connect that
// finds no daemon in that window must wait for run.lock before it starts
// one; before, its daemon exited at once ("already running") and Connect
// failed. Here the test holds run.lock with no socket, as such a daemon.
func TestConnectWaitsForTheRunLockOfAnExitingDaemon(t *testing.T) {
	o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
	if err := EnsureDir(o.Dir); err != nil {
		t.Fatal(err)
	}
	l, err := os.OpenFile(filepath.Join(o.Dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := connectAsync(t, o)
	// The held lock is the fixture: a daemon still drains for this long.
	// A Connect that does not wait starts its daemon in this window.
	const drain = 750 * time.Millisecond
	select {
	case r := <-done:
		t.Fatalf("Connect returned while an exiting daemon held run.lock: %+v (daemon log: %s)", r, strings.TrimSpace(readLog(t)))
	case <-time.After(drain):
	}
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.hello.Protocol != Protocol {
		t.Fatalf("Connect after run.lock was released: %+v; want the started daemon", r)
	}
	if log := readLog(t); strings.Contains(log, "already running") {
		t.Fatalf("a daemon was started while run.lock was held; daemon log: %s", log)
	}
}

// holdRunLock takes run.lock of dir, as a daemon that runs holds it, and
// returns its release.
func holdRunLock(t *testing.T, dir string) func() {
	t.Helper()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Flock(int(l.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
	}
}

// Criterion 6c, round-4 review part 2 (ported from
// TestReviewRunLockWaitRespectsDeadline): the wait for a run.lock that a
// daemon without a listener holds ends at the command's deadline with the
// deadline error, and starts no daemon.
func TestRunLockWaitEndsAtTheDeadline(t *testing.T) {
	o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
	holdRunLock(t, o.Dir)
	// The deadline and its grace are the behavior under test: Connect
	// returns by the command deadline plus grace (round-5 review: a
	// Connect that returned 1 s late passed a wait for the test deadline).
	const deadline, grace = 300 * time.Millisecond, 300 * time.Millisecond
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(deadline))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, err := Connect(ctx, o)
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), filepath.Join(o.Dir, "run.lock")) {
			t.Fatalf("Connect while run.lock is held: %v; want the deadline error that names run.lock", err)
		}
	case <-time.After(time.Until(start.Add(deadline + grace))):
		t.Fatalf("Connect did not return within %s of its %s deadline while run.lock was held", grace, deadline)
	}
	if log := readLog(t); log != "" {
		t.Fatalf("a daemon was started while run.lock was held; daemon log: %s", log)
	}
}

// Criterion 6c, round-4 review part 2 (ported from
// TestReviewStartupAnswersWhileHoldingRunLock): a daemon that another
// process starts ('daemon run') takes run.lock before it listens. A
// Connect that waits for that lock reuses the daemon as soon as it
// answers, while the lock stays held.
func TestRunLockWaitReusesADaemonThatAnswers(t *testing.T) {
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	dir := filepath.Join(base, "agent-ios")
	holdRunLock(t, dir)
	// The executable to start does not exist, so a Connect that starts a
	// daemon fails: the answering daemon must be reused instead (round-5
	// review: a Connect that ignored the answer and started a daemon still
	// found the answering one in its start loop).
	exe := filepath.Join(base, "no-such-agent-ios")
	exePath, err := selfExe()
	if err != nil {
		t.Fatal(err)
	}
	self, err := identify(exePath)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	saved := runLockWaiting
	runLockWaiting = func() { once.Do(func() { close(waiting) }) }
	t.Cleanup(func() { runLockWaiting = saved })
	done := connectAsync(t, Options{Dir: dir, Exe: exe})
	// The daemon listens only after Connect entered the run.lock wait.
	select {
	case <-waiting:
	case r := <-done:
		t.Fatalf("Connect ended before it waited for run.lock: %+v", r)
	case <-untilTestDeadline(t):
		t.Fatal("Connect did not wait for the held run.lock")
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Hello{Protocol: Protocol, PID: 1234, Executable: self})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()
	select {
	case r := <-done:
		if r.err != nil || r.hello.PID != 1234 {
			t.Fatalf("Connect: %+v; want the daemon that answers (pid 1234)", r)
		}
	case <-untilTestDeadline(t):
		t.Fatal("Connect did not reuse the daemon that answers while run.lock stayed held")
	}
}
