// 80-dialogs.js (W3b): the dialog override of design 3.7. Answers come
// from the daemon ("dialogs" issue); a dialog with no answer gets the
// session policy (D6: alert accepted, confirm false, prompt null; policy
// accept: confirm true, prompt its default). Each use is reported once,
// by the next "dialogs" call or the action that caused it, and logged with
// a marked console.debug message (telemetry, not proof).
(function (I) {
  "use strict";
  if (!I) { return; } // already installed (10-core.js)
  var _ = I._, w = window, dbg = console.debug.bind(console);
  var on = false, orig = null, ans = [], used = [], pol = "dismiss";
  function handle(type, msg, d) {
    var a = ans.shift(), acc = a ? a.accept : pol === "accept";
    var v = type === "confirm" ? acc : type === "prompt" ? (acc ? (a && a.text !== undefined ? a.text : d === undefined ? "" : String(d)) : null) : undefined;
    var u = { id: a ? a.id : "", type: type, message: String(msg === undefined ? "" : msg), result: v === undefined ? null : v };
    used.push(u);
    try { dbg("__iwdp_dialog__ " + JSON.stringify(u)); } catch (e) { /* console replaced by the page */ }
    return v;
  }
  function install(p) {
    pol = p;
    if (on) { return; }
    on = true;
    orig = { alert: w.alert, confirm: w.confirm, prompt: w.prompt };
    w.alert = function (m) { handle("alert", m); };
    w.confirm = function (m) { return handle("confirm", m); };
    w.prompt = function (m, d) { return handle("prompt", m, d); };
  }
  I.onReset(function () { ans = []; used = []; });
  I.onRelease(function () {
    if (on) { w.alert = orig.alert; w.confirm = orig.confirm; w.prompt = orig.prompt; }
    on = false;
  });
  _.dlgOn = function (p) { if (p) { install(p); } };
  _.dlgUsed = function () {
    var u = used;
    used = [];
    return u;
  };
  // dialogs: install with a policy, withdraw the unused answers, issue new
  // ones, report uses; nav or hist starts a navigation in the same call.
  I.def("dialogs", function (a) {
    if (a.policy) { install(a.policy); }
    var wd = a.withdraw ? ans.map(function (x) { return x.id; }) : [];
    if (a.withdraw) { ans = []; }
    (a.issue || []).forEach(function (x) { ans.push(x); });
    var r = { doc: _.doc(), used: _.dlgUsed(), pending: ans.map(function (x) { return x.id; }), withdrawn: wd, on: on };
    if (a.nav) { location.href = a.nav; }
    if (a.hist) { history.go(a.hist); }
    return r;
  });
})(window.__iwdpBuilding);
