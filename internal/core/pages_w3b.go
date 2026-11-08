package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
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
	s, _ := tabStates.LoadOrStore(m, &tabState{ids: map[string]int{}, labels: map[string]string{}, current: map[string]PageRequest{}})
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
	ao = ao.normalized()
	session := ao.Session
	var out []Tab
	reserved := map[string]bool{} // physical device id -> ao's reservation covers it
	var resErr error
	_, _, err = m.findListed(ctx, req, func(key string, p transport.Page) bool {
		// The key is "<kind>|<device id>|<page id>" (findListed).
		kind, rest, _ := strings.Cut(key, "|")
		device, _, _ := strings.Cut(rest, "|")
		if physicalDevice(transport.Kind(kind), device) {
			mine, seen := reserved[device]
			if !seen && m.o.Reservations != nil && resErr == nil {
				var r *Reservation
				r, _, resErr = m.o.Reservations.Check(device, ao.Reservation)
				mine = r != nil
				reserved[device] = mine
			}
			if !mine {
				hidden++
				return false
			}
		}
		out = append(out, Tab{Key: key, Title: p.Title, URL: redactURL(p.URL), Debuggable: p.Debuggable})
		return false
	})
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
}

// sessionPage is the page request a command of session resolves: a tab id
// or label in req.Page becomes its page key, and a request that names no
// page gets the session's current page when its transport and address
// agree (an explicit --page always wins).
func (m *Manager) sessionPage(session string, req PageRequest) PageRequest {
	if req.Page != "" {
		if k, ok := m.TabKey(req.Page); ok {
			req.Page = k
		}
		return req
	}
	cur, ok := m.CurrentPage(session)
	if !ok {
		return req
	}
	auto := func(k string) bool { return k == "" || k == "auto" }
	if (auto(req.Transport) || auto(cur.Transport) || req.Transport == cur.Transport) && (req.Addr == "" || req.Addr == cur.Addr) {
		return cur
	}
	return req
}
