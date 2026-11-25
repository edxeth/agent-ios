package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// jsRoutePatch is the JavaScript fallback of "network route --mode js"
// (design 4.2): fetch and XMLHttpRequest are patched once per document,
// and window.__iwdpRoute.rules holds the session's rules. A matching
// request is delayed, aborted (fetch rejects with a TypeError, XHR fires
// error) or answered with the rule's status and body; a rule with neither
// lets the request through after its delay. A request held by a rule
// (during its delay) continues unchanged when the rule goes away
// ("network unroute", 7.7 test 3), as WIP's interceptContinue does. An
// XHR answer is a set of own properties on the instance (the names in
// fake); open() deletes them, so a reused XMLHttpRequest shows its next
// response, and drops an answer still held for the earlier request.
// ES2017, no dependencies.
const jsRoutePatch = `(function (rules) {
  var W = window;
  if (!W.__iwdpRoute) {
    var R = W.__iwdpRoute = {rules: [], held: []};
    var origFetch = W.fetch, XHR = W.XMLHttpRequest, P = XHR && XHR.prototype;
    var match = function (u) {
      var abs = String(u);
      try { abs = new URL(abs, location.href).href; } catch (e) {}
      for (var i = 0; i < R.rules.length; i++) {
        if (new RegExp(R.rules[i].re).test(abs)) return R.rules[i];
      }
      return null;
    };
    // hold calls go(false) after the rule's delay, or go(true) at once
    // when the rule is removed first (release): continue unchanged.
    var hold = function (r, go) {
      var h = {id: r.id, done: false};
      var fire = function (unchanged) {
        if (h.done) return;
        h.done = true;
        clearTimeout(h.timer);
        R.held.splice(R.held.indexOf(h), 1);
        go(unchanged);
      };
      h.release = function () { fire(true); };
      R.held.push(h);
      h.timer = setTimeout(function () { fire(false); }, r.delay || 0);
    };
    var fulfills = function (r) { return r.status || r.body !== undefined; };
    var headersOf = function (r) {
      var h = {"Content-Type": r.contentType || "text/plain"};
      for (var k in (r.headers || {})) h[k] = r.headers[k];
      return h;
    };
    if (origFetch) {
      W.fetch = function (input, init) {
        var r = match(input && input.url ? input.url : input);
        if (!r) return origFetch.apply(this, arguments);
        var self = this, args = arguments;
        return new Promise(function (res) { hold(r, res); }).then(function (unchanged) {
          if (unchanged) return origFetch.apply(self, args);
          if (r.abort) throw new TypeError("Load failed (agent-ios route abort)");
          if (fulfills(r)) return new Response(r.body || "", {status: r.status || 200, headers: headersOf(r)});
          return origFetch.apply(self, args);
        });
      };
    }
    if (P) {
      var open = P.open, send = P.send;
      var fake = ["readyState", "status", "statusText", "responseText", "response", "responseURL", "getAllResponseHeaders", "getResponseHeader"];
      P.open = function (m, u) {
        if (this.__iwdpFaked) {
          for (var i = 0; i < fake.length; i++) delete this[fake[i]];
          this.__iwdpFaked = false;
        }
        this.__iwdpURL = u;
        this.__iwdpOpen = (this.__iwdpOpen || 0) + 1;
        return open.apply(this, arguments);
      };
      P.send = function () {
        var x = this, args = arguments, r = x.__iwdpURL !== undefined ? match(x.__iwdpURL) : null;
        if (!r) return send.apply(x, args);
        var opened = x.__iwdpOpen;
        hold(r, function (unchanged) {
          if (x.__iwdpOpen !== opened) return; // open() started another request
          if (unchanged || (!r.abort && !fulfills(r))) { send.apply(x, args); return; }
          var status = r.abort ? 0 : (r.status || 200), body = r.abort ? "" : (r.body || "");
          var h = headersOf(r), text = "";
          for (var k in h) text += k + ": " + h[k] + "\r\n";
          x.__iwdpFaked = true;
          var def = function (n, v) { Object.defineProperty(x, n, {configurable: true, get: function () { return v; }}); };
          def("readyState", 4); def("status", status); def("statusText", r.abort ? "" : "OK");
          def("responseText", body); def("response", body); def("responseURL", r.abort ? "" : String(x.__iwdpURL));
          x.getAllResponseHeaders = function () { return r.abort ? "" : text; };
          x.getResponseHeader = function (n) { for (var k in h) if (k.toLowerCase() === String(n).toLowerCase()) return h[k]; return null; };
          x.dispatchEvent(new Event("readystatechange"));
          x.dispatchEvent(new ProgressEvent(r.abort ? "error" : "load"));
          x.dispatchEvent(new ProgressEvent("loadend"));
        });
      };
    }
  }
  var keep = {};
  for (var i = 0; i < rules.length; i++) keep[rules[i].id] = true;
  W.__iwdpRoute.rules = rules;
  W.__iwdpRoute.held.slice().forEach(function (h) { if (!keep[h.id]) h.release(); });
})`

// jsRoutesClear empties the current document's JS route rules.
const jsRoutesClear = "if (window.__iwdpRoute) { window.__iwdpRoute.rules = []; window.__iwdpRoute.held.slice().forEach(function (h) { h.release(); }); } true"

type jsRule struct {
	ID          int               `json:"id"` // Route.ID: held requests of a removed rule continue
	Re          string            `json:"re"`
	Abort       bool              `json:"abort,omitempty"`
	Status      int               `json:"status,omitempty"`
	Body        *string           `json:"body,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Delay       int64             `json:"delay,omitempty"` // ms
}

// JSRoutesSource returns the bootstrap source that installs rules.
func JSRoutesSource(routes []Route) (string, error) {
	rules := []jsRule{}
	for _, r := range routes {
		rules = append(rules, jsRule{
			ID: r.ID, Re: r.Regex, Abort: r.Abort, Status: r.Status, Body: r.Body,
			ContentType: r.ContentType, Headers: r.Headers, Delay: r.Delay.Milliseconds(),
		})
	}
	// json.Marshal escapes <, >, &, U+2028 and U+2029, so the literal is
	// safe inside a script.
	b, err := json.Marshal(rules)
	if err != nil {
		return "", err
	}
	return jsRoutePatch + "(" + string(b) + ");", nil
}

// syncJSRoutes makes the bootstrap part "routes:<session>" and the current
// document hold session's JS routes. applied reports whether the page may
// run the new rules: Page.setBootstrapScript succeeded or its outcome is
// unknown (a lost or late reply), whatever happened to the current
// document's install after it. It is false only when nothing reached the
// page: a local error, or a refused or unsent bootstrap script (noEffect).
func (p *Page) syncJSRoutes(ctx context.Context, c webkit.Caller, session string) (applied bool, err error) {
	var js []Route
	for _, r := range p.Routes(session) {
		if r.Mode == ModeJS {
			js = append(js, r)
		}
	}
	name := "routes:" + session
	var src string
	if len(js) == 0 {
		p.ps.Boot.Remove(name)
		src = jsRoutesClear
	} else {
		if src, err = JSRoutesSource(js); err != nil {
			return false, err
		}
		if err := p.ps.State.SetPart(owner("route", session), name, core.RankShim, src); err != nil {
			return false, err
		}
	}
	if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": p.ps.Boot.Script()}); err != nil {
		return !noEffect(err), fmt.Errorf("Page.setBootstrapScript: %w", err)
	}
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": src, "returnByValue": true}); err != nil {
		return true, fmt.Errorf("installing the JS routes in the current document: %w", err)
	}
	return true, nil
}

// logf writes to the daemon log (the daemon's standard error).
func (p *Page) logf(format string, args ...any) { log.Printf("devtools: "+format, args...) }
