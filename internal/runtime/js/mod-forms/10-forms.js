// mod-forms/10-forms.js (parity-045 WP-SPLIT, decision D5): the forms
// module of the runtime, installed on demand (internal/runtime Module
// ModForms) before a fill, type, check, uncheck, select or upload and
// before the CDP bridge's DOM.setFileInputFiles. It holds the form
// preconditions and read-backs of agent-browser 0.45 (WP-FORMS), the
// value-input fill, the native and ARIA select and upload; the core keeps
// the attempt machinery (50-actionability.js act) that runs them. It
// builds on the event primitives of 55-actions.js and 60-keyboard.js.
(function (I) {
  "use strict";
  if (!I) { return; } // installed (internal/runtime module wrapper)
  var _ = I._, B = _.B, w = window, A = _.A, center = _.center, ev = _.ev, gap = _.gap, disp = _.disp, bub = _.bub;
  // Built-ins captured at the core's install (design 3.1), not at this one.
  var over = _.over, press = _.press, ie = _.ie, VS = _.VS, Ev = _.Ev, sT = _.sT, hclick = _.hclick;
  // Form preconditions of agent-browser 0.45 (cli/src/native/forms.rs). A
  // refusal changes nothing; its code is the error code and its message the
  // JSON of fixed facts (fd) that internal/commands/forms045.go turns into
  // text. Tables come from Go in the step args (g.cb, g.fk).
  var own = Object.prototype.hasOwnProperty;
  function fd(e, o) { o = o || {}; o.t = e.localName; o.y = String(e.type); return JSON.stringify(o); }
  function ff(c, e, o) { return _.fail(c, fd(e, o)); }
  // isEd: an editing host other than a text control (select, option,
  // optgroup and button keep their type unless they are hosts themselves).
  function isEd(n) {
    var t = n.localName;
    return !/^(input|textarea)$/.test(t) && !(/^(select|option|optgroup|button)$/.test(t) && !/^(true|plaintext-only)$/.test(n.contentEditable)) && !!n.isContentEditable;
  }
  // ctl: the control a target stands for: a label's control, the input of
  // a part of a date or time input.
  function ctl(e) {
    var h = e.getRootNode && e.getRootNode().host;
    return e.localName === "label" && e.control && !isEd(e) ? e.control : h && h.localName === "input" ? h : e;
  }
  function nrm(s) { return String(s == null ? "" : s).replace(/[\u200B-\u200D\u2060\uFEFF]/g, "").replace(/\s+/g, " ").trim(); }
  function has(l, x) { return !!x && (" " + l + " ").indexOf(" " + x + " ") >= 0; }
  function roles(n, l) { return (B.getAttr(n, "role") || "").split(/\s+/).filter(function (x) { return has(l, x); }); }
  // boxOf: the element check toggles (c = g.cb: box roles r, other roles n
  // and tags t, selectors i of interactive and o of other elements).
  function nat(n) { return n.localName === "input" && (n.type === "checkbox" || n.type === "radio"); }
  function qa(r, s) { return r.nodeType === 11 ? B.qsaFrag(r, s) : B.qsaEl(r, s); }
  function only(r, c) {
    var ins = qa(r, "input[type=checkbox],input[type=radio]"), all = qa(r, "*");
    for (var i = 0; i < all.length; i++) { if (all[i] !== ins[0] && typeof all[i].onclick === "function") { return null; } }
    return ins.length === 1 && !qa(r, c.o).length ? ins[0] : null;
  }
  function boxOf(n, c) {
    if (nat(n) || roles(n, c.r).length) { return n; }
    if (has(c.t, n.localName) || roles(n, c.n).length) { return null; }
    var l = n.localName === "label" ? n : B.closest(n, "label");
    if (l && l.control) {
      for (var p = n; p && p !== l; p = p.parentElement) { if (B.matches(p, c.i) || typeof p.onclick === "function") { return null; } }
      return nat(l.control) ? l.control : null;
    }
    return only(n, c) || (B.shadowRoot(n) && only(B.shadowRoot(n), c)) || null;
  }
  function cstate(b) {
    var a = B.getAttr, ro = roles(b, "option treeitem").length;
    return nat(b) ? b.checked : ro && !B.hasAttr(b, "aria-checked") ? a(b, "aria-selected") === "true" : a(b, "aria-checked") === "true";
  }
  // check and uncheck press the box itself; a native box without a visible
  // box gets a DOM click; a state that already matches is left alone.
  async function pressOn(x, e, g) {
    var c = center(e);
    await over(x, e, c, g);
    await press(x, e, c, g);
  }
  function dclick(x, e) { return ev(x, function () { hclick.call(e); }); }
  A.check = async function (e, g, a, r, x) {
    if (!g.noop) {
      await (g.dom ? dclick(x, e) : pressOn(x, e, g));
      if (cstate(e) !== g.v && nat(e)) { await dclick(x, e); } // one DOM-click retry
    }
    r.v = cstate(e);
    if (r.v !== g.v) { r.fw = fd(e, { k: "still" }); }
  };
  A.check.chk = function (e, g) {
    var t = ctl(e), b = boxOf(t, g.cb);
    if (!b) { throw ff("not_checkable", t, { k: "box" }); }
    if (cstate(b) === g.v) { g.f = g.noop = true; return b; }
    if (B.matches(t, ":disabled") || B.matches(b, ":disabled")) { throw ff("disabled", t); }
    if (!g.v && (roles(b, "radio menuitemradio").length || b.type === "radio")) { throw ff("not_checkable", t, { k: "radio" }); }
    if (nat(b) && !(_.visible(b) && _.css(b, "opacity") > 0 && _.css(b, "pointer-events") !== "none")) { g.f = g.dom = true; }
    return b;
  };
  // select: a native <select> (step 1 selects, step 2 reads back) or an
  // ARIA combobox or listbox (step 1 opens a collapsed combobox, step 2
  // clicks the matching role=option elements).
  // match: the indexes of the options ([value, label] pairs) that the
  // values g.vals pick (--by g.by). Native (s, e the <select>): exact label
  // or value, then the normalized label; options sharing a value are one
  // choice. ARIA lists compare normalized texts and count every match.
  function match(ps, g, s, e) {
    var by = g.by, out = [];
    g.vals.forEach(function (v, i) {
      function f(n) {
        return ps.reduce(function (a, p, j) {
          var l = n ? nrm(p[1]) : s ? p[1].trim() : p[1], x = n ? nrm(v) : v;
          if (by !== "value" && l === x || by !== "label" && p[0] && p[0] === x) { a.push(j); }
          return a;
        }, []);
      }
      var m = f(!s), d = m;
      if (!m.length && s && by !== "value") { m = f(1); }
      if (s) { d = m.filter(function (j, k) { return m.map(function (q) { return ps[q][0]; }).indexOf(ps[j][0]) === k; }); }
      if (!m.length) { throw ff("option_not_found", e, { k: s ? "sel" : "list", n: ps.length, o: ps.map(function (p) { return [p[0], nrm(p[1])]; }) }); }
      if (d.length > 1) { throw ff("ambiguous", e, { k: s ? "sel" : "list", i: i, d: d.length }); }
      out = out.concat(m);
    });
    return out;
  }
  var SEL = {};
  I.onReset(function () { SEL = {}; });
  A.select = async function (e, g, a, r, x) {
    var j, ga = B.getAttr, role = ga(e, "role");
    if (e.localName === "select") {
      var os = e.options, p = a.step === 1 ? g.pick : SEL[a.opId];
      delete SEL[a.opId];
      if (a.step === 1) {
        SEL[a.opId] = p;
        await ev(x, function () {
          for (j = 0; j < os.length; j++) { os[j].selected = p.indexOf(j) >= 0; }
          disp(e, bub("input"));
        });
        await ev(x, function () { disp(e, bub("change")); });
      }
      for (j = 0; a.step > 1 && p && j < os.length; j++) { if (os[j].selected !== p.indexOf(j) >= 0) { r.fw = fd(e, { k: "sel" }); } }
      r.v = p && p.map(function (i) { return os[i] && os[i].value; });
      return;
    }
    if (a.step === 1) {
      if (role === "combobox" && ga(e, "aria-expanded") !== "true") { await pressOn(x, e, g); }
      return;
    }
    var ac = ga(e, "aria-controls"), opts = [], want = [];
    function collect() {
      var s = role === "listbox" ? e : ac ? B.byId(document, ac) : document;
      return s ? [].filter.call(s.querySelectorAll("[role=option]"), function (o) { return B.gcr(o).length; }) : [];
    }
    for (j = 0; j < 5 && !(opts = collect()).length; j++) { await new Promise(function (f) { sT.call(w, f, 20); }); }
    if (!opts.length) { r.err = "combobox opened but no visible role=option elements were found"; return; }
    var pv = opts.map(function (o) { return [nrm(ga(o, "data-value")) || nrm(ga(o, "value")), nrm(_.name(o))]; });
    try {
      match(pv, g, 0, e).forEach(function (i) {
        if (ga(opts[i], "aria-disabled") === "true") { throw ff("disabled", e, { k: "opt" }); }
        if (want.indexOf(opts[i]) < 0) { want.push(opts[i]); }
      });
    } catch (y) {
      if (!y.__iwdpError) { throw y; }
      r.err = y.code + ": " + y.message;
      return;
    }
    for (j = 0; j < want.length; j++) {
      var o = want[j];
      if (role === "listbox" && ga(e, "aria-multiselectable") === "true" && ga(o, "aria-selected") === "true") { continue; }
      await ev(x, function () { o.scrollIntoView({ block: "nearest" }); });
      await pressOn(x, o, g);
    }
    r.v = want.map(function (o) { o = pv[opts.indexOf(o)]; return o[0] || o[1]; });
  };
  A.select.pre = true;
  A.select.chk = function (e, g, a) {
    if (e.localName !== "select") {
      if (!/^(combobox|listbox)$/.test(B.getAttr(e, "role"))) { throw ff("not_editable", e, { k: "role" }); }
      return e;
    }
    if (a.step > 1) { return e; }
    if (B.matches(e, ":disabled")) { throw ff("disabled", e, { k: "sel" }); }
    if (g.vals.length > 1 && !e.multiple) { throw ff("not_multiple", e); }
    g.pick = match([].map.call(e.options, function (o) { return [o.value, o.label]; }), g, 1, e);
    return e;
  };
  A.upload = async function (e, g, a, r, x) {
    if (e.localName !== "input" || e.type !== "file") { r.err = "the element is not a file input"; return; } // CDP DOM.setFileInputFiles
    if (g.files.length > 1 && !e.multiple) { r.err = "the file input takes one file"; return; }
    var dt = new w.DataTransfer();
    g.files.forEach(function (f) {
      var s = w.atob(f.b64), u = new Uint8Array(s.length);
      for (var i = 0; i < s.length; i++) { u[i] = s.charCodeAt(i); }
      dt.items.add(new w.File([u], f.name, { type: f.type }));
    });
    await ev(x, function () { e.files = dt.files; disp(e, bub("input")); });
    await ev(x, function () { disp(e, bub("change")); });
    r.v = e.files.length;
  };
  // The upload command (g.fi) refuses with agent-browser's not_file_input.
  A.upload.chk = function (e, g) {
    if (g.fi && e.localName !== "input" || e.type !== "file") { throw ff("not_file_input", e, { a: B.getAttr(e, "type") }); }
  };
  // Form preconditions of fill and type (agent-browser 0.45 forms.rs): g.fk
  // from Go maps a tag, "input:<type>" or "v:<type>" (value inputs) to the
  // refusal kind; m is the mode, fill or type.
  function tchk(e, g, m) {
    e = ctl(e);
    var t = e.localName, y = t === "input" ? e.type : "", K = g.fk, k = null, v = own.call(K, "v:" + y);
    if (B.matches(e, ":disabled")) {
      k = "disabled";
    } else if (own.call(K, t) && !isEd(e)) {
      k = K[t];
    } else if (own.call(K, "input:" + y) || m === "type" && v) {
      k = v ? "fill" : K["input:" + y];
    } else if (t !== "input" && t !== "textarea" && !isEd(e)) {
      k = "";
    } else if (m === "fill" && e.readOnly) {
      k = "read_only";
    }
    if (k !== null) { throw ff(k === "disabled" || k === "read_only" ? k : "not_editable", e, { k: k }); }
    if (m === "fill" && v) { g.vv = vval(e, y, String(g.text).trim()); }
    return e;
  }
  // vval: the value fill stores in a value input, or malformed_value when
  // the input would sanitize it away; a color is any opaque CSS color,
  // stored as #rrggbb.
  function vval(e, y, s) {
    var ok, lo = e.min === "" ? 0 : +e.min, hi = e.max === "" ? 100 : +e.max, c = document.createElement(y === "color" ? "canvas" : "input");
    if (y === "color") {
      c = c.getContext("2d");
      // An opaque color serializes as #rrggbb; another (rgba) is refused.
      var p = function (z) { c.fillStyle = z; c.fillStyle = s; return c.fillStyle; }, q = c && p("#010203");
      s = q === p("#040506") && q.charAt(0) === "#" && q;
      ok = !!s;
    } else if (y === "range") {
      ok = /^[-+]?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$/i.test(s) && +s >= lo && +s <= hi;
    } else {
      c.type = y;
      VS.input.call(c, s);
      ok = c.value || !s;
    }
    if (!ok) { throw ff("malformed_value", e, { lo: lo, hi: hi }); }
    return s;
  }
  // back: the read-back warning when the page changed the value it was
  // given (Go words it from the lengths; passwords are not judged).
  function back(e, want, r) {
    var ed = isEd(e), o = ed ? B.innerText(e) : e.value;
    function cmp(s) {
      s = String(s).replace(/\r\n/g, "\n");
      return ed ? s.split(/\s+/).filter(Boolean).join(" ") : e.type === "color" ? s.toLowerCase() : s;
    }
    if (e.type !== "password" && cmp(o) !== cmp(want)) { r.fw = fd(e, { k: o === "" ? "empty" : "len", g: want.length, h: o.length }); }
  }
  A.fill = async function (e, g, a, r, x) {
    var vs = VS[e.localName], got;
    if (g.vv !== undefined) {
      await ev(x, function () {
        vs.call(e, g.vv);
        got = e.value;
        disp(e, new Ev("input", { bubbles: true, composed: true }));
        disp(e, new Ev("change", { bubbles: true }));
      });
      await gap(x);
      if (e.type === "range" && +got !== +g.vv) { r.fw = fd(e, { k: "snap" }); } else { back(e, got, r); }
      r.v = e.value;
      return;
    }
    if (_.deepActive() !== e) {
      var seen = false, h = function () { seen = true; };
      await ev(x, function () { e.addEventListener("focus", h, true); e.focus(); e.removeEventListener("focus", h, true); });
      for (var n = _.deepActive(); n && !seen; n = n.parentNode || n.host) { seen = n === e; }
      if (!seen) { r.err = "not_editable: " + fd(e, { k: "focus" }); return; }
    }
    if (vs && e.readOnly) { r.err = "read_only: " + fd(e, { k: "read_only" }); return; }
    if (!await ev(x, function () { return disp(e, ie("beforeinput", "insertReplacementText", g.text, true)); })) { r.err = "page canceled the input"; return; }
    await ev(x, function () {
      if (vs) {
        try { e.select(); } catch (y) { /* no selection */ }
        vs.call(e, g.text);
        disp(e, ie("input", "insertReplacementText", g.text, false));
      } else {
        document.execCommand("selectAll");
        document.execCommand("insertText", false, g.text);
      }
    });
    if (vs) { await ev(x, function () { disp(e, new Ev("change", { bubbles: true })); }); }
    await gap(x);
    back(e, g.text, r);
    r.v = vs ? e.value : B.textContent(e);
  };
  A.fill.chk = function (e, g) { return tchk(e, g, "fill"); };
  A.fill.as = "focus"; // the precheck decides editability: only attached and visible are awaited
  A.type.chk = function (e, g) { return g.ko ? e : tchk(e, g, "type"); };
})(window.__iwdpBuilding);
