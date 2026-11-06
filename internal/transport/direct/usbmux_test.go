package direct

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

func newTestTransport(t *testing.T, m *fakeUsbmuxd) *Transport {
	t.Helper()
	tr, err := New(Options{SocketAddress: m.socketAddress()})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// The whole path through the fake usbmuxd: device list with the version from
// lockdown, pair record, lockdown session with TLS, StartService, service
// connection with TLS, WIR session. Every socket is closed afterwards.
func TestUsbmuxPathToWebInspector(t *testing.T) {
	m := newFakeUsbmuxd(t, nil)
	tr := newTestTransport(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devs, err := tr.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	want := transport.Device{ID: testUDID, Label: "Fake iPhone …6F70", Name: "Fake iPhone", OSVersion: "16.7.10", Transport: transport.KindDirect}
	if len(devs) != 1 || devs[0] != want {
		t.Fatalf("Devices = %+v, want only the USB device %+v", devs, want)
	}
	if _, err := tr.Devices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := m.count(stepGetValue); n != 2 {
		t.Fatalf("lockdown GetValue calls = %d, want 2 (version and name, cached for the second listing)", n)
	}

	pages, err := tr.Pages(ctx, testUDID)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(pages) != 1 || pages[0].ID != "PID:100/1" || pages[0].Title != "Page" {
		t.Fatalf("Pages = %+v", pages)
	}
	if _, err := tr.Pages(ctx, testUDID); err != nil {
		t.Fatal(err)
	}
	if n := m.count(stepStartSession); n != 1 {
		t.Fatalf("lockdown sessions = %d, want 1: the webinspector connection is kept per device", n)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	m.waitBalanced(t)
	if m.accepted.Load() < 5 {
		t.Fatalf("accepted %d connections; the test did not reach the service", m.accepted.Load())
	}
}

// Acceptance 7.4 test 3: a failure, and in a second run a stall, at every
// step. After each run every accepted connection was closed by the client,
// the call returned within its deadline, and lockdown retries stopped after 2.
func TestUsbmuxFailureAndStallAtEveryStep(t *testing.T) {
	steps := []struct {
		step    muxStep
		retried bool // a lockdown step: retried at most maxDialRetries times
	}{
		{stepUsbmuxConnect, false},
		{stepListDevices, false},
		{stepReadPairRecord, false},
		{stepLockdownConnect, true},
		{stepStartSession, true},
		{stepSessionTLS, true},
		{stepStartService, true},
		{stepServiceConnect, true},
		{stepServiceTLS, true},
		{stepFirstRead, false},
	}
	for _, mode := range []injectMode{injectFail, injectStall} {
		for _, s := range steps {
			t.Run(fmt.Sprintf("%s at %s", mode, s.step), func(t *testing.T) {
				t.Parallel()
				m := newFakeUsbmuxd(t, map[muxStep]injectMode{s.step: mode})
				if s.step == stepUsbmuxConnect && mode == injectFail {
					_ = m.ln.Close() // usbmuxd is not running
				}
				tr := newTestTransport(t, m)
				deadline := 5 * time.Second
				if mode == injectStall {
					deadline = 800 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), deadline)
				defer cancel()
				start := time.Now()
				_, err := tr.Pages(ctx, testUDID)
				el := time.Since(start)
				if err == nil {
					t.Fatal("Pages succeeded")
				}
				if el > deadline+300*time.Millisecond {
					t.Fatalf("Pages returned after %s, past its deadline %s: %v", el, deadline, err)
				}
				if mode == injectStall && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stall: %v, want a deadline error", err)
				}
				if mode == injectFail && el > 2*time.Second {
					t.Fatalf("failure took %s: %v", el, err)
				}
				_ = tr.Close()
				m.waitBalanced(t)

				attempts := m.count(s.step)
				want := 1
				switch {
				case s.step == stepUsbmuxConnect && mode == injectFail:
					want = 0
					if !strings.Contains(err.Error(), "is usbmuxd running?") {
						t.Fatalf("error %q does not name the cause", err)
					}
				case s.retried && mode == injectFail:
					want = 1 + maxDialRetries
				}
				if attempts != want {
					t.Fatalf("step reached %d times, want %d (%v)", attempts, want, err)
				}
				if n := m.count(stepStartSession); n > 1+maxDialRetries {
					t.Fatalf("%d lockdown sessions, want at most %d", n, 1+maxDialRetries)
				}
			})
		}
	}
}

// Devices keeps the device when the lockdown query for its version fails or
// stalls; the version is then unknown, and every socket is closed.
func TestUsbmuxDevicesVersionQueryFails(t *testing.T) {
	for _, mode := range []injectMode{injectFail, injectStall} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			m := newFakeUsbmuxd(t, map[muxStep]injectMode{stepGetValue: mode})
			tr := newTestTransport(t, m)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			devs, err := tr.Devices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(devs) != 1 || devs[0].OSVersion != "" || devs[0].Label != "iOS device …6F70" {
				t.Fatalf("Devices = %+v, want the device with an unknown version", devs)
			}
			if el := time.Since(start); el > infoBudget+300*time.Millisecond {
				t.Fatalf("Devices took %s, want at most the %s info budget", el, infoBudget)
			}
			_ = tr.Close()
			m.waitBalanced(t)
		})
	}
}

func TestNewRejectsBadSocketAddress(t *testing.T) {
	for _, addr := range []string{"/var/run/usbmuxd", "udp://x", "unix://"} {
		if _, err := New(Options{SocketAddress: addr}); err == nil || !strings.Contains(err.Error(), addr) {
			t.Fatalf("New(%q) = %v, want an error naming the address", addr, err)
		}
	}
}

// Retries stay inside the deadline: a retry is made only when the context
// leaves time for its backoff, and the lockdown error is returned, not a
// deadline error.
func TestUsbmuxRetriesStopWhenTheDeadlineLeavesNoTime(t *testing.T) {
	m := newFakeUsbmuxd(t, map[muxStep]injectMode{stepStartSession: injectFail})
	tr := newTestTransport(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err := tr.Pages(ctx, testUDID)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "InvalidHostID") {
		t.Fatalf("Pages: %v, want the lockdown error before the deadline", err)
	}
	if n := m.count(stepStartSession); n != 2 {
		t.Fatalf("lockdown sessions = %d, want 2 (the second backoff of 500 ms does not fit)", n)
	}
	_ = tr.Close()
	m.waitBalanced(t)
}

// A device that usbmuxd does not list over USB is an error without a
// lockdown attempt.
func TestUsbmuxDeviceNotListed(t *testing.T) {
	m := newFakeUsbmuxd(t, nil)
	m.udid = "SOME-OTHER-DEVICE"
	tr := newTestTransport(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tr.Pages(ctx, testUDID); err == nil || !strings.Contains(err.Error(), "not attached over USB") {
		t.Fatalf("Pages: %v", err)
	}
	if n := m.count(stepLockdownConnect); n != 0 {
		t.Fatalf("lockdown connects = %d, want 0", n)
	}
	_ = tr.Close()
	m.waitBalanced(t)
}

// An empty socket address takes USBMUXD_SOCKET_ADDRESS (go-ios's rules).
func TestNewUsesTheUsbmuxdEnvironment(t *testing.T) {
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "/tmp/custom-usbmuxd")
	tr, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d := tr.dialer.(*usbmuxDialer); d.network != "unix" || d.address != "/tmp/custom-usbmuxd" {
		t.Fatalf("dialer = %s %s", d.network, d.address)
	}
}
