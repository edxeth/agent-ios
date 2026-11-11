package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// CPU profile in the .cpuprofile format of Chrome DevTools (CDP
// Profiler.Profile): nodes with call frames, hit counts and children, the
// sampled node ids, and the time deltas between samples, in microseconds.

// CallFrame is a CDP Runtime.CallFrame (0-based line and column).
type CallFrame struct {
	FunctionName string `json:"functionName"`
	ScriptID     string `json:"scriptId"`
	URL          string `json:"url"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

// ProfileNode is a CDP Profiler.ProfileNode.
type ProfileNode struct {
	ID        int       `json:"id"`
	CallFrame CallFrame `json:"callFrame"`
	HitCount  int       `json:"hitCount"`
	Children  []int     `json:"children,omitempty"`
}

// CPUProfile is a CDP Profiler.Profile.
type CPUProfile struct {
	Nodes      []ProfileNode `json:"nodes"`
	StartTime  int64         `json:"startTime"`
	EndTime    int64         `json:"endTime"`
	Samples    []int         `json:"samples"`
	TimeDeltas []int64       `json:"timeDeltas"`
}

// StackFrame is a WebKit ScriptProfiler.StackFrame (1-based line and
// column).
type StackFrame struct {
	SourceID string `json:"sourceID"`
	Name     string `json:"name"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	URL      string `json:"url"`
}

// StackSample is a WebKit ScriptProfiler.StackTrace: a timestamp in
// seconds and the frames, innermost first.
type StackSample struct {
	Timestamp   float64      `json:"timestamp"`
	StackFrames []StackFrame `json:"stackFrames"`
}

// ConvertSamples builds a CPU profile from ScriptProfiler samples. Each
// sample's frames (innermost first) become a path from the root; the
// innermost frame's node gets the hit. A sample without frames counts for
// "(program)". start and end are the tracking start and completion times
// in seconds (0: the first and last sample).
func ConvertSamples(samples []StackSample, start, end float64) CPUProfile {
	type key struct {
		parent int
		frame  CallFrame
	}
	us := func(sec float64) int64 { return int64(math.Round(sec * 1e6)) }
	p := CPUProfile{
		Nodes:   []ProfileNode{{ID: 1, CallFrame: CallFrame{FunctionName: "(root)", ScriptID: "0", LineNumber: -1, ColumnNumber: -1}}},
		Samples: []int{}, TimeDeltas: []int64{},
	}
	index := map[int]int{1: 0} // node id -> position in Nodes
	child := map[key]int{}
	node := func(parent int, f CallFrame) int {
		k := key{parent, f}
		if id, ok := child[k]; ok {
			return id
		}
		id := len(p.Nodes) + 1
		p.Nodes = append(p.Nodes, ProfileNode{ID: id, CallFrame: f})
		index[id] = len(p.Nodes) - 1
		pi := index[parent]
		p.Nodes[pi].Children = append(p.Nodes[pi].Children, id)
		child[k] = id
		return id
	}
	if len(samples) > 0 {
		if start == 0 {
			start = samples[0].Timestamp
		}
		if end == 0 {
			end = samples[len(samples)-1].Timestamp
		}
	}
	p.StartTime, p.EndTime = us(start), us(end)
	last := p.StartTime
	for _, s := range samples {
		cur := 1
		if len(s.StackFrames) == 0 {
			cur = node(1, CallFrame{FunctionName: "(program)", ScriptID: "0", LineNumber: -1, ColumnNumber: -1})
		}
		for i := len(s.StackFrames) - 1; i >= 0; i-- {
			f := s.StackFrames[i]
			name := f.Name
			if name == "" {
				name = "(anonymous)"
			}
			cur = node(cur, CallFrame{FunctionName: name, ScriptID: f.SourceID, URL: f.URL, LineNumber: f.Line - 1, ColumnNumber: f.Column - 1})
		}
		p.Nodes[index[cur]].HitCount++
		p.Samples = append(p.Samples, cur)
		t := us(s.Timestamp)
		p.TimeDeltas = append(p.TimeDeltas, t-last)
		last = t
	}
	if p.EndTime < last {
		p.EndTime = last
	}
	return p
}

// profileRec is a running "profiler start" recording. It takes the events
// of its own ScriptProfiler run only: trackingStart after its
// startTracking was sent (armed), and trackingComplete after that
// trackingStart (started). The trackingComplete of a run that was going
// before (the reset of a release's run, or a run that startScriptRun
// stops) is not its.
type profileRec struct {
	mu       sync.Mutex
	armed    bool
	started  bool
	start    float64
	done     chan struct{}
	end      float64
	samples  []StackSample
	unsub    []func()
	complete bool
}

// arm notes that the recording's startTracking is about to be sent.
func (r *profileRec) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed, r.started = true, false
}

// StartProfiler starts ScriptProfiler sampling for session (owner
// "profiler:<session>"); a release stops it.
func (p *Page) StartProfiler(ctx context.Context, c webkit.Caller, session string) error {
	p.mu.Lock()
	if p.profiles[session] != nil {
		p.mu.Unlock()
		return fmt.Errorf("profiler start: session %q is already profiling; run 'profiler stop' first", session)
	}
	r := &profileRec{done: make(chan struct{})}
	p.profiles[session] = r
	p.mu.Unlock()
	sub := subscribeInOrder(p.ps.Caller)
	r.unsub = append(r.unsub,
		sub("ScriptProfiler.trackingStart", func(_ string, params json.RawMessage) {
			var v struct {
				Timestamp float64 `json:"timestamp"`
			}
			if json.Unmarshal(params, &v) == nil {
				r.mu.Lock()
				if r.armed && !r.started {
					r.started, r.start = true, v.Timestamp
				}
				r.mu.Unlock()
			}
		}),
		sub("ScriptProfiler.trackingComplete", func(_ string, params json.RawMessage) {
			var v struct {
				Timestamp float64 `json:"timestamp"`
				Samples   *struct {
					StackTraces []StackSample `json:"stackTraces"`
				} `json:"samples"`
			}
			if json.Unmarshal(params, &v) != nil {
				return
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.started || r.complete {
				return
			}
			r.end, r.complete = v.Timestamp, true
			if v.Samples != nil {
				r.samples = v.Samples.StackTraces
			}
			close(r.done)
		}))
	// The entry, whose Release stops the sampling, is recorded in a
	// desired-state change before startTracking is sent, so that a lease
	// release always stops a sampling that started (W4 round 3, review
	// finding profile.go:181). The sampling itself runs on the active
	// target only (c; the entry's Apply is a no-op). A start whose outcome
	// is unknown keeps the entry and the record. The page's ScriptProfiler
	// lifecycle (core.StartScriptProfiler) refuses the start while another
	// recording holds the run, runs a stop that a release left pending
	// first, and never joins a run that no recording holds.
	started := false
	err := p.ps.State.Change(ctx, c, func(cc webkit.Caller) error {
		var err error
		_, started, err = core.StartScriptProfiler(ctx, p.ps, cc, p.profilerEntry(session), map[string]any{"includeSamples": true}, r.arm)
		return scriptProfilerBusy("profiler start", err)
	})
	if err != nil && started {
		return fmt.Errorf("profiler start: %w; the profiler may be running: 'profiler stop' or the end of the lease stops it", err)
	}
	if err != nil {
		p.mu.Lock()
		delete(p.profiles, session)
		p.mu.Unlock()
		r.stop()
	}
	return err
}

// profilerEntry ties the sampling of session to its lease: the release
// drops the recording and stops the run (core.ScriptProfilerEntry; its
// stop is sent also when the recording is gone). The entry belongs to the
// ScriptProfiler agent: a release that cannot run the Release (it missed
// the change lock) or whose stop fails marks the agent unknown, and core's
// reset (ScriptProfiler.stopTracking) stops the run later, at the latest
// before the next start.
func (p *Page) profilerEntry(session string) core.Entry {
	return core.ScriptProfilerEntry("devtools:profiler:"+session, owner("profiler", session), func() {
		p.mu.Lock()
		cur := p.profiles[session]
		delete(p.profiles, session)
		p.mu.Unlock()
		if cur != nil {
			cur.stop()
		}
	})
}

// scriptProfilerBusy turns core's refusal of a second ScriptProfiler
// recording into what to do for op ("profiler start", "trace start").
func scriptProfilerBusy(op string, err error) error {
	var busy *core.ScriptProfilerBusyError
	if !errors.As(err, &busy) {
		return err
	}
	var what string
	kind, session, _ := strings.Cut(strings.TrimPrefix(string(busy.Owner), "devtools:"), ":")
	switch {
	case kind == "profiler":
		what = fmt.Sprintf("the profiler of session %q uses it; run 'profiler stop' first", session)
	case kind == "trace":
		what = fmt.Sprintf("the trace of session %q records JavaScript with it; run 'trace stop' first", session)
	case cdpKind(kind):
		what = cdpHolder(kind, session, "Profiler.stop or Tracing.end")
	default:
		what = fmt.Sprintf("a legacy recording of %s uses it; stop that recording first", busy.Owner)
	}
	return fmt.Errorf("%s: %w: %s, or start the trace with '--categories timeline'", op, err, what)
}

// cdpKind reports whether an owner kind is a CDP attachment's (internal/cdp
// names an attachment's owner core.SessionOwner("cdp-"+id, session)).
func cdpKind(kind string) bool { return strings.HasPrefix(kind, "cdp-") }

// cdpHolder says which CDP attachment holds an agent and how its client
// gives it up: stop, its stop command, or the detach.
func cdpHolder(kind, session, stop string) string {
	return fmt.Sprintf("a CDP client of session %q (attachment %s) records with it; stop that recording first (%s, or detach the attachment)", session, strings.TrimPrefix(kind, "cdp-"), stop)
}

// timelineBusy refuses a trace start while the recording of owner holds
// the page's Timeline.
func timelineBusy(owner core.Owner) error {
	what := "stop that recording first"
	if kind, session, _ := strings.Cut(string(owner), ":"); cdpKind(kind) {
		what = cdpHolder(kind, session, "Tracing.end")
	}
	return fmt.Errorf("trace start: the page's Timeline runs one recording at a time, and %s holds it: %s", owner, what)
}

// keepRecording puts a recording back (put, under p.mu) after a stop that
// failed, while one of its entries keys is still in the desired state, so
// that a retry finds it; when the release took the entries meanwhile, it
// ends the recording's subscriptions (end).
func (p *Page) keepRecording(keys []string, put, end func()) {
	p.mu.Lock()
	for _, k := range keys {
		if p.ps.State.HasEntry(k) {
			put()
			p.mu.Unlock()
			return
		}
	}
	p.mu.Unlock()
	end()
}

func (r *profileRec) stop() {
	r.mu.Lock()
	unsub := r.unsub
	r.unsub = nil
	r.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// StopProfiler stops sampling and returns the CPU profile; it waits for
// ScriptProfiler.trackingComplete within ctx.
func (p *Page) StopProfiler(ctx context.Context, c webkit.Caller, session string) (CPUProfile, error) {
	p.mu.Lock()
	r := p.profiles[session]
	delete(p.profiles, session)
	p.mu.Unlock()
	if r == nil {
		return CPUProfile{}, fmt.Errorf("profiler stop: session %q is not profiling; run 'profiler start' first", session)
	}
	// The entry goes only with a stop that succeeded
	// (core.StopScriptProfiler): after a failed stop the run may go on, and
	// the recording stays, so that a retry or the release stops it.
	key := "devtools:profiler:" + session
	if err := p.ps.State.Change(ctx, c, func(cc webkit.Caller) error {
		_, err := core.StopScriptProfiler(ctx, p.ps, cc, key)
		return err
	}); err != nil {
		p.keepRecording([]string{key}, func() { p.profiles[session] = r }, r.stop)
		return CPUProfile{}, fmt.Errorf("profiler stop: %w; the profiler may still be running: run 'profiler stop' again, or the end of the lease stops it", err)
	}
	defer r.stop()
	select {
	case <-r.done:
	case <-ctx.Done():
		return CPUProfile{}, fmt.Errorf("profiler stop: WebKit sent no ScriptProfiler.trackingComplete: %w", ctx.Err())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return ConvertSamples(r.samples, r.start, r.end), nil
}
