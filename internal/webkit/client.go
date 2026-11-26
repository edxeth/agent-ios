package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// EventHandler is a callback for protocol events.
type EventHandler func(method string, params json.RawMessage)

// Client communicates with a WebKit Inspector Protocol endpoint over WebSocket.
// When connected through ios-webkit-debug-proxy, messages are automatically
// wrapped/unwrapped using Target.sendMessageToTarget routing.
type Client struct {
	conn   *websocket.Conn
	wire   *cutConn // the network connection under conn (InterruptWrites)
	url    string
	nextID atomic.Int64
	// writeSem holds one token while a goroutine writes to conn
	// (gorilla/websocket is not concurrent-write-safe). It is a channel, not a
	// mutex, so Send can stop waiting for it when its context ends.
	writeSem chan struct{}
	mu       sync.Mutex
	pending  map[int64]chan *Message

	eventMu        sync.RWMutex
	handlers       map[string][]handlerEntry
	globalHandlers []EventHandler
	nextHandler    int

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error // why the connection ended (Err); set before done closes

	// Target-based routing (used by ios-webkit-debug-proxy)
	targetID    string
	targetReady chan struct{}

	contexts clientContexts // execution contexts per target (ContextInventory)

	// Dialer allows overriding the WebSocket dialer for testing.
	Dialer *websocket.Dialer
}

// TargetWaitTimeout controls how long NewClient waits for a Target.targetCreated
// event before falling back to direct (non-Target) mode. Default is 100ms which
// is enough for local iwdp connections. Increase for slower environments (CI).
var TargetWaitTimeout = 100 * time.Millisecond

// DefaultSendTimeout bounds one Send call when its context has no deadline.
// A page that is stalled, or held by another inspector session, never answers;
// without a bound the caller would wait forever.
var DefaultSendTimeout = 30 * time.Second

// DialTimeout bounds the WebSocket handshake in NewClient. iwdp runs on the
// local machine, so a healthy handshake takes milliseconds; gorilla's default
// of 45 s only delays the error.
var DialTimeout = 5 * time.Second

// ErrHangsThroughTarget is returned by Send for methods that never get a
// response through ios-webkit-debug-proxy Target routing. Sending them would
// also stall every later command on the same connection.
var ErrHangsThroughTarget = errors.New("method hangs through ios-webkit-debug-proxy Target routing and would stall the connection, so it was not sent")

// hangsThroughTarget lists the methods that get no response through iwdp
// Target routing (see docs/webkit-protocol.md, "Known limitations").
var hangsThroughTarget = map[string]bool{
	"CSS.enable":            true,
	"CSS.getAllStyleSheets": true,
	"CSS.getStyleSheetText": true,
}

// NewClient creates a new WebKit Inspector Protocol client connected to the given WebSocket URL.
// If the endpoint uses Target-based routing (ios-webkit-debug-proxy), the client
// automatically wraps/unwraps messages via Target.sendMessageToTarget.
func NewClient(ctx context.Context, wsURL string) (*Client, error) {
	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment, // same as websocket.DefaultDialer
		HandshakeTimeout: DialTimeout,
	}
	return NewClientWithDialer(ctx, wsURL, dialer)
}

// NewClientWithDialer creates a client with a custom WebSocket dialer.
func NewClientWithDialer(ctx context.Context, wsURL string, dialer *websocket.Dialer) (*Client, error) {
	c := &Client{
		url:         wsURL,
		pending:     make(map[int64]chan *Message),
		writeSem:    make(chan struct{}, 1),
		handlers:    make(map[string][]handlerEntry),
		done:        make(chan struct{}),
		targetReady: make(chan struct{}),
		Dialer:      dialer,
	}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	go c.readLoop()

	// Wait briefly for Target.targetCreated — if received, enable Target routing.
	select {
	case <-c.targetReady:
		// Target routing enabled
	case <-time.After(TargetWaitTimeout):
		// No Target event — use direct mode (e.g., mock servers in tests)
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctxErr(ctx)
	}

	return c, nil
}

// maxReadSize is the maximum WebSocket message size the client will accept (512 MB).
// Heap snapshots on heavy pages can be 50-200+ MB, and Target-based routing adds
// ~30% overhead from JSON string escaping, so we need a generous limit.
const maxReadSize = 512 * 1024 * 1024

func (c *Client) connect(ctx context.Context) error {
	conn, wire, resp, err := dialCuttable(ctx, c.Dialer, c.url)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", c.url, deadlineCaused(ctx, err))
	}
	conn.SetReadLimit(maxReadSize)
	c.conn, c.wire = conn, wire
	return nil
}

// InterruptWrites ends the write in progress at once and then fails every
// later Send before it writes anything (ErrNotSent). A write that it
// interrupts may have written part of its message, so its error does not
// wrap ErrNotSent. Without a write in progress it changes nothing. The
// daemon calls it when the liveness probe declares the connection stalled;
// the connection is replaced then and the daemon does not write to it
// again (core pageConn.probe).
func (c *Client) InterruptWrites() { interruptInProgress(c.writeSem, c.wire) }

// Close closes the WebSocket connection.
// Pending Send calls detect closure via the done channel and return an error.
func (c *Client) Close() error { return c.end(ErrSessionClosed) }

// end closes the connection once, with cause as the reason that Err
// returns. It writes nothing to the socket.
func (c *Client) end(cause error) error {
	var err error
	c.closeOnce.Do(func() {
		c.closeErr = cause
		close(c.done)
		if c.conn != nil {
			err = c.conn.Close()
		}
	})
	return err
}

// Err returns why the connection ended, or nil while it is open. After a
// failed socket write its chain holds ErrWriteFailed (retireOn); else it is
// ErrSessionClosed.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.closeErr
	default:
		return nil
	}
}

// retireOn closes the connection when err, the error of a write, says that
// it cannot be written again (a failed WebSocket write, cutConn.failed), so
// Done closes and Err holds the failure's category (retiredBy).
func (c *Client) retireOn(err error) {
	if cause := retiredBy(err); cause != nil {
		_ = c.end(cause)
	}
}

// WaitForTarget waits until the page's target is announced
// (Target.targetCreated), for a connection that must be Target-routed,
// such as every connection to iwdp: NewClient waits for the announcement
// only TargetWaitTimeout and then sends in direct mode, while iwdp on a
// loaded machine can announce the target later. It returns ctx's error
// when ctx ends first, and the connection's error when it closes first.
func (c *Client) WaitForTarget(ctx context.Context) error {
	select {
	case <-c.targetReady:
		return nil
	case <-c.done:
		return fmt.Errorf("waiting for the page's target: %w", c.Err())
	case <-ctx.Done():
		return ctxErr(ctx)
	}
}

// IsTargetRouted returns true if this connection uses iwdp Target-based routing.
func (c *Client) IsTargetRouted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetID != ""
}

// Send sends a method call and waits for the response.
// If Target routing is active, the message is automatically wrapped.
// When ctx has no deadline, Send waits at most DefaultSendTimeout.
func (c *Client) Send(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	// Early check: if already closed, fail fast. Every failure before the
	// write is marked ErrNotSent; one after it is not.
	select {
	case <-c.done:
		if err := c.Err(); errors.Is(err, ErrWriteFailed) {
			return nil, notSent(fmt.Errorf("%s: not sent: %w", method, err))
		}
		return nil, notSent(fmt.Errorf("connection closed"))
	default:
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSendTimeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	start := time.Now()

	innerID := c.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return nil, notSent(fmt.Errorf("marshaling params: %w", err))
		}
	}

	innerMsg := Message{
		ID:     innerID,
		Method: method,
		Params: rawParams,
	}

	// Build the wire message — either wrapped in Target or sent directly.
	// Read targetID under lock to avoid data race with handleTargetCreated.
	c.mu.Lock()
	tid := c.targetID
	c.mu.Unlock()

	if tid != "" && hangsThroughTarget[method] {
		return nil, notSent(&CallError{Method: method, Err: ErrHangsThroughTarget})
	}
	// A call addressed to a target (WithTarget, for example one that
	// carries an execution context id of that target) never goes to the
	// target that replaced it.
	if want, ok := TargetOf(ctx); ok && want != tid {
		return nil, notSent(fmt.Errorf("%s to %s: %w", method, want, ErrTargetSwapped))
	}

	var wireData []byte
	if tid != "" {
		innerData, err := json.Marshal(innerMsg)
		if err != nil {
			return nil, notSent(fmt.Errorf("marshaling inner message: %w", err))
		}
		outerID := c.nextID.Add(1)
		outer := Message{
			ID:     outerID,
			Method: "Target.sendMessageToTarget",
		}
		outerParams, _ := json.Marshal(map[string]string{
			"targetId": tid,
			"message":  string(innerData),
		})
		outer.Params = outerParams
		wireData, err = json.Marshal(outer)
		if err != nil {
			return nil, notSent(fmt.Errorf("marshaling outer message: %w", err))
		}
	} else {
		var err error
		wireData, err = json.Marshal(innerMsg)
		if err != nil {
			return nil, notSent(fmt.Errorf("marshaling message: %w", err))
		}
	}

	ch := make(chan *Message, 1)
	c.mu.Lock()
	c.pending[innerID] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, innerID)
		c.mu.Unlock()
	}()

	// Wait for the socket at most until ctx ends: another command's write may
	// be stalled. The write deadline keeps a stalled socket from blocking this
	// write past ctx. Every failure before WriteMessage wraps ErrNotSent with
	// its text unchanged; a failed WriteMessage may have written part of the
	// message, so it does not.
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return nil, notSent(fmt.Errorf("%s: not sent, another command was still writing after %s: %w", method, time.Since(start).Round(time.Millisecond), ctxErr(ctx)))
	case <-c.done:
		return nil, notSent(fmt.Errorf("connection closed"))
	}
	// select picks at random among ready cases, so ctx can have ended while
	// the token was free; never send a command whose caller has given up.
	if ctx.Err() != nil {
		<-c.writeSem
		return nil, notSent(fmt.Errorf("%s: not sent after %s: %w", method, time.Since(start).Round(time.Millisecond), ctxErr(ctx)))
	}
	if c.wire.cut.Load() {
		<-c.writeSem
		return nil, notSent(fmt.Errorf("%s: not sent: %w", method, errWritesCut))
	}
	// One frame per message (wsframe.go). A failed write may have written
	// part of the message: not marked.
	started, err := c.wire.writeFrame(ctx, opText, wireData, deadline)
	if err != nil && !started {
		<-c.writeSem
		return nil, notSent(fmt.Errorf("%s: writing message: %w", method, deadlineCaused(ctx, err)))
	}
	if err != nil {
		err = c.wire.failed(err) // before the next writer takes the slot
	}
	<-c.writeSem
	if err != nil {
		c.retireOn(err)
		return nil, fmt.Errorf("%s: writing message: %w", method, deadlineCaused(ctx, err))
	}
	MarkWritten(ctx)

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: no response after %s: %w", method, time.Since(start).Round(time.Millisecond), ctxErr(ctx))
	case <-c.done:
		return nil, fmt.Errorf("connection closed")
	case resp := <-ch:
		if resp == nil {
			return nil, fmt.Errorf("connection closed while waiting for response")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// SendWithTimeout sends a method call with a timeout.
func (c *Client) SendWithTimeout(method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Send(ctx, method, params)
}

// handlerEntry is a registered event handler; id lets Subscribe remove it.
type handlerEntry struct {
	id int
	h  EventHandler
}

// Subscribe registers a handler for a specific event method and returns a
// function that removes it. Client handlers run synchronously in the read
// loop, as they always have.
func (c *Client) Subscribe(method string, handler EventHandler) func() {
	c.eventMu.Lock()
	id := c.nextHandler
	c.nextHandler++
	c.handlers[method] = append(c.handlers[method], handlerEntry{id: id, h: handler})
	c.eventMu.Unlock()
	return func() {
		c.eventMu.Lock()
		defer c.eventMu.Unlock()
		hs := c.handlers[method]
		for i, e := range hs {
			if e.id == id {
				c.handlers[method] = append(hs[:i:i], hs[i+1:]...)
				return
			}
		}
	}
}

// OnAnyEvent registers a handler that receives all events.
func (c *Client) OnAnyEvent(handler EventHandler) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.globalHandlers = append(c.globalHandlers, handler)
}

func (c *Client) readLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// Connection closed or error — stop reading
			c.closeOnce.Do(func() {
				c.closeErr = ErrSessionClosed
				close(c.done)
			})
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Handle Target-based routing from iwdp
		switch msg.Method {
		case "Target.targetCreated":
			c.handleTargetCreated(msg.Params)
			continue
		case "Target.dispatchMessageFromTarget":
			c.handleDispatchFromTarget(msg.Params)
			continue
		}

		if msg.ID > 0 {
			// Response to a request
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			c.mu.Unlock()
			if ok {
				select {
				case ch <- &msg:
				case <-c.done:
				}
			}
		} else if msg.Method != "" {
			// Event
			c.contexts.track("", msg.Method, msg.Params)
			c.dispatchEvent(msg.Method, msg.Params)
		}
	}
}

func (c *Client) handleTargetCreated(params json.RawMessage) {
	var p struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.TargetInfo.TargetID != "" && p.TargetInfo.Type == "page" {
		c.mu.Lock()
		old := c.targetID
		c.targetID = p.TargetInfo.TargetID
		c.mu.Unlock()
		if old != p.TargetInfo.TargetID {
			c.contexts.drop(old) // commands no longer go there
		}
		select {
		case <-c.targetReady:
			// already closed
		default:
			close(c.targetReady)
		}
	}
}

func (c *Client) handleDispatchFromTarget(params json.RawMessage) {
	var p struct {
		TargetID string `json:"targetId"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}

	var innerMsg Message
	if err := json.Unmarshal([]byte(p.Message), &innerMsg); err != nil {
		return
	}

	if innerMsg.ID > 0 {
		// Response to a request — route by inner ID
		c.mu.Lock()
		ch, ok := c.pending[innerMsg.ID]
		c.mu.Unlock()
		if ok {
			select {
			case ch <- &innerMsg:
			case <-c.done:
			}
		}
	} else if innerMsg.Method != "" {
		// Event from the target (e.g., Console.messageAdded, Network.requestWillBeSent)
		c.contexts.track(p.TargetID, innerMsg.Method, innerMsg.Params)
		c.dispatchEvent(innerMsg.Method, innerMsg.Params)
	}
}

func (c *Client) dispatchEvent(method string, params json.RawMessage) {
	c.eventMu.RLock()
	// Copy slices so handlers can safely call Subscribe/OnAnyEvent without deadlock or race.
	handlers := make([]handlerEntry, len(c.handlers[method]))
	copy(handlers, c.handlers[method])
	globals := make([]EventHandler, len(c.globalHandlers))
	copy(globals, c.globalHandlers)
	c.eventMu.RUnlock()

	for _, e := range handlers {
		e.h(method, params)
	}
	for _, h := range globals {
		h(method, params)
	}
}

// Done returns a channel that is closed when the connection is closed.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// URL returns the WebSocket URL this client is connected to.
func (c *Client) URL() string {
	return c.url
}
