// mod-gestures/10-gestures.js (parity-045 WP-SPLIT, decision D5): the
// gestures module of the runtime, installed on demand (internal/runtime
// ModGestures) before a swipe, drag or mouse command: rarely used
// dispatchers built on the event primitives of 55-actions.js, whose
// attempt machinery (50-actionability.js act) runs them.
(function (I) {
  "use strict";
  if (!I) { return; } // installed (internal/runtime module wrapper)
  // Built-ins captured at the core's install (design 3.1), not at this one.
  var _ = I._, w = window, A = _.A, center = _.center, at = _.at, ev = _.ev, gap = _.gap, over = _.over, fire = _.fire, pi = _.pi;
  var touch = _.touch, noTouch = _.noTouch, scroller = _.scroller, BTN = _.BTN, PE = _.PE, ME = _.ME, DE = _.DE;
  // swipe: touch events along the gesture, then the scroll fallback when
  // the page canceled none of them.
  A.swipe = async function (e, g, a, r, x) {
    var t = e || document.documentElement, c = e ? center(e) : { x: w.innerWidth / 2, y: w.innerHeight / 2 };
    var d = { x: c.x - g.dx, y: c.y - g.dy };
    var s = await touch(x, t, c, "touchstart"), m = await touch(x, t, d, "touchmove"), en = await touch(x, t, d, "touchend");
    if (s === null) { noTouch(r); }
    if (s !== false && m !== false && en !== false) {
      await ev(x, function () { scroller(e).scrollBy(g.dx, g.dy); });
      r.d.push("scroll");
    }
  };
  A.swipe.as = "scroll";
  // drag: step 1 presses on the source and sends dragstart; each later step
  // moves toward the target; the last one drops (only when the last
  // dragover was canceled, as in HTML drag and drop) and releases.
  var DR = {};
  function dinit(c, dt) {
    var o = pi(c, {}, 1);
    o.dataTransfer = dt;
    return o;
  }
  A.drag = async function (e, g, a, r, x) {
    var S = DR[a.opId];
    if (a.step === 1) {
      var dt = null;
      try { dt = new w.DataTransfer(); } catch (y) { /* no DataTransfer constructor */ }
      S = DR[a.opId] = { s: center(e), t: center(g.el2), src: e, dt: dt };
      r.v = { step: 1 };
      await over(x, e, S.s, g);
      await gap(x);
      await fire(x, e, PE, "pointerdown", pi(S.s, g, 1)); await fire(x, e, ME, "mousedown", pi(S.s, g, 1));
      S.ok = await fire(x, e, DE, "dragstart", dinit(S.s, S.dt));
      return;
    }
    if (!S) { r.err = "the page lost the drag state (it navigated or reset)"; return; }
    var k = (a.step - 1) / (a.steps - 1), c = { x: S.s.x + (S.t.x - S.s.x) * k, y: S.s.y + (S.t.y - S.s.y) * k };
    var h = at(c.x, c.y) || document.documentElement;
    r.v = { step: a.step };
    await fire(x, h, PE, "pointermove", pi(c, g, 1)); await fire(x, h, ME, "mousemove", pi(c, g, 1));
    if (S.ok) {
      if (h !== S.over) { await fire(x, h, DE, "dragenter", dinit(c, S.dt)); S.over = h; }
      S.can = !await fire(x, h, DE, "dragover", dinit(c, S.dt));
    }
    if (a.step === a.steps) {
      r.v.dropped = !!(S.ok && S.can);
      delete DR[a.opId];
      await gap(x);
      if (r.v.dropped) { await fire(x, h, DE, "drop", dinit(c, S.dt)); }
      if (S.ok) { await fire(x, S.src, DE, "dragend", dinit(c, S.dt)); }
      await fire(x, h, PE, "pointerup", pi(c, g, 0)); await fire(x, h, ME, "mouseup", pi(c, g, 0));
    }
  };
  A.drag.pre = true;
  // mouse: the pointer position and buttons belong to the page session.
  var M = { x: 0, y: 0, b: 0 };
  I.onReset(function () { M = { x: 0, y: 0, b: 0 }; });
  A.mouse = async function (e, g, a, r, x) {
    if (g.op === "move") {
      if (a.step === 1) { M.f = { x: M.x, y: M.y }; }
      var f = M.f || M, k = a.step / a.steps;
      M.x = f.x + (g.x - f.x) * k; M.y = f.y + (g.y - f.y) * k;
    }
    var c = { x: M.x, y: M.y }, h = at(c.x, c.y) || document.documentElement, b = g.b || 0;
    if (g.op === "down") { M.b |= BTN[b]; }
    if (g.op === "up") { M.b &= ~BTN[b]; }
    g.mod = _.held(); // the modifiers that keydown holds (60-keyboard.js)
    var o = pi(c, g, M.b), t = { move: "move", down: "down", up: "up" }[g.op];
    r.v = { x: M.x, y: M.y, buttons: M.b };
    if (t) {
      await fire(x, h, PE, "pointer" + t, o); await fire(x, h, ME, "mouse" + t, o);
    } else {
      o.deltaX = g.dx; o.deltaY = g.dy; o.deltaMode = 0;
      if (await fire(x, h, w.WheelEvent, "wheel", o)) {
        await ev(x, function () { w.scrollBy(g.dx, g.dy); });
        r.d.push("scroll");
      }
    }
  };
})(window.__iwdpBuilding);
