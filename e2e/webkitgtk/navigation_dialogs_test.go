//go:build webkitgtk

package webkitgtk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/daemon"
)

// confirmOut clicks #confirm and returns the click output and what the
// page recorded ("confirm:true" or "confirm:false").
func (e *navEnv) confirmOut(session string) (string, string) {
	e.t.Helper()
	out := e.must(session, "click", "#confirm")
	return out, strings.Trim(e.js(session, "document.getElementById('out').textContent"), "\"")
}

// 7.6 item 7 (a), (d), (e): an armed prompt answer reaches the page; an
// unarmed confirm returns false and the click reports it; nothing blocks;
// Console stays enabled for the dialog bridge and the log lists each use;
// after release the native functions are back and the queue is empty.
func TestDialogBridgeOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "dialog.html", false)
	e.must(agentA, "dialog", "accept", "x")
	out := e.must(agentA, "click", "#prompt")
	if got := e.js(agentA, "document.getElementById('out').textContent"); got != `"prompt:\"x\""` {
		t.Fatalf("prompt answer in the page: %s (click output %q)", got, out)
	}
	if !strings.Contains(out, `dialog prompt "Your name?" answered "x" (answer d1)`) {
		t.Fatalf("click output %q does not report the answered prompt", out)
	}
	out, page := e.confirmOut(agentA)
	if page != "confirm:false" || !strings.Contains(out, `dialog confirm "Proceed?" answered false (no answer armed: session default)`) {
		t.Fatalf("unarmed confirm: page %q, click output %q", page, out)
	}
	if got := e.js(agentA, "1+1"); got != "2" {
		t.Fatalf("an evaluate after the dialogs: %s", got)
	}
	// (d) Console ownership and the dialog log.
	enabled, disabled := 0, 0
	for _, r := range withoutAgentResets(e.tap.records()) {
		if r.Out && r.Method == "Console.enable" {
			enabled++
		}
		if r.Out && r.Method == "Console.disable" {
			disabled++
		}
	}
	if enabled == 0 || disabled != 0 {
		t.Fatalf("Console.enable sent %d times, Console.disable %d times; want Console held by the dialog bridge", enabled, disabled)
	}
	status := e.must(agentA, "dialog", "status")
	if !strings.Contains(status, "dialog override: on") || !strings.Contains(status, `prompt "Your name?" answered "x"`) || !strings.Contains(status, `confirm "Proceed?" answered false`) {
		t.Fatalf("dialog status:\n%s", status)
	}
	// (e) Release restores the native functions and clears the queue.
	e.must(agentA, "dialog", "accept")
	e.must(agentA, "session", "release")
	if got := e.js(agentA, "String(window.confirm).indexOf('[native code]') >= 0 && String(window.prompt).indexOf('[native code]') >= 0"); got != "true" {
		t.Fatalf("after release window.confirm is not native: %s", e.js(agentA, "String(window.confirm)"))
	}
	status = e.must(agentA, "dialog", "status")
	if !strings.Contains(status, "answers waiting in the page: 0; queued in the daemon: 0") {
		t.Fatalf("dialog status after release:\n%s", status)
	}
}

// 7.6 item 7 (c): unused answers are withdrawn by the navigating call and
// issued into the new document; with that reply lost they expire.
func TestDialogWithdrawalOnOpen(t *testing.T) {
	e := newNavEnv(t, "dialog.html", false)
	e.must(agentA, "dialog", "accept")
	e.must(agentA, "open", e.sites.b+"/dialog.html")
	if out, page := e.confirmOut(agentA); page != "confirm:true" {
		t.Fatalf("confirm in the new document: %q (%s); want the withdrawn answer issued there", page, out)
	}
	if _, page := e.confirmOut(agentA); page != "confirm:false" {
		t.Fatalf("second confirm: %q, want false (the answer was written once)", page)
	}
	// The withdrawing call's reply is dropped: the answer expires.
	e.must(agentA, "dialog", "accept")
	e.tap.set(func(h *tapHooks) { h.drop = evalWith(`"withdraw":true`, `"nav":`) })
	out := e.must(agentA, "open", e.sites.a+"/dialog.html")
	if !strings.Contains(out, "expired with its document") {
		t.Fatalf("open with the withdrawal reply dropped: %q, want the answer expired", out)
	}
	if _, page := e.confirmOut(agentA); page != "confirm:false" {
		t.Fatalf("confirm after the lost withdrawal: %q, want false", page)
	}
	trues := 0
	for _, r := range e.recordsOf("confirm") {
		if r.Result == "true" {
			trues++
		}
	}
	if trues != 1 {
		t.Fatalf("the page recorded %d accepted confirms, want 1: %+v", trues, e.recordsOf("confirm"))
	}
}

// 7.6 item 7 (b), lost acknowledgment: the page uses an armed answer, but
// the click's reply and the console.debug marker are dropped and the page
// connection then closes without a request from the daemon. After a
// reattach with --takeover the answer is never written again: a confirm in
// the same document and one in a new cross-origin document both return
// false, the answer is reported as expired, and the fixture records the
// accept in exactly one document.
func TestDialogLostAcknowledgmentOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "dialog.html", false)
	e.must(agentA, "dialog", "accept")
	// The click's reply is the awaited act envelope that reports the use
	// of answer d1 (js/50-actionability.js r.dlg); the marker is the
	// Console.messageAdded event of js/80-dialogs.js.
	e.tap.set(func(h *tapHooks) {
		h.dropReply = func(result json.RawMessage) bool { return bytes.Contains(result, []byte(`"dlg":[{"id":"d1"`)) }
		h.dropEvent = func(method string, params json.RawMessage) bool {
			return method == "Console.messageAdded" && bytes.Contains(params, []byte("__iwdp_dialog__"))
		}
	})
	type result struct {
		out, msg string
		code     int
		err      error
	}
	clicked := make(chan result, 1)
	go func() {
		out, code, msg, err := e.runAs(daemon.Owner{Session: agentA}, "click", "#confirm")
		clicked <- result{out, msg, code, err}
	}()
	// Close the connection only after the page used the answer.
	for end := time.Now().Add(10 * time.Second); len(e.recordsOf("confirm")) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the page did not record the confirm within 10 s")
		}
	}
	if got := e.recordsOf("confirm"); got[0].Result != "true" {
		t.Fatalf("the click's confirm recorded %+v, want the armed accept", got)
	}
	// The page records the confirm before it sends the click's reply and
	// the marker. Close the connection only after the hook dropped both:
	// a close before they arrive loses them in the close instead, and the
	// drop counts below would be 0 (a race of this test, seen 2 of 6 runs).
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		replies, markers := droppedOf(e.tap.records())
		if replies >= 1 && markers >= 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the hook dropped %d replies and %d console.debug markers within 10 s, want the click's reply and its marker", replies, markers)
		}
	}
	e.tap.closeConns()
	var c result
	select {
	case c = <-clicked:
	case <-time.After(30 * time.Second):
		t.Fatal("the click did not end within 30 s after its connection closed")
	}
	if c.err != nil || c.code == 0 {
		t.Fatalf("click with its reply dropped and the connection closed: exit %d, %q, %v; want a failure", c.code, c.msg+c.out, c.err)
	}
	droppedReplies, droppedMarkers := droppedOf(e.tap.records())
	if droppedReplies != 1 || droppedMarkers != 1 {
		t.Fatalf("the hook dropped %d replies and %d console.debug markers, want the click's reply and its marker", droppedReplies, droppedMarkers)
	}
	e.tap.set(func(h *tapHooks) { h.dropReply, h.dropEvent = nil, nil })
	// Without --takeover the lost page stays lost (design 2.5 rule 3).
	if _, code, msg := e.run(agentA, "click", "#confirm"); code == 0 || !strings.Contains(msg, "ownership is unknown") {
		t.Fatalf("click on the lost page without --takeover: exit %d, %q; want the lost-page error", code, msg)
	}
	// The takeover reconnects right after the close. On WebKitGTK such a
	// connection sometimes gets no Target.targetCreated (design section 9,
	// R2); webkit.Dial then closes it and retries after a gap, so the test
	// does not wait before it.
	out, code, msg, err := e.runAs(daemon.Owner{Session: agentA, Takeover: true}, "click", "#confirm")
	if err != nil || code != 0 {
		t.Fatalf("click --takeover: exit %d, %q, %v", code, msg, err)
	}
	if page := strings.Trim(e.js(agentA, "document.getElementById('out').textContent"), `"`); page != "confirm:false" {
		t.Fatalf("confirm in the same document after the takeover: %q (%s); want false", page, out)
	}
	if !strings.Contains(out, "dialog answer d1 expired with its document") {
		t.Fatalf("click --takeover output %q does not report answer d1 as expired", out)
	}
	e.must(agentA, "open", e.sites.b+"/dialog.html")
	if out, page := e.confirmOut(agentA); page != "confirm:false" {
		t.Fatalf("confirm in the new cross-origin document: %q (%s); want false", page, out)
	}
	docs := map[string]bool{}
	for _, r := range e.recordsOf("confirm") {
		if r.Result == "true" {
			docs[r.Doc] = true
		}
	}
	if len(docs) != 1 || len(e.recordsOf("confirm")) != 3 {
		t.Fatalf("confirm records %+v; want three confirms and the accept in exactly one document", e.recordsOf("confirm"))
	}
}

// 7.6 item 7 (c) for reload and reload --ignore-cache: phase 1 withdraws,
// Page.reload goes only after its reply (with ignoreCache), a confirm
// between the phases gets the default, the new document gets the answer
// once; with the phase-1 reply dropped the answer expires and the reload
// still runs.
func TestDialogWithdrawalOnReload(t *testing.T) {
	e := newNavEnv(t, "dialog.html", false)
	for _, ignore := range []bool{false, true} {
		argv := []string{"reload"}
		if ignore {
			argv = append(argv, "--ignore-cache")
		}
		e.must(agentA, "dialog", "accept")
		var between string
		e.tap.set(func(h *tapHooks) {
			h.beforeReload = func(probe func(string) (string, error)) {
				v, err := probe("confirm('between phases')")
				between = fmt.Sprintf("%s %v", v, err)
			}
		})
		mark := len(e.tap.records())
		e.must(agentA, argv...)
		if between != "false <nil>" {
			t.Fatalf("%v: a confirm between the phases returned %q, want false", argv, between)
		}
		recs := e.tap.records()[mark:]
		withdrawID, replyAt, reloadAt := int64(0), -1, -1
		for i, r := range recs {
			switch {
			case r.Out && r.Method == "Runtime.evaluate" && strings.Contains(r.Params, `\"withdraw\":true`):
				withdrawID = r.ID
			case !r.Out && r.Reply && r.ID == withdrawID && withdrawID != 0:
				replyAt = i
			case r.Out && r.Method == "Page.reload":
				reloadAt = i
				var p struct{ IgnoreCache bool }
				_ = json.Unmarshal([]byte(r.Params), &p)
				if p.IgnoreCache != ignore {
					t.Fatalf("%v: Page.reload ignoreCache %v", argv, p.IgnoreCache)
				}
			}
		}
		if replyAt < 0 || reloadAt < replyAt {
			t.Fatalf("%v: withdrawal reply at %d, Page.reload at %d; want Page.reload after the reply", argv, replyAt, reloadAt)
		}
		if _, page := e.confirmOut(agentA); page != "confirm:true" {
			t.Fatalf("%v: first confirm in the new document: %q, want true", argv, page)
		}
		if _, page := e.confirmOut(agentA); page != "confirm:false" {
			t.Fatalf("%v: second confirm: %q, want false", argv, page)
		}
	}
	e.must(agentA, "dialog", "accept")
	e.tap.set(func(h *tapHooks) { h.drop = evalWith(`"withdraw":true`) })
	mark := len(e.tap.records())
	out := e.must(agentA, "reload")
	if !strings.Contains(out, "expired with its document") {
		t.Fatalf("reload with the phase-1 reply dropped: %q", out)
	}
	reloaded := false
	for _, r := range e.tap.records()[mark:] {
		reloaded = reloaded || (r.Out && r.Method == "Page.reload")
	}
	if !reloaded {
		t.Fatal("Page.reload was not sent after the phase-1 reply was lost")
	}
	if _, page := e.confirmOut(agentA); page != "confirm:false" {
		t.Fatalf("confirm after the lost phase 1: %q, want false", page)
	}
}

// 7.6 item 8: with pause-on-start, the first inline script of a new
// cross-process document sees the dialog override (its confirm is
// answered by the policy and logged) and a user init script.
func TestEarlyInstallWithPauseOnStart(t *testing.T) {
	// The start page has no early confirm: before the override exists a
	// native confirm would block the page.
	e := newNavEnv(t, "nav.html", daemon.PauseOnStart)
	e.must(agentA, "dialog", "default", "accept")
	// A user init script as a bootstrap part (the W4 command adds these
	// through the same core API).
	e.js(agentA, "1")
	for _, st := range e.m.Status() {
		pg := e.m.LookupPage(st.Key)
		if err := pg.PS.State.SetPart("init:"+agentA, "user-init", 50, "window.__userInit = 'ran';"); err != nil {
			t.Fatal(err)
		}
	}
	for run := 1; run <= 4; run++ {
		site := []string{e.sites.a, e.sites.b}[run%2]
		url := fmt.Sprintf("%s/early.html?run=%d", site, run)
		e.must(agentA, "open", url)
		if got := e.js(agentA, "JSON.stringify(window.__early)"); got != `"{\"confirm\":true,\"init\":\"ran\"}"` {
			t.Fatalf("run %d (%s): the first inline script saw %s, want the override (confirm true by policy) and the init script", run, url, got)
		}
	}
	if e.swaps() < 4 {
		t.Fatalf("%d process swaps for 4 cross-site opens", e.swaps())
	}
	status := e.must(agentA, "dialog", "status")
	if strings.Count(status, `confirm "early?" answered true (no answer armed: session default)`) < 1 {
		t.Fatalf("dialog status does not log the early confirm:\n%s", status)
	}
}

// droppedOf counts the replies and the console.debug dialog markers that
// the tap hooks dropped.
func droppedOf(recs []tapRecord) (replies, markers int) {
	for _, r := range recs {
		if r.Params != "dropped" {
			continue
		}
		if r.Reply {
			replies++
		} else if r.Method == "Console.messageAdded" {
			markers++
		}
	}
	return replies, markers
}
