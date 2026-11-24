package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// docs/known-limits.md, "iwdp does not list pages while a page is
// connected": the fake listing closes every request without a reply while
// the rig's page has a connection, as the iwdp of the second device check
// did.

// silentRig is a rig whose listing goes silent while its page is
// connected, with the page attached for agent's session (the listing
// answered that first attach).
func silentRig(t *testing.T) (*rig, *Page) {
	t.Helper()
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.l.EmptyReplyWhileConnected(r.f)
	r.reserve(t, "agent")
	pg, _, err := r.m.Touch(ctxFor(t, 10*time.Second), r.req, agent)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	return r, pg
}

// wantListingAdvice checks the error of a listing read while the page is
// connected: exit 5, what happened, what still works, what to do, and the
// empty reply as the cause.
func wantListingAdvice(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error while the listing does not answer", what)
	}
	if exitCode(err) != CodeNoPage {
		t.Errorf("%s: exit code %d, want %d: %v", what, exitCode(err), CodeNoPage, err)
	}
	for _, want := range []string{"iwdp did not answer the page list while the daemon has 1 page connection open", "commands on an attached page still work", "iwdp-cli daemon stop", "iwdp-cli restart-iwdp", "only whoever started that iwdp, and only when nobody else uses it, may stop it", "Stopping an iwdp by hand", "EOF"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not contain %q", what, err, want)
		}
	}
}

// Requests for the attached page resolve without the listing: the
// session's request without a page (its default page), the page's key,
// and CDP's ResolvePage of the key and of the session's request without a
// page. Requests that need the listing get the advice: another session
// without a default page, a title, ResolvePage without a page for another
// session, ListPages and Tabs.
func TestAttachedPageResolvesWithoutTheListing(t *testing.T) {
	r, pg := silentRig(t)
	ctx := ctxFor(t, 10*time.Second)
	again, _, err := r.m.Touch(ctx, r.req, agent)
	if err != nil || again != pg {
		t.Fatalf("Touch without a page after the attach = %v, %v; want the attached page", again, err)
	}
	byKey := r.req
	byKey.Page = pg.Key
	if again, _, err := r.m.Touch(ctx, byKey, agent); err != nil || again != pg {
		t.Fatalf("Touch of the page key = %v, %v; want the attached page", again, err)
	}
	lp, _, err := r.m.ResolvePage(ctx, agent.Session, agent.Reservation, byKey)
	if err != nil || lp.Key != pg.Key || lp.Title != "Fixture" {
		t.Fatalf("ResolvePage of the page key = %+v, %v; want the attached page", lp, err)
	}
	lp, listing, err := r.m.ResolvePage(ctx, agent.Session, agent.Reservation, r.req)
	if err != nil || lp.Key != pg.Key {
		t.Fatalf("ResolvePage without a page for the session of the attach = %+v, %v; want its default page, the attached one", lp, err)
	}
	if want := (PageRequest{Transport: r.req.Transport, Addr: r.req.Addr}); listing != want {
		t.Fatalf("ResolvePage's listing = %+v, want the request's transport and address %+v", listing, want)
	}
	if n := r.f.ConnectionCount(); n != 1 {
		t.Fatalf("page connections: %d, want 1", n)
	}

	_, _, err = r.m.Touch(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"})
	wantListingAdvice(t, "another session without a page", err)
	byTitle := r.req
	byTitle.Page = "Fixture"
	_, _, err = r.m.Touch(ctx, byTitle, agent)
	wantListingAdvice(t, "a page selected by title", err)
	_, _, err = r.m.ResolvePage(ctx, "s2", "", r.req)
	wantListingAdvice(t, "ResolvePage without a page for another session", err)
	_, err = r.m.ListPages(ctx, r.req)
	wantListingAdvice(t, "ListPages", err)
	_, _, err = r.m.Tabs(ctx, r.req, agent)
	wantListingAdvice(t, "Tabs", err)
	// A key requested for another transport kind is not the record's.
	other := byKey
	other.Transport = "webkitgtk"
	_, _, err = r.m.Touch(ctx, other, agent)
	wantListingAdvice(t, "the page key with another transport", err)
}

// A closed tab (ForgetTab, tab close) is no longer the session's default
// page: the next request without a page reads the listing.
func TestForgottenTabIsNoDefaultPage(t *testing.T) {
	r, pg := silentRig(t)
	r.m.ForgetTab(pg.Key)
	_, _, err := r.m.Touch(ctxFor(t, 10*time.Second), r.req, agent)
	wantListingAdvice(t, "a request without a page after ForgetTab", err)
}

// The advice counts the open iwdp page connections, applies to a listing
// error that the transport function reports (the daemon's listing check),
// and leaves every other transport error unchanged.
func TestListingAdviceOnlyForListingErrors(t *testing.T) {
	var fail atomic.Pointer[error] // the error the transport function returns, or nil
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
		if err := fail.Load(); err != nil {
			return nil, *err
		}
		return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
	}})
	f2 := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f2.Close)
	r.dev.AddPage("Other", "https://other.example/", f2.URL)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	for _, title := range []string{"Fixture", "Other"} {
		q := r.req
		q.Page = title
		if _, _, err := r.m.Touch(ctx, q, AcquireOptions{Session: title, Reservation: "agent"}); err != nil {
			t.Fatalf("attach %s: %v", title, err)
		}
	}
	r.l.EmptyReplyWhileConnected(r.f, f2)
	_, err := r.m.ListPages(ctx, r.req)
	if err == nil || !strings.Contains(err.Error(), "while the daemon has 2 page connections open") {
		t.Fatalf("ListPages with two connections = %v, want the advice for 2 page connections", err)
	}
	checkErr := error(&ListingError{Kind: transport.KindIWDP, Err: errors.New("check: empty reply")})
	fail.Store(&checkErr)
	_, err = r.m.ListPages(ctx, r.req)
	if exitCode(err) != CodeNoPage || !strings.Contains(err.Error(), "did not answer the page list") || !strings.Contains(err.Error(), "check: empty reply") {
		t.Fatalf("ListPages with a failed listing check = %v, want exit 5 and the advice with the cause", err)
	}
	otherErr := errors.New("unknown transport \"x\"")
	fail.Store(&otherErr)
	_, err = r.m.ListPages(ctx, r.req)
	if exitCode(err) != CodeNoPage || err.Error() != "unknown transport \"x\"" {
		t.Fatalf("ListPages with another transport error = %v, want that error unchanged (exit 5)", err)
	}
}

// When the attachment is gone, resolution reads the listing again, as
// before: the page left the listing, so no page matches.
func TestGoneAttachmentFallsBackToTheListing(t *testing.T) {
	for _, gone := range []string{"lost", "closed"} {
		t.Run(gone, func(t *testing.T) {
			r, pg := silentRig(t)
			ctx := ctxFor(t, 10*time.Second)
			switch gone {
			case "lost": // the peer ended the connection
				r.f.CloseConnections()
				eventually(t, 5*time.Second, "the page is lost", func() bool { return pg.ConnState() == ConnLost })
				// The fake listing answers again once its server has
				// dropped the closed connection, as iwdp did on the device.
				eventually(t, 5*time.Second, "the fake server to drop the connection", func() bool { return r.f.OpenConnections() == 0 })
			case "closed": // the daemon closed it after its idle time
				if err := r.m.Release(ctx, agent.Session, pg.Key); err != nil {
					t.Fatal(err)
				}
				r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
				eventually(t, 5*time.Second, "the page connection is closed", func() bool { return r.f.OpenConnections() == 0 })
			}
			r.dev.RemovePage(r.pg)
			byKey := r.req
			byKey.Page = pg.Key
			for _, req := range []PageRequest{r.req, byKey} {
				_, _, err := r.m.Touch(ctx, req, agent)
				if !errors.Is(err, ErrNoPage) || !strings.Contains(err.Error(), "among 0 listed") {
					t.Errorf("Touch(%q) after the page was %s = %v; want the listing's answer (no page among 0 listed)", req.Page, gone, err)
				}
			}
		})
	}
}
