// Package iwdp is the transport through ios_webkit_debug_proxy (design
// docs/design/agentic-parity.md, section 2.8): the HTTP device list (port
// 9221), one page list per device port (9224 and up), and one WebSocket per
// page. iwdp always uses Target routing.
package iwdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/edxeth/agent-ios/internal/proxy"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// DefaultListingAddr is iwdp's device list (proxy.ListingAddr).
var DefaultListingAddr = proxy.ListingAddr()

// Options configure a Transport.
type Options struct {
	// ListingAddr is the host:port of the device list; "" means
	// DefaultListingAddr.
	ListingAddr string
}

// Transport reaches pages through a running iwdp.
type Transport struct {
	listing string
	client  *http.Client

	mu      sync.Mutex
	devAddr map[string]string // device id -> host:port of its page list
}

// New returns an iwdp transport. It does not connect.
func New(o Options) *Transport {
	addr := o.ListingAddr
	if addr == "" {
		addr = DefaultListingAddr
	}
	return &Transport{listing: addr, client: &http.Client{}, devAddr: map[string]string{}}
}

func (t *Transport) Kind() transport.Kind { return transport.KindIWDP }

// ListingAddr is the device list address this transport reads.
func (t *Transport) ListingAddr() string { return t.listing }

func (t *Transport) getJSON(ctx context.Context, addr string, v any) error {
	u := "http://" + addr + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("iwdp listing %s: %w", u, err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("iwdp listing %s: %w; is ios_webkit_debug_proxy running?", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("iwdp listing %s returned HTTP %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("iwdp listing %s: decoding: %w", u, err)
	}
	return nil
}

// Label is the short device label of design 2.8: the name and the last
// four characters of the id, so that a full UDID is never printed.
func Label(name, id string) string {
	tail := strings.ToLower(id)
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	if name == "" {
		name = "device"
	}
	return name + " …" + tail
}

// Devices lists the devices iwdp sees, with deviceOSVersion when listed.
func (t *Transport) Devices(ctx context.Context) ([]transport.Device, error) {
	var entries []webkit.DeviceEntry
	if err := t.getJSON(ctx, t.listing, &entries); err != nil {
		return nil, err
	}
	out := make([]transport.Device, 0, len(entries))
	addrs := map[string]string{}
	for _, e := range entries {
		port, err := proxy.DevicePort(e)
		if err != nil {
			continue // a device without a page port cannot be inspected
		}
		host := "127.0.0.1"
		if h, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(e.URL, "http://"), "ws://"), ":"); ok && h != "" && h != "localhost" {
			host = h
		}
		addrs[e.DeviceID] = fmt.Sprintf("%s:%d", host, port)
		out = append(out, transport.Device{
			ID: e.DeviceID, Label: Label(e.DeviceName, e.DeviceID), Name: e.DeviceName,
			OSVersion: e.DeviceOSVersion, Transport: transport.KindIWDP,
		})
	}
	t.mu.Lock()
	t.devAddr = addrs
	t.mu.Unlock()
	return out, nil
}

// Pages lists the pages of a device. The ownership hint (design 2.5):
// iwdp leaves devtoolsFrontendUrl empty for a page that has a WebSocket
// client, but only when it serves its frontend; with -F every entry is
// empty. When at least one page of the device lists a frontend URL, the
// frontend is on and an empty entry means busy; otherwise ownership is
// unknown for every page.
func (t *Transport) Pages(ctx context.Context, deviceID string) ([]transport.Page, error) {
	t.mu.Lock()
	addr, ok := t.devAddr[deviceID]
	t.mu.Unlock()
	if !ok {
		if _, err := t.Devices(ctx); err != nil {
			return nil, err
		}
		t.mu.Lock()
		addr, ok = t.devAddr[deviceID]
		t.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("iwdp lists no device %q", deviceID)
		}
	}
	var entries []webkit.PageEntry
	if err := t.getJSON(ctx, addr, &entries); err != nil {
		return nil, err
	}
	frontend := false
	for _, e := range entries {
		if e.DevToolsFrontendURL != "" {
			frontend = true
		}
	}
	out := make([]transport.Page, 0, len(entries))
	for _, e := range entries {
		own := transport.OwnershipUnknown
		if frontend {
			own = transport.OwnershipFree
			if e.DevToolsFrontendURL == "" {
				own = transport.OwnershipBusy
			}
		}
		out = append(out, transport.Page{
			DeviceID: deviceID, ID: e.WebSocketDebuggerURL, Title: e.Title, URL: e.URL,
			Debuggable: e.WebSocketDebuggerURL != "", Ownership: own,
		})
	}
	return out, nil
}

// Open dials the page's WebSocket. Page.ID is its webSocketDebuggerUrl.
func (t *Transport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	if p.ID == "" {
		return nil, fmt.Errorf("page %q has no WebSocket URL; it may already be connected to another debugger", p.Title)
	}
	return webkit.DialWebSocket(ctx, p.ID, Limits())
}

// Limits are iwdp's message limits.
func Limits() transport.Limits {
	return transport.Limits{MaxOutgoing: transport.IWDPMaxOutgoing, MaxIncoming: transport.DefaultMaxIncoming, Routed: true}
}

func (t *Transport) Close() error { return nil }
