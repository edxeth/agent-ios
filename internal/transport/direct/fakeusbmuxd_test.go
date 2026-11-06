package direct

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"
)

// muxStep names a step of the path from usbmuxd to the webinspector service
// where the fake can inject a failure or a stall (design 7.4 test 3).
type muxStep string

const (
	stepUsbmuxConnect   muxStep = "usbmuxd connect"
	stepListDevices     muxStep = "ListDevices"
	stepReadPairRecord  muxStep = "ReadPairRecord"
	stepLockdownConnect muxStep = "lockdown connect"
	stepStartSession    muxStep = "lockdown session start"
	stepSessionTLS      muxStep = "TLS handshake"
	stepStartService    muxStep = "StartService"
	stepServiceConnect  muxStep = "service connect"
	stepServiceTLS      muxStep = "service TLS handshake"
	stepFirstRead       muxStep = "first read"
	stepGetValue        muxStep = "lockdown GetValue"
)

type injectMode int

const (
	injectNone  injectMode = iota
	injectFail             // a protocol error, or a closed stream where the protocol has none
	injectStall            // stop answering
	// Pair records that usbmuxd returns but the client must refuse
	// (stepReadPairRecord only). Each carries pairSecretMarker where it has
	// key material.
	injectBadPairRecord     // record bytes that are not a property list
	injectNoHostKey         // a record without the host private key
	injectNoHostCertificate // a record with the key but without the host certificate
)

func (m injectMode) String() string {
	return map[injectMode]string{
		injectNone: "none", injectFail: "failure", injectStall: "stall",
		injectBadPairRecord: "malformed pair record", injectNoHostKey: "pair record without key",
		injectNoHostCertificate: "pair record without certificate",
	}[m]
}

// pairSecretMarker stands for private key bytes in the refused pair records;
// it must never appear in an error or a log line.
const pairSecretMarker = "SECRET-HOST-PRIVATE-KEY-MATERIAL-5f3a9c0e71b2d4"

// refusedPairRecord returns the record bytes that usbmuxd sends for one of
// the refused-record modes.
func (m *fakeUsbmuxd) refusedPairRecord(mode injectMode) []byte {
	secretKey := []byte("-----BEGIN EC PRIVATE KEY-----\n" + pairSecretMarker + "\n-----END EC PRIVATE KEY-----\n")
	var rec ios.PairRecord
	switch mode {
	case injectBadPairRecord:
		return append([]byte("bplist00"), secretKey...) // a binary plist header, then no valid plist
	case injectNoHostKey:
		rec = ios.PairRecord{HostID: "HOST-ID", SystemBUID: "BUID", HostCertificate: []byte("-----BEGIN CERTIFICATE-----\n")}
	case injectNoHostCertificate:
		rec = ios.PairRecord{HostID: "HOST-ID", SystemBUID: "BUID", HostPrivateKey: secretKey}
	default:
		m.t.Errorf("fake usbmuxd: %s is not a refused pair record", mode)
		return nil
	}
	b, err := plist.Marshal(rec, plist.XMLFormat)
	if err != nil {
		m.t.Errorf("fake usbmuxd: %v", err)
	}
	return b
}

const (
	fakeDeviceID    = 7
	fakeServicePort = 51234
)

// pairMaterial holds a host certificate and key (in the pair record) and the
// device's TLS certificate, generated once per test binary.
type pairMaterial struct {
	record []byte // plist of ios.PairRecord
	device tls.Certificate
}

var (
	pairOnce sync.Once
	pairData pairMaterial
)

func selfSigned(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func testPairMaterial(t *testing.T) pairMaterial {
	pairOnce.Do(func() {
		hostCert, hostKey := selfSigned(t, "host")
		devCert, devKey := selfSigned(t, "device")
		rec, err := plist.Marshal(ios.PairRecord{HostID: "HOST-ID", SystemBUID: "BUID", HostCertificate: hostCert, HostPrivateKey: hostKey}, plist.XMLFormat)
		if err != nil {
			t.Fatal(err)
		}
		dev, err := tls.X509KeyPair(devCert, devKey)
		if err != nil {
			t.Fatal(err)
		}
		pairData = pairMaterial{record: rec, device: dev}
	})
	return pairData
}

// fakeUsbmuxd is a fake usbmuxd on a Unix socket with a fake lockdownd and a
// fake webinspector service behind it. It counts accepted connections and
// connections that the client closed: a connection counts as closed only
// when a read on the fake's side returns because the client closed its end.
type fakeUsbmuxd struct {
	t      *testing.T
	path   string
	ln     net.Listener
	udid   string
	pair   pairMaterial
	inject map[muxStep]injectMode
	apps   []fakeApp

	accepted, closed atomic.Int64

	mu      sync.Mutex
	reached map[muxStep]int
	conns   []net.Conn
	wg      sync.WaitGroup
}

func newFakeUsbmuxd(t *testing.T, inject map[muxStep]injectMode) *fakeUsbmuxd {
	t.Helper()
	dir, err := os.MkdirTemp("", "mux") // short: Unix socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "usbmuxd")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeUsbmuxd{
		t: t, path: path, ln: ln, udid: testUDID, pair: testPairMaterial(t), inject: inject,
		reached: make(map[muxStep]int),
		apps:    []fakeApp{{id: "PID:100", bundle: "com.apple.mobilesafari", pages: []fakePage{{id: 1, title: "Page", url: "https://a.example/"}}}},
	}
	m.wg.Add(1)
	go m.acceptLoop()
	t.Cleanup(m.stop)
	return m
}

func (m *fakeUsbmuxd) socketAddress() string { return "unix://" + m.path }

func (m *fakeUsbmuxd) stop() {
	_ = m.ln.Close()
	m.mu.Lock()
	for _, c := range m.conns {
		_ = c.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *fakeUsbmuxd) acceptLoop() {
	defer m.wg.Done()
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.accepted.Add(1)
		m.mu.Lock()
		m.conns = append(m.conns, c)
		m.mu.Unlock()
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.serveConn(c)
		}()
	}
}

// hit records that a step was reached and returns what to inject there.
func (m *fakeUsbmuxd) hit(s muxStep) injectMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reached[s]++
	return m.inject[s]
}

func (m *fakeUsbmuxd) count(s muxStep) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reached[s]
}

// waitClientClose reads the raw socket until the client closes it, then
// counts the connection as closed.
func (m *fakeUsbmuxd) waitClientClose(c net.Conn) {
	_, _ = io.Copy(io.Discard, c)
	m.closed.Add(1)
}

// failStream ends the fake's direction of the stream, so the client reads
// EOF, and waits for the client to close.
func (m *fakeUsbmuxd) failStream(c net.Conn) {
	_ = c.(*net.UnixConn).CloseWrite()
	m.waitClientClose(c)
}

// waitBalanced waits until every accepted connection was closed by the client.
func (m *fakeUsbmuxd) waitBalanced(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for m.accepted.Load() != m.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("accepted %d connections, the client closed %d", m.accepted.Load(), m.closed.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readMux(r io.Reader) (tag uint32, msg map[string]any, err error) {
	var h struct{ Length, Version, Request, Tag uint32 }
	if err := binary.Read(r, binary.LittleEndian, &h); err != nil {
		return 0, nil, err
	}
	body := make([]byte, h.Length-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	_, err = plist.Unmarshal(body, &msg)
	return h.Tag, msg, err
}

func writeMux(w io.Writer, tag uint32, v any) {
	body, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, struct{ Length, Version, Request, Tag uint32 }{uint32(16 + len(body)), 1, 8, tag})
	buf.Write(body)
	_, _ = w.Write(buf.Bytes())
}

func readLockdown(r io.Reader) (map[string]any, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var msg map[string]any
	_, err := plist.Unmarshal(body, &msg)
	return msg, err
}

func writeLockdown(w io.Writer, v any) {
	body, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(body)))
	buf.Write(body)
	_, _ = w.Write(buf.Bytes())
}

func (m *fakeUsbmuxd) serveConn(c net.Conn) {
	if m.hit(stepUsbmuxConnect) == injectStall {
		m.waitClientClose(c)
		return
	}
	for {
		tag, msg, err := readMux(c)
		if err != nil {
			m.waitClientClose(c)
			return
		}
		switch msg["MessageType"] {
		case "ListDevices":
			switch m.hit(stepListDevices) {
			case injectFail:
				m.failStream(c)
				return
			case injectStall:
				m.waitClientClose(c)
				return
			}
			props := func(conn, udid string, id int) map[string]any {
				return map[string]any{"DeviceID": id, "MessageType": "Attached", "Properties": map[string]any{
					"ConnectionType": conn, "SerialNumber": udid, "DeviceID": id,
				}}
			}
			writeMux(c, tag, map[string]any{"DeviceList": []any{
				props("USB", m.udid, fakeDeviceID),
				props("Network", m.udid, fakeDeviceID+1), // the same device over Wi-Fi: listed once
				props("Network", "WIFI-ONLY-0001", fakeDeviceID+2),
			}})
		case "ReadPairRecord":
			switch mode := m.hit(stepReadPairRecord); mode {
			case injectFail:
				writeMux(c, tag, map[string]any{"MessageType": "Result", "Number": 2})
				m.waitClientClose(c)
				return
			case injectStall:
				m.waitClientClose(c)
				return
			case injectBadPairRecord, injectNoHostKey, injectNoHostCertificate:
				writeMux(c, tag, map[string]any{"PairRecordData": m.refusedPairRecord(mode)})
				m.waitClientClose(c)
				return
			}
			writeMux(c, tag, map[string]any{"PairRecordData": m.pair.record})
		case "Connect":
			port, _ := plistInt(msg["PortNumber"])
			step := stepServiceConnect
			if uint16(port) == ios.Lockdownport {
				step = stepLockdownConnect
			} else if uint16(port) != ios.Ntohs(fakeServicePort) {
				m.t.Errorf("fake usbmuxd: Connect to unknown port %d", port)
			}
			switch m.hit(step) {
			case injectFail:
				writeMux(c, tag, map[string]any{"MessageType": "Result", "Number": 3})
				m.waitClientClose(c)
				return
			case injectStall:
				m.waitClientClose(c)
				return
			}
			writeMux(c, tag, map[string]any{"MessageType": "Result", "Number": 0})
			if step == stepLockdownConnect {
				m.serveLockdown(c)
			} else {
				m.serveService(c)
			}
			return
		default:
			m.t.Errorf("fake usbmuxd: unknown message %v", msg["MessageType"])
		}
	}
}

func (m *fakeUsbmuxd) serveLockdown(c net.Conn) {
	var rw io.ReadWriter = c
	for {
		req, err := readLockdown(rw)
		if err != nil {
			m.waitClientClose(c)
			return
		}
		switch req["Request"] {
		case "GetValue":
			key, _ := req["Key"].(string)
			if m.hit(stepGetValue) == injectFail {
				writeLockdown(rw, map[string]any{"Request": "GetValue", "Key": key, "Error": "MissingValue"})
				continue
			} else if m.inject[stepGetValue] == injectStall {
				m.waitClientClose(c)
				return
			}
			value := map[string]string{"ProductVersion": "16.7.10", "DeviceName": "Fake iPhone"}[key]
			writeLockdown(rw, map[string]any{"Request": "GetValue", "Key": key, "Value": value})
		case "StartSession":
			switch m.hit(stepStartSession) {
			case injectFail:
				writeLockdown(rw, map[string]any{"Request": "StartSession", "Error": "InvalidHostID"})
				m.waitClientClose(c)
				return
			case injectStall:
				m.waitClientClose(c)
				return
			}
			writeLockdown(rw, map[string]any{"Request": "StartSession", "SessionID": "SESSION-1", "EnableSessionSSL": true})
			tc, ok := m.serverTLS(c, stepSessionTLS)
			if !ok {
				return
			}
			rw = tc
		case "StartService":
			switch m.hit(stepStartService) {
			case injectFail:
				writeLockdown(rw, map[string]any{"Request": "StartService", "Error": "InvalidService"})
				m.waitClientClose(c)
				return
			case injectStall:
				m.waitClientClose(c)
				return
			}
			writeLockdown(rw, map[string]any{"Request": "StartService", "Service": req["Service"], "Port": fakeServicePort, "EnableServiceSSL": true})
		case "StopSession":
			writeLockdown(rw, map[string]any{"Request": "StopSession"})
		default:
			m.t.Errorf("fake lockdownd: unknown request %v", req["Request"])
		}
	}
}

// serverTLS runs the device side of a TLS handshake on c, with injection at
// step. It returns false when the connection is done.
func (m *fakeUsbmuxd) serverTLS(c net.Conn, step muxStep) (*tls.Conn, bool) {
	switch m.hit(step) {
	case injectFail:
		m.failStream(c)
		return nil, false
	case injectStall:
		m.waitClientClose(c)
		return nil, false
	}
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{m.pair.device}})
	if err := tc.Handshake(); err != nil {
		m.waitClientClose(c)
		return nil, false
	}
	return tc, true
}

func (m *fakeUsbmuxd) serveService(c net.Conn) {
	tc, ok := m.serverTLS(c, stepServiceTLS)
	if !ok {
		return
	}
	f := newFakeInspector(m.t, tc, m.apps)
	switch m.hit(stepFirstRead) {
	case injectFail:
		f.failOnFirst = true
	case injectStall:
		f.silent = true
	}
	f.serve()
	m.waitClientClose(c)
}
