package core

import (
	"sort"
	"sync"
	"time"
)

// Clock is the daemon's monotonic clock. Fencing never uses wall clocks
// (design 2.6, clock bounds).
type Clock interface {
	// Now is the time since an arbitrary fixed origin.
	Now() time.Duration
	// After returns a channel that is closed once Now() >= now+d.
	After(d time.Duration) <-chan struct{}
}

// RealClock is the Go monotonic clock.
type RealClock struct{ origin time.Time }

// NewRealClock returns a Clock whose origin is now.
func NewRealClock() *RealClock { return &RealClock{origin: time.Now()} }

func (c *RealClock) Now() time.Duration { return time.Since(c.origin) }

func (c *RealClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

// Rho is the assumed bound on the page clock rate relative to the daemon
// clock: 100 ppm (design 2.6). It is an operating assumption, not a
// measured device fact.
const rhoPPM = 100

// drift is the conservative drift allowance rho*d, rounded up.
func drift(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return (d*rhoPPM + 999_999) / 1_000_000
}

// ClockSample is one measurement: the daemon read H0 at send and H1 at
// reply of a runtime call that returned the page's performance.now() value P.
// P is on the page clock, as a duration since the document's time origin.
type ClockSample struct {
	H0, H1 time.Duration
	P      time.Duration
}

// ClockMap maps the daemon clock to one document's performance.now() clock
// with the conservative bounds of design 2.6. Every sample gives a valid
// lower and upper bound; the map uses the tightest of each. The document is
// the one whose runtime answered the first measurement (BindDoc).
type ClockMap struct {
	mu      sync.Mutex
	doc     string
	samples []ClockSample
	last    time.Duration // round trip (H1-H0) of the latest sample
}

// BindDoc binds the map to the document id doc that a measurement
// reported. It reports false when the map belongs to another document.
func (m *ClockMap) BindDoc(doc string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.doc == "" {
		m.doc = doc
	}
	return m.doc == doc
}

// Doc is the id of the map's document ("" before the first measurement).
func (m *ClockMap) Doc() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doc
}

// Observe adds a sample. Samples with H1 < H0 are ignored.
func (m *ClockMap) Observe(s ClockSample) {
	if s.H1 < s.H0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = s.H1 - s.H0
	m.samples = append(m.samples, s)
	// Keep the sample that gives the largest lower bound and the one that
	// gives the smallest upper bound; for a fixed rho the order of the bounds
	// of two samples does not depend on T.
	if len(m.samples) > 2 {
		lo := m.samples[0]
		hi := m.samples[0]
		for _, x := range m.samples[1:] {
			if lowKey(x) > lowKey(lo) {
				lo = x
			}
			if highKey(x) < highKey(hi) {
				hi = x
			}
		}
		m.samples = []ClockSample{lo, hi}
		sort.Slice(m.samples, func(i, j int) bool { return m.samples[i].H1 < m.samples[j].H1 })
	}
}

// lowKey orders samples by their lower bound (p - (1-rho)*h1, scaled).
func lowKey(s ClockSample) time.Duration { return s.P - s.H1 + drift(s.H1) }

// highKey orders samples by their upper bound (p - (1+rho)*h0, scaled).
func highKey(s ClockSample) time.Duration { return s.P - s.H0 - drift(s.H0) }

// RoundTrip is the round trip of the latest sample: the time from the send
// of the runtime call that measured it to its reply (0 before the first).
func (m *ClockMap) RoundTrip() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Measured reports whether the map has a sample.
func (m *ClockMap) Measured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.samples) > 0
}

// PageMin is the earliest page-clock value the page can show at daemon time
// t (t at or after the samples): p + (1-rho)*(t-h1), maximized over samples.
func (m *ClockMap) PageMin(t time.Duration) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best time.Duration
	for i, s := range m.samples {
		v := s.P + (t - s.H1) - drift(t-s.H1)
		if i == 0 || v > best {
			best = v
		}
	}
	return best
}

// PageMax is the latest page-clock value the page can show at daemon time t:
// p + (1+rho)*(t-h0), minimized over samples.
func (m *ClockMap) PageMax(t time.Duration) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best time.Duration
	for i, s := range m.samples {
		v := s.P + (t - s.H0) + drift(t-s.H0)
		if i == 0 || v < best {
			best = v
		}
	}
	return best
}

// Uncertainty is the width of the possible page values at daemon time t.
// For one sample it is (h1-h0) + rho*(2t-h0-h1).
func (m *ClockMap) Uncertainty(t time.Duration) time.Duration {
	return m.PageMax(t) - m.PageMin(t)
}
