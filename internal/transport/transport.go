// Package transport hides how the daemon reaches a page: through
// ios_webkit_debug_proxy (iwdp), directly through usbmuxd and webinspectord,
// or through a local WebKitGTK MiniBrowser (design docs/design/agentic-parity.md,
// section 2.8). Target routing, provisional-target swaps, deadlines and
// reconnects live above this package, in webkit.Session.
package transport

import (
	"context"
	"errors"
)

// ErrNotWritten marks a Write that was refused before any byte of the
// message was written, so the far side never saw it.
var ErrNotWritten = errors.New("not written")

// Kind names a transport implementation.
type Kind string

// Transport kinds (design 2.8).
const (
	KindIWDP      Kind = "iwdp"
	KindDirect    Kind = "direct"
	KindWebKitGTK Kind = "webkitgtk"
)

// Device is one inspectable device or local browser.
type Device struct {
	ID        string // UDID or synthetic id; never printed in full by default
	Label     string // short label for output, for example "iPhone X …a1b2"
	Name      string
	OSVersion string // "16.7.10"; "" when unknown. iwdp lists it as deviceOSVersion.
	Transport Kind
}

// Page is one inspectable page (Safari tab or web view) of a device.
type Page struct {
	DeviceID   string
	ID         string // transport-local id, stable while the tab lives
	Title, URL string
	AppID      string // for example "com.apple.mobilesafari"
	Debuggable bool
	// Ownership is the non-disruptive hint whether another inspector holds
	// the page (design 2.5). It is a hint, not a lock.
	Ownership Ownership
}

// Ownership is what a listing says about other inspector clients of a page.
type Ownership int

const (
	// OwnershipUnknown: the listing has no hint (iwdp with -F, direct,
	// WebKitGTK). A physical device then needs a reservation (design 2.5).
	OwnershipUnknown Ownership = iota
	// OwnershipFree: the listing shows that no client holds the page.
	OwnershipFree
	// OwnershipBusy: the listing shows that another client holds the page.
	OwnershipBusy
)

func (o Ownership) String() string {
	switch o {
	case OwnershipFree:
		return "free"
	case OwnershipBusy:
		return "busy"
	}
	return "unknown"
}

// Transport lists devices and pages and opens page connections.
type Transport interface {
	Kind() Kind
	Devices(ctx context.Context) ([]Device, error)
	Pages(ctx context.Context, deviceID string) ([]Page, error)
	Open(ctx context.Context, p Page) (MessageConn, error)
	Close() error
}

// MessageConn carries WIP JSON text messages for one page connection.
// It knows nothing about Target routing; webkit.Session does that.
type MessageConn interface {
	// Write sends one message. It honours the ctx deadline. An error that
	// wraps ErrNotWritten means that nothing of the message was written.
	Write(ctx context.Context, msg []byte) error
	// Read returns the next message. It returns an error after Close or
	// when the far side closes the connection.
	Read(ctx context.Context) ([]byte, error)
	Close() error
	Limits() Limits
}

// Limits describes what the far side of a MessageConn accepts.
type Limits struct {
	// MaxOutgoing is the largest message the far side accepts, in serialized
	// wire bytes: WebKitGTK about 128 KiB (spike A), iwdp 64 MiB, direct 16 MiB.
	// Zero means no known limit.
	MaxOutgoing int
	// MaxIncoming is the read limit: 512 MiB for WebSocket connections.
	MaxIncoming int
	// Routed is true when the endpoint uses Target routing.
	Routed bool
}

// Limits of the known endpoints (design 2.8 and spike A step D).
const (
	// WebKitGTKMaxOutgoing: libsoup closes the inspector socket for an
	// incoming message above about 128 KiB (127 KiB passed, 130 KiB closed).
	WebKitGTKMaxOutgoing = 127 * 1024
	// IWDPMaxOutgoing is the outgoing limit of ios_webkit_debug_proxy.
	IWDPMaxOutgoing = 64 << 20
	// DefaultMaxIncoming is the WebSocket read limit (heap snapshots can be
	// 50-200+ MB, and Target routing escapes them once more).
	DefaultMaxIncoming = 512 << 20
)
