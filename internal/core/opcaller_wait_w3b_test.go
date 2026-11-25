package core

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// W3b final repair round 5 (round 4, acceptance criterion 3: every
// failure before the write carries webkit.ErrNotSent): an operation's
// caller (opCaller) that waits for protocol capacity sends nothing when
// the operation ends during the wait, or when the lease moved to another
// epoch by the time the capacity is free. The text "<method>: not sent:
// ..." is unchanged. capacityWaitContext (with hideDone, so the caller's
// own ctx never ends the wait) reports that the send waits for capacity.
func TestOperationCallerRefusalsAroundTheCapacityWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(s *Scheduler, op *Op)
		want   error
	}{
		{"the operation ends during the wait", func(_ *Scheduler, op *Op) { op.Cancel(ErrOpEnded) }, ErrOpEnded},
		{"the lease moves while the capacity is in use", func(s *Scheduler, _ *Op) { s.SetLease("b", 2) }, ErrStaleEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &paramsCaller{}
			s := NewScheduler(c, SchedulerOptions{MaxInFlight: 1})
			t.Cleanup(s.Close)
			s.SetLease("a", 1)
			op, err := s.Begin(ctxFor(t, 5*time.Second), OpSpec{Name: "get", Class: ClassRead, Session: "a", Epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer op.End()
			s.sem <- struct{}{} // all protocol capacity is in use
			reached := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				wait := capacityWaitContext{Context: ctxFor(t, 5*time.Second), reached: reached, once: &sync.Once{}, hideDone: true}
				_, err := op.Caller().Send(wait, "Runtime.evaluate", map[string]any{"expression": "release"})
				result <- err
			}()
			<-reached // the send passed the first check and waits for capacity
			tc.change(s, op)
			if tc.want == ErrStaleEpoch {
				<-s.sem // the capacity is free; the operation is not active any more
			}
			err = <-result
			if !errors.Is(err, webkit.ErrNotSent) || !errors.Is(err, tc.want) || !strings.HasPrefix(err.Error(), "Runtime.evaluate: not sent: ") {
				t.Fatalf("send: %v; want ErrNotSent with %v and the text \"Runtime.evaluate: not sent: ...\"", err, tc.want)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.sent) != 0 {
				t.Fatalf("the call was sent: %+v", c.sent)
			}
		})
	}
}
