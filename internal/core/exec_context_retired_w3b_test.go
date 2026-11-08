package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// retiredReleaseExpr is the expression of the release whose write fails.
const retiredReleaseExpr = "w3b-retired-release"

// W3b second rebase (core-events r9b, r9c): a frame release whose socket
// write fails retires the connection (webkit.ErrWriteFailed), on a
// webkit.Client and on a webkit.Session. The failed write may have
// written part of its message, so its error carries no webkit.ErrNotSent
// and it sets no write notice (webkit.WithWriteNotice: only a completed
// write sets it). reachedPage therefore has no evidence
// either way: ForgetFailedExecutionContexts keeps the frame record, and the
// next release, after the reconnect, releases the frame in its context on
// the new connection. This holds for both write categories: a timeout
// (the daemon reconnects by itself) and a broken write (the page is lost
// until --takeover connects it again). The Session case fails its write
// in the WebSocket's own socket write, past the call's deadline
// (preemptOnce), so only the timeout category applies there.
func TestRetiredReleaseWriteKeepsTheFrameRecord(t *testing.T) {
	// retiredConn is the first connection: its release write fails. Each
	// case's open returns it and a report of whether that write failed.
	type retiredConn interface {
		webkit.Caller
		Err() error
	}
	clientWith := func(fail error) func(t *testing.T, ctx context.Context, url string) (retiredConn, func() bool) {
		return func(t *testing.T, ctx context.Context, url string) (retiredConn, func() bool) {
			// Every write of a Runtime.evaluate fails with fail;
			// Runtime.enable passes.
			w := &fakewebkit.WriteStall{Method: "Runtime.evaluate", Fail: fail}
			c, err := webkit.NewClientWithDialer(ctx, url, w.Dialer())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return c, func() bool { return w.Stalled() == 1 }
		}
	}
	for _, tc := range []struct {
		name     string
		open     func(t *testing.T, ctx context.Context, url string) (retiredConn, func() bool)
		category error
	}{
		{"client, write timeout", clientWith(&net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}), webkit.ErrWriteTimeout},
		{"client, broken write", clientWith(&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}), webkit.ErrWriteBroken},
		{"session, write timeout", func(t *testing.T, ctx context.Context, url string) (retiredConn, func() bool) {
			conn, err := webkit.DialWebSocket(ctx, url, transport.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			var armed atomic.Bool
			armed.Store(true)
			s, err := webkit.NewSession(ctx, preemptOnce{MessageConn: conn, marker: []byte(retiredReleaseExpr), armed: &armed}, webkit.SessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s, func() bool { return !armed.Load() }
		}, webkit.ErrWriteTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			// The frame "kid" runs in context 17 on the first connection
			// and in context 27 on the second.
			f.Handle("Runtime.enable", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
				id := 17
				if call.Conn != 1 {
					id = 27
				}
				f.EmitFrom(call.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "frameId": "kid", "type": "normal"}})
				return map[string]any{}, nil
			})
			ctx := ctxFor(t, 5*time.Second)
			c1, failed := tc.open(t, ctx, f.URL)
			if _, err := c1.Send(ctx, "Runtime.enable", nil); err != nil {
				t.Fatal(err)
			}
			pc := newPageConn(c1, func() {}, func(func(context.Context)) bool { return false }, t.Logf)
			t.Cleanup(pc.close)
			s := NewScheduler(pc, SchedulerOptions{})
			t.Cleanup(s.Close)
			s.SetLease("s1", 2)
			ec := s.FrameContext("kid")
			if ec.ID != 17 {
				t.Fatalf("frame context = %d, want 17", ec.ID)
			}
			s.installContext(1, ec)

			// release runs the owner-change cleanup of epoch 1 in an
			// operation of epoch 2, as commands releaseEarlierFrames does:
			// the release, then ForgetFailedExecutionContexts after a
			// failure.
			release := func() error {
				op, err := s.Begin(ctx, OpSpec{Session: "s1", Epoch: 2, Name: "get", Class: ClassRead})
				if err != nil {
					t.Fatal(err)
				}
				defer op.End()
				err = s.ReleaseExecutionContexts(ctxFor(t, time.Second), 1, op.Caller(), func(ctx context.Context, c webkit.Caller) error {
					_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": retiredReleaseExpr})
					return err
				})
				if err != nil {
					s.ForgetFailedExecutionContexts(1)
				}
				return err
			}

			err := release()
			if err == nil || errors.Is(err, webkit.ErrNotSent) {
				t.Fatalf("release through the failing write = %v, want a failure without ErrNotSent (the write may have been partial)", err)
			}
			select {
			case <-c1.Done():
			default:
				t.Fatal("fixture: the failed write did not retire the connection")
			}
			if cerr := c1.Err(); !errors.Is(cerr, webkit.ErrWriteFailed) || !errors.Is(cerr, tc.category) {
				t.Fatalf("fixture: the retired connection's Err = %v, want ErrWriteFailed and %v", cerr, tc.category)
			}
			if !failed() {
				t.Fatal("fixture: the release write did not fail")
			}
			if left := s.ContextEpochsBefore(2); !reflect.DeepEqual(left, []uint64{1}) {
				t.Fatalf("epochs left after the retired write = %v, want [1]: a failed write is no evidence that the release reached the page", left)
			}

			// The reconnect, as Manager.openConn and the recovery do it:
			// the new connection reports the frame's context, the recorded
			// contexts become their frames, and the connection is swapped.
			var c2 webkit.Caller
			if _, ok := c1.(*webkit.Session); ok {
				conn, err := webkit.DialWebSocket(ctx, f.URL, transport.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				if c2, err = webkit.NewSession(ctx, conn, webkit.SessionOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if c2, err = webkit.NewClient(ctx, f.URL); err != nil {
				t.Fatal(err)
			}
			if _, err := c2.Send(ctx, "Runtime.enable", nil); err != nil {
				t.Fatal(err)
			}
			s.contextsMoved()
			pc.swap(c2)

			if err := release(); err != nil {
				t.Fatalf("release on the new connection = %v", err)
			}
			// The failed write delivered nothing here; the page received
			// only the release on the new connection.
			var got []int64
			for _, rec := range f.ReceivedMethod("Runtime.evaluate") {
				var p struct {
					ContextID int64 `json:"contextId"`
				}
				_ = json.Unmarshal(rec.Params, &p)
				got = append(got, p.ContextID)
			}
			if !reflect.DeepEqual(got, []int64{27}) {
				t.Fatalf("releases received in contexts %v, want [27] (the frame's context on the new connection)", got)
			}
			if left := s.ContextEpochsBefore(2); len(left) != 0 {
				t.Fatalf("epochs left after the release on the new connection = %v, want none", left)
			}
		})
	}
}
