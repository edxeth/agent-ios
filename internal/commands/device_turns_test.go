package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
)

// Regression tests of the physical devices' reservations (design 2.5 rules
// 1 and 4) on the ways to a page that the simulator's turns were checked on
// (sim_turns_test.go): a page that the daemon is already attached to, a
// held lease, the page observer and the sweep.

// newPhoneManager is a manager over a tabFake that lists the physical
// device phone-1 with one page, Page.
func newPhoneManager(t *testing.T) (*core.Manager, *tabFake, *core.Reservations) {
	t.Helper()
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device = transport.KindIWDP, "phone-1"
	f.add("Page", "https://phone.test/")
	return m, f, res
}

// Rule 1 on a page that the daemon is already attached to: once the owner
// that attached it released its reservation, a page command of that owner,
// or of an owner without a reservation, gets exit 6 (no reservation); once
// another owner reserved the phone, exit 6 (reserved). The reserving owner
// is admitted.
func TestPhoneReservationOnAnAttachedPage(t *testing.T) {
	m, _, res := newPhoneManager(t)
	if _, err := res.Reserve("phone-1", "old", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustIn(t, m, "old", "eval", "1+1")
	if _, err := m.ReleaseSession(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := res.Release("phone-1", "old"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"old", "other"} {
		for _, argv := range [][]string{{"eval", "--page", "page-1", "1+1"}, {"eval", "1+1"}} {
			if _, err := runIn(t, m, session, argv...); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrNoReservation) {
				t.Errorf("%s %v on the attached page without a reservation: %v (exit %d), want exit 6", session, argv, err, ExitCodeOf(err))
			}
		}
	}
	if _, err := res.Reserve("phone-1", "new", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"eval", "--page", "page-1", "1+1"}, {"eval", "1+1"}} {
		if _, err := runIn(t, m, "old", argv...); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
			t.Errorf("old %v on the attached page in new's reservation: %v (exit %d), want exit 6", argv, err, ExitCodeOf(err))
		}
	}
	mustIn(t, m, "new", "eval", "1+1")
}

// Rule 1 on a lease that the session holds: a session that keeps its lease
// after its owner's reservation moved to another owner gets exit 6 on its
// next command.
func TestHeldPhoneLeaseAdmitsNothingAfterTheReservationMoved(t *testing.T) {
	m, _, res := newPhoneManager(t)
	if _, err := res.Reserve("phone-1", "old", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustIn(t, m, "old", "eval", "1+1")
	if err := res.Release("phone-1", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Reserve("phone-1", "new", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "old", "eval", "1+1"); ExitCodeOf(err) != ExitReservation || !errors.Is(err, core.ErrReserved) {
		t.Fatalf("old's held lease in new's reservation: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
}

// An owner refused on a physical device's page gets no page item: the
// page observer, which the daemon streams to the client, sees neither the
// page's title nor its key, whether another owner reserves the device or
// nobody does, and also when the daemon is already attached to the page.
func TestRefusedOwnerObservesNoPhonePage(t *testing.T) {
	observe := func(t *testing.T, m *core.Manager, session string, want error) {
		t.Helper()
		var seen []string
		ctx := core.WithPageObserver(context.Background(), func(key, title string) { seen = append(seen, key, title) })
		if _, _, err := m.Touch(ctx, core.PageRequest{Page: "Page"}, core.AcquireOptions{Session: session}); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", session, err, want)
		}
		if len(seen) != 0 {
			t.Fatalf("the refused owner %s observed %q", session, seen)
		}
	}
	t.Run("not attached", func(t *testing.T) {
		m, _, res := newPhoneManager(t)
		observe(t, m, "b", core.ErrNoReservation)
		if _, err := res.Reserve("phone-1", "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		observe(t, m, "b", core.ErrReserved)
	})
	t.Run("attached", func(t *testing.T) {
		m, _, res := newPhoneManager(t)
		if _, err := res.Reserve("phone-1", "a", time.Hour); err != nil {
			t.Fatal(err)
		}
		mustIn(t, m, "a", "eval", "1+1")
		observe(t, m, "b", core.ErrReserved)
	})
}

// A lease on a phone's page whose owner no longer holds the phone's
// reservation (released, or moved to another owner) ends in the daemon's
// sweep, so nothing bound to the lease (an event stream, a CDP attachment)
// goes on without it; the reserving owner's lease stays.
func TestSweepEndsALeaseWhoseOwnerLostThePhone(t *testing.T) {
	for _, next := range []string{"", "b"} {
		t.Run("next owner "+next, func(t *testing.T) {
			m, _, res := newPhoneManager(t)
			if _, err := res.Reserve("phone-1", "a", time.Hour); err != nil {
				t.Fatal(err)
			}
			mustIn(t, m, "a", "eval", "1+1")
			m.Sweep(time.Now())
			if n := len(sessionPages(m, "a")); n != 1 {
				t.Fatalf("the sweep ended the reserving owner's own lease: %d lease(s)", n)
			}
			if err := res.Release("phone-1", "a"); err != nil {
				t.Fatal(err)
			}
			if next != "" {
				if _, err := res.Reserve("phone-1", next, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			m.Sweep(time.Now())
			if n := len(sessionPages(m, "a")); n != 0 {
				t.Fatalf("a's lease outlived its reservation by a sweep (next owner %q): %d lease(s)", next, n)
			}
			if next == "" {
				return
			}
			mustIn(t, m, next, "eval", "1+1")
			m.Sweep(time.Now())
			if n := len(sessionPages(m, next)); n != 1 {
				t.Fatalf("the sweep ended the reserving owner's own lease: %d lease(s)", n)
			}
		})
	}
}

// An operation under a lease held from before (a CDP attachment's
// BeginLeased) sends nothing once the phone's reservation was released or
// moved to another owner, before any sweep ends the lease.
func TestHeldPhoneLeaseSendsNothingInAnotherOwnersReservation(t *testing.T) {
	m, _, res := newPhoneManager(t)
	if _, err := res.Reserve("phone-1", "bob", time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	pg, epoch, err := m.Touch(ctx, core.PageRequest{}, core.AcquireOptions{Session: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	mo, err := m.BeginLeased(ctx, pg, "bob", epoch, "cdp probe", core.ClassRead)
	if err != nil {
		t.Fatalf("bob's held lease in his reservation: %v", err)
	}
	m.End(mo.ID)
	if err := res.Release("phone-1", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginLeased(ctx, pg, "bob", epoch, "cdp probe", core.ClassRead); !errors.Is(err, core.ErrNoReservation) {
		t.Fatalf("bob's held lease without a reservation: %v, want ErrNoReservation", err)
	}
	if _, err := res.Reserve("phone-1", "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginLeased(ctx, pg, "bob", epoch, "cdp probe", core.ClassRead); !errors.Is(err, core.ErrReserved) {
		t.Fatalf("bob's held lease in alice's reservation: %v, want ErrReserved", err)
	}
}

// A reservation store that cannot be read admits no page command on an
// attached phone page (exit 6): it fails closed, as on a first attach.
func TestUnreadablePhoneReservationAdmitsNothingOnAnAttachedPage(t *testing.T) {
	m, _, res := newPhoneManager(t)
	if _, err := res.Reserve("phone-1", "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustIn(t, m, "a", "eval", "1+1")
	if err := os.WriteFile(filepath.Join(res.Dir, "device-broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runIn(t, m, "a", "eval", "1+1"); ExitCodeOf(err) != ExitReservation {
		t.Fatalf("an unreadable reservation: %v (exit %d), want exit 6", err, ExitCodeOf(err))
	}
}
