package cdp

import (
	"context"
	"encoding/json"
	"sort"
)

// The Performance domain (design docs/design/agentic-parity.md, sections
// 5.2 and 5.3; acceptance 7.8 item 7). Puppeteer fails its attach when
// Performance.enable fails (Page.ts lines 433-447), so enable and disable
// always succeed. getMetrics reads the page: navigation timing, the DOM
// node count and the frame and document counts. The JS heap comes from
// WIP Memory tracking where it is present: the bridge never starts a
// tracking run (stopping it could end another owner's run, for example a
// "memory" recording), it reports the size of the javascript category of
// the last Memory.trackingUpdate it saw on the page.

func init() {
	register(
		Method{Name: "Performance.enable", Levels: OnPage},
		Method{Name: "Performance.disable", Levels: OnPage},
		Method{Name: "Performance.getMetrics", Levels: OnPage, Handle: getMetrics},
	)
	registerEvents("Performance")
	onPageEvent("Memory.trackingUpdate", memoryUpdate)
}

// metricsExpr reads the page's metrics. Times are seconds since the
// epoch (the page's clock for all of them, so that clients can subtract
// them); Chrome's Timestamp is monotonic, which clients only subtract too.
const metricsExpr = `(function () {
  var t = performance.timing || {}, n = 0, docs = 1, w = document.createTreeWalker(document, NodeFilter.SHOW_ALL);
  while (w.nextNode()) { n++; }
  for (var i = 0; i < frames.length; i++) { try { if (frames[i].document) { docs++; } } catch (e) {} }
  var m = {Timestamp: Date.now() / 1000, Documents: docs, Frames: frames.length + 1, Nodes: n + 1};
  if (t.navigationStart) { m.NavigationStart = t.navigationStart / 1000; }
  if (t.domContentLoadedEventEnd) { m.DomContentLoaded = t.domContentLoadedEventEnd / 1000; }
  if (t.loadEventEnd) { m.LoadEventEnd = t.loadEventEnd / 1000; }
  return m;
})()`

func getMetrics(ctx context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	var m map[string]float64
	if err := call.session.evalInto(ctx, call.Method, metricsExpr, &m); err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	if ps.jsHeap > 0 {
		m["JSHeapUsedSize"] = ps.jsHeap
	}
	ps.mu.Unlock()
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]map[string]any, len(names))
	for i, k := range names {
		out[i] = map[string]any{"name": k, "value": m[k]}
	}
	return map[string]any{"metrics": out}, nil, nil
}

// memoryUpdate keeps the javascript category size of a Memory tracking
// update (WIP Memory.trackingUpdate{event{categories[{type, size}]}}).
func memoryUpdate(ps *pageState, _ string, params json.RawMessage) {
	var p struct {
		Event struct {
			Categories []struct {
				Type string  `json:"type"`
				Size float64 `json:"size"`
			} `json:"categories"`
		} `json:"event"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	for _, c := range p.Event.Categories {
		if c.Type == "javascript" {
			ps.jsHeap = c.Size
		}
	}
}
