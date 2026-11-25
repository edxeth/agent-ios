package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// WS2b-1b-a item 1 (review ws2a-r3-c F9): the operation's deadline is the
// caller's absolute deadline, fixed before the request travels. A request
// whose body reaches the daemon after that deadline gets the deadline
// error (exit 124) and does no page work: no attach, no lease and no queue
// admission. The same request in time attaches and runs.
func TestLateRequestGetsTheDeadlineErrorWithoutPageWork(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	f := newFake(t, fakewebkit.Options{})
	owner := Owner{Session: "late"}
	argv := append(append([]string{}, leaseFixture...), "Runtime.evaluate")
	const budget = 300 * time.Millisecond
	deadline := time.Now().Add(budget)
	body, err := json.Marshal(RunRequest{Argv: argv, Deadline: deadline, PageReq: PageReq{Page: f.URL}, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", SocketPath(o.Dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := t.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "POST /v1/run HTTP/1.1\r\nHost: iwdp\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body)); err != nil {
		t.Fatal(err)
	}
	// The daemon has the request's headers; its body, with the deadline
	// fixed above, arrives after that deadline. The wait past the
	// deadline is the behavior under test.
	time.Sleep(time.Until(deadline) + 100*time.Millisecond)
	if _, err := conn.Write(body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var it Item
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		t.Fatalf("reading the first item: %v", err)
	}
	if it.Error == nil || !it.Error.Timeout || it.Error.Code != core.CodeTimeout {
		t.Fatalf("first item %+v (error %+v); want the deadline error (timeout, exit %d)", it, it.Error, core.CodeTimeout)
	}
	if n := f.ConnectionCount(); n != 0 {
		t.Fatalf("the late request dialed the page %d time(s); want no page work", n)
	}
	c := connect(t, o)
	st, err := c.Status(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pages) != 0 || st.Daemon.Leases != 0 {
		t.Fatalf("after the late request the daemon has pages %+v and %d lease(s); want none", st.Pages, st.Daemon.Leases)
	}
	if err := leasePage(t, c, f.URL, owner, 5*time.Second, "Runtime.evaluate"); err != nil {
		t.Fatalf("the same request in time: %v", err)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")); n != 1 {
		t.Fatalf("the request in time sent Runtime.evaluate %d time(s), want 1", n)
	}
}

// A request without a deadline runs under DefaultCommandTimeout from its
// arrival (RunRequest.Deadline): the zero deadline is not one that passed.
func TestRequestWithoutADeadlineRuns(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	// No context deadline, so the client sends none; the daemon's default
	// bounds the call.
	if _, err := c.Run(context.Background(), RunRequest{Argv: append(append([]string{}, leaseFixture...), "Runtime.evaluate"), PageReq: PageReq{Page: f.URL}, Owner: Owner{Session: "nodeadline"}}, nil); err != nil {
		t.Fatalf("a request without a deadline: %v; want it to run", err)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")); n != 1 {
		t.Fatalf("Runtime.evaluate reached the page %d time(s), want 1", n)
	}
}

// WS2b-1b-a item 3: a client that starts a daemon of other executable
// content (Options.Exe) never uses it, and its error says why.
func TestStartedDaemonOfAnotherExecutableIsNotUsed(t *testing.T) {
	o := spawnOptions(t, "AGENT_IOS_DAEMON_IDLE=1m")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	o.Exe = filepath.Join(t.TempDir(), "other-build")
	if err := os.WriteFile(o.Exe, append(data, "other build"...), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Connect(ctxFor(t, StartTimeout+5*time.Second), o)
	if err == nil || !strings.Contains(err.Error(), "runs another executable") {
		t.Fatalf("Connect that started a daemon of another executable: %v; want the error that names it", err)
	}
}

// fileSHA256 is the hex SHA-256 of the file at path.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// wantExecutable fails unless e is the identity and the SHA-256 of the
// file at path.
func wantExecutable(t *testing.T, e Executable, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := fileIDOf(fi)
	if err != nil {
		t.Fatal(err)
	}
	if e.FileID != id || e.SHA256 != fileSHA256(t, path) {
		t.Fatalf("hello reports executable %+v; want %+v with SHA-256 %s (%s)", e, id, fileSHA256(t, path), path)
	}
}

// WS2b-1b-a item 3: the hello reports the identity (device, inode, size,
// modification time) and the SHA-256 of the daemon's own executable, and
// of the file that RunOptions.Executable names when a process embeds the
// daemon for clients of another binary. A client that runs other content
// does not reuse such a daemon; a client that runs the same content does.
func TestHelloReportsTheDaemonExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, startInProcess(t, RunOptions{}))
	wantExecutable(t, c.Hello.Executable, self)
	if ok, err := reusable(c.Hello); !ok || err != nil {
		t.Fatalf("reusable(own daemon) = %v, %v; want true", ok, err)
	}

	other := filepath.Join(t.TempDir(), "other-binary")
	if err := os.WriteFile(other, []byte("another executable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c = connect(t, startInProcess(t, RunOptions{Executable: other}))
	wantExecutable(t, c.Hello.Executable, other)
	if ok, err := reusable(c.Hello); ok || err != nil {
		t.Fatalf("reusable(daemon of another binary) = %v, %v; want false", ok, err)
	}

	copied := filepath.Join(t.TempDir(), "copy")
	data, err := os.ReadFile(self)
	if err == nil {
		err = os.WriteFile(copied, data, 0o755)
	}
	if err != nil {
		t.Fatal(err)
	}
	c = connect(t, startInProcess(t, RunOptions{Executable: copied}))
	if c.Hello.Executable.FileID == mustFileID(t, self) {
		t.Fatalf("the copy has the file identity of the test binary; the SHA-256 comparison is not reached")
	}
	if ok, err := reusable(c.Hello); !ok || err != nil {
		t.Fatalf("reusable(daemon of an identical copy) = %v, %v; want true", ok, err)
	}
	if ok, err := reusable(Hello{Protocol: Protocol - 1, Executable: c.Hello.Executable}); ok || err != nil {
		t.Fatalf("reusable(daemon of protocol %d) = %v, %v; want false", Protocol-1, ok, err)
	}
}

func mustFileID(t *testing.T, path string) FileID {
	t.Helper()
	id, err := statExe(path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A daemon whose executable cannot be read does not start: it would
// report no identity that a client could compare.
func TestRunRefusesAnUnreadableExecutable(t *testing.T) {
	base := shortDir(t)
	missing := filepath.Join(base, "missing")
	err := Run(ctxFor(t, 5*time.Second), RunOptions{Dir: filepath.Join(base, "agent-ios"), Version: "test", Executable: missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("Run with a missing executable: %v; want an error that names %s", err, missing)
	}
	if _, err := os.Lstat(SocketPath(filepath.Join(base, "agent-ios"))); err == nil {
		t.Fatalf("a socket was created")
	}
}
