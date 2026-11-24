package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"math"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/proxy"
	"github.com/nnemirovsky/iwdp-cli/internal/sim"
)

// The simulator commands: sim up, sim status and sim down manage an iOS
// Simulator on a Mac that this host reaches over SSH (internal/sim,
// docs/simulator.md). They are Local: they run in the iwdp-cli process,
// which starts the ssh forward. Before a restart of iwdp they count the
// daemon's active leases through ActiveLeases, which the daemon package
// sets (it holds the daemon client).
func init() {
	device := Flag{Name: "device", Type: FlagString, Help: "the simulator: its UDID or exact name (default: \"device\" of the configuration, else the one booted simulator)"}
	Register(&Command{
		Name: "sim up", Local: true, Timeout: 15 * time.Minute,
		Summary: "Make the iOS Simulator of the Mac in the simulator configuration ready for iwdp-cli, doing only what is still needed: " +
			"start the Mac with the configuration's start command when ssh gets no answer, wait for the desktop login, boot the simulator, open its window, start Safari, " +
			"forward 127.0.0.1:27753 to its Web Inspector over ssh, and start ios_webkit_debug_proxy, or restart the one that iwdp-cli started when it does not use that forward. " +
			"Refuses that restart while a page has an active lease, unless --restart-iwdp, and refuses an iwdp that iwdp-cli did not start. Default deadline 15m.",
		Flags: []Flag{
			device,
			{Name: "no-window", Type: FlagBool, Help: "do not open the Simulator window"},
			{Name: "url", Type: FlagString, Help: "open this http or https URL in the simulator's Safari (a URL of this host: the Mac's address for it, for example http://10.0.2.2:8080/ in a QEMU VM)"},
			{Name: "restart-iwdp", Type: FlagBool, Help: "restart iwdp even when that ends active page leases"},
		},
		Check: func(in Input) error {
			if u, _ := in.Flags["url"].(string); u != "" {
				return sim.CheckURL(u)
			}
			return nil
		},
		Run: runSimUp,
	})
	Register(&Command{
		Name: "sim status", Local: true, Timeout: time.Minute,
		Summary: "Check each part that sim up sets up (the Mac over ssh, the desktop login, the simulator, its window, the forward, iwdp's listing) without changing any. Exits 0 also when a part is down; --json has \"ready\".",
		Flags:   []Flag{device},
		Run:     runSimStatus,
	})
	Register(&Command{
		Name: "sim screenshot", Local: true, Timeout: time.Minute,
		Summary: "Capture the simulator's whole screen as PNG (simctl io screenshot): the status bar, Safari's bars, the on-screen keyboard and other apps, which the page capture of screenshot leaves out. " +
			"The simulator must be booted (sim up); it starts nothing.",
		Args:  []Arg{{Name: "path", Help: "PNG file to write (default $TMPDIR/iwdp-screenshot-<time>.png)"}},
		Flags: []Flag{{Name: "output", Short: "o", Type: FlagString, Help: "PNG file to write (same as path)"}, device},
		Check: func(in Input) error { _, err := simShotPath(in); return err },
		Run:   runSimScreenshot,
	})
	touch := " A real touch through AXe (the configuration's \"axe\"), which iOS handles as the user's: a tapped text field opens the on-screen keyboard, unlike click. Coordinates are screen points (sim ui, sim screenshot's size / 3 on a 3x iPhone)."
	Register(&Command{
		Name: "sim tap", Local: true, Timeout: time.Minute,
		Summary: "Tap the simulator's screen at <x> <y>, or the element with --label or --id (sim ui lists them)." + touch,
		Args:    []Arg{{Name: "x", Help: "x in points"}, {Name: "y", Help: "y in points"}},
		Flags: []Flag{
			{Name: "label", Type: FlagString, Help: "tap the element with this accessibility label"},
			{Name: "id", Type: FlagString, Help: "tap the element with this accessibility identifier"},
			{Name: "hold", Type: FlagString, Help: "seconds the finger stays down: a long press"},
			device,
		},
		Check: simTapValues,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			args, err := simTapArgs(in)
			if err != nil {
				return Result{}, err
			}
			return runSimTouch(ctx, env, in, args, "Tapped.")
		},
	})
	Register(&Command{
		Name: "sim swipe", Local: true, Timeout: time.Minute,
		Summary: "Swipe on the simulator's screen from <x1> <y1> to <x2> <y2> in points (one second; --duration changes it)." + touch,
		Args:    []Arg{{Name: "x1", Help: "start x in points", Required: true}, {Name: "y1", Help: "start y in points", Required: true}, {Name: "x2", Help: "end x in points", Required: true}, {Name: "y2", Help: "end y in points", Required: true}},
		Flags:   []Flag{{Name: "duration", Type: FlagString, Help: "seconds the swipe takes"}, device},
		Check: func(in Input) error {
			if d := flagString(in, "duration"); d != "" {
				return simSeconds("sim swipe --duration", d)
			}
			return nil
		},
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			args, err := simSwipeArgs(in)
			if err != nil {
				return Result{}, err
			}
			return runSimTouch(ctx, env, in, args, "Swiped.")
		},
	})
	Register(&Command{
		Name: "sim type", Local: true, Timeout: time.Minute,
		Summary: "Type <text> into the simulator's focused field as key presses (sim tap the field first). The text is on the command lines of this host and the Mac: never type a secret this way; fill --stdin types into a page without that." + touch,
		Args:    []Arg{{Name: "text", Help: "the text to type", Required: true}},
		Flags:   []Flag{device},
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			s, err := newSim(env)
			if err != nil {
				return Result{}, err
			}
			if _, err := s.Type(ctx, flagString(in, "device"), in.Args[0]); err != nil {
				return Result{}, err
			}
			return Result{Text: "Typed."}, nil
		},
	})
	Register(&Command{
		Name: "sim keyboard", Local: true, Timeout: 15 * time.Minute,
		Summary: "Show or set the simulator's keyboard: onscreen shows iOS's on-screen keyboard when sim tap taps a field, as on an iPhone, for keyboard bugs; sim type then sends only lowercase letters, digits and unshifted symbols, on a US layout. " +
			"hardware (the Simulator's default) types the text that the simulator's keyboard layout has keys for (AXe refuses the rest) but hides the on-screen keyboard. A change restarts the Simulator app and the simulator, then runs sim up; it refuses while a page has an active lease, unless --restart-iwdp.",
		Args:  []Arg{{Name: "mode", Help: "onscreen or hardware; without it, print the mode"}},
		Flags: []Flag{device, {Name: "restart-iwdp", Type: FlagBool, Help: "restart even when that ends active page leases"}},
		Run:   runSimKeyboard,
	})
	Register(&Command{
		Name: "sim ui", Local: true, Timeout: time.Minute,
		Summary: "List the elements of the simulator's screen that have a label, value or identifier, with their frames in points (its accessibility tree, through AXe): the targets of sim tap. It shows page and app text.",
		Flags:   []Flag{device},
		Run:     runSimUI,
	})
	Register(&Command{
		Name: "sim down", Local: true, Timeout: 3 * time.Minute,
		Summary: "Stop the forward that sim up started and shut the simulator down; with --mac also run the configuration's stop command (for example the shutdown of a VM).",
		Flags:   []Flag{device, {Name: "mac", Type: FlagBool, Help: "also stop the Mac with the configuration's stop command"}},
		Run:     runSimDown,
	})
}

// newSim builds the Sim of the configuration file, with real programs.
var newSim = func(env Env) (*sim.Sim, error) {
	path, err := sim.ConfigPath()
	if err != nil {
		return nil, err
	}
	cfg, err := sim.Load(path)
	if err != nil {
		return nil, err
	}
	dir, err := SimStateDir()
	if err != nil {
		return nil, err
	}
	mac := sim.NewMac(cfg, sim.ExecRunner)
	procs := sim.Processes{
		Spawn:    sim.SpawnDetached(proxy.StartTokenEnv),
		Identify: proxy.ProcessIdentity,
		Signal:   syscall.Kill,
		Endpoint: func(ctx context.Context, pgid int) (bool, bool, error) {
			return proxy.EndpointOwner(ctx, pgid, netip.MustParseAddrPort(sim.InspectorAddr))
		},
		NewToken: proxy.NewStartToken,
	}
	return &sim.Sim{
		Cfg: cfg, Mac: mac, Run: sim.ExecRunner, Poll: 3 * time.Second, PageWait: 30 * time.Second,
		Forward: &sim.Forward{Mac: mac, Dir: dir, Addr: sim.InspectorAddr, Procs: procs, Wait: 15 * time.Second},
		IWDP:    hostIWDP{env: env},
	}, nil
}

// ActiveLeases counts the active page leases of the daemon on every
// device; no running daemon is 0. The daemon package sets it.
var ActiveLeases func(ctx context.Context, env Env) (int, error)

// SimStateDir is the private directory of the forward's record; the daemon
// package sets it to its socket directory.
var SimStateDir = func() (string, error) { return "", errors.New("sim: no state directory in this build") }

// hostIWDP is the iwdp on this host's listing address.
type hostIWDP struct{ env Env }

func (h hostIWDP) State(ctx context.Context) (sim.IWDPState, error) {
	listening, own, err := proxy.OwnsListing(ctx)
	if err != nil || !listening {
		return sim.IWDPState{}, err
	}
	st := sim.IWDPState{Running: true, Own: own}
	ds, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		return st, fmt.Errorf("reading ios_webkit_debug_proxy's device list: %w", err)
	}
	for _, d := range ds {
		if d.DeviceID == "SIMULATOR" {
			st.Listed = true
		}
	}
	return st, nil
}

func (h hostIWDP) Start(ctx context.Context) error { return withUSBMuxHint(proxy.Start(ctx)) }

// Restart restarts iwdp (proxy.Restart: only the one that iwdp-cli started).
func (h hostIWDP) Restart(ctx context.Context) error { return withUSBMuxHint(proxy.Restart(ctx)) }

func (h hostIWDP) ActiveLeases(ctx context.Context) (int, error) {
	if ActiveLeases == nil {
		return 0, errors.New("the daemon's leases cannot be read in this build; run sim up --restart-iwdp")
	}
	return ActiveLeases(ctx, h.env)
}

// SimulatorPages counts the pages of the SIMULATOR device in iwdp's listing.
func (h hostIWDP) SimulatorPages(ctx context.Context) (int, error) {
	ds, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		return 0, err
	}
	for _, d := range ds {
		if d.DeviceID != "SIMULATOR" {
			continue
		}
		port, err := proxy.DevicePort(d)
		if err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json", port), nil)
		if err != nil {
			return 0, err
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return 0, fmt.Errorf("reading the simulator's pages: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var pages []json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
			return 0, fmt.Errorf("reading the simulator's pages: %w", err)
		}
		return len(pages), nil
	}
	return 0, errors.New("ios_webkit_debug_proxy does not list the simulator")
}

// usbmuxSocket is usbmuxd's socket on Linux.
var usbmuxSocket = "/var/run/usbmuxd"

// withUSBMuxHint adds the cause that a failed iwdp start most likely has on
// Linux: ios_webkit_debug_proxy 1.9.2 exits at its start (status 255) when
// usbmuxd does not answer, and Linux runs usbmuxd only while an Apple
// device is plugged in.
func withUSBMuxHint(err error) error {
	if err == nil || runtime.GOOS != "linux" {
		return err
	}
	c, dialErr := net.DialTimeout("unix", usbmuxSocket, time.Second)
	if dialErr == nil {
		_ = c.Close()
		return err
	}
	return fmt.Errorf("%w (usbmuxd does not answer on %s, and ios_webkit_debug_proxy exits without it: keep usbmuxd running also with no iPhone plugged in, docs/simulator.md)", err, usbmuxSocket)
}

func simDid(done bool, yes, no string) string {
	if done {
		return yes
	}
	return no
}

func runSimUp(ctx context.Context, env Env, in Input) (Result, error) {
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	s.RestartAnyway, _ = in.Flags["restart-iwdp"].(bool)
	dev, _ := in.Flags["device"].(string)
	noWindow, _ := in.Flags["no-window"].(bool)
	openURL, _ := in.Flags["url"].(string)
	r, err := s.Up(ctx, dev, openURL, noWindow)
	if err != nil {
		return Result{}, err
	}
	iwdp := "lists the simulator"
	switch {
	case r.IWDPRestarted:
		iwdp = "restarted, lists the simulator"
	case r.IWDPStarted:
		iwdp = "started, lists the simulator"
	}
	lines := []string{
		fmt.Sprintf("Simulator ready: %s (%s, %s) on %s", r.Device.Name, r.Device.Runtime, r.Device.UDID, s.Cfg.SSH),
		"  mac:        " + simDid(r.MacStarted, "started", "was running"),
		"  simulator:  " + simDid(r.Booted, "booted", "was booted"),
		"  window:     " + simDid(noWindow, "not opened (--no-window)", simDid(r.WindowOpened, "opened", "was open")),
		"  safari:     " + simDid(r.SafariStarted, "started", "was running"),
		"  forward:    " + simDid(r.ForwardStarted, "started ", "running ") + sim.InspectorAddr + " -> " + r.InspectorSocket,
		"  iwdp:       " + iwdp,
		fmt.Sprintf("  tabs:       %d", r.Pages),
		simDid(r.Pages > 0, "Next: iwdp-cli tab list", "Safari offers no tab for inspection yet (its start page is not inspectable): run iwdp-cli sim up --url <url>"),
	}
	return Result{Text: strings.Join(lines, "\n"), Data: r}, nil
}

func runSimStatus(ctx context.Context, env Env, in Input) (Result, error) {
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	dev, _ := in.Flags["device"].(string)
	st, err := s.Status(ctx, dev)
	if err != nil {
		return Result{}, err
	}
	yn := func(b bool) string { return simDid(b, "yes", "no") }
	device := st.DeviceError
	if st.Device != nil {
		device = fmt.Sprintf("%s (%s, %s): %s", st.Device.Name, st.Device.Runtime, st.Device.UDID, st.Device.State)
	}
	if !st.MacReachable {
		device = "unknown"
	}
	lines := []string{
		"ready:      " + yn(st.Ready) + simDid(st.Ready, "", " (run iwdp-cli sim up)"),
		"mac:        " + s.Cfg.SSH + ", answers over ssh: " + yn(st.MacReachable),
		"login:      " + yn(st.LoggedIn),
		"simulator:  " + device,
		"window:     " + yn(st.Window),
		"forward:    " + yn(st.Forward),
		"iwdp lists: " + yn(st.Listed),
	}
	return Result{Text: strings.Join(lines, "\n"), Data: st}, nil
}

// simShotPath is the file of sim screenshot: path or -o, given once.
func simShotPath(in Input) (string, error) {
	out := arg(in, 0)
	if o := flagString(in, "output"); o != "" {
		if out != "" {
			return "", usagef("sim screenshot: give the file once, as path or with -o")
		}
		out = o
	}
	return out, nil
}

func runSimScreenshot(ctx context.Context, env Env, in Input) (Result, error) {
	out, err := simShotPath(in)
	if err != nil {
		return Result{}, err
	}
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	dev, _ := in.Flags["device"].(string)
	_, data, err := s.Screenshot(ctx, dev)
	if err != nil {
		return Result{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("reading the simulator screenshot: %w", err)
	}
	if out != "" {
		out = env.ResolvePath(out)
	}
	path, err := writeCapture(out, data)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Text: fmt.Sprintf("Screenshot saved to %s (%dx%d)", path, cfg.Width, cfg.Height),
		Data: map[string]any{"path": path, "width": cfg.Width, "height": cfg.Height},
	}, nil
}

// simNumbers parses the arguments of a touch command as points: finite
// numbers, 0 or more (AXe's range).
func simNumbers(cmd string, args []string) error {
	for _, a := range args {
		v, err := strconv.ParseFloat(a, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return usagef("%s: %q is not a number of points, 0 or more", cmd, a)
		}
	}
	return nil
}

// simSeconds checks a duration in seconds: above 0 and at most 10 (AXe's
// range for --hold and a swipe's --duration).
func simSeconds(cmd, v string) error {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f <= 0 || f > 10 {
		return usagef("%s: %q is not a number of seconds above 0 and at most 10", cmd, v)
	}
	return nil
}

// simTapValues checks the flags that sim tap was given (Check runs for
// --help too, with the arguments' names as arguments, so it checks no
// argument): not both --label and --id, and a number for --hold.
func simTapValues(in Input) error {
	if flagString(in, "label") != "" && flagString(in, "id") != "" {
		return usagef("sim tap: give one of --label and --id")
	}
	if h := flagString(in, "hold"); h != "" {
		return simSeconds("sim tap --hold", h)
	}
	return nil
}

// mustFloat is a number that simNumbers accepted.
func mustFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// simTapArgs are the axe arguments of sim tap: a point, or one element.
func simTapArgs(in Input) ([]string, error) {
	label, id := flagString(in, "label"), flagString(in, "id")
	args := []string{"tap"}
	switch {
	case len(in.Args) == 2 && label == "" && id == "":
		if err := simNumbers("sim tap", in.Args); err != nil {
			return nil, err
		}
		args = append(args, "-x", in.Args[0], "-y", in.Args[1])
	case len(in.Args) == 0 && (label == "") != (id == ""):
		// One word with "=", so that a value such as "-x" stays the
		// value and is not read as an option.
		if label != "" {
			args = append(args, "--label="+label)
		} else {
			args = append(args, "--id="+id)
		}
	default:
		return nil, usagef("sim tap: give <x> <y>, or one of --label and --id")
	}
	if h := flagString(in, "hold"); h != "" {
		if err := simSeconds("sim tap --hold", h); err != nil {
			return nil, err
		}
		args = append(args, "--hold", h)
	}
	return args, nil
}

// simSwipeArgs are the axe arguments of sim swipe.
func simSwipeArgs(in Input) ([]string, error) {
	if err := simNumbers("sim swipe", in.Args); err != nil {
		return nil, err
	}
	if p, q := in.Args[:2], in.Args[2:]; mustFloat(p[0]) == mustFloat(q[0]) && mustFloat(p[1]) == mustFloat(q[1]) {
		return nil, usagef("sim swipe: the start and the end are the same point")
	}
	args := []string{"swipe", "--start-x", in.Args[0], "--start-y", in.Args[1], "--end-x", in.Args[2], "--end-y", in.Args[3]}
	if d := flagString(in, "duration"); d != "" {
		if err := simSeconds("sim swipe --duration", d); err != nil {
			return nil, err
		}
		args = append(args, "--duration", d)
	}
	return args, nil
}

func runSimTouch(ctx context.Context, env Env, in Input, args []string, done string) (Result, error) {
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	if _, err := s.Touch(ctx, flagString(in, "device"), args[0], args[1:]...); err != nil {
		return Result{}, err
	}
	return Result{Text: done}, nil
}

func runSimKeyboard(ctx context.Context, env Env, in Input) (Result, error) {
	mode := arg(in, 0)
	if mode != "" && mode != "onscreen" && mode != "hardware" {
		return Result{}, usagef("sim keyboard: the mode is onscreen or hardware, not %q", mode)
	}
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	if mode == "" {
		hw, err := s.Mac.HardwareKeyboard(ctx)
		if err != nil {
			return Result{}, err
		}
		m := simDid(hw, "hardware", "onscreen")
		return Result{Text: "keyboard: " + m, Data: map[string]any{"mode": m, "changed": false}}, nil
	}
	s.RestartAnyway, _ = in.Flags["restart-iwdp"].(bool)
	changed, r, err := s.Keyboard(ctx, flagString(in, "device"), mode == "onscreen")
	if err != nil {
		return Result{}, err
	}
	text := "keyboard: " + mode + simDid(changed, " (the simulator restarted)", " (unchanged)")
	if changed {
		text += fmt.Sprintf("\ntabs: %d", r.Pages)
	}
	return Result{Text: text, Data: map[string]any{"mode": mode, "changed": changed}}, nil
}

func runSimUI(ctx context.Context, env Env, in Input) (Result, error) {
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	els, err := s.UI(ctx, flagString(in, "device"))
	if err != nil {
		return Result{}, err
	}
	return Result{Text: simUIText(els), Data: map[string]any{"elements": els}}, nil
}

// simUIText prints one element per line: its type, label, value or id,
// and the center of its frame, where sim tap would touch it.
func simUIText(els []sim.Element) string {
	lines := make([]string, 0, len(els))
	for _, e := range els {
		var name []string
		if e.Label != "" {
			name = append(name, strconv.Quote(e.Label))
		}
		if e.Value != "" {
			name = append(name, "value "+strconv.Quote(e.Value))
		}
		if e.ID != "" {
			name = append(name, "id "+strconv.Quote(e.ID))
		}
		lines = append(lines, fmt.Sprintf("%s %s at %.0f %.0f (%.0fx%.0f)", e.Type, strings.Join(name, " "), e.X+e.W/2, e.Y+e.H/2, e.W, e.H))
	}
	return strings.Join(lines, "\n")
}

func runSimDown(ctx context.Context, env Env, in Input) (Result, error) {
	s, err := newSim(env)
	if err != nil {
		return Result{}, err
	}
	dev, _ := in.Flags["device"].(string)
	mac, _ := in.Flags["mac"].(bool)
	r, err := s.Down(ctx, dev, mac)
	if err != nil {
		return Result{}, err
	}
	lines := []string{"Forward stopped."}
	if r.ShutDown != nil {
		lines = append(lines, fmt.Sprintf("Simulator %s (%s) shut down.", r.ShutDown.Name, r.ShutDown.UDID))
	} else {
		lines = append(lines, "No booted simulator to shut down.")
	}
	if r.MacStopped {
		lines = append(lines, "Mac stopped: "+strings.Join(s.Cfg.Stop, " "))
	}
	return Result{Text: strings.Join(lines, "\n"), Data: r}, nil
}
