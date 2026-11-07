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
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
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
var ErrNotRunning = errors.New("the iwdp daemon is not running")

// Options configure Connect.
type Options struct {
	Dir     string   // socket directory; "" = SocketDir()
	Exe     string   // executable to start; "" = os.Executable()
	Args    []string // its arguments; nil = "daemon run"
	Env     []string // added to the started daemon's environment
	Version string   // this client's version, for messages
	NoStart bool     // only connect; ErrNotRunning when no daemon answers
	// StartIWDP is the in-process core's iwdp start (IWDP_NO_DAEMON; see
	// RunOptions.StartIWDP). A started daemon uses its own.
	StartIWDP func(context.Context) error
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
	return &Client{dir: dir, hc: &http.Client{Transport: tr}}
}

// Dir is the socket directory of the daemon.
func (c *Client) Dir() string { return c.dir }

// Close releases the client's idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// notRunning reports a dial error that means no daemon listens.
func notRunning(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// Connect returns a client of the daemon of o.Dir. When no daemon answers
// it starts one (design 2.3): an exclusive flock on daemon.lock, a second
// check, then "<exe> daemon run" in its own session with stdin closed and
// output appended to the daemon log, polled every 20 ms for up to 3 s. A
// daemon of another protocol is asked to exit when it has no leases.
// The socket directory is checked (lstat: no symlink, this user's, mode
// 0700) before any connection, so a socket that another user placed is
// never trusted. With IWDP_NO_DAEMON=1 the core runs in this process.
func Connect(ctx context.Context, o Options) (*Client, error) {
	if o.Dir == "" {
		o.Dir = SocketDir()
	}
	if NoDaemon() {
		return connectInProcess(ctx, o)
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
		if h.Protocol == Protocol {
			c.Hello = h
			return c, nil
		}
		if err := c.retire(ctx, h); err != nil {
			return nil, err
		}
	} else if !notRunning(err) {
		return nil, fmt.Errorf("connecting to the iwdp daemon at %s: %w", SocketPath(o.Dir), err)
	}
	if o.NoStart {
		return nil, fmt.Errorf("%w (no daemon answers at %s)", ErrNotRunning, SocketPath(o.Dir))
	}
	unlock, err := lockFile(ctx, filepath.Join(o.Dir, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	if h, err := c.hello(ctx); err == nil && h.Protocol == Protocol {
		c.Hello = h // another client started it while this one waited
		return c, nil
	}
	exited, logPath, err := spawn(o)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	for {
		h, err := c.hello(ctx)
		if err == nil && h.Protocol == Protocol {
			c.Hello = h
			return c, nil
		}
		select {
		case werr := <-exited:
			return nil, fmt.Errorf("the iwdp daemon exited at start (%v); see %s", werr, logPath)
		case <-ctx.Done():
			return nil, fmt.Errorf("starting the iwdp daemon: %w", ctx.Err())
		case <-time.After(startPoll):
		}
		if time.Since(start) > StartTimeout {
			return nil, fmt.Errorf("the iwdp daemon did not answer within %s of its start (last error: %v); see %s", StartTimeout, err, logPath)
		}
	}
}

// retire asks a daemon of another protocol to exit when it has no leases
// (design 2.2) and waits until its socket is gone. The daemon checks the
// leases again when the request arrives (ifIdle).
func (c *Client) retire(ctx context.Context, h Hello) error {
	busy := fmt.Errorf("daemon version %s (protocol %d) is running with active sessions; run 'iwdp-cli daemon stop' when they finish", h.Version, h.Protocol)
	if h.Leases > 0 {
		return busy
	}
	var out shutdownReply
	if err := c.call(ctx, http.MethodPost, "/v1/shutdown", shutdownRequest{IfIdle: true}, &out); err != nil {
		return fmt.Errorf("asking daemon version %s (protocol %d) to exit: %w", h.Version, h.Protocol, err)
	}
	if out.Error != nil {
		return fmt.Errorf("%w (%s)", busy, out.Error.Message)
	}
	c.Close()
	return c.WaitGone(ctx, stopWait)
}

// WaitGone waits until no daemon answers on the socket.
func (c *Client) WaitGone(ctx context.Context, limit time.Duration) error {
	end := time.Now().Add(limit)
	for {
		c.Close() // a kept-alive connection would still reach the old daemon
		if _, err := c.hello(ctx); err != nil && notRunning(err) {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("the iwdp daemon at %s is still answering after %s", SocketPath(c.dir), limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startPoll):
		}
	}
}

// lockFile takes an exclusive flock on path, polling until ctx ends.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon lock: %w", err)
	}
	for {
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
			return nil, "", fmt.Errorf("starting the iwdp daemon: %w", err)
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
	cmd.Env = append(os.Environ(), "IWDP_DAEMON_DIR="+o.Dir)
	cmd.Env = append(cmd.Env, o.Env...)
	cmd.Stdout, cmd.Stderr = logf, logf // stdin stays /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting the iwdp daemon (%s daemon run): %w", exe, err)
	}
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }() // reaps the child if it exits while this process runs
	return ch, logPath, nil
}

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
		return fmt.Errorf("%w (%v)", cerr, err)
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

// Lease runs a lease operation (design 2.5) and returns the page status.
func (c *Client) Lease(ctx context.Context, req LeaseRequest) ([]core.PageStatus, error) {
	var out struct {
		Pages []core.PageStatus `json:"pages"`
		Error *ErrorReply       `json:"error"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/lease", req, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, &RemoteError{*out.Error}
	}
	return out.Pages, nil
}

// Raw sends one protocol call (design 2.10). A deadline of ctx is sent as
// the call's timeout.
func (c *Client) Raw(ctx context.Context, req RawRequest) (json.RawMessage, error) {
	if dl, ok := ctx.Deadline(); ok && req.TimeoutMs == 0 {
		req.TimeoutMs = max(ms(time.Until(dl)), 1)
	}
	var out RawReply
	if err := c.call(ctx, http.MethodPost, "/v1/raw", req, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, &RemoteError{*out.Error}
	}
	return out.Result, nil
}

// RunResult is the result item of a registry command.
type RunResult struct {
	Text string
	Data json.RawMessage
}

// Run runs a registry command in the daemon (POST /v1/run); onLine gets
// each streamed line before the result.
func (c *Client) Run(ctx context.Context, req RunRequest, onLine func(string)) (RunResult, error) {
	if dl, ok := ctx.Deadline(); ok && req.TimeoutMs == 0 {
		req.TimeoutMs = max(ms(time.Until(dl)), 1)
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
