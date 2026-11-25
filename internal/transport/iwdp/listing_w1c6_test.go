package iwdp_test

import (
	"testing"

	"github.com/edxeth/agent-ios/internal/proxy"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
)

// W1c T01: the transport's default device list is proxy.ListingAddr, the
// one listing address of a binary (127.0.0.1:9221 unless the e2e tests
// link another), so a daemon reads the same iwdp as list_devices.
func TestDefaultListingIsTheProxyListing(t *testing.T) {
	if got := iwdp.New(iwdp.Options{}).ListingAddr(); got != proxy.ListingAddr() || got != "127.0.0.1:9221" {
		t.Fatalf("default listing = %s, want proxy.ListingAddr() %s (127.0.0.1:9221)", got, proxy.ListingAddr())
	}
}
