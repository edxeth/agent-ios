package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// core-events r6: ResumeTarget runs bound to the connection's stall like
// every other call through pageConn (core-events r5 decision): a declared
// stall ends a waiting resume at once with the stall classification, a
// stalled page refuses it, and a deadline that ends first keeps its own
// error.

// resumeCaller answers no command, so the liveness probe fails at its
// budget, and its ResumeTarget waits until its context ends.
type resumeCaller struct {
	done     chan struct{}
	resuming chan struct{}
}

func newResumeCaller() *resumeCaller {
	return &resumeCaller{done: make(chan struct{}), resuming: make(chan struct{}, 1)}
}

func (c *resumeCaller) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("%s: no response: %w", method, ctx.Err())
}
func (c *resumeCaller) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (c *resumeCaller) IsTargetRouted() bool                         { return true }
func (c *resumeCaller) Done() <-chan struct{}                        { return c.done }
func (c *resumeCaller) ResumeTarget(ctx context.Context, id string) error {
	c.resuming <- struct{}{}
	<-ctx.Done()
	return fmt.Errorf("Target.resume of %s: no response: %w", id, ctx.Err())
}

func TestResumeTargetIsBoundToTheStall(t *testing.T) {
	newConn := func() (*pageConn, *resumeCaller) {
		c := newResumeCaller()
		return newPageConn(c, nil, func(func(context.Context)) bool { return false }, func(string, ...any) {}), c
	}
	isStall := func(err error) bool {
		return errors.Is(err, webkit.ErrSessionStalled) && exitCode(err) == CodeBusy
	}

	t.Run("a declared stall ends a waiting resume", func(t *testing.T) {
		p, c := newConn()
		const deadline = 8 * time.Second
		res := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			res <- p.ResumeTarget(ctx, "page-1")
		}()
		<-c.resuming
		pctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := p.probeNow(pctx); err == nil || p.State() != ConnStalled {
			t.Fatalf("fixture: probe = %v, state %s; want a failed probe and a stalled page", err, p.State())
		}
		select {
		case err := <-res:
			if !isStall(err) || !strings.Contains(err.Error(), "resuming page target page-1") {
				t.Errorf("resume = %v (exit %d), want the stall classification (exit %d) for \"resuming page target page-1\"", err, exitCode(err), CodeBusy)
			}
		case <-time.After(time.Second):
			t.Errorf("the resume still waits after the probe declared the stall (deadline %v)", deadline)
		}

		// The stalled page refuses the next resume without calling the
		// connection.
		ctx, cancel2 := context.WithTimeout(context.Background(), deadline)
		defer cancel2()
		if err := p.ResumeTarget(ctx, "page-1"); !isStall(err) {
			t.Errorf("resume on the stalled page = %v (exit %d), want the stall error (exit %d)", err, exitCode(err), CodeBusy)
		}
		if len(c.resuming) != 0 {
			t.Error("the stalled page passed the resume to its connection")
		}
	})

	t.Run("a deadline that ends first keeps its error", func(t *testing.T) {
		p, _ := newConn()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := p.ResumeTarget(ctx, "page-1")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, webkit.ErrSessionStalled) {
			t.Errorf("resume = %v, want its deadline without the stall classification", err)
		}
	})
}
