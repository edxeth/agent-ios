package devtools

import (
	"context"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// W5c-1b parity (e): WebKit runs one Timeline recording per page, and a
// CDP client's Tracing.start (internal/cdp) records it as a Timeline entry
// of its attachment's owner. "trace start" beside such a recording would
// record into it, and its Timeline.stop would end the other trace, so it
// is refused before anything is sent and names the holder and its stop.
// The CDP endpoint's own test of both orders is internal/cdp
// TestCDPAndCLITracesOnOnePage; this one pins the texts for each kind of
// holder, also of a held ScriptProfiler.
func TestTraceStartRefusedWhileAnotherRecordingHoldsTheTimeline(t *testing.T) {
	holder := func(key string, o core.Owner) core.Entry {
		return core.Entry{
			Key: key, Owner: o, Domain: "Timeline",
			Apply:   func(context.Context, core.Sender) error { return nil },
			Release: func(context.Context, core.Sender) error { return nil },
		}
	}
	for _, tc := range []struct {
		owner core.Owner
		want  []string
	}{
		{"cdp-0A1B:s1", []string{"cdp-0A1B:s1 holds it", `a CDP client of session "s1" (attachment 0A1B)`, "Tracing.end, or detach the attachment"}},
		{"legacy:x", []string{"legacy:x holds it: stop that recording first"}},
	} {
		t.Run(string(tc.owner), func(t *testing.T) {
			r := newRig(t)
			p := r.page(t)
			p.ps.State.Put(holder("other:trace", tc.owner))
			mark := lastSeq(r.f)
			for _, script := range []bool{true, false} {
				err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", script) })
				if err == nil {
					t.Fatalf("trace start (script %v) beside %s succeeded", script, tc.owner)
				}
				for _, w := range tc.want {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("trace start (script %v): %v, want %q", script, err, w)
					}
				}
			}
			for _, m := range []string{"Timeline.enable", "Timeline.start", "ScriptProfiler.startTracking"} {
				if n := len(receivedAfter(r.f, m, mark)); n != 0 {
					t.Errorf("the refused trace start sent %s %d times", m, n)
				}
			}
			if p.ps.State.HasEntry("devtools:trace:s1") || p.ps.State.HasEntry(traceScriptKey("s1")) {
				t.Error("the refused trace start left its entries")
			}
			p.ps.State.Remove("other:trace")
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", true) })
		})
	}
	// A CDP attachment's profile holds the ScriptProfiler run.
	r := newRig(t)
	p := r.page(t)
	p.ps.State.Put(core.ScriptProfilerEntry("cdp:profiler:0A1B", "cdp-0A1B:s1", nil))
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartProfiler(ctx, c, "s1") })
	if err == nil || !strings.Contains(err.Error(), `a CDP client of session "s1" (attachment 0A1B)`) || !strings.Contains(err.Error(), "Profiler.stop or Tracing.end, or detach the attachment") {
		t.Fatalf("profiler start beside a CDP profile: %v, want the CDP holder and its stop", err)
	}
}
