package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"testing/quick"
	"time"
)

// shutdownEnd is how a generated old daemon ends the shutdown request of
// retire (TestRetireProperty).
type shutdownEnd uint8

const (
	endOK        shutdownEnd = iota // 200 {"ok":true}
	endErrorBody                    // 200 with an error body (it refuses)
	endClose                        // closes the connection without an answer (EOF)
	endReset                        // closes it with the request unread (ECONNRESET)
	end503                          // HTTP 503
	end302                          // a redirect, whose target closes without an answer
	shutdownEnds
)

var shutdownEndNames = [...]string{"200", "200-error-body", "close", "reset", "503", "302"}

func (e shutdownEnd) String() string { return shutdownEndNames[e] }

// goingAway reports whether the end means that the daemon goes away (it
// accepted the request, or closed the connection without an answer), by
// the contract of retire; every other end is the daemon's answer.
func (e shutdownEnd) goingAway() bool { return e == endOK || e == endClose || e == endReset }

// retirePlan is a generated old daemon: a daemon of the previous protocol
// that a client of this protocol replaces (Connect, retire).
type retirePlan struct {
	leases int
	end    shutdownEnd
	// exits: after the shutdown request it exits after exitDelay: it
	// closes its listener (which removes the socket), waits gap, then
	// releases run.lock. Else it keeps run.lock and its listener.
	exits bool
	// answers: after the shutdown request it still answers hellos until
	// it exits; else it closes every connection without an answer.
	answers bool
	gap     time.Duration
	// lockDelay delays the replacement daemon before it takes run.lock
	// (lockDelayEnv), as a slowly scheduled process.
	lockDelay time.Duration
}

func (p retirePlan) String() string {
	return fmt.Sprintf("leases %d, shutdown request ends with %s, exits %v, answers hellos %v, gap %s, replacement lock delay %s", p.leases, p.end, p.exits, p.answers, p.gap, p.lockDelay)
}

// lockDelayEnv names the delay of a test daemon before Run takes run.lock
// (TestMain).
const lockDelayEnv = "IWDP_TEST_LOCK_DELAY"

// exitDelay is how long a generated daemon that exits takes after the
// shutdown request before it closes its socket.
const exitDelay = 20 * time.Millisecond

// refusal is the error body of endErrorBody.
const refusal = "sessions are active"

// retireDaemon is the fake daemon of a retirePlan.
type retireDaemon struct {
	mu        sync.Mutex
	shutdowns int
	after     bool        // the shutdown request has come
	exit      *time.Timer // the exit after the shutdown request
	ended     bool        // the test ended
	exiting   sync.WaitGroup
	lock      *os.File // its run.lock
}

// holdsRunLock probes run.lock through the old daemon's own open file
// without waiting: false when another holder (the replacement) has it.
// True means that the old daemon holds it, or that nobody held it and the
// probe took it for the old daemon; the cleanup releases it.
func (f *retireDaemon) holdsRunLock(t *testing.T) bool {
	err := syscall.Flock(int(f.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	if err != nil {
		t.Errorf("probing run.lock of the old daemon: %v", err)
	}
	return true
}

func startRetireDaemon(t *testing.T, dir string, p retirePlan) *retireDaemon {
	t.Helper()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	f := &retireDaemon{lock: lock}
	exit := func() {
		defer f.exiting.Done()
		_ = l.Close() // removes the socket
		time.Sleep(p.gap)
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Errorf("releasing run.lock of the old daemon: %v", err)
		}
	}
	scheduleExit := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.ended && f.exit == nil {
			f.exiting.Add(1)
			f.exit = time.AfterFunc(exitDelay, exit)
		}
	}
	t.Cleanup(func() { // before the listener and run.lock close
		f.mu.Lock()
		f.ended = true
		if f.exit != nil && f.exit.Stop() {
			f.exiting.Done() // the exit never runs
		}
		f.mu.Unlock()
		f.exiting.Wait()
	})
	hello := fmt.Sprintf("{\"version\":\"0.4.0\",\"protocol\":%d,\"pid\":1,\"leases\":%d}", previousProtocol, p.leases)
	reply := func(conn net.Conn, status, body string) {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, len(body), body)
	}
	const shutdownLine = "POST /v1/shutdown "
	handle := func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		head := make([]byte, len(shutdownLine))
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		shutdown := string(head) == shutdownLine
		f.mu.Lock()
		after := f.after
		if shutdown {
			f.shutdowns++
			f.after = true
		}
		f.mu.Unlock()
		if shutdown && p.end == endReset {
			if p.exits {
				scheduleExit()
			}
			return // the rest of the request is unread: the client gets ECONNRESET
		}
		req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(head), conn)))
		if err != nil {
			return
		}
		switch {
		case shutdown:
			switch p.end {
			case endOK:
				reply(conn, "200 OK", "{\"ok\":true}")
			case endErrorBody:
				reply(conn, "200 OK", "{\"ok\":false,\"error\":{\"code\":1,\"message\":\""+refusal+"\"}}")
			case end503:
				reply(conn, "503 Service Unavailable", "broken")
			case end302:
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", redirectTarget)
			}
			if p.exits {
				scheduleExit()
			}
		case after && !p.answers: // going away: no answer
		case req.URL.Path == "/v1/hello":
			reply(conn, "200 OK", hello)
		case req.URL.Path == redirectTarget: // closed without an answer
		default:
			reply(conn, "404 Not Found", "")
		}
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return f
}

// Orchestrator item (retire decides when a replacement daemon may start):
// a property of Connect and its retire against an old daemon of the
// previous protocol. Every combination of the discrete dimensions runs:
// leases or none, how the old daemon ends the shutdown request, whether it
// then exits or keeps run.lock, and whether it keeps answering hellos.
// testing/quick generates the timings of each combination: the gap
// between the old daemon's socket close and its run.lock release (0 to
// 300 ms) and a delay of the replacement daemon before it takes run.lock
// (0 to 150 ms). The expected results come from the documented contract
// (Connect, retire, WaitGone), not from the code:
//
//	(a) Connect succeeds only after the old daemon has closed its socket
//	    and released run.lock;
//	(b) no new daemon starts while the old one holds run.lock: a probe
//	    of run.lock at the moment a daemon process starts
//	    (spawnStarting) finds it free; a failed Connect starts none;
//	(c) an HTTP answer to the shutdown request (a 3xx, a 5xx, an error
//	    body) is the daemon's answer, never "going away": Connect fails
//	    with it at once;
//	(d) with leases, Connect fails with the 'daemon stop' advice and
//	    sends no shutdown request.
//
// A daemon that accepts the request (200), or closes or resets its
// connection, is going away: Connect waits for it and starts a daemon
// once it is gone, or reaches its deadline while it stays. The deadline of
// those waits is short (waitBound), not the real stopWait.
func TestRetireProperty(t *testing.T) {
	const waitBound = 200 * time.Millisecond
	gaps := [...]time.Duration{0, 30 * time.Millisecond, 120 * time.Millisecond, 300 * time.Millisecond}
	lockDelays := [...]time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond}
	gapSeen := map[time.Duration]bool{}
	delaySeen := map[time.Duration]bool{}
	seed := int64(20261111)
	for _, leases := range []int{0, 1} {
		for end := shutdownEnd(0); end < shutdownEnds; end++ {
			for _, exits := range []bool{false, true} {
				for _, answers := range []bool{false, true} {
					base := retirePlan{leases: leases, end: end, exits: exits, answers: answers}
					// The timings matter where the old daemon goes away and
					// exits: Connect then waits for it and starts a daemon.
					timed := leases == 0 && end.goingAway() && exits
					runs := 1
					if timed {
						runs = 2
					}
					seed++
					name := fmt.Sprintf("leases=%d/%s/exits=%v/answers=%v", leases, end, exits, answers)
					t.Run(name, func(t *testing.T) {
						run := 0
						prop := func(gapStep, delayStep uint8) bool {
							p := base
							p.gap = gaps[int(gapStep)%len(gaps)]
							p.lockDelay = lockDelays[int(delayStep)%len(lockDelays)]
							if timed {
								gapSeen[p.gap] = true
								delaySeen[p.lockDelay] = true
							}
							run++
							ok := true
							t.Run(fmt.Sprintf("run%d", run), func(t *testing.T) {
								if why := runRetirePlan(t, p, waitBound); why != "" {
									t.Errorf("plan %s: %s", p, why)
									ok = false
								}
							})
							return ok
						}
						cfg := &quick.Config{MaxCount: runs, Rand: rand.New(rand.NewSource(seed))}
						if err := quick.Check(prop, cfg); err != nil {
							t.Error(err)
						}
					})
				}
			}
		}
	}
	// The generated timings of the plans that wait reach both ends of
	// each range.
	if !gapSeen[0] || !gapSeen[gaps[len(gaps)-1]] || !delaySeen[0] || !delaySeen[lockDelays[len(lockDelays)-1]] {
		t.Fatalf("the generated timings miss an end of a range: gaps %v, lock delays %v", gapSeen, delaySeen)
	}
}

// runRetirePlan runs Connect against the old daemon of p and returns the
// first broken expectation, or "". Each fact is observed where it
// happens: the daemon starts and run.lock at each start (probeSpawns),
// the shutdown requests in the old daemon, and the owner of run.lock by a
// probe at Connect's return.
func runRetirePlan(t *testing.T, p retirePlan, waitBound time.Duration) string {
	o := spawnOptions(t, "IWDP_DAEMON_IDLE=1m", lockDelayEnv+"="+p.lockDelay.String())
	f := startRetireDaemon(t, o.Dir, p)
	spawns := probeSpawns(t)
	busy := "daemon version 0.4.0 (protocol 1) is running with active sessions; run 'iwdp-cli daemon stop' when they finish"
	goingAway := p.leases == 0 && p.end.goingAway()
	limit := 8 * time.Second
	if goingAway && !p.exits {
		limit = waitBound // the old daemon stays: Connect waits until its deadline
	}
	c, err := Connect(ctxFor(t, limit), o)
	oldHolds := f.holdsRunLock(t) // at Connect's return, for (a)
	if c != nil {
		defer c.Close()
	}
	f.mu.Lock()
	shutdowns := f.shutdowns
	f.mu.Unlock()
	errText := fmt.Sprint(err)
	if spawns.whileLocked > 0 { // (b)
		return fmt.Sprintf("Connect (%v) started a daemon while the old one held run.lock (%d of %d starts)", err, spawns.whileLocked, spawns.starts)
	}
	switch {
	case p.leases > 0: // (d)
		if errText != busy || shutdowns != 0 {
			return fmt.Sprintf("Connect: %v, shutdown requests %d; want %q and none", err, shutdowns, busy)
		}
	case shutdowns != 1:
		return fmt.Sprintf("Connect: %v, shutdown requests %d; want one", err, shutdowns)
	case p.end == end503 || p.end == end302: // (c)
		status := map[shutdownEnd]string{end503: "503 Service Unavailable: broken", end302: "302 Found: "}[p.end]
		if want := "asking daemon version 0.4.0 (protocol 1) to exit: daemon POST /v1/shutdown: " + status; errText != want {
			return fmt.Sprintf("Connect: %v; want %q", err, want)
		}
	case p.end == endErrorBody: // (c)
		if want := busy + " (" + refusal + ")"; errText != want {
			return fmt.Sprintf("Connect: %v; want %q", err, want)
		}
	case p.exits: // going away, then gone: (a)
		if err != nil || c.Hello.Protocol != Protocol || c.Hello.PID == 1 || spawns.starts != 1 {
			return fmt.Sprintf("Connect: %v, daemon starts %d; want the one daemon that it started", err, spawns.starts)
		}
		if oldHolds {
			return "at Connect's return, the started daemon does not hold run.lock (the old daemon holds it, or nobody does)"
		}
		return ""
	default: // going away, but it stays
		if !errors.Is(err, context.DeadlineExceeded) || !oldHolds {
			return fmt.Sprintf("Connect: %v (old daemon holds run.lock: %v); want the deadline while the old daemon holds run.lock", err, oldHolds)
		}
	}
	if c != nil || spawns.starts != 0 { // (b): a failed Connect starts no daemon
		return fmt.Sprintf("Connect failed (%v) but started %d daemons", err, spawns.starts)
	}
	return ""
}
