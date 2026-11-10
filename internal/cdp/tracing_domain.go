package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Tracing (design 5.3, tier 2): Tracing.start and Tracing.end with the
// events dataCollected and tracingComplete. A trace is a WIP Timeline
// recording, with the ScriptProfiler's script events when the categories
// ask for JavaScript, converted by W4's devtools.ConvertTimeline.
//
// Ownership: the recording is state of the attachment's desired-state
// owner. Its entries (Timeline, and the ScriptProfiler run) belong to that
// owner, so the attachment's detach, or the end of its CDP connection,
// releases them and stops the recording in the page (conn.cleanup), as
// the lease release does for W4's "trace start". The in-memory part ends
// with the attachment (attachmentExtras).
//
// Puppeteer (page.tracing, which chrome-devtools-mcp's
// performance_start_trace uses) and Playwright (browser.startTracing) ask
// for transferMode ReturnAsStream, so the trace is then kept for IO.read
// and IO.close (io_domain.go). Tracing is served on page sessions only;
// Playwright's browser.startTracing() without a page sends it on the
// browser session and gets "wasn't found".

func init() {
	register(
		Method{Name: "Tracing.start", Levels: OnPage, Handle: tracingStart},
		Method{Name: "Tracing.end", Levels: OnPage, Handle: tracingEnd},
	)
	registerEvents("Tracing", "dataCollected", "tracingComplete")
}

// Chrome's texts (tracing_handler.cc).
const (
	errTracingStarted    = "Tracing has already been started (possibly in another tab)."
	errTracingNotStarted = "Tracing is not started"
)

// Bounds of a trace's data.
const (
	// maxTraceBytes bounds the JSON of one trace (events beyond it are
	// dropped and dataLossOccurred is true).
	maxTraceBytes = 32 << 20
	// traceChunkBytes bounds the events of one Tracing.dataCollected.
	traceChunkBytes = 256 << 10
	// timelineDepth is the stack depth that Timeline.start records, as W4's
	// "trace start" asks for.
	timelineDepth = 5
)

// attachmentExtras is the recording state of one attachment: its trace,
// its profile, and the streams of its ended traces. It lives until the
// attachment detaches (session.gone), and then its subscriptions end and
// its streams are dropped.
type attachmentExtras struct {
	mu        sync.Mutex
	trace     *cdpTrace
	prof      *cdpProfile
	profOn    bool
	streams   map[string]*traceStream
	streamSeq int
}

var extras = struct {
	sync.Mutex
	by map[*session]*attachmentExtras
}{by: map[*session]*attachmentExtras{}}

// extrasOf returns the recording state of s, made on first use.
func extrasOf(s *session) *attachmentExtras {
	extras.Lock()
	defer extras.Unlock()
	if x := extras.by[s]; x != nil {
		return x
	}
	x := &attachmentExtras{streams: map[string]*traceStream{}}
	extras.by[s] = x
	go func() {
		<-s.gone
		extras.Lock()
		delete(extras.by, s)
		extras.Unlock()
		x.drop()
	}()
	return x
}

// drop ends the recording state of a detached attachment. The page side
// is the release's (conn.cleanup).
func (x *attachmentExtras) drop() {
	x.mu.Lock()
	t, p := x.trace, x.prof
	x.trace, x.prof, x.streams = nil, nil, map[string]*traceStream{}
	x.mu.Unlock()
	if t != nil {
		t.stop()
	}
	if p != nil {
		p.stop()
	}
}

// subscribeSync subscribes in the read order of the page connection
// (SubscribeSync of core's page connection), so a recording sees its last
// events before the reply to its stop, as W4's recordings do.
func subscribeSync(c webkit.Caller) func(string, webkit.EventHandler) func() {
	if ss, ok := c.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	}); ok {
		return ss.SubscribeSync
	}
	return c.Subscribe
}

// wipNoEffect reports an error after which the command did not run: it
// was not sent, or WebKit refused it.
func wipNoEffect(err error) bool {
	var pe *webkit.ErrorData
	return errors.Is(err, webkit.ErrNotSent) || errors.As(err, &pe)
}

// cdpTrace is a running trace of one attachment. It takes Timeline events
// only after its Timeline.start was sent (armed) and script events only
// after the trackingStart of its own ScriptProfiler run, and no events of
// a part whose stop succeeded (end), as W4's trace does.
type cdpTrace struct {
	stream bool // transferMode ReturnAsStream
	script bool // with the ScriptProfiler's script events

	mu                         sync.Mutex
	armed                      bool
	scriptArmed, scriptStarted bool
	records                    []devtools.TimelineRecord
	scripts                    []devtools.ScriptEvent
	start                      float64
	lost                       bool // records dropped at devtools.MaxTraceRecords
	unsub                      []func()
}

func (t *cdpTrace) subscribe(c webkit.Caller) {
	sub := subscribeSync(c)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unsub = append(t.unsub,
		sub("Timeline.eventRecorded", func(_ string, params json.RawMessage) {
			var v struct {
				Record devtools.TimelineRecord `json:"record"`
			}
			if json.Unmarshal(params, &v) != nil {
				return
			}
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.armed {
				return
			}
			if len(t.records) >= devtools.MaxTraceRecords {
				t.lost = true
				return
			}
			t.records = append(t.records, v.Record)
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
				Event devtools.ScriptEvent `json:"event"`
			}
			if json.Unmarshal(params, &v) != nil {
				return
			}
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.scriptStarted {
				return
			}
			if len(t.scripts) >= devtools.MaxTraceRecords {
				t.lost = true
				return
			}
			t.scripts = append(t.scripts, v.Event)
		}))
}

func (t *cdpTrace) arm() {
	t.mu.Lock()
	t.armed = true
	t.mu.Unlock()
}

func (t *cdpTrace) armScript() {
	t.mu.Lock()
	t.scriptArmed, t.scriptStarted = true, false
	t.mu.Unlock()
}

// end closes the event admission of the parts whose stop succeeded.
func (t *cdpTrace) end(timeline, script bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if timeline {
		t.armed = false
	}
	if script {
		t.scriptArmed, t.scriptStarted = false, false
	}
}

func (t *cdpTrace) stop() {
	t.mu.Lock()
	unsub := t.unsub
	t.unsub = nil
	t.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// Desired-state keys of an attachment's trace. The script key sorts
// before the Timeline key, so a release stops the run first, as W4's keys
// do.
func traceKeys(s *session) (timeline, script string) {
	return "cdp:trace:" + s.id, "cdp:trace-script:" + s.id
}

// timelineEntry ties the Timeline recording to the attachment's owner: its
// release stops the recording. It belongs to the Timeline agent, which the
// owner holds, so a release that cannot send the stop marks the agent for
// core's reset (Timeline.disable stops a recording).
func timelineEntry(key string, o core.Owner, t *cdpTrace) core.Entry {
	return core.Entry{
		Key: key, Owner: o, Domain: "Timeline",
		Apply: func(context.Context, core.Sender) error { return nil }, // a new target gets no recording
		Release: func(ctx context.Context, s core.Sender) error {
			t.stop()
			_, err := s.Send(ctx, "Timeline.stop", nil)
			return err
		},
	}
}

// wantsScript reports whether categories ask for JavaScript: none given
// (Chrome's default includes v8), "*", or a category of V8 (v8.execute,
// disabled-by-default-v8.cpu_profiler).
func wantsScript(included []string) bool {
	if len(included) == 0 {
		return true
	}
	for _, c := range included {
		if c == "*" || strings.Contains(c, "v8") {
			return true
		}
	}
	return false
}

func tracingStart(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Categories        string `json:"categories"`
		TransferMode      string `json:"transferMode"`
		StreamFormat      string `json:"streamFormat"`
		StreamCompression string `json:"streamCompression"`
		TraceConfig       *struct {
			IncludedCategories []string `json:"includedCategories"`
		} `json:"traceConfig"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	switch {
	case p.TransferMode != "" && p.TransferMode != "ReportEvents" && p.TransferMode != "ReturnAsStream":
		return nil, nil, invalidParam("transferMode %q: want ReportEvents or ReturnAsStream", p.TransferMode)
	case p.StreamFormat != "" && p.StreamFormat != "json":
		return nil, nil, serverError("Tracing.start: streamFormat %q is not supported; the bridge writes json traces", p.StreamFormat)
	case p.StreamCompression != "" && p.StreamCompression != "none":
		return nil, nil, serverError("Tracing.start: streamCompression %q is not supported; the bridge writes uncompressed traces", p.StreamCompression)
	}
	var included []string
	if p.TraceConfig != nil {
		included = p.TraceConfig.IncludedCategories
	} else {
		for _, c := range strings.Split(p.Categories, ",") {
			if c = strings.TrimSpace(c); c != "" && !strings.HasPrefix(c, "-") {
				included = append(included, c)
			}
		}
	}
	s := call.session
	t := &cdpTrace{stream: p.TransferMode == "ReturnAsStream", script: wantsScript(included)}
	x := extrasOf(s)
	x.mu.Lock()
	if x.trace != nil {
		x.mu.Unlock()
		return nil, nil, serverError(errTracingStarted)
	}
	x.trace = t
	x.mu.Unlock()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return t.begin(ctx, s, mo)
	})
	if err != nil {
		x.mu.Lock()
		keep := x.trace == t && t.mayRun(s)
		if !keep && x.trace == t {
			x.trace = nil
		}
		x.mu.Unlock()
		if !keep {
			t.stop()
		}
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// mayRun reports whether a part of a failed start may still record in the
// page: its entry stayed, so Tracing.end or the detach stops it.
func (t *cdpTrace) mayRun(s *session) bool {
	tk, sk := traceKeys(s)
	st := s.pg.PS.State
	return st.HasEntry(tk) || st.HasEntry(sk)
}

// begin starts the recording in one desired-state change: the script run
// first (core.StartScriptProfiler refuses it while another recording holds
// the page's ScriptProfiler), then Timeline as a domain of the
// attachment's owner, its entry, and Timeline.start on the active target.
// Any Timeline user of the page (another attachment's trace, W4's "trace
// start", a legacy Timeline.enable) makes the start fail with Chrome's
// text. A start that fails after a part started stops that part again.
func (t *cdpTrace) begin(ctx context.Context, s *session, mo *core.ManagedOp) error {
	ps := s.pg.PS
	oc := mo.Op.Caller()
	tk, sk := traceKeys(s)
	t.subscribe(ps.Caller)
	timeline, scripts := false, false
	err := ps.State.Change(ctx, oc, func(cc webkit.Caller) error {
		if ps.State.Needs("Timeline") {
			return serverError(errTracingStarted)
		}
		if t.script {
			var err error
			_, scripts, err = core.StartScriptProfiler(ctx, ps, cc, core.ScriptProfilerEntry(sk, s.owner, nil), map[string]any{"includeSamples": false}, t.armScript)
			if err != nil {
				return scriptProfilerBusy("Tracing.start", err)
			}
		}
		if err := core.ResetUnknownAgent(ctx, ps, cc, "Timeline"); err != nil {
			return fmt.Errorf("starting the trace: stopping a Timeline recording that a release left: %w", err)
		}
		if ps.State.Acquire(s.owner, "Timeline") {
			if _, err := cc.Send(ctx, "Timeline.enable", nil); err != nil {
				// An enable that may have reached the page keeps the
				// owner's Timeline, so the detach's release disables it.
				if wipNoEffect(err) {
					ps.State.ReleaseDomain(s.owner, "Timeline")
				}
				return fmt.Errorf("starting the trace: Timeline.enable: %w", err)
			}
		}
		ps.State.Put(timelineEntry(tk, s.owner, t))
		t.arm()
		if _, err := oc.Send(ctx, "Timeline.start", map[string]int{"maxCallStackDepth": timelineDepth}); err != nil {
			timeline = !wipNoEffect(err)
			return fmt.Errorf("starting the trace: Timeline.start: %w", err)
		}
		timeline = true
		return nil
	})
	if err == nil {
		return nil
	}
	// Undo: stop what may run, then give up the entries and the domain. A
	// stop that fails keeps its entry, so Tracing.end or the detach stops
	// it.
	uerr := ps.State.Change(ctx, oc, func(cc webkit.Caller) error {
		var errs []error
		if timeline {
			if _, e := oc.Send(ctx, "Timeline.stop", nil); e != nil {
				errs = append(errs, fmt.Errorf("stopping the timeline (Timeline.stop): %w", e))
			} else {
				t.end(true, false)
			}
		}
		if scripts {
			if _, e := core.StopScriptProfiler(ctx, ps, cc, sk); e != nil {
				errs = append(errs, e)
			} else {
				t.end(false, true)
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		ps.State.Remove(tk)
		return releaseTimeline(ctx, s, cc)
	})
	if uerr != nil {
		return fmt.Errorf("%w; stopping the started part failed too (%w): Tracing.end or the detach stops it", err, uerr)
	}
	return err
}

// releaseTimeline gives up the attachment's Timeline domain and disables
// Timeline when the attachment was its last owner. When the disable fails,
// the attachment keeps the domain, so Tracing.end again or the detach's
// release sends the disable again.
func releaseTimeline(ctx context.Context, s *session, cc webkit.Caller) error {
	if s.pg.PS.State.ReleaseDomain(s.owner, "Timeline") {
		if _, err := cc.Send(ctx, "Timeline.disable", nil); err != nil {
			s.pg.PS.State.Acquire(s.owner, "Timeline")
			return fmt.Errorf("disabling the timeline (Timeline.disable): %w", err)
		}
	}
	return nil
}

// scriptProfilerBusy names the recording that holds the page's
// ScriptProfiler run.
func scriptProfilerBusy(method string, err error) error {
	var busy *core.ScriptProfilerBusyError
	if !errors.As(err, &busy) {
		return err
	}
	return serverError("%s: the page's ScriptProfiler runs one recording at a time, and %s holds it; stop that recording first (Profiler.stop, Tracing.end, 'iwdp-cli profiler stop' or 'iwdp-cli trace stop' of its session)", method, busy.Owner)
}

func tracingEnd(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	x := extrasOf(s)
	x.mu.Lock()
	t := x.trace
	x.mu.Unlock()
	if t == nil {
		return nil, nil, serverError(errTracingNotStarted)
	}
	tk, sk := traceKeys(s)
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		ps := s.pg.PS
		oc := mo.Op.Caller()
		return ps.State.Change(ctx, oc, func(cc webkit.Caller) error {
			var errs []error
			if ps.State.HasEntry(tk) {
				if _, err := oc.Send(ctx, "Timeline.stop", nil); err != nil {
					errs = append(errs, fmt.Errorf("stopping the timeline (Timeline.stop): %w", err))
				} else {
					ps.State.Remove(tk)
				}
			}
			if ps.State.HasEntry(sk) {
				if _, err := core.StopScriptProfiler(ctx, ps, cc, sk); err != nil {
					errs = append(errs, err)
				}
			}
			t.end(!ps.State.HasEntry(tk), !ps.State.HasEntry(sk))
			if len(errs) > 0 {
				return fmt.Errorf("ending the trace: %w; the recording may still run: send Tracing.end again, or the detach stops it", errors.Join(errs...))
			}
			return releaseTimeline(ctx, s, cc)
		})
	})
	if err != nil {
		return nil, nil, err
	}
	x.mu.Lock()
	if x.trace == t {
		x.trace = nil
	}
	x.mu.Unlock()
	t.stop()
	t.mu.Lock()
	tr := devtools.ConvertTimeline(t.records, t.scripts, t.start)
	lost := t.lost
	t.mu.Unlock()
	events, cut := encodeEvents(tr.TraceEvents, maxTraceBytes)
	lost = lost || cut
	c, sid := call.conn, call.SessionID
	if t.stream {
		handle := x.addStream(traceJSON(events))
		call.after = func() {
			c.event(sid, "Tracing.tracingComplete", map[string]any{
				"dataLossOccurred": lost, "stream": handle, "traceFormat": "json", "streamCompression": "none",
			})
		}
	} else {
		call.after = func() {
			for _, chunk := range chunkEvents(events, traceChunkBytes) {
				c.event(sid, "Tracing.dataCollected", map[string]any{"value": chunk})
			}
			c.event(sid, "Tracing.tracingComplete", map[string]any{"dataLossOccurred": lost})
		}
	}
	return map[string]any{}, nil, nil
}

// encodeEvents encodes the events in order until their JSON reaches limit
// bytes; cut reports dropped events.
func encodeEvents(evs []devtools.TraceEvent, limit int) (out []json.RawMessage, cut bool) {
	n := 0
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil || n+len(b)+1 > limit {
			return out, true
		}
		n += len(b) + 1
		out = append(out, b)
	}
	return out, false
}

// traceJSON is a Chrome trace file (JSON object format) of the events.
func traceJSON(events []json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteString(`{"traceEvents":[`)
	for i, e := range events {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteString(`],"displayTimeUnit":"ms"}`)
	return b.Bytes()
}

// chunkEvents splits events into Tracing.dataCollected values of about
// limit bytes each (an event larger than limit is a chunk of its own).
func chunkEvents(events []json.RawMessage, limit int) [][]json.RawMessage {
	var out [][]json.RawMessage
	var cur []json.RawMessage
	size := 0
	for _, e := range events {
		if len(cur) > 0 && size+len(e) > limit {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, e)
		size += len(e)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// traceStream is the data of an ended ReturnAsStream trace, read with
// IO.read and dropped by IO.close or the detach.
type traceStream struct {
	data []byte
	pos  int
}

// maxStreams bounds the unread streams of one attachment; a new stream
// drops the oldest, so an attachment holds at most maxStreams *
// maxTraceBytes.
const maxStreams = 2

// addStream keeps data as a new stream and returns its handle (a decimal
// number, as Chrome's handles are).
func (x *attachmentExtras) addStream(data []byte) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.streamSeq++
	h := strconv.Itoa(x.streamSeq)
	x.streams[h] = &traceStream{data: data}
	for len(x.streams) > maxStreams {
		oldest := ""
		for k := range x.streams {
			if oldest == "" || streamNumber(k) < streamNumber(oldest) {
				oldest = k
			}
		}
		delete(x.streams, oldest)
	}
	return h
}

func streamNumber(h string) int {
	n, _ := strconv.Atoi(h) // every handle is a number from addStream
	return n
}
