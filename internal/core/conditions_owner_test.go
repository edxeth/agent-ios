package core

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Parity-045 review F4: the condition owners hold only what WebKit holds.
// A refused send (a protocol error, or a call never written) puts the
// owner's record back, so an identical retry is sent again; after an
// unknown outcome (the call may have run) the next set sends the
// effective conditions even when the record did not change.
func TestEmulatedConditionsFollowWebKit(t *testing.T) {
	o, other := Owner("a"), Owner("b")
	limit := json.RawMessage(`{"bandwidth":1}`)
	for _, tc := range []struct {
		name string
		err  error
		// resend: the identical retry is sent
		resend bool
	}{
		{"refused", &webkit.ErrorData{Code: -32000, Message: "refused"}, true},
		{"not sent", webkit.ErrNotSent, true},
		{"unknown outcome", errors.New("connection lost"), true},
		{"applied", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDesiredState()
			send, undo := d.SetEmulatedConditions(o, limit)
			if string(send) != string(limit) {
				t.Fatalf("first set sends %s", send)
			}
			d.SentEmulatedConditions(tc.err, undo)
			if send, _ := d.SetEmulatedConditions(o, limit); (send != nil) != tc.resend {
				t.Fatalf("retry sends %s, want a send: %v", send, tc.resend)
			}
		})
	}
	// A refused change of one owner leaves another owner's effective
	// conditions recorded.
	d := NewDesiredState()
	send, undo := d.SetEmulatedConditions(other, limit)
	d.SentEmulatedConditions(nil, undo)
	if string(send) != string(limit) {
		t.Fatalf("other's set sends %s", send)
	}
	send, undo = d.SetEmulatedConditions(o, json.RawMessage(`{"bandwidth":2}`))
	d.SentEmulatedConditions(&webkit.ErrorData{Code: -32000, Message: "refused"}, undo)
	if send == nil {
		t.Fatal("a's set sent nothing")
	}
	if got := d.effectiveCondsLocked(); string(got) != string(limit) {
		t.Fatalf("after a's refused set the effective conditions are %s, want other's %s", got, limit)
	}
}
