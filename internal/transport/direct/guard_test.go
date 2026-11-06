package direct

import (
	"encoding/json"
	"errors"
	"testing"
)

func snapshotMsg(t *testing.T, routed bool, w, h float64) []byte {
	t.Helper()
	inner, err := json.Marshal(map[string]any{"id": 3, "method": "Page.snapshotRect", "params": map[string]any{"x": 0, "y": 0, "width": w, "height": h, "coordinateSystem": "Page"}})
	if err != nil {
		t.Fatal(err)
	}
	if !routed {
		return inner
	}
	outer, err := json.Marshal(map[string]any{"id": 4, "method": "Target.sendMessageToTarget", "params": map[string]any{"targetId": "t", "message": string(inner)}})
	if err != nil {
		t.Fatal(err)
	}
	return outer
}

// The estimate is the uncompressed RGBA image at scale 3, base64-encoded,
// plus 1024 bytes of envelope (guard.go): 100 x 100 CSS px is 300 x 300
// device px, 360000 bytes raw, 480000 in base64.
func TestSnapshotRectEstimate(t *testing.T) {
	if got := snapshotRectEstimate(100, 100); got != 481024 {
		t.Fatalf("estimate(100x100) = %d, want 481024", got)
	}
}

func TestCheckLargeResult(t *testing.T) {
	const limit = 481024
	for _, routed := range []bool{false, true} {
		if err := checkLargeResult(snapshotMsg(t, routed, 100, 100), limit); err != nil {
			t.Fatalf("routed=%v: estimate at the limit refused: %v", routed, err)
		}
		err := checkLargeResult(snapshotMsg(t, routed, 100, 101), limit)
		var tl *ResultTooLargeError
		if !errors.As(err, &tl) || tl.Method != "Page.snapshotRect" || tl.Limit != limit || tl.Estimate != snapshotRectEstimate(100, 101) {
			t.Fatalf("routed=%v: estimate above the limit: %v", routed, err)
		}
	}
	for name, msg := range map[string]string{
		"other method":           `{"id":1,"method":"Runtime.evaluate","params":{"expression":"'Page.snapshotRect'"}}`,
		"malformed JSON":         `{"method":"Page.snapshotRect",`,
		"malformed routed":       `{"method":"Target.sendMessageToTarget","params":{"message":"Page.snapshotRect{"}}`,
		"snapshotNode (no size)": `{"id":1,"method":"Page.snapshotNode","params":{"nodeId":1}}`,
		"bad params":             `{"id":1,"method":"Page.snapshotRect","params":{"width":"wide"}}`,
	} {
		if err := checkLargeResult([]byte(msg), 1); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
	}
}
