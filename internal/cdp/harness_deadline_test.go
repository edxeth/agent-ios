package cdp

import (
	"context"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// oldFixedBound is the fixed time that call and waitSessionEvent waited
// before they waited until the test's deadline.
const oldFixedBound = 10 * time.Second

// slowStep is the fake page's delay of each of the two replies that
// Tracing.end waits for (Timeline.stop and Timeline.disable): together
// they take Tracing.end past oldFixedBound, and each stays below the 10 s
// that the page session waits for one reply (webkit.DefaultCallTimeout).
const slowStep = 6 * time.Second

// slowTraceEnd attaches page One and starts a trace whose Tracing.end
// takes at least 2*slowStep.
func slowTraceEnd(t *testing.T) (*cdptest.Client, string) {
	t.Helper()
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", nil)
	r.fake["One"].DelayReplies("Timeline.stop", slowStep)
	r.fake["One"].DelayReplies("Timeline.disable", slowStep)
	return c, page
}

// The waits of the harness last until the test's deadline, not a fixed
// time (AGENTS.md, Tests): under -race on a host with a load of 30
// to 50, Tracing.end of a trace at the record limit took up to 11 s and
// its tracingComplete up to 10 s more, and the fixed 10 s of call and
// waitSessionEvent failed TestCDPTraceRecordLimitReportsDataLoss and
// TestCDPTraceByteLimitReportsDataLoss. Here a slow page makes Tracing.end
// and its tracingComplete arrive later than 10 s, and both waits still
// succeed.
func TestHarnessWaitsPastTheOldFixedBound(t *testing.T) {
	t.Run("call", func(t *testing.T) {
		t.Parallel()
		c, page := slowTraceEnd(t)
		start := time.Now()
		call(t, c, page, "Tracing.end", nil)
		if took := time.Since(start); took <= oldFixedBound {
			t.Fatalf("Tracing.end took %v, want more than %v (the page's delays)", took, oldFixedBound)
		}
	})
	t.Run("waitSessionEvent", func(t *testing.T) {
		t.Parallel()
		c, page := slowTraceEnd(t)
		seq := c.Seq()
		ended := make(chan error, 1)
		start := time.Now()
		// The client's Close at the test's end ends this call if the
		// wait below fails.
		go func() {
			_, err := c.Call(context.Background(), page, "Tracing.end", nil)
			ended <- err
		}()
		waitSessionEvent(t, c, seq, page, "Tracing.tracingComplete")
		if took := time.Since(start); took <= oldFixedBound {
			t.Fatalf("Tracing.tracingComplete came after %v, want more than %v (the page's delays)", took, oldFixedBound)
		}
		// The endpoint sends tracingComplete after the reply to Tracing.end.
		if err := <-ended; err != nil {
			t.Fatalf("Tracing.end: %v", err)
		}
	})
}
