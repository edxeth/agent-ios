package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IWDPState is the state of ios_webkit_debug_proxy on this host.
type IWDPState struct {
	Running bool // something listens on iwdp's listing address
	Own     bool // it is the iwdp that iwdp-cli started and recorded, which looks for a simulator on InspectorAddr
	Listed  bool // it lists the SIMULATOR device, which it does only while its connection to InspectorAddr works
}

// IWDP is ios_webkit_debug_proxy on this host, as sim needs it.
type IWDP interface {
	// State reads iwdp's state; it starts nothing.
	State(ctx context.Context) (IWDPState, error)
	// Start starts iwdp when none runs; a new iwdp connects to
	// InspectorAddr at its start.
	Start(ctx context.Context) error
	// Restart restarts the iwdp that iwdp-cli started, so that it connects
	// to InspectorAddr again: iwdp connects to a simulator only at its start.
	Restart(ctx context.Context) error
	// ActiveLeases counts the daemon's active page leases on every device,
	// the simulator's too: a restart of iwdp, and a new forward, end their
	// page connections.
	ActiveLeases(ctx context.Context) (int, error)
	// SimulatorPages counts the pages that iwdp lists for the simulator.
	SimulatorPages(ctx context.Context) (int, error)
}

// Sim is the simulator of a Config.
type Sim struct {
	Cfg     Config
	Mac     Mac
	Run     Runner // the start and stop commands of the configuration
	Forward *Forward
	IWDP    IWDP
	Poll    time.Duration // between checks of the Mac, the login and iwdp
	// PageWait bounds the wait for Safari's first inspectable tab: Safari
	// restores its tabs some seconds after it starts.
	PageWait time.Duration
	// RestartAnyway lets a restart of iwdp end active page leases.
	RestartAnyway bool
}

// UpReport is what Up did. Each bool is true when Up did that step itself.
type UpReport struct {
	Device          Device `json:"device"`
	MacStarted      bool   `json:"macStarted"`
	Booted          bool   `json:"booted"`
	WindowOpened    bool   `json:"windowOpened"`
	SafariStarted   bool   `json:"safariStarted"`
	ForwardStarted  bool   `json:"forwardStarted"`
	IWDPStarted     bool   `json:"iwdpStarted"`
	IWDPRestarted   bool   `json:"iwdpRestarted"`
	InspectorSocket string `json:"inspectorSocket"`
	Pages           int    `json:"pages"` // Safari tabs that iwdp lists
}

// ErrNotOwnIWDP is the error of Up when the iwdp on the listing address is
// not the one that iwdp-cli started: sim cannot tell where that iwdp looks
// for a simulator, nor restart it.
var ErrNotOwnIWDP = errors.New("the ios_webkit_debug_proxy on its listing address was not started by iwdp-cli, so sim up cannot tell where it looks for a simulator or restart it: ask whoever started it to stop it, then run sim up again")

// Up makes the simulator ready for iwdp-cli, doing only the steps still
// needed: start the Mac (the configuration's start command) when ssh gets
// no answer, wait for the desktop login, boot the simulator, open its
// window unless noWindow, start Safari, forward InspectorAddr to its Web
// Inspector socket, and start iwdp, or restart the one that iwdp-cli
// started when it does not use that forward. Then it opens openURL when
// given (CheckURL), and waits up to PageWait for a tab. device overrides the
// configuration's device.
func (s *Sim) Up(ctx context.Context, device, openURL string, noWindow bool) (UpReport, error) {
	if openURL != "" {
		if err := CheckURL(openURL); err != nil {
			return UpReport{}, err
		}
	}
	r, err := s.up(ctx, device, noWindow)
	if err != nil {
		return r, err
	}
	if openURL != "" {
		if err := s.Mac.OpenURL(ctx, r.Device.UDID, openURL); err != nil {
			return r, err
		}
	}
	// No tab after PageWait is no error when iwdp answers: Safari's start
	// page is not offered for inspection, and the report says so. A page
	// list that keeps failing is an error.
	wait, cancel := context.WithTimeout(ctx, s.PageWait)
	defer cancel()
	var lastErr error
	_ = s.until(wait, "Safari lists a tab", func() (bool, error) {
		r.Pages, lastErr = s.IWDP.SimulatorPages(wait)
		return lastErr == nil && r.Pages > 0, nil
	})
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if lastErr != nil {
		return r, fmt.Errorf("reading the simulator's pages from ios_webkit_debug_proxy: %w", lastErr)
	}
	return r, nil
}

func (s *Sim) up(ctx context.Context, device string, noWindow bool) (UpReport, error) {
	var r UpReport
	reachable, err := s.Mac.Reachable(ctx)
	if err != nil {
		return r, err
	}
	if !reachable {
		if len(s.Cfg.Start) == 0 {
			return r, fmt.Errorf("the Mac %s does not answer over ssh, and the simulator configuration has no start command (\"start\")", s.Cfg.SSH)
		}
		if _, err := s.Run(ctx, s.Cfg.Start[0], s.Cfg.Start[1:]...); err != nil {
			return r, fmt.Errorf("starting the Mac with %q: %w", strings.Join(s.Cfg.Start, " "), err)
		}
		r.MacStarted = true
		if err := s.until(ctx, "the Mac "+s.Cfg.SSH+" answers over ssh", func() (bool, error) { return s.Mac.Reachable(ctx) }); err != nil {
			return r, err
		}
	}
	if err := s.until(ctx, "a user is logged in to the desktop of "+s.Cfg.SSH+" (log in there, or turn on automatic login)", func() (bool, error) { return s.Mac.LoggedIn(ctx) }); err != nil {
		return r, err
	}
	d, err := s.device(ctx, device)
	if err != nil {
		return r, err
	}
	r.Device = d
	if d.State != "Booted" {
		if err := s.Mac.Boot(ctx, d.UDID); err != nil {
			return r, err
		}
		r.Booted = true
		r.Device.State = "Booted"
	}
	if !noWindow {
		open, err := s.Mac.WindowOpen(ctx)
		if err != nil {
			return r, err
		}
		if !open {
			if err := s.Mac.OpenWindow(ctx, d.UDID); err != nil {
				return r, err
			}
			r.WindowOpened = true
		}
	}
	running, err := s.Mac.SafariRunning(ctx, d.UDID)
	if err != nil {
		return r, err
	}
	if !running {
		if err := s.Mac.LaunchSafari(ctx, d.UDID); err != nil {
			return r, err
		}
		r.SafariStarted = true
	}
	var sock string
	if err := s.until(ctx, "the simulator's Web Inspector socket appears", func() (bool, error) {
		var err error
		sock, err = s.Mac.InspectorSocket(ctx, d.UDID)
		return err == nil, ctx.Err()
	}); err != nil {
		return r, err
	}
	r.InspectorSocket = sock
	return r, s.connect(ctx, sock, &r)
}

// connect makes iwdp list the simulator through a forward to sock. Every
// check that can refuse runs before the first change: a refusal leaves the
// forward and iwdp as they were.
func (s *Sim) connect(ctx context.Context, sock string, r *UpReport) error {
	fs, err := s.Forward.State(ctx, sock)
	if err != nil {
		return err
	}
	st, err := s.IWDP.State(ctx)
	if err != nil {
		return err
	}
	if st.Running && !st.Own {
		return ErrNotOwnIWDP
	}
	// The listing is this forward's only when iwdp started after it
	// listened (Synced): an earlier Up may have replaced the forward and
	// then been refused the restart.
	if fs.Serving && fs.Synced && st.Listed {
		return nil
	}
	// iwdp connects to a simulator only at its start: a running iwdp that
	// does not list the simulator, or that is connected to another
	// forward, must restart.
	restart := st.Running
	if restart { // without a running iwdp no page connection exists
		if err := s.leaseCheck(ctx); err != nil {
			return err
		}
	}
	if !fs.Serving {
		if r.ForwardStarted, err = s.Forward.Ensure(ctx, sock); err != nil {
			return err
		}
	}
	if restart {
		// Again right before the restart: a session may have taken a
		// lease since the first check. A lease taken between this check
		// and the restart still ends (docs/known-limits.md).
		if err := s.leaseCheck(ctx); err != nil {
			return err
		}
		if err := s.IWDP.Restart(ctx); err != nil {
			return err
		}
		r.IWDPRestarted = true
	} else {
		if err := s.IWDP.Start(ctx); err != nil {
			return err
		}
		r.IWDPStarted = true
	}
	// Another program may have started an iwdp of its own first.
	if st, err := s.IWDP.State(ctx); err != nil {
		return err
	} else if !st.Own {
		return ErrNotOwnIWDP
	}
	if err := s.Forward.MarkSynced(ctx); err != nil {
		return err
	}
	return s.until(ctx, "ios_webkit_debug_proxy lists the simulator", func() (bool, error) {
		st, err := s.IWDP.State(ctx)
		return st.Listed && st.Own, err
	})
}

// leaseCheck refuses a restart of iwdp while it would end active leases,
// unless RestartAnyway.
func (s *Sim) leaseCheck(ctx context.Context) error {
	if s.RestartAnyway {
		return nil
	}
	n, err := s.IWDP.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("ios_webkit_debug_proxy must restart to reach the simulator, and that would end the page connections of %d active page lease(s): wait until those sessions end, or run sim up --restart-iwdp", n)
	}
	return nil
}

// device picks the simulator: want, else the configuration's device.
func (s *Sim) device(ctx context.Context, want string) (Device, error) {
	if want == "" {
		want = s.Cfg.Device
	}
	ds, err := s.Mac.Devices(ctx)
	if err != nil {
		return Device{}, err
	}
	return Pick(ds, want)
}

// until checks ok every Poll until it holds; the end of ctx is an error that
// says what did not happen.
func (s *Sim) until(ctx context.Context, what string, ok func() (bool, error)) error {
	for {
		done, err := ok()
		if ctx.Err() != nil {
			return fmt.Errorf("waiting until %s: %w", what, ctx.Err())
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting until %s: %w", what, ctx.Err())
		case <-time.After(s.Poll):
		}
	}
}

// Status is the state of each part that Up sets up.
type Status struct {
	MacReachable bool    `json:"macReachable"`
	LoggedIn     bool    `json:"loggedIn"`
	Device       *Device `json:"device,omitempty"`
	DeviceError  string  `json:"deviceError,omitempty"`
	Window       bool    `json:"window"`
	Forward      bool    `json:"forward"` // the forward serves the booted simulator's socket, and iwdp started after it
	Listed       bool    `json:"listed"`  // the iwdp that iwdp-cli started lists the simulator
	Ready        bool    `json:"ready"`
}

// Status checks each part without changing any; it starts nothing.
func (s *Sim) Status(ctx context.Context, device string) (Status, error) {
	var st Status
	var err error
	if st.MacReachable, err = s.Mac.Reachable(ctx); err != nil || !st.MacReachable {
		return st, err
	}
	if st.LoggedIn, err = s.Mac.LoggedIn(ctx); err != nil {
		return st, err
	}
	d, err := s.device(ctx, device)
	switch {
	case errors.Is(err, ErrNoDevice):
		st.DeviceError = err.Error()
	case err != nil:
		return st, err
	default:
		st.Device = &d
	}
	if st.Window, err = s.Mac.WindowOpen(ctx); err != nil {
		return st, err
	}
	if st.Device != nil && st.Device.State == "Booted" {
		sock, err := s.Mac.InspectorSocket(ctx, d.UDID)
		if err == nil {
			fs, err := s.Forward.State(ctx, sock)
			if err != nil {
				return st, err
			}
			st.Forward = fs.Serving && fs.Synced
		} else if ctx.Err() != nil {
			return st, err
		}
	}
	is, err := s.IWDP.State(ctx)
	if err != nil {
		return st, err
	}
	st.Listed = is.Own && is.Listed
	st.Ready = st.LoggedIn && st.Device != nil && st.Device.State == "Booted" && st.Forward && st.Listed
	return st, nil
}

// DownReport is what Down did.
type DownReport struct {
	ShutDown   *Device `json:"shutDown,omitempty"`
	MacStopped bool    `json:"macStopped"`
}

// Down stops the forward, shuts the simulator down when the Mac answers
// and it is booted, and with mac runs the configuration's stop command.
func (s *Sim) Down(ctx context.Context, device string, mac bool) (DownReport, error) {
	var r DownReport
	if mac && len(s.Cfg.Stop) == 0 {
		return r, fmt.Errorf("the simulator configuration has no stop command (\"stop\") for --mac")
	}
	if err := s.Forward.Stop(ctx); err != nil {
		return r, err
	}
	reachable, err := s.Mac.Reachable(ctx)
	if err != nil {
		return r, err
	}
	if reachable {
		d, err := s.device(ctx, device)
		if err != nil && !errors.Is(err, ErrNoDevice) {
			return r, err
		}
		if err == nil && d.State == "Booted" {
			if err := s.Mac.Shutdown(ctx, d.UDID); err != nil {
				return r, err
			}
			r.ShutDown = &d
		}
	}
	if mac {
		if _, err := s.Run(ctx, s.Cfg.Stop[0], s.Cfg.Stop[1:]...); err != nil {
			return r, fmt.Errorf("stopping the Mac with %q: %w", strings.Join(s.Cfg.Stop, " "), err)
		}
		r.MacStopped = true
	}
	return r, nil
}
