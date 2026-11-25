package devtools

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf16"

	"github.com/nnemirovsky/agent-ios/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// W4 repair round 3, storage review findings storage.go:46 (a failed
// state load still signalled a clear and intermediate writes to other
// same-origin documents) and state.go:178 (a saved state above the
// transport's message limit could not be loaded). Brief W4-R3 items 5 and
// 6: validate before any write, write only the difference, send the
// restore in several calls below the transport's limit, and put the
// earlier values back when a write fails part-way. The fake page
// (devtoolstest) has WebKitGTK's 127 KiB message limit and logs every
// storage write.

func loadState(origin string, local, session []StorageEntry) State {
	return State{Version: StateVersion, Cookies: []StateCookie{}, Origins: []StateOrigin{{Origin: origin, LocalStorage: local, SessionStorage: session}}}
}

// sizes describes a storage area by its keys and value lengths, for
// failure messages (the values can be large).
func sizes(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%q:%d ", k, len(m[k]))
	}
	return b.String()
}

func applies(p *devtoolstest.Page) int {
	n := 0
	for _, rec := range p.Server.Received() {
		if rec.Method == "Runtime.evaluate" && strings.Contains(string(rec.Params), `\"op\":\"apply\"`) {
			n++
		}
	}
	return n
}

// Only the difference is written: a key that the file lacks is removed, a
// key whose value differs is set, an equal key is not written, and
// nothing is cleared.
func TestStateLoadWritesOnlyTheDifference(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.LocalStorage = map[string]string{"same": "1", "gone": "x", "change": "old"}
	p.SessionStorage = map[string]string{"keep": "s"}
	st := loadState("https://app.test",
		[]StorageEntry{{"same", "1"}, {"change", "new"}, {"add", "v"}},
		[]StorageEntry{{"keep", "s"}})
	if _, err := LoadState(bg(t), p.Conn, st); err != nil {
		t.Fatal(err)
	}
	// Order: removes, then the sets that do not grow the area, then the
	// others (restore.go planArea).
	if want := []string{"local remove gone", "local set change", "local set add"}; !reflect.DeepEqual(p.Writes, want) {
		t.Fatalf("writes %q, want %q", p.Writes, want)
	}
	if want := map[string]string{"same": "1", "change": "new", "add": "v"}; !reflect.DeepEqual(p.LocalStorage, want) {
		t.Fatalf("local %s, want %s", sizes(p.LocalStorage), sizes(want))
	}

	// Loading the same state again writes nothing and sends no apply.
	p.Writes = nil
	before := applies(p)
	if _, err := LoadState(bg(t), p.Conn, st); err != nil {
		t.Fatal(err)
	}
	if len(p.Writes) != 0 || applies(p) != before {
		t.Fatalf("an unchanged state wrote %q in %d applies", p.Writes, applies(p)-before)
	}
}

// The size check uses WebKit's quota of 5 MiB per origin and area
// (LocalStorageManager.cpp localStorageQuotaInBytes, Settings.yaml
// SessionStorageQuota 5242880) in WebKit's byte count (W4 repair round 4,
// storage review finding restore.go:138): String::sizeInBytes
// (WTFString.h:127) counts 1 byte per character of an 8-bit (Latin-1)
// string and 2 per UTF-16 code unit of any other string; StorageMap::setItem
// (StorageMap.cpp:91-118) counts keys and values. 10% is kept free:
// 5*1024*1024*0.9 = 4718592 bytes.
func TestStateStorageSizeLimit(t *testing.T) {
	const limit = 4718592
	for _, tc := range []struct {
		name string
		key  string
		val  string // limit bytes with key
	}{
		// ASCII and Latin-1: 1 byte per character.
		{"ascii", "k", strings.Repeat("v", limit-1)},
		{"latin-1", "\u00e9", strings.Repeat("\u00ff", limit-1)},
		// U+0100 is outside Latin-1: the whole value counts 2 bytes per
		// unit, also its ASCII characters.
		{"two-byte", "kk", "\u0100" + strings.Repeat("v", limit/2-2)},
		// U+1F600 is two UTF-16 code units, 4 bytes.
		{"non-BMP", "kk", strings.Repeat("\U0001F600", (limit/2-1)/2) + "v"},
	} {
		at := StateOrigin{Origin: "https://app.test", SessionStorage: []StorageEntry{{tc.key, tc.val}}}
		if err := checkStorageSize(at); err != nil {
			t.Fatalf("%s: an area of exactly %d bytes is refused: %v", tc.name, limit, err)
		}
		at.SessionStorage[0].Name += "x" // one more byte
		if err := checkStorageSize(at); err == nil || !strings.Contains(err.Error(), "sessionStorage") || !strings.Contains(err.Error(), "4718593 bytes") {
			t.Fatalf("%s: an area of %d bytes: %v; want a refusal that names sessionStorage and its size", tc.name, limit+1, err)
		}
	}
	// Of two items with one name, the later counts (setItem in order).
	dup := StateOrigin{Origin: "https://app.test", LocalStorage: []StorageEntry{{"k", strings.Repeat("v", limit)}, {"k", "small"}}}
	if err := checkStorageSize(dup); err != nil {
		t.Fatalf("a replaced item still counts: %v", err)
	}
}

// A load over the limit is refused before the page is touched: no write,
// so no storage event; and "state save" refuses a page whose storage
// "state load" would refuse. The save reads megabytes, so its context has
// throughputBound (it took more than bg's 10 s under -race and host load,
// W4 round 6).
func TestStateLoadRefusesAnOversizedAreaBeforeAnyWrite(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.LocalStorage["live"] = "1"
	big := strings.Repeat("x", storageRestoreLimit)
	st := loadState("https://app.test", []StorageEntry{{"a", "1"}, {"big", big}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), throughputBound)
	defer cancel()
	_, err := LoadState(ctx, p.Conn, st)
	if err == nil || !strings.Contains(err.Error(), "localStorage") || !strings.Contains(err.Error(), "not changed") {
		t.Fatalf("oversized load: %v; want a refusal that names localStorage and says nothing changed", err)
	}
	if p.Evals != 0 || len(p.Writes) != 0 || !reflect.DeepEqual(p.LocalStorage, map[string]string{"live": "1"}) {
		t.Fatalf("a refused load reached the page: %d evaluations, writes %q, local %s", p.Evals, p.Writes, sizes(p.LocalStorage))
	}

	p.Mu.Lock()
	p.LocalStorage["big"] = big
	p.Mu.Unlock()
	if _, err := SaveState(ctx, p.Conn); err == nil || !strings.Contains(err.Error(), "localStorage") {
		t.Fatalf("save of an oversized area: %v; want a refusal", err)
	}
}

// Brief W4-R3 item 6: a 140,000-byte value (above WebKitGTK's 127 KiB
// message limit) saves and loads; every apply stays below the limit (the
// fake session refuses a larger message).
func TestStateLoadOfAValueAboveTheMessageLimit(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ctx := bg(t)
	p.LocalStorage["big"] = strings.Repeat("a", 140_000)
	saved, err := SaveState(ctx, p.Conn)
	if err != nil {
		t.Fatal(err)
	}
	p.Mu.Lock()
	p.LocalStorage = map[string]string{"other": "1"}
	p.Mu.Unlock()
	if _, err := LoadState(ctx, p.Conn, saved); err != nil {
		t.Fatalf("load of the saved state: %v", err)
	}
	if got := p.LocalStorage["big"]; got != strings.Repeat("a", 140_000) || len(p.LocalStorage) != 1 {
		t.Fatalf("after load: %d items, big has %d bytes", len(p.LocalStorage), len(got))
	}
	if n := applies(p); n < 2 || n > 3 {
		t.Fatalf("%d applies for 140,000 bytes; want 2 or 3 (127 KiB each at most)", n)
	}
}

// Property: any state within the size limit loads exactly, whatever its
// characters cost on the wire (quotes, backslashes, control characters,
// HTML characters and U+2028 are escaped up to three times; non-BMP
// characters are four bytes), and no message exceeds the fake's 127 KiB.
// Some keys are larger than one message too.
func TestStateLoadChunksAnyContentProperty(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	alphabet := []rune("a\"\\\x01\n<>&\u2028\u00e9\U0001F600 z")
	gen := func(r *rand.Rand, max int) string {
		n := r.Intn(max + 1)
		b := make([]rune, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var local, session []StorageEntry
		for i := 0; i < 1+r.Intn(4); i++ {
			key := gen(r, 20)
			if r.Intn(4) == 0 {
				key = gen(r, 60_000) // a key sent in parts (round 4)
			}
			local = append(local, StorageEntry{key + "l" + string(rune('0'+i)), gen(r, 60_000)})
			session = append(session, StorageEntry{gen(r, 20) + "s" + string(rune('0'+i)), gen(r, 3_000)})
		}
		p.Mu.Lock()
		p.LocalStorage = map[string]string{"old": gen(r, 50_000)}
		p.SessionStorage = map[string]string{}
		p.Mu.Unlock()
		if _, err := LoadState(bg(t), p.Conn, loadState("https://app.test", local, session)); err != nil {
			t.Logf("seed %d: %v", seed, err)
			return false
		}
		p.Mu.Lock()
		defer p.Mu.Unlock()
		if !reflect.DeepEqual(p.LocalStorage, lastWins(local)) || !reflect.DeepEqual(p.SessionStorage, lastWins(session)) {
			t.Logf("seed %d: the page's storage differs from the state", seed)
			return false
		}
		return p.Stage == nil // the last apply removed the staged parts
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}

// A write that fails in a later apply puts back the values that earlier
// applies changed; the cookies stay.
func TestStateLoadPutsEarlierAppliesBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(p *devtoolstest.Page)
		want   string
		intact bool
	}{
		{"quota in a later apply", func(p *devtoolstest.Page) {
			p.RestoreError, p.RestoreErrorKey = "QuotaExceededError: the quota has been exceeded", "zz"
		}, "the earlier storage values were put back", true},
		{"reload between applies", func(p *devtoolstest.Page) {
			p.Reload = func(p *devtoolstest.Page, n int) {
				if n == 2 {
					p.Stage = nil
				}
			}
		}, "the earlier storage values were put back", true},
		{"navigation between applies", func(p *devtoolstest.Page) {
			p.Reload = func(p *devtoolstest.Page, n int) {
				if n == 2 {
					p.Origin = "https://elsewhere.test"
				}
			}
		}, "may be partly loaded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := devtoolstest.New(t, "https://app.test")
			live := webkit.Cookie{Name: "live", Value: "c1", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"}
			p.Cookies = []webkit.Cookie{live}
			p.LocalStorage = map[string]string{"a": "before-a", "b": "before-b", "keep": "k"}
			p.SessionStorage = map[string]string{"s": "before-s"}
			before := map[string]string{"a": "before-a", "b": "before-b", "keep": "k"}
			big := strings.Repeat("y", 100_000)
			// a and b are changed in the first apply, zz in a later one.
			st := loadState("https://app.test",
				[]StorageEntry{{"a", "file-a"}, {"b", ""}, {"keep", "k"}, {"y1", big}, {"zz", big + big}},
				[]StorageEntry{{"s", "file-s"}})
			st.Cookies = []StateCookie{{Name: "fromfile", Value: "f", Domain: "app.test", Path: "/", Expires: -1, SameSite: "Lax"}}
			tc.setup(p)
			_, err := LoadState(bg(t), p.Conn, st)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "the cookies were not changed") {
				t.Fatalf("load: %v; want %q and that the cookies were not changed", err, tc.want)
			}
			// The load's first apply succeeds and a later one fails (with
			// the undo, an intact case sends at least 3 applies; after the
			// navigation the undo stops at its snapshot of the storage).
			if applies(p) < 2 {
				t.Fatalf("the load used %d applies; the test needs a failure after the first", applies(p))
			}
			if !reflect.DeepEqual(p.Cookies, []webkit.Cookie{live}) {
				t.Fatalf("cookies %v, want only the live cookie", p.Cookies)
			}
			if tc.intact && (!reflect.DeepEqual(p.LocalStorage, before) || !reflect.DeepEqual(p.SessionStorage, map[string]string{"s": "before-s"})) {
				t.Fatalf("after the failed load: local %s session %s; want the earlier values", sizes(p.LocalStorage), sizes(p.SessionStorage))
			}
		})
	}
}

// W4 repair round 4, storage review finding state.go:110: "state save"
// accepted a key larger than one inspector message, which "state load"
// then refused. Keys are sent in parts as values are: a saved state with a
// 140,000-character key (above the fake's 127 KiB message limit, as on
// WebKitGTK) loads, also a long key with a long value, and a load removes
// a long key that the state lacks.
func TestStateLoadOfAKeyAboveTheMessageLimit(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ctx := bg(t)
	long := strings.Repeat("k", 140_000)
	p.LocalStorage[long] = "v"
	p.SessionStorage["\u00e9\"<"+long] = strings.Repeat("\u4e2d", 50_000)
	saved, err := SaveState(ctx, p.Conn)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	p.Mu.Lock()
	p.LocalStorage = map[string]string{"other": "1", long + "2": "x"}
	p.SessionStorage = map[string]string{}
	p.Mu.Unlock()
	if _, err := LoadState(ctx, p.Conn, saved); err != nil {
		t.Fatalf("load of the saved state: %v", err)
	}
	p.Mu.Lock()
	local, session, stage := p.LocalStorage, p.SessionStorage, p.Stage
	p.Mu.Unlock()
	if !reflect.DeepEqual(local, lastWins(saved.Origins[0].LocalStorage)) || !reflect.DeepEqual(session, lastWins(saved.Origins[0].SessionStorage)) {
		t.Fatalf("after load: local %d items, session %d items; want the saved state", len(local), len(session))
	}
	if stage != nil {
		t.Fatalf("the load left staged parts: %d key bytes", len(stage.Key))
	}
	if _, err := LoadState(ctx, p.Conn, loadState("https://app.test", nil, nil)); err != nil {
		t.Fatalf("load of an empty state: %v", err)
	}
	if len(p.LocalStorage)+len(p.SessionStorage) != 0 {
		t.Fatalf("the long keys were not removed: %d items left", len(p.LocalStorage)+len(p.SessionStorage))
	}
}

// quotaTestBytes is the fake page's storage quota in the quota property
// tests: small, so that a state near the quota is quick to load, and
// above one message, so that a load takes several applies.
const quotaTestBytes = 200_000

// quotaTexts are the kinds of text that WebKit counts differently: ASCII
// and Latin-1 at 1 byte per character, two-byte BMP and non-BMP text at 2
// bytes per UTF-16 code unit (a non-Latin-1 character makes every
// character of its string count 2).
var quotaTexts = [][]rune{
	[]rune("abxyz"),
	[]rune("a\u00e9\u00ffx"),
	[]rune("\u0100\u4e2d"),
	[]rune("\U0001F600a\u00e9"),
}

// quotaArea returns a random storage area that fits quotaTestBytes in
// WebKit's count (devtoolstest.AreaBytes); it is often near full. Its keys
// start with prefix, so that the areas of one test have different keys.
func quotaArea(r *rand.Rand, prefix string) map[string]string {
	m := map[string]string{}
	keys := []string{"a", "b", "c", "d", "\u00e9", "\u4e2d"}
	for _, i := range r.Perm(len(keys))[:r.Intn(len(keys)+1)] {
		k := prefix + keys[i]
		left := quotaTestBytes - devtoolstest.AreaBytes(m) - devtoolstest.WebKitBytes(k)
		if left < 0 {
			continue
		}
		budget := left
		if r.Intn(2) == 0 {
			budget = r.Intn(left + 1)
		}
		// Characters are added while the value's bytes stay within the
		// budget: the first non-Latin-1 character doubles the bytes of
		// the characters before it.
		kind := quotaTexts[r.Intn(len(quotaTexts))]
		var v []rune
		for units, wide := 0, false; ; {
			c := kind[r.Intn(len(kind))]
			u, w := units+utf16.RuneLen(c), wide || c > 0xFF
			size := u
			if w {
				size *= 2
			}
			if size > budget {
				break
			}
			v, units, wide = append(v, c), u, w
		}
		m[k] = string(v)
	}
	return m
}

func quotaEntries(m map[string]string) []StorageEntry {
	var out []StorageEntry
	for k, v := range m {
		out = append(out, StorageEntry{k, v})
	}
	return out
}

// W4 repair round 4, storage review finding restore.go:138: WebKit's quota
// counts bytes, 1 per character of Latin-1 text and 2 per UTF-16 code unit
// of other text, so a shorter value can be larger. Property: a load from
// any state that fits the quota to any other state that fits it succeeds
// on a page that enforces the quota as WebKit does, and no area ever holds
// more than the larger of its size before and after the load.
func TestStateLoadStaysWithinTheQuotaProperty(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.QuotaBytes = quotaTestBytes
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		start := map[string]map[string]string{"local": quotaArea(r, "l"), "session": quotaArea(r, "s")}
		want := map[string]map[string]string{"local": quotaArea(r, "l"), "session": quotaArea(r, "s")}
		p.Mu.Lock()
		p.LocalStorage, p.SessionStorage = maps.Clone(start["local"]), maps.Clone(start["session"])
		p.Peak = nil
		p.Mu.Unlock()
		if _, err := LoadState(bg(t), p.Conn, loadState("https://app.test", quotaEntries(want["local"]), quotaEntries(want["session"]))); err != nil {
			t.Logf("seed %d: %v", seed, err)
			return false
		}
		p.Mu.Lock()
		defer p.Mu.Unlock()
		if !reflect.DeepEqual(p.LocalStorage, want["local"]) || !reflect.DeepEqual(p.SessionStorage, want["session"]) {
			t.Logf("seed %d: the page's storage differs from the state", seed)
			return false
		}
		for _, a := range []string{"local", "session"} {
			if bound := max(devtoolstest.AreaBytes(start[a]), devtoolstest.AreaBytes(want[a])); p.Peak[a] > bound {
				t.Logf("seed %d: %s held %d bytes; before %d, after %d", seed, a, p.Peak[a], devtoolstest.AreaBytes(start[a]), devtoolstest.AreaBytes(want[a]))
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 50}); err != nil {
		t.Fatal(err)
	}
}

// W4 repair round 4, storage review finding restore.go:333: the undo of a
// failed load put the earlier values back in the load's order and could
// reach the quota. Property: when a set fails at a random key of a load
// between two states that fit the quota (on a page that enforces it as
// WebKit does), the page ends with exactly its earlier storage, and the
// undo never reaches the quota.
func TestStateLoadUndoStaysWithinTheQuotaProperty(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	p.QuotaBytes = quotaTestBytes
	acrossApplies := 0
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		start := map[string]map[string]string{"local": quotaArea(r, "l"), "session": quotaArea(r, "s")}
		want := map[string]map[string]string{"local": quotaArea(r, "l"), "session": quotaArea(r, "s")}
		var sets []string
		for _, a := range []string{"local", "session"} {
			for k, v := range want[a] {
				if old, had := start[a][k]; !had || old != v {
					sets = append(sets, k)
				}
			}
		}
		if len(sets) == 0 {
			return true
		}
		sort.Strings(sets)
		p.Mu.Lock()
		p.LocalStorage, p.SessionStorage = maps.Clone(start["local"]), maps.Clone(start["session"])
		p.RestoreError, p.RestoreErrorKey = "QuotaExceededError: injected", sets[r.Intn(len(sets))]
		p.Mu.Unlock()
		// The load and its undo send many applies; under -race and host
		// load one case took more than bg's 10 s, and the undo ran out of
		// time (W4 round 7), so the case has throughputBound.
		ctx, cancel := context.WithTimeout(context.Background(), throughputBound)
		defer cancel()
		_, err := LoadState(ctx, p.Conn, loadState("https://app.test", quotaEntries(want["local"]), quotaEntries(want["session"])))
		p.Mu.Lock()
		defer p.Mu.Unlock()
		p.RestoreError, p.RestoreErrorKey = "", ""
		if err == nil || !strings.Contains(err.Error(), "injected") || strings.Contains(err.Error(), "failed too") {
			t.Logf("seed %d: %v; want the injected failure and the earlier values put back", seed, err)
			return false
		}
		if strings.Contains(err.Error(), "the earlier storage values were put back") {
			acrossApplies++
		}
		if !reflect.DeepEqual(p.LocalStorage, start["local"]) || !reflect.DeepEqual(p.SessionStorage, start["session"]) {
			t.Logf("seed %d: the failed load left local %s session %s", seed, sizes(p.LocalStorage), sizes(p.SessionStorage))
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 50}); err != nil {
		t.Fatal(err)
	}
	// The cases must include undos of earlier applies, not only the
	// page's undo of the failed apply.
	if acrossApplies == 0 {
		t.Fatal("no case failed after the first apply")
	}
	t.Logf("%d cases put back the values of earlier applies", acrossApplies)
}
