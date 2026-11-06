package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Handler is a function that handles an incoming WebKit protocol message
// and returns the result or an error.
type Handler func(method string, params json.RawMessage) (interface{}, error)

// mockConn wraps a WebSocket connection with a write mutex to prevent
// concurrent writes from multiple goroutines (e.g., event sending + response sending).
type mockConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (mc *mockConn) writeMessage(messageType int, data []byte) error {
	mc.writeMu.Lock()
	defer mc.writeMu.Unlock()
	return mc.conn.WriteMessage(messageType, data)
}

// MockServer simulates a WebKit Inspector Protocol endpoint.
//
// By default it answers in direct mode, like a plain WebKit endpoint. Call
// EnableTargetRouting before a client connects to simulate
// ios-webkit-debug-proxy instead: the server then announces a page target and
// expects every command wrapped in Target.sendMessageToTarget.
type MockServer struct {
	Server   *httptest.Server
	URL      string // WebSocket URL (ws://...)
	mu       sync.Mutex
	handlers map[string]Handler
	conns    []*mockConn

	silent      map[string]bool // methods that never get a response
	received    []string        // methods received, unwrapped when Target-routed
	connections int             // WebSocket connections accepted so far
	targetID    string          // non-empty: simulate iwdp Target routing
}

// NewMockServer creates and starts a mock WebSocket server.
func NewMockServer() *MockServer {
	m := &MockServer{
		handlers: make(map[string]Handler),
		silent:   make(map[string]bool),
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mc := &mockConn{conn: conn}
		m.mu.Lock()
		m.conns = append(m.conns, mc)
		m.connections++
		targetID := m.targetID
		m.mu.Unlock()

		if targetID != "" {
			created, _ := json.Marshal(map[string]interface{}{
				"method": "Target.targetCreated",
				"params": map[string]interface{}{
					"targetInfo": map[string]interface{}{"targetId": targetID, "type": "page"},
				},
			})
			_ = mc.writeMessage(websocket.TextMessage, created)
		}

		go m.handleConn(mc)
	}))

	m.URL = "ws" + strings.TrimPrefix(m.Server.URL, "http")
	return m
}

// Handle registers a handler for a specific method.
func (m *MockServer) Handle(method string, handler Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[method] = handler
}

// NeverRespond makes the server read the given method and never answer it,
// like a page that is stalled or held by another inspector session.
func (m *MockServer) NeverRespond(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.silent[method] = true
}

// EnableTargetRouting makes the server behave like ios-webkit-debug-proxy for
// connections accepted after the call: it sends Target.targetCreated with the
// given targetID on connect, unwraps Target.sendMessageToTarget commands, and
// answers them through Target.dispatchMessageFromTarget.
func (m *MockServer) EnableTargetRouting(targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targetID = targetID
}

// ReceivedMethods returns the methods received so far, in order. Commands
// wrapped in Target.sendMessageToTarget are listed by their inner method.
func (m *MockServer) ReceivedMethods() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.received))
	copy(out, m.received)
	return out
}

// ConnectionCount returns how many WebSocket connections the server accepted.
func (m *MockServer) ConnectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connections
}

// HandleFunc registers a simple handler that returns a fixed result.
func (m *MockServer) HandleFunc(method string, result interface{}) {
	m.Handle(method, func(_ string, _ json.RawMessage) (interface{}, error) {
		return result, nil
	})
}

// SendEvent sends an event to all connected clients.
func (m *MockServer) SendEvent(method string, params interface{}) error {
	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}

	msg := struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
	}{
		Method: method,
		Params: rawParams,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.targetID != "" {
		data, err = json.Marshal(map[string]interface{}{
			"method": "Target.dispatchMessageFromTarget",
			"params": map[string]string{"targetId": m.targetID, "message": string(data)},
		})
		if err != nil {
			return err
		}
	}
	for _, mc := range m.conns {
		_ = mc.writeMessage(websocket.TextMessage, data)
	}
	return nil
}

// Close shuts down the mock server.
func (m *MockServer) Close() {
	m.mu.Lock()
	for _, mc := range m.conns {
		_ = mc.conn.Close()
	}
	m.conns = nil
	m.mu.Unlock()
	m.Server.Close()
}

func (m *MockServer) handleConn(mc *mockConn) {
	defer func() { _ = mc.conn.Close() }()
	for {
		_, data, err := mc.conn.ReadMessage()
		if err != nil {
			return
		}

		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Method == "Target.sendMessageToTarget" {
			m.handleTargetMessage(mc, msg.ID, msg.Params)
			continue
		}

		if resp, ok := m.respond(msg.ID, msg.Method, msg.Params); ok {
			_ = mc.writeMessage(websocket.TextMessage, resp)
		}
	}
}

// handleTargetMessage acknowledges a Target.sendMessageToTarget command and
// answers its inner command through Target.dispatchMessageFromTarget, as iwdp does.
func (m *MockServer) handleTargetMessage(mc *mockConn, outerID int64, params json.RawMessage) {
	var outer struct {
		TargetID string `json:"targetId"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(params, &outer); err != nil {
		return
	}
	ack, _ := json.Marshal(map[string]interface{}{"id": outerID, "result": json.RawMessage("{}")})
	_ = mc.writeMessage(websocket.TextMessage, ack)

	var inner struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(outer.Message), &inner); err != nil {
		return
	}
	resp, ok := m.respond(inner.ID, inner.Method, inner.Params)
	if !ok {
		return
	}
	wrapped, _ := json.Marshal(map[string]interface{}{
		"method": "Target.dispatchMessageFromTarget",
		"params": map[string]string{"targetId": outer.TargetID, "message": string(resp)},
	})
	_ = mc.writeMessage(websocket.TextMessage, wrapped)
}

// respond records the method and builds its response. It returns false when
// the method is registered with NeverRespond.
func (m *MockServer) respond(id int64, method string, params json.RawMessage) ([]byte, bool) {
	m.mu.Lock()
	m.received = append(m.received, method)
	handler, ok := m.handlers[method]
	silent := m.silent[method]
	m.mu.Unlock()

	if silent {
		return nil, false
	}

	var resp []byte
	if ok {
		result, herr := handler(method, params)
		if herr != nil {
			resp, _ = json.Marshal(map[string]interface{}{
				"id": id,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": herr.Error(),
				},
			})
		} else {
			resultJSON, _ := json.Marshal(result)
			resp, _ = json.Marshal(map[string]interface{}{
				"id":     id,
				"result": json.RawMessage(resultJSON),
			})
		}
	} else {
		// Default: return empty result
		resp, _ = json.Marshal(map[string]interface{}{
			"id":     id,
			"result": json.RawMessage("{}"),
		})
	}
	return resp, true
}
