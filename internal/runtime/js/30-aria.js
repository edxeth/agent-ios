// 30-aria.js (W3a): a compact HTML-AAM implicit role table, the
// accessible-name steps that matter for agents and the ARIA states of the
// snapshot (design 3.2). Playwright's roleUtils.ts is the reference for
// the rules; no code is copied from it.
(function (I) {
  "use strict";
  if (!I) { return; }
  var _ = I._, B = _.B;

  function attr(el, n) { return B.getAttr(el, n); }
  function has(el, n) { return B.hasAttr(el, n); }
  function norm(s) { return String(s || "").replace(/\s+/g, " ").trim(); }

  var KNOWN = ("alert alertdialog application article banner blockquote button caption cell checkbox code columnheader " +
    "combobox complementary contentinfo definition dialog directory document feed figure form generic grid gridcell group " +
    "heading img link list listbox listitem log main marquee math menu menubar menuitem menuitemcheckbox menuitemradio " +
    "meter navigation none note option paragraph presentation progressbar radio radiogroup region row rowgroup rowheader " +
    "scrollbar search searchbox separator slider spinbutton status switch tab table tablist tabpanel term textbox timer " +
    "toolbar tooltip tree treegrid treeitem").split(" ");
  var known = {};
  for (var i = 0; i < KNOWN.length; i++) { known[KNOWN[i]] = true; }

  // Elements without a role whose children take their place (phrasing
  // content: their text joins the surrounding text, as in Chrome).
  var TRANSPARENT = {};
  ("a abbr b bdi bdo cite data em font i kbd mark q s samp small span strong sub sup time u var wbr").split(" ")
    .forEach(function (t) { TRANSPARENT[t] = true; });

  function landmarkScoped(el) {
    return !B.closest(el.parentElement || el, "article, aside, main, nav, section") || el.parentElement === null;
  }

  function inputRole(el) {
    var t = (attr(el, "type") || "text").toLowerCase();
    var list = has(el, "list");
    switch (t) {
    case "hidden": return null;
    case "button": case "submit": case "reset": case "image": case "file": case "color": return "button";
    case "checkbox": return "checkbox";
    case "radio": return "radio";
    case "range": return "slider";
    case "number": return "spinbutton";
    case "search": return list ? "combobox" : "searchbox";
    case "email": case "tel": case "text": case "url": return list ? "combobox" : "textbox";
    }
    return "textbox";
  }

  // implicitRole returns the HTML-AAM role, "generic" for elements that
  // show up as a group, or "" for transparent elements.
  function implicitRole(el) {
    var t = el.localName;
    switch (t) {
    case "a": case "area": return has(el, "href") ? "link" : "";
    case "article": return "article";
    case "aside": return "complementary";
    case "button": case "summary": return "button";
    case "datalist": return "listbox";
    case "details": case "fieldset": case "optgroup": return "group";
    case "dialog": return "dialog";
    case "figure": return "figure";
    case "footer": return landmarkScoped(el) ? "contentinfo" : "generic";
    case "header": return landmarkScoped(el) ? "banner" : "generic";
    case "form": return name(el, "form") ? "form" : "generic";
    case "section": return name(el, "region") ? "region" : "generic";
    case "h1": case "h2": case "h3": case "h4": case "h5": case "h6": return "heading";
    case "hr": return "separator";
    case "img": return attr(el, "alt") === "" ? "presentation" : "img";
    case "input": return inputRole(el);
    case "li": return "listitem";
    case "main": return "main";
    case "menu": case "ol": case "ul": return "list";
    case "nav": return "navigation";
    case "option": return "option";
    case "output": return "status";
    case "p": return "paragraph";
    case "progress": return "progressbar";
    case "select": return (el.multiple || el.size > 1) ? "listbox" : "combobox";
    case "table": return "table";
    case "tbody": case "thead": case "tfoot": return "rowgroup";
    case "td": return "cell";
    case "th": return attr(el, "scope") === "row" ? "rowheader" : "columnheader";
    case "tr": return "row";
    case "textarea": return "textbox";
    case "iframe": return "Iframe";
    case "blockquote": return "blockquote";
    }
    return TRANSPARENT[t] ? "" : "generic";
  }

  // role returns the explicit or implicit role; "presentation" and ""
  // mean that the element itself is not exposed.
  function role(el) {
    var r = attr(el, "role");
    if (r) {
      var parts = r.trim().toLowerCase().split(/\s+/);
      for (var i = 0; i < parts.length; i++) {
        if (known[parts[i]]) { return parts[i] === "none" ? "presentation" : parts[i]; }
      }
    }
    return implicitRole(el);
  }

  var FROM_CONTENT = {};
  ("button cell checkbox columnheader gridcell heading link menuitem menuitemcheckbox menuitemradio option radio row " +
    "rowheader switch tab tooltip treeitem").split(" ").forEach(function (r) { FROM_CONTENT[r] = true; });

  function hiddenForName(el) {
    if (attr(el, "aria-hidden") === "true" || has(el, "hidden")) { return true; }
    var t = el.localName;
    if (t === "script" || t === "style" || t === "template" || t === "noscript") { return true; }
    return _.css(el, "display") === "none";
  }

  // flatChildren returns the children of node in the flat tree: the
  // children of its open shadow root, the nodes assigned to a slot (or the
  // slot's fallback content), else its child nodes.
  function flatChildren(node) {
    if (node.nodeType === 1) {
      var sr = B.shadowRoot(node);
      if (sr) { return sr.childNodes; }
      if (node.localName === "slot" && B.assigned) {
        var a = B.assigned(node, { flatten: true });
        if (a.length) { return a; }
      }
    }
    return node.childNodes;
  }

  // textOf is the text alternative of a subtree in the flat tree: text, alt
  // text and the values of embedded controls; skip is a control to leave
  // out (the control a label names).
  function textOf(node, skip) {
    var out = "";
    var kids = flatChildren(node);
    for (var i = 0; i < kids.length; i++) {
      var c = kids[i];
      if (c.nodeType === 3) { out += c.data; continue; }
      if (c.nodeType !== 1 || c === skip || hiddenForName(c)) { continue; }
      var t = c.localName;
      var piece;
      if (attr(c, "aria-label")) { piece = attr(c, "aria-label"); }
      else if (t === "img" || (t === "input" && attr(c, "type") === "image")) { piece = attr(c, "alt") || ""; }
      else if (t === "input" || t === "textarea") { piece = c.value || ""; }
      else if (t === "select") { piece = c.selectedOptions && c.selectedOptions.length ? c.selectedOptions[0].text : ""; }
      else { piece = textOf(c, skip); }
      var display = _.css(c, "display");
      out += (display && display.indexOf("inline") !== 0 && display !== "contents") ? " " + piece + " " : piece;
    }
    return out;
  }

  function byIds(el, ids) {
    var root = el.getRootNode ? el.getRootNode() : el.ownerDocument;
    var out = [];
    var list = ids.trim().split(/\s+/);
    for (var i = 0; i < list.length; i++) {
      var t = root.getElementById ? root.getElementById(list[i]) : B.byId(el.ownerDocument, list[i]);
      if (t) { out.push(t); }
    }
    return out;
  }

  // labelText is the name from labels only: aria-labelledby, aria-label
  // and <label> (the "label=" locator matches it).
  function labelText(el) {
    var lb = attr(el, "aria-labelledby");
    if (lb) {
      var parts = byIds(el, lb).map(function (t) { return norm(attr(t, "aria-label") || textOf(t)); });
      var s = norm(parts.join(" "));
      if (s) { return s; }
    }
    var al = norm(attr(el, "aria-label"));
    if (al) { return al; }
    if (el.labels && el.labels.length) {
      var texts = [];
      for (var i = 0; i < el.labels.length; i++) { texts.push(norm(textOf(el.labels[i], el))); }
      return norm(texts.join(" "));
    }
    return "";
  }

  // name returns the accessible name of el with role r.
  function name(el, r) {
    if (r === undefined) { r = role(el); }
    var s = labelText(el);
    if (s) { return s; }
    var t = el.localName;
    if (t === "input") {
      var type = (attr(el, "type") || "text").toLowerCase();
      if (type === "submit" || type === "reset" || type === "button") {
        return norm(el.value) || (type === "submit" ? "Submit" : type === "reset" ? "Reset" : "");
      }
      if (type === "image") { return norm(attr(el, "alt")) || norm(attr(el, "title")) || "Submit"; }
    }
    if (t === "img" || t === "area") { s = norm(attr(el, "alt")); if (s) { return s; } }
    if (t === "fieldset" || t === "figure" || t === "table") {
      var cap = el.querySelector(t === "fieldset" ? "legend" : t === "figure" ? "figcaption" : "caption");
      if (cap) { s = norm(textOf(cap)); if (s) { return s; } }
    }
    if (FROM_CONTENT[r]) {
      s = norm(textOf(el));
      if (s) { return s; }
    }
    s = norm(attr(el, "title"));
    if (s) { return s; }
    if (t === "input" || t === "textarea") { return norm(attr(el, "placeholder")); }
    return "";
  }

  var CHECKABLE = { checkbox: 1, radio: 1, "switch": 1, menuitemcheckbox: 1, menuitemradio: 1 };

  // states returns the snapshot states of el with role r.
  function states(el, r) {
    var st = {};
    var t = el.localName;
    if (CHECKABLE[r]) {
      if (t === "input" && (el.type === "checkbox" || el.type === "radio")) {
        st.checked = el.indeterminate ? "mixed" : String(el.checked);
      } else {
        var ac = attr(el, "aria-checked");
        st.checked = ac === "true" || ac === "mixed" ? ac : "false";
      }
    }
    var ae = attr(el, "aria-expanded");
    if (ae === "true" || ae === "false") { st.expanded = ae === "true"; }
    else if (t === "select" && r === "combobox") { st.expanded = false; }
    else if (t === "summary" && el.parentElement && el.parentElement.localName === "details") { st.expanded = !!el.parentElement.open; }
    if (t === "option" && el.selected) { st.selected = true; }
    else if (attr(el, "aria-selected") === "true") { st.selected = true; }
    if (disabled(el)) { st.disabled = true; }
    if ((el.required && (t === "input" || t === "select" || t === "textarea")) || attr(el, "aria-required") === "true") { st.required = true; }
    if (r === "heading") {
      var lvl = parseInt(attr(el, "aria-level"), 10);
      st.level = lvl > 0 ? lvl : (/^h[1-6]$/.test(t) ? Number(t.charAt(1)) : 2);
    } else if (has(el, "aria-level")) {
      var l2 = parseInt(attr(el, "aria-level"), 10);
      if (l2 > 0) { st.level = l2; }
    }
    var v = value(el, r);
    if (v) { st.value = v; }
    return st;
  }

  function value(el, r) {
    var t = el.localName;
    if (r === "textbox" || r === "searchbox" || (r === "combobox" && t !== "select")) {
      if (t === "input" || t === "textarea") {
        var v = String(el.value || "");
        return el.type === "password" ? v.replace(/./g, "\u2022") : v;
      }
      return "";
    }
    if (t === "select" && r === "combobox") {
      return el.selectedOptions && el.selectedOptions.length ? norm(el.selectedOptions[0].text) : "";
    }
    if (r === "slider" || r === "spinbutton" || r === "progressbar" || r === "meter" || r === "scrollbar") {
      var vt = attr(el, "aria-valuetext") || attr(el, "aria-valuenow");
      if (vt) { return norm(vt); }
      return t === "input" || t === "progress" ? String(el.value) : "";
    }
    return "";
  }

  // disabled: native :disabled (including a disabled fieldset),
  // aria-disabled on the element or an ancestor, or an inert subtree.
  function disabled(el) {
    try { if (B.matches(el, ":disabled")) { return true; } } catch (e) { /* not a form element */ }
    return !!B.closest(el, "[aria-disabled=true], [inert]");
  }

  _.norm = norm;
  _.role = role;
  _.name = name;
  _.labelText = labelText;
  _.states = states;
  _.disabled = disabled;
  _.textOf = textOf;
  _.flatChildren = flatChildren;
})(window.__iwdpBuilding);
