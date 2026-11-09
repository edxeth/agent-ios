package fakewebkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Listing is a fake ios_webkit_debug_proxy listing (design 8.1): a device
// list like port 9221 and one page list per device like ports 9224 and up.
// With Frontend on, a page that has a WebSocket client lists an empty
// devtoolsFrontendUrl (the busy hint of design 2.5); with Frontend off
// (iwdp -F, how proxy.Start runs it) the field is always empty.
type Listing struct {
	Frontend bool

	mu      sync.Mutex
	srv     *httptest.Server
	devices []*ListedDevice
	silent  []*Server // EmptyReplyWhileConnected
}

// ListedDevice is one device of a Listing, with its own page-list server.
type ListedDevice struct {
	ID, Name, OSVersion string

	l     *Listing
	srv   *httptest.Server
	pages []*ListedPage
}

// ListedPage is one page of a ListedDevice.
type ListedPage struct {
	Title, URL, WSURL string
	AppID             int
	Busy              bool // another client holds the page (shown only with Frontend on)

	server *Server // ServedBy: the page is also busy while server has a connection
}

// NewListing starts a fake device listing.
func NewListing(frontend bool) *Listing {
	l := &Listing{Frontend: frontend}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.emptyReply(w) {
			return
		}
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		l.mu.Lock()
		out := []map[string]string{}
		for _, d := range l.devices {
			out = append(out, map[string]string{
				"deviceId": d.ID, "deviceName": d.Name, "deviceOSVersion": d.OSVersion,
				"url": strings.TrimPrefix(d.srv.URL, "http://"),
			})
		}
		l.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	}))
	return l
}

// EmptyReplyWhileConnected models the iwdp of the second device check
// (docs/known-limits.md, "iwdp does not list pages while a page is
// connected"): while any of servers has an open WebSocket connection, the
// device list and every page list accept each HTTP connection and close it
// without a reply. They answer again once no connection is open.
func (l *Listing) EmptyReplyWhileConnected(servers ...*Server) {
	l.mu.Lock()
	l.silent = append(l.silent, servers...)
	l.mu.Unlock()
}

// emptyReply closes the request's connection without a reply when
// EmptyReplyWhileConnected applies, and reports whether it did.
func (l *Listing) emptyReply(w http.ResponseWriter) bool {
	l.mu.Lock()
	connected := false
	for _, s := range l.silent {
		connected = connected || s.OpenConnections() > 0
	}
	l.mu.Unlock()
	if !connected {
		return false
	}
	c, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(fmt.Sprintf("fakewebkit listing: hijacking a listing request: %v", err))
	}
	_ = c.Close()
	return true
}

// Addr is the host:port of the device list.
func (l *Listing) Addr() string { return strings.TrimPrefix(l.srv.URL, "http://") }

// AddDevice adds a device with its own page-list server.
func (l *Listing) AddDevice(id, name, osVersion string) *ListedDevice {
	d := &ListedDevice{ID: id, Name: name, OSVersion: osVersion, l: l}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	l.mu.Lock()
	l.devices = append(l.devices, d)
	l.mu.Unlock()
	return d
}

// AddPage lists a page whose WebSocket URL is wsURL (usually Server.URL).
func (d *ListedDevice) AddPage(title, url, wsURL string) *ListedPage {
	p := &ListedPage{Title: title, URL: url, WSURL: wsURL, AppID: len(d.pages) + 1}
	d.l.mu.Lock()
	d.pages = append(d.pages, p)
	d.l.mu.Unlock()
	return p
}

// RemovePage drops p from the device's page list (a closed tab).
func (d *ListedDevice) RemovePage(p *ListedPage) {
	d.l.mu.Lock()
	defer d.l.mu.Unlock()
	for i, q := range d.pages {
		if q == p {
			d.pages = append(d.pages[:i:i], d.pages[i+1:]...)
			return
		}
	}
}

// SetBusy marks p as held (or not) by another WebSocket client.
func (d *ListedDevice) SetBusy(p *ListedPage, busy bool) {
	d.l.mu.Lock()
	p.Busy = busy
	d.l.mu.Unlock()
}

// ServedBy makes p busy (with Frontend on) while s has an open connection,
// as iwdp lists a page that any inspector client holds, the caller's own
// connection included.
func (d *ListedDevice) ServedBy(p *ListedPage, s *Server) {
	d.l.mu.Lock()
	p.server = s
	d.l.mu.Unlock()
}

// Port is the device's page-list address, as the device list shows it.
func (d *ListedDevice) Addr() string { return strings.TrimPrefix(d.srv.URL, "http://") }

func (d *ListedDevice) serve(w http.ResponseWriter, r *http.Request) {
	if d.l.emptyReply(w) {
		return
	}
	if r.URL.Path != "/json" {
		http.NotFound(w, r)
		return
	}
	d.l.mu.Lock()
	out := []map[string]any{}
	for _, p := range d.pages {
		frontend := ""
		if d.l.Frontend && !p.Busy && (p.server == nil || p.server.OpenConnections() == 0) {
			frontend = "http://inspector/devtools/inspector.html?ws=" + strings.TrimPrefix(p.WSURL, "ws://")
		}
		out = append(out, map[string]any{
			"devtoolsFrontendUrl": frontend, "title": p.Title, "type": "page", "url": p.URL,
			"webSocketDebuggerUrl": p.WSURL, "appId": p.AppID,
		})
	}
	d.l.mu.Unlock()
	_ = json.NewEncoder(w).Encode(out)
}

// Close stops the listing and every device server.
func (l *Listing) Close() {
	l.mu.Lock()
	devs := append([]*ListedDevice(nil), l.devices...)
	l.mu.Unlock()
	for _, d := range devs {
		d.srv.Close()
	}
	l.srv.Close()
}
