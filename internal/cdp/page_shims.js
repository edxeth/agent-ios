// Page compatibility shims of CDP attachments (docs/cdp.md, "Page
// compatibility shims"). CDP clients believe they drive Chromium and call
// newer Web APIs without a check; Safari 16.6 (iOS 16.7) lacks some of
// them. Each shim is installed only when its API is missing, so a native
// implementation is never replaced, and its function carries the marker
// property __iwdpShim (true, not enumerable). The value of the script is
// the list of the shims it installed.
(function () {
  "use strict";
  var installed = [];
  var E = window.Element;

  // Element.prototype.checkVisibility (CSSOM View,
  // https://drafts.csswg.org/cssom-view/#dom-element-checkvisibility;
  // Safari 17.4). Playwright's injected script calls it on every
  // visibility check (locator actions, getByRole, ariaSnapshot).
  if (E && typeof E.prototype.checkVisibility !== "function") {
    // The shim keeps the built-ins it found, so a page script that
    // replaces them later does not change its answers.
    var getClientRects = E.prototype.getClientRects;
    var getComputedStyle = window.getComputedStyle;
    var ShadowRootType = window.ShadowRoot;
    var ObjectCtor = Object;

    // flatParent returns the parent element in the flat tree: the slot of a
    // slotted node, the host of a shadow root's child.
    var flatParent = function (el) {
      if (el.assignedSlot) {
        return el.assignedSlot;
      }
      var p = el.parentNode;
      if (ShadowRootType && p instanceof ShadowRootType) {
        return p.host;
      }
      return p && p.nodeType === 1 ? p : null;
    };
    var style = function (el) {
      return getComputedStyle.call(window, el);
    };
    var isHTML = function (el, name) {
      return el.localName === name && el.namespaceURI === "http://www.w3.org/1999/xhtml";
    };
    // noOwnBox reports elements that have client rects but no box of their
    // own in WebKit:
    // - the content of a closed <details> element (every child except its
    //   first <summary>, and their descendants): Safari 16 lays it out in
    //   a display:none slot of the element's user-agent shadow root (no
    //   box, so getClientRects already has no rects); later WebKit uses
    //   content-visibility:hidden on that slot (step 2) and still reports
    //   rects;
    // - the content of a <select> element (options and option groups):
    //   WebKit lays out the select itself (menu list or list box), and a
    //   list box reports its rows as the options' client rects.
    var noOwnBox = function (el) {
      for (var child = el, a = flatParent(el); a; child = a, a = flatParent(a)) {
        if (isHTML(a, "select")) {
          return true;
        }
        if (isHTML(a, "details") && !a.open) {
          var summary = null;
          for (var c = a.firstElementChild; c; c = c.nextElementSibling) {
            if (isHTML(c, "summary")) {
              summary = c;
              break;
            }
          }
          if (child !== summary) {
            return true;
          }
        }
      }
      return false;
    };

    var checkVisibility = function checkVisibility() {
      var options = arguments[0];
      // The receiver check comes first, as in a WebIDL operation:
      // getClientRects throws a TypeError for a this that is not an
      // Element.
      getClientRects.call(this);
      // WebIDL dictionary conversion, before the method's steps: undefined
      // and null are an empty dictionary; other non-objects are a
      // TypeError; each member is read once, in lexicographic order, and
      // converted with ToBoolean. A getter may change the layout, so the
      // box test below comes after the conversion. Object(x) === x is the
      // object test: typeof is "undefined" for the object document.all.
      if (options !== undefined && options !== null && ObjectCtor(options) !== options) {
        throw new TypeError("Failed to execute 'checkVisibility' on 'Element': The provided value is not of type 'CheckVisibilityOptions'.");
      }
      var o = options === undefined || options === null ? {} : options;
      var checkOpacity = !!o.checkOpacity;
      var checkVisibilityCSS = !!o.checkVisibilityCSS;
      void !!o.contentVisibilityAuto; // read as WebIDL does; nothing to check on Safari 16 (no content-visibility)
      var opacityProperty = !!o.opacityProperty;
      var visibilityProperty = !!o.visibilityProperty;
      // Step 1: no associated box. An element without one has no client
      // rects (display:none on the element or an ancestor,
      // display:contents, a detached element); noOwnBox adds the elements
      // that have rects without a box of their own.
      if (getClientRects.call(this).length === 0 || noOwnBox(this)) {
        return false;
      }
      // Steps 2 and 5 (content-visibility: hidden and auto): Safari 16 has
      // no content-visibility, so no ancestor skips its contents.
      // Step 3: computed opacity 0 on the element or a flat-tree ancestor.
      if (checkOpacity || opacityProperty) {
        for (var a = this; a; a = flatParent(a)) {
          if (Number(style(a).opacity) === 0) {
            return false;
          }
        }
      }
      // Step 4: the element is invisible (computed visibility is not
      // "visible").
      if ((checkVisibilityCSS || visibilityProperty) && style(this).visibility !== "visible") {
        return false;
      }
      return true;
    };
    Object.defineProperty(checkVisibility, "__iwdpShim", { value: true });
    // A WebIDL operation: writable, enumerable, configurable.
    Object.defineProperty(E.prototype, "checkVisibility", { value: checkVisibility, writable: true, enumerable: true, configurable: true });
    installed.push("checkVisibility");
  }
  return installed;
})();
