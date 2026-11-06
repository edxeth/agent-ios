package fakewebkit

import (
	"encoding/json"
	"testing"
	"time"
)

func attemptJSON(op string, epoch uint64, step int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"opId": op, "epoch": epoch, "doc": "doc-1", "step": step, "steps": 3, "attemptEnd": 100.0, "deadline": 100.0})
	return b
}

func statusOf(t *testing.T, v any) string {
	t.Helper()
	env, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("reply %#v is not an envelope", v)
	}
	val, _ := env["value"].(map[string]any)
	s, _ := val["status"].(string)
	return s
}

// Design 3.1, operation fencing: page code does no side effect when the
// epoch changed. An attempt of an older lease epoch than the page's owner
// is refused.
func TestPageRuntimeRefusesOldEpoch(t *testing.T) {
	r := NewPageRuntime()
	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-1", 2, 1))); s != "dispatched" {
		t.Fatalf("epoch 2 attempt status = %q, want dispatched", s)
	}
	if s := statusOf(t, r.HandleAt(time.Millisecond, "act", attemptJSON("op-2", 1, 1))); s != "refused" {
		t.Fatalf("epoch 1 attempt after epoch 2 status = %q, want refused", s)
	}
	if d := r.Dispatched(); len(d) != 1 || d[0].OpID != "op-1" {
		t.Fatalf("dispatches = %+v, want only op-1", d)
	}
}

// Design 2.6, cancellation rule 2: page code never dispatches after the
// call has settled. A synchronous attempt settles with its reply, so no
// later event of its step runs.
func TestPageRuntimeRefusesEventsAfterSettlement(t *testing.T) {
	r := NewPageRuntime()
	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-1", 1, 1))); s != "dispatched" {
		t.Fatalf("status = %q, want dispatched", s)
	}
	if r.TryEvent("op-1", time.Millisecond) {
		t.Fatal("an event of the step ran after its call settled")
	}
}

// KeepOpen models an attempt whose events run in separate tasks: later
// events run until the attempt end, a cancel, a newer owner epoch or the
// settlement of the call.
func TestPageRuntimeOpenAttemptEvents(t *testing.T) {
	r := NewPageRuntime()
	r.KeepOpen = true
	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-1", 1, 1))); s != "dispatched" {
		t.Fatalf("status = %q, want dispatched", s)
	}
	if !r.TryEvent("op-1", 50*time.Millisecond) {
		t.Fatal("a later event before the attempt end did not run")
	}
	if r.TryEvent("op-1", 100*time.Millisecond) {
		t.Fatal("an event at the attempt end ran")
	}
	r.Settle("op-1")
	if r.TryEvent("op-1", 60*time.Millisecond) {
		t.Fatal("an event ran after Settle")
	}

	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-2", 1, 1))); s != "dispatched" {
		t.Fatalf("op-2 status = %q", s)
	}
	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-3", 2, 1))); s != "dispatched" {
		t.Fatalf("op-3 status = %q", s)
	}
	if r.TryEvent("op-2", 10*time.Millisecond) {
		t.Fatal("an event of epoch 1 ran after epoch 2 became the owner")
	}
}

// Design 2.6: a new document has a new runtime, and an attempt addressed
// to another document does nothing.
func TestPageRuntimeRefusesOtherDocument(t *testing.T) {
	r := NewPageRuntime()
	clock := r.HandleAt(0, "clock", nil).(map[string]any)["value"].(map[string]any)
	if clock["doc"] != "doc-1" {
		t.Fatalf("clock reply %v, want doc-1", clock)
	}
	r.Navigate()
	if s := statusOf(t, r.HandleAt(0, "act", attemptJSON("op-1", 1, 1))); s != "refused" {
		t.Fatalf("attempt for doc-1 on doc-2 status = %q, want refused", s)
	}
	if d := r.Dispatched(); len(d) != 0 {
		t.Fatalf("dispatches = %+v", d)
	}
}
