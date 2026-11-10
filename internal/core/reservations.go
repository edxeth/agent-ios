package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// AnyDevice is the device of a reservation that covers every device.
const AnyDevice = "*"

// Reservation is a cooperative device reservation (design 2.5, rule 1):
// flock-protected JSON files that every iwdp-cli of this version
// honours. It does not stop clients that ignore it.
type Reservation struct {
	Device  string        `json:"device"` // UDID, or AnyDevice
	Owner   string        `json:"owner"`
	Created time.Time     `json:"created"`
	Expires time.Time     `json:"expires"`
	TTL     time.Duration `json:"ttl"`
}

// Live reports whether r has not expired at now.
func (r Reservation) Live(now time.Time) bool { return now.Before(r.Expires) }

// Covers reports whether r applies to device.
func (r Reservation) Covers(device string) bool {
	return r.Device == AnyDevice || r.Device == device || device == AnyDevice
}

// Reservations reads and writes the reservation files of one directory.
type Reservations struct {
	Dir string
	Now func() time.Time // nil: time.Now
}

func (rs *Reservations) now() time.Time {
	if rs.Now != nil {
		return rs.Now()
	}
	return time.Now()
}

// withLock runs fn while holding an exclusive flock on the directory's
// lock file. The directory is created with mode 0700.
func (rs *Reservations) withLock(fn func() error) error {
	if err := os.MkdirAll(rs.Dir, 0o700); err != nil {
		return fmt.Errorf("reservation directory %s: %w", rs.Dir, err)
	}
	f, err := os.OpenFile(filepath.Join(rs.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reservation lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("reservation lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func fileName(device string) string {
	if device == AnyDevice {
		return "any-device.json"
	}
	clean := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, device)
	return "device-" + clean + ".json"
}

// listLocked reads every reservation file; expired ones are included.
func (rs *Reservations) listLocked() ([]Reservation, error) {
	entries, err := os.ReadDir(rs.Dir)
	if err != nil {
		return nil, fmt.Errorf("reading reservations: %w", err)
	}
	var out []Reservation
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rs.Dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading reservation %s: %w", e.Name(), err)
		}
		var r Reservation
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("reservation %s is not valid JSON: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

func (rs *Reservations) writeLocked(r Reservation) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := filepath.Join(rs.Dir, ".tmp-"+fileName(r.Device))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing reservation: %w", err)
	}
	return os.Rename(tmp, filepath.Join(rs.Dir, fileName(r.Device)))
}

// ErrReserved: another owner holds a live reservation of the device.
var ErrReserved = errors.New("the device is reserved by another owner")

// Reserve creates or renews owner's reservation of device for ttl. It
// fails when another owner holds a live reservation that covers device.
func (rs *Reservations) Reserve(device, owner string, ttl time.Duration) (Reservation, error) {
	if owner == "" {
		return Reservation{}, fmt.Errorf("reserve: an owner name is required (--owner)")
	}
	if ttl <= 0 {
		return Reservation{}, fmt.Errorf("reserve: ttl %v must be positive", ttl)
	}
	var out Reservation
	err := rs.withLock(func() error {
		all, err := rs.listLocked()
		if err != nil {
			return err
		}
		now := rs.now()
		for _, r := range all {
			if r.Live(now) && r.Covers(device) && r.Owner != owner {
				return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: device %s is reserved by %q until %s", ErrReserved, deviceText(r.Device), r.Owner, r.Expires.Format(time.RFC3339))}
			}
		}
		out = Reservation{Device: device, Owner: owner, Created: now, Expires: now.Add(ttl), TTL: ttl}
		return rs.writeLocked(out)
	})
	return out, err
}

// Release removes owner's reservation of device. Releasing a reservation
// that does not exist is not an error; one of another owner is.
func (rs *Reservations) Release(device, owner string) error {
	return rs.withLock(func() error {
		p := filepath.Join(rs.Dir, fileName(device))
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading reservation: %w", err)
		}
		var r Reservation
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("reservation %s is not valid JSON: %w", p, err)
		}
		if r.Owner != owner && r.Live(rs.now()) {
			return &CodedError{Code: CodeReservation, Err: fmt.Errorf("%w: device %s is reserved by %q, not %q", ErrReserved, deviceText(device), r.Owner, owner)}
		}
		return os.Remove(p)
	})
}

// List returns the live reservations.
func (rs *Reservations) List() ([]Reservation, error) {
	var out []Reservation
	err := rs.withLock(func() error {
		all, err := rs.listLocked()
		now := rs.now()
		for _, r := range all {
			if r.Live(now) {
				out = append(out, r)
			}
		}
		return err
	})
	return out, err
}

// Check returns the live reservation that covers device: mine is owner's,
// other is another owner's. Both are nil when device is not reserved.
func (rs *Reservations) Check(device, owner string) (mine, other *Reservation, err error) {
	live, err := rs.List()
	if err != nil {
		return nil, nil, err
	}
	for i := range live {
		r := live[i]
		if !r.Covers(device) {
			continue
		}
		if owner != "" && r.Owner == owner {
			mine = &r
		} else if other == nil {
			other = &r
		}
	}
	return mine, other, nil
}

// Renew extends every live reservation of owner by its TTL from now; the
// daemon calls it while owner's leases are active.
func (rs *Reservations) Renew(owner string) error {
	return rs.withLock(func() error {
		all, err := rs.listLocked()
		if err != nil {
			return err
		}
		now := rs.now()
		for _, r := range all {
			if r.Owner == owner && r.Live(now) {
				r.Expires = now.Add(r.TTL)
				if err := rs.writeLocked(r); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func deviceText(d string) string {
	if d == AnyDevice {
		return "(any device)"
	}
	if len(d) > 8 {
		return "…" + strings.ToLower(d[len(d)-4:])
	}
	return d
}
