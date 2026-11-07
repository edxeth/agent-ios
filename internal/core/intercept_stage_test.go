package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// WebKit's Network.json requires both requestId and stage for
// Network.interceptContinue, and InspectorNetworkAgent::interceptContinue
// takes the hold from its pending requests or its pending responses by that
// stage. The fake answers a continue without a stage with WebKit's
// InvalidParams error, as WebKit's generated dispatcher does.

// continues returns the params of every Network.interceptContinue that the
// fake received, in order.
func continues(t *testing.T, f *fakewebkit.Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range f.ReceivedMethod("Network.interceptContinue") {
		var p map[string]any
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("interceptContinue params %s: %v", r.Params, err)
		}
		out = append(out, p)
	}
	return out
}

// Design 2.10: the release barrier continues every held request. Each is
// continued at the stage of the event that held it.
func TestReleaseContinuesEachHoldAtItsStage(t *testing.T) {
	for _, tc := range []struct{ event, stage string }{
		{"Network.requestIntercepted", "request"},
		{"Network.responseIntercepted", "response"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{})
			ctx, _ := timeoutCtx(t, 5*time.Second)
			h.f.Emit(tc.event, map[string]string{"requestId": "r1"})
			waitUntil(t, "the held request", func() bool { return len(h.s.PendingRequests()) == 1 })
			if err := h.s.Release(ctx, nil); err != nil {
				t.Fatalf("Release: %v", err)
			}
			want := []map[string]any{{"requestId": "r1", "stage": tc.stage}}
			if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
				t.Fatalf("interceptContinue params = %v, want %v", got, want)
			}
		})
	}
}

// Design 2.4 step 2: the Network reinitialization barrier continues every
// held request, each at the stage of its hold, before Network.disable.
func TestNetworkResetContinuesHeldRequestsAtTheirStages(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Emit("Network.requestIntercepted", map[string]string{"requestId": "r1"})
	h.f.Emit("Network.responseIntercepted", map[string]string{"requestId": "r2"})
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
	op := h.begin(t, ctx, ClassExclusive)
	defer op.End()
	if err := NetworkReset(ctx, h.s, op.Caller(), NewDesiredState()); err != nil {
		t.Fatalf("NetworkReset: %v", err)
	}
	stages := map[string]any{}
	for _, p := range continues(t, h.f) {
		stages[p["requestId"].(string)] = p["stage"]
	}
	if want := map[string]any{"r1": "request", "r2": "response"}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("continued stages = %v, want %v", stages, want)
	}
	if seqIn(h.f, "Network.interceptContinue") > seqIn(h.f, "Network.disable") {
		t.Fatal("Network.disable was sent before the held requests were continued")
	}
	if p := h.s.PendingRequests(); len(p) != 0 {
		t.Fatalf("pending = %v after the reset, want none", p)
	}
}

// Design 2.10: a completion through the control lane names an open held
// request. The core sends interceptContinue with the stage of the hold, and
// refuses, without sending it, a completion for the other stage: WebKit
// would answer it with "Missing pending intercept ..." while the request
// stays held.
func TestControlCompletionMatchesTheHoldStage(t *testing.T) {
	h := newHarness(t, SchedulerOptions{})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	h.f.Emit("Network.responseIntercepted", map[string]string{"requestId": "r1"})
	h.f.Emit("Network.requestIntercepted", map[string]string{"requestId": "r2"})
	waitUntil(t, "the held requests", func() bool { return len(h.s.PendingRequests()) == 2 })
	msg := func(id, method string, params map[string]any) ControlMsg {
		return ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: id, Method: method, Params: params}
	}
	for _, m := range []ControlMsg{
		msg("r1", "Network.interceptContinue", map[string]any{"stage": "request"}),
		msg("r1", "Network.interceptRequestWithError", map[string]any{"errorType": "Cancellation"}),
		msg("r1", "Network.interceptWithRequest", nil),
		msg("r1", "Network.interceptRequestWithResponse", map[string]any{"content": "", "base64Encoded": false, "mimeType": "text/plain", "status": 200, "statusText": "OK", "headers": map[string]any{}}),
		msg("r2", "Network.interceptContinue", map[string]any{"stage": "response"}),
		msg("r2", "Network.interceptWithResponse", map[string]any{"content": "", "base64Encoded": false}),
	} {
		if _, err := h.s.Control(ctx, m); !errors.Is(err, ErrControlRefused) {
			t.Fatalf("%s %v for %s: err = %v, want ErrControlRefused", m.Method, m.Params, m.RequestID, err)
		}
	}
	if n := len(h.f.Received()); n != 0 {
		for _, r := range h.f.Received() {
			t.Logf("sent %s %s", r.Method, r.Params)
		}
		t.Fatalf("the fake received %d messages; a refused completion must not be sent", n)
	}
	if p := h.s.PendingRequests(); len(p) != 2 {
		t.Fatalf("pending = %v, want r1 and r2 still held", p)
	}
	if _, err := h.s.Control(ctx, msg("r1", "Network.interceptContinue", nil)); err != nil {
		t.Fatalf("continue r1 without a stage: %v", err)
	}
	if _, err := h.s.Control(ctx, msg("r2", "Network.interceptContinue", map[string]any{"stage": "request"})); err != nil {
		t.Fatalf("continue r2 at its stage: %v", err)
	}
	want := []map[string]any{{"requestId": "r1", "stage": "response"}, {"requestId": "r2", "stage": "request"}}
	if got := continues(t, h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("interceptContinue params = %v, want %v", got, want)
	}
}

// sentCall is one command that stageConn sent.
type sentCall struct {
	method string
	params map[string]any
}

// stageConn is a page connection that answers every command with success,
// records it, and delivers events synchronously through emit.
type stageConn struct {
	handlers map[string]webkit.EventHandler
	sent     []sentCall
}

func (c *stageConn) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	c.sent = append(c.sent, sentCall{method, p})
	return json.RawMessage("{}"), nil
}
func (c *stageConn) OnEvent(method string, h webkit.EventHandler) { c.handlers[method] = h }
func (c *stageConn) Subscribe(method string, h webkit.EventHandler) func() {
	c.handlers[method] = h
	return func() {}
}
func (c *stageConn) IsTargetRouted() bool  { return true }
func (c *stageConn) Done() <-chan struct{} { return nil }
func (c *stageConn) emit(method, id string) {
	c.handlers[method](method, json.RawMessage(fmt.Sprintf("{\"requestId\":%q}", id)))
}

// For any interleaving of request-stage and response-stage holds and
// completions through the control lane: a completion of an open hold is
// sent exactly once when it fits the stage of the hold, and then the hold
// is closed; Network.interceptContinue then carries requestId and that
// stage (both required by Network.json). A completion for the other stage
// (an explicit interceptContinue stage, or a method that WebKit takes only
// from its pending requests or only from its pending responses) and a
// completion of an id that is not held are refused, sent nothing, and
// leave the holds as they were.
//
// Oracle (from WebKit's Network.json and InspectorNetworkAgent, not from
// the scheduler): a model of the holds, each at the stage of the latest
// interception event of its id; interceptWithRequest,
// interceptRequestWithResponse and interceptRequestWithError complete
// request-stage holds, interceptWithResponse response-stage holds,
// interceptContinue the stage it names.
func TestControlCompletionStageProperty(t *testing.T) {
	ids := []string{"r1", "r2", "r3"}
	events := []struct{ method, stage string }{
		{"Network.requestIntercepted", "request"},
		{"Network.responseIntercepted", "response"},
	}
	methods := []struct{ name, stage string }{
		{"Network.interceptContinue", ""},
		{"Network.interceptWithRequest", "request"},
		{"Network.interceptWithResponse", "response"},
		{"Network.interceptRequestWithResponse", "request"},
		{"Network.interceptRequestWithError", "request"},
	}
	stageParams := []any{nil, "request", "response"} // nil: no stage parameter
	var sentCount, refusedHeld int                   // generator reach
	prop := func(ops []uint16) bool {
		conn := &stageConn{handlers: map[string]webkit.EventHandler{}}
		s := NewScheduler(conn, SchedulerOptions{})
		defer s.Close()
		s.SetLease(testSession, 1)
		held := map[string]string{}
		for _, op := range ops {
			id := ids[int(op)%len(ids)]
			op /= uint16(len(ids))
			if op%3 == 0 {
				e := events[int(op/3)%len(events)]
				conn.emit(e.method, id)
				held[id] = e.stage
				continue
			}
			op /= 3
			m := methods[int(op)%len(methods)]
			sp := stageParams[int(op/uint16(len(methods)))%len(stageParams)]
			params := map[string]any{}
			if m.name == "Network.interceptContinue" && sp != nil {
				params["stage"] = sp
			}
			want := m.stage
			if m.name == "Network.interceptContinue" {
				want = held[id]
				if sp != nil {
					want = sp.(string)
				}
			}
			before := len(conn.sent)
			_, err := s.Control(context.Background(), ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: id, Method: m.name, Params: params})
			stage, isHeld := held[id]
			if !isHeld || want != stage {
				if isHeld {
					refusedHeld++
				}
				if !errors.Is(err, ErrControlRefused) || len(conn.sent) != before {
					t.Logf("%s %v for %s (held %q): err %v, sent %d; want refused, none sent", m.name, params, id, stage, err, len(conn.sent)-before)
					return false
				}
				continue
			}
			if err != nil || len(conn.sent) != before+1 {
				t.Logf("%s %v for %s held at %q: err %v, sent %d; want one send", m.name, params, id, stage, err, len(conn.sent)-before)
				return false
			}
			got := conn.sent[before]
			if got.method != m.name || got.params["requestId"] != id {
				t.Logf("sent %s %v, want %s for %s", got.method, got.params, m.name, id)
				return false
			}
			if m.name == "Network.interceptContinue" && got.params["stage"] != stage {
				t.Logf("interceptContinue %v for a hold at %q", got.params, stage)
				return false
			}
			sentCount++
			delete(held, id)
		}
		pending := map[string]bool{}
		for _, id := range s.PendingRequests() {
			pending[id] = true
		}
		for id := range held {
			if !pending[id] {
				t.Logf("pending %v, model %v", pending, held)
				return false
			}
		}
		return len(pending) == len(held)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
	if sentCount == 0 || refusedHeld == 0 {
		t.Fatalf("generator reach: %d completions sent, %d refused for the other stage; want both", sentCount, refusedHeld)
	}
}
