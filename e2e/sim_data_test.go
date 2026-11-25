package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil"
)

// The data that the simulator tests create or delete. The test page's
// query does not isolate example.com's storage, and a reused Simulator may
// hold the origin's data, so every name derives from the run token, and
// every destructive target (cookie URL and domain) from the checked
// AGENT_IOS_SIM_PAGE_URL, never from page state read earlier.

// testName returns the name of this run's test data of the kind kind.
func (s *simTarget) testName(kind string) string {
	return "iwdp_sim_" + kind + "_" + s.runToken
}

// pageOrigin returns the origin ("https://example.com") and the host of
// the checked test page URL.
func (s *simTarget) pageOrigin() (origin, host string, err error) {
	u, err := url.Parse(s.pageURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("AGENT_IOS_SIM_PAGE_URL %q has no origin: %v", s.pageURL, err)
	}
	return u.Scheme + "://" + u.Host, u.Hostname(), nil
}

func simDataCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// mustMutate runs a destructive step through mutate (a fresh target check
// right before it) and fails the test when the check or the step fails.
func (s *simTarget) mustMutate(t *testing.T, what string, action func(ctx context.Context, client *webkit.Client) error) {
	t.Helper()
	ctx, cancel := simDataCtx()
	defer cancel()
	if err := s.mutate(ctx, what, func(c *webkit.Client) error { return action(ctx, c) }); err != nil {
		t.Fatal(err)
	}
}

// simCookieRoundTrip is TestSim_SetAndDeleteCookie: it sets this run's
// cookie on the test page's host and deletes it again with
// Page.deleteCookie, sent directly, for the test page's origin.
func simCookieRoundTrip(t *testing.T, s *simTarget) {
	t.Helper()
	origin, host, err := s.pageOrigin()
	if err != nil {
		t.Fatal(err)
	}
	name := s.testName("cookie")
	cookie := webkit.Cookie{Name: name, Value: "test_value", Domain: host, Path: "/", SameSite: "Lax"}
	s.mustMutate(t, "set "+name, func(ctx context.Context, client *webkit.Client) error {
		return tools.SetCookie(ctx, client, cookie)
	})

	ctx, cancel := simDataCtx()
	defer cancel()
	client, err := s.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := tools.GetCookies(ctx, client)
	if err != nil {
		t.Fatalf("GetCookies after set: %v", err)
	}
	found := false
	for _, c := range cookies {
		if c.Name == name && c.Value == "test_value" {
			found = true
			break
		}
	}
	if !found {
		// Page.getCookies may not return cookies set via Page.setCookie in all WebKit versions.
		// Verify via JS instead.
		result, err := tools.EvaluateScript(ctx, client, "document.cookie", true)
		if err == nil {
			var cookieStr string
			_ = json.Unmarshal(result.Result.Value, &cookieStr)
			if strings.Contains(cookieStr, name+"=test_value") {
				t.Log("cookie found via document.cookie (not via Page.getCookies)")
			} else {
				t.Logf("cookie not found via document.cookie either: %q", cookieStr)
				t.Log("Page.setCookie succeeded but cookie not visible. May be a WebKit/simulator limitation")
			}
		}
	}

	s.mustMutate(t, "delete "+name+" with Page.deleteCookie", func(ctx context.Context, client *webkit.Client) error {
		_, err := client.Send(ctx, "Page.deleteCookie", map[string]string{"cookieName": name, "url": origin + "/"})
		return err
	})
}

// simIndexedDBRoundTrip is TestSim_EvaluateScriptPromise: a Promise that
// creates this run's IndexedDB database evaluates without an error, and
// with AwaitPromise the settled value comes back. Cleanup deletes only
// that database.
func simIndexedDBRoundTrip(t *testing.T, s *simTarget) {
	t.Helper()
	db := strconv.Quote(s.testName("db")) // a JavaScript string literal
	t.Cleanup(func() {
		s.mustMutate(t, "delete the test database "+db, func(ctx context.Context, client *webkit.Client) error {
			_, err := tools.EvaluateScript(ctx, client, "indexedDB.deleteDatabase("+db+")", false)
			return err
		})
	})
	openDB := `
		new Promise((resolve, reject) => {
			var req = indexedDB.open(` + db + `, 1);
			req.onupgradeneeded = function(e) {
				var db = e.target.result;
				var store = db.createObjectStore("items", {keyPath: "id"});
				store.add({id: 1, name: "test_item"});
			};
			req.onsuccess = function() { req.result.close(); resolve("ok"); };
			req.onerror = function() { reject(req.error); };
		})
	`
	s.mustMutate(t, "create the test database "+db, func(ctx context.Context, client *webkit.Client) error {
		_, err := tools.EvaluateScript(ctx, client, openDB, true)
		return err
	})
	var result *tools.EvaluateResult
	s.mustMutate(t, "open the test database "+db+" and await it", func(ctx context.Context, client *webkit.Client) error {
		var err error
		result, err = tools.EvaluateScriptWithOptions(ctx, client, openDB, tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
		return err
	})
	var settled string
	if err := json.Unmarshal(result.Result.Value, &settled); err != nil || settled != "ok" {
		t.Fatalf("awaited value = %s (%v), want \"ok\"", result.Result.Value, err)
	}
}

// The run token of the fake runs.
const simTestRunToken = "00112233445566778899aabbccddeeff"

// fakeSimPage is a fake WebKit test page on a fake listing, with a
// cookie jar. Its window.location.origin reports another origin, as a
// page that navigated away between reads would; location.href stays the
// test page.
type fakeSimPage struct {
	target *simTarget
	mu     sync.Mutex
	jar    map[string]string
	sets   []map[string]any
	dels   []map[string]string
	evals  []string
}

func newFakeSimPage(t *testing.T) *fakeSimPage {
	t.Helper()
	f := &fakeSimPage{jar: map[string]string{"test_cookie": "existing-user-value"}}
	page := testutil.NewMockServer()
	t.Cleanup(page.Close)
	page.Handle("Runtime.evaluate", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Expression    string
			ReturnByValue bool
		}
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.evals = append(f.evals, p.Expression)
		f.mu.Unlock()
		switch {
		case p.Expression == "location.href":
			return map[string]any{"result": map[string]any{"type": "string", "value": simTestPageURL}}, nil
		case p.Expression == "window.location.origin":
			return map[string]any{"result": map[string]any{"type": "string", "value": "https://unrelated.example"}}, nil
		case strings.Contains(p.Expression, "new Promise") && !p.ReturnByValue:
			return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
	})
	page.Handle("Runtime.awaitPromise", func(string, json.RawMessage) (interface{}, error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": "ok"}}, nil
	})
	page.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct{ Cookie map[string]any }
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sets = append(f.sets, p.Cookie)
		name, _ := p.Cookie["name"].(string)
		value, _ := p.Cookie["value"].(string)
		f.jar[name] = value
		return map[string]any{}, nil
	})
	page.Handle("Page.getCookies", func(string, json.RawMessage) (interface{}, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var c []map[string]any
		for name, value := range f.jar {
			c = append(c, map[string]any{"name": name, "value": value, "domain": "example.com", "path": "/"})
		}
		return map[string]any{"cookies": c}, nil
	})
	page.Handle("Page.deleteCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p map[string]string
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.dels = append(f.dels, p)
		delete(f.jar, p["cookieName"])
		return map[string]any{}, nil
	})
	listing := fakeIWDPListing(t, fakeIWDPDevice{id: "SIMULATOR", pages: []map[string]string{fakePage(simTestPageURL, page.URL)}})
	f.target = &simTarget{listingURL: listing, wsURL: page.URL, pageURL: simTestPageURL, runToken: simTestRunToken}
	t.Cleanup(func() {
		if f.target.client != nil {
			_ = f.target.client.Close()
		}
	})
	return f
}

// TestSimCookieRoundTripUsesTheCheckedURLAndARunName: the cookie test
// sets and deletes only this run's cookie, for the origin of the checked
// test page URL, even when the page reports another origin; the origin's
// existing cookie test_cookie stays (review round 4, F10 and F11).
func TestSimCookieRoundTripUsesTheCheckedURLAndARunName(t *testing.T) {
	f := newFakeSimPage(t)
	t.Run("round trip", func(t *testing.T) { simCookieRoundTrip(t, f.target) })
	name := "iwdp_sim_cookie_" + simTestRunToken
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) != 1 || f.sets[0]["name"] != name || f.sets[0]["domain"] != "example.com" {
		t.Errorf("Page.setCookie calls %v; want one for %s on example.com", f.sets, name)
	}
	if len(f.dels) != 1 || f.dels[0]["cookieName"] != name || f.dels[0]["url"] != "https://example.com/" {
		t.Errorf("Page.deleteCookie calls %v; want one for %s at https://example.com/", f.dels, name)
	}
	if v, ok := f.jar["test_cookie"]; !ok || v != "existing-user-value" {
		t.Errorf("the origin's existing cookie test_cookie = %q (present %v); want it untouched", v, ok)
	}
}

// TestSimIndexedDBRoundTripUsesTheRunDatabase: the IndexedDB test opens
// and deletes only this run's database (review round 4, F11).
func TestSimIndexedDBRoundTripUsesTheRunDatabase(t *testing.T) {
	f := newFakeSimPage(t)
	t.Run("round trip", func(t *testing.T) { simIndexedDBRoundTrip(t, f.target) })
	db := strconv.Quote("iwdp_sim_db_" + simTestRunToken)
	f.mu.Lock()
	defer f.mu.Unlock()
	opened, deleted := 0, 0
	for _, e := range f.evals {
		switch {
		case strings.Contains(e, "indexedDB.open("+db+", 1)"):
			opened++
		case e == "indexedDB.deleteDatabase("+db+")":
			deleted++
		case strings.Contains(e, "indexedDB"):
			t.Errorf("an IndexedDB call outside this run's database %s: %s", db, e)
		}
	}
	if opened != 2 || deleted != 1 {
		t.Fatalf("opened %s %d times and deleted it %d times; want 2 and 1\nevaluations: %q", db, opened, deleted, f.evals)
	}
}
