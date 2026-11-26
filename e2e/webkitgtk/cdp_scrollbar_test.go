//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"testing"
)

// Review F3 (FIX-U): with classic scrollbars (GTK_OVERLAY_SCROLLING=0) an
// unzoomed page with a vertical scrollbar has visualViewport.width =
// innerWidth less the scrollbar, and Page.getLayoutMetrics still reports
// visualViewport.scale 1 and zoom 1, as Chrome does: Playwright's
// viewport screenshot (innerWidth / scale CSS pixels) keeps the scrollbar
// column, as the endpoint's own viewport capture does.
func TestCDPLayoutMetricsWithAClassicScrollbar(t *testing.T) {
	t.Setenv("GTK_OVERLAY_SCROLLING", "0")
	p := cdp045Client(t, "cdp045.html")
	var w struct{ IW, VW, SC float64 }
	raw := p.evalString(t, "document.body.insertAdjacentHTML('beforeend', '<div style=\"height:5000px\"></div>'); JSON.stringify({iw: innerWidth, vw: visualViewport.width, sc: visualViewport.scale})")
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("page widths %q: %v", raw, err)
	}
	if w.SC != 1 || w.VW >= w.IW {
		t.Fatalf("fixture: innerWidth %v, visualViewport.width %v, scale %v; want a classic scrollbar on an unzoomed page", w.IW, w.VW, w.SC)
	}
	var m struct {
		VisualViewport, CSSVisualViewport struct{ Scale, Zoom float64 }
	}
	if err := json.Unmarshal(p.call(t, "Page.getLayoutMetrics", nil), &m); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]struct{ Scale, Zoom float64 }{"visualViewport": m.VisualViewport, "cssVisualViewport": m.CSSVisualViewport} {
		if v.Scale != 1 || v.Zoom != 1 {
			t.Fatalf("%s scale %v, zoom %v; want 1 and 1 (innerWidth %v, visualViewport.width %v)", name, v.Scale, v.Zoom, w.IW, w.VW)
		}
	}
}
