//go:build device

package direct

// Gated real-device check of the direct transport (design 7.4 test 4 and
// 8.3). It runs only with -tags device and IWDP_DEVICE_TEST=1, after the
// orchestrator has granted the device; runbook in docs/notes/w2.md. It takes
// the chosen page from any other inspector (ios_webkit_debug_proxy, Safari
// on a Mac), so never run it while another agent uses the device.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

func TestDeviceDirectEval(t *testing.T) {
	if os.Getenv("IWDP_DEVICE_TEST") != "1" {
		t.Fatal("set IWDP_DEVICE_TEST=1 after the device grant (docs/notes/w2.md)")
	}
	udid := os.Getenv("IWDP_DEVICE_UDID")
	urlPart := os.Getenv("IWDP_DEVICE_PAGE_URL") // a substring of the URL of the granted tab
	if udid == "" || urlPart == "" {
		t.Fatal("set IWDP_DEVICE_UDID and IWDP_DEVICE_PAGE_URL")
	}
	tr, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	devs, err := tr.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	var dev transport.Device
	for _, d := range devs {
		if d.ID == udid {
			dev = d
		}
	}
	if dev.ID == "" {
		t.Fatalf("device %s not listed by usbmuxd over USB (%d devices)", shortID(udid), len(devs))
	}
	t.Logf("device %s, iOS %q", dev.Label, dev.OSVersion)

	start := time.Now()
	pages, err := tr.Pages(ctx, udid)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	t.Logf("first Pages (dial, lockdown, WIR session, listings): %s, %d pages", time.Since(start), len(pages))
	var page transport.Page
	for _, p := range pages {
		if strings.Contains(p.URL, urlPart) {
			page = p
		}
	}
	if page.ID == "" {
		t.Fatalf("no page whose URL contains %q", urlPart)
	}

	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return tr.Open(ctx, page) },
		webkit.SessionOptions{Nudge: true})
	if err != nil {
		t.Fatalf("opening %s: %v", page.ID, err)
	}
	defer func() { _ = s.Close() }()
	for i := range 5 {
		start := time.Now()
		raw, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1+1", "returnByValue": true})
		if err != nil {
			t.Fatalf("Runtime.evaluate: %v", err)
		}
		var r struct {
			Result struct {
				Value float64 `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.Result.Value != 2 {
			t.Fatalf("1+1 = %s (%v)", raw, err)
		}
		t.Logf("eval %d: %s", i, time.Since(start))
	}
}
