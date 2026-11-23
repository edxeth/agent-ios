package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
type Mac struct {
	cfg Config
	run Runner
}

// NewMac returns the Mac of cfg; run runs ssh on this host.
func NewMac(cfg Config, run Runner) Mac { return Mac{cfg: cfg, run: run} }

// sshArgs are the arguments of an ssh call to the Mac that runs script, or
// of one with extra options before the destination (a forward).
func (m Mac) sshArgs(opts []string, script string) []string {
	var a []string
	if m.cfg.SSHConfig != "" {
		a = append(a, "-F", m.cfg.SSHConfig)
	}
	a = append(a, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5")
	a = append(a, opts...)
	a = append(a, m.cfg.SSH)
	if script != "" {
		a = append(a, "--", script)
	}
	return a
}

// shell runs script in the login shell of the Mac's user.
func (m Mac) shell(ctx context.Context, script string) ([]byte, error) {
	return m.run(ctx, "ssh", m.sshArgs(nil, script)...)
}

// Reachable reports whether ssh logs in to the Mac. The end of ctx is an
// error, not an unreachable Mac.
func (m Mac) Reachable(ctx context.Context) (bool, error) {
	_, err := m.shell(ctx, "true")
	if err != nil && ctx.Err() != nil {
		return false, fmt.Errorf("checking ssh to %s: %w", m.cfg.SSH, ctx.Err())
	}
	return err == nil, nil
}

// LoggedIn reports whether a user is logged in to the Mac's desktop (the
// console), which the simulator needs.
func (m Mac) LoggedIn(ctx context.Context) (bool, error) {
	out, err := m.shell(ctx, "who | grep -q ' console ' && echo yes || echo no")
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

// Devices lists the available iOS simulators of the Mac.
func (m Mac) Devices(ctx context.Context) ([]Device, error) {
	out, err := m.shell(ctx, "xcrun simctl list devices available -j")
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
	out, err := m.shell(ctx, "pgrep -xq Simulator && echo yes || echo no")
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
	out, err := m.shell(ctx, "l=$(xcrun simctl spawn "+udid+" launchctl list) || exit 1; "+
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

// InspectorSocket is the path of the simulator's webinspectord_sim socket on
// the Mac: a socket of the simulator's launchd_sim, whose command line names
// the device directory. It changes at every boot.
func (m Mac) InspectorSocket(ctx context.Context, udid string) (string, error) {
	if !udidRE.MatchString(udid) {
		return "", fmt.Errorf("finding the Web Inspector socket: bad UDID %q", udid)
	}
	script := "p=$(pgrep -f 'launchd_sim .*/Devices/" + udid + "/' | head -1); " +
		"[ -n \"$p\" ] && lsof -a -U -p \"$p\" -Fn 2>/dev/null | sed -n 's/^n//p' | grep -m1 '/com.apple.webinspectord_sim.socket$'"
	out, err := m.shell(ctx, script)
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
