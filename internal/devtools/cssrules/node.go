// Package cssrules reads the CSS of one element through the WebKit CSS
// domain's node methods, for the css matched and css computed commands
// (internal/commands/css.go) and the CDP endpoint's CSS domain
// (internal/cdp/css_domain.go).
//
// Only CSS.getMatchedStylesForNode, CSS.getInlineStylesForNode and
// CSS.getComputedStyleForNode are sent, each under CallDeadline. They
// answer without CSS.enable, which is never sent: through
// ios-webkit-debug-proxy CSS.enable, CSS.getAllStyleSheets and
// CSS.getStyleSheetText hang and stall the page connection
// (docs/webkit-protocol.md).
//
// A WIP node id is valid on one target and for one document of the
// connection: a provisional commit replaces the target, a navigation
// replaces the document, and DOM.getDocument forgets the ids given
// before. A Node therefore sends every call to the target that gave its
// id (webkit.WithTarget, which fails a call to a replaced target) and
// fails its calls once the connection reported DOM.documentUpdated. The
// callers run the sequence in an operation that runs alone on the page
// (core.ClassMutate or ClassExclusive), so no other work of the daemon
// sends DOM.getDocument in between.
package cssrules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// CallDeadline bounds each CSS node call: on a healthy page it answers in
// milliseconds (about 42 ms on the iOS 26.5 simulator), so a missing reply
// is reported as such long before the command deadline.
var CallDeadline = 5 * time.Second

// ErrDocumentChanged: the page's document changed (DOM.documentUpdated)
// after the node id was given, so the id may name another node.
var ErrDocumentChanged = errors.New("the page's document changed while its styles were read; run the command again")

// groupSeq numbers the object groups of this process.
var groupSeq atomic.Uint64

// ObjectGroup returns a remote object group of its own for one read: a
// read releases only the objects it made, never another read's.
func ObjectGroup() string {
	return fmt.Sprintf("agent-ios-css-%d", groupSeq.Add(1))
}

// ActiveTarget returns the target of c that receives commands now, or ""
// when c does not say (a connection without targets).
func ActiveTarget(c webkit.Caller) string {
	if a, ok := c.(interface{ ActiveTarget() string }); ok {
		return a.ActiveTarget()
	}
	return ""
}

// Pin addresses ctx's sends to target; "" leaves them as they are.
func Pin(ctx context.Context, target string) context.Context {
	if target == "" {
		return ctx
	}
	return webkit.WithTarget(ctx, target)
}

// wipNoDocument is WIP's refusal to push a node before the client asked
// for the document (WebKit InspectorDOMAgent::pushNodePathToFrontend).
const wipNoDocument = "Document must have been requested"

// PushRefused reports whether err is WIP's refusal to push a node for
// want of the document: wipNoDocument, or the same refusal with an empty
// message, which is what WebKitGTK 2.54 sends (pushNodePathToFrontend
// takes its error string by value; webkit.Session reports an error reply
// without a message as malformed, with the reply quoted).
func PushRefused(err error) bool {
	s := err.Error()
	return strings.Contains(s, wipNoDocument) || strings.Contains(s, "malformed error reply") && strings.Contains(s, `\"message\":\"\"`)
}

// Conn is where a Node's commands go.
type Conn struct {
	// C sends the commands (an operation's fenced caller).
	C webkit.Caller
	// Watch reports DOM.documentUpdated; nil: C. A caller with
	// SubscribeSync (the page's connection) reports it before the reply
	// that follows the event, so a read never uses such a reply.
	Watch webkit.Caller
	// Target receives every command ("" = the connection's own routing).
	Target string
}

// Node is the WIP node id of one element, pinned to the target that gave
// it and to the document it was given for.
type Node struct {
	ID     int
	c      webkit.Caller
	target string
	stale  atomic.Bool
	unsub  func()
}

// Open maps the remote object objectID to its WIP node with DOM.requestNode
// on conn. WIP pushes a
// node only after DOM.getDocument on its connection, and DOM.getDocument
// forgets the node ids given before (also those of other clients of the
// page), so DOM.getDocument is sent only after WIP refused the push for
// that reason (PushRefused). The Node watches DOM.documentUpdated from
// before its request; Close ends the watch.
func Open(ctx context.Context, conn Conn, objectID string) (*Node, error) {
	n := &Node{c: conn.C, target: conn.Target}
	mark := func(string, json.RawMessage) { n.stale.Store(true) }
	w := conn.Watch
	if w == nil {
		w = conn.C
	}
	if s, ok := w.(interface {
		SubscribeSync(method string, h webkit.EventHandler) func()
	}); ok {
		n.unsub = s.SubscribeSync("DOM.documentUpdated", mark)
	} else {
		n.unsub = w.Subscribe("DOM.documentUpdated", mark)
	}
	req := map[string]any{"objectId": objectID}
	raw, err := n.Send(ctx, "DOM.requestNode", req)
	if err != nil && PushRefused(err) {
		if _, err = n.Send(ctx, "DOM.getDocument", nil); err == nil {
			// Our own DOM.getDocument replaced the ids; the watch starts
			// from here.
			n.stale.Store(false)
			raw, err = n.Send(ctx, "DOM.requestNode", req)
		}
	}
	if err != nil {
		n.Close()
		return nil, err
	}
	var res struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.NodeID == 0 {
		n.Close()
		return nil, fmt.Errorf("DOM.requestNode gave no node id: %s", raw)
	}
	n.ID = res.NodeID
	return n, nil
}

// Close ends the document watch.
func (n *Node) Close() {
	if n.unsub != nil {
		n.unsub()
		n.unsub = nil
	}
}

// Target is the target the node's sends go to ("" = not pinned).
func (n *Node) Target() string { return n.target }

// Send sends one command to the node's target.
func (n *Node) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return n.c.Send(Pin(ctx, n.target), method, params)
}

// Call sends the CSS node method with the node's id and params under
// CallDeadline. It fails, without sending, once the document changed, and
// a reply that came after a document change is not used.
func (n *Node) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if n.stale.Load() {
		return nil, fmt.Errorf("%s: %w", method, ErrDocumentChanged)
	}
	p := map[string]any{"nodeId": n.ID}
	for k, v := range params {
		p[k] = v
	}
	cctx, cancel := context.WithTimeout(ctx, CallDeadline)
	defer cancel()
	raw, err := n.Send(cctx, method, p)
	if err != nil {
		if cctx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("%s got no reply within %s; the page's CSS domain did not answer", method, CallDeadline)
		}
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if n.stale.Load() {
		return nil, fmt.Errorf("%s: %w", method, ErrDocumentChanged)
	}
	return raw, nil
}

// CallInto is Call that decodes the reply into out.
func (n *Node) CallInto(ctx context.Context, method string, params map[string]any, out any) error {
	raw, err := n.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decoding the reply: %w", method, err)
	}
	return nil
}

// ReleaseBudget bounds the release of a read's object group.
const ReleaseBudget = 2 * time.Second

// Release frees the object group on c, sent to target, within
// ReleaseBudget, also when ctx already ended: c must be a caller that
// still sends then (core Op.CleanupCaller, fenced by the lease). The
// error is returned for the caller's log.
func Release(ctx context.Context, c webkit.Caller, target, group string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseBudget)
	defer cancel()
	_, err := c.Send(Pin(rctx, target), "Runtime.releaseObjectGroup", map[string]any{"objectGroup": group})
	return err
}
