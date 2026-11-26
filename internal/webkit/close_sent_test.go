package webkit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// F2: once a close frame went out (the reply to the peer's close, or
// gorilla's own close after a protocol error), no data frame follows it
// (RFC 6455 5.5.1): the write fails as not written with gorilla's
// ErrCloseSent, as it did before the single-frame writer.
func TestNoDataFrameAfterACloseFrame(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first []byte // what the server sends first
	}{
		{"the reply to the peer's close", []byte{0x88, 2, 0x03, 0xe8}},
		{"gorilla's close after a protocol error", []byte{0xc1, 0}}, // RSV1 set
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			after := make(chan byte, 4)
			url := newRawWSServer(t, func(c net.Conn, br *bufio.Reader) {
				_, _ = c.Write(tc.first)
				sawClose := false
				for {
					f, err := readRawFrame(br)
					if err != nil {
						close(after)
						return
					}
					if sawClose {
						after <- f.b0
					}
					if f.b0 == 0x88 {
						sawClose = true
					}
				}
			})
			conn, err := dialWebSocket(ctx, &websocket.Dialer{}, url, transport.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Read(ctx); err == nil {
				t.Fatal("read after the server's frame: no error")
			}
			werr := conn.Write(ctx, []byte("{}"))
			_ = conn.Close()
			b, ok := <-after
			if ok {
				t.Fatalf("a frame (first byte %#02x) went out after the close frame; write err %v", b, werr)
			}
			if !errors.Is(werr, websocket.ErrCloseSent) || !errors.Is(werr, transport.ErrNotWritten) {
				t.Fatalf("write after the close frame = %v, want ErrNotWritten with websocket.ErrCloseSent", werr)
			}
		})
	}
}

// F3: gorilla's close after a protocol error waits for the frame lock only
// until its own write deadline (1 s), as gorilla's WriteControl did: the
// read reports the protocol error while a large data frame is still stuck
// in the socket, instead of waiting for that frame's deadline.
func TestProtocolErrorEndsTheReadDuringABlockedWrite(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	url := newRawWSServer(t, func(c net.Conn, br *bufio.Reader) {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return
		}
		close(started) // the data frame began; this server reads no more
		_, _ = c.Write([]byte{0xc1, 0})
		<-release
	})
	t.Cleanup(func() { close(release) }) // before the server's cleanup
	dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(4096)
		}
		return c, err
	}}
	ctx := testCtx(t)
	conn, err := dialWebSocket(ctx, dialer, url, transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The data frame's own deadline: the bound that the close must not wait for.
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- conn.Write(wctx, bytes.Repeat([]byte{'a'}, 32<<20)) }()
	<-started
	read := make(chan error, 1)
	go func() {
		_, err := conn.Read(ctx)
		read <- err
	}()
	select {
	case err := <-read:
		if err == nil || !strings.Contains(err.Error(), "RSV1 set") {
			t.Fatalf("read = %v, want gorilla's protocol error (RSV1 set)", err)
		}
	case err := <-written:
		t.Fatalf("the data frame ended first (%v): the read waited for it", err)
	}
}
