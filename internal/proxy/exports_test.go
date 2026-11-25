package proxy

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

// ownsListing says "not listening" for a free address and "not own" for a
// listener that agent-ios did not start and record: sim up refuses such an
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

// EndpointOwner with no group (pgid 0) reports a listener as foreign and
// scans no process group: on Linux group 0 holds the kernel threads, whose
// fds a user cannot read, which made the check fail (live run, 2026-11-24).
func TestEndpointOwnerWithoutAGroup(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ep := netip.MustParseAddrPort(l.Addr().String())
	for _, pgid := range []int{0, -5} {
		own, foreign, err := EndpointOwner(context.Background(), pgid, ep)
		if err != nil || own || !foreign {
			t.Fatalf("pgid %d: own %v, foreign %v, %v; want foreign", pgid, own, foreign, err)
		}
	}
}
