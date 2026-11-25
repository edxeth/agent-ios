package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/transport/iwdp"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design 5.2 (review of the session-page fix, F1 and F2): an endpoint
// record lists and attaches its selected page through the transport and
// address that page was found through, not through the request's. The
// session's current tab, and an explicit selector of an attached page,
// can name a page of another transport than the request's (auto here, the
// default iwdp listing).

// kindAs is a fake iwdp listing that reports another transport kind.
type kindAs struct {
	transport.Transport
	kind transport.Kind
}

func (k kindAs) Kind() transport.Kind { return k.kind }

// altRig is a manager whose default (auto) transport is the rig's iwdp
// listing and whose transport kind alt is a second listing with one page,
// "Selected", served by the rig's fake page Two; its CDP endpoint runs.
type altRig struct {
	r    *rig
	m    *core.Manager
	srv  *Server
	alt  string           // the second listing's transport kind
	page core.ListedPage  // the second listing's page
	req  core.PageRequest // selects exactly that page through alt
}

func newAltRig(t *testing.T, alt transport.Kind) *altRig {
	t.Helper()
	r := newRig(t)
	other := fakewebkit.NewListing(false)
	t.Cleanup(other.Close)
	dev := other.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage("Selected", "https://selected.example/", r.fake["Two"].URL)
	if _, err := r.res.Reserve(dev.ID, "s1", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := core.NewManager(core.ManagerOptions{
		Transport: func(_ context.Context, kind, addr string) (transport.Transport, error) {
			if kind == string(alt) {
				return kindAs{iwdp.New(iwdp.Options{ListingAddr: other.Addr()}), alt}, nil
			}
			return iwdp.New(iwdp.Options{ListingAddr: r.l.Addr()}), nil
		},
		Reservations: r.res,
		Session:      webkit.SessionOptions{TargetWait: time.Second},
		Logf:         t.Logf,
	})
	t.Cleanup(func() { _ = m.Close(ctxFor(t, 5*time.Second)) })
	s := New(Options{Manager: m, Reservations: r.res, Logf: t.Logf})
	if _, err := s.Start(0, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	pages, err := m.ListPages(testCtx(t), core.PageRequest{Transport: string(alt)})
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages of the %s listing: %+v, %v", alt, pages, err)
	}
	return &altRig{r: r, m: m, srv: s, alt: string(alt), page: pages[0], req: core.PageRequest{Transport: string(alt), Page: pages[0].Key}}
}

// wantDiscoverable issues the URL of session s1 for page and checks that
// it selects the alt page, that its /json/list has exactly that page, and
// that Target.attachToTarget through it attaches the page for s1.
func (a *altRig) wantDiscoverable(t *testing.T, page core.PageRequest) {
	t.Helper()
	ep, err := a.srv.Issue(testCtx(t), IssueRequest{Session: "s1", Page: page})
	if err != nil || ep.Page != "Selected" {
		t.Fatalf("Issue for s1 = %+v, %v; want the page Selected", ep, err)
	}
	code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
	var listed []struct{ ID, Title string }
	if code != 200 || json.Unmarshal([]byte(body), &listed) != nil || len(listed) != 1 || listed[0].Title != ep.Page {
		t.Fatalf("Issue selected %q, but its /json/list returned %d %s; want the selected page", ep.Page, code, body)
	}
	c := dial(t, ep.WS)
	att := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": listed[0].ID, "flatten": true}))
	if att.SessionID == "" {
		t.Fatal("Target.attachToTarget on the selected page gave no session id")
	}
	if session, _ := leaseOfIn(a.m, "Selected"); session != "s1" {
		t.Fatalf("the selected page is leased by %q after the attach, want s1", session)
	}
}

// F1: the session's tab, chosen on the direct transport, is selected by a
// request with transport auto; it is not attached, so the resolution reads
// the direct listing.
func TestIssueKeepsTheTransportOfTheSessionsTab(t *testing.T) {
	a := newAltRig(t, transport.KindDirect)
	a.m.SelectPage("s1", a.req)
	a.wantDiscoverable(t, core.PageRequest{})
}

// F2: an explicit selector of an attached page on the WebKitGTK transport
// (whose page ids are URLs, as iwdp's), with transport auto: a tab id, a
// label, a page key and a ws:// URL. Commands resolve them to the attached
// page without its listing (attachedTarget, resolveURL's attached page);
// the record must still list that page's transport.
func TestIssueKeepsTheTransportOfAnExplicitSelection(t *testing.T) {
	for _, selector := range []string{"tab-id", "label", "page-key", "ws-url"} {
		t.Run(selector, func(t *testing.T) {
			a := newAltRig(t, transport.KindWebKitGTK)
			ctx := ctxFor(t, 5*time.Second)
			a.m.SelectPage("s1", a.req) // hands out t1 and makes it the tab
			if _, _, err := a.m.Touch(ctx, a.req, core.AcquireOptions{Session: "s1"}); err != nil {
				t.Fatalf("attaching the tab: %v", err)
			}
			if err := a.m.LabelTab(a.page.Key, "selected"); err != nil {
				t.Fatal(err)
			}
			sel := map[string]string{"tab-id": "t1", "label": "selected", "page-key": a.page.Key, "ws-url": a.r.fake["Two"].URL}[selector]
			// A command with the same selector acts on the attached page.
			if pg, _, err := a.m.Touch(ctx, core.PageRequest{Page: sel}, core.AcquireOptions{Session: "s1"}); err != nil || pg.Title() != "Selected" {
				t.Fatalf("a command with --page %s: %v, %v; want the attached page", sel, pg, err)
			}
			a.wantDiscoverable(t, core.PageRequest{Page: sel})
		})
	}
}

// F2, the docs review's case: the page key of an attached page on the
// direct transport, with the transport omitted, and no current tab.
func TestIssueKeepsTheDirectTransportOfAnAttachedPageKey(t *testing.T) {
	a := newAltRig(t, transport.KindDirect)
	if _, _, err := a.m.Touch(ctxFor(t, 5*time.Second), a.req, core.AcquireOptions{Session: "s1"}); err != nil {
		t.Fatalf("attaching the direct page: %v", err)
	}
	a.wantDiscoverable(t, core.PageRequest{Page: a.page.Key})
}

// A local endpoint (a ws:// URL that no listing has) stays a local record
// after the daemon holds it open: issued again for its URL, the record has
// only that page and refuses Target.createTarget with the local-record
// message (record.createTarget), as before the listing context moved to
// the resolved page.
func TestIssueKeepsALocalRecordForAnAttachedLocalPage(t *testing.T) {
	r := newRig(t)
	newFakePage(r.fake["One"])
	r.devA.RemovePage(r.listed["One"]) // the page still runs, unlisted
	u := r.fake["One"].URL
	first := r.issue(t, "s1", u, ScopePage)
	list := func(ep Endpoint) []struct{ ID string } {
		t.Helper()
		code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
		if code != 200 {
			t.Fatalf("GET /json/list: %d %s", code, body)
		}
		return decode[[]struct{ ID string }](t, []byte(body))
	}
	targets := list(first)
	if len(targets) != 1 {
		t.Fatalf("the local record lists %d pages, want 1", len(targets))
	}
	call(t, dial(t, first.WS), "", "Target.attachToTarget", map[string]any{"targetId": targets[0].ID, "flatten": true})
	if pg := r.m.LookupPage("url|" + u); pg == nil {
		t.Fatal("the local page was not attached as a url| record")
	}

	again := r.issue(t, "s1", u, ScopeDevice)
	if n := len(list(again)); n != 1 {
		t.Fatalf("the record issued again for the attached local page lists %d pages, want 1", n)
	}
	_, err := dial(t, again.WS).Call(ctxFor(t, 5*time.Second), "", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err == nil || !strings.Contains(err.Error(), "a new tab needs a page from the transport's page list") {
		t.Fatalf("Target.createTarget through the local record: %v, want the local-record refusal", err)
	}
}

// leaseOfIn is the session that leases the page title in m.
func leaseOfIn(m *core.Manager, title string) (string, uint64) {
	for _, ps := range m.Status() {
		if ps.Title == title {
			return ps.Session, ps.Epoch
		}
	}
	return "", 0
}
