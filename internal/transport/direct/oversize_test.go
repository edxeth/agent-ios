package direct

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

const testCap = 1 << 20 // frame cap used by the oversized-frame tests (design 7.4 test 2)

// dropRecorder collects EventMessageDropped events of one session.
type dropRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (r *dropRecorder) handler(_ string, params json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	r.mu.Lock()
	r.events = append(r.events, p)
	r.mu.Unlock()
}

func (r *dropRecorder) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		got := append([]map[string]any(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Acceptance 7.4 test 2: a reply above the frame cap is skipped; the next
// frame, for page B, decodes; every page session of the device gets the
// dropped-message event; the call whose reply was dropped fails at its
// deadline instead of hanging.
func TestOversizedFrameIsDrainedForTheWholeDevice(t *testing.T) {
	d, srvA, _ := twoPageDevice(t)
	big := strings.Repeat("x", testCap+testCap/2)
	srvA.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if p.Expression == "big" {
			return map[string]any{"result": map[string]any{"type": "string", "value": big}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "string", "value": "from A"}}, nil
	})
	tr := newWithDialer(d, Options{MaxIncoming: testCap, DrainTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = tr.Close() })
	pages := listPages(t, tr)
	const callBudget = 700 * time.Millisecond
	sa := openSession(t, tr, findPage(t, pages, "PID:100/1"), webkit.SessionOptions{CallTimeout: callBudget})
	sb := openSession(t, tr, findPage(t, pages, "PID:100/2"), webkit.SessionOptions{CallTimeout: callBudget})
	if got := sa.Limits(); got.MaxIncoming != testCap || got.MaxOutgoing != MaxOutgoing || !got.Routed {
		t.Fatalf("limits = %+v, want the frame cap, 16 MiB outgoing, routed", got)
	}
	var ra, rb dropRecorder
	sa.Subscribe(EventMessageDropped, ra.handler)
	sb.Subscribe(EventMessageDropped, rb.handler)

	start := time.Now()
	_, err := sa.Send(context.Background(), "Runtime.evaluate", map[string]any{"expression": "big"})
	el := time.Since(start)
	if !errors.Is(err, webkit.ErrCallTimeout) {
		t.Fatalf("call with the dropped reply: %v, want ErrCallTimeout", err)
	}
	if el < callBudget || el > callBudget+time.Second {
		t.Fatalf("call with the dropped reply ended after %s, want its budget %s", el, callBudget)
	}
	for name, r := range map[string]*dropRecorder{"A": &ra, "B": &rb} {
		evs := r.wait(t, 1)
		if len(evs) != 1 {
			t.Fatalf("page %s got %d dropped-message events, want 1", name, len(evs))
		}
		n, _ := evs[0]["bytes"].(float64)
		msg, _ := evs[0]["message"].(string)
		if n <= testCap || !strings.Contains(msg, "dropped") || !strings.Contains(msg, "pending reply may be lost") {
			t.Fatalf("page %s event %v: want the dropped size above the cap and the message", name, evs[0])
		}
	}
	// The stream stayed aligned: the next frames decode, for page B and A.
	if got := evalValue(t, sb); got != "from B" {
		t.Fatalf("page B after the drain: %q", got)
	}
	if got := evalValue(t, sa); got != "from A" {
		t.Fatalf("page A after the drain: %q", got)
	}
	if n := d.dials; n != 1 {
		t.Fatalf("device dialed %d times; the drain must keep the connection", n)
	}
}

// Acceptance 7.4 test 2: a drain that misses its deadline closes the device
// connection, and every page connection of the device fails with
// ErrPageClosed.
func TestDrainPastDeadlineClosesTheDevice(t *testing.T) {
	d, _, _ := twoPageDevice(t)
	const drain = 300 * time.Millisecond
	tr := newWithDialer(d, Options{MaxIncoming: testCap, DrainTimeout: drain})
	t.Cleanup(func() { _ = tr.Close() })
	pages := listPages(t, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var conns []transport.MessageConn
	for _, id := range []string{"PID:100/1", "PID:100/2"} {
		c, err := tr.Open(ctx, findPage(t, pages, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(ctx); err != nil { // Target.targetCreated
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	fake := d.fake(0)
	start := time.Now()
	fake.sendHeader(testCap + 1000)       // announces more than the cap
	fake.sendRaw(make([]byte, testCap/2)) // then stalls before the end of the frame
	for i, c := range conns {
		_, err := c.Read(ctx)
		if !errors.Is(err, webkit.ErrPageClosed) || !errors.Is(err, errDrainFailed) {
			t.Fatalf("page %d: Read = %v, want ErrPageClosed caused by the failed drain", i, err)
		}
	}
	if el := time.Since(start); el < drain || el > drain+time.Second {
		t.Fatalf("pages failed after %s, want the drain deadline %s", el, drain)
	}
	select {
	case <-fake.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the device connection was not closed")
	}
	// The next call dials the device again.
	if _, err := tr.Pages(ctx, testUDID); err != nil {
		t.Fatalf("Pages after the closed device: %v", err)
	}
	if d.dials != 2 {
		t.Fatalf("dials = %d, want a new connection after the failed drain", d.dials)
	}
}

// Acceptance 7.4 test 2: a full-page screenshot whose estimate exceeds the
// cap is refused before sending; a small one is sent.
func TestLargeScreenshotRefusedBeforeSending(t *testing.T) {
	d, srvA, _ := twoPageDevice(t)
	srvA.Result("Page.snapshotRect", map[string]any{"dataURL": "data:image/png;base64,AAAA"})
	tr := newWithDialer(d, Options{MaxIncoming: testCap})
	t.Cleanup(func() { _ = tr.Close() })
	s := openSession(t, tr, findPage(t, listPages(t, tr), "PID:100/1"), webkit.SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 375 x 1000 CSS px at scale 3, RGBA, base64: about 18 MB > 1 MiB.
	_, err := s.Send(ctx, "Page.snapshotRect", map[string]any{"x": 0, "y": 0, "width": 375, "height": 1000, "coordinateSystem": "Page"})
	var tooLarge *ResultTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != testCap || tooLarge.Estimate <= testCap {
		t.Fatalf("large snapshot: %v, want ResultTooLargeError above the cap", err)
	}
	if !strings.Contains(err.Error(), "--transport iwdp") {
		t.Fatalf("error %q does not tell the user what to do", err)
	}
	// 100 x 100 CSS px: about 480 KB, below the cap.
	if _, err := s.Send(ctx, "Page.snapshotRect", map[string]any{"x": 0, "y": 0, "width": 100, "height": 100, "coordinateSystem": "Viewport"}); err != nil {
		t.Fatalf("small snapshot: %v", err)
	}
	got := srvA.ReceivedMethod("Page.snapshotRect")
	if len(got) != 1 {
		t.Fatalf("the page received %d Page.snapshotRect calls, want only the small one", len(got))
	}
}
