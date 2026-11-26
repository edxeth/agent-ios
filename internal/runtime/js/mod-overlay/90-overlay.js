// mod-overlay/90-overlay.js (W3b): the annotated screenshot overlay of
// design 3.9, the overlay module of the runtime (parity-045 decision D5:
// installed on demand before screenshot --annotate draws or removes it,
// internal/runtime ModOverlay).
// One host element with a closed shadow root, maximum z-index and no
// pointer events, drawn in document coordinates (it also works for a
// full-page capture); "overlayClear", release and an owner reset remove
// it. The result names the capture area in document coordinates: the
// viewport, the whole document, or with a target (a.t) that element's box
// (also for a target inside a same-origin frame), so that the capture
// uses the same area. A ref inside a same-origin frame (the snapshot walks
// those) is placed through each frame element's box, border, padding and
// scale, and only while it is in that frame's viewport; "boxes" gives each
// drawn ref's box in document coordinates (Go makes them relative to the
// area, the captured image).
(function (I) {
  "use strict";
  if (!I) { return; } // installed (internal/runtime module wrapper)
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
  // place: e's box in this window's viewport, or null when a frame's
  // viewport cuts it off.
  function place(e) {
    var r = B.gbcr(e), x = r.left, y = r.top, wd = r.width, ht = r.height, d = B.ownerDoc(e);
    while (d !== document) {
      var v = B.defaultView(d), f = _.frameOf(d);
      if (!f || x + wd <= 0 || y + ht <= 0 || x >= v.innerWidth || y >= v.innerHeight) { return null; }
      var p = _.lift(f, x, y);
      x = p.x; y = p.y; wd *= p.sx; ht *= p.sy; d = B.ownerDoc(f);
    }
    return { x: x, y: y, width: wd, height: ht };
  }
  I.def("overlay", function (a, call) {
    clear();
    var de = document.documentElement, out = [], boxes = [];
    var ax = a.full ? 0 : w.scrollX, ay = a.full ? 0 : w.scrollY;
    var aw = a.full ? de.scrollWidth : w.innerWidth, ah = a.full ? de.scrollHeight : w.innerHeight;
    if (a.t) {
      var te = _.all(a.t, call)[0];
      if (!te) { throw _.fail("not_found", "no element matches " + JSON.stringify(a.t.selector || "@" + a.t.ref)); }
      var tr = place(te);
      if (!tr) { throw _.fail("unsupported", "the element of " + JSON.stringify(a.t.selector || "@" + a.t.ref) + " is outside its frame's viewport; scroll it into view first"); }
      ax = tr.x + w.scrollX; ay = tr.y + w.scrollY; aw = tr.width; ah = tr.height;
    }
    host = document.createElement("div");
    css(host, "position:absolute;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none");
    var root = host.attachShadow({ mode: "closed" });
    a.refs.forEach(function (ref) {
      var e = _.refGet(ref);
      var r = e && B.connected(e) && _.visible(e) ? place(e) : null;
      if (!r) { return; }
      var x = r.x + w.scrollX, y = r.y + w.scrollY;
      if (x + r.width <= ax || y + r.height <= ay || x >= ax + aw || y >= ay + ah) { return; }
      var b = document.createElement("div"), l = document.createElement("span");
      css(b, "position:absolute;box-sizing:border-box;border:2px solid #e5197e;left:" + x + "px;top:" + y + "px;width:" + r.width + "px;height:" + r.height + "px");
      // The label sits above its box, or inside it at the top of the area.
      css(l, "position:absolute;left:-2px;top:" + (y - ay < 15 ? 0 : -15) + "px;background:#e5197e;color:#fff;font:bold 11px/13px monospace;padding:0 2px;white-space:nowrap");
      l.textContent = "[" + ref.slice(1) + "]";
      b.appendChild(l);
      root.appendChild(b);
      out.push(ref);
      boxes.push({ x: x, y: y, width: r.width, height: r.height });
    });
    de.appendChild(host);
    return { refs: out, boxes: boxes, area: { x: ax, y: ay, width: aw, height: ah } };
  });
  I.def("overlayClear", function () { return clear(); });
  I.def("overlayPresent", function () { return !!host && B.connected(host); });
})(window.__iwdpBuilding);
