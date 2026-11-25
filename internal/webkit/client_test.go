package webkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil"
	"github.com/gorilla/websocket"
)

func TestClient_Send(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	mock.HandleFunc("Test.method", map[string]string{"key": "value"})

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	result, err := client.Send(ctx, "Test.method", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("got %v, want key=value", got)
	}
}

func TestClient_SendWithParams(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	mock.Handle("Echo.params", func(_ string, params json.RawMessage) (interface{}, error) {
		return json.RawMessage(params), nil
	})

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	input := map[string]string{"hello": "world"}
	result, err := client.Send(ctx, "Echo.params", input)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("got %v, want hello=world", got)
	}
}

func TestClient_SendError(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	mock.Handle("Fail.method", func(_ string, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("something went wrong")
	})

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Send(ctx, "Fail.method", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "something went wrong" {
		t.Errorf("got error %q, want %q", err.Error(), "something went wrong")
	}
}

func TestClient_SendTimeout(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	// Register a handler that never responds — the mock server won't have this
	// method, so it returns empty result immediately. Instead we test context cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Register a handler that blocks
	mock.Handle("Slow.method", func(_ string, _ json.RawMessage) (interface{}, error) {
		time.Sleep(5 * time.Second)
		return nil, nil
	})

	_, err = client.Send(ctx, "Slow.method", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestClient_Events(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	var mu sync.Mutex
	var received []string

	client.Subscribe("Console.messageAdded", func(method string, params json.RawMessage) {
		mu.Lock()
		received = append(received, method)
		mu.Unlock()
	})

	// Send an event from the mock server
	if err := mock.SendEvent("Console.messageAdded", map[string]string{"text": "hello"}); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	// Wait for event to be received
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if received[0] != "Console.messageAdded" {
		t.Errorf("got event %q, want Console.messageAdded", received[0])
	}
}

func TestClient_GlobalEventHandler(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	var mu sync.Mutex
	var received []string

	client.OnAnyEvent(func(method string, params json.RawMessage) {
		mu.Lock()
		received = append(received, method)
		mu.Unlock()
	})

	if err := mock.SendEvent("Page.loadEventFired", nil); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if err := mock.SendEvent("DOM.documentUpdated", nil); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}
}

func TestClient_ConcurrentSends(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	mock.Handle("Test.echo", func(_ string, params json.RawMessage) (interface{}, error) {
		return json.RawMessage(params), nil
	})

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			input := map[string]int{"n": n}
			result, err := client.Send(ctx, "Test.echo", input)
			if err != nil {
				errs <- fmt.Errorf("send %d: %w", n, err)
				return
			}
			var got map[string]int
			if err := json.Unmarshal(result, &got); err != nil {
				errs <- fmt.Errorf("unmarshal %d: %w", n, err)
				return
			}
			if got["n"] != n {
				errs <- fmt.Errorf("got n=%d, want %d", got["n"], n)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClient_Close(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Double close should be safe
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Send after close should fail
	_, err = client.Send(ctx, "Test.method", nil)
	if err == nil {
		t.Fatal("expected error after close")
	}
}

func TestClient_EnableDisableDomain(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()

	var enabled []string
	var mu sync.Mutex

	mock.Handle("Page.enable", func(method string, _ json.RawMessage) (interface{}, error) {
		mu.Lock()
		enabled = append(enabled, method)
		mu.Unlock()
		return nil, nil
	})
	mock.Handle("Page.disable", func(method string, _ json.RawMessage) (interface{}, error) {
		mu.Lock()
		enabled = append(enabled, method)
		mu.Unlock()
		return nil, nil
	})

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Enable(ctx, "Page"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := client.Disable(ctx, "Page"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(enabled))
	}
}

// TestClient_SendDefaultTimeout pins the per-call deadline: a context without
// a deadline must not let Send wait forever for a page that never answers.
func TestClient_SendDefaultTimeout(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.NeverRespond("Runtime.evaluate")

	old := webkit.DefaultSendTimeout
	webkit.DefaultSendTimeout = 200 * time.Millisecond
	defer func() { webkit.DefaultSendTimeout = old }()

	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	start := time.Now()
	_, err = client.Send(context.Background(), "Runtime.evaluate", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a deadline error from a method that never answers")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "Runtime.evaluate") {
		t.Errorf("error %q does not name the method", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Send returned after %v, want about 200ms", elapsed)
	}
}

// TestClient_SendKeepsCallerDeadline checks that the default per-call timeout
// does not shorten a deadline the caller set on purpose.
func TestClient_SendKeepsCallerDeadline(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.Handle("Slow.method", func(_ string, _ json.RawMessage) (interface{}, error) {
		time.Sleep(300 * time.Millisecond)
		return map[string]bool{"done": true}, nil
	})

	old := webkit.DefaultSendTimeout
	webkit.DefaultSendTimeout = 50 * time.Millisecond
	defer func() { webkit.DefaultSendTimeout = old }()

	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Send(ctx, "Slow.method", nil); err != nil {
		t.Fatalf("Send with a caller deadline of 5s failed: %v", err)
	}
}

// TestNewClient_HandshakeTimeout pins the short WebSocket handshake timeout: a
// listener that accepts TCP and never answers the upgrade must fail fast
// instead of hanging for gorilla's 45 s default.
func TestNewClient_HandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, conn) // never answer the HTTP upgrade
			heldMu.Unlock()
		}
	}()
	defer func() {
		heldMu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		heldMu.Unlock()
	}()

	old := webkit.DialTimeout
	webkit.DialTimeout = 200 * time.Millisecond
	defer func() { webkit.DialTimeout = old }()

	start := time.Now()
	_, err = webkit.NewClient(context.Background(), "ws://"+ln.Addr().String()+"/devtools/page/1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a handshake timeout error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("NewClient returned after %v, want about 200ms", elapsed)
	}
}

// TestDialTimeoutDefault pins the documented default handshake timeout.
func TestDialTimeoutDefault(t *testing.T) {
	if webkit.DialTimeout != 5*time.Second {
		t.Errorf("DialTimeout = %v, want 5s", webkit.DialTimeout)
	}
	if webkit.DefaultSendTimeout != 30*time.Second {
		t.Errorf("DefaultSendTimeout = %v, want 30s", webkit.DefaultSendTimeout)
	}
}

// TestClient_TargetRoutedCSSGuard pins the rule from CLAUDE.md: CSS.enable,
// CSS.getAllStyleSheets and CSS.getStyleSheetText hang through iwdp Target
// routing and break the connection, so the client never sends them there.
func TestClient_TargetRoutedCSSGuard(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.EnableTargetRouting("page-1")
	mock.HandleFunc("DOM.getDocument", map[string]interface{}{"root": map[string]int{"nodeId": 1}})

	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !client.IsTargetRouted() {
		t.Fatal("client did not detect Target routing from Target.targetCreated")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Control: ordinary commands still travel through Target routing.
	if _, err := client.Send(ctx, "DOM.getDocument", nil); err != nil {
		t.Fatalf("DOM.getDocument through Target routing: %v", err)
	}

	for _, method := range []string{"CSS.enable", "CSS.getAllStyleSheets", "CSS.getStyleSheetText"} {
		_, err := client.Send(ctx, method, nil)
		if err == nil {
			t.Errorf("%s: expected an error through Target routing", method)
			continue
		}
		if !strings.Contains(err.Error(), method) {
			t.Errorf("%s: error %q does not name the method", method, err)
		}
	}
	for _, m := range mock.ReceivedMethods() {
		if strings.HasPrefix(m, "CSS.") {
			t.Errorf("mock received %s through Target routing; the guard must not send it", m)
		}
	}
}

// TestClient_SendDeadlineWhileAnotherWriteIsBlocked pins that a Send waiting
// for the socket gives up at its own deadline and never transmits its command
// late. gorilla/websocket allows one writer at a time, so a concurrent Send
// waits while another command's write is stalled; that wait must honor ctx.
func TestClient_SendDeadlineWhileAnotherWriteIsBlocked(t *testing.T) {
	started := make(chan struct{}) // the first frame of the large command arrived
	release := make(chan struct{}) // the server may read again
	var mu sync.Mutex
	var received []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// A small receive buffer keeps the blocked write independent of
		// the host's TCP autotuning limits; a large one after release lets
		// the rest of the command arrive quickly.
		tcp, _ := conn.NetConn().(*net.TCPConn)
		setReadBuffer := func(n int) {
			if tcp != nil {
				_ = tcp.SetReadBuffer(n)
			}
		}
		setReadBuffer(4096)
		for first := true; ; first = false {
			_, rd, err := conn.NextReader()
			if err != nil {
				return
			}
			if first {
				// Stop reading inside the first message: the client's write
				// of the rest stays blocked until release.
				close(started)
				<-release
				setReadBuffer(4 << 20)
			}
			data, err := io.ReadAll(rd)
			if err != nil {
				return
			}
			var msg webkit.Message
			if err := json.Unmarshal(data, &msg); err != nil {
				return
			}
			mu.Lock()
			received = append(received, msg.Method)
			mu.Unlock()
			resp, _ := json.Marshal(map[string]interface{}{"id": msg.ID, "result": map[string]interface{}{}})
			if err := conn.WriteMessage(websocket.TextMessage, resp); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	client, err := webkit.NewClient(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	// A command far larger than the socket buffers (send buffer at most 4 MB
	// on common Linux defaults), so its write blocks while the server does not
	// read. Its deadline is long: building and masking 16 MB is slow under
	// the race detector.
	largeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := client.Send(ctx, "Large.method", map[string]string{"blob": strings.Repeat("x", 16<<20)})
		largeDone <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the large command never reached the server")
	}

	const shortDeadline = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), shortDeadline)
	defer cancel()
	start := time.Now()
	_, err = client.Send(ctx, "Short.method", nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(fmt.Sprint(err), "Short.method") {
		t.Errorf("err = %v, want a deadline error naming Short.method", err)
	}
	if elapsed > shortDeadline+time.Second {
		t.Errorf("Send returned after %v with a %v deadline: it waited for the blocked write", elapsed, shortDeadline)
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-largeDone; err != nil {
		t.Fatalf("large command after release: %v", err)
	}
	// A later command still works, and the expired one was never sent.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := client.Send(ctx2, "Later.method", nil); err != nil {
		t.Fatalf("Later.method: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Large.method", "Later.method"}; fmt.Sprint(received) != fmt.Sprint(want) {
		t.Errorf("server received %v, want %v", received, want)
	}
}

// TestClient_SendWithExpiredContextSendsNothing pins that Send never
// transmits a command whose context has already ended, and reports the
// context error. Send waits for the socket in a select whose choice among
// ready cases is random, so the call is repeated to reach both paths.
func TestClient_SendWithExpiredContextSendsNothing(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	for i := 0; i < 64; i++ {
		if _, err := client.Send(ctx, "Expired.method", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: err = %v, want context.DeadlineExceeded", i, err)
		}
	}
	// A live command after them proves the server read everything sent before.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := client.Send(ctx2, "Live.method", nil); err != nil {
		t.Fatalf("Live.method: %v", err)
	}
	if got := mock.ReceivedMethods(); fmt.Sprint(got) != "[Live.method]" {
		t.Errorf("server received %v, want only Live.method", got)
	}
}
