package sim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runner runs a program on this host to its end and returns its standard
// output. A failure is an error that holds the end of its standard error.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the Runner of real programs.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A child that keeps the output pipes open after the end of ctx (ssh's
	// ControlMaster, a wrapper script) does not hold Output past it.
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = "..." + msg[len(msg)-400:]
		}
		if msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Mac runs commands on the Mac of a Config over ssh.
//
// Only read-only, repeatable steps (read) share one ssh connection; every
// other step (shell), and the forward, has a connection of its own. When a
// read-only step cannot attach to the shared connection (stopped, full, or
// gone), ssh logs in anew by itself: that step then runs twice at most,
// which changes nothing, where an action that ran twice would.
type Mac struct {
	cfg Config
	run Runner
	// share is the connection that the read-only steps share (Shared),
	// found by the first of them; nil shares none. m's copies share it.
	share *shared
	// probeWait bounds Reachable's step over the shared connection.
	probeWait time.Duration
}

// NewMac returns the Mac of cfg; run runs ssh on this host. It shares no
// connection (Shared).
func NewMac(cfg Config, run Runner) Mac { return Mac{cfg: cfg, run: run} }

// The shared connection (OpenSSH's ControlMaster). A step over an open
// connection costs about 15 ms where a login costs about 177 ms (measured
// against the macOS VM, 2026-11-25).
const (
	// controlPersist keeps the connection open after its last step.
	controlPersist = "5m"
	// The connection ends at the keepalive check after aliveCountMax
	// unanswered ones: within 9 s of a Mac that stopped answering (a
	// killed or frozen VM), also while no step runs (OpenSSH 10.6p2
	// against a frozen sshd: 8.7 s).
	aliveInterval = "3"
	aliveCountMax = "2"
	// sharedProbeWait bounds Reachable's step over the connection, which
	// answers in milliseconds while the Mac does. The master holds the
	// step's output pipes, so ExecRunner returns WaitDelay later (OpenSSH
	// 10.6p2 against a frozen sshd: 5.1 s in all).
	sharedProbeWait = 3 * time.Second
	// controlWait bounds ssh -O, which only talks to the local master.
	controlWait = 2 * time.Second
	// routeWait bounds ssh -G, which can run Match exec commands of the
	// ssh configuration (whose children can keep ssh's pipes open).
	routeWait = time.Second
	// sunPathMax is the size of a unix socket path on macOS (Linux: 108),
	// with its NUL; ssh binds the master's socket first at its path plus
	// 17 bytes (".XXXXXXXXXXXXXXXX").
	sunPathMax = 104
)

// safeDir is the form of a directory that a control socket may be in:
// absolute, of characters that neither ssh's option parser (spaces,
// quotes) nor its expansions of ControlPath (%, a leading ~, ${}) read.
var safeDir = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*$`)

// controlName names the socket of a destination: a hash of the
// configuration file, the destination and its route, ssh's resolved
// configuration of it (ssh -G), so that another alias, file, host, port or
// user, or an edit of any, does not reach a master of another. Its 40 hex
// digits are literal to ssh.
func controlName(cfg Config, route string) string {
	h := sha256.Sum256([]byte(cfg.SSHConfig + "\x00" + cfg.SSH + "\x00" + route))
	return hex.EncodeToString(h[:20])
}

// controlFits reports whether ssh can bind a control socket at path.
func controlFits(path string) bool { return len(path)+17 < sunPathMax }

// shared is the connection that a Mac and its copies share: the
// directory of its socket, and the socket, which the first read-only step
// finds (Mac.control).
type shared struct {
	dir string
	// mu guards found: nil until a step starts the lookup, closed when
	// control is known.
	mu      sync.Mutex
	found   chan struct{}
	control string // "" shares none; written before found is closed
}

// Shared returns m whose read-only steps share one ssh connection through
// a control socket in dir, a private directory (ControlDir). It runs
// nothing: the first read-only step reads the route (control), so that
// building the simulator's setup, and the steps before the first query
// (a check of the URL, the forward's stop), run as without sharing.
func (m Mac) Shared(dir string) Mac {
	m.share = &shared{dir: dir}
	m.probeWait = sharedProbeWait
	return m
}

// control returns the socket of the shared connection, or "" for none.
// The first call reads the route, ssh -G on a connection of its own,
// bounded by routeWait and by ctx; the answer holds for m and its copies,
// that is for the command. Nothing is shared when ssh -G fails or does not
// answer in time, names a ProxyCommand or ProxyJump (the route then
// depends on what ssh -G does not show, such as the proxy's target or the
// jump host's own configuration), or gives a hostname that is not an IP
// address (shareableRoute), or when the directory is not safe for ssh or
// too long for a socket (controlAt); the caller gets no error from it.
// Another caller waits for the first's answer until its own ctx ends, and
// then shares nothing.
func (m Mac) control(ctx context.Context) string {
	s := m.share
	if s == nil {
		return ""
	}
	s.mu.Lock()
	if found := s.found; found != nil {
		s.mu.Unlock()
		select {
		case <-found:
			return s.control
		case <-ctx.Done():
			return ""
		}
	}
	s.found = make(chan struct{})
	s.mu.Unlock()
	s.control = m.lookup(ctx, s.dir)
	close(s.found)
	return s.control
}

// knownControl is the socket of the shared connection when a step has
// found it already, else "": it runs nothing.
func (m Mac) knownControl() string {
	s := m.share
	if s == nil {
		return ""
	}
	s.mu.Lock()
	found := s.found
	s.mu.Unlock()
	if found == nil {
		return ""
	}
	select {
	case <-found:
		return s.control
	default:
		return ""
	}
}

// lookup reads the route of m's destination (ssh -G) and returns the
// socket in dir for it, or "" (control). It returns when its bound ends,
// also while ssh, killed, still waits for a child that holds its pipes.
func (m Mac) lookup(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, routeWait)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := m.run(ctx, "ssh", m.sshArgs([]string{"-G"}, "")...)
		done <- result{out, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
	}
	if ctx.Err() != nil || r.err != nil || !shareableRoute(string(r.out)) {
		return ""
	}
	return controlAt(m.cfg, dir, string(r.out))
}

// literalHost reports whether ssh -G's hostname is an IP address: a
// master keeps the address it connected to, while a login on a connection
// of its own resolves a name again, so after a DNS change the shared
// queries and the actions could reach different Macs.
func literalHost(route string) bool {
	for _, l := range strings.Split(route, "\n") {
		if k, v, _ := strings.Cut(strings.TrimSpace(l), " "); strings.EqualFold(k, "hostname") {
			_, err := netip.ParseAddr(strings.TrimSpace(v))
			return err == nil
		}
	}
	return false
}

// shareableRoute reports whether ssh -G's output is a route that may be
// shared: no proxy, and an IP address as hostname.
func shareableRoute(route string) bool { return !viaProxy(route) && literalHost(route) }

// viaProxy reports whether ssh -G's output names a proxy.
func viaProxy(route string) bool {
	for _, l := range strings.Split(route, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		if (strings.EqualFold(k, "proxycommand") || strings.EqualFold(k, "proxyjump")) && !strings.EqualFold(strings.TrimSpace(v), "none") {
			return true
		}
	}
	return false
}

// controlAt is the socket in dir of cfg's destination with route, or ""
// when dir is not safe for ssh or the socket's path would pass the unix
// limit.
func controlAt(cfg Config, dir, route string) string {
	dir = filepath.Clean(dir)
	if !safeDir.MatchString(dir) {
		return ""
	}
	p := filepath.Join(dir, controlName(cfg, route))
	if !controlFits(p) {
		return ""
	}
	return p
}

// controlFallback is the directory of the control sockets when the state
// directory is too long for them (macOS's $TMPDIR).
var controlFallback = func() string { return fmt.Sprintf("/tmp/iwdp-ssh-%d", os.Getuid()) }

// ControlDir returns the private directory of the shared ssh connection:
// "ssh" in base, the private state directory, or controlFallback when a
// socket there would pass the unix limit. It creates the directory with
// mode 0700 (whatever the umask) and refuses one that is a symlink,
// another user's, not of mode exactly 0700, or not writable.
func ControlDir(base string) (string, error) {
	dir := filepath.Join(base, "ssh")
	if !controlFits(filepath.Join(dir, controlName(Config{}, ""))) {
		dir = controlFallback()
	}
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		err = os.Chmod(dir, 0o700) // the umask may have taken bits
	} else if errors.Is(err, fs.ErrExist) {
		err = nil
	}
	if err != nil {
		return "", fmt.Errorf("creating the ssh control directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("checking the ssh control directory: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() ||
		fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 ||
		syscall.Access(dir, 0o3) != nil { // W_OK|X_OK: a read-only file system
		return "", fmt.Errorf("the ssh control directory %s is not a usable private directory of this user (mode %v)", dir, fi.Mode())
	}
	return dir, nil
}

// ownConnection are the options of an ssh call that neither opens nor
// uses a shared connection, whatever the ssh configuration says.
var ownConnection = []string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}

// sharing are the options of a read-only step over the shared connection
// of socket control: ssh attaches to its master, or logs in anew and
// becomes the master.
func sharing(control string) []string {
	return []string{
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + control, "-o", "ControlPersist=" + controlPersist,
		"-o", "ServerAliveInterval=" + aliveInterval, "-o", "ServerAliveCountMax=" + aliveCountMax,
	}
}

// sshArgs are the arguments of an ssh call to the Mac that runs script, or
// of one with extra options before the destination (a forward), on a
// connection of its own: a call shares only through readArgs.
func (m Mac) sshArgs(opts []string, script string) []string {
	return m.args(ownConnection, opts, script)
}

// readArgs are the arguments of a read-only step: over the shared
// connection of socket control, or on one of its own when control is "".
func (m Mac) readArgs(control string, opts []string, script string) []string {
	if control == "" {
		return m.sshArgs(opts, script)
	}
	return m.args(sharing(control), opts, script)
}

func (m Mac) args(conn, opts []string, script string) []string {
	var a []string
	if m.cfg.SSHConfig != "" {
		a = append(a, "-F", m.cfg.SSHConfig)
	}
	a = append(a, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10")
	a = append(a, conn...)
	a = append(a, opts...)
	a = append(a, m.cfg.SSH)
	if script != "" {
		a = append(a, "--", script)
	}
	return a
}

// shell runs script, a step that may act, in the login shell of the Mac's
// user, on a connection of its own.
func (m Mac) shell(ctx context.Context, script string) ([]byte, error) {
	return m.run(ctx, "ssh", m.sshArgs(nil, script)...)
}

// readGrace is how long a shared step may run past the end of its ctx: a
// master started or used by the step keeps its output pipes open, which
// ExecRunner would wait up to WaitDelay (2 s) for. The killed ssh client
// is gone by then; its runner returns in the background.
const readGrace = 200 * time.Millisecond

// read runs script, a query that only reads and may run twice without
// harm, over the shared connection when m has one. Once ctx has ended, its
// result is ctx's error, whatever ssh answered: an answer that came at the
// deadline is no answer (an empty one would read as a default).
func (m Mac) read(ctx context.Context, script string) ([]byte, error) {
	type result struct {
		out []byte
		err error
	}
	control := m.control(ctx)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ssh: %w", ctx.Err())
	}
	done := make(chan result, 1)
	go func() {
		out, err := m.run(ctx, "ssh", m.readArgs(control, nil, script)...)
		done <- result{out, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		select {
		case r = <-done:
		case <-time.After(readGrace):
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ssh: %w", ctx.Err())
	}
	return r.out, r.err
}

// controlCmd sends op (check, stop) to the master of the shared
// connection of socket control, bounded by controlWait.
func (m Mac) controlCmd(ctx context.Context, control, op string) error {
	ctx, cancel := context.WithTimeout(ctx, controlWait)
	defer cancel()
	_, err := m.run(ctx, "ssh", m.readArgs(control, []string{"-O", op}, "")...)
	return err
}

// CloseShared makes the shared connection take no new steps (ssh -O stop:
// its master removes its socket and ends once the steps that run over it
// end), for a Mac that goes away (sim down --mac). Later read-only steps
// open a new one. It uses only a route that a step of m found already
// (sim down's Reachable looks it up), and runs nothing to find one.
func (m Mac) CloseShared(ctx context.Context) {
	if c := m.knownControl(); c != "" {
		_ = m.controlCmd(ctx, c, "stop") // no master: nothing to stop
	}
}

// Reachable reports whether ssh logs in to the Mac. The end of ctx is an
// error, not an unreachable Mac.
//
// A shared connection's master can answer while the Mac is gone (a killed
// or frozen VM), and a step over it then waits for the keepalives. So a
// master that answers (ssh -O check) gets one "true", bounded by
// probeWait; when that fails, it takes no new steps (ssh -O stop) and the
// answer comes from fresh logins on connections of their own, as without a
// shared connection. Only a master that ran a step now counts.
//
// A busy Mac (another agent's simulator starting) can be slow to accept a
// login, and a "no" here makes sim up run the start command and sim status
// say the Mac is down, so it asks twice before it answers no (live run,
// 2026-11-24: one 5 s attempt failed while another agent's sim up ran).
func (m Mac) Reachable(ctx context.Context) (bool, error) {
	if c := m.control(ctx); c != "" && m.controlCmd(ctx, c, "check") == nil {
		pctx, cancel := context.WithTimeout(ctx, m.probeWait)
		_, err := m.read(pctx, "true")
		cancel()
		if err == nil {
			return true, nil
		}
		if ctx.Err() == nil {
			_ = m.controlCmd(ctx, c, "stop")
		}
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("checking ssh to %s: %w", m.cfg.SSH, ctx.Err())
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = m.shell(ctx, "true"); err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("checking ssh to %s: %w", m.cfg.SSH, ctx.Err())
		}
	}
	return false, nil
}

// LoggedIn reports whether a user is logged in to the Mac's desktop (the
// console), which the simulator needs.
func (m Mac) LoggedIn(ctx context.Context) (bool, error) {
	out, err := m.read(ctx, "who | grep -q ' console ' && echo yes || echo no")
	if err != nil {
		return false, fmt.Errorf("checking the desktop login on %s: %w", m.cfg.SSH, err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// Device is a simulator of the Mac (simctl list devices).
type Device struct {
	UDID    string `json:"udid"`
	Name    string `json:"name"`
	State   string `json:"state"` // Booted, Shutdown, ...
	Runtime string `json:"runtime"`
}

// udidRE is the form of a simulator UDID. Only such a UDID reaches a shell
// command line on the Mac.
var udidRE = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)

// BootedAll lists the booted simulators of the Mac on every platform
// (iOS, watchOS, tvOS, visionOS): quitting the Simulator app shuts down
// those it shows.
func (m Mac) BootedAll(ctx context.Context) ([]Device, error) {
	out, err := m.read(ctx, "xcrun simctl list devices booted -j")
	if err != nil {
		return nil, fmt.Errorf("listing the booted simulators on %s: %w", m.cfg.SSH, err)
	}
	var l struct {
		Devices map[string][]Device `json:"devices"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("reading the booted simulators: %w", err)
	}
	var ds []Device
	for rt, list := range l.Devices {
		for _, d := range list {
			if d.State == "Booted" {
				d.Runtime = rt
				ds = append(ds, d)
			}
		}
	}
	return ds, nil
}

// Devices lists the available iOS simulators of the Mac.
func (m Mac) Devices(ctx context.Context) ([]Device, error) {
	out, err := m.read(ctx, "xcrun simctl list devices available -j")
	if err != nil {
		return nil, fmt.Errorf("listing the simulators on %s: %w", m.cfg.SSH, err)
	}
	return parseDevices(out)
}

// parseDevices reads the iOS devices of 'simctl list devices -j', sorted by
// runtime version and name.
func parseDevices(out []byte) ([]Device, error) {
	var l struct {
		Devices map[string][]Device `json:"devices"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("reading the simulator list: %w", err)
	}
	var ds []Device
	for rt, list := range l.Devices {
		if !strings.Contains(rt, ".SimRuntime.iOS-") {
			continue
		}
		for _, d := range list {
			if !udidRE.MatchString(d.UDID) {
				continue
			}
			d.Runtime = strings.ReplaceAll(rt[strings.Index(rt, ".SimRuntime.")+len(".SimRuntime."):], "-", ".")
			d.Runtime = strings.Replace(d.Runtime, "iOS.", "iOS ", 1)
			ds = append(ds, d)
		}
	}
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Runtime != ds[j].Runtime {
			return newer(ds[j].Runtime, ds[i].Runtime)
		}
		return ds[i].Name < ds[j].Name
	})
	return ds, nil
}

// ErrNoDevice is the error of Pick when no simulator matches.
var ErrNoDevice = errors.New("no simulator matches")

// Pick chooses the simulator: want is a UDID or an exact name (the newest
// runtime's device of that name); "" means the one booted device.
func Pick(ds []Device, want string) (Device, error) {
	if want == "" {
		var booted []Device
		for _, d := range ds {
			if d.State == "Booted" {
				booted = append(booted, d)
			}
		}
		if len(booted) == 1 {
			return booted[0], nil
		}
		return Device{}, fmt.Errorf("%w: %d simulators are booted; name one with --device or \"device\" in the configuration (available: %s)", ErrNoDevice, len(booted), names(ds))
	}
	var found *Device
	for i := range ds {
		d := ds[i]
		if d.UDID == strings.ToUpper(want) {
			return d, nil
		}
		if d.Name == want && (found == nil || newer(d.Runtime, found.Runtime)) {
			found = &ds[i]
		}
	}
	if found == nil {
		return Device{}, fmt.Errorf("%w %q (available: %s)", ErrNoDevice, want, names(ds))
	}
	return *found, nil
}

// newer reports whether runtime a ("iOS 26.5") is newer than b, by the
// numbers of their versions.
func newer(a, b string) bool {
	na, nb := versionNumbers(a), versionNumbers(b)
	for i := 0; i < len(na) && i < len(nb); i++ {
		if na[i] != nb[i] {
			return na[i] > nb[i]
		}
	}
	return len(na) > len(nb)
}

func versionNumbers(rt string) []int {
	_, v, _ := strings.Cut(rt, " ")
	var n []int
	for _, p := range strings.Split(v, ".") {
		x, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		n = append(n, x)
	}
	return n
}

func names(ds []Device) string {
	if len(ds) == 0 {
		return "none; install an iOS Simulator runtime in Xcode"
	}
	s := make([]string, len(ds))
	for i, d := range ds {
		s[i] = fmt.Sprintf("%s (%s, %s)", d.Name, d.Runtime, d.UDID)
	}
	return strings.Join(s, "; ")
}

// Boot boots the simulator and waits until it has finished booting.
func (m Mac) Boot(ctx context.Context, udid string) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("booting a simulator: bad UDID %q", udid)
	}
	if _, err := m.shell(ctx, "xcrun simctl bootstatus "+udid+" -b >/dev/null"); err != nil {
		return fmt.Errorf("booting the simulator %s: %w", udid, err)
	}
	return nil
}

// Shutdown shuts the simulator down.
func (m Mac) Shutdown(ctx context.Context, udid string) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("shutting down a simulator: bad UDID %q", udid)
	}
	if _, err := m.shell(ctx, "xcrun simctl shutdown "+udid); err != nil {
		return fmt.Errorf("shutting down the simulator %s: %w", udid, err)
	}
	return nil
}

// OpenWindow opens the Simulator app with the window of the simulator.
// Quitting that app later shuts the simulators down.
func (m Mac) OpenWindow(ctx context.Context, udid string) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("opening the Simulator window: bad UDID %q", udid)
	}
	if _, err := m.shell(ctx, "open -a Simulator --args -CurrentDeviceUDID "+udid); err != nil {
		return fmt.Errorf("opening the Simulator window: %w", err)
	}
	return nil
}

// WindowOpen reports whether the Simulator app runs.
func (m Mac) WindowOpen(ctx context.Context) (bool, error) {
	out, err := m.read(ctx, "pgrep -xq Simulator && echo yes || echo no")
	if err != nil {
		return false, fmt.Errorf("checking the Simulator app: %w", err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// SafariRunning reports whether Safari runs in the simulator (its launchd
// job, which simctl spawn lists).
func (m Mac) SafariRunning(ctx context.Context, udid string) (bool, error) {
	if !udidRE.MatchString(udid) {
		return false, fmt.Errorf("checking Safari: bad UDID %q", udid)
	}
	// The list first, so that a failed simctl fails the call instead of
	// reading as "not running" (which would launch, and bring forward, a
	// running Safari).
	out, err := m.read(ctx, "l=$(xcrun simctl spawn "+udid+" launchctl list) || exit 1; "+
		"case \"$l\" in *'UIKitApplication:com.apple.mobilesafari['*) echo yes ;; *) echo no ;; esac")
	if err != nil {
		return false, fmt.Errorf("checking Safari in the simulator: %w", err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// LaunchSafari starts Safari in the simulator, or brings it to the front.
func (m Mac) LaunchSafari(ctx context.Context, udid string) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("launching Safari: bad UDID %q", udid)
	}
	if _, err := m.shell(ctx, "xcrun simctl launch "+udid+" com.apple.mobilesafari >/dev/null"); err != nil {
		return fmt.Errorf("launching Safari in the simulator: %w", err)
	}
	return nil
}

// CheckURL accepts an absolute http or https URL with a host: what
// sim up --url opens.
func CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--url %q: want an absolute http or https URL", rawURL)
	}
	return nil
}

// OpenURL opens rawURL (CheckURL) in the simulator's Safari.
func (m Mac) OpenURL(ctx context.Context, udid, rawURL string) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("opening a URL: bad UDID %q", udid)
	}
	if err := CheckURL(rawURL); err != nil {
		return err
	}
	if _, err := m.shell(ctx, "xcrun simctl openurl "+udid+" "+shellQuote(rawURL)); err != nil {
		return fmt.Errorf("opening the URL in the simulator: %w", err)
	}
	return nil
}

// shellQuote quotes s as one word for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ErrNoAXe is the error of the touch commands when the configuration names
// no axe program.
var ErrNoAXe = errors.New("the simulator configuration names no axe program (\"axe\"): real touches need AXe on the Mac, docs/simulator.md")

// AXe runs the configured axe subcommand sub on the Mac for the simulator
// udid, with the options opts and then, after "--", the positional
// arguments pos, so that text such as "--stdin" stays text. Every word is
// quoted for the Mac's shell. It returns axe's output.
func (m Mac) AXe(ctx context.Context, udid, sub string, opts []string, pos ...string) ([]byte, error) {
	if m.cfg.AXe == "" {
		return nil, ErrNoAXe
	}
	if !udidRE.MatchString(udid) {
		return nil, fmt.Errorf("running axe: bad UDID %q", udid)
	}
	words := []string{shellQuote(m.cfg.AXe), shellQuote(sub)}
	for _, a := range opts {
		words = append(words, shellQuote(a))
	}
	words = append(words, "--udid", udid)
	if len(pos) > 0 {
		words = append(words, "--")
		for _, a := range pos {
			words = append(words, shellQuote(a))
		}
	}
	out, err := m.shell(ctx, strings.Join(words, " "))
	if err != nil {
		return out, fmt.Errorf("axe %s on the simulator: %w", sub, err)
	}
	return out, nil
}

// USKeyboard reports whether the simulator's hardware keyboard layout is
// US: its first keyboard names hw=U.S., or it is US English with
// hw=Automatic while the Mac's own layout is US (the simulator then follows
// the Mac). layout describes
// what was read, for an error.
func (m Mac) USKeyboard(ctx context.Context, udid string) (us bool, layout string, err error) {
	if !udidRE.MatchString(udid) {
		return false, "", fmt.Errorf("reading the keyboard layout: bad UDID %q", udid)
	}
	out, err := m.read(ctx, "k=$(xcrun simctl spawn "+udid+" defaults read -g AppleKeyboards 2>/dev/null | sed -n 2p); "+
		"h=$(defaults read com.apple.HIToolbox AppleCurrentKeyboardLayoutInputSourceID 2>/dev/null); printf '%s\\n%s\\n' \"$k\" \"$h\"")
	if err != nil {
		return false, "", fmt.Errorf("reading the simulator's keyboard layout: %w", err)
	}
	lines := strings.SplitN(strings.TrimRight(string(out), "\n"), "\n", 2)
	for len(lines) < 2 {
		lines = append(lines, "")
	}
	sim := strings.Trim(strings.TrimSpace(lines[0]), "\",")
	mac := strings.TrimSpace(lines[1])
	layout = fmt.Sprintf("simulator %q, Mac %q", sim, mac)
	switch {
	case strings.Contains(sim, "hw=U.S."):
		return true, layout, nil
	case sim == "" || mac == "":
		// Unreadable: not taken for US.
		return false, layout, nil
	case strings.Contains(sim, "hw=Automatic"):
		// Conservative: a US Mac layout and US English keyboard in the
		// simulator; an Automatic layout under another language is not
		// taken for US.
		return mac == "com.apple.keylayout.US" && strings.HasPrefix(sim, "en_US"), layout, nil
	}
	return false, layout, nil
}

// HardwareKeyboard reports the Simulator app's "Connect Hardware Keyboard"
// setting, which is on unless set off. While it is on, iOS hides the
// on-screen keyboard; while it is off, typed key presses lose Shift.
func (m Mac) HardwareKeyboard(ctx context.Context) (bool, error) {
	out, err := m.read(ctx, "defaults read com.apple.iphonesimulator ConnectHardwareKeyboard 2>/dev/null || echo 1")
	if err != nil {
		return false, fmt.Errorf("reading the Simulator's keyboard setting: %w", err)
	}
	return strings.TrimSpace(string(out)) != "0", nil
}

// SetHardwareKeyboard sets "Connect Hardware Keyboard", which the
// Simulator app reads when it starts: it quits the app (which shuts down
// the simulators it shows), waits up to 20 s for it to end, shuts down
// udid when it still runs, and writes the setting. Each step must succeed;
// an error names the step, and the setting is written only after the app
// ended. It shuts down no simulator but udid.
func (m Mac) SetHardwareKeyboard(ctx context.Context, udid string, on bool) error {
	if !udidRE.MatchString(udid) {
		return fmt.Errorf("setting the Simulator's keyboard: bad UDID %q", udid)
	}
	script := "if pgrep -xq Simulator; then osascript -e 'quit app \"Simulator\"' >/dev/null || { echo 'quitting the Simulator app failed' >&2; exit 3; }; fi; " +
		"i=0; while pgrep -xq Simulator; do i=$((i+1)); [ $i -gt 20 ] && { echo 'the Simulator app did not quit within 20 s' >&2; exit 4; }; sleep 1; done; " +
		"l=$(xcrun simctl list devices " + udid + ") || { echo 'reading the simulator state failed' >&2; exit 6; }; " +
		"case \"$l\" in *'(Booted)'*) xcrun simctl shutdown " + udid + " || { echo 'shutting down the simulator failed' >&2; exit 5; } ;; esac; " +
		"defaults write com.apple.iphonesimulator ConnectHardwareKeyboard -bool " + strconv.FormatBool(on)
	if _, err := m.shell(ctx, script); err != nil {
		return fmt.Errorf("setting the Simulator's keyboard: %w", err)
	}
	return nil
}

// pngMagic starts every PNG file.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// Screenshot returns a PNG of the simulator's whole screen (simctl io
// screenshot): the status bar, Safari's own bars, the on-screen keyboard
// and other apps too, where a Web Inspector capture has the page only.
func (m Mac) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	if !udidRE.MatchString(udid) {
		return nil, fmt.Errorf("taking a simulator screenshot: bad UDID %q", udid)
	}
	// simctl's "-" for standard output writes a file named "-" (Xcode
	// 26.6), so the PNG goes through a private temporary directory.
	out, err := m.shell(ctx, "d=$(mktemp -d) || exit 1; "+
		"xcrun simctl io "+udid+" screenshot --type=png \"$d/screen.png\" >/dev/null 2>&1 && cat \"$d/screen.png\"; "+
		"rc=$?; rm -rf \"$d\"; exit $rc")
	if err != nil {
		return nil, fmt.Errorf("taking a screenshot of the simulator %s: %w", udid, err)
	}
	if !bytes.HasPrefix(out, pngMagic) {
		return nil, fmt.Errorf("taking a screenshot of the simulator %s: simctl wrote %d bytes that are not a PNG", udid, len(out))
	}
	return out, nil
}

// InspectorSocket is the path of the simulator's webinspectord_sim socket on
// the Mac: a socket of the simulator's launchd_sim, whose command line names
// the device directory. It changes at every boot.
func (m Mac) InspectorSocket(ctx context.Context, udid string) (string, error) {
	if !udidRE.MatchString(udid) {
		return "", fmt.Errorf("finding the Web Inspector socket: bad UDID %q", udid)
	}
	script := "p=$(pgrep -f 'launchd_sim .*/Devices/" + udid + "/' | head -1); " +
		"[ -n \"$p\" ] && lsof -a -U -p \"$p\" -Fn 2>/dev/null | sed -n 's/^n//p' | grep -m1 '/com.apple.webinspectord_sim.socket$'"
	out, err := m.read(ctx, script)
	sock := strings.TrimSpace(string(out))
	if err != nil || sock == "" {
		if err == nil {
			err = errors.New("the simulator has no webinspectord_sim socket")
		}
		return "", fmt.Errorf("finding the Web Inspector socket of the simulator %s: %w", udid, err)
	}
	if strings.ContainsAny(sock, " \t\n:") {
		return "", fmt.Errorf("the Web Inspector socket path %q cannot be forwarded", sock)
	}
	return sock, nil
}
