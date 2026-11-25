package devtools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 rebase review F4 (round 2): after a partial "trace stop", the part
// that stopped takes no more events, so the retry returns only the events
// of this trace; the unfinished part still records until its own stop,
// and the retry sends only the stop still owed.

// traceEventNames returns the names of the trace's events.
func traceEventNames(tr Trace) map[string]bool {
	names := map[string]bool{}
	for _, ev := range tr.TraceEvents {
		names[ev.Name] = true
	}
	return names
}

// emitAndWait emits method on the fake and waits until the page connection
// delivered it to its read-loop subscribers.
func emitAndWait(t *testing.T, r *rig, method string, params any) {
	t.Helper()
	var delivered atomic.Bool
	ss := r.page(t).ps.Caller.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	})
	defer ss.SubscribeSync(method, func(string, json.RawMessage) { delivered.Store(true) })()
	r.f.Emit(method, params)
	eventually(t, method+" delivered", delivered.Load)
}

// Timeline.stop fails and the script stop succeeds: a profiler that starts
// next (the trace no longer holds the ScriptProfiler run) keeps its script
// events out of the old trace, whose Timeline recording still records.
func TestPartiallyStoppedTraceIgnoresNextRecordingsScriptEvents(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
	p := r.page(t)
	emitAndWait(t, r, "ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"type": "OwnScript", "startTime": 1, "endTime": 1.5}})
	r.f.Handle("Timeline.stop", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Handle("Timeline.stop", nil)
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate Timeline stop refusal"}
	})
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, _, err := p.StopTrace(ctx, c, "s1")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "Timeline.stop") {
		t.Fatalf("setup: the first stop should fail on Timeline only, got %v", err)
	}
	if p.ps.State.HasEntry(traceScriptKey("s1")) || r.f.Tracking("page-1") {
		t.Fatal("setup: the script part did not stop")
	}
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
	emitAndWait(t, r, "ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"type": "NewProfilerOnly", "startTime": 2, "endTime": 3}})
	emitAndWait(t, r, "Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "OwnTimelineAfterPartialStop", "startTime": 2.5, "endTime": 2.75, "children": []any{}}})
	mark := lastSeq(r.f)
	var tr Trace
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) (err error) {
		tr, _, err = p.StopTrace(ctx, c, "s1")
		return err
	})
	names := traceEventNames(tr)
	if names["Script NewProfilerOnly"] {
		t.Error("the partly stopped trace took the next profiler's script event")
	}
	for _, own := range []string{"Script OwnScript", "OwnTimelineAfterPartialStop"} {
		if !names[own] {
			t.Errorf("the trace lost its own event %q: %v", own, names)
		}
	}
	if got := len(receivedAfter(r.f, "Timeline.stop", mark)); got != 1 {
		t.Errorf("the retry sent Timeline.stop %d times; want 1, the stop still owed", got)
	}
	if got := len(receivedAfter(r.f, "ScriptProfiler.stopTracking", mark)); got != 0 {
		t.Errorf("the retry sent ScriptProfiler.stopTracking %d times; the script part had stopped", got)
	}
	if !r.f.Tracking("page-1") {
		t.Error("the trace retry stopped the next profiler's run")
	}
}

// The script stop fails and Timeline.stop succeeds: a legacy Timeline
// recording that starts next keeps its records out of the old trace,
// whose ScriptProfiler run still records.
func TestPartiallyStoppedTraceIgnoresNextRecordingsTimelineEvents(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
	p := r.page(t)
	emitAndWait(t, r, "Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "OwnTimeline", "startTime": 1, "endTime": 1.5, "children": []any{}}})
	r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Handle("ScriptProfiler.stopTracking", nil)
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate script stop refusal"}
	})
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, _, err := p.StopTrace(ctx, c, "s1")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "script stop refusal") {
		t.Fatalf("setup: the first stop should fail on ScriptProfiler only, got %v", err)
	}
	if p.ps.State.HasEntry("devtools:trace:s1") || !p.ps.State.HasEntry(traceScriptKey("s1")) {
		t.Fatal("setup: the Timeline part did not stop on its own")
	}
	rawReview(t, r, "Timeline.enable", "{}")
	rawReview(t, r, "Timeline.start", "{}")
	emitAndWait(t, r, "Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "NewTimelineOnly", "startTime": 2, "endTime": 3, "children": []any{}}})
	emitAndWait(t, r, "ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"type": "OwnScriptAfterPartialStop", "startTime": 2.5, "endTime": 2.75}})
	mark := lastSeq(r.f)
	var tr Trace
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) (err error) {
		tr, _, err = p.StopTrace(ctx, c, "s1")
		return err
	})
	names := traceEventNames(tr)
	if names["NewTimelineOnly"] {
		t.Error("the partly stopped trace took the next Timeline recording's record")
	}
	for _, own := range []string{"OwnTimeline", "Script OwnScriptAfterPartialStop"} {
		if !names[own] {
			t.Errorf("the trace lost its own event %q: %v", own, names)
		}
	}
	if got := len(receivedAfter(r.f, "ScriptProfiler.stopTracking", mark)); got != 1 {
		t.Errorf("the retry sent ScriptProfiler.stopTracking %d times; want 1, the stop still owed", got)
	}
	if got := len(receivedAfter(r.f, "Timeline.stop", mark)); got != 0 {
		t.Errorf("the retry sent Timeline.stop %d times; the Timeline part had stopped", got)
	}
	rawReview(t, r, "Timeline.stop", "{}")
	rawReview(t, r, "Timeline.disable", "{}")
}
