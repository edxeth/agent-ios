// 55-actions.js (W3b): pointer, touch and scroll actions (design 3.5; the
// form actions are in the forms module, mod-forms/).
// Every event is synthetic (isTrusted false). A dispatcher is an async
// function of the element, the step arguments g, the attempt a, the result
// r (r.d names emulated default actions, r.err a failure after dispatch,
// r.v a value) and the attempt context x. Events run through _.ev and
// _.gap (50-actionability.js): one task per native input event.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, w = window, A = _.A, center = _.center, at = _.at, ev = _.ev, gap = _.gap;
  var disp = Function.prototype.call.bind(EventTarget.prototype.dispatchEvent);
  var PE = w.PointerEvent || w.MouseEvent, ME = w.MouseEvent, DE = w.DragEvent || w.MouseEvent, Ev = w.Event;
  // BTN: the buttons mask of MouseEvent.button left, middle, right, back, forward.
  var BTN = [1, 4, 2, 8, 16];
  var FOC = "a[href],area[href],button,input,select,textarea,iframe,summary,[tabindex],[contenteditable]";
  // fire dispatches new C(type, o) on t as the next event of attempt x;
  // it resolves to false when the page canceled the event.
  function fire(x, t, C, type, o) { return ev(x, function () { return disp(t, new C(type, o)); }); }
  function bub(type) { return new Ev(type, { bubbles: true }); }
  // pi: the init of a pointer or mouse event at c; g.mod holds the
  // modifier bits (Alt 1, Control 2, Meta 4, Shift 8).
  function pi(c, g, buttons) {
    var m = g.mod || 0;
    return { bubbles: true, cancelable: true, composed: true, view: w, clientX: c.x, clientY: c.y, screenX: c.x, screenY: c.y,
      button: g.b || 0, buttons: buttons, detail: g.n || 0, pointerId: 1, pointerType: g.pt || "mouse", isPrimary: true,
      altKey: !!(m & 1), ctrlKey: !!(m & 2), metaKey: !!(m & 4), shiftKey: !!(m & 8) };
  }
  function ne(o) {
    var n = {};
    for (var k in o) { n[k] = o[k]; }
    n.bubbles = n.cancelable = false;
    return n;
  }
  function deepActive() {
    var a = document.activeElement;
    while (a && B.shadowRoot(a) && B.shadowRoot(a).activeElement) { a = B.shadowRoot(a).activeElement; }
    return a;
  }
  // over: the events of one native mouse move onto e.
  async function over(x, e, c, g) {
    var o = pi(c, g, 0);
    await gap(x);
    await fire(x, e, PE, "pointerover", o); await fire(x, e, PE, "pointerenter", ne(o));
    await fire(x, e, ME, "mouseover", o); await fire(x, e, ME, "mouseenter", ne(o));
    await fire(x, e, PE, "pointermove", o); await fire(x, e, ME, "mousemove", o);
  }
  // focusDown: the default action of a mousedown on e. It focuses the
  // focusable element at or above e, or blurs the focused element when
  // there is none.
  function focusDown(e) {
    var f = B.closest(e, FOC);
    if (f && !_.disabled(f)) {
      if (deepActive() !== f) { f.focus(); }
    } else if (document.activeElement && document.activeElement !== document.body) { document.activeElement.blur(); }
  }
  // press: one native mouse down (pointerdown, mousedown, and focus, the
  // default action of mousedown), then, after the next frame, one native
  // mouse up (pointerup, mouseup, then click for the left button,
  // contextmenu for the right one, auxclick for the others). A canceled pointerdown
  // suppresses the mouse compatibility events, not the click.
  async function press(x, e, c, g) {
    var b = g.b || 0, o = pi(c, g, BTN[b]);
    await gap(x);
    var pd = await fire(x, e, PE, "pointerdown", o), md = pd && await fire(x, e, ME, "mousedown", o);
    if (md) { await ev(x, function () { focusDown(e); }); }
    await gap(x, true);
    o = pi(c, g, 0);
    await fire(x, e, PE, "pointerup", o);
    if (pd) { await fire(x, e, ME, "mouseup", o); }
    await fire(x, e, ME, b === 2 ? "contextmenu" : b ? "auxclick" : "click", o);
  }
  // click: one move, then g.cnt presses (detail 1..n); the second press of
  // the left button ends with dblclick, as Chrome's clickCount 2 does.
  A.click = async function (e, g, a, r, x) {
    var c = center(e);
    await over(x, e, c, g);
    for (var i = 1; i <= (g.cnt || 1); i++) {
      g.n = i;
      await press(x, e, c, g);
      if (i === 2 && !g.b) { await fire(x, e, ME, "dblclick", pi(c, g, 0)); }
    }
  };
  A.dblclick = function (e, g, a, r, x) { g.cnt = 2; return A.click(e, g, a, r, x); };
  // touch dispatches one touch event as its own native input event; null
  // when the page has no Touch constructor.
  async function touch(x, e, c, type) {
    var t;
    try {
      t = new w.Touch({ identifier: 1, target: e, clientX: c.x, clientY: c.y, pageX: c.x + w.scrollX, pageY: c.y + w.scrollY });
    } catch (y) { return null; }
    var l = type === "touchend" ? [] : [t];
    await gap(x);
    return fire(x, e, w.TouchEvent, type, { bubbles: true, cancelable: true, composed: true, view: w, touches: l, targetTouches: l, changedTouches: [t] });
  }
  function noTouch(r) { r.warn = "this page has no Touch constructor; touch events were not sent"; }
  // tapOn: touchstart and touchend, then the click sequence with
  // pointerType touch, all at client point c on e.
  async function tapOn(x, e, c, g, r) {
    var s = await touch(x, e, c, "touchstart"), en = await touch(x, e, c, "touchend");
    if (s === null) { noTouch(r); }
    if (s === false || en === false) { r.v = { canceled: "touch" }; return; }
    g.pt = "touch";
    await over(x, e, c, g);
    await press(x, e, c, g);
  }
  A.tap = function (e, g, a, r, x) { return tapOn(x, e, center(e), g, r); };
  // tapAt: tap <x> <y> on the element at a viewport point (design 4.2);
  // every event carries that point.
  A.tapAt = function (e, g, a, r, x) {
    var c = { x: g.x, y: g.y }, h = at(c.x, c.y);
    if (!h) { r.err = "no element at (" + g.x + ", " + g.y + "); the point is outside the viewport"; return; }
    r.v = { at: _.desc(h) };
    return tapOn(x, h, c, g, r);
  };
  // newtab: click --new-tab (design 3.5) runs click's checks, then
  // window.open(href). The attempt function is actGesture, whose
  // Runtime.evaluate carries emulateUserGesture; window.open runs
  // synchronously inside that evaluation, so the page sees the gesture.
  A.newtab = function (e, g, a, r, x) {
    var h = e.href;
    if (typeof h !== "string" || !h) { r.err = "the element has no href; --new-tab works for links"; return; }
    if (!x.live()) { x.interrupt("attempt end"); return; }
    w.open(h);
    r.v = { href: h };
  };
  A.newtab.as = "click";
  A.hover = function (e, g, a, r, x) { return over(x, e, center(e), g); };
  A.focus = function (e, g, a, r, x) { return ev(x, function () { e.focus(); }); };
  A.blur = function (e, g, a, r, x) {
    return ev(x, function () {
      var f = deepActive();
      if (f && f.blur) { f.blur(); }
    });
  };
  function scroller(e) { return e || w; }
  A.scroll = async function (e, g, a, r, x) {
    await ev(x, function () { scroller(e).scrollBy(g.dx, g.dy); });
    r.v = e ? { x: e.scrollLeft, y: e.scrollTop } : { x: w.scrollX, y: w.scrollY };
  };
  A.scrollintoview = function (e, g, a, r, x) { return ev(x, function () { e.scrollIntoView({ block: "center", inline: "center" }); }); };
  _.disp = disp; _.bub = bub; _.deepActive = deepActive; _.Ev = Ev;
  // For the gestures module (mod-gestures/): swipe, drag and mouse.
  _.touch = touch; _.noTouch = noTouch; _.scroller = scroller; _.BTN = BTN; _.PE = PE; _.ME = ME; _.DE = DE;
  // The event primitives that the CDP Input dispatchers (65-cdp.js) build on.
  _.pi = pi; _.ne = ne; _.fire = fire; _.over = over; _.press = press; _.focusDown = focusDown;
  // pick returns the element of a target (highlight and hover --css get its node through it).
  I.def("pick", function (a, call) { return _.resolve(a.t, call); });
})(window.__iwdpBuilding);
