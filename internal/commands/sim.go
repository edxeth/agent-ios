package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"
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
