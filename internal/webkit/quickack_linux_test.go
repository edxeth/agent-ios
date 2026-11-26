//go:build linux

package webkit

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// FIX-R (iPhone X profile, docs/cdp.md "Latency"): through
// ios_webkit_debug_proxy, each Target.sendMessageToTarget gets two
// frames, the empty result {"result":{},"id":N} at once and the page's
// reply (Target.dispatchMessageFromTarget) a few milliseconds later, on a
// socket with Nagle's algorithm on (iwdp sets no TCP_NODELAY). The reply
// waited until the daemon acknowledged the first frame, which Linux
// delays by 40 ms: every WIP round trip through iwdp took 44 ms. The
// daemon's WebSocket connection acknowledges at once, so the reply
// arrives in about the time the page took.
func TestSecondReplyFrameIsNotDelayed(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		raw := c.UnderlyingConn()
		if tc, ok := raw.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(false) // iwdp's socket: Nagle on
		}
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			// Two unmasked text frames (payloads below 126 bytes), each
			// in one write, the second after the page's time, as iwdp
			// does.
			ack := []byte(`{"result":{},"id":1}`)
			if _, err := raw.Write(append([]byte{0x81, byte(len(ack))}, ack...)); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
			if _, err := raw.Write(append([]byte{0x81, byte(len(msg))}, msg...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := DialWebSocket(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	const warmup, rounds = 10, 30
	var took []time.Duration
	for i := range warmup + rounds {
		start := time.Now()
		if err := conn.Write(ctx, []byte(`{"id":1,"method":"Runtime.evaluate"}`)); err != nil {
			t.Fatal(err)
		}
		for range 2 { // the empty result, then the reply
			if _, err := conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if i >= warmup {
			took = append(took, time.Since(start))
		}
	}
	slices.Sort(took)
	// A delayed acknowledgement costs 40 ms on Linux; the round trip takes
	// the fake page's 2 ms and the loopback's well under 1 ms.
	if median := took[rounds/2]; median >= 20*time.Millisecond {
		t.Fatalf("median round trip %v over %d rounds (sorted %v); want under 20ms: the reply waited for a delayed acknowledgement of the first frame", median, rounds, took)
	}
}
