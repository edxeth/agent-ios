package cdp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
)

// detachPage detaches the page attachment and waits for its detach event.
func (w *w5b2Page) detachPage(t *testing.T) {
	t.Helper()
	call(t, w.c, "", "Target.detachFromTarget", map[string]any{"sessionId": w.page})
	if _, err := w.c.WaitEvent(testCtx(t), 0, "Target.detachedFromTarget", nil); err != nil {
		t.Fatal(err)
	}
}

// cliOverride sets method with params as a CLI command of session s1
// does: a legacy operation whose owner is the session.
func (w *w5b2Page) cliOverride(t *testing.T, method, params string) {
	t.Helper()
	req := w.r.req
	req.Page = "One"
	mo, err := w.r.m.Begin(testCtx(t), req, core.AcquireOptions{Session: "s1", Reservation: "o1"}, "cli", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer w.r.m.End(mo.ID)
	if _, err := w.r.m.RawAs(testCtx(t), mo, core.SessionOwner("legacy", mo.Session), method, []byte(params)); err != nil {
		t.Fatal(err)
	}
}

// only keeps the lines that contain one of the words.
func only(lines []string, words ...string) []string {
	var out []string
	for _, l := range lines {
		for _, w := range words {
			if strings.Contains(l, w) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// Design 5.3 Emulation and 5.2: setEmulatedMedia (WIP setEmulatedMedia and
// overrideUserPreference) and setUserAgentOverride (overrideUserAgent) are
// state of the attachment's desired-state owner. Detaching the attachment
// removes only its own overrides (the media type, the color scheme, the
// user agent go back to the device's); an override that the session's
// CLI commands set (reduced motion) stays.
func TestEmulationOwnedByTheAttachment(t *testing.T) {
	w := newW5b2Page(t)
	w.cliOverride(t, "Page.overrideUserPreference", `{"name":"PrefersReducedMotion","value":"Reduce"}`)
	m := w.mark()
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "print", "features": []any{map[string]any{"name": "prefers-color-scheme", "value": "dark"}}})
	w.call(t, "Emulation.setUserAgentOverride", map[string]any{"userAgent": "cdp-ua", "acceptLanguage": "de"})
	sameLines(t, "overrides sent", only(w.sentSince(m), "Page.setEmulatedMedia", "Page.overrideUser"), []string{
		`Page.setEmulatedMedia {"media":"print"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme","value":"Dark"}`,
		`Page.overrideUserAgent {"value":"cdp-ua"}`,
	})
	m = w.mark()
	w.detachPage(t)
	after := only(w.sentSince(m), "Page.setEmulatedMedia", "Page.overrideUser")
	joined := strings.Join(after, "\n")
	for _, want := range []string{`Page.setEmulatedMedia {"media":""}`, `Page.overrideUserPreference {"name":"PrefersColorScheme"}`, `Page.overrideUserAgent {}`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("after the detach the page received:\n%s\nwant %s", joined, want)
		}
	}
	if strings.Contains(joined, "PrefersReducedMotion") {
		t.Fatalf("the detach touched the session's CLI override:\n%s", joined)
	}
}

// A clear (an empty value) sends nothing unless this attachment set the
// override (Playwright sends empty values and forced-colors none on every
// page setup, and they must not remove the session's CLI overrides); a
// clear of the attachment's own override is sent. Baseline values of
// features that iOS WebKit cannot emulate are accepted and change nothing.
func TestEmulationClearsOnlyItsOwnOverrides(t *testing.T) {
	w := newW5b2Page(t)
	m := w.mark()
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "", "features": []any{
		map[string]any{"name": "prefers-color-scheme", "value": ""},
		map[string]any{"name": "forced-colors", "value": "none"},
		map[string]any{"name": "prefers-reduced-motion", "value": ""},
		map[string]any{"name": "color-gamut", "value": ""},
	}})
	w.call(t, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ""})
	if s := w.sentSince(m); len(s) != 0 {
		t.Fatalf("clears of overrides the attachment never set sent %v", s)
	}
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "screen", "features": []any{map[string]any{"name": "prefers-contrast", "value": "more"}}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "", "features": []any{map[string]any{"name": "prefers-contrast", "value": ""}, map[string]any{"name": "prefers-color-scheme", "value": ""}}})
	w.call(t, "Emulation.setUserAgentOverride", map[string]any{"userAgent": "x"})
	w.call(t, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ""})
	w.call(t, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ""})
	sameLines(t, "sent", only(w.sentSince(m), "Page.setEmulatedMedia", "Page.overrideUser"), []string{
		`Page.setEmulatedMedia {"media":"screen"}`,
		`Page.overrideUserPreference {"name":"PrefersContrast","value":"More"}`,
		`Page.setEmulatedMedia {"media":""}`,
		`Page.overrideUserPreference {"name":"PrefersContrast"}`,
		`Page.overrideUserAgent {"value":"x"}`,
		`Page.overrideUserAgent {}`,
	})
}

// Review finding F4 (W5b-2 review): setEmulatedMedia keeps the presence
// of its fields, as pinned Puppeteer needs (puppeteer-core 25.16.0
// EmulationManager.ts:477-532 sends media and features in separate
// calls). An omitted media changes no media type; an omitted features
// list changes no feature; a supplied list replaces the attachment's
// feature set, so an omitted feature is cleared and [] clears all of the
// attachment's overrides (in name order), never the session's CLI
// override.
func TestEmulationMediaKeepsFieldPresence(t *testing.T) {
	w := newW5b2Page(t)
	w.cliOverride(t, "Page.overrideUserPreference", `{"name":"PrefersReducedMotion","value":"Reduce"}`)
	m := w.mark()
	feature := func(name, value string) map[string]any { return map[string]any{"name": name, "value": value} }
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "print"})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{feature("prefers-color-scheme", "dark")}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "screen"})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{feature("prefers-color-scheme", "dark"), feature("prefers-contrast", "more")}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{feature("prefers-contrast", "more"), feature("forced-colors", "none")}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": ""})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{feature("prefers-contrast", "more"), feature("prefers-color-scheme", "dark")}})
	w.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{}})
	sameLines(t, "sent", only(w.sentSince(m), "Page.setEmulatedMedia", "Page.overrideUser"), []string{
		`Page.setEmulatedMedia {"media":"print"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme","value":"Dark"}`,
		`Page.setEmulatedMedia {"media":"screen"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme","value":"Dark"}`,
		`Page.overrideUserPreference {"name":"PrefersContrast","value":"More"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme"}`,
		`Page.overrideUserPreference {"name":"PrefersContrast","value":"More"}`,
		`Page.overrideUserPreference {"name":"PrefersContrast"}`,
		`Page.setEmulatedMedia {"media":""}`,
		`Page.overrideUserPreference {"name":"PrefersContrast","value":"More"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme","value":"Dark"}`,
		`Page.overrideUserPreference {"name":"PrefersColorScheme"}`,
		`Page.overrideUserPreference {"name":"PrefersContrast"}`,
	})
}

// setEmulatedMedia refuses a media type other than print, screen or empty
// (invalid params) and a feature value that iOS WebKit cannot emulate;
// nothing is sent.
func TestEmulationMediaErrors(t *testing.T) {
	w := newW5b2Page(t)
	m := w.mark()
	if e := w.fail(t, "Emulation.setEmulatedMedia", map[string]any{"media": "tv"}); e.Code != codeInvalidParams {
		t.Fatalf("media tv: %+v", e)
	}
	for _, f := range []map[string]any{{"name": "forced-colors", "value": "active"}, {"name": "prefers-color-scheme", "value": "purple"}} {
		if e := w.fail(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{f}}); !strings.Contains(e.Message, "cannot be emulated on iOS WebKit") {
			t.Fatalf("feature %v: %+v", f, e)
		}
	}
	if s := w.sentSince(m); len(s) != 0 {
		t.Fatalf("refused overrides sent %v", s)
	}
}

// Design 5.3 Emulation setGeolocationOverride: W4's geolocation shim is a
// bootstrap part of the attachment's owner and is installed in the current
// document; accuracy defaults to 1. A detach restores navigator.geolocation
// and sends the bootstrap script without the shim. A position without
// coordinates cannot be emulated, and coordinates out of range are
// invalid.
func TestEmulationGeolocation(t *testing.T) {
	w := newW5b2Page(t)
	m := w.mark()
	w.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 48.5, "longitude": 2.25})
	sent := w.sentSince(m)
	var boot, eval bool
	shim, _ := json.Marshal(devtools.GeoShim(48.5, 2.25, 1))
	for _, s := range sent {
		boot = boot || strings.HasPrefix(s, "Page.setBootstrapScript") && strings.Contains(s, "48.5")
		eval = eval || strings.HasPrefix(s, "Runtime.evaluate") && strings.Contains(s, string(shim))
	}
	if !boot || !eval {
		t.Fatalf("setGeolocationOverride sent:\n%s\nwant the shim as a bootstrap part and in the current document", strings.Join(sent, "\n"))
	}
	m = w.mark()
	w.detachPage(t)
	restore, _ := json.Marshal(devtools.GeoRestore)
	var restored, reboot bool
	for _, s := range w.sentSince(m) {
		restored = restored || strings.HasPrefix(s, "Runtime.evaluate") && strings.Contains(s, string(restore))
		reboot = reboot || strings.HasPrefix(s, "Page.setBootstrapScript") && !strings.Contains(s, "48.5")
	}
	if !restored || !reboot {
		t.Fatalf("after the detach the page received:\n%s\nwant the restore and a bootstrap script without the shim", strings.Join(w.sentSince(m), "\n"))
	}
	w2 := newW5b2Page(t)
	if e := w2.fail(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 1}); !strings.Contains(e.Message, "cannot be emulated") {
		t.Fatalf("no longitude: %+v", e)
	}
	if e := w2.fail(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 91, "longitude": 0}); e.Code != codeInvalidParams {
		t.Fatalf("latitude 91: %+v", e)
	}
}

// cliGeo sets the geolocation override of session s1 as its CLI command
// "set geo" does (W4, devtools.Page.SetGeolocation).
func (w *w5b2Page) cliGeo(t *testing.T, lat, lng float64) {
	t.Helper()
	req := w.r.req
	req.Page = "One"
	mo, err := w.r.m.Begin(testCtx(t), req, core.AcquireOptions{Session: "s1", Reservation: "o1"}, "cli", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer w.r.m.End(mo.ID)
	p, err := devtools.For(w.r.m.LookupPage(mo.Page.Key).PS)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetGeolocation(testCtx(t), mo.Op.Caller(), "s1", lat, lng, 1); err != nil {
		t.Fatal(err)
	}
}

// Review finding F2 (W5b-2 review; design 5.2: a detach removes only the
// attachment's state): with the session's CLI geolocation override set,
// detaching a CDP override restores the current document and installs the
// CLI shim there again, and the bootstrap script keeps the CLI shim only.
func TestEmulationGeolocationDetachKeepsTheCLIOverride(t *testing.T) {
	w := newW5b2Page(t)
	w.cliGeo(t, 12, 34)
	w.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 56, "longitude": 78})
	m := w.mark()
	w.detachPage(t)
	var evals []string
	var boot string
	for _, rec := range w.f.Received()[m:] {
		var p map[string]any
		_ = json.Unmarshal(rec.Params, &p)
		switch rec.Method {
		case "Runtime.evaluate":
			if e, _ := p["expression"].(string); e == devtools.GeoRestore || strings.Contains(e, "navigator.geolocation") {
				evals = append(evals, e)
			}
		case "Page.setBootstrapScript":
			boot, _ = p["source"].(string)
		}
	}
	if len(evals) != 2 || evals[0] != devtools.GeoRestore || evals[1] != devtools.GeoShim(12, 34, 1) {
		t.Fatalf("the detach evaluated:\n%s\nwant the restore, then the CLI shim", strings.Join(evals, "\n---\n"))
	}
	if !strings.Contains(boot, devtools.GeoShim(12, 34, 1)) || strings.Contains(boot, "latitude: 56") {
		t.Fatalf("the bootstrap script after the detach:\n%s\nwant the CLI shim without the CDP one", boot)
	}
}

// Design 5.3 Emulation: setDeviceMetricsOverride and
// clearDeviceMetricsOverride fail with "viewport is fixed by the device",
// except an override equal to the real viewport (CSS width and height,
// deviceScaleFactor 0 or the real one), which succeeds; 0, 0 and 0 means
// no override and succeeds without a page call.
func TestEmulationViewportIsFixed(t *testing.T) {
	w := newW5b2Page(t)
	w.pf.setPage(func(src, _ string, _ json.RawMessage) any {
		if src == "[innerWidth, innerHeight, devicePixelRatio]" {
			return []any{390, 844, 3}
		}
		return nil
	})
	m := w.mark()
	w.call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 0, "height": 0, "deviceScaleFactor": 0, "mobile": true})
	if s := w.sentSince(m); len(s) != 0 {
		t.Fatalf("a zero override sent %v", s)
	}
	w.call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 3, "mobile": true})
	w.call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 0, "mobile": true})
	for _, p := range []map[string]any{
		{"width": 800, "height": 600, "deviceScaleFactor": 1, "mobile": false},
		{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true},
	} {
		e := w.fail(t, "Emulation.setDeviceMetricsOverride", p)
		if e.Message != "viewport is fixed by the device: the page is 390x844 CSS pixels at scale 3; connect with defaultViewport: null" {
			t.Fatalf("override %v: %+v", p, e)
		}
	}
	if e := w.fail(t, "Emulation.clearDeviceMetricsOverride", map[string]any{}); e.Message != "viewport is fixed by the device" {
		t.Fatalf("clearDeviceMetricsOverride: %+v", e)
	}
}

// set geo follows the bridge's rule (parity-045 review F5): the last set
// wins, also for a replaced override. A CLI override set again after a CDP
// override must run last in the bootstrap script, so new documents get
// the CLI's newer position.
func TestCLIGeoReplacedOverrideBecomesLatest(t *testing.T) {
	w := newW5b2Page(t)
	w.cliGeo(t, 1, 0)
	w.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 2, "longitude": 0})
	m := w.mark()
	w.cliGeo(t, 3, 0)
	boot := ""
	for _, rec := range w.f.Received()[m:] {
		if rec.Method == "Page.setBootstrapScript" {
			var p struct{ Source string }
			_ = json.Unmarshal(rec.Params, &p)
			boot = p.Source
		}
	}
	cdp, cli := strings.Index(boot, devtools.GeoShim(2, 0, 1)), strings.Index(boot, devtools.GeoShim(3, 0, 1))
	if cdp < 0 || cli < 0 || cli < cdp || strings.Contains(boot, devtools.GeoShim(1, 0, 1)) {
		t.Fatalf("the bootstrap script after set geo 3 0:\n%s\nwant the CDP shim (latitude 2), then the replaced CLI shim (latitude 3)", boot)
	}
}
