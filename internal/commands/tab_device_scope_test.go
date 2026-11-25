package commands

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/transport"
)

// scopedDevices is an iwdp listing of a phone and the simulator whose page
// lists can fail one at a time.
type scopedDevices struct {
	sim, phone         *tabFake
	phoneFail, simFail atomic.Bool
}

func (d *scopedDevices) Kind() transport.Kind { return transport.KindIWDP }

func (d *scopedDevices) Devices(context.Context) ([]transport.Device, error) {
	return []transport.Device{
		{ID: "phone-1", Label: "Phone", Transport: transport.KindIWDP},
		{ID: core.SimulatorDevice, Label: "Simulator", Transport: transport.KindIWDP},
	}, nil
}

func (d *scopedDevices) Pages(ctx context.Context, id string) ([]transport.Page, error) {
	if id == "phone-1" {
		if d.phoneFail.Load() {
			return nil, errors.New("phone page list unavailable")
		}
		return d.phone.Pages(ctx, id)
	}
	if d.simFail.Load() {
		return nil, errors.New("simulator page list unavailable")
	}
	return d.sim.Pages(ctx, id)
}

func (d *scopedDevices) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	if p.DeviceID == "phone-1" {
		return d.phone.Open(ctx, p)
	}
	return d.sim.Open(ctx, p)
}

func (d *scopedDevices) Close() error { return nil }

func newScopedDevices(t *testing.T) (*core.Manager, *core.Reservations, *scopedDevices) {
	t.Helper()
	res := &core.Reservations{Dir: t.TempDir()}
	d := &scopedDevices{
		sim:   &tabFake{t: t, kind: transport.KindIWDP, device: core.SimulatorDevice},
		phone: &tabFake{t: t, kind: transport.KindIWDP, device: "phone-1"},
	}
	d.phone.add("Phone page", "http://phone.test/")
	d.sim.add("Simulator page", "http://sim.test/")
	m := core.NewManager(core.ManagerOptions{Reservations: res, Transport: func(context.Context, string, string) (transport.Transport, error) { return d, nil }})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, res, d
}

// Review F6: with --device, only that device's pages are listed: another
// device's failed page list (here while the daemon holds a page of it,
// which makes it an error of an unscoped listing) does not stop the
// scoped one, in either direction.
func TestDeviceScopedListingIgnoresOtherDevicesFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Run("simulator scope, phone fails", func(t *testing.T) {
		m, res, d := newScopedDevices(t)
		if _, err := res.Reserve("phone-1", "phone", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.Touch(ctx, core.PageRequest{Page: "Phone page"}, core.AcquireOptions{Session: "phone"}); err != nil {
			t.Fatal(err)
		}
		d.phoneFail.Store(true)
		tabs, hidden, err := m.DeviceTabs(ctx, core.PageRequest{}, core.AcquireOptions{Session: "sim"}, core.SimulatorDevice)
		if err != nil || len(tabs) != 1 || hidden != 0 {
			t.Fatalf("simulator tabs while the phone's page list fails: %+v, hidden %d, err %v", tabs, hidden, err)
		}
	})
	t.Run("phone scope, simulator fails", func(t *testing.T) {
		m, res, d := newScopedDevices(t)
		if _, err := res.Reserve("phone-1", "phone", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.Touch(ctx, core.PageRequest{Page: "Simulator page"}, core.AcquireOptions{Session: "sim"}); err != nil {
			t.Fatal(err)
		}
		d.simFail.Store(true)
		tabs, hidden, err := m.DeviceTabs(ctx, core.PageRequest{}, core.AcquireOptions{Session: "phone"}, "phone-1")
		if err != nil || len(tabs) != 1 || hidden != 0 {
			t.Fatalf("phone tabs while the simulator's page list fails: %+v, hidden %d, err %v", tabs, hidden, err)
		}
	})
}

// Review F16: the selected device's own failed page list is an error,
// also without an attached page of it: tab list does not show it as a
// device without tabs, and tab find does not say that no tab shows the
// target (exit 7).
func TestDeviceScopedListingFailureIsAnError(t *testing.T) {
	m, _, d := newScopedDevices(t)
	d.simFail.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if tabs, _, err := m.DeviceTabs(ctx, core.PageRequest{}, core.AcquireOptions{Session: "sim"}, core.SimulatorDevice); err == nil {
		t.Fatalf("a failed simulator page list read as %d tab(s)", len(tabs))
	}
	_, err := runIn(t, m, "sim", "tab", "find", "--device", "SIMULATOR", "http://sim.test/")
	if ExitCodeOf(err) != core.CodeNoPage {
		t.Fatalf("tab find with the simulator's page list failing: %v (exit %d), want the failed listing (exit 5), not exit 7", err, ExitCodeOf(err))
	}
}
