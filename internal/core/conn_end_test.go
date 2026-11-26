package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// endingCaller is a page connection whose end the test chooses. It acts
// as webkit.Session does: a call on an ended connection is refused before
// the write (ErrNotSent, with the end's error); a call in flight fails
// with the error that the test passes to end, after the connection ended
// (Done closed, Err set). Close is the daemon's close (closeCaller).
type endingCaller struct {
	webkit.Caller // nil: only Send, Done, Err and Close are used
	waiting       chan struct{}
	fail          chan error
	done          chan struct{}
	once          sync.Once
	mu            sync.Mutex
	err           error
}

func newEndingCaller() *endingCaller {
	return &endingCaller{waiting: make(chan struct{}, 1), fail: make(chan error, 1), done: make(chan struct{})}
}

func (c *endingCaller) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	select {
	case <-c.done:
		return nil, notSent{&webkit.CallError{Method: method, Err: c.Err()}}
	default:
	}
	c.waiting <- struct{}{}
	select {
	case err := <-c.fail:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *endingCaller) Done() <-chan struct{} { return c.done }

func (c *endingCaller) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// end ends the connection with err, then fails the call in flight, if
// any, with callErr.
func (c *endingCaller) end(err, callErr error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
	select {
	case c.fail <- callErr:
	default:
	}
}

func (c *endingCaller) Close() error {
	err := fmt.Errorf("%w: %w", webkit.ErrSessionClosed, net.ErrClosed)
	c.end(err, &webkit.CallError{Method: "Runtime.evaluate", Err: err})
	return nil
}

// docs/sessions.md, "Lost pages and takeover", and design 2.5 rule 3: only
// an end of the page connection that the daemon did not ask for and that
// no write timeout caused makes the page lost, and the command in flight
// (or the first one after the end) then fails with the lost-page error
// (exit 4); a closed tab keeps its own error (webkit.ErrPageClosed), and a
// write timeout makes the page stalled (design 2.6). The state is checked
// right after the command (send's own verdict, before watch runs) and
// after watch's verdict on the ended connection (ended).
func TestPageConnectionEndsMarkOnlyAPeerEndLost(t *testing.T) {
	const method = "Runtime.evaluate"
	claimed := fmt.Errorf("%w: websocket: close 1001 (going away): %s", webkit.ErrSessionClosed, claimedReason)
	timedOut := fmt.Errorf("%w: %w: %w", webkit.ErrSessionClosed, webkit.ErrWriteFailed, webkit.ErrWriteTimeout)
	peerEnd := func(err error) func(*pageConn, *endingCaller) {
		return func(_ *pageConn, c *endingCaller) { c.end(err, &webkit.CallError{Method: method, Err: err}) }
	}
	for _, tc := range []struct {
		name string
		// before ends the connection before the command, which is then
		// refused before the write; otherwise it ends during the command.
		before          bool
		end             func(*pageConn, *endingCaller)
		check           func(t *testing.T, err error)
		state, watched  ConnState
		recoveryStarted bool
	}{
		{
			name: "another inspector claims the page during the command", end: peerEnd(claimed),
			check: func(t *testing.T, err error) {
				want := method + ": the connection ended before the reply, so the page may have run it: " + ErrLost.Error() + ": " + method + ": " + claimed.Error()
				if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || errors.Is(err, webkit.ErrNotSent) || err.Error() != want {
					t.Fatalf("command = %v (exit %d), want exit %d and the text\n%s", err, exitCode(err), CodeBusy, want)
				}
			},
			state: ConnLost, watched: ConnLost,
		},
		{
			name: "another inspector claimed the page before the command", before: true, end: peerEnd(claimed),
			check: func(t *testing.T, err error) {
				want := method + ": not sent: " + ErrLost.Error() + ": " + method + ": " + claimed.Error()
				if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || !errors.Is(err, webkit.ErrNotSent) || err.Error() != want {
					t.Fatalf("command = %v (exit %d), want exit %d, webkit.ErrNotSent and the text\n%s", err, exitCode(err), CodeBusy, want)
				}
			},
			state: ConnLost, watched: ConnLost,
		},
		{
			name: "the tab closes during the command", end: peerEnd(webkit.ErrPageClosed),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrPageClosed) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the closed-tab error (webkit.ErrPageClosed) without ErrLost", err)
				}
			},
			// The tab is gone: watch still marks its page lost.
			state: ConnOK, watched: ConnLost,
		},
		{
			name: "the tab closed before the command", before: true, end: peerEnd(webkit.ErrPageClosed),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrPageClosed) || !errors.Is(err, webkit.ErrNotSent) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the closed-tab error (webkit.ErrPageClosed), not sent, without ErrLost", err)
				}
			},
			state: ConnOK, watched: ConnLost,
		},
		{
			name: "the daemon closes the page (daemon stop, idle close)",
			end:  func(p *pageConn, _ *endingCaller) { p.close() },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrSessionClosed) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the closed connection's error without ErrLost", err)
				}
			},
			state: ConnClosed, watched: ConnClosed,
		},
		{
			name: "the daemon retires the connection for a reconnect",
			end:  func(p *pageConn, _ *endingCaller) { p.retire() },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrSessionClosed) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the closed connection's error without ErrLost", err)
				}
			},
			state: ConnOK, watched: ConnOK,
		},
		{
			name: "the daemon retired the connection before the command", before: true,
			end: func(p *pageConn, _ *endingCaller) { p.retire() },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrSessionClosed) || !errors.Is(err, webkit.ErrNotSent) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the closed connection's error, not sent, without ErrLost", err)
				}
			},
			state: ConnOK, watched: ConnOK,
		},
		{
			name: "a write times out", end: peerEnd(timedOut),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, webkit.ErrWriteTimeout) || errors.Is(err, ErrLost) {
					t.Fatalf("command = %v, want the write timeout without ErrLost", err)
				}
			},
			state: ConnStalled, watched: ConnStalled, recoveryStarted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEndingCaller()
			var recoveries atomic.Int32
			// watch does not run (background refuses): the test calls its
			// verdict, ended, after the command.
			p := newPageConn(c, func() { recoveries.Add(1) }, func(func(context.Context)) bool { return false }, func(string, ...any) {})
			if tc.before {
				tc.end(p, c)
			}
			res := make(chan error, 1)
			go func() {
				_, err := p.Send(t.Context(), method, nil)
				res <- err
			}()
			if !tc.before {
				<-c.waiting
				tc.end(p, c)
			}
			tc.check(t, <-res)
			if st := p.State(); st != tc.state {
				t.Errorf("state after the command = %v, want %v", st, tc.state)
			}
			p.ended(c)
			if st := p.State(); st != tc.watched {
				t.Errorf("state after watch's verdict = %v, want %v", st, tc.watched)
			}
			if got := recoveries.Load() > 0; got != tc.recoveryStarted {
				t.Errorf("recovery started: %v, want %v", got, tc.recoveryStarted)
			}
		})
	}
}
