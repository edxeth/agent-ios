//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
)

func gtkEpoch(t *testing.T, session string) uint64 {
	t.Helper()
	r := cli(t, "--session", session, "session", "info", "--json")
	var info struct {
		Pages []struct {
			Epoch uint64 `json:"epoch"`
		} `json:"pages"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &info); r.code != 0 || err != nil || len(info.Pages) != 1 {
		t.Fatalf("session info of %s: exit %d, stdout %q (%v); want one leased page", session, r.code, r.stdout, err)
	}
	return info.Pages[0].Epoch
}

func gtkCall(t *testing.T, c *cdptest.Client, session, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := c.Call(ctx, session, method, params)
	if err != nil {
		t.Fatalf("CDP %s: %v", method, err)
	}
	return res
}

func gtkLastAttach(t *testing.T, c *cdptest.Client) (sessionID, kind string) {
	t.Helper()
	var a struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	for _, e := range c.Events() {
		if e.Method == "Target.attachedToTarget" {
			_ = json.Unmarshal(e.Params, &a)
		}
	}
	return a.SessionID, a.TargetInfo.Type
}

// W5a smoke on WebKitGTK (design 7.8 item 11 (a) and 8.2): after a CLI
// snapshot, a Go CDP client connects through the exact URL that
// "get cdp-url" printed, attaches the tab and then the page as Puppeteer
// does, and evaluates in the real page; the session's lease epoch is
// unchanged and the snapshot's ref still works in a later CLI process;
// after close the URL gets 404.
func TestCDPClientEvaluatesThroughGetCDPURL(t *testing.T) {
	addr := runtimeBrowser(t, "forms.html")
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	if r := cli(t, "cdp", "start", "--port", port); r.code != 0 || !strings.Contains(r.stdout, "127.0.0.1:"+port) {
		t.Fatalf("cdp start --port %s: exit %d, stdout %q, stderr %q", port, r.code, r.stdout, r.stderr)
	}
	ref := snapshotRef(t, "s1")
	epoch := gtkEpoch(t, "s1")
	r := cli(t, "--session", "s1", "get", "cdp-url")
	ws := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.HasPrefix(ws, "ws://127.0.0.1:"+port+"/t/") {
		t.Fatalf("get cdp-url: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer c.Close()
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, kind := gtkLastAttach(t, c)
	if kind != "tab" {
		t.Fatalf("browser auto-attach attached a %q, want the tab", kind)
	}
	gtkCall(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	page, kind := gtkLastAttach(t, c)
	if kind != "page" || page == tab {
		t.Fatalf("tab auto-attach attached a %q in session %q, want the page", kind, page)
	}
	var ev struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Runtime.evaluate", map[string]any{"expression": "document.title + ':' + (6*7)", "returnByValue": true}), &ev); err != nil || ev.Result.Value != "Forms:42" {
		t.Fatalf("Runtime.evaluate in the page = %+v (%v), want Forms:42", ev, err)
	}
	if got := gtkEpoch(t, "s1"); got != epoch {
		t.Fatalf("s1's lease epoch went from %d to %d after the CDP attach", epoch, got)
	}
	if r := cli(t, "--session", "s1", "get", "text", "@e"+strconv.Itoa(ref)); r.code != 0 || !strings.HasPrefix(r.stdout, "Sign In") {
		t.Fatalf("get text @e%d after the CDP attach: exit %d, stdout %q, stderr %q", ref, r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "--session", "s1", "close"); r.code != 0 {
		t.Fatalf("close: exit %d, stderr %q", r.code, r.stderr)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if _, resp, err := cdptest.Dial(dctx, ws, nil); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the URL after close: %v, %v; want a refused upgrade with 404", err, resp)
	}
}
