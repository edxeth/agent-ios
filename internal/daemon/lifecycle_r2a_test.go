package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/commands"
	"github.com/edxeth/agent-ios/internal/core"
)

// fakeProtocol1Daemon serves the lifecycle endpoints of a daemon of
// protocol 1 with the request and answer shapes of its server at 70e525d
// (internal/daemon/server.go there: hello, status, shutdownReq). The JSON
// is written by hand, so that a change of this package's types cannot
// hide a difference.
type fakeProtocol1Daemon struct {
	mu        sync.Mutex
	leases    int
	shutdowns []string // the bodies of POST /v1/shutdown
	statuses  int
	l         net.Listener
}

// protocol1PID is the PID that the fake daemon of protocol 1 reports.
const protocol1PID = 4242

func startFakeProtocol1Daemon(t *testing.T, dir string, leases int) *fakeProtocol1Daemon {
	t.Helper()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProtocol1Daemon{leases: leases, l: l}
	hello := func() string {
		return fmt.Sprintf(`{"id":"d-old","version":"v0.9.0","protocol":1,"pid":%d,"startedAt":"2026-11-01T10:00:00Z","transports":["iwdp","direct","webkitgtk"],"leases":%d}`, protocol1PID, f.leases)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = fmt.Fprintln(w, hello())
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.statuses++
		pages := "[]"
		if f.leases > 0 {
			pages = `[{"key":"p1","title":"Old page","url":"https://example.test/","device":"sim","transport":"iwdp","state":"attached","session":"s1","lease":"active","epoch":1}]`
		}
		_, _ = fmt.Fprintf(w, `{"daemon":%s,"pages":%s,"cdp":{"listening":false,"endpoints":null,"connections":0}}`+"\n", hello(), pages)
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Force  bool `json:"force"`
			IfIdle bool `json:"ifIdle"`
		}
		body, _ := json.Marshal(nil)
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			body, _ = json.Marshal(req)
		}
		f.mu.Lock()
		f.shutdowns = append(f.shutdowns, string(body))
		busy := req.IfIdle && f.leases > 0
		n := f.leases
		f.mu.Unlock()
		if busy {
			_, _ = fmt.Fprintf(w, `{"ok":false,"error":{"code":4,"message":"the daemon has %d active lease(s); it stays"}}`+"\n", n)
			return
		}
		_, _ = fmt.Fprintln(w, `{"ok":true}`)
		go func() { _ = l.Close() }() // the daemon exits and removes its socket
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeProtocol1Daemon) counts() (statuses int, shutdowns []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses, append([]string(nil), f.shutdowns...)
}

// lifecycleHost is the Env of the Local daemon commands in a test: it
// reaches the daemon of o.
type lifecycleHost struct{ o Options }

func (lifecycleHost) Page(context.Context) (*core.PageSession, error) {
	return nil, errors.New("no page")
}
func (lifecycleHost) Emit(string)                 {}
func (lifecycleHost) ResolvePath(p string) string { return p }
func (h lifecycleHost) ClientOptions() Options    { return h.o }
func (h lifecycleHost) RunOptions() RunOptions    { return RunOptions{Dir: h.o.Dir} }

// runLifecycle runs the registry command words (daemon status, daemon
// stop) against the daemon of o.
func runLifecycle(t *testing.T, o Options, words ...string) (commands.Result, error) {
	t.Helper()
	c, n := commands.Lookup(words)
	if c == nil || n != len(words) {
		t.Fatalf("no command %q", words)
	}
	return c.Run(ctxFor(t, 8*time.Second), lifecycleHost{o}, commands.Input{Flags: map[string]any{}})
}

// WS2 round-1 review S2 F1: after an upgrade, 'daemon status' reaches the
// running daemon of the previous protocol, idle or busy, says that it is
// older and leaves it running; 'daemon stop' stops it as it stops a
// current daemon (POST /v1/shutdown without ifIdle). Other commands keep
// the rule: a busy one gets the 'daemon stop' advice, which now works.
func TestLifecycleCommandsReachAProtocol1Daemon(t *testing.T) {
	for _, leases := range []int{0, 1} {
		t.Run(fmt.Sprintf("leases=%d", leases), func(t *testing.T) {
			o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
			f := startFakeProtocol1Daemon(t, o.Dir, leases)
			if leases > 0 {
				_, err := Connect(ctxFor(t, 5*time.Second), o)
				if err == nil || !strings.Contains(err.Error(), "daemon version v0.9.0 (protocol 1) is running with active sessions; run 'agent-ios daemon stop' when they finish") {
					t.Fatalf("a command against the busy protocol-1 daemon: %v; want the daemon stop advice", err)
				}
			}
			res, err := runLifecycle(t, o, "daemon", "status")
			statuses, shutdowns := f.counts()
			if err != nil || statuses != 1 || len(shutdowns) != 0 {
				t.Fatalf("daemon status: %q, %v; status requests %d, shutdown requests %q; want the daemon's status and no shutdown", res.Text, err, statuses, shutdowns)
			}
			wantPage := "Pages: 0"
			if leases > 0 {
				wantPage = `p1  "Old page"  connection attached, lease active by session s1`
			}
			for _, want := range []string{"agent-ios daemon v0.9.0 (protocol 1) pid 4242", "older", "protocol 2", wantPage} {
				if !strings.Contains(res.Text, want) {
					t.Errorf("daemon status text %q lacks %q", res.Text, want)
				}
			}
			if st, ok := res.Data.(Status); !ok || st.Daemon.Protocol != 1 || st.Daemon.PID != protocol1PID {
				t.Errorf("daemon status data %+v; want the protocol-1 daemon's status", res.Data)
			}
			res, err = runLifecycle(t, o, "daemon", "stop")
			_, shutdowns = f.counts()
			if err != nil || res.Text != fmt.Sprintf("Stopped the agent-ios daemon (pid %d).", protocol1PID) || len(shutdowns) != 1 || shutdowns[0] != `{"force":false,"ifIdle":false}` {
				t.Fatalf("daemon stop: %q, %v; shutdown requests %q; want one plain shutdown request", res.Text, err, shutdowns)
			}
			if _, err := os.Lstat(SocketPath(o.Dir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("socket after daemon stop: %v; want it gone", err)
			}
		})
	}
}

// Ported from TestReviewNoStartReachesPreviousProtocol (review S2 F1):
// Connect with NoStart uses a daemon of the previous protocol and never
// asks it to exit.
func TestNoStartReachesPreviousProtocol(t *testing.T) {
	for _, leases := range []int{0, 1} {
		t.Run(fmt.Sprintf("leases=%d", leases), func(t *testing.T) {
			o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
			f := startFakeDaemonOf(t, o.Dir, Protocol-1, leases)
			o.NoStart = true
			c, err := Connect(ctxFor(t, 5*time.Second), o)
			if err != nil {
				t.Fatalf("NoStart to previous protocol with %d leases: %v", leases, err)
			}
			defer c.Close()
			f.mu.Lock()
			n := f.shutdown
			f.mu.Unlock()
			if n != 0 || c.Hello.Protocol != Protocol-1 {
				t.Fatalf("NoStart: %d shutdowns, hello %+v; want the previous protocol's daemon, kept", n, c.Hello)
			}
		})
	}
}

// NoStart reaches only the protocols whose lifecycle endpoints this client
// knows: a daemon of a newer protocol is reported as not reachable, and
// it is not asked to exit.
func TestNoStartRefusesANewerProtocol(t *testing.T) {
	o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
	f := startFakeDaemonOf(t, o.Dir, Protocol+1, 0)
	o.NoStart = true
	_, err := Connect(ctxFor(t, 5*time.Second), o)
	f.mu.Lock()
	n := f.shutdown
	f.mu.Unlock()
	if err == nil || n != 0 || !strings.Contains(err.Error(), fmt.Sprintf("protocol %d", Protocol+1)) {
		t.Fatalf("NoStart to a newer protocol: %v, %d shutdowns; want an error naming the protocol and no shutdown", err, n)
	}
}

// holdStartLock takes the startup lock of o.Dir, as a client that starts
// a daemon holds it, and returns its release.
func holdStartLock(t *testing.T, o Options) func() {
	t.Helper()
	if err := EnsureDir(o.Dir); err != nil {
		t.Fatal(err)
	}
	l, err := os.OpenFile(filepath.Join(o.Dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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

// lockWaiter returns a channel that is closed when a client finds the
// startup lock held and starts to wait for it (lockWaiting).
func lockWaiter(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	saved := lockWaiting
	lockWaiting = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(func() { lockWaiting = saved })
	return ch
}

// awaitLockWait returns when a client waits for the startup lock
// (waiting, from lockWaiter). It fails the test at once when the client's
// Connect returned first (done) or when the test's deadline comes.
func awaitLockWait(t *testing.T, waiting <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("Connect returned before it waited for the startup lock: %v", err)
	case <-untilTestDeadline(t):
		t.Fatal("no client waited for the startup lock before the test's deadline")
	}
}

// untilTestDeadline fires one second before the test's deadline (go test
// -timeout), so that the test reports its own failure before the binary's
// timeout panics; it never fires without a deadline.
func untilTestDeadline(t *testing.T) <-chan time.Time {
	dl, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(time.Until(dl) - time.Second)
}

// Ported from TestReviewConcurrentDifferentBinaryStart (review S2 F2):
// a client that waits for the startup lock while another binary starts an
// idle daemon retires that daemon and starts its own; a busy one gets the
// 'daemon stop' advice and is kept.
func TestConcurrentStartByAnotherBinary(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
		release := holdStartLock(t, o)
		waiting := lockWaiter(t)
		done := make(chan error, 1)
		var hello Hello
		go func() {
			c, err := Connect(ctxFor(t, 8*time.Second), o)
			if c != nil {
				hello = c.Hello
				c.Close()
			}
			done <- err
		}()
		awaitLockWait(t, waiting, done) // the client found no daemon and waits for the lock
		f := startFakeDaemonOf(t, o.Dir, Protocol, 0)
		release()
		err := <-done
		f.mu.Lock()
		n := f.shutdown
		f.mu.Unlock()
		if err != nil || n != 1 || hello.PID == 1 || hello.Protocol != Protocol {
			t.Fatalf("Connect while an idle incompatible daemon starts: err=%v, shutdowns=%d, hello %+v; want successful replacement", err, n, hello)
		}
	})
	t.Run("busy", func(t *testing.T) {
		o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
		release := holdStartLock(t, o)
		waiting := lockWaiter(t)
		done := make(chan error, 1)
		go func() {
			c, err := Connect(ctxFor(t, 8*time.Second), o)
			if c != nil {
				c.Close()
			}
			done <- err
		}()
		awaitLockWait(t, waiting, done) // the client found no daemon and waits for the lock
		f := startFakeDaemonOf(t, o.Dir, Protocol, 1)
		release()
		err := <-done
		f.mu.Lock()
		n := f.shutdown
		f.mu.Unlock()
		if err == nil || n != 0 || !strings.Contains(err.Error(), "run 'agent-ios daemon stop' when they finish") {
			t.Fatalf("Connect while a busy incompatible daemon starts: err=%v, shutdowns=%d; want the daemon stop advice and no shutdown", err, n)
		}
	})
}
