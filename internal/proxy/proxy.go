package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/logcause"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

const (
	// DefaultListingPort is the port where iwdp lists all connected devices.
	// http://localhost:9221 → JSON array of devices
	DefaultListingPort = 9221

	// DefaultFirstDevicePort is the port pinned to the first real device
	// when auto-starting the proxy. Each additional device gets the next
	// port (9225, 9226, ...).
	// http://localhost:9224/json → JSON array of pages for that device
	DefaultFirstDevicePort = 9224

	// DefaultLastDevicePort is the end of the device port range used when
	// auto-starting the proxy (9224-9229).
	DefaultLastDevicePort = 9229
)

// listingHost is used for all proxy HTTP requests. Use 127.0.0.1, not
// localhost: localhost resolves to ::1 first, and some hosts' firewalls drop
// ::1, turning probes into multi-second hangs instead of fast dials.
const listingHost = "127.0.0.1"

// listingAddr is iwdp's listing address, listingHost:DefaultListingPort.
// It is a variable only so that the e2e tests can link their binaries
// with a private fake listing (-ldflags -X, e2e/listing_w1c6_test.go);
// no flag or environment variable sets it.
var listingAddr = "127.0.0.1:9221"

// ListingAddr is iwdp's default listing address (host:port), which
// IsRunning, Start, EnsureRunning, ListDevices and the iwdp transport use.
func ListingAddr() string { return listingAddr }

// ProbeBudget bounds one check whether iwdp's listing answers (CheckListing,
// IsRunning and the daemon's check). iwdp is a single-threaded event loop;
// under heavy WebKit inspector traffic or a loaded host its /json can stall
// past 2 s, and a shorter probe reports a running iwdp as stopped.
const ProbeBudget = 5 * time.Second

// readTimeout bounds one read of a listing (ListDevices, ListPages).
const readTimeout = 5 * time.Second

// StartBackoff is how long Start returns the error of a failed start
// without a new attempt, so a broken iwdp is not started on every call.
const StartBackoff = 30 * time.Second

const (
	// startReadyWait bounds the wait for a started iwdp's listing.
	startReadyWait = 5 * time.Second
	// startPoll is the interval of the readiness checks.
	startPoll = 200 * time.Millisecond
	// stopGrace is how long a started iwdp that failed its start gets to
	// exit after SIGTERM before its process group gets SIGKILL.
	stopGrace = 2 * time.Second
	// stopReap bounds the wait for the rest of that process group to go.
	stopReap = 2 * time.Second
)

// ErrNotListening means that no process owns the iwdp listing address:
// every address of it can be bound. Only then may iwdp be started (D10).
var ErrNotListening = errors.New("nothing listens on the iwdp listing address")

// CheckListing reports whether ios_webkit_debug_proxy answers on its listing
// address addr (host:port) within ProbeBudget. It returns an error wrapping
// ErrNotListening when the address can be bound (portFree), nil when
// GET /json answers 200, and otherwise an error that says a process owns
// the address but does not answer as iwdp, and how it failed: a status
// other than 200, a refusal, an empty reply, no answer within ProbeBudget,
// or another error. Then no iwdp may be started.
func CheckListing(ctx context.Context, addr string) error {
	pctx, cancel := context.WithTimeout(ctx, ProbeBudget)
	defer cancel()
	// Ownership first: only a successful bind proves that no process owns
	// the address. A refused connection does not (a socket that is bound
	// but does not listen refuses too).
	bindErr := portFree(pctx, addr)
	if bindErr == nil {
		return fmt.Errorf("%w (%s)", ErrNotListening, addr)
	}
	err := getListing(pctx, addr)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("checking ios_webkit_debug_proxy on %s: %w", addr, ctx.Err())
	}
	var limit time.Duration
	if errors.Is(pctx.Err(), context.DeadlineExceeded) {
		limit = ProbeBudget
	}
	return ownerFailure(addr, err, bindErr, limit)
}

// ReadFailure classifies err, a failed read of GET /json on the iwdp
// endpoint addr (its device list, or a device's page list) after a
// CheckListing of the listing passed, as CheckListing does: an error
// wrapping ErrNotListening when nothing owns addr, otherwise how the owner
// failed. A failed request (an empty reply with the advice, a timeout, a
// refusal or another error) is told apart from an answer that arrived but
// is unusable (a status other than 200, or a body that does not decode).
// The check is no evidence that a later read succeeds: the iwdp of the
// second device check closed listing requests while a page WebSocket was
// open (docs/known-limits.md).
func ReadFailure(ctx context.Context, addr string, err error) error {
	bindErr := portFree(ctx, addr)
	if bindErr == nil {
		return fmt.Errorf("%w (%s)", ErrNotListening, addr)
	}
	// Only the request's own failure (*url.Error) is a failed request; the
	// read wraps it with its own advice ("Is ios_webkit_debug_proxy
	// running?", ListDevices and ListPages), which is dropped.
	var ue *url.Error
	if !errors.As(err, &ue) {
		return logcause.Errorf("ios_webkit_debug_proxy on %s answered GET /json, but its answer is not a usable list (%v). A process owns the port, so no second iwdp is started", addr, err)
	}
	err = ue
	var limit time.Duration
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		limit = readTimeout
	}
	return ownerFailure(addr, err, bindErr, limit)
}

// ownerFailure describes err, a failed GET /json on addr, which a process
// owns (bindErr is why addr cannot be bound). limit is the time limit that
// ended the request, or 0 when no limit was hit.
func ownerFailure(addr string, err, bindErr error, limit time.Duration) error {
	var status statusError
	if errors.As(err, &status) {
		return fmt.Errorf("the process on %s answers GET /json with HTTP %d, which is not ios_webkit_debug_proxy's device list; no second iwdp is started while it owns the port", addr, int(status))
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return logcause.Errorf("%s refuses connections but cannot be bound (%v): another process holds the port without listening, or the address is not local; no second iwdp is started", addr, bindErr)
	}
	const owned = "A process owns the port, so no second iwdp is started"
	const restart = "The restart_iwdp tool and 'iwdp-cli restart-iwdp' restart it only when iwdp-mcp started it; they refuse another program's iwdp"
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET):
		// The second device check saw this from an iwdp while a page
		// WebSocket was open (docs/known-limits.md); the cause is unknown.
		return logcause.Errorf("ios_webkit_debug_proxy on %s closed the connection without an answer to GET /json (an empty reply: %v). An iwdp has been seen to do this while a WebSocket to one of its pages is open, for example the iwdp-cli daemon's: run 'iwdp-cli daemon stop' to close the daemon's page connections, or restart that iwdp. %s. %s",
			addr, err, owned, restart)
	case limit > 0:
		return logcause.Errorf("ios_webkit_debug_proxy on %s does not answer GET /json within %s (%v): it is busy or stuck. %s; wait and try again. %s",
			addr, limit, err, owned, restart)
	}
	return logcause.Errorf("GET /json on %s failed (%v), so ios_webkit_debug_proxy does not answer there. %s. %s", addr, err, owned, restart)
}

// statusError is a listing answer other than 200.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func getListing(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json", nil)
	if err != nil {
		return err
	}
	req.Close = true // no idle connection is kept in iwdp's event loop
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp.StatusCode)
	}
	return nil
}

// Starter starts ios_webkit_debug_proxy for one listing address. It never
// starts iwdp while a process owns that address, starts one process at a
// time (also across processes, lockStart), accepts only a listener of the
// process it started as ready, stops a started process that does not
// become ready, and backs off for StartBackoff after a failed start. It
// records each iwdp it started (saveRecord), and Restart stops only a
// recorded iwdp.
type Starter struct {
	addr      string        // listing address, host:port
	readyWait time.Duration // startReadyWait; shorter in tests
	// listens tells who receives a connection to an endpoint
	// (groupListens; tests choose an implementation or a failure).
	listens func(ctx context.Context, pgid int, ep netip.AddrPort) (listener, error)
	// identify reads a process's identity (readIdentity) and kill sends a
	// signal (syscall.Kill); tests inject failures.
	identify func(pid int) (identity, error)
	kill     func(pid int, sig syscall.Signal) error

	mu       sync.Mutex // held for a whole start: one start at a time
	failErr  error      // the last failed start
	failedAt time.Time  // when it failed
	own      *record    // the iwdp this Starter started last, until Restart stops it
}

// NewStarter returns a Starter for the listing address addr (host:port).
func NewStarter(addr string) *Starter {
	return &Starter{addr: addr, readyWait: startReadyWait, listens: groupListens, identify: readIdentity, kill: syscall.Kill}
}

// std is the Starter of iwdp's default listing address, used by IsRunning,
// Start and EnsureRunning.
var std = NewStarter(listingAddr)

// IsRunning checks if ios_webkit_debug_proxy answers on the listing port
// (9221) within ProbeBudget. This is the canonical way to detect a running
// proxy.
func IsRunning() bool {
	return CheckListing(context.Background(), std.addr) == nil
}

// Start starts ios_webkit_debug_proxy on the default listing port unless it
// already runs (Starter.Start).
func Start(ctx context.Context) error { return std.Start(ctx) }

// Start starts ios_webkit_debug_proxy as a background process in its own
// process group, so it outlives the caller, and waits until its listing
// answers. It returns nil at once when iwdp already answers, and the
// CheckListing error, without a start, when another process owns the port.
// When the started process does not answer within the readiness wait,
// another process takes the port, or ctx ends first, Start stops its
// process group and waits for it to exit before it returns the error. A
// failed start (not a cancelled one, nor one that lost the port to another
// process) is returned again, without an attempt, for StartBackoff. A
// started iwdp that answers is recorded for Restart (saveRecord); when
// the record cannot be written, Start stops it and fails.
func (s *Starter) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockStart(ctx, s.addr)
	if err != nil {
		return err
	}
	defer unlock()
	return s.startLocked(ctx, false)
}

// startLocked is Start with s.mu and the start lock held. With fresh (a
// restart that stopped the recorded iwdp), an iwdp that answers is not
// accepted as running: it is not the stopped iwdp's successor, so
// startLocked fails.
func (s *Starter) startLocked(ctx context.Context, fresh bool) error {
	err := CheckListing(ctx, s.addr)
	if fresh && err == nil {
		return fmt.Errorf("restarting ios_webkit_debug_proxy: the recorded iwdp was stopped, but another process serves %s now, so no new ios_webkit_debug_proxy was started", s.addr)
	}
	if !errors.Is(err, ErrNotListening) {
		return err // nil (running) or a process owns the port
	}
	if s.failErr != nil {
		if since := time.Since(s.failedAt); since < StartBackoff {
			return fmt.Errorf("%w (failed %s ago; not trying again for another %s)", s.failErr, since.Round(time.Second), (StartBackoff - since).Round(time.Second))
		}
	}
	err = s.launch(ctx)
	if err != nil && ctx.Err() == nil && !errors.Is(err, errPortTaken) {
		s.failErr, s.failedAt = err, time.Now()
	} else {
		s.failErr = nil
	}
	return err
}

// maxIWDPLog is the size at which a start keeps the iwdp log as
// iwdp.log.1 and begins a new one, so the logs take at most about twice
// this plus one run's output.
const maxIWDPLog = 1 << 20

// LogPath is the log of an iwdp that iwdp-mcp starts:
// $XDG_STATE_HOME/iwdp-mcp/iwdp.log, else ~/.local/state/iwdp-mcp/iwdp.log.
// It is iwdp-mcp's own file: the user's iwdpctl writes and truncates
// ~/.local/state/iwdp/iwdp.log.
func LogPath() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "iwdp-mcp", "iwdp.log"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("the iwdp log: no XDG_STATE_HOME and no home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "iwdp-mcp", "iwdp.log"), nil
}

// openIWDPLog opens the iwdp log (LogPath) for appending, with mode 0600
// in a directory of mode 0700. A log of maxIWDPLog bytes or more is kept
// as iwdp.log.1 first, replacing an older one.
func openIWDPLog() (*os.File, error) {
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the directory of the iwdp log: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxIWDPLog {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, fmt.Errorf("keeping the previous iwdp log: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the iwdp log: %w", err)
	}
	return f, nil
}

// linuxSimulatorAddr is the SIMULATOR inspector address that a start on
// Linux passes to iwdp (-s). iwdp 1.9.2 connects a SIMULATOR inspector to
// localhost:27753 at start and tries each address of localhost in turn: a
// refused 127.0.0.1 goes on to the next address, so where ::1 is one of
// them and does not answer, that connect stays pending, whichever address
// comes first. About 133 s later iwdp removes it and leaves its send queue
// behind, so the next connection on that file-descriptor number gets an
// empty reply until iwdp restarts (docs/known-limits.md). With the one
// address 127.0.0.1 the connect is refused at once.
// Linux runs no iOS Simulator, so nothing listens there anyway; macOS
// keeps iwdp's default, which the simulator tests use.
const linuxSimulatorAddr = "127.0.0.1:27753"

// simulatorArgs are the iwdp arguments that set the SIMULATOR inspector
// address on goos: -s linuxSimulatorAddr on Linux, none elsewhere.
func simulatorArgs(goos string) []string {
	if goos == "linux" {
		return []string{"-s", linuxSimulatorAddr}
	}
	return nil
}

// iwdpArgs are the arguments of an iwdp start on goos with the device
// configuration config: frontend off (-F), config (-c), and the SIMULATOR
// address of simulatorArgs.
func iwdpArgs(goos, config string) []string {
	return append([]string{"-F", "-c", config}, simulatorArgs(goos)...)
}

// manualCommand is the iwdp command line that start and recovery advice on
// goos tells a user to run: frontend off and the SIMULATOR address of
// simulatorArgs, as an automatic start uses, with iwdp's default device
// configuration.
func manualCommand(goos string) string {
	return strings.Join(append([]string{"ios_webkit_debug_proxy", "--no-frontend"}, simulatorArgs(goos)...), " ")
}

// ManualCommand is the iwdp command line that start and recovery advice on
// this system tells a user to run (manualCommand).
func ManualCommand() string { return manualCommand(runtime.GOOS) }

// iwdpCommand is the command that runs ios_webkit_debug_proxy with args.
// iwdp writes its stdout ("Listing devices", "Connected :9224 to ...")
// through C stdio, which buffers a file in blocks: those lines would reach
// the log only when iwdp exits (device check 3). GNU stdbuf -oL makes
// that stdout line-buffered (through an LD_PRELOAD library, so for a
// dynamically linked iwdp only), so iwdp runs under stdbuf when it is on
// PATH. Without stdbuf (macOS has none by default) the stdout lines appear
// at exit; stderr is unbuffered either way. stdbuf execs iwdp, so the
// started process keeps its pid, process group and environment.
func iwdpCommand(args ...string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("ios_webkit_debug_proxy")
	if err != nil {
		return nil, err
	}
	if sb, err := exec.LookPath("stdbuf"); err == nil {
		return exec.Command(sb, append([]string{"-oL", bin}, args...)...), nil
	}
	return exec.Command(bin, args...), nil
}

// launch runs one ios_webkit_debug_proxy, waits for its listing and
// records it. The started iwdp appends its stdout and stderr to the iwdp
// log (LogPath); it writes to the file itself, so the log goes on after
// the starting process exits.
func (s *Starter) launch(ctx context.Context) error {
	cmd, err := iwdpCommand(iwdpArgs(runtime.GOOS, BuildConfig(DeviceUDIDs()))...)
	if err != nil {
		return fmt.Errorf("starting ios_webkit_debug_proxy: %w\nMake sure it is installed: brew install ios-webkit-debug-proxy", err)
	}
	log, err := openIWDPLog()
	if err != nil {
		return fmt.Errorf("starting ios_webkit_debug_proxy: %w; no ios_webkit_debug_proxy was started", err)
	}
	defer func() { _ = log.Close() }() // the started process has its own copy
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	token := newToken()
	cmd.Env = append(os.Environ(), tokenEnv+"="+token)
	// Device discovery can take a while; ownership is checked again right
	// before the spawn. The readiness check covers the rest of the window.
	if err := portFree(ctx, s.addr); err != nil {
		return logcause.Errorf("%w: %s is no longer free (%v); no ios_webkit_debug_proxy was started", errPortTaken, s.addr, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ios_webkit_debug_proxy: %w\nMake sure it is installed: brew install ios-webkit-debug-proxy", err)
	}
	// The process is reaped here whether it fails its start or runs on.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	readyBy := time.Now().Add(s.readyWait)
	deadline := time.NewTimer(s.readyWait)
	defer deadline.Stop()
	poll := time.NewTicker(startPoll)
	defer poll.Stop()
	for {
		select {
		case <-exited:
			stopGroup(cmd.Process.Pid, exited)
			return logcause.Errorf("ios_webkit_debug_proxy exited during its start (%v); run '%s' to see why", waitErr, ManualCommand())
		case <-ctx.Done():
			stopGroup(cmd.Process.Pid, exited)
			return fmt.Errorf("starting ios_webkit_debug_proxy: %w; the started process was stopped", ctx.Err())
		case <-deadline.C:
			stopGroup(cmd.Process.Pid, exited)
			return fmt.Errorf("the started ios_webkit_debug_proxy did not answer on %s within %s, so it was stopped; run '%s' to see why", s.addr, s.readyWait, ManualCommand())
		case <-poll.C:
			ok, err := s.ready(ctx, readyBy, cmd.Process.Pid)
			if err != nil {
				stopGroup(cmd.Process.Pid, exited)
				return err
			}
			if !ok {
				continue
			}
			if err := s.saveRecord(cmd.Process.Pid, token); err != nil {
				stopGroup(cmd.Process.Pid, exited)
				return err
			}
			return nil
		}
	}
}

// ready reports whether the started process group pgid serves the listing.
// The group must own the listening socket that receives a connection to
// the listing endpoint (address and port; classify). A listener of another
// process is never the started iwdp: then ready returns an errPortTaken
// error. A failed ownership check is an error too, and when ctx ends
// during the check, ready returns ctx's error.
func (s *Starter) ready(ctx context.Context, readyBy time.Time, pgid int) (bool, error) {
	pctx, cancel := context.WithDeadline(ctx, readyBy)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(pctx, "tcp", s.addr)
	if err != nil {
		return false, nil // nothing listens yet; the wait goes on
	}
	remote := conn.RemoteAddr().(*net.TCPAddr).AddrPort()
	_ = conn.Close()
	ep := netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	owner, err := s.listens(pctx, pgid, ep)
	switch {
	case err != nil && ctx.Err() != nil:
		return false, fmt.Errorf("starting ios_webkit_debug_proxy: %w; the started process was stopped", ctx.Err())
	case err != nil && pctx.Err() != nil:
		return false, nil // the readiness wait ended; launch reports it
	case err != nil:
		return false, fmt.Errorf("checking which process listens on %s: %w; the started ios_webkit_debug_proxy was stopped", ep, err)
	}
	switch owner {
	case listenerForeign:
		return false, fmt.Errorf("%w: another process took port %d (%s) during the start of ios_webkit_debug_proxy; the started process was stopped and no other is started", errPortTaken, ep.Port(), ep)
	case listenerOwn:
		return getListing(pctx, s.addr) == nil, nil
	}
	return false, nil // the listener went away; the wait goes on
}

// stopGroup stops the process group pgid of a started iwdp: SIGTERM, then
// SIGKILL after stopGrace, until its leader has exited (exited closed), and
// SIGKILL for any rest of the group. It returns when the group is gone, or
// after stopReap more.
func stopGroup(pgid int, exited <-chan struct{}) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopGrace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-exited
	for end := time.Now().Add(stopReap); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
}

// StartError is an error of Status that comes from a start of iwdp: the
// listing address was free and Status started iwdp, which failed. Every
// other error of Status comes from its listing check, and then no start
// was attempted.
type StartError struct{ Err error }

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Status reports on iwdp at the default listing address and starts it
// when asked (Starter.Status); the iwdp_status tool uses it.
func Status(ctx context.Context, autoStart bool) (started bool, err error) {
	return std.Status(ctx, autoStart)
}

// Status checks once whether iwdp answers on the listing address
// (CheckListing) and returns false and nil when it does. When nothing owns
// the address, it returns the ErrNotListening error, or with autoStart it
// starts iwdp (Start) and returns true, or the failed start's error as a
// *StartError. Otherwise it returns the check's error and starts nothing.
// The one check serves both steps, so an owner that does not answer costs
// one ProbeBudget; Start's own check of the free address is a bind.
func (s *Starter) Status(ctx context.Context, autoStart bool) (started bool, err error) {
	err = CheckListing(ctx, s.addr)
	if !autoStart || !errors.Is(err, ErrNotListening) {
		return false, err
	}
	if err := s.Start(ctx); err != nil {
		return false, &StartError{Err: err}
	}
	return true, nil
}

// EnsureRunning checks if iwdp is running and starts it if not.
// If iwdp crashed (e.g., after a large heap snapshot), this will restart it.
// Start checks the listing first, so a running iwdp is left alone.
func EnsureRunning(ctx context.Context) error {
	return Start(ctx)
}

// ListDevices fetches connected devices from the listing port (9221).
// Each device entry contains a URL like "localhost:9224" indicating
// which port to query for that device's pages.
func ListDevices() ([]webkit.DeviceEntry, error) {
	return listDevicesFromURL("http://" + listingAddr + "/json")
}

func listDevicesFromURL(u string) ([]webkit.DeviceEntry, error) {
	client := &http.Client{Timeout: readTimeout}
	resp, err := client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("connecting to iwdp: %w\nIs ios_webkit_debug_proxy running?", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("iwdp returned HTTP %d for %s", resp.StatusCode, u)
	}

	var devices []webkit.DeviceEntry
	if err := json.NewDecoder(resp.Body).Decode(&devices); err != nil {
		return nil, fmt.Errorf("decoding device list: %w", err)
	}
	return devices, nil
}

// DeviceAddr is the address (host:port) of the device port devicePort,
// whose GET /json ListPages reads.
func DeviceAddr(devicePort int) string {
	return net.JoinHostPort(listingHost, strconv.Itoa(devicePort))
}

// ListPages fetches open pages/tabs from a device-specific port.
// The port comes from DeviceEntry.URL (e.g., "localhost:9224").
func ListPages(devicePort int) ([]webkit.PageEntry, error) {
	u := "http://" + DeviceAddr(devicePort) + "/json"
	client := &http.Client{Timeout: readTimeout}
	resp, err := client.Get(u)
	if err != nil {
		return nil, fmt.Errorf("connecting to device on port %d: %w\nCheck the device port from 'list_devices' output", devicePort, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device on port %d returned HTTP %d", devicePort, resp.StatusCode)
	}

	var pages []webkit.PageEntry
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		return nil, fmt.Errorf("decoding page list: %w", err)
	}
	return pages, nil
}

// ListAllPages fetches pages from all connected devices.
// It first lists devices on port 9221, then queries each device's port for pages.
// If some devices fail but others succeed, partial results are returned.
// If ALL devices fail, a combined error is returned.
func ListAllPages() ([]webkit.PageEntry, error) {
	devices, err := ListDevices()
	if err != nil {
		return nil, err
	}

	var allPages []webkit.PageEntry
	var errs []error
	for _, d := range devices {
		port, err := DevicePort(d)
		if err != nil {
			errs = append(errs, fmt.Errorf("device %q: %w", d.DeviceName, err))
			continue
		}
		pages, err := ListPages(port)
		if err != nil {
			errs = append(errs, fmt.Errorf("device %q (port %d): %w", d.DeviceName, port, err))
			continue
		}
		allPages = append(allPages, pages...)
	}

	// If all devices failed, return a combined error.
	if len(allPages) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("all devices failed: %w", errors.Join(errs...))
	}
	return allPages, nil
}

// DevicePort extracts the port number from a DeviceEntry's URL field.
// The URL is typically "localhost:9224".
func DevicePort(d webkit.DeviceEntry) (int, error) {
	if d.URL == "" {
		return 0, fmt.Errorf("device %q has no URL", d.DeviceName)
	}
	// Normalize: always add http:// so net/url can parse host:port correctly
	raw := d.URL
	if !hasScheme(raw) {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing device URL %q: %w", d.URL, err)
	}
	port := u.Port()
	if port == "" {
		return 0, fmt.Errorf("device URL %q has no explicit port", d.URL)
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		return 0, fmt.Errorf("parsing port from %q: %w", d.URL, err)
	}
	return p, nil
}

// hasScheme checks if a string starts with a URI scheme (e.g., "http://").
// A scheme must start with a letter and be followed by "://".
func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			// Scheme requires "://" after the colon
			return i > 0 && i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/'
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			continue
		case i > 0 && ((c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'):
			continue
		default:
			return false
		}
	}
	return false
}
