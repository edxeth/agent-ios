package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// timerNotFired is a command context whose deadline is dl and whose
// state is that of the embedded context: with a live one it is the
// instant when the clock has passed the deadline but the deadline timer
// has not run yet, so ctx.Err() is still nil. An error that the deadline
// caused elsewhere (the in-process core's expiry of the operation, the
// daemon's answer) can reach the CLI in that instant.
type timerNotFired struct {
	context.Context
	dl time.Time
}

func (c timerNotFired) Deadline() (time.Time, bool) { return c.dl, true }

// The CLI's deadline decision (commandRun.DeadlineMessage, which
// commands.RunCLIStarted uses for exit 124) depends on the clock and the
// command context, not on whether the deadline timer ran before the
// command's error arrived. A cancellation (a signal) and an error before
// the deadline stay what they are. Regression of
// TestCLINoDaemonTimedOutCommandStillSendsItsCleanup, which exited 1 with
// "context canceled" on a loaded host.
func TestDeadlineMessageDoesNotWaitForTheDeadlineTimer(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	past, future := time.Now().Add(-time.Millisecond), time.Now().Add(time.Hour)
	for _, c := range []struct {
		name string
		ctx  context.Context
		hit  bool
	}{
		{"deadline passed, timer not run", timerNotFired{context.Background(), past}, true},
		{"canceled, deadline passed", timerNotFired{canceled, past}, false},
		{"before the deadline", timerNotFired{context.Background(), future}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &commandRun{}
			if _, ok := r.started("eval", c.ctx); !ok {
				t.Fatal("started recorded no deadline")
			}
			msg, hit := r.DeadlineMessage(context.Canceled)
			if hit != c.hit {
				t.Fatalf("DeadlineMessage(context.Canceled) hit = %v, want %v; msg %q", hit, c.hit, msg)
			}
			if hit && !strings.Contains(msg, "Error: agent-ios eval timed out") {
				t.Errorf("deadline message %q: want the full deadline message", msg)
			}
			if _, hit := r.DeadlineMessage(errors.New("unrelated")); hit != c.hit {
				t.Errorf("DeadlineMessage of another error hit = %v, want %v: the command context decides", hit, c.hit)
			}
		})
	}
}
