package core

import (
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Test support of this package's tests only: nothing in the daemon uses
// these, so they live here and not in the production files.

// ManualClock is a Clock that moves only when a test sets it.
type ManualClock struct {
	mu      sync.Mutex
	now     time.Duration
	waiters []manualWaiter
}

type manualWaiter struct {
	at time.Duration
	ch chan struct{}
}

// NewManualClock returns a clock that shows now.
func NewManualClock(now time.Duration) *ManualClock { return &ManualClock{now: now} }

func (c *ManualClock) Now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ManualClock) After(d time.Duration) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	if d <= 0 {
		close(ch)
		return ch
	}
	c.waiters = append(c.waiters, manualWaiter{at: c.now + d, ch: ch})
	return ch
}

// Set moves the clock to t (never backwards) and fires due waiters.
func (c *ManualClock) Set(t time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t > c.now {
		c.now = t
	}
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at <= c.now {
			close(w.ch)
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
}

// followTargets is followCommits without a pending cleanup.
func (d *DesiredState) followTargets(c interface {
	SubscribeSync(method string, h webkit.EventHandler) func()
},
) {
	d.followCommits(c, nil)
}

// unreportedFrames is unreportedIn on the current connection's inventory.
func (s *Scheduler) unreportedFrames(epoch uint64) []string {
	return s.unreportedIn(epoch, s.inventory())
}

// pauseOpen reports whether a debugger pause of any target is an open
// pending item.
func (s *Scheduler) pauseOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.openPausesLocked()) > 0
}

// openPausesLocked returns the targets whose pause is an open pending item.
func (s *Scheduler) openPausesLocked() []string {
	var out []string
	for t, p := range s.pauses {
		if p.open() {
			out = append(out, t)
		}
	}
	return out
}
