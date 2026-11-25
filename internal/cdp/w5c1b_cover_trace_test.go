package cdp

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/devtools"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Behavior tests of the W5c-1b Tracing and IO bounds and failure paths
// (W5c-1b review F2). Expected values come from the CDP protocol
// (Tracing.tracingComplete.dataLossOccurred, IO.read), Chrome's rules in
// /home/devkit/.pi/artifacts/worker/w5c1b/chrome-research.md and the
// bounds that docs/notes/w5.md "W5c-1b" states.

// extrasByID is the recording state of the attachment id.
func extrasByID(t *testing.T, id string) *attachmentExtras {
	t.Helper()
	extras.Lock()
	defer extras.Unlock()
	for s, x := range extras.by {
		if s.id == id {
			return x
		}
	}
	t.Fatalf("no recording state for attachment %s", id)
	return nil
}

// traceEnd ends the trace of page and returns its tracingComplete params.
func traceEnd(t *testing.T, c *cdptest.Client, page string) map[string]any {
	t.Helper()
	seq := c.Seq()
	call(t, c, page, "Tracing.end", nil)
	return decode[map[string]any](t, waitSessionEvent(t, c, seq, page, "Tracing.tracingComplete").Params)
}

// collected returns the events of the dataCollected events after seq, in
// order, as JSON texts.
func collected(c *cdptest.Client, seq int) []string {
	var out []string
	for _, e := range c.Events() {
		if e.Method != "Tracing.dataCollected" || e.Seq <= seq {
			continue
		}
		var v struct{ Value []json.RawMessage }
		_ = json.Unmarshal(e.Params, &v)
		for _, ev := range v.Value {
			out = append(out, string(ev))
		}
	}
	return out
}

// Property: encodeEvents keeps the events in order and stops at the first
// event that would take the JSON past the limit (each event counts its
// bytes and one separator); cut reports that events were dropped. This is
// the 32 MiB cap of a trace, after which tracingComplete has
// dataLossOccurred true.
func TestTraceEncodeEventsProperty(t *testing.T) {
	f := func(names []string, limit uint16) bool {
		evs := make([]devtools.TraceEvent, len(names))
		var enc [][]byte
		for i, n := range names {
			evs[i] = devtools.TraceEvent{Name: n, Ph: "X"}
			b, _ := json.Marshal(evs[i])
			enc = append(enc, b)
		}
		out, cut := encodeEvents(evs, int(limit))
		// The expected prefix: events while the running size stays within
		// the limit.
		want, size := 0, 0
		for want < len(enc) && size+len(enc[want])+1 <= int(limit) {
			size += len(enc[want]) + 1
			want++
		}
		if len(out) != want || cut != (want < len(enc)) {
			return false
		}
		for i := range out {
			if string(out[i]) != string(enc[i]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Property: chunkEvents (the Tracing.dataCollected values of a
// ReportEvents trace) keeps every event once and in order, and a chunk
// goes past the limit only when it is one event larger than the limit.
func TestTraceChunkEventsProperty(t *testing.T) {
	gen := func(vals []reflect.Value, r *rand.Rand) {
		evs := make([]json.RawMessage, r.Intn(40))
		for i := range evs {
			evs[i] = json.RawMessage(fmt.Sprintf("%q", strings.Repeat("x", r.Intn(30))+fmt.Sprint(i)))
		}
		vals[0], vals[1] = reflect.ValueOf(evs), reflect.ValueOf(1+r.Intn(60))
	}
	f := func(evs []json.RawMessage, limit int) bool {
		chunks := chunkEvents(evs, limit)
		var flat []json.RawMessage
		for _, ch := range chunks {
			if len(ch) == 0 {
				return false
			}
			size := 0
			for _, e := range ch {
				size += len(e)
			}
			if size > limit && len(ch) > 1 {
				return false
			}
			flat = append(flat, ch...)
		}
		return reflect.DeepEqual(flat, evs) || (len(evs) == 0 && len(flat) == 0)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300, Values: gen}); err != nil {
		t.Fatal(err)
	}
}

// A ReportEvents trace larger than one dataCollected chunk arrives in
// several dataCollected events, in order, every event once, before
// tracingComplete.
func TestCDPTracingReportEventsChunks(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", map[string]any{"categories": "devtools.timeline"})
	f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
	pad := strings.Repeat("p", 4000)
	const n = 200 // 200 events of about 4 KB: more than one 256 KiB chunk
	for i := range n {
		f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{
			"type": "EventDispatch", "data": map[string]any{"type": fmt.Sprintf("e%03d-%s", i, pad)}, "startTime": 10.0 + float64(i)/1000, "endTime": 10.0005 + float64(i)/1000,
		}})
	}
	seq := c.Seq()
	done := traceEnd(t, c, page)
	if done["dataLossOccurred"] != false {
		t.Fatalf("tracingComplete %v", done)
	}
	chunks := 0
	var got []string
	for _, e := range c.Events() {
		if e.Method != "Tracing.dataCollected" || e.Seq <= seq {
			continue
		}
		chunks++
		var v struct{ Value []struct{ Name string } }
		_ = json.Unmarshal(e.Params, &v)
		for _, ev := range v.Value {
			if strings.HasPrefix(ev.Name, "EventDispatch e") {
				got = append(got, ev.Name[len("EventDispatch "):len("EventDispatch e000")])
			}
		}
	}
	if chunks < 2 {
		t.Fatalf("%d dataCollected events for about 800 KB of events, want several", chunks)
	}
	if len(got) != n || !slices.IsSorted(got) || slices.Compact(slices.Clone(got))[n-1] != got[n-1] {
		t.Fatalf("dataCollected events %d, want %d in order once each: %v", len(got), n, got)
	}
}

// A trace that reaches devtools.MaxTraceRecords Timeline records, or as
// many script events, drops the next ones and reports dataLossOccurred
// true. The test fills the trace's record list to the limit in memory and
// lets the page send one record more (200,000 events through the fake
// would take too long).
func TestCDPTraceRecordLimitReportsDataLoss(t *testing.T) {
	for _, kind := range []string{"timeline", "script"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			_, page := puppeteerAttach(t, c)
			call(t, c, page, "Tracing.start", nil)
			x := extrasByID(t, page)
			x.mu.Lock()
			tr := x.trace
			x.mu.Unlock()
			tr.mu.Lock()
			if kind == "timeline" {
				tr.records = make([]devtools.TimelineRecord, devtools.MaxTraceRecords)
			} else {
				tr.scripts = make([]devtools.ScriptEvent, devtools.MaxTraceRecords)
			}
			tr.mu.Unlock()
			emitRecording(f)
			// The fake's events arrive before the replies that follow them.
			call(t, c, page, "Runtime.evaluate", map[string]any{"expression": "1"})
			tr.mu.Lock()
			nr, ns := len(tr.records), len(tr.scripts)
			tr.mu.Unlock()
			if max(nr, ns) != devtools.MaxTraceRecords {
				t.Fatalf("records %d, script events %d: the list grew past %d", nr, ns, devtools.MaxTraceRecords)
			}
			if done := traceEnd(t, c, page); done["dataLossOccurred"] != true {
				t.Fatalf("tracingComplete %v, want dataLossOccurred true", done)
			}
		})
	}
}

// Malformed Timeline and ScriptProfiler events are ignored; the trace
// keeps recording and holds the well-formed events.
func TestCDPTraceIgnoresMalformedEvents(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", nil)
	f.Emit("Timeline.eventRecorded", map[string]any{"record": "not a record"})
	f.Emit("ScriptProfiler.trackingUpdate", map[string]any{"event": 5})
	emitRecording(f)
	seq := c.Seq()
	if done := traceEnd(t, c, page); done["dataLossOccurred"] != false {
		t.Fatalf("tracingComplete %v", done)
	}
	var names []string
	for _, e := range collected(c, seq) {
		var v struct{ Name string }
		_ = json.Unmarshal([]byte(e), &v)
		names = append(names, v.Name)
	}
	if !slices.Contains(names, "EventDispatch click") || !slices.Contains(names, "Script Other") {
		t.Fatalf("trace events %v, want the well-formed click and script events", names)
	}
}

// Tracing.start refuses parameters it cannot serve before it starts
// anything in the page, and a valid start afterwards works.
func TestCDPTracingStartParameterErrors(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	mark := len(f.Received())
	for _, tc := range []struct {
		params map[string]any
		code   int
		msg    string
	}{
		{map[string]any{"categories": 5}, -32602, "Invalid parameters"},
		{map[string]any{"transferMode": "ReturnAsBlob"}, -32602, "transferMode"},
		{map[string]any{"transferMode": "ReturnAsStream", "streamFormat": "proto"}, -32000, "streamFormat \"proto\" is not supported"},
		{map[string]any{"transferMode": "ReturnAsStream", "streamCompression": "gzip"}, -32000, "streamCompression \"gzip\" is not supported"},
	} {
		e := cdpFail(t, c, page, "Tracing.start", tc.params)
		if e.Code != tc.code || !strings.Contains(e.Message, tc.msg) {
			t.Errorf("Tracing.start %v: %+v, want %d %q", tc.params, e, tc.code, tc.msg)
		}
	}
	for _, m := range sinceMark(f, mark) {
		if strings.HasPrefix(m, "Timeline.") || strings.HasPrefix(m, "ScriptProfiler.") {
			t.Fatalf("a refused Tracing.start sent %s", m)
		}
	}
	call(t, c, page, "Tracing.start", map[string]any{"transferMode": "ReturnAsStream", "streamFormat": "json", "streamCompression": "none"})
	traceEnd(t, c, page)
}

// failWIP makes method fail with a WebKit error until the returned
// function is called; then the fake's own handling comes back.
func failWIP(f *fakewebkit.Server, method string) (restore func()) {
	f.Handle(method, func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate " + method + " failure"}
	})
	return func() { f.Handle(method, nil) }
}

// A failed Timeline.enable fails Tracing.start, undoes the started
// ScriptProfiler run and leaves no trace state, so a retry starts the
// trace.
func TestCDPTracingStartTimelineEnableFailure(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	restore := failWIP(f, "Timeline.enable")
	e := cdpFail(t, c, page, "Tracing.start", nil)
	restore()
	if !strings.Contains(e.Message, "Timeline.enable") || !strings.Contains(e.Message, "deliberate") {
		t.Fatalf("Tracing.start with a failing Timeline.enable: %+v", e)
	}
	st := r.pageStateOf(t, "One")
	if st.HasEntry("cdp:trace:"+page) || st.HasEntry("cdp:trace-script:"+page) || st.Needs("Timeline") || f.Tracking(f.ActiveTarget()) {
		t.Fatal("the failed start left trace state or a ScriptProfiler run")
	}
	call(t, c, page, "Tracing.start", nil)
	emitRecording(f)
	if done := traceEnd(t, c, page); done["dataLossOccurred"] != false {
		t.Fatalf("tracingComplete after the retry %v", done)
	}
}

// A failed Timeline.disable at Tracing.end fails the call and names the
// command. The attachment keeps the trace and the Timeline domain, so
// Tracing.end again disables Timeline and delivers the events, and a
// detach instead disables Timeline in its release.
func TestCDPTracingEndTimelineDisableFailure(t *testing.T) {
	for _, then := range []string{"retry", "detach"} {
		t.Run(then, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			tab, page := puppeteerAttach(t, c)
			call(t, c, page, "Tracing.start", nil)
			emitRecording(f)
			restore := failWIP(f, "Timeline.disable")
			e := cdpFail(t, c, page, "Tracing.end", nil)
			restore()
			if !strings.Contains(e.Message, "Timeline.disable") {
				t.Fatalf("Tracing.end with a failing Timeline.disable: %+v", e)
			}
			st := r.pageStateOf(t, "One")
			if !st.Needs("Timeline") {
				t.Fatal("the failed disable gave up the attachment's Timeline domain")
			}
			mark := len(f.Received())
			if then == "detach" {
				detachAndWait(t, c, tab, page)
				eventually(t, 5*time.Second, "the release's Timeline.disable", func() bool {
					return slices.Contains(sinceMark(f, mark), "Timeline.disable") && !st.Needs("Timeline")
				})
				return
			}
			seq := c.Seq()
			if done := traceEnd(t, c, page); done["dataLossOccurred"] != false {
				t.Fatalf("tracingComplete %v", done)
			}
			if got := sinceMark(f, mark); !slices.Contains(got, "Timeline.disable") || st.Needs("Timeline") {
				t.Fatalf("Tracing.end again sent %v, want Timeline.disable and no Timeline owner left", got)
			}
			if got := collected(c, seq); !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "EventDispatch click") }) {
				t.Fatalf("the trace after the retry lacks the recorded click: %v", got)
			}
			if e := cdpFail(t, c, page, "Tracing.end", nil); e.Message != errTracingNotStarted {
				t.Fatalf("a third Tracing.end: %+v", e)
			}
		})
	}
}

// A release that cannot stop a trace's Timeline recording leaves the
// Timeline agent to be reset: the next trace first sends that reset
// (Timeline.disable), and when the reset fails, Tracing.start fails with a
// message that says so and starts nothing; a retry resets and starts.
func TestCDPTracingStartResetsATimelineThatAReleaseLeft(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	tab, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", map[string]any{"categories": "devtools.timeline"})
	restoreStop := failWIP(f, "Timeline.stop")
	restoreDisable := failWIP(f, "Timeline.disable")
	detachAndWait(t, c, tab, page)
	st := r.pageStateOf(t, "One")
	eventually(t, 5*time.Second, "the release of the trace", func() bool { return !st.HasEntry("cdp:trace:" + page) })
	restoreStop()
	c = dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page2 := puppeteerAttach(t, c)
	mark := len(f.Received())
	e := cdpFail(t, c, page2, "Tracing.start", map[string]any{"categories": "devtools.timeline"})
	if !strings.Contains(e.Message, "a release left") || !strings.Contains(e.Message, "Timeline.disable") {
		t.Fatalf("Tracing.start whose Timeline reset fails: %+v", e)
	}
	if got := sinceMark(f, mark); slices.Contains(got, "Timeline.start") {
		t.Fatalf("the refused start sent %v", got)
	}
	restoreDisable()
	mark = len(f.Received())
	call(t, c, page2, "Tracing.start", map[string]any{"categories": "devtools.timeline"})
	if got := sinceMark(f, mark); !slices.Contains(got, "Timeline.disable") || !slices.Contains(got, "Timeline.start") {
		t.Fatalf("the retried start sent %v, want the reset and Timeline.start", got)
	}
	traceEnd(t, c, page2)
}

// An attachment keeps at most two unread trace streams (maxStreams): a
// third ReturnAsStream trace drops the oldest stream, whose handle is then
// invalid, and the two newer streams read back whole and unchanged.
func TestCDPTraceStreamsEvictTheOldest(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	var handles []string
	for i := range 3 {
		call(t, c, page, "Tracing.start", map[string]any{"transferMode": "ReturnAsStream", "categories": "devtools.timeline"})
		f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
		f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "EventDispatch", "data": map[string]any{"type": fmt.Sprintf("trace%d", i)}, "startTime": 10.001, "endTime": 10.002}})
		done := traceEnd(t, c, page)
		h, _ := done["stream"].(string)
		if h == "" || slices.Contains(handles, h) {
			t.Fatalf("tracingComplete %v: want a new stream handle", done)
		}
		handles = append(handles, h)
	}
	if e := cdpFail(t, c, page, "IO.read", map[string]any{"handle": handles[0]}); e.Code != -32602 || e.Message != "Invalid stream handle" {
		t.Fatalf("IO.read of the oldest stream: %+v", e)
	}
	for i, h := range handles[1:] {
		names := traceNames(decode[traceFile](t, json.RawMessage(readStream(t, c, page, h, 1000))))
		if want := fmt.Sprintf("EventDispatch trace%d", i+1); !slices.Contains(names, want) || slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "EventDispatch trace") && n != want }) {
			t.Fatalf("stream %s: events %v, want %s only", h, names, want)
		}
	}
}

// IO.read never splits a UTF-8 character: with any size, every chunk is
// valid UTF-8 and the chunks join to the stream; a size smaller than the
// character at the position returns that character whole. Malformed
// requests fail with invalid params.
func TestCDPIOReadUTF8Boundaries(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Tracing.start", map[string]any{"transferMode": "ReturnAsStream", "categories": "devtools.timeline"})
	f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
	f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{"type": "EventDispatch", "data": map[string]any{"type": "é€😀ü"}, "startTime": 10.001, "endTime": 10.002}})
	h, _ := traceEnd(t, c, page)["stream"].(string)
	whole := readStream(t, c, page, h, 1<<20)
	if !strings.Contains(whole, "EventDispatch é€😀ü") {
		t.Fatalf("stream %q lacks the event name", whole)
	}
	for _, size := range []int{1, 2, 3, 5} {
		call(t, c, page, "IO.read", map[string]any{"handle": h, "offset": 0, "size": 0x7fffffff})
		var b strings.Builder
		pos := 0
		for i := 0; ; i++ {
			off := map[string]any{"handle": h, "size": size}
			if i == 0 {
				off["offset"] = 0
			}
			r := decode[struct {
				Data string
				EOF  bool
			}](t, call(t, c, page, "IO.read", off))
			if r.EOF {
				break
			}
			if r.Data == "" || !utf8.ValidString(r.Data) {
				t.Fatalf("size %d: chunk %q at %d is empty or not valid UTF-8", size, r.Data, pos)
			}
			if len(r.Data) > size {
				if _, n := utf8.DecodeRuneInString(r.Data); n != len(r.Data) {
					t.Fatalf("size %d: chunk %q is longer than the size and more than one character", size, r.Data)
				}
			}
			b.WriteString(r.Data)
			pos += len(r.Data)
		}
		if b.String() != whole {
			t.Fatalf("size %d: the chunks join to %q, want %q", size, b.String(), whole)
		}
	}
	for _, m := range []string{"IO.read", "IO.close"} {
		if e := cdpFail(t, c, page, m, map[string]any{"handle": 7}); e.Code != -32602 || !strings.Contains(e.Message, "Invalid parameters") {
			t.Errorf("%s with a number handle: %+v", m, e)
		}
	}
	call(t, c, page, "IO.close", map[string]any{"handle": h})
}

// A trace below the record limit whose JSON crosses maxTraceBytes (the
// 32 MiB cap of a trace, docs/notes/w5.md "W5c-1b") keeps its events in
// order up to the cap, drops the rest, and reports the loss in
// Tracing.tracingComplete (CDP: dataLossOccurred), through dataCollected
// (ReportEvents) and through the stream (ReturnAsStream). Each of the 34
// timeline events carries 1 MiB of data, far below the record limit:
// exactly 31 of them fit below 32 MiB with the two metadata events, since
// the data of 32 alone already reaches the cap.
func TestCDPTraceByteLimitReportsDataLoss(t *testing.T) {
	const records, blob, kept = 34, 1 << 20, 31
	big := strings.Repeat("x", blob)
	for _, mode := range []string{"ReportEvents", "ReturnAsStream"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			_, page := puppeteerAttach(t, c)
			call(t, c, page, "Tracing.start", map[string]any{"transferMode": mode})
			f.Emit("Timeline.recordingStarted", map[string]any{"startTime": 10.0})
			for i := range records {
				start := 10 + float64(i+1)/1000
				f.Emit("Timeline.eventRecorded", map[string]any{"record": map[string]any{
					"type": "EventDispatch", "data": map[string]any{"i": i, "blob": big}, "startTime": start, "endTime": start + 0.0005,
				}})
			}
			// The fake's events arrive before the replies that follow them.
			call(t, c, page, "Runtime.evaluate", map[string]any{"expression": "1"})
			seq := c.Seq()
			done := traceEnd(t, c, page)
			if done["dataLossOccurred"] != true {
				t.Fatalf("tracingComplete %v, want dataLossOccurred true", done)
			}
			var events []json.RawMessage
			if mode == "ReportEvents" {
				for _, e := range collected(c, seq) {
					events = append(events, json.RawMessage(e))
				}
			} else {
				handle, _ := done["stream"].(string)
				text := readStream(t, c, page, handle, ioChunk)
				var tf struct{ TraceEvents []json.RawMessage }
				if err := json.Unmarshal([]byte(text), &tf); err != nil {
					t.Fatalf("the stream is not a trace file: %v", err)
				}
				events = tf.TraceEvents
			}
			size := 0
			var got []int
			for _, e := range events {
				size += len(e) + 1
				var v struct {
					Args struct{ Data struct{ I *int } }
				}
				if err := json.Unmarshal(e, &v); err != nil {
					t.Fatalf("trace event: %v", err)
				}
				if v.Args.Data.I != nil {
					got = append(got, *v.Args.Data.I)
				}
			}
			want := make([]int, kept)
			for i := range want {
				want[i] = i
			}
			if !slices.Equal(got, want) || size > maxTraceBytes {
				t.Fatalf("the trace holds the timeline events %v in %d bytes, want the first %d in at most %d", got, size, kept, maxTraceBytes)
			}
		})
	}
}

// A Tracing.start that fails after Timeline.start reached the page, and
// whose undo cannot stop the timeline (Timeline.stop fails), fails and
// says that stopping the started part failed too; the trace's Timeline
// entry stays (tracing_domain.go cdpTrace.begin: "A stop that fails keeps
// its entry"), so the detach's release stops the recording and removes
// the entry.
func TestCDPTraceStartUndoWhoseTimelineStopFails(t *testing.T) {
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
	failOnce(f, "Timeline.stop", "deliberate Timeline stop refusal")
	mark := len(f.Received())
	e := cdpFail(t, c, page, "Tracing.start", nil)
	if !strings.Contains(e.Message, "provisional target refused") || !strings.Contains(e.Message, "stopping the started part failed too") || !strings.Contains(e.Message, "deliberate Timeline stop refusal") {
		t.Fatalf("Tracing.start whose undo fails: %+v", e)
	}
	if countSince(f, mark, "Timeline.start") != 1 || countSince(f, mark, "Timeline.stop") != 1 {
		t.Fatalf("setup: %v, want one Timeline.start and one failed Timeline.stop", sinceMark(f, mark))
	}
	st := r.pageStateOf(t, "One")
	if !st.HasEntry("cdp:trace:" + page) {
		t.Fatal("the failed undo dropped the trace's Timeline entry, so nothing stops the recording")
	}
	detachAndWait(t, c, tab, page)
	// The release sends Timeline.stop again (to each target of the page).
	eventually(t, 5*time.Second, "the release of the timeline", func() bool {
		return countSince(f, mark, "Timeline.stop") > 1 && !st.HasEntry("cdp:trace:"+page)
	})
}
