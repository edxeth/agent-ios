package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of open.go on the fake WebKit endpoint. The fake page models what
// design 3.6 and 3.7 need from a document: its URL and generation, and the
// dialog answers it holds ("dialogs" withdraw and issue); the navigation
// itself runs in the WebKitGTK suite (e2e/webkitgtk/navigation_test.go).

type navFake struct {
	h *rtHarness

	mu        sync.Mutex
	href      string
	gen       uint64
	held      []string         // answer ids the current document holds
	dialogs   []map[string]any // args of every "dialogs" call, in order
	navigates bool             // a nav or hist call moves to a new document
	// delayNext delays the replies of the calls after the next
	// location.href read by this much; dropNext drops the next reply.
	delayNext, dropNext time.Duration
	reloads             []map[string]any
	reloadAt            time.Time
	withdrawAt          time.Time
}

func newNavFake(t *testing.T) *navFake {
	t.Helper()
	h := newActionHarness(t)
	n := &navFake{h: h, href: "http://a.test/one", gen: 1, navigates: true}
	h.page.set("wait", okEnv(map[string]any{"ok": true, "state": "document.readyState is complete"}))
	str := func(s string) map[string]any {
		return map[string]any{"result": map[string]any{"type": "string", "value": s}}
	}
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			var v any
			if m[1] == "dialogs" {
				v = n.dialogsCall(json.RawMessage(m[2]))
			} else {
				v = h.page.handler(c, m[1], json.RawMessage(m[2]))
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		switch p.Expression {
		case "location.href":
			if n.delayNext > 0 {
				h.f.DelayReplies("Runtime.evaluate", n.delayNext)
				n.delayNext = 0
			}
			if n.dropNext > 0 {
				h.f.DropReplies("Runtime.evaluate", 1)
				n.dropNext = 0
			}
			return str(n.href), nil
		case "document.title":
			return str("Title of " + n.href), nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0}}, nil
	})
	h.f.Handle("Page.reload", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p map[string]any
		_ = json.Unmarshal(c.Params, &p)
		h.f.DelayReplies("Runtime.evaluate", 0)
		n.mu.Lock()
		n.reloads = append(n.reloads, p)
		n.reloadAt = time.Now()
		n.newDocLocked(n.href)
		n.mu.Unlock()
		return map[string]any{}, nil
	})
	return n
}

// newDocLocked moves the page to a new document at href: a new
// generation, and none of the old document's dialog answers.
func (n *navFake) newDocLocked(href string) {
	n.href, n.held = href, nil
	n.gen++
	n.h.setGen(n.gen)
}

// dialogsCall is the page side of 80-dialogs.js "dialogs".
func (n *navFake) dialogsCall(raw json.RawMessage) any {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dialogs = append(n.dialogs, args)
	withdrawn := []string{}
	if args["withdraw"] == true {
		withdrawn = append(withdrawn, n.held...)
		n.held = nil
		n.withdrawAt = time.Now()
	}
	if is, ok := args["issue"].([]any); ok {
		for _, x := range is {
			n.held = append(n.held, x.(map[string]any)["id"].(string))
		}
	}
	reply := okEnv(map[string]any{"doc": "doc", "used": []any{}, "pending": append([]string{}, n.held...), "withdrawn": withdrawn, "on": true})
	if nav, ok := args["nav"].(string); ok && n.navigates {
		n.newDocLocked(nav)
	}
	if hist, ok := args["hist"].(float64); ok && n.navigates {
		n.newDocLocked(n.href + map[float64]string{-1: "#back", 1: "#forward"}[hist])
	}
	return reply
}

func (n *navFake) calls() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]map[string]any(nil), n.dialogs...)
}

// issued lists the answer ids that "dialogs" call i issued.
func issued(call map[string]any) []string {
	var out []string
	is, _ := call["issue"].([]any)
	for _, x := range is {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

// Design 3.6: open sets location.href in the runtime call that withdraws
// the unused dialog answers (3.7), waits for the new document and its load
// state, and issues the withdrawn answers into the new document.
func TestOpenWithdrawsNavigatesAndIssuesAgain(t *testing.T) {
	n := newNavFake(t)
	n.h.mustRun(t, "dialog", "accept")
	out := n.h.mustRun(t, "open", "b.test/two")
	if !strings.HasPrefix(out, "navigated to https://b.test/two") {
		t.Fatalf("open output %q, want navigated to https://b.test/two (https:// added)", out)
	}
	calls := n.calls()
	nav, after := -1, -1
	for i, c := range calls {
		if c["nav"] != nil {
			nav = i
		} else if nav >= 0 && len(issued(c)) > 0 {
			after = i
		}
	}
	if nav < 0 || calls[nav]["nav"] != "https://b.test/two" || calls[nav]["withdraw"] != true || calls[nav]["issue"] != nil {
		t.Fatalf("dialogs calls %v: want one call that withdraws and navigates to https://b.test/two without issuing", calls)
	}
	if after < 0 || strings.Join(issued(calls[after]), ",") != "d1" {
		t.Fatalf("dialogs calls %v: want d1 issued again after the navigation", calls)
	}
	n.mu.Lock()
	held := strings.Join(n.held, ",")
	n.mu.Unlock()
	if held != "d1" {
		t.Fatalf("the new document holds %q, want d1", held)
	}
}

// Design 3.7: when the reply of the withdrawing call is lost, the answer
// may still be in the old document, so it expires and is never issued
// again; the navigation still completes (3.6: a lost reply is accepted).
func TestOpenWithLostWithdrawReplyExpiresTheAnswer(t *testing.T) {
	n := newNavFake(t)
	n.h.mustRun(t, "dialog", "accept")
	n.mu.Lock()
	n.dropNext = 1
	n.mu.Unlock()
	out := n.h.mustRun(t, "open", "http://b.test/two")
	if !strings.Contains(out, "navigated to http://b.test/two") || !strings.Contains(out, "dialog answer d1 expired") {
		t.Fatalf("open output %q, want the navigation and d1 expired", out)
	}
	calls := n.calls()
	navAt := -1
	for i, c := range calls {
		if c["nav"] != nil {
			navAt = i
		}
	}
	for _, c := range calls[navAt+1:] {
		if len(issued(c)) > 0 {
			t.Fatalf("an expired answer was issued again: %v", calls)
		}
	}
}

// Design 3.6 and 3.7: reload is two phases; Page.reload is sent only after
// the reply of the withdrawing call, with ignoreCache from --ignore-cache;
// the withdrawn answer is issued once into the new document.
func TestReloadSendsPageReloadOnlyAfterTheWithdrawReply(t *testing.T) {
	const replyDelay = 300 * time.Millisecond
	n := newNavFake(t)
	n.h.mustRun(t, "dialog", "accept")
	n.mu.Lock()
	n.delayNext = replyDelay
	n.mu.Unlock()
	out := n.h.mustRun(t, "reload", "--ignore-cache")
	if !strings.HasPrefix(out, "navigated to http://a.test/one") {
		t.Fatalf("reload output %q", out)
	}
	n.mu.Lock()
	reloads, gap := n.reloads, n.reloadAt.Sub(n.withdrawAt)
	n.mu.Unlock()
	if len(reloads) != 1 || reloads[0]["ignoreCache"] != true {
		t.Fatalf("Page.reload calls %v, want one with ignoreCache true", reloads)
	}
	if gap < replyDelay {
		t.Fatalf("Page.reload arrived %v after the withdrawing call, before its reply (delayed %v)", gap, replyDelay)
	}
	calls := n.calls()
	last := calls[len(calls)-1]
	if strings.Join(issued(last), ",") != "d1" || last["withdraw"] != nil {
		t.Fatalf("dialogs calls %v: want d1 issued into the reloaded document last", calls)
	}
	if code, _, _ := n.h.run("reload"); code != 0 {
		t.Fatalf("plain reload: exit %d", code)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.reloads) != 2 || n.reloads[1]["ignoreCache"] != false {
		t.Fatalf("Page.reload calls %v, want ignoreCache false without --ignore-cache", n.reloads)
	}
}

// Design 3.6: with the reply of phase 1 lost, the withdrawn answers expire
// and phase 2 still runs.
func TestReloadWithLostPhaseOneReplyStillReloads(t *testing.T) {
	n := newNavFake(t)
	n.h.mustRun(t, "dialog", "accept")
	n.mu.Lock()
	n.dropNext = 1
	n.mu.Unlock()
	out := n.h.mustRun(t, "reload")
	if !strings.Contains(out, "dialog answer d1 expired") {
		t.Fatalf("reload output %q, want d1 expired", out)
	}
	n.mu.Lock()
	reloads := len(n.reloads)
	n.mu.Unlock()
	if reloads != 1 {
		t.Fatalf("Page.reload sent %d times, want 1", reloads)
	}
	calls := n.calls()
	if c := calls[len(calls)-1]; len(issued(c)) > 0 {
		t.Fatalf("the expired answer was issued into the new document: %v", calls)
	}
}

// back and forward start the history move in the withdrawing call.
func TestBackAndForward(t *testing.T) {
	n := newNavFake(t)
	if out := n.h.mustRun(t, "back"); !strings.HasPrefix(out, "navigated to http://a.test/one#back") {
		t.Fatalf("back output %q", out)
	}
	if out := n.h.mustRun(t, "forward"); !strings.HasPrefix(out, "navigated to http://a.test/one#back#forward") {
		t.Fatalf("forward output %q", out)
	}
	var hist []any
	for _, c := range n.calls() {
		if c["hist"] != nil {
			if c["withdraw"] != true {
				t.Fatalf("history call %v does not withdraw", c)
			}
			hist = append(hist, c["hist"])
		}
	}
	if len(hist) != 2 || hist[0] != float64(-1) || hist[1] != float64(1) {
		t.Fatalf("history moves %v, want -1 then 1", hist)
	}
}

// A history move that starts no navigation ends with exit 3 and tells to
// check the URL and run the command again (device check of 2026-11-11:
// WebKit ignored a forward sent at once after a back); the move is never
// asked for twice. open keeps its message.
func TestHistoryMoveThatDoesNotNavigateTellsWhatToDo(t *testing.T) {
	n := newNavFake(t)
	n.navigates = false
	code, _, errOut := n.h.run("--timeout", "600ms", "forward")
	if code != ExitWaitTimeout || !strings.Contains(errOut, "did not navigate; URL is still http://a.test/one") || !strings.Contains(errOut, "run 'forward' again") {
		t.Fatalf("forward without a navigation: exit %d, stderr %q; want 3, the old URL and what to do", code, errOut)
	}
	k := 0
	for _, c := range n.calls() {
		if c["hist"] != nil {
			k++
		}
	}
	if k != 1 {
		t.Fatalf("forward sent %d history moves, want 1", k)
	}
	if code, _, errOut := n.h.run("--timeout", "600ms", "open", "http://b.test/never"); code != ExitWaitTimeout || strings.Contains(errOut, "again") {
		t.Fatalf("open without a navigation: exit %d, stderr %q; want 3 without the history advice", code, errOut)
	}
}

// Design 3.6: a navigation that never produces a new document ends with
// exit 3 and the last state; --wait none returns without waiting.
func TestOpenThatDoesNotNavigateTimesOut(t *testing.T) {
	n := newNavFake(t)
	n.navigates = false
	code, _, errOut := n.h.run("--timeout", "600ms", "open", "http://b.test/never")
	if code != ExitWaitTimeout || !strings.Contains(errOut, "did not navigate; URL is still http://a.test/one") {
		t.Fatalf("exit %d, stderr %q; want 3 and the old URL", code, errOut)
	}
	if out := n.h.mustRun(t, "open", "--wait", "none", "http://b.test/never"); !strings.HasPrefix(out, "navigation started") {
		t.Fatalf("open --wait none: %q", out)
	}
}

func TestPushState(t *testing.T) {
	n := newNavFake(t)
	n.h.page.set("pushstate", okEnv("history.pushState"))
	out := n.h.mustRun(t, "pushstate", "/next")
	if !strings.Contains(out, "(history.pushState)") || n.h.page.last("pushstate")["url"] != "/next" {
		t.Fatalf("pushstate output %q, args %v", out, n.h.page.last("pushstate"))
	}
}

// Every W3b-2a command has help, and an unknown flag exits 2 before any
// connection (the brief's acceptance; registry rule of design 2.11).
func TestNavigationCommandsHelpAndUnknownFlag(t *testing.T) {
	for _, name := range []string{"open", "goto", "navigate", "back", "forward", "reload", "pushstate", "tab", "tab list", "tab new", "window new", "tab close", "frame", "screenshot", "find", "wait", "dialog"} {
		words := strings.Fields(name)
		connected := false
		factory := func(context.Context, *Invocation) (Env, error) {
			connected = true
			return nil, errors.New("no connection in this test")
		}
		var out, errb bytes.Buffer
		if code := RunCLIStarted(context.Background(), append(append([]string{}, words...), "--help"), &out, &errb, nil, factory); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-ios ") {
			t.Errorf("%s --help: exit %d, stdout %q", name, code, out.String())
		}
		out.Reset()
		errb.Reset()
		if code := RunCLIStarted(context.Background(), append(append([]string{}, words...), "--no-such-flag"), &out, &errb, nil, factory); code != ExitUsage {
			t.Errorf("%s --no-such-flag: exit %d, want %d (%q)", name, code, ExitUsage, errb.String())
		}
		if connected {
			t.Errorf("%s: help or an unknown flag opened a connection", name)
		}
	}
}

// W3b-R1 merge brief item 3, design 4.2 and agent-browser open
// (cli/src/commands.rs normalize_navigation_url, commit 6682da3): the CLI
// open passes http:, https:, about:, data: and file: URLs unchanged and
// puts https:// in front of every other one, so a javascript: URL never
// reaches location.href as script. The normalized URL must parse (W3b-R2
// finding 4: normalize, then validate); "https://javascript:alert(1)" has
// the invalid port "alert(1)", so open refuses it as a usage error before
// any page call.
func TestOpenNormalizesURLsLikeAgentBrowser(t *testing.T) {
	for raw, want := range map[string]string{
		"https://a.test/":    "https://a.test/",
		"HTTP://a.test/":     "HTTP://a.test/",
		"about:blank":        "about:blank",
		"data:text/html,x":   "data:text/html,x",
		"file:///tmp/a.html": "file:///tmp/a.html",
		"a.test/two":         "https://a.test/two",
		"localhost:8080/x":   "https://localhost:8080/x",
		"javascript:8080/x":  "https://javascript:8080/x", // host "javascript", port 8080
	} {
		if got, err := absoluteURL(raw); err != nil || got != want {
			t.Fatalf("absoluteURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "JAVASCRIPT:alert(1)", "vbscript:msgbox(1)"} {
		var ue *UsageError
		if got, err := absoluteURL(raw); !errors.As(err, &ue) || !strings.Contains(err.Error(), "https://"+raw) {
			t.Fatalf("absoluteURL(%q) = %q, %v; want a usage error that names https://%s", raw, got, err, raw)
		}
	}
	// Property: the result is the input when it starts with one of
	// agent-browser's prefixes that WebKit has (listed here, not taken
	// from the code under test), else https:// and the input; so its
	// scheme is never one that runs script.
	kept := []string{"http://", "https://", "about:", "data:", "file:"}
	prop := func(s string) bool {
		got, err := absoluteURL(s)
		if err != nil {
			return true // the parse error is a usage error before any page call
		}
		lower := strings.ToLower(s)
		if slices.ContainsFunc(kept, func(p string) bool { return strings.HasPrefix(lower, p) }) {
			return got == s
		}
		return got == "https://"+s
	}
	gen := func(r *rand.Rand) string {
		parts := []string{"javascript:", "JavaScript:", "vbscript:", "data:", "about:", "file:", "http://", "https://", "HTTPS://", "a.test", "/", ":", "%", "x", " ", "\t"}
		var b strings.Builder
		for n := r.Intn(5); n >= 0; n-- {
			b.WriteString(parts[r.Intn(len(parts))])
		}
		return b.String()
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000, Values: func(v []reflect.Value, r *rand.Rand) { v[0] = reflect.ValueOf(gen(r)) }}); err != nil {
		t.Fatal(err)
	}
	n := newNavFake(t)
	if code, _, errOut := n.h.run("open", "javascript:alert(1)"); code != 2 || !strings.Contains(errOut, "https://javascript:alert(1)") {
		t.Fatalf("open javascript:alert(1): exit %d, stderr %q; want exit 2 with the normalized URL", code, errOut)
	}
	if calls := n.calls(); len(calls) != 0 {
		t.Fatalf("open javascript:alert(1) made page calls %v; want none", calls)
	}
}

// W3b-R2 finding 4: agent-browser puts https:// in front of a URL without
// one of its prefixes before anything parses it (normalize_navigation_url),
// so a schemeless host and port, an IPv4 or IPv6 address included, opens
// as https://host:port/path. Before, open parsed the raw input first and
// refused "127.0.0.1:8080/path" ("first path segment in URL cannot contain
// colon").
func TestOpenNormalizesSchemelessHostAndPort(t *testing.T) {
	for raw, want := range map[string]string{
		"127.0.0.1:8080/path":   "https://127.0.0.1:8080/path",
		"127.0.0.1:8080":        "https://127.0.0.1:8080",
		"[::1]:8080/x?y=1":      "https://[::1]:8080/x?y=1",
		"10.0.0.2:3000/a:b":     "https://10.0.0.2:3000/a:b",
		"a.test:443/p#frag":     "https://a.test:443/p#frag",
		"192.168.1.10:8000/":    "https://192.168.1.10:8000/",
		"[fe80::1%25en0]:80/":   "https://[fe80::1%25en0]:80/",
		"localhost:8080/x:y/z":  "https://localhost:8080/x:y/z",
		"0.0.0.0:1/first:colon": "https://0.0.0.0:1/first:colon",
	} {
		if got, err := absoluteURL(raw); err != nil || got != want {
			t.Fatalf("absoluteURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	// Property over generated host:port[/path] forms (hosts: IPv4, IPv6
	// in brackets, DNS names; ports 0 to 65535; paths that may hold
	// colons): the result is https:// and the input, it parses, and its
	// host and port are the generated ones.
	type form struct{ host, port, path string }
	gen := func(r *rand.Rand) form {
		var host string
		switch r.Intn(3) {
		case 0:
			host = fmt.Sprintf("%d.%d.%d.%d", r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256))
		case 1:
			groups := make([]string, 8)
			for i := range groups {
				groups[i] = strconv.FormatUint(uint64(r.Intn(1<<16)), 16)
			}
			host = "[" + strings.Join(groups, ":") + "]"
			if r.Intn(2) == 0 {
				host = "[::" + groups[0] + "]"
			}
		default:
			labels := []string{"a", "b-c", "example", "x9", "localhost", "test"}
			n := 1 + r.Intn(3)
			parts := make([]string, n)
			for i := range parts {
				parts[i] = labels[r.Intn(len(labels))]
			}
			host = strings.Join(parts, ".")
		}
		segs := []string{"", "/", "/p", "/a:b", "/x/y:z", "/?q=1:2", "/#f:g", "/%41"}
		return form{host, strconv.Itoa(r.Intn(65536)), segs[r.Intn(len(segs))]}
	}
	prop := func(f form) bool {
		raw := f.host + ":" + f.port + f.path
		got, err := absoluteURL(raw)
		if err != nil || got != "https://"+raw {
			return false
		}
		u, err := url.Parse(got)
		return err == nil && u.Scheme == "https" && u.Host == f.host+":"+f.port
	}
	cfg := &quick.Config{MaxCount: 3000, Values: func(v []reflect.Value, r *rand.Rand) { v[0] = reflect.ValueOf(gen(r)) }}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
	// Through the command: open sends the normalized URL to the page.
	n := newNavFake(t)
	if out := n.h.mustRun(t, "open", "127.0.0.1:8080/path"); !strings.HasPrefix(out, "navigated to https://127.0.0.1:8080/path") {
		t.Fatalf("open 127.0.0.1:8080/path output %q", out)
	}
	// A URL that is not valid even with https:// in front stays a usage
	// error before any page call.
	if _, err := absoluteURL("a.test/%zz"); err == nil {
		t.Fatal("absoluteURL(a.test/%zz) = nil error, want the parse error")
	}
}
