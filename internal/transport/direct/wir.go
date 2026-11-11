package direct

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// WIR selectors and keys (webinspectord's remote inspector protocol, as used
// by ios_webkit_debug_proxy, go-ios and pymobiledevice3; facts only, no code
// copied).
const (
	selReportIdentifier   = "_rpc_reportIdentifier:"
	selGetConnectedApps   = "_rpc_getConnectedApplications:"
	selForwardGetListing  = "_rpc_forwardGetListing:"
	selForwardSocketSetup = "_rpc_forwardSocketSetup:"
	selForwardSocketData  = "_rpc_forwardSocketData:"
	selForwardDidClose    = "_rpc_forwardDidClose:"
	selReportAppList      = "_rpc_reportConnectedApplicationList:"
	selAppConnected       = "_rpc_applicationConnected:"
	selAppUpdated         = "_rpc_applicationUpdated:"
	selAppDisconnected    = "_rpc_applicationDisconnected:"
	selAppSentListing     = "_rpc_applicationSentListing:"
	selAppSentData        = "_rpc_applicationSentData:"
	keyConnectionID       = "WIRConnectionIdentifierKey"
	keyAppID              = "WIRApplicationIdentifierKey"
	keyAppBundleID        = "WIRApplicationBundleIdentifierKey"
	keyAppName            = "WIRApplicationNameKey"
	keyAppIsProxy         = "WIRIsApplicationProxyKey"
	keyHostAppID          = "WIRHostApplicationIdentifierKey"
	keyAppDictionary      = "WIRApplicationDictionaryKey"
	keyListing            = "WIRListingKey"
	keyPageID             = "WIRPageIdentifierKey"
	keyType               = "WIRTypeKey"
	keyTitle              = "WIRTitleKey"
	keyURL                = "WIRURLKey"
	keySender             = "WIRSenderKey"
	keySession            = "WIRSessionIdentifierKey"
	keySocketData         = "WIRSocketDataKey"
	keyDestination        = "WIRDestinationKey"
	keyMessageData        = "WIRMessageDataKey"
	keyChunkSupported     = "WIRMessageDataTypeChunkSupportedKey"
	keyAutomaticallyPause = "WIRAutomaticallyPause"
	typeAutomation        = "WIRTypeAutomation"
	deviceWriteTimeout    = 30 * time.Second // one frame to the device; a stream that takes no data this long is dead
	pageQueueLen          = 4096             // unread messages per page connection before it is ended
	closeBudget           = time.Second      // _rpc_forwardDidClose: on Close
)

var errPageConnClosed = errors.New("direct: page connection closed")

// notWrittenError marks a refusal before any byte of the message reached the
// device (errors.Is(err, transport.ErrNotWritten)) and keeps err's text and
// chain. webkit.Session reports it as webkit.ErrNotSent, so a paused target
// stays paused and a held request stays pending.
type notWrittenError struct{ err error }

func (e notWrittenError) Error() string        { return e.err.Error() }
func (e notWrittenError) Unwrap() error        { return e.err }
func (e notWrittenError) Is(target error) bool { return target == transport.ErrNotWritten }

func notWritten(err error) error { return notWrittenError{err} }

type wirApp struct {
	id, bundleID, hostID string
	proxy                bool
}

type wirPage struct {
	id                  int
	typ, title, pageURL string
}

// wirDevice is one webinspector connection with its application and page
// state. Its read loop never writes, and its write loop never reads, so a
// peer that writes while it reads cannot deadlock it.
type wirDevice struct {
	udid    string
	conn    deviceConn
	connID  string
	opts    Options
	writes  chan *writeReq
	onClose func(*wirDevice)
	done    chan struct{}

	mu       sync.Mutex
	err      error // why the connection ended; nil while open
	apps     map[string]wirApp
	gotApps  bool
	listings map[string]map[int]wirPage // app id -> page id -> page
	rev      map[string]uint64          // listings received per app
	changed  chan struct{}              // closed and replaced on every state change
	pages    map[string]*pageConn       // sender key -> page connection
}

type writeReq struct {
	frame []byte
	done  chan error
}

// openDevice starts the WIR session on conn: it reports the connection
// identifier and waits for the application list (the first read). On
// failure conn is closed.
func openDevice(ctx context.Context, udid string, conn deviceConn, opts Options, onClose func(*wirDevice)) (*wirDevice, error) {
	d := &wirDevice{
		udid: udid, conn: conn, connID: newUUID(), opts: opts.withDefaults(),
		writes: make(chan *writeReq), onClose: onClose, done: make(chan struct{}),
		apps: make(map[string]wirApp), listings: make(map[string]map[int]wirPage),
		rev: make(map[string]uint64), changed: make(chan struct{}), pages: make(map[string]*pageConn),
	}
	go d.readLoop()
	go d.writeLoop()
	err := d.send(ctx, selReportIdentifier, nil)
	if err == nil {
		err = d.send(ctx, selGetConnectedApps, nil)
	}
	if err == nil {
		err = d.waitFor(ctx, func() bool { return d.gotApps })
	}
	if err != nil {
		err = fmt.Errorf("direct: device %s: webinspectord did not report its applications: %w; check that Settings > Safari > Advanced > Web Inspector is on", shortID(udid), err)
		d.fail(err)
		return nil, err
	}
	return d, nil
}

// fail ends the connection once: the socket closes and every page
// connection fails with webkit.ErrPageClosed.
func (d *wirDevice) fail(cause error) {
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	d.err = cause
	victims := d.pages
	d.pages = make(map[string]*pageConn)
	d.broadcastLocked()
	d.mu.Unlock()
	close(d.done)
	_ = d.conn.Close()
	for _, pc := range victims {
		pc.end(fmt.Errorf("%w: the connection to device %s ended: %w", webkit.ErrPageClosed, shortID(d.udid), cause))
	}
	if d.onClose != nil {
		d.onClose(d)
	}
}

func (d *wirDevice) closedErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *wirDevice) broadcastLocked() {
	close(d.changed)
	d.changed = make(chan struct{})
}

// waitFor waits until pred (called with d.mu held) is true, the connection
// ends or ctx ends.
func (d *wirDevice) waitFor(ctx context.Context, pred func() bool) error {
	for {
		d.mu.Lock()
		if d.err != nil {
			err := d.err
			d.mu.Unlock()
			return err
		}
		if pred() {
			d.mu.Unlock()
			return nil
		}
		ch := d.changed
		d.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// send writes one WIR message. ctx bounds the wait for the write loop; once
// the frame is handed over it is written whole (within deviceWriteTimeout),
// because a partial frame would break the stream for every page. A caller
// whose ctx ends during that write gets an error that says the message may
// still arrive. Every failure before the handoff wraps
// transport.ErrNotWritten; failures after it do not.
func (d *wirDevice) send(ctx context.Context, selector string, arg map[string]any) error {
	if arg == nil {
		arg = make(map[string]any)
	}
	arg[keyConnectionID] = d.connID
	frame, err := encodeFrame(wirMessage{Selector: selector, Argument: arg})
	if err != nil {
		return notWritten(err)
	}
	// An empty frame follows every message: ios_webkit_debug_proxy found
	// that webinspectord on a device may not process a message with a
	// large payload until the next frame arrives (webinspector.c,
	// wi_send_plist).
	frame = append(frame, make([]byte, frameHeaderLen)...)
	req := &writeReq{frame: frame, done: make(chan error, 1)}
	// An ended ctx is checked first: the select below picks at random
	// when the write loop is also ready.
	if ctx.Err() != nil {
		return notWritten(fmt.Errorf("direct: %s not sent: %w", selector, context.Cause(ctx)))
	}
	select {
	case d.writes <- req:
	case <-ctx.Done():
		return notWritten(fmt.Errorf("direct: %s not sent: %w", selector, context.Cause(ctx)))
	case <-d.done:
		return notWritten(fmt.Errorf("direct: %s not sent: %w", selector, d.closedErr()))
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("direct: %s was being written when the deadline passed; it may still arrive: %w", selector, context.Cause(ctx))
	}
}

func (d *wirDevice) writeLoop() {
	for {
		select {
		case req := <-d.writes:
			var result error
			err := d.conn.SetWriteDeadline(time.Now().Add(deviceWriteTimeout))
			if err != nil {
				err = fmt.Errorf("direct: writing to webinspectord: setting the write deadline: %w", err)
				result = notWritten(err) // nothing of the frame was written
			} else if _, err = d.conn.Write(req.frame); err != nil {
				err = fmt.Errorf("direct: writing to webinspectord: %w", err)
				result = err // part of the frame may have been written
			}
			if err != nil {
				d.fail(err)
			}
			req.done <- result
		case <-d.done:
			return
		}
	}
}

func (d *wirDevice) readLoop() {
	fr := &frameReader{r: d.conn, dl: d.conn, maxFrame: d.opts.MaxIncoming, drainTimeout: d.opts.DrainTimeout}
	for {
		payload, dropped, err := fr.next()
		if err != nil {
			d.fail(fmt.Errorf("direct: reading from webinspectord: %w", err))
			return
		}
		if dropped > 0 {
			d.notifyDropped(dropped, fmt.Sprintf("above the frame cap of %d bytes", d.opts.MaxIncoming))
			continue
		}
		if len(payload) == 0 {
			continue // an empty frame carries nothing
		}
		m, err := decodeMessage(payload)
		if err != nil {
			d.notifyDropped(int64(len(payload)), "not a valid property list")
			continue
		}
		d.handle(m)
	}
}

// notifyDropped tells every page connection of the device that a message
// was lost. Which page it was for is unknown.
func (d *wirDevice) notifyDropped(n int64, reason string) {
	ev, err := json.Marshal(map[string]any{"method": EventMessageDropped, "params": map[string]any{
		"bytes":   n,
		"reason":  reason,
		"message": fmt.Sprintf("device dropped a %d-byte message (%s); a pending reply may be lost", n, reason),
	}})
	if err != nil {
		panic(err) // a map of strings and integers always marshals
	}
	d.mu.Lock()
	pcs := make([]*pageConn, 0, len(d.pages))
	for _, pc := range d.pages {
		pcs = append(pcs, pc)
	}
	d.mu.Unlock()
	for _, pc := range pcs {
		pc.deliver(ev)
	}
}

func (d *wirDevice) handle(m wirMessage) {
	arg := m.Argument
	switch m.Selector {
	case selReportAppList:
		dict, _ := arg[keyAppDictionary].(map[string]any)
		apps := make(map[string]wirApp, len(dict))
		for _, raw := range dict {
			if a, ok := parseApp(raw); ok {
				apps[a.id] = a
			}
		}
		d.mu.Lock()
		var gone []string
		for id := range d.apps {
			if _, ok := apps[id]; !ok {
				gone = append(gone, id)
			}
		}
		d.apps = apps
		d.gotApps = true
		victims := d.dropAppsLocked(gone)
		d.broadcastLocked()
		d.mu.Unlock()
		d.endApps(victims)
	case selAppConnected, selAppUpdated:
		a, ok := parseApp(arg)
		if !ok {
			return
		}
		d.mu.Lock()
		d.apps[a.id] = a
		d.broadcastLocked()
		d.mu.Unlock()
	case selAppDisconnected:
		id, _ := arg[keyAppID].(string)
		d.mu.Lock()
		delete(d.apps, id)
		victims := d.dropAppsLocked([]string{id})
		d.broadcastLocked()
		d.mu.Unlock()
		d.endApps(victims)
	case selAppSentListing:
		id, _ := arg[keyAppID].(string)
		raw, _ := arg[keyListing].(map[string]any)
		listing := make(map[int]wirPage, len(raw))
		for _, r := range raw {
			if p, ok := parsePage(r); ok {
				listing[p.id] = p
			}
		}
		d.mu.Lock()
		d.listings[id] = listing
		d.rev[id]++
		d.broadcastLocked()
		d.mu.Unlock()
	case selAppSentData:
		dest, _ := arg[keyDestination].(string)
		var data []byte
		switch v := arg[keyMessageData].(type) {
		case []byte:
			data = v
		case string:
			data = []byte(v)
		}
		d.mu.Lock()
		pc := d.pages[dest]
		d.mu.Unlock()
		if pc != nil && data != nil {
			pc.deliver(data)
		}
	}
}

// dropAppsLocked forgets the listings of apps that are gone and returns
// their page connections, removed from the routing table.
func (d *wirDevice) dropAppsLocked(ids []string) []*pageConn {
	var victims []*pageConn
	for _, id := range ids {
		delete(d.listings, id)
		for key, pc := range d.pages {
			if pc.appID == id {
				victims = append(victims, pc)
				delete(d.pages, key)
			}
		}
	}
	return victims
}

func (d *wirDevice) endApps(victims []*pageConn) {
	for _, pc := range victims {
		pc.end(fmt.Errorf("%w: application %s disconnected", webkit.ErrPageClosed, pc.appID))
	}
}

// listPages asks every app for its listing and waits until each has
// answered (its listing revision moved) or ListingWait passes.
func (d *wirDevice) listPages(ctx context.Context) ([]transport.Page, error) {
	d.mu.Lock()
	before := make(map[string]uint64, len(d.apps))
	for id := range d.apps {
		before[id] = d.rev[id]
	}
	d.mu.Unlock()
	for id := range before {
		if err := d.send(ctx, selForwardGetListing, map[string]any{keyAppID: id}); err != nil {
			return nil, err
		}
	}
	wctx, cancel := context.WithTimeout(ctx, d.opts.ListingWait)
	_ = d.waitFor(wctx, func() bool {
		for id, r := range before {
			if _, live := d.apps[id]; live && d.rev[id] == r {
				return false
			}
		}
		return true
	})
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("direct: listing pages of device %s: %w", shortID(d.udid), context.Cause(ctx))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	var out []transport.Page
	for appID, listing := range d.listings {
		app, ok := d.apps[appID]
		if !ok {
			continue
		}
		bundle := app.bundleID
		if host, ok := d.apps[app.hostID]; app.proxy && ok {
			bundle = host.bundleID
		}
		for _, p := range listing {
			if p.typ == typeAutomation {
				continue
			}
			out = append(out, transport.Page{
				DeviceID: d.udid, ID: pageKey(appID, p.id), Title: p.title, URL: p.pageURL,
				AppID: bundle, Debuggable: true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func pageKey(appID string, pageID int) string { return appID + "/" + strconv.Itoa(pageID) }

func parsePageKey(id string) (appID string, pageID int, ok bool) {
	i := strings.LastIndex(id, "/")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return "", 0, false
	}
	return id[:i], n, true
}

// open sets up an inspector socket for one page. WebKit then announces the
// page target on it (Target.targetCreated), which webkit.Session waits for.
func (d *wirDevice) open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	appID, pageID, ok := parsePageKey(p.ID)
	if !ok {
		return nil, fmt.Errorf("direct: page id %q: want <application id>/<page id> as listed by Pages", p.ID)
	}
	pc := &pageConn{
		d: d, appID: appID, pageID: pageID, sender: newUUID(),
		limits: transport.Limits{MaxOutgoing: MaxOutgoing, MaxIncoming: d.opts.MaxIncoming, Routed: true},
		in:     make(chan []byte, pageQueueLen), ended: make(chan struct{}),
	}
	d.mu.Lock()
	if d.err != nil {
		err := d.err
		d.mu.Unlock()
		return nil, err
	}
	if _, listed := d.listings[appID][pageID]; !listed {
		d.mu.Unlock()
		return nil, fmt.Errorf("direct: page %s is not in the listing of device %s; list the pages again", p.ID, shortID(d.udid))
	}
	d.pages[pc.sender] = pc
	d.mu.Unlock()
	err := d.send(ctx, selForwardSocketSetup, map[string]any{
		keyAppID: appID, keyPageID: pageID, keySender: pc.sender,
		keyChunkSupported: 0, keyAutomaticallyPause: false,
	})
	if err != nil {
		_ = pc.Close() // the setup may still reach the device
		return nil, err
	}
	return pc, nil
}

// removePage takes a page connection out of the routing table. It reports
// whether the connection was routed on a live device.
func (d *wirDevice) removePage(pc *pageConn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pages[pc.sender] != pc {
		return false
	}
	delete(d.pages, pc.sender)
	return d.err == nil
}

// pageConn is a transport.MessageConn for one page on a shared device
// connection.
type pageConn struct {
	d      *wirDevice
	appID  string
	pageID int
	sender string
	limits transport.Limits

	in        chan []byte
	ended     chan struct{}
	endOnce   sync.Once
	endErr    error
	closeOnce sync.Once
	closeErr  error
}

var _ transport.MessageConn = (*pageConn)(nil)

func (c *pageConn) Limits() transport.Limits { return c.limits }

// end makes Read return err once the queued messages are read.
func (c *pageConn) end(err error) {
	c.endOnce.Do(func() {
		c.endErr = err
		close(c.ended)
	})
}

// deliver queues one message from the device. It never blocks the device
// read loop: a page connection whose reader falls pageQueueLen messages
// behind is ended.
func (c *pageConn) deliver(msg []byte) {
	select {
	case <-c.ended:
		return
	default:
	}
	select {
	case c.in <- msg:
	default:
		c.end(fmt.Errorf("direct: page connection ended: its reader fell %d messages behind", pageQueueLen))
		go func() { _ = c.Close() }()
	}
}

// Read returns the next message. ctx bounds only this wait.
func (c *pageConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case m := <-c.in:
		return m, nil
	default:
	}
	select {
	case m := <-c.in:
		return m, nil
	case <-c.ended:
		select {
		case m := <-c.in:
			return m, nil
		default:
			return nil, c.endErr
		}
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// Write sends one WIP message to the page. Messages above MaxOutgoing and
// operations whose result would exceed the frame cap are refused before
// anything is written; these refusals, and a Write on an ended connection,
// wrap transport.ErrNotWritten.
func (c *pageConn) Write(ctx context.Context, msg []byte) error {
	select {
	case <-c.ended:
		return notWritten(c.endErr)
	default:
	}
	if len(msg) > c.limits.MaxOutgoing {
		return notWritten(fmt.Errorf("direct: message of %d bytes not sent; the direct transport sends at most %d", len(msg), c.limits.MaxOutgoing))
	}
	if err := checkLargeResult(msg, c.limits.MaxIncoming); err != nil {
		return notWritten(err)
	}
	return c.d.send(ctx, selForwardSocketData, map[string]any{
		keyAppID: c.appID, keyPageID: c.pageID, keySession: c.sender, keySender: c.sender, keySocketData: msg,
	})
}

// Close ends the page connection and sends _rpc_forwardDidClose: within
// closeBudget when the device connection is still open.
func (c *pageConn) Close() error {
	c.closeOnce.Do(func() {
		c.end(errPageConnClosed)
		if !c.d.removePage(c) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), closeBudget)
		defer cancel()
		c.closeErr = c.d.send(ctx, selForwardDidClose, map[string]any{keyAppID: c.appID, keyPageID: c.pageID, keySender: c.sender})
	})
	return c.closeErr
}

func parseApp(raw any) (wirApp, bool) {
	m, _ := raw.(map[string]any)
	id, _ := m[keyAppID].(string)
	if id == "" {
		return wirApp{}, false
	}
	a := wirApp{id: id}
	a.bundleID, _ = m[keyAppBundleID].(string)
	a.hostID, _ = m[keyHostAppID].(string)
	a.proxy, _ = m[keyAppIsProxy].(bool)
	return a, true
}

func parsePage(raw any) (wirPage, bool) {
	m, _ := raw.(map[string]any)
	id, ok := plistInt(m[keyPageID])
	if !ok {
		return wirPage{}, false
	}
	p := wirPage{id: id}
	p.typ, _ = m[keyType].(string)
	p.title, _ = m[keyTitle].(string)
	p.pageURL, _ = m[keyURL].(string)
	return p, true
}

// plistInt reads a plist integer, which decodes as uint64 or int64.
func plistInt(v any) (int, bool) {
	switch n := v.(type) {
	case uint64:
		return int(n), n <= uint64(^uint(0)>>1)
	case int64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// newUUID returns a random UUID in upper case, the form WebKit uses for
// connection identifiers and sender keys.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
