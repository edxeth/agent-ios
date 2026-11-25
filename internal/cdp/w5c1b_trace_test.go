package cdp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// pageStateOf is the desired state of the rig's page title.
func (r *rig) pageStateOf(t *testing.T, title string) *core.DesiredState {
	t.Helper()
	for _, ps := range r.m.Status() {
		if ps.Title == title {
			return r.m.LookupPage(ps.Key).PS.State
		}
	}
	t.Fatalf("no page %q", title)
	return nil
}

// emitRecording sends one Timeline record and one script event, as WebKit
// does while it records.
func emitRecording(f *fakewebkit.Server) {
	f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
	f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{
		"type": "EventDispatch", "data": map[string]any{"type": "click"}, "startTime": 10.001, "endTime": 10.002,
	}})
	f.Emit("ScriptProfiler.trackingUpdate", map[string]any{"event": map[string]any{"startTime": 10.0012, "endTime": 10.0018, "type": "Other"}})
}

func sinceMark(f *fakewebkit.Server, mark int) []string {
	var out []string
	for _, rec := range f.Received()[mark:] {
		out = append(out, rec.Method)
	}
	return out
}

func waitSessionEvent(t *testing.T, c *cdptest.Client, after int, session, method string) cdptest.Event {
	t.Helper()
	e, err := c.WaitEvent(testCtx(t), after, method, func(e cdptest.Event) bool { return e.SessionID == session })
	if err != nil {
		t.Fatalf("waiting for %s: %v", method, err)
	}
	return e
}

// readStream reads a stream with IO.read in chunks of size until eof.
func readStream(t *testing.T, c *cdptest.Client, session, handle string, size int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; ; i++ {
		r := decode[struct {
			Data          string
			EOF           bool
			Base64Encoded bool
		}](t, call(t, c, session, "IO.read", map[string]any{"handle": handle, "size": size}))
		if r.Base64Encoded {
			t.Fatal("IO.read: base64Encoded for a JSON trace")
		}
		b.WriteString(r.Data)
		if r.EOF {
			if r.Data != "" {
				t.Fatalf("IO.read: eof with data %q (Chrome: eof comes with the read that finds no data)", r.Data)
			}
			return b.String()
		}
		if i > 1e5 {
			t.Fatal("IO.read never reached eof")
		}
	}
}

type traceFile struct {
	TraceEvents []struct {
		Name string `json:"name"`
		Ph   string `json:"ph"`
		Tid  int    `json:"tid"`
	} `json:"traceEvents"`
}

func traceNames(tf traceFile) []string {
	var out []string
	for _, e := range tf.TraceEvents {
		out = append(out, e.Name)
	}
	return out
}

// Tracing.start and Tracing.end as Puppeteer's page.tracing sends them
// (transferMode ReturnAsStream, traceConfig): the WIP Timeline and the
// ScriptProfiler's script events recorded and converted to a Chrome trace,
// tracingComplete with a stream that IO.read returns in UTF-8 chunks and
// IO.close drops; Chrome's errors for a second start, an end without a
// start and an unknown stream; the page's recording stopped and its
// Timeline disabled at the end.
func TestCDPTracingStream(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	mark := len(f.Received())
	call(t, c, page, "Tracing.start", map[string]any{
		"transferMode": "ReturnAsStream",
		"traceConfig":  map[string]any{"includedCategories": []string{"devtools.timeline", "v8.execute"}, "excludedCategories": []string{"*"}},
	})
	got := sinceMark(f, mark)
	for _, m := range []string{"ScriptProfiler.startTracking", "Timeline.enable", "Timeline.start"} {
		if !slices.Contains(got, m) {
			t.Fatalf("Tracing.start sent %v, want %s", got, m)
		}
	}
	if e := cdpFail(t, c, page, "Tracing.start", nil); e.Message != errTracingStarted {
		t.Fatalf("second Tracing.start: %+v", e)
	}
	emitRecording(f)
	mark = len(f.Received())
	seq := c.Seq()
	call(t, c, page, "Tracing.end", nil)
	done := waitSessionEvent(t, c, seq, page, "Tracing.tracingComplete")
	tc := decode[struct {
		DataLossOccurred  bool
		Stream            string
		TraceFormat       string
		StreamCompression string
	}](t, done.Params)
	if tc.Stream == "" || tc.DataLossOccurred || tc.TraceFormat != "json" || tc.StreamCompression != "none" {
		t.Fatalf("tracingComplete %s", done.Params)
	}
	if n := len(eventsOf(c, "Tracing.dataCollected")); n != 0 {
		t.Fatalf("%d dataCollected events with ReturnAsStream, want 0", n)
	}
	got = sinceMark(f, mark)
	for _, m := range []string{"Timeline.stop", "ScriptProfiler.stopTracking", "Timeline.disable"} {
		if !slices.Contains(got, m) {
			t.Fatalf("Tracing.end sent %v, want %s", got, m)
		}
	}
	data := readStream(t, c, page, tc.Stream, 7)
	tf := decode[traceFile](t, json.RawMessage(data))
	names := traceNames(tf)
	if !slices.Contains(names, "EventDispatch click") || !slices.Contains(names, "Script Other") {
		t.Fatalf("trace events %v, want the converted click and script events", names)
	}
	if e := cdpFail(t, c, page, "IO.read", map[string]any{"handle": tc.Stream, "size": 0}); e.Code != -32602 || e.Message != "Invalid max read size" {
		t.Fatalf("IO.read size 0: %+v", e)
	}
	// offset reads again from a position.
	again := decode[struct{ Data string }](t, call(t, c, page, "IO.read", map[string]any{"handle": tc.Stream, "offset": 0, "size": 14}))
	if again.Data != `{"traceEvents"` {
		t.Fatalf("IO.read offset 0: %q", again.Data)
	}
	call(t, c, page, "IO.close", map[string]any{"handle": tc.Stream})
	for _, m := range []string{"IO.read", "IO.close"} {
		if e := cdpFail(t, c, page, m, map[string]any{"handle": tc.Stream}); e.Code != -32602 || e.Message != "Invalid stream handle" {
			t.Fatalf("%s after IO.close: %+v", m, e)
		}
	}
	if e := cdpFail(t, c, page, "Tracing.end", nil); e.Message != errTracingNotStarted {
		t.Fatalf("Tracing.end without a trace: %+v", e)
	}
	st := r.pageStateOf(t, "One")
	tk, sk := "cdp:trace:"+page, "cdp:trace-script:"+page
	if st.HasEntry(tk) || st.HasEntry(sk) || st.Needs("Timeline") {
		t.Fatal("the ended trace left desired state")
	}
	if f.Tracking(f.ActiveTarget()) {
		t.Fatal("the page's ScriptProfiler still tracks")
	}
}

// The default transferMode (ReportEvents) sends the events in
// dataCollected, then tracingComplete without a stream; categories without
// V8 record no JavaScript. Another Timeline user of the page refuses the
// start with Chrome's text.
func TestCDPTracingReportEvents(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	mark := len(f.Received())
	call(t, c, page, "Tracing.start", map[string]any{"categories": "devtools.timeline,-v8.execute"})
	if got := sinceMark(f, mark); slices.Contains(got, "ScriptProfiler.startTracking") {
		t.Fatalf("a trace without V8 categories sent %v", got)
	}
	// A second attachment of the same session: the page's Timeline is in use.
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page2 := puppeteerAttach(t, c2)
	if e := cdpFail(t, c2, page2, "Tracing.start", nil); e.Message != errTracingStarted {
		t.Fatalf("Tracing.start beside another trace: %+v", e)
	}
	emitRecording(f)
	seq := c.Seq()
	call(t, c, page, "Tracing.end", nil)
	done := waitSessionEvent(t, c, seq, page, "Tracing.tracingComplete")
	if string(done.Params) != `{"dataLossOccurred":false}` {
		t.Fatalf("tracingComplete %s", done.Params)
	}
	var names []string
	for _, e := range c.Events() {
		if e.Method == "Tracing.dataCollected" {
			if e.Seq > done.Seq {
				t.Fatal("dataCollected after tracingComplete")
			}
			var v struct{ Value []struct{ Name string } }
			_ = json.Unmarshal(e.Params, &v)
			for _, ev := range v.Value {
				names = append(names, ev.Name)
			}
		}
	}
	if !slices.Contains(names, "EventDispatch click") || slices.Contains(names, "Script Other") {
		t.Fatalf("dataCollected events %v, want the click and no script event", names)
	}
}

// A trace is state of the attachment's owner: detaching the attachment,
// or closing its CDP connection, stops the recording in the page, gives
// up its Timeline domain and ends the ScriptProfiler run.
func TestCDPTraceStopsAtDetach(t *testing.T) {
	for _, how := range []string{"detach", "close"} {
		t.Run(how, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			tab, page := puppeteerAttach(t, c)
			call(t, c, page, "Tracing.start", map[string]any{"transferMode": "ReturnAsStream"})
			st := r.pageStateOf(t, "One")
			if !st.HasEntry("cdp:trace:"+page) || !f.Tracking(f.ActiveTarget()) {
				t.Fatal("the trace did not start")
			}
			mark := len(f.Received())
			if how == "detach" {
				call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
				waitSessionEvent(t, c, 0, tab, "Target.detachedFromTarget")
			} else {
				c.Close()
			}
			eventually(t, 5*time.Second, "the release of the trace", func() bool {
				got := sinceMark(f, mark)
				return slices.Contains(got, "Timeline.stop") && slices.Contains(got, "Timeline.disable") && !f.Tracking(f.ActiveTarget())
			})
			if st.HasEntry("cdp:trace:"+page) || st.HasEntry("cdp:trace-script:"+page) || st.Needs("Timeline") {
				t.Fatal("the detached attachment's trace state stayed")
			}
			if s, _ := r.leaseOf(t, "One"); s != "s1" {
				t.Fatalf("lease %q after the detach, want s1", s)
			}
		})
	}
}

// Profiler.enable, start and stop: the ScriptProfiler run with samples,
// converted to a CDP Profiler.Profile; V8's errors and no-ops; disable
// discards a running profile; the run is the attachment's (a trace with
// JavaScript holds the page's run, and a detach stops the profile).
func TestCDPProfiler(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	if e := cdpFail(t, c, page, "Profiler.start", nil); e.Message != errProfilerDisabled {
		t.Fatalf("Profiler.start before enable: %+v", e)
	}
	if e := cdpFail(t, c, page, "Profiler.stop", nil); e.Message != errNoProfile {
		t.Fatalf("Profiler.stop without a profile: %+v", e)
	}
	call(t, c, page, "Profiler.enable", nil)
	call(t, c, page, "Profiler.start", nil)
	call(t, c, page, "Profiler.start", nil) // V8: a second start changes nothing
	f.ProfilerSample("inner", "outer")
	f.ProfilerSample("inner", "outer")
	p := decode[struct {
		Profile struct {
			Nodes []struct {
				ID        int
				CallFrame struct{ FunctionName string }
				HitCount  int
				Children  []int
			}
			Samples []int
		}
	}](t, call(t, c, page, "Profiler.stop", nil))
	byName := map[string]int{}
	for _, n := range p.Profile.Nodes {
		byName[n.CallFrame.FunctionName] = n.HitCount
	}
	if byName["inner"] != 2 || len(p.Profile.Samples) != 2 {
		t.Fatalf("profile %+v, want 2 samples of inner", p.Profile)
	}
	if f.Tracking(f.ActiveTarget()) {
		t.Fatal("Profiler.stop left the run")
	}
	// disable discards a running profile.
	call(t, c, page, "Profiler.start", nil)
	call(t, c, page, "Profiler.disable", nil)
	if f.Tracking(f.ActiveTarget()) {
		t.Fatal("Profiler.disable left the run")
	}
	if e := cdpFail(t, c, page, "Profiler.stop", nil); e.Message != errNoProfile {
		t.Fatalf("Profiler.stop after disable: %+v", e)
	}
	// A trace with JavaScript holds the page's run.
	call(t, c, page, "Tracing.start", nil)
	call(t, c, page, "Profiler.enable", nil)
	if e := cdpFail(t, c, page, "Profiler.start", nil); !strings.Contains(e.Message, "runs one recording at a time") {
		t.Fatalf("Profiler.start beside a trace: %+v", e)
	}
	call(t, c, page, "Tracing.end", nil)
	// A detach stops the attachment's profile.
	call(t, c, page, "Profiler.start", nil)
	if !f.Tracking(f.ActiveTarget()) {
		t.Fatal("Profiler.start did not start the run")
	}
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
	eventually(t, 5*time.Second, "the release of the profile", func() bool { return !f.Tracking(f.ActiveTarget()) })
	if r.pageStateOf(t, "One").HasEntry("cdp:profiler:" + page) {
		t.Fatal("the detached attachment's profile entry stayed")
	}
}
