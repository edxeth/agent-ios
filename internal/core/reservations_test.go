package core

import (
	"errors"
	"testing"
	"time"
)

// Design 2.5 rule 1: a reservation fails while another live reservation
// covers the device; it expires after its TTL; Renew extends it.
func TestReservations(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	rs := &Reservations{Dir: t.TempDir(), Now: func() time.Time { return now }}
	if _, err := rs.Reserve("udid-1", "alice", time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	_, err := rs.Reserve("udid-1", "bob", time.Hour)
	var ce *CodedError
	if !errors.Is(err, ErrReserved) || !errors.As(err, &ce) || ce.Code != CodeReservation {
		t.Fatalf("second owner: %v, want ErrReserved with exit 6", err)
	}
	if _, err := rs.Reserve(AnyDevice, "bob", time.Hour); !errors.Is(err, ErrReserved) {
		t.Fatalf("any-device reservation over alice's: %v, want ErrReserved", err)
	}
	if _, err := rs.Reserve("udid-2", "bob", time.Hour); err != nil {
		t.Fatalf("other device: %v", err)
	}
	mine, other, err := rs.Check("udid-1", "alice")
	if err != nil || mine == nil || other != nil {
		t.Fatalf("Check alice = %v, %v, %v", mine, other, err)
	}
	mine, other, _ = rs.Check("udid-1", "bob")
	if mine != nil || other == nil || other.Owner != "alice" {
		t.Fatalf("Check bob = %v, %v", mine, other)
	}
	if err := rs.Release("udid-1", "bob"); !errors.Is(err, ErrReserved) {
		t.Fatalf("bob releases alice's reservation: %v", err)
	}
	now = now.Add(50 * time.Minute)
	if ok, err := rs.Renew("alice", "udid-1"); err != nil || !ok {
		t.Fatalf("Renew: %t, %v", ok, err)
	}
	now = now.Add(50 * time.Minute) // 100 min after the start, 50 after the renewal
	if mine, _, _ := rs.Check("udid-1", "alice"); mine == nil {
		t.Fatal("renewed reservation expired early")
	}
	now = now.Add(11 * time.Minute)
	if mine, other, _ := rs.Check("udid-1", "alice"); mine != nil || other != nil {
		t.Fatal("reservation outlived its TTL")
	}
	if _, err := rs.Reserve("udid-1", "bob", time.Hour); err != nil {
		t.Fatalf("Reserve after expiry: %v", err)
	}
	if err := rs.Release("udid-1", "bob"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := rs.Reserve("udid-1", "", time.Hour); err == nil {
		t.Fatal("Reserve without an owner succeeded")
	}
}

// The simulator's reservation stands apart from the physical devices':
// a reservation of every device (*) neither covers nor blocks it, and it
// blocks only another owner of the simulator.
func TestSimulatorReservationStandsApart(t *testing.T) {
	rs := &Reservations{Dir: t.TempDir()}
	if _, err := rs.Reserve(AnyDevice, "phone", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Reserve(SimulatorDevice, "a", time.Hour); err != nil {
		t.Fatalf("* blocked the simulator: %v", err)
	}
	if _, err := rs.Reserve(AnyDevice, "phone", time.Hour); err != nil {
		t.Fatalf("the simulator's reservation blocked *: %v", err)
	}
	if _, err := rs.Reserve(SimulatorDevice, "b", time.Hour); !errors.Is(err, ErrReserved) {
		t.Fatalf("b took a's simulator: %v", err)
	}
	if mine, other, _ := rs.Check(SimulatorDevice, "phone"); mine != nil || other == nil || other.Owner != "a" {
		t.Fatalf("Check(simulator, phone) = %+v, %+v; want a's as another owner's", mine, other)
	}
	if mine, other, _ := rs.Check("udid-1", "a"); mine != nil || other == nil || other.Owner != "phone" {
		t.Fatalf("Check(udid-1, a) = %+v, %+v; want phone's *", mine, other)
	}
}
