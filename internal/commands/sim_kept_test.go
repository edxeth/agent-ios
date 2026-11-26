package commands

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/edxeth/agent-ios/internal/proxy"
	"github.com/edxeth/agent-ios/internal/sim"
)

// Review F4 of 2026-11-26: sim up tells a kept, still connecting iwdp
// from a failed start (sim.ErrIWDPKept), and keeps the start's message
// and its context error; another failure is no kept start.
func TestIWDPStartErrorMarksAKeptStart(t *testing.T) {
	kept := fmt.Errorf("%w: the started ios_webkit_debug_proxy (pid 7) listens (%w)", proxy.ErrStillConnecting, context.DeadlineExceeded)
	err := iwdpStartError(kept)
	if !errors.Is(err, sim.ErrIWDPKept) || !errors.Is(err, context.DeadlineExceeded) || err.Error() != kept.Error() {
		t.Fatalf("iwdpStartError(kept) = %v; want sim.ErrIWDPKept, the deadline and the same message", err)
	}
	if err := iwdpStartError(errors.New("ios_webkit_debug_proxy exited during its start")); errors.Is(err, sim.ErrIWDPKept) {
		t.Fatalf("a failed start counts as kept: %v", err)
	}
	if err := iwdpStartError(nil); err != nil {
		t.Fatalf("iwdpStartError(nil) = %v", err)
	}
}
