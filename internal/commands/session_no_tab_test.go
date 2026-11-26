package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// wantNoSessionTab checks the error of a page command of a session that
// chose no tab on a listing of n tabs: exit 5, the count and how to choose.
func wantNoSessionTab(t *testing.T, what string, err error, n string) {
	t.Helper()
	if ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("%s: %v (exit %d); want exit %d", what, err, ExitCodeOf(err), ExitNoPage)
	}
	for _, want := range []string{"this session has no tab", n + " tab(s)", "agent-ios tab list", "agent-ios tab <id>", "--page <id>", "agent-ios tab new [url]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not say %q", what, err, want)
		}
	}
}

// noLeases fails when any page of m has a lease.
func noLeases(t *testing.T, m *core.Manager, what string) {
	t.Helper()
	for _, p := range m.Status() {
		if p.Lease != "none" {
			t.Fatalf("%s: page %q has lease %q of %q; want none", what, p.Title, p.Lease, p.Session)
		}
	}
}

// User report (shared iPhone, 2026-11-26): a session that chose no tab ran
// its commands on the listing's first tab, another agent's. With two tabs
// and no tab chosen, eval, open and click fail with exit 5 and the way to
// choose a tab; neither page gets a message or a lease.
func TestSessionWithoutTabGetsNoPageOfTwo(t *testing.T) {
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	two := f.add("Two", "http://two.test/")
	for _, argv := range [][]string{
		{"eval", "1+1"},
		{"open", "http://mine.test/"},
		{"click", "#go"},
	} {
		_, err := runIn(t, m, "a", argv...)
		wantNoSessionTab(t, strings.Join(argv, " "), err, "2")
	}
	for name, s := range map[string]*fakewebkit.Server{"One": one, "Two": two} {
		if n, got := s.ConnectionCount(), s.Received(); n != 0 || len(got) != 0 {
			t.Errorf("page %s: %d connections, received %v; want none", name, n, got)
		}
	}
	noLeases(t, m, "after the refused commands")
}

// A page hidden from the caller (a phone it has not reserved) counts: one
// tab and one hidden page are not a single tab (as --only-tab).
func TestSessionWithoutTabGetsNoPageBesideAHiddenOne(t *testing.T) {
	m, f := newTabManagerWith(t, &core.Reservations{Dir: t.TempDir()})
	f.kind, f.device, f.second = transport.KindIWDP, core.SimulatorDevice, "00008020-0000000000000001"
	sim := f.add("Simulator tab", "http://one.test/")
	f.addOn(f.second, "Phone tab", "http://phone.test/")
	_, err := runIn(t, m, "a", "eval", "1")
	wantNoSessionTab(t, "eval", err, "1")
	if !strings.Contains(err.Error(), "1 hidden page(s)") {
		t.Errorf("eval: %v; want the hidden page counted", err)
	}
	if n := sim.ConnectionCount(); n != 0 {
		t.Errorf("the simulator tab got %d connections; want none", n)
	}
}

// With one tab, a session that chose no tab still gets it: eval runs, and
// open and click reach that page.
func TestSessionWithoutTabGetsTheOnlyTab(t *testing.T) {
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	if r, err := runIn(t, m, "a", "eval", "1"); err != nil || r.Text != "1" {
		t.Fatalf("eval with one tab: %q, %v; want 1", r.Text, err)
	}
	for _, argv := range [][]string{{"open", "http://mine.test/"}, {"click", "#go"}} {
		before := len(one.Received())
		// The fake page never loads: only the start of the command counts.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := runInCtx(t, ctx, m, "a", argv...)
		cancel()
		if ExitCodeOf(err) == ExitNoPage {
			t.Fatalf("%v with one tab: %v; want the command run on the tab", argv, err)
		}
		if len(one.Received()) == before {
			t.Fatalf("%v with one tab sent nothing to it (%v)", argv, err)
		}
	}
}

// tab new in a session without a tab, with two tabs: window.open runs in
// one of them only as the opener; the session's page is then the new
// tab, never the opener, and the other tab gets nothing.
func TestTabNewWithoutTabUsesTheOpenerOnlyForWindowOpen(t *testing.T) {
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	two := f.add("Two", "http://two.test/")
	out := mustIn(t, m, "a", "tab", "new", "http://new.test/")
	if !strings.Contains(out, "opened tab t3 (http://new.test/)") {
		t.Fatalf("tab new: %q", out)
	}
	if got := pageOf(t, m, "a"); got != "Opened" {
		t.Fatalf("after tab new session a runs on %q, want the new tab", got)
	}
	for _, r := range one.ReceivedMethod("Runtime.evaluate") {
		if p := string(r.Params); strings.Contains(p, "new.test") && !strings.Contains(p, `!!window.open(\"http://new.test/\")`) {
			t.Errorf("the opener got %s; want only window.open", p)
		}
	}
	if n := two.ConnectionCount(); n != 0 {
		t.Errorf("the other tab got %d connections; want none", n)
	}
	// The opener did not become the session's page: after the new tab
	// closes, the session has no tab again.
	mustIn(t, m, "a", "tab", "close")
	_, err := runIn(t, m, "a", "eval", "1")
	wantNoSessionTab(t, "eval after closing the new tab", err, "2")
}

// tab new without a tab, when the listing right before window.open fails:
// the warning says that the session still has no tab (the opener is not
// its page), and its next command gets no page of the two.
func TestTabNewWithoutTabKeepsNoTabWhenTheListingBeforeOpenFails(t *testing.T) {
	const session = "tab-new-by-order" // the hook below acts only for it
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	f.add("Two", "http://two.test/")
	onWindowOpen(one, func() { f.failing.Store(false) })
	// Lease hooks are global and stay registered for the test binary: the
	// listing fails from the lease that tab new takes on the opener.
	core.OnLeaseStart(func(_ context.Context, _ *core.PageSession, sess string, _ uint64) error {
		if sess == session {
			f.failing.Store(true)
		}
		return nil
	})
	r, err := runInCtx(t, core.WithLeaseHooks(context.Background()), m, session, "tab", "new", "http://next.test/page")
	if err != nil || !strings.Contains(r.Text, "warning: the session still has no tab") || strings.Contains(r.Text, "stays on this page") {
		t.Fatalf("tab new: %q, %v; want the warning that the session has no tab", r.Text, err)
	}
	_, err = runIn(t, m, session, "eval", "1")
	wantNoSessionTab(t, "eval after tab new", err, "2")
}

// The session's choices still apply on a listing of several tabs: its
// default page (the only tab when it ran, while attached), its "tab <id>"
// and --page t2.
func TestSessionChoicesStillSelectAmongSeveralTabs(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("one tab: session a runs on %q", got)
	}
	f.add("Two", "http://two.test/")
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("two tabs: session a runs on %q, want its default page One", got)
	}
	mustIn(t, m, "b", "tab", "list") // hands out the tab ids
	if got, err := pageAt(m, "b", "t2"); err != nil || got != "Two" {
		t.Fatalf("--page t2: %q, %v; want Two", got, err)
	}
	if _, err := pageAt(m, "c", ""); ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("session c without a tab: %v; want exit 5", err)
	}
	mustIn(t, m, "b", "tab", "t2")
	if got := pageOf(t, m, "b"); got != "Two" {
		t.Fatalf("after tab t2 session b runs on %q, want Two", got)
	}
}
