package devtools

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	"github.com/nnemirovsky/agent-ios/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// bg is the context of a test's calls to a fake page, which answers every
// call: only the test's own deadline (go test -timeout) bounds it, to turn
// a hang into a failure. A fixed bound does not fit: a state load or save
// of megabytes, and a property's run of 60 of them on one context, took
// more than 10 s under -race and a host load of 70 (devfix R3).
func bg(t *testing.T) context.Context {
	d, ok := t.Deadline()
	if !ok {
		return context.Background()
	}
	ctx, cancel := context.WithDeadline(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// Design 4.2 "storage": localStorage and sessionStorage through the page
// (JS, no storage id needed).
func TestStorageOperations(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	s := p.Conn
	ctx := bg(t)
	if o, err := StorageSet(ctx, s, "local", "b", "2"); err != nil || o != "https://app.test" {
		t.Fatalf("set: %q, %v", o, err)
	}
	if _, err := StorageSet(ctx, s, "local", "a", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := StorageSet(ctx, s, "session", "s", "x"); err != nil {
		t.Fatal(err)
	}
	_, items, err := StorageList(ctx, s, "local")
	if want := []StorageEntry{{"a", "1"}, {"b", "2"}}; err != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("list local = %v, %v; want %v (sorted by name)", items, err, want)
	}
	if _, v, found, err := StorageGet(ctx, s, "session", "s"); err != nil || !found || v != "x" {
		t.Fatalf("get session s = %q %v %v", v, found, err)
	}
	if _, _, found, err := StorageGet(ctx, s, "local", "missing"); err != nil || found {
		t.Fatalf("get missing: found %v, %v", found, err)
	}
	if _, n, err := StorageClear(ctx, s, "local"); err != nil || n != 2 || len(p.LocalStorage) != 0 || len(p.SessionStorage) != 1 {
		t.Fatalf("clear local: %d, %v; local %v session %v", n, err, p.LocalStorage, p.SessionStorage)
	}
	if _, _, err := StorageList(ctx, s, "cookie"); err == nil || !strings.Contains(err.Error(), "local or session") {
		t.Fatalf("bad area: %v", err)
	}
}

// genState is a random state of origin https://app.test: cookies of the
// origin's host and its parent domain with distinct (name, domain, path),
// both expiry kinds and every SameSite value, and storage with any text.
// A host-only cookie (domain "app.test") and a domain cookie (".app.test")
// may have the same name and path: WebKit's jar keeps them apart
// (TestDevtoolsCookieIdentityIsStoredAsSet in e2e/webkitgtk).
func genState(r *rand.Rand) State {
	s := State{Version: StateVersion, Cookies: []StateCookie{}, Origins: []StateOrigin{{Origin: "https://app.test", LocalStorage: []StorageEntry{}, SessionStorage: []StorageEntry{}}}}
	seen := map[string]bool{}
	for i := 0; i < r.Intn(6); i++ {
		c := StateCookie{
			Name: randFrom(r, tokenChars, 1, 6), Value: randFrom(r, cookieOctets, 0, 10),
			Domain: []string{"app.test", ".app.test"}[r.Intn(2)], Path: []string{"/", "/a", "/a/b"}[r.Intn(3)],
			Expires: -1, HTTPOnly: r.Intn(2) == 0, Secure: r.Intn(2) == 0, SameSite: []string{"Lax", "Strict", "None"}[r.Intn(3)],
		}
		if r.Intn(2) == 0 {
			c.Expires = float64(1_700_000_000_000+r.Int63n(1e11)) / 1000
		}
		k := c.Name + "|" + c.Domain + "|" + c.Path
		if !seen[k] {
			seen[k] = true
			s.Cookies = append(s.Cookies, c)
		}
	}
	text := func() string { return randString(r) + randFrom(r, cookieOctets, 0, 4) }
	o := &s.Origins[0]
	keys := map[string]bool{}
	for i := 0; i < r.Intn(5); i++ {
		if k := text(); !keys["l"+k] {
			keys["l"+k] = true
			o.LocalStorage = append(o.LocalStorage, StorageEntry{k, text()})
		}
		if k := text(); !keys["s"+k] {
			keys["s"+k] = true
			o.SessionStorage = append(o.SessionStorage, StorageEntry{k, text()})
		}
	}
	sortState(&s)
	return s
}

// sortState puts a state in the order SaveState writes it.
func sortState(s *State) {
	sort.Slice(s.Cookies, func(i, j int) bool {
		a, b := s.Cookies[i], s.Cookies[j]
		return a.Domain+"\x00"+a.Path+"\x00"+a.Name < b.Domain+"\x00"+b.Path+"\x00"+b.Name
	})
	for _, o := range s.Origins {
		for _, l := range [][]StorageEntry{o.LocalStorage, o.SessionStorage} {
			sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
		}
	}
}

// Property (design 4.2 and the W4-2b brief: "state load restores exactly
// what state save wrote"): for any state of the page's origin, the file
// decodes to the same state, and after "state load" of it into a page with
// other cookies and storage of that origin, "state save" gives the same
// state again. Cookies of other sites stay untouched.
func TestStateLoadSaveRoundTripProperty(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	s := p.Conn
	ctx := bg(t)
	other := webkit.Cookie{Name: "o", Value: "keep", Domain: "other.test", Path: "/", Session: true, SameSite: "Lax"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		want := genState(r)
		data, err := EncodeState(want)
		if err != nil {
			fmt.Println("encode:", err)
			return false
		}
		decoded, err := DecodeState(data)
		if err != nil || !reflect.DeepEqual(decoded, want) {
			fmt.Printf("decode: %v\n got %+v\nwant %+v\n", err, decoded, want)
			return false
		}
		// Other state of the origin, which load must replace.
		junk := genState(r)
		p.Mu.Lock()
		p.Cookies = []webkit.Cookie{other}
		for _, c := range junk.Cookies {
			p.Cookies = append(p.Cookies, c.WebKit())
		}
		p.LocalStorage = map[string]string{"junk": "1"}
		p.SessionStorage = map[string]string{"junk": "2"}
		p.Mu.Unlock()
		if _, err := LoadState(ctx, s, decoded); err != nil {
			fmt.Println("load:", err)
			return false
		}
		got, err := SaveState(ctx, s)
		if err != nil {
			fmt.Println("save:", err)
			return false
		}
		got.SavedAt = want.SavedAt
		if !reflect.DeepEqual(got, want) {
			fmt.Printf("round trip\n got %+v\nwant %+v\n", got, want)
			return false
		}
		p.Mu.Lock()
		defer p.Mu.Unlock()
		for _, c := range p.Cookies {
			if c == other {
				return true
			}
		}
		fmt.Println("the cookie of other.test was removed")
		return false
	}
	// Seeds that failed before, checked first on every run. The first
	// holds a host-only and a domain cookie of the same name and path.
	for _, seed := range []int64{-8373736535080432565} {
		if !f(seed) {
			t.Fatalf("seed %d: the round trip changed the state", seed)
		}
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 60}); err != nil {
		t.Fatal(err)
	}
}

// State files hold the cookies and storage of the current origin only
// (W4-2b brief): cookies of other sites are left out.
func TestStateSaveKeepsTheOriginOnly(t *testing.T) {
	p := devtoolstest.New(t, "http://app.test:8080")
	s := p.Conn
	p.Cookies = []webkit.Cookie{
		{Name: "mine", Value: "1", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "parent", Value: "2", Domain: ".test", Path: "/", Expires: 1800000000500, SameSite: "None", Secure: true},
		{Name: "sub", Value: "3", Domain: "api.app.test", Path: "/", Session: true, SameSite: "Lax"},
		{Name: "tracker", Value: "4", Domain: ".ads.example", Path: "/", Session: true, SameSite: "None"},
	}
	p.LocalStorage["k"] = "v"
	st, err := SaveState(bg(t), s)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range st.Cookies {
		names = append(names, c.Name)
	}
	if want := []string{"parent", "mine"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("saved cookies %v, want %v (the origin's host and its parent domain)", names, want)
	}
	if st.Cookies[0].Expires != 1800000000.5 || st.Cookies[1].Expires != -1 {
		t.Fatalf("expires %v and %v; want 1800000000.5 s (from 1800000000500 ms) and -1 (session)", st.Cookies[0].Expires, st.Cookies[1].Expires)
	}
	if len(st.Origins) != 1 || st.Origins[0].Origin != "http://app.test:8080" || !reflect.DeepEqual(st.Origins[0].LocalStorage, []StorageEntry{{"k", "v"}}) {
		t.Fatalf("origins %+v", st.Origins)
	}
}

func TestStateLoadRefusals(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	s := p.Conn
	ctx := bg(t)
	base := State{Version: StateVersion, Origins: []StateOrigin{{Origin: "https://app.test"}}}
	wrongOrigin := base
	wrongOrigin.Origins = []StateOrigin{{Origin: "https://elsewhere.test"}}
	foreign := base
	foreign.Cookies = []StateCookie{{Name: "x", Value: "secretvalue", Domain: "evil.test", Path: "/", Expires: -1, SameSite: "Lax"}}
	cases := []struct {
		name string
		st   State
		want string
	}{
		{"other origin", wrongOrigin, "https://elsewhere.test"},
		{"cookie of another site", foreign, "evil.test"},
		{"two origins", State{Version: StateVersion, Origins: []StateOrigin{{Origin: "https://app.test"}, {Origin: "https://b.test"}}}, "one origin"},
		{"version", State{Version: 2, Origins: base.Origins}, "version"},
	}
	for _, c := range cases {
		_, err := LoadState(ctx, s, c.st)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "secretvalue") {
			t.Errorf("%s: %v; want an error naming %q without cookie values", c.name, err, c.want)
		}
	}
	if len(p.Cookies) != 0 || p.Evals > 1 {
		t.Fatalf("a refused load changed the page: cookies %v, %d evaluations", p.Cookies, p.Evals)
	}
	if _, err := DecodeState([]byte(`{"cookies":[]}`)); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("a file without version: %v", err)
	}
}

// "state load" of a file with an empty storage area (W4 storage review,
// finding storage.go:55): the page script gets both lists, also empty
// ones, so each area ends up exactly as the file has it, and the cookies
// of the file are set.
func TestStateLoadWithAnEmptyStorageArea(t *testing.T) {
	entry := []StorageEntry{{"saved", "v"}}
	for _, c := range []struct {
		name           string
		local, session []StorageEntry
	}{
		{"both empty", nil, nil},
		{"local empty", nil, entry},
		{"session empty", entry, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := devtoolstest.New(t, "https://app.test")
			p.LocalStorage["junk"] = "1"
			p.SessionStorage["junk"] = "2"
			st := State{
				Version: StateVersion,
				Cookies: []StateCookie{{Name: "theme", Value: "dark", Domain: "app.test", Path: "/", Expires: -1, SameSite: "Lax"}},
				Origins: []StateOrigin{{Origin: "https://app.test", LocalStorage: c.local, SessionStorage: c.session}},
			}
			// A state file always has the lists (SaveState writes [] for an
			// empty area); decode one as "state load" does.
			data, err := EncodeState(st)
			if err != nil {
				t.Fatal(err)
			}
			if st, err = DecodeState(data); err != nil {
				t.Fatal(err)
			}
			res, err := LoadState(bg(t), p.Conn, st)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			want := func(es []StorageEntry) map[string]string {
				m := map[string]string{}
				for _, e := range es {
					m[e.Name] = e.Value
				}
				return m
			}
			if !reflect.DeepEqual(p.LocalStorage, want(c.local)) || !reflect.DeepEqual(p.SessionStorage, want(c.session)) {
				t.Fatalf("storage after load: local %v, session %v; want local %v, session %v", p.LocalStorage, p.SessionStorage, want(c.local), want(c.session))
			}
			if res.CookiesSet != 1 || res.LocalStorage != len(c.local) || res.SessionStorage != len(c.session) {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

// A state load whose storage restore fails changes nothing: the page
// script puts the earlier items back (the fake's RestoreError stands in
// for a setItem that throws), and LoadState then leaves the cookies
// alone. When the cookies fail after the restore, the error says that
// the storage is restored and the cookies are not.
func TestStateLoadFailures(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	live := webkit.Cookie{Name: "live", Value: "c1", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"}
	p.Cookies = []webkit.Cookie{live}
	p.LocalStorage["live"] = "1"
	st := State{
		Version: StateVersion,
		Cookies: []StateCookie{{Name: "fromfile", Value: "f", Domain: "app.test", Path: "/", Expires: -1, SameSite: "Lax"}},
		Origins: []StateOrigin{{Origin: "https://app.test", LocalStorage: []StorageEntry{{"a", "1"}}, SessionStorage: []StorageEntry{}}},
	}
	p.RestoreError = "QuotaExceededError: the quota has been exceeded"
	_, err := LoadState(bg(t), p.Conn, st)
	if err == nil || !strings.Contains(err.Error(), "QuotaExceededError") || !strings.Contains(err.Error(), "not changed") {
		t.Fatalf("load with a failing restore: %v; want the page's error and that nothing changed", err)
	}
	if !reflect.DeepEqual(p.Cookies, []webkit.Cookie{live}) || !reflect.DeepEqual(p.LocalStorage, map[string]string{"live": "1"}) {
		t.Fatalf("a failed restore changed the page: cookies %v, local %v", p.Cookies, p.LocalStorage)
	}

	p.RestoreError = ""
	p.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "setCookie refused"}
	})
	_, err = LoadState(bg(t), p.Conn, st)
	if err == nil || !strings.Contains(err.Error(), "the storage is restored, the cookies are not") || !strings.Contains(err.Error(), "setCookie refused") {
		t.Fatalf("load with a failing cookie: %v; want an error that says the storage is restored and the cookies are not", err)
	}
	if !reflect.DeepEqual(p.LocalStorage, map[string]string{"a": "1"}) {
		t.Fatalf("local storage %v, want the file's", p.LocalStorage)
	}
}

// A storage value with JavaScript call syntax ("})(") is plain data in the
// page script's argument. The fake page (devtoolstest) must find the
// argument after the function, not after the last "})(" in the expression:
// TestStateLoadSaveRoundTripProperty failed on such a random value under
// -race (seed 3603571629472469047, W4 round 2 final run).
func TestStorageValueWithCallSyntax(t *testing.T) {
	p := devtoolstest.New(t, "https://app.test")
	ctx := bg(t)
	const v = `x"})({"y":1})(`
	if _, err := StorageSet(ctx, p.Conn, "local", "k})(", v); err != nil {
		t.Fatal(err)
	}
	if _, got, found, err := StorageGet(ctx, p.Conn, "local", "k})("); err != nil || !found || got != v {
		t.Fatalf("StorageGet = %q, %v, %v; want %q", got, found, err, v)
	}
}
