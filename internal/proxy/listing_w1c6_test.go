package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// The link-time seam listingAddr (W1c T01) defaults to iwdp's listing
// address: the default Starter and ListDevicesContext use 127.0.0.1:9221.
func TestListingAddrIsTheDefaultListing(t *testing.T) {
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultListingPort))
	if listingAddr != want || std.addr != want || ListingAddr() != want {
		t.Fatalf("listingAddr = %s, std.addr = %s, ListingAddr() = %s, want %s", listingAddr, std.addr, ListingAddr(), want)
	}
}

// ListDevicesContext reads the device list at listingAddr, so a binary
// linked with another address (the e2e tests) never reads 127.0.0.1:9221.
func TestListDevicesReadsTheListingAddr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"deviceId":"udid-1","deviceName":"Fake","url":"127.0.0.1:9300"}]`))
	}))
	defer srv.Close()
	saved := listingAddr
	listingAddr = strings.TrimPrefix(srv.URL, "http://")
	t.Cleanup(func() { listingAddr = saved })
	got, err := ListDevicesContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := webkit.DeviceEntry{DeviceID: "udid-1", DeviceName: "Fake", URL: "127.0.0.1:9300"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ListDevicesContext = %+v, want [%+v] from %s", got, want, listingAddr)
	}
}
