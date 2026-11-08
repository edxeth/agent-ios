package webkit

import (
	"context"
	"encoding/json"
)

// ReplyHook is told that a connection read the successful reply of a call
// (WithReplyHook). It runs in the connection's read loop, after the
// connection read the reply and before it delivers it to the caller or
// reads the next message: c is the connection (a *Session), target the
// target that sent the reply, result the reply's result. So the hook sees
// the reply at its place among the connection's events: every event read
// before the reply has been dispatched, and no later one has. It must not
// block, send, subscribe or unsubscribe.
type ReplyHook func(c Caller, target string, result json.RawMessage)

type replyHookKey struct{}

// WithReplyHook returns ctx with h: a Session that sends a call to a target
// with ctx, or a context derived from it, runs h when it reads the call's
// successful reply (ReplyHook). An error reply, a call that ends without a
// reply and other callers run nothing.
func WithReplyHook(ctx context.Context, h ReplyHook) context.Context {
	return context.WithValue(ctx, replyHookKey{}, h)
}

func replyHookOf(ctx context.Context) ReplyHook {
	h, _ := ctx.Value(replyHookKey{}).(ReplyHook)
	return h
}

// ReadPosition is the number of messages the session has read. While its
// read loop handles a message, it is that message's position in the
// connection's stream: the event that the message dispatches, or the reply
// that it delivers (ReplyHook), has it, and positions grow in the order of
// the stream.
func (s *Session) ReadPosition() uint64 { return s.reads.Load() }
