package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IWDPState is the state of ios_webkit_debug_proxy on this host.
type IWDPState struct {
	Running bool // something listens on iwdp's listing address
	Own     bool // it is the iwdp that agent-ios started and recorded, which looks for a simulator on InspectorAddr
	Listed  bool // it lists the SIMULATOR device, which it does only while its connection to InspectorAddr works
}

// IWDP is ios_webkit_debug_proxy on this host, as sim needs it.
type IWDP interface {
	// State reads iwdp's state; it starts nothing.
	State(ctx context.Context) (IWDPState, error)
	// Start starts iwdp when none runs; a new iwdp connects to
	// InspectorAddr at its start. An error that wraps ErrIWDPKept says
	// that the started iwdp listens but has not answered yet, and runs on.
	Start(ctx context.Context) error
	// Restart restarts the iwdp that agent-ios started, so that it connects
	// to InspectorAddr again: iwdp connects to a simulator only at its start.
	// Its errors are those of Start.
	Restart(ctx context.Context) error
	// ActiveLeases counts the daemon's active page leases on every device,
	// the simulator's too: a restart of iwdp, and a new forward, end their
	// page connections.
	ActiveLeases(ctx context.Context) (int, error)
	// SimulatorPages returns the pages that iwdp lists for the simulator.
	SimulatorPages(ctx context.Context) ([]Page, error)
}

// Page is a page that iwdp lists for the simulator: ID identifies its tab
// for the tab's life (iwdp's webSocketDebuggerUrl), URL is its address.
type Page struct {
	ID, URL, Title string
}

// Tabs are the simulator's tabs as the daemon lists them, for the session
// of sim up: Up closes through them a tab that its own open made when it
// fails.
type Tabs interface {
	// ID returns the tab id (t<N>, as tab list shows it) of the page whose
	// id in iwdp's listing is pageID; "" when the daemon does not list it.
	ID(ctx context.Context, pageID string) (string, error)
	// Close closes the tab with that tab id as tab close does
	// (window.close(), then a check that the tab is no longer listed).
	Close(ctx context.Context, id string) error
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
	// Tabs closes a tab that Up's open made when Up fails after the open;
	// nil closes none, and the error names the tab.
	Tabs Tabs
	// CleanupWait bounds that close and its check, from the failure on:
	// also after the caller's deadline has passed.
	CleanupWait time.Duration
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
	Pages           int    `json:"pages"`     // Safari tabs that iwdp lists
	TabOpened       bool   `json:"tabOpened"` // a tab shows the URL that Up opened
	// NewPage is the page id (Page.ID) of the one new tab that shows the
	// URL that Up opened; "" when there is none, or more than one. It
	// stays out of the output: the command prints the tab's id (Tab).
	NewPage string `json:"-"`
	// SharedURL is true when the new tab is not the only tab that shows
	// the URL that Up opened (a tab listed before the open, or a second
	// new one): tab find finds several tabs of that URL, so it cannot name
	// the new tab when the command has no tab id for it (final review F6).
	SharedURL bool `json:"sharedURL,omitempty"`
	// Tab is the tab id (t<N>, as tab list shows it) of NewPage, which the
	// command sets when the daemon lists it; "" otherwise.
	Tab string `json:"tab,omitempty"`
}

// ErrNotOwnIWDP is the error of Up when the iwdp on the listing address is
// not the one that agent-ios started: sim cannot tell where that iwdp looks
// for a simulator, nor restart it.
// ErrNoURLTab is Up's error when Safari shows no tab of its URL within
// PageWait, mostly a server that does not answer the simulator.
var ErrNoURLTab = errors.New("safari in the simulator shows no tab of the --url")

// ErrIWDPKept marks a start or restart of iwdp whose new iwdp listens but
// has not answered yet (it connects its USB devices first), and that
// agent-ios kept running and recorded instead of stopping it.
var ErrIWDPKept = errors.New("the started ios_webkit_debug_proxy was kept running while it connects its devices")

var ErrNotOwnIWDP = errors.New("the ios_webkit_debug_proxy on its listing address was not started by agent-ios, so sim up cannot tell where it looks for a simulator or restart it: ask whoever started it to stop it, then run sim up again")

// Up makes the simulator ready for agent-ios, doing only the steps still
// needed: start the Mac (the configuration's start command) when ssh gets
// no answer, wait for the desktop login, boot the simulator, open its
// window unless noWindow, start Safari, forward InspectorAddr to its Web
// Inspector socket, and start iwdp, or restart the one that agent-ios
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
	// Without openURL, no tab after PageWait is no error when iwdp
	// answers: Safari's start page is not offered for inspection, and the
	// report says so. With openURL, Up returns once a tab that was not
	// listed before the open shows it, so that tab find finds it at once
	// (live run, 2026-11-24: an agent's tab find right after sim up ran
	// before Safari listed the page); a tab of an earlier turn with the
	// same URL proves nothing. The report names the new tab (NewPage), so
	// that the command can print its tab id: tab find cannot tell it from
	// an earlier tab of the same URL (review F18). Exit 7 (ErrNoURLTab) needs a listing that
	// was read: a page list that keeps failing is that error instead.
	wait, cancel := context.WithTimeout(ctx, s.PageWait)
	defer cancel()
	// The tabs listed before the open, by id, with the URL each showed: a
	// tab that showed another URL before is the open's too, since Safari
	// opens a URL in an empty tab it has (live run, 2026-11-24: right after
	// a Safari reset, its one about:blank tab took the URL, and sim up
	// counted it as an earlier turn's).
	var before map[string]string
	if openURL != "" {
		var lastErr error
		_ = s.until(wait, "Safari's tabs are listed", func() (bool, error) {
			var pages []Page
			pages, lastErr = s.IWDP.SimulatorPages(wait)
			if lastErr != nil {
				return false, nil
			}
			before = map[string]string{}
			for _, p := range pages {
				before[p.ID] = p.URL
			}
			return true, nil
		})
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if before == nil {
			return r, fmt.Errorf("reading the simulator's pages from ios_webkit_debug_proxy before opening the URL: %w", lastErr)
		}
		if err := s.Mac.OpenURL(ctx, r.Device.UDID, openURL); err != nil {
			// The open may have run on the Mac before it failed here.
			return r, s.closeOwnTabs(ctx, before, err)
		}
	}
	// After the open, a failure closes the tab that the open made.
	fail := func(err error) (UpReport, error) {
		if openURL == "" {
			return r, err
		}
		return r, s.closeOwnTabs(ctx, before, err)
	}
	var lastErr error
	earlier := false // a tab listed before the open shows openURL
	_ = s.until(wait, "Safari lists the tab", func() (bool, error) {
		var pages []Page
		pages, lastErr = s.IWDP.SimulatorPages(wait)
		if lastErr != nil {
			return false, nil
		}
		r.Pages = len(pages)
		if openURL == "" {
			return r.Pages > 0, nil
		}
		earlier = false
		var opened []string // the new tabs of openURL
		for _, p := range pages {
			switch {
			case !sameURL(p.URL, openURL):
			case p.ID == "" || shownBefore(before, p.ID, openURL):
				// A tab without an id (not offered for debugging) is
				// not known to be new.
				earlier = true
			default:
				opened = append(opened, p.ID)
			}
		}
		r.TabOpened = len(opened) > 0
		r.SharedURL = r.TabOpened && (earlier || len(opened) > 1)
		r.NewPage = ""
		if len(opened) == 1 {
			r.NewPage = opened[0]
		}
		return r.TabOpened, nil
	})
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if lastErr != nil {
		return fail(fmt.Errorf("reading the simulator's pages from ios_webkit_debug_proxy: %w", lastErr))
	}
	if openURL != "" && !r.TabOpened {
		if earlier {
			return fail(fmt.Errorf("%w %s after %s, except a tab that was open before this sim up, perhaps an earlier run's, which is not yours to touch: Safari opened no new tab of it. Serve your page on a URL that no other tab shows (a port or path of your own), and run sim up with that URL", ErrNoURLTab, openURL, s.PageWait))
		}
		return fail(fmt.Errorf("%w %s after %s: check that your server answers on the same port of this host (curl http://127.0.0.1:<port>/) and runs outside your tool call; the simulator reaches this host at 10.0.2.2", ErrNoURLTab, openURL, s.PageWait))
	}
	return r, nil
}

// closeOwnTabs runs when Up fails with err after its open call: it closes
// each tab that the open made, which is one that iwdp did not list before
// the open (before) and lists now, through Tabs as tab close does, and
// checks in iwdp's listing that it is gone. It never closes a tab listed
// before, also not an empty tab that Safari reused for the URL (the tab
// that Up takes as the open's on success), and names that one instead. It
// returns err with what it did: the tabs closed, each tab it could not
// close, with the command that closes it, or that no tab was left. Its
// steps get CleanupWait, detached from ctx when less of ctx's deadline is
// left (also when it has passed); a ctx canceled for another reason (the
// turn lost, a signal) closes nothing.
func (s *Sim) closeOwnTabs(ctx context.Context, before map[string]string, err error) error {
	if c := ctx.Err(); c != nil && !errors.Is(c, context.DeadlineExceeded) {
		return fmt.Errorf("%w. sim up was stopped before it could close a tab that its open may have made: look in agent-ios tab list --device SIMULATOR for a tab that was not there before, and close it (agent-ios tab close <id>)", err)
	}
	base := ctx
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < s.CleanupWait {
		base = context.WithoutCancel(ctx)
	}
	cctx, cancel := context.WithTimeout(base, s.CleanupWait)
	defer cancel()
	pages, lerr := s.pagesWithin(cctx)
	if lerr != nil {
		return fmt.Errorf("%w. sim up could not read Safari's tabs to close a tab that its open may have made (%v): look in agent-ios tab list --device SIMULATOR for a tab that was not there before, and close it (agent-ios tab close <id>)", err, lerr)
	}
	var closed, left, reused []string
	for _, p := range pages {
		u, listed := before[p.ID]
		switch {
		case p.ID == "": // not offered for inspection: not known to be new
		case listed && emptyURL(u) && !emptyURL(p.URL):
			reused = append(reused, tabText(p, s.tabID(cctx, p.ID)))
		case listed:
		default:
			id := s.tabID(cctx, p.ID)
			if cerr := s.closeTab(cctx, id, p.ID); cerr != nil {
				what := tabText(p, id)
				if id == "" {
					left = append(left, what+": find its id in agent-ios tab list --device SIMULATOR, then run agent-ios tab close <id>")
				} else {
					left = append(left, what+": run agent-ios tab close "+id)
				}
				continue
			}
			closed = append(closed, tabText(p, id))
		}
	}
	var says []string
	if len(closed) > 0 {
		says = append(says, "sim up closed the tab that its open made: "+strings.Join(closed, "; "))
	}
	if len(left) > 0 {
		says = append(says, "sim up could not close the tab that its open made, close it yourself: "+strings.Join(left, "; "))
	}
	if len(closed) == 0 && len(left) == 0 {
		says = append(says, "sim up left no tab open: Safari listed no new tab after the open")
	}
	if len(reused) > 0 {
		says = append(says, "Safari showed the URL in an empty tab that was open before this sim up, which now shows the failed address: "+strings.Join(reused, "; ")+"; sim up did not open that tab and leaves it as it is")
	}
	return fmt.Errorf("%w. %s", err, strings.Join(says, ". "))
}

// pagesWithin reads the simulator's pages, again every Poll while the read
// fails, until ctx ends.
func (s *Sim) pagesWithin(ctx context.Context) ([]Page, error) {
	var pages []Page
	var err error
	_ = s.until(ctx, "Safari's tabs are listed", func() (bool, error) {
		pages, err = s.IWDP.SimulatorPages(ctx)
		return err == nil, nil
	})
	return pages, err
}

// tabID is the tab id of the page pageID through Tabs; "" when there are
// no Tabs or they do not give one.
func (s *Sim) tabID(ctx context.Context, pageID string) string {
	if s.Tabs == nil {
		return ""
	}
	id, err := s.Tabs.ID(ctx, pageID)
	if err != nil {
		return ""
	}
	return id
}

// closeTab closes the tab id (the page pageID) through Tabs and waits until
// iwdp no longer lists the page; an error when it cannot, or the tab stays
// listed until ctx ends.
func (s *Sim) closeTab(ctx context.Context, id, pageID string) error {
	if id == "" {
		return errors.New("no tab id")
	}
	if err := s.Tabs.Close(ctx, id); err != nil {
		return err
	}
	return s.until(ctx, "the closed tab is no longer listed", func() (bool, error) {
		pages, err := s.IWDP.SimulatorPages(ctx)
		if err != nil {
			return false, nil
		}
		for _, p := range pages {
			if p.ID == pageID {
				return false, nil
			}
		}
		return true, nil
	})
}

// tabText names a tab: its tab id, title and URL; a long URL (Safari's
// error page is a data: URL) is cut.
func tabText(p Page, id string) string {
	if id == "" {
		id = "(no tab id)"
	}
	u := p.URL
	if r := []rune(u); len(r) > 100 {
		u = string(r[:100]) + "..."
	}
	return fmt.Sprintf("tab %s (title %q, URL %s)", id, p.Title, u)
}

// emptyURL reports whether a tab's URL is that of an empty tab, which
// Safari reuses for a URL that it opens.
func emptyURL(u string) bool {
	return u == "" || u == "about:blank"
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
		if err := s.leaseCheck(ctx, "sim up", "restarting ios_webkit_debug_proxy to reach the simulator"); err != nil {
			return err
		}
	}
	if !fs.Serving {
		if r.ForwardStarted, err = s.Forward.Ensure(ctx, sock); err != nil {
			return err
		}
	}
	var startErr error
	if restart {
		// Again right before the restart: a session may have taken a
		// lease since the first check. A lease taken between this check
		// and the restart still ends (docs/known-limits.md).
		if err := s.leaseCheck(ctx, "sim up", "restarting ios_webkit_debug_proxy to reach the simulator"); err != nil {
			return err
		}
		startErr = s.IWDP.Restart(ctx)
		r.IWDPRestarted = startErr == nil || errors.Is(startErr, ErrIWDPKept)
	} else {
		startErr = s.IWDP.Start(ctx)
		r.IWDPStarted = startErr == nil || errors.Is(startErr, ErrIWDPKept)
	}
	kept := errors.Is(startErr, ErrIWDPKept)
	if startErr != nil && !kept {
		return startErr
	}
	if kept {
		// The kept iwdp is the one that this start spawned and recorded,
		// after the forward listened: it is synced, also when the command
		// ended at the keep, so that the next Up does not restart it in
		// the middle of its device connection (review F4 of 2026-11-26).
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markKeptWait)
		err := s.Forward.MarkSynced(mctx)
		cancel()
		if err != nil {
			return fmt.Errorf("%w; recording that it started after the forward failed: %w", startErr, err)
		}
		if ctx.Err() != nil {
			return startErr
		}
	} else {
		// Another program may have started an iwdp of its own first.
		if st, err := s.IWDP.State(ctx); err != nil {
			return err
		} else if !st.Own {
			return ErrNotOwnIWDP
		}
		if err := s.Forward.MarkSynced(ctx); err != nil {
			return err
		}
	}
	return s.until(ctx, "ios_webkit_debug_proxy lists the simulator", func() (bool, error) {
		st, err := s.IWDP.State(ctx)
		if kept && err != nil && st.Running && st.Own {
			return false, nil // the kept iwdp answers once its devices are connected
		}
		return st.Listed && st.Own, err
	})
}

// markKeptWait bounds the forward record's update after a kept iwdp start,
// which runs also when the command's deadline has ended.
const markKeptWait = 5 * time.Second

// leaseCheck refuses a restart of iwdp while it would end active leases,
// unless RestartAnyway.
func (s *Sim) leaseCheck(ctx context.Context, cmd, what string) error {
	if s.RestartAnyway {
		return nil
	}
	n, err := s.IWDP.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%s would end the page connections of %d active page lease(s), your own session's too: wait until those sessions end, or run %s --restart-iwdp", what, n, cmd)
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
	Listed       bool    `json:"listed"`  // the iwdp that agent-ios started lists the simulator
	Ready        bool    `json:"ready"`
	// The simulator's reservation (sim reserve), filled in by the caller:
	// its owner and end, and whether the caller's owner holds it.
	ReservedBy    string    `json:"reservedBy,omitempty"`
	ReservedUntil time.Time `json:"reservedUntil,omitzero"`
	ReservedByYou bool      `json:"reservedByYou"`
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

// Booted returns the simulator, which must be booted: one that is not, or
// a Mac that does not answer, is an error that says to run sim up. It
// starts nothing.
func (s *Sim) Booted(ctx context.Context, device string) (Device, error) {
	reachable, err := s.Mac.Reachable(ctx)
	if err != nil {
		return Device{}, err
	}
	if !reachable {
		return Device{}, fmt.Errorf("the Mac %s does not answer over ssh: run agent-ios sim up", s.Cfg.SSH)
	}
	d, err := s.device(ctx, device)
	if err != nil {
		return Device{}, err
	}
	if d.State != "Booted" {
		return d, fmt.Errorf("the simulator %s (%s) is not booted: run agent-ios sim up", d.Name, d.UDID)
	}
	return d, nil
}

// Screenshot returns the simulator and a PNG of its whole screen.
func (s *Sim) Screenshot(ctx context.Context, device string) (Device, []byte, error) {
	d, err := s.Booted(ctx, device)
	if err != nil {
		return d, nil, err
	}
	png, err := s.Mac.Screenshot(ctx, d.UDID)
	return d, png, err
}

// Touch runs one axe input command sub (tap, swipe) with opts on the
// booted simulator: a real touch, which iOS handles as the user's, so a
// tapped text field opens the on-screen keyboard.
func (s *Sim) Touch(ctx context.Context, device, sub string, opts ...string) (Device, error) {
	d, err := s.Booted(ctx, device)
	if err != nil {
		return d, err
	}
	_, err = s.Mac.AXe(ctx, d.UDID, sub, opts)
	return d, err
}

// unshifted are the characters that a US keyboard types without Shift:
// all that sim type can send while the on-screen keyboard is shown.
const unshifted = "abcdefghijklmnopqrstuvwxyz0123456789 `-=[]\\;',./"

// Type types text into the simulator's focused field as key presses. With
// the on-screen keyboard (no hardware keyboard) key presses lose Shift, so
// it then types only unshifted US characters, with US key positions, on a
// simulator whose keyboard layout is US; anything else is refused before a
// key is pressed instead of arriving changed ("A@" as "a2"). With the
// hardware keyboard AXe types what the simulator's layout has keys for,
// and refuses the rest itself.
func (s *Sim) Type(ctx context.Context, device, text string) (Device, error) {
	d, err := s.Booted(ctx, device)
	if err != nil {
		return d, err
	}
	hw, err := s.Mac.HardwareKeyboard(ctx)
	if err != nil {
		return d, err
	}
	var opts []string
	if !hw {
		for _, r := range text {
			if !strings.ContainsRune(unshifted, r) {
				return d, fmt.Errorf("sim type: %q needs Shift or another layout, which key presses lose while the on-screen keyboard is shown: run agent-ios sim keyboard hardware, or tap the keys (sim ui lists them)", string(r))
			}
		}
		us, layout, err := s.Mac.USKeyboard(ctx, d.UDID)
		if err != nil {
			return d, err
		}
		if !us {
			return d, fmt.Errorf("sim type: with the on-screen keyboard it types only on a US keyboard layout, and this one is not (%s): run agent-ios sim keyboard hardware, or tap the keys", layout)
		}
		opts = []string{"--layout", "us"}
	}
	_, err = s.Mac.AXe(ctx, d.UDID, "type", opts, text)
	return d, err
}

// Keyboard sets the keyboard mode: onscreen shows iOS's on-screen keyboard
// for a tapped field, as on an iPhone; hardware types what the layout has
// keys for but hides it. A change quits the Simulator app and shuts the
// simulator down, so every refusal is checked first, before any change:
// the simulator must exist, no other simulator may be booted (quitting the
// app would shut it down too), the iwdp must be agent-ios's, and no page may
// have an active lease (unless RestartAnyway). Then it switches the
// setting and runs Up for that same simulator. It reports whether the mode
// changed; after a failed Up the mode is changed and sim up finishes it.
func (s *Sim) Keyboard(ctx context.Context, device string, onscreen bool) (bool, UpReport, error) {
	hw, err := s.Mac.HardwareKeyboard(ctx)
	if err != nil {
		return false, UpReport{}, err
	}
	if hw != onscreen {
		return false, UpReport{}, nil
	}
	if device == "" {
		device = s.Cfg.Device
	}
	ds, err := s.Mac.Devices(ctx)
	if err != nil {
		return false, UpReport{}, err
	}
	d, err := Pick(ds, device)
	if err != nil {
		return false, UpReport{}, err
	}
	booted, err := s.Mac.BootedAll(ctx)
	if err != nil {
		return false, UpReport{}, err
	}
	for _, o := range booted {
		if o.UDID != d.UDID {
			return false, UpReport{}, fmt.Errorf("sim keyboard: the simulator %s (%s) also runs, and quitting the Simulator app would shut it down: shut it down first", o.Name, o.UDID)
		}
	}
	st, err := s.IWDP.State(ctx)
	if err != nil {
		return false, UpReport{}, err
	}
	if st.Running && !st.Own {
		return false, UpReport{}, ErrNotOwnIWDP
	}
	if st.Running {
		if err := s.leaseCheck(ctx, "sim keyboard", "restarting the simulator to change its keyboard"); err != nil {
			return false, UpReport{}, err
		}
	}
	if err := s.Mac.SetHardwareKeyboard(ctx, d.UDID, !onscreen); err != nil {
		return false, UpReport{}, err
	}
	r, err := s.Up(ctx, d.UDID, "", false)
	if err != nil {
		return true, r, fmt.Errorf("the keyboard is now %s, but bringing the simulator back failed (run agent-ios sim up --device %s): %w", map[bool]string{true: "onscreen", false: "hardware"}[onscreen], d.UDID, err)
	}
	return true, r, nil
}

// Element is an element of the simulator's screen, from its accessibility
// tree: frames are in points, as sim tap takes them.
type Element struct {
	Type  string  `json:"type"`
	Label string  `json:"label,omitempty"`
	Value string  `json:"value,omitempty"`
	ID    string  `json:"id,omitempty"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"width"`
	H     float64 `json:"height"`
}

// UI lists the elements of the simulator's screen that have a label, a
// value or an identifier, in the tree's order (axe describe-ui).
func (s *Sim) UI(ctx context.Context, device string) ([]Element, error) {
	d, err := s.Booted(ctx, device)
	if err != nil {
		return nil, err
	}
	out, err := s.Mac.AXe(ctx, d.UDID, "describe-ui", nil)
	if err != nil {
		return nil, err
	}
	return parseUI(out)
}

// uiNode is a node of axe describe-ui's JSON.
type uiNode struct {
	Type     string          `json:"type"`
	Label    json.RawMessage `json:"AXLabel"`
	Value    json.RawMessage `json:"AXValue"`
	ID       json.RawMessage `json:"AXUniqueId"`
	Frame    uiFrame         `json:"frame"`
	Children []uiNode        `json:"children"`
}

type uiFrame struct{ X, Y, Width, Height float64 }

// scalarText is a JSON string, number or boolean as text; null, an
// object or an array is "" (AXe reports values of several types).
func scalarText(v json.RawMessage) string {
	var x any
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber() // a number keeps its digits, also beyond float64
	if len(v) == 0 || dec.Decode(&x) != nil {
		return ""
	}
	switch x := x.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func parseUI(out []byte) ([]Element, error) {
	var roots []uiNode
	if err := json.Unmarshal(out, &roots); err != nil {
		var one uiNode
		if err2 := json.Unmarshal(out, &one); err2 != nil {
			return nil, fmt.Errorf("reading axe describe-ui: %w", err)
		}
		roots = []uiNode{one}
	}
	var els []Element
	var walk func(n uiNode)
	walk = func(n uiNode) {
		e := Element{Type: n.Type, X: n.Frame.X, Y: n.Frame.Y, W: n.Frame.Width, H: n.Frame.Height}
		e.Label = scalarText(n.Label)
		e.Value = scalarText(n.Value)
		e.ID = scalarText(n.ID)
		if e.Label != "" || e.Value != "" || e.ID != "" {
			els = append(els, e)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return els, nil
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
		// The shared connection takes no new commands; the ones that run
		// over it end with the Mac, or finish when the stop fails.
		s.Mac.CloseShared(ctx)
		if _, err := s.Run(ctx, s.Cfg.Stop[0], s.Cfg.Stop[1:]...); err != nil {
			return r, fmt.Errorf("stopping the Mac with %q: %w", strings.Join(s.Cfg.Stop, " "), err)
		}
		r.MacStopped = true
	}
	return r, nil
}

// shownBefore reports whether the tab id was listed before the open
// showing openURL already: only such a tab can be an earlier turn's.
func shownBefore(before map[string]string, id, openURL string) bool {
	u, ok := before[id]
	return ok && sameURL(u, openURL)
}
