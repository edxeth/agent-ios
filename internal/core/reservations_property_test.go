package core

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"testing/quick"
	"time"
)

// resModel is the oracle of TestReservationTransitionsProperty: the
// reservation rules as the design states them (2.5 rule 1, and the
// simulator's turns), over plain maps, without the store's files.
type resModel struct {
	now   time.Time
	files map[string]modelRes // device -> its reservation (one file per device)
}

type modelRes struct {
	owner   string
	expires time.Time
	ttl     time.Duration
}

// modelCovers: a reservation of the simulator covers the simulator alone,
// and nothing else covers it; "*" covers every physical device.
func modelCovers(reserved, device string) bool {
	if reserved == SimulatorDevice || device == SimulatorDevice {
		return reserved == device
	}
	return reserved == AnyDevice || device == AnyDevice || reserved == device
}

func (m *resModel) live(device string) (modelRes, bool) {
	r, ok := m.files[device]
	return r, ok && m.now.Before(r.expires)
}

// holders are the owners of the live reservations that cover device, by
// device id (Check's order).
func (m *resModel) holders(device string) []string {
	var devs []string
	for d := range m.files {
		if _, ok := m.live(d); ok && modelCovers(d, device) {
			devs = append(devs, d)
		}
	}
	sort.Strings(devs)
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = m.files[d].owner
	}
	return out
}

func (m *resModel) reserve(device, owner string, ttl time.Duration) bool {
	for _, o := range m.holders(device) {
		if o != owner {
			return false
		}
	}
	m.files[device] = modelRes{owner, m.now.Add(ttl), ttl}
	return true
}

func (m *resModel) release(device, owner string) bool {
	r, ok := m.files[device]
	if !ok {
		return true
	}
	if r.owner != owner && m.now.Before(r.expires) {
		return false
	}
	delete(m.files, device)
	return true
}

func (m *resModel) renew(owner, device string) bool {
	renewed := false
	for d, r := range m.files {
		if _, ok := m.live(d); ok && r.owner == owner && modelCovers(d, device) {
			r.expires = m.now.Add(r.ttl)
			m.files[d] = r
			renewed = true
		}
	}
	return renewed
}

// resStep is one generated step: an operation, a device, an owner, a TTL
// and a clock advance, each picked from a small set so that owners and
// devices collide.
type resStep struct{ Op, Device, Owner, TTL, Advance uint8 }

var (
	propDevices = []string{SimulatorDevice, "udid-1", "udid-2", AnyDevice}
	propOwners  = []string{"a", "b", "c"}
	propTTLs    = []time.Duration{30 * time.Second, 2 * time.Minute, time.Hour}
	propAdvance = []time.Duration{0, 10 * time.Second, 31 * time.Second, 2*time.Minute + time.Second}
)

func (s resStep) String() string {
	return fmt.Sprintf("%s(%s,%s,ttl %s)+%s", [...]string{"reserve", "release", "renew", "wait"}[s.Op%4],
		propDevices[int(s.Device)%len(propDevices)], propOwners[int(s.Owner)%len(propOwners)],
		propTTLs[int(s.TTL)%len(propTTLs)], propAdvance[int(s.Advance)%len(propAdvance)])
}

// Review F13: generated sequences of reserve, release, renew (the
// sweep's and a running sim command's) and expiry by competing owners on
// the simulator and physical devices, against the Reservations store, a
// Manager's simulator admission and the model above. After each step:
//   - each operation succeeds exactly when the model's does, and Renew
//     reports a renewal exactly when the model renews;
//   - Check of every device and owner gives the model's own and other
//     reservation;
//   - no device is covered by live reservations of two owners in the
//     store (mutual exclusion), and the simulator's reservation never
//     covers a physical device nor the reverse;
//   - SimulatorAdmits(owner) admits exactly while the simulator is free or
//     owner holds it.
func TestReservationTransitionsProperty(t *testing.T) {
	prop := func(steps []resStep) bool {
		if len(steps) > 24 {
			steps = steps[:24]
		}
		start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		model := &resModel{now: start, files: map[string]modelRes{}}
		rs := &Reservations{Dir: t.TempDir(), Now: func() time.Time { return model.now }}
		m := &Manager{o: ManagerOptions{Reservations: rs}}
		for i, s := range steps {
			dev := propDevices[int(s.Device)%len(propDevices)]
			owner := propOwners[int(s.Owner)%len(propOwners)]
			ttl := propTTLs[int(s.TTL)%len(propTTLs)]
			var got, want bool
			switch s.Op % 4 {
			case 0:
				_, err := rs.Reserve(dev, owner, ttl)
				if err != nil && !errors.Is(err, ErrReserved) {
					t.Errorf("step %d %v: %v", i, s, err)
					return false
				}
				got, want = err == nil, model.reserve(dev, owner, ttl)
			case 1:
				err := rs.Release(dev, owner)
				if err != nil && !errors.Is(err, ErrReserved) {
					t.Errorf("step %d %v: %v", i, s, err)
					return false
				}
				got, want = err == nil, model.release(dev, owner)
			case 2:
				ok, err := rs.Renew(owner, dev)
				if err != nil {
					t.Errorf("step %d %v: %v", i, s, err)
					return false
				}
				got, want = ok, model.renew(owner, dev)
			}
			if got != want {
				t.Errorf("steps %v: step %d succeeded %t, the model %t", steps, i, got, want)
				return false
			}
			model.now = model.now.Add(propAdvance[int(s.Advance)%len(propAdvance)])
			if why := checkResModel(rs, m, model); why != "" {
				t.Errorf("steps %v: after step %d: %s", steps, i, why)
				return false
			}
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261124))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// checkResModel compares the store and the manager with the model; "" when
// they agree.
func checkResModel(rs *Reservations, m *Manager, model *resModel) string {
	live, err := rs.List()
	if err != nil {
		return fmt.Sprintf("List: %v", err)
	}
	for _, dev := range propDevices {
		if dev == AnyDevice {
			continue // no device: reservations of two physical devices both cover it
		}
		owners := map[string]bool{}
		for _, r := range live {
			if modelCovers(r.Device, dev) {
				owners[r.Owner] = true
			}
		}
		if len(owners) > 1 {
			return fmt.Sprintf("device %s is held by %v at once", dev, owners)
		}
	}
	for _, dev := range propDevices {
		holders := model.holders(dev)
		for _, owner := range propOwners {
			mine, other, err := rs.Check(dev, owner)
			if err != nil {
				return fmt.Sprintf("Check(%s, %s): %v", dev, owner, err)
			}
			wantMine, wantOther := "", ""
			for _, h := range holders {
				if h == owner {
					wantMine = h
				} else if wantOther == "" {
					wantOther = h
				}
			}
			gotMine, gotOther := "", ""
			if mine != nil {
				gotMine = mine.Owner
				if (mine.Device == SimulatorDevice) != (dev == SimulatorDevice) {
					return fmt.Sprintf("Check(%s, %s) gave the reservation of %s", dev, owner, mine.Device)
				}
			}
			if other != nil {
				gotOther = other.Owner
			}
			if gotMine != wantMine || gotOther != wantOther {
				return fmt.Sprintf("Check(%s, %s) = mine %q, other %q; the model: %q, %q", dev, owner, gotMine, gotOther, wantMine, wantOther)
			}
			if dev != SimulatorDevice {
				continue
			}
			admitted := m.SimulatorAdmits(owner) == nil
			if want := len(holders) == 0 || holders[0] == owner; admitted != want {
				return fmt.Sprintf("SimulatorAdmits(%s) = %t with holders %v", owner, admitted, holders)
			}
		}
	}
	return ""
}
