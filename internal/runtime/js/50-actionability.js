// 50-actionability.js (W3b): actionability checks of design 3.5 and the
// "act" attempt function (one step under the fence of design 2.6). Notes:
// docs/notes/w3b.md.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, w = window, E = Element.prototype;
  var raf = w.requestAnimationFrame.bind(w), scrollIV = E.scrollIntoView, scrollIVN = E.scrollIntoViewIfNeeded;
  var TXT = { "": 1, text: 1, email: 1, password: 1, search: 1, tel: 1, url: 1, number: 1, date: 1, "datetime-local": 1, month: 1, time: 1, week: 1 };
  var ST = {}; // stability records by op and step
  I.onReset(function () { ST = {}; });
  function desc(e) {
    var s = "<" + e.localName, id = B.getAttr(e, "id"), c = B.getAttr(e, "class");
    return s + (id ? ' id="' + id + '"' : "") + (c ? ' class="' + c + '"' : "") + ">";
  }
  function editable(e) {
    var t = e.localName;
    if (_.disabled(e)) { return false; }
    if (t === "textarea") { return !e.readOnly; }
    if (t === "input") { return !e.readOnly && !!TXT[(B.getAttr(e, "type") || "").toLowerCase()]; }
    return !!e.isContentEditable;
  }
  // center is the point that actions aim at, in the client coordinates of
  // e's own window (a frame's for an element inside a frame): the
  // center of e's bounding box, or for an element in several boxes (an
  // inline element that wraps across lines) the center of its first box
  // with an area whose center is in the viewport, else of its first box
  // with an area. The bounding box of a wrapped element spans both lines,
  // and its center can lie on another element (Playwright's clickable
  // point likewise takes the first content quad).
  function center(e) {
    var rs = B.gcr(e), f = null, v = view(e);
    if (rs.length > 1) {
      for (var i = 0; i < rs.length; i++) {
        var q = rs[i], c = { x: q.left + q.width / 2, y: q.top + q.height / 2 };
        if (q.width * q.height < 1) { continue; }
        if (inView(c, v)) { return c; }
        f = f || c;
      }
      if (f) { return f; }
    }
    var r = B.gbcr(e);
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }
  // at: the element at client point (x, y) of document d (default: this
  // window's document), through open shadow roots.
  function at(x, y, d) {
    var h = B.fromPoint(d || document, x, y), s;
    while (h && B.shadowFromPoint && (s = B.shadowRoot(h))) {
      var d = B.shadowFromPoint(s, x, y);
      if (!d || d === h) { break; }
      h = d;
    }
    return h;
  }
  function within(h, e) {
    for (var p = h; p; p = _.flatParent(p)) { if (p === e) { return true; } }
    return false;
  }
  function view(e) { return B.defaultView(B.ownerDoc(e)) || w; }
  function inView(c, v) { return c.x >= 0 && c.y >= 0 && c.x < v.innerWidth && c.y < v.innerHeight; }
  // lift maps point (x, y) of the viewport of the document that frame
  // element f shows into the viewport of f's own window, through f's box,
  // border, padding and CSS scale (sx, sy).
  function lift(f, x, y) {
    var r = B.gbcr(f), sx = f.offsetWidth ? r.width / f.offsetWidth : 1, sy = f.offsetHeight ? r.height / f.offsetHeight : 1;
    return { x: r.left + (f.clientLeft + (parseFloat(_.css(f, "padding-left")) || 0) + x) * sx,
      y: r.top + (f.clientTop + (parseFloat(_.css(f, "padding-top")) || 0) + y) * sy, sx: sx, sy: sy };
  }
  function frameOf(d) {
    var v = B.defaultView(d);
    try { return v && v.frameElement; } catch (err) { return null; } // cross-origin parent
  }
  // reaches: the hit h takes e's press: h is e or inside it, or h is in a
  // label that activates e (its control, or a label around e) and neither h
  // nor an element between h and that label is interactive (agent-browser
  // 0.45.0 BLOCKER_AT_JS, element.rs: its selector or an onclick function).
  var IA = 'button,a[href],area[href],input,select,textarea,summary,details,iframe,embed,object,[role],[onclick],' +
    '[tabindex]:not([tabindex="-1"]),[contenteditable=""],[contenteditable="true"]';
  // An editing host (any contenteditable state that edits, plaintext-only
  // too) is interactive as well; its editable descendants are not hosts,
  // and a label inside an editable region still activates its control.
  function host(n) {
    var p = _.flatParent(n);
    return !!n.isContentEditable && !(p && p.isContentEditable);
  }
  function reaches(h, e) {
    if (within(h, e)) { return true; }
    var l = B.closest(h, "label");
    if (!l) { return false; }
    for (var n = h; n && n !== l; n = _.flatParent(n)) {
      if (B.matches(n, IA) || typeof n.onclick === "function" || host(n)) { return false; }
    }
    return l.control === e || within(e, l);
  }
  // framed: "" when point c of document d shows through every frame that
  // encloses d: each frame element is visible, c is inside its viewport,
  // and the element at c's place in the parent document is that frame.
  function framed(d, c) {
    for (var x = c.x, y = c.y, f; d !== document; d = B.ownerDoc(f)) {
      var v = B.defaultView(d);
      f = frameOf(d);
      if (!f || !_.visible(f)) { return "the element's frame is not visible"; }
      if (!inView({ x: x, y: y }, v)) { return "the element center is outside its frame's viewport"; }
      var p = lift(f, x, y), h = at(x = p.x, y = p.y, B.ownerDoc(f));
      if (h !== f) { return h ? "covered by " + desc(h) : "the element center is outside the viewport"; }
    }
    return "";
  }
  // hits: the receives-events check. It first scrolls e into view (out of a
  // clipping scroll container too, and fully when it straddles the
  // viewport edge); when that moved e, it answers retry, so the next
  // attempt checks again after scroll listeners, frames and reflow ran
  // (stable waits for the new rect) and scrolls again when reflow moved e
  // back out (agent-browser #2587: press the settled point).
  // An element inside a frame is hit-tested in its own document, then
  // through every enclosing frame (framed).
  function hits(e) {
    var k = rk(e), d = B.ownerDoc(e), v = view(e);
    if (scrollIVN) { scrollIVN.call(e, true); }
    if (!inView(center(e), v)) { scrollIV.call(e, { block: "center", inline: "center" }); }
    if (rk(e) !== k) { return "waiting for the element to settle after scrolling"; }
    var c = center(e), h = inView(c, v) && at(c.x, c.y, d);
    if (!h) { return "the element center (" + Math.round(c.x) + ", " + Math.round(c.y) + ") is outside the viewport"; }
    return reaches(h, e) ? framed(d, c) : "covered by " + desc(h);
  }
  function rk(e) {
    var r = B.gbcr(e);
    return r.left + "," + r.top + "," + r.width + "," + r.height;
  }
  // stable: same rect in two consecutive animation frames, recorded by
  // frame callbacks between attempts (the checks of an attempt run in one
  // task, before its events).
  function stable(e, key) {
    var s = ST[key], k = rk(e);
    if (s && s.el === e && s.n >= 2 && s.a === k && s.b === k) { return ""; }
    if (!s || s.el !== e || !s.busy) {
      s = ST[key] = { el: e, n: 0, busy: 1 };
      var f = function () {
        s.a = s.b; s.b = rk(e); s.n++;
        if (s.n < 2) { raf(f); } else { s.busy = 0; }
      };
      raf(f);
    }
    return "waiting for the element to be stable";
  }
  var HIT = { click: 1, dblclick: 1, tap: 1, hover: 1, check: 1, drag: 1 };
  var EN = { click: 1, dblclick: 1, tap: 1, check: 1, select: 1, fill: 1, type: 1, upload: 1 };
  // check returns "" when e can take action k, else the reason to retry.
  function check(k, e, key) {
    if (!B.connected(e)) { return "the element is not attached"; }
    if (!_.visible(e)) { return "the element is not visible"; }
    if (EN[k] && _.disabled(e)) { return "the element is not enabled"; }
    if ((k === "fill" || k === "type") && !editable(e)) { return "the element is not editable"; }
    if (HIT[k]) { return stable(e, key) || hits(e); }
    return "";
  }
  _.desc = desc; _.editable = editable; _.center = center; _.at = at; _.check = check; _.lift = lift; _.frameOf = frameOf;
  // Event tasks (design 3.5, W3b decision "one task per event"). The events
  // of one native input event (a mouse move, a mouse down, a key down) run
  // in one task, each after a microtask turn, so microtasks that the page
  // queued in one event run before the next, as between listeners of real
  // input. Two native input events are separated by gap: page tasks of
  // delay 0 that the earlier events scheduled run first, and the later
  // events run in a new task (with frame, after the next animation frame
  // too, at most FRAME_MS later). Every event checks that its attempt is
  // still live (attempt end, deadline, cancel, epoch) before it runs.
  var P = w.Promise, sT = w.setTimeout, MC = w.MessageChannel, FRAME_MS = 50;
  var STOP = {};
  // hop resolves in a new task that is not a timer callback, so the timers
  // set by the events that follow have nesting level 0 (no 4 ms clamp).
  function hop() {
    return new P(function (res) {
      if (!MC) { sT.call(w, res, 0); return; }
      var c = new MC();
      c.port1.onmessage = function () { res(); };
      c.port2.postMessage(0);
    });
  }
  function timer(ms) { return new P(function (res) { sT.call(w, res, ms); }); }
  function frame() {
    return new P(function (res) { raf(res); sT.call(w, res, FRAME_MS); });
  }
  // gap ends the current native input event; before the first event of an
  // attempt it does nothing, so the first event runs in the task of the
  // checks.
  async function gap(x, f) {
    if (!x.n) { return; }
    if (f) { await frame(); }
    await timer(0);
    await hop();
  }
  // ev runs fn, one event or default action of attempt x, after a
  // microtask turn; it stops the step when the attempt is no longer live.
  async function ev(x, fn) {
    await null;
    return evs(x, fn);
  }
  // evs is ev without the microtask turn: an event whose reply the step
  // computes in the task of the call (the native select of mod-forms/).
  function evs(x, fn) {
    if (!x.live()) { x.interrupt("attempt end"); throw STOP; }
    x.n = (x.n || 0) + 1;
    return fn();
  }
  _.ev = ev; _.evs = evs; _.gap = gap; _.sT = sT;
  _.A = {}; // dispatchers by action kind: 55-actions.js, 60-keyboard.js and the modules mod-*/
  function failed(e) {
    if (!e || !e.__iwdpError) { throw e; }
    return { status: "failed", reason: e.code + ": " + e.message, now: B.now(), doc: _.doc() };
  }
  // act runs one attempt of an action step. args: k (kind), t (target spec),
  // f (force: skip checks), dp (dialog policy) and kind-specific fields.
  // A target that is ambiguous, stale or unknown fails at once; a missing
  // one is retried.
  // act answers a promise only when its step waits (a later event, task or
  // frame); a check that fails, a refusal and a step that runs whole in the
  // task of the call answer at once, so the reply comes with the call's
  // own Runtime.evaluate, before a navigation that the step started can
  // destroy the document (internal/runtime asyncFns).
  // With probe, act runs the fence and the checks and dispatches nothing:
  // it answers "retry" with reason READY when it would dispatch now.
  var READY = "ready to dispatch";
  function act(a, call, probe) {
    var g = a.args || {}, el = null, why = "";
    try {
      if (g.t) { el = _.resolve(g.t, call); }
      if (g.t2) { g.el2 = _.resolve(g.t2, call); }
    } catch (e) {
      if (e && e.code === "not_found") { why = e.message; } else { return failed(e); }
    }
    var d = _.A[g.k];
    if (!d) { return failed(_.fail("unsupported", "the agent runtime has no action " + g.k)); }
    // chk: the form preconditions (mod-forms/), inside the fence: a refusal
    // answers only for the next step of a live operation, and changes nothing.
    var no = null;
    try {
      return I.attempt(a, function () {
        if (why) { return why; }
        // vet: a refusal of the dispatcher that needs no target (60-keyboard.js).
        try {
          if (d.vet) { d.vet(el, g); }
          if (el && d.chk) { el = d.chk(el, g, a) || el; }
        } catch (e) { no = e; throw e; }
        var r = el && !g.f && !(d.pre && a.step > 1) ? check(d.as || g.k, d.on ? d.on(el) : el, a.opId + "/" + a.step) : "";
        return r || (probe ? READY : "");
      }, dispatch);
    } catch (e) {
      if (e === no) { return failed(e); }
      throw e;
    }
    function dispatch(x) {
      _.dlgOn(g.dp);
      var r = { trusted: false, d: [] }, p;
      function fin() { r.dlg = _.dlgUsed(); return r; }
      function stop(e) {
        if (e !== STOP) { throw e; }
        return fin();
      }
      try {
        p = d(el, g, a, r, x);
      } catch (e) {
        return stop(e);
      }
      return p && typeof p.then === "function" ? p.then(fin, stop) : fin();
    }
  }
  // click --new-tab (actGesture, with emulateUserGesture): the runtime
  // first calls actProbe without the gesture and calls actGesture only when
  // the probe answers READY, so an attempt that cannot dispatch gives the
  // page no user activation (internal/runtime gestureFns).
  I.def("act", function (a, call) { return act(a, call, false); });
  I.def("actGesture", function (a, call) { return act(a, call, false); });
  I.def("actProbe", function (a, call) { return act(a, call, true); });
})(window.__iwdpBuilding);
