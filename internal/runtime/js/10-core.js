// 10-core.js (W3a): window.__iwdp, the call contract, the owner epoch, the
// operation fence and the built-ins captured at install (design 3.1).
// Later files read the runtime under construction from
// window.__iwdpBuilding and add functions with I.def; the installer removes
// that property after the last file.
(function () {
  "use strict";
  var VERSION = "__IWDP_VERSION__";
  var w = window;
  var defProp = Object.defineProperty;
  defProp(w, "__iwdpBuilding", { value: null, configurable: true, writable: true, enumerable: false });
  var old = w.__iwdp;
  if (old && old.v === VERSION) {
    return; // installed: a second install changes nothing
  }

  // uncurry(f)(self, a, b) runs f with this = self, through the captured
  // Function.prototype.call, so later page overrides do not apply.
  var FnCall = Function.prototype.call;
  var FnBind = Function.prototype.bind;
  function uncurry(f) { return FnBind.call(FnCall, f); }
  function getter(proto, name) {
    var d = Object.getOwnPropertyDescriptor(proto, name);
    return d && d.get ? uncurry(d.get) : function (o) { return o[name]; };
  }
  var EP = Element.prototype, NP = Node.prototype, DP = Document.prototype;
  var perf = w.performance;
  var MapProto = Map.prototype, WMProto = WeakMap.prototype;
  var B = {
    gbcr: uncurry(EP.getBoundingClientRect),
    gcr: uncurry(EP.getClientRects),
    getAttr: uncurry(EP.getAttribute),
    hasAttr: uncurry(EP.hasAttribute),
    closest: uncurry(EP.closest),
    matches: uncurry(EP.matches),
    qsaEl: uncurry(EP.querySelectorAll),
    qsaDoc: uncurry(DP.querySelectorAll),
    qsaFrag: uncurry(DocumentFragment.prototype.querySelectorAll),
    byId: uncurry(DP.getElementById),
    fromPoint: uncurry(DP.elementFromPoint),
    shadowFromPoint: w.ShadowRoot && ShadowRoot.prototype.elementFromPoint ? uncurry(ShadowRoot.prototype.elementFromPoint) : null,
    gcs: uncurry(w.getComputedStyle),
    prop: uncurry(CSSStyleDeclaration.prototype.getPropertyValue),
    createRange: uncurry(DP.createRange),
    selectContents: uncurry(Range.prototype.selectNodeContents),
    rangeRect: uncurry(Range.prototype.getBoundingClientRect),
    connected: getter(NP, "isConnected"),
    textContent: getter(NP, "textContent"),
    innerText: getter(HTMLElement.prototype, "innerText"),
    innerHTML: getter(EP, "innerHTML"),
    shadowRoot: getter(EP, "shadowRoot"),
    ownerDoc: getter(NP, "ownerDocument"),
    defaultView: getter(DP, "defaultView"),
    contentDocument: getter(HTMLIFrameElement.prototype, "contentDocument"),
    assigned: w.HTMLSlotElement ? uncurry(HTMLSlotElement.prototype.assignedNodes) : null,
    now: function () { return perfNow(perf); },
    mapGet: uncurry(MapProto.get), mapSet: uncurry(MapProto.set), mapDelete: uncurry(MapProto.delete),
    mapForEach: uncurry(MapProto.forEach), mapSize: getter(MapProto, "size"),
    wmGet: uncurry(WMProto.get), wmSet: uncurry(WMProto.set), wmDelete: uncurry(WMProto.delete)
  };
  var perfNow = uncurry(Performance.prototype.now);
  var MapC = Map, WeakMapC = WeakMap;

  function randomId() {
    return "doc-" + Math.floor(Math.random() * 1e12).toString(36) + "-" + Math.floor(B.now() * 1000).toString(36);
  }
  var DOC = randomId();

  var fns = {};
  var owner = null;        // {daemon, session, epoch}
  var last = null;         // the latest owner, kept after release: the epoch fence
  var refusal = null;      // own's refusal of a stale owner, the result of the call that follows it
  var epochFloor = 0;      // highest attempt epoch seen without an owner
  var pendingReset = null; // reported with the next call's envelope
  var refs = new MapC();   // "e5" -> element
  var refInfo = new WeakMapC(); // element -> {ref, role, name}
  var maxRef = 0;          // highest ref this document handed out; survives resets
  var resetBelow = 0;      // refs up to this number were dropped by the last reset
  var resetReason = "";    // why: daemon, session, epoch, release or reset
  var canceled = {}, lastStep = {};
  var resetHooks = [], releaseHooks = [];

  function fail(code, message, details) {
    return { __iwdpError: true, code: code, message: message, details: details };
  }
  function errEnvelope(e) {
    if (e && e.__iwdpError) {
      var err = { code: e.code, message: e.message };
      if (e.details !== undefined) { err.details = e.details; }
      return { ok: false, error: err };
    }
    return { ok: false, error: { code: "internal", message: "runtime error in the page: " + String(e && e.message ? e.message : e) } };
  }
  function runHooks(list) {
    for (var i = 0; i < list.length; i++) {
      try { list[i](); } catch (e) { /* one failing hook must not stop the others */ }
    }
  }

  // reset clears the page maps. Every ref handed out so far belongs to the
  // owner before the reset, so the resolver reports it as unknown instead
  // of finding its element again by role and name (design 3.1, 3.3).
  function reset(reason) {
    refs = new MapC();
    refInfo = new WeakMapC();
    canceled = {};
    lastStep = {};
    resetBelow = maxRef;
    resetReason = reason;
    runHooks(resetHooks);
  }

  function sameOwner(a, b) {
    return a.daemon === b.daemon && a.session === b.session && a.epoch === b.epoch;
  }

  // own(o) makes o the owner (design 3.1). The owned call expression runs
  // own and then call, so a refusal set here is the result of that call.
  // Lease epochs only grow within one daemon: an owner of the latest
  // owner's daemon with an older epoch is a delayed message of an earlier
  // lease, refused without any change, also after a release. Another owner
  // resets the page maps and reports the reset with the next envelope, also
  // when a release already cleared them, so its ref table forgets the refs
  // of the earlier owner.
  function own(o) {
    refusal = null;
    if (!o || typeof o !== "object") { return true; }
    var next = { daemon: String(o.daemon), session: String(o.session), epoch: Number(o.epoch) };
    if (owner && sameOwner(owner, next)) { return true; }
    var prev = owner || last;
    if (prev && prev.daemon === next.daemon && next.epoch < prev.epoch) {
      refusal = fail("stale_owner", "lease epoch " + next.epoch + " of session \"" + next.session + "\" is older than epoch " +
        prev.epoch + " of the latest owner of this page; acquire the page again");
      return true;
    }
    if (prev && !sameOwner(prev, next)) {
      var reason = prev.daemon !== next.daemon ? "daemon" : (prev.session !== next.session ? "session" : "epoch");
      reset(reason);
      pendingReset = { reason: reason, maxRef: maxRef };
    }
    owner = last = next;
    return true;
  }

  // release ends the ownership of epoch; the owner stays the epoch fence of
  // own (last).
  function release(epoch) {
    if (owner && epoch !== undefined && epoch !== null && owner.epoch !== Number(epoch)) { return false; }
    runHooks(releaseHooks);
    reset("release");
    owner = null;
    pendingReset = null;
    return true;
  }

  function call(fn, args) {
    var refused = refusal;
    refusal = null;
    if (refused) { return errEnvelope(refused); } // a pending reset stays for the owner's next call
    var resetInfo = pendingReset;
    pendingReset = null;
    var warnings = [];
    function done(env) {
      if (warnings.length) { env.warnings = warnings; }
      if (resetInfo) { env.reset = resetInfo; }
      return env;
    }
    var ctx = { warn: function (m) { warnings.push(String(m)); } };
    try {
      var f = Object.prototype.hasOwnProperty.call(fns, fn) ? fns[fn] : null;
      if (typeof f !== "function") { throw fail("unsupported", "the agent runtime has no function " + fn); }
      var v = f(args === undefined || args === null ? {} : args, ctx);
      if (v && typeof v.then === "function") {
        return v.then(function (x) { return done({ ok: true, value: x === undefined ? null : x }); },
          function (e) { return done(errEnvelope(e)); });
      }
      return done({ ok: true, value: v === undefined ? null : v });
    } catch (e) {
      return done(errEnvelope(e));
    }
  }

  function epochCurrent(epoch) {
    return owner ? owner.epoch === epoch : epochFloor === epoch;
  }

  // attempt runs one attempt of an action step under the fence of design
  // 2.6: nothing happens for a canceled operation, another document, an
  // older epoch, a passed attempt end or deadline, or any step but the
  // next one. check(args) returns "" when ready or the reason to retry;
  // dispatch(ctx, args) runs the step's events and calls ctx.live() before
  // each one (false: stop and ctx.interrupt(reason)).
  function attempt(a, check, dispatch) {
    function reply(status, reason, value) {
      var r = { status: status, reason: reason || "", now: B.now(), doc: DOC };
      if (value !== undefined) { r.value = value; }
      return r;
    }
    if (!a || typeof a.opId !== "string") { throw fail("internal", "attempt without fence arguments"); }
    if (canceled[a.opId]) { return reply("refused", "canceled"); }
    if (a.doc !== DOC) { return reply("refused", "other document"); }
    var floor = owner ? owner.epoch : epochFloor;
    if (a.epoch < floor) { return reply("refused", "other epoch"); }
    if (a.epoch > floor) {
      if (owner) {
        reset("epoch");
        pendingReset = { reason: "epoch", maxRef: maxRef };
        owner = last = { daemon: owner.daemon, session: owner.session, epoch: a.epoch };
      } else {
        epochFloor = a.epoch;
      }
    }
    var now = B.now();
    if (now >= a.attemptEnd || now >= a.deadline) { return reply("expired"); }
    if (a.step !== (lastStep[a.opId] || 0) + 1) { return reply("refused", "not the next step"); }
    var why = check ? check(a.args) : "";
    if (why) { return reply("retry", why); }
    var settled = false;
    var ctx = {
      interrupted: "",
      live: function () {
        var t = B.now();
        return !settled && !canceled[a.opId] && t < a.attemptEnd && t < a.deadline && epochCurrent(a.epoch);
      },
      interrupt: function (reason) { ctx.interrupted = reason || "attempt end"; }
    };
    lastStep[a.opId] = a.step; // from here on the step counts as run, so it never runs twice
    var v;
    try {
      v = dispatch(ctx, a.args);
      if (v && typeof v.then === "function") {
        return v.then(function (x) {
          settled = true;
          return ctx.interrupted ? reply("interrupted", ctx.interrupted) : reply("dispatched", "", x);
        }, function (e) { settled = true; throw e; });
      }
    } catch (e) {
      settled = true; // the call settles with this exception: no event of the step may follow
      throw e;
    }
    settled = true;
    return ctx.interrupted ? reply("interrupted", ctx.interrupted) : reply("dispatched", "", v);
  }

  // refFor gives el a ref: its old one when role and name are unchanged
  // (design 3.3), else e<n> with n = max(alloc.next, maxRef + 1).
  function refFor(el, role, name, alloc) {
    var info = B.wmGet(refInfo, el);
    if (info && info.role === role && info.name === name && B.mapGet(refs, info.ref) === el) { return info.ref; }
    if (info && B.mapGet(refs, info.ref) === el) { B.mapDelete(refs, info.ref); }
    var n = Math.max(alloc.next, maxRef + 1);
    alloc.next = n + 1;
    maxRef = n;
    var ref = "e" + n;
    B.mapSet(refs, ref, el);
    B.wmSet(refInfo, el, { ref: ref, role: role, name: name });
    return ref;
  }

  var I = {};
  function def(name, fn) {
    if (typeof fn !== "function") { throw new TypeError("__iwdp.def(" + name + "): not a function"); }
    fns[name] = fn;
  }
  var api = {
    v: VERSION,
    call: call,
    def: def,
    own: own,
    reset: function () { reset("reset"); return true; },
    release: release,
    attempt: attempt,
    onReset: function (f) { resetHooks.push(f); },
    onRelease: function (f) { releaseHooks.push(f); }
  };
  for (var k in api) {
    if (Object.prototype.hasOwnProperty.call(api, k)) { defProp(I, k, { value: api[k], enumerable: false }); }
  }
  // Shared internals for the later files of the bundle.
  var internals = {
    B: B, fail: fail, refFor: refFor,
    doc: function () { return DOC; },
    refGet: function (ref) { return B.mapGet(refs, ref); },
    rebind: function (ref, el, role, name) { B.mapSet(refs, ref, el); B.wmSet(refInfo, el, { ref: ref, role: role, name: name }); },
    maxRef: function () { return maxRef; },
    // resetFrom(n): why the last reset dropped ref e<n>, or "" when it did not.
    resetFrom: function (n) { return n <= resetBelow ? resetReason : ""; }
  };
  defProp(I, "_", { value: internals, enumerable: false });

  def("ping", function () { return true; });
  def("clock", function () { return { now: B.now(), doc: DOC }; });
  def("cancel", function (a) { canceled[String(a.opId)] = true; return null; });
  def("release", function (a) { return release(a.epoch); });
  def("reset", function () { reset("reset"); return true; });
  def("stats", function () {
    return { refs: B.mapSize(refs), maxRef: maxRef, owner: owner, doc: DOC, v: VERSION };
  });

  if (old && typeof old.release === "function") {
    try { old.release(); } catch (e) { /* an older runtime that cannot release still gets replaced */ }
  }
  defProp(w, "__iwdp", { value: I, configurable: true, writable: true, enumerable: false });
  w.__iwdpBuilding = I;
})();
