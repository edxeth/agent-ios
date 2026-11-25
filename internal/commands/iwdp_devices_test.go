package commands

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edxeth/agent-ios/internal/proxy"
)

// Device check of 2026-11-12: devices printed "No devices connected."
// with exit 0 while iwdp had lost the iPhone that usbmuxd still reported,
// and with no iwdp running it printed a manual start command, although
// every page command starts iwdp (decision D10). devices now starts and
// recovers iwdp as page commands do, and says what to do about a device
// that iwdp does not list.

// devicesSeams replaces the listing address and the iwdp starts of devices
// for t, and counts the starts and recoveries.
type devicesSeams struct {
	starts, recovers atomic.Int32
}

func useDevicesSeams(t *testing.T, addr string, start func() error, recov func() error, attached []string, attachedErr error) *devicesSeams {
	t.Helper()
	s := &devicesSeams{}
	saved := []any{devicesListing, devicesStart, devicesRecover, devicesAwait, devicesAttached}
	devicesListing = func() string { return addr }
	devicesStart = func(context.Context) error { s.starts.Add(1); return start() }
	devicesRecover = func(context.Context) (bool, error) { s.recovers.Add(1); return false, recov() }
	devicesAwait = func(context.Context) (bool, error) { return true, nil }
	devicesAttached = func(context.Context) ([]string, error) { return attached, attachedErr }
	t.Cleanup(func() {
		devicesListing = saved[0].(func() string)
		devicesStart = saved[1].(func(context.Context) error)
		devicesRecover = saved[2].(func(context.Context) (bool, error))
		devicesAwait = saved[3].(func(context.Context) (bool, error))
		devicesAttached = saved[4].(func(context.Context) ([]string, error))
	})
	return s
}

// listingServer serves body as a device list on a private address.
func listingServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// freeListingAddr returns a loopback address where nothing listens.
func freeListingAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func ok() error { return nil }

// An empty listing with an attached USB device exits 5 with the device
// advice; without one, or when usbmuxd's list cannot be read, devices
// prints the legacy line with exit 0. The recovery runs before the read.
func TestDevicesExplainsAnUnlistedAttachedDevice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attached []string
		err      error
		code     int
	}{
		{"attached", []string{"udid-1"}, nil, 5},
		{"none attached", nil, nil, 0},
		{"unreadable", nil, errors.New("usbmuxd's socket: connection refused"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := useDevicesSeams(t, listingServer(t, "[]"), ok, ok, tc.attached, tc.err)
			code, stdout, stderr, _ := runCLI("devices")
			if code != tc.code {
				t.Fatalf("devices: exit %d, stdout %q, stderr %q; want exit %d", code, stdout, stderr, tc.code)
			}
			if tc.code == 0 && stdout != "No devices connected.\n" {
				t.Errorf("devices: stdout %q, want the legacy line", stdout)
			}
			if tc.code != 0 {
				for _, want := range []string{"lists no device, although usbmuxd reports 1 USB device", "Unlock the device", "run devices again"} {
					if !strings.Contains(stderr, want) {
						t.Errorf("stderr %q does not contain %q", stderr, want)
					}
				}
			}
			if s.recovers.Load() != 1 || s.starts.Load() != 0 {
				t.Errorf("devices ran %d recoveries and %d starts, want 1 and 0", s.recovers.Load(), s.starts.Load())
			}
		})
	}
}

// With nothing on the listing address devices starts iwdp; a failed start
// exits 1 with its error. A refused recovery exits 5 with its error.
func TestDevicesStartsAndRecoversIWDP(t *testing.T) {
	t.Run("start fails", func(t *testing.T) {
		s := useDevicesSeams(t, freeListingAddr(t), func() error { return errors.New("exited during its start") }, ok, nil, nil)
		code, stdout, stderr, _ := runCLI("devices")
		if code != 1 || !strings.Contains(stderr, "not running and starting it failed: exited during its start") {
			t.Fatalf("devices with a failing start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if s.starts.Load() != 1 {
			t.Errorf("devices ran %d starts, want 1", s.starts.Load())
		}
	})
	t.Run("recovery refused", func(t *testing.T) {
		useDevicesSeams(t, listingServer(t, "[]"), ok, func() error { return proxy.ErrDeviceLost }, nil, nil)
		code, stdout, stderr, _ := runCLI("devices")
		if code != 5 || !strings.Contains(stderr, "lost its connection to the device") {
			t.Fatalf("devices with a refused recovery: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("started", func(t *testing.T) {
		addr := freeListingAddr(t)
		var srv *http.Server
		start := func() error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[{"deviceId":"udid-1","deviceName":"iPhone X","url":"localhost:9224"}]`))
			})}
			go func() { _ = srv.Serve(ln) }()
			return nil
		}
		s := useDevicesSeams(t, addr, start, ok, nil, nil)
		t.Cleanup(func() {
			if srv != nil {
				_ = srv.Close()
			}
		})
		code, stdout, stderr, _ := runCLI("devices")
		if code != 0 || !strings.Contains(stdout, "iPhone X") || s.starts.Load() != 1 {
			t.Fatalf("devices that starts iwdp: exit %d, stdout %q, stderr %q, %d starts; want the started iwdp's device", code, stdout, stderr, s.starts.Load())
		}
	})
}

// An empty listing whose iwdp the recovery restarted is read again: the
// restarted iwdp's device is listed, with exit 0.
func TestDevicesReadsAgainAfterARecovery(t *testing.T) {
	var recovered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if recovered.Load() {
			_, _ = w.Write([]byte(`[{"deviceId":"udid-1","deviceName":"iPhone X","url":"localhost:9224"}]`))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	s := useDevicesSeams(t, strings.TrimPrefix(srv.URL, "http://"), ok, ok, []string{"udid-1"}, nil)
	devicesRecover = func(context.Context) (bool, error) { s.recovers.Add(1); recovered.Store(true); return true, nil }
	code, stdout, stderr, _ := runCLI("devices")
	if code != 0 || !strings.Contains(stdout, "iPhone X") || s.recovers.Load() != 1 {
		t.Fatalf("devices after a recovery: exit %d, stdout %q, stderr %q, %d recoveries; want the restarted iwdp's device", code, stdout, stderr, s.recovers.Load())
	}
}
