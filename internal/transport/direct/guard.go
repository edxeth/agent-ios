package direct

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Operations with a known large result are refused before sending when the
// result would exceed the frame cap, because the reply would be dropped
// (design section 6). Results above the cap need chunk reassembly, which v1
// does not have. Page.snapshotRect is the one such operation whose size can
// be estimated from its parameters; heap snapshots cannot be estimated, so
// their reply is dropped and reported with EventMessageDropped instead.

const (
	// maxDeviceScale is the largest devicePixelRatio of an iPhone; the
	// snapshot is rendered at the device scale.
	maxDeviceScale = 3
	// bytesPerPixel: worst case for a PNG that does not compress (RGBA).
	bytesPerPixel = 4
	// snapshotOverhead covers the data URL prefix and the JSON envelopes.
	snapshotOverhead = 1024
)

// ResultTooLargeError reports an operation refused before sending because
// its estimated result exceeds the frame cap.
type ResultTooLargeError struct {
	Method   string
	Estimate int64 // upper estimate of the reply, in bytes
	Limit    int   // frame cap (transport.Limits.MaxIncoming)
}

func (e *ResultTooLargeError) Error() string {
	return fmt.Sprintf("%s not sent: its result can reach %d bytes, above the %d-byte message limit of the direct transport; capture a smaller region or use --transport iwdp",
		e.Method, e.Estimate, e.Limit)
}

// snapshotRectEstimate is an upper bound for the reply of Page.snapshotRect
// of width x height CSS pixels: an uncompressed RGBA image at the largest
// device scale, base64-encoded in a data URL.
func snapshotRectEstimate(width, height float64) int64 {
	raw := width * maxDeviceScale * height * maxDeviceScale * bytesPerPixel
	return int64(raw*4/3) + snapshotOverhead
}

// checkLargeResult returns a *ResultTooLargeError for a Page.snapshotRect
// message, plain or wrapped in Target.sendMessageToTarget, whose estimate
// exceeds limit. A message that does not parse is not this guard's to judge:
// it is passed on, and WebKit answers it with a protocol error.
func checkLargeResult(msg []byte, limit int) error {
	const method = "Page.snapshotRect"
	if !bytes.Contains(msg, []byte(method)) {
		return nil
	}
	type call struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var c call
	if json.Unmarshal(msg, &c) != nil {
		return nil
	}
	if c.Method == "Target.sendMessageToTarget" {
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(c.Params, &p) != nil || json.Unmarshal([]byte(p.Message), &c) != nil {
			return nil
		}
	}
	if c.Method != method {
		return nil
	}
	var r struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	if json.Unmarshal(c.Params, &r) != nil {
		return nil
	}
	if est := snapshotRectEstimate(r.Width, r.Height); est > int64(limit) {
		return &ResultTooLargeError{Method: method, Estimate: est, Limit: limit}
	}
	return nil
}
