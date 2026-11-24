package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/logcause"
	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/wip"
)

// Defaults of ManagerOptions (design 2.3, 2.5 and 2.6).
const (
	DefaultLeaseTTL       = 5 * time.Minute
	DefaultIdleClose      = 10 * time.Minute
	DefaultRecoveryBudget = 15 * time.Second
	DefaultIdleProbe      = 20 * time.Second
	DefaultSweepEvery     = 5 * time.Second
)

// messageDroppedEvent is the direct transport's EventMessageDropped
// (internal/transport/direct); core does not import that package so that
// binaries without the direct transport do not link go-ios.
const messageDroppedEvent = "Transport.messageDropped"

// iwdpPortFirst and iwdpPortLast bound the device ports of iwdp's default
// configuration ("null:9221,:9222-9322"). An explicit ws:// URL on a
// loopback port in this range can only be a page of a physical device, so
// it is refused when no listing has it (resolveURL).
const (
	iwdpPortFirst = 9222
	iwdpPortLast  = 9322
)

// TransportFunc returns the transport of kind ("auto", "iwdp", "direct",
// "webkitgtk") for the listing or inspector address addr ("" = default).
type TransportFunc func(ctx context.Context, kind, addr string) (transport.Transport, error)

// ManagerOptions configure a Manager. Zero durations take the defaults.
type ManagerOptions struct {
	Transport    TransportFunc
	Reservations *Reservations
	// Session configures page sessions; Setup is set by the manager.
	Session        webkit.SessionOptions
	LeaseTTL       time.Duration
	IdleClose      time.Duration
	RecoveryBudget time.Duration
	IdleProbe      time.Duration
	SweepEvery     time.Duration
	// OpenURL opens an explicit ws:// URL that is not an iwdp device page
	// (a local endpoint or the legacy mock); nil: webkit.NewClient.
	OpenURL func(ctx context.Context, wsURL string) (webkit.Caller, error)
	Logf    func(format string, args ...any)
	// DaemonID identifies the daemon process (stable for its life). Page
	// runtimes carry it as their owner's daemon (PageSession.DaemonID);
	// "" leaves runtime.DaemonID.
	DaemonID string
}

// PageRequest selects a page.
type PageRequest struct {
	Transport string // auto, iwdp, direct, webkitgtk; "" = auto
	Addr      string // listing or inspector address; "" = the transport's default
	Page      string // ws:// URL, page id or key, or a substring of the title or URL; "" = first page
	// prefer is the session's default page for a request without a page
	// (sessionPage): its record key, used while its connection is open
	// (attachedTarget).
	prefer string
}

// AcquireOptions are a client's lease and ownership options (design 2.5).
type AcquireOptions struct {
	Session     string        // logical session; "" = "default"
	Reservation string        // reservation owner; "" = the session name
	WaitLease   time.Duration // wait this long for another session's lease
	Takeover    bool          // reattach a lost page (needs the reservation)
	// Claim, when set, records each lease that this acquisition grants,
	// also when the operation's admission fails after the grant, so that
	// a caller that took a lease only for its own operations can end it
	// with ReleaseClaimed.
	Claim *LeaseClaim
}

// LeaseClaim is the record of the leases that the acquisitions of one
// caller (one command) granted (AcquireOptions.Claim). Its zero value is
// ready for use; it is safe for concurrent use.
//
// An epoch identifies a lease, not exclusive use of it: every command of
// the session uses the session's lease. So only the core, under the page's
// lock that every grant and use takes, can tell whether a lease is still
// the caller's own (ReleaseClaimed).
type LeaseClaim struct {
	mu     sync.Mutex
	leases map[string]*lease // page key: the lease that an acquisition with the claim granted there
}

// record notes that an acquisition with c granted l on the page of key.
// The caller holds that page's lock.
func (c *LeaseClaim) record(key string, l *lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leases == nil {
		c.leases = map[string]*lease{}
	}
	c.leases[key] = l
}

// Keys returns, sorted, the keys of the pages on which an acquisition
// with c was granted a lease.
func (c *LeaseClaim) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.leases))
	for k := range c.leases {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (c *LeaseClaim) lease(key string) *lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leases[key]
}

func (ao AcquireOptions) normalized() AcquireOptions {
	if ao.Session == "" {
		ao.Session = "default"
	}
	if ao.Reservation == "" {
		ao.Reservation = ao.Session
	}
	return ao
}

// SessionOwner is the desired-state owner name of kind for a session.
// Owners of a session end with ":<session>", so that a release finds them.
func SessionOwner(kind, session string) Owner { return Owner(kind + ":" + session) }

type pageTarget struct {
	key      string
	title    string
	url      string
	device   transport.Device
	page     transport.Page
	physical bool
	// listing is the transport and listing address the page was found
	// through (Page empty): the request of the listing that listed it, or
	// for a local URL the request that classified it. An attached page
	// keeps it, so a resolution that returns the attached page without a
	// listing read (attachedTarget, resolveURL) still knows it.
	listing PageRequest
	// open opens a connection whose provisional targets get state (Apply)
	// and whose context replay runs as a change of state (replayContexts).
	open func(ctx context.Context, state *DesiredState) (webkit.Caller, error)
	// refresh lists the page again on its device (nil for a local URL):
	// the current busy hint, for a preflight after the daemon closed its own
	// connection (design 2.5 rule 2).
	refresh func(ctx context.Context) (transport.Page, error)
}

type lease struct {
	session  string
	resOwner string
	epoch    uint64
	since    time.Time
	lastUsed time.Time
	// claim is the claim of the acquisition that granted the lease (nil
	// without one). shared (under the page's mu) is set by every other use
	// of the lease: an acquisition without that claim, an operation begun
	// under the lease by epoch (BeginLeased), an event stream that binds
	// to it (LeaseEnded) or that held it (the end of HoldLease; an open
	// hold counts in Page.holders). ReleaseClaimed ends only a lease that
	// is not shared.
	claim  *LeaseClaim
	shared bool
}

// Page is a page the daemon has attached.
//
// PS and conn are set once, by the first attach (under attachMu and m.mu),
// and never replaced, so a release, recovery or operation that found the
// page through the manager reads them without a lock. A reattach replaces
// only the connection inside conn (pageConn.swap), and only with a setup
// that no release outdated (release_lifecycle.go): every release removes
// its session's owners from the desired state before it ends, without the
// change lock and also when the connection is not attached
// (forgetSession), and the removal increases the release generation.
// reattach and recover check under mu, at the swap, that no release runs
// and that the generation is still the one their setup snapshot was taken
// with; otherwise they close the new connection, wait for a running
// release, and set up again with the current state, until the deadline
// (exit 124). A takeover's lease is granted in the same critical section.
// target changes when the page is selected again or preflighted; it is
// read under mu only (Title, targetNow).
type Page struct {
	Key string
	PS  *PageSession

	m        *Manager
	pageID   string   // transport page id (the ws:// URL for iwdp); identifies the page
	attachMu ctxMutex // serializes attach, takeover and recovery
	conn     *pageConn
	// gapFrom (under attachMu) is where the next Network gap of a new
	// connection starts: the retirement of the last installed connection,
	// or the end of the gap that a new connection reported without being
	// installed. It is zero while an installed connection has reported its
	// gap (retireConn, reportNetworkGap).
	gapFrom time.Time

	mu         sync.Mutex
	target     pageTarget
	lease      *lease
	releasing  bool           // a release of lease runs; acquisition waits for it
	freed      chan struct{}  // closed and replaced when a lease ends or is replaced
	running    int            // managed operations in progress
	holders    map[string]int // open event streams per session (HoldLease)
	lastUsed   time.Time
	recovering bool
	// recoveryRuns counts the background recoveries that set recovering;
	// recoveryDone is closed when the latest one ends (waitRecovery).
	recoveryRuns uint64
	recoveryDone chan struct{}
	// removed: the sweeper closed the page and removed the record from
	// the manager (closeIdle). Set under attachMu and mu; an attach or an
	// acquisition that finds it set returns errPageRemoved.
	removed bool
}

// ctxMutex is a mutex whose Lock gives up when ctx ends, so that a command
// that waits for an attach or a background recovery keeps its deadline.
// The zero value is unlocked.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (l *ctxMutex) slot() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

// Lock locks l, or returns ctx's error when ctx ends first.
func (l *ctxMutex) Lock(ctx context.Context) error {
	select {
	case l.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *ctxMutex) Unlock() { <-l.slot() }

// TryLock locks l when no one holds it and reports whether it did.
func (l *ctxMutex) TryLock() bool {
	select {
	case l.slot() <- struct{}{}:
		return true
	default:
		return false
	}
}

// ManagedOp is an operation that a client opened (design 2.10).
type ManagedOp struct {
	ID      string
	Op      *Op
	Page    *Page
	Session string
	Epoch   uint64
}

// Manager owns every page connection of the daemon: devices, pages,
// sessions, leases, page sessions and operations (design 2.1).
type Manager struct {
	o ManagerOptions

	mu     sync.Mutex
	pages  map[string]*Page
	ops    map[string]*ManagedOp
	nextOp int
	closed bool

	// Background goroutines (the sweeper, background recoveries and
	// background liveness probes) run under bgCtx; Close cancels it and
	// waits for bg (goBackground).
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	// bgMu guards bgClosed and each start of a bg goroutine. It is a leaf
	// lock, so goBackground can run under any other lock: pageConn.swap
	// starts the new connection's watch and event handoff under pg.mu,
	// and Manager.page takes pg.mu under m.mu.
	bgMu     sync.Mutex
	bgClosed bool

	// recoveryHold, when set (tests only), runs in a background recovery
	// after the old connection is retired and before the preflight.
	recoveryHold func(ctx context.Context)
	// lifecycleHook, when set (tests only), is called under pg.mu at each
	// lease grant, connection install (reattach, recover), and release
	// begin and end.
	lifecycleHook func(ev lifecycleEvent)
	// installChecked, when set (tests only), runs in a background recovery
	// under pg.mu after the install check passed and before the swap.
	installChecked func(pg *Page)
	// commitResetHold, when set under mu (tests only), runs in the
	// background reset that a commit with a pending cleanup starts
	// (commitLeftCleanup), before the reset, with the reset's context.
	commitResetHold func(ctx context.Context)

	// epochs counts the lease grants of every page. Epochs only grow
	// within one DaemonID (design 3.1): the page runtime refuses an owner
	// of this daemon with an older epoch than its latest owner's
	// (stale_owner), and the page keeps that owner when the daemon closes
	// an idle page record. A per-record counter would start again at 1.
	epochs atomic.Uint64
}

// NewManager returns a manager and starts its sweeper (idle TTL, idle
// page close, reservation renewal). Close stops it.
func NewManager(o ManagerOptions) *Manager {
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	if o.IdleClose <= 0 {
		o.IdleClose = DefaultIdleClose
	}
	if o.RecoveryBudget <= 0 {
		o.RecoveryBudget = DefaultRecoveryBudget
	}
	if o.IdleProbe <= 0 {
		o.IdleProbe = DefaultIdleProbe
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = DefaultSweepEvery
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.OpenURL == nil {
		o.OpenURL = func(ctx context.Context, u string) (webkit.Caller, error) { return webkit.NewClient(ctx, u) }
	}
	m := &Manager{o: o, pages: map[string]*Page{}, ops: map[string]*ManagedOp{}}
	m.bgCtx, m.bgCancel = context.WithCancel(context.Background())
	m.goBackground(m.sweeper)
	return m
}

// goBackground runs fn in a goroutine that Close cancels (through ctx) and
// waits for. After Close it starts nothing and reports false.
func (m *Manager) goBackground(fn func(ctx context.Context)) bool {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if m.bgClosed {
		return false
	}
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		fn(m.bgCtx)
	}()
	return true
}

// ---- page resolution ----

func isWSURL(s string) bool { return strings.HasPrefix(s, "ws://") || strings.HasPrefix(s, "wss://") }

// iwdpDevicePort reports whether u is a loopback URL on an iwdp device port.
func iwdpDevicePort(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(p.Port())
	if err != nil || port < iwdpPortFirst || port > iwdpPortLast {
		return false
	}
	h := p.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// samePageURL reports whether the page URLs a and b name the same
// endpoint: equal, or equal but for the loopback host names localhost and
// 127.0.0.1. iwdp writes the host of each listing request into the page's
// webSocketDebuggerUrl, so curl http://localhost:9224/json lists
// ws://localhost:9224/devtools/page/1, while the iwdp transport reads the
// listing on 127.0.0.1 (proxy's listingAddr) and names the same page
// ws://127.0.0.1:9224/devtools/page/1 (device check of 2026-11-12: --page
// with the localhost URL was refused as not listed).
func samePageURL(a, b string) bool {
	if a == b {
		return true
	}
	pa, err := url.Parse(a)
	if err != nil {
		return false
	}
	pb, err := url.Parse(b)
	if err != nil {
		return false
	}
	loopback := func(h string) bool { return h == "localhost" || h == "127.0.0.1" }
	if !loopback(pa.Hostname()) || !loopback(pb.Hostname()) || pa.Port() != pb.Port() {
		return false
	}
	pa.Host, pb.Host = "", ""
	return pa.String() == pb.String()
}

// redactURL drops the query and fragment of a page URL for output (R17).
func redactURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func (m *Manager) resolve(ctx context.Context, req PageRequest) (pageTarget, error) {
	if isWSURL(req.Page) {
		return m.resolveURL(ctx, req)
	}
	if t, ok := m.attachedTarget(req); ok {
		return t, nil
	}
	sel := strings.ToLower(req.Page)
	t, listed, err := m.findListed(ctx, req, func(key string, p transport.Page) bool {
		if sel == "" {
			return p.Debuggable
		}
		return req.Page == p.ID || req.Page == key ||
			strings.Contains(strings.ToLower(p.Title), sel) || strings.Contains(strings.ToLower(p.URL), sel)
	})
	if err != nil || t.key != "" {
		return t, err
	}
	if sel == "" {
		return pageTarget{}, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no debuggable page among %d listed; open a page in Safari on the device", ErrNoPage, listed)}
	}
	return pageTarget{}, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no page matches %q among %d listed", ErrNoPage, req.Page, listed)}
}

// findListed lists the pages of req's transport and returns the first page
// for which match holds, with its device (key "" when none matches), and the
// number of pages listed. A matching page is returned even when the page
// list of another device failed; when none matches, a failed page list
// that the request may need (pageListFailed) is the error.
func (m *Manager) findListed(ctx context.Context, req PageRequest, match func(key string, p transport.Page) bool) (pageTarget, int, error) {
	kind := req.Transport
	if kind == "" {
		kind = "auto"
	}
	if m.o.Transport == nil {
		return pageTarget{}, 0, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no transport is configured; pass a ws:// page URL", ErrNoPage)}
	}
	tr, err := m.o.Transport(ctx, kind, req.Addr)
	if err != nil {
		return pageTarget{}, 0, &CodedError{Code: CodeNoPage, Err: m.listingFailed(err)}
	}
	devs, err := tr.Devices(ctx)
	if err != nil {
		return pageTarget{}, 0, &CodedError{Code: CodeNoPage, Err: m.listingFailed(&ListingError{Kind: tr.Kind(), Err: fmt.Errorf("listing devices (%s): %w", tr.Kind(), err)})}
	}
	if len(devs) == 0 {
		return pageTarget{}, 0, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: %s lists no device; connect and unlock the device and enable Web Inspector", ErrNoPage, tr.Kind())}
	}
	var listed int
	var failed error // the first failed page list that the request may need
	for _, d := range devs {
		pages, err := tr.Pages(ctx, d.ID)
		if err != nil {
			if err := m.pageListFailed(tr, d, err); err != nil && failed == nil {
				failed = err
			}
			continue
		}
		for _, p := range pages {
			listed++
			key := string(tr.Kind()) + "|" + d.ID + "|" + p.ID
			if !match(key, p) {
				continue
			}
			page, t, dev := p, tr, d
			return pageTarget{
				refresh: func(ctx context.Context) (transport.Page, error) {
					pages, err := t.Pages(ctx, dev.ID)
					if err != nil {
						return transport.Page{}, fmt.Errorf("listing the pages of %s: %w", dev.Label, err)
					}
					for _, q := range pages {
						if q.ID == page.ID {
							return q, nil
						}
					}
					return transport.Page{}, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: %s no longer lists page %q", ErrNoPage, dev.Label, page.Title)}
				},
				key: key, title: p.Title, url: p.URL, device: d, page: p,
				physical: physicalDevice(t.Kind(), d.ID),
				listing:  PageRequest{Transport: req.Transport, Addr: req.Addr},
				open: func(ctx context.Context, state *DesiredState) (webkit.Caller, error) {
					o := m.o.Session
					o.Setup = func(ctx context.Context, tc webkit.TargetCaller) error { return state.Apply(ctx, tc) }
					o.AgentReset = agentResetCalls
					o.ContextReplay = state.replayContexts
					o.NetworkDisabled = func(s *webkit.Session, target string) { state.networkDisabled(s, target) }
					return webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return t.Open(ctx, page) }, o)
				},
			}, listed, nil
		}
	}
	return pageTarget{}, listed, failed
}

// resolveURL resolves an explicit ws:// URL to the page record that
// discovery would give it (design 2.5: one page, one connection, one
// lease): a connected page with that page id, else the listed page of the
// request's transport with its device, OS version and busy hint. The
// transport is the request's or, for a request without transport or
// address, the daemon's configured one (daemon start --transport iwdp
// --addr host:port): an empty request is no evidence of a local page. Its
// listing is read whenever its page ids can be the URL (urlListing).
// Classifying a URL never starts iwdp (WithoutTransportStart). An iwdp
// device-port URL that the listing does not have is refused; any other URL
// that the listing does not have (or that no listing answers for) is a
// local endpoint (WebKitGTK, the legacy mock), which needs no reservation.
func (m *Manager) resolveURL(ctx context.Context, req PageRequest) (pageTarget, error) {
	u := req.Page
	if pg := m.attachedPage(u); pg != nil {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return pg.target, nil
	}
	iwdpPort := iwdpDevicePort(u)
	lctx := WithoutTransportStart(ctx)
	if m.urlListing(lctx, req, iwdpPort) {
		t, _, err := m.findListed(lctx, req, func(_ string, p transport.Page) bool { return samePageURL(p.ID, u) })
		if err == nil && t.key != "" {
			return t, nil
		}
		if err != nil {
			m.o.Logf("resolving a page URL: the listing did not answer, so it is a local endpoint: %v", logsafe.Error(err))
		}
		if iwdpPort {
			if err != nil {
				return pageTarget{}, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: %s is on an iwdp device port, and the device of its page is unknown: %w", ErrNoPage, redactURL(u), err)}
			}
			return pageTarget{}, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: iwdp does not list %s; list the pages again and use a current URL", ErrNoPage, redactURL(u))}
		}
	}
	return m.urlTarget(u, req), nil
}

// urlListing reports whether the listing of req's effective transport can
// list a page whose id is an explicit ws:// URL. iwdp page ids are their
// WebSocket URLs, and iwdp's device ports are configurable (-c) for the
// default listing address as for any other, so only the listing itself
// shows whether a URL is one of its pages: it is always read. WebKitGTK
// page ids are URLs too. Direct-transport page ids are never URLs, and
// listing them would open a device service.
func (m *Manager) urlListing(ctx context.Context, req PageRequest, iwdpPort bool) bool {
	if m.o.Transport == nil {
		return false
	}
	kind := req.Transport
	if kind == "" {
		kind = "auto"
	}
	tr, err := m.o.Transport(ctx, kind, req.Addr)
	if err != nil {
		m.o.Logf("resolving a page URL: transport %s: %v", logsafe.ID(kind), logsafe.Error(err))
		return iwdpPort
	}
	switch tr.Kind() {
	case transport.KindIWDP, transport.KindWebKitGTK:
		return true
	}
	return false
}

// attachedPage returns the page record whose page id is id (samePageURL)
// and that has a connection, or nil. A record without one (its attach was refused) keeps
// the device and busy hint of an old listing, so the URL is listed again.
func (m *Manager) attachedPage(id string) *Page {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pg := range m.pages {
		if samePageURL(pg.pageID, id) && pg.conn != nil {
			return pg
		}
	}
	return nil
}

// urlTarget is the local endpoint u, classified as one by the request req.
func (m *Manager) urlTarget(u string, req PageRequest) pageTarget {
	t := pageTarget{
		key: "url|" + u, title: redactURL(u), url: u, page: transport.Page{ID: u, Debuggable: true},
		listing: PageRequest{Transport: req.Transport, Addr: req.Addr},
	}
	t.open = func(ctx context.Context, _ *DesiredState) (webkit.Caller, error) {
		return m.o.OpenURL(ctx, u)
	}
	return t
}

// ---- ownership preflight (design 2.5) ----

func (m *Manager) preflight(t pageTarget, resOwner string) error {
	if !t.physical {
		// Local targets need no reservation; a simulator that another
		// owner reserved (agents taking turns, sim reserve) is refused.
		if t.device.ID != SimulatorDevice {
			return nil
		}
		return m.SimulatorAdmits(resOwner)
	}
	if m.o.Reservations == nil {
		return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: reservations are not configured", ErrNoReservation)}
	}
	mine, other, err := m.o.Reservations.Check(t.device.ID, resOwner)
	if err != nil {
		return &CodedError{Code: CodeReservation, Err: err}
	}
	if mine == nil {
		if other != nil {
			return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: device %s is reserved by %q until %s; wait, or ask that owner", ErrReserved, t.device.Label, other.Owner, other.Expires.Format(time.RFC3339))}
		}
		return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w for %q: a physical device needs one before any page connection; run 'iwdp-cli device reserve --owner %s' (or set IWDP_DEVICE_RESERVATION to the owner of an existing reservation)", ErrNoReservation, resOwner, resOwner)}
	}
	if t.page.Ownership == transport.OwnershipBusy {
		return &CodedError{Code: CodeBusy, Err: fmt.Errorf("page %q: %w", t.title, ErrPageBusy)}
	}
	return nil
}

// freshPreflight is the preflight after the daemon retired its own
// connection to pg (recovery, reopen). The stored busy hint can come from a
// listing taken while that connection was open, when iwdp lists the page as
// held by the daemon itself; the page is listed again first. iwdp notices
// the closed connection asynchronously, so a busy page is listed again for
// up to selfBusyGrace before the busy hint counts. A page that another
// client holds stays busy and is refused.
func (m *Manager) freshPreflight(ctx context.Context, pg *Page, t pageTarget, resOwner string) error {
	if t.physical && t.refresh != nil {
		end := time.Now().Add(selfBusyGrace)
		for {
			p, err := t.refresh(ctx)
			if err != nil {
				return err
			}
			t.page.Ownership = p.Ownership
			if p.Ownership != transport.OwnershipBusy || !time.Now().Before(end) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(selfBusyPoll):
			}
		}
		pg.mu.Lock()
		pg.target.page.Ownership = t.page.Ownership
		pg.mu.Unlock()
	}
	return m.preflight(t, resOwner)
}

// selfBusyGrace bounds how long freshPreflight lists a busy page again
// after the daemon closed its own connection; selfBusyPoll is the interval.
const (
	selfBusyGrace = time.Second
	selfBusyPoll  = 50 * time.Millisecond
)

// ---- attach, takeover and recovery ----

func (m *Manager) page(t pageTarget) (*Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errShuttingDown
	}
	pg := m.pages[t.key]
	if pg == nil && t.page.ID != "" {
		// The same page selected another way (design 2.5: one record).
		for _, p := range m.pages {
			if p.pageID == t.page.ID {
				pg = p
				break
			}
		}
		if pg != nil && !strings.HasPrefix(t.key, "url|") {
			// A listed selection carries the device, OS version and busy
			// hint that a bare URL lacks; the record keeps its key.
			pg.mu.Lock()
			if strings.HasPrefix(pg.target.key, "url|") {
				t.key = pg.Key
				pg.target = t
			}
			pg.mu.Unlock()
		}
	}
	if pg == nil {
		pg = &Page{Key: t.key, m: m, pageID: t.page.ID, target: t, freed: make(chan struct{})}
		m.pages[t.key] = pg
	}
	return pg, nil
}

// openConn opens a connection to t that applies pg's desired state to
// provisional targets, then sends the complete desired state to it. It
// returns the release generation of the snapshot it sent (applyConn).
func (m *Manager) openConn(ctx context.Context, pg *Page, t pageTarget, state *DesiredState, fresh bool) (webkit.Caller, uint64, error) {
	c, err := t.open(ctx, state)
	if err != nil {
		return nil, 0, attachError(ctx, fmt.Errorf("connecting to page %q: %w", t.title, err))
	}
	drop := closeCaller
	if pg.PS != nil {
		// A reconnect: the recorded execution contexts are void on c
		// (Scheduler.contextsMoved), and the events of c's setup reach the
		// page's subscribers when c is installed (pageConn.prepare); the
		// caller drops them with c when it is not (Page.dropConn).
		pg.PS.Sched.contextsMoved()
		if pg.conn != nil {
			pg.conn.prepare(c)
			drop = pg.dropConn
		}
	}
	if tf, ok := c.(interface {
		SubscribeSync(method string, h webkit.EventHandler) func()
	}); ok {
		state.followCommits(tf, func() { m.commitLeftCleanup(pg) })
	}
	if fresh && t.physical {
		// Design 2.6 step 4: a hidden tab accepts commands and never answers.
		if err := probeCaller(ctx, c); err != nil {
			// A peer end tells another story than a hidden tab; it is read
			// before c is closed here.
			lerr := lostOnPeerEnd(c, err)
			drop(c)
			switch {
			case canceledByOwner(ctx, err):
				return nil, 0, fmt.Errorf("connecting to page %q: %w: %w", t.title, errProbeCanceled, err)
			case errors.Is(lerr, ErrLost):
				return nil, 0, pg.newConnLost(t.title, "its liveness probe", lerr, err)
			}
			return nil, 0, &CodedError{Code: CodeBusy, Err: fmt.Errorf("tab is not answering; it is probably not the visible Safari tab. Bring it to the front on the device (%w)", err)}
		}
	}
	gen, err := state.applyConn(ctx, c)
	if err != nil {
		lerr := lostOnPeerEnd(c, err) // read before c is closed here
		drop(c)
		if errors.Is(lerr, ErrLost) && !errors.Is(err, context.DeadlineExceeded) && !canceledByOwner(ctx, err) {
			return nil, 0, pg.newConnLost(t.title, "the re-apply of the page state", lerr, err)
		}
		return nil, 0, attachError(ctx, fmt.Errorf("re-applying the page state: %w", err))
	}
	return c, gen, nil
}

// newConnLost is the error of a new connection that a peer ended during
// what, before it was installed: lerr is cause with ErrLost
// (lostOnPeerEnd). Another inspector may hold the page now. A reconnect
// (reopen, takeover, recovery) marks the page lost (pageConn.markLost),
// so that only --takeover connects it again, also on the next command
// (design 2.5 rule 3), and returns lerr. A first attach has no page state
// to mark and nothing to take over: its error is not ErrLost and tells to
// run the command again. In the device check of 2026-11-12, iwdp ended
// first attaches to a page that no other client held, about every second
// time, and the next command attached; the --takeover guidance of ErrLost
// was wrong there. The caller holds attachMu.
func (pg *Page) newConnLost(title, what string, lerr, cause error) error {
	if pg.conn == nil {
		return &CodedError{Code: CodeBusy, Err: fmt.Errorf("connecting to page %q: ios_webkit_debug_proxy or the device closed the new connection during %s, and nothing was attached: run the command again. When it fails the same way again, another inspector (Safari Web Inspector on a Mac, another tool) may hold the page, or iwdp lost the device (run iwdp-cli devices): %w", title, what, cause)}
	}
	err := &CodedError{Code: CodeBusy, Err: fmt.Errorf("connecting to page %q: the new connection ended during %s: %w", title, what, lerr)}
	pg.conn.markLost(err)
	return err
}

// attachError codes a failed connect or setup of openConn: a timeout (exit
// 124, decision D9) when the command's deadline ended it, which webkit
// reports as context.DeadlineExceeded with its cause (a call without a
// reply, or a write or read that the deadline cut off); the cancellation
// itself when ctx's owner (the client, Manager.Close) canceled it, which
// says nothing about the page (canceledByOwner); else busy (exit 4).
func attachError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &CodedError{Code: CodeTimeout, Err: err}
	case canceledByOwner(ctx, err):
		return err
	}
	return &CodedError{Code: CodeBusy, Err: err}
}

// dropConn discards and closes c, a new connection of a reconnect that is
// not installed (pageConn.discard).
func (pg *Page) dropConn(c webkit.Caller) {
	pg.conn.discard(c)
	closeCaller(c)
}

// commitLeftCleanup starts the background reset of the agents that a
// commit left unknown (DesiredState.followCommits): the committed target
// may still hold released state, and no later change may come to reset it
// (resetUnknownAgents). A page record whose first attach has not finished
// has no state to reset.
func (m *Manager) commitLeftCleanup(pg *Page) {
	m.mu.Lock()
	attached, hold := pg.PS != nil, m.commitResetHold
	m.mu.Unlock()
	if attached {
		m.pageLogf(pg.targetNow(), "a commit left a cleanup pending; resetting its agents in the background")
		m.resetUnknownLater(pg, hold)
	}
}

// waitEnded is the error of a wait for what that ctx ended: a timeout (exit
// 124) at the deadline, the cancellation when ctx's owner canceled it.
func waitEnded(ctx context.Context, what string) error {
	err := fmt.Errorf("waiting for %s: %w", what, ctx.Err())
	if canceledByOwner(ctx, ctx.Err()) {
		return err
	}
	return &CodedError{Code: CodeTimeout, Err: err}
}

// ensureAttached attaches pg for a command: the first attach, a reopen of a
// stalled or closed page, or a takeover of a lost one. A command that comes
// while a background recovery runs waits for it within ctx (orchestrator
// decision C05): it continues on the new connection when the recovery
// succeeds, gets the recovery's failure (exit 4) when it fails, and a
// timeout (exit 124) when ctx ends first. A lost page is never waited for;
// it needs --takeover.
func (m *Manager) ensureAttached(ctx context.Context, pg *Page, t pageTarget, ao AcquireOptions) error {
	pg.mu.Lock()
	runs := pg.recoveryRuns
	if pg.recovering {
		runs-- // the running recovery is one that this command waits for
	}
	pg.mu.Unlock()
	for {
		err := m.attach(ctx, pg, t, ao, runs)
		if !errors.Is(err, errAwaitRecovery) {
			return err
		}
		if err := pg.waitRecovery(ctx); err != nil {
			return err
		}
	}
}

// errAwaitRecovery: a background recovery has started and not yet taken
// attachMu (attach); the command waits for it without the lock.
var errAwaitRecovery = errors.New("a background reconnect is running")

// waitRecovery waits until the background recovery of pg that runs, if
// any, has ended, at most until ctx ends.
func (pg *Page) waitRecovery(ctx context.Context) error {
	pg.mu.Lock()
	rec, done, title := pg.recovering, pg.recoveryDone, pg.target.title
	pg.mu.Unlock()
	if !rec {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return waitEnded(ctx, fmt.Sprintf("the background reconnect of page %q", title))
	}
}

// attach is one look at pg under attachMu (ensureAttached). It returns
// errAwaitRecovery when a background recovery has started and not yet
// taken attachMu. runs is recoveryRuns when the command came, less one when
// a recovery ran then: a later count means that the command waited for a
// recovery, here or in waitRecovery, and a stalled page is then that
// recovery's outcome.
func (m *Manager) attach(ctx context.Context, pg *Page, t pageTarget, ao AcquireOptions, runs uint64) error {
	if err := pg.attachMu.Lock(ctx); err != nil {
		pg.mu.Lock()
		what := "another attach of page %q"
		if pg.recovering {
			what = "the background reconnect of page %q"
		}
		pg.mu.Unlock()
		return waitEnded(ctx, fmt.Sprintf(what, t.title))
	}
	defer pg.attachMu.Unlock()
	pg.mu.Lock()
	if pg.removed {
		pg.mu.Unlock()
		return errPageRemoved
	}
	pg.target.title, pg.target.url, pg.target.page.Ownership = t.title, t.url, t.page.Ownership
	recovering, waited := pg.recovering, pg.recoveryRuns > runs
	pg.mu.Unlock()
	if pg.conn == nil {
		if err := m.preflight(t, ao.Reservation); err != nil {
			return err
		}
		state := NewDesiredState()
		// The agent runtime is a page part, not a session's (design 3.1
		// install path 2): new documents and provisional targets get it
		// before page scripts run, and a release keeps it. A bootstrap
		// part is a requirement of Page (DesiredState.Needs), so Page
		// stays enabled while the part exists, as the bootstrap script
		// needs (design 3.1), and Page events give the document
		// generations: a legacy Page.disable and the release of the last
		// Page owner send nothing. The part is set before the first
		// connection opens, so every setup snapshot has it, also that of
		// a provisional target created while the connection opens; no
		// target exists yet that a change (beginChange) would have to
		// reach.
		if err := state.SetPart(Owner(RuntimePart), RuntimePart, RankRuntime, runtime.Source()); err != nil {
			return err
		}
		c, _, err := m.openConn(ctx, pg, t, state, true)
		if err != nil {
			return err
		}
		pc := newPageConn(c, func() { m.goBackground(func(ctx context.Context) { m.recover(ctx, pg) }) }, m.goBackground,
			func(f string, a ...any) { m.pageLogf(t, f, a...) })
		pc.Subscribe(messageDroppedEvent, func(string, json.RawMessage) { pc.markDropped() })
		// One runtime and one ref table per page record (design 3.3): the
		// page keeps one set of ref maps per document, held by one lease
		// owner at a time, and the table mirrors them. Both live on pc, so
		// they survive reconnects; an owner change resets the page maps,
		// and the page's reset report clears the table (NoteReset), whose
		// counter never moves back while the record lives.
		rt := runtime.New(pc, pc.Generation)
		sched := NewScheduler(pc, SchedulerOptions{Generation: pc.Generation, Runtime: rt.RuntimeCaller()})
		state.useHolds(sched)
		// m.mu: allPages (the sweeper, Status) reads PS under it. After
		// Close began, nothing is installed: Close no longer looks for
		// pages to close (allPages after closed).
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			sched.Close()
			pc.close()
			return errShuttingDown
		}
		pg.conn = pc
		pg.PS = &PageSession{
			Caller: pc, Device: t.device, Page: t.page, Runtime: rt, Refs: refs.NewTable(), DaemonID: m.o.DaemonID,
			Sched: sched, State: state, Boot: state.Boot,
		}
		m.mu.Unlock()
		return nil
	}
	st := pg.conn.State()
	if st != ConnOK && (st != ConnLost || ao.Takeover) {
		// A reattach below installs a new connection, and a takeover a
		// new lease. A release that runs may still clean up on the
		// connection, and when it ends it clears the scheduler lease and
		// closes the lease channel, which would end a lease granted
		// meanwhile. So a reattach starts only after that release ended.
		// A release claimed later finds this connection not attached and
		// cleans up nothing (releasePage); reattach then leaves the grant
		// to acquire, which waits for that release too.
		if err := pg.waitReleased(ctx); err != nil {
			return err
		}
		pg.mu.Lock()
		recovering, waited = pg.recovering, pg.recoveryRuns > runs
		pg.mu.Unlock()
		st = pg.conn.State()
	}
	switch {
	case st == ConnLost:
		if !ao.Takeover {
			// A loss that a new connection's peer end caused names it
			// (markLost, with the recovery's failure when a recovery
			// lost it).
			pg.conn.mu.Lock()
			lerr := pg.conn.lostErr
			pg.conn.mu.Unlock()
			if lerr == nil {
				lerr = ErrLost
			}
			return &CodedError{Code: CodeBusy, Err: fmt.Errorf("page %q: %w", t.title, lerr)}
		}
		if t.physical {
			if err := m.preflight(t, ao.Reservation); err != nil {
				return err
			}
		}
		return m.reattach(ctx, pg, t, ao, "takeover")
	case st == ConnStalled && recovering:
		return errAwaitRecovery
	case st == ConnStalled && waited:
		// The recovery that this command waited for failed: the command
		// gets that failure (exit 4) and does not reconnect again itself.
		// A command that comes after the failure reopens the page below.
		pg.conn.mu.Lock()
		serr := pg.conn.stallErr
		pg.conn.mu.Unlock()
		if errors.Is(serr, ErrRecoveryFailed) {
			return &CodedError{Code: CodeBusy, Err: fmt.Errorf("page %q: %w", t.title, serr)}
		}
		fallthrough
	case st == ConnClosed || st == ConnStalled:
		// The busy hint of t may show the daemon's own stalled connection:
		// retire it, then list the page again (design 2.5 rule 2).
		pg.retireConn()
		if err := m.freshPreflight(ctx, pg, t, ao.Reservation); err != nil {
			return err
		}
		return m.reattach(ctx, pg, t, ao, "reopen")
	}
	return nil
}

// reattach opens a new connection, re-applies the desired state, swaps it
// in and grants the lease to the caller's session with a new epoch.
//
// No connection is installed and no lease granted while a release of the
// lease runs, nor after a release that began during the connect: the
// release ends the old lease when it finishes, and it drops the released
// session's desired state, which this connection's setup may have applied.
// The check is the release generation of the setup's snapshot, compared
// under pg.mu at the swap (release_lifecycle.go). Then the new connection
// is closed, and after the release the page is connected again with the
// state that remains, until ctx ends (exit 124).
func (m *Manager) reattach(ctx context.Context, pg *Page, t pageTarget, ao AcquireOptions, why logsafe.Label) error {
	pg.retireConn()
	for {
		c, gen, err := m.openConn(ctx, pg, t, pg.PS.State, true)
		if err != nil {
			if networkDisableWritten(err) {
				pg.reportNetworkGap()
			}
			return err
		}
		// The new backend holds none of the held requests, whether this
		// connection is installed or closed below.
		pg.reportNetworkGap()
		// pg.mu from the check to the grant: a release cannot be claimed
		// between them (releaseLease claims under pg.mu), and Close does not
		// close the page's connection between them (shuttingDown).
		pg.mu.Lock()
		if m.shuttingDown() {
			pg.mu.Unlock()
			pg.dropConn(c)
			return errShuttingDown
		}
		if pg.releasing || pg.PS.State.ReleaseGen() != gen {
			pg.mu.Unlock()
			pg.dropConn(c)
			if err := pg.waitReleased(ctx); err != nil {
				return err
			}
			m.pageLogf(t, "%s: a release ran during the connect; connecting again", why)
			continue
		}
		if err := pg.conn.swap(c); err != nil {
			pg.mu.Unlock()
			pg.dropConn(c)
			return &CodedError{Code: CodeBusy, Err: fmt.Errorf("page %q: %s: %w", t.title, why, err)}
		}
		pg.gapFrom = time.Time{}
		m.lifecycle("install", pg, nil)
		if why == "takeover" {
			pg.grantLocked(ao, time.Now())
		}
		pg.mu.Unlock()
		m.pageLogf(t, "%s: reattached", why)
		return nil
	}
}

// waitReleased waits until no release of pg's lease runs, at most until
// ctx ends.
func (pg *Page) waitReleased(ctx context.Context) error {
	for {
		pg.mu.Lock()
		releasing, freed, title := pg.releasing, pg.freed, pg.target.title
		pg.mu.Unlock()
		if !releasing {
			return nil
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return &CodedError{Code: CodeTimeout, Err: fmt.Errorf("waiting for the release of the lease of page %q: %w", title, ctx.Err())}
		}
	}
}

// recover is the background recovery of design 2.6 step 3: one reconnect,
// bounded by RecoveryBudget, only while the lease is valid and the page is
// not lost, after the ownership preflight; it re-applies the desired state.
// It is a barrier (design 2.10): admission closes and the running operation
// ends first, the old connection is retired as expected before the new one
// is dialed (a single-inspector endpoint closes the old one on a new
// connection), and the new connection is installed only while the lease it
// started under still holds the page. A new connection that fails the
// liveness probe is closed and the page stays stalled: the page still does
// not answer. It runs in the background (goBackground): Manager.Close
// cancels bg and waits for it.
func (m *Manager) recover(bg context.Context, pg *Page) {
	pg.mu.Lock()
	l := pg.lease
	if l == nil || pg.recovering || pg.releasing {
		pg.mu.Unlock()
		return
	}
	pg.recovering = true
	pg.recoveryRuns++
	done := make(chan struct{})
	pg.recoveryDone = done
	resOwner := l.resOwner
	t := pg.target
	pg.mu.Unlock()
	defer func() {
		pg.mu.Lock()
		pg.recovering = false
		close(done)
		pg.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(bg, m.o.RecoveryBudget)
	defer cancel()
	fail := func(err error) {
		if bg.Err() != nil {
			// Close canceled the recovery: no failure of the page, and
			// Close closes the connection next.
			m.pageLogf(t, "background recovery canceled by the manager's shutdown: %v", logsafe.Error(err))
			return
		}
		m.pageLogf(t, "background recovery failed: %v", logsafe.Error(err))
		pg.conn.mu.Lock()
		switch pg.conn.state {
		case ConnStalled:
			pg.conn.stallErr = fmt.Errorf("%w: %w", ErrRecoveryFailed, err)
		case ConnLost:
			// The recovery's new connection was ended by a peer
			// (newConnLost): the page is lost with this failure.
			pg.conn.lostErr = fmt.Errorf("%w: %w", ErrRecoveryFailed, err)
		}
		pg.conn.mu.Unlock()
	}
	if err := pg.attachMu.Lock(ctx); err != nil {
		fail(fmt.Errorf("waiting for another attach: %w", err))
		return
	}
	defer pg.attachMu.Unlock()
	if pg.conn.State() != ConnStalled {
		return // lost pages are never recovered
	}
	reopen, err := pg.PS.Sched.Barrier(ctx)
	if err != nil {
		fail(err)
		return
	}
	defer reopen()
	if !pg.holds(l) {
		fail(errLeaseChanged)
		return
	}
	// The stored busy hint may show this daemon's own connection (a
	// listing taken by a later command while it was open): retire the
	// connection first, then list the page again for the preflight.
	pg.retireConn()
	if m.recoveryHold != nil {
		m.recoveryHold(ctx)
	}
	if err := ctx.Err(); err != nil {
		fail(err) // Close canceled the recovery, or its budget ended
		return
	}
	if err := m.freshPreflight(ctx, pg, t, resOwner); err != nil {
		fail(err)
		return
	}
	for {
		c, gen, err := m.openConn(ctx, pg, t, pg.PS.State, false)
		if err != nil {
			if networkDisableWritten(err) {
				pg.reportNetworkGap()
			}
			fail(err)
			return
		}
		// The new connection's setup continued every held request, also
		// when the connection is not installed below.
		pg.reportNetworkGap()
		if err := probeCaller(ctx, c); err != nil {
			lerr := lostOnPeerEnd(c, err) // read before c is closed here
			pg.dropConn(c)
			if errors.Is(lerr, ErrLost) {
				// Another inspector may hold the page: lost, not stalled,
				// so that only --takeover connects it again (newConnLost).
				fail(pg.newConnLost(t.title, "its liveness probe", lerr, err))
			} else {
				fail(fmt.Errorf("the new connection does not answer either: %w", err))
			}
			return
		}
		// The install rule of reattach (release_lifecycle.go): a changed
		// release generation means the snapshot may hold removed state, so
		// the connection is not installed. A release of the lease ends l
		// (it holds the page no more, or releasing is set), and the
		// recovery stops. Otherwise only an owner's state was removed under
		// the same lease (a CDP attachment's release, ForgetOwner; the
		// barrier excludes ReleaseOwner inside an operation), and the page
		// is set up again with the current state until the budget ends. An
		// owner removal takes pg.mu too (Manager.forgetOwner), so it comes
		// before this check or after the swap.
		// Close does not close the page's connection between the check
		// and the swap (shuttingDown).
		pg.mu.Lock()
		closing := m.shuttingDown()
		held := pg.lease == l && !pg.releasing && pg.conn.State() == ConnStalled
		install := !closing && held && pg.PS.State.ReleaseGen() == gen
		var serr error
		if install {
			if m.installChecked != nil {
				m.installChecked(pg)
			}
			if serr = pg.conn.swap(c); serr == nil {
				pg.gapFrom = time.Time{}
				m.lifecycle("install", pg, nil)
			}
		}
		pg.mu.Unlock()
		if install && serr == nil {
			m.pageLogf(t, "background recovery reconnected")
			return
		}
		pg.dropConn(c)
		switch {
		case closing:
			fail(errShuttingDown)
			return
		case serr != nil:
			fail(serr)
			return
		}
		if !held {
			fail(errLeaseChanged)
			return
		}
		m.pageLogf(t, "background recovery: the page state changed during the reconnect; connecting again")
	}
}

// errShuttingDown: Manager.Close has begun, so no attach, reattach or
// recovery installs a connection (shuttingDown).
var errShuttingDown = errors.New("the daemon is shutting down")

// shuttingDown reports whether Close has begun. Close sets it (bgClosed)
// before it closes the pages, and it locks each page's pg.mu before it
// closes that page's connection. A reattach or recovery checks it under
// pg.mu and keeps pg.mu until its swap: so either Close sees and closes
// the new connection, or the install sees Close and closes the connection
// itself. bgMu is a leaf lock, so the check runs under pg.mu.
func (m *Manager) shuttingDown() bool {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	return m.bgClosed
}

// errLeaseChanged: the lease that a recovery started under ended or was
// replaced, so the recovery installs nothing.
var errLeaseChanged = errors.New("the lease ended or changed during the reconnect")

// retireConn retires the page's connection (pageConn.retire). No
// connection delivers the page's events from then on, so the Network gap
// that the next new connection reports starts here, unless an earlier gap
// is still open (gapFrom). The caller holds attachMu.
func (pg *Page) retireConn() {
	if pg.gapFrom.IsZero() {
		pg.gapFrom = time.Now()
	}
	pg.conn.retire()
}

// reportNetworkGap reports the Network reset of a new connection's setup
// (newConnectionReset) from gapFrom. A setup that may have written its
// Network.disable reports it also when it fails (networkDisableWritten):
// WebKit continued the held requests then. Until a new connection is
// installed, which clears gapFrom, the next gap starts where this one ends,
// so a retry adds the interval without a hole and without these requests.
// The caller holds attachMu.
func (pg *Page) reportNetworkGap() {
	pg.gapFrom = newConnectionReset(pg.PS.Sched, pg.PS.State, pg.gapFrom)
}

// holds reports whether l is still the page's lease and no release runs.
func (pg *Page) holds(l *lease) bool {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.lease == l && !pg.releasing
}

// Do runs fn inside an operation and ends it. A retrySafe fn (a pure read,
// design 2.6) that fails with ErrSessionStalled or ErrTargetSwapped runs
// once more after the page is attached again, inside the same deadline;
// every other command is never retried automatically. An error of fn that
// the operation's own deadline caused is a deadline error (deadlineCaused).
func (m *Manager) Do(ctx context.Context, req PageRequest, ao AcquireOptions, name string, class Class, retrySafe bool, fn func(context.Context, *ManagedOp) error) error {
	var pg *Page
	attempt := func() error {
		mo, err := m.Begin(ctx, req, ao, name, class)
		if err != nil {
			return err
		}
		pg = mo.Page
		defer m.End(mo.ID)
		return deadlineCaused(mo.Op.Context(), fn(mo.Op.Context(), mo))
	}
	err := attempt()
	if retrySafe && pg != nil && (errors.Is(err, webkit.ErrSessionStalled) || errors.Is(err, webkit.ErrTargetSwapped)) {
		if werr := m.WaitRecovered(ctx, pg); werr != nil {
			return fmt.Errorf("%w; waiting for the reconnect to retry: %w", err, werr)
		}
		return attempt()
	}
	return err
}

// deadlineCaused returns err, the error of a function that ran under ctx,
// an operation's context, with ErrDeadline in its chain when it is
// context.Canceled and the operation's deadline canceled ctx. The scheduler
// ends an operation at its deadline with the cause ErrDeadline
// (Op.watch), and ctx.Err() is then context.Canceled: a function that
// returns ctx.Err() would fail as a cancellation (exit 1) instead of a
// deadline (exit 124, decision D9). The operation's expiry and the
// deadline timer of the caller's context fire at the same instant in
// either order, so the result must not depend on which runs first.
func deadlineCaused(ctx context.Context, err error) error {
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !errors.Is(context.Cause(ctx), ErrDeadline) {
		return err
	}
	return fmt.Errorf("%w: %w", err, ErrDeadline)
}

// WaitRecovered waits until the page is attached again after a stall, at
// most until ctx ends. It is for retry-safe reads (design 2.6).
func (m *Manager) WaitRecovered(ctx context.Context, pg *Page) error {
	for {
		switch pg.conn.State() {
		case ConnOK:
			return nil
		case ConnLost, ConnClosed:
			return &CodedError{Code: CodeBusy, Err: ErrLost}
		}
		pg.mu.Lock()
		rec := pg.recovering
		pg.mu.Unlock()
		if !rec {
			pg.conn.mu.Lock()
			err := pg.conn.stallErr
			pg.conn.mu.Unlock()
			if errors.Is(err, ErrRecoveryFailed) {
				return &CodedError{Code: CodeBusy, Err: err}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ---- leases ----

func (pg *Page) grantLocked(ao AcquireOptions, now time.Time) *lease {
	if pg.lease != nil {
		// A takeover replaces the lease: what was bound to it ends.
		close(pg.freed)
		pg.freed = make(chan struct{})
	}
	l := &lease{session: ao.Session, resOwner: ao.Reservation, epoch: pg.m.epochs.Add(1), since: now, lastUsed: now, claim: ao.Claim}
	pg.lease = l
	pg.m.lifecycle("grant", pg, l)
	pg.PS.Sched.SetLease(ao.Session, l.epoch)
	if ao.Claim != nil {
		ao.Claim.record(pg.Key, l)
	}
	return l
}

func (pg *Page) leaseErrLocked(l *lease) error {
	return &CodedError{Code: CodeBusy, Err: fmt.Errorf("%w: page %q is leased by session %q since %s (last command %s ago); use --session %s, wait, or run 'iwdp-cli lease break %s'",
		ErrLeased, pg.target.title, l.session, l.since.Format(time.TimeOnly), time.Since(l.lastUsed).Round(time.Second), l.session, pg.Key)}
}

// acquire grants or renews the lease of ao's session. For an operation
// (op), the page's running count is raised in the same critical section, so
// that the sweeper never expires a lease between its renewal and the
// operation's start. It returns the epoch and the page's last use before
// this renewal (zero for a page never used). A release in progress is
// waited for: nothing is admitted under a lease that is being released.
func (m *Manager) acquire(ctx context.Context, pg *Page, ao AcquireOptions, op bool) (uint64, time.Time, error) {
	var waitEnd <-chan time.Time
	if ao.WaitLease > 0 {
		t := time.NewTimer(ao.WaitLease)
		defer t.Stop()
		waitEnd = t.C
	}
	for {
		now := time.Now()
		pg.mu.Lock()
		if pg.removed {
			pg.mu.Unlock()
			return 0, time.Time{}, errPageRemoved
		}
		if pg.releasing {
			freed, title := pg.freed, pg.target.title
			pg.mu.Unlock()
			select {
			case <-freed:
				continue
			case <-ctx.Done():
				return 0, time.Time{}, &CodedError{Code: CodeTimeout, Err: fmt.Errorf("waiting for the release of the lease of page %q: %w", title, ctx.Err())}
			}
		}
		l := pg.lease
		if l == nil {
			l = pg.grantLocked(ao, now)
		}
		if l.session == ao.Session {
			if ao.Claim == nil || l.claim != ao.Claim {
				l.shared = true // another command of the session uses the lease
			}
			prev := pg.lastUsed
			l.lastUsed = now
			l.resOwner = ao.Reservation
			pg.lastUsed = now
			if op {
				pg.running++
			}
			e := l.epoch
			pg.mu.Unlock()
			return e, prev, nil
		}
		err := pg.leaseErrLocked(l)
		freed, title := pg.freed, pg.target.title
		pg.mu.Unlock()
		if ao.WaitLease <= 0 {
			return 0, time.Time{}, err
		}
		select {
		case <-freed:
		case <-waitEnd:
			return 0, time.Time{}, err
		case <-ctx.Done():
			return 0, time.Time{}, &CodedError{Code: CodeTimeout, Err: fmt.Errorf("waiting for the lease of page %q: %w", title, ctx.Err())}
		}
	}
}

// ---- operations ----

// HoldLease keeps the lease of ao's session on pg from the idle TTL until the
// returned function is called (design 2.6 heartbeat: a client such as a
// CDP attachment or a CLI command that follows events keeps an event stream
// open, and the daemon renews its lease while the stream lives). It holds nothing while another session leases
// the page. The TTL counts again from the end of the last hold.
func (m *Manager) HoldLease(pg *Page, ao AcquireOptions) func() {
	session := ao.normalized().Session
	pg.mu.Lock()
	if pg.holders == nil {
		pg.holders = map[string]int{}
	}
	pg.holders[session]++
	pg.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			now := time.Now()
			pg.mu.Lock()
			defer pg.mu.Unlock()
			if pg.holders[session]--; pg.holders[session] <= 0 {
				delete(pg.holders, session)
			}
			if l := pg.lease; l != nil && l.session == session {
				l.lastUsed = now
				l.shared = true
			}
		})
	}
}

// Touch resolves and attaches the page and acquires (or renews) the lease
// for the caller's session, without opening an operation.
func (m *Manager) Touch(ctx context.Context, req PageRequest, ao AcquireOptions) (*Page, uint64, error) {
	pg, epoch, _, err := m.touch(ctx, req, ao.normalized(), false)
	return pg, epoch, err
}

func (m *Manager) touch(ctx context.Context, req PageRequest, ao AcquireOptions, op bool) (*Page, uint64, time.Time, error) {
	// The session's current page and tab ids (pages_w3b.go, design 3.8).
	sreq := m.sessionPage(ao.Session, req)
	t, err := m.resolve(ctx, sreq)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	// The simulator's turns, also on a page that the daemon is attached
	// to and on a lease that the session holds (simulator_turns.go). It
	// runs before the observer, which streams the page's key and title to
	// the client: a refused owner learns neither.
	if err := m.admitSimulator(ctx, sreq, t, ao.Reservation); err != nil {
		return nil, 0, time.Time{}, err
	}
	if observe, ok := ctx.Value(pageObserverKey{}).(func(key, title string)); ok {
		observe(t.key, t.title)
	}
	if sreq.Page == "" {
		m.setDefaultPage(ao.Session, sreq, t.key)
	}
	for {
		pg, err := m.page(t)
		if err != nil {
			return nil, 0, time.Time{}, err
		}
		err = m.ensureAttached(ctx, pg, t, ao)
		var epoch uint64
		var prev time.Time
		if err == nil {
			epoch, prev, err = m.acquire(ctx, pg, ao, op)
		}
		if errors.Is(err, errPageRemoved) {
			continue // the sweeper closed the record meanwhile: look the page up again
		}
		if err != nil {
			return nil, 0, time.Time{}, err
		}
		m.leaseStarted(ctx, pg, ao.Session, epoch)
		return pg, epoch, prev, nil
	}
}

type pageObserverKey struct{}

// WithPageObserver returns ctx with observe. An operation begun under the
// returned context (Begin, Do) calls observe with the key and title of
// its page as soon as it resolved the page, before the attach, the lease
// and the scheduler's queue, so that the caller can name the page in an
// error of those steps (the CLI's deadline message, design 2.6).
func WithPageObserver(ctx context.Context, observe func(key, title string)) context.Context {
	return context.WithValue(ctx, pageObserverKey{}, observe)
}

// errPageRemoved: the sweeper closed the page record and removed it from
// the manager (closeIdle) after the command found it; the command looks
// the page up again (touch).
var errPageRemoved = errors.New("the idle page record was closed")

// LeaseEnded returns a channel that is closed when the lease of session
// with epoch on pg ends: release, lease break, idle expiry or takeover. It
// fails when that lease does not hold the page. Event streams bind to it
// (design 2.5: nothing of a session outlives its lease).
func (m *Manager) LeaseEnded(pg *Page, session string, epoch uint64) (<-chan struct{}, error) {
	session = AcquireOptions{Session: session}.normalized().Session
	pg.mu.Lock()
	defer pg.mu.Unlock()
	l := pg.lease
	if l == nil || l.session != session || l.epoch != epoch || pg.releasing {
		return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("%w: the lease of session %q on page %q ended", ErrLeased, session, pg.target.title)}
	}
	l.shared = true // a stream of the session is bound to the lease
	return pg.freed, nil
}

// Begin opens an operation of class on the selected page for the caller's
// session (design 2.10): resolve, preflight and attach, acquire the lease,
// then queue in the page's scheduler. ctx must have the command deadline.
func (m *Manager) Begin(ctx context.Context, req PageRequest, ao AcquireOptions, name string, class Class) (*ManagedOp, error) {
	ao = ao.normalized()
	pg, epoch, prev, err := m.touch(ctx, req, ao, true)
	if err != nil {
		return nil, err
	}
	notRunning := func() {
		pg.mu.Lock()
		pg.running--
		pg.mu.Unlock()
	}
	if idle := time.Since(prev); !prev.IsZero() && idle > m.o.IdleProbe && pg.targetNow().physical {
		// Design 2.6 step 4: after 20 s idle the probe runs before the
		// command; a page that does not answer gets nothing sent.
		if perr := pg.conn.probeNow(ctx); perr != nil {
			notRunning()
			if errors.Is(perr, errProbeCanceled) {
				return nil, fmt.Errorf("%s was not sent: %w", name, perr)
			}
			if errors.Is(perr, ErrLost) {
				// The probe found the connection ended by a peer: the page
				// is lost, not stalled, and no reconnect runs (probe).
				return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("page %q: the connection ended during the liveness probe after %s idle, so %s was not sent: %w", pg.Title(), idle.Round(time.Second), name, perr)}
			}
			return nil, &CodedError{Code: CodeBusy, Err: logcause.Errorf("%w: page %q did not answer the liveness probe after %s idle, so %s was not sent (%v)", webkit.ErrSessionStalled, pg.Title(), idle.Round(time.Second), name, perr)}
		}
	}
	if err := m.recoverSetup(ctx, pg); err != nil {
		notRunning()
		return nil, fmt.Errorf("%s was not sent: %w", name, err)
	}
	op, err := pg.PS.Sched.Begin(ctx, OpSpec{Name: name, Class: class, Session: ao.Session, Epoch: epoch})
	if err != nil {
		notRunning()
		if errors.Is(err, ErrDeadline) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &CodedError{Code: CodeTimeout, Err: err}
		}
		return nil, err
	}
	m.mu.Lock()
	m.nextOp++
	mo := &ManagedOp{ID: fmt.Sprintf("o%d", m.nextOp), Op: op, Page: pg, Session: ao.Session, Epoch: epoch}
	m.ops[mo.ID] = mo
	m.mu.Unlock()
	return mo, nil
}

// End ends a managed operation.
func (m *Manager) End(id string) {
	m.mu.Lock()
	mo := m.ops[id]
	delete(m.ops, id)
	m.mu.Unlock()
	if mo == nil {
		return
	}
	mo.Op.End()
	now := time.Now()
	pg := mo.Page
	pg.mu.Lock()
	pg.running--
	pg.lastUsed = now
	if pg.lease != nil && pg.lease.session == mo.Session {
		pg.lease.lastUsed = now
	}
	pg.mu.Unlock()
}

// RawAs is Raw inside operation mo with owner as the desired-state owner
// of the call, for example a CDP attachment (design 5.2), whose state is
// released alone when it detaches.
func (m *Manager) RawAs(ctx context.Context, mo *ManagedOp, owner Owner, method string, params json.RawMessage) (json.RawMessage, error) {
	pg := mo.Page
	if webkit.HangsThroughTarget(method) && pg.conn.IsTargetRouted() {
		return nil, notSent{fmt.Errorf("%s: %w", method, webkit.ErrHangsThroughTarget)}
	}
	osVersion := pg.targetNow().device.OSVersion
	if known, ok := wip.Supported(osVersion, method); known && !ok {
		return nil, notSent{fmt.Errorf("%s is not in the iOS %s command list, so it was not sent (unknown methods can wedge the page)", method, osVersion)}
	}
	if dl, ok := ctx.Deadline(); ok {
		ctx = webkit.WithCallBudget(ctx, time.Until(dl))
	}
	res, err := legacyCall(ctx, pg.PS, owner, mo.Op.Caller(), method, params)
	if err != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(context.Cause(mo.Op.Context()), ErrDeadline)) {
		// The command deadline ended the call (design 2.6; exit 124, decision D9).
		return nil, &CodedError{Code: CodeTimeout, Err: fmt.Errorf("%w: %w", err, context.DeadlineExceeded)}
	}
	return res, err
}

// notSent marks a gate refusal: the call never reached the page, so
// errors.Is(err, webkit.ErrNotSent) holds (the daemon reports it on the wire).
type notSent struct{ error }

func (e notSent) Unwrap() []error { return []error{e.error, webkit.ErrNotSent} }

// legacyCall maps a legacy call onto the desired state (design 2.4, 2.10).
// A call that changes it runs as one change, which provisional targets
// receive too (DesiredState.Change); its result includes the failures of
// the change's sends to those targets (review finding R6-1).
func legacyCall(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (res json.RawMessage, err error) {
	if !desiredStateMethod(method) {
		return legacyCallOn(ctx, ps, owner, c, method, params)
	}
	err = ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var cerr error
		res, cerr = legacyCallOn(ctx, ps, owner, c, method, params)
		return cerr
	})
	return res, err
}

// legacyCallOn runs a legacy call on c, which is the change's caller when
// the call changes the desired state.
func legacyCallOn(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (json.RawMessage, error) {
	empty := json.RawMessage("{}")
	var p any
	if len(params) > 0 && string(params) != "null" {
		p = params
	}
	dom, verb, _ := strings.Cut(method, ".")
	if scriptProfilerMethod(method) {
		// The page's ScriptProfiler lifecycle (scriptprofiler.go): it runs
		// a pending stop itself, after its admission check.
		return legacyScriptProfiler(ctx, ps, owner, c, method, params)
	}
	if ud := cmp.Or(ruleDomain(method), dom); desiredStateMethod(method) && ps.State.isUnknown(ud) {
		// A release left this agent's state unknown: reset it first
		// (release_lifecycle.go).
		if err := resetAgent(ctx, ps, c, ud); err != nil {
			return nil, err
		}
	}
	switch {
	case verb == "enable":
		if !ps.State.Acquire(owner, dom) {
			return empty, nil // already enabled for another owner or earlier
		}
		res, err := c.Send(ctx, method, p)
		if err != nil {
			releaseFailedAcquire(ps.State, owner, dom, err)
		}
		return res, err
	case verb == "disable":
		return disableDomain(ctx, ps, owner, c, dom, method, p)
	case method == "Network.setExtraHTTPHeaders":
		var v struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(params, &v); err != nil {
			return nil, fmt.Errorf("%s: bad params: %w", method, err)
		}
		if err := acquireNetworkFor(ctx, ps, owner, c, len(v.Headers) > 0); err != nil {
			return nil, err
		}
		ch := ps.State.SetHeaders(owner, v.Headers)
		if ch.NeedsNetworkReset {
			return empty, NetworkReset(ctx, ps.Sched, c, ps.State)
		}
		if ch.Send != nil {
			return c.Send(ctx, method, map[string]any{"headers": ch.Send})
		}
		return empty, nil
	case method == "Network.addInterception" || method == "Network.removeInterception":
		var r InterceptRule
		if err := json.Unmarshal(params, &r); err != nil {
			return nil, fmt.Errorf("%s: bad params: %w", method, err)
		}
		if method == "Network.addInterception" {
			if err := acquireNetworkFor(ctx, ps, owner, c, true); err != nil {
				return nil, err
			}
			ps.State.AddInterception(owner, r)
		}
		// held: the removal takes away one copy of owner's rule r, which
		// owner may not hold at all.
		held := false
		if method == "Network.removeInterception" {
			held = slices.Contains(ps.State.InterceptRules()[owner], r)
			ps.State.RemoveInterception(owner, r)
		}
		res, err := c.Send(ctx, method, p)
		if err != nil && noEffect(err) {
			// WebKit's rules did not change: owner's record goes back to
			// what it was, so that a retry sends the call again and a
			// release still removes a rule whose removal failed (W5c-1a
			// review F9, F13). A removal refused because the target does
			// not hold the rule reached its goal: the record stays
			// removed.
			switch {
			case method == "Network.addInterception":
				ps.State.RemoveInterception(owner, r)
			case held && !MissingInterceptRule(err):
				ps.State.AddInterception(owner, r)
			}
		}
		return res, err
	case method == "Page.setBootstrapScript":
		var v struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(params, &v); err != nil && len(params) > 0 {
			return nil, fmt.Errorf("%s: bad params: %w", method, err)
		}
		// The parts are a requirement of Page (DesiredState.Needs): the
		// first one enables Page, and removing the last requirement
		// disables it.
		part := string(owner)
		if v.Source == "" {
			last := ps.State.RemovePart(part)
			res, err := c.Send(ctx, method, map[string]string{"source": ps.State.Boot.Script()})
			if last {
				err = errors.Join(err, sendDisable(ctx, c, "Page.disable", nil))
			}
			return res, err
		}
		first := !ps.State.Needs("Page")
		if err := ps.State.SetPart(owner, part, RankInit, v.Source); err != nil {
			return nil, err
		}
		if first {
			if _, err := c.Send(ctx, "Page.enable", nil); err != nil {
				if noEffect(err) {
					ps.State.RemovePart(part) // nothing else needed Page, so no part existed before
				}
				return nil, err
			}
		}
		return c.Send(ctx, method, map[string]string{"source": ps.State.Boot.Script()})
	}
	if res, ok, err := legacyStateCall(ctx, ps, owner, c, method, params); ok {
		return res, err
	}
	return c.Send(ctx, method, p)
}

// ---- release ----

// Release ends session's lease on the page key (design 2.5): the release
// barrier, then the cleanup of every desired-state owner of the session.
func (m *Manager) Release(ctx context.Context, session, key string) error {
	pg := m.lookup(key)
	if pg == nil {
		return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no attached page %q", ErrNoPage, key)}
	}
	pg.mu.Lock()
	l := pg.lease
	pg.mu.Unlock()
	if l == nil || l.session != session {
		return nil // nothing of this session to release
	}
	return m.releaseLease(ctx, pg, l)
}

// ReleaseClaimed is Release of the lease on the page of key that an
// acquisition with claim granted (AcquireOptions.Claim), only while that
// lease is the caller's alone: nothing else of the session used it since
// the grant (LeaseClaim, lease.shared) and no operation runs or is queued
// on the page, and no event stream of the session holds it. The decision
// is taken under the page's lock, which every grant and every use takes,
// and the release is claimed in the same critical section, so a command
// that comes later waits for the release and gets a new lease, which
// stays. A lease that ended or was granted again stays too. It returns nil
// when it has nothing to end.
func (m *Manager) ReleaseClaimed(ctx context.Context, claim *LeaseClaim, key string) error {
	l := claim.lease(key)
	pg := m.lookup(key)
	if l == nil || pg == nil {
		return nil // nothing granted there, or the record and its lease ended
	}
	return m.releaseLeaseIf(ctx, pg, l, func() bool {
		return !l.shared && pg.running == 0 && pg.holders[l.session] == 0
	})
}

// ReleaseSession releases every lease of session.
func (m *Manager) ReleaseSession(ctx context.Context, session string) (int, error) {
	n := 0
	var firstErr error
	for _, pg := range m.allPages() {
		pg.mu.Lock()
		l := pg.lease
		pg.mu.Unlock()
		if l != nil && l.session == session {
			n++
			if err := m.releaseLease(ctx, pg, l); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return n, firstErr
}

// Break ends the lease of key whoever holds it (lease break).
func (m *Manager) Break(ctx context.Context, key string) error {
	pg := m.lookup(key)
	if pg == nil {
		return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no attached page %q", ErrNoPage, key)}
	}
	pg.mu.Lock()
	l := pg.lease
	pg.mu.Unlock()
	if l == nil {
		return nil
	}
	return m.releaseLease(ctx, pg, l)
}

// releaseLease claims the release of l and runs it. When another release
// of l runs, it waits for that one (at most until ctx ends) instead.
func (m *Manager) releaseLease(ctx context.Context, pg *Page, l *lease) error {
	return m.releaseLeaseIf(ctx, pg, l, nil)
}

// releaseLeaseIf is releaseLease, only when may, called under pg.mu with
// l still the page's lease, is nil or allows it.
func (m *Manager) releaseLeaseIf(ctx context.Context, pg *Page, l *lease, may func() bool) error {
	pg.mu.Lock()
	if pg.lease != l || (may != nil && !may()) {
		pg.mu.Unlock()
		return nil
	}
	if pg.releasing {
		freed := pg.freed
		pg.mu.Unlock()
		select {
		case <-freed:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("waiting for the release of page %q: %w", pg.Title(), ctx.Err())
		}
	}
	pg.releasing = true
	m.lifecycle("release-begin", pg, l)
	pg.mu.Unlock()
	return m.releasePage(ctx, pg, l)
}

// releasePage releases l, which the caller claimed (pg.releasing).
func (m *Manager) releasePage(ctx context.Context, pg *Page, l *lease) error {
	// Release and lease break cancel the running operations (design 2.10);
	// their clients' later sends find the operation closed.
	m.mu.Lock()
	var open []string
	for id, mo := range m.ops {
		if mo.Page == pg {
			open = append(open, id)
		}
	}
	m.mu.Unlock()
	for _, id := range open {
		m.End(id)
	}
	var err error
	var failed []string // domains whose cleanup send failed
	live := pg.conn.State() == ConnOK
	if live {
		err = pg.PS.Sched.Release(ctx, func(ctx context.Context, c webkit.Caller) error {
			// The required cleanup first: the best-effort runtime release
			// gets only what is left of the deadline after it.
			var err error
			failed, err = releaseOwners(ctx, pg.PS, l.session, c)
			m.releaseRuntime(ctx, pg, l, c)
			return err
		})
	} else {
		// Design 2.4: no reconnect just to clean up; the page-side cleanup
		// stays pending and the page may keep page-side overrides until it
		// navigates. A connection that is not attached is never used
		// again: the next one's setup resets every agent it does not know
		// (resetCalls). The scheduler lease still ends.
		_ = pg.PS.Sched.Release(ctx, nil)
		m.pageLogf(pg.targetNow(), "lease of %q released without page-side cleanup (pending); the connection is %s", logsafe.ID(l.session), pg.conn.State())
	}
	// The session's owners leave the desired state before the lease ends,
	// whatever happened above (a cleanup that could not take the change
	// lock before the deadline, a failure, no connection), so no later
	// setup (takeover, reopen, recovery, a provisional target, the context
	// replay) applies them again (release_lifecycle.go). A cleanup that
	// was not sent or failed stays pending until a reset succeeds.
	if pg.PS.State.forgetSession(l.session, live, failed) {
		m.pageLogf(pg.targetNow(), "lease of %q released; its page-side cleanup was not sent or failed (%v) and is pending", logsafe.ID(l.session), logsafe.Error(err))
		m.resetUnknownAgents(pg)
	}
	if live {
		m.recoverSetupLater(pg)
	}
	pg.mu.Lock()
	if pg.lease == l {
		pg.lease = nil
	}
	pg.releasing = false
	m.lifecycle("release-end", pg, l)
	close(pg.freed)
	pg.freed = make(chan struct{})
	pg.mu.Unlock()
	return err
}

// lifecycleEvent is an event of lifecycleHook: kind is "grant",
// "install", "release-begin" or "release-end"; l is the lease granted or
// released (nil for an install).
type lifecycleEvent struct {
	kind string
	pg   *Page
	l    *lease
}

// lifecycle calls lifecycleHook. The caller holds pg.mu.
func (m *Manager) lifecycle(kind string, pg *Page, l *lease) {
	if m.lifecycleHook != nil {
		m.lifecycleHook(lifecycleEvent{kind: kind, pg: pg, l: l})
	}
}

// Budget of the page-side runtime release of a lease (releaseRuntime). It
// is best effort and runs after the required desired-state cleanup, so a
// page that does not answer it must not cost the release its deadline.
const (
	// runtimeReleaseBudget is the longest wait for the page's answer.
	runtimeReleaseBudget = time.Second
	// runtimeReleaseReserve is the part of the remaining deadline that the
	// runtime release never uses: the release still ends the lease, and
	// its reply must reach the client before the client's own deadline.
	// The daemon runs the release under the client's own absolute
	// deadline (daemon.RunRequest.Deadline), and the reply needs its
	// transit time before that deadline. Transit and reply take well
	// under a millisecond on the local socket; 50 ms keeps that true on a
	// loaded host (the race detector, a busy CI machine) and is small
	// against the 1 s budget.
	runtimeReleaseReserve = 50 * time.Millisecond
	// runtimeReleaseMin is the shortest wait worth a send: a runtime call
	// through the daemon takes milliseconds even on a local page (warm
	// eval p50 13.8 ms through the CLI, docs/notes/w3a-wiring.md). With
	// less time left the page could not answer anyway, so the release is
	// skipped and logged.
	runtimeReleaseMin = 10 * time.Millisecond
)

// releaseRuntime runs the page-side release of l's epoch on the release
// barrier's control caller c (design 2.4, row "Page-side runtime state"):
// the page drops the owner's ref maps and restores its hooks. It waits at
// most runtimeReleaseBudget, and at most the time left before ctx's
// deadline minus runtimeReleaseReserve; when that is below
// runtimeReleaseMin it sends nothing. A skip or a failure is logged and
// does not fail the release: the next owner's first runtime call resets
// the page maps anyway (design 3.1 owner epoch). The release runs in the
// main context and then in every frame context that operations of l's
// epoch selected (Scheduler.ReleaseExecutionContexts); a frame context
// whose release failed or was skipped is released at the next owner's
// first runtime command (commands.openRuntime).
func (m *Manager) releaseRuntime(ctx context.Context, pg *Page, l *lease, c webkit.Caller) {
	budget := runtimeReleaseBudget
	if dl, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(dl)-runtimeReleaseReserve)
	}
	if budget < runtimeReleaseMin {
		m.pageLogf(pg.targetNow(), "page-side runtime release of %q (epoch %d) skipped: %v left for it; the next owner resets the page maps",
			logsafe.ID(l.session), l.epoch, max(budget, 0))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := pg.PS.Runtime.Release(ctx, c, l.epoch); err != nil {
		m.pageLogf(pg.targetNow(), "page-side runtime release of %q (epoch %d): %v", logsafe.ID(l.session), l.epoch, logsafe.Error(err))
	}
	release := func(ctx context.Context, c webkit.Caller) error { return pg.PS.Runtime.Release(ctx, c, l.epoch) }
	if err := pg.PS.ReleaseFrameContexts(ctx, l.epoch, l.session, c, release); err != nil {
		m.pageLogf(pg.targetNow(), "page-side runtime release of %q (epoch %d) in frames: %v", logsafe.ID(l.session), l.epoch, logsafe.Error(err))
	}
}

// releaseOwners releases every owner of session in one change through c
// and returns the first failure with the domains, sorted, whose cleanup
// send failed (ReleaseResult.Failed; Network for a failed Network reset).
// The owners leave the model whatever fails; the caller keeps the failed
// domains' cleanup pending (forgetSession).
func releaseOwners(ctx context.Context, ps *PageSession, session string, c webkit.Caller) (failed []string, err error) {
	ctx = asCleanup(ctx) // a forwarded send that fails stays pending too
	cs, end, err := ps.State.beginChange(ctx, c)
	if err != nil {
		return nil, err
	}
	defer func() { err = end(err) }()
	c = cs
	var firstErr error
	for _, o := range ps.State.AllOwners() {
		if !strings.HasSuffix(string(o), ":"+session) {
			continue
		}
		res, err := ps.State.Release(ctx, o, c)
		failed = append(failed, res.Failed...)
		if err == nil && res.NeedsNetworkReset {
			if err = NetworkReset(ctx, ps.Sched, c, ps.State); err != nil {
				failed = append(failed, "Network")
			}
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	slices.Sort(failed)
	return slices.Compact(failed), firstErr
}

// resetUnknownAgents resets, in the background, the agents whose state a
// release left unknown on the live connection (release_lifecycle.go):
// once the change lock is free, inside a barrier (design 2.10), as one
// change through the control lane. It is bounded by forwardBudget; a
// failure is logged, and the agents stay unknown for the next change or
// setup that uses them.
func (m *Manager) resetUnknownAgents(pg *Page) { m.resetUnknownLater(pg, nil) }

// resetUnknownLater is resetUnknownAgents; hold, when not nil (tests
// only), runs first with the reset's context.
func (m *Manager) resetUnknownLater(pg *Page, hold func(ctx context.Context)) {
	m.goBackground(func(bg context.Context) {
		ctx, cancel := context.WithTimeout(bg, forwardBudget)
		defer cancel()
		if hold != nil {
			hold(ctx)
		}
		if err := resetUnknown(ctx, pg); err != nil {
			m.pageLogf(pg.targetNow(), "resetting the agents whose cleanup is pending: %v", logsafe.Error(err))
		}
	})
}

// recoverSetup applies the desired state again to the page's active target
// when its provisional setup failed (webkit.SetupError, design 2.7): the
// target lacks part of the state, and the session refuses every gated send
// to it until a setup succeeds (Session.ReapplySetup). It runs before an
// operation is queued (Manager.Begin) and after a lease release
// (recoverSetupLater), inside a barrier (design 2.10: the running
// operations end first; their sends to the target are refused anyway) and
// as one change, so no change interleaves with the setup's snapshot. Apply
// runs it as an active Network reset (applyResetting). One attempt per
// call: a refusal is returned with what to do, and the next Begin tries
// again. It returns nil when the active target's setup did not fail.
func (m *Manager) recoverSetup(ctx context.Context, pg *Page) (err error) {
	if !setupFailed(ctx, pg.conn) {
		return nil
	}
	reopen, err := pg.PS.Sched.Barrier(ctx)
	if err != nil {
		return setupRecoveryError(pg, err)
	}
	defer reopen()
	_, end, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		return setupRecoveryError(pg, err)
	}
	defer func() { err = end(err) }()
	if !setupFailed(ctx, pg.conn) {
		return nil // a recovery that held the change before this one succeeded
	}
	if err := pg.conn.ReapplySetup(ctx); err != nil {
		return setupRecoveryError(pg, err)
	}
	m.pageLogf(pg.targetNow(), "the page state was applied again after a failed setup")
	return nil
}

// setupFailed reports whether the provisional setup of c's active target
// failed (webkit.SetupError). A setup that still runs is waited for, at
// most until ctx ends; then the scheduler's setup gate reports it.
func setupFailed(ctx context.Context, c *pageConn) bool {
	return errors.As(c.ActiveSetup(ctx), new(*webkit.SetupError))
}

// setupRecoveryError is a failed recovery of a failed setup
// (recoverSetup): a timeout (exit 124) when the deadline ended it.
func setupRecoveryError(pg *Page, err error) error {
	err = fmt.Errorf("applying the page state again after a failed setup of page %q: %w; retry the command: the next command and a lease release apply the page state again", pg.Title(), err)
	if errors.Is(err, context.DeadlineExceeded) {
		return &CodedError{Code: CodeTimeout, Err: err}
	}
	return err
}

// recoverSetupLater runs recoverSetup in the background after a lease
// release, bounded by forwardBudget: the release changed the desired state
// and must not spend its deadline on a whole setup; the next Begin waits
// for it (the change lock) and tries again when it failed. A failure is
// logged.
func (m *Manager) recoverSetupLater(pg *Page) {
	m.goBackground(func(bg context.Context) {
		ctx, cancel := context.WithTimeout(bg, forwardBudget)
		defer cancel()
		if err := m.recoverSetup(ctx, pg); err != nil {
			m.pageLogf(pg.targetNow(), "%v", logsafe.Error(err))
		}
	})
}

func resetUnknown(ctx context.Context, pg *Page) (err error) {
	reopen, err := pg.PS.Sched.Barrier(ctx)
	if err != nil {
		return err
	}
	defer reopen()
	cs, end, err := pg.PS.State.beginChange(ctx, controlCaller{pg.conn})
	if err != nil {
		return err
	}
	defer func() { err = end(err) }()
	var errs []error
	for _, dom := range pg.PS.State.unknownDomains() {
		errs = append(errs, resetAgent(ctx, pg.PS, cs, dom))
	}
	return errors.Join(errs...)
}

// ---- lookup, events, status ----

func (m *Manager) lookup(key string) *Page {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pg := m.pages[key]; pg != nil && pg.PS != nil {
		return pg
	}
	return nil
}

func (m *Manager) allPages() []*Page {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Page, 0, len(m.pages))
	for _, pg := range m.pages {
		if pg.PS != nil {
			out = append(out, pg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// PageStatus describes one attached page.
type PageStatus struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Device    string    `json:"device"`
	Transport string    `json:"transport"`
	State     string    `json:"state"`
	Session   string    `json:"session,omitempty"`
	Lease     string    `json:"lease"` // none, active or lost
	Epoch     uint64    `json:"epoch,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
}

// Status lists the attached pages.
func (m *Manager) Status() []PageStatus {
	var out []PageStatus
	for _, pg := range m.allPages() {
		st := pg.conn.State()
		pg.mu.Lock()
		ps := PageStatus{
			Key: pg.Key, Title: pg.target.title, URL: redactURL(pg.target.url), Device: pg.target.device.Label,
			Transport: string(pg.target.device.Transport), State: st.String(), Lease: "none",
		}
		if l := pg.lease; l != nil {
			ps.Session, ps.Epoch, ps.Since, ps.LastUsed, ps.Lease = l.session, l.epoch, l.since, l.lastUsed, "active"
			if st == ConnLost {
				ps.Lease = "lost"
			}
		}
		pg.mu.Unlock()
		out = append(out, ps)
	}
	return out
}

// ActiveLeases counts every lease, also the lease of a lost page: a lost
// lease ends only by its idle TTL, release, break or takeover (design 2.5),
// so the daemon neither exits idle (2.3) nor retires for another protocol
// (2.2) while one exists.
func (m *Manager) ActiveLeases() int {
	n := 0
	for _, pg := range m.allPages() {
		pg.mu.Lock()
		if pg.lease != nil {
			n++
		}
		pg.mu.Unlock()
	}
	return n
}

// ---- sweeper and shutdown ----

func (m *Manager) sweeper(ctx context.Context) {
	t := time.NewTicker(m.o.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil {
				return // select picks at random when both are ready
			}
			m.Sweep(time.Now())
		}
	}
}

// Sweep releases leases idle for LeaseTTL (with cleanup), releases the
// leases on the simulator's pages of owners other than the one that
// reserves it, closes pages without a lease after IdleClose, and renews,
// for each active lease, its owner's reservation of the lease's device.
func (m *Manager) Sweep(now time.Time) {
	type ownerDevice struct{ owner, device string }
	renew := map[ownerDevice]bool{}
	simHolder := m.simulatorHolder()
	for _, pg := range m.allPages() {
		// The expiry is claimed under the same lock that acquire renews
		// the lease and counts operations under (design 2.5).
		pg.mu.Lock()
		// The target and the lease's reservation owner change under pg.mu
		// (an attach, acquire): copied here for the reads below.
		l, running, last, t := pg.lease, pg.running, pg.lastUsed, pg.target
		var resOwner string
		if l != nil {
			resOwner = l.resOwner
		}
		expire := l != nil && running == 0 && pg.holders[l.session] == 0 && !pg.releasing && now.Sub(l.lastUsed) >= m.o.LeaseTTL
		// Another owner's turn on the simulator began (sim reserve) while
		// this lease was open, which a run on the free simulator can
		// leave: the lease ends, so that nothing bound to it (an event
		// stream, a CDP attachment) goes on in that owner's turn.
		fence := l != nil && !expire && !pg.releasing && t.device.ID == SimulatorDevice && simHolder != "" && simHolder != resOwner
		if expire || fence {
			pg.releasing = true
		}
		pg.mu.Unlock()
		switch {
		case fence:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.releasePage(ctx, pg, l); err != nil {
				m.pageLogf(t, "simulator lease release: %v", logsafe.Error(err))
			}
			cancel()
			m.pageLogf(t, "lease of %q ended: another owner reserves the simulator", logsafe.ID(l.session))
		case expire:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.releasePage(ctx, pg, l); err != nil {
				m.pageLogf(t, "idle lease release: %v", logsafe.Error(err))
			}
			cancel()
			m.pageLogf(t, "lease of %q expired after %s idle", logsafe.ID(l.session), m.o.LeaseTTL)
		case l != nil:
			// A lease keeps its owner's reservation alive: a physical
			// device's, and the simulator's (sim reserve), whose TTL would
			// otherwise end a turn that works on its page alone.
			if (t.physical || t.device.ID == SimulatorDevice) && pg.conn.State() != ConnLost {
				renew[ownerDevice{resOwner, t.device.ID}] = true
			}
		case running == 0 && now.Sub(last) >= m.o.IdleClose:
			if m.closeIdle(pg, now) {
				m.pageLogf(t, "closed after %s without a lease", m.o.IdleClose)
			}
		}
	}
	if m.o.Reservations != nil {
		for od := range renew {
			if _, err := m.o.Reservations.Renew(od.owner, od.device); err != nil {
				m.o.Logf("renewing the reservation of %q: %v", logsafe.ID(od.owner), logsafe.Error(err))
			}
		}
	}
}

// closeIdle closes pg, a page without a lease that was idle for IdleClose
// at now, and removes its record from the manager. It reports whether it
// did. A command can hold the record that it found before the close: its
// attach or acquisition must not install a connection or a lease on a
// record that the manager no longer lists (review devfix-R1 F4). So
// closeIdle takes attachMu, without waiting: while an attach or a
// recovery of pg runs, pg is in use and a later sweep looks again. Under
// mu it checks again that pg has no lease, operation or release and was
// idle, and marks pg removed; an attach or acquisition that comes later
// returns errPageRemoved, and the command looks the page up again.
func (m *Manager) closeIdle(pg *Page, now time.Time) bool {
	if !pg.attachMu.TryLock() {
		return false
	}
	defer pg.attachMu.Unlock()
	pg.mu.Lock()
	idle := pg.lease == nil && pg.running == 0 && !pg.releasing && now.Sub(pg.lastUsed) >= m.o.IdleClose
	pg.removed = idle
	pg.mu.Unlock()
	if !idle {
		return false
	}
	m.mu.Lock()
	delete(m.pages, pg.Key)
	m.mu.Unlock()
	pg.PS.Sched.Close()
	pg.conn.close()
	return true
}

// Close releases every lease with cleanup within ctx (design 2.3: 5 s in
// total) and closes every page connection. It first cancels the background
// goroutines (recoveries and liveness probes end at once; a sweep that runs
// finishes its lease release, bounded at 5 s) and returns only after every
// one of them ended, so that nothing touches a page or the reservations
// directory after Close.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.bgMu.Lock()
	m.bgClosed = true
	m.bgMu.Unlock()
	m.bgCancel()
	defer m.bg.Wait()
	var firstErr error
	for _, pg := range m.allPages() {
		// pg.mu after bgClosed: a reattach or recovery that checked
		// shuttingDown before has swapped its connection in by now, so
		// pg.conn.close below closes it (shuttingDown).
		pg.mu.Lock()
		l := pg.lease
		pg.mu.Unlock()
		if l != nil {
			if err := m.releaseLease(ctx, pg, l); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		pg.PS.Sched.Close()
		pg.conn.close()
	}
	return firstErr
}

// FindAttached returns the attached page whose key, page id (ws:// URL for
// iwdp) or URL equals sel, or whose title or URL contains sel; nil when
// none matches.
func (m *Manager) FindAttached(sel string) *Page {
	low := strings.ToLower(sel)
	for _, pg := range m.allPages() {
		pg.mu.Lock()
		t := pg.target
		pg.mu.Unlock()
		if sel == pg.Key || sel == t.page.ID || sel == t.url ||
			(low != "" && (strings.Contains(strings.ToLower(t.title), low) || strings.Contains(strings.ToLower(t.url), low))) {
			return pg
		}
	}
	return nil
}

// DaemonID is ManagerOptions.DaemonID.
func (m *Manager) DaemonID() string { return m.o.DaemonID }

// LookupPage returns the attached page key, or nil.
func (m *Manager) LookupPage(key string) *Page { return m.lookup(key) }

// logName names the page of t in the daemon log: its page id (the iwdp
// WebSocket URL, a direct-transport application and page id) without a
// query. The log is a file on disk, so it never holds the page's title or
// URL, which can be private (device check 3, P2-C).
func (t pageTarget) logName() logsafe.ID { return logsafe.ID(redactURL(t.page.ID)) }

// pageLogf logs a line about the page of t as "page <logName>: <line>".
// An error in a is rendered by logsafe.Error, which leaves out the page
// text that errors carry (core's errors name a page by its quoted title).
func (m *Manager) pageLogf(t pageTarget, format string, a ...any) {
	m.o.Logf("page %s: %s", t.logName(), fmt.Sprintf(format, a...))
}

// Title is the page title from the listing (or the URL without query).
func (pg *Page) Title() string {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.target.title
}

// targetNow returns a copy of the page's target. An attach (ensureAttached,
// Manager.page) and a fresh preflight update the target under pg.mu while
// releases, operations and the sweeper run, so every read outside pg.mu
// takes a copy here (or Title).
func (pg *Page) targetNow() pageTarget {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.target
}

// ConnState is the state of the page connection.
func (pg *Page) ConnState() ConnState { return pg.conn.State() }
