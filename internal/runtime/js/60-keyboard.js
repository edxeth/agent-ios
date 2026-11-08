// 60-keyboard.js (W3b): fill, type, keyboard and press (design 3.5). Go
// sends each key as {key, code, kc (keyCode), ki (keyIdentifier), text}.
// Cancellable events come before any change, and their result decides
// the next step; default actions of synthetic keys are emulated once, only
// when no event of the key was canceled, and named in r.d. A key is two
// native input events (_.gap between them): keydown with keypress, the
// insertion or default action, then keyup.
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, B = _.B, w = window, A = _.A, ev = _.ev, gap = _.gap, disp = _.disp, desc = _.desc;
  var KE = w.KeyboardEvent, IE = w.InputEvent || w.Event, Ev = w.Event;
  function setter(C) { return Object.getOwnPropertyDescriptor(C.prototype, "value").set; }
  var VS = { input: setter(HTMLInputElement), textarea: setter(HTMLTextAreaElement) };
  var click = HTMLElement.prototype.click, def = Object.defineProperty;
  var BLOCK = { text: 1, search: 1, url: 1, tel: 1, email: 1, password: 1, date: 1, month: 1, week: 1, time: 1, "datetime-local": 1, number: 1 };
  function kev(x, t, type, k, m) {
    return ev(x, function () {
      var e = new KE(type, { key: k.key, code: k.code || "", bubbles: true, cancelable: true, composed: true, view: w,
        shiftKey: !!m.s, ctrlKey: !!m.c, altKey: !!m.a, metaKey: !!m.m });
      try {
        def(e, "keyCode", { value: type === "keypress" ? k.cc || k.kc : k.kc });
        def(e, "which", { value: type === "keypress" ? k.cc || k.kc : k.kc });
        def(e, "charCode", { value: type === "keypress" ? k.cc || 0 : 0 });
        def(e, "keyIdentifier", { value: k.ki || "" });
      } catch (y) { /* read-only in this engine */ }
      return disp(t, e);
    });
  }
  function ie(type, it, data, c) {
    return new IE(type, { bubbles: true, cancelable: c, composed: true, inputType: it, data: data });
  }
  function sel(e) {
    var a = null, b = null;
    try { a = e.selectionStart; b = e.selectionEnd; } catch (y) { /* no selection API */ }
    return a === null ? [e.value.length, e.value.length] : [a, b];
  }
  // edit replaces range [s, e) (default: the selection) of t with txt
  // after an uncanceled beforeinput, moves the caret to the end of the
  // insertion and sends input in the same task, as an editor does. It
  // resolves to false when the page canceled the beforeinput.
  async function edit(x, t, txt, it, s, e) {
    if (!await ev(x, function () { return disp(t, ie("beforeinput", it, txt || null, true)); })) { return false; }
    await ev(x, function () {
      var vs = VS[t.localName];
      if (!vs) {
        if (t.isContentEditable) { document.execCommand(txt ? "insertText" : it === "deleteContentForward" ? "forwardDelete" : "delete", false, txt); }
        return;
      }
      var r = sel(t), v = t.value;
      if (s === undefined) { s = r[0]; e = r[1]; }
      vs.call(t, v.slice(0, s) + txt + v.slice(e));
      try { t.setSelectionRange(s + txt.length, s + txt.length); } catch (y) { /* input type without a caret */ }
      disp(t, ie("input", it, txt || null, false));
    });
    return true;
  }
  function editable(t) { return !!VS[t.localName] && _.editable(t) || t.isContentEditable; }
  function submit(t, r) {
    var f = t.form, els = f ? f.elements : [], i, n = 0;
    if (!f) { return; }
    for (i = 0; i < els.length; i++) {
      var y = els[i];
      if (y.type === "submit" && (y.localName === "button" || y.localName === "input") || y.type === "image" && y.localName === "input") {
        if (!_.disabled(y)) { click.call(y); r.d.push("implicit submission: clicked " + desc(y)); }
        return;
      }
    }
    for (i = 0; i < els.length; i++) { if (els[i].localName === "input" && BLOCK[els[i].type]) { n++; } }
    if (n === 1) { f.requestSubmit ? f.requestSubmit() : f.submit(); r.d.push("implicit submission: form.requestSubmit()"); }
  }
  function tab(t, back, r) {
    var all = B.qsaDoc(document, "a[href],button,input,select,textarea,iframe,summary,[tabindex],[contenteditable]"), pos = [], zero = [];
    for (var i = 0; i < all.length; i++) {
      var y = all[i];
      if (y.tabIndex >= 0 && !_.disabled(y) && y.type !== "hidden" && _.visible(y)) { (y.tabIndex > 0 ? pos : zero).push(y); }
    }
    pos.sort(function (a, b) { return a.tabIndex - b.tabIndex; });
    var L = pos.concat(zero), j = L.indexOf(t), n = L.length;
    if (!n) { return; }
    var to = L[back ? (j <= 0 ? n - 1 : j - 1) : (j + 1) % n];
    to.focus();
    r.d.push("focus moved to " + desc(to));
  }
  // defaults runs the default action of key k on t after its keydown.
  async function defaults(x, t, k, m, r) {
    var K = k.key, tn = t.localName, vs = VS[tn];
    if (K === "Enter" && tn === "textarea" && editable(t)) {
      if (await edit(x, t, "\n", "insertLineBreak")) { r.d.push("line break"); }
    } else if ((K === "Backspace" || K === "Delete") && editable(t)) {
      var s = vs ? sel(t) : [0, 0], f = K === "Delete";
      var a = s[0] < s[1] ? s[0] : f ? s[0] : Math.max(0, s[0] - 1), b = s[0] < s[1] ? s[1] : f ? s[0] + 1 : s[0];
      if ((a < b || !vs) && await edit(x, t, "", f ? "deleteContentForward" : "deleteContentBackward", a, b)) { r.d.push(f ? "delete" : "backspace"); }
    } else {
      await ev(x, function () {
        if (K === "Enter") {
          // Enter activates links and buttons; checkboxes and radios
          // activate on Space (design 3.5).
          if (tn === "a" || tn === "button" || tn === "input" && /^(submit|button|reset|image)$/.test(t.type)) { click.call(t); r.d.push("click " + desc(t)); } else if (t.form) { submit(t, r); }
        } else if (K === "Tab") {
          tab(t, m.s, r);
        } else if ((K === "ArrowLeft" || K === "ArrowRight") && vs) {
          var c = sel(t), p = K === "ArrowLeft" ? (c[0] < c[1] ? c[0] : Math.max(0, c[0] - 1)) : (c[0] < c[1] ? c[1] : Math.min(t.value.length, c[1] + 1));
          try { t.setSelectionRange(p, p); r.d.push("caret moved to " + p); } catch (y) { /* no caret */ }
        } else if ((K === "ArrowUp" || K === "ArrowDown") && tn === "select") {
          var q = t.selectedIndex + (K === "ArrowUp" ? -1 : 1);
          if (q >= 0 && q < t.options.length) {
            t.selectedIndex = q;
            disp(t, _.bub("input")); disp(t, _.bub("change"));
            r.d.push("selected option " + q);
          }
        }
      });
    }
  }
  // KD holds the keys whose keydown phase ran alone (ph "down"), by key,
  // with its target and whether the page let it through. A keyup phase
  // alone (ph "up") applies the keyup default action only after a matching
  // uncanceled keydown of the same key on the same target (design 3.5: a
  // canceled keydown skips the default action). A full press of the key
  // and an owner reset (a release also resets, 10-core.js) forget it.
  var KD = {};
  I.onReset(function () { KD = {}; });
  // key: keydown; unless canceled, keypress and text insertion (printable
  // keys) or the default action; then, as the next native input event,
  // keyup (Space activates on keyup). ph "down" sends only keydown, ph "up"
  // only keyup.
  async function key(x, t, k, m, r, ph) {
    var ok, d = KD[k.key];
    delete KD[k.key];
    if (ph === "up") {
      ok = !!d && d.t === t && d.ok;
    } else {
      await gap(x);
      ok = await kev(x, t, "keydown", k, m);
      if (ph === "down") {
        KD[k.key] = { t: t, ok: ok };
        return;
      }
      if (ok && !m.c && !m.m && (k.text || k.key === "Enter")) { ok = await kev(x, t, "keypress", k, m); }
      if (ok) {
        if (k.text && !m.c && !m.m) {
          if (editable(t)) { await edit(x, t, k.text, "insertText"); }
        } else { await defaults(x, t, k, m, r); }
      }
    }
    await gap(x);
    if (await kev(x, t, "keyup", k, m) && ok && k.key === " " && !editable(t) && /^(button|submit|reset|checkbox|radio)$/.test(t.type)) {
      await ev(x, function () { click.call(t); });
      r.d.push("click " + desc(t));
    }
  }
  function target(e) { return e || _.deepActive() || document.body; }
  A.type = async function (e, g, a, r, x) {
    if (e && _.deepActive() !== e) { await ev(x, function () { e.focus(); }); }
    var t = target(e);
    for (var i = 0; i < g.keys.length; i++) { await key(x, t, g.keys[i], g.m || {}, r); }
    r.v = t.value;
  };
  A.press = function (e, g, a, r, x) { return key(x, target(e), g.keys[0], g.m || {}, r, g.ph); };
  A.inserttext = async function (e, g, a, r, x) {
    var t = target(e);
    if (editable(t) && !await edit(x, t, g.text, "insertText")) { r.err = "page canceled the input"; }
  };
  A.fill = async function (e, g, a, r, x) {
    if (_.deepActive() !== e) { await ev(x, function () { e.focus(); }); }
    if (!await ev(x, function () { return disp(e, ie("beforeinput", "insertReplacementText", g.text, true)); })) { r.err = "page canceled the input"; return; }
    var vs = VS[e.localName];
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
    if (vs) {
      await ev(x, function () { disp(e, new Ev("change", { bubbles: true })); });
      r.v = e.value;
    } else {
      r.v = B.textContent(e);
    }
  };
})(window.__iwdpBuilding);
