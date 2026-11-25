package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Decision D9, final check of core-events
// (TestEveryExpiredCommandDeadlineIsATimeout/legacy_call exited 4 after
// "re-applying the page state: applying desired state:
// Page.setBootstrapScript: writing message: write tcp ...: i/o timeout"):
// the write of the attach's setup batch was cut off by the command's
// deadline. The attach is a timeout (exit 124, context.DeadlineExceeded
// with its cause), for the legacy client of a local endpoint and for a
// Session with its pipelined batch. A wire failure while the deadline is
// still ahead stays a busy page (exit 4). docs/notes/w1b.md, "core-events
// r1: a write cut off by the deadline".
func TestAttachWriteCutOffByTheDeadlineIsATimeout(t *testing.T) {
	errCause := errors.New("the command deadline passed (test cause)")
	openers := map[string]func(w *fakewebkit.WriteStall) func(ctx context.Context, u string) (webkit.Caller, error){
		"legacy client": func(w *fakewebkit.WriteStall) func(ctx context.Context, u string) (webkit.Caller, error) {
			return func(ctx context.Context, u string) (webkit.Caller, error) {
				return webkit.NewClientWithDialer(ctx, u, w.Dialer())
			}
		},
		"session batch": func(w *fakewebkit.WriteStall) func(ctx context.Context, u string) (webkit.Caller, error) {
			return func(ctx context.Context, u string) (webkit.Caller, error) {
				return webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
					c, err := webkit.DialWebSocket(ctx, u, transport.Limits{Routed: true})
					if err != nil {
						return nil, err
					}
					return w.Conn(c), nil
				}, webkit.SessionOptions{TargetWait: time.Second})
			}
		},
	}
	// A connect that the deadline ended (webkit reports the handshake's
	// timeout as the deadline) is a timeout too; another connect failure
	// stays busy.
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"connect cut off by the deadline", fmt.Errorf("connecting: %w", context.DeadlineExceeded), CodeTimeout},
		{"connect refused", errors.New("connecting: connection refused"), CodeBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{OpenURL: func(context.Context, string) (webkit.Caller, error) { return nil, tc.err }})
			local := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(local.Close)
			_, err := r.m.Begin(ctxFor(t, 5*time.Second), PageRequest{Transport: "iwdp", Addr: r.l.Addr(), Page: local.URL}, agent, "a", ClassExclusive)
			var ce *CodedError
			if !errors.As(err, &ce) || ce.Code != tc.wantCode || !errors.Is(err, tc.err) {
				t.Fatalf("Begin: %v; want code %d wrapping %v", err, tc.wantCode, tc.err)
			}
		})
	}
	for name, opener := range openers {
		for _, tc := range []struct {
			name     string
			fail     error
			timeout  time.Duration
			wantCode int
		}{
			{"cut off by the deadline", nil, 300 * time.Millisecond, CodeTimeout},
			{"wire failure before the deadline", &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}, 5 * time.Second, CodeBusy},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				// The page runtime is a bootstrap part of every page, so the
				// setup batch carries Page.setBootstrapScript.
				w := &fakewebkit.WriteStall{Method: "Page.setBootstrapScript", Fail: tc.fail}
				r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{OpenURL: opener(w)})
				local := fakewebkit.New(fakewebkit.Options{})
				t.Cleanup(local.Close)
				ctx, cancel := context.WithTimeoutCause(context.Background(), tc.timeout, errCause)
				defer cancel()
				_, err := r.m.Begin(ctx, PageRequest{Transport: "iwdp", Addr: r.l.Addr(), Page: local.URL}, agent, "a", ClassExclusive)
				var ce *CodedError
				if !errors.As(err, &ce) || ce.Code != tc.wantCode {
					t.Fatalf("Begin: %v (%#v); want code %d", err, ce, tc.wantCode)
				}
				// A pipelined batch collects its replies after its writes, so
				// the deadline can end the wait for an earlier call's reply
				// first, and the error names that call.
				if (name != "session batch" || tc.fail != nil) && !strings.Contains(err.Error(), "Page.setBootstrapScript") {
					t.Fatalf("Begin: %v; want the error of Page.setBootstrapScript", err)
				}
				deadline := errors.Is(err, context.DeadlineExceeded) && errors.Is(err, errCause)
				if deadline != (tc.wantCode == CodeTimeout) {
					t.Fatalf("Begin: %v; deadline with its cause = %v, want %v", err, deadline, tc.wantCode == CodeTimeout)
				}
				if w.Stalled() != 1 {
					t.Fatalf("%d cut-off writes of Page.setBootstrapScript, want 1", w.Stalled())
				}
			})
		}
	}
}
