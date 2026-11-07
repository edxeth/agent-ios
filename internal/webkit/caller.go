package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Caller is what every tool, the runtime layer and the daemon use to talk to
// one page (design docs/design/agentic-parity.md, section 2.9). Session and
// the legacy Client implement it.
type Caller interface {
	// Send sends one protocol command and waits for its reply.
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
	// OnEvent registers a handler for the life of the connection. It is kept
	// for the existing collectors; new code uses Subscribe.
	OnEvent(method string, h EventHandler)
	// Subscribe registers a handler and returns a function that removes it.
	Subscribe(method string, h EventHandler) func()
	IsTargetRouted() bool
	// Done is closed when the connection ends.
	Done() <-chan struct{}
}

// ctxErr is the error of an ended ctx with its cause. A context canceled
// with a cause (context.WithCancelCause; core cancels an operation at its
// deadline with core.ErrDeadline, a deadline error) reports context.Canceled
// and the cause, so that callers can tell an expired deadline (exit 124,
// decision D9) from a cancellation.
func ctxErr(ctx context.Context) error {
	err := ctx.Err()
	if cause := context.Cause(ctx); cause != nil && cause != err {
		return fmt.Errorf("%w: %w", err, cause)
	}
	return err
}

// deadlineCaused returns err with ctx's end in its chain when ctx's
// deadline caused err: err is the timeout of a socket write or read whose
// deadline came from ctx, and ctx's deadline has passed. Such a failure is
// the deadline (context.DeadlineExceeded with its cause, exit 124, decision
// D9), not a wire failure. The socket's deadline and ctx's timer expire at
// the same instant, so ctx can look live for a moment after the socket's
// timeout; the function then waits for ctx to end, which happens at once.
// Any other error, and a timeout while ctx's deadline is still ahead (a
// shorter call budget, a stalled socket), is returned unchanged.
func deadlineCaused(ctx context.Context, err error) error {
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
		return err
	}
	if dl, ok := ctx.Deadline(); !ok || time.Now().Before(dl) {
		return err
	}
	<-ctx.Done()
	return fmt.Errorf("%w: %w", ctxErr(ctx), err)
}

// AllEvents subscribes a handler to every event of a Session.
const AllEvents = "*"

// TargetEventHandler is a callback for protocol events that also receives
// the id of the target that sent the event: the targetId of the
// Target.dispatchMessageFromTarget that carried it. A provisional target
// keeps its id when it commits. target is "" for an event of the connection
// itself (the Target domain) and for every event of a connection that does
// not know event sources (SubscribeTarget); WithTarget and core read "" as
// the connection's active target.
type TargetEventHandler func(target, method string, params json.RawMessage)

// TargetSubscriber is a connection that knows the source target of each
// event (Session, and the daemon's page connection over it).
type TargetSubscriber interface {
	// SubscribeTarget is Caller.Subscribe with the event's source target.
	SubscribeTarget(method string, h TargetEventHandler) func()
}

// SubscribeTarget registers h for method on c with each event's source
// target. On a connection that does not know event sources (the legacy
// Client, test connections), h gets "" as the target: the connection's own
// target.
func SubscribeTarget(c Caller, method string, h TargetEventHandler) func() {
	if ts, ok := c.(TargetSubscriber); ok {
		return ts.SubscribeTarget(method, h)
	}
	return c.Subscribe(method, func(m string, p json.RawMessage) { h("", m, p) })
}

var (
	_ Caller           = (*Client)(nil)
	_ Caller           = (*Session)(nil)
	_ TargetSubscriber = (*Session)(nil)
)
