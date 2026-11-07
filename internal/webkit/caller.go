package webkit

import (
	"context"
	"encoding/json"
	"fmt"
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

// AllEvents subscribes a handler to every event of a Session.
const AllEvents = "*"

var (
	_ Caller = (*Client)(nil)
	_ Caller = (*Session)(nil)
)
