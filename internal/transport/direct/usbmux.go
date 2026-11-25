package direct

// This file is the only code that uses go-ios (github.com/danielpaulus/go-ios,
// MIT). It uses go-ios only for steps that take a connection this package
// dialed itself: usbmux messages, lockdown messages and the TLS upgrade. go-ios's
// own entry points (ConnectToService, ConnectLockdownWithSession, StartService,
// ReadPairRecord) dial without a context and leave usbmux sockets open on some
// error paths (go-ios issue #880), so they are not called here (design
// section 6, "Socket ownership").

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"

	"github.com/nnemirovsky/agent-ios/internal/transport"
)

const (
	webInspectorService = "com.apple.webinspector"
	usbConnection       = "USB"
	// maxDialRetries: lockdown failures get at most 2 retries (design section
	// 6). A failed lockdown session can leave a device unreachable (go-ios
	// issue #880), so retries stay few.
	maxDialRetries = 2
	// dialRetryBackoff is the wait before the first retry; the second waits twice as long.
	dialRetryBackoff = 250 * time.Millisecond
	// stopSessionBudget bounds the courtesy StopSession after StartService.
	stopSessionBudget = 500 * time.Millisecond
	// infoBudget bounds the lockdown query for the version and name of one device.
	infoBudget = 2 * time.Second
	// usbmuxClientName identifies this program to usbmuxd.
	usbmuxClientName = "agent-ios"
)

// deviceConn is a device stream with deadlines (the TLS service connection
// in production, one end of a net.Pipe in tests).
type deviceConn interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// deviceDialer is the device layer of the direct transport. usbmuxDialer is
// the production implementation; tests supply fakes.
type deviceDialer interface {
	// Devices lists USB devices with the OS version from lockdown.
	Devices(ctx context.Context) ([]transport.Device, error)
	// Dial opens the com.apple.webinspector service of a device. The
	// returned connection has no deadlines set.
	Dial(ctx context.Context, udid string) (deviceConn, error)
}

// lockdownError marks a failed lockdown step. Only these are retried.
type lockdownError struct {
	step string
	err  error
}

func (e *lockdownError) Error() string { return e.step + ": " + e.err.Error() }
func (e *lockdownError) Unwrap() error { return e.err }

// usbmuxDialer talks to usbmuxd and lockdown with go-ios. It owns every
// socket it dials and closes each on every path.
type usbmuxDialer struct {
	network, address string // from Options.SocketAddress
	backoff          time.Duration

	mu   sync.Mutex
	info map[infoKey]deviceInfo // lockdown values per attached device
}

type infoKey struct {
	udid     string
	deviceID int // usbmuxd's id; it changes when the device is attached again
}

type deviceInfo struct {
	name, version string
}

func newUsbmuxDialer(socketAddress string) (*usbmuxDialer, error) {
	if socketAddress == "" {
		socketAddress = ios.GetUsbmuxdSocket()
	}
	scheme, address, ok := strings.Cut(socketAddress, "://")
	if !ok || (scheme != "unix" && scheme != "tcp") || address == "" {
		return nil, fmt.Errorf("direct: usbmuxd socket address %q: want unix:///path or tcp://host:port", socketAddress)
	}
	return &usbmuxDialer{network: scheme, address: address, backoff: dialRetryBackoff, info: make(map[infoKey]deviceInfo)}, nil
}

// boundConn is a dialed socket bound to a context: when the context ends,
// its deadline moves to the past, so every blocked read and write returns.
type boundConn struct {
	net.Conn
	stop func() bool
}

func (d *usbmuxDialer) connect(ctx context.Context) (*boundConn, error) {
	var nd net.Dialer
	c, err := nd.DialContext(ctx, d.network, d.address)
	if err != nil {
		return nil, fmt.Errorf("connecting to usbmuxd at %s://%s: %w; is usbmuxd running?", d.network, d.address, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	return &boundConn{Conn: c, stop: stop}, nil
}

// Close stops the context binding and closes the socket.
func (b *boundConn) Close() error {
	b.stop()
	return b.Conn.Close()
}

// unbind removes the context binding from a socket that is handed on. It
// fails when the context already ended.
func (b *boundConn) unbind(ctx context.Context) error {
	if !b.stop() {
		return fmt.Errorf("handing on the service connection: %w", context.Cause(ctx))
	}
	return b.SetDeadline(time.Time{})
}

func muxOver(c net.Conn) (*ios.UsbMuxConnection, *ios.DeviceConnection) {
	dc := ios.NewDeviceConnectionWithConn(c)
	return ios.NewUsbMuxConnection(dc), dc
}

// listUSB returns the USB entries of usbmuxd's device list, one per UDID.
func (d *usbmuxDialer) listUSB(ctx context.Context) ([]ios.DeviceEntry, error) {
	c, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	mux, _ := muxOver(c)
	list, err := mux.ListDevices()
	if err != nil {
		return nil, fmt.Errorf("listing devices from usbmuxd: %w", err)
	}
	var out []ios.DeviceEntry
	seen := make(map[string]bool)
	for _, e := range list.DeviceList {
		udid := e.Properties.SerialNumber
		if e.Properties.ConnectionType != usbConnection || udid == "" || seen[udid] {
			continue
		}
		seen[udid] = true
		out = append(out, e)
	}
	return out, nil
}

// Devices lists USB devices. The name and OS version come from a lockdown
// query without a session (no pairing step, no TLS); when that query fails,
// both stay empty, which transport.Device documents as unknown, and the next
// call asks again.
func (d *usbmuxDialer) Devices(ctx context.Context) ([]transport.Device, error) {
	entries, err := d.listUSB(ctx)
	if err != nil {
		return nil, fmt.Errorf("direct: %w", err)
	}
	out := make([]transport.Device, 0, len(entries))
	for _, e := range entries {
		info := d.deviceInfo(ctx, e)
		out = append(out, transport.Device{
			ID:        e.Properties.SerialNumber,
			Label:     deviceLabel(info.name, e.Properties.SerialNumber),
			Name:      info.name,
			OSVersion: info.version,
			Transport: transport.KindDirect,
		})
	}
	return out, nil
}

func deviceLabel(name, udid string) string {
	if name == "" {
		name = "iOS device"
	}
	tail := udid
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return name + " …" + tail
}

func (d *usbmuxDialer) deviceInfo(ctx context.Context, e ios.DeviceEntry) deviceInfo {
	key := infoKey{udid: e.Properties.SerialNumber, deviceID: e.DeviceID}
	d.mu.Lock()
	info, ok := d.info[key]
	d.mu.Unlock()
	if ok {
		return info
	}
	ctx, cancel := context.WithTimeout(ctx, infoBudget)
	defer cancel()
	info, err := d.queryInfo(ctx, e.DeviceID)
	if err != nil {
		return deviceInfo{}
	}
	d.mu.Lock()
	d.info[key] = info
	d.mu.Unlock()
	return info
}

func (d *usbmuxDialer) queryInfo(ctx context.Context, deviceID int) (deviceInfo, error) {
	c, err := d.connect(ctx)
	if err != nil {
		return deviceInfo{}, err
	}
	defer func() { _ = c.Close() }()
	mux, _ := muxOver(c)
	ld, err := mux.ConnectLockdown(deviceID)
	if err != nil {
		return deviceInfo{}, err
	}
	version, err := ld.GetProductVersion()
	if err != nil {
		return deviceInfo{}, err
	}
	name, err := ld.GetValue("DeviceName")
	if err != nil {
		return deviceInfo{}, err
	}
	s, _ := name.(string) // a missing name stays empty; the label then says "iOS device"
	return deviceInfo{name: s, version: version}, nil
}

// Dial opens com.apple.webinspector on the device. Lockdown failures are
// retried at most maxDialRetries times, and only while the context leaves
// time for the backoff.
func (d *usbmuxDialer) Dial(ctx context.Context, udid string) (deviceConn, error) {
	for attempt := 0; ; attempt++ {
		conn, err := d.dialOnce(ctx, udid)
		if err == nil {
			return conn, nil
		}
		err = fmt.Errorf("direct: opening the web inspector service of device %s: %w", shortID(udid), err)
		if cerr := context.Cause(ctx); cerr != nil {
			return nil, fmt.Errorf("%w: %w", cerr, err)
		}
		var le *lockdownError
		if !errors.As(err, &le) || attempt == maxDialRetries {
			return nil, err
		}
		wait := d.backoff * time.Duration(attempt+1)
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= wait {
			return nil, err
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, err
		}
	}
}

func (d *usbmuxDialer) dialOnce(ctx context.Context, udid string) (deviceConn, error) {
	entries, err := d.listUSB(ctx)
	if err != nil {
		return nil, err
	}
	var entry *ios.DeviceEntry
	for i := range entries {
		if entries[i].Properties.SerialNumber == udid {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return nil, errors.New("the device is not attached over USB (usbmuxd does not list it)")
	}

	// Lockdown: pair record, session with TLS, StartService. The socket closes
	// on every path; lockdown is only needed to learn the service port.
	ldConn, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ldConn.Close() }()
	mux, _ := muxOver(ldConn)
	pair, err := readPairRecord(mux, udid)
	if err != nil {
		return nil, err
	}
	ld, err := mux.ConnectLockdown(entry.DeviceID)
	if err != nil {
		return nil, &lockdownError{"connecting to lockdown", err}
	}
	if _, err := ld.StartSession(pair); err != nil {
		return nil, &lockdownError{"starting the lockdown session (TLS)", err}
	}
	svc, err := ld.StartService(webInspectorService)
	if err != nil {
		return nil, &lockdownError{"starting " + webInspectorService, err}
	}
	stopSession(ctx, ld, ldConn)

	// The service: a second usbmux connection to the returned port, then TLS.
	sc, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	handedOn := false
	defer func() {
		if !handedOn {
			_ = sc.Close()
		}
	}()
	smux, sdc := muxOver(sc)
	if err := smux.Connect(entry.DeviceID, svc.Port); err != nil {
		return nil, &lockdownError{"connecting to the service port", err}
	}
	if svc.EnableServiceSSL {
		if err := sdc.EnableSessionSsl(pair); err != nil {
			return nil, &lockdownError{"TLS handshake with the service", err}
		}
	}
	if err := sc.unbind(ctx); err != nil {
		return nil, err
	}
	handedOn = true
	return &serviceConn{raw: sc.Conn, rw: sdc}, nil
}

// stopSession ends the lockdown session politely within a short budget. The
// socket closes right after, which also ends the session, so a failure here
// changes nothing.
func stopSession(ctx context.Context, ld *ios.LockDownConnection, c *boundConn) {
	dl := time.Now().Add(stopSessionBudget)
	if ctxDL, ok := ctx.Deadline(); ok && ctxDL.Before(dl) {
		dl = ctxDL
	}
	if err := c.SetDeadline(dl); err != nil {
		return
	}
	ld.StopSession()
}

// readPairRecord asks usbmuxd for the pair record of udid. It is written here
// instead of using go-ios's ReadPair because that logs the raw record, which
// holds the host private key, when it cannot decode it.
func readPairRecord(mux *ios.UsbMuxConnection, udid string) (ios.PairRecord, error) {
	req := ios.ReadPair{
		BundleID: usbmuxClientName, ClientVersionString: usbmuxClientName, MessageType: "ReadPairRecord",
		ProgName: usbmuxClientName, LibUSBMuxVersion: 3, PairRecordID: udid,
	}
	if err := mux.Send(req); err != nil {
		return ios.PairRecord{}, fmt.Errorf("requesting the pair record from usbmuxd: %w", err)
	}
	resp, err := mux.ReadMessage()
	if err != nil {
		return ios.PairRecord{}, fmt.Errorf("reading the pair record from usbmuxd: %w", err)
	}
	var data ios.PairRecordData
	if _, err := plist.Unmarshal(resp.Payload, &data); err != nil || len(data.PairRecordData) == 0 {
		return ios.PairRecord{}, fmt.Errorf("usbmuxd has no pair record for this device (usbmuxd result %d); unlock the device, tap Trust and pair it with this computer",
			ios.MuxResponsefromBytes(resp.Payload).Number)
	}
	var rec ios.PairRecord
	if _, err := plist.Unmarshal(data.PairRecordData, &rec); err != nil {
		return ios.PairRecord{}, errors.New("the pair record from usbmuxd is not a valid property list; pair the device again")
	}
	if len(rec.HostCertificate) == 0 || len(rec.HostPrivateKey) == 0 {
		return ios.PairRecord{}, errors.New("the pair record from usbmuxd has no host certificate or key; pair the device again")
	}
	return rec, nil
}

// serviceConn is the webinspector service stream: reads and writes go through
// TLS when the service asked for it, deadlines and Close act on the socket.
type serviceConn struct {
	raw net.Conn
	rw  io.ReadWriter
}

func (s *serviceConn) Read(p []byte) (int, error)         { return s.rw.Read(p) }
func (s *serviceConn) Write(p []byte) (int, error)        { return s.rw.Write(p) }
func (s *serviceConn) Close() error                       { return s.raw.Close() }
func (s *serviceConn) SetReadDeadline(t time.Time) error  { return s.raw.SetReadDeadline(t) }
func (s *serviceConn) SetWriteDeadline(t time.Time) error { return s.raw.SetWriteDeadline(t) }

// SetLogger routes go-ios's internal logging, process-wide. go-ios logs to
// slog.Default otherwise, for example "Handshake error" at Info level.
func SetLogger(l *slog.Logger) { ios.SetLogger(l) }

// shortID shortens a UDID for messages (AGG [287], [333]: never print it in full).
func shortID(udid string) string {
	if len(udid) <= 4 {
		return udid
	}
	return "…" + udid[len(udid)-4:]
}
