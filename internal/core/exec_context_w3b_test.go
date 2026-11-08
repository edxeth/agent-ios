package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// paramsCaller records the parameters of every Send.
type paramsCaller struct {
	nopCaller
	mu   sync.Mutex
	sent []map[string]any
}

func (c *paramsCaller) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(params)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	c.sent = append(c.sent, map[string]any{"method": method, "params": m})
	c.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

// Design 3.8 (W3b review finding 3): an operation with an execution
// context sends every Runtime.evaluate to it unless the call names one;
// other methods and the main context are unchanged; each context of a
// document generation has its own clock mapping.
func TestOperationExecutionContext(t *testing.T) {
	c := &paramsCaller{}
	s := NewScheduler(c, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease("a", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.Begin(ctx, OpSpec{Name: "t", Class: ClassRead, Session: "a", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	send := func(method string, params any) map[string]any {
		t.Helper()
		if _, err := op.Caller().Send(ctx, method, params); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		p, _ := c.sent[len(c.sent)-1]["params"].(map[string]any)
		return p
	}
	if p := send("Runtime.evaluate", map[string]any{"expression": "1"}); p["contextId"] != nil {
		t.Fatalf("main context sent %v", p)
	}
	main := s.clockMap(op.clockKey(1))
	op.SetExecutionContext(ExecContext{ID: 7})
	if op.ExecutionContext().ID != 7 {
		t.Fatal("ExecutionContext does not return the context")
	}
	in := map[string]any{"expression": "1"}
	if p := send("Runtime.evaluate", in); p["contextId"] != float64(7) || p["expression"] != "1" {
		t.Fatalf("frame context sent %v", p)
	}
	if _, set := in["contextId"]; set {
		t.Fatal("the caller's parameter map was changed")
	}
	if p := send("Runtime.evaluate", map[string]any{"expression": "1", "contextId": 3}); p["contextId"] != float64(3) {
		t.Fatalf("an explicit contextId was replaced: %v", p)
	}
	if p := send("DOM.getDocument", map[string]any{"depth": 0}); p["contextId"] != nil {
		t.Fatalf("DOM.getDocument got a contextId: %v", p)
	}
	frame := s.clockMap(op.clockKey(1))
	if frame == main || s.clockMap(clockKey{gen: 1}) != main {
		t.Fatal("the frame context shares the main context's clock mapping")
	}
	s.dropClock(op.clockKey(1), frame)
	if s.clockMap(op.clockKey(1)) == frame || s.clockMap(clockKey{gen: 1}) != main {
		t.Fatal("dropClock dropped the wrong mapping")
	}
}
