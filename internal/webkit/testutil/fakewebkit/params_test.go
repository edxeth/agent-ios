package fakewebkit

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The expected errors are WebKit's: BackendDispatcher::getPropertyValue and
// sendPendingErrors (Source/JavaScriptCore/inspector/InspectorBackendDispatcher.cpp)
// and the generated command preamble (scripts/codegen/cpp_generator_templates.py,
// BackendDispatcherImplementationPrepareCommandArguments); the required
// parameters are those of Network.json.
func TestCheckParamsLikeWebKitDispatcher(t *testing.T) {
	invalid := func(method string, msgs ...string) *Error {
		e := &Error{Code: -32602, Message: "Some arguments of method '" + method + "' can't be processed"}
		for _, m := range msgs {
			e.Data = append(e.Data, Error{Code: -32602, Message: m})
		}
		e.Data = append(e.Data, Error{Code: e.Code, Message: e.Message})
		return e
	}
	unknown := func(msg string) *Error {
		return &Error{Code: -32000, Message: msg, Data: []Error{{Code: -32000, Message: msg}}}
	}
	const (
		cont   = "Network.interceptContinue"
		withRq = "Network.interceptWithRequest"
		withRs = "Network.interceptWithResponse"
		reqRs  = "Network.interceptRequestWithResponse"
		reqErr = "Network.interceptRequestWithError"
	)
	fullReqRs := map[string]any{"requestId": "r1", "content": "", "base64Encoded": false, "mimeType": "text/plain", "status": 200, "statusText": "OK", "headers": map[string]string{}}
	without := func(m map[string]any, key string) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	with := func(m map[string]any, key string, v any) map[string]any {
		out := without(m, key)
		out[key] = v
		return out
	}
	for _, tc := range []struct {
		name   string
		method string
		params any // nil: no params object
		want   *Error
	}{
		{"continue without stage", cont, map[string]any{"requestId": "r1"}, invalid(cont, "Parameter 'stage' with type 'String' was not found.")},
		{"continue without params", cont, nil, invalid(cont,
			"'params' object must contain required parameter 'requestId' with type 'String'.",
			"'params' object must contain required parameter 'stage' with type 'String'.")},
		{"continue with an unknown stage", cont, map[string]any{"requestId": "r1", "stage": "body"}, unknown("Unknown stage: body")},
		{"continue with a stage of the wrong type", cont, map[string]any{"requestId": "r1", "stage": 1}, invalid(cont, "Parameter 'stage' has wrong type. It must be 'String'.")},
		{"continue a request", cont, map[string]any{"requestId": "r1", "stage": "request"}, nil},
		{"continue a response", cont, map[string]any{"requestId": "r1", "stage": "response"}, nil},
		{"replace a request", withRq, map[string]any{"requestId": "r1"}, nil},
		{"replace a request without requestId", withRq, map[string]any{"url": "https://x/"}, invalid(withRq, "Parameter 'requestId' with type 'String' was not found.")},
		{"replace a request with bad headers", withRq, map[string]any{"requestId": "r1", "headers": "x"}, invalid(withRq, "Parameter 'headers' has wrong type. It must be 'Object'.")},
		{"replace a response", withRs, map[string]any{"requestId": "r1", "content": "x", "base64Encoded": false}, nil},
		{"replace a response without base64Encoded", withRs, map[string]any{"requestId": "r1", "content": "x"}, invalid(withRs, "Parameter 'base64Encoded' with type 'Boolean' was not found.")},
		{"replace a response with a string flag", withRs, map[string]any{"requestId": "r1", "content": "x", "base64Encoded": "true"}, invalid(withRs, "Parameter 'base64Encoded' has wrong type. It must be 'Boolean'.")},
		{"fulfill a request", reqRs, fullReqRs, nil},
		{"fulfill a request without headers", reqRs, without(fullReqRs, "headers"), invalid(reqRs, "Parameter 'headers' with type 'Object' was not found.")},
		{"fulfill a request with a string status", reqRs, with(fullReqRs, "status", "200"), invalid(reqRs, "Parameter 'status' has wrong type. It must be 'Integer'.")},
		{"fail a request", reqErr, map[string]any{"requestId": "r1", "errorType": "Cancellation"}, nil},
		{"fail a request without errorType", reqErr, map[string]any{"requestId": "r1"}, invalid(reqErr, "Parameter 'errorType' with type 'String' was not found.")},
		{"fail a request with an unknown errorType", reqErr, map[string]any{"requestId": "r1", "errorType": "Bogus"}, unknown("Unknown errorType: Bogus")},
		{"another method", "Page.reload", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.params != nil {
				raw = mustJSON(tc.params)
			}
			if got := checkParams(tc.method, raw); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("checkParams(%s, %s) = %+v, want %+v", tc.method, raw, got, tc.want)
			}
		})
	}
}

// A completion with a missing parameter gets WebKit's error reply on the
// wire, before any handler runs; a complete one reaches the handler.
func TestInterceptCompletionParamsCheckedOnTheWire(t *testing.T) {
	f := New(Options{})
	defer f.Close()
	handled := 0
	f.Handle("Network.interceptContinue", func(Call) (any, *Error) { handled++; return nil, nil })
	ws, _, err := websocket.DefaultDialer.Dial(f.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	read := func() map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	read() // Target.targetCreated
	send := func(id int, params any) map[string]json.RawMessage {
		t.Helper()
		inner := mustJSON(map[string]any{"id": id, "method": "Network.interceptContinue", "params": params})
		if err := ws.WriteJSON(map[string]any{"id": 100 + id, "method": "Target.sendMessageToTarget", "params": map[string]string{"targetId": "page-1", "message": string(inner)}}); err != nil {
			t.Fatal(err)
		}
		read() // the empty outer ack
		var ev struct {
			Params struct {
				Message string `json:"message"`
			} `json:"params"`
		}
		if err := json.Unmarshal(mustJSON(read()), &ev); err != nil {
			t.Fatal(err)
		}
		var reply map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ev.Params.Message), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	reply := send(1, map[string]string{"requestId": "r1"})
	want := `{"code":-32602,"message":"Some arguments of method 'Network.interceptContinue' can't be processed","data":[{"code":-32602,"message":"Parameter 'stage' with type 'String' was not found."},{"code":-32602,"message":"Some arguments of method 'Network.interceptContinue' can't be processed"}]}`
	if got := string(reply["error"]); got != want {
		t.Fatalf("error = %s, want %s", got, want)
	}
	if handled != 0 {
		t.Fatal("the handler ran for invalid parameters")
	}
	reply = send(2, map[string]string{"requestId": "r1", "stage": "request"})
	if _, isErr := reply["error"]; isErr || handled != 1 {
		t.Fatalf("complete parameters: reply %s, handler ran %d times; want a result from the handler", mustJSON(reply), handled)
	}
}
