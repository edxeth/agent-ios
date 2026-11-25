package devtools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 rebase repair round 1, review finding F2. A ScriptProfiler run
// survives a same-target navigation, and WebKit's startTracking starts no
// new run while one tracks (InspectorScriptProfilerAgent; reproduced on
// WebKitGTK, e2e/webkitgtk/profiler_navigation_test.go). The fake models
// that agent (fakewebkit/scriptprofiler.go).

// scriptStarts counts the ScriptProfiler.startTracking calls that the fake
// received after mark while its agent was tracking already: starts that
// joined an earlier run instead of starting a new one. It replays the
// fake's log, in which a received call comes before the events its
// handling emits.
func scriptStarts(f *fakewebkit.Server, mark int) (joined int) {
	tracking := false
	for _, rec := range f.Log() {
		switch {
		case !rec.In && rec.Method == "ScriptProfiler.trackingStart":
			tracking = true
		case !rec.In && rec.Method == "ScriptProfiler.trackingComplete":
			tracking = false
		case rec.In && rec.Method == "ScriptProfiler.startTracking" && rec.Seq > mark && tracking:
			joined++
		}
	}
	return joined
}

// firstAfter returns the sequence number of the first call of method that
// the fake received after mark, or 0.
func firstAfter(f *fakewebkit.Server, method string, mark int) int {
	if got := receivedAfter(f, method, mark); len(got) > 0 {
		return got[0].Seq
	}
	return 0
}

// profileNames returns the hit count of each function in a profile.
func profileNames(p CPUProfile) map[string]int {
	out := map[string]int{}
	for _, n := range p.Nodes {
		out[n.CallFrame.FunctionName] += n.HitCount
	}
	return out
}

// A release whose cleanup could not take the change lock leaves session
// s1's sampling running; the page then navigates on the same target. The
// stop of that run stays pending in core (the ScriptProfiler agent is
// marked unknown) and is sent before the next lease samples: by core's
// background reset, or, when that reset failed, by the next start itself.
// No start of the next lease joins the old run, and its profile holds no
// sample of s1. The Timeline recording of a trace is stopped
// (Timeline.disable) before the next Timeline.start too.
func TestReleaseWithoutCleanupStopsScriptSamplingBeforeTheNextLease(t *testing.T) {
	start := map[string]func(ctx context.Context, p *Page, c webkit.Caller, s string) error{
		"profiler": func(ctx context.Context, p *Page, c webkit.Caller, s string) error { return p.StartProfiler(ctx, c, s) },
		"trace --script": func(ctx context.Context, p *Page, c webkit.Caller, s string) error {
			return p.StartTrace(ctx, c, s, true)
		},
	}
	for _, first := range []string{"profiler", "trace --script"} {
		for _, next := range []string{"profiler", "trace --script"} {
			for _, by := range []string{"background reset", "next start"} {
				t.Run(first+"/"+next+"/"+by, func(t *testing.T) {
					r := newRig(t)
					r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return start[first](ctx, p, c, "s1") })
					r.f.ProfilerSample("oldLease")
					p := r.page(t)
					inside, unblock, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
					go func() {
						held <- p.ps.State.Change(context.Background(), p.ps.Caller, func(webkit.Caller) error {
							close(inside)
							<-unblock
							return nil
						})
					}()
					<-inside
					ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					_, err := r.m.ReleaseSession(ctx, "s1")
					cancel()
					if err == nil {
						t.Fatal("setup: the release got the change lock")
					}
					mark := lastSeq(r.f)
					var refused, timelineRefused atomic.Bool
					if by == "next start" {
						// The background reset's stops fail; the run and the
						// recording go on.
						r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
							refused.Store(true)
							r.f.Handle("ScriptProfiler.stopTracking", nil) // the agent's model again
							return nil, &fakewebkit.Error{Code: -32000, Message: "stop refused (test)"}
						})
						if first == "trace --script" {
							r.f.Handle("Timeline.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
								timelineRefused.Store(true)
								// Then, as WebKit stops a running recording: its
								// last record and recordingStopped, before the reply.
								r.f.Handle("Timeline.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
									r.f.Handle("Timeline.disable", nil)
									r.f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "TimerFire", "data": map[string]any{"oldLease": true}, "startTime": 1.0, "children": []any{}}})
									r.f.Emit("Timeline.recordingStopped", map[string]any{"endTime": 1.5})
									return nil, nil
								})
								return nil, &fakewebkit.Error{Code: -32000, Message: "disable refused (test)"}
							})
						}
					}
					close(unblock)
					if err := <-held; err != nil {
						t.Fatal(err)
					}
					if by == "next start" {
						eventually(t, "core's background reset of the ScriptProfiler", refused.Load)
						if first == "trace --script" {
							eventually(t, "core's background reset of the Timeline", timelineRefused.Load)
						}
						if !r.f.Tracking("page-1") {
							t.Fatal("setup: the refused stop ended the run")
						}
					} else {
						eventually(t, "core's background reset stops s1's run", func() bool { return !r.f.Tracking("page-1") })
					}
					r.f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "f1", "loaderId": "l2", "url": "https://fixture.example/next"}})
					r.f.ProfilerSample("oldLease")

					r.ao.Session = "s2"
					r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return start[next](ctx, p, c, "s2") })
					if n := scriptStarts(r.f, mark); n != 0 {
						t.Fatalf("%d ScriptProfiler.startTracking of the next lease joined s1's run: its stop was not sent first", n)
					}
					if first == "trace --script" && next == "trace --script" {
						// One Timeline.disable that took effect before the
						// next Timeline.start: after the refused one, when
						// the background reset failed.
						want := 1
						if by == "next start" {
							want = 2
						}
						start := firstAfter(r.f, "Timeline.start", mark)
						disables := 0
						for _, rec := range receivedAfter(r.f, "Timeline.disable", mark) {
							if rec.Seq < start {
								disables++
							}
						}
						if disables < want {
							t.Fatalf("s1's Timeline recording was not stopped before the next Timeline.start: %d Timeline.disable before it, want %d", disables, want)
						}
						var tr Trace
						r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) (err error) {
							tr, _, err = p.StopTrace(ctx, c, "s2")
							return err
						})
						for _, ev := range tr.TraceEvents {
							if ev.Name == "TimerFire" {
								t.Fatalf("the next lease's trace holds a record of s1's recording: %+v", ev)
							}
						}
					}
					if next != "profiler" {
						return
					}
					r.f.ProfilerSample("newLease")
					var prof CPUProfile
					r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) (err error) {
						prof, err = p.StopProfiler(ctx, c, "s2")
						return err
					})
					names := profileNames(prof)
					if names["oldLease"] != 0 || names["newLease"] != 1 {
						t.Fatalf("the next lease's profile: %v; want one sample of newLease and none of s1's oldLease", names)
					}
				})
			}
		}
	}
}

// A start never joins a run that was already tracking: a run that nobody
// here owns (here one that a raw call started) is stopped, and the start
// is sent again. A backend that still starts no new run fails the start.
func TestScriptStartStopsARunThatWasAlreadyTracking(t *testing.T) {
	leftover := func(t *testing.T, r *rig) int {
		r.must(t, func(ctx context.Context, _ *Page, c webkit.Caller) error {
			_, err := c.Send(ctx, "ScriptProfiler.startTracking", map[string]bool{"includeSamples": true})
			return err
		})
		r.f.ProfilerSample("leftover")
		return lastSeq(r.f)
	}
	t.Run("profiler", func(t *testing.T) {
		r := newRig(t)
		mark := leftover(t, r)
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
		if s, st := firstAfter(r.f, "ScriptProfiler.stopTracking", mark), len(receivedAfter(r.f, "ScriptProfiler.startTracking", mark)); s == 0 || st != 2 {
			t.Fatalf("the leftover run was not stopped and the start not sent again: stopTracking at %d, %d starts", s, st)
		}
		r.f.ProfilerSample("mine")
		var prof CPUProfile
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) (err error) {
			prof, err = p.StopProfiler(ctx, c, "s1")
			return err
		})
		if names := profileNames(prof); names["leftover"] != 0 || names["mine"] != 1 {
			t.Fatalf("profile %v; want one sample of mine and none of the leftover run", names)
		}
	})
	t.Run("trace --script", func(t *testing.T) {
		r := newRig(t)
		mark := leftover(t, r)
		r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
		if s, st := firstAfter(r.f, "ScriptProfiler.stopTracking", mark), len(receivedAfter(r.f, "ScriptProfiler.startTracking", mark)); s == 0 || st != 2 {
			t.Fatalf("the leftover run was not stopped and the start not sent again: stopTracking at %d, %d starts", s, st)
		}
		p := r.page(t)
		p.mu.Lock()
		tr := p.traces["s1"]
		p.mu.Unlock()
		r.f.Emit("ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"startTime": 1, "endTime": 2, "type": "API"}})
		eventually(t, "the trace's script event", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return len(tr.scripts) == 1 })
	})
	t.Run("a backend that starts no new run", func(t *testing.T) {
		r := newRig(t)
		r.f.Handle("ScriptProfiler.startTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) { return nil, nil })
		err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
		if err == nil || !strings.Contains(err.Error(), "no new run") {
			t.Fatalf("profiler start without trackingStart: %v; want an error that names the missing run", err)
		}
	})
}

// WebKit runs one ScriptProfiler recording per page: the profiler and a
// trace with script tracking cannot share it (the trace's run has no
// samples, and the first stop would end both), so the second start is
// refused with what to do. A Timeline-only trace runs beside the profiler.
func TestOneScriptProfilerRecordingAtATime(t *testing.T) {
	r := newRig(t)
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
	if err == nil || !strings.Contains(err.Error(), "trace stop") {
		t.Fatalf("profiler start during a script trace: %v; want a refusal that names 'trace stop'", err)
	}
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		if _, _, err := p.StopTrace(ctx, c, "s1"); err != nil {
			return err
		}
		return p.StartProfiler(ctx, c, "s1")
	})
	err = r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
	if err == nil || !strings.Contains(err.Error(), "profiler stop") {
		t.Fatalf("script trace start during the profiler: %v; want a refusal that names 'profiler stop'", err)
	}
	r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", false) })
	if n := scriptStarts(r.f, 0); n != 0 {
		t.Fatalf("%d starts joined a running ScriptProfiler run", n)
	}
}
