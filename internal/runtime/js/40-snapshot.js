// 40-snapshot.js (W3a): the snapshot walk (design 3.2). It walks the flat
// tree (light DOM, open shadow roots, same-origin iframes up to four levels
// deep, as agent-browser 0.45.0) and returns JSON nodes; Go renders the
// agent-browser text.
(function (I) {
  "use strict";
  if (!I) { return; }
  var _ = I._, B = _.B;

  var SKIP = {};
  ("script style template noscript head title meta link base").split(" ").forEach(function (t) { SKIP[t] = true; });
  // CONTROL: agent-browser 0.45.0's INTERACTIVE_ROLES without Iframe, the
  // roles of controls (cursor scan and hints); INTERACTIVE: the roles that
  // get a ref.
  var CONTROL = {}, INTERACTIVE = {};
  function set(o, s) { s.split(" ").forEach(function (r) { o[r] = true; }); }
  var CONTROLS = "button link textbox checkbox radio combobox listbox menuitem menuitemcheckbox menuitemradio option searchbox slider " +
    "spinbutton switch tab treeitem";
  set(CONTROL, CONTROLS);
  set(INTERACTIVE, CONTROLS + " Iframe cell gridcell columnheader rowheader");
  var CONTENT = { heading: 1, listitem: 1, article: 1, region: 1, main: 1, navigation: 1 };
  var NATIVE = { a: 1, button: 1, input: 1, select: 1, textarea: 1, summary: 1 };
  var HIDDEN = '[hidden],[aria-hidden="true"]';
  // The controls that a link without href may wrap (agent-browser 0.45.0).
  var WRAPPED = 'a[href],button,input:not([type="hidden"]),select,textarea,summary' +
    "button link checkbox radio tab menuitem switch textbox combobox".replace(/(\w+) ?/g, ",[role=$1]");
  var FRAME_DEPTH = 4; // nested iframes walked (agent-browser MAX_IFRAME_SNAPSHOT_DEPTH)

  // cursorInfo finds elements without an interactive role that react to
  // the pointer or keyboard (agent-browser 0.45.0's cursor-interactive
  // scan, snapshot.rs scan_cursor_interactive_elements): a <details> is a
  // container (its <summary> is the control); a native control is a
  // candidate only when its role is not a control role (<summary>, <a>
  // without href, <button role=menu>); a link without href that wraps a
  // visible control is covered by that control.
  function cursorInfo(el, role) {
    var tag = el.localName;
    if (tag === "details") { return null; }
    var explicit = _.norm(B.getAttr(el, "role")).toLowerCase();
    if (CONTROL[explicit]) { return null; }
    if (NATIVE[tag] && (CONTROL[role] || (!explicit && (tag === "a" ? B.hasAttr(el, "href") : tag !== "summary" && (tag !== "input" || el.type !== "hidden"))))) { return null; }
    var pointer = _.css(el, "cursor") === "pointer";
    var onclick = B.hasAttr(el, "onclick") || el.onclick !== null;
    var ti = B.getAttr(el, "tabindex");
    var tabindex = ti !== null && ti !== "-1";
    var ce = B.getAttr(el, "contenteditable");
    var editable = ce === "" || ce === "true";
    if (!pointer && !onclick && !tabindex && !editable) { return null; }
    // Inherited cursor:pointer: compared with the flat-tree parent.
    var up = _.flatParent(el);
    if (pointer && !onclick && !tabindex && !editable && up && _.css(up, "cursor") === "pointer") { return null; }
    if (B.closest(el, HIDDEN)) { return null; }
    var r = B.gbcr(el);
    if (r.width === 0 || r.height === 0) { return null; }
    if (tag === "a" && !explicit) {
      var inner = B.qsaEl(el, WRAPPED);
      for (var i = 0; i < inner.length; i++) {
        if (!B.closest(inner[i], HIDDEN) && _.visible(inner[i])) { return null; }
      }
    }
    var hints = [];
    if (pointer) { hints.push("cursor:pointer"); }
    if (onclick) { hints.push("onclick"); }
    if (tabindex) { hints.push("tabindex"); }
    if (editable) { hints.push("contenteditable"); }
    var info = {
      kind: pointer || onclick ? "clickable" : (editable ? "editable" : "focusable"),
      hints: hints,
      text: _.norm(B.textContent(el)).slice(0, 100)
    };
    // A label that wraps a hidden checkbox or radio stands for it.
    var input = el.querySelector('input[type="radio"], input[type="checkbox"]');
    if (input && (_.css(input, "display") === "none" || _.css(input, "visibility") === "hidden" || input.hidden)) {
      info.hiddenInput = { role: input.type, checked: input.indeterminate ? "mixed" : String(input.checked) };
    }
    return info;
  }

  // visibilityHidden: computed visibility hidden or collapse. Visibility is
  // inherited, and a descendant can set it back to visible.
  function visibilityHidden(el) {
    var v = _.css(el, "visibility");
    return v === "hidden" || v === "collapse";
  }

  function firstSummary(details) {
    for (var c = details.firstElementChild; c; c = c.nextElementSibling) {
      if (c.localName === "summary") { return c; }
    }
    return null;
  }

  // The page walk and the scoped walk are one traversal. ctx.emit is true
  // while the walk is inside what it returns: the whole page, or a scope
  // root once the walk enters it (ctx.scopes). Outside, the walk follows the
  // same edges and rules but returns no nodes and hands out no refs, so a
  // scoped snapshot shows exactly the page snapshot's part of each scope
  // root, in walk order, and nothing of a root the page walk never enters
  // (design 3.2). A root inside another root is part of it.

  // walkChildren walks the flat-tree children of node. Text takes the
  // visibility of node, its parent in the flat tree. A closed <details>
  // shows only its first <summary> child (design 3.4).
  function walkChildren(node, ctx, out) {
    if (node.localName === "details" && !node.open) {
      var summary = firstSummary(node);
      if (summary) { walkElement(summary, ctx, out); }
      return;
    }
    var kids = _.flatChildren(node);
    var textHidden = null;
    for (var i = 0; i < kids.length; i++) {
      var k = kids[i];
      if (k.nodeType === 3) {
        if (!ctx.emit) { continue; }
        if (textHidden === null) { textHidden = visibilityHidden(node); }
        if (textHidden) { continue; }
        var t = k.data.replace(/\s+/g, " ");
        if (t !== "") {
          var sn = { role: "StaticText", name: t };
          if (ctx.full) { sn.h = _.handle(k); }
          out.push(sn);
        }
      } else if (k.nodeType === 1) {
        walkElement(k, ctx, out);
      }
    }
  }

  function walkElement(el, ctx, out) {
    if (!ctx.emit && ctx.scopes.indexOf(el) >= 0) {
      // The walk enters a scope root: return its part, then go on.
      ctx.emit = true;
      walkElement(el, ctx, ctx.tree);
      ctx.emit = false;
      return;
    }
    var tag = el.localName;
    if (SKIP[tag] || B.getAttr(el, "aria-hidden") === "true") { return; }
    if (_.css(el, "display") === "none") { return; }
    var role = _.role(el);
    if (role === null) { return; } // input type=hidden
    var hidden = visibilityHidden(el);
    var exposed = role !== "" && role !== "presentation" && !hidden;
    var cursor = hidden ? null : cursorInfo(el, role); // an invisible element is no action target
    if (!exposed && !cursor) {
      if (_.css(el, "content-visibility") !== "hidden") { walkChildren(el, ctx, out); }
      return;
    }
    var node = describe(el, exposed ? role : "generic", cursor, ctx);
    var children = [];
    if (tag === "select") {
      var opts = el.querySelectorAll("option");
      for (var i = 0; i < opts.length; i++) { walkElement(opts[i], ctx, children); }
    } else if (tag === "iframe") {
      if (ctx.frameDepth < FRAME_DEPTH) {
        var doc = null;
        try { doc = el.contentDocument; } catch (e) { doc = null; } // cross-origin
        if (doc && doc.body) {
          var frame = ctx.frame;
          ctx.frameDepth++;
          ctx.frame = ++ctx.frames;
          walkChildren(doc.body, ctx, children);
          ctx.frameDepth--;
          ctx.frame = frame;
        }
      }
    } else if (tag !== "input" && tag !== "textarea" && _.css(el, "content-visibility") !== "hidden") {
      walkChildren(el, ctx, children);
    }
    if (ctx.emit) {
      if (children.length) { node.children = children; }
      out.push(node);
    }
  }

  // describe returns the snapshot node of an exposed element, without
  // children, and hands out its ref while the walk emits. Outside a scope
  // it only records the node's label, which hints count page-wide.
  function describe(el, role, cursor, ctx) {
    var name = _.name(el, role);
    if (cursor && cursor.hiddenInput && role === "generic") {
      role = cursor.hiddenInput.role;
      name = name || cursor.text;
    }
    var node = { role: role };
    if (name) { node.name = name; }
    var st = _.states(el, role);
    for (var k in st) {
      if (Object.prototype.hasOwnProperty.call(st, k)) { node[k] = st[k]; }
    }
    if (cursor && cursor.hiddenInput && node.role === cursor.hiddenInput.role) { node.checked = cursor.hiddenInput.checked; }
    var ref = INTERACTIVE[role] || (CONTENT[role] && name) || cursor;
    if (ctx.full) {
      node.h = _.handle(el);
    } else if (ref) {
      if (ctx.emit) {
        node.ref = _.refFor(el, role, name, ctx.alloc);
        ctx.refs.push({ ref: node.ref, role: role, name: name });
      }
      ctx.labels.push({ node: node, el: el, key: ctx.frame + "\u0000" + role + "\u0000" + (name || (cursor ? cursor.text : "")) });
    }
    if (role === "Iframe" && !name) {
      // Presentation only: the refs table keeps the empty name.
      var label = _.norm(B.getAttr(el, "title")) || _.norm(B.getAttr(el, "name"));
      if (label) { node.displayName = label; }
    }
    if (role === "link" && el.href) { node.url = String(el.href); }
    if (cursor) { node.cursor = { kind: cursor.kind, hints: cursor.hints, text: cursor.text }; }
    return node;
  }

  function first(el, sel) { return B.qsaEl(el, sel)[0]; }
  function tail(s, sep) { return String(s || "").split(sep).pop(); }
  function text(el) { try { return String(B.innerText(el)); } catch (e) { return ""; } } // SVG: no innerText

  // hint describes a control from its icon, image file, stable attribute
  // and nearest row (agent-browser 0.45.0 snapshot.rs HINT_FUNCTION).
  function hint(el) {
    var use = first(el, "svg use"), svg = first(el, "svg[id], svg[data-icon]"), mark = first(el, "svg[class], i[class]");
    var img = first(el, 'img[src]:not([src^="data:"])');
    var icon = use ? tail(B.getAttr(use, "href") || B.getAttr(use, "xlink:href"), "#")
      : svg ? B.getAttr(svg, "id") || B.getAttr(svg, "data-icon")
      : mark ? B.getAttr(mark, "class").split(/\s+/).filter(function (c) { return /^(lucide|fa|bi|mdi|ri|ti|icon)-/.test(c); }).join(" ") : "";
    var file = img ? tail(B.getAttr(img, "src").split(/[?#]/)[0], "/").replace(/\.\w+$/, "") : "";
    var key = ["data-testid", "name", "id"].filter(function (k) { return /^(?!.*\d{3})[a-z][\w-]*$/i.test(B.getAttr(el, k) || ""); })[0];
    var row = B.closest(el, "tr, li, article, [role=row], [role=listitem]");
    var near = row ? text(row).replace(text(el), "").replace(/\s+/g, " ").trim() : "";
    return [icon && "icon:" + icon, file && "img:" + file, key && key + ":" + B.getAttr(el, key), near && "in:" + near]
      .filter(Boolean).map(function (s) { return s.slice(0, 48); }).join("; ");
  }

  // addHints gives a hint to every ref'd control (a control role or a
  // cursor-interactive element) whose label (name, else the cursor text) is
  // empty or shared with another ref of the same document (agent-browser
  // 0.45.0 snapshot.rs hint_targets). Labels outside the scope count too.
  function addHints(labels) {
    var count = {};
    labels.forEach(function (l) { count[l.key] = (count[l.key] || 0) + 1; });
    labels.forEach(function (l) {
      var n = l.node;
      if (!n.ref || !(n.cursor || CONTROL[n.role])) { return; }
      if (count[l.key] === 1 && !/\u0000$/.test(l.key)) { return; }
      var h = hint(l.el);
      if (h) { n.hint = h; }
    });
  }

  // walk returns the nodes of root and hands out refs from nextRef on.
  // Without a root, or with this document's body or html element, it walks
  // the page from the body. With another root it runs the page walk and
  // returns the root's part of it; a root that the page walk never enters
  // gives no nodes. In full mode (the CDP accessibility tree, 65-cdp.js)
  // it hands out no refs: every node, text included, gets its node handle
  // h instead.
  function walk(root, nextRef, full) {
    var roots = root ? [].concat(root) : [];
    var ctx = { alloc: { next: nextRef || 1 }, refs: [], labels: [], frameDepth: 0, frame: 0, frames: 0, tree: [], emit: true, scopes: roots, full: !!full };
    if (roots.length && roots.indexOf(document.body) < 0 && roots.indexOf(document.documentElement) < 0) {
      ctx.emit = false;
    }
    if (document.body) { walkChildren(document.body, ctx, ctx.tree); }
    addHints(ctx.labels);
    var count = {};
    ctx.refs.forEach(function (r) { var key = r.role + "\u0000" + r.name; count[key] = (count[key] || 0) + 1; });
    ctx.refs.forEach(function (r) { r.unique = count[r.role + "\u0000" + r.name] === 1; });
    return { tree: ctx.tree, refs: ctx.refs, nextRef: Math.max(ctx.alloc.next, _.maxRef() + 1), doc: _.doc() };
  }

  // scopeRoots: the elements of snapshot -s, every match of a selector
  // (agent-browser 0.45.0) or the element of a ref.
  function scopeRoots(spec, call) {
    if (spec.ref) { return [_.resolve(spec, call)]; }
    var sel = String(spec.selector), els, pre = "invalid selector " + JSON.stringify(sel) + ": ";
    try {
      els = _.all(spec, call);
    } catch (e) {
      if (e && e.code === "bad_selector" && e.message.indexOf(pre) === 0) {
        throw _.fail("bad_selector", "Invalid selector '" + sel + "': " + e.message.slice(pre.length));
      }
      throw e;
    }
    if (!els.length) { throw _.fail("not_found", "Selector '" + sel + "' did not match any element"); }
    return els;
  }

  I.def("snapshot", function (a, call) {
    var res = walk(a.scope ? scopeRoots(a.scope, call) : null, a.nextRef);
    res.url = String(location.href);
    res.title = String(document.title);
    return res;
  });
  _.walk = walk;
})(window.__iwdpBuilding);
