// Package cdptest is a small CDP client for tests of the CDP endpoint
// (design docs/design/agentic-parity.md, section 8.1: "CDP client
// harness"). It speaks flat sessions only, records every event, and lets a
// test wait for an event or check their order.
package cdptest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event is one CDP event the client received.
type Event struct {
	Seq       int
	Method    string
	SessionID string
	Params    json.RawMessage
}

// Error is a CDP error reply.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message) }

type message struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *Error          `json:"error"`

	seq int // the client's receive order
}

// Client is one CDP WebSocket connection.
type Client struct {
	ws  *websocket.Conn
	wmu sync.Mutex

	mu      sync.Mutex
	next    int64
	pending map[int64]chan message
	events  []Event
	seq     int
	changed chan struct{}
	done    chan struct{}
	err     error
}

// Dial connects to a CDP WebSocket URL. header may be nil.
func Dial(ctx context.Context, url string, header http.Header) (*Client, *http.Response, error) {
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := d.DialContext(ctx, url, header)
	if err != nil {
		return nil, resp, err
	}
	c := &Client{ws: ws, pending: map[int64]chan message{}, changed: make(chan struct{}), done: make(chan struct{})}
	go c.read()
	return c, resp, nil
}

func (c *Client) read() {
	defer close(c.done)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.err = err
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = map[int64]chan message{}
			c.notifyLocked()
			c.mu.Unlock()
			return
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		c.mu.Lock()
		if m.Method != "" {
			c.seq++
			c.events = append(c.events, Event{Seq: c.seq, Method: m.Method, SessionID: m.SessionID, Params: m.Params})
			c.notifyLocked()
		} else if ch, ok := c.pending[m.ID]; ok {
			delete(c.pending, m.ID)
			c.seq++
			m.seq = c.seq
			ch <- m
			close(ch)
		}
		c.mu.Unlock()
	}
}

func (c *Client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Call sends a command on a session ("" = the browser session) and waits
// for its reply. A CDP error reply is returned as *Error.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	res, _, err := c.CallSeq(ctx, sessionID, method, params)
	return res, err
}

// CallSeq is Call that also returns the receive sequence number of the
// reply, to compare with Event.Seq (events before or after a reply).
func (c *Client) CallSeq(ctx context.Context, sessionID, method string, params any) (json.RawMessage, int, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, 0, fmt.Errorf("connection closed: %w", c.err)
	}
	c.next++
	id := c.next
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	out := map[string]any{"id": id, "method": method}
	if params != nil {
		out["params"] = params
	}
	if sessionID != "" {
		out["sessionId"] = sessionID
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	c.wmu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, data)
	c.wmu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, 0, fmt.Errorf("%s: connection closed before the reply", method)
		}
		if m.Error != nil {
			return nil, m.seq, m.Error
		}
		return m.Result, m.seq, nil
	case <-ctx.Done():
		return nil, 0, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Send writes a raw message without waiting (for malformed-input tests).
func (c *Client) Send(raw []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, raw)
}

// Events returns the events received so far.
func (c *Client) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

// WaitEvent waits for the first event of method (after the seq'th
// event) for which match holds (match nil: any).
func (c *Client) WaitEvent(ctx context.Context, after int, method string, match func(Event) bool) (Event, error) {
	for {
		c.mu.Lock()
		for _, e := range c.events {
			if e.Seq > after && e.Method == method && (match == nil || match(e)) {
				c.mu.Unlock()
				return e, nil
			}
		}
		ch, err := c.changed, c.err
		c.mu.Unlock()
		if err != nil {
			return Event{}, fmt.Errorf("waiting for %s: connection closed: %w", method, err)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return Event{}, fmt.Errorf("waiting for %s: %w", method, ctx.Err())
		}
	}
}

// Seq is the sequence number of the last event or reply received.
func (c *Client) Seq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close closes the connection and waits for the reader.
func (c *Client) Close() {
	_ = c.ws.Close()
	<-c.done
}
