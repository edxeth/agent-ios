package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// fakePage scripts the WIP side of page "One" for the W5b-1 domain tests:
// a resource tree with main frame "main" (and optional child frames), the
// agent runtime's calls (recorded, answered ok), and page expressions:
// location.href starts a navigation (a new document with the next WIP
// loader id, or a same-document one with the hook's marked message),
// document.readyState is "complete", and eval answers the rest. As
// WebKit does, an expression with the contextId of a replaced document's
// main world fails (stale); before runs first for each page expression,
// to race a navigation with the call. With replay, Runtime.enable reports
// the current document's main world before its reply, as WebKit does when
// its Runtime agent was off; without it, the agent counts as enabled by
// another owner before, so the attachment's events never report the
// world.
type fakePage struct {
	f *fakewebkit.Server

	mu       sync.Mutex
	loader   int
	url      string
	ctxID    int
	children []wipTree
	rtCalls  []string // "fn args"
	exprs    []string
	eval     func(expr string) (any, bool) // the result object of other expressions
	before   func(expr string)
	stale    map[int]bool // main-world ids of replaced documents
	replay   bool
}

func newFakePage(f *fakewebkit.Server) *fakePage {
	fp := &fakePage{f: f, loader: 1, url: "https://one.example/", ctxID: 1, stale: map[int]bool{}}
	f.Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return map[string]any{"frameTree": map[string]any{
			"frame":       fp.mainFrame(),
			"childFrames": fp.children,
			"resources":   []any{map[string]any{"url": fp.url + "app.js", "type": "Script", "mimeType": "text/javascript"}},
		}}, nil
	})
	f.Handle("Runtime.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		fp.mu.Lock()
		replay, id := fp.replay, fp.ctxID
		fp.mu.Unlock()
		if replay {
			f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "type": "normal", "name": "", "frameId": "main"}})
		}
		return map[string]any{}, nil
	})
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
			ContextID  int    `json:"contextId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			fp.mu.Lock()
			fp.rtCalls = append(fp.rtCalls, m[1]+" "+m[2])
			fp.mu.Unlock()
			return value(map[string]any{"ok": true, "value": true}), nil
		}
		fp.mu.Lock()
		before := fp.before
		fp.mu.Unlock()
		if before != nil {
			before(p.Expression)
		}
		fp.mu.Lock()
		if fp.stale[p.ContextID] {
			fp.mu.Unlock()
			return nil, &fakewebkit.Error{Code: -32000, Message: "Missing injected script for given executionContextId"}
		}
		fp.exprs = append(fp.exprs, p.Expression)
		eval := fp.eval
		fp.mu.Unlock()
		switch {
		case p.Expression == "document.readyState":
			return value("complete"), nil
		case strings.HasPrefix(p.Expression, "location.href = "):
			var u string
			_ = json.Unmarshal([]byte(strings.TrimPrefix(p.Expression, "location.href = ")), &u)
			fp.navigate(u)
			return value(u), nil
		}
		if eval != nil {
			if r, ok := eval(p.Expression); ok {
				return r, nil
			}
		}
		return value(0), nil
	})
	return fp
}

func value(v any) map[string]any {
	return map[string]any{"result": map[string]any{"type": fmt.Sprintf("%T", v), "value": v}}
}

func (fp *fakePage) mainFrame() map[string]any {
	return map[string]any{"id": "main", "loaderId": fmt.Sprintf("0.%d", fp.loader), "url": fp.url, "securityOrigin": "https://one.example", "mimeType": "text/html"}
}

// navigate makes the page go to u as WebKit reports it: a fragment
// change of the current URL is a same-document navigation (the hook's
// marked console.debug message); anything else is a new document
// (frameNavigated with the next loader id, then its main world).
func (fp *fakePage) navigate(u string) {
	fp.mu.Lock()
	base, _, _ := strings.Cut(fp.url, "#")
	nb, _, hasFrag := strings.Cut(u, "#")
	fp.url = u
	if hasFrag && nb == base {
		fp.mu.Unlock()
		fp.consoleDebug(navMarker + fmt.Sprintf(`{"u":%q,"t":"fragment"}`, u))
		return
	}
	fp.loader++
	fp.stale[fp.ctxID] = true
	fp.ctxID++
	frame, id := fp.mainFrame(), fp.ctxID
	fp.mu.Unlock()
	fp.f.Emit("Page.frameNavigated", map[string]any{"frame": frame})
	fp.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "type": "normal", "name": "", "frameId": "main"}})
}

// consoleDebug emits a console.debug message of the page.
func (fp *fakePage) consoleDebug(text string) {
	fp.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "console-api", "level": "debug", "type": "log", "text": text,
		"parameters": []any{map[string]any{"type": "string", "value": text}},
	}})
}

func (fp *fakePage) runtimeCalls() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]string(nil), fp.rtCalls...)
}

func (fp *fakePage) expressions() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]string(nil), fp.exprs...)
}

// attachPage attaches page One for session s1 as Puppeteer does (tab,
// then page) with fp scripting its WIP side; it returns the client, the
// page session id and the page target id.
func attachPage(t *testing.T) (*rig, *fakePage, *cdptest.Client, string, string) {
	t.Helper()
	r := newRig(t)
	fp := newFakePage(r.fake["One"])
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, sid := puppeteerAttach(t, c)
	var target string
	for _, e := range eventsOf(c, "Target.attachedToTarget") {
		if a := decode[attached](t, e.Params); a.SessionID == sid {
			target = a.TargetInfo.TargetID
		}
	}
	return r, fp, c, sid, target
}

// waitEvent waits for an event of method on the session sid after seq.
func waitEvent(t *testing.T, c *cdptest.Client, after int, sid, method string, match func(json.RawMessage) bool) cdptest.Event {
	t.Helper()
	e, err := c.WaitEvent(ctxFor(t, 5*time.Second), after, method, func(e cdptest.Event) bool {
		return e.SessionID == sid && (match == nil || match(e.Params))
	})
	if err != nil {
		t.Fatalf("%s on session %s: %v; events: %v", method, sid, err, methodsOf(c.Events()))
	}
	return e
}

func methodsOf(es []cdptest.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Method
	}
	return out
}

// sessionEvents lists the events of method on session sid after seq.
func sessionEvents(c *cdptest.Client, after int, sid, method string) []cdptest.Event {
	var out []cdptest.Event
	for _, e := range c.Events() {
		if e.Seq > after && e.SessionID == sid && e.Method == method {
			out = append(out, e)
		}
	}
	return out
}

// cdpContext is the context of a Runtime.executionContextCreated.
type cdpContext struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	UniqueID string `json:"uniqueId"`
	AuxData  struct {
		FrameID   string `json:"frameId"`
		IsDefault bool   `json:"isDefault"`
		Type      string `json:"type"`
	} `json:"auxData"`
}

func contextsOf(t *testing.T, c *cdptest.Client, after int, sid string) []cdpContext {
	t.Helper()
	var out []cdpContext
	for _, e := range sessionEvents(c, after, sid, "Runtime.executionContextCreated") {
		out = append(out, decode[struct{ Context cdpContext }](t, e.Params).Context)
	}
	return out
}
