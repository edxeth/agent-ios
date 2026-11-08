package main

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
)

// Review round 2, F8: a restart whose old iwdp did not stop (the SIGKILL
// escalation failed, or its process group did not end) reports
// restarted false and keeps the page client: the old iwdp may still serve
// that page.
func TestRestartIWDPToolKeepsTheClientWhenTheStopFails(t *testing.T) {
	old := restartIWDP
	t.Cleanup(func() { restartIWDP = old })
	cs := mcpServerSession(t)
	for _, stopErr := range []error{
		fmt.Errorf("%w: ios_webkit_debug_proxy (process group 4242) did not exit after SIGTERM, and SIGKILL failed: %w", proxy.ErrStopFailed, syscall.EPERM),
		fmt.Errorf("%w: ios_webkit_debug_proxy (process group 4242) is still running 2s after SIGKILL", proxy.ErrStopFailed),
	} {
		client := &closeCounter{}
		sess.mu.Lock()
		sess.client = client
		sess.mu.Unlock()
		restartIWDP = func(context.Context) error { return stopErr }
		_, text := callTool(t, cs, "restart_iwdp", nil)
		if !strings.Contains(text, `"restarted":false`) || !strings.Contains(text, "process group 4242") {
			t.Errorf("restart_iwdp with a failed stop = %s, want restarted false and the error", text)
		}
		sess.mu.Lock()
		kept := sess.client == pageClient(client)
		sess.mu.Unlock()
		if !kept || client.closed != 0 {
			t.Errorf("restart_iwdp with a failed stop (%v) changed the page client (kept %v, closed %d times)", stopErr, kept, client.closed)
		}
	}
}
