// 65-cdp.js (W5b-2): what the CDP endpoint (internal/cdp) needs of the
// runtime: node handles (backendNodeId, design 5.2), the accessibility
// tree from the snapshot walk in full mode and the AX query through the
// locator (5.3 Accessibility), and the CDP mouse and touch dispatchers,
// built from the W3b event primitives of 55-actions.js (3.5). Notes:
// docs/notes/w5.md, "W5b-2".
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, A = _.A, w = window, WR = w.WeakRef, fire = _.fire, pi = _.pi, ne = _.ne, gap = _.gap;
  var PE = w.PointerEvent || w.MouseEvent, ME = w.MouseEvent;
  // Handles: one number per node for the life of this document. An owner
  // change does not reset them, so a number never names another node; Go
  // maps (document, handle) to a backendNodeId (internal/cdp/handles.go).
  var HW = new WeakMap(), HN = new Map(), hn = 0;
  function handle(n) {
    var h = B.wmGet(HW, n);
    if (!h) { h = ++hn; B.wmSet(HW, n, h); B.mapSet(HN, h, WR ? new WR(n) : n); }
    return h;
  }
  function byHandle(doc, h) {
    var r = doc === _.doc() ? B.mapGet(HN, h) : null, n = r && (WR ? r.deref() : r);
    if (!n) { throw _.fail("no_node", "No node with given id found"); }
    return n;
  }
  _.handle = handle; _.byHandle = byHandle;
  // A target spec {doc, h} names a node by its handle (DOM.setFileInputFiles
  // runs W3b's upload on it).
  var resolve = _.resolve;
  _.resolve = function (s, call) { return s && s.h ? byHandle(s.doc, s.h) : resolve(s, call); };
  // cdpAX: the snapshot walk in full mode, with focusable (fo) and
  // focused (fd) for elements; Go builds the AX nodes.
  I.def("cdpAX", function () {
    var tree = _.walk(null, 1, true).tree, f = _.deepActive();
    (function mark(l) {
      for (var i = 0; i < l.length; i++) {
        var n = l[i], e = byHandle(_.doc(), n.h);
        if (e.nodeType === 1) { n.fo = e.tabIndex >= 0 && !_.disabled(e); n.fd = e === f; }
        if (n.children) { mark(n.children); }
      }
    })(tree);
    return { doc: _.doc(), h: handle(document), title: String(document.title), tree: tree };
  });
  // cdpQueryAX: the rendered elements at or below node h in the flat tree
  // whose role and accessible name (exact) match. The elements are the
  // locator's (45-locate.js els: open shadow roots, same-origin iframes);
  // an element is below h when its flat-tree ancestors (flatParent, as the
  // snapshot walks) reach h or a child of h, so a document or a shadow
  // root scopes its own tree and an iframe's document stays separate.
  I.def("cdpQueryAX", function (a) {
    var root = byHandle(a.doc, a.h), out = [];
    _.els(true).forEach(function (e) {
      var r = _.role(e), p = e, nm, n;
      if (!r || r === "presentation" || (a.role && r !== a.role)) { return; }
      while (p && p !== root && p.parentNode !== root) { p = _.flatParent(p); }
      if (!p || !B.gcr(e).length) { return; }
      nm = _.name(e, r);
      if (a.name !== undefined && _.norm(nm) !== _.norm(a.name)) { return; }
      n = _.states(e, r);
      n.h = handle(e); n.role = r; n.name = nm;
      out.push(n);
    });
    return { doc: _.doc(), nodes: out };
  });
  // cdpMouse: one CDP Input.dispatchMouseEvent at client point (x, y). Go
  // keeps the CDP session's buttons (bs) and says whether a release ends a
  // press of that session (cl); the page keeps the hovered node and the
  // press targets per CDP session (a, the attachment) and button. A
  // release consumes only its own session's press; one that ends a press
  // clicks the common ancestor of the press and release targets (auxclick
  // for the middle button, contextmenu for the right one, as press does),
  // and adds dblclick for click count 2. The attachment's release at
  // detach drops its press targets and touch (cdpDrop, internal/cdp
  // input_domain.go); cdpDrop reports whether it found the session's state.
  var C = { h: null, d: {} }, TS = {};
  _.cdpDrop = function (a) { var had = !!(C.d[a] || TS[a]); delete C.d[a]; delete TS[a]; return had; };
  // seq fires the events of names (space separated) on t: pointer events
  // as PointerEvent, the others as MouseEvent; enter and leave do not
  // bubble. It resolves to false when the page canceled one of them.
  async function seq(x, t, o, names) {
    var l = names.split(" "), ok = true;
    for (var i = 0; i < l.length; i++) {
      ok = await fire(x, t, l[i].charAt(0) === "p" ? PE : ME, l[i], /enter|leave/.test(l[i]) ? ne(o) : o) && ok;
    }
    return ok;
  }
  function mods(o, m) {
    o.altKey = !!(m & 1); o.ctrlKey = !!(m & 2); o.metaKey = !!(m & 4); o.shiftKey = !!(m & 8);
    return o;
  }
  // fx: how frame element f maps a client point (px, py) of its frame to
  // one of f's document: (px * sx + x, py * sy + y). Its border box's bounding
  // rect and the scales of f's and its ancestors' transforms give it
  // exactly for scales and translations; a rotation, skew, flip,
  // perspective or 3D transform is refused, as no exact mapping exists.
  function fx(f) {
    var v = f.ownerDocument.defaultView, sx = 1, sy = 1, e, m, r = B.gbcr(f), s = v.getComputedStyle(f), n = function (k) { return parseFloat(s[k]) || 0; };
    for (e = f; e; e = e.parentElement || (e.parentNode && e.parentNode.host)) {
      m = /^matrix(3d)?\((.*)\)$/.exec(v.getComputedStyle(e).transform);
      if (!m) { continue; }
      m = m[2].split(",").map(Number);
      if (m.length === 16) { // a 3D matrix maps like a 2D one only without z mixing and perspective
        m = [2, 3, 6, 7, 8, 9, 11, 14].some(function (i) { return m[i]; }) || m[10] !== 1 || m[15] !== 1 ? null : [m[0], m[1], m[4], m[5]];
      }
      if (!m || m[1] || m[2] || !(m[0] > 0) || !(m[3] > 0)) {
        throw new Error("the frame or one of its ancestors has a transform other than scale and translate; points in the frame cannot be mapped to the main frame");
      }
      sx *= m[0]; sy *= m[3];
    }
    return { sx: sx, sy: sy, x: r.left + (n("borderLeftWidth") + n("paddingLeft")) * sx, y: r.top + (n("borderTopWidth") + n("paddingTop")) * sy };
  }
  // fto: the mapping of node n's frame's client points to the main
  // frame's (fx through every frame element up to the main window).
  _.fto = function (n) {
    var t = { sx: 1, sy: 1, x: 0, y: 0 }, v = n.ownerDocument.defaultView, m, f;
    for (; v && v !== w && (f = B.frameEl(v)); v = B.parent(v)) {
      m = fx(f);
      t = { sx: t.sx * m.sx, sy: t.sy * m.sy, x: t.x * m.sx + m.x, y: t.y * m.sy + m.y };
    }
    return t;
  };
  // deep: the element at client point (x, y), also inside same-origin
  // frames (a CDP client gives main-frame points, as Chrome takes them); a
  // cross-origin iframe is the element itself, and so is a frameset's
  // <frame> (B.contentDocument reads iframes only; docs/cdp.md).
  function deep(x, y) {
    var h = _.at(x, y), d, m;
    while (h && (d = B.contentDocument(h))) {
      m = fx(h); x = (x - m.x) / m.sx; y = (y - m.y) / m.sy;
      h = _.at(x, y, d) || d.documentElement;
    }
    return h;
  }
  _.deep = deep;
  function common(a, b) {
    while (a && !a.contains(b)) { a = a.parentNode; }
    return a;
  }
  A.cdpMouse = async function (e, g, a, r, x) {
    var c = { x: g.x, y: g.y }, h = deep(c.x, c.y) || document.documentElement, b = g.b || 0, k = g.op, o = mods(pi(c, g, g.bs), g.m);
    await gap(x);
    if (k === "wheel") {
      o.deltaX = g.dx; o.deltaY = g.dy; o.deltaMode = 0;
      if (await fire(x, h, w.WheelEvent, "wheel", o)) {
        await _.ev(x, function () { w.scrollBy(g.dx, g.dy); });
        r.d.push("scroll");
      }
      return;
    }
    if (h !== C.h) {
      var p = C.h;
      C.h = h;
      if (p && B.connected(p)) { await seq(x, p, o, "pointerout pointerleave mouseout mouseleave"); }
      await seq(x, h, o, "pointerover pointerenter mouseover mouseenter");
    }
    if (k === "move") {
      await seq(x, h, o, "pointermove mousemove");
    } else if (k === "down") {
      (C.d[g.a] = C.d[g.a] || [])[b] = h;
      if (await seq(x, h, o, "pointerdown") && await seq(x, h, o, "mousedown")) { await _.ev(x, function () { _.focusDown(h); }); }
    } else {
      await seq(x, h, o, "pointerup mouseup");
      var D = C.d[g.a] || [], t = g.cl && D[b] && common(D[b], h);
      D[b] = null;
      if (t) { await seq(x, t, o, (b === 2 ? "contextmenu" : b === 1 ? "auxclick" : "click") + (b === 0 && g.n === 2 ? " dblclick" : "")); }
    }
  };
  // cdpTouch: one CDP touch event (op start, move, end or cancel; p the
  // touch points) of CDP session a on the node under the first point of
  // that session's touchstart; each session has its own touch (TS). A
  // touchend after a touchstart that moved at most SLOP pixels, with
  // neither canceled, runs W3b's tap click sequence (over and press with
  // pointerType touch).
  var SLOP = 10;
  A.cdpTouch = async function (e, g, a, r, x) {
    var k = g.op, ps = g.p || [], ts = [];
    if (k === "start") { TS[g.a] = { t: deep(ps[0].x, ps[0].y) || document.documentElement, s: ps[0], l: [], ok: true }; }
    var T = TS[g.a];
    if (!T) { r.err = "no touch is active"; return; }
    try {
      ps.forEach(function (p, i) {
        ts.push(new w.Touch({ identifier: p.id === undefined ? i : p.id, target: T.t, clientX: p.x, clientY: p.y, pageX: p.x + w.scrollX, pageY: p.y + w.scrollY }));
      });
    } catch (y) { T.nt = true; }
    if (k === "move" && Math.abs(ps[0].x - T.s.x) + Math.abs(ps[0].y - T.s.y) > SLOP) { T.mv = true; }
    var end = k === "end" || k === "cancel", cur = end ? [] : ts;
    if (T.nt) {
      r.warn = "no Touch constructor; touch events were not sent";
    } else {
      await gap(x);
      if (!await fire(x, T.t, w.TouchEvent, "touch" + k, { bubbles: true, cancelable: k !== "cancel", composed: true, view: w, touches: cur, targetTouches: cur, changedTouches: end ? T.l : ts }) && k !== "move") { T.ok = false; }
      T.l = ts;
    }
    if (end) {
      delete TS[g.a];
      if (k === "end" && T.ok && !T.mv) {
        g.pt = "touch"; g.b = 0; g.n = 1;
        await _.over(x, T.t, T.s, g);
        await _.press(x, T.t, T.s, g);
        r.d.push("tap");
      }
    }
  };
})(window.__iwdpBuilding);
