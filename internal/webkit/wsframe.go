package webkit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// Every message that agent-ios writes to a page WebSocket leaves as one
// frame (FIX-V). gorilla/websocket splits a client message larger than its
// write buffer (4096 bytes by default) into several frames, and
// ios_webkit_debug_proxy 1.9.2 keeps one fragmentation flag for both
// directions of a connection (websocket.c, continued_opcode): while a
// fragmented message from us is half received, iwdp marks every frame it
// forwards to us as a continuation. gorilla rejects such a frame
// ("continuation after FIN"), the page connection ends, and a device reply
// that iwdp then cannot forward makes iwdp drop the whole device. So the
// data frames are built here and written straight to the cutConn; gorilla
// only reads, and its control writes (pong, close) go through the same
// frame lock (installControlHandlers, cutConn.Write).

// WebSocket opcodes (RFC 6455 5.2).
const (
	opText  = 0x1
	opClose = 0x8
	opPong  = 0xa
)

// controlWriteWait bounds a pong or close reply, waiting for the frame lock
// included; gorilla uses the same 1 s for its own control writes.
const controlWriteWait = time.Second

// frameChunk is the largest piece of a frame handed to one socket write: a
// large message is masked piece by piece instead of copied whole. The
// pieces form one frame; the frame lock keeps other frames out of it.
const frameChunk = 64 << 10

// lockFrames takes c's frame lock, waiting at most until deadline or until
// ctx ends.
func (c *cutConn) lockFrames(ctx context.Context, deadline time.Time) error {
	select {
	case c.frameSem <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case c.frameSem <- struct{}{}:
		return nil
	case <-timer.C:
		return fmt.Errorf("waiting for another frame write on the connection: %w", os.ErrDeadlineExceeded)
	case <-ctx.Done():
		return fmt.Errorf("waiting for another frame write on the connection: %w", ctx.Err())
	}
}

// writeFrame writes payload as one final, masked client frame with opcode
// op, under the frame lock, with deadline as the socket write deadline.
// started is false when the frame was refused before any socket write (the
// peer never saw it); after a failed socket write started is true, as it
// was for gorilla's writes: it may have written part of the frame, and the
// stream is broken.
func (c *cutConn) writeFrame(ctx context.Context, op byte, payload []byte, deadline time.Time) (started bool, err error) {
	if err := c.lockFrames(ctx, deadline); err != nil {
		return false, err
	}
	defer func() { <-c.frameSem }()
	if c.cut.Load() {
		return false, errWritesCut
	}
	if err := c.Conn.SetWriteDeadline(deadline); err != nil {
		return false, fmt.Errorf("setting the write deadline: %w", err)
	}
	// interrupt sets cut before its passed deadline: when it ran between
	// the check above and this deadline, cut is set now.
	if c.cut.Load() {
		return false, errWritesCut
	}
	var key [4]byte
	_, _ = rand.Read(key[:]) // crypto/rand.Read never fails (Go 1.24+)
	buf := make([]byte, 0, min(len(payload), frameChunk)+14)
	buf = appendFrameHeader(buf, op, len(payload), key)
	for pos := 0; ; {
		n := min(len(payload)-pos, cap(buf)-len(buf))
		start := len(buf)
		buf = append(buf, payload[pos:pos+n]...)
		maskBytes(key, pos, buf[start:])
		if pos > 0 && c.cut.Load() { // a later piece after interrupt
			return true, errWritesCut
		}
		pos += n
		if _, err := c.Conn.Write(buf); err != nil {
			return true, err
		}
		if pos == len(payload) {
			return true, nil
		}
		buf = buf[:0]
	}
}

// appendFrameHeader appends the header of a final, masked frame with
// opcode op and an n-byte payload, masking key included (RFC 6455 5.2).
func appendFrameHeader(b []byte, op byte, n int, key [4]byte) []byte {
	b = append(b, 0x80|op)
	switch {
	case n <= 125:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	return append(b, key[:]...)
}

// maskBytes masks b, the payload bytes from offset pos on, with key.
func maskBytes(key [4]byte, pos int, b []byte) {
	i := 0
	for ; i < len(b) && (pos+i)%4 != 0; i++ {
		b[i] ^= key[(pos+i)%4]
	}
	k := uint64(binary.LittleEndian.Uint32(key[:]))
	k |= k << 32
	for ; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], binary.LittleEndian.Uint64(b[i:])^k)
	}
	for ; i < len(b); i++ {
		b[i] ^= key[(pos+i)%4]
	}
}

// writeControl writes a pong or close reply that gorilla's read side asks
// for, as gorilla's own handlers would: a reply that cannot get the frame
// lock or the socket before controlWriteWait is dropped (nothing was
// written); a reply that failed after it started breaks the stream, so the
// connection takes no more writes and the error ends the read.
func (c *cutConn) writeControl(op byte, data []byte) error {
	started, err := c.writeFrame(context.Background(), op, data, time.Now().Add(controlWriteWait))
	if err == nil || !started {
		return nil
	}
	return c.failed(err)
}

// installControlHandlers routes conn's pong and close replies through
// wire's frame lock (writeControl), so that no control frame lands inside
// a data frame. Only a reply to a protocol error is still written by
// gorilla itself; it goes through cutConn.Write, which takes the lock too.
func installControlHandlers(conn *websocket.Conn, wire *cutConn) {
	conn.SetPingHandler(func(data string) error { return wire.writeControl(opPong, []byte(data)) })
	conn.SetCloseHandler(func(code int, _ string) error {
		_ = wire.writeControl(opClose, websocket.FormatCloseMessage(code, ""))
		return nil
	})
}
