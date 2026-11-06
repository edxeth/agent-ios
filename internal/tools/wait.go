package tools

import (
	"context"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// waitForSignal waits until ch is closed or ctx ends. When ctx has no
// deadline, it waits at most webkit.DefaultSendTimeout, so a page that never
// sends the awaited event (for example *.trackingComplete) cannot block the
// caller forever. The callers then return the events collected so far.
func waitForSignal(ctx context.Context, ch <-chan struct{}) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, webkit.DefaultSendTimeout)
		defer cancel()
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
}
