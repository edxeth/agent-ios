package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
)

// W1c T02: no two tests share a daemon. A test that reaches the daemon
// without privateDaemonDir gets an error from the binary's default socket
// directory instead of a daemon that the next test would see too.
func TestSharedDaemonDirStartsNoDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, mcpDaemonOptions())
	if err == nil {
		c.Close()
		stopDaemonIn(daemon.SocketDir())
		t.Fatalf("a daemon started in the shared socket directory %s", daemon.SocketDir())
	}
	if !errors.Is(err, daemon.ErrUnsafeDir) {
		t.Fatalf("daemon.Connect in the shared directory: %v, want %v", err, daemon.ErrUnsafeDir)
	}
}
