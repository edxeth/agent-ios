package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
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
}

func (f *tabFake) Kind() transport.Kind {
	if f.kind != "" {
		return f.kind
	}
	return transport.KindWebKitGTK
}

func (f *tabFake) Devices(context.Context) ([]transport.Device, error) {
	return []transport.Device{{ID: "fake", Label: "Fake browser", Transport: transport.KindWebKitGTK}}, nil
}

func (f *tabFake) Pages(context.Context, string) ([]transport.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transport.Page(nil), f.pages...), nil
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
// window.open(url) by listing a new page at url, and window.close() by
// leaving the list, as a browser that lists script-opened windows does.
func (f *tabFake) add(title, url string) *fakewebkit.Server {
	s := fakewebkit.New(fakewebkit.Options{})
	f.t.Cleanup(s.Close)
	f.mu.Lock()
	f.n++
	id := fmt.Sprintf("page-%d", f.n)
	f.pages = append(f.pages, transport.Page{DeviceID: "fake", ID: id, Title: title, URL: url, Debuggable: true})
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
			f.add("Opened", u)
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
	t.Helper()
	f := &tabFake{t: t}
	m := core.NewManager(core.ManagerOptions{
		Transport: func(context.Context, string, string) (transport.Transport, error) { return f, nil },
		Session:   webkit.SessionOptions{TargetWait: time.Second},
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
	err = m.Do(ctx, core.PageRequest{Transport: inv.Input.Global.Transport, Page: inv.Input.Global.Page}, core.AcquireOptions{Session: session},
		inv.Cmd.Name, class, false, func(ctx context.Context, mo *core.ManagedOp) error {
			env.mo = mo
			var err error
			res, err = inv.Cmd.Run(ctx, env, inv.Input)
			return err
		})
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
	var title string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Do(ctx, core.PageRequest{}, core.AcquireOptions{Session: session}, "probe", core.ClassRead, false,
		func(_ context.Context, mo *core.ManagedOp) error { title = mo.Page.Title(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return title
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
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("before tab t2 session a runs on %q, want One", got)
	}
	if out := mustIn(t, m, "a", "tab", "t2"); !strings.Contains(out, "tab t2 is now the current page") {
		t.Fatalf("tab t2: %q", out)
	}
	if got := pageOf(t, m, "a"); got != "Two" {
		t.Fatalf("after tab t2 session a runs on %q, want Two", got)
	}
	// The lease moved: session b can take page One now.
	if got := pageOf(t, m, "b"); got != "One" {
		t.Fatalf("session b runs on %q, want One (its own default)", got)
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
	want := "No tabs.\n1 page(s) of physical devices without a reservation of owner \"a\" are not listed; run 'iwdp-cli device reserve --owner a' or pass --reservation <owner>"
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
