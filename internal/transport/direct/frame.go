package direct

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"howett.net/plist"
)

// The webinspectord service speaks length-prefixed property lists: a 4-byte
// big-endian length, then the plist. All pages of a device share this one
// stream, so the reader must never leave part of a frame in it. go-ios's
// PlistCodecReadWriter rejects an oversized frame after reading only the
// length and leaves the body in the stream (design section 6), so this
// package owns the frame reader.

const frameHeaderLen = 4

// errDrainFailed: an oversized frame could not be skipped in time. The stream
// position is unknown afterwards, so the device connection must close.
var errDrainFailed = errors.New("could not skip an oversized message from the device in time")

// readDeadliner is the part of a connection that the drain needs.
type readDeadliner interface {
	SetReadDeadline(t time.Time) error
}

// frameReader reads frames from one device stream.
type frameReader struct {
	r            io.Reader
	dl           readDeadliner
	maxFrame     int           // frame cap (transport.Limits.MaxIncoming)
	drainTimeout time.Duration // budget to skip one frame above the cap
	header       [frameHeaderLen]byte
}

// next returns the payload of the next frame. A frame above the cap is read
// to io.Discard within drainTimeout, and next returns a nil payload with its
// size in dropped. Any error leaves the stream unusable.
func (f *frameReader) next() (payload []byte, dropped int64, err error) {
	if _, err := io.ReadFull(f.r, f.header[:]); err != nil {
		return nil, 0, err
	}
	n := int64(binary.BigEndian.Uint32(f.header[:]))
	if n > int64(f.maxFrame) {
		if err := f.dl.SetReadDeadline(time.Now().Add(f.drainTimeout)); err != nil {
			return nil, 0, fmt.Errorf("%w: setting the read deadline: %v", errDrainFailed, err)
		}
		copied, err := io.CopyN(io.Discard, f.r, n)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: skipped %d of %d bytes within %s: %v", errDrainFailed, copied, n, f.drainTimeout, err)
		}
		if err := f.dl.SetReadDeadline(time.Time{}); err != nil {
			return nil, 0, fmt.Errorf("%w: clearing the read deadline: %v", errDrainFailed, err)
		}
		return nil, n, nil
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return nil, 0, fmt.Errorf("reading a %d-byte message from the device: %w", n, err)
	}
	return payload, 0, nil
}

// encodeFrame serializes v as a binary plist with its length header.
// webinspectord reads binary and XML plists; ios_webkit_debug_proxy sends
// binary ones. Binary keeps socket data unencoded (XML would base64 it) and
// carries any string, while XML 1.0 cannot carry most control characters.
func encodeFrame(v any) ([]byte, error) {
	body, err := plist.Marshal(v, plist.BinaryFormat)
	if err != nil {
		return nil, fmt.Errorf("encoding a webinspector message: %w", err)
	}
	frame := make([]byte, frameHeaderLen+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[frameHeaderLen:], body)
	return frame, nil
}

// wirMessage is one decoded webinspector message.
type wirMessage struct {
	Selector string         `plist:"__selector"`
	Argument map[string]any `plist:"__argument"`
}

func decodeMessage(payload []byte) (wirMessage, error) {
	var m wirMessage
	if _, err := plist.Unmarshal(payload, &m); err != nil {
		return wirMessage{}, fmt.Errorf("decoding a webinspector message: %w", err)
	}
	return m, nil
}
