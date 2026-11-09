package e2e_test

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// exitNoPage is the exit code of a listing failure (core.CodeNoPage).
const exitNoPage = 5

// runBin runs bin with args and returns its combined output and exit code.
func runBin(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("running %s %v: %v", bin, args, err)
	return "", 0
}

// port is the port of the host:port addr.
func port(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("port of %q: %v", addr, err)
	}
	return p
}

// docs/known-limits.md, "iwdp does not list pages while a page is
// connected": the legacy listing commands never call an iwdp that owns its
// port and closes the listing request without a reply "not running"; they
// name the empty reply and the advice. devices and pages exit 5, the code
// of a listing failure; status has no exit-code contract. The binary reads
// the private listing of these tests (deadCLIListing), which answers that
// way.
func TestCLI_LegacyListingCommandsOnAnEmptyReply(t *testing.T) {
	bin := buildCLI(t)
	for _, cmd := range []string{"devices", "pages", "status"} {
		output, code := runBin(t, bin, cmd)
		if code == 0 {
			t.Errorf("%s succeeded on an empty reply: %s", cmd, output)
			continue
		}
		if cmd != "status" && code != exitNoPage {
			t.Errorf("%s: exit %d, want %d (a listing failure)", cmd, code, exitNoPage)
		}
		for _, want := range []string{deadCLIListing().Addr(), "closed the connection without an answer", "iwdp-cli daemon stop"} {
			if !strings.Contains(output, want) {
				t.Errorf("%s: output %q does not contain %q", cmd, output, want)
			}
		}
		if strings.Contains(strings.ToLower(output), "not running") {
			t.Errorf("%s: output %q calls the iwdp not running", cmd, output)
		}
	}
}

// Review F1 (review brief, Judge 2): the legacy listing commands exit 5
// with the advice on an empty listing reply. pages reads the dead listing's
// port, never iwdp's default device port.
func TestReviewLegacyListingExitCode(t *testing.T) {
	bin := buildCLI(t)
	for _, args := range [][]string{{"devices"}, {"pages", port(t, deadCLIListing().Addr())}} {
		output, code := runBin(t, bin, args...)
		if code != exitNoPage {
			t.Errorf("%v: got exit %d; want exit %d; output: %s", args, code, exitNoPage, output)
		}
	}
}

// On a healthy listing the legacy listing commands answer from it: the
// device list, the page list of the device's port, and status.
func TestCLI_LegacyListingCommandsOnAHealthyListing(t *testing.T) {
	listing, device := fakeMCPListing()
	bin := buildCLIFor(t, listing.Addr())
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"devices"}, []string{e2eDeviceName, e2eDeviceID, "(pages on " + device.Addr() + ")"}},
		{[]string{"pages", port(t, device.Addr())}, []string{e2ePageTitle, e2ePageURL, e2ePageWSURL}},
		{[]string{"status"}, []string{"ios_webkit_debug_proxy is running"}},
	} {
		before := listing.hits.Load()
		output, code := runBin(t, bin, c.args...)
		if code != 0 {
			t.Errorf("%v on a healthy listing: exit %d, output %q", c.args, code, output)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(output, want) {
				t.Errorf("%v: output %q does not contain %q", c.args, output, want)
			}
		}
		if listing.hits.Load() == before {
			t.Errorf("%v did not check the private listing %s", c.args, listing.Addr())
		}
	}
}

// When no process owns the listing port, iwdp is not running: the legacy
// listing commands say so with the start advice and exit 1. pages reads
// the unowned port, never iwdp's default device port.
func TestCLI_LegacyListingCommandsOnAnUnownedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	bin := buildCLIFor(t, addr)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"devices"}, "ios_webkit_debug_proxy is not running."},
		{[]string{"pages", port(t, addr)}, "ios_webkit_debug_proxy is not running."},
		{[]string{"status"}, "ios_webkit_debug_proxy is NOT running"},
	} {
		output, code := runBin(t, bin, c.args...)
		if code != 1 {
			t.Errorf("%v on an unowned port: exit %d, want 1; output %q", c.args, code, output)
		}
		for _, want := range []string{c.want, "Start it with: ios_webkit_debug_proxy --no-frontend"} {
			if !strings.Contains(output, want) {
				t.Errorf("%v: output %q does not contain %q", c.args, output, want)
			}
		}
	}
}

// closeWithoutReply closes the connection of the request without a reply,
// as the iwdp of the second device check did while a page WebSocket was
// open.
func closeWithoutReply(t *testing.T, w http.ResponseWriter) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijacking the request: %v", err)
		return
	}
	_ = c.Close()
}

// wantListingAdvice checks a legacy listing command whose read of the
// listing failed after its preliminary check: exit 5, the empty reply and
// the advice, never "not running".
func wantListingAdvice(t *testing.T, what, output string, code int) {
	t.Helper()
	if code != exitNoPage {
		t.Errorf("%s: exit %d, want %d (a listing failure); output %q", what, code, exitNoPage, output)
	}
	for _, want := range []string{"closed the connection without an answer", "iwdp-cli daemon stop"} {
		if !strings.Contains(output, want) {
			t.Errorf("%s: output %q does not contain %q", what, output, want)
		}
	}
	if strings.Contains(strings.ToLower(output), "not running") {
		t.Errorf("%s: output %q calls the iwdp not running", what, output)
	}
}

// Review F5: the device list answers, but the device's page list closes
// each request without a reply while a WebSocket to its page is open. The
// legacy pages command reports that as a listing failure with the advice,
// as its preliminary check would. A device port that nothing owns keeps
// exit 1.
func TestCLI_LegacyPagesOnASilentDevicePageList(t *testing.T) {
	page := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(page.Close)
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if page.OpenConnections() > 0 {
			closeWithoutReply(t, w)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"title": "Fixture", "url": "https://fixture.example/", "webSocketDebuggerUrl": page.URL}})
	}))
	t.Cleanup(device.Close)
	deviceAddr := strings.TrimPrefix(device.URL, "http://")
	listing := newFakeListing([]map[string]string{{"deviceId": "fixture-device", "deviceName": "Fixture", "url": deviceAddr}})
	t.Cleanup(listing.srv.Close)
	bin := buildCLIFor(t, listing.Addr())

	if output, code := runBin(t, bin, "pages", port(t, deviceAddr)); code != 0 || !strings.Contains(output, "Fixture") {
		t.Fatalf("pages before the page connection: exit %d, output %q", code, output)
	}
	c, _, err := websocket.DefaultDialer.Dial(page.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	output, code := runBin(t, bin, "pages", port(t, deviceAddr))
	wantListingAdvice(t, "pages while a page connection is open", output, code)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unowned := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	output, code = runBin(t, bin, "pages", port(t, unowned))
	if code != 1 || !strings.Contains(output, "Check the device port") {
		t.Errorf("pages of a device port that nothing owns: exit %d, output %q; want exit 1 and the device-port advice", code, output)
	}
}

// Review F5: the listing answers the preliminary check and then closes
// each request without a reply. The legacy devices command reports the
// failed read as a listing failure with the advice: the check is no
// evidence that the read succeeds.
func TestCLI_LegacyDevicesWhenTheListingFailsAfterItsCheck(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		closeWithoutReply(t, w)
	}))
	t.Cleanup(srv.Close)
	bin := buildCLIFor(t, strings.TrimPrefix(srv.URL, "http://"))
	output, code := runBin(t, bin, "devices")
	if n := hits.Load(); n != 2 {
		t.Fatalf("the listing got %d requests, want 2 (the check, then the read)", n)
	}
	wantListingAdvice(t, "devices after a passed check", output, code)
}
