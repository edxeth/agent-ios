package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design docs/design/agentic-parity.md 5.2: the selectedPage of an
// endpoint record "is the session's current page when the URL is issued,
// resolved like the page of any command (exit 5 when there is none)". So
// get cdp-url selects what every other page command of the session acts
// on: the tab chosen with "tab <id>", else the session's default page,
// else the listing's first debuggable page; --page takes the same forms
// (a tab id, a page key, a title or URL text).

// twoPageDaemon is a daemon whose transport is a fake iwdp listing of one
// simulator (no reservation needed) with the pages "Fixture" (listed
// first) and "Other", and with the CDP endpoint running.
type twoPageDaemon struct {
	c       *Client
	dev     *fakewebkit.ListedDevice
	fixture *fakewebkit.Server
	other   *fakewebkit.Server
	pFix    *fakewebkit.ListedPage
	pOther  *fakewebkit.ListedPage
}

func newTwoPageDaemon(t *testing.T) *twoPageDaemon {
	t.Helper()
	d := &twoPageDaemon{fixture: newFake(t, fakewebkit.Options{}), other: newFake(t, fakewebkit.Options{})}
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	d.dev = l.AddDevice("SIMULATOR", "Simulator", "16.7")
	d.pFix = d.dev.AddPage("Fixture", "https://fixture.example/", d.fixture.URL)
	d.pOther = d.dev.AddPage("Other", "https://other.example/", d.other.URL)
	d.c = connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr()}))
	// Port 0: the endpoint binds a free port itself (get cdp-url keeps a
	// running endpoint).
	mustRunOn(t, d.c, "s1", "", "cdp", "start", "--port", "0")
	return d
}

// issued is "get cdp-url --json" (cdp.Endpoint).
type issued struct {
	WS    string `json:"ws"`
	HTTP  string `json:"http"`
	Scope string `json:"scope"`
	Page  string `json:"page"`
}

// issue runs "get cdp-url --json" for session with the page selection
// page ("" = none).
func (d *twoPageDaemon) issue(t *testing.T, session, page string, extra ...string) (issued, error) {
	t.Helper()
	res, err := d.c.Run(ctxFor(t, 10*time.Second), RunRequest{
		Argv: append([]string{"get", "cdp-url"}, extra...), JSON: true,
		Owner: Owner{Session: session}, PageReq: PageReq{Page: page},
	}, nil)
	if err != nil {
		return issued{}, err
	}
	var e issued
	if err := json.Unmarshal(res.Data, &e); err != nil {
		t.Fatalf("get cdp-url --json data %s: %v", res.Data, err)
	}
	return e, nil
}

func (d *twoPageDaemon) mustIssue(t *testing.T, session, page string, extra ...string) issued {
	t.Helper()
	e, err := d.issue(t, session, page, extra...)
	if err != nil {
		t.Fatalf("get cdp-url for session %s, page %q: %v", session, page, err)
	}
	return e
}

// wantListed checks that the record's /json/list, read through the issued
// URL, lists exactly the page title (scope page).
func wantListed(t *testing.T, e issued, title string) {
	t.Helper()
	code, body := httpGet(t, e.HTTP+"/json/list")
	var list []struct {
		Title string `json:"title"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0].Title != title {
		t.Fatalf("GET <issued http>/json/list: %d %s; want only the page %q", code, body, title)
	}
}

// The tab chosen with "tab t2" is the selected page, and the same
// session, page and scope get the same record again.
func TestGetCDPURLSelectsTheSessionsTab(t *testing.T) {
	d := newTwoPageDaemon(t)
	mustRunOn(t, d.c, "s1", "", "tab", "list")
	mustRunOn(t, d.c, "s1", "", "tab", "t2")

	e := d.mustIssue(t, "s1", "")
	if e.Page != "Other" {
		t.Fatalf("get cdp-url after 'tab t2' selected %q, want the tab's page Other", e.Page)
	}
	wantListed(t, e, "Other")
	if again := d.mustIssue(t, "s1", ""); again.HTTP != e.HTTP {
		t.Fatalf("issuing again for the same session, page and scope gave %s, want the same record %s", again.HTTP, e.HTTP)
	}
}

// "--page t2" names the tab t2 as on any command; a page key and a title
// still select as before. Each session and page has its own record.
// Issuing takes no lease: session s2 issues for the page that s1 holds,
// gets a URL and leases nothing.
func TestGetCDPURLPageTakesATabID(t *testing.T) {
	d := newTwoPageDaemon(t)
	mustRunOn(t, d.c, "s1", "", "tab", "list")
	mustRunOn(t, d.c, "s1", "", "tab", "t2")
	e := d.mustIssue(t, "s1", "")

	byTab := d.mustIssue(t, "s2", "t2")
	if byTab.Page != "Other" {
		t.Fatalf("get cdp-url --page t2 selected %q, want the tab's page Other", byTab.Page)
	}
	wantListed(t, byTab, "Other")
	if byTab.HTTP == e.HTTP {
		t.Fatal("session s2 got the record of session s1")
	}
	if byKey := d.mustIssue(t, "s2", "iwdp|SIMULATOR|"+d.fixture.URL); byKey.Page != "Fixture" {
		t.Fatalf("get cdp-url --page <page key> selected %q, want Fixture", byKey.Page)
	}
	byTitle := d.mustIssue(t, "s2", "fixture")
	if byTitle.Page != "Fixture" || byTitle.HTTP == byTab.HTTP {
		t.Fatalf("get cdp-url --page fixture = %+v; want Fixture in another record than %s", byTitle, byTab.HTTP)
	}
	wantListed(t, byTitle, "Fixture")

	st, err := d.c.Status(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Pages {
		if p.Session != "" && (p.Session != "s1" || p.Title != "Other") {
			t.Fatalf("daemon pages %+v; want only s1's lease on Other (issuing takes no lease)", st.Pages)
		}
	}
}

// The session's default page (the page of its last request without a
// page) is the selected page while it is attached, also when the listing
// now lists another page first, as for any command of the session.
func TestGetCDPURLUsesTheSessionsDefaultPage(t *testing.T) {
	d := newTwoPageDaemon(t)
	d.dev.RemovePage(d.pFix) // the listing: Other
	var ran PageInfo
	if _, err := d.c.RunWithPage(ctxFor(t, 10*time.Second), RunRequest{Argv: leaseFixture, Owner: Owner{Session: "s3"}}, nil, func(p PageInfo) { ran = p }); err != nil {
		t.Fatalf("a command of s3 without a page: %v", err)
	}
	if ran.Title != "Other" {
		t.Fatalf("the command without a page ran on %q, want Other (the only listed page)", ran.Title)
	}
	// The listing again: Fixture first, then Other.
	d.dev.RemovePage(d.pOther)
	d.pFix = d.dev.AddPage("Fixture", "https://fixture.example/", d.fixture.URL)
	d.pOther = d.dev.AddPage("Other", "https://other.example/", d.other.URL)

	if e := d.mustIssue(t, "s3", ""); e.Page != "Other" {
		t.Fatalf("get cdp-url of s3 selected %q, want its default page Other", e.Page)
	}
	// A session without a current or default page gets the listing's first
	// debuggable page, as before.
	if e := d.mustIssue(t, "s4", ""); e.Page != "Fixture" {
		t.Fatalf("get cdp-url of a new session selected %q, want the listing's first page Fixture", e.Page)
	}
}

// Exit 5 when the session has no page: its tab left the device (here the
// tab's page left the listing and its connection ended), so any command of
// the session fails with exit 5, and get cdp-url fails the same way
// instead of picking another page; a listing without pages too.
func TestGetCDPURLWithoutTheSessionsPageExits5(t *testing.T) {
	d := newTwoPageDaemon(t)
	mustRunOn(t, d.c, "s1", "", "tab", "list")
	mustRunOn(t, d.c, "s1", "", "tab", "t2")
	d.dev.RemovePage(d.pOther)
	d.other.CloseConnections()
	waitPageState(t, d.c, core.ConnLost)

	if err := leasePage(t, d.c, "", Owner{Session: "s1"}, 10*time.Second); commands.ExitCodeOf(err) != 5 {
		t.Fatalf("a command of s1 after its tab left: %v (exit %d), want exit 5", err, commands.ExitCodeOf(err))
	}
	e, err := d.issue(t, "s1", "")
	if commands.ExitCodeOf(err) != 5 {
		t.Fatalf("get cdp-url after the session's tab left = %+v, %v (exit %d); want exit 5", e, err, commands.ExitCodeOf(err))
	}

	d.dev.RemovePage(d.pFix)
	if e, err := d.issue(t, "s5", ""); commands.ExitCodeOf(err) != 5 || !strings.Contains(err.Error(), "no debuggable page") {
		t.Fatalf("get cdp-url without a listed page = %+v, %v (exit %d); want exit 5, no debuggable page", e, err, commands.ExitCodeOf(err))
	}
}
