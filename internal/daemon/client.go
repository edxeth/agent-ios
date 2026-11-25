package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/cdp"
	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/logcause"
)

// Auto-start timing (design 2.3): poll the socket every 20 ms for up to 3 s.
const (
	startPoll    = 20 * time.Millisecond
	StartTimeout = 3 * time.Second
	// stopWait bounds how long a client waits for a daemon it asked to
	// exit to remove its socket (the daemon's cleanup budget plus margin).
	stopWait = ShutdownBudget + time.Second
)

// BuildVersion is the module version of this binary ("(devel)" for a
// local build), reported by /v1/hello.
func BuildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(unknown)"
}

// ErrNotRunning: no daemon answers on the socket (Options.NoStart).
var ErrNotRunning = errors.New("the agent-ios daemon is not running")

// Options configure Connect.
type Options struct {
	Dir     string   // socket directory; "" = SocketDir()
	Exe     string   // executable to start; "" = os.Executable()
	Args    []string // its arguments; nil = "daemon run"
	Env     []string // added to the started daemon's environment
	Version string   // this client's version, for messages
	NoStart bool     // only connect; ErrNotRunning when no daemon answers
	// StartIWDP is the in-process core's iwdp start (AGENT_IOS_NO_DAEMON; see
	// RunOptions.StartIWDP). A started daemon uses its own.
	StartIWDP func(context.Context) error
	// RecoverIWDP is the in-process core's iwdp recovery (see
	// RunOptions.RecoverIWDP). A started daemon uses its own.
	RecoverIWDP func(context.Context) (bool, error)
}

// Client talks to the daemon's socket API (design 2.2).
type Client struct {
	Hello Hello
	// Logf reports problems that have no caller to return them to (an
	// event stream that cannot start); nil drops them.
	Logf func(format string, args ...any)
	dir  string
	hc   *http.Client
}

// newClient returns a client of the socket in dir. Every connection is
// checked: its peer must run as this user (design 2.2, R16).
func newClient(dir string) *Client {
	sock := SocketPath(dir)
	uid := os.Getuid()
	return newClientDial(dir, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", sock)
		if err != nil {
			return nil, err
		}
		if err := verifyPeer(conn, sock, uid); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	})
}

func newClientDial(dir string, dial func(context.Context) (net.Conn, error)) *Client {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
	}
	return &Client{dir: dir, hc: &http.Client{Transport: answersAsIs{tr}}}
}

// answersAsIs makes every daemon answer final: it removes the Location of
// a 3xx answer, and http.Client returns a 3xx without a Location to its
// caller. So a redirect is an answer with an unexpected status, never
// followed, whatever its Location (http.Client parses the Location before
// it would ask CheckRedirect). A followed redirect's failure would stand
// for the daemon's answer (notRunning would read its closed connection as
// "no daemon answers"). It embeds the *http.Transport so that
// Client.Close still closes the idle connections (http.Client calls
// CloseIdleConnections of its Transport only when it has one).
type answersAsIs struct{ *http.Transport }

func (t answersAsIs) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Header.Del("Location")
	}
	return resp, err
}

// Dir is the socket directory of the daemon.
func (c *Client) Dir() string { return c.dir }

// Close releases the client's idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// notRunning reports a request error that means no daemon answers: none
// listens (no socket: ENOENT; a socket without a listener: ECONNREFUSED),
// or the daemon accepted the connection and closed or reset it without
// an answer. A daemon that exits closes its listener and its connections,
// so a request that reaches it in that window (a hello, or the request
// after a hello that it still answered) gets EOF, ECONNRESET (the request
// was unread) or EPIPE (the close came while the client wrote).
// Only errors of the connection count (the *url.Error of the request): a
// daemon that answers, also with an error status or a broken body, does
// answer.
func notRunning(err error) bool {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Err.Error() == serverClosedIdle {
		return true
	}
	for _, gone := range []error{syscall.ENOENT, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, io.EOF} {
		if errors.Is(ue.Err, gone) {
			return true
		}
	}
	return false
}

// serverClosedIdle is the text of net/http's errServerClosedIdle, which is
// not exported: the transport reports an EOF with it when the EOF comes
// before the request was sent on the connection.
const serverClosedIdle = "http: server closed idle connection"

// Connect returns a client of the daemon of o.Dir. When no daemon answers
// it starts one (design 2.3): an exclusive flock on daemon.lock, a second
// check, then "<exe> daemon run" in its own session with stdin closed and
// output appended to the daemon log, polled every 20 ms for up to 3 s. A
// daemon of another protocol or another executable (reusable) is asked to
// exit when it has no leases (retire), only under the lock and after the
// second check, so that two clients never both replace it: the second
// one waits for the lock and then reuses the daemon that the first one
// started. The new daemon starts only when the old one is gone
// (WaitGone). With o.NoStart (daemon
// status, daemon stop) no daemon is ever asked to exit: a daemon of this
// protocol or of previousProtocol is used whatever executable it runs, so
// that 'daemon status' reports and 'daemon stop' stops the daemon of an
// older binary, idle or busy.
// The socket directory is checked (lstat: no symlink, this user's, mode
// 0700) before any connection, so a socket that another user placed is
// never trusted. With AGENT_IOS_NO_DAEMON=1 the core runs in this process.
func Connect(ctx context.Context, o Options) (*Client, error) {
	if o.Dir == "" {
		o.Dir = SocketDir()
	}
	if NoDaemon() {
		return connectInProcess(ctx, o)
	}
	if err := checkSocketPath(SocketPath(o.Dir)); err != nil {
		return nil, err
	}
	if err := CheckDir(o.Dir, os.Getuid()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if o.NoStart {
			return nil, fmt.Errorf("%w (no daemon directory %s)", ErrNotRunning, o.Dir)
		}
		if err := EnsureDir(o.Dir); err != nil {
			return nil, err
		}
	}
	c := newClient(o.Dir)
	h, err := c.hello(ctx)
	if err == nil {
		if o.NoStart {
			if h.Protocol != Protocol && h.Protocol != previousProtocol {
				return nil, fmt.Errorf("daemon version %s (protocol %d) is running at %s; this agent-ios speaks protocol %d and cannot inspect or stop it: use the agent-ios that started it", h.Version, h.Protocol, SocketPath(o.Dir), Protocol)
			}
			c.Hello = h
			return c, nil
		}
		ok, err := reusable(h)
		if err != nil {
			return nil, err
		}
		if ok {
			c.Hello = h
			return c, nil
		}
		// Another protocol or executable: retired under the lock below.
	} else if !notRunning(err) {
		return nil, fmt.Errorf("connecting to the agent-ios daemon at %s: %w", SocketPath(o.Dir), err)
	}
	if o.NoStart {
		return nil, fmt.Errorf("%w (no daemon answers at %s)", ErrNotRunning, SocketPath(o.Dir))
	}
	unlock, err := lockFile(ctx, filepath.Join(o.Dir, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	h, err = c.hello(ctx)
	for waited := false; err != nil; waited = true {
		// No daemon answers. One may still hold run.lock: an exiting
		// daemon closes its listener before it releases the lock (idle
		// exit, a 'daemon stop' of another client), and one that starts
		// takes the lock before it listens. A daemon started now would
		// exit at once ("already running"), so wait until the lock is
		// free or a daemon answers.
		free, ferr := runLockFree(o.Dir)
		if ferr != nil {
			return nil, ferr
		}
		if free {
			break
		}
		if !waited && runLockWaiting != nil {
			runLockWaiting()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("starting the agent-ios daemon: a daemon that does not answer holds %s: %w", filepath.Join(o.Dir, "run.lock"), ctx.Err())
		case <-time.After(startPoll):
		}
		c.Close() // a kept-alive connection would still reach the old daemon
		h, err = c.hello(ctx)
	}
	if err == nil {
		ok, err := reusable(h)
		if err != nil {
			return nil, err
		}
		if ok {
			c.Hello = h // another client started it while this one waited
			return c, nil
		}
		// The daemon of the first hello, or one that another binary
		// started while this client waited for the lock.
		if err := c.retire(ctx, h); err != nil {
			return nil, err
		}
	}
	exited, logPath, err := spawn(o)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	for {
		h, err := c.hello(ctx)
		if err == nil {
			ok, rerr := reusable(h)
			if rerr != nil {
				return nil, rerr
			}
			if ok {
				c.Hello = h
				return c, nil
			}
			err = fmt.Errorf("the daemon that answers (pid %d) runs another executable", h.PID)
		}
		select {
		case werr := <-exited:
			return nil, logcause.Errorf("the agent-ios daemon exited at start (%v); see %s", werr, logPath)
		case <-ctx.Done():
			return nil, fmt.Errorf("starting the agent-ios daemon: %w", ctx.Err())
		case <-time.After(startPoll):
		}
		if time.Since(start) > StartTimeout {
			return nil, logcause.Errorf("the agent-ios daemon did not answer within %s of its start (last error: %v); see %s", StartTimeout, err, logPath)
		}
	}
}

// reusable reports whether a client may use the running daemon that
// answered h: the same protocol, and the same executable content as this
// process (runsThisExecutable).
func reusable(h Hello) (bool, error) {
	if h.Protocol != Protocol {
		return false, nil
	}
	return runsThisExecutable(h.Executable)
}

// retire asks a daemon of another protocol or executable to exit when it
// has no leases (design 2.2) and waits until its socket is gone. The
// daemon checks the leases again when the request arrives (ifIdle). A
// request that the daemon closes without an answer (notRunning) means it
// is going away already (an idle exit, another client's 'daemon stop'):
// retire waits for it in the same way.
func (c *Client) retire(ctx context.Context, h Hello) error {
	build := ""
	if h.Protocol == Protocol {
		build = ", another executable"
	}
	busy := fmt.Errorf("daemon version %s (protocol %d%s) is running with active sessions; run 'agent-ios daemon stop' when they finish", h.Version, h.Protocol, build)
	if h.Leases > 0 {
		return busy
	}
	var out shutdownReply
	err := c.call(ctx, http.MethodPost, "/v1/shutdown", shutdownRequest{IfIdle: true}, &out)
	switch {
	case err != nil && !notRunning(err):
		return fmt.Errorf("asking daemon version %s (protocol %d) to exit: %w", h.Version, h.Protocol, err)
	case err != nil: // going away
	case out.Error != nil:
		return fmt.Errorf("%w (%s)", busy, out.Error.Message)
	case retireReplied != nil:
		retireReplied()
	}
	c.Close()
	return c.WaitGone(ctx, stopWait)
}

// retireReplied, when set, is called when the daemon that retire asked
// to exit has replied. Tests set it to hold a client there.
var retireReplied func()

// WaitGone waits until no daemon answers on the socket and the daemon
// that answered has ended: it closes its listener before it releases
// run.lock (Run), and a daemon started before that release exits at
// once ("already running").
func (c *Client) WaitGone(ctx context.Context, limit time.Duration) error {
	end := time.Now().Add(limit)
	for {
		c.Close() // a kept-alive connection would still reach the old daemon
		still := "answering"
		if _, err := c.hello(ctx); err != nil && notRunning(err) {
			free, err := runLockFree(c.dir)
			if err != nil {
				return err
			}
			if free {
				return nil
			}
			still = "holding " + filepath.Join(c.dir, "run.lock")
		}
		if time.Now().After(end) {
			return fmt.Errorf("the agent-ios daemon at %s is still %s after %s", SocketPath(c.dir), still, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startPoll):
		}
	}
}

// runLockFree reports whether no daemon holds run.lock of dir (runLock):
// it takes the lock without waiting and releases it at once. A missing
// lock file is free.
func runLockFree(dir string) (bool, error) {
	path := filepath.Join(dir, "run.lock")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("daemon run lock: %w", err)
	}
	defer func() { _ = f.Close() }() // closing also releases the lock
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("daemon run lock %s: %w", path, err)
	}
	return true, nil
}

// runLockWaiting, when set, is called once when Connect finds run.lock
// held while no daemon answers, before it polls. Tests set it to learn
// that a client waits for that lock.
var runLockWaiting func()

// lockWaiting, when set, is called once when lockFile finds the lock
// held, before it polls. Tests set it to learn that a client waits for
// the startup lock.
var lockWaiting func()

// lockFile takes an exclusive flock on path, polling until ctx ends.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon lock: %w", err)
	}
	for waited := false; ; waited = true {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("daemon lock %s: %w", path, err)
		}
		if !waited && lockWaiting != nil {
			lockWaiting()
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the daemon lock %s: %w", path, ctx.Err())
		case <-time.After(startPoll):
		}
	}
}

// spawn starts the daemon process. exited receives its exit error if it
// ends while this client runs.
func spawn(o Options) (exited <-chan error, logPath string, err error) {
	exe := o.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, "", fmt.Errorf("starting the agent-ios daemon: %w", err)
		}
	}
	args := o.Args
	if args == nil {
		args = []string{"daemon", "run"}
	}
	if logPath, err = LogPath(); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, "", fmt.Errorf("daemon log directory: %w", err)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("daemon log: %w", err)
	}
	defer func() { _ = logf.Close() }() // the child keeps its own descriptor
	cmd := exec.Command(exe, args...)
	cmd.Dir = "/" // never keep a caller's (possibly temporary) directory busy
	cmd.Env = append(os.Environ(), "AGENT_IOS_DAEMON_DIR="+o.Dir)
	cmd.Env = append(cmd.Env, o.Env...)
	cmd.Stdout, cmd.Stderr = logf, logf // stdin stays /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if spawnStarting != nil {
		spawnStarting(o.Dir)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting the agent-ios daemon (%s daemon run): %w", exe, err)
	}
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }() // reaps the child if it exits while this process runs
	return ch, logPath, nil
}

// spawnStarting, when set, is called just before spawn starts a daemon
// process for the socket directory dir. Tests set it to check that no
// daemon holds run.lock at that moment.
var spawnStarting func(dir string)

// ---- requests ----

const base = "http://iwdp"

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("daemon %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	return resp, nil
}

// call sends a request and decodes its JSON answer into out.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return wrapCtx(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return wrapCtx(ctx, fmt.Errorf("daemon %s %s: reading the answer: %w", method, path, err))
	}
	return nil
}

// wrapCtx makes an error caused by ctx carry ctx's error, so a deadline is
// recognized as one (exit 124, D9).
func wrapCtx(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return logcause.Errorf("%w (%v)", cerr, err)
	}
	return err
}

func (c *Client) hello(ctx context.Context) (Hello, error) {
	var h Hello
	err := c.call(ctx, http.MethodGet, "/v1/hello", nil, &h)
	return h, err
}

// Status is the answer of GET /v1/status.
type Status struct {
	Daemon            Hello              `json:"daemon"`
	Pages             []core.PageStatus  `json:"pages"`
	Reservations      []core.Reservation `json:"reservations,omitempty"`
	ReservationsError string             `json:"reservationsError,omitempty"`
	CDP               cdp.Status         `json:"cdp"` // the CDP listener, without its token (design 2.2, 5.1)
}

// Status returns the daemon's pages, leases and reservations.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.call(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// shutdownRequest is the body of POST /v1/shutdown. IfIdle: exit only
// when no lease exists at the time the daemon handles the request.
type shutdownRequest struct {
	Force  bool `json:"force"`
	IfIdle bool `json:"ifIdle,omitempty"`
}

type shutdownReply struct {
	OK    bool        `json:"ok"`
	Error *ErrorReply `json:"error,omitempty"`
}

// Shutdown asks the daemon to run its cleanup and exit.
func (c *Client) Shutdown(ctx context.Context, force bool) error {
	var out shutdownReply
	if err := c.call(ctx, http.MethodPost, "/v1/shutdown", shutdownRequest{Force: force}, &out); err != nil {
		return err
	}
	if out.Error != nil {
		return &RemoteError{*out.Error}
	}
	return nil
}

// RunResult is the result item of a registry command.
type RunResult struct {
	Text string
	Data json.RawMessage
}

// Run runs a registry command in the daemon (POST /v1/run); onLine gets
// each streamed line before the result.
func (c *Client) Run(ctx context.Context, req RunRequest, onLine func(string)) (RunResult, error) {
	return c.RunWithPage(ctx, req, onLine, nil)
}

// RunWithPage is Run; onPage, when not nil, gets the page that the
// command's operation runs on as soon as the daemon has it (a "page"
// item), so that a later error can name the page.
func (c *Client) RunWithPage(ctx context.Context, req RunRequest, onLine func(string), onPage func(PageInfo)) (RunResult, error) {
	if dl, ok := ctx.Deadline(); ok && req.Deadline.IsZero() {
		req.Deadline = dl
	}
	resp, err := c.request(ctx, http.MethodPost, "/v1/run", req)
	if err != nil {
		return RunResult{}, wrapCtx(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxItem)
	for sc.Scan() {
		var it Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			return RunResult{}, fmt.Errorf("daemon /v1/run: bad item: %w", err)
		}
		switch {
		case it.Error != nil:
			return RunResult{}, &RemoteError{*it.Error}
		case it.Type == "line":
			if onLine != nil {
				onLine(it.Text)
			}
		case it.Type == "page" && it.Page != nil:
			if onPage != nil {
				onPage(*it.Page)
			}
		case it.Type == "result":
			return RunResult{Text: it.Text, Data: it.Data}, nil
		}
	}
	err = sc.Err()
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return RunResult{}, wrapCtx(ctx, fmt.Errorf("daemon /v1/run ended without a result: %w", err))
}

// maxItem bounds one NDJSON item (a screenshot result can be large).
const maxItem = 256 << 20
