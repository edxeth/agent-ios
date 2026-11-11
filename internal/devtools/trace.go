package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Chrome trace event format (Trace Event Format, Google, "JSON Object
// Format"): {"traceEvents": [...], "displayTimeUnit": "ms"}; ts and dur in
// microseconds; ph "X" is a complete event, "I" an instant event (s: "t",
// thread scope) and "M" metadata.

// TraceEvent is one Chrome trace event.
type TraceEvent struct {
	Name string         `json:"name"`
	Cat  string         `json:"cat,omitempty"`
	Ph   string         `json:"ph"`
	Ts   float64        `json:"ts"`
	Dur  *float64       `json:"dur,omitempty"`
	Pid  int            `json:"pid"`
	Tid  int            `json:"tid"`
	S    string         `json:"s,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

// Trace is a Chrome trace file.
type Trace struct {
	TraceEvents     []TraceEvent   `json:"traceEvents"`
	DisplayTimeUnit string         `json:"displayTimeUnit"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// TimelineRecord is a WebKit Timeline record (Timeline.eventRecorded). The
// protocol declares type, data and children; the backend also sets
// startTime, endTime and stackTrace (seconds on the inspector's stopwatch).
type TimelineRecord struct {
	Type      string           `json:"type"`
	Data      map[string]any   `json:"data"`
	Children  []TimelineRecord `json:"children"`
	StartTime *float64         `json:"startTime"`
	EndTime   *float64         `json:"endTime"`
}

// ScriptEvent is a ScriptProfiler.trackingUpdate event (seconds).
type ScriptEvent struct {
	StartTime float64 `json:"startTime"`
	EndTime   float64 `json:"endTime"`
	Type      string  `json:"type"`
}

const (
	tracePid       = 1
	traceMainTid   = 1
	traceScriptTid = 2
)

// timelineName gives the trace event name of a record: its type, with the
// event or timer that it is about when the record says so.
func timelineName(r TimelineRecord) string {
	switch r.Type {
	case "EventDispatch":
		if t, ok := r.Data["type"].(string); ok && t != "" {
			return "EventDispatch " + t
		}
	case "TimeStamp", "Time", "TimeEnd", "ConsoleProfile":
		if m, ok := r.Data["message"].(string); ok && m != "" {
			return r.Type + " " + m
		}
	}
	return r.Type
}

// ConvertTimeline converts Timeline records and ScriptProfiler events into
// a Chrome trace. Times are made relative to start (seconds, the
// recordingStarted time; the earliest record when 0). A record with an end
// time becomes a complete event and its children nest inside it; a record
// without one becomes an instant event. Script events go on a second
// thread.
func ConvertTimeline(records []TimelineRecord, scripts []ScriptEvent, start float64) Trace {
	if start == 0 {
		start = earliest(records, scripts)
	}
	// Rounded to whole nanoseconds: seconds as doubles do not convert exactly.
	us := func(sec float64) float64 { return math.Round((sec-start)*1e9) / 1e3 }
	tr := Trace{DisplayTimeUnit: "ms", TraceEvents: []TraceEvent{
		{Name: "process_name", Ph: "M", Pid: tracePid, Tid: traceMainTid, Args: map[string]any{"name": "WebKit page"}},
		{Name: "thread_name", Ph: "M", Pid: tracePid, Tid: traceMainTid, Args: map[string]any{"name": "Main thread (Timeline)"}},
	}}
	var walk func(r TimelineRecord)
	walk = func(r TimelineRecord) {
		if r.StartTime != nil {
			ev := TraceEvent{Name: timelineName(r), Cat: "webkit.timeline", Pid: tracePid, Tid: traceMainTid, Ts: us(*r.StartTime)}
			if len(r.Data) > 0 {
				ev.Args = map[string]any{"data": r.Data}
			}
			if r.EndTime != nil && *r.EndTime >= *r.StartTime {
				d := us(*r.EndTime) - ev.Ts
				ev.Ph, ev.Dur = "X", &d
			} else {
				ev.Ph, ev.S = "I", "t"
			}
			tr.TraceEvents = append(tr.TraceEvents, ev)
		}
		for _, c := range r.Children {
			walk(c)
		}
	}
	for _, r := range records {
		walk(r)
	}
	if len(scripts) > 0 {
		tr.TraceEvents = append(tr.TraceEvents, TraceEvent{Name: "thread_name", Ph: "M", Pid: tracePid, Tid: traceScriptTid, Args: map[string]any{"name": "JavaScript (ScriptProfiler)"}})
		for _, s := range scripts {
			ts := us(s.StartTime)
			d := us(s.EndTime) - ts
			if d < 0 {
				d = 0
			}
			tr.TraceEvents = append(tr.TraceEvents, TraceEvent{Name: "Script " + s.Type, Cat: "webkit.script", Ph: "X", Ts: ts, Dur: &d, Pid: tracePid, Tid: traceScriptTid})
		}
	}
	// Stable order: metadata first, then by time; a parent before the
	// children that start at the same time (longer first).
	sort.SliceStable(tr.TraceEvents, func(i, j int) bool {
		a, b := tr.TraceEvents[i], tr.TraceEvents[j]
		if (a.Ph == "M") != (b.Ph == "M") {
			return a.Ph == "M"
		}
		if a.Ts != b.Ts {
			return a.Ts < b.Ts
		}
		return dur(a) > dur(b)
	})
	return tr
}

func dur(e TraceEvent) float64 {
	if e.Dur == nil {
		return 0
	}
	return *e.Dur
}

func earliest(records []TimelineRecord, scripts []ScriptEvent) float64 {
	first := 0.0
	seen := false
	note := func(v float64) {
		if !seen || v < first {
			first, seen = v, true
		}
	}
	var walk func(r TimelineRecord)
	walk = func(r TimelineRecord) {
		if r.StartTime != nil {
			note(*r.StartTime)
		}
		for _, c := range r.Children {
			walk(c)
		}
	}
	for _, r := range records {
		walk(r)
	}
	for _, s := range scripts {
		note(s.StartTime)
	}
	return first
}

// traceRec is a running "trace start" recording. It takes the Timeline
// events only after its Timeline.start was sent (armed), and the script
// events only after the trackingStart of its own ScriptProfiler run
// (scriptStarted, see profileRec): a recording or run of an earlier lease
// that the start's change stopped first (Timeline.disable,
// ScriptProfiler.stopTracking) is not its. A part takes no events after
// its stop succeeded (end): a later recording is not its either.
type traceRec struct {
	mu            sync.Mutex
	armed         bool
	scriptArmed   bool
	scriptStarted bool
	records       []TimelineRecord
	scripts       []ScriptEvent
	start         float64
	script        bool
	unsub         []func()
}

// armScript notes that the recording's startTracking is about to be sent.
func (t *traceRec) armScript() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scriptArmed, t.scriptStarted = true, false
}

// end closes the event admission of the recording's Timeline part
// (timeline) and of its ScriptProfiler part (script), whose stops
// succeeded. WebKit sends a recording's last events (Timeline.eventRecorded
// and recordingStopped, ScriptProfiler.trackingComplete) before the reply
// to its stop, and the page connection delivers events in order
// (subscribeInOrder), so the part has all its events. A later event of
// that agent belongs to a recording that started after the stop, such as
// another profiler or a legacy Timeline recording, while the other part
// of a partly stopped trace still records (W4 rebase review F4).
func (t *traceRec) end(timeline, script bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if timeline {
		t.armed = false
	}
	if script {
		t.scriptArmed, t.scriptStarted = false, false
	}
}

// traceScriptKey is the key of the entry of a trace's ScriptProfiler run.
func traceScriptKey(session string) string { return "devtools:trace-script:" + session }

// MaxTraceRecords bounds a recording; later records are dropped and counted.
const MaxTraceRecords = 200_000

// StartTrace starts a Timeline recording of session (owner
// "trace:<session>"): Timeline enabled as an owned domain, Timeline.start;
// with script, ScriptProfiler.startTracking too. A release stops it.
func (p *Page) StartTrace(ctx context.Context, c webkit.Caller, session string, script bool) error {
	p.mu.Lock()
	if p.traces[session] != nil {
		p.mu.Unlock()
		return fmt.Errorf("trace start: a trace of session %q is already recording; run 'trace stop' first", session)
	}
	t := &traceRec{script: script}
	p.traces[session] = t
	p.mu.Unlock()
	fail := func(err error) error {
		p.mu.Lock()
		delete(p.traces, session)
		p.mu.Unlock()
		t.stop()
		return err
	}
	sub := subscribeInOrder(p.ps.Caller)
	t.unsub = append(t.unsub,
		sub("Timeline.eventRecorded", func(_ string, params json.RawMessage) {
			var v struct {
				Record TimelineRecord `json:"record"`
			}
			if json.Unmarshal(params, &v) == nil {
				t.mu.Lock()
				if t.armed && len(t.records) < MaxTraceRecords {
					t.records = append(t.records, v.Record)
				}
				t.mu.Unlock()
			}
		}),
		sub("Timeline.recordingStarted", func(_ string, params json.RawMessage) {
			var v struct {
				StartTime float64 `json:"startTime"`
			}
			if json.Unmarshal(params, &v) == nil {
				t.mu.Lock()
				if t.armed {
					t.start = v.StartTime
				}
				t.mu.Unlock()
			}
		}),
		sub("ScriptProfiler.trackingStart", func(string, json.RawMessage) {
			t.mu.Lock()
			if t.scriptArmed {
				t.scriptStarted = true
			}
			t.mu.Unlock()
		}),
		sub("ScriptProfiler.trackingUpdate", func(_ string, params json.RawMessage) {
			var v struct {
				Event ScriptEvent `json:"event"`
			}
			if json.Unmarshal(params, &v) == nil {
				t.mu.Lock()
				if t.scriptStarted && len(t.scripts) < MaxTraceRecords {
					t.scripts = append(t.scripts, v.Event)
				}
				t.mu.Unlock()
			}
		}))
	o := owner("trace", session)
	// The owned Timeline domain and the entry are one desired-state
	// change (cc). The recording itself (Timeline.start and the script
	// tracking) goes to the active target only (c): the entry's Apply is
	// a no-op, so no new target gets a recording. The entry, whose Release
	// stops the recording, is recorded before the starts are sent, so that
	// a lease release always stops a recording that started (W4 round 3,
	// review finding trace.go:271). timeline and scripts say what may run
	// on the page: a start that succeeded or whose outcome is unknown.
	// A Timeline recording and a ScriptProfiler run survive a same-target
	// navigation: a recording of an earlier lease whose stop a release left
	// pending is stopped first (core.ResetUnknownAgent; for the script
	// tracking, the page's ScriptProfiler lifecycle, core.StartScriptProfiler,
	// which also refuses the start while another recording holds the run).
	// The script tracking starts first, so that such a refusal starts
	// nothing. Another recording's Timeline entry (a CDP client's
	// Tracing.start of the same session) refuses the start before anything
	// is sent: WebKit runs one Timeline recording per page, so this trace
	// would record into it, and its Timeline.stop would end the other one.
	entered, timeline, scripts := false, false, false
	err := p.ps.State.Change(ctx, c, func(cc webkit.Caller) error {
		entered = true
		if h, held := p.ps.State.DomainEntry("Timeline"); held {
			return timelineBusy(h.Owner)
		}
		if script {
			var err error
			_, scripts, err = core.StartScriptProfiler(ctx, p.ps, cc, traceScriptEntry(session, o), map[string]any{"includeSamples": false}, t.armScript)
			if err != nil {
				return scriptProfilerBusy("trace start", err)
			}
		}
		if err := core.ResetUnknownAgent(ctx, p.ps, cc, "Timeline"); err != nil {
			return fmt.Errorf("trace start: stopping the Timeline recording of an earlier lease: %w", err)
		}
		if err := acquireDomain(ctx, p.ps, cc, o, "Timeline"); err != nil {
			return err
		}
		p.ps.State.Put(p.traceEntry(session, o))
		t.mu.Lock()
		t.armed = true
		t.mu.Unlock()
		if _, err := c.Send(ctx, "Timeline.start", map[string]int{"maxCallStackDepth": 5}); err != nil {
			timeline = !noEffect(err)
			return fmt.Errorf("starting the timeline (Timeline.start): %w", err)
		}
		timeline = true
		return nil
	})
	switch {
	case err == nil:
		return nil
	case !entered:
		return fail(err)
	}
	return p.undoTraceStart(ctx, c, session, o, t, timeline, scripts, err, fail)
}

// undoTraceStart ends a StartTrace that failed after its change began (the
// active target refused a start, or a provisional target refused the
// forwarded Timeline.enable): it stops what may run on the page, then
// removes the entry and gives up the Timeline domain. When a stop fails,
// the record and the entry stay, so that "trace stop" or the lease
// release stops the recording. A part whose stop succeeded takes no more
// events (traceRec.end).
func (p *Page) undoTraceStart(ctx context.Context, c webkit.Caller, session string, o core.Owner, t *traceRec, timeline, scripts bool, err error, fail func(error) error) error {
	var stopErrs []error
	if timeline {
		if _, serr := c.Send(ctx, "Timeline.stop", nil); serr != nil {
			stopErrs = append(stopErrs, fmt.Errorf("stopping the timeline (Timeline.stop): %w", serr))
		} else {
			t.end(true, false)
		}
	}
	if scripts {
		if _, serr := c.Send(ctx, "ScriptProfiler.stopTracking", nil); serr != nil {
			stopErrs = append(stopErrs, fmt.Errorf("stopping the script tracking (ScriptProfiler.stopTracking): %w", serr))
		} else {
			t.end(false, true)
		}
	}
	if len(stopErrs) > 0 {
		return fmt.Errorf("trace start: %w; stopping the recording failed too (%w): 'trace stop' or the end of the lease stops it", err, errors.Join(stopErrs...))
	}
	cerr := p.ps.State.Change(ctx, c, func(cc webkit.Caller) error {
		p.ps.State.Remove("devtools:trace:" + session)
		p.ps.State.Remove(traceScriptKey(session))
		return releaseDomain(ctx, p.ps, cc, o, "Timeline")
	})
	if cerr != nil {
		return fail(fmt.Errorf("%w; giving up the Timeline domain: %w", err, cerr))
	}
	return fail(err)
}

// traceEntry ties the recording of session to its lease: the release
// stops it. The entry belongs to the Timeline agent, which its owner holds:
// a release that cannot run the Release, or whose stop fails, marks the
// agent unknown, and core's reset (Timeline.disable, which stops a
// recording) runs later, at the latest before the next trace start.
func (p *Page) traceEntry(session string, o core.Owner) core.Entry {
	return core.Entry{
		Key: "devtools:trace:" + session, Owner: o, Domain: "Timeline", Apply: noop,
		Release: func(ctx context.Context, s core.Sender) error {
			p.mu.Lock()
			cur := p.traces[session]
			delete(p.traces, session)
			p.mu.Unlock()
			if cur != nil {
				cur.stop()
			}
			// Also without the recording: a stop that failed kept the
			// entry, not necessarily the recording.
			_, err := s.Send(ctx, "Timeline.stop", nil)
			return err
		},
	}
}

// traceScriptEntry ties the ScriptProfiler run of a trace with script
// tracking to the lease: the release stops it. It belongs to the
// ScriptProfiler agent, as the profiler's entry does (profilerEntry), so a
// stop that a release could not send stays pending in core. Its key sorts
// before the trace entry's, so a release stops the run first.
func traceScriptEntry(session string, o core.Owner) core.Entry {
	return core.ScriptProfilerEntry(traceScriptKey(session), o, nil)
}

func (t *traceRec) stop() {
	t.mu.Lock()
	unsub := t.unsub
	t.unsub = nil
	t.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// StopTrace stops the recording of session and returns its Chrome trace.
func (p *Page) StopTrace(ctx context.Context, c webkit.Caller, session string) (Trace, int, error) {
	p.mu.Lock()
	t := p.traces[session]
	delete(p.traces, session)
	p.mu.Unlock()
	if t == nil {
		return Trace{}, 0, fmt.Errorf("trace stop: no trace of session %q is recording; run 'trace start' first", session)
	}
	o := owner("trace", session)
	// Each entry goes only with a stop of its part that succeeded: after a
	// failed stop the part may run on, and the recording stays, so that a
	// retry (which sends only the stops still owed) or the release stops it.
	// A part whose stop is no longer owed takes no more events (end),
	// before the change ends and another recording can start.
	timelineKey, scriptKey := "devtools:trace:"+session, traceScriptKey(session)
	err := p.ps.State.Change(ctx, c, func(cc webkit.Caller) error {
		var errs []error
		if p.ps.State.HasEntry(timelineKey) {
			if _, err := c.Send(ctx, "Timeline.stop", nil); err != nil {
				errs = append(errs, fmt.Errorf("stopping the timeline (Timeline.stop): %w", err))
			} else {
				p.ps.State.Remove(timelineKey)
			}
		}
		if p.ps.State.HasEntry(scriptKey) {
			if _, err := core.StopScriptProfiler(ctx, p.ps, cc, scriptKey); err != nil {
				errs = append(errs, fmt.Errorf("stopping the script tracking: %w", err))
			}
		}
		t.end(!p.ps.State.HasEntry(timelineKey), !p.ps.State.HasEntry(scriptKey))
		return errors.Join(errs...)
	})
	if err != nil {
		p.keepRecording([]string{timelineKey, scriptKey}, func() { p.traces[session] = t }, t.stop)
		return Trace{}, 0, fmt.Errorf("trace stop: %w; the recording may still be running: run 'trace stop' again, or the end of the lease stops it", err)
	}
	t.stop()
	if err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error { return releaseDomain(ctx, p.ps, c, o, "Timeline") }); err != nil {
		return Trace{}, 0, fmt.Errorf("trace stop: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return ConvertTimeline(t.records, t.scripts, t.start), len(t.records), nil
}
