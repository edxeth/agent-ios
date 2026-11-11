package cdp

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// storagePage makes the fake page answer the bridge's clear function with
// a page at origin whose localStorage holds n items, and counts the
// clears.
type storagePage struct {
	mu      sync.Mutex
	origin  string
	items   int
	clears  int
	evalled int
}

func (sp *storagePage) install(f *fakewebkit.Server) {
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if !strings.Contains(p.Expression, "localStorage.clear()") {
			return map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}}, nil
		}
		sp.evalled++
		var reply string
		if strings.Contains(p.Expression, `("`+sp.origin+`")`) {
			reply = `{"origin":"` + sp.origin + `","cleared":` + string(rune('0'+sp.items)) + `}`
			sp.items = 0
			sp.clears++
		} else {
			reply = `{"origin":"` + sp.origin + `","cleared":-1}`
		}
		return map[string]any{"result": map[string]any{"type": "string", "value": reply}}, nil
	})
}

func (sp *storagePage) counts() (evalled, clears, items int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.evalled, sp.clears, sp.items
}

// Storage.clearDataForOrigin clears localStorage in the scope's pages of
// the origin (local_storage, or all), on the browser session and on a
// page session; storageTypes is parsed as Chrome parses it; a request that
// names no type iOS can clear fails and evaluates nothing; a page that
// another session leases fails with the lease message.
func TestCDPClearDataForOrigin(t *testing.T) {
	r := newRig(t)
	one := &storagePage{origin: "https://one.example", items: 3}
	one.install(r.fake["One"])
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": "https://one.example", "storageTypes": "cookies, local_storage"})
	if ev, cl, items := one.counts(); ev != 1 || cl != 1 || items != 0 {
		t.Fatalf("after local_storage: %d evaluations, %d clears, %d items; want 1, 1, 0", ev, cl, items)
	}
	one.mu.Lock()
	one.items = 2
	one.mu.Unlock()
	// A full URL is reduced to its origin, as Chrome does; "all" clears too.
	call(t, c, page, "Storage.clearDataForOrigin", map[string]any{"origin": "https://ONE.example:443/path", "storageTypes": "all"})
	if _, cl, items := one.counts(); cl != 2 || items != 0 {
		t.Fatalf("after all on the page session: %d clears, %d items", cl, items)
	}
	for _, tc := range []struct {
		origin, types string
		code          int
		msg           string
	}{
		{"https://one.example", "cookies,indexeddb,cache_storage", -32000, "iOS WebKit cannot clear"},
		{"https://one.example", "websql, foo", -32602, "No valid storage type specified"},
		{"https://one.example", "", -32602, "No valid storage type specified"},
		{"not a url", "local_storage", -32602, "not a url is not a valid URL"},
		{"https://three.example", "local_storage", -32000, "no page of this endpoint's scope shows https://three.example"},
	} {
		e := cdpFail(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": tc.origin, "storageTypes": tc.types})
		if e.Code != tc.code || !strings.Contains(e.Message, tc.msg) {
			t.Errorf("clearDataForOrigin(%q, %q): %+v, want %d %q", tc.origin, tc.types, e, tc.code, tc.msg)
		}
	}
	if ev, _, _ := one.counts(); ev != 2 {
		t.Fatalf("a refused request evaluated in the page: %d evaluations, want 2", ev)
	}
	// The page shows another origin when the clear runs: nothing cleared.
	one.mu.Lock()
	one.origin = "https://moved.example"
	one.mu.Unlock()
	if e := cdpFail(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": "https://one.example", "storageTypes": "local_storage"}); !strings.Contains(e.Message, "navigated to another origin") {
		t.Fatalf("clear after a navigation: %+v", e)
	}
	// Another session's endpoint: the lease message, s1's lease unchanged.
	_, epoch := r.leaseOf(t, "One")
	c2 := dial(t, r.issue(t, "s2", "One", ScopePage).WS)
	if e := cdpFail(t, c2, "", "Storage.clearDataForOrigin", map[string]any{"origin": "https://one.example", "storageTypes": "local_storage"}); !strings.Contains(e.Message, "s1") {
		t.Fatalf("clear through s2: %+v, want the lease message", e)
	}
	if s, e := r.leaseOf(t, "One"); s != "s1" || e != epoch {
		t.Fatalf("lease %s/%d, want s1/%d", s, e, epoch)
	}
	if ev, _, _ := one.counts(); ev != 3 {
		t.Fatalf("the leased page evaluated the clear: %d evaluations, want 3", ev)
	}
}

func TestWebOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://A.example":         "https://a.example",
		"http://a.example:80/x":     "http://a.example",
		"https://a.example:8443/":   "https://a.example:8443",
		"http://[::1]:9000/":        "http://[::1]:9000",
		"http://127.0.0.1:443/page": "http://127.0.0.1:443",
	} {
		if got, ok := webOrigin(in); !ok || got != want {
			t.Errorf("webOrigin(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"about:blank", "file:///x", "https://", "::"} {
		if got, ok := webOrigin(in); ok {
			t.Errorf("webOrigin(%q) = %q, want none", in, got)
		}
	}
}
