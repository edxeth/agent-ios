// 90-overlay.js (W3b): the annotated screenshot overlay of design 3.9.
// One host element with a closed shadow root, maximum z-index and no
// pointer events, drawn in document coordinates (it also works for a
// full-page capture); "overlayClear", release and an owner reset remove
// it. With a target (a.t) the capture area is that element's box, and the
// result names it, so that the capture uses the same area.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, w = window, host = null;
  function clear() {
    var had = !!host;
    if (host && host.parentNode) { host.parentNode.removeChild(host); }
    host = null;
    return had;
  }
  I.onRelease(clear);
  I.onReset(clear);
  function css(e, s) { e.setAttribute("style", s); }
  I.def("overlay", function (a, call) {
    clear();
    var de = document.documentElement, out = [];
    var ax = a.full ? 0 : w.scrollX, ay = a.full ? 0 : w.scrollY;
    var aw = a.full ? de.scrollWidth : w.innerWidth, ah = a.full ? de.scrollHeight : w.innerHeight;
    if (a.t) {
      var te = _.all(a.t, call)[0];
      if (!te) { throw _.fail("not_found", "no element matches " + JSON.stringify(a.t.selector || "@" + a.t.ref)); }
      var tr = B.gbcr(te);
      ax = tr.left + w.scrollX; ay = tr.top + w.scrollY; aw = tr.width; ah = tr.height;
    }
    host = document.createElement("div");
    css(host, "position:absolute;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none");
    var root = host.attachShadow({ mode: "closed" });
    a.refs.forEach(function (ref) {
      var e = _.refGet(ref);
      if (!e || !B.connected(e) || !_.visible(e)) { return; }
      var r = B.gbcr(e), x = r.left + w.scrollX, y = r.top + w.scrollY;
      if (x + r.width <= ax || y + r.height <= ay || x >= ax + aw || y >= ay + ah) { return; }
      var b = document.createElement("div"), l = document.createElement("span");
      css(b, "position:absolute;box-sizing:border-box;border:2px solid #e5197e;left:" + x + "px;top:" + y + "px;width:" + r.width + "px;height:" + r.height + "px");
      // The label sits above its box, or inside it at the top of the area.
      css(l, "position:absolute;left:-2px;top:" + (y - ay < 15 ? 0 : -15) + "px;background:#e5197e;color:#fff;font:bold 11px/13px monospace;padding:0 2px;white-space:nowrap");
      l.textContent = "[" + ref.slice(1) + "]";
      b.appendChild(l);
      root.appendChild(b);
      out.push(ref);
    });
    de.appendChild(host);
    return { refs: out, area: { x: ax, y: ay, width: aw, height: ah } };
  });
  I.def("overlayClear", function () { return clear(); });
  I.def("overlayPresent", function () { return !!host && B.connected(host); });
})(window.__iwdpBuilding);
