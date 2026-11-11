package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// proxyListingVar is the link-time seam of internal/proxy: the iwdp
// listing address of a binary, which its daemon's iwdp transport uses too.
const proxyListingVar = "github.com/nnemirovsky/iwdp-cli/internal/proxy.listingAddr"

// listingLDFlags links a binary whose iwdp listing is addr, a private
// loopback server of these tests, so that no test reads the device list
// of the user's iwdp on 127.0.0.1:9221 (W1c T01). The linker ignores -X
// for a missing variable, so the tests check that the binary reads addr.
func listingLDFlags(addr string) string { return "-X " + proxyListingVar + "=" + addr }

// The fake iwdp of a healthy listing: one device with one page.
const (
	e2eDeviceID   = "e2e-fake-udid"
	e2eDeviceName = "E2E Fake iPhone"
	e2eDeviceOS   = "16.7"
	e2ePageTitle  = "E2E Fake Page"
	e2ePageURL    = "https://e2e.test/"
	e2ePageWSURL  = "ws://127.0.0.1:1/devtools/page/1"
)

// fakeListing is a private loopback server that stands in for one iwdp
// endpoint: GET /json returns body as JSON, or, when body is nil, the
// connection closes without a reply, like an iwdp that is not running.
// It counts the requests.
type fakeListing struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeListing(body any) *fakeListing {
	f := &fakeListing{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if body != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no iwdp", http.StatusServiceUnavailable)
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, "no iwdp", http.StatusServiceUnavailable)
			return
		}
		_ = c.Close()
	}))
	return f
}

// Addr is the host:port of the server.
func (f *fakeListing) Addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

var (
	deviceListingOnce sync.Once
	deviceListing     *fakeListing // the device list of a healthy listing
	devicePages       *fakeListing // the page list of its one device
)

// fakeDeviceListing returns a healthy fake iwdp for the binaries of these
// tests: its device list and the page list of its one device. The
// servers live until the process exits.
func fakeDeviceListing() (listing, device *fakeListing) {
	deviceListingOnce.Do(func() {
		devicePages = newFakeListing([]map[string]any{{
			"devtoolsFrontendUrl": "", "title": e2ePageTitle, "type": "page",
			"url": e2ePageURL, "webSocketDebuggerUrl": e2ePageWSURL, "appId": 1,
		}})
		deviceListing = newFakeListing([]map[string]string{{
			"deviceId": e2eDeviceID, "deviceName": e2eDeviceName,
			"deviceOSVersion": e2eDeviceOS, "url": devicePages.Addr(),
		}})
	})
	return deviceListing, devicePages
}

var (
	cliListingOnce sync.Once
	cliListing     *fakeListing
)

// deadCLIListing returns the listing that the iwdp-cli binary of these
// tests reads: no iwdp runs there. It lives until the process exits.
func deadCLIListing() *fakeListing {
	cliListingOnce.Do(func() { cliListing = newFakeListing(nil) })
	return cliListing
}

// The binaries of these tests are linked to private listings, never to
// iwdp's default listing address.
func TestE2EBinariesUseAPrivateListing(t *testing.T) {
	listing, device := fakeDeviceListing()
	for _, addr := range []string{listing.Addr(), device.Addr(), deadCLIListing().Addr()} {
		if addr == "127.0.0.1:9221" || addr == "127.0.0.1:9224" || !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Fatalf("private listing %s is not a private loopback address", addr)
		}
	}
}
