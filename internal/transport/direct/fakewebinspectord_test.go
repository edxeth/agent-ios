package direct

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// fakeInspector is a fake webinspectord. It speaks WIR (length-prefixed
// plists) on one connection and relays each inspector socket to a fakewebkit
// page server over WebSocket, the way ios_webkit_debug_proxy relays pages.
// So WIP behavior (Target routing, stalls, results) comes from fakewebkit.
type fakeInspector struct {
	t    *testing.T
	conn net.Conn

	writeMu sync.Mutex

	mu          sync.Mutex
	apps        []fakeApp
	received    []wirMessage
	sockets     map[string]*fakeSocket // sender key -> relay
	silent      bool                   // read requests, never answer (first-read stall)
	failOnFirst bool                   // close on the first request (first-read failure)
	listingHold map[string]time.Duration
	emptyFrames int // empty frames read (one follows every message)
	closed      chan struct{}
}

type fakeApp struct {
	id, bundle, host string
	proxy            bool
	pages            []fakePage
}

type fakePage struct {
	id         int
	title, url string
	typ        string
	server     *fakewebkit.Server
}

type fakeSocket struct {
	sender, app string
	page        int
	ws          transport.MessageConn
}

func newFakeInspector(t *testing.T, conn net.Conn, apps []fakeApp) *fakeInspector {
	f := &fakeInspector{
		t: t, conn: conn, apps: apps, sockets: make(map[string]*fakeSocket),
		listingHold: make(map[string]time.Duration), closed: make(chan struct{}),
	}
	return f
}

// serve handles requests until the client closes the connection.
func (f *fakeInspector) serve() {
	defer close(f.closed)
	defer f.closeSockets()
	fr := &frameReader{r: f.conn, dl: f.conn, maxFrame: 1 << 30, drainTimeout: time.Second}
	for {
		payload, _, err := fr.next()
		if err != nil {
			return
		}
		if len(payload) == 0 {
			f.mu.Lock()
			f.emptyFrames++
			f.mu.Unlock()
			continue
		}
		m, err := decodeMessage(payload)
		if err != nil {
			f.t.Errorf("fake webinspectord: %v", err)
			return
		}
		f.mu.Lock()
		f.received = append(f.received, m)
		silent, failFirst := f.silent, f.failOnFirst
		f.mu.Unlock()
		if failFirst {
			// End only the fake's direction where possible, so that the
			// fake usbmuxd can still see the client close its end.
			if cw, ok := f.conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			} else {
				_ = f.conn.Close()
			}
			return
		}
		if silent {
			continue
		}
		f.handle(m)
	}
}

func (f *fakeInspector) closeSockets() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sockets {
		_ = s.ws.Close()
	}
}

func (f *fakeInspector) appDict(a fakeApp) map[string]any {
	d := map[string]any{keyAppID: a.id, keyAppBundleID: a.bundle, keyAppName: a.bundle, keyAppIsProxy: a.proxy}
	if a.host != "" {
		d[keyHostAppID] = a.host
	}
	return d
}

func (f *fakeInspector) handle(m wirMessage) {
	arg := m.Argument
	switch m.Selector {
	case selReportIdentifier:
		f.send("_rpc_reportCurrentState:", map[string]any{"WIRAutomationAvailabilityKey": "WIRAutomationAvailabilityNotAvailable"})
	case selGetConnectedApps:
		f.mu.Lock()
		dict := make(map[string]any)
		for _, a := range f.apps {
			dict[a.id] = f.appDict(a)
		}
		f.mu.Unlock()
		f.send(selReportAppList, map[string]any{keyAppDictionary: dict})
	case selForwardGetListing:
		id, _ := arg[keyAppID].(string)
		f.mu.Lock()
		hold := f.listingHold[id]
		f.mu.Unlock()
		if hold > 0 {
			go func() { time.Sleep(hold); f.sendListing(id) }()
			return
		}
		f.sendListing(id)
	case selForwardSocketSetup:
		f.setup(arg)
	case selForwardSocketData:
		sender, _ := arg[keySender].(string)
		data, _ := arg[keySocketData].([]byte)
		f.mu.Lock()
		s := f.sockets[sender]
		f.mu.Unlock()
		if s == nil {
			f.t.Errorf("fake webinspectord: data for unknown sender %q", sender)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.ws.Write(ctx, data); err != nil {
			f.t.Errorf("fake webinspectord: relaying to the page: %v", err)
		}
	case selForwardDidClose:
		sender, _ := arg[keySender].(string)
		f.mu.Lock()
		s := f.sockets[sender]
		delete(f.sockets, sender)
		f.mu.Unlock()
		if s != nil {
			_ = s.ws.Close()
		}
	}
}

func (f *fakeInspector) sendListing(appID string) {
	f.mu.Lock()
	listing := make(map[string]any)
	for _, a := range f.apps {
		if a.id != appID {
			continue
		}
		for _, p := range a.pages {
			typ := p.typ
			if typ == "" {
				typ = "WIRTypeWebPage"
			}
			listing[fmt.Sprint(p.id)] = map[string]any{keyPageID: p.id, keyType: typ, keyTitle: p.title, keyURL: p.url}
		}
	}
	f.mu.Unlock()
	f.send(selAppSentListing, map[string]any{keyAppID: appID, keyListing: listing})
}

func (f *fakeInspector) page(appID string, pageID int) *fakePage {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.apps {
		if a.id != appID {
			continue
		}
		for i := range a.pages {
			if a.pages[i].id == pageID {
				return &a.pages[i]
			}
		}
	}
	return nil
}

func (f *fakeInspector) setup(arg map[string]any) {
	sender, _ := arg[keySender].(string)
	appID, _ := arg[keyAppID].(string)
	pageID, _ := plistInt(arg[keyPageID])
	p := f.page(appID, pageID)
	if p == nil || p.server == nil {
		f.t.Errorf("fake webinspectord: socket setup for unknown page %s/%d", appID, pageID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, err := webkit.DialWebSocket(ctx, p.server.URL, transport.Limits{})
	if err != nil {
		f.t.Errorf("fake webinspectord: dialing the page: %v", err)
		return
	}
	f.mu.Lock()
	f.sockets[sender] = &fakeSocket{sender: sender, app: appID, page: pageID, ws: ws}
	f.mu.Unlock()
	go func() {
		for {
			data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			f.send(selAppSentData, map[string]any{keyDestination: sender, keyMessageData: data})
		}
	}()
}

// send writes one WIR message to the client.
func (f *fakeInspector) send(selector string, arg map[string]any) {
	frame, err := encodeFrame(wirMessage{Selector: selector, Argument: arg})
	if err != nil {
		f.t.Errorf("fake webinspectord: %v", err)
		return
	}
	f.sendRaw(frame)
}

// sendRaw writes bytes as they are, for frames that the fake builds by hand.
func (f *fakeInspector) sendRaw(b []byte) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.conn.Write(b)
}

// sendHeader writes only a frame header that announces n bytes.
func (f *fakeInspector) sendHeader(n uint32) {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], n)
	f.sendRaw(h[:])
}

func (f *fakeInspector) disconnectApp(appID string) {
	f.send(selAppDisconnected, map[string]any{keyAppID: appID})
}

func (f *fakeInspector) messages(selector string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.received {
		if m.Selector == selector {
			out = append(out, m.Argument)
		}
	}
	return out
}

func (f *fakeInspector) setPages(appID string, pages []fakePage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.apps {
		if f.apps[i].id == appID {
			f.apps[i].pages = pages
		}
	}
}

// pipeDialer is a deviceDialer whose device is a fakeInspector on a net.Pipe.
type pipeDialer struct {
	t     *testing.T
	udid  string
	apps  []fakeApp
	wrap  func(net.Conn) deviceConn // optional wrapper of the client end
	mu    sync.Mutex
	fakes []*fakeInspector
	dials int
}

func (p *pipeDialer) Devices(ctx context.Context) ([]transport.Device, error) {
	return []transport.Device{{ID: p.udid, Label: deviceLabel("Fake", p.udid), Name: "Fake", OSVersion: "16.7.10", Transport: transport.KindDirect}}, nil
}

func (p *pipeDialer) Dial(ctx context.Context, udid string) (deviceConn, error) {
	if udid != p.udid {
		return nil, fmt.Errorf("unknown device %q", udid)
	}
	client, server := net.Pipe()
	f := newFakeInspector(p.t, server, p.apps)
	p.mu.Lock()
	p.fakes = append(p.fakes, f)
	p.dials++
	p.mu.Unlock()
	go f.serve()
	if p.wrap != nil {
		return p.wrap(client), nil
	}
	return client, nil
}

func (p *pipeDialer) fake(i int) *fakeInspector {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fakes[i]
}
