package webkitgtk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
)

// listing is the target list of WebKitGTK 2.54 MiniBrowser, captured on this
// host (two rows; the second title shows HTML escaping).
const listing = `<html><head><title>Remote inspector</title></head><body><h1>Inspectable targets</h1><div id='targetlist'><table><tbody><tr><td class="data"><div class="targetname">T1</div><div class="targeturl">file:///tmp/w1a-probe/p.html</div></td><td class="input"><input type="button" value="Inspect" onclick="window.open('Main.html?ws=' + window.location.host + '/socket/1/1/WebPage', '_blank', 'location=no,menubar=no,status=no,toolbar=no');"></td></tr><tr><td class="data"><div class="targetname">A &amp; B</div><div class="targeturl">http://127.0.0.1:8001/?a=1&amp;b=2</div></td><td class="input"><input type="button" value="Inspect" onclick="window.open('Main.html?ws=' + window.location.host + '/socket/1/7/WebPage', '_blank', 'location=no');"></td></tr></tbody></table></div></body></html>`

func TestPagesParsesHTMLListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(listing))
	}))
	defer srv.Close()
	tr := New(strings.TrimPrefix(srv.URL, "http://"))
	devs, err := tr.Devices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Transport != transport.KindWebKitGTK {
		t.Fatalf("devices = %+v, %v", devs, err)
	}
	pages, err := tr.Pages(context.Background(), devs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []transport.Page{
		{DeviceID: devs[0].ID, ID: "/socket/1/1/WebPage", Title: "T1", URL: "file:///tmp/w1a-probe/p.html", Debuggable: true},
		{DeviceID: devs[0].ID, ID: "/socket/1/7/WebPage", Title: "A & B", URL: "http://127.0.0.1:8001/?a=1&b=2", Debuggable: true},
	}
	if len(pages) != 2 || pages[0] != want[0] || pages[1] != want[1] {
		t.Fatalf("pages = %+v\nwant %+v", pages, want)
	}
	if _, err := tr.Pages(context.Background(), "other"); err == nil {
		t.Fatal("an unknown device was accepted")
	}
}
