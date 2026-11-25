package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type logEntry struct {
	Entry struct {
		Source, Level, Text, URL string
		LineNumber               int
	}
}

// Design 7.8 item 6 (5.2 "Dialogs"): Page.handleJavaScriptDialog returns
// the unsupported error; no javascriptDialogOpening is emitted; an
// automatic answer of the session's policy appears as a Log.entryAdded
// warning; a later dialog is answered by the session policy, never by an
// earlier CDP call. Page.enable installs the session's default policy
// (D6: dismiss) when the session has none, and keeps it afterwards.
func TestDialogs(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	_, err := c.Call(ctxFor(t, 5*time.Second), sid, "Page.handleJavaScriptDialog", map[string]any{"accept": true, "promptText": "yes"})
	if err == nil || !strings.Contains(err.Error(), "dynamic dialog handling is not supported on iOS WebKit; pre-arm answers with agent-ios dialog accept|dismiss") {
		t.Fatalf("handleJavaScriptDialog: %v", err)
	}
	call(t, c, sid, "Page.enable", nil)
	boots := f.ReceivedMethod("Page.setBootstrapScript")
	policy := `window.__iwdp.call(\"dialogs\", {policy: \"dismiss\"})`
	if len(boots) == 0 || !strings.Contains(string(boots[len(boots)-1].Params), policy) {
		t.Fatalf("%d bootstrap scripts, the last without the session's dismiss policy", len(boots))
	}
	if !strings.Contains(strings.Join(fp.runtimeCalls(), "\n"), `cdpDialogs {"policy":"dismiss"}`) {
		t.Fatalf("runtime calls %v, want the policy installed in the current document", fp.runtimeCalls())
	}
	n := len(f.ReceivedMethod("Page.setBootstrapScript"))
	call(t, c, sid, "Page.enable", nil)
	for _, b := range f.ReceivedMethod("Page.setBootstrapScript")[n:] {
		if n := strings.Count(string(b.Params), policy); n != 1 {
			t.Fatalf("a bootstrap script after the second Page.enable has %d dialog parts, want the one", n)
		}
	}
	call(t, c, sid, "Runtime.enable", nil)
	call(t, c, sid, "Log.enable", nil)

	seq := c.Seq()
	fp.consoleDebug(dialogMarker + `{"id":"","type":"confirm","message":"Delete?","result":false}`)
	fp.consoleDebug(dialogMarker + `{"id":"d3","type":"prompt","message":"Name?","result":"Ann"}`)
	e := decode[logEntry](t, waitEvent(t, c, seq, sid, "Log.entryAdded", nil).Params)
	if e.Entry.Level != "warning" || e.Entry.Source != "other" || !strings.HasPrefix(e.Entry.Text, `dialog confirm "Delete?" answered false by the session's dialog policy; dynamic dialog handling is not supported`) {
		t.Fatalf("the policy answer's entry %+v", e)
	}
	e = decode[logEntry](t, waitEvent(t, c, seq, sid, "Log.entryAdded", func(p json.RawMessage) bool { return strings.Contains(string(p), "Name?") }).Params)
	if !strings.Contains(e.Entry.Text, `answered "Ann" by the answer d3 armed with agent-ios dialog`) {
		t.Fatalf("the armed answer's entry %+v", e)
	}
	for _, ev := range c.Events() {
		if strings.HasPrefix(ev.Method, "Page.javascriptDialog") || (ev.Method == "Runtime.consoleAPICalled" && strings.Contains(string(ev.Params), "__iwdp_")) {
			t.Fatalf("the bridge sent %s %s", ev.Method, ev.Params)
		}
	}
	// The earlier CDP answer was never sent to the page: the only runtime
	// calls are the bridge's hooks and the policy.
	for _, rc := range fp.runtimeCalls() {
		if !strings.HasPrefix(rc, "cdpNav ") && !strings.HasPrefix(rc, "cdpDialogs ") {
			t.Fatalf("runtime call %q after handleJavaScriptDialog", rc)
		}
	}

	// Log.disable stops the entries.
	call(t, c, sid, "Log.disable", nil)
	seq = c.Seq()
	fp.consoleDebug(dialogMarker + `{"id":"","type":"alert","message":"Hi","result":null}`)
	fp.consoleDebug("sync")
	waitEvent(t, c, seq, sid, "Runtime.consoleAPICalled", nil)
	if ev := sessionEvents(c, seq, sid, "Log.entryAdded"); len(ev) != 0 {
		t.Fatalf("Log entries after Log.disable: %v", ev)
	}
}

// Log entries come from console messages other than console API calls and
// JavaScript errors, with source network, security or other.
func TestLogEntries(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	call(t, c, sid, "Log.enable", nil)
	seq := c.Seq()
	for _, m := range []map[string]any{
		{"source": "network", "level": "error", "text": "Failed to load resource", "url": "https://one.example/x.png", "line": 3, "networkRequestId": "0.7"},
		{"source": "security", "level": "warning", "text": "Mixed content"},
		{"source": "rendering", "level": "debug", "text": "Layout"},
	} {
		fp.f.Emit("Console.messageAdded", map[string]any{"message": m})
	}
	waitEvent(t, c, seq, sid, "Log.entryAdded", func(p json.RawMessage) bool { return strings.Contains(string(p), "Layout") })
	var got []string
	for _, ev := range sessionEvents(c, seq, sid, "Log.entryAdded") {
		e := decode[logEntry](t, ev.Params).Entry
		got = append(got, e.Source+"/"+e.Level)
	}
	if strings.Join(got, ",") != "network/error,security/warning,other/verbose" {
		t.Fatalf("Log entries %v", got)
	}
	first := decode[logEntry](t, sessionEvents(c, seq, sid, "Log.entryAdded")[0].Params).Entry
	if first.URL != "https://one.example/x.png" || first.LineNumber != 2 {
		t.Fatalf("network entry %+v, want its URL and the 0-based line", first)
	}
}
