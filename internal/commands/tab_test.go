package commands

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of tab.go with a daemon manager over fake pages (design 3.8 and
// 7.6 item 11: tab new and tab close with the fake where the browser does
// not list script-opened windows; the hidden-tab probe with the fake).

// tabFake is a local transport whose page list the test changes: each
// page is a fake WebKit endpoint.
type tabFake struct {
	t       *testing.T
	mu      sync.Mutex
	pages   []transport.Page
	servers map[string]*fakewebkit.Server
	n       int
	kind    transport.Kind // "": a local browser (WebKitGTK)
	device  string         // "": "fake"
	failing atomic.Bool    // Pages of the device fails while set
	// second is a second device's id ("": none), listed after; its Pages
	// fails while secondFailing is set.
	second        string
	secondFailing atomic.Bool
}

func (f *tabFake) deviceID() string { return cmp.Or(f.device, "fake") }

func (f *tabFake) Kind() transport.Kind {
	if f.kind != "" {
		return f.kind
	}
	return transport.KindWebKitGTK
}

func (f *tabFake) Devices(context.Context) ([]transport.Device, error) {
	d := []transport.Device{{ID: f.deviceID(), Label: "Fake browser", Transport: f.Kind()}}
	if f.second != "" {
		d = append(d, transport.Device{ID: f.second, Label: "Second fake browser", Transport: f.Kind()})
	}
	return d, nil
}

func (f *tabFake) Pages(_ context.Context, id string) ([]transport.Page, error) {
	if (id == f.second && f.secondFailing.Load()) || (id == f.deviceID() && f.failing.Load()) {
		return nil, errors.New("fake page list failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []transport.Page
	for _, p := range f.pages {
		if p.DeviceID == id {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *tabFake) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	f.mu.Lock()
	s := f.servers[p.ID]
	f.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("page %s is gone", p.ID)
	}
	return webkit.DialWebSocket(ctx, s.URL, transport.Limits{Routed: true})
}
func (f *tabFake) Close() error { return nil }

// add lists a new page served by a new fake endpoint. A page answers
// window.open(url) by listing a new page at url on its own device, and window.close() by
// leaving the list, as a browser that lists script-opened windows does.
func (f *tabFake) add(title, url string) *fakewebkit.Server {
	return f.addOn(f.deviceID(), title, url)
}

// addOn is add on the device id.
func (f *tabFake) addOn(device, title, url string) *fakewebkit.Server {
	s := fakewebkit.New(fakewebkit.Options{})
	f.t.Cleanup(s.Close)
	f.mu.Lock()
	f.n++
	id := fmt.Sprintf("page-%d", f.n)
	f.pages = append(f.pages, transport.Page{DeviceID: device, ID: id, Title: title, URL: url, Debuggable: true})
	if f.servers == nil {
		f.servers = map[string]*fakewebkit.Server{}
	}
	f.servers[id] = s
	f.mu.Unlock()
	s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		switch {
		case strings.HasPrefix(p.Expression, "!!window.open("):
			var u string
			_ = json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(p.Expression, "!!window.open("), ")")), &u)
			f.addOn(device, "Opened", u)
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		case p.Expression == "window.close()":
			f.mu.Lock()
			for i, q := range f.pages {
				if q.ID == id {
					f.pages = append(f.pages[:i], f.pages[i+1:]...)
					break
				}
			}
			f.mu.Unlock()
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	return s
}

// mgrEnv is the daemon's Env for one command (internal/daemon runEnv).
type mgrEnv struct {
	m  *core.Manager
	mo *core.ManagedOp
}

func (e *mgrEnv) Page(context.Context) (*core.PageSession, error) {
	if e.mo == nil {
		return nil, fmt.Errorf("this command runs without a page")
	}
	return e.mo.Page.PS, nil
}

func (e *mgrEnv) Op() *core.Op {
	if e.mo == nil {
		return nil
	}
	return e.mo.Op
}
func (e *mgrEnv) Manager() *core.Manager      { return e.m }
func (e *mgrEnv) Emit(string)                 {}
func (e *mgrEnv) ResolvePath(p string) string { return p }

func newTabManager(t *testing.T) (*core.Manager, *tabFake) {
	return newTabManagerWith(t, nil)
}

// newTabManagerWith is newTabManager with a reservations store.
func newTabManagerWith(t *testing.T, res *core.Reservations) (*core.Manager, *tabFake) {
	t.Helper()
	f := &tabFake{t: t}
	m := core.NewManager(core.ManagerOptions{
		Transport:    func(context.Context, string, string) (transport.Transport, error) { return f, nil },
		Session:      webkit.SessionOptions{TargetWait: time.Second},
		Reservations: res,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m, f
}

// runIn runs a command line as the daemon does (internal/daemon server.run).
func runIn(t *testing.T, m *core.Manager, session string, argv ...string) (Result, error) {
	t.Helper()
	return runInCtx(t, context.Background(), m, session, argv...)
}

// runInCtx is runIn in a context derived from parent: with
// core.WithLeaseHooks, a new lease starts the network and console captures
// of its session, as for a registry command in the daemon.
func runInCtx(t *testing.T, parent context.Context, m *core.Manager, session string, argv ...string) (Result, error) {
	t.Helper()
	inv, err := Parse(argv, func(string) string { return "" })
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	inv.Input.Global.Session = session
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	env := &mgrEnv{m: m}
	if inv.Cmd.Page == NoPage {
		return inv.Cmd.Run(ctx, env, inv.Input)
	}
	var res Result
	class := core.ClassRead
	if inv.Cmd.Mutates {
		class = core.ClassMutate
	}
	err = m.Do(ctx, core.PageRequest{Transport: inv.Input.Global.Transport, Page: inv.Input.Global.Page}, core.AcquireOptions{Session: session, Reservation: inv.Input.Global.Reservation},
		inv.Cmd.Name, class, false, func(ctx context.Context, mo *core.ManagedOp) error {
			env.mo = mo
			var err error
			res, err = inv.Cmd.Run(ctx, env, inv.Input)
			return err
		})
	if err == nil && res.After != nil {
		res.After(ctx, &res)
	}
	return res, err
}

func mustIn(t *testing.T, m *core.Manager, session string, argv ...string) string {
	t.Helper()
	r, err := runIn(t, m, session, argv...)
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return r.Text
}

// pageOf is the title of the page a page command of session runs on.
func pageOf(t *testing.T, m *core.Manager, session string) string {
	t.Helper()
	title, err := pageAt(m, session, "")
	if err != nil {
		t.Fatal(err)
	}
	return title
}

// pageAt is the title of the page a page command of session with
// --page page runs on, or its error.
func pageAt(m *core.Manager, session, page string) (string, error) {
	var title string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Do(ctx, core.PageRequest{Page: page}, core.AcquireOptions{Session: session}, "probe", core.ClassRead, false,
		func(_ context.Context, mo *core.ManagedOp) error { title = mo.Page.Title(); return nil })
	return title, err
}

// Design 3.8: tab list gives stable ids; "tab t2" moves the session's
// current page and its lease; another session is not affected.
func TestTabListAndSelect(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/?q=secret")
	f.add("Two", "http://two.test/")
	out := mustIn(t, m, "a", "tab")
	if out != "  t1  \"One\"  http://one.test/\n  t2  \"Two\"  http://two.test/" {
		t.Fatalf("tab list:\n%s", out)
	}
	// Two tabs and no choice: no page by order (exit 5).
	if got, err := pageAt(m, "a", ""); ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("before tab t2 session a runs on %q (%v), want exit 5", got, err)
	}
	if out := mustIn(t, m, "a", "tab", "t2"); !strings.Contains(out, "tab t2 is now the current page") {
		t.Fatalf("tab t2: %q", out)
	}
	if got := pageOf(t, m, "a"); got != "Two" {
		t.Fatalf("after tab t2 session a runs on %q, want Two", got)
	}
	// The lease moved: session b can take page One now.
	if got, err := pageAt(m, "b", "t1"); err != nil || got != "One" {
		t.Fatalf("session b with --page t1 runs on %q (%v), want One", got, err)
	}
	if out := mustIn(t, m, "a", "tab", "list"); !strings.Contains(out, "* t2") || !strings.Contains(out, "(leased by a)") {
		t.Fatalf("tab list after the move:\n%s", out)
	}
	if _, err := runIn(t, m, "a", "tab", "t7"); err == nil || !strings.Contains(err.Error(), "no tab \"t7\"") {
		t.Fatalf("tab t7: %v, want no tab", err)
	}
}

// Design 2.6 step 4 and 3.8: a tab that accepts commands and never answers
// (a hidden Safari tab) is not selected, and the error says what to do.
func TestTabSelectProbesHiddenTab(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	hidden := f.add("Hidden", "http://hidden.test/")
	hidden.Stall("Runtime.evaluate")
	_, err := runIn(t, m, "a", "tab", "t2")
	if err == nil || !strings.Contains(err.Error(), "tab t2 is not the visible Safari tab; bring it to the front on the device") {
		t.Fatalf("tab t2 on a hidden tab: %v", err)
	}
	if _, ok := m.CurrentPage("a"); ok {
		t.Fatal("the hidden tab became the current page")
	}
}

// Design 3.8: tab new opens a window from the current page, finds it in
// the page list and makes it current (with its label); tab close closes
// it and the session falls back to its default page.
func TestTabNewAndClose(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	out := mustIn(t, m, "a", "tab", "new", "--label", "docs", "http://docs.test/")
	if !strings.Contains(out, "opened tab t2 (http://docs.test/)") {
		t.Fatalf("tab new: %q", out)
	}
	if got := pageOf(t, m, "a"); got != "Opened" {
		t.Fatalf("after tab new session a runs on %q, want the new tab", got)
	}
	list := mustIn(t, m, "a", "tab", "list")
	if !strings.Contains(list, "* t2 [docs]") {
		t.Fatalf("tab list:\n%s", list)
	}
	if out := mustIn(t, m, "a", "tab", "close", "docs"); out != "closed tab docs" {
		t.Fatalf("tab close docs: %q", out)
	}
	if list := mustIn(t, m, "a", "tab", "list"); strings.Contains(list, "t2") {
		t.Fatalf("the closed tab is still listed:\n%s", list)
	}
	if _, ok := m.CurrentPage("a"); ok {
		t.Fatal("the closed tab is still the current page")
	}
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("after closing the current tab session a runs on %q, want One", got)
	}
}

// newTabOpener makes the page served by s answer click --new-tab #link:
// the probe passes, and the gesture call runs gesture, then reports that
// window.open opened http://next.test/page.
func newTabOpener(s *fakewebkit.Server, gesture func()) {
	s.Handle("Runtime.evaluate", nil) // the runtime handler below answers
	p := &fakePage{replies: map[string]any{}}
	p.set("clock", okEnv(map[string]any{"now": 1000, "doc": "doc-1"}))
	p.set("dialogs", okEnv(map[string]any{"doc": "doc-1", "used": []any{}, "pending": []any{}, "withdrawn": []any{}, "on": true}))
	s.SetRuntime(func(c fakewebkit.Call, fn string, args json.RawMessage) any {
		switch fn {
		case "actProbe":
			return attempt("retry", "ready to dispatch", nil)
		case "actGesture":
			gesture()
			return attempt("dispatched", "", map[string]any{"v": map[string]any{"href": "http://next.test/page?q=1#f", "opened": true}})
		}
		return p.handler(c, fn, args)
	})
}

// notFollowed is the warning of click --new-tab when the tab list right
// before the gesture call could not be read (review 2 F9).
const notFollowed = "warning: the session stays on this page: the tab list right before the click could not be read"

// Review 2 F9 (instance 1): the listing right before the gesture call is
// a complete listing of the opener's device. When that device's page list
// fails, also for a local browser, whose failure Manager.Tabs leaves out
// as a device without pages, the click runs and the session stays on the
// opener with a warning; no tab is taken for the new one.
func TestClickNewTabStaysOnTheOpenerWhenItsDeviceListingFails(t *testing.T) {
	m, f := newTabManager(t) // a local browser (WebKitGTK)
	s := f.add("One", "http://one.test/")
	newTabOpener(s, func() {
		f.failing.Store(false)
		f.add("Opened", "http://next.test/page")
	})
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.failing.Store(true) // the listing right before the call fails
	out := mustIn(t, m, "a", "click", "--new-tab", "#link")
	if !strings.Contains(out, notFollowed) || !strings.Contains(out, "fake page list failed") || strings.Contains(out, "the new tab is") {
		t.Fatalf("click --new-tab: %q; want the warning %q with the listing's error, and no new tab", out, notFollowed)
	}
	if !strings.HasPrefix(out, "opened http://next.test/page?q=1#f with window.open") {
		t.Fatalf("click --new-tab: %q; want the click reported as run", out)
	}
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("after click --new-tab session a runs on %q, want the opener One", got)
	}
}

// Review 2 F9 (instance 2): when the listing right before the call fails,
// no tab is guessed by its URL: a tab without a lease that showed the
// link's href before the click is not followed, and neither is the new
// tab; the session stays on the opener with a warning.
func TestClickNewTabTakesNoTabByURLWhenTheListingBeforeTheCallFails(t *testing.T) {
	m, f := newTabManager(t)
	// iwdp: a failed page list of a device with an open page is an error
	// for Manager.Tabs too.
	f.kind, f.device = transport.KindIWDP, "SIMULATOR"
	s := f.add("One", "http://one.test/")
	newTabOpener(s, func() {
		f.failing.Store(false)
		// The browser lists the new tab a moment after window.open.
		time.AfterFunc(400*time.Millisecond, func() { f.add("Opened", "http://next.test/page") })
	})
	// One is the session's default page: the only tab when it ran.
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.add("Old", "http://next.test/page") // opened before the click, no lease
	f.failing.Store(true)
	out := mustIn(t, m, "a", "click", "--new-tab", "#link")
	if !strings.Contains(out, notFollowed) || strings.Contains(out, "the new tab is") {
		t.Fatalf("click --new-tab: %q; want the warning %q and no new tab", out, notFollowed)
	}
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("after click --new-tab session a runs on %q, want the opener One", got)
	}
}

// Review 2 F9: only the opener's device is listed and polled. A second
// device whose page list fails before the call (Manager.Tabs leaves it
// out) and lists a tab afterwards does not give the new tab; the session
// follows the tab that opens on the opener's device.
func TestClickNewTabFollowsOnlyATabOfTheOpenersDevice(t *testing.T) {
	m, f := newTabManager(t)
	f.second = "other"
	s := f.add("One", "http://one.test/")
	newTabOpener(s, func() {
		f.secondFailing.Store(false)
		time.AfterFunc(400*time.Millisecond, func() { f.add("Opened", "http://next.test/page") })
	})
	// One is the session's default page: the only tab when it ran.
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.addOn("other", "Other", "http://other.test/")
	f.secondFailing.Store(true) // the other device's list fails until the call
	out := mustIn(t, m, "a", "click", "--new-tab", "#link")
	if !strings.Contains(out, "(http://next.test/page?q=1#f); it is now the current page") || strings.Contains(out, "warning") {
		t.Fatalf("click --new-tab: %q; want the new tab followed", out)
	}
	if got := pageOf(t, m, "a"); got != "Opened" {
		t.Fatalf("after click --new-tab session a runs on %q, want the new tab Opened", got)
	}
}

// onWindowOpen makes the page served by s answer tab new's window.open
// by running open, then reporting a window; other expressions get 1.
func onWindowOpen(s *fakewebkit.Server, open func()) {
	s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if strings.HasPrefix(p.Expression, "!!window.open(") {
			open()
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
}

// tabNewNotFollowed is the warning of tab new when the tab list right
// before window.open could not be read (review 2 F9, as click --new-tab).
const tabNewNotFollowed = "warning: the session stays on this page: the tab list right before window.open could not be read"

// Review 2 F9 for tab new (instance 1): the listing right before
// window.open is a complete listing of the opener's device. When that
// device's page list fails, also for a local browser, whose failure
// Manager.Tabs leaves out as a device without pages, window.open still
// runs and the session stays on the opener with a warning; the opener is
// not taken for the new tab.
func TestTabNewStaysOnTheOpenerWhenItsDeviceListingFails(t *testing.T) {
	m, f := newTabManager(t) // a local browser (WebKitGTK)
	s := f.add("One", "http://one.test/")
	var opened atomic.Bool
	onWindowOpen(s, func() {
		opened.Store(true)
		f.failing.Store(false)
		f.add("Opened", "http://next.test/page")
	})
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.failing.Store(true) // the listing right before window.open fails
	r, err := runIn(t, m, "a", "tab", "new", "http://next.test/page")
	if err != nil {
		t.Fatalf("tab new: %v; want the warning %q and exit 0 (window.open ran)", err, tabNewNotFollowed)
	}
	if !opened.Load() {
		t.Fatal("tab new did not run window.open")
	}
	if !strings.Contains(r.Text, tabNewNotFollowed) || !strings.Contains(r.Text, "fake page list failed") || strings.Contains(r.Text, "opened tab t") {
		t.Fatalf("tab new: %q; want the warning %q with the listing's error, and no new tab", r.Text, tabNewNotFollowed)
	}
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("after tab new session a runs on %q, want the opener One", got)
	}
}

// Review 2 F9 for tab new (instance 2): when the listing right before
// window.open fails, tab new still opens the tab, and no tab is guessed:
// a tab without a lease that showed the URL before is not followed, and
// neither is the new tab; the session stays on the opener with a warning.
func TestTabNewTakesNoTabWhenTheListingBeforeOpenFails(t *testing.T) {
	m, f := newTabManager(t)
	// iwdp: a failed page list of a device with an open page is an error
	// for Manager.Tabs too.
	f.kind, f.device = transport.KindIWDP, "SIMULATOR"
	s := f.add("One", "http://one.test/")
	var opened atomic.Bool
	onWindowOpen(s, func() {
		opened.Store(true)
		f.failing.Store(false)
		// The browser lists the new tab a moment after window.open.
		time.AfterFunc(400*time.Millisecond, func() { f.add("Opened", "http://next.test/page") })
	})
	// One is the session's default page: the only tab when it ran.
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.add("Old", "http://next.test/page") // opened before tab new, no lease
	f.failing.Store(true)
	r, err := runIn(t, m, "a", "tab", "new", "http://next.test/page")
	if err != nil || !opened.Load() {
		t.Fatalf("tab new: %v (window.open ran: %v); want window.open run and exit 0", err, opened.Load())
	}
	if !strings.Contains(r.Text, tabNewNotFollowed) || strings.Contains(r.Text, "opened tab t") {
		t.Fatalf("tab new: %q; want the warning %q and no new tab", r.Text, tabNewNotFollowed)
	}
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("after tab new session a runs on %q, want the opener One", got)
	}
}

// Review 2 F9 for tab new: when the listing right before window.open
// fails in a session that held no lease on the opener, the session stays
// there without the lease that tab new took only for window.open
// (guide.md, tab new).
func TestTabNewEndsTheBorrowedLeaseWhenTheListingBeforeOpenFails(t *testing.T) {
	const session = "tab-new-borrowed" // the hook below acts only for it
	m, f := newTabManager(t)
	s := f.add("One", "http://one.test/")
	onWindowOpen(s, func() { f.failing.Store(false) })
	// Lease hooks are global and stay registered for the test binary: the
	// listing fails from the lease that tab new takes on the opener.
	core.OnLeaseStart(func(_ context.Context, _ *core.PageSession, sess string, _ uint64) error {
		if sess == session {
			f.failing.Store(true)
		}
		return nil
	})
	r, err := runInCtx(t, core.WithLeaseHooks(context.Background()), m, session, "tab", "new", "http://next.test/page")
	if err != nil || !strings.Contains(r.Text, tabNewNotFollowed) {
		t.Fatalf("tab new: %q, %v; want the warning %q", r.Text, err, tabNewNotFollowed)
	}
	for _, p := range m.Status() {
		if p.Title == "One" && p.Lease != "none" {
			t.Fatalf("the opener's lease after tab new: %q of %q, want none", p.Lease, p.Session)
		}
	}
}

// Review 2 F9 for tab new: only the opener's device is listed and polled.
// A second device whose page list fails before window.open (Manager.Tabs
// leaves it out) and lists a tab afterwards does not give the new tab;
// the session follows the tab that opens on the opener's device.
func TestTabNewFollowsOnlyATabOfTheOpenersDevice(t *testing.T) {
	m, f := newTabManager(t)
	f.second = "other"
	s := f.add("One", "http://one.test/")
	onWindowOpen(s, func() {
		f.secondFailing.Store(false)
		time.AfterFunc(400*time.Millisecond, func() { f.add("Opened", "http://next.test/page") })
	})
	// One is the session's default page: the only tab when it ran.
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("fixture: session a runs on %q", got)
	}
	f.addOn("other", "Other", "http://other.test/")
	f.secondFailing.Store(true) // the other device's list fails until window.open
	out := mustIn(t, m, "a", "tab", "new", "http://next.test/page")
	if !strings.Contains(out, "(http://next.test/page); it is now the current page") || strings.Contains(out, "warning") {
		t.Fatalf("tab new: %q; want the new tab followed", out)
	}
	if got := pageOf(t, m, "a"); got != "Opened" {
		t.Fatalf("after tab new session a runs on %q, want the new tab Opened", got)
	}
}

// Design 2.5 rule 4 (review finding 1): tab list shows no title or URL of
// a physical device without the caller's reservation; it says how many
// pages it left out and how to reserve the device.
func TestTabListLeavesOutUnreservedDevicePages(t *testing.T) {
	m, f := newTabManager(t)
	f.kind = transport.KindDirect
	f.add("Bank", "https://bank.test/account")
	r, err := runIn(t, m, "a", "tab", "list")
	if err != nil {
		t.Fatal(err)
	}
	want := "No tabs.\n1 page(s) are not listed: those of physical devices without a reservation of owner \"a\" (run 'agent-ios device reserve --owner a' or pass --reservation <owner>), and the simulator's while another owner reserves it (agent-ios sim reserve waits for it)"
	if r.Text != want || strings.Contains(r.Text, "Bank") || r.Data.(map[string]any)["hidden"] != 1 {
		t.Fatalf("tab list of an unreserved device:\n%s\n%v\nwant:\n%s", r.Text, r.Data, want)
	}
}

// Device check 3, P3-G: on iwdp, a closed tab's page connection ends with
// the tab ("websocket: close 1001 (going away)"), so the release after
// "tab close" cannot send its page-side cleanup (here Console.disable of
// the console capture). That is expected for a page that is gone: tab
// close prints no warning for it, and the daemon log keeps a line. A
// cleanup that fails otherwise is still a warning.
func TestTabCloseReportsOnlyACleanupFailureOfALivePage(t *testing.T) {
	for _, c := range []struct {
		name        string
		disable     func(s *fakewebkit.Server) fakewebkit.Handler
		wantWarning bool
	}{
		{"connection gone", func(s *fakewebkit.Server) fakewebkit.Handler {
			return func(fakewebkit.Call) (any, *fakewebkit.Error) {
				s.CloseConnections() // the tab is gone: no reply
				return nil, nil
			}
		}, false},
		{"page answers an error", func(*fakewebkit.Server) fakewebkit.Handler {
			return func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "Console.disable refused"}
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &tabFake{t: t}
			var mu sync.Mutex
			var log []string
			m := core.NewManager(core.ManagerOptions{
				Transport: func(context.Context, string, string) (transport.Transport, error) { return f, nil },
				Session:   webkit.SessionOptions{TargetWait: time.Second},
				Logf: func(format string, a ...any) {
					mu.Lock()
					log = append(log, fmt.Sprintf(format, a...))
					mu.Unlock()
				},
			})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = m.Close(ctx)
			})
			f.add("One", "http://one.test/")
			mustIn(t, m, "a", "tab", "new", "--label", "docs", "http://docs.test/")
			mustIn(t, m, "a", "console") // starts the console capture of the new tab
			f.mu.Lock()
			s := f.servers["page-2"]
			f.mu.Unlock()
			s.Handle("Console.disable", c.disable(s))
			out := mustIn(t, m, "a", "tab", "close", "docs")
			if got := strings.Contains(out, "warning"); got != c.wantWarning {
				t.Fatalf("tab close docs = %q; want a warning: %v", out, c.wantWarning)
			}
			if !strings.HasPrefix(out, "closed tab docs") {
				t.Fatalf("tab close docs = %q, want it to report the closed tab", out)
			}
			if c.wantWarning {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, line := range log {
				if strings.Contains(line, "closed tab") && strings.Contains(line, "Console.disable") {
					return
				}
			}
			t.Fatalf("the daemon log has no line about the failed cleanup of the closed tab: %q", log)
		})
	}
}

// Device check 4, P3-G residue: "tab close" closed the tab, then its page
// listing failed (iwdp F1), so the closed tab's lease stayed. Here the tab
// leaves the list without a tab close, which leaves the same state. The
// next "tab <id>" releases that lease; on iwdp its page connection ended with the tab,
// so the page-side cleanup cannot be sent. That is the case that tab
// close already treats as expected (closedPageGone): no warning, and no
// title of the closed tab in the output; the daemon log keeps a line. A
// cleanup that fails otherwise is still a warning.
func TestTabSelectReportsOnlyACleanupFailureOfALivePage(t *testing.T) {
	for _, c := range []struct {
		name        string
		disable     func(s *fakewebkit.Server) fakewebkit.Handler
		wantWarning bool
	}{
		{"connection gone", func(s *fakewebkit.Server) fakewebkit.Handler {
			return func(fakewebkit.Call) (any, *fakewebkit.Error) {
				s.CloseConnections() // the tab is gone: no reply
				return nil, nil
			}
		}, false},
		{"page answers an error", func(*fakewebkit.Server) fakewebkit.Handler {
			return func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "Console.disable refused"}
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &tabFake{t: t}
			var mu sync.Mutex
			var log []string
			m := core.NewManager(core.ManagerOptions{
				Transport: func(context.Context, string, string) (transport.Transport, error) { return f, nil },
				Session:   webkit.SessionOptions{TargetWait: time.Second},
				Logf: func(format string, a ...any) {
					mu.Lock()
					log = append(log, fmt.Sprintf(format, a...))
					mu.Unlock()
				},
			})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = m.Close(ctx)
			})
			f.add("One", "http://one.test/")
			mustIn(t, m, "a", "tab", "new", "--label", "docs", "http://docs.test/")
			mustIn(t, m, "a", "console") // starts the console capture of the new tab
			f.mu.Lock()
			s := f.servers["page-2"]
			f.pages = f.pages[:1] // the tab closed; its lease stays
			f.mu.Unlock()
			s.Handle("Console.disable", c.disable(s))
			out := mustIn(t, m, "a", "tab", "t1")
			if !strings.HasPrefix(out, "tab t1 is now the current page") {
				t.Fatalf("tab t1 = %q, want the switch", out)
			}
			if got := strings.Contains(out, "warning"); got != c.wantWarning {
				t.Fatalf("tab t1 = %q; want a warning: %v", out, c.wantWarning)
			}
			if c.wantWarning {
				return
			}
			if strings.Contains(out, "Opened") {
				t.Fatalf("tab t1 = %q; it names the closed tab by its title", out)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, line := range log {
				if strings.Contains(line, "page-side cleanup was not sent") && strings.Contains(line, "Console.disable") {
					return
				}
			}
			t.Fatalf("the daemon log has no line about the cleanup that the gone page did not get: %q", log)
		})
	}
}

// Device check 4, rule checks: "tab new" runs window.open in the session's
// current page, which in a new session is the device's first tab, the
// user's own. After tab new that page keeps no lease of the session and no
// capture: the lease that the lease hooks gave it (network log and console
// capture, design 4.2) ends with its cleanup, as "tab <id>" ends the
// session's other leases (design 3.8). Only the daemon's page connection
// stays (docs/guide.md). When tab new fails, a lease that it took only to
// run window.open ends too; a lease that the session held before stays.
func TestTabNewLeavesNoLeaseOrCaptureOnTheOpener(t *testing.T) {
	hooks := core.WithLeaseHooks(context.Background())
	leaseOf := func(m *core.Manager, title string) string {
		for _, p := range m.Status() {
			if p.Title == title {
				return p.Lease + " " + p.Session
			}
		}
		return "not attached"
	}
	// stopped reports whether the page got a disable of domain after its
	// last enable (the capture's cleanup).
	stopped := func(s *fakewebkit.Server, domain string) bool {
		last := map[string]int{}
		for i, r := range s.Received() {
			last[r.Method] = i + 1
		}
		return last[domain+".enable"] < last[domain+".disable"]
	}
	t.Run("success", func(t *testing.T) {
		m, f := newTabManager(t)
		user := f.add("User", "https://user.test/")
		r, err := runInCtx(t, hooks, m, "a", "tab", "new", "http://docs.test/")
		if err != nil || !strings.HasPrefix(r.Text, "opened tab t2") {
			t.Fatalf("tab new: %q, %v", r.Text, err)
		}
		if strings.Contains(r.Text, "warning") {
			t.Fatalf("tab new = %q, want no warning", r.Text)
		}
		if got := leaseOf(m, "User"); got != "none " {
			t.Fatalf("the opener's lease after tab new: %q, want none", got)
		}
		if len(user.ReceivedMethod("Console.enable")) == 0 || len(user.ReceivedMethod("Network.enable")) == 0 {
			t.Fatal("the lease hooks started no capture on the opener; the test does not run as the daemon does")
		}
		for _, d := range []string{"Console", "Network"} {
			if !stopped(user, d) {
				t.Errorf("the opener's %s capture of session a is still on after tab new", d)
			}
		}
		if got := pageOf(t, m, "a"); got != "Opened" {
			t.Fatalf("after tab new session a runs on %q, want the new tab", got)
		}
	})
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("popup blocked, lease held before %v", held), func(t *testing.T) {
			m, f := newTabManager(t)
			user := f.add("User", "https://user.test/")
			if held {
				if _, err := runInCtx(t, hooks, m, "a", "console"); err != nil {
					t.Fatal(err)
				}
			}
			user.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return map[string]any{"result": map[string]any{"type": "boolean", "value": false}}, nil
			})
			if _, err := runInCtx(t, hooks, m, "a", "tab", "new", "http://docs.test/"); err == nil || !strings.Contains(err.Error(), "blocked the popup") {
				t.Fatalf("tab new with a blocked popup: %v", err)
			}
			want := "none "
			if held {
				want = "active a"
			}
			if got := leaseOf(m, "User"); got != want {
				t.Fatalf("the opener's lease after a failed tab new: %q, want %q", got, want)
			}
			if !held {
				for _, d := range []string{"Console", "Network"} {
					if !stopped(user, d) {
						t.Errorf("the opener's %s capture of session a is still on after a failed tab new", d)
					}
				}
			}
		})
	}
	// A cleanup that the opener refuses joins the error of the failed tab
	// new; the lease ends all the same.
	t.Run("popup blocked, cleanup refused", func(t *testing.T) {
		m, f := newTabManager(t)
		user := f.add("User", "https://user.test/")
		user.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			return map[string]any{"result": map[string]any{"type": "boolean", "value": false}}, nil
		})
		var disables atomic.Int32
		user.Handle("Console.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			if disables.Add(1) == 1 {
				return nil, nil // the reset of the connection's setup
			}
			return nil, &fakewebkit.Error{Code: -32000, Message: "Console.disable refused"}
		})
		_, err := runInCtx(t, hooks, m, "a", "tab", "new", "http://docs.test/")
		for _, want := range []string{"blocked the popup", "warning: releasing the lease of the page that ran window.open", "Console.disable refused"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("tab new with a blocked popup and a refused cleanup: %v; want %q in it", err, want)
			}
		}
		if got := leaseOf(m, "User"); got != "none " {
			t.Fatalf("the opener's lease after a failed tab new: %q, want none", got)
		}
	})
}

// Review F2: the operation of tab new can fail after its lease on the
// opener was granted and before the operation is admitted (here the
// command is canceled when the lease starts). That lease, which tab new
// took only to run window.open, ends too.
//
// Whether the canceled operation fails at its admission or in window.open
// is a race in the scheduler, so the test repeats the case until it has
// seen a failed admission (a few tries), and checks every try.
func TestTabNewReleasesALeaseItGotBeforeAFailedAdmission(t *testing.T) {
	const session = "tab-new-admission" // the hook below acts only for it
	var mu sync.Mutex
	var cancelNow context.CancelFunc // the current try's cancel
	// Lease hooks are global and stay registered for the test binary.
	core.OnLeaseStart(func(_ context.Context, _ *core.PageSession, s string, _ uint64) error {
		if s == session {
			mu.Lock()
			cancelNow()
			mu.Unlock()
		}
		return nil
	})
	const tries = 200
	for try := 1; ; try++ {
		m, f := newTabManager(t)
		f.add("User", "https://user.test/")
		parent, cancel := context.WithCancel(core.WithLeaseHooks(context.Background()))
		mu.Lock()
		cancelNow = cancel
		mu.Unlock()
		_, err := runInCtx(t, parent, m, session, "tab", "new", "https://new.test/")
		cancel()
		if err == nil {
			t.Fatal("tab new succeeded; want the cancel")
		}
		for _, p := range m.Status() {
			if p.Session == session {
				t.Fatalf("try %d: a failed tab new (%v) kept the lease it took: %s %s on %q", try, err, p.Lease, p.Session, p.Title)
			}
		}
		if strings.HasPrefix(err.Error(), "operation tab new") {
			return // a failed admission was seen and checked
		}
		if try == tries {
			t.Fatalf("no failed admission in %d tries; the last error: %v", tries, err)
		}
	}
}

// Review F4: a failed tab new ends only the opener lease that it was
// granted. A lease that a concurrent command of the same session took on
// another page meanwhile stays.
func TestFailedTabNewKeepsAConcurrentLeaseOnAnotherPage(t *testing.T) {
	m, f := newTabManager(t)
	user := f.add("User", "https://user.test/")
	f.add("Other", "https://other.test/")
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	user.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if strings.HasPrefix(p.Expression, "!!window.open(") {
			close(entered)
			<-unblock
			return map[string]any{"result": map[string]any{"type": "boolean", "value": false}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	hooks := core.WithLeaseHooks(context.Background())
	// The tab ids t1 and t2 come from a listing, as for a user who ran
	// tab list (tab new lists only inside its operation on the opener).
	mustIn(t, m, "lister", "tab", "list")
	done := make(chan error, 1)
	go func() {
		_, err := runInCtx(t, hooks, m, "concurrent", "tab", "new", "--page", "t1")
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("tab new ended before window.open: %v", err)
	}
	if _, err := runInCtx(t, hooks, m, "concurrent", "console", "--page", "t2"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(unblock) })
	if err := <-done; err == nil || !strings.Contains(err.Error(), "blocked the popup") {
		t.Fatalf("tab new: %v; want the blocked popup", err)
	}
	got := map[string]string{}
	for _, p := range m.Status() {
		got[p.Title] = p.Lease + " " + p.Session
	}
	if got["Other"] != "active concurrent" || got["User"] != "none " {
		t.Fatalf("leases after the failed tab new: %v; want Other kept by the concurrent command and User released", got)
	}
}
