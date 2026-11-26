// 45-locate.js (W3a): the target resolver (refs, CSS and locator forms,
// design 3.3), find, the getters, is, hit-test and read.
(function (I) {
  "use strict";
  if (!I) { return; }
  var _ = I._, B = _.B, fail = _.fail;

  // allElements lists the elements of the document, its open shadow roots
  // and its same-origin iframes (one level), in tree order per root.
  function allElements(frames) {
    var out = [];
    function add(root, depth) {
      var list = root.nodeType === 9 ? B.qsaDoc(root, "*") : B.qsaFrag(root, "*");
      for (var i = 0; i < list.length; i++) {
        var el = list[i];
        out.push(el);
        var sr = B.shadowRoot(el);
        if (sr) { add(sr, depth); }
        if (frames && depth === 0 && el.localName === "iframe") {
          var d = null;
          try { d = el.contentDocument; } catch (e) { d = null; }
          if (d) { add(d, 1); }
        }
      }
    }
    add(document, 0);
    return out;
  }

  // deepQuery runs a CSS selector in the document and its open shadow roots.
  function deepQuery(sel) {
    var out = [];
    function add(root) {
      var list;
      try {
        list = root.nodeType === 9 ? B.qsaDoc(root, sel) : B.qsaFrag(root, sel);
      } catch (e) {
        throw fail("bad_selector", "invalid selector " + JSON.stringify(sel) + ": " + (e && e.message ? e.message : e));
      }
      for (var i = 0; i < list.length; i++) { out.push(list[i]); }
      var all = root.nodeType === 9 ? B.qsaDoc(root, "*") : B.qsaFrag(root, "*");
      for (var j = 0; j < all.length; j++) {
        var sr = B.shadowRoot(all[j]);
        if (sr) { add(sr); }
      }
    }
    add(document);
    return out;
  }

  // matcher returns a test for text: exact match for exact or a quoted
  // value, else a case-insensitive substring; NBSP counts as a space.
  function matcher(value, exact) {
    var v = String(value);
    if (v.length >= 2 && v.charAt(0) === '"' && v.charAt(v.length - 1) === '"') { v = v.slice(1, -1); exact = true; }
    v = _.norm(v.replace(/\u00a0/g, " "));
    var lv = v.toLowerCase();
    return function (s) {
      s = _.norm(String(s || "").replace(/\u00a0/g, " "));
      return exact ? s === v : s.toLowerCase().indexOf(lv) >= 0;
    };
  }

  var NO_TEXT = { html: 1, head: 1, body: 1, script: 1, style: 1, template: 1, noscript: 1, title: 1 };

  // locate returns every element that a locator matches.
  function locate(kind, value, opts) {
    opts = opts || {};
    var m, els, out = [];
    switch (kind) {
    case "css":
      return deepQuery(value);
    case "role":
      var nm = opts.name !== undefined && opts.name !== null && opts.name !== "" ? matcher(opts.name, opts.exact) : null;
      els = allElements(false);
      for (var i = 0; i < els.length; i++) {
        if (_.role(els[i]) === value && (!nm || nm(_.name(els[i])))) { out.push(els[i]); }
      }
      return out;
    case "text":
      m = matcher(value, opts.exact);
      els = allElements(false).filter(function (el) { return !NO_TEXT[el.localName] && m(B.textContent(el)); });
      // innermost matches only: drop an element that contains another match
      return els.filter(function (el) {
        return !els.some(function (o) { return o !== el && el.contains(o); });
      });
    case "label":
      m = matcher(value, opts.exact);
      return allElements(false).filter(function (el) { var t = _.labelText(el); return t !== "" && m(t); });
    case "placeholder": case "alt": case "title":
      m = matcher(value, opts.exact);
      return allElements(false).filter(function (el) { return B.hasAttr(el, kind) && m(B.getAttr(el, kind)); });
    case "testid":
      return allElements(false).filter(function (el) { return B.getAttr(el, "data-testid") === String(value); });
    }
    throw fail("bad_selector", "unknown locator kind " + JSON.stringify(kind) + "; use role, text, label, placeholder, alt, title or testid");
  }

  // parseSelector splits the locator forms of design 3.3 from CSS.
  function parseSelector(sel) {
    var m = /^(text|label|placeholder|testid|alt|title)=([\s\S]*)$/.exec(sel);
    if (m) { return { kind: m[1], value: m[2] }; }
    m = /^role=([a-zA-Z]+)(?:\[name=(?:"((?:[^"\\]|\\.)*)"|'([^']*)')\])?$/.exec(sel);
    if (m) {
      var name = m[2] !== undefined ? m[2].replace(/\\(.)/g, "$1") : m[3];
      return { kind: "role", value: m[1], name: name, exact: name !== undefined };
    }
    return { kind: "css", value: sel };
  }

  function describe(el) {
    var s = "<" + el.localName;
    if (el.id) { s += ' id="' + el.id + '"'; }
    var cls = B.getAttr(el, "class");
    if (cls) { s += ' class="' + cls + '"'; }
    return s + ">";
  }

  // docCurrent: d is this document, or the document that a connected
  // iframe of a current document shows now. WebKit keeps an element
  // connected to its old document after its frame navigates, so
  // isConnected alone does not prove that the frame still shows it.
  function docCurrent(d) {
    while (d !== document) {
      var win = B.defaultView(d), fe = null;
      try { fe = win ? win.frameElement : null; } catch (e) { fe = null; } // cross-origin parent
      if (!fe || B.contentDocument(fe) !== d || !B.connected(fe)) { return false; }
      d = B.ownerDoc(fe);
    }
    return true;
  }

  // The refs table's messages for refs that an owner change dropped
  // (internal/refs Lookup).
  function droppedRef(reason, at) {
    if (reason === "daemon") { return at + ": daemon restarted; run snapshot"; }
    if (reason === "epoch") { return at + ": the lease of this page changed since the snapshot that produced it; run snapshot"; }
    return "no snapshot produced " + at;
  }

  // replaced: agent-browser 0.45.0's message for a ref whose document is
  // gone (element.rs; internal/refs Lookup has the same text).
  function replaced(at) {
    return "Ref " + at + " is from a document that is no longer loaded (the page navigated or the frame was removed). Take a new snapshot and use a fresh ref.";
  }

  // resolveRef: resolve order steps 3 to 5 of design 3.3. A ref that a
  // reset of the page maps dropped belongs to an earlier owner: it is
  // unknown, never found again by role and name (design 3.1). A ref into a
  // frame that shows another document now, or that was removed, is stale.
  function resolveRef(spec, call) {
    var at = "@" + spec.ref;
    if (spec.doc && spec.doc !== _.doc()) {
      throw fail("stale_ref", replaced(at));
    }
    var el = _.refGet(spec.ref);
    if (!el) {
      var dropped = _.resetFrom(Number(String(spec.ref).slice(1)));
      if (dropped) { throw fail("unknown_ref", droppedRef(dropped, at)); }
    } else if (!docCurrent(B.ownerDoc(el))) {
      throw fail("stale_ref", replaced(at));
    }
    if (el && B.connected(el)) { return el; }
    if (spec.unique && spec.role) {
      var same = locate("role", spec.role, { name: spec.name || "", exact: true }).filter(function (c) {
        return (_.name(c) || "") === (spec.name || "");
      });
      if (same.length === 1) {
        _.rebind(spec.ref, same[0], spec.role, spec.name || "");
        call.warn(at + " was replaced in the page; resolved it by its role and name");
        return same[0];
      }
    }
    if (!el && !spec.role) { throw fail("unknown_ref", "no snapshot produced " + at); }
    throw fail("stale_ref", at + " no longer matches an element; run snapshot");
  }

  // all returns every element a target spec matches.
  function all(spec, call) {
    if (spec.ref) { return [resolveRef(spec, call)]; }
    var p = parseSelector(String(spec.selector));
    return locate(p.kind, p.value, p);
  }

  // resolve returns the one element of a target spec; several matches
  // are ambiguous (design 3.3).
  function resolve(spec, call) {
    var els = all(spec, call);
    if (els.length === 0) { throw fail("not_found", "no element matches " + JSON.stringify(spec.selector)); }
    if (els.length > 1) {
      throw fail("ambiguous", JSON.stringify(spec.selector) + " matches " + els.length +
        " elements; use a more specific selector, find first|last|nth, or a ref from snapshot", { count: els.length });
    }
    return els[0];
  }

  function box(el) {
    var r = B.gbcr(el);
    return { x: r.x, y: r.y, width: r.width, height: r.height };
  }

  var TEXT_INPUTS = { "": 1, text: 1, email: 1, password: 1, search: 1, tel: 1, url: 1, number: 1, date: 1, "datetime-local": 1, month: 1, time: 1, week: 1 };
  function editable(el) {
    if (_.disabled(el)) { return false; }
    var t = el.localName;
    if (t === "textarea") { return !el.readOnly; }
    if (t === "input") { return !el.readOnly && !!TEXT_INPUTS[(B.getAttr(el, "type") || "").toLowerCase()]; }
    return !!el.isContentEditable;
  }
  function deepActive() {
    var a = document.activeElement;
    while (a && B.shadowRoot(a) && B.shadowRoot(a).activeElement) { a = B.shadowRoot(a).activeElement; }
    return a;
  }

  var DEFAULT_STYLES = ["display", "visibility", "opacity", "position", "z-index", "overflow", "width", "height", "margin",
    "padding", "color", "background-color", "font-family", "font-size", "font-weight", "line-height", "cursor",
    "pointer-events", "transform"];

  I.def("get", function (a, call) {
    switch (a.what) {
    case "url": return String(location.href);
    case "title": return String(document.title);
    case "count": return all(a.target, call).length;
    }
    var el = resolve(a.target, call);
    switch (a.what) {
    case "text": return _.norm(el instanceof HTMLElement ? B.innerText(el) : B.textContent(el));
    case "html": return String(B.innerHTML(el));
    case "value":
      if (!("value" in el)) { throw fail("unsupported", describe(el) + " has no value; use get text"); }
      return String(el.value);
    case "attr": return B.getAttr(el, String(a.name));
    case "box":
      var b = box(el);
      b.visible = _.visible(el);
      b.enabled = !_.disabled(el);
      return b;
    case "styles":
      var s = _.style(el);
      var names = a.name ? String(a.name).split(",").map(function (n) { return n.trim(); }).filter(Boolean) : DEFAULT_STYLES;
      return names.map(function (n) { return [n, s ? B.prop(s, n) : ""]; });
    }
    throw fail("unsupported", "get " + a.what + " is not supported");
  });

  I.def("is", function (a, call) {
    if (a.state === "visible") {
      var els = all(a.target, call);
      if (els.length > 1) { resolve(a.target, call); }
      return els.length === 1 && _.visible(els[0]);
    }
    var el = resolve(a.target, call);
    switch (a.state) {
    case "enabled": return !_.disabled(el);
    case "editable": return editable(el);
    case "focused": return deepActive() === el;
    case "checked":
      if (el.localName === "input" && (el.type === "checkbox" || el.type === "radio")) { return !!el.checked; }
      var r = _.role(el);
      if (r === "checkbox" || r === "radio" || r === "switch" || r === "menuitemcheckbox" || r === "menuitemradio") {
        return B.getAttr(el, "aria-checked") === "true";
      }
      throw fail("unsupported", describe(el) + " is not a checkbox, radio button or switch");
    }
    throw fail("unsupported", "is " + a.state + " is not supported; use visible, enabled, checked, editable or focused");
  });

  // match describes one element with a new or kept ref, like a snapshot line.
  function match(el, alloc, refs) {
    var role = _.role(el) || "generic";
    if (role === "presentation") { role = "generic"; }
    var name = _.name(el, role);
    var ref = _.refFor(el, role, name, alloc);
    refs.push({ ref: ref, role: role, name: name });
    var node = { ref: ref, role: role };
    if (name) { node.name = name; }
    var st = _.states(el, role);
    for (var k in st) {
      if (Object.prototype.hasOwnProperty.call(st, k)) { node[k] = st[k]; }
    }
    return node;
  }
  function withRefs(nodes, alloc, refs) {
    var count = {};
    refs.forEach(function (r) { var key = r.role + "\u0000" + r.name; count[key] = (count[key] || 0) + 1; });
    refs.forEach(function (r) { r.unique = count[r.role + "\u0000" + r.name] === 1; });
    return { nodes: nodes, refs: refs, nextRef: Math.max(alloc.next, _.maxRef() + 1), doc: _.doc() };
  }

  I.def("find", function (a, call) {
    var els;
    switch (a.kind) {
    case "first": case "last": case "nth":
      els = all({ selector: a.value }, call);
      if (a.kind === "first") { els = els.slice(0, 1); }
      else if (a.kind === "last") { els = els.slice(-1); }
      else {
        // 0-based; a negative index counts from the end (agent-browser handle_nth)
        var i = a.index < 0 ? els.length + a.index : a.index;
        els = i >= 0 && i < els.length ? [els[i]] : [];
      }
      break;
    default:
      els = locate(a.kind, a.value, { name: a.name, exact: !!a.exact });
    }
    if (els.length === 0) {
      throw fail("not_found", "no element matches find " + a.kind + " " + JSON.stringify(a.value));
    }
    var alloc = { next: a.nextRef || 1 }, refs = [];
    return withRefs(els.map(function (el) { return match(el, alloc, refs); }), alloc, refs);
  });

  I.def("hitTest", function (a) {
    var el = B.fromPoint(document, a.x, a.y);
    while (el && B.shadowRoot(el) && B.shadowFromPoint) {
      var inner = B.shadowFromPoint(B.shadowRoot(el), a.x, a.y);
      if (!inner || inner === el) { break; }
      el = inner;
    }
    if (!el) { throw fail("not_found", "no element at " + a.x + ", " + a.y + "; the point is outside the viewport"); }
    var alloc = { next: a.nextRef || 1 }, refs = [];
    var res = withRefs([match(el, alloc, refs)], alloc, refs);
    res.element = describe(el);
    res.box = box(el);
    return res;
  });

  I.def("read", function (a) {
    if (a.outline) {
      return allElements(false).filter(function (el) { return _.role(el) === "heading" && _.visible(el); }).map(function (el) {
        return new Array(_.states(el, "heading").level + 1).join("#") + " " + _.name(el, "heading");
      }).join("\n");
    }
    return document.body ? String(B.innerText(document.body)).replace(/\n{3,}/g, "\n\n").trim() : "";
  });

  _.resolve = resolve;
  _.all = all;
  _.locate = locate;
  _.els = allElements;
})(window.__iwdpBuilding);
