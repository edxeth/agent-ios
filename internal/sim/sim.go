package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IWDP is ios_webkit_debug_proxy on this host, as sim needs it.
type IWDP interface {
	// SimulatorListed reports whether iwdp lists the SIMULATOR device,
	// which it does only while its connection to InspectorAddr works. It
	// starts iwdp when none runs.
	SimulatorListed(ctx context.Context) (bool, error)
	// Restart restarts iwdp so that it connects to InspectorAddr again:
	// iwdp connects to a simulator only at its start.
	Restart(ctx context.Context) error
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
	Settle  time.Duration // after a new forward, before iwdp's listing counts
	// PageWait bounds the wait for Safari's first inspectable tab: Safari
	// restores its tabs some seconds after it starts.
	PageWait time.Duration
}

// UpReport is what Up did. Each bool is true when Up did that step itself.
type UpReport struct {
	Device          Device `json:"device"`
	MacStarted      bool   `json:"macStarted"`
	Booted          bool   `json:"booted"`
	WindowOpened    bool   `json:"windowOpened"`
	ForwardStarted  bool   `json:"forwardStarted"`
	IWDPRestarted   bool   `json:"iwdpRestarted"`
	InspectorSocket string `json:"inspectorSocket"`
	Pages           int    `json:"pages"` // Safari tabs that iwdp lists
}

// Up makes the simulator ready for iwdp-cli, doing only the steps still
// needed: start the Mac (the configuration's start command) when ssh gets
// no answer, wait for the desktop login, boot the simulator, open its
// window unless noWindow, start Safari, forward InspectorAddr to its Web
// Inspector socket, and restart iwdp when it does not list the simulator.
// Then it opens openURL when given, and waits up to PageWait for a tab.
// device overrides the configuration's device.
func (s *Sim) Up(ctx context.Context, device, openURL string, noWindow bool) (UpReport, error) {
	r, err := s.up(ctx, device, noWindow)
	if err != nil {
		return r, err
	}
	if openURL != "" {
		if err := s.Mac.OpenURL(ctx, r.Device.UDID, openURL); err != nil {
			return r, err
		}
	}
	// No tab after PageWait is no error: Safari can be on its start page,
	// which it does not offer for inspection; the report says so.
	wait, cancel := context.WithTimeout(ctx, s.PageWait)
	defer cancel()
	_ = s.until(wait, "Safari lists a tab", func() (bool, error) {
		n, err := s.IWDP.SimulatorPages(wait)
		r.Pages = n
		return err == nil && n > 0, nil
	})
	return r, ctx.Err()
}

func (s *Sim) up(ctx context.Context, device string, noWindow bool) (UpReport, error) {
	var r UpReport
	if !s.Mac.Reachable(ctx) {
		if len(s.Cfg.Start) == 0 {
			return r, fmt.Errorf("the Mac %s does not answer over ssh, and the simulator configuration has no start command (\"start\")", s.Cfg.SSH)
		}
		if _, err := s.Run(ctx, s.Cfg.Start[0], s.Cfg.Start[1:]...); err != nil {
			return r, fmt.Errorf("starting the Mac with %q: %w", strings.Join(s.Cfg.Start, " "), err)
		}
		r.MacStarted = true
		if err := s.until(ctx, "the Mac "+s.Cfg.SSH+" answers over ssh", func() (bool, error) { return s.Mac.Reachable(ctx), nil }); err != nil {
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
	if err := s.Mac.LaunchSafari(ctx, d.UDID); err != nil {
		return r, err
	}
	var sock string
	if err := s.until(ctx, "the simulator's Web Inspector socket appears", func() (bool, error) {
		var err error
		sock, err = s.Mac.InspectorSocket(ctx, d.UDID)
		return err == nil, nil
	}); err != nil {
		return r, err
	}
	r.InspectorSocket = sock
	if r.ForwardStarted, err = s.Forward.Ensure(ctx, sock); err != nil {
		return r, err
	}
	if r.ForwardStarted {
		// A replaced forward closed iwdp's connection to the simulator;
		// give iwdp the moment it takes to drop the SIMULATOR device, so
		// that only a connection to the new forward counts as listed.
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(s.Settle):
		}
	}
	listed, err := s.IWDP.SimulatorListed(ctx)
	if err != nil {
		return r, err
	}
	if listed {
		return r, nil
	}
	if err := s.IWDP.Restart(ctx); err != nil {
		return r, err
	}
	r.IWDPRestarted = true
	err = s.until(ctx, "ios_webkit_debug_proxy lists the simulator", func() (bool, error) { return s.IWDP.SimulatorListed(ctx) })
	return r, err
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
	Forward      bool    `json:"forward"` // the forward runs and reaches the booted simulator's socket
	Listed       bool    `json:"listed"`  // iwdp lists the simulator
	Ready        bool    `json:"ready"`
}

// Status checks each part without changing any; it does not start iwdp.
func (s *Sim) Status(ctx context.Context, device string, listed func(context.Context) (bool, error)) (Status, error) {
	var st Status
	if st.MacReachable = s.Mac.Reachable(ctx); !st.MacReachable {
		return st, nil
	}
	var err error
	if st.LoggedIn, err = s.Mac.LoggedIn(ctx); err != nil {
		return st, err
	}
	d, err := s.device(ctx, device)
	if errors.Is(err, ErrNoDevice) {
		st.DeviceError = err.Error()
	} else if err != nil {
		return st, err
	} else {
		st.Device = &d
	}
	if st.Window, err = s.Mac.WindowOpen(ctx); err != nil {
		return st, err
	}
	if st.Device != nil && st.Device.State == "Booted" {
		sock, err := s.Mac.InspectorSocket(ctx, d.UDID)
		if err == nil {
			fwd, err := s.Forward.Socket(ctx)
			if err != nil {
				return st, err
			}
			st.Forward = fwd == sock
		}
	}
	if st.Listed, err = listed(ctx); err != nil {
		return st, err
	}
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
	if s.Mac.Reachable(ctx) {
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
