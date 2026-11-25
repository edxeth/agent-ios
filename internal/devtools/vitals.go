package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// "vitals" (design 4.2): Web Vitals read in the page. Navigation timing
// (TTFB, DOMContentLoaded, load) and paint timing (FCP) exist on iOS 16.7;
// LCP, CLS and event timing (INP) only where the engine lists the entry
// type in PerformanceObserver.supportedEntryTypes (Safari 26.2+ for LCP
// and event timing, MAP 1.12). A metric the page cannot give is reported
// as unavailable with the reason.

// vitalsWaitMs is how long the page function waits for the buffered
// PerformanceObserver entries, which arrive in a later task.
const vitalsWaitMs = 100

// vitalsFn returns a Promise of the metrics as JSON text: for each metric
// {value: number} or {unavailable: reason}. Times are milliseconds from
// the start of the navigation. INP is the longest duration of an
// interaction (event entries with an interactionId), CLS the sum of the
// layout shifts without recent input (a single session window is not
// formed).
const vitalsFn = `function iwdpVitals(a) {
  var types = (window.PerformanceObserver && PerformanceObserver.supportedEntryTypes) || [];
  function has(t) { return Array.prototype.indexOf.call(types, t) >= 0; }
  function val(v) { return {value: v}; }
  function no(why) { return {unavailable: why}; }
  var out = {};
  var nav = performance.getEntriesByType ? performance.getEntriesByType("navigation")[0] : null;
  if (nav) {
    out.ttfb = val(nav.responseStart);
    out.domContentLoaded = nav.domContentLoadedEventEnd > 0 ? val(nav.domContentLoadedEventEnd) : no("DOMContentLoaded has not finished");
    out.load = nav.loadEventEnd > 0 ? val(nav.loadEventEnd) : no("the load event has not finished");
  } else if (performance.timing && performance.timing.navigationStart) {
    var t = performance.timing, s = t.navigationStart;
    out.ttfb = val(t.responseStart - s);
    out.domContentLoaded = t.domContentLoadedEventEnd > 0 ? val(t.domContentLoadedEventEnd - s) : no("DOMContentLoaded has not finished");
    out.load = t.loadEventEnd > 0 ? val(t.loadEventEnd - s) : no("the load event has not finished");
  } else {
    out.ttfb = out.domContentLoaded = out.load = no("this page has no navigation timing");
  }
  var fcp = performance.getEntriesByName ? performance.getEntriesByName("first-contentful-paint")[0] : null;
  out.fcp = fcp ? val(fcp.startTime) : no(has("paint") ? "nothing has been painted yet" : "this engine has no paint timing");
  var lcp = null, cls = 0, inp = 0, observers = [];
  function observe(type, fn) {
    if (!has(type)) return false;
    try {
      var po = new PerformanceObserver(function (list) { list.getEntries().forEach(fn); });
      po.observe({type: type, buffered: true});
      observers.push(po);
      return true;
    } catch (e) { return false; }
  }
  var hasLCP = observe("largest-contentful-paint", function (e) { lcp = e; });
  var hasCLS = observe("layout-shift", function (e) { if (!e.hadRecentInput) cls += e.value; });
  var hasINP = observe("event", function (e) { if (e.interactionId) inp = Math.max(inp, e.duration); });
  return new Promise(function (resolve) {
    setTimeout(function () {
      observers.forEach(function (po) { po.disconnect(); });
      out.lcp = hasLCP ? (lcp ? val(lcp.renderTime || lcp.loadTime || lcp.startTime) : no("no largest-contentful-paint entry yet")) : no("this engine has no largest-contentful-paint entries");
      out.cls = hasCLS ? val(cls) : no("this engine has no layout-shift entries");
      out.inp = hasINP ? (inp > 0 ? val(inp) : no("no interaction yet")) : no("this engine has no event timing entries");
      resolve(JSON.stringify(out));
    }, a.waitMs);
  });
}`

// VitalsMetric is one metric: a value, or the reason it is unavailable.
type VitalsMetric struct {
	Value       *float64 `json:"value,omitempty"`
	Unavailable string   `json:"unavailable,omitempty"`
}

// Vitals are the metrics of the page's current document.
type Vitals struct {
	LCP              VitalsMetric `json:"lcp"`
	CLS              VitalsMetric `json:"cls"`
	TTFB             VitalsMetric `json:"ttfb"`
	FCP              VitalsMetric `json:"fcp"`
	INP              VitalsMetric `json:"inp"`
	DOMContentLoaded VitalsMetric `json:"domContentLoaded"`
	Load             VitalsMetric `json:"load"`
}

// vitalsRow is a metric with its label and JSON name.
type vitalsRow struct {
	label, key string
	m          *VitalsMetric
}

// metrics lists the metrics in output order.
func (v *Vitals) metrics() []vitalsRow {
	return []vitalsRow{
		{"LCP", "lcp", &v.LCP},
		{"CLS", "cls", &v.CLS},
		{"TTFB", "ttfb", &v.TTFB},
		{"FCP", "fcp", &v.FCP},
		{"INP", "inp", &v.INP},
		{"DOMContentLoaded", "domContentLoaded", &v.DOMContentLoaded},
		{"Load", "load", &v.Load},
	}
}

// ReadVitals runs the vitals function in the page.
func ReadVitals(ctx context.Context, c webkit.Caller) (Vitals, error) {
	args, _ := json.Marshal(map[string]int{"waitMs": vitalsWaitMs})
	res, err := tools.EvaluateScriptWithOptions(ctx, c, "("+vitalsFn+")("+string(args)+")", tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
	if err != nil {
		return Vitals{}, fmt.Errorf("reading the vitals in the page: %w", err)
	}
	var text string
	if err := json.Unmarshal(res.Result.Value, &text); err != nil {
		return Vitals{}, fmt.Errorf("vitals: the page returned %s, not JSON text", res.Result.Type)
	}
	var v Vitals
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return Vitals{}, fmt.Errorf("vitals: decoding the page's reply: %w", err)
	}
	for _, m := range v.metrics() {
		if (m.m.Value == nil) == (m.m.Unavailable == "") {
			return Vitals{}, fmt.Errorf("vitals: the page gave %s neither a value nor a reason", m.key)
		}
	}
	return v, nil
}

// Text renders one line per metric: milliseconds rounded to whole numbers
// (halves away from zero), CLS as a score with three decimals.
func (v Vitals) Text() string {
	lines := make([]string, 0, 7)
	for _, m := range v.metrics() {
		switch {
		case m.m.Value == nil:
			lines = append(lines, fmt.Sprintf("%s: unavailable (%s)", m.label, m.m.Unavailable))
		case m.key == "cls":
			lines = append(lines, fmt.Sprintf("%s: %.3f", m.label, *m.m.Value))
		default:
			lines = append(lines, fmt.Sprintf("%s: %d ms", m.label, int64(math.Round(*m.m.Value))))
		}
	}
	return strings.Join(lines, "\n")
}
