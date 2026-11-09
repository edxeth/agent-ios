package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W4 parity of the CDP recording lifecycle (W5c-1b finish brief, item
// 2). Each case has a W4 counterpart in internal/devtools: (a) a start
// that fails after its start was sent is undone (undoTraceStart,
// TestTraceStartForwardingFailureStopsTheRecording); (b) a start whose
// outcome is unknown is stopped at the release
// (TestTraceStartWithAnUnknownOutcomeIsStoppedAtRelease,
// TestProfilerStartRecordsItsCleanupBeforeTheSend); (c) a partly stopped
// trace ignores the events of the next recording
// (trace_partial_stop_r3_test.go); (d) a held ScriptProfiler names its
// holder (scriptprofiler_r1_test.go TestOneScriptProfilerRecordingAtATime);
// (e) a CLI trace and a CDP trace on one page.

// pageSessionOf is the page session of the rig's page title.
func (r *rig) pageSessionOf(t *testing.T, title string) *core.PageSession {
	t.Helper()
	for _, ps := range r.m.Status() {
		if ps.Title == title {
			return r.m.LookupPage(ps.Key).PS
		}
	}
	t.Fatalf("no page %q", title)
	return nil
}

// cliDo runs fn as the daemon runs a W4 CLI command of session s1 on page
// One: in one exclusive operation under the session's lease, with the
// page's devtools.Page.
func (r *rig) cliDo(t *testing.T, fn func(ctx context.Context, p *devtools.Page, c webkit.Caller) error) error {
	t.Helper()
	req := r.req
	req.Page = "One"
	mo, err := r.m.Begin(ctxFor(t, 10*time.Second), req, core.AcquireOptions{Session: "s1", Reservation: "o1"}, "cli", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	p, err := devtools.For(r.m.LookupPage(mo.Page.Key).PS)
	if err != nil {
		t.Fatal(err)
	}
	return fn(mo.Op.Context(), p, mo.Op.Caller())
}

func cliTraceStart(script bool) func(context.Context, *devtools.Page, webkit.Caller) error {
	return func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
		return p.StartTrace(ctx, c, "s1", script)
	}
}

// cliTraceStop stops the CLI trace of s1 and returns its event names.
func (r *rig) cliTraceStop(t *testing.T) []string {
	t.Helper()
	var tr devtools.Trace
	if err := r.cliDo(t, func(ctx context.Context, p *devtools.Page, c webkit.Caller) (err error) {
		tr, _, err = p.StopTrace(ctx, c, "s1")
		return err
	}); err != nil {
		t.Fatalf("CLI trace stop: %v", err)
	}
	var names []string
	for _, ev := range tr.TraceEvents {
		names = append(names, ev.Name)
	}
	return names
}

// emitAndWait emits method on page One and waits until the page
// connection delivered it to its read-loop subscribers, the recordings
// included (they subscribed before).
func (r *rig) emitAndWait(t *testing.T, method string, params any) {
	t.Helper()
	ss := r.pageSessionOf(t, "One").Caller.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	})
	var delivered atomic.Bool
	defer ss.SubscribeSync(method, func(string, json.RawMessage) { delivered.Store(true) })()
	r.fake["One"].Emit(method, params)
	eventually(t, 5*time.Second, method+" delivered", delivered.Load)
}

func timelineRecord(typ string, start float64) map[string]any {
	return map[string]any{"record": map[string]any{"type": typ, "startTime": start, "endTime": start + 0.25, "children": []any{}}}
}

func scriptEvent(typ string, start float64) map[string]any {
	return map[string]any{"event": map[string]any{"type": typ, "startTime": start, "endTime": start + 0.25}}
}

// endTraceNames sends Tracing.end for a ReportEvents trace and returns the
// names of the events of its dataCollected.
func endTraceNames(t *testing.T, c *cdptest.Client, page string) []string {
	t.Helper()
	seq := c.Seq()
	call(t, c, page, "Tracing.end", nil)
	done := waitSessionEvent(t, c, seq, page, "Tracing.tracingComplete")
	var names []string
	for _, e := range c.Events() {
		if e.Method != "Tracing.dataCollected" || e.Seq <= seq || e.Seq > done.Seq || e.SessionID != page {
			continue
		}
		var v struct{ Value []struct{ Name string } }
		if err := json.Unmarshal(e.Params, &v); err != nil {
			t.Fatal(err)
		}
		for _, ev := range v.Value {
			names = append(names, ev.Name)
		}
	}
	return names
}

func countSince(f *fakewebkit.Server, mark int, method string) int {
	n := 0
	for _, m := range sinceMark(f, mark) {
		if m == method {
			n++
		}
	}
	return n
}

// failOnce makes the next call of method fail with a WebKit error.
func failOnce(f *fakewebkit.Server, method, msg string) {
	f.Handle(method, func(fakewebkit.Call) (any, *fakewebkit.Error) {
		f.Handle(method, nil)
		return nil, &fakewebkit.Error{Code: -32000, Message: msg}
	})
}

// detachAndWait detaches the page attachment and waits for its
// detachedFromTarget.
func detachAndWait(t *testing.T, c *cdptest.Client, tab, page string) {
	t.Helper()
	seq := c.Seq()
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
	waitSessionEvent(t, c, seq, tab, "Target.detachedFromTarget")
}

// (a) A Tracing.start whose forwarded Timeline.enable a provisional
// target refuses fails after Timeline.start and
// ScriptProfiler.startTracking reached the active target: the start stops
// both again, and nothing of the attachment's trace stays.
func TestCDPTraceStartForwardingFailureStopsTheRecording(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	tab, page := puppeteerAttach(t, c)
	waitResumed(t, f.Provisional("next"))
	f.Handle("Timeline.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == "next" {
			return nil, &fakewebkit.Error{Code: -32000, Message: "provisional target refused Timeline.enable"}
		}
		return nil, nil
	})
	mark := len(f.Received())
	e := cdpFail(t, c, page, "Tracing.start", nil)
	if !strings.Contains(e.Message, "provisional target refused") {
		t.Fatalf("Tracing.start: %+v, want the forwarding failure", e)
	}
	if countSince(f, mark, "Timeline.start") != 1 || countSince(f, mark, "ScriptProfiler.startTracking") != 1 {
		t.Fatalf("setup: the active recordings did not start: %v", sinceMark(f, mark))
	}
	if countSince(f, mark, "Timeline.stop") == 0 || f.Tracking(f.ActiveTarget()) {
		t.Fatalf("the failed start left the recording running: %v", sinceMark(f, mark))
	}
	st := r.pageStateOf(t, "One")
	if st.HasEntry("cdp:trace:"+page) || st.HasEntry("cdp:trace-script:"+page) || st.Needs("Timeline") {
		t.Fatal("the failed start left desired state")
	}
	if e := cdpFail(t, c, page, "Tracing.end", nil); e.Message != errTracingNotStarted {
		t.Fatalf("Tracing.end after the undone start: %+v", e)
	}
	detachAndWait(t, c, tab, page)
}

// (b) A start whose reply never comes (the fake runs it and drops the
// reply) has an unknown outcome when the detach cancels it: the detach's
// release stops what may run, a Timeline recording and a ScriptProfiler
// run.
func TestCDPRecordingStartWithAnUnknownOutcomeIsStoppedAtDetach(t *testing.T) {
	for _, tc := range []struct {
		name, lost string
		calls      []string
		stops      []string
	}{
		{"trace", "Timeline.start", []string{"Tracing.start"}, []string{"Timeline.stop", "ScriptProfiler.stopTracking"}},
		{"trace script", "ScriptProfiler.startTracking", []string{"Tracing.start"}, []string{"ScriptProfiler.stopTracking"}},
		{"profile", "ScriptProfiler.startTracking", []string{"Profiler.enable", "Profiler.start"}, []string{"ScriptProfiler.stopTracking"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			tab, page := puppeteerAttach(t, c)
			for _, m := range tc.calls[:len(tc.calls)-1] {
				call(t, c, page, m, nil)
			}
			f.DropReplies(tc.lost, 1)
			mark := len(f.Received())
			done := make(chan error, 1)
			go func() {
				_, err := c.Call(ctxFor(t, 20*time.Second), page, tc.calls[len(tc.calls)-1], nil)
				done <- err
			}()
			eventually(t, 5*time.Second, tc.lost+" received", func() bool { return countSince(f, mark, tc.lost) > 0 })
			detachAndWait(t, c, tab, page)
			if err := <-done; err == nil {
				t.Fatalf("%s with a lost reply succeeded", tc.calls[len(tc.calls)-1])
			}
			eventually(t, 5*time.Second, "the release of the unknown start", func() bool {
				for _, m := range tc.stops {
					if countSince(f, mark, m) == 0 {
						return false
					}
				}
				return !f.Tracking(f.ActiveTarget())
			})
			st := r.pageStateOf(t, "One")
			for _, k := range []string{"cdp:trace:", "cdp:trace-script:", "cdp:profiler:"} {
				if st.HasEntry(k + page) {
					t.Fatalf("entry %s%s stayed after the detach", k, page)
				}
			}
			if st.Needs("Timeline") {
				t.Fatal("the Timeline domain stayed after the detach")
			}
		})
	}
}

// (c) Timeline.stop fails and the script stop succeeds: a profile that
// starts next keeps its script events out of the old trace, whose
// Timeline recording still records; the retry sends only the stop still
// owed and leaves the profile running.
func TestCDPPartiallyStoppedTraceIgnoresNextRecordingsScriptEvents(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", nil)
	r.emitAndWait(t, "ScriptProfiler.trackingUpdate", scriptEvent("OwnScript", 1))
	failOnce(f, "Timeline.stop", "deliberate Timeline stop refusal")
	if e := cdpFail(t, c, page, "Tracing.end", nil); !strings.Contains(e.Message, "Timeline.stop") {
		t.Fatalf("setup: the first Tracing.end should fail on Timeline only: %+v", e)
	}
	st := r.pageStateOf(t, "One")
	if st.HasEntry("cdp:trace-script:"+page) || f.Tracking(f.ActiveTarget()) || !st.HasEntry("cdp:trace:"+page) {
		t.Fatal("setup: the script part did not stop alone")
	}
	call(t, c, page, "Profiler.enable", nil)
	call(t, c, page, "Profiler.start", nil)
	r.emitAndWait(t, "ScriptProfiler.trackingUpdate", scriptEvent("NewProfilerOnly", 2))
	r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("OwnTimelineAfterPartialStop", 2.5))
	mark := len(f.Received())
	names := endTraceNames(t, c, page)
	if slices.Contains(names, "Script NewProfilerOnly") {
		t.Error("the partly stopped trace took the next profile's script event")
	}
	for _, own := range []string{"Script OwnScript", "OwnTimelineAfterPartialStop"} {
		if !slices.Contains(names, own) {
			t.Errorf("the trace lost its own event %q: %v", own, names)
		}
	}
	if n := countSince(f, mark, "Timeline.stop"); n != 1 {
		t.Errorf("the retry sent Timeline.stop %d times; want 1, the stop still owed", n)
	}
	if n := countSince(f, mark, "ScriptProfiler.stopTracking"); n != 0 {
		t.Errorf("the retry sent ScriptProfiler.stopTracking %d times; the script part had stopped", n)
	}
	if !f.Tracking(f.ActiveTarget()) {
		t.Error("the trace retry stopped the next profile's run")
	}
	call(t, c, page, "Profiler.stop", nil)
}

// (c) and (e) The script stop fails and Timeline.stop succeeds: a CLI
// trace of the same session that starts next on the page keeps its
// Timeline records out of the old CDP trace, whose ScriptProfiler run
// still records; the retry sends only the stop still owed, and it neither
// stops nor disables the CLI's Timeline recording.
func TestCDPPartiallyStoppedTraceIgnoresNextRecordingsTimelineEvents(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", nil)
	r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("OwnTimeline", 1))
	failOnce(f, "ScriptProfiler.stopTracking", "deliberate script stop refusal")
	if e := cdpFail(t, c, page, "Tracing.end", nil); !strings.Contains(e.Message, "script stop refusal") {
		t.Fatalf("setup: the first Tracing.end should fail on ScriptProfiler only: %+v", e)
	}
	st := r.pageStateOf(t, "One")
	if st.HasEntry("cdp:trace:"+page) || !st.HasEntry("cdp:trace-script:"+page) {
		t.Fatal("setup: the Timeline part did not stop alone")
	}
	if err := r.cliDo(t, cliTraceStart(false)); err != nil {
		t.Fatalf("CLI trace start (timeline) beside the stopped Timeline part: %v", err)
	}
	r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("NewTimelineOnly", 2))
	r.emitAndWait(t, "ScriptProfiler.trackingUpdate", scriptEvent("OwnScriptAfterPartialStop", 2.5))
	mark := len(f.Received())
	names := endTraceNames(t, c, page)
	if slices.Contains(names, "NewTimelineOnly") {
		t.Error("the partly stopped trace took the next Timeline recording's record")
	}
	for _, own := range []string{"OwnTimeline", "Script OwnScriptAfterPartialStop"} {
		if !slices.Contains(names, own) {
			t.Errorf("the trace lost its own event %q: %v", own, names)
		}
	}
	if n := countSince(f, mark, "ScriptProfiler.stopTracking"); n != 1 {
		t.Errorf("the retry sent ScriptProfiler.stopTracking %d times; want 1, the stop still owed", n)
	}
	for _, m := range []string{"Timeline.stop", "Timeline.disable"} {
		if n := countSince(f, mark, m); n != 0 {
			t.Errorf("the retry sent %s %d times; the CLI trace records with Timeline", m, n)
		}
	}
	r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("CLIAfterCDPEnd", 3))
	if cli := r.cliTraceStop(t); !slices.Contains(cli, "NewTimelineOnly") || !slices.Contains(cli, "CLIAfterCDPEnd") {
		t.Errorf("the CLI trace lost its records: %v", cli)
	}
}

// (d) The page's ScriptProfiler is held by a CLI recording or by another
// attachment: a CDP Profiler.start or Tracing.start with JavaScript
// fails, names the holder and what stops it, and sends nothing; a CLI
// start beside a CDP holder names that holder too.
func TestCDPScriptProfilerBusyNamesTheHolder(t *testing.T) {
	t.Run("CLI profiler", func(t *testing.T) {
		r := newRig(t)
		f := r.fake["One"]
		c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
		_, page := puppeteerAttach(t, c)
		if err := r.cliDo(t, func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
			return p.StartProfiler(ctx, c, "s1")
		}); err != nil {
			t.Fatal(err)
		}
		call(t, c, page, "Profiler.enable", nil)
		mark := len(f.Received())
		for _, m := range []string{"Profiler.start", "Tracing.start"} {
			e := cdpFail(t, c, page, m, nil)
			if !strings.Contains(e.Message, "profiler:s1 holds it") || !strings.Contains(e.Message, "'iwdp-cli profiler stop'") {
				t.Errorf("%s beside the CLI profiler: %q, want the holder and its stop", m, e.Message)
			}
		}
		if got := sinceMark(f, mark); len(got) != 0 {
			t.Errorf("the refused starts sent %v", got)
		}
		if err := r.cliDo(t, func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
			_, err := p.StopProfiler(ctx, c, "s1")
			return err
		}); err != nil {
			t.Fatalf("the CLI profiler after the refused CDP starts: %v", err)
		}
	})
	t.Run("another attachment", func(t *testing.T) {
		r := newRig(t)
		f := r.fake["One"]
		c1 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
		_, holder := puppeteerAttach(t, c1)
		c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
		_, page := puppeteerAttach(t, c2)
		call(t, c1, holder, "Profiler.enable", nil)
		call(t, c1, holder, "Profiler.start", nil)
		call(t, c2, page, "Profiler.enable", nil)
		mark := len(f.Received())
		owner := "cdp-" + holder + ":s1"
		for _, m := range []string{"Profiler.start", "Tracing.start"} {
			e := cdpFail(t, c2, page, m, nil)
			if !strings.Contains(e.Message, owner+" holds it") || !strings.Contains(e.Message, "Profiler.stop") {
				t.Errorf("%s beside another attachment's profile: %q, want the holder %s and its stop", m, e.Message, owner)
			}
		}
		if got := sinceMark(f, mark); len(got) != 0 {
			t.Errorf("the refused starts sent %v", got)
		}
		err := r.cliDo(t, func(ctx context.Context, p *devtools.Page, c webkit.Caller) error {
			return p.StartProfiler(ctx, c, "s1")
		})
		if err == nil || !strings.Contains(err.Error(), owner) || !strings.Contains(err.Error(), "CDP client") || !strings.Contains(err.Error(), "Profiler.stop") {
			t.Errorf("CLI profiler start beside a CDP profile: %v, want the CDP holder and its stop", err)
		}
		call(t, c1, holder, "Profiler.stop", nil)
	})
}

// (e) A CLI "trace start" and a CDP Tracing.start of one session on one
// page: the page has one Timeline recording, so the second start fails,
// sends nothing and leaves the first recording, its events and its
// release alone, in both orders and with and without JavaScript.
func TestCDPAndCLITracesOnOnePage(t *testing.T) {
	t.Run("CLI first", func(t *testing.T) {
		for _, cats := range []any{nil, map[string]any{"categories": "devtools.timeline"}} {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			tab, page := puppeteerAttach(t, c)
			if err := r.cliDo(t, cliTraceStart(true)); err != nil {
				t.Fatal(err)
			}
			mark := len(f.Received())
			if e := cdpFail(t, c, page, "Tracing.start", cats); e.Message != errTracingStarted {
				t.Fatalf("Tracing.start %v beside a CLI trace: %+v", cats, e)
			}
			if got := sinceMark(f, mark); len(got) != 0 {
				t.Fatalf("the refused Tracing.start sent %v", got)
			}
			mark = len(f.Received())
			detachAndWait(t, c, tab, page)
			// The detach releases only the attachment's own state: its page
			// compatibility shims (docs/cdp.md).
			if recs := f.Received()[mark:]; len(recs) != 1 || recs[0].Method != "Page.setBootstrapScript" || strings.Contains(string(recs[0].Params), "__iwdpShim") {
				t.Fatalf("the detach sent %v, want only the bootstrap script without the attachment's page shims", sinceMark(f, mark))
			}
			st := r.pageStateOf(t, "One")
			if !st.HasEntry("devtools:trace:s1") || !st.HasEntry("devtools:trace-script:s1") || !f.Tracking(f.ActiveTarget()) {
				t.Fatal("the CLI trace lost its state")
			}
			r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("CLIRecord", 1))
			if names := r.cliTraceStop(t); !slices.Contains(names, "CLIRecord") {
				t.Fatalf("CLI trace events %v, want CLIRecord", names)
			}
		}
	})
	t.Run("CDP first", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cats any
		}{{"with JavaScript", nil}, {"timeline only", map[string]any{"categories": "devtools.timeline"}}} {
			t.Run(tc.name, func(t *testing.T) {
				r := newRig(t)
				f := r.fake["One"]
				c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
				_, page := puppeteerAttach(t, c)
				call(t, c, page, "Tracing.start", tc.cats)
				tracking := f.Tracking(f.ActiveTarget())
				mark := len(f.Received())
				for _, script := range []bool{false, true} {
					err := r.cliDo(t, cliTraceStart(script))
					if err == nil {
						t.Fatalf("CLI trace start (script %v) beside a CDP trace succeeded", script)
					}
					if !strings.Contains(err.Error(), "cdp-"+page+":s1") {
						t.Errorf("CLI trace start (script %v): %v, want the CDP holder named", script, err)
					}
				}
				if got := sinceMark(f, mark); len(got) != 0 {
					t.Fatalf("the refused CLI starts sent %v", got)
				}
				if f.Tracking(f.ActiveTarget()) != tracking {
					t.Fatal("the refused CLI starts changed the page's ScriptProfiler run")
				}
				st := r.pageStateOf(t, "One")
				if st.HasEntry("devtools:trace:s1") || st.HasEntry("devtools:trace-script:s1") {
					t.Fatal("a refused CLI start left its entries")
				}
				r.emitAndWait(t, "Timeline.eventRecorded", timelineRecord("CDPRecord", 1))
				if names := endTraceNames(t, c, page); !slices.Contains(names, "CDPRecord") {
					t.Fatalf("CDP trace events %v, want CDPRecord", names)
				}
				// The page is free again.
				if err := r.cliDo(t, cliTraceStart(true)); err != nil {
					t.Fatalf("CLI trace start after Tracing.end: %v", err)
				}
				r.cliTraceStop(t)
			})
		}
	})
}

// hasExtras reports whether the attachment id has recording state
// (extrasOf).
func hasExtras(id string) bool {
	extras.Lock()
	defer extras.Unlock()
	for s := range extras.by {
		if s.id == id {
			return true
		}
	}
	return false
}

// Item 3 of the finish brief: the recording state of an attachment (the
// package map extras, keyed by *session, and its goroutine that waits for
// session.gone) never outlives the attachment, whichever way it ends: a
// detach, the end of its CDP connection, the end of the session's lease,
// the revocation of the session's endpoint, or the endpoint's stop (the
// daemon's stop). The page side of its recordings is stopped too.
func TestCDPRecordingStateEndsWithTheAttachment(t *testing.T) {
	for _, how := range []string{"detach", "close", "lease end", "revoke", "server stop"} {
		t.Run(how, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			tab, page := puppeteerAttach(t, c)
			call(t, c, page, "Tracing.start", map[string]any{"categories": "devtools.timeline"})
			call(t, c, page, "Profiler.enable", nil)
			call(t, c, page, "Profiler.start", nil)
			if !hasExtras(page) || !f.Tracking(f.ActiveTarget()) {
				t.Fatal("setup: no recording state")
			}
			mark := len(f.Received())
			switch how {
			case "detach":
				detachAndWait(t, c, tab, page)
			case "close":
				c.Close()
			case "lease end":
				if _, err := r.m.ReleaseSession(ctxFor(t, 5*time.Second), "s1"); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				r.srv.RevokeSession("s1")
			case "server stop":
				r.srv.Stop()
			}
			eventually(t, 5*time.Second, "the end of the attachment's recording state", func() bool { return !hasExtras(page) })
			eventually(t, 5*time.Second, "the page side of the recordings stopped", func() bool {
				return countSince(f, mark, "Timeline.stop") > 0 && !f.Tracking(f.ActiveTarget())
			})
		})
	}
}

// soupDeleteCookie is WebKitGTK's Page.deleteCookie(name, url)
// (NetworkStorageSessionSoup deleteCookie, seen on WebKitGTK in
// TestCDPCookiesStorageTracingProfilerOnWebKitGTK): it deletes only the
// first cookie of that name in libsoup's list for the url. libsoup lists
// the cookies by path length, longest first, and then by domain bucket:
// the domain cookie of the host (".host"), the host-only cookie ("host"),
// then the domain cookies of the parent domains, nearest first.
func soupDeleteCookie(jar *devtoolstest.Page) fakewebkit.Handler {
	return func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var v struct{ CookieName, URL string }
		_ = json.Unmarshal(c.Params, &v)
		u, err := url.Parse(v.URL)
		if err != nil {
			return nil, &fakewebkit.Error{Code: -32602, Message: err.Error()}
		}
		host := strings.ToLower(u.Hostname())
		bucket := func(domain string) int {
			d := strings.ToLower(domain)
			switch d {
			case "." + host:
				return 0
			case host:
				return 1
			default:
				return 2 + strings.Count(host, ".") - strings.Count(d, ".")
			}
		}
		jar.Mu.Lock()
		defer jar.Mu.Unlock()
		first := -1
		for i, ck := range jar.Cookies {
			if ck.Name != v.CookieName || !sentTo(ck, u) {
				continue
			}
			if first < 0 {
				first = i
				continue
			}
			f := jar.Cookies[first]
			if len(ck.Path) > len(f.Path) || (len(ck.Path) == len(f.Path) && bucket(ck.Domain) < bucket(f.Domain)) {
				first = i
			}
		}
		if first >= 0 {
			jar.Cookies = append(jar.Cookies[:first:first], jar.Cookies[first+1:]...)
		}
		return nil, nil
	}
}

// cocoaDeleteCookie is iOS WebKit's Page.deleteCookie(name, url)
// (NetworkStorageSessionCocoa deleteCookie): it deletes every cookie of
// that name that the url gets, as devtoolstest.Page does.
func cocoaDeleteCookie(jar *devtoolstest.Page) fakewebkit.Handler {
	return func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var v struct{ CookieName, URL string }
		_ = json.Unmarshal(c.Params, &v)
		u, err := url.Parse(v.URL)
		if err != nil {
			return nil, &fakewebkit.Error{Code: -32602, Message: err.Error()}
		}
		jar.Mu.Lock()
		defer jar.Mu.Unlock()
		jar.Cookies = slices.DeleteFunc(jar.Cookies, func(ck webkit.Cookie) bool { return ck.Name == v.CookieName && sentTo(ck, u) })
		return nil, nil
	}
}

// expiredSets returns the cookies of the Page.setCookie calls that the
// jar received since mark whose cookie has expired.
func expiredSets(t *testing.T, jar *devtoolstest.Page, mark int) []webkit.Cookie {
	t.Helper()
	var out []webkit.Cookie
	for _, rec := range jar.Server.Received()[mark:] {
		if rec.Method != "Page.setCookie" {
			continue
		}
		var p struct{ Cookie webkit.Cookie }
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			t.Fatalf("Page.setCookie params %s: %v", rec.Params, err)
		}
		if !p.Cookie.Session && p.Cookie.Expires <= float64(time.Now().UnixMilli()) {
			out = append(out, p.Cookie)
		}
	}
	return out
}

// Network.deleteCookies deletes one identity with Page.setCookie of that
// cookie with an expiry in the past (RFC 6265 section 5.3: a new cookie
// replaces the cookie of its name, domain and path, and an expired cookie
// is evicted; WebKitGTK: TestCDPCookiesStorageTracingProfilerOnWebKitGTK).
// It never sends Page.deleteCookie(name, url), which also deletes
// namesakes: on iOS every cookie of the name that the url gets, on
// WebKitGTK the first of them in libsoup's order. With either engine's
// Page.deleteCookie in the fake, the jar loses exactly the identity.
func TestCDPDeleteCookiesExpiresExactlyTheIdentity(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	all := []webkit.Cookie{
		sessionCookie("k", "sub", "app.example", "/sub"),
		sessionCookie("k", "root", "app.example", "/"),
		sessionCookie("k", "domsub", ".app.example", "/sub"),
		sessionCookie("k", "dom", ".app.example", "/"),
	}
	for _, engine := range []struct {
		name   string
		delete fakewebkit.Handler
	}{{"iOS", cocoaDeleteCookie(jar)}, {"WebKitGTK", soupDeleteCookie(jar)}} {
		jar.Server.Handle("Page.deleteCookie", engine.delete)
		for _, tc := range []struct {
			params map[string]any
			want   []string
			gone   []cookieID
		}{
			{map[string]any{"name": "k", "domain": "app.example", "path": "/sub"}, []string{"k=dom .app.example /", "k=domsub .app.example /sub", "k=root app.example /"}, []cookieID{{"k", "app.example", "/sub"}}},
			{map[string]any{"name": "k", "domain": "app.example", "path": "/"}, []string{"k=dom .app.example /", "k=domsub .app.example /sub", "k=sub app.example /sub"}, []cookieID{{"k", "app.example", "/"}}},
			{map[string]any{"name": "k", "domain": ".app.example", "path": "/"}, []string{"k=domsub .app.example /sub", "k=root app.example /", "k=sub app.example /sub"}, []cookieID{{"k", ".app.example", "/"}}},
			{map[string]any{"name": "k", "domain": "app.example"}, []string{"k=dom .app.example /", "k=domsub .app.example /sub"}, []cookieID{{"k", "app.example", "/sub"}, {"k", "app.example", "/"}}},
		} {
			setJar(jar, all...)
			mark := len(jar.Server.Received())
			call(t, c, page, "Network.deleteCookies", tc.params)
			if got := jarCookies(jar); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: deleteCookies %v: jar %v, want %v", engine.name, tc.params, got, tc.want)
			}
			var sent []cookieID
			for _, ck := range expiredSets(t, jar, mark) {
				sent = append(sent, idOf(ck))
			}
			if !reflect.DeepEqual(sent, tc.gone) {
				t.Errorf("%s: deleteCookies %v: expired Page.setCookie of %v, want %v", engine.name, tc.params, sent, tc.gone)
			}
		}
	}
	if n := len(jar.Server.ReceivedMethod("Page.deleteCookie")); n != 0 {
		t.Errorf("the bridge sent Page.deleteCookie %d times", n)
	}
}

// When the cookie is still there after its expired Page.setCookie (a
// WebKit that does not delete it so, or a page that set it again), when
// WebKit refuses the Page.setCookie, or when the read-back fails,
// Network.deleteCookies fails, names the cookie without its value, and
// sets nothing again.
func TestCDPDeleteCookiesFailsWhenTheCookieStays(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	all := []webkit.Cookie{sessionCookie("k", "secretsub", "app.example", "/sub"), sessionCookie("k", "secretroot", "app.example", "/")}
	del := map[string]any{"name": "k", "domain": "app.example", "path": "/sub"}
	// Page.getCookies: the handler's lookup (1), the read-back (2).
	reads, failRead := 0, 0
	jar.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		jar.Mu.Lock()
		defer jar.Mu.Unlock()
		if reads++; reads == failRead {
			return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate read failure"}
		}
		return map[string]any{"cookies": slices.Clone(jar.Cookies)}, nil
	})
	setJar(jar, all...)
	failRead = 2
	e := cdpFail(t, c, page, "Network.deleteCookies", del)
	if !strings.Contains(e.Message, "reading the cookies back") || !strings.Contains(e.Message, "deliberate read failure") {
		t.Errorf("deleteCookies whose read-back fails: %+v", e)
	}
	failRead = 0

	jar.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	setJar(jar, all...)
	mark := len(jar.Server.Received())
	e = cdpFail(t, c, page, "Network.deleteCookies", del)
	if !strings.Contains(e.Message, "k (domain app.example, path /sub)") || strings.Contains(e.Message, "secret") {
		t.Errorf("deleteCookies of a cookie that WebKit keeps: %+v, want the cookie named without its value", e)
	}
	if got := jarCookies(jar); len(got) != len(all) {
		t.Errorf("jar after the failed delete: %v", got)
	}
	if n := countSince(jar.Server, mark, "Page.setCookie"); n != 1 {
		t.Errorf("Page.setCookie sent %d times, want 1 (the expired cookie, nothing set again)", n)
	}

	jar.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate set refusal"}
	})
	e = cdpFail(t, c, page, "Network.deleteCookies", del)
	if !strings.Contains(e.Message, "k (domain app.example, path /sub)") || !strings.Contains(e.Message, "deliberate set refusal") || strings.Contains(e.Message, "secret") {
		t.Errorf("deleteCookies that WebKit refuses: %+v", e)
	}
	if n := len(jar.Server.ReceivedMethod("Page.deleteCookie")); n != 0 {
		t.Errorf("the bridge sent Page.deleteCookie %d times", n)
	}
}

// Network.setCookie and Storage.setCookies with an expired cookie (Chrome:
// the store deletes the cookie of that identity; Puppeteer's
// BrowserContext.deleteCookie sends expires 1) pass the cookie to WebKit
// as converted, in one Page.setCookie, and read the jar back: success
// when the identity is gone, also when it was never there; success false
// or an error naming the cookie when WebKit keeps it.
func TestCDPSetCookieWithAnExpiredCookie(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	gone := map[string]any{"name": "k", "value": "gone", "domain": "app.example", "path": "/sub", "expires": 1, "secure": true, "httpOnly": true, "sameSite": "Strict"}
	setJar(jar, sessionCookie("k", "sub", "app.example", "/sub"), sessionCookie("k", "root", "app.example", "/"))
	mark := len(jar.Server.Received())
	if res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", gone)); !res.Success {
		t.Fatal("Network.setCookie of an expired cookie: success false")
	}
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"k=root app.example /"}) {
		t.Fatalf("jar %v, want only k on /", got)
	}
	// expires 1 s is 1000 ms in WebKit's Page.Cookie.
	want := webkit.Cookie{Name: "k", Value: "gone", Domain: "app.example", Path: "/sub", Expires: 1000, HTTPOnly: true, Secure: true, SameSite: "Strict"}
	if got := expiredSets(t, jar, mark); !reflect.DeepEqual(got, []webkit.Cookie{want}) || countSince(jar.Server, mark, "Page.setCookie") != 1 {
		t.Fatalf("Page.setCookie sent %+v, want only %+v", got, want)
	}
	if res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", gone)); !res.Success {
		t.Fatal("Network.setCookie of an expired cookie that is not in the jar: success false")
	}
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"k=root app.example /"}) {
		t.Fatalf("jar after an expired cookie of no stored identity %v", got)
	}

	jar.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	setJar(jar, sessionCookie("k", "secretsub", "app.example", "/sub"))
	if res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", gone)); res.Success {
		t.Fatal("Network.setCookie of an expired cookie that WebKit keeps: success true")
	}
	e := cdpFail(t, c, "", "Storage.setCookies", map[string]any{"cookies": []any{gone}})
	if !strings.Contains(e.Message, "1 of 1 cookies were not set") || !strings.Contains(e.Message, "k (domain app.example, path /sub)") || strings.Contains(e.Message, "secret") {
		t.Fatalf("Storage.setCookies of an expired cookie that WebKit keeps: %+v", e)
	}
	if n := len(jar.Server.ReceivedMethod("Page.deleteCookie")); n != 0 {
		t.Errorf("the bridge sent Page.deleteCookie %d times", n)
	}
}

// Reviewer repro of W5c-1b review F1, kept: a page script deletes an
// unrelated cookie (auth, a logout) while the bridge deletes k, by
// Network.deleteCookies or by an expired cookie. The bridge must not set
// auth again. The page's change runs at the bridge's first cookie write,
// whichever WIP command that is (devtoolstest.Page.BeforeCookieWrite), so
// after the bridge read the jar; the fake's Page.deleteCookie is iOS's.
func TestReviewerDeleteDoesNotUndoAnUnrelatedPageCookieDeletion(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	for _, req := range []struct {
		session, method string
		params          map[string]any
	}{
		{page, "Network.deleteCookies", map[string]any{"name": "k", "domain": "app.example", "path": "/sub"}},
		{"", "Storage.setCookies", map[string]any{"cookies": []any{map[string]any{"name": "k", "value": "", "domain": "app.example", "path": "/sub", "expires": 1}}}},
	} {
		setJar(jar, sessionCookie("k", "target", "app.example", "/sub"), sessionCookie("auth", "logged-in", "app.example", "/"))
		jar.Mu.Lock()
		jar.BeforeCookieWrite = func(p *devtoolstest.Page) {
			p.BeforeCookieWrite = nil
			p.Cookies = slices.DeleteFunc(p.Cookies, func(ck webkit.Cookie) bool { return ck.Name == "auth" })
		}
		jar.Mu.Unlock()
		call(t, c, req.session, req.method, req.params)
		if got := jarCookies(jar); len(got) != 0 {
			t.Errorf("%s resurrected a cookie that the page deleted: %v", req.method, got)
		}
	}
}

// Reviewer repro of W5c-1b review F1, kept: the page rotates the
// namesake k of path / (new-auth) while the bridge deletes k of /sub. The
// bridge must not write the namesake's old value over it.
func TestReviewerDeleteDoesNotOverwriteARotatedNamesake(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	setJar(jar, sessionCookie("k", "target", "app.example", "/sub"), sessionCookie("k", "old-auth", "app.example", "/"))
	jar.Mu.Lock()
	jar.BeforeCookieWrite = func(p *devtoolstest.Page) {
		p.BeforeCookieWrite = nil
		for i, ck := range p.Cookies {
			if ck.Name == "k" && ck.Path == "/" {
				p.Cookies[i].Value = "new-auth"
			}
		}
	}
	jar.Mu.Unlock()
	call(t, c, page, "Network.deleteCookies", map[string]any{"name": "k", "domain": "app.example", "path": "/sub"})
	if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"k=new-auth app.example /"}) {
		t.Fatalf("deleteCookies overwrote the page's rotated namesake: %v", got)
	}
}

// Property: whichever engine's Page.deleteCookie the page has (iOS:
// every cookie of the name that the url gets; WebKitGTK: the first of
// them; the bridge does not use it), Network.deleteCookies of one
// identity leaves exactly the other cookies of the jar, with their
// values.
func TestCDPDeleteCookiesPropertyLeavesTheOthers(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	domains := []string{"app.example", ".app.example", "sub.app.example", ".sub.app.example", ".example"}
	paths := []string{"/", "/sub", "/sub/x", "/other"}
	type jarCase struct {
		cookies []webkit.Cookie
		target  int
		soup    bool
	}
	gen := func(vals []reflect.Value, rnd *rand.Rand) {
		seen := map[cookieID]bool{}
		var cs []webkit.Cookie
		for n := 1 + rnd.Intn(6); len(cs) < n; {
			ck := sessionCookie("k", "", domains[rnd.Intn(len(domains))], paths[rnd.Intn(len(paths))])
			if rnd.Intn(4) == 0 {
				ck.Name = "other"
			}
			if seen[idOf(ck)] {
				continue
			}
			seen[idOf(ck)] = true
			ck.Value = strings.TrimPrefix(ck.Domain, ".") + ck.Path
			cs = append(cs, ck)
		}
		vals[0] = reflect.ValueOf(jarCase{cookies: cs, target: rnd.Intn(len(cs)), soup: rnd.Intn(2) == 0})
	}
	list := func(cs []webkit.Cookie) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name+"="+c.Value+" "+c.Domain+" "+c.Path)
		}
		sort.Strings(out)
		return out
	}
	check := func(jc jarCase) bool {
		if jc.soup {
			jar.Server.Handle("Page.deleteCookie", soupDeleteCookie(jar))
		} else {
			jar.Server.Handle("Page.deleteCookie", cocoaDeleteCookie(jar))
		}
		setJar(jar, jc.cookies...)
		tg := jc.cookies[jc.target]
		call(t, c, page, "Network.deleteCookies", map[string]any{"name": tg.Name, "domain": tg.Domain, "path": tg.Path})
		want := list(slices.Delete(slices.Clone(jc.cookies), jc.target, jc.target+1))
		got := jarCookies(jar)
		if !slices.Equal(got, want) {
			t.Logf("jar %v, soup %v, target %v: got %v, want %v", list(jc.cookies), jc.soup, tg, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 60, Values: gen}); err != nil {
		t.Fatal(err)
	}
}

// Network.getCookies{urls} with a url whose path is a cookie's path
// exactly: the cookie is sent (RFC 6265 5.1.4, identical paths). The
// path match read past the url's path for it.
func TestCDPGetCookiesOfAURLWithTheCookiesPath(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	setJar(jar, sessionCookie("k", "sub", "app.example", "/sub"), sessionCookie("k", "subx", "app.example", "/sub/x"))
	for _, tc := range []struct {
		url  string
		want []string
	}{
		{"https://app.example/sub", []string{"sub"}},
		{"https://app.example/sub/x", []string{"sub", "subx"}},
		{"https://app.example/subx", nil},
	} {
		got := decode[struct{ Cookies []struct{ Value string } }](t, call(t, c, page, "Network.getCookies", map[string]any{"urls": []string{tc.url}}))
		var values []string
		for _, ck := range got.Cookies {
			values = append(values, ck.Value)
		}
		sort.Strings(values)
		if !reflect.DeepEqual(values, tc.want) {
			t.Errorf("Network.getCookies{%s} = %v, want %v", tc.url, values, tc.want)
		}
	}
}

// Reviewer repro of W5c-1b review R1 F4, kept: a CDP cookie with expires
// 0 (the Unix epoch), or a positive expiry that rounds to 0 ms, is a past
// expiry, as in Chrome, and deletes the cookie of its identity through
// every setter; it never becomes a session cookie (tools.SetCookie sends
// Expires 0 as a session cookie, W4's convention for its callers). The
// rounding boundary: 0.00049 s rounds to 0 ms, 0.0005 s to 1 ms; both are
// past expiries. Each call succeeds, sends one Page.setCookie with an
// expiry in the past (session false), and leaves k on / alone.
func TestReviewerZeroExpiryDeletesRatherThanStoresASessionCookie(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	for _, method := range []string{"Network.setCookie", "Network.setCookies", "Storage.setCookies"} {
		for _, expiry := range []float64{0, 0.0001, 0.00049, 0.0005} {
			t.Run(method+":"+fmt.Sprint(expiry), func(t *testing.T) {
				setJar(jar, sessionCookie("k", "old", "app.example", "/sub"), sessionCookie("k", "root", "app.example", "/"))
				mark := len(jar.Server.Received())
				cp := map[string]any{"name": "k", "value": "replacement", "domain": "app.example", "path": "/sub", "expires": expiry}
				var params any = cp
				if method != "Network.setCookie" {
					params = map[string]any{"cookies": []any{cp}}
				}
				raw, err := c.Call(ctxFor(t, 10*time.Second), page, method, params)
				if got := jarCookies(jar); !reflect.DeepEqual(got, []string{"k=root app.example /"}) {
					t.Errorf("expires=%v must delete /sub, but jar=%v; response=%s error=%v", expiry, got, raw, err)
				}
				switch {
				case err != nil:
					t.Errorf("%s with expires %v: %v", method, expiry, err)
				case method == "Network.setCookie" && !decode[struct{ Success bool }](t, raw).Success:
					t.Errorf("Network.setCookie with expires %v: %s, want success true", expiry, raw)
				}
				if sets := expiredSets(t, jar, mark); len(sets) != 1 || countSince(jar.Server, mark, "Page.setCookie") != 1 {
					t.Errorf("expires=%v: Page.setCookie with an expiry in the past %+v, %d Page.setCookie in all; want one, expired", expiry, sets, countSince(jar.Server, mark, "Page.setCookie"))
				}
			})
		}
	}
}
