// Package webkitgtk is the transport for a local WebKitGTK browser started
// with WEBKIT_INSPECTOR_HTTP_SERVER=<host:port> (design 8.2 and spike A).
// Its inspector server lists targets only as HTML at "/" (there is no
// /json); each page socket is ws://<host:port>/socket/<connection>/<target>/WebPage.
package webkitgtk

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Transport reaches one WebKitGTK inspector HTTP server.
type Transport struct {
	addr   string
	client *http.Client
}

// New returns a transport for the inspector server at addr ("127.0.0.1:9555").
func New(addr string) *Transport { return &Transport{addr: addr, client: &http.Client{}} }

func (t *Transport) Kind() transport.Kind { return transport.KindWebKitGTK }

func (t *Transport) deviceID() string { return "webkitgtk-" + t.addr }

// Devices returns the one local browser.
func (t *Transport) Devices(ctx context.Context) ([]transport.Device, error) {
	return []transport.Device{{ID: t.deviceID(), Label: "WebKitGTK " + t.addr, Name: "WebKitGTK", Transport: transport.KindWebKitGTK}}, nil
}

// targetRow matches one row of the HTML target list.
var targetRow = regexp.MustCompile(`(?s)<div class="targetname">(.*?)</div><div class="targeturl">(.*?)</div>.*?'(/socket/\d+/\d+/WebPage)'`)

// Pages parses the HTML target list.
func (t *Transport) Pages(ctx context.Context, deviceID string) ([]transport.Page, error) {
	if deviceID != t.deviceID() {
		return nil, fmt.Errorf("webkitgtk: unknown device %q (this transport has %q)", deviceID, t.deviceID())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+t.addr+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webkitgtk: listing targets at %s: %w", t.addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("webkitgtk: reading the target list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webkitgtk: target list at %s: HTTP %d", t.addr, resp.StatusCode)
	}
	var pages []transport.Page
	for _, m := range targetRow.FindAllStringSubmatch(string(body), -1) {
		pages = append(pages, transport.Page{
			DeviceID: t.deviceID(), ID: m[3], Title: html.UnescapeString(m[1]), URL: html.UnescapeString(m[2]), Debuggable: true,
		})
	}
	return pages, nil
}

// Open dials the page socket with WebKitGTK's outgoing message limit.
func (t *Transport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	return webkit.DialWebSocket(ctx, "ws://"+t.addr+p.ID, transport.Limits{
		MaxOutgoing: transport.WebKitGTKMaxOutgoing,
		MaxIncoming: transport.DefaultMaxIncoming,
		Routed:      true,
		CSSDomain:   true,
	})
}

func (t *Transport) Close() error { return nil }
