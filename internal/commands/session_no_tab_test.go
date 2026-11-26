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

// loseDefaultPage makes session's default page One (the only tab when it
// ran), lists a second tab, and makes One's connection lost (another
// inspector took it): the state of review F1 (tab choice).
func loseDefaultPage(t *testing.T, m *core.Manager, one *fakewebkit.Server, session string) {
	t.Helper()
	one.CloseConnections()
	waitUntil(t, "page One to be lost", func() bool {
		for _, p := range m.Status() {
			if p.Title == "One" && p.Lease == "lost" && p.Session == session {
				return true
			}
		}
		return false
	})
}

// Review F1 (tab choice): a session whose default page is lost while the
// listing shows several tabs still has that page (docs/sessions.md, "When
// a page is lost"): the next command fails with the lost error (exit 4,
// "ownership is unknown"), get cdp-url resolves it, and lease takeover
// without a page and a command with --takeover reattach it; exit 5 is
// only for a session without a page.
func TestLostDefaultPageAmongSeveralTabsStaysTheSessionsPage(t *testing.T) {
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	if got := pageOf(t, m, "a"); got != "One" {
		t.Fatalf("one tab: session a runs on %q", got)
	}
	two := f.add("Two", "http://two.test/")
	loseDefaultPage(t, m, one, "a")
	_, err := runIn(t, m, "a", "eval", "1")
	if ExitCodeOf(err) != ExitBusy || !strings.Contains(err.Error(), "ownership is unknown") {
		t.Fatalf("eval on the lost default page: %v (exit %d); want exit 4, ownership is unknown", err, ExitCodeOf(err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lp, _, err := m.ResolvePage(ctx, "a", "", core.PageRequest{})
	if err != nil || lp.Title != "One" {
		t.Fatalf("get cdp-url's page of session a: %q, %v; want One", lp.Title, err)
	}
	r, err := runIn(t, m, "a", "lease", "takeover")
	if err != nil || !strings.Contains(r.Text, `holds "One" (epoch 2)`) {
		t.Fatalf("lease takeover without a page: %q, %v; want One reattached", r.Text, err)
	}
	loseDefaultPage(t, m, one, "a")
	var title string
	err = m.Do(ctx, core.PageRequest{}, core.AcquireOptions{Session: "a", Takeover: true}, "probe", core.ClassRead, false,
		func(_ context.Context, mo *core.ManagedOp) error { title = mo.Page.Title(); return nil })
	if err != nil || title != "One" {
		t.Fatalf("a command with --takeover: page %q, %v; want One", title, err)
	}
	if n := two.ConnectionCount(); n != 0 {
		t.Errorf("the other tab got %d connections; want none", n)
	}
	// Another session gets no page of the two, and a session whose lost
	// page's lease ended (break) has no page any more.
	if _, err := pageAt(m, "b", ""); ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("session b without a tab: %v; want exit 5", err)
	}
	loseDefaultPage(t, m, one, "a")
	for _, p := range m.Status() {
		if p.Title == "One" {
			if err := m.Break(ctx, p.Key); err != nil {
				t.Fatalf("lease break: %v", err)
			}
		}
	}
	_, err = runIn(t, m, "a", "eval", "1")
	wantNoSessionTab(t, "eval after the lost page's lease was broken", err, "2")
}

// windowOpens counts the window.open calls that page s got.
func windowOpens(s *fakewebkit.Server) int {
	n := 0
	for _, r := range s.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(r.Params), "window.open(") {
			n++
		}
	}
	return n
}

// leaseOf is the session that holds the lease of the page titled title
// ("" for none).
func leaseOf(m *core.Manager, title string) string {
	for _, p := range m.Status() {
		if p.Title == title && p.Lease != "none" {
			return p.Session
		}
	}
	return ""
}

// Review F4 and decision D21 (tab choice): tab new of a session without a
// tab never takes another session's tab as its opener: with the first tab
// leased by session x, window.open runs in the second tab, and the new
// tab becomes the session's page.
func TestTabNewWithoutTabSkipsAnotherSessionsTab(t *testing.T) {
	m, f := newTabManager(t)
	one := f.add("One", "http://one.test/")
	two := f.add("Two", "http://two.test/")
	mustIn(t, m, "x", "tab", "list")
	mustIn(t, m, "x", "tab", "t1")
	out := mustIn(t, m, "y", "tab", "new", "http://mine.test/")
	if !strings.Contains(out, "opened tab t3 (http://mine.test/)") {
		t.Fatalf("tab new: %q", out)
	}
	if got := pageOf(t, m, "y"); got != "Opened" {
		t.Fatalf("after tab new session y runs on %q, want the new tab", got)
	}
	if n, w := windowOpens(one), windowOpens(two); n != 0 || w != 1 {
		t.Fatalf("window.open calls: %d in x's tab, %d in the free tab; want 0 and 1", n, w)
	}
	if got := leaseOf(m, "One"); got != "x" {
		t.Fatalf("the lease of One is %q's; want x's", got)
	}
	if got := leaseOf(m, "Two"); got != "" {
		t.Fatalf("the opener keeps a lease of %q; want none", got)
	}
}

// D21: the opener must answer a short liveness probe, because a hidden
// Safari tab accepts commands and never answers (iOS): tab new skips a
// first tab that does not answer, runs window.open in the next one, and
// keeps no lease on either.
func TestTabNewWithoutTabSkipsATabThatDoesNotAnswer(t *testing.T) {
	m, f := newTabManager(t)
	hidden := f.add("Hidden", "http://hidden.test/")
	hidden.Stall("Runtime.evaluate")
	visible := f.add("Visible", "http://visible.test/")
	out := mustIn(t, m, "a", "tab", "new", "http://mine.test/")
	if !strings.Contains(out, "opened tab t3 (http://mine.test/)") {
		t.Fatalf("tab new: %q", out)
	}
	if got := pageOf(t, m, "a"); got != "Opened" {
		t.Fatalf("after tab new session a runs on %q, want the new tab", got)
	}
	if n, w := windowOpens(hidden), windowOpens(visible); n != 0 || w != 1 {
		t.Fatalf("window.open calls: %d in the hidden tab, %d in the visible one; want 0 and 1", n, w)
	}
	for _, title := range []string{"Hidden", "Visible"} {
		if got := leaseOf(m, title); got != "" {
			t.Fatalf("tab %s keeps a lease of %q; want none", title, got)
		}
	}
	// The probe left the hidden tab's record stalled: another session's
	// tab new passes over it again and does not wait for its recovery.
	out = mustIn(t, m, "b", "tab", "new", "http://other.test/")
	if !strings.Contains(out, "opened tab t4 (http://other.test/)") || windowOpens(hidden) != 0 {
		t.Fatalf("second tab new: %q; window.open calls in the hidden tab: %d", out, windowOpens(hidden))
	}
}

// D21 on a phone (iwdp, a physical device): the first attach of a hidden
// tab fails its own liveness probe (openConn), so tab new runs
// window.open in the next tab, the visible one.
func TestTabNewWithoutTabSkipsAHiddenPhoneTab(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, "00008020-0000000000000001"
	if _, err := res.Reserve(f.device, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	hidden := f.add("Hidden", "http://hidden.test/")
	hidden.Stall("Runtime.evaluate")
	visible := f.add("Visible", "http://visible.test/")
	out := mustIn(t, m, "a", "tab", "new", "http://mine.test/")
	if !strings.Contains(out, "opened tab t3 (http://mine.test/)") {
		t.Fatalf("tab new: %q", out)
	}
	if n, w := windowOpens(hidden), windowOpens(visible); n != 0 || w != 1 {
		t.Fatalf("window.open calls: %d in the hidden tab, %d in the visible one; want 0 and 1", n, w)
	}
	if got := leaseOf(m, "Hidden"); got != "" {
		t.Fatalf("the hidden tab keeps a lease of %q; want none", got)
	}
}

// D21: when no tab can be the opener (each one leased by another session
// or not answering), tab new fails with exit 4 and says why, and the
// session still has no tab.
func TestTabNewWithoutTabFailsWhenNoTabCanOpen(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One", "http://one.test/")
	hidden := f.add("Hidden", "http://hidden.test/")
	hidden.Stall("Runtime.evaluate")
	mustIn(t, m, "x", "tab", "list")
	mustIn(t, m, "x", "tab", "t1")
	_, err := runIn(t, m, "a", "tab", "new", "http://mine.test/")
	if ExitCodeOf(err) != ExitBusy {
		t.Fatalf("tab new: %v (exit %d); want exit 4", err, ExitCodeOf(err))
	}
	for _, want := range []string{"no tab can run window.open", "t1 is leased by session \"x\"", "t2 did not answer", "bring", "front"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("tab new: error %q does not say %q", err, want)
		}
	}
	if got := leaseOf(m, "Hidden"); got != "" {
		t.Fatalf("the hidden tab keeps a lease of %q; want none", got)
	}
	_, err = runIn(t, m, "a", "eval", "1")
	wantNoSessionTab(t, "eval after the failed tab new", err, "2")
}
