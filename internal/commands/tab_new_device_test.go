package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Decision D22 (tab new device scope): tab new of a session without a tab
// takes its opener, and so the new tab, only from the devices that the
// caller reserves, from one device only, or from the device of --device.

const phoneUDID = "00008020-0000000000000001"

// newPhoneAndSimulator is an iwdp listing of the free simulator (listed
// first, with one tab) and a phone (one tab) that owner reserves.
func newPhoneAndSimulator(t *testing.T, owner string) (m *core.Manager, f *tabFake, sim, phone *fakewebkit.Server) {
	t.Helper()
	res := &core.Reservations{Dir: t.TempDir()}
	m, f = newTabManagerWith(t, res)
	f.kind, f.device, f.second = transport.KindIWDP, core.SimulatorDevice, phoneUDID
	if _, err := res.Reserve(phoneUDID, owner, time.Hour); err != nil {
		t.Fatal(err)
	}
	sim = f.add("Simulator tab", "http://sim.test/")
	phone = f.addOn(phoneUDID, "Phone tab", "http://phone.test/")
	return m, f, sim, phone
}

// deviceOfTab is the device id of the tab titled title in session's tab
// list ("" when none).
func deviceOfTab(t *testing.T, m *core.Manager, session, title string) string {
	t.Helper()
	tabs, _, err := m.Tabs(context.Background(), core.PageRequest{}, core.AcquireOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	for _, tab := range tabs {
		if tab.Title == title {
			return tab.DeviceID
		}
	}
	return ""
}

// D22: with a reserved phone and a free simulator listed first, tab new
// without a tab opens from the phone, never from the simulator.
func TestTabNewWithoutTabOpensOnTheReservedDevice(t *testing.T) {
	m, _, sim, phone := newPhoneAndSimulator(t, "a")
	out := mustIn(t, m, "a", "tab", "new", "http://mine.test/")
	if !strings.Contains(out, "(http://mine.test/); it is now the current page") {
		t.Fatalf("tab new: %q", out)
	}
	if n, w := windowOpens(sim), windowOpens(phone); n != 0 || w != 1 {
		t.Fatalf("window.open calls: %d on the simulator, %d on the phone; want 0 and 1", n, w)
	}
	if n := sim.ConnectionCount(); n != 0 {
		t.Errorf("the simulator tab got %d connections; want none", n)
	}
	if got := deviceOfTab(t, m, "a", "Opened"); got != phoneUDID {
		t.Fatalf("the new tab is on device %q; want the phone", got)
	}
}

// D22 (watchout of the tab-choice repair): when the reserved phone's only
// tab does not answer (a hidden Safari tab), tab new fails with exit 4 and
// does not fall back to the free simulator.
func TestTabNewWithoutTabDoesNotFallBackToTheSimulator(t *testing.T) {
	m, _, sim, phone := newPhoneAndSimulator(t, "a")
	phone.Stall("Runtime.evaluate")
	_, err := runIn(t, m, "a", "tab", "new", "http://mine.test/")
	if ExitCodeOf(err) != ExitBusy || !strings.Contains(err.Error(), "no tab can run window.open") {
		t.Fatalf("tab new: %v (exit %d); want exit 4, no tab can run window.open", err, ExitCodeOf(err))
	}
	if n := sim.ConnectionCount(); n != 0 || windowOpens(sim) != 0 {
		t.Fatalf("the simulator tab got %d connections and %d window.open calls; want none", n, windowOpens(sim))
	}
}

// D22: when the reserved phone lists no tab, tab new exits 5 and does not
// open the tab on the free simulator.
func TestTabNewWithoutTabRefusesWhenTheReservedDeviceHasNoTab(t *testing.T) {
	res := &core.Reservations{Dir: t.TempDir()}
	m, f := newTabManagerWith(t, res)
	f.kind, f.device, f.second = transport.KindIWDP, core.SimulatorDevice, phoneUDID
	if _, err := res.Reserve(phoneUDID, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	sim := f.add("Simulator tab", "http://sim.test/")
	f.add("Simulator tab 2", "http://sim2.test/")
	_, err := runIn(t, m, "a", "tab", "new", "http://mine.test/")
	if ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), "reserves list no debuggable tab") {
		t.Fatalf("tab new: %v (exit %d); want exit 5, the reserved devices list no tab", err, ExitCodeOf(err))
	}
	if n := sim.ConnectionCount(); n != 0 {
		t.Fatalf("the simulator tab got %d connections; want none", n)
	}
}

// D22: without a reservation, tabs of two devices are candidates: tab new
// refuses with exit 5, names both devices and --device, and touches no
// page.
func TestTabNewWithoutTabRefusesTwoDevices(t *testing.T) {
	m, f := newTabManager(t)
	f.second = "other"
	one := f.add("One", "http://one.test/")
	two := f.addOn("other", "Two", "http://two.test/")
	_, err := runIn(t, m, "a", "tab", "new", "http://mine.test/")
	if ExitCodeOf(err) != ExitNoPage {
		t.Fatalf("tab new: %v (exit %d); want exit 5", err, ExitCodeOf(err))
	}
	for _, want := range []string{"fake", "other", "--device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("tab new: error %q does not say %q", err, want)
		}
	}
	for name, s := range map[string]*fakewebkit.Server{"One": one, "Two": two} {
		if n := s.ConnectionCount(); n != 0 {
			t.Errorf("page %s got %d connections; want none", name, n)
		}
	}
}

// D22: --device SIMULATOR takes the opener and the new tab from the
// simulator, also while the caller reserves a phone.
func TestTabNewDeviceSimulator(t *testing.T) {
	m, _, sim, phone := newPhoneAndSimulator(t, "a")
	out := mustIn(t, m, "a", "tab", "new", "--device", "SIMULATOR", "http://mine.test/")
	if !strings.Contains(out, "(http://mine.test/); it is now the current page") {
		t.Fatalf("tab new --device SIMULATOR: %q", out)
	}
	if n, w := windowOpens(sim), windowOpens(phone); n != 1 || w != 0 {
		t.Fatalf("window.open calls: %d on the simulator, %d on the phone; want 1 and 0", n, w)
	}
	if got := deviceOfTab(t, m, "a", "Opened"); got != core.SimulatorDevice {
		t.Fatalf("the new tab is on device %q; want the simulator", got)
	}
}

// D22: --device <udid> takes the opener from that device, also when the
// session's tab is on another device; a --page of another device is
// refused (exit 5).
func TestTabNewDeviceUDID(t *testing.T) {
	m, _, sim, phone := newPhoneAndSimulator(t, "a")
	mustIn(t, m, "a", "tab", "list")
	mustIn(t, m, "a", "tab", "t1") // the simulator tab
	if _, err := runIn(t, m, "a", "--page", "t1", "tab", "new", "--device", phoneUDID, "http://x.test/"); ExitCodeOf(err) != ExitNoPage || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("tab new --page <simulator tab> --device <phone>: %v (exit %d); want exit 5", err, ExitCodeOf(err))
	}
	out := mustIn(t, m, "a", "tab", "new", "--device", phoneUDID, "http://mine.test/")
	if !strings.Contains(out, "(http://mine.test/); it is now the current page") {
		t.Fatalf("tab new --device <udid>: %q", out)
	}
	if n, w := windowOpens(sim), windowOpens(phone); n != 0 || w != 1 {
		t.Fatalf("window.open calls: %d on the simulator, %d on the phone; want 0 and 1", n, w)
	}
	if got := deviceOfTab(t, m, "a", "Opened"); got != phoneUDID {
		t.Fatalf("the new tab is on device %q; want the phone", got)
	}
}
