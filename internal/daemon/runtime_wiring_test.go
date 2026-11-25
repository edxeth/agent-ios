package daemon

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// scriptedPage answers runtime calls of the fake page with fixed
// envelopes and records each call with the owner its expression carries.
type scriptedPage struct {
	mu    sync.Mutex
	calls []scriptedCall
}

type scriptedCall struct {
	fn     string
	args   map[string]any
	owners string // the JSON argument of window.__iwdp.own, "" without one
}

func (p *scriptedPage) handler(c fakewebkit.Call, fn string, args json.RawMessage) any {
	var params struct{ Expression string }
	_ = json.Unmarshal(c.Params, &params)
	owner := ""
	if _, rest, ok := strings.Cut(params.Expression, "window.__iwdp.own("); ok {
		owner, _, _ = strings.Cut(rest, "))")
	}
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	p.mu.Lock()
	p.calls = append(p.calls, scriptedCall{fn, a, owner})
	p.mu.Unlock()
	ok := func(v any) any { return map[string]any{"ok": true, "value": v} }
	switch fn {
	case "snapshot":
		return ok(map[string]any{
			"tree":    []any{map[string]any{"role": "button", "name": "Save", "ref": "e1"}},
			"refs":    []any{map[string]any{"ref": "e1", "role": "button", "name": "Save", "unique": true}},
			"nextRef": 2, "doc": "d1", "url": "http://x.test/", "title": "X",
		})
	case "get":
		return ok("Save")
	case "ping":
		return ok(true)
	}
	return map[string]any{"ok": false, "error": map[string]string{"code": "unsupported", "message": "fake page has no " + fn}}
}

func (p *scriptedPage) last(fn string) (scriptedCall, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.calls) - 1; i >= 0; i-- {
		if p.calls[i].fn == fn {
			return p.calls[i], true
		}
	}
	return scriptedCall{}, false
}

// W3a wiring (docs/notes/w1b.md "Hooks for W3a"; design 3.1, 3.3): the
// registry commands of the agent runtime run in the daemon. A snapshot
// and a later "get text @e1" from another client connection (another CLI
// process) share the page's ref table, and every runtime call carries the
// daemon's id with the session and its lease epoch as the owner.
func TestRuntimeCommandsRunInTheDaemon(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	page := &scriptedPage{}
	f.SetRuntime(page.handler)
	runAs := func(session string, argv ...string) (RunResult, string, error) {
		c := connect(t, o) // one client per command, as one CLI process each
		res, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: argv, Owner: Owner{Session: session}, PageReq: PageReq{Page: f.URL}}, nil)
		return res, c.Hello.ID, err
	}
	snap, daemonID, err := runAs("s1", "snapshot")
	if err != nil {
		t.Fatalf("snapshot through the daemon: %v", err)
	}
	if !strings.Contains(snap.Text, `button "Save" [ref=e1]`) {
		t.Fatalf("snapshot = %q, want the button with ref e1", snap.Text)
	}
	got, _, err := runAs("s1", "get", "text", "@e1")
	if err != nil {
		t.Fatalf("get text @e1 after the snapshot: %v", err)
	}
	if got.Text != "Save" {
		t.Fatalf("get text @e1 = %q, want %q", got.Text, "Save")
	}
	call, ok := page.last("get")
	if !ok {
		t.Fatal("the page received no get call")
	}
	if target, _ := call.args["target"].(map[string]any); target["ref"] != "e1" || target["role"] != "button" || target["name"] != "Save" {
		t.Fatalf("get sent target %v, want ref e1 with the role and name of the snapshot", call.args["target"])
	}
	var owner struct {
		Daemon, Session string
		Epoch           uint64
	}
	if err := json.Unmarshal([]byte(call.owners), &owner); err != nil {
		t.Fatalf("the get call carries no owner: %q (%v)", call.owners, err)
	}
	if owner.Daemon != daemonID || owner.Session != "s1" || owner.Epoch == 0 {
		t.Fatalf("owner = %+v, want daemon %q (the hello id), session s1 and its lease epoch", owner, daemonID)
	}
	if boots := f.ReceivedMethod("Page.setBootstrapScript"); len(boots) != 1 || !strings.Contains(string(boots[0].Params), "iwdp bootstrap part: runtime") {
		t.Fatalf("the daemon sent %d bootstrap scripts, want one with the runtime part", len(boots))
	}
}
