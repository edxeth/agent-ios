package webkit

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// closeOnce connects a session to f and closes it, so that f's reconnect
// gap starts.
func closeOnce(t *testing.T, f *fakewebkit.Server) {
	t.Helper()
	s := newFakeSession(t, f, SessionOptions{})
	_ = s.Close()
	for end := time.Now().Add(5 * time.Second); f.OpenConnections() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the first connection did not close within 5 s")
		}
	}
}

// C08 (W1c-3; docs/notes/w3b.md "A reconnect right after a close"): a
// connection opened right after a close gets no Target.targetCreated, as
// on WebKitGTK, and so does one opened right after that silent attempt was
// closed. Dial closes the silent attempt and waits dialRetryGap before the
// next one, which gets its target.
func TestDialRetriesAfterAGapWhenAReconnectStaysSilent(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{ReconnectGap: 300 * time.Millisecond})
	defer f.Close()
	closeOnce(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Dial(ctx, dialFake(t, f), SessionOptions{TargetWait: time.Second})
	if err != nil {
		t.Fatalf("Dial right after a close: %v", err)
	}
	defer func() { _ = s.Close() }()
	if n := f.ConnectionCount(); n != 3 {
		t.Fatalf("connections = %d, want 3 (the closed one, a silent attempt, a successful one)", n)
	}
	if _, err := s.Send(ctx, "Runtime.evaluate", nil); err != nil {
		t.Fatalf("Send after the retry: %v", err)
	}
}

// C08: a long TargetWait (12 s right after a close, design 2.6) does not
// hold one silent connection for 12 s; Dial gives up on it after
// DefaultTargetWait and connects on the next attempt.
func TestDialDoesNotHoldASilentAttemptForTheWholeTargetWait(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{ReconnectGap: 300 * time.Millisecond})
	defer f.Close()
	closeOnce(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	s, err := Dial(ctx, dialFake(t, f), SessionOptions{TargetWait: 12 * time.Second})
	el := time.Since(start)
	if err != nil {
		t.Fatalf("Dial with a 12 s target wait right after a close: %v", err)
	}
	defer func() { _ = s.Close() }()
	if lo, hi := DefaultTargetWait+dialRetryGap, DefaultTargetWait+dialRetryGap+2*time.Second; el < lo || el > hi {
		t.Fatalf("connected after %s, want one silent attempt of %s, the %s gap and a quick second attempt (%s to %s)", el, DefaultTargetWait, dialRetryGap, lo, hi)
	}
}

// closingOpen dials f and closes the connection at once, so every attempt
// ends before a target is announced; it counts the attempts.
func closingOpen(t *testing.T, f *fakewebkit.Server, opens *atomic.Int32) func(context.Context) (transport.MessageConn, error) {
	t.Helper()
	return func(ctx context.Context) (transport.MessageConn, error) {
		opens.Add(1)
		c, err := DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		if err != nil {
			return nil, err
		}
		_ = c.Close()
		return c, nil
	}
}

// C08: the number of attempts is bounded by the target wait, at
// max(2, ceil(TargetWait / DefaultTargetWait)), with the gap between two
// attempts, so an endpoint that closes every connection is not dialed in a
// tight loop.
func TestDialBoundsItsAttemptsByTheTargetWait(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{NoTarget: true})
	defer f.Close()
	for _, tc := range []struct {
		wait     time.Duration
		attempts int32
	}{
		{200 * time.Millisecond, 2},
		{DefaultTargetWait, 2},
		{DefaultTargetWait + time.Millisecond, 2},
		{12 * time.Second, 4},
	} {
		var opens atomic.Int32
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := time.Now()
		_, err := Dial(ctx, closingOpen(t, f, &opens), SessionOptions{TargetWait: tc.wait})
		el := time.Since(start)
		cancel()
		if !errors.Is(err, ErrNoTarget) {
			t.Fatalf("TargetWait %s: err = %v, want ErrNoTarget", tc.wait, err)
		}
		if n := opens.Load(); n != tc.attempts {
			t.Fatalf("TargetWait %s: %d attempts, want %d", tc.wait, n, tc.attempts)
		}
		if want := "connection attempts"; !strings.Contains(err.Error(), want) {
			t.Fatalf("TargetWait %s: err = %q, want it to name the %s", tc.wait, err, want)
		}
		if least := time.Duration(tc.attempts-1) * dialRetryGap; el < least {
			t.Fatalf("TargetWait %s: %d attempts within %s, want a %s gap between two attempts", tc.wait, tc.attempts, el, dialRetryGap)
		}
	}
}

// C08: the gap never runs past ctx. A deadline during the gap ends Dial at
// once with the deadline in the error chain (exit 124 in the daemon), not
// with ErrNoTarget.
func TestDialGapEndsWithTheContext(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{NoTarget: true})
	defer f.Close()
	var opens atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, closingOpen(t, f, &opens), SessionOptions{TargetWait: 12 * time.Second})
	el := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want the deadline and not ErrNoTarget", err)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("%d attempts, want 1 (the deadline came during the gap)", n)
	}
	if el > dialRetryGap {
		t.Fatalf("Dial returned after %s, want at the 200 ms deadline, before the gap ended", el)
	}
}
