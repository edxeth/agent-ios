package fakewebkit

import "encoding/json"

// The ScriptProfiler agent of each target, as WebKit's
// InspectorScriptProfilerAgent (JavaScriptCore/inspector/agents) runs it:
// at most one tracking run. ScriptProfiler.startTracking while the agent
// tracks returns at once and starts nothing (no trackingStart, and its
// includeSamples is ignored); ScriptProfiler.stopTracking while it does not
// track returns at once (no trackingComplete). Otherwise startTracking
// emits ScriptProfiler.trackingStart, and stopTracking emits
// ScriptProfiler.trackingComplete with the run's samples (for a run
// started with includeSamples), both before the reply. A run survives a
// document navigation of its target; it ends without trackingComplete when
// the target goes or the last connection closes (the backend's
// willDestroyFrontendAndBackend). A Handle of either method replaces this
// model for that method.

// profilerRun is the tracking run of one target.
type profilerRun struct {
	samples bool
	traces  []any
}

// ProfilerSample adds one sample, whose stack is frames (function names,
// innermost first), to the run of every target that tracks with
// includeSamples, as the sampling profiler takes it while a function runs.
func (s *Server) ProfilerSample(frames ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profClock += 0.001
	stack := make([]any, len(frames))
	for i, f := range frames {
		stack[i] = map[string]any{"sourceID": "1", "name": f, "line": 1, "column": 1, "url": "https://fixture.example/app.js"}
	}
	for _, r := range s.profiling {
		if r.samples {
			r.traces = append(r.traces, map[string]any{"timestamp": s.profClock, "stackFrames": stack})
		}
	}
}

// Tracking reports whether the ScriptProfiler agent of target tracks.
func (s *Server) Tracking(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profiling[target] != nil
}

// scriptProfiler runs the model for call (a startTracking or stopTracking
// without a handler) and emits its event before the reply.
func (s *Server) scriptProfiler(call Call) {
	s.mu.Lock()
	if s.profiling == nil {
		s.profiling = map[string]*profilerRun{}
	}
	s.profClock += 0.001
	now, run := s.profClock, s.profiling[call.Target]
	var event string
	var params map[string]any
	switch {
	case call.Method == "ScriptProfiler.startTracking" && run == nil:
		var p struct {
			IncludeSamples bool `json:"includeSamples"`
		}
		_ = json.Unmarshal(call.Params, &p) // checkParams ran before
		s.profiling[call.Target] = &profilerRun{samples: p.IncludeSamples}
		event, params = "ScriptProfiler.trackingStart", map[string]any{"timestamp": now}
	case call.Method == "ScriptProfiler.stopTracking" && run != nil:
		delete(s.profiling, call.Target)
		params = map[string]any{"timestamp": now}
		if run.samples {
			params["samples"] = map[string]any{"stackTraces": append([]any{}, run.traces...)}
		}
		event = "ScriptProfiler.trackingComplete"
	}
	s.mu.Unlock()
	if event != "" {
		s.EmitFrom(call.Target, event, params)
	}
}
