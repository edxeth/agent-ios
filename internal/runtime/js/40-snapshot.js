// 40-snapshot.js (W3a): the snapshot walk (design 3.2). It walks the flat
// tree (light DOM, open shadow roots, same-origin iframes one level deep)
// and returns JSON nodes; Go renders the agent-browser text.
(function (I) {
  "use strict";
  if (!I) { return; }
  var _ = I._, B = _.B;

  var SKIP = {};
  ("script style template noscript head title meta link base").split(" ").forEach(function (t) { SKIP[t] = true; });
  var INTERACTIVE = {};
  ("button link textbox checkbox radio combobox listbox menuitem menuitemcheckbox menuitemradio option searchbox slider " +
    "spinbutton switch tab treeitem Iframe cell gridcell columnheader rowheader").split(" ")
    .forEach(function (r) { INTERACTIVE[r] = true; });
  var CONTENT = { heading: 1, listitem: 1, article: 1, region: 1, main: 1, navigation: 1 };
  var INTERACTIVE_TAGS = { a: 1, button: 1, input: 1, select: 1, textarea: 1, details: 1, summary: 1 };

  // cursorInfo finds elements without an interactive role that react to
  // the pointer or keyboard (agent-browser's cursor-interactive rule).
  function cursorInfo(el, role) {
    if (INTERACTIVE_TAGS[el.localName]) { return null; }
    var explicit = B.getAttr(el, "role");
    if (explicit && INTERACTIVE[explicit.toLowerCase()]) { return null; }
    var pointer = _.css(el, "cursor") === "pointer";
    var onclick = B.hasAttr(el, "onclick") || el.onclick !== null;
    var ti = B.getAttr(el, "tabindex");
    var tabindex = ti !== null && ti !== "-1";
    var ce = B.getAttr(el, "contenteditable");
    var editable = ce === "" || ce === "true";
    if (!pointer && !onclick && !tabindex && !editable) { return null; }
    if (pointer && !onclick && !tabindex && !editable && el.parentElement && _.css(el.parentElement, "cursor") === "pointer") { return null; }
    var r = B.gbcr(el);
    if (r.width === 0 || r.height === 0) { return null; }
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
  // while the walk is inside what it returns: the whole page, or the scope
  // root once the walk enters it (ctx.scope). Outside, the walk follows the
  // same edges and rules but builds no nodes and hands out no refs, so a
  // scoped snapshot shows exactly the page snapshot's part of the scope
  // root, and nothing when the page walk never enters it (design 3.2).

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
    for (var i = 0; i < kids.length && !ctx.done; i++) {
      var k = kids[i];
      if (k.nodeType === 3) {
        if (!ctx.emit) { continue; }
        if (textHidden === null) { textHidden = visibilityHidden(node); }
        if (textHidden) { continue; }
        var t = k.data.replace(/\s+/g, " ");
        if (t !== " " && t !== "") { out.push({ role: "StaticText", name: t }); }
        else if (t === " ") { out.push({ role: "StaticText", name: " " }); }
      } else if (k.nodeType === 1) {
        walkElement(k, ctx, out);
      }
    }
  }

  function walkElement(el, ctx, out) {
    if (ctx.done) { return; }
    if (!ctx.emit && el === ctx.scope) {
      // The walk enters the scope root: return its part, then stop.
      ctx.emit = true;
      walkElement(el, ctx, ctx.tree);
      ctx.emit = false;
      ctx.done = true;
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
    var node = ctx.emit ? describe(el, exposed ? role : "generic", cursor, ctx) : null;
    var children = [];
    if (tag === "select") {
      var opts = el.querySelectorAll("option");
      for (var i = 0; i < opts.length; i++) { walkElement(opts[i], ctx, children); }
    } else if (tag === "iframe") {
      if (ctx.frameDepth === 0) {
        var doc = null;
        try { doc = el.contentDocument; } catch (e) { doc = null; } // cross-origin
        if (doc && doc.body) {
          ctx.frameDepth++;
          walkChildren(doc.body, ctx, children);
          ctx.frameDepth--;
        }
      }
    } else if (tag !== "input" && tag !== "textarea" && _.css(el, "content-visibility") !== "hidden") {
      walkChildren(el, ctx, children);
    }
    if (node) {
      if (children.length) { node.children = children; }
      out.push(node);
    }
  }

  // describe returns the snapshot node of an exposed element, without
  // children, and hands out its ref.
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
    if (ref) {
      node.ref = _.refFor(el, role, name, ctx.alloc);
      ctx.refs.push({ ref: node.ref, role: role, name: name });
    }
    if (role === "link" && el.href) { node.url = String(el.href); }
    if (cursor) { node.cursor = { kind: cursor.kind, hints: cursor.hints, text: cursor.text }; }
    return node;
  }

  // walk returns the nodes of root and hands out refs from nextRef on.
  // Without a root, or with this document's body or html element, it walks
  // the page from the body. With another root it runs the page walk and
  // returns the root's part of it; a root that the page walk never enters
  // gives no nodes.
  function walk(root, nextRef) {
    var ctx = { alloc: { next: nextRef || 1 }, refs: [], frameDepth: 0, tree: [], emit: true, scope: null, done: false };
    if (root && root !== document.body && root !== document.documentElement) {
      ctx.emit = false;
      ctx.scope = root;
    }
    if (document.body) { walkChildren(document.body, ctx, ctx.tree); }
    var count = {};
    ctx.refs.forEach(function (r) { var key = r.role + "\u0000" + r.name; count[key] = (count[key] || 0) + 1; });
    ctx.refs.forEach(function (r) { r.unique = count[r.role + "\u0000" + r.name] === 1; });
    return { tree: ctx.tree, refs: ctx.refs, nextRef: Math.max(ctx.alloc.next, _.maxRef() + 1), doc: _.doc() };
  }

  I.def("snapshot", function (a, call) {
    var root = a.scope ? _.resolve(a.scope, call) : null;
    var res = walk(root, a.nextRef);
    res.url = String(location.href);
    res.title = String(document.title);
    return res;
  });
  _.walk = walk;
})(window.__iwdpBuilding);
