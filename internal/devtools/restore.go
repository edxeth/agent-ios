package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-mcp/internal/logcause"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// The storage part of "state load" (W4 repair rounds 3 and 4, storage
// review findings storage.go:46, state.go:178, restore.go:138,
// restore.go:333 and state.go:110):
//
//   - Validation comes before any write: an area of the file larger than
//     storageRestoreLimit in WebKit's byte count (webkitBytes) is refused,
//     so a load that the storage quota would stop writes nothing and fires
//     no storage event in other documents of the origin.
//   - Only the difference is written: the keys that the file lacks are
//     removed and the keys whose value differs are set; nothing is cleared.
//     The page skips a write whose key already holds the result, so other
//     documents see storage events only for real changes.
//   - The writes are ordered so that the area never holds more bytes than
//     the larger of its size before and after the load (planArea); both
//     fit the quota, so no write in between reaches it.
//   - The writes are sent in several Runtime.evaluate calls (applies of
//     storageFn), each below restoreWireBudget on the wire; a key or a
//     value larger than one call is sent in parts that the page joins. So
//     every state that passes the validation loads, whatever the
//     transport's message limit.
//   - When a write still fails (the storage refuses it, the page reloads,
//     the connection fails), the earlier values of every key that the load
//     may have changed are put back, in planArea's order from the storage
//     as it is then (undoApplies). Other documents of the origin can then
//     see storage events for the failed writes and for the undo.

const (
	// storageQuotaBytes is WebKit's quota of one storage area of one
	// origin, in bytes. localStorage: localStorageQuotaInBytes (5 MB) in
	// Source/WebKit/NetworkProcess/storage/LocalStorageManager.cpp and
	// localStorageDatabaseQuotaInBytes (5 * 1024 * 1024) in
	// Source/WebCore/storage/StorageNamespaceProvider.cpp. sessionStorage:
	// SessionStorageQuota, default 5242880, in
	// Source/WebCore/page/Settings.yaml (WebKit commit f20844f96faf). Both
	// count keys and values in bytes (webkitBytes).
	storageQuotaBytes = 5 * 1024 * 1024
	// storageRestoreLimit is the largest area that a state load writes, in
	// WebKit's byte count of its keys and values (webkitBytes). It keeps
	// 10% of the quota free for a WebKit release or port that counts a
	// little differently, and for values that the page stored as 16-bit
	// strings although they hold only Latin-1 characters (webkitBytes).
	storageRestoreLimit = storageQuotaBytes * 9 / 10

	// restoreWireBudget bounds the serialized message of one apply. It is
	// below the smallest MaxOutgoing of the transports
	// (transport.WebKitGTKMaxOutgoing; iwdp and the direct transport accept
	// more), so one plan works on every transport. restoreWireReserve is
	// the part of the message that applyWire does not count: the other
	// Runtime.evaluate parameters, the message id and method, and the
	// Target.sendMessageToTarget envelope with its target id (well below
	// 1 KiB together).
	restoreWireBudget  = transport.WebKitGTKMaxOutgoing - restoreWireReserve
	restoreWireReserve = 4096
)

// storageOp is one op of an apply (storageFn). With KeyPart it is a
// staged part of the key of the next op, which the page puts before that
// op's Key. Else with Part a staged part of the value of the next set of
// the same key, with Value nil a remove, and else a set.
type storageOp struct {
	Area    string  `json:"a"`
	Key     string  `json:"k"`
	Value   *string `json:"v,omitempty"`
	Part    bool    `json:"p,omitempty"`
	KeyPart bool    `json:"kp,omitempty"`
}

// latin1Max is the largest character of an 8-bit WebKit string.
const latin1Max = 0xFF

// webkitBytes is the size that WebKit counts for s against the storage
// quota. StorageMap::setItem (Source/WebCore/storage/StorageMap.cpp:91-118,
// WebKit commit f20844f96faf) adds the bytes of a new key and its value,
// or the bytes of the new value minus the old value's; removeItem (lines
// 130-150) subtracts the key's and the value's. The bytes of a string are
// String::sizeInBytes (Source/WTF/wtf/text/WTFString.h:127): its length in
// UTF-16 code units, times 1 for an 8-bit (Latin-1) string and 2 for a
// 16-bit one. webkitBytes counts a string whose characters all fit
// Latin-1 as 8-bit, as the strings that a state load writes are: the
// WebKitGTK test TestDevtoolsStateLoadCountsLatin1AsOneByte loads Latin-1
// text that fits the quota only at one byte per character.
//
// A value that page script stored as a 16-bit string although it holds
// only Latin-1 characters counts 2 bytes per character in WebKit (for
// localStorage until WebKit reads it again from its database:
// Source/WebCore/platform/sql/SQLiteStatement.cpp:267-281 reads with
// create8BitIfPossible), and webkitBytes counts it at 1. When a load keeps
// such a value, the area is larger in WebKit than checkStorageSize counts;
// when a load replaces it, a set that grows the area can run before the
// set that frees its extra bytes. The removes and the sets that do not
// grow the area stay safe: they free at least what planArea counts.
// storageRestoreLimit keeps 10% of the quota free for this difference.
func webkitBytes(s string) int {
	units, wide := 0, false
	for _, r := range s {
		units += utf16.RuneLen(r)
		wide = wide || r > latin1Max
	}
	if wide {
		return 2 * units
	}
	return units
}

// lastWins returns the items of a storage list as a map; of two items with
// the same name the later one counts, as with setItem in order.
func lastWins(es []StorageEntry) map[string]string {
	m := make(map[string]string, len(es))
	for _, e := range es {
		m[e.Name] = e.Value
	}
	return m
}

// checkStorageSize refuses a state whose localStorage or sessionStorage is
// larger than storageRestoreLimit in WebKit's byte count.
func checkStorageSize(o StateOrigin) error {
	for _, a := range []struct {
		name string
		list []StorageEntry
	}{{"localStorage", o.LocalStorage}, {"sessionStorage", o.SessionStorage}} {
		n := 0
		for k, v := range lastWins(a.list) {
			n += webkitBytes(k) + webkitBytes(v)
		}
		if n > storageRestoreLimit {
			return fmt.Errorf("the %s of %s counts %d bytes in WebKit's storage quota (keys and values: 1 byte per character of Latin-1 text, 2 bytes per UTF-16 code unit of other text); a state load writes at most %d bytes, 90%% of the quota of 5 MiB per origin and area, so that the quota cannot stop it part-way",
				a.name, o.Origin, n, storageRestoreLimit)
		}
	}
	return nil
}

// planArea returns the ops that make area hold want instead of before, in
// an order that keeps the area's size in WebKit's count (webkitBytes) at
// or below the larger of its size before and after: the removes of the
// keys that want lacks, then the sets that do not make the area larger,
// then the sets that do (new keys and larger values), each group sorted by
// key. Each op of the first two groups makes the area smaller or keeps its
// size, and each op of the last makes it larger, so the size falls to its
// lowest and then rises to the size of want.
func planArea(area string, before [][2]string, want []StorageEntry) []storageOp {
	old := map[string]string{}
	for _, p := range before {
		old[p[0]] = p[1]
	}
	next := lastWins(want)
	var removes, shrinks, grows []storageOp
	for k := range old {
		if _, ok := next[k]; !ok {
			removes = append(removes, storageOp{Area: area, Key: k})
		}
	}
	for k, v := range next {
		ov, had := old[k]
		growth := webkitBytes(v) - webkitBytes(ov) // a replaced value (StorageMap::setItem)
		if !had {
			growth = webkitBytes(k) + webkitBytes(v) // a new key and its value
		}
		switch {
		case had && ov == v:
		case growth <= 0:
			shrinks = append(shrinks, storageOp{Area: area, Key: k, Value: &v})
		default:
			grows = append(grows, storageOp{Area: area, Key: k, Value: &v})
		}
	}
	var ops []storageOp
	for _, g := range [][]storageOp{removes, shrinks, grows} {
		sort.Slice(g, func(i, j int) bool { return g[i].Key < g[j].Key })
		ops = append(ops, g...)
	}
	return ops
}

// escaped is s as the body of a JSON string (json.Marshal without the
// quotes).
func escaped(s string) string {
	b, _ := json.Marshal(s) // a string always marshals
	return string(b[1 : len(b)-1])
}

// applyWire is the number of bytes that the text t of an apply's
// expression adds to its wire message: Runtime.evaluate's params hold the
// expression as a JSON string, and a Target-routed transport holds that
// message as a JSON string again. JSON escapes each character on its own,
// so the wire size of a text is the sum of the wire sizes of its parts.
func applyWire(t string) int { return len(escaped(escaped(t))) }

// asciiValueWire holds valueWire of the ASCII characters.
var asciiValueWire = func() (w [utf8.RuneSelf]int) {
	for r := range w {
		w[r] = applyWire(escaped(string(rune(r))))
	}
	return w
}()

// valueWire is the wire size of r in a string of the apply's argument,
// which the argument's JSON escapes once more.
func valueWire(r rune) int {
	if r < utf8.RuneSelf {
		return asciiValueWire[r]
	}
	if r == 0x2028 || r == 0x2029 || r == utf8.RuneError {
		return applyWire(escaped(string(r))) // JSON escapes these
	}
	return utf8.RuneLen(r)
}

func opWire(op storageOp) int {
	b, _ := json.Marshal(op) // a storageOp always marshals
	return applyWire(string(b) + ",")
}

// maxValueWire is the largest valueWire of one character ("\u2028" and
// the escaped control characters).
const maxValueWire = 16

// prefix returns the length in bytes of the longest prefix of s whose
// characters take at most budget bytes on the wire (valueWire), and their
// wire size.
func prefix(s string, budget int) (n, used int) {
	for i, r := range s {
		rw := valueWire(r)
		if used+rw > budget {
			return i, used
		}
		used += rw
	}
	return len(s), used
}

// planApplies splits ops into the ops of the applies of one load, each
// below restoreWireBudget on the wire. An op that does not fit in one
// apply is split at character boundaries: its key into key parts, then its
// value into value parts, and the op itself keeps an empty key and the
// value's last part; the page joins the parts (storageFn). So a key or a
// value of any length loads.
func planApplies(origin, token string, ops []storageOp) [][]storageOp {
	args, _ := json.Marshal(storageArgs{Op: "apply", Origin: origin, Token: token, First: true, Last: true}) // always marshals
	room := restoreWireBudget - applyWire("("+storageFn+")("+string(args)+")") - applyWire(`,"ops":[]`)
	var calls [][]storageOp
	var cur []storageOp
	left := room
	flush := func() {
		if len(cur) > 0 {
			calls = append(calls, cur)
			cur, left = nil, room
		}
	}
	for _, op := range ops {
		w := opWire(op)
		if w > left {
			flush()
		}
		if w <= left {
			cur = append(cur, op)
			left -= w
			continue
		}
		// Every piece is counted with the largest frame that a piece has.
		// An empty apply has room for a frame and a character of any
		// wire size, so each piece holds at least one character.
		empty := ""
		frame := opWire(storageOp{Area: op.Area, Value: &empty, Part: true, KeyPart: true})
		take := func(s string) string {
			if left < frame+maxValueWire {
				flush()
			}
			n, used := prefix(s, left-frame)
			left -= frame + used
			return s[:n]
		}
		for k := op.Key; k != ""; {
			part := take(k)
			cur = append(cur, storageOp{Area: op.Area, Key: part, KeyPart: true})
			k = k[len(part):]
		}
		if op.Value == nil {
			take("")
			cur = append(cur, storageOp{Area: op.Area})
			continue
		}
		for v := *op.Value; ; {
			part := take(v)
			v = v[len(part):]
			if v == "" {
				cur = append(cur, storageOp{Area: op.Area, Value: &part})
				break
			}
			cur = append(cur, storageOp{Area: op.Area, Value: &part, Part: true})
		}
	}
	flush()
	return calls
}

// runApplies sends the applies of calls. It returns how many applies may
// have kept changes in the storage (those that succeeded, and one whose
// outcome is unknown), whether the failed apply changed the storage before
// it undid its changes, and the error.
func runApplies(ctx context.Context, c webkit.Caller, origin, token string, calls [][]storageOp) (kept int, undone bool, err error) {
	for i, ops := range calls {
		r, err := storageCall(ctx, c, storageArgs{Op: "apply", Origin: origin, Token: token, First: i == 0, Last: i == len(calls)-1, Ops: ops})
		switch {
		case err != nil:
			return i + 1, false, err
		case r.WrongOrigin:
			return i, false, fmt.Errorf("the page left %s while the state loaded (it is at %s now)", origin, r.Origin)
		case r.UndoError != "":
			return i + 1, false, fmt.Errorf("%s, and the page's undo of its part failed: %s", r.Error, r.UndoError)
		case r.Error != "":
			return i, r.Changed > 0, errors.New(r.Error)
		}
	}
	return len(calls), false, nil
}

// newToken returns the token of one load's applies.
func newToken() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// restoreStorage makes the page's storage hold o's items; before is the
// page's storage (snapshot) at the start. On failure it puts the earlier
// values back and returns an error that says what the storage holds.
func restoreStorage(ctx context.Context, c webkit.Caller, o StateOrigin, before storageReply) error {
	ops := append(planArea("local", before.Local, o.LocalStorage), planArea("session", before.Session, o.SessionStorage)...)
	if len(ops) == 0 {
		return nil
	}
	token := newToken()
	calls := planApplies(o.Origin, token, ops)
	kept, undone, err := runApplies(ctx, c, o.Origin, token, calls)
	if err == nil {
		return nil
	}
	if kept == 0 {
		if undone {
			return fmt.Errorf("%w; the page put its earlier storage values back (other documents of the origin may have seen storage events) and the cookies were not changed", err)
		}
		return fmt.Errorf("%w; the storage and the cookies were not changed", err)
	}
	if uerr := undoApplies(ctx, c, o.Origin, before, calls[:kept]); uerr != nil {
		return logcause.Errorf("%w; putting the earlier storage values back failed too (%v), so the storage may be partly loaded; the cookies were not changed", err, uerr)
	}
	return fmt.Errorf("%w; the earlier storage values were put back (other documents of the origin may have seen storage events) and the cookies were not changed", err)
}

// undoApplies gives every key that calls change its value in before back
// (or removes it when before lacks it), with applies of its own. It plans
// the undo from the page's storage as it is now (a new snapshot) with
// planArea, so the storage never holds more than the larger of what it
// holds now and what it held before the load; both fit the quota.
func undoApplies(ctx context.Context, c webkit.Caller, origin string, before storageReply, calls [][]storageOp) error {
	now, err := storageCall(ctx, c, storageArgs{Op: "snapshot"})
	if err != nil {
		return err
	}
	if now.Origin != origin {
		return fmt.Errorf("the page left %s while the state loaded (it is at %s now)", origin, now.Origin)
	}
	changed := changedKeys(calls)
	ops := append(undoArea("local", now.Local, before.Local, changed["local"]), undoArea("session", now.Session, before.Session, changed["session"])...)
	if len(ops) == 0 {
		return nil
	}
	token := newToken()
	_, _, err = runApplies(ctx, c, origin, token, planApplies(origin, token, ops))
	return err
}

// changedKeys returns, per area, the keys of the sets and removes of
// calls; a key sent in key parts is joined as the page joins it.
func changedKeys(calls [][]storageOp) map[string]map[string]bool {
	out := map[string]map[string]bool{"local": {}, "session": {}}
	staged := ""
	for _, ops := range calls {
		for _, op := range ops {
			switch {
			case op.KeyPart:
				staged += op.Key
			case !op.Part:
				out[op.Area][staged+op.Key] = true
				staged = ""
			}
		}
	}
	return out
}

// undoArea returns the ops that give the keys of changed their values in
// before back (or remove them when before lacks them), from the area's
// items now; the other keys keep their values.
func undoArea(area string, now, before [][2]string, changed map[string]bool) []storageOp {
	old := map[string]string{}
	for _, p := range before {
		old[p[0]] = p[1]
	}
	var want []StorageEntry
	for _, p := range now {
		if !changed[p[0]] {
			want = append(want, StorageEntry{Name: p[0], Value: p[1]})
		}
	}
	for k := range changed {
		if v, ok := old[k]; ok {
			want = append(want, StorageEntry{Name: k, Value: v})
		}
	}
	return planArea(area, now, want)
}
