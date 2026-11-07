package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the W1b review (docs/notes/w1b.md, "Review repairs").

// fakePeer serves a hello of this protocol on dir's socket: another local
// user's impersonation of the daemon.
func fakePeer(t *testing.T, dir string) {
	t.Helper()
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Hello{Version: "impostor", Protocol: Protocol})
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// Design 2.2 (R16): the client checks the socket directory before it
// trusts any peer on the socket, also with NoStart; a live peer in an
// unsafe directory is refused.
func TestConnectRefusesAPeerInAnUnsafeDir(t *testing.T) {
	base := shortDir(t)
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	fakePeer(t, open)
	fakePeer(t, real)
	for _, dir := range []string{open, link} {
		for _, noStart := range []bool{true, false} {
			c, err := Connect(ctxFor(t, 2*time.Second), Options{Dir: dir, NoStart: noStart, Exe: "/bin/false"})
			if !errors.Is(err, ErrUnsafeDir) {
				if c != nil {
					t.Errorf("connected to %q in %s", c.Hello.Version, dir)
				}
				t.Fatalf("Connect(%s, NoStart=%v) = %v, want ErrUnsafeDir", dir, noStart, err)
			}
		}
	}
}

// Design 2.10: Debugger.resume for a paused evaluation goes through the
// control lane. It does not queue behind the evaluation's operation, and
// it completes the pause at most once.
func TestControlLaneResumesAPausedEvaluation(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	f.HoldUntil("Runtime.evaluate", "Debugger.resume", "Debugger.paused", map[string]any{"callFrames": []any{}, "reason": "other"})
	a, err := openPage(t, c, f, "s1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	evalDone := make(chan error, 1)
	go func() {
		_, err := a.Send(ctxFor(t, 10*time.Second), "Runtime.evaluate", map[string]any{"expression": "debugger"})
		evalDone <- err
	}()
	waitSent(t, f, "Debugger.paused")
	b, err := c.AttachPage(ctxFor(t, 3*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}, Name: "resume"})
	if err != nil {
		t.Fatalf("attaching the resume command while the evaluation is paused: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Send(ctxFor(t, 3*time.Second), "Debugger.resume", nil); err != nil {
		t.Fatalf("Debugger.resume while the evaluation is paused: %v", err)
	}
	select {
	case err := <-evalDone:
		if err != nil {
			t.Fatalf("the evaluation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the evaluation did not finish after the resume")
	}
	if _, err := b.Send(ctxFor(t, 3*time.Second), "Debugger.resume", nil); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("a second resume: %v, want the refusal 'the debugger is not paused'", err)
	}
	if n := len(f.ReceivedMethod("Debugger.resume")); n != 1 {
		t.Fatalf("the fake received %d Debugger.resume, want 1", n)
	}
}

// Design 2.10: the completion of a held request goes through the control
// lane while the awaited fetch holds the page, exactly once.
func TestControlLaneCompletesAnInterceptedFetch(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	f.HoldUntil("Runtime.evaluate", "Network.interceptContinue", "Network.requestIntercepted", map[string]any{"requestId": "r1", "request": map[string]any{"url": "https://fixture.example/api"}})
	a, err := openPage(t, c, f, "s1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	evalDone := make(chan error, 1)
	go func() {
		_, err := a.Send(ctxFor(t, 10*time.Second), "Runtime.evaluate", map[string]any{"expression": "fetch('/api')", "awaitPromise": true})
		evalDone <- err
	}()
	waitSent(t, f, "Network.requestIntercepted")
	b, err := c.AttachPage(ctxFor(t, 3*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}, Name: "intercept-continue"})
	if err != nil {
		t.Fatalf("attaching while the fetch is held: %v", err)
	}
	defer func() { _ = b.Close() }()
	cont := map[string]string{"requestId": "r1", "stage": "request"}
	if _, err := b.Send(ctxFor(t, 3*time.Second), "Network.interceptContinue", cont); err != nil {
		t.Fatalf("Network.interceptContinue while the fetch is held: %v", err)
	}
	select {
	case err := <-evalDone:
		if err != nil {
			t.Fatalf("the evaluation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the evaluation did not finish after the completion")
	}
	if _, err := b.Send(ctxFor(t, 3*time.Second), "Network.interceptContinue", cont); err == nil || !strings.Contains(err.Error(), "no open intercepted request") {
		t.Fatalf("a second completion: %v, want the refusal", err)
	}
	if n := len(f.ReceivedMethod("Network.interceptContinue")); n != 1 {
		t.Fatalf("the fake received %d completions, want 1", n)
	}
}

// waitSent waits until the fake has sent the event method.
func waitSent(t *testing.T, f *fakewebkit.Server, method string) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		for _, r := range f.Log() {
			if !r.In && r.Method == method {
				time.Sleep(100 * time.Millisecond) // the daemon's read loop records it
				return
			}
		}
		if time.Now().After(end) {
			t.Fatalf("the fake did not send %s", method)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Design 2.5: an event stream is bound to its lease. When session A
// releases the page, A's stream ends; after session B takes the page, A
// receives none of B's events.
func TestEventStreamEndsWithItsLease(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	a, err := openPage(t, c, f, "a", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	got := make(chan string, 4)
	a.Subscribe("Console.messageAdded", func(_ string, p json.RawMessage) { got <- string(p) })
	a.EndOp()
	if _, err := c.Lease(ctxFor(t, 5*time.Second), LeaseRequest{Op: "release", Owner: Owner{Session: "a"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("A's caller is not done after its lease ended")
	}
	b, err := openPage(t, c, f, "b", 5*time.Second)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	defer func() { _ = b.Close() }()
	f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"text": "b's secret"}})
	select {
	case p := <-got:
		t.Fatalf("A received %s after its lease ended", p)
	case <-time.After(500 * time.Millisecond):
	}
}

// Design 2.2: a client of another protocol asks the daemon to exit only
// when it is idle; the daemon checks that itself when the request arrives,
// so a daemon with a lease stays.
func TestShutdownIfIdleRefusedWithALease(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	a, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Close() // the lease stays
	var out struct {
		OK    bool        `json:"ok"`
		Error *ErrorReply `json:"error"`
	}
	if err := c.call(ctxFor(t, 3*time.Second), http.MethodPost, "/v1/shutdown", map[string]bool{"ifIdle": true}, &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || out.Error == nil || !strings.Contains(out.Error.Message, "lease") {
		t.Fatalf("shutdown ifIdle with a lease answered %+v, want a refusal that names the lease", out)
	}
	time.Sleep(300 * time.Millisecond)
	c.Close()
	if _, err := c.Status(ctxFor(t, 3*time.Second)); err != nil {
		t.Fatalf("the daemon is gone after a refused shutdown: %v", err)
	}
}

// Design 2.1: IWDP_NO_DAEMON=1 runs the core in this process: no socket,
// no socket directory and no daemon process.
func TestNoDaemonModeRunsInProcess(t *testing.T) {
	noDaemonCore(t)
	dir := filepath.Join(shortDir(t), "iwdp")
	c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: dir, Exe: "/bin/false", Version: "test"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	f := newFake(t, fakewebkit.Options{})
	pc, err := c.OpenPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}})
	if err != nil {
		t.Fatalf("OpenPage: %v", err)
	}
	res, err := pc.Send(ctxFor(t, 5*time.Second), "Runtime.evaluate", map[string]any{"expression": "1+1"})
	if err != nil || !strings.Contains(string(res), `"value":2`) {
		t.Fatalf("Runtime.evaluate: %s, %v", res, err)
	}
	_ = pc.Close()
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-daemon mode created %s (%v)", dir, err)
	}
	if c.Hello.PID != os.Getpid() {
		t.Fatalf("the core runs in pid %d, want this process %d", c.Hello.PID, os.Getpid())
	}
}
