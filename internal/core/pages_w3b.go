package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/edxeth/agent-ios/internal/transport"
)

// Tabs and the per-session current page (design
// docs/design/agentic-parity.md, sections 3.8 and 4.1): "tab" lists the
// pages of the selected device with stable ids t1, t2, ... (kept per page
// key for the daemon's life) and optional labels; "tab <id>" makes a page
// the session's current page, which every later command of the session
// uses unless it names one with --page.

// Tab is one listed page.
type Tab struct {
	ID         string `json:"id"` // t1, t2, ...
	Label      string `json:"label,omitempty"`
	Key        string `json:"key"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Device     string `json:"device"`
	DeviceID   string `json:"deviceId"` // the device's id: a UDID, or SIMULATOR
	Debuggable bool   `json:"debuggable"`
	Current    bool   `json:"current"`            // the session's current page
	LeasedBy   string `json:"leasedBy,omitempty"` // session that holds its lease
}

// tabState is the tab bookkeeping of one Manager.
type tabState struct {
	mu      sync.Mutex
	ids     map[string]int    // page key -> N of tN
	next    int               // last N handed out
	labels  map[string]string // label -> page key
	current map[string]PageRequest
	// defaults holds, per session, the page that its last request without
	// a page and without a current page resolved to (Page is the record
	// key; Transport, with "" as "auto", and Addr are the request's).
	defaults map[string]PageRequest
}

// tabStates holds the tabState of each Manager. It lives here and not in
// Manager so that this feature does not edit manager.go, which parallel
// work changes; a daemon has one Manager, so the entries of managers that
// tests close are the only ones never dropped. Revisit when manager.go is
// free to take a field.
var tabStates sync.Map // *Manager -> *tabState

func (m *Manager) tabs() *tabState {
	if s, ok := tabStates.Load(m); ok {
		return s.(*tabState)
	}
	s, _ := tabStates.LoadOrStore(m, &tabState{ids: map[string]int{}, labels: map[string]string{}, current: map[string]PageRequest{}, defaults: map[string]PageRequest{}})
	return s.(*tabState)
}

var tabIDPattern = regexp.MustCompile(`^t[1-9][0-9]*$`)

// idLocked returns the tab id of key, handing out the next one when key
// has none.
func (s *tabState) idLocked(key string) string {
	n := s.ids[key]
	if n == 0 {
		s.next++
		n = s.next
		s.ids[key] = n
	}
	return "t" + strconv.Itoa(n)
}

// physicalDevice reports whether a device of a transport kind is a
// physical device, which needs a reservation before any page connection
// and before tab discovery shows its pages (design 2.5 rules 1 and 4).
// Simulators and local endpoints (WebKitGTK) are not.
func physicalDevice(kind transport.Kind, deviceID string) bool {
	return (kind == transport.KindIWDP && deviceID != "SIMULATOR") || kind == transport.KindDirect
}

// Tabs lists the pages of req's transport with their tab ids; current and
// LeasedBy are seen from ao's session. Design 2.5 rule 4: the pages of a
// physical device are listed only when ao's reservation owner holds a live
// reservation of that device; the others are left out, without titles or
// URLs, and hidden counts them.
func (m *Manager) Tabs(ctx context.Context, req PageRequest, ao AcquireOptions) (tabs []Tab, hidden int, err error) {
	return m.DeviceTabs(ctx, req, ao, "")
}

// DeviceTabs is Tabs of the pages of one device (its id: SIMULATOR or a
// UDID; "" for every device): the pages of other devices are neither
// listed nor counted as hidden, so that an agent on the simulator is not
// stopped by a phone that it has not reserved, nor the reverse. Only that
// device's page list is read: another device's failure does not matter,
// and its own failure is an error, not a device without pages (review
// F6, F16). A device that the listing does not have is an error
// (CodeNoPage).
func (m *Manager) DeviceTabs(ctx context.Context, req PageRequest, ao AcquireOptions, only string) (tabs []Tab, hidden int, err error) {
	ao = ao.normalized()
	session := ao.Session
	var out []Tab
	admitted := map[string]bool{} // device id -> its pages are listed for ao's reservation owner
	var resErr error
	visit := func(key string, p transport.Page) bool {
		// The key is "<kind>|<device id>|<page id>" (findListed).
		kind, rest, _ := strings.Cut(key, "|")
		device, _, _ := strings.Cut(rest, "|")
		// The reservation rule of the device (device_admission.go): an
		// unreserved physical device's pages, and the simulator's while
		// another owner reserves it (sim reserve), are hidden.
		ok, seen := admitted[device]
		if !seen && resErr == nil {
			err := m.admits(transport.Device{ID: device}, physicalDevice(transport.Kind(kind), device), ao.Reservation)
			if err != nil && !refused(err) {
				resErr = err
			}
			ok = err == nil
			admitted[device] = ok
		}
		if !ok {
			hidden++
			return false
		}
		out = append(out, Tab{Key: key, Title: p.Title, URL: redactURL(p.URL), Debuggable: p.Debuggable, DeviceID: device})
		return false
	}
	if only == "" {
		_, _, err = m.findListed(ctx, req, visit)
	} else {
		err = m.deviceListed(ctx, req, only, visit)
	}
	if err != nil {
		return nil, 0, err
	}
	if resErr != nil {
		return nil, 0, &CodedError{Code: CodeReservation, Err: fmt.Errorf("tab list: reading the device reservations: %w", resErr)}
	}
	s := m.tabs()
	s.mu.Lock()
	cur := s.current[session]
	byKey := map[string]string{}
	for l, k := range s.labels {
		byKey[k] = l
	}
	for i := range out {
		out[i].ID = s.idLocked(out[i].Key)
		out[i].Label = byKey[out[i].Key]
		out[i].Current = cur.Page == out[i].Key
	}
	s.mu.Unlock()
	for i := range out {
		if pg := m.lookup(out[i].Key); pg != nil {
			pg.mu.Lock()
			out[i].Device = pg.target.device.Label
			if l := pg.lease; l != nil {
				out[i].LeasedBy = l.session
			}
			pg.mu.Unlock()
		}
	}
	return out, hidden, nil
}

// OnlyTab returns the page key of the only tab of the listing of req
// (--only-tab; req.Page is ignored), for a caller that may act on a page
// only when no other page can be meant: the listing shows exactly one tab,
// and no page is hidden from ao's reservation owner. Any other listing is
// an error (CodeNoPage) that gives the two counts and names no page.
func (m *Manager) OnlyTab(ctx context.Context, req PageRequest, ao AcquireOptions) (string, error) {
	req.Page = ""
	tabs, hidden, err := m.Tabs(ctx, req, ao)
	if err != nil {
		return "", err
	}
	if !soleTab(tabs, hidden) {
		return "", &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: --only-tab needs a listing of exactly one tab and no hidden page; it shows %d tab(s) and %d hidden page(s)", ErrNoPage, len(tabs), hidden)}
	}
	return tabs[0].Key, nil
}

// soleTab reports whether a listing of tabs, and hidden pages that it
// hides from the caller, can mean only one page: exactly one tab and no
// hidden page (--only-tab, and a session without a page, sessionTarget).
func soleTab(tabs []Tab, hidden int) bool { return len(tabs) == 1 && hidden == 0 }

// Logf writes to the daemon log (ManagerOptions.Logf); commands use it
// for work that ends after their result, such as a cleanup operation.
func (m *Manager) Logf(format string, a ...any) { m.o.Logf(format, a...) }

// TabKey returns the page key of a tab id (tN) or label that this
// manager handed out.
func (m *Manager) TabKey(ref string) (string, bool) {
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.labels[ref]; ok {
		return k, true
	}
	if tabIDPattern.MatchString(ref) {
		n, _ := strconv.Atoi(ref[1:])
		for k, v := range s.ids {
			if v == n {
				return k, true
			}
		}
	}
	return "", false
}

// LabelTab gives the page key a label (tab new --label); a label names one
// page, so an earlier page with the label loses it.
func (m *Manager) LabelTab(key, label string) error {
	if tabIDPattern.MatchString(label) {
		return fmt.Errorf("tab label %q looks like a tab id; pick another label", label)
	}
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[label] = key
	s.idLocked(key)
	return nil
}

// SelectPage makes req (its Page is a page key) the session's current page.
func (m *Manager) SelectPage(session string, req PageRequest) {
	session = AcquireOptions{Session: session}.normalized().Session
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current[session] = req
	s.idLocked(req.Page)
}

// CurrentPage returns the session's current page, if it selected one.
func (m *Manager) CurrentPage(session string) (PageRequest, bool) {
	session = AcquireOptions{Session: session}.normalized().Session
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.current[session]
	return r, ok
}

// ForgetTab drops a closed page: its labels, and the current-page choice
// of every session that selected it. Its tab id is never handed out again.
func (m *Manager) ForgetTab(key string) {
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	for l, k := range s.labels {
		if k == key {
			delete(s.labels, l)
		}
	}
	for sess, r := range s.current {
		if r.Page == key {
			delete(s.current, sess)
		}
	}
	for sess, r := range s.defaults {
		if r.Page == key {
			delete(s.defaults, sess)
		}
	}
}

// setDefaultPage records key as the default page of session for requests
// with req's transport and address and no page (sessionPage).
func (m *Manager) setDefaultPage(session string, req PageRequest, key string) {
	session = AcquireOptions{Session: session}.normalized().Session
	s := m.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaults[session] = PageRequest{Transport: autoKind(req.Transport), Addr: req.Addr, Page: key}
}

// autoKind is a requested transport kind with "" as "auto".
func autoKind(kind string) string {
	if kind == "" {
		return "auto"
	}
	return kind
}

// sessionPage is the page request a command of session resolves: a tab id
// or label in req.Page becomes its page key, and a request that names no
// page gets the session's current page when its transport and address
// agree (an explicit --page always wins). Without such a current page, it
// carries the session's default page (setDefaultPage) for the same
// transport and address, which resolve uses while that page's connection
// is open; otherwise sessionTarget resolves the page only when the listing
// has a single tab.
func (m *Manager) sessionPage(session string, req PageRequest) PageRequest {
	if req.Page != "" {
		if k, ok := m.TabKey(req.Page); ok {
			req.Page = k
		}
		return req
	}
	cur, ok := m.CurrentPage(session)
	auto := func(k string) bool { return k == "" || k == "auto" }
	if ok && (auto(req.Transport) || auto(cur.Transport) || req.Transport == cur.Transport) && (req.Addr == "" || req.Addr == cur.Addr) {
		return cur
	}
	session = AcquireOptions{Session: session}.normalized().Session
	s := m.tabs()
	s.mu.Lock()
	if d, ok := s.defaults[session]; ok && d.Transport == autoKind(req.Transport) && d.Addr == req.Addr {
		req.prefer = d.Page
	}
	s.mu.Unlock()
	return req
}

// sessionTarget resolves and admits the page of a command of ao's session
// (touch, ResolvePage): sessionPage's request sreq, resolved, under the
// reservation rule of the page's device (admitTarget). A request that
// names no page, of a session without a current page and without an open
// default page, resolves a page only when the listing can mean only one
// (soleTab, as --only-tab): a page is never picked by its order, because
// on a shared device the first tab can be another agent's or the user's.
// Any other listing is an error (CodeNoPage) that says how to choose a
// tab, except for req.Opener (tab new's window.open), which then gets the
// listing's first debuggable tab that ao may use. implicit reports that
// the page came from no choice of the session (sreq.Page is ""), and is
// to become its default page; never for an opener picked by order.
func (m *Manager) sessionTarget(ctx context.Context, req PageRequest, ao AcquireOptions) (sreq PageRequest, t pageTarget, implicit bool, err error) {
	sreq = m.sessionPage(ao.Session, req)
	implicit = sreq.Page == ""
	switch {
	case sreq.Page != "":
		t, err = m.resolve(ctx, sreq)
	default:
		var ok bool
		if t, ok = m.attachedTarget(sreq); !ok {
			t, implicit, err = m.soleTarget(ctx, sreq, ao)
		}
	}
	if err != nil {
		return PageRequest{}, pageTarget{}, false, err
	}
	if err := m.admitTarget(ctx, sreq, t, ao.Reservation); err != nil {
		return PageRequest{}, pageTarget{}, false, err
	}
	return sreq, t, implicit, nil
}

// soleTarget is the page of sreq (no page, no open default page) when the
// listing that ao's reservation owner sees has a single tab (soleTab). A
// listing of several tabs is an error (CodeNoPage), except for an opener
// (sreq.Opener), which gets the first debuggable tab, with implicit false.
// When every page is hidden, the error is the admission error of the
// listing's first debuggable page (its device's reservation), which names
// what to reserve; that page is only refused, never used.
func (m *Manager) soleTarget(ctx context.Context, sreq PageRequest, ao AcquireOptions) (t pageTarget, implicit bool, err error) {
	listing := PageRequest{Transport: sreq.Transport, Addr: sreq.Addr}
	tabs, hidden, err := m.Tabs(ctx, listing, ao)
	if err != nil {
		return pageTarget{}, false, err
	}
	if len(tabs) == 0 && hidden > 0 {
		t, err := m.resolve(ctx, listing)
		if err == nil {
			err = m.admitTarget(ctx, sreq, t, ao.Reservation)
		}
		if err == nil {
			err = m.noSessionTab(tabs, hidden, ao)
		}
		return pageTarget{}, false, err
	}
	key := ""
	switch {
	case soleTab(tabs, hidden):
		implicit = true
		if tabs[0].Debuggable {
			key = tabs[0].Key
		}
	case len(tabs) > 0 && !sreq.Opener:
		return pageTarget{}, false, m.noSessionTab(tabs, hidden, ao)
	default:
		for _, tab := range tabs {
			if tab.Debuggable {
				key = tab.Key
				break
			}
		}
	}
	if key == "" {
		return pageTarget{}, false, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no debuggable page among %d listed; open a page in Safari on the device", ErrNoPage, len(tabs)+hidden)}
	}
	listing.Page = key
	if t, ok := m.attachedTarget(listing); ok {
		return t, implicit, nil
	}
	t, _, err = m.findListed(ctx, listing, func(k string, _ transport.Page) bool { return k == key })
	if err == nil && t.key == "" {
		err = &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: the listing no longer has the page %q", ErrNoPage, key)}
	}
	return t, implicit, err
}

// noSessionTab is the error of a command of a session without a tab on a
// listing of several pages (soleTarget). Tabs of the simulator only, in
// the turn of ao's owner, get the simulator's advice (simulatorUnchosen).
func (m *Manager) noSessionTab(tabs []Tab, hidden int, ao AcquireOptions) error {
	simulator := len(tabs) > 1 && hidden == 0
	for _, t := range tabs {
		kind, _, _ := strings.Cut(t.Key, "|")
		simulator = simulator && t.DeviceID == SimulatorDevice && !physicalDevice(transport.Kind(kind), t.DeviceID)
	}
	if simulator {
		if mine, err := m.simulatorTurn(ao.Reservation); err == nil && mine {
			return simulatorUnchosen(len(tabs))
		}
	}
	shown := fmt.Sprintf("%d tab(s)", len(tabs))
	if hidden > 0 {
		shown += fmt.Sprintf(" and %d hidden page(s)", hidden)
	}
	return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: this session has no tab, and the listing shows %s; agent-ios does not pick one by order, because another agent or the user may be using it: run 'agent-ios tab list', then 'agent-ios tab <id>' (or pass --page <id>), or open your own tab with 'agent-ios tab new [url]'", ErrNoPage, shown)}
}

// deviceListed calls visit for each page of the device id in the listing
// of req, with the page key that findListed gives it. The listing of
// devices, a listing without that device, and that device's page list
// failing are errors (CodeNoPage); the other devices' page lists are not
// read.
func (m *Manager) deviceListed(ctx context.Context, req PageRequest, id string, visit func(key string, p transport.Page) bool) error {
	if m.o.Transport == nil {
		return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no transport is configured", ErrNoPage)}
	}
	tr, err := m.o.Transport(ctx, autoKind(req.Transport), req.Addr)
	if err != nil {
		return &CodedError{Code: CodeNoPage, Err: m.listingFailed(err)}
	}
	devs, err := tr.Devices(ctx)
	if err != nil {
		return &CodedError{Code: CodeNoPage, Err: m.listingFailed(&ListingError{Kind: tr.Kind(), Err: fmt.Errorf("listing devices (%s): %w", tr.Kind(), err)})}
	}
	for _, d := range devs {
		if d.ID != id {
			continue
		}
		pages, err := tr.Pages(ctx, d.ID)
		if err != nil {
			return &CodedError{Code: CodeNoPage, Err: m.listingFailed(&ListingError{Kind: tr.Kind(), Err: fmt.Errorf("listing the pages of %s (%s): %w", d.Label, tr.Kind(), err)})}
		}
		for _, p := range pages {
			visit(string(tr.Kind())+"|"+d.ID+"|"+p.ID, p)
		}
		return nil
	}
	return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: %s lists no device %q", ErrNoPage, tr.Kind(), id)}
}
