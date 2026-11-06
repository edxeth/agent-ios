package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// webkitThrown is the Runtime.evaluate response WebKit sends when the
// expression throws: no exceptionDetails, the thrown value is a RemoteObject
// whose description is String(error) (WebKit InjectedScriptSource.js
// _createThrownValue; see the research note in the W0 report).
func webkitThrown(description, objectID string) map[string]interface{} {
	return map[string]interface{}{
		"result": map[string]interface{}{
			"type":        "object",
			"subtype":     "error",
			"className":   "TypeError",
			"description": description,
			"objectId":    objectID,
		},
		"wasThrown": true,
	}
}

func TestEvaluateScriptWebKitExceptionHasMessageAndStack(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Runtime.evaluate", webkitThrown("TypeError: undefined is not an object (evaluating 'a.b')", "err-1"))
	var stackObjectID string
	mock.Handle("Runtime.callFunctionOn", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			ObjectID      string `json:"objectId"`
			ReturnByValue bool   `json:"returnByValue"`
		}
		_ = json.Unmarshal(params, &p)
		stackObjectID = p.ObjectID
		return map[string]interface{}{
			"result": map[string]interface{}{"type": "string", "value": "global code@\nevaluateWithScopeExtension@[native code]"},
		}, nil
	})

	_, err := tools.EvaluateScript(context.Background(), client, "a.b", true)
	if err == nil {
		t.Fatal("expected an error for a thrown exception")
	}
	var jsErr *tools.JSException
	if !errors.As(err, &jsErr) {
		t.Fatalf("error %T is not a *tools.JSException", err)
	}
	if jsErr.Message != "TypeError: undefined is not an object (evaluating 'a.b')" {
		t.Errorf("Message = %q", jsErr.Message)
	}
	if jsErr.Stack != "global code@\nevaluateWithScopeExtension@[native code]" {
		t.Errorf("Stack = %q", jsErr.Stack)
	}
	if stackObjectID != "err-1" {
		t.Errorf("stack read from object %q, want err-1", stackObjectID)
	}
	if !strings.Contains(err.Error(), "TypeError") || !strings.Contains(err.Error(), "global code@") {
		t.Errorf("Error() = %q, want message and stack", err.Error())
	}
}

func TestEvaluateScriptThrownPrimitive(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result":    map[string]interface{}{"type": "string", "value": "boom"},
		"wasThrown": true,
	})
	_, err := tools.EvaluateScript(context.Background(), client, "throw 'boom'", true)
	var jsErr *tools.JSException
	if !errors.As(err, &jsErr) {
		t.Fatalf("error %v (%T) is not a *tools.JSException", err, err)
	}
	if jsErr.Message != "boom" {
		t.Errorf("Message = %q, want boom", jsErr.Message)
	}
}

func TestEvaluateScriptAwaitResolvesPromise(t *testing.T) {
	mock, client := setup(t)
	var evalByValue *bool
	mock.Handle("Runtime.evaluate", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			ReturnByValue bool `json:"returnByValue"`
		}
		_ = json.Unmarshal(params, &p)
		evalByValue = &p.ReturnByValue
		return map[string]interface{}{
			"result": map[string]interface{}{"type": "object", "className": "Promise", "objectId": "promise-1", "description": "Promise"},
		}, nil
	})
	var awaited struct {
		PromiseObjectID string `json:"promiseObjectId"`
		ReturnByValue   bool   `json:"returnByValue"`
	}
	mock.Handle("Runtime.awaitPromise", func(_ string, params json.RawMessage) (interface{}, error) {
		_ = json.Unmarshal(params, &awaited)
		return map[string]interface{}{
			"result": map[string]interface{}{"type": "object", "value": map[string]int{"status": 200}},
		}, nil
	})

	res, err := tools.EvaluateScriptWithOptions(context.Background(), client, "fetch('/x').then(r => ({status: r.status}))",
		tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
	if err != nil {
		t.Fatalf("EvaluateScriptWithOptions: %v", err)
	}
	if evalByValue == nil || *evalByValue {
		t.Error("Runtime.evaluate must use returnByValue=false so the Promise keeps its objectId")
	}
	if awaited.PromiseObjectID != "promise-1" || !awaited.ReturnByValue {
		t.Errorf("Runtime.awaitPromise params = %+v, want promise-1 by value", awaited)
	}
	if string(res.Result.Value) != `{"status":200}` {
		t.Errorf("value = %s, want {\"status\":200}", res.Result.Value)
	}
}

func TestEvaluateScriptAwaitRejectedPromise(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{"type": "object", "className": "Promise", "objectId": "promise-2"},
	})
	mock.HandleFunc("Runtime.awaitPromise", webkitThrown("Error: nope", "err-2"))
	mock.HandleFunc("Runtime.callFunctionOn", map[string]interface{}{
		"result": map[string]interface{}{"type": "string", "value": "reject@"},
	})

	_, err := tools.EvaluateScriptWithOptions(context.Background(), client, "Promise.reject(new Error('nope'))",
		tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
	var jsErr *tools.JSException
	if !errors.As(err, &jsErr) {
		t.Fatalf("error %v (%T) is not a *tools.JSException", err, err)
	}
	if jsErr.Message != "Error: nope" || jsErr.Stack != "reject@" {
		t.Errorf("exception = %+v", jsErr)
	}
}

func TestEvaluateScriptAwaitNonPromiseObjectByValue(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{"type": "object", "className": "Object", "objectId": "obj-1"},
	})
	var called string
	mock.Handle("Runtime.callFunctionOn", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			ObjectID      string `json:"objectId"`
			ReturnByValue bool   `json:"returnByValue"`
		}
		_ = json.Unmarshal(params, &p)
		if p.ReturnByValue {
			called = p.ObjectID
		}
		return map[string]interface{}{"result": map[string]interface{}{"type": "object", "value": map[string]int{"a": 1}}}, nil
	})
	res, err := tools.EvaluateScriptWithOptions(context.Background(), client, "({a: 1})",
		tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
	if err != nil {
		t.Fatalf("EvaluateScriptWithOptions: %v", err)
	}
	if called != "obj-1" {
		t.Errorf("object not fetched by value (callFunctionOn objectId = %q)", called)
	}
	if string(res.Result.Value) != `{"a":1}` {
		t.Errorf("value = %s", res.Result.Value)
	}
}

// --- Cookies ---

func TestRedactCookieValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "(redacted, 3 chars)"},
		{"abcdefgh", "ab...(redacted, 8 chars)"},
		{"abcdefghijklmnopqrstuvwxyz012345", "abcd...(redacted, 32 chars)"},
	}
	for _, tt := range tests {
		if got := tools.RedactCookieValue(tt.in); got != tt.want {
			t.Errorf("RedactCookieValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRedactCookieValueProperty: for any value, the output reveals at most a
// prefix of min(4, n/4) characters and reports the exact length n.
func TestRedactCookieValueProperty(t *testing.T) {
	prop := func(v string) bool {
		if !utf8.ValidString(v) {
			return true // cookie values are text; invalid UTF-8 is out of scope
		}
		n := utf8.RuneCountInString(v)
		got := tools.RedactCookieValue(v)
		if n == 0 {
			return got == ""
		}
		suffix := fmt.Sprintf("(redacted, %d chars)", n)
		if !strings.HasSuffix(got, suffix) {
			return false
		}
		shown := strings.TrimSuffix(strings.TrimSuffix(got, suffix), "...")
		limit := n / 4
		if limit > 4 {
			limit = 4
		}
		return strings.HasPrefix(v, shown) && utf8.RuneCountInString(shown) <= limit
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

func TestRedactCookiesKeepsOtherFields(t *testing.T) {
	in := []webkit.Cookie{{Name: "sid", Value: "secret-session-token", Domain: "example.com", Path: "/", HTTPOnly: true, SameSite: "Lax"}}
	out := tools.RedactCookies(in)
	if out[0].Value == in[0].Value || strings.Contains(out[0].Value, "secret-session-token") {
		t.Errorf("value not redacted: %q", out[0].Value)
	}
	if out[0].Name != "sid" || out[0].Domain != "example.com" || !out[0].HTTPOnly || out[0].SameSite != "Lax" {
		t.Errorf("other fields changed: %+v", out[0])
	}
	if in[0].Value != "secret-session-token" {
		t.Error("RedactCookies modified its input")
	}
}

func TestSetCookieNeverSendsEmptySameSite(t *testing.T) {
	mock, client := setup(t)
	var sent webkit.Cookie
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		_ = json.Unmarshal(params, &p)
		sent = p.Cookie
		return map[string]interface{}{}, nil
	})
	err := tools.SetCookie(context.Background(), client, webkit.Cookie{Name: "a", Value: "b", Domain: "example.com", Path: "/"})
	if err != nil {
		t.Fatalf("SetCookie: %v", err)
	}
	if sent.SameSite != "Lax" {
		t.Errorf("sameSite sent = %q, want Lax", sent.SameSite)
	}
	if !sent.Session {
		t.Error("a cookie without expiry must be sent as a session cookie")
	}
}

func TestSetCookieRejectsInvalidSameSite(t *testing.T) {
	mock, client := setup(t)
	err := tools.SetCookie(context.Background(), client, webkit.Cookie{Name: "a", Value: "b", Domain: "example.com", Path: "/", SameSite: "Sometimes"})
	if err == nil || !strings.Contains(err.Error(), "Sometimes") {
		t.Fatalf("err = %v, want an error naming the bad value", err)
	}
	for _, m := range mock.ReceivedMethods() {
		if m == "Page.setCookie" {
			t.Error("Page.setCookie was sent with an invalid sameSite")
		}
	}
}

func TestNormalizeSameSite(t *testing.T) {
	for in, want := range map[string]string{"": "Lax", "lax": "Lax", "STRICT": "Strict", "none": "None", "Lax": "Lax"} {
		got, err := tools.NormalizeSameSite(in)
		if err != nil || got != want {
			t.Errorf("NormalizeSameSite(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestSetCookieVerifiedReadsBack(t *testing.T) {
	mock, client := setup(t)
	var stored []webkit.Cookie
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		_ = json.Unmarshal(params, &p)
		c := p.Cookie
		c.Domain = "." + c.Domain // WebKit stores domain cookies with a leading dot
		stored = append(stored, c)
		return map[string]interface{}{}, nil
	})
	mock.Handle("Page.getCookies", func(_ string, _ json.RawMessage) (interface{}, error) {
		return map[string]interface{}{"cookies": stored}, nil
	})

	got, err := tools.SetCookieVerified(context.Background(), client, webkit.Cookie{
		Name: "sid", Value: "v", Domain: "example.com", Path: "/", SameSite: "strict", Secure: true, HTTPOnly: true,
	})
	if err != nil {
		t.Fatalf("SetCookieVerified: %v", err)
	}
	if got.SameSite != "Strict" || !got.Secure || !got.HTTPOnly {
		t.Errorf("read-back cookie = %+v", got)
	}
}

func TestSetCookieVerifiedFailsWhenCookieMissing(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": []webkit.Cookie{}})
	_, err := tools.SetCookieVerified(context.Background(), client, webkit.Cookie{Name: "sid", Value: "v", Domain: "example.com", Path: "/"})
	if err == nil || !strings.Contains(err.Error(), "sid") {
		t.Fatalf("err = %v, want an error naming the missing cookie", err)
	}
}

func TestSetCookieVerifiedFailsOnAttributeMismatch(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": []webkit.Cookie{
		{Name: "sid", Value: "v", Domain: "example.com", Path: "/", SameSite: "None"},
	}})
	_, err := tools.SetCookieVerified(context.Background(), client, webkit.Cookie{Name: "sid", Value: "v", Domain: "example.com", Path: "/", SameSite: "Lax"})
	if err == nil || !strings.Contains(err.Error(), "sameSite") {
		t.Fatalf("err = %v, want an error naming the sameSite mismatch", err)
	}
	if strings.Contains(err.Error(), "=v") {
		t.Errorf("error leaks the cookie value: %v", err)
	}
}

// sentCookieJSON records the raw cookie object of each Page.setCookie call,
// so tests can check which keys are on the wire.
func sentCookieJSON(mock *testutil.MockServer) func() []map[string]interface{} {
	var mu sync.Mutex
	var sent []map[string]interface{}
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Cookie map[string]interface{} `json:"cookie"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		mu.Lock()
		sent = append(sent, p.Cookie)
		mu.Unlock()
		return map[string]interface{}{}, nil
	})
	return func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), sent...)
	}
}

// TestSetCookieSessionOmitsExpires pins the session-cookie wire format. iOS
// 16.7 WebKit keeps any supplied expires, and Cocoa turns expires 0 into an
// expiry at the Unix epoch (InspectorPageAgent.cpp#L684-L691,
// CookieCocoa.mm#L158-L160, WebKit-7615.3.12.11.5; research note
// webkit-protocol-eval-cookie-snapshot-20261106.md). So a session cookie sends
// session=true and no expires key; session must always be present because
// that version dereferences it unchecked.
func TestSetCookieSessionOmitsExpires(t *testing.T) {
	mock, client := setup(t)
	sent := sentCookieJSON(mock)
	const persistentMs = 1893456000000.0 // 2030-01-01T00:00:00Z

	for _, c := range []webkit.Cookie{
		{Name: "a", Value: "b", Domain: "example.com", Path: "/"},
		{Name: "a", Value: "b", Domain: "example.com", Path: "/", Session: true},
		{Name: "a", Value: "b", Domain: "example.com", Path: "/", Expires: persistentMs},
	} {
		if err := tools.SetCookie(context.Background(), client, c); err != nil {
			t.Fatalf("SetCookie(%+v): %v", c, err)
		}
	}
	got := sent()
	if len(got) != 3 {
		t.Fatalf("Page.setCookie calls = %d, want 3", len(got))
	}
	for i, wire := range got[:2] {
		if v, ok := wire["expires"]; ok {
			t.Errorf("session cookie %d sent expires=%v; WebKit would store an expiry at the Unix epoch", i, v)
		}
		if wire["session"] != true {
			t.Errorf("session cookie %d sent session=%v, want true", i, wire["session"])
		}
	}
	if wire := got[2]; wire["expires"] != persistentMs || wire["session"] != false {
		t.Errorf("persistent cookie sent expires=%v session=%v, want %v and false", wire["expires"], wire["session"], persistentMs)
	}
	for i, wire := range got {
		for _, key := range []string{"name", "value", "domain", "path", "httpOnly", "secure", "sameSite", "session"} {
			if _, ok := wire[key]; !ok {
				t.Errorf("cookie %d lacks required key %q", i, key)
			}
		}
	}
}

// readBackCookie makes Page.getCookies return c for any Page.setCookie.
func readBackCookie(mock *testutil.MockServer, c webkit.Cookie) {
	mock.HandleFunc("Page.setCookie", map[string]interface{}{})
	mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": []webkit.Cookie{c}})
}

// TestSetCookieVerifiedChecksSessionAndExpiry pins that verification covers
// the session status and the expiry, not only name, flags and SameSite.
// Expiry is compared to the second: cookie dates have second precision
// (RFC 6265 section 5.1.1) and the MCP expires input is in seconds.
func TestSetCookieVerifiedChecksSessionAndExpiry(t *testing.T) {
	const wantMs = 1893456000000.0 // 2030-01-01T00:00:00Z
	persistent := webkit.Cookie{Name: "sid", Value: "v", Domain: "example.com", Path: "/", SameSite: "Lax", Expires: wantMs}
	session := webkit.Cookie{Name: "sid", Value: "v", Domain: "example.com", Path: "/", SameSite: "Lax", Session: true}

	for _, tc := range []struct {
		name      string
		requested webkit.Cookie
		stored    webkit.Cookie
		wantErr   string // empty: verified
	}{
		{"persistent stored as session", persistent, session, "session=true (requested false)"},
		{"session stored as persistent", session, persistent, "session=false (requested true)"},
		{"persistent with other expiry", persistent, withExpires(persistent, wantMs+86400000), "expires=2030-01-02T00:00:00Z (requested 2030-01-01T00:00:00Z)"},
		{"persistent within a second", withExpires(persistent, wantMs+400), persistent, ""},
		{"persistent matches", persistent, persistent, ""},
		{"session matches", session, session, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, client := setup(t)
			readBackCookie(mock, tc.stored)
			_, err := tools.SetCookieVerified(context.Background(), client, tc.requested)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("SetCookieVerified: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestSetCookieVerifiedReportsEachMismatch pins the rest of the read-back
// contract (SetCookieVerified doc): each differing attribute is named,
// cookies with another name, domain or path are skipped, an empty requested
// domain matches any domain, and no error contains the cookie value.
func TestSetCookieVerifiedReportsEachMismatch(t *testing.T) {
	const value = "s3cr3t-value"
	want := webkit.Cookie{Name: "sid", Value: value, Domain: "example.com", Path: "/", SameSite: "Lax", Session: true, Secure: true, HTTPOnly: true}
	other := func(edit func(*webkit.Cookie)) webkit.Cookie {
		c := want
		edit(&c)
		return c
	}
	for _, tc := range []struct {
		name      string
		requested webkit.Cookie
		stored    []webkit.Cookie
		wantErr   string // empty: verified
	}{
		{"value differs", want, []webkit.Cookie{other(func(c *webkit.Cookie) { c.Value = "other" })}, "value differs"},
		{"secure differs", want, []webkit.Cookie{other(func(c *webkit.Cookie) { c.Secure = false })}, "secure=false (requested true)"},
		{"httpOnly differs", want, []webkit.Cookie{other(func(c *webkit.Cookie) { c.HTTPOnly = false })}, "httpOnly=false (requested true)"},
		{"other cookies skipped", want, []webkit.Cookie{
			other(func(c *webkit.Cookie) { c.Name = "other"; c.Secure = false }),
			other(func(c *webkit.Cookie) { c.Domain = "example.org"; c.Secure = false }),
			other(func(c *webkit.Cookie) { c.Path = "/app"; c.Secure = false }),
			want,
		}, ""},
		{"empty domain matches any", other(func(c *webkit.Cookie) { c.Domain = "" }), []webkit.Cookie{other(func(c *webkit.Cookie) { c.Domain = ".example.net" })}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, client := setup(t)
			mock.HandleFunc("Page.setCookie", map[string]interface{}{})
			mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": tc.stored})
			_, err := tools.SetCookieVerified(context.Background(), client, tc.requested)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("SetCookieVerified: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("error leaks the cookie value: %v", err)
			}
		})
	}
}

// TestSetCookieVerifiedForwardsErrors pins that a rejected SameSite, a failed
// Page.setCookie and a failed read-back are errors, the last naming the cookie.
func TestSetCookieVerifiedForwardsErrors(t *testing.T) {
	cookie := webkit.Cookie{Name: "sid", Value: "v", Domain: "example.com", Path: "/"}

	mock, client := setup(t)
	bad := cookie
	bad.SameSite = "Sometimes"
	if _, err := tools.SetCookieVerified(context.Background(), client, bad); err == nil || !strings.Contains(err.Error(), "Sometimes") {
		t.Errorf("invalid sameSite: err = %v", err)
	}
	if sent := strings.Join(mock.ReceivedMethods(), " "); strings.Contains(sent, "Page.setCookie") {
		t.Error("Page.setCookie was sent with an invalid sameSite")
	}

	mock.Handle("Page.setCookie", func(string, json.RawMessage) (interface{}, error) { return nil, errors.New("set refused") })
	if _, err := tools.SetCookieVerified(context.Background(), client, cookie); err == nil || !strings.Contains(err.Error(), "set refused") {
		t.Errorf("Page.setCookie failure: err = %v", err)
	}

	mock.HandleFunc("Page.setCookie", map[string]interface{}{})
	mock.Handle("Page.getCookies", func(string, json.RawMessage) (interface{}, error) { return nil, errors.New("read refused") })
	if _, err := tools.SetCookieVerified(context.Background(), client, cookie); err == nil || !strings.Contains(err.Error(), "reading cookie sid back") || !strings.Contains(err.Error(), "read refused") {
		t.Errorf("Page.getCookies failure: err = %v", err)
	}
}

func withExpires(c webkit.Cookie, ms float64) webkit.Cookie {
	c.Expires = ms
	return c
}

// --- Screenshots ---

func snapshotRectRecorder(mock *testutil.MockServer) *map[string]interface{} {
	var got map[string]interface{}
	mock.Handle("Page.snapshotRect", func(_ string, params json.RawMessage) (interface{}, error) {
		_ = json.Unmarshal(params, &got)
		return map[string]interface{}{"dataURL": "data:image/png;base64,abc"}, nil
	})
	return &got
}

func TestCaptureScreenshotDefaultIsViewport(t *testing.T) {
	mock, client := setup(t)
	var expr string
	mock.Handle("Runtime.evaluate", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(params, &p)
		expr = p.Expression
		return map[string]interface{}{"result": map[string]interface{}{"type": "string", "value": `{"width":390,"height":664}`}}, nil
	})
	rect := snapshotRectRecorder(mock)

	if _, err := tools.CaptureScreenshot(context.Background(), client, tools.ScreenshotOptions{}); err != nil {
		t.Fatalf("CaptureScreenshot: %v", err)
	}
	if !strings.Contains(expr, "innerWidth") || !strings.Contains(expr, "innerHeight") {
		t.Errorf("viewport size expression = %q, want window.innerWidth/innerHeight", expr)
	}
	r := *rect
	if r["coordinateSystem"] != "Viewport" || r["x"] != float64(0) || r["y"] != float64(0) ||
		r["width"] != float64(390) || r["height"] != float64(664) {
		t.Errorf("Page.snapshotRect params = %v, want 0,0,390x664 Viewport", r)
	}
}

func TestCaptureScreenshotFullPage(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{"type": "string", "value": `{"width":390,"height":5000}`},
	})
	rect := snapshotRectRecorder(mock)
	if _, err := tools.CaptureScreenshot(context.Background(), client, tools.ScreenshotOptions{FullPage: true}); err != nil {
		t.Fatalf("CaptureScreenshot: %v", err)
	}
	if (*rect)["coordinateSystem"] != "Page" || (*rect)["height"] != float64(5000) {
		t.Errorf("Page.snapshotRect params = %v, want full document in Page coordinates", *rect)
	}
}

func TestCaptureScreenshotSelector(t *testing.T) {
	mock, client := setup(t)
	mock.HandleFunc("DOM.getDocument", map[string]interface{}{"root": map[string]interface{}{"nodeId": 1, "nodeName": "#document"}})
	var selector string
	mock.Handle("DOM.querySelector", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Selector string `json:"selector"`
		}
		_ = json.Unmarshal(params, &p)
		selector = p.Selector
		return map[string]int{"nodeId": 77}, nil
	})
	var nodeID int
	mock.Handle("Page.snapshotNode", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			NodeID int `json:"nodeId"`
		}
		_ = json.Unmarshal(params, &p)
		nodeID = p.NodeID
		return map[string]string{"dataURL": "data:image/png;base64,node"}, nil
	})
	dataURL, err := tools.CaptureScreenshot(context.Background(), client, tools.ScreenshotOptions{Selector: "#card"})
	if err != nil {
		t.Fatalf("CaptureScreenshot: %v", err)
	}
	if selector != "#card" || nodeID != 77 || dataURL != "data:image/png;base64,node" {
		t.Errorf("selector=%q nodeId=%d dataURL=%q", selector, nodeID, dataURL)
	}
}

func TestCaptureScreenshotRejectsFullPageWithSelector(t *testing.T) {
	mock, client := setup(t)
	_, err := tools.CaptureScreenshot(context.Background(), client, tools.ScreenshotOptions{FullPage: true, Selector: "#a"})
	if err == nil {
		t.Fatal("expected an error for full page plus selector")
	}
	if n := len(mock.ReceivedMethods()); n != 0 {
		t.Errorf("sent %d commands before rejecting the options", n)
	}
}

// --- CSS guard through Target routing ---

func TestCSSStylesheetToolsNeverSendThroughTargetRouting(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.EnableTargetRouting("page-1")
	client, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !client.IsTargetRouted() {
		t.Fatal("expected Target routing")
	}
	ctx := context.Background()
	if _, err := tools.GetAllStylesheets(ctx, client); err == nil {
		t.Error("GetAllStylesheets: expected an error through Target routing")
	}
	if _, err := tools.GetStylesheetText(ctx, client, "ss1"); err == nil {
		t.Error("GetStylesheetText: expected an error through Target routing")
	}
	for _, m := range mock.ReceivedMethods() {
		if strings.HasPrefix(m, "CSS.") {
			t.Errorf("sent %s through Target routing", m)
		}
	}
}

// --- Bounded collectors ---

func TestConsoleCollectorKeepsNewestEntriesUpToCap(t *testing.T) {
	mock, client := setup(t)
	cc := tools.NewConsoleCollector()
	if err := cc.Start(context.Background(), client); err != nil {
		t.Fatalf("Start: %v", err)
	}
	const total = 10005
	var seen atomic.Int64
	client.OnEvent("Console.messageAdded", func(string, json.RawMessage) { seen.Add(1) })
	for i := 0; i < total; i++ {
		_ = mock.SendEvent("Console.messageAdded", map[string]interface{}{
			"message": map[string]interface{}{"level": "log", "text": fmt.Sprintf("msg-%d", i)},
		})
	}
	waitFor(t, func() bool { return seen.Load() == total })
	msgs := cc.GetMessages()
	if len(msgs) != 10000 {
		t.Fatalf("collector holds %d messages, want 10000", len(msgs))
	}
	if msgs[0].Text != "msg-5" || msgs[len(msgs)-1].Text != fmt.Sprintf("msg-%d", total-1) {
		t.Errorf("kept %q..%q, want the newest 10000", msgs[0].Text, msgs[len(msgs)-1].Text)
	}
}

func TestNetworkMonitorStopsAtCap(t *testing.T) {
	mock, client := setup(t)
	nm := tools.NewNetworkMonitor()
	if err := nm.Start(context.Background(), client); err != nil {
		t.Fatalf("Start: %v", err)
	}
	const total = 10005
	var seen atomic.Int64
	client.OnEvent("Network.requestWillBeSent", func(string, json.RawMessage) { seen.Add(1) })
	for i := 0; i < total; i++ {
		_ = mock.SendEvent("Network.requestWillBeSent", map[string]interface{}{
			"requestId": fmt.Sprintf("r%d", i),
			"request":   map[string]interface{}{"url": "https://example.com/", "method": "GET"},
		})
	}
	waitFor(t, func() bool { return seen.Load() == total })
	if n := len(nm.GetRequests()); n != 10000 {
		t.Errorf("monitor holds %d requests, want the cap of 10000", n)
	}
}

// TestTrackingStopIsBoundedWithoutDeadline: Stop waits for a
// *.trackingComplete event; when the page never sends it and the caller's
// context has no deadline, Stop must still return.
func TestTrackingStopIsBoundedWithoutDeadline(t *testing.T) {
	old := webkit.DefaultSendTimeout
	webkit.DefaultSendTimeout = 200 * time.Millisecond
	defer func() { webkit.DefaultSendTimeout = old }()

	type stopper func(ctx context.Context, client *webkit.Client) error
	cases := map[string]func(*webkit.Client) (stopper, error){
		"memory": func(c *webkit.Client) (stopper, error) {
			col := tools.NewMemoryTrackingCollector()
			return func(ctx context.Context, c *webkit.Client) error { _, err := col.Stop(ctx, c); return err }, col.Start(context.Background(), c)
		},
		"cpu": func(c *webkit.Client) (stopper, error) {
			col := tools.NewCPUProfilerCollector()
			return func(ctx context.Context, c *webkit.Client) error { _, err := col.Stop(ctx, c); return err }, col.Start(context.Background(), c)
		},
		"script": func(c *webkit.Client) (stopper, error) {
			col := tools.NewScriptProfilerCollector()
			return func(ctx context.Context, c *webkit.Client) error { _, err := col.Stop(ctx, c); return err }, col.Start(context.Background(), c)
		},
		"animation": func(c *webkit.Client) (stopper, error) {
			col := tools.NewAnimationTrackingCollector()
			return func(ctx context.Context, c *webkit.Client) error { _, err := col.Stop(ctx, c); return err }, col.Start(context.Background(), c)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			_, client := setup(t)
			stop, err := mk(client)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- stop(context.Background(), client) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not return without a trackingComplete event")
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
