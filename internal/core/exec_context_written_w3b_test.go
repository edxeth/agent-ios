package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// W3b final repair round 4: an operation's own refusals (lease, capacity,
// setup) are failures before the write, marked webkit.ErrNotSent with the
// text "<method>: not sent: ..." they had before.
func TestOperationRefusalIsNotSent(t *testing.T) {
	s := NewScheduler(&paramsCaller{}, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease("a", 1)
	op, err := s.Begin(ctxFor(t, 5*time.Second), OpSpec{Name: "t", Class: ClassRead, Session: "a", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	op.End()
	_, err = op.Caller().Send(ctxFor(t, time.Second), "Runtime.evaluate", map[string]any{"expression": "1"})
	if !errors.Is(err, webkit.ErrNotSent) || !strings.HasPrefix(err.Error(), "Runtime.evaluate: not sent: ") {
		t.Fatalf("send of an ended operation: %v, want ErrNotSent with the text \"Runtime.evaluate: not sent: ...\"", err)
	}
}

// twoCallConn answers the first call of a release (written) and fails the
// second; lastWritten says whether the second was written.
type twoCallConn struct {
	nopCaller
	mu          sync.Mutex
	calls       int
	lastWritten bool
}

func (c *twoCallConn) Send(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 || c.lastWritten {
		webkit.MarkWritten(ctx)
	}
	if c.calls == 1 {
		return json.RawMessage(`{}`), nil
	}
	return nil, errors.New(method + ": failed")
}

// A release counts as reached only on the evidence of its failing call:
// a release whose first call was written and whose failing second call
// was not keeps its record; the same release whose failing call was
// written is forgotten.
func TestReleaseReachedOnlyByItsFailingCall(t *testing.T) {
	for _, lastWritten := range []bool{false, true} {
		c := &twoCallConn{lastWritten: lastWritten}
		s := NewScheduler(c, SchedulerOptions{})
		s.installContext(1, ExecContext{ID: 5, Frame: "kid", conn: c})
		err := s.ReleaseExecutionContexts(ctxFor(t, time.Second), 1, c, func(ctx context.Context, cc webkit.Caller) error {
			if _, err := cc.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "install"}); err != nil {
				return err
			}
			_, err := cc.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
			return err
		})
		s.Close()
		if err == nil {
			t.Fatal("the release succeeded")
		}
		s.ForgetFailedExecutionContexts(1)
		want := []uint64{1}
		if lastWritten {
			want = nil
		}
		if got := s.ContextEpochsBefore(2); !reflect.DeepEqual(got, want) {
			t.Fatalf("failing call written %v: epochs left %v, want %v", lastWritten, got, want)
		}
	}
}
