//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The backend premise of the pending ScriptProfiler stop (W4 rebase review,
// finding F2; docs/notes/w4.md "Profiler and trace leftovers"), on the
// suite's real page connection: a ScriptProfiler run survives a
// same-target document navigation, and a second startTracking while it
// runs starts no new run (no trackingStart; WebKit's
// InspectorScriptProfilerAgent::startTracking returns at once while it
// tracks). stopTracking ends the run (trackingComplete), and the next
// startTracking starts a new one. A Timeline recording survives the
// navigation too, and Timeline.disable, the reset core sends for an
// unknown Timeline agent, stops it.
//
// This is the reviewer's reproduction, which asserted the opposite
// premise and failed: "trackingStart=1, trackingComplete=0" after the
// navigation (/home/devkit/.pi/artifacts/reviewer/w4-rebase-evidence/
// profiler-navigation.log).
func TestScriptProfilerTrackingSurvivesSameTargetNavigation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var starts, stops, recStarts, recStops atomic.Int32
	count := func(n *atomic.Int32) func(string, json.RawMessage) {
		return func(string, json.RawMessage) { n.Add(1) }
	}
	for _, u := range []func(){
		sess.SubscribeSync("ScriptProfiler.trackingStart", count(&starts)),
		sess.SubscribeSync("ScriptProfiler.trackingComplete", count(&stops)),
		sess.SubscribeSync("Timeline.recordingStarted", count(&recStarts)),
		sess.SubscribeSync("Timeline.recordingStopped", count(&recStops)),
	} {
		defer u()
	}
	defer func() {
		_, _ = sess.Send(context.Background(), "ScriptProfiler.stopTracking", nil)
		_, _ = sess.Send(context.Background(), "Timeline.disable", nil)
	}()
	send := func(method string, params any) {
		t.Helper()
		if _, err := sess.Send(ctx, method, params); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	target := sess.ActiveTarget()
	send("ScriptProfiler.startTracking", map[string]bool{"includeSamples": true})
	send("Timeline.enable", nil)
	send("Timeline.start", nil)
	if starts.Load() != 1 || recStarts.Load() != 1 {
		t.Fatalf("setup: start events: trackingStart %d, recordingStarted %d; want 1 and 1", starts.Load(), recStarts.Load())
	}
	navigate(t, "forms.html?profiler-after-navigation", "")
	if sess.ActiveTarget() != target {
		t.Fatalf("setup: the navigation changed the target %s -> %s; the premise is about one target", target, sess.ActiveTarget())
	}
	send("ScriptProfiler.startTracking", map[string]bool{"includeSamples": true})
	t.Logf("same target %s after a document navigation: trackingStart=%d, trackingComplete=%d, recordingStarted=%d, recordingStopped=%d",
		target, starts.Load(), stops.Load(), recStarts.Load(), recStops.Load())
	if starts.Load() != 1 || stops.Load() != 0 {
		t.Fatalf("the navigation ended the ScriptProfiler run or a second start began a new one (trackingStart %d, trackingComplete %d; want 1 and 0): the pending stop of a release may be unnecessary, revisit docs/notes/w4.md",
			starts.Load(), stops.Load())
	}
	if recStops.Load() != 0 {
		t.Fatalf("the navigation stopped the Timeline recording (recordingStopped %d)", recStops.Load())
	}
	send("ScriptProfiler.stopTracking", nil)
	if stops.Load() != 1 {
		t.Fatalf("stopTracking did not end the surviving run: trackingComplete %d, want 1", stops.Load())
	}
	send("ScriptProfiler.startTracking", map[string]bool{"includeSamples": true})
	if starts.Load() != 2 {
		t.Fatalf("a start after stopTracking began no new run: trackingStart %d, want 2", starts.Load())
	}
	send("Timeline.disable", nil)
	if recStops.Load() != 1 {
		t.Fatalf("Timeline.disable did not stop the surviving recording: recordingStopped %d, want 1", recStops.Load())
	}
}

// W4 rebase review, finding F2, through the daemon's core and devtools on
// a real page: session a starts the profiler, or a trace with script
// tracking, and runs a hot function; its lease release misses the change
// lock, so its cleanup (ScriptProfiler.stopTracking) is not sent; the page
// then navigates on the same target. The next lease's profiler must start
// a new ScriptProfiler run (a second trackingStart) whose profile holds
// the hot function of the next lease and none of session a's, and the
// next lease's script trace starts a new run too.
func TestNextLeaseSamplesNothingOfAReleaseWithoutCleanup(t *testing.T) {
	for _, first := range []string{"profiler", "trace --script"} {
		t.Run(first, func(t *testing.T) {
			m, req := managedBrowser(t, origin+"/forms.html")
			var ps *core.PageSession
			do := func(session string, fn func(ctx context.Context, p *devtools.Page, c webkit.Caller) error) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				err := m.Do(ctx, req, core.AcquireOptions{Session: session}, "test", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
					ps = mo.Page.PS
					p, err := devtools.For(mo.Page.PS)
					if err != nil {
						return err
					}
					return fn(ctx, p, mo.Op.Caller())
				})
				if err != nil {
					t.Fatalf("session %s: %v", session, err)
				}
			}
			evalIn := func(ctx context.Context, c webkit.Caller, expr string) (string, error) {
				raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "String(" + expr + ")", "returnByValue": true})
				if err != nil {
					return "", err
				}
				var r struct {
					Result struct {
						Value string `json:"value"`
					} `json:"result"`
				}
				err = json.Unmarshal(raw, &r)
				return r.Result.Value, err
			}
			// hot runs a named function for 300 ms, so that the sampling
			// profiler takes samples with its frame.
			hot := func(name string) func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
				return func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
					_, err := evalIn(ctx, c, fmt.Sprintf("(function %s() { const end = Date.now() + 300; let x = 0; while (Date.now() < end) x += Math.sqrt(x + 1); return x; })()", name))
					return err
				}
			}
			var starts, stops atomic.Int32
			do("a", func(context.Context, *devtools.Page, webkit.Caller) error {
				ss := ps.Caller.(interface {
					SubscribeSync(string, webkit.EventHandler) func()
				})
				t.Cleanup(ss.SubscribeSync("ScriptProfiler.trackingStart", func(string, json.RawMessage) { starts.Add(1) }))
				t.Cleanup(ss.SubscribeSync("ScriptProfiler.trackingComplete", func(string, json.RawMessage) { stops.Add(1) }))
				return nil
			})
			do("a", func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
				if first == "profiler" {
					return p.StartProfiler(ctx, c, "a")
				}
				return p.StartTrace(ctx, c, "a", true)
			})
			do("a", hot("w4OldLeaseHotLoop"))
			if starts.Load() != 1 {
				t.Fatalf("setup: session a's start: trackingStart %d, want 1", starts.Load())
			}

			// The release of a misses the change lock: its cleanup is not sent.
			inside, unblock, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				held <- ps.State.Change(context.Background(), ps.Caller, func(webkit.Caller) error {
					close(inside)
					<-unblock
					return nil
				})
			}()
			<-inside
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_, err := m.ReleaseSession(ctx, "a")
			cancel()
			if err == nil {
				t.Fatal("setup: the release got the change lock")
			}
			close(unblock)
			if err := <-held; err != nil {
				t.Fatal(err)
			}

			// A same-target document navigation.
			target := ps.Caller.(interface{ ActiveTarget() string }).ActiveTarget()
			url := origin + "/forms.html?after-release"
			do("b", func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
				_, _ = c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("location.href = %q", url)})
				return nil
			})
			loaded := false
			for end := time.Now().Add(10 * time.Second); !loaded && time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
				do("b", func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
					v, _ := evalIn(ctx, c, fmt.Sprintf("location.href === %q && document.readyState === 'complete'", url))
					loaded = v == "true"
					return nil
				})
			}
			if !loaded {
				t.Fatalf("setup: the page did not load %s", url)
			}
			if now := ps.Caller.(interface{ ActiveTarget() string }).ActiveTarget(); now != target {
				t.Fatalf("setup: the navigation changed the target %s -> %s", target, now)
			}

			// The next lease's profiler.
			do("b", func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
				return p.StartProfiler(ctx, c, "b")
			})
			do("b", hot("w4NewLeaseHotLoop"))
			var prof devtools.CPUProfile
			do("b", func(ctx context.Context, p *devtools.Page, c webkit.Caller) (err error) {
				prof, err = p.StopProfiler(ctx, c, "b")
				return err
			})
			names := map[string]int{}
			for _, n := range prof.Nodes {
				names[n.CallFrame.FunctionName] += n.HitCount
			}
			t.Logf("after session a's %s: trackingStart %d, trackingComplete %d; next lease's profile: %d samples, w4OldLeaseHotLoop %d hits, w4NewLeaseHotLoop %d hits",
				first, starts.Load(), stops.Load(), len(prof.Samples), names["w4OldLeaseHotLoop"], names["w4NewLeaseHotLoop"])
			if starts.Load() != 2 {
				t.Errorf("the next lease's profiler started no new ScriptProfiler run: trackingStart %d, want 2 (it reused session a's run)", starts.Load())
			}
			if _, ok := names["w4OldLeaseHotLoop"]; ok {
				t.Errorf("the next lease's profile holds %d samples of session a's w4OldLeaseHotLoop", names["w4OldLeaseHotLoop"])
			}
			if names["w4NewLeaseHotLoop"] == 0 {
				t.Errorf("the next lease's profile holds no sample of its own w4NewLeaseHotLoop (%d samples in all)", len(prof.Samples))
			}

			// The next lease's script trace starts a new run too.
			do("b", func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
				return p.StartTrace(ctx, c, "b", true)
			})
			// A timer callback is a script call that the ScriptProfiler
			// reports (trackingUpdate); an inspector evaluation is not.
			do("b", func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
				_, err := evalIn(ctx, c, "setTimeout(function w4TraceHotLoop() { const end = Date.now() + 50; while (Date.now() < end); window.__w4TimerRan = true; }, 0)")
				return err
			})
			ran := false
			for end := time.Now().Add(5 * time.Second); !ran && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
				do("b", func(ctx context.Context, _ *devtools.Page, c webkit.Caller) error {
					v, err := evalIn(ctx, c, "window.__w4TimerRan === true")
					ran = v == "true"
					return err
				})
			}
			var tr devtools.Trace
			do("b", func(ctx context.Context, p *devtools.Page, c webkit.Caller) (err error) {
				tr, _, err = p.StopTrace(ctx, c, "b")
				return err
			})
			scripts := 0
			for _, ev := range tr.TraceEvents {
				if strings.HasPrefix(ev.Name, "Script ") {
					scripts++
				}
			}
			if starts.Load() != 3 || scripts == 0 {
				t.Errorf("the next lease's script trace: trackingStart %d (want 3), %d script events (want some)", starts.Load(), scripts)
			}
		})
	}
}
