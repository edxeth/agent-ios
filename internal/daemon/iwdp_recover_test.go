package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/proxy"
)

// The daemon runs RunOptions.RecoverIWDP (proxy.Recover in the binaries)
// on every check of an answering iwdp on the default address, so that an
// iwdp that lost the USB devices when usbmuxd restarted is replaced
// without a person (device check of 2026-11-11), and reports its error.

// countingRecovers is a RunOptions.RecoverIWDP that counts its calls and
// returns restarted and err.
func countingRecovers(n *atomic.Int32, restarted bool, err error) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) {
		n.Add(1)
		return restarted, err
	}
}

func TestEnsureIWDPRecoversAnAnsweringIWDPOnTheDefaultAddress(t *testing.T) {
	addr := slowIWDPListing(t, 0)
	t.Run("restarted", func(t *testing.T) {
		var calls atomic.Int32
		ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, true, nil)}, logf: t.Logf}
		if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); err != nil {
			t.Fatalf("ensureIWDP after a recovery restart: %v", err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("RecoverIWDP ran %d times, want 1", n)
		}
	})
	t.Run("error", func(t *testing.T) {
		var calls atomic.Int32
		want := errors.New("ios_webkit_debug_proxy lists no device although usbmuxd reports an attached USB device")
		ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, false, want)}, logf: t.Logf}
		if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); !errors.Is(err, want) {
			t.Fatalf("ensureIWDP with a failed recovery = %v, want %v", err, want)
		}
	})
	// A restart whose new iwdp still lists no device reports the device
	// advice on that same call, not an empty listing.
	t.Run("restarted, still unlisted", func(t *testing.T) {
		var calls atomic.Int32
		want := fmt.Errorf("%w (1 attached): the new one lists no device after 3s", proxy.ErrDeviceNotListed)
		ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, true, want)}, logf: t.Logf}
		if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true); !errors.Is(err, proxy.ErrDeviceNotListed) {
			t.Fatalf("ensureIWDP after a restart that left the device unlisted = %v, want ErrDeviceNotListed", err)
		}
	})
	t.Run("custom address", func(t *testing.T) {
		var calls atomic.Int32
		ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, true, nil)}, logf: t.Logf}
		if err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, false); err != nil {
			t.Fatalf("ensureIWDP on a custom address: %v", err)
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("RecoverIWDP ran %d times for a custom address, want 0: only the iwdp that agent-ios starts is recovered", n)
		}
	})
}

// The wait for a running start ends with the command's deadline.
func TestEnsureIWDPRecoveryWaitsWithinTheDeadline(t *testing.T) {
	addr := slowIWDPListing(t, 0)
	var calls atomic.Int32
	ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, false, nil)}, logf: t.Logf}
	ts.starts.Lock() // a start runs
	defer ts.starts.Unlock()
	ctx, cancel := context.WithTimeout(ctxFor(t, 10*time.Second), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := ts.ensureIWDP(ctx, addr, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensureIWDP while a start holds the lock = %v, want the deadline", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("ensureIWDP returned after %s with a 300ms deadline", d)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("RecoverIWDP ran %d times, want 0", n)
	}
}

// The start branch waits for a running start or recovery within the
// command's deadline too.
func TestEnsureIWDPStartWaitsWithinTheDeadline(t *testing.T) {
	addr := deadListing(t)
	var starts atomic.Int32
	ts := &transports{o: RunOptions{Transport: "iwdp", StartIWDP: countingStarts(&starts)}, logf: t.Logf}
	ts.starts.Lock() // a recovery runs
	defer ts.starts.Unlock()
	ctx, cancel := context.WithTimeout(ctxFor(t, 10*time.Second), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := ts.ensureIWDP(ctx, addr, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensureIWDP while a recovery holds the lock = %v, want the deadline", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("ensureIWDP returned after %s with a 300ms deadline", d)
	}
	if n := starts.Load(); n != 0 {
		t.Fatalf("iwdp started %d times, want 0", n)
	}
}

// An iwdp that does not answer is not recovered: it is started (nothing
// listens) or reported (a process owns the port), as before.
func TestEnsureIWDPRecoversNoSilentIWDP(t *testing.T) {
	addr := silentIWDPListing(t)
	var calls atomic.Int32
	ts := &transports{o: RunOptions{Transport: "iwdp", RecoverIWDP: countingRecovers(&calls, true, nil)}, logf: t.Logf}
	err := ts.ensureIWDP(ctxFor(t, 30*time.Second), addr, true)
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("ensureIWDP on a silent listing = %v, want the busy-or-stuck error", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("RecoverIWDP ran %d times on a silent listing, want 0", n)
	}
}
