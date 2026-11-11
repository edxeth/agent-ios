package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// core-events r6: InterruptWrites ends a socket write that real TCP
// backpressure blocks (small socket buffers, a peer that stops reading),
// and the connection is not written again. A write that it interrupts
// may have written part of its message, so its error does not wrap
// ErrNotSent; every write refused after it does.

// interruptPromptly is how soon an interrupted write must return; the
// write's own deadline is 5 s.
const interruptPromptly = 750 * time.Millisecond

// blockedPeer is a WebSocket server that announces page-1, then reads no
// more client data, and a dialer whose socket reports the first write
// after arm and counts the bytes written.
type blockedPeer struct {
	url     string
	dialer  *websocket.Dialer
	entered chan struct{}
	armed   atomic.Bool
	written atomic.Int64
	remote  chan *websocket.Conn
}

type enteredConn struct {
	net.Conn
	p    *blockedPeer
	once sync.Once
}

func (c *enteredConn) Write(b []byte) (int, error) {
	if c.p.armed.Load() {
		c.once.Do(func() { close(c.p.entered) })
	}
	n, err := c.Conn.Write(b)
	c.p.written.Add(int64(n))
	return n, err
}

func newBlockedPeer(t *testing.T) *blockedPeer {
	t.Helper()
	p := &blockedPeer{entered: make(chan struct{}), remote: make(chan *websocket.Conn, 1)}
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
		p.remote <- ws
		<-release // reads no client data
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	p.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	p.dialer = &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		return &enteredConn{Conn: c, p: p}, nil
	}}
	return p
}

// startLargeWrite arms the peer, starts send with a 4 MiB parameter and
// the test's own deadline, and waits until its socket write blocks: it
// began, and no byte was written for blockedFor. Only the test's deadline
// bounds the send and the wait for its write: building the message took
// more than 5 s under -race and a host load of 70 (devfix R3). Closing the
// connection at the end of the test ends a write that still blocks.
func (p *blockedPeer) startLargeWrite(t *testing.T, send func(context.Context, string, any) (json.RawMessage, error)) <-chan error {
	t.Helper()
	p.armed.Store(true)
	done := make(chan error, 1)
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if d, ok := t.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, d)
	}
	go func() {
		defer cancel()
		_, err := send(ctx, "Test.large", map[string]string{"payload": strings.Repeat("x", 4<<20)})
		done <- err
	}()
	select {
	case <-p.entered:
	case err := <-done:
		t.Fatalf("fixture: the large send ended before its socket write began: %v", err)
	}
	const blockedFor = 100 * time.Millisecond
	for last, still := p.written.Load(), time.Now(); time.Since(still) < blockedFor; {
		time.Sleep(10 * time.Millisecond)
		if n := p.written.Load(); n != last {
			last, still = n, time.Now()
		}
		select {
		case err := <-done:
			t.Fatalf("fixture: the large write ended before it blocked: %v", err)
		default:
		}
	}
	return done
}

// checkInterrupted waits for the interrupted large write.
func checkInterrupted(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrNotSent) {
			t.Errorf("interrupted write = %v, want a write error without ErrNotSent (part of the message was written)", err)
		}
	case <-time.After(interruptPromptly):
		t.Errorf("InterruptWrites ran, but the socket write still waits for its deadline")
	}
}

// checkNeverWrittenAgain: a call after the interrupt is refused at once,
// before anything is written.
func checkNeverWrittenAgain(t *testing.T, send func(context.Context, string, any) (json.RawMessage, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := send(ctx, "Test.after", nil)
	if !errors.Is(err, ErrNotSent) || !errors.Is(err, errWritesCut) {
		t.Errorf("call after InterruptWrites = %v, want ErrNotSent with the interruption as cause", err)
	}
	if took := time.Since(start); took > interruptPromptly {
		t.Errorf("call after InterruptWrites returned after %v, want at once", took.Round(time.Millisecond))
	}
}

func TestClientInterruptWritesEndsABlockedWrite(t *testing.T) {
	p := newBlockedPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClientWithDialer(ctx, p.url, p.dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	<-p.remote
	large := p.startLargeWrite(t, c.Send)

	// A call whose deadline ends while it waits for the socket was never
	// written: ErrNotSent, with the text it always had.
	qctx, qcancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = c.Send(qctx, "Test.queued", nil)
	qcancel()
	if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "Test.queued: not sent, another command was still writing after") {
		t.Errorf("queued call = %v, want ErrNotSent and the text \"Test.queued: not sent, another command was still writing after ...\"", err)
	}

	c.InterruptWrites()
	checkInterrupted(t, large)
	checkNeverWrittenAgain(t, c.Send)
}

func TestSessionInterruptWritesEndsABlockedWrite(t *testing.T) {
	p := newBlockedPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialWebSocket(ctx, p.dialer, p.url, transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ctx, conn, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	remote := <-p.remote
	events := make(chan string, 1)
	defer s.Subscribe("Test.event", func(method string, _ json.RawMessage) { events <- method })()
	large := p.startLargeWrite(t, s.Send)

	// A call queued behind the blocked write, without a deadline of its
	// own that ends: the interrupt refuses it before it writes anything.
	queued := make(chan error, 1)
	queuedStarted := make(chan struct{})
	go func() {
		_, err := s.Send(ctx, "Test.queued", marshalHook{func() { close(queuedStarted) }})
		queued <- err
	}()
	<-queuedStarted

	s.InterruptWrites()
	checkInterrupted(t, large)
	select {
	case err := <-queued:
		if !errors.Is(err, ErrNotSent) {
			t.Errorf("queued call = %v, want ErrNotSent", err)
		}
	case <-time.After(interruptPromptly):
		t.Error("the queued call did not end after InterruptWrites")
	}
	checkNeverWrittenAgain(t, s.Send)

	// Only the writes end: the session still reads the page's messages.
	inner, _ := json.Marshal(map[string]any{"method": "Test.event", "params": map[string]any{}})
	if err := remote.WriteJSON(map[string]any{"method": "Target.dispatchMessageFromTarget", "params": map[string]any{"targetId": "page-1", "message": string(inner)}}); err != nil {
		t.Fatalf("fixture: the peer could not write an event: %v", err)
	}
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Error("the session read no event after InterruptWrites")
	}
}

// cutConn keeps its writes cut when gorilla sets the deadline again before
// the next frame of a message: a write blocked in the kernel ends, and the
// next frame fails before it writes anything.
func TestCutConnKeepsWritesCutAcrossFrames(t *testing.T) {
	a, b := net.Pipe() // a write blocks until the other end reads; b never reads
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	c := &cutConn{Conn: a}
	blocked := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte("frame 1"))
		blocked <- err
	}()
	c.interrupt()
	select {
	case err := <-blocked:
		if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, errWritesCut) {
			t.Errorf("write in progress = %v, want the passed deadline (or the cut, when it had not begun)", err)
		}
	case <-time.After(interruptPromptly):
		t.Fatal("the write in progress still waits after interrupt")
	}
	if err := c.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("frame 2")); !errors.Is(err, errWritesCut) {
		t.Errorf("next frame = %v, want errWritesCut", err)
	}
	// The deadline that the frame set is not applied either.
	socket := make(chan error, 1)
	go func() {
		_, err := c.Conn.Write([]byte("frame 2"))
		socket <- err
	}()
	select {
	case err := <-socket:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("socket write after a new deadline = %v, want the passed deadline kept", err)
		}
	case <-time.After(interruptPromptly):
		t.Error("socket write after a new deadline still waits, want the passed deadline kept")
	}
}

// Without a write in progress InterruptWrites changes nothing: a stall
// declared on a socket that takes data leaves it writable, so Close can
// still resume targets paused on start (design 2.7).
func TestInterruptWritesWithoutAWriteKeepsTheConnectionWritable(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newFakeSession(t, f, SessionOptions{})
	s.InterruptWrites()
	if _, err := s.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"}); err != nil {
		t.Errorf("session call after an idle InterruptWrites = %v, want its reply", err)
	}
	c, err := NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.InterruptWrites()
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"}); err != nil {
		t.Errorf("client call after an idle InterruptWrites = %v, want its reply", err)
	}
}

// Parameters that do not encode fail before the write: ErrNotSent, with
// the text Send always had.
func TestClientParamsThatDoNotEncodeAreNotSent(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Send(ctx, "Runtime.evaluate", map[string]any{"bad": make(chan int)})
	if !errors.Is(err, ErrNotSent) || !strings.HasPrefix(err.Error(), "marshaling params: ") {
		t.Errorf("Send with a channel parameter = %v, want ErrNotSent and the text \"marshaling params: ...\"", err)
	}
	if got := f.ReceivedMethod("Runtime.evaluate"); len(got) != 0 {
		t.Errorf("the page got %d Runtime.evaluate calls, want none", len(got))
	}
}
