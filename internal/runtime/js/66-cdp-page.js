// 66-cdp-page.js (W5b-1): page hooks of the CDP bridge (design 5.3,
// internal/cdp). cdpNav reports same-document navigations of the top
// document (hashchange, popstate, history.pushState and replaceState) and
// cdpBind installs bindings; both write marked console.debug messages that
// the bridge turns into Page.navigatedWithinDocument and
// Runtime.bindingCalled and hides from console output. cdpDialogs turns on
// the dialog override (80-dialogs.js) with a policy, without taking the
// reported uses that the "dialogs" call returns.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, w = window, h = history, dbg = console.debug.bind(console), nav = false, last = "";
  function say(m, o) {
    try { dbg(m + JSON.stringify(o)); } catch (e) { /* console replaced by the page */ }
  }
  function moved(t) {
    if (location.href === last) { return; } // popstate and hashchange of one fragment change
    last = location.href;
    say("__iwdp_nav__ ", { u: last, t: t });
  }
  // popstate also fires, before hashchange, for a fragment navigation:
  // a change of the fragment alone is a fragment navigation.
  function popped() {
    moved(location.href.split("#")[0] === last.split("#")[0] ? "fragment" : "historyApi");
  }
  I.def("cdpNav", function () {
    if (nav || w !== w.top) { return nav; }
    nav = true;
    last = location.href;
    w.addEventListener("hashchange", function () { moved("fragment"); });
    w.addEventListener("popstate", popped);
    ["pushState", "replaceState"].forEach(function (k) {
      var f = h[k];
      h[k] = function () {
        var r = f.apply(this, arguments);
        moved("historyApi");
        return r;
      };
    });
    return nav;
  });
  I.def("cdpBind", function (a) {
    (a.add || []).forEach(function (b) {
      w[b.n] = function (p) { say("__iwdp_bind__ ", { n: b.n, k: b.k, p: String(p) }); };
    });
    (a.del || []).forEach(function (n) {
      try { delete w[n]; } catch (e) { /* not configurable */ }
    });
    return true;
  });
  I.def("cdpDialogs", function (a) {
    _.dlgOn(a.policy);
    return true;
  });
})(window.__iwdpBuilding);
