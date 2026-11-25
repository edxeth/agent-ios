package iwdp_test

import (
	"context"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/transport/iwdp"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Design 2.8 and 7.3 (W1b): the iwdp transport lists devices with their iOS
// version (deviceOSVersion), pages with the ownership hint, and opens a
// routed page connection with iwdp's limits.
func TestDevicesPagesAndOpen(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	l := fakewebkit.NewListing(true)
	defer l.Close()
	d := l.AddDevice("00008020-001A2B3C4D5E6F70", "iPhone X", "16.7.10")
	d.AddPage("Example", "https://example.com/", f.URL)
	busy := d.AddPage("Held", "https://held.example/", f.URL)
	d.SetBusy(busy, true)

	tr := iwdp.New(iwdp.Options{ListingAddr: l.Addr()})
	if tr.Kind() != transport.KindIWDP {
		t.Fatalf("Kind = %q", tr.Kind())
	}
	ctx := ctx5(t)
	devs, err := tr.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 1 || devs[0].ID != d.ID || devs[0].OSVersion != "16.7.10" || devs[0].Name != "iPhone X" || devs[0].Transport != transport.KindIWDP {
		t.Fatalf("Devices = %+v", devs)
	}
	if devs[0].Label != "iPhone X …6f70" {
		t.Errorf("Label = %q, want the name and the last 4 characters of the UDID", devs[0].Label)
	}
	pages, err := tr.Pages(ctx, d.ID)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("Pages = %+v", pages)
	}
	if pages[0].Ownership != transport.OwnershipFree || pages[1].Ownership != transport.OwnershipBusy {
		t.Errorf("ownership hints = %v, %v; want free, busy", pages[0].Ownership, pages[1].Ownership)
	}
	if pages[0].Title != "Example" || pages[0].URL != "https://example.com/" || pages[0].DeviceID != d.ID || !pages[0].Debuggable {
		t.Errorf("page = %+v", pages[0])
	}
	conn, err := tr.Open(ctx, pages[0])
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if lim := conn.Limits(); lim.MaxOutgoing != transport.IWDPMaxOutgoing || !lim.Routed || lim.MaxIncoming != transport.DefaultMaxIncoming {
		t.Errorf("Limits = %+v", lim)
	}
	s, err := webkit.NewSession(ctx, conn, webkit.SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession over the iwdp connection: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// With the frontend off (iwdp -F) every devtoolsFrontendUrl is empty, so
// the listing says nothing about ownership: unknown, never busy.
func TestNoFrontendGivesUnknownOwnership(t *testing.T) {
	l := fakewebkit.NewListing(false)
	defer l.Close()
	d := l.AddDevice("udid-1", "iPhone", "")
	d.AddPage("A", "https://a.example/", "ws://127.0.0.1:1/devtools/page/1")
	tr := iwdp.New(iwdp.Options{ListingAddr: l.Addr()})
	pages, err := tr.Pages(ctx5(t), "udid-1")
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(pages) != 1 || pages[0].Ownership != transport.OwnershipUnknown {
		t.Fatalf("pages = %+v, want one page with unknown ownership", pages)
	}
}

func TestUnknownDeviceAndUnreachableListing(t *testing.T) {
	l := fakewebkit.NewListing(true)
	defer l.Close()
	tr := iwdp.New(iwdp.Options{ListingAddr: l.Addr()})
	if _, err := tr.Pages(ctx5(t), "nope"); err == nil {
		t.Fatal("Pages of an unknown device succeeded")
	}
	l.Close()
	if _, err := tr.Devices(ctx5(t)); err == nil {
		t.Fatal("Devices succeeded with the listing down")
	}
}
