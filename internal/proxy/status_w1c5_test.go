package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// L06: iwdp_status (Status) decides from one CheckListing result: a stuck
// iwdp costs one ProbeBudget, not a second one in Start.
func TestStatusSpendsOneProbeBudget(t *testing.T) {
	addr := silentListing(t)
	f := installFakeIWDP(t, fakeFail, addr)
	useStd(t, testStarter(addr, 2*time.Second))
	begin := time.Now()
	started, err := Status(context.Background(), true)
	d := time.Since(begin)
	if started || err == nil || !strings.Contains(err.Error(), "busy or stuck") {
		t.Fatalf("Status against a stuck iwdp = %v, %v; want the busy-or-stuck error", started, err)
	}
	if limit := ProbeBudget + 2*time.Second; d > limit {
		t.Errorf("Status took %s against a stuck iwdp, want one probe budget (%s, limit %s)", d, ProbeBudget, limit)
	}
	if n := len(f.starts()); n != 0 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 0", n)
	}
}

// L06: the other Status outcomes: a running iwdp, a free port without and
// with auto-start.
func TestStatusReportsRunningStoppedAndStarted(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	useStd(t, testStarter(addr, 10*time.Second))
	if started, err := Status(context.Background(), false); started || !errors.Is(err, ErrNotListening) {
		t.Fatalf("Status(no auto-start) on a free port = %v, %v; want ErrNotListening", started, err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("Status without auto-start ran ios_webkit_debug_proxy %d times", n)
	}
	if started, err := Status(context.Background(), true); !started || err != nil {
		t.Fatalf("Status(auto-start) on a free port = %v, %v; want started", started, err)
	}
	if started, err := Status(context.Background(), true); started || err != nil {
		t.Fatalf("Status with iwdp running = %v, %v; want running, not started", started, err)
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}

// L06: a failed auto-start is Status's error.
func TestStatusReportsAFailedStart(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	useStd(t, testStarter(addr, 2*time.Second))
	started, err := Status(context.Background(), true)
	if started || err == nil || !strings.Contains(err.Error(), "exited during its start") {
		t.Fatalf("Status with an iwdp that exits = %v, %v; want the start error", started, err)
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}
