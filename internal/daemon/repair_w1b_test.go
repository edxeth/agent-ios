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

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
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

// Design 2.2: a client of another protocol asks the daemon to exit only
// when it is idle; the daemon checks that itself when the request arrives,
// so a daemon with a lease stays.
func TestShutdownIfIdleRefusedWithALease(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	if err := leasePage(t, c, f.URL, Owner{Session: "s1"}, 5*time.Second); err != nil { // the lease stays
		t.Fatal(err)
	}
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
	// The page outlives the core (cleanups run last-in first-out), so
	// that the core's cleanup reaches it instead of racing its close.
	f := newFake(t, fakewebkit.Options{})
	noDaemonCore(t)
	dir := filepath.Join(shortDir(t), "iwdp")
	c, err := Connect(ctxFor(t, 3*time.Second), Options{Dir: dir, Exe: "/bin/false", Version: "test"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	res, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: []string{"eval", "1+1"}, PageReq: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}}, nil)
	if err != nil || strings.TrimSpace(res.Text) != "2" {
		t.Fatalf("eval 1+1: %+v, %v", res, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-daemon mode created %s (%v)", dir, err)
	}
	if c.Hello.PID != os.Getpid() {
		t.Fatalf("the core runs in pid %d, want this process %d", c.Hello.PID, os.Getpid())
	}
}
