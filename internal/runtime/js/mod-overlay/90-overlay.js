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
// scale, and only while it is in that frame's viewport, cut to the part
// that every enclosing frame shows (a selector target that a frame cuts
// is refused); "boxes" gives each drawn ref's box in document coordinates
// (Go makes them relative to the area, the captured image).
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
  // place: the part of e's box that every enclosing frame's viewport shows,
  // in this window's viewport, with cut set when a frame cuts part of it
  // off and c the visible region of e's innermost frame (null for an
  // element of this document); null when a frame's viewport cuts it off.
  function place(e) {
    var r = B.gbcr(e), b = [r.left, r.top, r.right, r.bottom], c = null, cut = false, d = B.ownerDoc(e);
    while (d !== document) {
      var v = B.defaultView(d), f = _.frameOf(d), V = [0, 0, v.innerWidth, v.innerHeight];
      if (!f || b[2] <= 0 || b[3] <= 0 || b[0] >= V[2] || b[1] >= V[3]) { return null; }
      c = c ? meet(c, V) : V;
      cut = cut || b[0] < 0 || b[1] < 0 || b[2] > V[2] || b[3] > V[3];
      b = up(f, meet(b, V)); c = up(f, c); d = B.ownerDoc(f);
    }
    return { x: b[0], y: b[1], width: b[2] - b[0], height: b[3] - b[1], cut: cut, c: c };
  }
  function meet(a, b) {
    return [Math.max(a[0], b[0]), Math.max(a[1], b[1]), Math.max(Math.min(a[2], b[2]), a[0]), Math.max(Math.min(a[3], b[3]), a[1])];
  }
  // up: rectangle q of the document that frame f shows, in f's own viewport.
  function up(f, q) {
    var p = _.lift(f, q[0], q[1]);
    return [p.x, p.y, p.x + (q[2] - q[0]) * p.sx, p.y + (q[3] - q[1]) * p.sy];
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
      if (!tr || tr.cut) {
        throw _.fail("unsupported", "the element of " + JSON.stringify(a.t.selector || "@" + a.t.ref) + " is " + (tr ? "partly " : "") +
          "outside its frame's viewport, so a capture of its box would show other pixels; scroll it into view first");
      }
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
      // A ref inside a frame is drawn in a box that clips to the visible
      // region of its frame (k), so neither its box nor its label paints
      // over the page outside that frame.
      var k = r.c ? [r.c[0] + w.scrollX, r.c[1] + w.scrollY] : [0, 0], p = root;
      if (r.c) {
        p = document.createElement("div");
        css(p, "position:absolute;overflow:hidden;left:" + k[0] + "px;top:" + k[1] + "px;width:" + (r.c[2] - r.c[0]) + "px;height:" + (r.c[3] - r.c[1]) + "px");
        root.appendChild(p);
      }
      var b = document.createElement("div"), l = document.createElement("span");
      css(b, "position:absolute;box-sizing:border-box;border:2px solid #e5197e;left:" + (x - k[0]) + "px;top:" + (y - k[1]) + "px;width:" + r.width + "px;height:" + r.height + "px");
      // The label sits above its box, or inside it at the top of the area
      // or of its frame's visible region.
      css(l, "position:absolute;left:-2px;top:" + (y - (r.c ? Math.max(ay, k[1]) : ay) < 15 ? 0 : -15) + "px;background:#e5197e;color:#fff;font:bold 11px/13px monospace;padding:0 2px;white-space:nowrap");
      l.textContent = "[" + ref.slice(1) + "]";
      b.appendChild(l);
      p.appendChild(b);
      out.push(ref);
      boxes.push({ x: x, y: y, width: r.width, height: r.height });
    });
    de.appendChild(host);
    return { refs: out, boxes: boxes, area: { x: ax, y: ay, width: aw, height: ah } };
  });
  I.def("overlayClear", function () { return clear(); });
  I.def("overlayPresent", function () { return !!host && B.connected(host); });
})(window.__iwdpBuilding);
