package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
)

// Regression tests for the second W1b review (docs/notes/w1b.md, "Review
// repairs, round 2").

// deadListing is a loopback address where nothing listens.
func deadListing(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// R2-2, D10: classifying an explicit page URL resolves the daemon's
// default transport (core.WithoutTransportStart) without the iwdp check or
// start; a request without the mark checks the listing as before.
func TestTransportForURLClassificationNeverChecksIWDP(t *testing.T) {
	starts := 0
	addr := deadListing(t)
	ts := &transports{o: RunOptions{Transport: "iwdp", Addr: addr, StartIWDP: func(context.Context) error { starts++; return nil }}, logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := ts.get(core.WithoutTransportStart(ctx), "auto", "")
	if err != nil {
		t.Fatalf("transport for URL classification: %v", err)
	}
	if it, ok := tr.(*iwdp.Transport); !ok || it.ListingAddr() != addr {
		t.Fatalf("transport %T does not read the custom listing %s", tr, addr)
	}
	if _, err := ts.get(ctx, "auto", ""); err == nil {
		t.Fatal("a dead custom listing passed the iwdp check")
	}
	if starts != 0 {
		t.Fatalf("iwdp started %d times for a custom listing address, want 0", starts)
	}
}
