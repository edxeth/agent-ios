// 70-waits.js (W3b): one wait slice (design 3.6) and pushstate. A slice
// checks its condition on every DOM mutation and every 100 ms, and ends
// with {ok, state} when the condition holds or the slice ends; Go repeats
// slices until the wait deadline.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, w = window, RS = { loading: 0, interactive: 1, complete: 2 };
  // removed: the ref's element, handed out in this document, is no longer
  // in it. That satisfies detached and hidden; the resolver would call
  // the ref stale. A ref of another document or one the page never handed
  // out still fails in the resolver.
  function removed(t) {
    var e = t.ref && _.refGet(t.ref);
    return !!e && (!t.doc || t.doc === _.doc()) && !B.connected(e);
  }
  function test(a, call) {
    if (a.k === "target") {
      if ((a.state === "detached" || a.state === "hidden") && removed(a.t)) { return [true, "the element was removed"]; }
      var e = _.all(a.t, call)[0], v = !!e && _.visible(e), st = a.state;
      return [st === "attached" ? !!e : st === "detached" ? !e : st === "hidden" ? !v : v,
        !e ? "no element matches" : v ? "the element is visible" : "the element is attached and not visible"];
    }
    if (a.k === "text") {
      var b = document.body;
      return [!!b && B.innerText(b).indexOf(a.text) >= 0, "the text " + JSON.stringify(a.text) + " is not on the page"];
    }
    if (a.k === "url") { return [new RegExp(a.re).test(location.href), "URL is still " + location.href]; }
    return [RS[document.readyState] >= (a.load === "load" ? 2 : 1), "document.readyState is still " + document.readyState];
  }
  I.def("wait", function (a, call) {
    return new Promise(function (res, rej) {
      var end = B.now() + a.slice, mo = null, t = 0, done = false;
      function fin(f, v) {
        if (done) { return; }
        done = true;
        clearInterval(t);
        if (mo) { mo.disconnect(); }
        f(v);
      }
      function chk() {
        try {
          var r = test(a, call);
          if (r[0] || B.now() >= end) { fin(res, { ok: r[0], state: r[1] }); }
        } catch (e) { fin(rej, e); }
      }
      mo = new MutationObserver(chk);
      mo.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
      t = setInterval(chk, 100);
      chk();
    });
  });
  I.def("pushstate", function (a) {
    var n = w.next;
    if (n && n.router && typeof n.router.push === "function") { n.router.push(a.url); return "next.router.push"; }
    history.pushState(null, "", a.url);
    w.dispatchEvent(new PopStateEvent("popstate", { state: null }));
    return "history.pushState";
  });
})(window.__iwdpBuilding);
