// Package direct is the transport that reaches Safari on an iPhone without
// ios_webkit_debug_proxy: usbmuxd, lockdown and the com.apple.webinspector
// service (design docs/design/agentic-parity.md, section 6). go-ios is used
// only for usbmux, lockdown and pairing (usbmux.go); the webinspector (WIR)
// session layer and its frame reader are written here.
//
// One long-lived webinspector connection serves every page of a device,
// because the service stalls new connections for seconds after a close. Page
// connections are multiplexed on it by sender key. A message above the frame
// cap is skipped and reported to every page connection of the device.
package direct

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
)

// Limits and budgets of the direct transport (design section 6).
const (
	// DefaultMaxIncoming is the default frame cap. Larger frames are skipped.
	DefaultMaxIncoming = 256 << 20
	// MaxOutgoing is the largest WIP message sent: go-ios's 16 MiB frame
	// limit; the device-side limit is not measured.
	MaxOutgoing = 16 << 20
	// DefaultDrainTimeout bounds skipping one frame above the cap. A device
	// that does not deliver it in time is closed.
	DefaultDrainTimeout = 30 * time.Second
	// DefaultListingWait bounds the wait for fresh page listings. An app that
	// does not answer in time keeps its last listing.
	DefaultListingWait = time.Second
)

// EventMessageDropped is the method of the event that every page connection
// of a device reads when the device sent a message that could not be
// delivered (above the frame cap, or not a valid property list). Its params
// are {"bytes": N, "reason": "...", "message": "..."}. The call whose reply
// was lost fails at its own deadline.
const EventMessageDropped = "Transport.messageDropped"

// ErrTransportClosed: the transport was closed.
var ErrTransportClosed = errors.New("direct: transport closed")

// Options configure the direct transport. Zero fields take the defaults.
type Options struct {
	// SocketAddress of usbmuxd: "unix:///var/run/usbmuxd" or
	// "tcp://host:port". Empty: USBMUXD_SOCKET_ADDRESS, else the platform
	// default (go-ios's rules).
	SocketAddress string
	MaxIncoming   int           // frame cap in bytes; default DefaultMaxIncoming
	DrainTimeout  time.Duration // default DefaultDrainTimeout
	ListingWait   time.Duration // default DefaultListingWait
}

func (o Options) withDefaults() Options {
	if o.MaxIncoming <= 0 {
		o.MaxIncoming = DefaultMaxIncoming
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	if o.ListingWait <= 0 {
		o.ListingWait = DefaultListingWait
	}
	return o
}

// Transport implements transport.Transport over usbmuxd.
type Transport struct {
	dialer deviceDialer
	opts   Options

	mu      sync.Mutex
	devices map[string]*wirDevice // open webinspector connections by UDID
	dialing map[string]*dialCall
	closed  bool

	// beforePublish, when set, runs after a device's WIR session opened and
	// before the device is published. Tests use it to end the connection in
	// that window; it is nil otherwise.
	beforePublish func(*wirDevice)
}

type dialCall struct {
	done   chan struct{}
	cancel context.CancelCauseFunc // Close ends the dial with ErrTransportClosed
	dev    *wirDevice
	err    error
}

var _ transport.Transport = (*Transport)(nil)

// New returns a direct transport. It fails only for a malformed
// Options.SocketAddress; it does not connect yet.
func New(opts Options) (*Transport, error) {
	opts = opts.withDefaults()
	d, err := newUsbmuxDialer(opts.SocketAddress)
	if err != nil {
		return nil, err
	}
	return newWithDialer(d, opts), nil
}

func newWithDialer(d deviceDialer, opts Options) *Transport {
	return &Transport{
		dialer:  d,
		opts:    opts.withDefaults(),
		devices: make(map[string]*wirDevice),
		dialing: make(map[string]*dialCall),
	}
}

func (t *Transport) Kind() transport.Kind { return transport.KindDirect }

// Devices lists the USB devices that usbmuxd reports.
func (t *Transport) Devices(ctx context.Context) ([]transport.Device, error) {
	return t.dialer.Devices(ctx)
}

// Pages requests fresh listings from every inspectable app of the device
// and returns its pages. Page ids have the form "<app id>/<page id>".
func (t *Transport) Pages(ctx context.Context, deviceID string) ([]transport.Page, error) {
	d, err := t.device(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	return d.listPages(ctx)
}

// Open opens a page connection on the device's webinspector connection.
func (t *Transport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	d, err := t.device(ctx, p.DeviceID)
	if err != nil {
		return nil, err
	}
	return d.open(ctx, p)
}

// Close ends every device connection and every dial in progress; open page
// connections fail with webkit.ErrPageClosed, pending calls with
// ErrTransportClosed. A dial ends promptly but may still be closing its
// sockets when Close returns.
func (t *Transport) Close() error {
	t.mu.Lock()
	t.closed = true
	devs := make([]*wirDevice, 0, len(t.devices))
	for _, d := range t.devices {
		devs = append(devs, d)
	}
	t.devices = make(map[string]*wirDevice)
	calls := make([]*dialCall, 0, len(t.dialing))
	for _, c := range t.dialing {
		calls = append(calls, c)
	}
	t.mu.Unlock()
	for _, c := range calls {
		c.cancel(ErrTransportClosed)
	}
	for _, d := range devs {
		d.fail(ErrTransportClosed)
	}
	return nil
}

// device returns the open webinspector connection of a device, dialing it
// once when needed. Concurrent callers share one dial.
func (t *Transport) device(ctx context.Context, udid string) (*wirDevice, error) {
	if udid == "" {
		return nil, errors.New("direct: no device id given")
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrTransportClosed
	}
	if d := t.devices[udid]; d != nil {
		t.mu.Unlock()
		return d, nil
	}
	if c := t.dialing[udid]; c != nil {
		t.mu.Unlock()
		select {
		case <-c.done:
			return c.dev, c.err
		case <-ctx.Done():
			return nil, fmt.Errorf("direct: waiting for the connection to device %s: %w", shortID(udid), context.Cause(ctx))
		}
	}
	// The dial keeps the caller's deadline and also ends when Close cancels it.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c := &dialCall{done: make(chan struct{}), cancel: cancel}
	t.dialing[udid] = c
	t.mu.Unlock()

	conn, err := t.dialer.Dial(ctx, udid)
	var d *wirDevice
	if err == nil {
		d, err = openDevice(ctx, udid, conn, t.opts, t.forget)
	}
	if err == nil && t.beforePublish != nil {
		t.beforePublish(d)
	}
	t.mu.Lock()
	delete(t.dialing, udid)
	if err == nil && t.closed {
		err = ErrTransportClosed
	}
	if err == nil {
		// The connection may have ended since the handshake; its forget
		// then had nothing to drop. Checking under t.mu keeps a failed
		// device out of the table: a failure after this point finds it
		// there and drops it.
		if cause := d.closedErr(); cause != nil {
			err = fmt.Errorf("direct: the connection to device %s ended while it was being set up: %w", shortID(udid), cause)
		}
	}
	if err == nil {
		t.devices[udid] = d
	}
	c.dev, c.err = d, err
	close(c.done)
	t.mu.Unlock()
	if err != nil && d != nil {
		d.fail(err)
		d = nil
	}
	return d, err
}

// forget drops a device whose connection ended, so the next call dials again.
func (t *Transport) forget(d *wirDevice) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.devices[d.udid] == d {
		delete(t.devices, d.udid)
	}
}
