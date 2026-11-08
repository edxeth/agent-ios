package cdp

import (
	"strings"
	"testing"
	"time"
)

// Design 7.8 item 7: Performance.enable and Performance.getMetrics
// succeed on attach (Puppeteer fails its attach otherwise); the metrics
// come from the page, and the JS heap from a Memory tracking update when
// one was seen; the bridge never starts or stops tracking itself.
func TestPerformanceOnAttach(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	fp.eval = func(expr string) (any, bool) {
		if expr != metricsExpr {
			return nil, false
		}
		return value(map[string]any{"Timestamp": 1700000000.5, "Documents": 1, "Frames": 1, "Nodes": 42, "NavigationStart": 1699999999.0, "DomContentLoaded": 1699999999.25}), true
	}
	call(t, c, sid, "Performance.enable", nil)
	metrics := func() map[string]float64 {
		res := decode[struct {
			Metrics []struct {
				Name  string
				Value float64
			}
		}](t, call(t, c, sid, "Performance.getMetrics", nil))
		out := map[string]float64{}
		for _, m := range res.Metrics {
			out[m.Name] = m.Value
		}
		return out
	}
	m := metrics()
	if m["Nodes"] != 42 || m["NavigationStart"] != 1699999999.0 || m["DomContentLoaded"] != 1699999999.25 || m["Timestamp"] == 0 {
		t.Fatalf("metrics %v", m)
	}
	if _, ok := m["JSHeapUsedSize"]; ok {
		t.Fatalf("a JS heap without a Memory tracking update: %v", m)
	}
	fp.f.Emit("Memory.trackingUpdate", map[string]any{"event": map[string]any{"timestamp": 1, "categories": []any{
		map[string]any{"type": "javascript", "size": 1234}, map[string]any{"type": "images", "size": 5},
	}}})
	eventually(t, 5*time.Second, "the JS heap metric", func() bool { return metrics()["JSHeapUsedSize"] == 1234 })
	call(t, c, sid, "Performance.disable", nil)
	for _, rec := range r.fake["One"].Received() {
		if strings.HasPrefix(rec.Method, "Memory.") {
			t.Fatalf("the bridge sent %s", rec.Method)
		}
	}
}
