package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Review F1: a reservation names its turn. Reserve gives a new reservation
// a new random ID and keeps it when the owner renews (Reserve again,
// Renew); after a release the next reservation is a new turn. RenewTurn
// renews only the reservation of its ID. A file of an earlier version,
// without an ID, still reads and renews.
func TestReservationTurnID(t *testing.T) {
	rs := &Reservations{Dir: t.TempDir()}
	first, err := rs.Reserve(SimulatorDevice, "a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" {
		t.Fatal("a new reservation has no ID")
	}
	again, err := rs.Reserve(SimulatorDevice, "a", time.Hour)
	if err != nil || again.ID != first.ID {
		t.Fatalf("the owner's renewal by Reserve: ID %q, err %v; want %q", again.ID, err, first.ID)
	}
	if ok, err := rs.Renew("a", SimulatorDevice); !ok || err != nil {
		t.Fatalf("Renew: %t, %v", ok, err)
	}
	if ok, err := rs.RenewTurn("a", SimulatorDevice, first.ID); !ok || err != nil {
		t.Fatalf("RenewTurn of the live turn: %t, %v", ok, err)
	}
	if mine, _, _ := rs.Check(SimulatorDevice, "a"); mine == nil || mine.ID != first.ID {
		t.Fatalf("after the renewals: %+v, want ID %q", mine, first.ID)
	}
	if err := rs.Release(SimulatorDevice, "a"); err != nil {
		t.Fatal(err)
	}
	next, err := rs.Reserve(SimulatorDevice, "a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == "" || next.ID == first.ID {
		t.Fatalf("the turn after a release has ID %q, the earlier %q", next.ID, first.ID)
	}
	if ok, err := rs.RenewTurn("a", SimulatorDevice, first.ID); ok || err != nil {
		t.Fatalf("RenewTurn of the earlier turn renewed the next one: %t, %v", ok, err)
	}

	legacy := &Reservations{Dir: t.TempDir()}
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	data := `{"device":"SIMULATOR","owner":"old","created":"2026-01-01T00:00:00Z","expires":"` + exp + `","ttl":120000000000}`
	if err := os.WriteFile(filepath.Join(legacy.Dir, fileName(SimulatorDevice)), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	mine, _, err := legacy.Check(SimulatorDevice, "old")
	if err != nil || mine == nil || mine.ID != "" {
		t.Fatalf("a reservation file without an ID: %+v, %v", mine, err)
	}
	if ok, err := legacy.RenewTurn("old", SimulatorDevice, ""); !ok || err != nil {
		t.Fatalf("RenewTurn of a reservation without an ID: %t, %v", ok, err)
	}
}
