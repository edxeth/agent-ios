package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/proxy"
	"github.com/edxeth/agent-ios/internal/sim"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The ios_webkit_debug_proxy commands: status, devices and restart-iwdp.
// They talk to iwdp's listing address (proxy.ListingAddr) and to the
// processes of this host, not to a page. status and restart-iwdp are
// Local. devices runs in the daemon, like page commands: it may start or
// restart iwdp, and the daemon is then, without AGENT_IOS_NO_DAEMON=1, the one
// process that does so automatically, so the start back-off of its
// proxy.Starter covers every automatic start (review of 2026-11-12; the
// cases where it does not are in docs/known-limits.md). Their output is
// that of the legacy commands they replace.
func init() {
	Register(&Command{
		Name:    "status",
		Summary: "Check whether ios_webkit_debug_proxy answers on its listing address, count the devices that it lists, and name the log of an iwdp that agent-ios starts. It starts nothing. Exits 1 when iwdp does not answer or its device list cannot be read.",
		Local:   true,
		Run:     runStatus,
	})
	Register(&Command{
		Name: "devices",
		Summary: "List the devices of ios_webkit_debug_proxy's listing (name, UDID, the address of its pages). " +
			"Starts ios_webkit_debug_proxy when it is not running, and on Linux restarts the one that agent-ios started when it lost its devices, as page commands do. " +
			"Exits 5 when it lists no device although usbmuxd reports one.",
		Run: runDevices,
	})
	Register(&Command{
		Name: "restart-iwdp",
		Summary: "Restart the ios_webkit_debug_proxy that agent-ios started. " +
			"Stops only an iwdp that agent-ios started and recorded, then starts a new one. " +
			"Any other iwdp (for example one that the user or another program started) is never stopped: the command fails and names the owner pid and the manual restart command. " +
			"When no iwdp runs, it starts one.",
		Local: true,
		Run: func(ctx context.Context, _ Env, _ Input) (Result, error) {
			if err := restartIWDP(ctx); err != nil {
				return Result{}, err
			}
			return Result{Text: "ios_webkit_debug_proxy restarted", Data: map[string]bool{"restarted": true}}, nil
		},
	})
}

// restartIWDP is the restart of restart-iwdp: proxy.Restart, on iwdp's
// listing address. Tests set it to the Restart of a proxy.Starter on a
// private address, because no test may reach iwdp's address.
var restartIWDP = proxy.Restart

// iwdpLogLine names the log of an iwdp that agent-ios starts (its stdout
// and stderr: path and err from proxy.LogPath), or why its path is
// unknown; status prints it for every outcome.
func iwdpLogLine(path string, err error) string {
	const label = "iwdp log of an iwdp that agent-ios starts: "
	if err != nil {
		return label + "unknown (" + err.Error() + ")"
	}
	return label + path
}

// statusData is the --json form of status.
type statusData struct {
	Running bool `json:"running"`
	Devices *int `json:"devices,omitempty"` // listed devices, without iwdp's SIMULATOR entry; nil in a status that fails
	// SimulatorConfigured: no device is listed and a simulator is configured.
	SimulatorConfigured bool   `json:"simulatorConfigured,omitempty"`
	Log                 string `json:"log,omitempty"`      // the iwdp log's path
	LogError            string `json:"logError,omitempty"` // why that path is unknown
}

// statusDevices is the number of devices that the iwdp at addr lists,
// without the SIMULATOR entry that iwdp lists before real devices. It is
// one more GET of the listing that status just checked: it starts and
// recovers nothing, so a caller that may not restart iwdp learns from it
// whether a device is there.
func statusDevices(ctx context.Context, addr string) (int, error) {
	entries, err := proxy.ListDevicesAt(ctx, addr)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.DeviceID != "SIMULATOR" {
			n++
		}
	}
	return n, nil
}

// statusListing is the listing address that status checks:
// proxy.ListingAddr. Tests set it to a private address, because no test
// may reach iwdp's address.
var statusListing = proxy.ListingAddr

// runStatus answers as the legacy status did: when iwdp answers, the
// running line, the number of listed devices and the log line, exit 0. A
// status that succeeds always has that number: when iwdp answers the check
// and then not the read of its devices, the status fails (exit 1), so that
// no caller takes a status without a count for a device that is there.
// Otherwise it fails with a result
// (withResult), so that stdout holds what it found and the log line (a
// broken listing is when the user needs the log most): not running with
// the start advice (exit 1), or how the owner of the listing address
// failed (exit 1). The output flags (--json, --content-boundaries,
// --max-output) apply to that result as to any other.
func runStatus(ctx context.Context, _ Env, _ Input) (Result, error) {
	err := proxy.CheckListing(ctx, statusListing())
	path, perr := proxy.LogPath()
	line := iwdpLogLine(path, perr)
	data := statusData{Running: err == nil, Log: path}
	if perr != nil {
		data.LogError = perr.Error()
	}
	if err == nil {
		n, err := statusDevices(ctx, statusListing())
		if err != nil {
			return Result{}, &withResult{
				Result: Result{Text: "ios_webkit_debug_proxy answers, but its device list could not be read\n" + line, Data: data},
				Err:    fmt.Errorf("reading the device list of ios_webkit_debug_proxy: %w", err),
			}
		}
		data.Devices = &n
		text := fmt.Sprintf("ios_webkit_debug_proxy is running\ndevices: %d\n", n)
		if n == 0 && simConfigured() {
			// devices counts no simulator: say where the simulator is, or
			// "devices 0" reads as "no iOS target" (live run, 2026-11-24:
			// an agent stopped there).
			data.SimulatorConfigured = true
			text += simulatorHint + "\n"
		}
		return Result{Text: text + line, Data: data}, nil
	}
	if errors.Is(err, proxy.ErrNotListening) {
		return Result{}, &withResult{Result: Result{Text: "ios_webkit_debug_proxy is NOT running\n" + line, Data: data}, Err: errNotRunning()}
	}
	// A process owns the listing address but does not answer as iwdp.
	return Result{}, &withResult{Result: Result{Text: line, Data: data}, Err: err}
}

// simulatorHint is status's line for a configured simulator.
const simulatorHint = "simulator: an iOS Simulator is configured (devices counts none): run 'agent-ios sim reserve', then 'agent-ios sim up --url <your page>'"

// simConfigured reports whether a simulator configuration exists (sim
// commands); tests replace it.
var simConfigured = func() bool {
	path, err := sim.ConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// errNotRunning is the error of an iwdp listing address that nothing
// owns: exit 1 with the manual start command of this system.
func errNotRunning() error {
	return fmt.Errorf("ios_webkit_debug_proxy is not running.\nStart it with: %s", proxy.ManualCommand())
}

// The listing address of devices and its iwdp starts (decision D10, as the
// daemon's for page commands): proxy.ListingAddr, proxy.Start,
// proxy.Recover, proxy.AwaitDevice and proxy.AttachedDevices. Tests
// replace them, because no test may read, start or stop an iwdp on iwdp's
// address.
var (
	devicesListing  = proxy.ListingAddr
	devicesStart    = proxy.Start
	devicesRecover  = proxy.Recover
	devicesAwait    = proxy.AwaitDevice
	devicesAttached = proxy.AttachedDevices
)

// runDevices lists the devices as the legacy devices did, after the start
// that page commands get from the daemon: when nothing owns the listing
// address it starts iwdp and waits a moment for a device (devicesAwait),
// and an empty listing of an iwdp that lost its devices gets that iwdp
// restarted and read again (devicesRecover). A failed start exits 1 with the start's error. An
// owner of the listing address that does not answer, or answers with
// something other than a device list, gets an error that says how it
// failed, with the exit code of a listing failure (5): an iwdp can close
// listing requests while a page is connected (docs/known-limits.md). A
// read that fails after the check passed is classified the same way
// (proxy.ReadFailure); when nothing owns the address by then, the read's
// own error exits 1. An empty listing while usbmuxd reports a USB device
// exits 5 with what to do on the device; without one, or without
// usbmuxd's list, it prints the legacy line.
func runDevices(ctx context.Context, _ Env, _ Input) (Result, error) {
	addr := devicesListing()
	err := proxy.CheckListing(ctx, addr)
	if errors.Is(err, proxy.ErrNotListening) {
		if serr := devicesStart(ctx); serr != nil {
			return Result{}, proxy.AutoStartError(serr)
		}
		if _, aerr := devicesAwait(ctx); aerr != nil {
			return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("agent-ios started ios_webkit_debug_proxy, and its listing failed: %w", aerr)}
		}
		err = proxy.CheckListing(ctx, addr)
	}
	if err != nil {
		if errors.Is(err, proxy.ErrNotListening) {
			return Result{}, errNotRunning()
		}
		return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: err}
	}
	devices, err := readDevices(ctx, addr)
	if err != nil {
		return Result{}, err
	}
	if len(devices) == 0 {
		// Only an empty listing can be an iwdp that lost its devices; the
		// recovery reads the listing again under the start lock.
		restarted, rerr := devicesRecover(ctx)
		if rerr != nil {
			return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: rerr}
		}
		if restarted {
			if devices, err = readDevices(ctx, addr); err != nil {
				return Result{}, err
			}
		}
	}
	if len(devices) == 0 {
		if udids, aerr := devicesAttached(ctx); aerr == nil && len(udids) > 0 {
			return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("ios_webkit_debug_proxy lists no device, although usbmuxd reports %d USB device(s). %s, then run devices again", len(udids), proxy.DeviceAdvice)}
		}
		return Result{Text: "No devices connected.", Data: devices}, nil
	}
	lines := make([]string, len(devices))
	for i, d := range devices {
		lines[i] = fmt.Sprintf("%-40s  %s  (pages on %s)", d.DeviceName, d.DeviceID, d.URL)
	}
	return Result{Text: strings.Join(lines, "\n"), Data: devices}, nil
}

// readDevices reads the device list of the listing address addr. A read
// that fails after the listing's check is a listing failure (exit 5,
// proxy.ReadFailure), unless nothing owns the address by then.
func readDevices(ctx context.Context, addr string) ([]webkit.DeviceEntry, error) {
	devices, err := proxy.ListDevicesAt(ctx, addr)
	if err != nil {
		if cerr := proxy.ReadFailure(ctx, addr, err); !errors.Is(cerr, proxy.ErrNotListening) {
			err = &core.CodedError{Code: core.CodeNoPage, Err: cerr}
		}
		return nil, err
	}
	if devices == nil {
		devices = []webkit.DeviceEntry{}
	}
	return devices, nil
}
