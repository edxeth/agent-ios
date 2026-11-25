package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/commands"
)

// closeMode is how a fake daemon that goes away ends a connection that it
// accepted, without an answer.
type closeMode struct {
	name string
	// want is the error that a hello gets from such a connection.
	want error
	// end ends the accepted connection.
	end func(net.Conn)
}

// closeModes: a daemon that exits closes its listener and its connections
// (Run), so a hello that reaches it in that window gets EOF, or
// ECONNRESET when the close finds the request unread.
var closeModes = []closeMode{
	{"EOF", io.EOF, func(conn net.Conn) {
		// Read the whole request, then close: the client reads EOF.
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_ = conn.Close()
	}},
	{"ECONNRESET", syscall.ECONNRESET, func(conn net.Conn) {
		// Close with the rest of the request unread: Linux resets the
		// client's end of a Unix stream socket (ECONNRESET).
		_, _ = conn.Read(make([]byte, 1))
		_ = conn.Close()
	}},
}

// startGoingAwayDaemon listens on the socket of dir and ends every
// connection with m, as a daemon that is going away. Its stop closes the
// listener, which removes the socket.
func startGoingAwayDaemon(t *testing.T, dir string, m closeMode) (stop func()) {
	t.Helper()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			m.end(conn)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = l.Close()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	// The fixture must give the error class under test.
	c := newClient(dir)
	defer c.Close()
	if _, err := c.hello(ctxFor(t, 5*time.Second)); !errors.Is(err, m.want) {
		t.Fatalf("hello to the fake daemon that goes away: %v; want %v", err, m.want)
	}
	return stop
}

// runLockWaiter returns a channel that closes when a Connect waits for
// run.lock (runLockWaiting).
func runLockWaiter(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	saved := runLockWaiting
	runLockWaiting = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(func() { runLockWaiting = saved })
	return ch
}

// spawnProbe observes the daemon starts of Connect where they happen
// (spawnStarting): it counts them and probes run.lock at each one.
type spawnProbe struct {
	starts      int // daemon processes that Connect started
	whileLocked int // of them, started while another holder had run.lock
}

// probeSpawns sets spawnStarting for the test and returns its counts.
func probeSpawns(t *testing.T) *spawnProbe {
	t.Helper()
	p := &spawnProbe{}
	saved := spawnStarting
	spawnStarting = func(dir string) {
		p.starts++
		if runLockHeld(t, dir) {
			p.whileLocked++
		}
	}
	t.Cleanup(func() { spawnStarting = saved })
	return p
}

// runLockHeld probes run.lock of dir without waiting, as a daemon that
// starts takes it: true when another holder has it.
func runLockHeld(t *testing.T, dir string) bool {
	f, err := os.Open(filepath.Join(dir, "run.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Errorf("probing run.lock: %v", err)
		return false
	}
	defer func() { _ = f.Close() }() // closing releases a lock that the probe took
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		t.Errorf("probing run.lock: %v", err)
	}
	return false
}

// Race run on the WS3 rename tree (TestConcurrentClientsReplaceAnIdleDaemonOnce:
// client B failed with 'Get "http://iwdp/v1/hello": EOF'): a client's
// first hello, outside the startup lock, can reach a daemon that is
// going away. The daemon accepts the connection and closes it without an
// answer. Connect must treat that daemon as one that does not answer:
// take the startup lock, send its second hello, wait for run.lock and then
// start a daemon. Before, it returned the hello's error.
func TestConnectTreatsAClosedHelloAsNoDaemon(t *testing.T) {
	for _, m := range closeModes {
		t.Run(m.name, func(t *testing.T) {
			o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
			release := holdRunLock(t, o.Dir) // the exiting daemon still holds run.lock
			stop := startGoingAwayDaemon(t, o.Dir, m)
			waiting := runLockWaiter(t)
			spawns := probeSpawns(t)
			done := connectAsync(t, o)
			select {
			case <-waiting:
			case r := <-done:
				t.Fatalf("Connect to a daemon that closes the hello's connection (%s) ended before it waited for run.lock: %v; want it to wait", m.name, r.err)
			case <-untilTestDeadline(t):
				t.Fatal("Connect did not wait for the run.lock of the daemon that goes away")
			}
			// The daemon exits: it closes its listener, then releases run.lock.
			stop()
			release()
			r := <-done
			if r.err != nil || r.hello.Protocol != Protocol || r.hello.PID == 0 || spawns.starts != 1 || spawns.whileLocked != 0 {
				t.Fatalf("Connect after the daemon that went away released run.lock: %v, daemon %+v, starts %d (while run.lock was held: %d); want the one daemon that Connect started (protocol %d) after the release", r.err, r.hello, spawns.starts, spawns.whileLocked, Protocol)
			}
		})
	}
}

// 'daemon status' and 'daemon stop' (Connect with NoStart) report a
// daemon that closes the hello's connection as not running.
func TestNoStartTreatsAClosedHelloAsNotRunning(t *testing.T) {
	for _, m := range closeModes {
		t.Run(m.name, func(t *testing.T) {
			dir := filepath.Join(shortDir(t), "agent-ios")
			startGoingAwayDaemon(t, dir, m)
			c, err := Connect(ctxFor(t, 5*time.Second), Options{Dir: dir, NoStart: true})
			if c != nil {
				c.Close()
			}
			if !errors.Is(err, ErrNotRunning) {
				t.Fatalf("Connect with NoStart to a daemon that closes the hello's connection (%s): %v; want ErrNotRunning", m.name, err)
			}
		})
	}
}

// The commands 'daemon status' and 'daemon stop' report a daemon that
// closes the hello's connection as not running, as when no daemon
// listens; 'daemon status' also gives running: false in its data. Before,
// both failed with the hello's error.
func TestLifecycleCommandsTreatAClosedHelloAsNotRunning(t *testing.T) {
	for _, m := range closeModes {
		for _, cmd := range []string{"status", "stop"} {
			t.Run(m.name+"/daemon "+cmd, func(t *testing.T) {
				o := spawnOptions(t)
				startGoingAwayDaemon(t, o.Dir, m)
				spawns := probeSpawns(t)
				res, err := runLifecycle(t, o, "daemon", cmd)
				if err != nil || res.Text != "The agent-ios daemon is not running." {
					t.Fatalf("daemon %s with a daemon that closes the hello's connection (%s): %q, %v; want %q", cmd, m.name, res.Text, err, "The agent-ios daemon is not running.")
				}
				if cmd == "status" {
					data, ok := res.Data.(map[string]bool)
					running, found := data["running"]
					if !ok || len(data) != 1 || !found || running {
						t.Fatalf("daemon status data with a daemon that closes the hello's connection (%s): %#v; want running: false", m.name, res.Data)
					}
				} else if res.Data != nil {
					t.Fatalf("daemon stop data with a daemon that closes the hello's connection (%s): %#v; want none", m.name, res.Data)
				}
				if spawns.starts != 0 {
					t.Fatalf("daemon %s started %d daemons; want none", cmd, spawns.starts)
				}
			})
		}
	}
}

// WaitGone ('daemon stop', retire) counts a daemon that closes the
// hello's connection as one that does not answer: it then waits only for
// run.lock. Before, it reported the daemon as "still answering" and
// waited until its limit even with run.lock free.
func TestWaitGoneTreatsAClosedHelloAsNoAnswer(t *testing.T) {
	for _, m := range closeModes {
		t.Run(m.name, func(t *testing.T) {
			dir := filepath.Join(shortDir(t), "agent-ios")
			release := holdRunLock(t, dir)
			startGoingAwayDaemon(t, dir, m)
			c := newClient(dir)
			defer c.Close()
			// The limit is the behavior under test: the held lock keeps
			// WaitGone waiting until it.
			const limit = 200 * time.Millisecond
			err := c.WaitGone(ctxFor(t, 5*time.Second), limit)
			if err == nil || !strings.Contains(err.Error(), "still holding "+filepath.Join(dir, "run.lock")) {
				t.Fatalf("WaitGone while a daemon that closes the hello's connection (%s) holds run.lock: %v; want the error that names the held lock", m.name, err)
			}
			release()
			if err := c.WaitGone(ctxFor(t, 5*time.Second), time.Second); err != nil {
				t.Fatalf("WaitGone after run.lock was released, the socket still closing connections (%s): %v; want nil", m.name, err)
			}
		})
	}
}

// closedConn is a client's connection to a fake daemon that closed its
// end at accept. Its Read runs only when readAfter is closed or the
// transport closed the connection, so that the transport sees the
// daemon's close in one order: by the read (EOF) or by the write (EPIPE).
type closedConn struct {
	net.Conn
	readAfter  chan struct{} // closed when Read may run
	closedOnce sync.Once
	closed     chan struct{} // closed by Close
}

func (c *closedConn) Read(p []byte) (int, error) {
	select {
	case <-c.readAfter:
	case <-c.closed:
	}
	return c.Conn.Read(p)
}

func (c *closedConn) Close() error {
	err := c.Conn.Close()
	c.closedOnce.Do(func() { close(c.closed) })
	return err
}

// The net/http transport names two closes of a new connection itself,
// which the fake daemons above do not give deterministically: an EOF
// before the request was sent ("http: server closed idle connection")
// and a write after the daemon closed (EPIPE). WaitGone counts both as
// no answer.
func TestWaitGoneTreatsTransportClosesAsNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// readFirst: the transport reads the EOF, and the request is
		// sent only after that; else it writes first.
		readFirst bool
		want      func(error) bool
	}{
		{"server closed idle connection", true, func(err error) bool {
			return err != nil && strings.HasSuffix(err.Error(), ": http: server closed idle connection")
		}},
		{"EPIPE", false, func(err error) bool { return errors.Is(err, syscall.EPIPE) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(shortDir(t), "agent-ios")
			if err := EnsureDir(dir); err != nil {
				t.Fatal(err)
			}
			l, err := net.Listen("unix", SocketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			serverClosed := make(chan struct{}, 1)
			go func() {
				for {
					conn, err := l.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
					serverClosed <- struct{}{}
				}
			}()
			conns := make(chan *closedConn, 1)
			c := newClientDial(dir, func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				conn, err := d.DialContext(ctx, "unix", SocketPath(dir))
				if err != nil {
					return nil, err
				}
				<-serverClosed
				cc := &closedConn{Conn: conn, readAfter: make(chan struct{}), closed: make(chan struct{})}
				if tc.readFirst {
					close(cc.readAfter)
				}
				conns <- cc
				return cc, nil
			})
			defer c.Close()
			// GotConn runs before the transport sends the request: with
			// readFirst it waits until the transport has read the EOF and
			// closed the connection.
			trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
				cc := <-conns
				if tc.readFirst {
					<-cc.closed
				}
			}}
			ctx := func(d time.Duration) context.Context { return httptrace.WithClientTrace(ctxFor(t, d), trace) }
			// The fixture must give the transport's error under test.
			if _, err := c.hello(ctx(5 * time.Second)); !tc.want(err) {
				t.Fatalf("hello to the fake daemon that closed the connection (%s): %v; want that error", tc.name, err)
			}
			c.Close()
			release := holdRunLock(t, dir)
			// The limit is the behavior under test: the held lock keeps
			// WaitGone waiting until it.
			const limit = 200 * time.Millisecond
			err = c.WaitGone(ctx(5*time.Second), limit)
			if err == nil || !strings.Contains(err.Error(), "still holding "+filepath.Join(dir, "run.lock")) {
				t.Fatalf("WaitGone while a daemon that closes the connection (%s) holds run.lock: %v; want the error that names the held lock", tc.name, err)
			}
			release()
			if err := c.WaitGone(ctx(5*time.Second), time.Second); err != nil {
				t.Fatalf("WaitGone after run.lock was released, the socket still closing connections (%s): %v; want nil", tc.name, err)
			}
		})
	}
}

// A daemon that answers the hello, with an error status or with a
// malformed answer, keeps its handling: Connect returns the hello's error
// at once, with or without NoStart; it neither waits for run.lock nor
// starts a daemon.
func TestConnectReturnsTheErrorOfAnAnsweredHello(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(net.Conn)
		// want starts the hello's error after "connecting to the iwdp
		// daemon at <socket>: ".
		want string
	}{
		{"503", func(conn net.Conn) {
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 7\r\nConnection: close\r\n\r\nbroken\n")
			_ = conn.Close()
		}, "daemon GET /v1/hello: 503 Service Unavailable: broken"},
		{"malformed", func(conn net.Conn) {
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_, _ = io.WriteString(conn, "NOT HTTP\r\n\r\n")
			_ = conn.Close()
		}, `Get "http://iwdp/v1/hello": net/http: HTTP/1.x transport connection broken: malformed HTTP`},
		{"302", func(conn net.Conn) {
			// A redirect is an answer with an unexpected status; a
			// request to its target closes without an answer.
			if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil && req.URL.Path == "/v1/hello" {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", redirectTarget)
			}
			_ = conn.Close()
		}, "daemon GET /v1/hello: 302 Found: "},
		{"200 with an empty body", func(conn net.Conn) {
			// The JSON decoder of the answer returns EOF: an answer that
			// does not decode, not a closed connection.
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			_ = conn.Close()
		}, "daemon GET /v1/hello: reading the answer: EOF"},
	} {
		for _, noStart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/NoStart=%v", tc.name, noStart), func(t *testing.T) {
				o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
				o.NoStart = noStart
				if err := EnsureDir(o.Dir); err != nil {
					t.Fatal(err)
				}
				l, err := net.Listen("unix", SocketPath(o.Dir))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
				go func() {
					for {
						conn, err := l.Accept()
						if err != nil {
							return
						}
						tc.answer(conn)
					}
				}()
				release := holdRunLock(t, o.Dir) // a Connect that waits for it fails by its deadline
				spawns := probeSpawns(t)
				c, err := Connect(ctxFor(t, 5*time.Second), o)
				if c != nil {
					c.Close()
				}
				release()
				want := "connecting to the agent-ios daemon at " + SocketPath(o.Dir) + ": " + tc.want
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("Connect (NoStart %v) to a daemon that answers the hello (%s): %v; want an error that starts with %q", noStart, tc.name, err, want)
				}
				if spawns.starts != 0 {
					t.Fatalf("Connect (NoStart %v) started %d daemons; want none", noStart, spawns.starts)
				}
			})
		}
	}
}

// nextMode is what a fake daemon does with the request that follows its
// hello (afterHelloDaemon).
type nextMode int

const (
	// closeNext closes the request's connection without an answer; the
	// daemon then goes away: it closes every connection, and at the
	// first one after the close it exits (closes its listener, then
	// releases run.lock), as Run does.
	closeNext nextMode = iota
	// closeNextKeepAnswering closes the request's connection without an
	// answer and keeps answering hellos.
	closeNextKeepAnswering
	// answer503 answers the request with HTTP 503 and stays.
	answer503
	// answerEmpty answers the request with 200 and an empty body (its
	// JSON decoder returns EOF) and stays.
	answerEmpty
	// redirectNext answers the request with a redirect (302 to
	// redirectTarget) and stays; it closes a request to the target
	// without an answer.
	redirectNext
)

// redirectTarget is the Location of the redirects of the fake daemons.
const redirectTarget = "/other"

// afterHelloDaemon is a fake daemon that holds run.lock, answers every
// hello with hello and handles the request next ("POST /v1/shutdown") by
// its mode: a daemon that goes away right after a hello that it
// answered.
type afterHelloDaemon struct {
	mu         sync.Mutex
	nexts      int  // requests next seen
	redirected int  // requests to redirectTarget seen
	gone       bool // the daemon goes away
	exited     chan struct{}
	lock       *os.File // its run.lock
}

// holdsRunLock probes run.lock through the daemon's own open file
// without waiting: false when another holder (a replacement) has it. True
// means that this daemon holds it, or that nobody held it and the probe
// took it for this daemon; a probe at the end of a test leaves it to the
// cleanup.
func (f *afterHelloDaemon) holdsRunLock(t *testing.T) bool {
	err := syscall.Flock(int(f.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	if err != nil {
		t.Errorf("probing run.lock of the fake daemon: %v", err)
	}
	return true
}

func startAfterHelloDaemon(t *testing.T, dir string, hello Hello, next string, mode nextMode) *afterHelloDaemon {
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
	body, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	f := &afterHelloDaemon{exited: make(chan struct{}), lock: lock}
	exit := func() {
		_ = l.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Errorf("releasing run.lock of the fake daemon: %v", err)
		}
		close(f.exited)
	}
	reply := func(conn net.Conn, status string, body []byte) {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", status, len(body), body)
	}
	handle := func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.gone:
			_ = conn.Close()
			select {
			case <-f.exited:
			default:
				exit()
			}
		case req.Method+" "+req.URL.Path == "GET /v1/hello":
			reply(conn, "200 OK", body)
		case req.Method+" "+req.URL.Path == next:
			f.nexts++
			switch mode {
			case answer503:
				reply(conn, "503 Service Unavailable", []byte("broken\n"))
			case answerEmpty:
				reply(conn, "200 OK", nil)
			case redirectNext:
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", redirectTarget)
			case closeNext:
				f.gone = true
			}
		case req.URL.Path == redirectTarget:
			f.redirected++ // closed without an answer
		default:
			reply(conn, "404 Not Found", nil)
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

func (f *afterHelloDaemon) nextCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nexts
}

func (f *afterHelloDaemon) redirectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redirected
}

// Connect asks an idle daemon of another protocol to exit, and the daemon
// closes that request without an answer: it is going away already (an
// idle exit, another client's 'daemon stop'). Connect must wait until it
// is gone (WaitGone: its socket and its run.lock) and then start a
// daemon. Before, Connect failed with "asking daemon ... to exit: ...
// EOF". A daemon that answers the request with an error keeps that
// handling.
func TestRetireWaitsForADaemonThatClosesTheShutdownRequest(t *testing.T) {
	old := Hello{Version: "0.4.0", Protocol: previousProtocol, PID: 1}
	t.Run("closed", func(t *testing.T) {
		o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
		f := startAfterHelloDaemon(t, o.Dir, old, "POST /v1/shutdown", closeNext)
		spawns := probeSpawns(t)
		c, err := Connect(ctxFor(t, 8*time.Second), o)
		if err != nil {
			t.Fatalf("Connect to an idle old daemon that closes the shutdown request: %v; want the daemon that Connect started", err)
		}
		defer c.Close()
		select {
		case <-f.exited:
		default:
			t.Fatal("Connect returned before the old daemon exited")
		}
		if f.holdsRunLock(t) {
			t.Fatal("at Connect's return, the started daemon does not hold run.lock")
		}
		if n := f.nextCount(); n != 1 || c.Hello.Protocol != Protocol || c.Hello.PID == old.PID {
			t.Fatalf("shutdown requests %d, daemon %+v; want one request and a daemon of protocol %d", n, c.Hello, Protocol)
		}
		if spawns.starts != 1 || spawns.whileLocked != 0 {
			t.Fatalf("daemon starts %d, of them while run.lock was held %d; want one start after the old daemon released it", spawns.starts, spawns.whileLocked)
		}
	})
	t.Run("503", func(t *testing.T) {
		o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
		f := startAfterHelloDaemon(t, o.Dir, old, "POST /v1/shutdown", answer503)
		spawns := probeSpawns(t)
		c, err := Connect(ctxFor(t, 8*time.Second), o)
		if c != nil {
			c.Close()
		}
		want := "asking daemon version 0.4.0 (protocol 1) to exit: daemon POST /v1/shutdown: 503 Service Unavailable: broken"
		if err == nil || err.Error() != want || f.nextCount() != 1 {
			t.Fatalf("Connect to an old daemon that answers the shutdown request with 503: %v (shutdown requests %d); want %q after one request", err, f.nextCount(), want)
		}
		if spawns.starts != 0 {
			t.Fatalf("Connect started %d daemons; want none", spawns.starts)
		}
	})
}

// 'daemon stop': a daemon that closes the stop request without an answer
// is going away. The command waits for it (WaitGone, stopWait) and reports
// that it stopped, or that it still answers after the bound. Before, it
// failed with the request's EOF. A daemon that answers the request with
// an error keeps that handling.
func TestDaemonStopWaitsForADaemonThatClosesTheStopRequest(t *testing.T) {
	h := Hello{Version: "test", Protocol: Protocol, PID: 4242}
	t.Run("closed", func(t *testing.T) {
		o := spawnOptions(t)
		f := startAfterHelloDaemon(t, o.Dir, h, "POST /v1/shutdown", closeNext)
		res, err := runLifecycle(t, o, "daemon", "stop")
		if err != nil || res.Text != "Stopped the agent-ios daemon (pid 4242)." {
			t.Fatalf("daemon stop of a daemon that closes the stop request: %q, %v; want %q", res.Text, err, "Stopped the agent-ios daemon (pid 4242).")
		}
		select {
		case <-f.exited:
		default:
			t.Fatal("daemon stop reported the stop before the daemon exited")
		}
		if runLockHeld(t, o.Dir) {
			t.Fatal("daemon stop reported the stop while run.lock was held")
		}
	})
	t.Run("closed, still answering", func(t *testing.T) {
		o := spawnOptions(t)
		startAfterHelloDaemon(t, o.Dir, h, "POST /v1/shutdown", closeNextKeepAnswering)
		// stopWait is the bound under test; runLifecycle's context (8 s)
		// outlasts it.
		_, err := runLifecycle(t, o, "daemon", "stop")
		want := "the agent-ios daemon at " + SocketPath(o.Dir) + " is still answering after " + stopWait.String()
		if err == nil || err.Error() != want {
			t.Fatalf("daemon stop of a daemon that closes the stop request and keeps answering: %v; want %q", err, want)
		}
	})
	t.Run("503", func(t *testing.T) {
		o := spawnOptions(t)
		startAfterHelloDaemon(t, o.Dir, h, "POST /v1/shutdown", answer503)
		res, err := runLifecycle(t, o, "daemon", "stop")
		want := "daemon POST /v1/shutdown: 503 Service Unavailable: broken"
		if err == nil || err.Error() != want {
			t.Fatalf("daemon stop of a daemon that answers the stop request with 503: %q, %v; want %q", res.Text, err, want)
		}
	})
}

// 'daemon status': a daemon that closes the status request without an
// answer is going away; the command reports "The agent-ios daemon is not
// running.", as for a hello that finds no daemon. Before, it failed with
// the request's EOF. A daemon that answers the request with an error
// keeps that handling.
func TestDaemonStatusReportsADaemonThatClosesTheStatusRequestAsNotRunning(t *testing.T) {
	h := Hello{Version: "test", Protocol: Protocol, PID: 4242}
	t.Run("closed", func(t *testing.T) {
		o := spawnOptions(t)
		f := startAfterHelloDaemon(t, o.Dir, h, "GET /v1/status", closeNext)
		res, err := runLifecycle(t, o, "daemon", "status")
		data, _ := res.Data.(map[string]bool)
		running, found := data["running"]
		if err != nil || res.Text != "The agent-ios daemon is not running." || len(data) != 1 || !found || running || f.nextCount() != 1 {
			t.Fatalf("daemon status of a daemon that closes the status request: %q, %v, data %v (status requests %d); want %q and running false", res.Text, err, res.Data, f.nextCount(), "The agent-ios daemon is not running.")
		}
	})
	t.Run("200 with an empty body", func(t *testing.T) {
		// The answer does not decode (EOF of the JSON decoder): an answer,
		// never a daemon that goes away.
		o := spawnOptions(t)
		startAfterHelloDaemon(t, o.Dir, h, "GET /v1/status", answerEmpty)
		res, err := runLifecycle(t, o, "daemon", "status")
		want := "daemon GET /v1/status: reading the answer: EOF"
		if err == nil || err.Error() != want {
			t.Fatalf("daemon status of a daemon that answers the status request with an empty body: %q, %v; want %q", res.Text, err, want)
		}
	})
	t.Run("503", func(t *testing.T) {
		o := spawnOptions(t)
		startAfterHelloDaemon(t, o.Dir, h, "GET /v1/status", answer503)
		res, err := runLifecycle(t, o, "daemon", "status")
		want := "daemon GET /v1/status: 503 Service Unavailable: broken"
		if err == nil || err.Error() != want {
			t.Fatalf("daemon status of a daemon that answers the status request with 503: %q, %v; want %q", res.Text, err, want)
		}
	})
}

// Review finding (ported from the reviewer's TestReviewerRedirectAnswerThenClose):
// net/http followed a daemon's redirect answer to the hello, and the
// target's closed connection made Connect report that no daemon answers
// (ErrNotRunning), although the daemon answered. The daemon client never
// follows a redirect: the 302 is the answer, with an unexpected status.
func TestRedirectAnswerToTheHelloIsAnAnswer(t *testing.T) {
	dir := filepath.Join(shortDir(t), "agent-ios")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var hellos, redirected atomic.Int32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			switch {
			case err != nil:
			case req.URL.Path == "/v1/hello":
				hellos.Add(1)
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", redirectTarget)
			case req.URL.Path == redirectTarget:
				redirected.Add(1) // closed without an answer
			}
			_ = conn.Close()
		}
	}()
	c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: dir, NoStart: true})
	if c != nil {
		c.Close()
	}
	want := "connecting to the agent-ios daemon at " + SocketPath(dir) + ": daemon GET /v1/hello: 302 Found: "
	if hellos.Load() == 0 || errors.Is(err, ErrNotRunning) || err == nil || err.Error() != want || redirected.Load() != 0 {
		t.Fatalf("Connect with NoStart to a daemon that answers the hello with a redirect: %v (hellos %d, requests to the redirect target %d); want %q and no request to the target", err, hellos.Load(), redirected.Load(), want)
	}
}

// Every request of the daemon client treats a redirect as the answer: an
// unexpected status, with the handling of such an answer, and no request
// to the target (which here closes without an answer, the class of
// notRunning). The hello's case is in TestConnectReturnsTheErrorOfAnAnsweredHello.
func TestRedirectAnswersAreAnswers(t *testing.T) {
	current := Hello{Version: "test", Protocol: Protocol, PID: 4242}
	for _, tc := range []struct {
		name  string
		hello Hello
		next  string
		// run sends the request and returns its error.
		run  func(t *testing.T, o Options) error
		want string
	}{
		{
			"retire (POST /v1/shutdown)",
			Hello{Version: "0.4.0", Protocol: previousProtocol, PID: 1},
			"POST /v1/shutdown",
			func(t *testing.T, o Options) error {
				c, err := Connect(ctxFor(t, 8*time.Second), o)
				if c != nil {
					c.Close()
				}
				return err
			},
			"asking daemon version 0.4.0 (protocol 1) to exit: daemon POST /v1/shutdown: 302 Found: ",
		},
		{
			"daemon stop (POST /v1/shutdown)", current, "POST /v1/shutdown",
			func(t *testing.T, o Options) error {
				_, err := runLifecycle(t, o, "daemon", "stop")
				return err
			},
			"daemon POST /v1/shutdown: 302 Found: ",
		},
		{
			"daemon status (GET /v1/status)", current, "GET /v1/status",
			func(t *testing.T, o Options) error {
				_, err := runLifecycle(t, o, "daemon", "status")
				return err
			},
			"daemon GET /v1/status: 302 Found: ",
		},
		{
			"run (POST /v1/run)", current, "POST /v1/run",
			func(t *testing.T, o Options) error {
				c := newClient(o.Dir)
				defer c.Close()
				_, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{}, nil)
				return err
			},
			"daemon POST /v1/run: 302 Found: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
			f := startAfterHelloDaemon(t, o.Dir, tc.hello, tc.next, redirectNext)
			spawns := probeSpawns(t)
			err := tc.run(t, o)
			if want := tc.want; err == nil || err.Error() != want || f.nextCount() != 1 || f.redirectCount() != 0 {
				t.Fatalf("%s answered with a redirect: %v (requests %d, requests to the redirect target %d); want %q after one request and none to the target", tc.name, err, f.nextCount(), f.redirectCount(), want)
			}
			if spawns.starts != 0 {
				t.Fatalf("%s started %d daemons; want none", tc.name, spawns.starts)
			}
		})
	}
}

// redirectingDaemon is a fake daemon that answers the hello (protocol 2,
// pid 4242) and answers requests to path with a redirect (302 to
// redirectTarget); it closes every other request, also to the target,
// without an answer.
func redirectingDaemon(t *testing.T, path string) string {
	t.Helper()
	dir := filepath.Join(shortDir(t), "agent-ios")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				switch req.URL.Path {
				case path:
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", redirectTarget)
				case "/v1/hello":
					const body = `{"version":"review","protocol":2,"pid":4242}`
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
				}
			}
			_ = conn.Close()
		}
	}()
	return dir
}

// Review finding (ported from the reviewer's TestReviewerRedirectInstances):
// every use of notRunning after a redirect answer. A followed redirect
// whose target closes the connection made WaitGone, retire, 'daemon
// status' and 'daemon stop' treat the answering daemon as going away. The
// redirect is the answer: WaitGone reports the daemon as answering, and
// the others report the answer's unexpected status. (The reviewer's test
// expected the redirect's EOF of the baseline; the client no longer
// follows redirects, so the expected errors are those of an answer.)
func TestRedirectAnswersInEveryNotRunningUse(t *testing.T) {
	t.Run("WaitGone", func(t *testing.T) {
		dir := redirectingDaemon(t, "/v1/hello")
		c := newClient(dir)
		defer c.Close()
		// The limit is the behavior under test.
		const limit = 50 * time.Millisecond
		err := c.WaitGone(ctxFor(t, 5*time.Second), limit)
		want := "the agent-ios daemon at " + SocketPath(dir) + " is still answering after " + limit.String()
		if err == nil || err.Error() != want {
			t.Fatalf("WaitGone with a daemon that answers the hello with a redirect: %v; want %q", err, want)
		}
	})
	t.Run("retire", func(t *testing.T) {
		dir := redirectingDaemon(t, "/v1/shutdown")
		c := newClient(dir)
		defer c.Close()
		err := c.retire(ctxFor(t, 5*time.Second), Hello{Version: "review", Protocol: previousProtocol})
		want := "asking daemon version review (protocol 1) to exit: daemon POST /v1/shutdown: 302 Found: "
		if err == nil || err.Error() != want {
			t.Fatalf("retire of a daemon that answers the shutdown request with a redirect: %v; want %q", err, want)
		}
	})
	for _, tc := range []struct{ cmd, path, want string }{
		{"status", "/v1/status", "daemon GET /v1/status: 302 Found: "},
		{"stop", "/v1/shutdown", "daemon POST /v1/shutdown: 302 Found: "},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			dir := redirectingDaemon(t, tc.path)
			c, n := commands.Lookup([]string{"daemon", tc.cmd})
			if c == nil || n != 2 {
				t.Fatalf("no command daemon %s", tc.cmd)
			}
			res, err := c.Run(ctxFor(t, 5*time.Second), lifecycleHost{Options{Dir: dir}}, commands.Input{Flags: map[string]any{}})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("daemon %s with a daemon that answers its request with a redirect: %q, %v; want %q", tc.cmd, res.Text, err, tc.want)
			}
		})
	}
}

// Review finding F6: net/http parses a redirect's Location before it asks
// CheckRedirect, so a 3xx with an invalid Location ("/%zz") failed with
// "failed to parse Location header". Every 3xx is the daemon's answer,
// whatever its Location: each request kind reports the unexpected status,
// and the daemon gets no second request.
func TestRedirectWithAnInvalidLocationIsAnAnswer(t *testing.T) {
	dir := filepath.Join(shortDir(t), "agent-ios")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var requests atomic.Int32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				requests.Add(1)
				_, _ = io.WriteString(conn, "HTTP/1.1 302 Found\r\nLocation: /%zz\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}
			_ = conn.Close()
		}
	}()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/hello"}, {http.MethodGet, "/v1/status"}, {http.MethodPost, "/v1/shutdown"}, {http.MethodPost, "/v1/run"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			before := requests.Load()
			c := newClient(dir)
			defer c.Close()
			resp, err := c.request(ctxFor(t, 5*time.Second), tc.method, tc.path, nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			want := "daemon " + tc.method + " " + tc.path + ": 302 Found: "
			if n := requests.Load() - before; err == nil || err.Error() != want || n != 1 {
				t.Fatalf("%s %s answered with a 302 to an invalid Location: %v (requests %d); want %q after one request", tc.method, tc.path, err, n, want)
			}
		})
	}
}
