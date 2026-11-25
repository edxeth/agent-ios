package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// core-events r6: a declared stall interrupts a socket write in progress,
// and a call queued behind it keeps ErrNotSent. The reviewer's reproduction
// (/home/devkit/.pi/artifacts/reviewer/core-events-r5-repro/blocked_write_test.go),
// kept with its two assertions and extended with the classification of
// both calls. It uses real TCP backpressure: small socket buffers and a
// peer that stops reading, not a fake Write that ignores its context.

type reviewWriteConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
	armed   *atomic.Bool
}

func (c *reviewWriteConn) Write(b []byte) (int, error) {
	if c.armed.Load() {
		c.once.Do(func() { close(c.entered) })
	}
	return c.Conn.Write(b)
}

func TestReviewStallCutsAnAlreadyBlockedSocketWrite(t *testing.T) {
	peer := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if c, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
			_ = c.SetReadBuffer(1024)
		}
		_ = ws.WriteJSON(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{"targetId": "page-1", "type": "page"}}})
		peer <- ws
		<-release // deliberately do not read client data
	}))
	defer srv.Close()
	defer close(release)
	entered := make(chan struct{})
	var armed atomic.Bool
	dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		return &reviewWriteConn{Conn: c, entered: entered, armed: &armed}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := webkit.NewClientWithDialer(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	armed.Store(true)
	remote := <-peer
	defer func() { _ = remote.Close() }()
	p := newPageConn(c, nil, func(func(context.Context)) bool { return false }, func(string, ...any) {})
	sent := make(chan error, 1)
	writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	go func() {
		_, err := p.Send(writeCtx, "Test.large", map[string]string{"payload": strings.Repeat("x", 4<<20)})
		sent <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("large socket write did not begin")
	}
	queued := make(chan error, 1)
	queuedStarted := make(chan struct{})
	go func() {
		_, err := p.Send(writeCtx, "Test.queued", marshalHook{fn: func() { close(queuedStarted) }})
		queued <- err
	}()
	<-queuedStarted
	probeCtx, stopProbe := context.WithTimeout(ctx, 200*time.Millisecond)
	if err := p.probeNow(probeCtx); err == nil {
		t.Error("fixture: probe should fail while the writer owns the socket")
	}
	stopProbe()
	if p.State() != ConnStalled {
		t.Fatal("fixture: probe did not declare stalled")
	}
	select {
	case err := <-queued:
		if !errors.Is(err, webkit.ErrNotSent) {
			t.Errorf("queued call never acquired the socket, but lost ErrNotSent: %v", err)
		}
		if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy {
			t.Errorf("queued call = %v (exit %d), want the stall classification (exit %d)", err, exitCode(err), CodeBusy)
		}
	case <-time.After(time.Second):
		t.Error("queued writer did not end at stall")
	}
	select {
	case err := <-sent:
		t.Logf("write cut: %v", err)
		if errors.Is(err, webkit.ErrNotSent) {
			t.Errorf("the interrupted write had written part of its message, but its error wraps ErrNotSent: %v", err)
		}
		if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy {
			t.Errorf("interrupted write = %v (exit %d), want the stall classification (exit %d)", err, exitCode(err), CodeBusy)
		}
	case <-time.After(750 * time.Millisecond):
		t.Error("probe declared stalled, but the existing socket write still waits for its original 5s deadline")
	}
	_ = c.Close()
}

// stallCut classifies a call that the stall ended before bound's cancel
// reached the call's context: probe interrupts a write in progress right
// after it declares the stall, and the write can return first.
func TestStallCutBeforeTheCancelReachesTheCall(t *testing.T) {
	stall, fire := context.WithCancelCause(context.Background())
	live := context.Background() // the call's context, not ended yet
	written := errors.New("Test.large: writing message: i/o timeout")
	if err := stallCut(live, stall, "Test.large", written); err != written {
		t.Errorf("before the stall: %v, want the call's own error", err)
	}
	fire(fmt.Errorf("%w (liveness probe: no reply)", webkit.ErrSessionStalled))
	err := stallCut(live, stall, "Test.large", written)
	if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy || errors.Is(err, webkit.ErrNotSent) {
		t.Errorf("written call = %v (exit %d), want the stall classification (exit %d) without ErrNotSent", err, exitCode(err), CodeBusy)
	}
	err = stallCut(live, stall, "Test.queued", fmt.Errorf("Test.queued: not sent: %w", webkit.ErrNotSent))
	if !errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy || !errors.Is(err, webkit.ErrNotSent) {
		t.Errorf("unwritten call = %v (exit %d), want the stall classification (exit %d) with ErrNotSent", err, exitCode(err), CodeBusy)
	}
	ended, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := stallCut(ended, stall, "Test.large", written); err != written {
		t.Errorf("call that its deadline ended first: %v, want its own error", err)
	}
}
