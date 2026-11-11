package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Refuse only Target.setPauseOnStart, forwarding every other message to
// fakewebkit unchanged. The fake's own outer Target handler always replies
// successfully and does not honor Unsupported/Stall, so use a wire proxy.
func reviewPauseProxy(t *testing.T, upstream string, silent bool) string {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		front, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = front.Close() }()
		back, _, err := websocket.DefaultDialer.Dial(upstream, nil)
		if err != nil {
			t.Errorf("dial fakewebkit: %v", err)
			return
		}
		defer func() { _ = back.Close() }()
		var writes sync.Mutex
		writeFront := func(data []byte) error {
			writes.Lock()
			defer writes.Unlock()
			return front.WriteMessage(websocket.TextMessage, data)
		}
		go func() {
			defer func() { _ = front.Close() }()
			for {
				_, data, err := back.ReadMessage()
				if err != nil {
					return
				}
				if err := writeFront(data); err != nil {
					return
				}
			}
		}()
		for {
			_, data, err := front.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			if m.Method == "Target.setPauseOnStart" {
				if !silent {
					reply, _ := json.Marshal(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "'Target.setPauseOnStart' was not found"}})
					if err := writeFront(reply); err != nil {
						return
					}
				}
				continue
			}
			if err := back.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// Source: W1c-core-review-1.md, Judge especially 1: connect must degrade
// to no pause-on-start with a reason, rather than fail a usable page.
func TestReviewPauseOnStartFailureMustNotBreakDaemonConnect(t *testing.T) {
	for _, silent := range []bool{false, true} {
		name := "protocol error"
		if silent {
			name = "no reply"
		}
		t.Run(name, func(t *testing.T) {
			f := newFake(t, fakewebkit.Options{})
			wsURL := reviewPauseProxy(t, f.URL, silent)
			l := fakewebkit.NewListing(false)
			t.Cleanup(l.Close)
			dev := l.AddDevice("00008020-0000000000000001", "Fixture", "16.7.10")
			dev.AddPage("Fixture", "https://fixture.example/", wsURL)
			o := startInProcess(t, RunOptions{
				Transport: "iwdp", Addr: l.Addr(),
				Manager: core.ManagerOptions{Session: webkit.SessionOptions{CallTimeout: 150 * time.Millisecond}},
			})
			if _, err := (&core.Reservations{Dir: ReservationsDir(o.Dir)}).Reserve(dev.ID, "s1", time.Hour); err != nil {
				t.Fatal(err)
			}
			c := connect(t, o)
			start := time.Now()
			_, err := c.Run(ctxFor(t, 3*time.Second), RunRequest{Argv: []string{"eval", "1"}, PageReq: PageReq{Page: wsURL}, Owner: Owner{Session: "s1"}}, nil)
			if err != nil {
				t.Fatalf("usable page rejected after %s solely because pause-on-start %s: %v", time.Since(start).Round(time.Millisecond), name, err)
			}
		})
	}
}
