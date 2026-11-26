package webkit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// FIX-V: ios_webkit_debug_proxy 1.9.2 keeps one fragmentation flag for both
// directions of a page WebSocket (websocket.c continued_opcode). While a
// fragmented client message is half received, it sends every frame to the
// client as a continuation, which gorilla rejects ("continuation after
// FIN"), and the daemon loses the page. So every message that agent-ios
// writes must leave as one frame: FIN set, opcode text, never a
// continuation. These tests read the frames on the wire.

// rawFrame is one WebSocket frame as a raw server read it.
type rawFrame struct {
	b0     byte
	masked bool
	op     byte
	fin    bool
	data   []byte // unmasked payload
}

// readRawFrame reads one frame of any kind from br (RFC 6455 5.2).
func readRawFrame(br *bufio.Reader) (rawFrame, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return rawFrame{}, err
	}
	f := rawFrame{b0: h[0], fin: h[0]&0x80 != 0, op: h[0] & 0x0f, masked: h[1]&0x80 != 0}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return rawFrame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return rawFrame{}, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 64<<20 {
		return rawFrame{}, fmt.Errorf("frame length %d: the stream is out of step", n)
	}
	var key [4]byte
	if f.masked {
		if _, err := io.ReadFull(br, key[:]); err != nil {
			return rawFrame{}, err
		}
	}
	f.data = make([]byte, n)
	if _, err := io.ReadFull(br, f.data); err != nil {
		return rawFrame{}, err
	}
	if f.masked {
		for i := range f.data {
			f.data[i] ^= key[i%4]
		}
	}
	return f, nil
}

// writeServerFrame writes one unmasked final frame, as a server does.
func writeServerFrame(w io.Writer, op byte, data []byte) error {
	if len(data) > 125 {
		return fmt.Errorf("test server frames carry at most 125 bytes, got %d", len(data))
	}
	_, err := w.Write(append([]byte{0x80 | op, byte(len(data))}, data...))
	return err
}

// newRawWSServer starts a WebSocket server that completes the handshake
// itself and hands the raw connection to handle, so the test sees every
// frame header the client wrote. It returns the ws:// URL.
func newRawWSServer(t *testing.T, handle func(c net.Conn, br *bufio.Reader)) string {
	t.Helper()
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		wg.Add(1)
		defer wg.Done()
		defer func() { _ = c.Close() }()
		_, _ = fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		if brw.Flush() != nil {
			return
		}
		handle(c, brw.Reader)
	}))
	t.Cleanup(func() { srv.Close(); wg.Wait() })
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// checkSingleTextFrame fails unless f carries want whole: FIN set, opcode
// text (first byte 0x81), masked as a client frame must be.
func checkSingleTextFrame(t *testing.T, f rawFrame, want []byte) {
	t.Helper()
	if f.b0 != 0x81 || !f.masked {
		t.Fatalf("frame of a %d-byte message: first byte %#02x, masked %v; want 0x81 (FIN, text) and masked, frame payload %d bytes", len(want), f.b0, f.masked, len(f.data))
	}
	if !bytes.Equal(f.data, want) {
		t.Fatalf("frame payload of a %d-byte message differs (%d bytes): the message was not sent as one frame", len(want), len(f.data))
	}
}

// FIX-V: wsConn.Write (every iwdp and WebKitGTK page connection) sends each
// message as one frame, for every length encoding (7-bit, 16-bit, 64-bit)
// and for messages above gorilla's 4096-byte write buffer.
func TestWriteSendsEachMessageAsOneFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frames := make(chan rawFrame, 16)
	url := newRawWSServer(t, func(_ net.Conn, br *bufio.Reader) {
		for {
			f, err := readRawFrame(br)
			if err != nil {
				close(frames)
				return
			}
			frames <- f
		}
	})
	conn, err := DialWebSocket(ctx, url, transport.Limits{MaxOutgoing: transport.IWDPMaxOutgoing, Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, n := range []int{0, 1, 125, 126, 4096, 6206, 65535, 65536, 300_000} {
		msg := bytes.Repeat([]byte{byte('a' + n%26)}, n)
		if err := conn.Write(ctx, msg); err != nil {
			t.Fatalf("writing %d bytes: %v", n, err)
		}
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("the server lost the stream after a %d-byte message", n)
			}
			checkSingleTextFrame(t, f, msg)
		case <-ctx.Done():
			t.Fatalf("no frame for a %d-byte message", n)
		}
	}
}

// FIX-V: webkit.Client (the legacy single-connection client, also used
// against iwdp) sends each command as one frame too.
func TestClientSendsEachMessageAsOneFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frames := make(chan rawFrame, 4)
	url := newRawWSServer(t, func(c net.Conn, br *bufio.Reader) {
		for {
			f, err := readRawFrame(br)
			if err != nil {
				return
			}
			frames <- f
			var m struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(f.data, &m) == nil && m.ID != 0 {
				_ = writeServerFrame(c, 1, fmt.Appendf(nil, `{"id":%d,"result":{}}`, m.ID))
			}
		}
	})
	client, err := NewClient(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	sent := make(chan error, 1)
	go func() {
		_, err := client.Send(ctx, "Runtime.evaluate", map[string]any{"expression": strings.Repeat("x", 6206)})
		sent <- err
	}()
	f := <-frames
	if f.b0 != 0x81 || !f.masked {
		t.Fatalf("command frame: first byte %#02x, masked %v, payload %d bytes; want 0x81 (FIN, text), masked", f.b0, f.masked, len(f.data))
	}
	if !json.Valid(f.data) || len(f.data) < 6206 {
		t.Fatalf("the command's frame holds %d bytes that are not the whole command", len(f.data))
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

// slowConn hands its reader at most 4096 bytes per Read and pauses before
// each, so the client's large writes block in the kernel while the server
// sends pings and a close.
type slowConn struct{ net.Conn }

func (s slowConn) Read(b []byte) (int, error) {
	time.Sleep(200 * time.Microsecond)
	if len(b) > 4096 {
		b = b[:4096]
	}
	return s.Conn.Read(b)
}

// yieldingConn pauses after each socket write, so that a control frame
// waiting for the socket gets it between two writes of a large frame unless
// the frame lock keeps it out (the Go runtime alone rarely lets it in).
type yieldingConn struct{ net.Conn }

func (y yieldingConn) Write(b []byte) (int, error) {
	n, err := y.Conn.Write(b)
	time.Sleep(time.Millisecond)
	return n, err
}

// FIX-V: the pongs and the close reply that the read side writes (the ping
// and close handlers) never land inside a message frame, and concurrent
// writers stay serialized: on the wire every frame is whole, every message
// is one intact text frame, and the control frames come between them.
func TestControlFramesAndWritersDoNotInterleave(t *testing.T) {
	checkNoInterleave(t, 10, []byte{0x88, 2, 0x03, 0xe8}, 1000) // the server closes
}

// FIX-V: the close frame that gorilla itself writes after a protocol error
// (here a frame with a reserved bit set) does not land inside a message
// frame either.
func TestProtocolErrorCloseDoesNotInterleave(t *testing.T) {
	checkNoInterleave(t, 0, []byte{0xc1, 0}, 1002) // RSV1 set: protocol error
}

// checkNoInterleave runs concurrent large writes while the server sends
// pings and then last, raw, and checks the client's frame stream; the
// client must answer last with one close frame with code wantClose. The
// client reads the frames one by one and each pong waits for the data
// frame in progress, so few pings keep last inside the writes.
func checkNoInterleave(t *testing.T, pings int, last []byte, wantClose uint16) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const (
		writers   = 4
		perWriter = 3
	)
	type result struct {
		messages map[int]int // message size -> count
		pongs    []string
		closes   [][]byte
		err      error
	}
	done := make(chan result, 1)
	url := newRawWSServer(t, func(c net.Conn, _ *bufio.Reader) {
		var wmu sync.Mutex
		go func() {
			for i := range pings {
				time.Sleep(2 * time.Millisecond)
				wmu.Lock()
				_ = writeServerFrame(c, 0x9, fmt.Appendf(nil, "p%02d", i))
				wmu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
			wmu.Lock()
			_, _ = c.Write(last)
			wmu.Unlock()
		}()
		res := result{messages: map[int]int{}}
		br := bufio.NewReaderSize(slowConn{c}, 4096)
		for total := 0; total < writers*perWriter || len(res.closes) == 0; {
			f, err := readRawFrame(br)
			if err != nil {
				res.err = fmt.Errorf("after %d messages, %d pongs: %w", total, len(res.pongs), err)
				break
			}
			if !f.masked {
				res.err = fmt.Errorf("unmasked frame, first byte %#02x", f.b0)
				break
			}
			switch f.b0 {
			case 0x81:
				if len(f.data) == 0 || !bytes.Equal(f.data, bytes.Repeat(f.data[:1], len(f.data))) {
					res.err = fmt.Errorf("text frame of %d bytes is not one whole message: foreign bytes inside", len(f.data))
				}
				res.messages[len(f.data)]++
				total++
			case 0x8a:
				res.pongs = append(res.pongs, string(f.data))
			case 0x88:
				res.closes = append(res.closes, f.data)
			default:
				res.err = fmt.Errorf("frame with first byte %#02x (want 0x81 text, 0x8a pong or 0x88 close), payload %d bytes", f.b0, len(f.data))
			}
			if res.err != nil {
				break
			}
		}
		done <- res
	})
	dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(4096)
		}
		return yieldingConn{c}, nil
	}}
	conn, err := dialWebSocket(ctx, dialer, url, transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	go func() { // the read side answers pings and the close
		for {
			if _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	want := map[int]int{}
	var wg sync.WaitGroup
	for w := range writers {
		for m := range perWriter {
			n := 150_000 + (w*perWriter+m)*1000
			want[n] = 1
		}
		wg.Go(func() {
			for m := range perWriter {
				n := 150_000 + (w*perWriter+m)*1000
				if err := conn.Write(ctx, bytes.Repeat([]byte{byte('a' + w*perWriter + m)}, n)); err != nil {
					t.Errorf("writer %d message %d: %v", w, m, err)
					return
				}
			}
		})
	}
	wg.Wait()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		t.Fatal("the server did not get every message and the close reply")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	if fmt.Sprint(res.messages) != fmt.Sprint(want) {
		t.Fatalf("messages by size: got %v, want %v", res.messages, want)
	}
	if len(res.pongs) != pings {
		t.Fatalf("%d pongs, want %d", len(res.pongs), pings)
	}
	for i := 1; i < len(res.pongs); i++ {
		if res.pongs[i] <= res.pongs[i-1] {
			t.Fatalf("pongs out of order: %v", res.pongs)
		}
	}
	if len(res.closes) != 1 || len(res.closes[0]) < 2 || binary.BigEndian.Uint16(res.closes[0]) != wantClose {
		t.Fatalf("close replies: %q, want one with code %d", res.closes, wantClose)
	}
}

// FIX-V: maskBytes masks any piece of a payload, from any offset, as RFC
// 6455 5.3 defines it (byte i XOR key[i mod 4]); writeFrame masks large
// frames piece by piece.
func TestMaskBytesMatchesRFC(t *testing.T) {
	prop := func(key [4]byte, pos uint16, b []byte) bool {
		got := bytes.Clone(b)
		maskBytes(key, int(pos), got)
		for i := range b {
			if got[i] != b[i]^key[(int(pos)+i)%4] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
