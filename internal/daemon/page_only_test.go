package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// runOnlyTab runs a registry command for session with --only-tab
// (RunRequest.OnlyTab) and the page selection page ("" = none).
func runOnlyTab(t *testing.T, c *Client, session, page string, argv ...string) (RunResult, error) {
	t.Helper()
	return c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: argv, JSON: true, OnlyTab: true, Owner: Owner{Session: session}, PageReq: PageReq{Page: page}}, nil)
}

// issueOnlyTab runs "get cdp-url --json" for session with --only-tab.
func issueOnlyTab(t *testing.T, c *Client, session string) (issued, error) {
	t.Helper()
	res, err := runOnlyTab(t, c, session, "", "get", "cdp-url")
	if err != nil {
		return issued{}, err
	}
	var e issued
	if err := json.Unmarshal(res.Data, &e); err != nil {
		t.Fatalf("get cdp-url --json data %s: %v", res.Data, err)
	}
	return e, nil
}

// --only-tab (core.Manager.OnlyTab) through the daemon: a command runs on
// the one tab of the listing, and with two tabs it is refused with exit 5
// before any page work, in an error that names no page. It applies to a
// page command and to the commands without a page that read --page
// themselves (get cdp-url, tab new, lease takeover).
func TestOnlyTabNamesTheOneListedTab(t *testing.T) {
	two := newTwoPageDaemon(t)
	e, err := issueOnlyTab(t, two.c, "s1")
	if commands.ExitCodeOf(err) != 5 || !strings.Contains(err.Error(), "--only-tab needs a listing of exactly one tab and no hidden page; it shows 2 tab(s) and 0 hidden page(s)") ||
		strings.Contains(err.Error(), "Fixture") || strings.Contains(err.Error(), "Other") {
		t.Fatalf("get cdp-url with --only-tab and two tabs = %+v, %v (exit %d); want exit 5 and the two counts, without a title", e, err, commands.ExitCodeOf(err))
	}
	// A page command, and the other commands without a page that read
	// --page themselves (Command.ReadsPage).
	for _, argv := range [][]string{leaseFixture, {"tab", "new", "https://new.example/"}, {"lease", "takeover"}} {
		if _, err := runOnlyTab(t, two.c, "s1", "", argv...); commands.ExitCodeOf(err) != 5 || !strings.Contains(err.Error(), "it shows 2 tab(s)") {
			t.Fatalf("%v with --only-tab and two tabs: %v (exit %d), want exit 5", argv, err, commands.ExitCodeOf(err))
		}
	}
	if n := two.fixture.ConnectionCount() + two.other.ConnectionCount(); n != 0 {
		t.Fatalf("--only-tab with two tabs opened %d page connection(s), want 0", n)
	}

	fix := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	l.AddDevice("SIMULATOR", "Simulator", "16.7").AddPage("Fixture", "https://fixture.example/", fix.URL)
	one := connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()}))
	mustRunOn(t, one, "s1", "", "cdp", "start", "--port", "0")
	if e, err := issueOnlyTab(t, one, "s1"); err != nil || e.Page != "Fixture" {
		t.Fatalf("get cdp-url with --only-tab and one tab selected %q, %v; want Fixture", e.Page, err)
	}
	if _, err := runOnlyTab(t, one, "s1", "", leaseFixture...); err != nil {
		t.Fatalf("a page command with --only-tab and one tab: %v", err)
	}
}

// A request with --only-tab and --page is a usage error (exit 2) before
// any page work, as the CLI's Parse refuses the line.
func TestOnlyTabWithPageIsAUsageError(t *testing.T) {
	two := newTwoPageDaemon(t)
	for _, argv := range [][]string{leaseFixture, {"get", "cdp-url"}, {"tab", "list"}} {
		_, err := runOnlyTab(t, two.c, "s1", "Fixture", argv...)
		if commands.ExitCodeOf(err) != 2 || !strings.Contains(err.Error(), "--only-tab and --page") {
			t.Fatalf("%v with --only-tab and --page: %v (exit %d), want the usage error", argv, err, commands.ExitCodeOf(err))
		}
	}
	if n := two.fixture.ConnectionCount() + two.other.ConnectionCount(); n != 0 {
		t.Fatalf("--only-tab with --page opened %d page connection(s), want 0", n)
	}
}

// A command without a page that ignores --page (tab list, session info)
// ignores --only-tab too: with two tabs it runs as without the flag.
func TestOnlyTabIsIgnoredWhereThePageIs(t *testing.T) {
	two := newTwoPageDaemon(t)
	for _, argv := range [][]string{{"tab", "list"}, {"session", "info"}} {
		if _, err := runOn(t, two.c, "s1", "no such page", argv...); err != nil {
			t.Fatalf("%v with --page \"no such page\": %v; want it ignored", argv, err)
		}
		if _, err := runOnlyTab(t, two.c, "s1", "", argv...); err != nil {
			t.Fatalf("%v with --only-tab and two tabs: %v; want it ignored", argv, err)
		}
	}
}

// --page only selects a page as at the base commit a3513e07, by title
// or URL substring and by tab label: the word "only" has no meaning of
// its own (--only-tab names the only tab). From the reviewer's repro
// TestReviewOnlyPageStealsAnExistingTitleSelector (finding F26).
func TestPageOnlyIsAnOrdinarySelector(t *testing.T) {
	t.Run("title substring", testPageOnlySelectsByTitle)
	t.Run("tab label", testPageOnlySelectsByLabel)
}

func testPageOnlySelectsByTitle(t *testing.T) {
	f1 := newFake(t, fakewebkit.Options{})
	f2 := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage("Only dashboard", "https://fixture.example/", f1.URL)
	dev.AddPage("Other", "https://other.example/", f2.URL)
	d := &twoPageDaemon{c: connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()}))}
	mustRunOn(t, d.c, "s1", "", "cdp", "start", "--port", "0")
	if got := d.mustIssue(t, "s1", "Only dashboard"); got.Page != "Only dashboard" {
		t.Fatalf("--page \"Only dashboard\" selected %q", got.Page)
	}
	got, err := d.issue(t, "s1", "only")
	if err != nil || got.Page != "Only dashboard" {
		t.Fatalf("--page only (a title substring) with two pages: page %q, %v; want \"Only dashboard\"", got.Page, err)
	}
}

// testPageOnlySelectsByLabel selects the tab labeled "only" (tab new
// --label only) in a listing whose titles and URLs do not contain the
// word.
func testPageOnlySelectsByLabel(t *testing.T) {
	two := newTwoPageDaemon(t)
	opened := newFake(t, fakewebkit.Options{})
	two.fixture.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if strings.HasPrefix(p.Expression, "!!window.open(") {
			two.dev.AddPage("Docs", "https://docs.example/", opened.URL)
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 2}}, nil
	})
	mustRunOn(t, two.c, "s1", "", "tab", "new", "--label", "only", "https://docs.example/")
	if got, err := two.issue(t, "s2", "only"); err != nil || got.Page != "Docs" {
		t.Fatalf("--page only (a tab label) with three pages: page %q, %v; want Docs", got.Page, err)
	}
}

// F42, the reviewer's repro TestReviewOnlyTabTakeoverArgument: "lease
// takeover <ws:// URL>" with --only-tab connected to a page that the
// listing does not show. An argument that names a page beside --only-tab
// is a usage error (exit 2) before any page work, for every command that
// declares one (Arg.Page).
func TestOnlyTabWithAPageArgumentIsAUsageError(t *testing.T) {
	listed := newFake(t, fakewebkit.Options{})
	other := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	l.AddDevice("SIMULATOR", "Simulator", "16.7").AddPage("Listed", "https://fixture.example/", listed.URL)
	c := connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()}))
	for _, argv := range [][]string{
		{"lease", "takeover", other.URL},
		{"lease", "takeover", "Listed"},
		{"lease", "break", "Listed"},
		{"tab", "t1"},
		{"tab", "close", "t1"},
	} {
		_, err := runOnlyTab(t, c, "review", "", argv...)
		if commands.ExitCodeOf(err) != 2 || !strings.Contains(err.Error(), "--only-tab and the <") {
			t.Errorf("%v with --only-tab: %v (exit %d); want the usage error", argv, err, commands.ExitCodeOf(err))
		}
	}
	if n := listed.ConnectionCount() + other.ConnectionCount(); n != 0 {
		t.Fatalf("--only-tab with a page argument opened %d page connection(s), want 0 (other %d)", n, other.ConnectionCount())
	}
	// Without the argument, lease takeover takes the one listed tab.
	if _, err := runOnlyTab(t, c, "review", "", "lease", "takeover"); err != nil || other.ConnectionCount() != 0 {
		t.Fatalf("lease takeover with --only-tab: %v, other page connections %d; want the listed tab", err, other.ConnectionCount())
	}
}

// readsPageSentinel is a --page that matches no page: a command that
// resolves it fails with "no page matches" and repeats it.
const readsPageSentinel = "no-page-matches-this-selector"

// Command.ReadsPage is set by hand for a command without a page (Page
// NoPage) that reads --page itself; the daemon resolves --only-tab only
// for such a command and for page commands. This test runs every other
// command of the daemon with a --page that matches no page: a command
// that resolves --page fails with an error that repeats it, and then
// lacks ReadsPage. The commands with ReadsPage show that the check sees a
// command that reads --page. It cannot see a read of --page that does not
// resolve a page (for example one that only prints it); the commands of
// the registry that read --page all resolve it. Each command runs in a
// new daemon, with an argument "t1" for each required argument, because
// some of them end the session or stop the CDP endpoint.
func TestEveryCommandThatReadsPageDeclaresIt(t *testing.T) {
	var readers []string
	for _, c := range commands.All() {
		if c.Page != commands.NoPage || c.Local {
			continue
		}
		argv := strings.Fields(c.Name)
		for _, a := range c.Args {
			if a.Required {
				argv = append(argv, "t1")
			}
		}
		two := newTwoPageDaemon(t)
		_, err := runOn(t, two.c, "reads", readsPageSentinel, argv...)
		reads := err != nil && strings.Contains(err.Error(), readsPageSentinel)
		switch {
		case reads && !c.ReadsPage:
			t.Errorf("%s resolves --page (%v) but has neither a page nor ReadsPage: the daemon would ignore --only-tab for it", c.Name, err)
		case c.ReadsPage && !reads:
			t.Errorf("%s has ReadsPage, and with a --page that matches no page it returned %v; want the no-page error (is ReadsPage still needed?)", c.Name, err)
		case reads:
			readers = append(readers, c.Name)
		}
	}
	if want := []string{"get cdp-url", "lease takeover", "tab new"}; !slices.Equal(readers, want) {
		t.Errorf("commands without a page that resolve --page: %v; want %v", readers, want)
	}
}
