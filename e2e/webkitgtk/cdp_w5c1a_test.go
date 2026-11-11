//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
)

// W5c-1a on WebKitGTK (design 2.10, 5.2 "Navigation identity", 5.3;
// acceptance 7.8 items 1, 5 and 12): the CDP Network and Fetch domains of
// a client of session s1 in a real browser. Expected values come from the
// test's own servers and from the CDP definitions.

// w5c1aPage starts MiniBrowser on url with the suite's daemon and its CDP
// endpoint, and attaches a CDP client to the page as Puppeteer does. It
// returns the client and the page session.
func w5c1aPage(t *testing.T, url string) (*cdptest.Client, string) {
	t.Helper()
	devtoolsBrowser(t, url)
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	mustCLI(t, "cdp", "start", "--port", port)
	mustCLI(t, "--session", "s1", "snapshot")
	ws := strings.TrimSpace(mustCLI(t, "--session", "s1", "get", "cdp-url"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL %s: %v", ws, err)
	}
	t.Cleanup(c.Close)
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	gtkCall(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	page, kind := gtkLastAttach(t, c)
	if kind != "page" {
		t.Fatalf("tab auto-attach attached a %q, want the page", kind)
	}
	return c, page
}

type gtkNetEvent struct {
	RequestID string
	LoaderID  string
	FrameID   string
	Type      string
	Request   struct {
		URL, Method, PostData string
		HasPostData           bool
	}
	Response struct {
		URL      string
		Status   int
		MimeType string
	}
}

// gtkNet waits for the Network event method of the request for url.
func gtkNet(t *testing.T, c *cdptest.Client, after int, page, method, url string) gtkNetEvent {
	t.Helper()
	var reqID string
	if method != "Network.requestWillBeSent" {
		reqID = gtkNet(t, c, after, page, "Network.requestWillBeSent", url).RequestID
	}
	var e gtkNetEvent
	raw := gtkEvent(t, c, after, page, method, func(p json.RawMessage) bool {
		var x gtkNetEvent
		_ = json.Unmarshal(p, &x) // a mismatch is no match
		return (reqID == "" && x.Request.URL == url) || (reqID != "" && x.RequestID == reqID)
	})
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// gtkNavigate runs Page.navigate to url and checks navigation identity
// (5.2, 7.8 item 5): its loader id is the frameNavigated's and the
// document request's id and loader id.
func gtkNavigate(t *testing.T, c *cdptest.Client, page, mainFrame, url string) {
	t.Helper()
	seq := c.Seq()
	var nav struct{ FrameID, LoaderID, ErrorText string }
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.navigate", map[string]any{"url": url}), &nav); err != nil || nav.LoaderID == "" || nav.ErrorText != "" {
		t.Fatalf("Page.navigate %s = %+v (%v)", url, nav, err)
	}
	doc := gtkNet(t, c, seq, page, "Network.requestWillBeSent", url)
	var fn struct {
		Frame struct{ ID, LoaderID, URL string }
	}
	if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Page.frameNavigated", func(p json.RawMessage) bool {
		return strings.Contains(string(p), `"url":"`+url+`"`)
	}), &fn); err != nil {
		t.Fatal(err)
	}
	if doc.RequestID != nav.LoaderID || doc.LoaderID != nav.LoaderID || fn.Frame.LoaderID != nav.LoaderID ||
		doc.Type != "Document" || doc.FrameID != mainFrame || fn.Frame.ID != mainFrame {
		t.Fatalf("navigation to %s: Page.navigate %+v, document request %+v, frameNavigated %+v; want one loader id in all three and the main frame %s",
			url, nav, doc, fn.Frame, mainFrame)
	}
	gtkNet(t, c, seq, page, "Network.loadingFinished", url)
}

// The events of a page load with a subresource and a POST, the bodies,
// and navigation identity, also across a process swap (127.0.0.1 to
// localhost are two sites, devtools_test.go).
func TestCDPNetworkOnWebKitGTK(t *testing.T) {
	a, b := newSite(t, "127.0.0.1"), newSite(t, "localhost")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	a.html("/load.html", `<!doctype html><title>Load</title><script src="/sub.js"></script>`)
	a.handle("/sub.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = io.WriteString(w, "window.__sub = 'sub-ok'")
	})
	a.handle("/post", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) // a short read fails the comparison below
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, r.Method+":"+string(body))
	})
	b.html("/b.html", "<!doctype html><title>B</title><p>site B")
	c, page := w5c1aPage(t, a.url+"/")
	for _, m := range []string{"Page.enable", "Runtime.enable", "Network.enable"} {
		gtkCall(t, c, page, m, nil)
	}
	var tree struct {
		FrameTree struct{ Frame struct{ ID string } }
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.getFrameTree", nil), &tree); err != nil || tree.FrameTree.Frame.ID == "" {
		t.Fatalf("frame tree %+v (%v)", tree, err)
	}
	mainFrame := tree.FrameTree.Frame.ID

	seq := c.Seq()
	gtkNavigate(t, c, page, mainFrame, a.url+"/load.html")
	sub := gtkNet(t, c, seq, page, "Network.requestWillBeSent", a.url+"/sub.js")
	resp := gtkNet(t, c, seq, page, "Network.responseReceived", a.url+"/sub.js")
	gtkNet(t, c, seq, page, "Network.loadingFinished", a.url+"/sub.js")
	if sub.Type != "Script" || sub.FrameID != mainFrame || sub.RequestID == sub.LoaderID || resp.Type != "Script" || resp.Response.Status != 200 || resp.Response.MimeType != "text/javascript" {
		t.Fatalf("subresource request %+v and response %+v, want a Script of the main frame with its own id, status 200", sub, resp)
	}
	var body struct {
		Body          string
		Base64Encoded bool
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Network.getResponseBody", map[string]any{"requestId": sub.RequestID}), &body); err != nil || body.Body != "window.__sub = 'sub-ok'" || body.Base64Encoded {
		t.Fatalf("getResponseBody of /sub.js = %+v (%v)", body, err)
	}

	seq = c.Seq()
	gtkValue(t, c, page, `fetch("/post", {method: "POST", body: "a=1&b=2"}); 1`, 0)
	post := gtkNet(t, c, seq, page, "Network.requestWillBeSent", a.url+"/post")
	gtkNet(t, c, seq, page, "Network.loadingFinished", a.url+"/post")
	if post.Request.Method != "POST" || !post.Request.HasPostData || post.Type != "Fetch" {
		t.Fatalf("POST request %+v, want method POST with post data, type Fetch", post)
	}
	var pd struct{ PostData string }
	if err := json.Unmarshal(gtkCall(t, c, page, "Network.getRequestPostData", map[string]any{"requestId": post.RequestID}), &pd); err != nil || pd.PostData != "a=1&b=2" {
		t.Fatalf("getRequestPostData = %+v (%v), want a=1&b=2", pd, err)
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Network.getResponseBody", map[string]any{"requestId": post.RequestID}), &body); err != nil || body.Body != "POST:a=1&b=2" {
		t.Fatalf("getResponseBody of the POST = %+v (%v)", body, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := c.Call(ctx, page, "Network.getResponseBody", map[string]any{"requestId": "no-such-request"})
	var ce *cdptest.Error
	if !errors.As(err, &ce) || ce.Message != "No resource with given identifier found" {
		t.Fatalf("getResponseBody of an unknown request: %v, want Chrome's error", err)
	}

	// A cross-process navigation: the new process's document request.
	gtkNavigate(t, c, page, mainFrame, b.url+"/b.html")
	if v := gtkValue(t, c, page, "document.title", 0); v != "B" {
		t.Fatalf("title after the swap = %v", v)
	}
}

// gtkAwait evaluates expr with awaitPromise in a goroutine, bounded by
// 10 s (item 12), and delivers its string value or error text.
func gtkAwait(c *cdptest.Client, page, expr string) <-chan string {
	out := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := c.Call(ctx, page, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true})
		if err != nil {
			out <- "error: " + err.Error()
			return
		}
		var r struct{ Result struct{ Value any } }
		_ = json.Unmarshal(raw, &r) // a bad reply is a missing value below
		s, _ := r.Result.Value.(string)
		out <- s
	}()
	return out
}

// gtkPaused waits for the Fetch.requestPaused of url and returns its id.
func gtkPaused(t *testing.T, c *cdptest.Client, after int, page, url string) string {
	t.Helper()
	var p struct {
		RequestID, NetworkID string
		Request              struct{ URL string }
	}
	if err := json.Unmarshal(gtkEvent(t, c, after, page, "Fetch.requestPaused", func(raw json.RawMessage) bool {
		return strings.Contains(string(raw), `"url":"`+url+`"`)
	}), &p); err != nil || p.RequestID == "" || p.NetworkID == "" {
		t.Fatalf("requestPaused %+v (%v)", p, err)
	}
	return p.RequestID
}

// Item 12 (design 2.10, the control lane): an evaluation that awaits a
// fetch the client holds paused returns when the client answers it, with
// fulfillRequest, failRequest and continueRequest; and a detach continues
// a request that the client held, so the page's fetch completes.
func TestCDPFetchOnWebKitGTK(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	c, page := w5c1aPage(t, a.url+"/")
	gtkCall(t, c, page, "Runtime.enable", nil)
	gtkCall(t, c, page, "Network.enable", nil)
	gtkCall(t, c, page, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/resource*"}}})

	answer := func(name, query, method string, params map[string]any, expr, want string, served bool) {
		t.Helper()
		seq := c.Seq()
		done := gtkAwait(c, page, expr)
		id := gtkPaused(t, c, seq, page, a.url+"/resource"+query)
		params["requestId"] = id
		start := time.Now()
		gtkCall(t, c, page, method, params)
		select {
		case got := <-done:
			if got != want || time.Since(start) > 2*time.Second {
				t.Fatalf("%s: the awaited evaluation returned %q after %v, want %q within 2 s", name, got, time.Since(start), want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the awaited evaluation did not return within 5 s of the answer", name)
		}
		if got := len(a.got("/resource" + query)); (got > 0) != served {
			t.Fatalf("%s: the server got %d requests for /resource%s, want served=%v", name, got, query, served)
		}
	}
	answer("fulfill", "", "Fetch.fulfillRequest", map[string]any{"responseCode": 200, "body": base64.StdEncoding.EncodeToString([]byte("ok"))},
		"fetch('/resource').then(r => r.text())", "ok", false)
	answer("fail", "?f", "Fetch.failRequest", map[string]any{"errorReason": "Failed"},
		"fetch('/resource?f').then(r => 'loaded', e => 'failed:' + e.name)", "failed:TypeError", false)
	answer("continue", "?c", "Fetch.continueRequest", map[string]any{},
		"fetch('/resource?c').then(r => r.text())", "server:/resource", true)

	// A detach with a paused request: the page's fetch completes.
	seq := c.Seq()
	gtkValue(t, c, page, "window.__w5c1a = 'pending'; fetch('/resource?d').then(r => r.text()).then(t => { window.__w5c1a = t }); 1", 0)
	gtkPaused(t, c, seq, page, a.url+"/resource?d")
	gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
	var got string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if got = strings.TrimSpace(mustCLI(t, "--session", "s1", "eval", "window.__w5c1a")); got == "server:/resource" {
			break
		}
	}
	if got != "server:/resource" || len(a.got("/resource?d")) != 1 {
		t.Fatalf("after the detach the page's fetch left %q and the server got %d requests, want the server's answer", got, len(a.got("/resource?d")))
	}
}

// Review F6 (repair round 1): at the response stage a fulfill without
// body, which in CDP keeps the original body, is refused (WIP would
// replace the body), so the original response is never erased; the
// client can still continue it, and the page gets the server's body.
func TestCDPResponseFulfillWithoutBodyOnWebKitGTK(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title>")
	c, page := w5c1aPage(t, a.url+"/")
	gtkCall(t, c, page, "Runtime.enable", nil)
	gtkCall(t, c, page, "Network.enable", nil)
	gtkCall(t, c, page, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/resource", "requestStage": "Response"}}})
	seq := c.Seq()
	done := gtkAwait(c, page, "fetch('/resource').then(r => r.text())")
	id := gtkPaused(t, c, seq, page, a.url+"/resource")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := c.Call(ctx, page, "Fetch.fulfillRequest", map[string]any{"requestId": id, "responseCode": 200, "responseHeaders": []any{map[string]any{"name": "X-Review", "value": "yes"}}})
	var ce *cdptest.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "without body") {
		t.Fatalf("fulfill without body at the response stage: %v, want a refusal", err)
	}
	gtkCall(t, c, page, "Fetch.continueRequest", map[string]any{"requestId": id})
	select {
	case got := <-done:
		if got != "server:/resource" {
			t.Fatalf("response body %q, want the original %q", got, "server:/resource")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch did not finish")
	}
}
