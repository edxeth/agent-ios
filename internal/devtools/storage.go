package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/nnemirovsky/iwdp-cli/internal/tools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// StorageEntry is one localStorage or sessionStorage item.
type StorageEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// storageFn runs one storage operation in the page (design 4.2: JS
// localStorage, no storage id needed) and returns its reply as JSON text.
// Ops: list, get, set and clear of one area ("local" or "session");
// snapshot of both areas; apply, one part of a state load (restore.go).
// Every reply has the page's origin.
//
// apply runs only while the page is at the expected origin. It runs its
// ops in order: a remove, a set, or a staged part for a key or a value
// larger than one message. A key part (kp) goes before the key of the next
// op that is not a key part; a value part (p) goes before the value of the
// next set of the same key. A remove or set whose key already holds the
// result is skipped, so only real changes reach the storage and fire
// storage events. The staged parts live in a page property that the first
// apply of a load creates with the load's token; a later apply that finds
// no property with that token (the page reloaded) changes nothing and
// replies with an error.
// When an op throws (the storage quota), the apply undoes its own changes
// in reverse order and replies with the error. The last apply of a load,
// and a failed one, remove the staged parts.
const storageFn = `function iwdpStorage(a) {
  var names = {local: "localStorage", session: "sessionStorage"};
  function items(s) {
    var out = [];
    for (var i = 0; i < s.length; i++) { var k = s.key(i); out.push([k, s.getItem(k)]); }
    return out;
  }
  var r = {origin: location.origin};
  var s = a.area ? window[names[a.area]] : null;
  switch (a.op) {
  case "list": r.items = items(s); break;
  case "get": var v = s.getItem(a.key); r.found = v !== null; r.value = v === null ? "" : v; break;
  case "set": s.setItem(a.key, a.value); break;
  case "clear": r.cleared = s.length; s.clear(); break;
  case "snapshot": r.local = items(localStorage); r.session = items(sessionStorage); break;
  case "apply":
    if (location.origin !== a.origin) { r.wrongOrigin = true; break; }
    var stage = window.__iwdpStateStage;
    if (a.first) {
      stage = {token: a.token, key: "", parts: {}};
      Object.defineProperty(window, "__iwdpStateStage", {value: stage, configurable: true, writable: true});
    } else if (!stage || stage.token !== a.token) {
      r.error = "the page was reloaded while the state loaded";
      break;
    }
    var undo = [];
    try {
      a.ops.forEach(function (op) {
        if (op.kp) { stage.key += op.k; return; }
        var st = window[names[op.a]], k = stage.key + op.k, id = op.a + ":" + k;
        if (op.p) { stage.parts[id] = (stage.parts[id] || "") + op.v; return; }
        stage.key = "";
        var old = st.getItem(k);
        if (op.v === undefined) {
          if (old !== null) { undo.push([st, k, old]); st.removeItem(k); }
          return;
        }
        var v = (stage.parts[id] || "") + op.v;
        delete stage.parts[id];
        if (old !== v) { undo.push([st, k, old]); st.setItem(k, v); }
      });
    } catch (e) {
      r.error = String(e);
      for (var i = undo.length - 1; i >= 0; i--) {
        var u = undo[i];
        try { if (u[2] === null) { u[0].removeItem(u[1]); } else { u[0].setItem(u[1], u[2]); } } catch (e2) { r.undoError = String(e2); }
      }
    }
    r.changed = undo.length;
    if (a.last || r.error) { delete window.__iwdpStateStage; }
    break;
  }
  return JSON.stringify(r);
}`

// storageArgs are the arguments of storageFn. Token, First, Last and Ops
// are those of apply.
type storageArgs struct {
	Op     string      `json:"op"`
	Area   string      `json:"area,omitempty"`
	Key    string      `json:"key,omitempty"`
	Value  string      `json:"value,omitempty"`
	Origin string      `json:"origin,omitempty"`
	Token  string      `json:"token,omitempty"`
	First  bool        `json:"first,omitempty"`
	Last   bool        `json:"last,omitempty"`
	Ops    []storageOp `json:"ops,omitempty"`
}

type storageReply struct {
	Origin      string      `json:"origin"`
	Items       [][2]string `json:"items"`
	Found       bool        `json:"found"`
	Value       string      `json:"value"`
	Cleared     int         `json:"cleared"`
	Local       [][2]string `json:"local"`
	Session     [][2]string `json:"session"`
	WrongOrigin bool        `json:"wrongOrigin"`
	Error       string      `json:"error"`     // a failed apply, which undid its own changes
	UndoError   string      `json:"undoError"` // an undo of a failed apply that failed too
	Changed     int         `json:"changed"`   // the items an apply changed
}

// storageCall evaluates storageFn with a in the page.
func storageCall(ctx context.Context, c webkit.Caller, a storageArgs) (storageReply, error) {
	if a.Area != "" && a.Area != "local" && a.Area != "session" {
		return storageReply{}, fmt.Errorf("storage area %q: want local or session", a.Area)
	}
	args, err := json.Marshal(a)
	if err != nil {
		return storageReply{}, err
	}
	res, err := tools.EvaluateScript(ctx, c, "("+storageFn+")("+string(args)+")", true)
	if err != nil {
		return storageReply{}, fmt.Errorf("storage %s in the page: %w", a.Op, err)
	}
	var text string
	if err := json.Unmarshal(res.Result.Value, &text); err != nil {
		return storageReply{}, fmt.Errorf("storage %s: the page returned %s, not JSON text", a.Op, res.Result.Type)
	}
	var r storageReply
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return storageReply{}, fmt.Errorf("storage %s: decoding the page's reply: %w", a.Op, err)
	}
	return r, nil
}

// entries turns page pairs into entries sorted by name (the storage order
// of a browser is not defined).
func entries(pairs [][2]string) []StorageEntry {
	out := make([]StorageEntry, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, StorageEntry{Name: p[0], Value: p[1]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StorageList returns the items of area ("local" or "session") sorted by
// name, and the page's origin.
func StorageList(ctx context.Context, c webkit.Caller, area string) (string, []StorageEntry, error) {
	r, err := storageCall(ctx, c, storageArgs{Op: "list", Area: area})
	return r.Origin, entries(r.Items), err
}

// StorageGet returns the value of key in area and whether it exists.
func StorageGet(ctx context.Context, c webkit.Caller, area, key string) (origin, value string, found bool, err error) {
	r, err := storageCall(ctx, c, storageArgs{Op: "get", Area: area, Key: key})
	return r.Origin, r.Value, r.Found, err
}

// StorageSet sets key to value in area.
func StorageSet(ctx context.Context, c webkit.Caller, area, key, value string) (string, error) {
	r, err := storageCall(ctx, c, storageArgs{Op: "set", Area: area, Key: key, Value: value})
	return r.Origin, err
}

// StorageClear removes every item of area and returns how many there were.
func StorageClear(ctx context.Context, c webkit.Caller, area string) (string, int, error) {
	r, err := storageCall(ctx, c, storageArgs{Op: "clear", Area: area})
	return r.Origin, r.Cleared, err
}
