package direct

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

type noDeadline struct{ set []time.Time }

func (n *noDeadline) SetReadDeadline(t time.Time) error { n.set = append(n.set, t); return nil }

func rawFrame(payload []byte) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

// Property: for any sequence of frames, the reader returns each frame at or
// below the cap whole and in order, reports each frame above the cap with its
// size, and stays aligned to the next frame.
func TestFrameReaderPropertyStaysAligned(t *testing.T) {
	const maxFrame = 64
	prop := func(sizes []uint8, seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		var stream bytes.Buffer
		for _, s := range sizes {
			p := make([]byte, int(s)) // 0..255, so about three quarters exceed the cap
			rng.Read(p)
			stream.Write(rawFrame(p))
		}
		want := stream.Bytes()
		dl := &noDeadline{}
		fr := &frameReader{r: bytes.NewReader(want), dl: dl, maxFrame: maxFrame, drainTimeout: time.Second}
		off := 0
		for _, s := range sizes {
			payload, dropped, err := fr.next()
			if err != nil {
				return false
			}
			n := int(s)
			if n > maxFrame {
				if payload != nil || dropped != int64(n) {
					return false
				}
			} else if dropped != 0 || !bytes.Equal(payload, want[off+4:off+4+n]) {
				return false
			}
			off += 4 + n
		}
		_, _, err := fr.next()
		return errors.Is(err, io.EOF)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// The drain of an oversized frame runs under a read deadline and clears it
// afterwards.
func TestFrameReaderDrainSetsAndClearsDeadline(t *testing.T) {
	dl := &noDeadline{}
	fr := &frameReader{r: bytes.NewReader(rawFrame(make([]byte, 10))), dl: dl, maxFrame: 5, drainTimeout: time.Minute}
	if _, dropped, err := fr.next(); err != nil || dropped != 10 {
		t.Fatalf("next = %d, %v", dropped, err)
	}
	if len(dl.set) != 2 || time.Until(dl.set[0]) < 50*time.Second || !dl.set[1].IsZero() {
		t.Fatalf("deadlines = %v, want now+drainTimeout then none", dl.set)
	}
}

// A truncated frame is an error, never a short payload.
func TestFrameReaderTruncatedFrame(t *testing.T) {
	b := rawFrame([]byte("abcdef"))
	fr := &frameReader{r: bytes.NewReader(b[:7]), dl: &noDeadline{}, maxFrame: 100, drainTimeout: time.Second}
	if _, _, err := fr.next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame: %v, want ErrUnexpectedEOF", err)
	}
	fr = &frameReader{r: bytes.NewReader(rawFrame(make([]byte, 50))[:20]), dl: &noDeadline{}, maxFrame: 10, drainTimeout: time.Second}
	if _, _, err := fr.next(); !errors.Is(err, errDrainFailed) {
		t.Fatalf("truncated oversized frame: %v, want errDrainFailed", err)
	}
}

// Property: a WIR message survives encodeFrame and the frame reader with its
// selector, strings, integers and raw socket data (any bytes, including
// invalid UTF-8) unchanged.
func TestWIRMessageRoundTripProperty(t *testing.T) {
	prop := func(selector, sender string, page uint16, data []byte) bool {
		if data == nil {
			data = []byte{}
		}
		in := wirMessage{Selector: selector, Argument: map[string]any{
			keySender: sender, keyPageID: int(page), keySocketData: data, keyAutomaticallyPause: false,
		}}
		frame, err := encodeFrame(in)
		if err != nil {
			return false
		}
		fr := &frameReader{r: bytes.NewReader(frame), dl: &noDeadline{}, maxFrame: len(frame), drainTimeout: time.Second}
		payload, _, err := fr.next()
		if err != nil {
			return false
		}
		out, err := decodeMessage(payload)
		if err != nil {
			return false
		}
		gotPage, ok := plistInt(out.Argument[keyPageID])
		return out.Selector == selector && out.Argument[keySender] == sender && ok && gotPage == int(page) &&
			reflect.DeepEqual(out.Argument[keySocketData], data) && out.Argument[keyAutomaticallyPause] == false
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
