package core

import (
	"context"
	"fmt"
	"time"
)

// Agents taking turns on the simulator (iwdp-cli sim reserve): while an
// owner reserves the simulator, its pages are that owner's run. The rules
// hold on every way to a simulator page, not only on a first connection
// (device_admission.go, which applies them with the physical devices'):
//
//   - every admission (touch: a command, a CDP attachment's Touch, also on
//     a connection and a lease that the daemon already holds) and every CDP
//     operation under a held lease (BeginLeased) of another owner fails
//     with CodeReservation, before any tab choice;
//   - an owner that holds the reservation and has chosen no tab (no --page,
//     no 'tab <id>') is refused with CodeNoPage while the simulator shows
//     more than one tab, also when the session used a page implicitly
//     before the others appeared: the others can be earlier agents' tabs;
//   - discovery (tab list, tab find, the CDP endpoint's issue and page
//     list) hides the simulator's pages from another owner;
//   - the sweep ends the leases of other owners on the simulator's pages.
//
// A reservation that cannot be read admits nothing on the simulator.

// SimulatorAdmits reports whether owner may use the simulator's pages: nil
// while the simulator is free or owner reserves it, a CodeReservation
// error while another owner reserves it or when the reservation cannot be
// read.
func (m *Manager) SimulatorAdmits(owner string) error {
	_, err := m.simulatorTurn(owner)
	return err
}

// HoldsSimulator reports whether the reservation owner of ao (its session
// when it names none) holds the simulator's reservation; an error when the
// reservation cannot be read or another owner holds it, as SimulatorAdmits.
func (m *Manager) HoldsSimulator(ao AcquireOptions) (bool, error) {
	return m.simulatorTurn(ao.normalized().Reservation)
}

// simulatorTurn is SimulatorAdmits that also reports whether owner holds
// the reservation.
func (m *Manager) simulatorTurn(owner string) (mine bool, err error) {
	if m.o.Reservations == nil {
		return false, nil
	}
	r, other, err := m.o.Reservations.Check(SimulatorDevice, owner)
	if err != nil {
		return false, &CodedError{Code: CodeReservation, Err: fmt.Errorf("reading the simulator's reservation: %w", err)}
	}
	if r == nil && other != nil {
		return false, &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: the simulator is reserved by %q until %s, so its pages belong to that owner's run: run 'iwdp-cli sim reserve' (it waits for that owner), or use --reservation <owner> if your run already holds the reservation under another name", ErrReserved, other.Owner, other.Expires.Format(time.RFC3339))}
	}
	return r != nil, nil
}

// admitSimulator applies the rules above to the admission of t, a page of
// the simulator, resolved from sreq (sessionPage's request: Page is "" when
// the session chose no page), for resOwner (admitTarget).
func (m *Manager) admitSimulator(ctx context.Context, sreq PageRequest, t pageTarget, resOwner string) error {
	mine, err := m.simulatorTurn(resOwner)
	if err != nil || !mine || sreq.Page != "" {
		return err
	}
	n, err := m.simulatorTabs(ctx, t.listing)
	if err != nil {
		return err
	}
	if n > 1 {
		return &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: the simulator shows %d tabs, and this session has chosen none; the others may be other agents' work: find yours with 'iwdp-cli tab find --device SIMULATOR --url-file <file>' and select it with 'iwdp-cli tab <id>', or pass --page <id>", ErrNoPage, n)}
	}
	return nil
}

// simulatorTabs counts the pages, debuggable or not, that the listing of
// req shows for the simulator. A listing that fails is an error: it proves
// no single tab.
func (m *Manager) simulatorTabs(ctx context.Context, req PageRequest) (int, error) {
	kind := req.Transport
	if kind == "" {
		kind = "auto"
	}
	if m.o.Transport == nil {
		return 0, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no transport is configured", ErrNoPage)}
	}
	tr, err := m.o.Transport(ctx, kind, req.Addr)
	if err != nil {
		return 0, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("counting the simulator's tabs: %w", m.listingFailed(err))}
	}
	pages, err := tr.Pages(ctx, SimulatorDevice)
	if err != nil {
		return 0, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("counting the simulator's tabs: %w", m.listingFailed(err))}
	}
	return len(pages), nil
}
