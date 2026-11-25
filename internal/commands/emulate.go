package commands

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/devtools"
)

// emulate network: network throttling (design
// docs/design/agentic-parity.md, section 4.2 "emulate network <preset>").
// The throttle is desired state owned by the
// session: a reconnect or a new target gets it again, and the release of
// the lease removes it.
func init() {
	Register(&Command{
		Name:    "emulate network",
		Summary: "Throttle the page's network to a preset (" + strings.Join(networkPresets(), ", ") + ") or a byte rate; 'none' removes the throttle. Uses Network.setEmulatedConditions, which only the iOS 16.4 to 17.x and 27.3+ command lists have.",
		Args:    []Arg{{Name: "preset", Help: strings.Join(networkPresets(), ", ") + " or none"}},
		Flags: []Flag{
			// The underscore name is the parameter of the earlier tool
			// set_emulated_conditions (design 4.1).
			{Name: "bytes_per_second_limit", Type: FlagInt, Help: "a rate in bytes per second instead of a preset; 0 removes the throttle"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runEmulateNetwork,
	})
}

// networkPresets are the preset names in a stable order.
func networkPresets() []string {
	names := make([]string, 0, len(devtools.NetworkPresets))
	for n := range devtools.NetworkPresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func runEmulateNetwork(ctx context.Context, env Env, in Input) (Result, error) {
	preset := arg(in, 0)
	rate, hasRate := in.Flags["bytes_per_second_limit"].(int)
	switch {
	case preset == "" && !hasRate, preset != "" && hasRate:
		return Result{}, usagef("emulate network: give a preset (%s or none) or --bytes_per_second_limit", strings.Join(networkPresets(), ", "))
	case hasRate && rate < 0:
		return Result{}, usagef("emulate network: --bytes_per_second_limit %d: want 0 (no limit) or more", rate)
	case preset != "" && preset != "none" && !slices.Contains(networkPresets(), preset):
		return Result{}, usagef("emulate network %q: want %s or none", preset, strings.Join(networkPresets(), ", "))
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if hasRate {
		if err := d.p.EmulateNetworkRate(ctx, d.c, d.session, rate); err != nil {
			return Result{}, err
		}
		if rate == 0 {
			return Result{Text: "Network throttle removed."}, nil
		}
		return Result{Text: fmt.Sprintf("Network throttled to %d bytes per second.", rate)}, nil
	}
	if err := d.p.EmulateNetwork(ctx, d.c, d.session, preset); err != nil {
		return Result{}, err
	}
	if preset == "none" {
		return Result{Text: "Network throttle removed."}, nil
	}
	return Result{Text: fmt.Sprintf("Network throttled to %s (%d bytes per second).", preset, devtools.NetworkPresets[preset])}, nil
}
