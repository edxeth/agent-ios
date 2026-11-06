package direct

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A device whose connection ends after the WIR handshake and before it is
// published is not cached: the call that dialed it fails with the cause, and
// the next call dials again (review finding: a failed device was published
// after forget had run, so later calls reused the dead connection).
func TestDeviceThatEndsBeforePublicationIsDialedAgain(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	t.Cleanup(func() { _ = tr.Close() })
	disconnect := errors.New("test: the device disconnected")
	first := true
	tr.beforePublish = func(dev *wirDevice) {
		if first {
			first = false
			// fail is what the read loop runs when the stream ends; calling
			// it here makes the disconnect, including forget, complete
			// before publication.
			dev.fail(disconnect)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tr.Pages(ctx, testUDID); !errors.Is(err, disconnect) {
		t.Fatalf("Pages on the device that ended before publication: %v, want its cause", err)
	}
	if _, err := tr.Pages(ctx, testUDID); err != nil {
		t.Fatalf("Pages after the failed device: %v, want a new connection", err)
	}
	d.mu.Lock()
	dials := d.dials
	d.mu.Unlock()
	if dials != 2 {
		t.Fatalf("dials = %d, want 2: the failed device must not be reused", dials)
	}
}

// Close ends a connection that is still being dialed, even when the caller's
// context has no deadline: the pending call returns ErrTransportClosed, every
// socket is closed and no goroutine of the dial is left (review finding:
// Close ignored dials in progress).
func TestCloseEndsDialsInProgress(t *testing.T) {
	for _, step := range []muxStep{stepStartSession, stepServiceTLS, stepFirstRead} {
		t.Run(string(step), func(t *testing.T) {
			m := newFakeUsbmuxd(t, map[muxStep]injectMode{step: injectStall})
			tr := newTestTransport(t, m)
			before := runtime.NumGoroutine()
			ctx, cancel := context.WithCancel(context.Background()) // no deadline
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := tr.Pages(ctx, testUDID)
				done <- err
			}()
			waitReached(t, m, step)
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrTransportClosed) {
					t.Fatalf("pending Pages after Close: %v, want ErrTransportClosed", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Close did not end the pending dial")
			}
			m.waitBalanced(t)
			deadline := time.Now().Add(3 * time.Second)
			for runtime.NumGoroutine() > before {
				if time.Now().After(deadline) {
					buf := make([]byte, 1<<20)
					t.Fatalf("%d goroutines after Close, %d before the dial:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// Close after the WIR handshake and before publication: the dial's own
// cancellation no longer matters, so the device is closed instead of
// published and the call returns ErrTransportClosed (Close contract).
func TestCloseBeforePublicationClosesTheNewDevice(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	tr := newWithDialer(d, Options{})
	tr.beforePublish = func(*wirDevice) { _ = tr.Close() }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tr.Pages(ctx, testUDID); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Pages when Close came before publication: %v, want ErrTransportClosed", err)
	}
	select {
	case <-d.fake(0).closed: // the fake's serve ends when the client closes its end
	case <-time.After(2 * time.Second):
		t.Fatal("the device connection stayed open after Close")
	}
}

func waitReached(t *testing.T, m *fakeUsbmuxd, step muxStep) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for m.count(step) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the dial did not reach %s", step)
		}
		time.Sleep(time.Millisecond)
	}
}

// A pair record that is not a property list, or that lacks the host
// certificate or key, is refused with an error that tells the user to pair
// again. Neither the error nor go-ios's log carries record bytes, the step is
// not retried, and every socket is closed (design section 6: the adapter
// reads the record itself so that the private key is never logged).
func TestPairRecordRejected(t *testing.T) {
	for _, mode := range []injectMode{injectBadPairRecord, injectNoHostKey, injectNoHostCertificate} {
		t.Run(mode.String(), func(t *testing.T) {
			var log lockedBuffer
			SetLogger(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.Level(-8)}))) // go-ios trace level
			t.Cleanup(func() { SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil))) })
			m := newFakeUsbmuxd(t, map[muxStep]injectMode{stepReadPairRecord: mode})
			tr := newTestTransport(t, m)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := tr.Pages(ctx, testUDID)
			if err == nil || !strings.Contains(err.Error(), "pair the device again") {
				t.Fatalf("Pages: %v, want an error that says to pair the device again", err)
			}
			for what, text := range map[string]string{"error": err.Error(), "log": log.String()} {
				for _, form := range leakForms(pairSecretMarker) {
					if strings.Contains(text, form) {
						t.Fatalf("the %s carries key material (%q):\n%s", what, form, text)
					}
				}
			}
			if n := m.count(stepReadPairRecord); n != 1 {
				t.Fatalf("pair record read %d times, want 1 (not retried)", n)
			}
			if n := m.count(stepLockdownConnect); n != 0 {
				t.Fatalf("lockdown connects = %d, want 0 without a pair record", n)
			}
			_ = tr.Close()
			m.waitBalanced(t)
		})
	}
}

// leakForms returns the forms in which secret shows when the bytes around it
// are printed: as text, as hex, and base64-encoded once or twice (the pair
// record is a plist <data> field inside usbmuxd's plist reply). For base64,
// each of the three alignments of secret in the encoded stream is covered by
// the complete 3-byte groups of secret[k:].
func leakForms(secret string) []string {
	b64 := func(s string) []string {
		var out []string
		for k := range 3 {
			s := s[k:]
			out = append(out, base64.StdEncoding.EncodeToString([]byte(s[:len(s)/3*3])))
		}
		return out
	}
	forms := []string{secret, hex.EncodeToString([]byte(secret))}
	for _, once := range b64(secret) {
		forms = append(forms, once)
		forms = append(forms, b64(once)...)
	}
	return forms
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
