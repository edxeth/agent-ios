package daemon

import (
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// A restart of iwdp ends every page connection, the simulator's too, so
// sim up counts the active leases of every device, and only active ones.
func TestActiveLeasesCountsEveryDevice(t *testing.T) {
	st := Status{Pages: []core.PageStatus{
		{Device: "SIMULATOR …ator", Lease: "active"},
		{Device: "iPhone X …cbf", Lease: "active"},
		{Device: "iPhone X …cbf", Lease: "lost"},
		{Device: "SIMULATOR …ator", Lease: "none"},
	}}
	if n := activeLeases(st); n != 2 {
		t.Fatalf("activeLeases = %d, want 2", n)
	}
}
