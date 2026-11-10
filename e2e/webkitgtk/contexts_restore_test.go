//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Finding 1 of the core-events review (docs/notes/w1b.md, "core-events r1:
// contexts after a back/forward cache restore"), reproduced on WebKitGTK
// 2.54 by the reviewer: after history.back() restored a cached document,
// ExecutionContexts of the active target was empty. WebKit sends
// Page.frameNavigated for the restored document and no
// Runtime.executionContextCreated, and the ids recorded before the restore
// are invalid ("Missing injected script for given executionContextId").
// Now the inventory lists the restored document's main-world context, and
// every listed id evaluates in the restored document (its marker), for a
// same-process restore and for a restore across sites, whose target comes
// back with its old id.
//
// The page is attached through core's Manager, as the daemon does, and a
// legacy Runtime.enable makes its session an owner of Runtime: the session
// replays the contexts only as a change of the page's desired state while
// an owner needs Runtime (core-events r2, "the context replay and
// Runtime's owners").

// managedPage is a page that core's Manager attached, with one open
// operation of one session.
type managedPage struct {
	m  *core.Manager
	mo *core.ManagedOp
	pc interface {
		ActiveTarget() string
		ExecutionContexts(target string) []webkit.ExecutionContext
	}
}

// attachManaged starts broadwayd and MiniBrowser on url and attaches its
// page through a Manager on the WebKitGTK transport.
func attachManaged(t *testing.T, url string) *managedPage {
	t.Helper()
	m, req := managedBrowser(t, url)
	// The operation lives as long as Begin's context.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	mo, err := m.Begin(ctx, req, core.AcquireOptions{Session: "contexts"}, "contexts", core.ClassExclusive)
	if err != nil {
		t.Fatalf("attaching the page: %v", err)
	}
	t.Cleanup(func() { m.End(mo.ID) })
	pc, ok := mo.Page.PS.Caller.(interface {
		ActiveTarget() string
		ExecutionContexts(target string) []webkit.ExecutionContext
	})
	if !ok {
		t.Fatalf("the page connection %T has no context inventory", mo.Page.PS.Caller)
	}
	return &managedPage{m: m, mo: mo, pc: pc}
}

// managedBrowser starts broadwayd and MiniBrowser on url and returns a
// Manager on the WebKitGTK transport and the request of its page. The
// Manager closes at test cleanup.
func managedBrowser(t *testing.T, url string) (*core.Manager, core.PageRequest) {
	t.Helper()
	display := broadway(t)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr}, miniBrowser, url)
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(browserStart); ; time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("MiniBrowser did not list a page at %s within %v", addr, browserStart)
		}
	}
	m := core.NewManager(core.ManagerOptions{
		Transport: func(context.Context, string, string) (transport.Transport, error) { return tr, nil },
		Session:   webkit.SessionOptions{TargetWait: 12 * time.Second},
		Logf:      t.Logf,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m, core.PageRequest{Transport: "webkitgtk", Addr: addr}
}

// raw sends one legacy call inside the page's operation.
func (p *managedPage) raw(t *testing.T, method string, params any) (json.RawMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw json.RawMessage
	if params != nil {
		var err error
		if raw, err = json.Marshal(params); err != nil {
			t.Fatal(err)
		}
	}
	return p.m.RawAs(ctx, p.mo, core.SessionOwner("legacy", p.mo.Session), method, raw)
}

// evalIn evaluates expr in the active target, in context id when id > 0,
// and returns the string value.
func (p *managedPage) evalIn(t *testing.T, id int, expr string) (string, error) {
	t.Helper()
	params := map[string]any{"expression": "String(" + expr + ")", "returnByValue": true}
	if id > 0 {
		params["contextId"] = id
	}
	raw, err := p.raw(t, "Runtime.evaluate", params)
	if err != nil {
		return "", err
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Result.Value, nil
}

// restoreAndCheck enables Runtime, marks the document, loads next, goes
// back and checks the inventory of the restored document.
func restoreAndCheck(t *testing.T, p *managedPage, next, marker string) {
	t.Helper()
	if _, err := p.raw(t, "Runtime.enable", nil); err != nil {
		t.Fatalf("Runtime.enable: %v", err)
	}
	if len(p.pc.ExecutionContexts(p.pc.ActiveTarget())) == 0 {
		t.Fatal("no context before the navigation")
	}
	wait := func(what, expr, want string) {
		t.Helper()
		for end := time.Now().Add(8 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			got, err := p.evalIn(t, 0, expr)
			if err == nil && got == want {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("%s: %q, %v", what, got, err)
			}
		}
	}
	if _, err := p.evalIn(t, 0, fmt.Sprintf("window.__bfMarker = %q", marker)); err != nil {
		t.Fatalf("marking the document: %v", err)
	}
	if _, err := p.evalIn(t, 0, fmt.Sprintf("location.href = %q", next)); err != nil {
		t.Fatalf("navigating: %v", err)
	}
	wait("loading "+next, "location.href + ' ' + document.readyState", next+" complete")
	if _, err := p.evalIn(t, 0, "history.back()"); err != nil {
		t.Fatalf("history.back: %v", err)
	}
	wait("restoring the cached document", "window.__bfMarker", marker) // the cached document, not a reload
	var got []webkit.ExecutionContext
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if got = p.pc.ExecutionContexts(p.pc.ActiveTarget()); len(got) > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the restored document of target %s has no execution context in the inventory", p.pc.ActiveTarget())
		}
	}
	for _, c := range got {
		v, err := p.evalIn(t, c.ID, "window.__bfMarker")
		if err != nil || v != marker {
			t.Fatalf("context %+v of the restored document: %q, %v; want the marker %q", c, v, err, marker)
		}
	}
}

func TestContextInventoryAfterBFCacheRestore(t *testing.T) {
	sites := &twoSites{reqs: map[string][2]string{}}
	sites.a = sites.serve(t, "127.0.0.1")
	p := attachManaged(t, sites.a+"/a.html")
	restoreAndCheck(t, p, sites.a+"/a2.html", "cached-same-process")
}

func TestContextInventoryAfterCrossSiteBFCacheRestore(t *testing.T) {
	sites := &twoSites{reqs: map[string][2]string{}}
	sites.a, sites.b = sites.serve(t, "127.0.0.1"), sites.serve(t, "localhost")
	p := attachManaged(t, sites.a+"/a.html")
	first := p.pc.ActiveTarget()
	restoreAndCheck(t, p, sites.b+"/b.html", "cached-cross-site")
	if p.pc.ActiveTarget() != first {
		t.Fatalf("restored target %s, want the cached page's target %s", p.pc.ActiveTarget(), first)
	}
}
