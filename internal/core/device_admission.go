package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
)

// The reservation rule of a page's device holds on every way to the page,
// not only on a first connection (design 2.5 rules 1 and 4):
//
//   - a physical device's pages admit only the owner of a live reservation
//     of the device (device reserve);
//   - the simulator's pages admit every owner while it is free, and only
//     the reserving owner while one reserves it (sim reserve,
//     simulator_turns.go, which adds the simulator's tab choice);
//   - other targets (WebKitGTK, local endpoints) admit every owner.
//
// The rule is applied on every admission (touch: a command, a CDP
// attachment's Touch, also on a connection and a lease that the daemon
// already holds), before the page observer streams the page's key and
// title; on every operation under a held lease (BeginLeased); on the CDP
// endpoint's issue and page list; on tab discovery (DeviceTabs); and in
// the sweep, which ends the leases of owners that the device no longer
// admits. A reservation that cannot be read admits nothing on a physical
// device or the simulator.

// admits applies the rule of device d (physical: physicalDevice) for
// owner: nil when owner may use its pages, a CodeReservation error when
// not (ErrNoReservation or ErrReserved) or when the reservation cannot be
// read (neither).
func (m *Manager) admits(d transport.Device, physical bool, owner string) error {
	switch {
	case physical:
		return m.physicalAdmits(d, owner)
	case d.ID == SimulatorDevice:
		return m.SimulatorAdmits(owner)
	}
	return nil
}

// physicalAdmits is admits for a physical device.
func (m *Manager) physicalAdmits(d transport.Device, owner string) error {
	if m.o.Reservations == nil {
		return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: reservations are not configured", ErrNoReservation)}
	}
	mine, other, err := m.o.Reservations.Check(d.ID, owner)
	if err != nil {
		return &CodedError{Code: CodeReservation, Err: err}
	}
	if mine != nil {
		return nil
	}
	if other != nil {
		return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: device %s is reserved by %q until %s; wait, or ask that owner", ErrReserved, d.Label, other.Owner, other.Expires.Format(time.RFC3339))}
	}
	return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w for %q: a physical device needs one before any page connection; run 'agent-ios device reserve --owner %s' (or set AGENT_IOS_DEVICE_RESERVATION to the owner of an existing reservation)", ErrNoReservation, owner, owner)}
}

// refused reports whether err of admits is a refusal, not a reservation
// that could not be read.
func refused(err error) bool {
	return errors.Is(err, ErrReserved) || errors.Is(err, ErrNoReservation)
}

// PageAdmits applies the rule of lp's device for owner, as admits does:
// the CDP endpoint lists a page only to an owner that it admits.
func (m *Manager) PageAdmits(lp ListedPage, owner string) error {
	return m.admits(lp.Device, lp.Physical, owner)
}

// admitTarget applies the rule of t's device to the admission of t,
// resolved from sreq (sessionPage's request), for resOwner, with the
// simulator's tab choice (admitSimulator).
func (m *Manager) admitTarget(ctx context.Context, sreq PageRequest, t pageTarget, resOwner string) error {
	if t.device.ID == SimulatorDevice && !t.physical {
		return m.admitSimulator(ctx, sreq, t, resOwner)
	}
	return m.admits(t.device, t.physical, resOwner)
}
