package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
	"github.com/gorilla/websocket"
)

// core-events r6: lateDisable keeps a written Network.disable only while
// its reply is outstanding after its caller stopped waiting. The first test
// is the reviewer's reproduction
// (/home/devkit/.pi/artifacts/reviewer/core-events-r5-repro/late_retention_test.go).

func lateDisableCount(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lateDisable)
}

func TestReviewAnsweredDisableErrorsAreNotRetainedAsLate(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Network domain is not enabled"}
	})
	s := newFakeSession(t, f, SessionOptions{NetworkDisabled: func(*Session, string) {}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 16; i++ {
		if _, err := s.Send(ctx, "Network.disable", nil); err == nil {
			t.Fatal("fixture: expected completed error reply")
		}
	}
	if n := lateDisableCount(s); n != 0 {
		t.Fatalf("retained %d answered protocol errors as late replies, want none: their only replies already arrived", n)
	}
}

// A late error reply removes its record like a late success, and the
// records of an outstanding reply go with the connection.
func TestLateDisableRecordsEndWithTheirReplyOrTheConnection(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Network domain is not enabled"}
	})
	const replyDelay = 300 * time.Millisecond
	f.DelayReplies("Network.disable", replyDelay)
	s := newFakeSession(t, f, SessionOptions{NetworkDisabled: func(*Session, string) {}})

	stopWaiting := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), replyDelay/6)
		defer cancel()
		if _, err := s.Send(ctx, "Network.disable", nil); err == nil {
			t.Fatal("fixture: the caller got the delayed reply before its deadline")
		}
	}
	stopWaiting()
	if n := lateDisableCount(s); n != 1 {
		t.Fatalf("records after a caller stopped waiting for its written disable = %d, want 1", n)
	}
	waitFor(t, func() bool { return lateDisableCount(s) == 0 })
	if n := correlationCount(s); n != 0 {
		t.Fatalf("outer-to-inner correlations after the late reply = %d, want none", n)
	}

	stopWaiting()
	if n := lateDisableCount(s); n != 1 {
		t.Fatalf("fixture: records before the close = %d, want 1", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := lateDisableCount(s); n != 0 {
		t.Fatalf("records after the connection closed = %d, want none: no reply comes on a closed connection", n)
	}
	if n := correlationCount(s); n != 0 {
		t.Fatalf("outer-to-inner correlations after the connection closed = %d, want none", n)
	}
}

// A written Network.disable whose outer message the endpoint refuses (an
// error ack of Target.sendMessageToTarget) never reaches the page, so no
// late reply can come and nothing is recorded.
func TestRefusedOuterMessageIsNotRetainedAsLate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_ = ws.WriteJSON(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{"targetId": "page-1", "type": "page"}}})
		for {
			var m struct {
				ID int64 `json:"id"`
			}
			if err := ws.ReadJSON(&m); err != nil {
				return
			}
			_ = ws.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "Missing target for given targetId"}})
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWebSocket(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Limits{Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ctx, conn, SessionOptions{NetworkDisabled: func(*Session, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Send(ctx, "Network.disable", nil); err == nil {
		t.Fatal("fixture: the refused outer message did not fail the call")
	}
	if n := lateDisableCount(s); n != 0 {
		t.Fatalf("retained %d refused outer messages as late replies, want none: the page never got them", n)
	}
}

// core-events r7: the reviewer's reproduction
// (/home/devkit/.pi/artifacts/reviewer/core-events-r6-repro/late_outer_reply_test.go).
// Each written disable's only reply is an outer refusal that arrives after
// its caller gave up. The refusal is terminal, so it removes the record and
// the outer-to-inner correlation that the record keeps. No selective reply
// reordering is involved.
func TestReviewLateOuterRefusalsDoNotRetainDisableRecords(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_ = ws.WriteJSON(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{"targetId": "page-1", "type": "page"}}})
		for {
			var m struct {
				ID int64 `json:"id"`
			}
			if err := ws.ReadJSON(&m); err != nil {
				return
			}
			received <- struct{}{}
			<-release
			if err := ws.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "Missing target for given targetId"}}); err != nil {
				return
			}
			// A subsequent ordered event proves the read loop consumed the refusal.
			if err := ws.WriteJSON(map[string]any{"method": "Test.barrier", "params": map[string]any{}}); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWebSocket(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Limits{Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ctx, conn, SessionOptions{NetworkDisabled: func(*Session, string) { t.Error("refusal must not report a reset") }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	barrier := make(chan struct{}, 1)
	defer s.SubscribeSync("Test.barrier", func(string, json.RawMessage) { barrier <- struct{}{} })()
	for i := 0; i < 16; i++ {
		callCtx, stop := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { _, err := s.Send(callCtx, "Network.disable", nil); result <- err }()
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatal("server did not receive disable")
		}
		stop()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want cancellation after write: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("caller did not stop")
		}
		select {
		case release <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("server did not release reply")
		}
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatal("read loop did not consume outer reply")
		}
	}
	if n := lateDisableCount(s); n != 0 {
		t.Fatalf("retained %d answered late outer refusals as outstanding disables, want none", n)
	}
	if n := correlationCount(s); n != 0 {
		t.Fatalf("kept %d outer-to-inner correlations after every call ended, want none", n)
	}
}

// correlationCount is the number of outer Target.sendMessageToTarget ids
// that the session still maps to an inner call.
func correlationCount(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.wrapOf)
}
