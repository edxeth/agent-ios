//go:build webkitgtk

package webkitgtk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/e2e/jscov"
)

// shimFile is the page shims' path in the repository, the file of their
// coverage record.
const shimFile = "internal/cdp/page_shims.js"

// jscovVar names the LCOV file that TestCheckVisibilityShimMatchesNative
// writes the page shims' line coverage to (docs/notes/w5.md, "JavaScript
// coverage of the page shims"); unset, the test measures the coverage and
// writes nothing.
const jscovVar = "IWDP_JSCOV_LCOV"

// shimCoverage turns on JavaScriptCore's control-flow profiler, which must
// be on before a script is compiled, and the Debugger domain, whose
// Debugger.scriptParsed events give the script ids of the evaluated shim
// sources (source names it by its sourceURL). The returned function reads
// the basic blocks of every such script (Runtime.getBasicBlocks), turns
// both off again and returns the line counts of the shim file, summed over
// the scripts.
func shimCoverage(t *testing.T, source string) func() map[int]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ids []string
	stop := sess.Subscribe("Debugger.scriptParsed", func(_ string, p json.RawMessage) {
		var s struct {
			ScriptID  string `json:"scriptId"`
			SourceURL string `json:"sourceURL"`
		}
		if json.Unmarshal(p, &s) == nil {
			if f, ok := jscov.File(s.SourceURL); ok && f == shimFile {
				mu.Lock()
				ids = append(ids, s.ScriptID)
				mu.Unlock()
			}
		}
	})
	off := func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, m := range []string{"Debugger.disable", "Runtime.disableControlFlowProfiler"} {
			if _, err := sess.Send(ctx, m, nil); err != nil {
				t.Errorf("%s: %v", m, err)
			}
		}
	}
	for _, m := range []string{"Runtime.enableControlFlowProfiler", "Debugger.enable"} {
		if _, err := sess.Send(ctx, m, nil); err != nil {
			off()
			t.Fatalf("%s: %v", m, err)
		}
	}
	return func() map[int]int {
		defer off()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mu.Lock()
		defer mu.Unlock()
		lines := map[int]int{}
		for _, id := range ids {
			raw, err := sess.Send(ctx, "Runtime.getBasicBlocks", map[string]string{"sourceID": id})
			if err != nil {
				t.Fatalf("Runtime.getBasicBlocks of script %s: %v", id, err)
			}
			var r struct {
				BasicBlocks []jscov.Block `json:"basicBlocks"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("decoding the basic blocks of script %s: %v", id, err)
			}
			got, err := jscov.Lines(source, r.BasicBlocks)
			if err != nil {
				t.Fatalf("script %s: %v", id, err)
			}
			jscov.Add(lines, got)
		}
		if len(ids) == 0 || len(lines) == 0 {
			t.Fatalf("the control-flow profiler reported %d scripts and %d lines of %s; want the shim's evaluations", len(ids), len(lines), shimFile)
		}
		return lines
	}
}

// pageShimsSource reads the page compatibility shims of CDP attachments
// (internal/cdp/page_shims.js) as the endpoint sends them.
func pageShimsSource(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "internal", "cdp", "page_shims.js"))
	if err != nil {
		t.Fatalf("reading the page shims: %v", err)
	}
	return string(src)
}

// shimOracleJS compares the checkVisibility shim with WebKitGTK's native
// Element.prototype.checkVisibility on every case of checkvisibility.html,
// the shadow-DOM cases and detached elements, with undefined, null and
// every combination of the five CheckVisibilityOptions members. %s is the
// shim source as a JSON string. The native method is restored before the
// script returns.
const shimOracleJS = `(function (src) {
  var P = Element.prototype, native = P.checkVisibility;
  if (typeof native !== "function" || native.__iwdpShim) { return { error: "this WebKitGTK has no native checkVisibility" }; }
  var keptNative = (0, eval)(src);
  if (keptNative.length !== 0 || P.checkVisibility !== native) { return { error: "with a native checkVisibility the shim script installed " + JSON.stringify(keptNative) }; }
  delete P.checkVisibility;
  var installed, shim;
  try { installed = (0, eval)(src); shim = P.checkVisibility; } finally { P.checkVisibility = native; }
  var desc = Object.getOwnPropertyDescriptor(P, "checkVisibility");
  var cases = [];
  var add = function (list) { for (var i = 0; i < list.length; i++) { cases.push({ id: list[i].id, el: list[i] }); } };
  add(document.querySelectorAll('[id^="cv-"]'));
  window.cvShadowRoots.forEach(function (r) { add(r.querySelectorAll('[id^="cv-"]')); });
  var detached = document.createElement("div"); detached.textContent = "detached";
  cases.push({ id: "detached-new", el: detached });
  var removed = document.getElementById("cv-plain").cloneNode(true); document.body.appendChild(removed); removed.remove();
  cases.push({ id: "detached-removed", el: removed });
  var keys = ["checkOpacity", "checkVisibilityCSS", "contentVisibilityAuto", "opacityProperty", "visibilityProperty"];
  var opts = [undefined, null];
  for (var m = 0; m < 32; m++) { var o = {}; keys.forEach(function (k, i) { if (m & (1 << i)) { o[k] = true; } }); opts.push(o); }
  var outcome = function (f, thisArg, opt) { try { return String(f.call(thisArg, opt)); } catch (e) { return "throws " + (e && e.name); } };
  var diffs = [], table = {}, n = 0;
  cases.forEach(function (c) {
    table[c.id] = [outcome(native, c.el), outcome(native, c.el, { checkOpacity: true, checkVisibilityCSS: true })];
    opts.forEach(function (o) {
      n++;
      var a = outcome(native, c.el, o), b = outcome(shim, c.el, o);
      if (a !== b) { diffs.push({ id: c.id, options: o === undefined ? "undefined" : JSON.stringify(o), native: a, shim: b }); }
    });
  });
  var errors = [[document, undefined], [document.createTextNode("t"), undefined], [document.body, 5], [document.body, "x"], [document.body, true],
    [document.body, 0], [document.body, ""], [document.body, false], [document.body, NaN], [document.body, Symbol("s")], [document.body, BigInt(1)]];
  errors.forEach(function (e) {
    n++;
    var a = outcome(native, e[0], e[1]), b = outcome(shim, e[0], e[1]);
    if (a !== b) { diffs.push({ id: "error:" + String(e[0].nodeName) + ":" + String(e[1]), native: a, shim: b }); }
  });
  // Edge cases, each run once with the native method and once with the
  // shim: option getters with side effects (WebIDL converts the options
  // dictionary before the method's steps), the order and number of option
  // reads, ToBoolean conversion, and light-DOM children assigned to a slot
  // in a closed shadow root.
  var edges = [];
  var edge = function (id, run) {
    n++;
    var a = run(native), b = run(shim);
    edges.push({ id: id, native: a, shim: b });
  };
  var fresh = function (style) {
    var el = document.createElement("div"); el.textContent = "edge target"; el.setAttribute("style", style); document.body.appendChild(el);
    return el;
  };
  var withFresh = function (style, options) {
    return function (f) { var el = fresh(style); try { return outcome(f, el, options(el)); } finally { el.remove(); } };
  };
  edge("getter-hides-element", withFresh("", function (el) { return { get checkOpacity() { el.style.display = "none"; return false; } }; }));
  edge("getter-reveals-element", withFresh("display: none", function (el) { return { get checkOpacity() { el.style.display = "block"; return false; } }; }));
  edge("getter-sets-opacity", withFresh("", function (el) { return { get checkOpacity() { el.style.opacity = "0"; return true; } }; }));
  edge("getter-sets-visibility", withFresh("", function (el) { return { get visibilityProperty() { el.style.visibility = "hidden"; return true; } }; }));
  edge("getter-throws", withFresh("", function () { return { get checkVisibilityCSS() { throw new RangeError("from the getter"); } }; }));
  var reads = function (thisArg) {
    return function (f) {
      var log = [];
      var o = new Proxy({}, { get: function (t, k) { log.push(String(k)); return undefined; } });
      return outcome(f, thisArg, o) + " reads " + log.join(",");
    };
  };
  edge("read-order", reads(document.body));
  edge("read-order-bad-receiver", reads(document));
  edge("read-order-hidden-element", reads(document.getElementById("cv-none")));
  var truthy = { zero: 0, emptyString: "", nan: NaN, zeroString: "0", object: {}, documentAll: document.all, nullValue: null };
  Object.keys(truthy).forEach(function (k) {
    edge("to-boolean-" + k, function (f) { return outcome(f, document.getElementById("cv-opacity"), { checkOpacity: truthy[k] }); });
  });
  edge("options-function", function (f) { var o = function () {}; o.checkOpacity = true; return outcome(f, document.getElementById("cv-opacity"), o); });
  // document.all is an object whose typeof is "undefined" ([[IsHTMLDDA]]);
  // WebIDL converts it to a dictionary like any other object.
  edge("options-document-all", function (f) { return outcome(f, document.body, document.all); });
  // Closed shadow roots: page script cannot see the slot of a light-DOM
  // child assigned to a slot in a closed root (assignedSlot is null).
  var closedHost = function (inner) {
    var host = document.createElement("div"), child = document.createElement("span");
    child.textContent = "closed slotted child"; host.appendChild(child); document.body.appendChild(host);
    host.attachShadow({ mode: "closed" }).innerHTML = inner;
    return { host: host, child: child };
  };
  var closedCases = {
    "opacity": '<div style="opacity: 0"><slot></slot></div>',
    "visibility": '<div style="visibility: hidden"><slot></slot></div>',
    "display-none": '<div style="display: none"><slot></slot></div>',
    "details-closed": '<details><summary>s</summary><slot></slot></details>',
    "details-open": '<details open><summary>s</summary><slot></slot></details>',
    "select": '<select><slot></slot></select>'
  };
  var closedOptions = { "none": undefined, "checkOpacity": { checkOpacity: true }, "opacityProperty": { opacityProperty: true }, "checkVisibilityCSS": { checkVisibilityCSS: true }, "visibilityProperty": { visibilityProperty: true } };
  Object.keys(closedCases).forEach(function (k) {
    Object.keys(closedOptions).forEach(function (o) {
      edge("closed-root-slotted-" + k + ":" + o, function (f) {
        var c = closedHost(closedCases[k]);
        try { return outcome(f, c.child, closedOptions[o]); } finally { c.host.remove(); }
      });
    });
  });
  return {
    installed: installed, marker: shim.__iwdpShim === true && !Object.keys(shim).length, name: shim.name, length: shim.length,
    descriptor: { writable: desc.writable, enumerable: desc.enumerable, configurable: desc.configurable },
    cases: cases.length, comparisons: n, diffs: diffs, edges: edges, table: table
  };
})(%s)`

type shimOracle struct {
	Error      string   `json:"error"`
	Installed  []string `json:"installed"`
	Marker     bool     `json:"marker"`
	Name       string   `json:"name"`
	Length     int      `json:"length"`
	Descriptor struct {
		Writable, Enumerable, Configurable bool
	} `json:"descriptor"`
	Cases       int `json:"cases"`
	Comparisons int `json:"comparisons"`
	Diffs       []struct {
		ID, Options, Native, Shim string
	} `json:"diffs"`
	Edges []struct {
		ID, Native, Shim string
	} `json:"edges"`
	Table map[string][2]string `json:"table"`
}

// closedRootLimit is the documented limit of the shim (docs/cdp.md, "Page
// compatibility shims", Limits): page script cannot see a slot in a closed
// shadow root (the assignedSlot of a light-DOM child assigned to it is
// null), so the shim's flat-tree walk goes from that child straight to the
// host and misses the slot's ancestors in the closed root. Each entry is
// an edge case of shimOracleJS with the expected native and shim answers.
//   - An opacity:0 ancestor of the slot is missed: checkOpacity and
//     opacityProperty answer true, the native method false.
//   - A closed <details> ancestor of the slot is missed: WebKitGTK lays its
//     content out with content-visibility: hidden, which keeps client
//     rects, so every call answers true (observed here). Safari 16 lays that
//     content out in a display:none slot without client rects (inferred, not
//     run on a device), where the shim's box test answers false as the
//     native method does. Safari has the native method from 17.4.
var closedRootLimit = map[string][2]string{
	"closed-root-slotted-opacity:checkOpacity":              {"false", "true"},
	"closed-root-slotted-opacity:opacityProperty":           {"false", "true"},
	"closed-root-slotted-details-closed:none":               {"false", "true"},
	"closed-root-slotted-details-closed:checkOpacity":       {"false", "true"},
	"closed-root-slotted-details-closed:opacityProperty":    {"false", "true"},
	"closed-root-slotted-details-closed:checkVisibilityCSS": {"false", "true"},
	"closed-root-slotted-details-closed:visibilityProperty": {"false", "true"},
}

// The checkVisibility shim of CDP attachments (internal/cdp/page_shims.js,
// CSSOM View checkVisibility) gives the same answer as WebKitGTK's native
// method for every case of checkvisibility.html and every option
// combination; with the native method present it installs nothing; the
// installed method is a WebIDL operation with the shim marker.
func TestCheckVisibilityShimMatchesNative(t *testing.T) {
	navigate(t, "checkvisibility.html", "window.cvShadowRoots && window.cvShadowRoots.length === 3")
	source := pageShimsSource(t) + "\n//# sourceURL=" + jscov.SourceURL(shimFile)
	coverage := shimCoverage(t, source)
	src, _ := json.Marshal(source)
	var r shimOracle
	if err := json.Unmarshal(eval(t, fmt.Sprintf(shimOracleJS, src)), &r); err != nil {
		t.Fatalf("decoding the oracle's result: %v", err)
	}
	lines := coverage()
	hit := 0
	for _, n := range lines {
		if n > 0 {
			hit++
		}
	}
	t.Logf("coverage of %s: %d of %d lines executed", shimFile, hit, len(lines))
	if out := os.Getenv(jscovVar); out != "" {
		var b bytes.Buffer
		if err := jscov.WriteLCOV(&b, shimFile, lines); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, b.Bytes(), 0o644); err != nil {
			t.Fatalf("writing the coverage record (%s): %v", jscovVar, err)
		}
	}
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if strings.Join(r.Installed, ",") != "checkVisibility" || !r.Marker || r.Name != "checkVisibility" || r.Length != 0 {
		t.Errorf("installed %v, marker %v, name %q, length %d; want [checkVisibility], the __iwdpShim marker only, name checkVisibility, length 0", r.Installed, r.Marker, r.Name, r.Length)
	}
	if d := r.Descriptor; !d.Writable || !d.Enumerable || !d.Configurable {
		t.Errorf("property descriptor %+v; want writable, enumerable and configurable (a WebIDL operation)", d)
	}
	const minCases = 50
	if r.Cases < minCases {
		t.Fatalf("the oracle compared %d cases, want at least %d", r.Cases, minCases)
	}
	for _, d := range r.Diffs {
		t.Errorf("%s with options %s: native %s, shim %s", d.ID, d.Options, d.Native, d.Shim)
	}
	known := 0
	for _, e := range r.Edges {
		t.Logf("edge %s: native %s, shim %s", e.ID, e.Native, e.Shim)
		want, limit := closedRootLimit[e.ID]
		switch {
		case limit && (e.Native != want[0] || e.Shim != want[1]):
			t.Errorf("edge %s: native %s, shim %s; want the documented closed-shadow-root limit (native %s, shim %s); update docs/cdp.md when the limit changed", e.ID, e.Native, e.Shim, want[0], want[1])
		case limit:
			known++
		case e.Native != e.Shim:
			t.Errorf("edge %s: native %s, shim %s", e.ID, e.Native, e.Shim)
		}
	}
	if seen := len(r.Edges); seen < len(closedRootLimit) {
		t.Errorf("the oracle ran %d edge cases, fewer than the %d documented limits", seen, len(closedRootLimit))
	}
	t.Logf("%d edge cases, %d of them the documented closed-shadow-root limit", len(r.Edges), known)
	t.Logf("%d cases, %d comparisons, %d disagreements; native (no options, checkOpacity+checkVisibilityCSS): %v", r.Cases, r.Comparisons, len(r.Diffs), r.Table)
}
