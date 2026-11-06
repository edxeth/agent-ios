// 20-visibility.js (W3a): the runtime's own visibility rule (design 3.4),
// used on every WebKit version instead of Element.checkVisibility, which
// Safari 16.6 lacks. Rules follow Playwright (domUtils.ts).
(function (I) {
  "use strict";
  if (!I) { return; }
  var _ = I._, B = _.B;

  function style(el) {
    var view = el.ownerDocument && el.ownerDocument.defaultView;
    try { return B.gcs(view || window, el); } catch (e) { return null; }
  }
  function css(el, name) {
    var s = style(el);
    return s ? B.prop(s, name) : "";
  }
  // flatParent returns the parent element in the flat tree: the slot of a
  // slotted node, the host of a shadow root's child.
  function flatParent(n) {
    if (n.assignedSlot) { return n.assignedSlot; }
    var p = n.parentNode;
    if (p && p.nodeType === 11 && p.host) { return p.host; }
    return p && p.nodeType === 1 ? p : null;
  }
  // styleHidden: display none on the element or a flat-tree ancestor, or
  // content-visibility hidden on an ancestor.
  function styleHidden(el) {
    for (var n = el, self = true; n; n = flatParent(n), self = false) {
      var s = style(n);
      if (!s) { return true; }
      if (B.prop(s, "display") === "none") { return true; }
      if (!self && B.prop(s, "content-visibility") === "hidden") { return true; }
    }
    return false;
  }
  // inClosedDetails: content of a closed <details> other than its first
  // <summary>. WebKit hides it in the details element's user-agent shadow
  // root, which a page script cannot walk (assignedSlot is null), and
  // getBoundingClientRect may still report a box. Playwright's fallback
  // for engines without checkVisibility checks <details> the same way
  // (domUtils.ts isElementStyleVisibilityVisible).
  function inClosedDetails(el) {
    for (var child = el, a = flatParent(el); a; child = a, a = flatParent(a)) {
      if (a.localName === "details" && !a.open) {
        var first = null;
        for (var c = a.firstElementChild; c; c = c.nextElementSibling) {
          if (c.localName === "summary") { first = c; break; }
        }
        if (child !== first) { return true; }
      }
    }
    return false;
  }
  function textVisible(t) {
    var doc = t.ownerDocument;
    var r = B.createRange(doc);
    B.selectContents(r, t);
    var b = B.rangeRect(r);
    return b.width > 0 && b.height > 0;
  }
  function visible(el) {
    if (!el || !B.connected(el)) { return false; }
    var tag = el.localName;
    if (tag === "option" || tag === "optgroup") {
      var sel = B.closest(el, "select");
      if (sel && (sel.multiple || sel.size > 1)) { el = sel; }
    }
    if (inClosedDetails(el)) { return false; }
    if (css(el, "display") === "contents") {
      if (styleHidden(el)) { return false; }
      for (var c = el.firstChild; c; c = c.nextSibling) {
        if (c.nodeType === 1 && visible(c)) { return true; }
        if (c.nodeType === 3 && textVisible(c)) { return true; }
      }
      return false;
    }
    if (styleHidden(el)) { return false; }
    var vis = css(el, "visibility");
    if (vis === "hidden" || vis === "collapse") { return false; }
    if (B.gcr(el).length === 0) { return false; }
    var r = B.gbcr(el);
    return r.width > 0 && r.height > 0;
  }
  _.style = style;
  _.css = css;
  _.flatParent = flatParent;
  _.visible = visible;
})(window.__iwdpBuilding);
