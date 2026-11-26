package webkit

import (
	"context"
	"sync"
	"sync/atomic"
)

// writeNoticeKey is the context key of WithWriteNotice.
type writeNoticeKey struct{}

// WithWriteNotice returns ctx with written: a call that a Session or a
// Client sends with the returned context, or one derived from it, sets
// written once its message was written to the connection. It is the
// positive evidence that the page can have received the call; a call that
// fails without setting it wrote nothing (ErrNotSent) or failed during its
// write, so whether the page received it is unknown.
func WithWriteNotice(ctx context.Context, written *atomic.Bool) context.Context {
	return context.WithValue(ctx, writeNoticeKey{}, written)
}

// MarkWritten sets the write notice of ctx (WithWriteNotice), if it has
// one. Session and Client call it after a call's message was written;
// another connection (a test fake) calls it when it delivers a call.
func MarkWritten(ctx context.Context) {
	if w, ok := ctx.Value(writeNoticeKey{}).(*atomic.Bool); ok {
		w.Store(true)
	}
	s, _ := ctx.Value(writeSignalKey{}).(*writeSignal)
	for ; s != nil; s = s.outer {
		s.once.Do(func() { close(s.ch) })
	}
}

// writeSignalKey is the context key of WithWriteSignal.
type writeSignalKey struct{}

type writeSignal struct {
	once  sync.Once
	ch    chan struct{}
	outer *writeSignal // the signal of the context it was derived from
}

// WithWriteSignal returns ctx with written, which MarkWritten closes once
// the message of a call sent with the returned context, or one derived
// from it, was written to the connection: the positive evidence that a
// later call on the same connection reaches the page after it (WebKit runs
// a target's commands in the order they arrive). written must be open and
// used for one call. A signal of ctx itself is closed as well: a caller
// layer can watch the write of a call whose sender watches it too.
func WithWriteSignal(ctx context.Context, written chan struct{}) context.Context {
	outer, _ := ctx.Value(writeSignalKey{}).(*writeSignal)
	return context.WithValue(ctx, writeSignalKey{}, &writeSignal{ch: written, outer: outer})
}
