package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/transport/iwdp"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design 3.8 and 4.1: "tab" lists pages with stable ids t1, t2, ...;
// "tab <id>" makes a page the session's current page, which the session's
// later commands use; --page (also a tab id or label) overrides it for one
// command; other sessions keep resolving as before.
func TestTabSelectionSteersOnlyTheSelectingSession(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	f2 := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f2.Close)
	r.dev.AddPage("Second", "https://second.example/", f2.URL)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)

	tabs, _, err := r.m.Tabs(ctx, r.req, agent)
	if err != nil {
		t.Fatalf("Tabs: %v", err)
	}
	if len(tabs) != 2 || tabs[0].ID != "t1" || tabs[0].Title != "Fixture" || tabs[1].ID != "t2" || tabs[1].Title != "Second" {
		t.Fatalf("tabs = %+v, want t1 Fixture and t2 Second", tabs)
	}
	if tabs[0].URL != "https://fixture.example/" {
		t.Fatalf("tab URL %q keeps its query; listings redact it (R17)", tabs[0].URL)
	}

	keyOf := func(session string, req PageRequest) string {
		t.Helper()
		mo, err := r.m.Begin(ctx, req, AcquireOptions{Session: session, Reservation: "agent"}, "probe", ClassRead)
		if err != nil {
			t.Fatalf("%s: Begin(%+v): %v", session, req, err)
		}
		defer r.m.End(mo.ID)
		return mo.Page.Key
	}
	if k := keyOf("s1", r.req); k != tabs[0].Key {
		t.Fatalf("before a selection the session uses %s, want the first page %s", k, tabs[0].Key)
	}
	sel := r.req
	sel.Page = tabs[1].Key
	r.m.SelectPage("s1", sel)
	if k := keyOf("s1", r.req); k != tabs[1].Key {
		t.Fatalf("after tab t2 the session uses %s, want %s", k, tabs[1].Key)
	}
	byTitle := r.req
	byTitle.Page = "Fixture"
	if k := keyOf("s1", byTitle); k != tabs[0].Key {
		t.Fatalf("--page Fixture gave %s, want %s (--page overrides the current page)", k, tabs[0].Key)
	}
	// s1 leases both pages now; s2 needs them free.
	if _, err := r.m.ReleaseSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	byID := r.req
	byID.Page = "t2"
	if k := keyOf("s2", byID); k != tabs[1].Key {
		t.Fatalf("--page t2 gave %s, want %s", k, tabs[1].Key)
	}
	if err := r.m.Release(ctx, "s2", tabs[1].Key); err != nil {
		t.Fatal(err)
	}
	if k := keyOf("s2", r.req); k != tabs[0].Key {
		t.Fatalf("session s2 without a selection uses %s, want the first page %s", k, tabs[0].Key)
	}

	again, _, err := r.m.Tabs(ctx, r.req, AcquireOptions{Session: "s1", Reservation: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].ID != "t1" || again[1].ID != "t2" || !again[1].Current || again[0].Current {
		t.Fatalf("second listing = %+v, want the same ids and t2 current for s1", again)
	}
	if err := r.m.LabelTab(tabs[0].Key, "home"); err != nil {
		t.Fatal(err)
	}
	if k, ok := r.m.TabKey("home"); !ok || k != tabs[0].Key {
		t.Fatalf("TabKey(home) = %q, %v", k, ok)
	}
	if err := r.m.LabelTab(tabs[0].Key, "t9"); err == nil {
		t.Fatal("a label shaped like a tab id was accepted")
	}
	r.m.ForgetTab(tabs[1].Key)
	if _, ok := r.m.CurrentPage("s1"); ok {
		t.Fatal("the closed tab is still the current page of s1")
	}
	if k, ok := r.m.TabKey("t2"); !ok || k != tabs[1].Key {
		t.Fatalf("tab ids stay with their page key: TabKey(t2) = %q, %v", k, ok)
	}
}

// Design 2.5 rule 4 (review finding 1): tab discovery lists the pages of
// a physical device only for the owner of a live reservation of it; a
// local device (the simulator) needs none.
func TestTabsListPagesOnlyOfReservedPhysicalDevices(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	sim := r.l.AddDevice("SIMULATOR", "Simulator", "17.0")
	f2 := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f2.Close)
	sim.AddPage("Local", "https://local.example/", f2.URL)
	ctx := ctxFor(t, 10*time.Second)
	titles := func(tabs []Tab) string {
		var out []string
		for _, tb := range tabs {
			out = append(out, tb.Title+" "+tb.URL)
		}
		return strings.Join(out, ", ")
	}

	list := func(ao AcquireOptions) (string, int) {
		t.Helper()
		tabs, hidden, err := r.m.Tabs(ctx, r.req, ao)
		if err != nil {
			t.Fatalf("Tabs(%+v): %v", ao, err)
		}
		return titles(tabs), hidden
	}
	const local, both = "Local https://local.example/", "Fixture https://fixture.example/, Local https://local.example/"

	if got, hidden := list(AcquireOptions{Session: "stranger"}); got != local || hidden != 1 {
		t.Fatalf("without a reservation Tabs = %q, %d hidden; want only the simulator page and 1 hidden, no title or URL of the physical device", got, hidden)
	}
	r.reserve(t, "agent")
	if got, hidden := list(AcquireOptions{Session: "s1", Reservation: "agent"}); got != both || hidden != 0 {
		t.Fatalf("with the reservation Tabs = %q, %d hidden; want both pages", got, hidden)
	}
	// The reservation owner defaults to the session name (AcquireOptions).
	if got, hidden := list(AcquireOptions{Session: "agent"}); got != both || hidden != 0 {
		t.Fatalf("session agent without --reservation: Tabs = %q, %d hidden; want both pages", got, hidden)
	}
	if got, hidden := list(AcquireOptions{Session: "s2", Reservation: "other"}); got != local || hidden != 1 {
		t.Fatalf("another owner's reservation: Tabs = %q, %d hidden; want only the simulator page", got, hidden)
	}
	// Without reservations configured no physical page is listed.
	m := NewManager(ManagerOptions{Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
		return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
	}})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	tabs, hidden, err := m.Tabs(ctx, r.req, agent)
	if err != nil || titles(tabs) != local || hidden != 1 {
		t.Fatalf("without reservations: Tabs = %q, %d hidden, %v; want only the simulator page", titles(tabs), hidden, err)
	}
}
