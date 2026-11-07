package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// defaultReservationTTL is the lifetime of a device reservation; the daemon
// renews it while the owner holds leases (design 2.5 rule 1).
const defaultReservationTTL = "2h"

// Device reservation commands (design 2.5 rule 1, decision D11).
func init() {
	deviceFlag := Flag{Name: "device", Type: FlagString, Default: core.AnyDevice, Help: "device UDID; * reserves every device"}
	ownerFlag := Flag{Name: "owner", Type: FlagString, Help: "reservation owner; default --reservation (IWDP_DEVICE_RESERVATION), else the session"}
	Register(&Command{
		Name: "device reserve", Summary: "Reserve a physical device for an owner, so that only that owner's sessions attach to its pages.",
		Flags: []Flag{deviceFlag, ownerFlag, {Name: "ttl", Type: FlagDuration, Default: defaultReservationTTL, Help: "lifetime, renewed while the owner holds leases"}},
		Run: func(_ context.Context, env Env, in Input) (Result, error) {
			rs, err := reservationsOf(env)
			if err != nil {
				return Result{}, err
			}
			r, err := rs.Reserve(in.Flags["device"].(string), reservationOwner(in), in.Flags["ttl"].(time.Duration))
			if err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Device %s reserved by %q until %s.", r.Device, r.Owner, r.Expires.Format(time.RFC3339)), Data: r}, nil
		},
	})
	Register(&Command{
		Name: "device release", Summary: "Release an owner's device reservation.",
		Flags: []Flag{deviceFlag, ownerFlag},
		Run: func(_ context.Context, env Env, in Input) (Result, error) {
			rs, err := reservationsOf(env)
			if err != nil {
				return Result{}, err
			}
			dev, owner := in.Flags["device"].(string), reservationOwner(in)
			if err := rs.Release(dev, owner); err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Reservation of device %s by %q released.", dev, owner)}, nil
		},
	})
	Register(&Command{
		Name: "device list", Summary: "List the live device reservations.",
		Run: func(_ context.Context, env Env, _ Input) (Result, error) {
			rs, err := reservationsOf(env)
			if err != nil {
				return Result{}, err
			}
			list, err := rs.List()
			if err != nil {
				return Result{}, err
			}
			var b strings.Builder
			for _, r := range list {
				fmt.Fprintf(&b, "%s\treserved by %q until %s\n", r.Device, r.Owner, r.Expires.Format(time.RFC3339))
			}
			if len(list) == 0 {
				b.WriteString("No device is reserved.")
			}
			return Result{Text: b.String(), Data: list}, nil
		},
	})
}

func reservationOwner(in Input) string {
	if o, _ := in.Flags["owner"].(string); o != "" {
		return o
	}
	if in.Global.Reservation != "" {
		return in.Global.Reservation
	}
	return in.Global.Session
}
