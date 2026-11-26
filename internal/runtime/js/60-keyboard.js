// 60-keyboard.js (W3b): type, keyboard and press (design 3.5; fill and the
// form preconditions of type are in the forms module, mod-forms/). Go
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
  var KE = w.KeyboardEvent, IE = w.InputEvent || w.Event;
  function setter(C) { return Object.getOwnPropertyDescriptor(C.prototype, "value").set; }
  var VS = { input: setter(HTMLInputElement), textarea: setter(HTMLTextAreaElement) };
  var click = HTMLElement.prototype.click, def = Object.defineProperty;
  var BLOCK = { text: 1, search: 1, url: 1, tel: 1, email: 1, password: 1, date: 1, month: 1, week: 1, time: 1, "datetime-local": 1, number: 1 };
  function kev(x, t, type, k, m) {
    return ev(x, function () {
      var e = new KE(type, { key: k.key, code: k.code || "", location: k.loc || 0, bubbles: true, cancelable: true, composed: true, view: w,
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
  // H: the modifier bits (Alt 1, Control 2, Meta 4, Shift 8) that keydown
  // of a modifier key holds until its keyup; press, keydown, keyup and
  // mouse (55-actions.js) apply them. They belong to the page runtime, as
  // KD and the mouse state do, so an owner reset and a new document end
  // them.
  var H = 0;
  I.onReset(function () { KD = {}; H = 0; });
  _.held = function () { return H; };
  // key: keydown; unless canceled, keypress and text insertion (printable
  // keys) or the default action; then, as the next native input event,
  // keyup (Space activates on keyup). ph "down" sends only keydown, ph "up"
  // only keyup. The CDP key events (internal/cdp input_domain.go) use ph
  // "kd", everything before keyup (CDP keyDown and rawKeyDown), and ph
  // "ch", keypress and the insertion without keydown (CDP char). T gives
  // the target when an event is sent: keydown and keyup go to the element
  // focused then, as WebKit sends them; keypress, insertion and default
  // action stay with the keydown's target (WebKit's keypress of the same
  // platform event), and Space activates only when keyup reaches it too.
  async function key(x, T, k, m, r, ph) {
    var ok, d = KD[k.key], t, u;
    if (ph !== "ch") { delete KD[k.key]; }
    if (ph === "up") {
      ok = !!d && d.ok; t = d && d.t;
    } else {
      await gap(x);
      t = T();
      ok = ph === "ch" || await kev(x, t, "keydown", k, m);
      if (ph === "down") {
        KD[k.key] = { t: t, ok: ok };
        return;
      }
      if (ok && !k.np && !m.c && !m.m && (k.text || k.key === "Enter")) { ok = await kev(x, t, "keypress", k, m); }
      if (ok) {
        if (k.text && !m.c && !m.m) {
          if (editable(t)) { await edit(x, t, k.text, "insertText"); }
        } else { await defaults(x, t, k, m, r); }
      }
      if (ph === "kd") { KD[k.key] = { t: t, ok: ok }; }
      if (ph) { return; }
    }
    await gap(x);
    u = T();
    if (await kev(x, u, "keyup", k, m) && ok && u === t && k.key === " " && !editable(t) && /^(button|submit|reset|checkbox|radio)$/.test(t.type)) {
      await ev(x, function () { click.call(t); });
      r.d.push("click " + desc(t));
    }
  }
  function target(e) { return e || _.deepActive() || document.body; }
  A.type = async function (e, g, a, r, x) {
    if (e && _.deepActive() !== e) { await ev(x, function () { e.focus(); }); }
    var t = target(e), T = function () { return t; };
    for (var i = 0; i < g.keys.length; i++) { await key(x, T, g.keys[i], g.m || {}, r); }
    r.v = t.value;
  };
  function mo(n) { return { a: n & 1, c: n & 2, m: n & 4, s: n & 8 }; }
  // km: key k under modifier bits n (agent-browser 0.45 modified_key_info):
  // Shift gives the US-layout shifted key (sk) and text; Alt, Control or
  // Meta remove the text and the keypress (np).
  function km(k, n) {
    var o = {}, p;
    for (p in k) { o[p] = k[p]; }
    if (n & 8 && k.sk) {
      o.key = k.sk;
      if (o.text) { o.text = k.sk; o.cc = k.sk.charCodeAt(0); }
    }
    if (n & 7) { o.text = ""; o.np = 1; }
    return o;
  }
  // press: CDP key events (internal/cdp) send g.m; the press, keydown and
  // keyup commands send g.c, the chord's modifier bits, and g.mk, the
  // modifier keys by bit (keyboard.go), and get the held
  // ones (H) as agent-browser 0.45 does: ph "kd" (keydown) and "down" (a
  // bare keydown) hold a modifier key, "up" releases it; a full press
  // first presses the chord modifiers not held (Control, Alt, Shift,
  // Meta), then the key, then releases them in reverse.
  A.press = async function (e, g, a, r, x) {
    var t = function () { return target(e); }, k = g.keys[0], MK = g.mk;
    if (g.c === undefined) { return key(x, t, k, g.m || {}, r, g.ph); }
    var own = k.mb || 0, held = H, cur = H, n, i, ch = [2, 1, 8, 4].filter(function (b) { return g.c & b & ~held; });
    if (g.ph === "up") {
      H &= ~own; n = H | g.c;
      return key(x, t, km(k, n), mo(n), r, "up");
    }
    if (g.ph) {
      n = H | g.c | own; H |= own;
      return key(x, t, km(k, n), mo(n), r, g.ph);
    }
    for (i = 0; i < ch.length; i++) { cur |= ch[i]; await key(x, t, MK[ch[i]], mo(cur), r, "kd"); }
    await key(x, t, km(k, cur | own), mo(cur | own), r, "kd");
    n = cur & ~(own & ~held);
    await key(x, t, km(k, n), mo(n), r, "up");
    for (i = ch.length - 1; i >= 0; i--) { cur &= ~ch[i]; await key(x, t, MK[ch[i]], mo(cur), r, "up"); }
  };
  A.inserttext = async function (e, g, a, r, x) {
    var t = target(e);
    if (editable(t) && !await edit(x, t, g.text, "insertText")) { r.err = "page canceled the input"; }
  };
  // The forms module (mod-forms/) replaces this check with the form
  // preconditions of type; a type into a target without it is refused, so
  // a caller that did not declare the module never skips them.
  A.type.chk = function (e, g) {
    if (!g.ko) { throw _.fail("internal", "the runtime module forms is not installed"); }
    return e;
  };
  // For the forms module: built-ins captured at the core's install.
  _.VS = VS; _.ie = ie; _.hclick = click;
})(window.__iwdpBuilding);
