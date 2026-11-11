package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Cleanup at the end of an operation (design docs/design/agentic-parity.md,
// sections 2.4 and 2.10). Page state that an operation set only for its
// own duration (the annotation overlay of screenshot --annotate, 3.9) must
// be gone before any successor runs, also when the operation was canceled
// or its deadline passed and its fenced caller sends nothing more. A
// cleanup registered with Finally runs when the operation ends, after an
// in-flight attempt's reply or slot-release boundary, and before the
// operation's slot is released, so a queued successor is admitted only
// after it. It sends through the control lane (as the release barrier's
// cleanup does) and stays fenced to the operation's lease: nothing is sent
// once the lease ended or another epoch holds the page. End does not wait
// for it, so the client is answered at once; a successor waits at most the
// cleanup's budget.

type opFinally struct {
	budget time.Duration
	fn     func(ctx context.Context, c webkit.Caller)
}

// Finally registers fn to run once when the operation ends, before its
// slot is released. fn gets a context bounded by budget and a caller that
// sends only while the operation's lease epoch is current; the caller does
// not add the operation's execution context. It returns ErrOpEnded, and
// fn never runs, when End was already called.
func (op *Op) Finally(budget time.Duration, fn func(ctx context.Context, c webkit.Caller)) error {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.ended {
		return ErrOpEnded
	}
	op.finally = append(op.finally, opFinally{budget: budget, fn: fn})
	return nil
}

// runFinally runs the cleanups registered with Finally, in order.
func (op *Op) runFinally(fs []opFinally) {
	for _, f := range fs {
		ctx, cancel := context.WithTimeout(context.Background(), f.budget)
		f.fn(ctx, leaseCaller{op})
		cancel()
	}
}

// leaseCaller is the caller of an operation's Finally cleanups: it is
// fenced by the operation's lease (not released, same epoch), not by the
// operation's context, and sends on the control lane. Like opCaller, it
// checks the fence before and again after the wait for protocol capacity,
// because the lease can end or move to another epoch during that wait.
type leaseCaller struct{ op *Op }

func (c leaseCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s := c.op.s
	if err := c.current(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, ctx.Err())
	}
	defer func() { <-s.sem }()
	// The cleanup's budget can also end while capacity frees: a select
	// with both cases ready picks either one.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	if err := c.current(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	return controlCaller{s.c}.Send(ctx, method, params)
}

// current reports why the operation's lease no longer allows a send:
// ErrLeaseEnded after a release, ErrStaleEpoch when another epoch holds
// the page; nil while the operation's epoch is current.
func (c leaseCaller) current() error {
	s := c.op.s
	s.mu.Lock()
	released, epoch := s.released, s.epoch
	s.mu.Unlock()
	switch {
	case released:
		return ErrLeaseEnded
	case epoch != c.op.Spec.Epoch:
		return ErrStaleEpoch
	}
	return nil
}

func (c leaseCaller) Subscribe(method string, h webkit.EventHandler) func() {
	return c.op.s.c.Subscribe(method, h)
}
func (c leaseCaller) IsTargetRouted() bool  { return c.op.s.c.IsTargetRouted() }
func (c leaseCaller) Done() <-chan struct{} { return c.op.s.c.Done() }
