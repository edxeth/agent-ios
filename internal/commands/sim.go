package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/proxy"
	"github.com/nnemirovsky/iwdp-cli/internal/sim"
)

// The simulator commands: sim up, sim status and sim down manage an iOS
// Simulator on a Mac that this host reaches over SSH (internal/sim,
// docs/simulator.md). They are Local: they run in the iwdp-cli process,
// which starts the ssh forward. Before a restart of iwdp they count the
// daemon's active leases on other devices through ActiveLeasesElsewhere,
// which the daemon package sets (it holds the daemon client).
func init() {
	device := Flag{Name: "device", Type: FlagString, Help: "the simulator: its UDID or exact name (default: \"device\" of the configuration, else the one booted simulator)"}
	Register(&Command{
		Name: "sim up", Local: true, Timeout: 15 * time.Minute,
		Summary: "Make the iOS Simulator of the Mac in the simulator configuration ready for iwdp-cli, doing only what is still needed: " +
			"start the Mac with the configuration's start command when ssh gets no answer, wait for the desktop login, boot the simulator, open its window, start Safari, " +
			"forward 127.0.0.1:27753 to its Web Inspector over ssh, and restart the ios_webkit_debug_proxy that iwdp-cli started when it does not list the simulator. " +
			"Refuses that restart while another device's page has an active lease, unless --restart-iwdp. Default deadline 15m.",
		Flags: []Flag{
			device,
			{Name: "no-window", Type: FlagBool, Help: "do not open the Simulator window"},
			{Name: "url", Type: FlagString, Help: "open this http or https URL in the simulator's Safari (a URL of this host: the Mac's address for it, for example http://10.0.2.2:8080/ in a QEMU VM)"},
			{Name: "restart-iwdp", Type: FlagBool, Help: "restart iwdp even when that ends active page leases of other devices"},
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
var newSim = func(env Env, restartAnyway bool) (*sim.Sim, error) {
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
	return &sim.Sim{
		Cfg: cfg, Mac: mac, Run: sim.ExecRunner, Poll: 3 * time.Second, Settle: 2 * time.Second, PageWait: 30 * time.Second,
		Forward: &sim.Forward{Mac: mac, Dir: dir, Addr: sim.InspectorAddr, Run: sim.ExecRunner, Spawn: sim.SpawnDetached, Wait: 15 * time.Second},
		IWDP:    hostIWDP{env: env, restartAnyway: restartAnyway},
	}, nil
}

// ActiveLeasesElsewhere counts the active page leases of the daemon on
// devices other than the simulator; no running daemon is 0. The daemon
// package sets it.
var ActiveLeasesElsewhere func(ctx context.Context, env Env) (int, error)

// SimStateDir is the private directory of the forward's record; the daemon
// package sets it to its socket directory.
var SimStateDir = func() (string, error) { return "", errors.New("sim: no state directory in this build") }

// hostIWDP is the iwdp on this host's listing address.
type hostIWDP struct {
	env           Env
	restartAnyway bool
}

func (h hostIWDP) SimulatorListed(ctx context.Context) (bool, error) {
	listed, err := simulatorListed(ctx)
	if errors.Is(err, proxy.ErrNotListening) {
		if err := proxy.Start(ctx); err != nil {
			return false, withUSBMuxHint(err)
		}
		listed, err = simulatorListed(ctx)
	}
	return listed, err
}

// Restart restarts iwdp (proxy.Restart: only the one that iwdp-cli
// started). A restart ends every page connection, so it refuses while a
// page of another device has an active lease, unless restartAnyway.
func (h hostIWDP) Restart(ctx context.Context) error {
	if !h.restartAnyway {
		if ActiveLeasesElsewhere == nil {
			return errors.New("restarting ios_webkit_debug_proxy: the daemon's leases cannot be read in this build; run sim up --restart-iwdp")
		}
		n, err := ActiveLeasesElsewhere(ctx, h.env)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("ios_webkit_debug_proxy must restart to reach the simulator, and that would end %d active page lease(s) of other devices: wait until those sessions end, or run sim up --restart-iwdp", n)
		}
	}
	return withUSBMuxHint(proxy.Restart(ctx))
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
	return 0, nil
}

// simulatorListed reads iwdp's listing for the SIMULATOR device; it starts
// nothing.
func simulatorListed(ctx context.Context) (bool, error) {
	ds, err := proxy.ListDevicesContext(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		if d.DeviceID == "SIMULATOR" {
			return true, nil
		}
	}
	return false, nil
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

func runSimUp(ctx context.Context, env Env, in Input) (Result, error) {
	restartAnyway, _ := in.Flags["restart-iwdp"].(bool)
	s, err := newSim(env, restartAnyway)
	if err != nil {
		return Result{}, err
	}
	dev, _ := in.Flags["device"].(string)
	noWindow, _ := in.Flags["no-window"].(bool)
	openURL, _ := in.Flags["url"].(string)
	r, err := s.Up(ctx, dev, openURL, noWindow)
	if err != nil {
		return Result{}, err
	}
	did := func(done bool, yes, no string) string {
		if done {
			return yes
		}
		return no
	}
	lines := []string{
		fmt.Sprintf("Simulator ready: %s (%s, %s) on %s", r.Device.Name, r.Device.Runtime, r.Device.UDID, s.Cfg.SSH),
		"  mac:        " + did(r.MacStarted, "started", "was running"),
		"  simulator:  " + did(r.Booted, "booted", "was booted"),
		"  window:     " + did(noWindow, "not opened (--no-window)", did(r.WindowOpened, "opened", "was open")),
		"  forward:    " + did(r.ForwardStarted, "started ", "running ") + sim.InspectorAddr + " -> " + r.InspectorSocket,
		"  iwdp:       " + did(r.IWDPRestarted, "restarted, lists the simulator", "lists the simulator"),
		fmt.Sprintf("  tabs:       %d", r.Pages),
		did(r.Pages > 0, "Next: iwdp-cli tab list", "Safari offers no tab for inspection yet (its start page is not inspectable): run iwdp-cli sim up --url <url>"),
	}
	return Result{Text: strings.Join(lines, "\n"), Data: r}, nil
}

func runSimStatus(ctx context.Context, env Env, in Input) (Result, error) {
	s, err := newSim(env, false)
	if err != nil {
		return Result{}, err
	}
	dev, _ := in.Flags["device"].(string)
	st, err := s.Status(ctx, dev, func(ctx context.Context) (bool, error) {
		listed, err := simulatorListed(ctx)
		if errors.Is(err, proxy.ErrNotListening) {
			return false, nil
		}
		return listed, err
	})
	if err != nil {
		return Result{}, err
	}
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	device := st.DeviceError
	if st.Device != nil {
		device = fmt.Sprintf("%s (%s, %s): %s", st.Device.Name, st.Device.Runtime, st.Device.UDID, st.Device.State)
	}
	if !st.MacReachable {
		device = "unknown"
	}
	lines := []string{
		"ready:      " + yn(st.Ready) + map[bool]string{true: "", false: " (run iwdp-cli sim up)"}[st.Ready],
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
	s, err := newSim(env, false)
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
