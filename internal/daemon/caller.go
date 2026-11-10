package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/logcause"
	"github.com/nnemirovsky/iwdp-mcp/internal/logsafe"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// subscribeTimeout bounds the start of an event stream; Subscribe has no
// context of its own.
const subscribeTimeout = 10 * time.Second

// deadlineSkew bounds the wait for the caller's deadline after the daemon
// reported that the same deadline ended a call.
const deadlineSkew = 250 * time.Millisecond

// PageOptions select a page and the caller's ownership options.
type PageOptions struct {
	Page  PageReq
	Owner Owner
	Name  string // operation name in the daemon's status; "" = "legacy"
}

// PageCaller is a webkit.Caller whose calls run in the daemon (the legacy
// bridge of design 2.10). Calls run inside the caller's current operation:
// the fixed operation that OpenPage begins (a CLI command), or else a
// one-call operation each.
type PageCaller struct {
	c    *Client
	o    PageOptions
	info PageInfo

	mu     sync.Mutex
	hold   *hold // the fixed operation, until EndOp or its end
	subs   map[*eventStream]struct{}
	closed bool
	endErr error // why the page ended for this caller (Err)

	done     chan struct{}
	doneOnce sync.Once
}

var _ webkit.Caller = (*PageCaller)(nil)

// OpenPage resolves and leases the page and begins one exclusive operation
// that lasts until EndOp, Close or ctx's deadline (a CLI command).
func (c *Client) OpenPage(ctx context.Context, o PageOptions) (*PageCaller, error) {
	h, err := c.begin(ctx, o)
	if err != nil {
		return nil, err
	}
	p := newPageCaller(c, o, h.op.Page)
	p.hold = h
	go p.watch(h)
	return p, nil
}

// AttachPage resolves and leases the page without an operation (CLI
// commands that send only control-lane messages); calls then run one at a
// time. It does not queue behind a running operation of the session.
func (c *Client) AttachPage(ctx context.Context, o PageOptions) (*PageCaller, error) {
	var out struct {
		Page  *PageInfo   `json:"page"`
		Error *ErrorReply `json:"error"`
	}
	req := LeaseRequest{Op: "acquire", PageReq: o.Page, Owner: o.Owner}
	if dl, ok := ctx.Deadline(); ok {
		req.TimeoutMs = max(ms(time.Until(dl)), 1)
	}
	if err := c.call(ctx, http.MethodPost, "/v1/lease", req, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, &RemoteError{*out.Error}
	}
	if out.Page == nil {
		return nil, errors.New("daemon /v1/lease: no page in the answer")
	}
	return newPageCaller(c, o, *out.Page), nil
}

func newPageCaller(c *Client, o PageOptions, info PageInfo) *PageCaller {
	return &PageCaller{c: c, o: o, info: info, subs: map[*eventStream]struct{}{}, done: make(chan struct{})}
}

// Info describes the page.
func (p *PageCaller) Info() PageInfo { return p.info }

// PageReq is the page request the caller was opened with.
func (p *PageCaller) PageReq() PageReq { return p.o.Page }

// IsTargetRouted reports whether the page connection uses Target routing.
func (p *PageCaller) IsTargetRouted() bool { return p.info.Routed }

// Done is closed when the page connection ends or the caller is closed.
func (p *PageCaller) Done() <-chan struct{} { return p.done }

func (p *PageCaller) finish() { p.doneOnce.Do(func() { close(p.done) }) }

// lose records why the page ended (the first reason wins) and closes Done.
func (p *PageCaller) lose(err error) {
	p.mu.Lock()
	if p.endErr == nil {
		p.endErr = err
	}
	p.mu.Unlock()
	p.finish()
}

// Err says why Done closed because the page ended (the page connection was
// lost, the lease ended, the daemon shut down or its connection ended); nil
// while the page lives or after Close.
func (p *PageCaller) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endErr
}

// watch closes Done when the fixed operation reports that the page
// connection ended.
func (p *PageCaller) watch(h *hold) {
	<-h.ended
	if h.lost() {
		p.lose(h.err)
	}
}

// EndOp ends the fixed operation; later calls run one at a time. A
// streaming command calls it after its enable step, so a long stream does
// not block the session's other commands.
func (p *PageCaller) EndOp() {
	p.mu.Lock()
	h := p.hold
	p.hold = nil
	p.mu.Unlock()
	if h != nil {
		h.end(p.c)
	}
}

// Close ends the operation and every event stream.
func (p *PageCaller) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	subs := p.subs
	p.subs = nil
	p.mu.Unlock()
	p.EndOp()
	for s := range subs {
		s.stop()
	}
	p.finish()
	p.c.Close() // idle socket connections; the client stays usable
	return nil
}

// Send sends one protocol call through the daemon. A deadline of ctx is
// the call's deadline; without one the call gets webkit.DefaultSendTimeout,
// as with a direct connection.
func (p *PageCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, webkit.DefaultSendTimeout)
		defer cancel()
	}
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("%s: encoding params: %w", method, err)
		}
		raw = data
	}
	_, control := core.ControlKindOf(method)
	opID, err := p.opFor()
	if err != nil {
		return nil, withMethod(method, err)
	}
	req := RawRequest{OpID: opID, Method: method, Params: raw, PageReq: p.o.Page, Owner: p.o.Owner}
	if control && opID == "" {
		// Outside an operation a control message carries the lease epoch
		// that this caller acquired (design 2.10 fencing).
		req.Key, req.Epoch = p.info.Key, p.info.Epoch
	}
	res, err := p.c.Raw(ctx, req)
	if err != nil {
		return nil, withMethod(method, atDeadline(ctx, err))
	}
	return res, nil
}

// atDeadline aligns a failure that the daemon reports within deadlineSkew
// of ctx's deadline with the caller's view: the daemon's copy of the
// deadline (or the operation's) can end the call a few milliseconds early.
// It waits for ctx and, when ctx's deadline has then passed, returns a
// deadline error (exit 124), so callers that check ctx.Err() agree.
func atDeadline(ctx context.Context, err error) error {
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > deadlineSkew {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(dl) + startPoll):
	}
	return wrapCtx(ctx, err)
}

// withMethod names the method in an error that does not name it yet.
func withMethod(method string, err error) error {
	if strings.Contains(err.Error(), method) {
		return err
	}
	return fmt.Errorf("%s: %w", method, err)
}

// opFor returns the operation a call runs in: the fixed one while it is
// open, or "" for a one-call operation. A control-lane message (design
// 2.10) never begins an operation: it must not wait behind the operation it
// lets finish. It keeps the authority of an operation that is open (the
// daemon refuses it when that operation ended), and otherwise carries the
// caller's lease epoch ("").
func (p *PageCaller) opFor() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hold != nil {
		select {
		case <-p.hold.ended:
			if p.hold.lost() {
				return "", p.hold.err
			}
			p.hold = nil // ended at its deadline: later calls run one at a time
		default:
			return p.hold.op.OpID, nil
		}
	}
	return "", nil
}

// OnEvent registers h for the life of the caller.
func (p *PageCaller) OnEvent(method string, h webkit.EventHandler) { p.Subscribe(method, h) }

// Subscribe streams method events of the page from the daemon to h and
// returns once the daemon has subscribed, so no event after this call is
// missed. A stream that ends because the page connection ended closes
// Done; a stream that cannot start closes Done too, since the caller could
// not see the page's events.
func (p *PageCaller) Subscribe(method string, h webkit.EventHandler) func() {
	s, err := p.c.events(p.o, method, h)
	if err != nil {
		p.c.logf("subscribing to %s: %v", logsafe.ID(method), logsafe.Error(err))
		p.finish()
		return func() {}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		s.stop()
		return func() {}
	}
	p.subs[s] = struct{}{}
	p.mu.Unlock()
	go func() {
		<-s.ended
		if s.lost {
			p.lose(s.err)
		}
	}()
	return func() {
		p.mu.Lock()
		delete(p.subs, s)
		p.mu.Unlock()
		s.stop()
	}
}

// ---- operation holds ----

// hold is an open operation: the begin stream stays open until the client
// ends it, and the daemon ends the operation when the stream closes.
type hold struct {
	op     OpReply
	resp   *http.Response
	cancel context.CancelFunc
	ended  chan struct{}
	mu     sync.Mutex
	err    error // set when the page connection ended (a "closed" item)
}

func (h *hold) lost() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err != nil
}

func (c *Client) begin(ctx context.Context, o PageOptions) (*hold, error) {
	req := OpRequest{Op: "begin", Name: o.Name, Class: "exclusive", PageReq: o.Page, Owner: o.Owner}
	if dl, ok := ctx.Deadline(); ok {
		req.TimeoutMs = max(ms(time.Until(dl)), 1)
	}
	// The stream outlives ctx; ctx bounds only the wait for the first item.
	sctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	resp, err := c.request(sctx, http.MethodPost, "/v1/op", req)
	if err != nil {
		stop()
		cancel()
		return nil, wrapCtx(ctx, err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxItem)
	var it Item
	ok := sc.Scan()
	if ok {
		err = json.Unmarshal(sc.Bytes(), &it)
	} else {
		err = sc.Err()
	}
	if !stop() || err != nil || !ok || it.Error != nil || it.Op == nil {
		cancel()
		_ = resp.Body.Close()
		switch {
		case it.Error != nil:
			return nil, &RemoteError{*it.Error}
		case ctx.Err() != nil:
			return nil, wrapCtx(ctx, fmt.Errorf("opening an operation on the page"))
		case err == nil:
			err = errors.New("no operation in the answer")
		}
		return nil, fmt.Errorf("daemon /v1/op: %w", err)
	}
	h := &hold{op: *it.Op, resp: resp, cancel: cancel, ended: make(chan struct{})}
	go func() {
		defer close(h.ended)
		for sc.Scan() {
			var it Item
			if json.Unmarshal(sc.Bytes(), &it) == nil && it.Type == "closed" && it.Error != nil {
				h.mu.Lock()
				h.err = &RemoteError{*it.Error}
				h.mu.Unlock()
			}
		}
	}()
	return h, nil
}

// end ends the operation in the daemon, then closes the stream. The
// stream's context ends the reader goroutine; the body is closed only after
// that goroutine returned, because net/http can block a Read that ends at
// EOF forever when Close runs during that Read (the read loop of the
// connection has moved on to the next response by then).
func (h *hold) end(c *Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ok map[string]bool
	_ = c.call(ctx, http.MethodPost, "/v1/op", OpRequest{Op: "end", OpID: h.op.OpID}, &ok)
	h.cancel()
	<-h.ended
	_ = h.resp.Body.Close()
}

// ---- event streams ----

type eventStream struct {
	cancel context.CancelFunc
	ended  chan struct{}
	lost   bool  // the page ended for this stream (set before ended closes)
	err    error // the reason of lost
}

func (s *eventStream) stop() {
	s.cancel()
	<-s.ended
}

func (c *Client) events(o PageOptions, method string, h webkit.EventHandler) (*eventStream, error) {
	q := url.Values{}
	q.Set("methods", method)
	q.Set("session", o.Owner.Session)
	q.Set("reservation", o.Owner.Reservation)
	q.Set("transport", o.Page.Transport)
	q.Set("addr", o.Page.Addr)
	q.Set("page", o.Page.Page)
	sctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(subscribeTimeout, cancel)
	resp, err := c.request(sctx, http.MethodGet, "/v1/events?"+q.Encode(), nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxItem)
	var it Item
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &it) != nil || it.Type != "subscribed" || !timer.Stop() {
		cancel()
		_ = resp.Body.Close()
		if it.Error != nil {
			return nil, &RemoteError{*it.Error}
		}
		return nil, logcause.Errorf("daemon /v1/events: the stream did not start (%v)", sc.Err())
	}
	s := &eventStream{cancel: cancel, ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		defer func() { _ = resp.Body.Close() }()
		defer func() {
			// No final item and not stopped by the client: the daemon
			// died or its connection broke, so the page is gone for this
			// client (design 2.6).
			if !s.lost && sctx.Err() == nil {
				s.lost = true
				s.err = logcause.Errorf("the connection to the iwdp daemon ended (%v); select the page again", errOrEOF(sc.Err()))
			}
		}()
		for sc.Scan() {
			var it Item
			if json.Unmarshal(sc.Bytes(), &it) != nil {
				continue
			}
			switch it.Type {
			case "event":
				h(it.Method, it.Params)
			case "closed":
				s.lost = true
				s.err = errors.New("the daemon ended the event stream")
				if it.Error != nil {
					s.err = &RemoteError{*it.Error}
				}
				return
			}
		}
	}()
	return s, nil
}

func errOrEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

// logf reports a client-side problem that has no caller to return it to.
func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}
