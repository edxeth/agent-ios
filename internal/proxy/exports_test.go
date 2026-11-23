package proxy

import (
	"context"
	"net"
	"testing"
)

// ownsListing says "not listening" for a free address and "not own" for a
// listener that iwdp-cli did not start and record: sim up refuses such an
// iwdp instead of trusting where it looks for a simulator.
func TestOwnsListingOfAFreeAndAForeignAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	s := NewStarter(addr)
	listening, own, err := s.ownsListing(context.Background())
	if err != nil || !listening || own {
		t.Fatalf("foreign listener: listening %v, own %v, %v; want listening, not own", listening, own, err)
	}
	_ = l.Close()
	listening, own, err = s.ownsListing(context.Background())
	if err != nil || listening || own {
		t.Fatalf("free address: listening %v, own %v, %v; want neither", listening, own, err)
	}
}
