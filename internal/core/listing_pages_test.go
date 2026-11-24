package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// A device's page list that fails while the device list answers
// (pageListFailed in listing_attached.go): when the daemon holds an open
// connection to a page of that device, a request that may need the device
// gets the coded listing error with the advice; a device without an open
// page connection is left out and logged, so it hides no other device.

// pagesFailure is an iwdp transport whose page lists of the devices in
// fail end with an empty reply (io.EOF), while its device list answers.
type pagesFailure struct {
	transport.Transport
	mu   *sync.Mutex
	fail map[string]bool
}

func (tr pagesFailure) Pages(ctx context.Context, id string) ([]transport.Page, error) {
	tr.mu.Lock()
	failing := tr.fail[id]
	tr.mu.Unlock()
	if failing {
		return nil, io.EOF
	}
	return tr.Transport.Pages(ctx, id)
}

// pagesFailureRig is a rig whose transport fails the page lists of the
// device ids that its setter is given.
func pagesFailureRig(t *testing.T) (r *rig, failPages func(ids ...string)) {
	t.Helper()
	var mu sync.Mutex
	fail := map[string]bool{}
	r = newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
		return pagesFailure{Transport: iwdp.New(iwdp.Options{ListingAddr: addr}), mu: &mu, fail: fail}, nil
	}})
	return r, func(ids ...string) {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range ids {
			fail[id] = true
		}
	}
}

// Review F2 (acceptance criterion 2): an error reading the page list of
// the attached page's device must report the failure and the recovery
// advice, not "no page" or an empty list.
func TestReviewPageListingFailureHasAdvice(t *testing.T) {
	r, failPages := pagesFailureRig(t)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	if _, _, err := r.m.Touch(ctx, r.req, agent); err != nil {
		t.Fatal(err)
	}
	failPages(r.dev.ID) // /json still answers; the device page list fails.
	byTitle := r.req
	byTitle.Page = "Fixture"
	_, _, err := r.m.Touch(ctx, byTitle, agent)
	wantPageListAdvice(t, "select by title", err)
	tabs, _, err := r.m.Tabs(ctx, r.req, agent)
	if len(tabs) != 0 {
		t.Errorf("tab list returned %d tabs with the error", len(tabs))
	}
	wantPageListAdvice(t, "tab list", err)
	pages, err := r.m.ListPages(ctx, r.req)
	if len(pages) != 0 {
		t.Errorf("CDP ListPages returned %d pages with the error", len(pages))
	}
	wantPageListAdvice(t, "CDP ListPages", err)
}

// wantPageListAdvice checks the error of a failed page list of the
// attached page's device: exit 5, the advice, the device and the empty
// reply as the cause.
func wantPageListAdvice(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error while the page list of the attached page's device fails", what)
		return
	}
	if exitCode(err) != CodeNoPage {
		t.Errorf("%s: exit code %d, want %d: %v", what, exitCode(err), CodeNoPage, err)
	}
	for _, want := range []string{"iwdp did not answer the page list while the daemon has 1 page connection open", "commands on an attached page still work", "iwdp-cli daemon stop", "iwdp-cli restart-iwdp", "only whoever started that iwdp, and only when nobody else uses it, may stop it", "Stopping an iwdp by hand", "listing the pages of iPhone X", "EOF"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not contain %q", what, err, want)
		}
	}
}

// With two devices, a failed page list matters only for a request that may
// need its device: a page that matches on the other device is used, and a
// device without an open page connection is left out as before (a healthy
// iwdp can list a device whose port does not answer).
func TestPageListFailureOfOneOfTwoDevices(t *testing.T) {
	setup := func(t *testing.T) (*rig, func(...string), *fakewebkit.ListedDevice) {
		r, failPages := pagesFailureRig(t)
		f2 := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f2.Close)
		dev2 := r.l.AddDevice("00008020-0000000000000002", "iPad", "16.7.10")
		dev2.AddPage("Other", "https://other.example/", f2.URL)
		r.reserve(t, "agent")
		if _, err := r.res.Reserve(dev2.ID, "agent", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.m.Touch(ctxFor(t, 10*time.Second), r.req, agent); err != nil {
			t.Fatalf("attach Fixture on the first device: %v", err)
		}
		return r, failPages, dev2
	}
	titles := func(keys []string) string { return strings.Join(keys, ", ") }

	t.Run("the other device fails", func(t *testing.T) {
		r, failPages, dev2 := setup(t)
		ctx := ctxFor(t, 10*time.Second)
		failPages(dev2.ID) // no page connection is open on it
		tabs, _, err := r.m.Tabs(ctx, r.req, agent)
		var got []string
		for _, tb := range tabs {
			got = append(got, tb.Title)
		}
		if err != nil || titles(got) != "Fixture" {
			t.Errorf("Tabs = %q, %v; want the first device's page without an error", titles(got), err)
		}
		pages, err := r.m.ListPages(ctx, r.req)
		got = nil
		for _, p := range pages {
			got = append(got, p.Title)
		}
		if err != nil || titles(got) != "Fixture" {
			t.Errorf("ListPages = %q, %v; want the first device's page without an error", titles(got), err)
		}
		none := r.req
		none.Page = "nowhere"
		if _, _, err := r.m.ResolvePage(ctx, agent.Session, "", none); !errors.Is(err, ErrNoPage) || !strings.Contains(err.Error(), `no page matches "nowhere" among 1 listed`) {
			t.Errorf("ResolvePage(nowhere) = %v; want no page among the 1 listed", err)
		}
	})

	t.Run("the attached page's device fails", func(t *testing.T) {
		r, failPages, _ := setup(t)
		ctx := ctxFor(t, 10*time.Second)
		failPages(r.dev.ID)
		other := r.req
		other.Page = "Other"
		if lp, _, err := r.m.ResolvePage(ctx, agent.Session, "", other); err != nil || lp.Title != "Other" {
			t.Errorf("ResolvePage(Other) = %+v, %v; want the other device's page", lp, err)
		}
		none := r.req
		none.Page = "nowhere"
		_, _, err := r.m.ResolvePage(ctx, agent.Session, "", none)
		wantPageListAdvice(t, "a title that matches no listed page", err)
		_, _, err = r.m.Tabs(ctx, r.req, agent)
		wantPageListAdvice(t, "tab list", err)
		_, err = r.m.ListPages(ctx, r.req)
		wantPageListAdvice(t, "CDP ListPages", err)
	})
}
