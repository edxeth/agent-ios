package webkit

import (
	"context"
	"encoding/json"
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

// AllEvents subscribes a handler to every event of a Session.
const AllEvents = "*"

var (
	_ Caller = (*Client)(nil)
	_ Caller = (*Session)(nil)
)
