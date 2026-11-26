//go:build cdpcompat

package cdpcompat

import (
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// startPage is the page MiniBrowser starts on; every scenario navigates
// to the form first, so that navigation is a real one.
const startPage = "/second.html"

// clientSession issues the endpoint URLs of a logical session of the CDP
// endpoint for the browser's page and closes the session at cleanup, which
// releases its lease, so that the next client's session can attach.
func (g *gtk) clientSession(t *testing.T, session string) cdpURLs {
	t.Helper()
	u := g.getCDPURL(t, session, "")
	t.Cleanup(func() { cli(t, "--session", session, "close") })
	return u
}

// formState is the form fixture's state as pageState reads it.
type formState struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Button string `json:"button"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Late   bool   `json:"late"`
}

// pageState reads the form fixture's state through "agent-ios eval" in
// session: an oracle that does not depend on the client under test.
func pageState(t *testing.T, session string) formState {
	t.Helper()
	const expr = "JSON.stringify({url: location.href, title: document.title, " +
		"button: document.getElementById('change').textContent, name: document.getElementById('name').value, " +
		"status: document.getElementById('status').textContent, late: !!document.getElementById('late')})"
	var s formState
	out := cli(t, "--session", session, "eval", expr)
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("eval of the form state in session %s printed %q: %v", session, out, err)
	}
	return s
}

// checkUnknown applies the expected-missing rule (design 5.4) to a client
// run's unknown methods.
func checkUnknown(t *testing.T, client string, unknown map[string]int) {
	t.Helper()
	t.Logf("%s: unknown methods (cdp log): %v", client, unknown)
	if err := loadExpectedMissing(t).unexpectedUnknown(client, unknown); err != nil {
		t.Error(err)
	}
}

// stepValue decodes the value that the observed step name of r recorded,
// and fails the test unless the step ran and passed.
func stepValue(t *testing.T, r clientResult, name string, v any) {
	t.Helper()
	for _, s := range r.Steps {
		if s.Name != name {
			continue
		}
		if !s.OK {
			t.Fatalf("%s step %s failed: %s", r.Client, name, s.Error)
		}
		if err := json.Unmarshal(s.Value, v); err != nil {
			t.Fatalf("%s step %s value %s: %v", r.Client, name, s.Value, err)
		}
		return
	}
	t.Fatalf("%s did not run the step %s; steps %+v", r.Client, name, r.Steps)
}

// checkPNG checks that file is a PNG image of the given size.
func checkPNG(t *testing.T, file string, width, height int) {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatalf("the screenshot: %v", err)
	}
	defer func() { _ = f.Close() }()
	cfg, err := png.DecodeConfig(f)
	if err != nil || cfg.Width != width || cfg.Height != height {
		t.Fatalf("the screenshot %s: %dx%d (%v), want a %dx%d PNG", file, cfg.Width, cfg.Height, err, width, height)
	}
}

// viewport reads the page's viewport size in session.
func viewport(t *testing.T, session string) (int, int) {
	t.Helper()
	var v struct{ W, H int }
	out := cli(t, "--session", session, "eval", "JSON.stringify({w: innerWidth, h: innerHeight})")
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.W <= 0 || v.H <= 0 {
		t.Fatalf("the viewport: %q (%v)", out, err)
	}
	return v.W, v.H
}

// Design 7.8 test 8 on WebKitGTK: each client works through the URL that
// "get cdp-url" printed, on the form fixture, and each step is checked by
// its effect on the page. Every client gets a fresh MiniBrowser and its
// own logical session; the expected-missing rule (5.4) applies to each run.
func TestClientScenarios(t *testing.T) {
	requireNode(t)
	pinned := pinnedAgentBrowser(t)
	fromPath, runPath := pathAgentBrowser(t, pinned)
	writeBuilds(t, []agentBrowserBuild{pinned, fromPath})
	t.Run("agent-browser", func(t *testing.T) { agentBrowserScenario(t, pinned.Binary) })
	t.Run("agent-browser-features", func(t *testing.T) { agentBrowserFeatures(t, pinned.Binary) })
	if runPath {
		t.Run("agent-browser-path", func(t *testing.T) { agentBrowserScenario(t, fromPath.Binary) })
		t.Run("agent-browser-path-features", func(t *testing.T) { agentBrowserFeatures(t, fromPath.Binary) })
	}
	t.Run("playwright", func(t *testing.T) {
		g := startGTK(t, startPage)
		u := g.clientSession(t, "playwright")
		for _, form := range []struct{ name, url string }{{"ws", u.WS}, {"http", u.HTTP}} {
			run := g.runNode(t, "playwright-scenario-"+form.name, "playwright-core", "scenario", form.url, g.site.origin)
			checkUnknown(t, "playwright-core", run.unknown)
			if got := pageState(t, "playwright"); got != (formState{URL: g.site.origin + "/form.html", Title: "CDP form", Button: "Clicked 1", Name: "Grace", Status: "name: Grace", Late: true}) {
				t.Errorf("%s: the page after the scenario: %+v", form.name, got)
			}
		}
	})
	t.Run("chrome-devtools-mcp", func(t *testing.T) {
		g := startGTK(t, startPage)
		u := g.clientSession(t, "chrome-devtools-mcp")
		run := g.runNode(t, "devtools-mcp-scenario", "chrome-devtools-mcp", "scenario", u.WS, g.site.origin)
		checkUnknown(t, "chrome-devtools-mcp", run.unknown)
		// The server's protocol log saw every command: each method that the
		// endpoint counted as unknown is in the script's record.
		for m := range run.unknown {
			if !slices.Contains(run.result.methods(), m) {
				t.Errorf("the endpoint counted the unknown method %s, which chrome-devtools-mcp's protocol log did not record", m)
			}
		}
		if !slices.Contains(run.result.methods(), "Page.captureScreenshot") {
			t.Errorf("chrome-devtools-mcp's protocol log has no Page.captureScreenshot: %v", run.result.methods())
		}
		got := pageState(t, "chrome-devtools-mcp")
		got.Late = false // the click adds #late 300 ms later; this scenario does not wait for it
		if got != (formState{URL: g.site.origin + "/form.html", Title: "CDP form", Button: "Clicked 1", Status: "ready"}) {
			t.Errorf("the page after the scenario: %+v", got)
		}
		var shot struct{ Width, Height int }
		stepValue(t, run.result, "take_screenshot", &shot)
		if w, h := viewport(t, "chrome-devtools-mcp"); shot.Width != w || shot.Height != h {
			t.Errorf("take_screenshot returned a %dx%d PNG, want the viewport %dx%d", shot.Width, shot.Height, w, h)
		}
	})
	t.Run("chrome-devtools-mcp-features", devtoolsMCPFeatures)
	t.Run("puppeteer", func(t *testing.T) {
		g := startGTK(t, startPage)
		u := g.clientSession(t, "puppeteer")
		for _, form := range []struct{ name, url string }{{"ws", u.WS}, {"http", u.HTTP}} {
			run := g.runNode(t, "puppeteer-scenario-"+form.name, "puppeteer-core", "scenario", form.url, g.site.origin)
			checkUnknown(t, "puppeteer-core", run.unknown)
			if got := pageState(t, "puppeteer"); got != (formState{URL: g.site.origin + "/form.html", Title: "CDP form", Button: "Clicked 1", Name: "Ada", Status: "name: Ada", Late: true}) {
				t.Errorf("%s: the page after the scenario: %+v", form.name, got)
			}
		}
	})
}

// agentBrowserRef finds the ref of a snapshot line such as
// `- button "Press me" [ref=e3]`.
func agentBrowserRef(t *testing.T, snapshot, role, name string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(role+" \""+name+"\"") + `[^\n]*\[ref=(e\d+)\]`).FindStringSubmatch(snapshot)
	if m == nil {
		t.Fatalf("agent-browser snapshot -i has no %s %q with a ref:\n%s", role, name, snapshot)
	}
	return "@" + m[1]
}

// agentBrowserScenario is test 8 for agent-browser --cdp with the binary
// bin: open, snapshot -i, click @ref, fill @ref, get title, screenshot. All
// commands run in one agent-browser session; agent-ios eval in the same
// logical session checks each effect on the page.
func agentBrowserScenario(t *testing.T, bin string) {
	// agent-browser used exactly the printed ws URL and made no discovery
	// request (design 5.1; P2 checked the same for "get title"). The
	// endpoint records an upgrade when its connection ends, so this cleanup
	// is registered first: it runs after the daemon stopped.
	var g *gtk
	var u cdpURLs
	mark := 0
	t.Cleanup(func() {
		if g == nil || u.WS == "" {
			return
		}
		want := request{Method: "GET", URI: strings.TrimPrefix(u.WS, "ws://"+u.Host), Host: u.Host, Upgrade: true, Status: 101}
		reqs := g.rec.since(mark)
		for _, r := range reqs {
			if r != want {
				t.Errorf("agent-browser made the request %s; want only upgrades of the printed URL", g.redact(r.String()))
			}
		}
		if len(reqs) == 0 {
			t.Error("the endpoint saw no request of agent-browser")
		}
	})
	g = startGTK(t, startPage)
	const session = "agent-browser"
	u = g.clientSession(t, session)
	name := strings.ReplaceAll(t.Name(), "/", "_")
	mark = g.rec.mark()
	ab := g.agentBrowserSession(t, bin, name, u.WS)
	before := unknownCounts(t)
	must := func(step string, args ...string) string {
		t.Helper()
		code, out := ab.run(t, name+"-"+step, args...)
		if code != 0 {
			t.Fatalf("agent-browser %s: exit %d: %s", strings.Join(args, " "), code, out)
		}
		return out
	}
	form := g.site.origin + "/form.html"
	must("open", "open", form)
	if got := pageState(t, session); got.URL != form || got.Title != "CDP form" {
		t.Fatalf("after open: %+v, want the form", got)
	}
	snap := must("snapshot", "snapshot", "-i")
	button, textbox := agentBrowserRef(t, snap, "button", "Press me"), agentBrowserRef(t, snap, "textbox", "Name")
	must("click", "click", button)
	if got := pageState(t, session); got.Button != "Clicked 1" {
		t.Fatalf("after click %s: the button says %q, want Clicked 1", button, got.Button)
	}
	must("fill", "fill", textbox, "Linus")
	if got := pageState(t, session); got.Name != "Linus" || got.Status != "name: Linus" {
		t.Fatalf("after fill %s: %+v, want the name Linus seen by the page's input handler", textbox, got)
	}
	if out := strings.TrimSpace(must("title", "get", "title")); out != "CDP form" {
		t.Fatalf("agent-browser get title printed %q, want CDP form", out)
	}
	shot := filepath.Join(t.TempDir(), "shot.png")
	must("screenshot", "screenshot", shot)
	w, h := viewport(t, session)
	checkPNG(t, shot, w, h)
	checkUnknown(t, "agent-browser", newUnknown(before, unknownCounts(t)))
}

// devtoolsMCPFeatures runs the features scenario of chrome-devtools-mcp
// (clients/chrome-devtools-mcp.mjs): the tools that 1.14.0 added or
// changed, through the printed ws URL. The script checks each tool's
// answer and its effect in the page; the driver checks that the mock kept
// the first fetch of /resource from the fixture server and the second one
// reached it (the offline fetch did not), that the endpoint refused the
// Slow 3G preset for its upload limit, and that the logpoint bound and
// logged.
func devtoolsMCPFeatures(t *testing.T) {
	g := startGTK(t, startPage)
	u := g.clientSession(t, "chrome-devtools-mcp")
	run := g.runNode(t, "devtools-mcp-features", "chrome-devtools-mcp", "features", u.WS, g.site.origin)
	checkUnknown(t, "chrome-devtools-mcp", run.unknown)
	for m := range run.unknown {
		if !slices.Contains(run.result.methods(), m) {
			t.Errorf("the endpoint counted the unknown method %s, which chrome-devtools-mcp's protocol log did not record", m)
		}
	}
	if n := g.site.resourceRequests(); n != 1 {
		t.Errorf("the fixture server got %d requests for /resource, want 1 (the offline fetch failed before the server, the mock answered the next one, the last came after remove_network_mock)", n)
	}
	// Network throttling: every preset of chrome-devtools-mcp's emulate has
	// an upload limit (Puppeteer 25.18.1 PredefinedNetworkConditions), which
	// no WebKit target can emulate. The endpoint refuses it before any
	// change and before the WIP Network.setEmulatedConditions, on WebKitGTK
	// as on iOS (internal/cdp/network_conditions.go throttleParams), and
	// Puppeteer passes the error on (it swallows only "Not supported" and
	// "wasn't found").
	var network struct{ Error string }
	stepValue(t, run.result, "emulate networkConditions", &network)
	if want := "Network.emulateNetworkConditionsByRule: upload throttling cannot be emulated on iOS WebKit"; !strings.Contains(network.Error, want) {
		t.Errorf("emulate networkConditions Slow 3G answered %q, want the endpoint's refusal %q", network.Error, want)
	}
	var offline string
	stepValue(t, run.result, "emulate Offline", &offline)
	if offline != "failed: TypeError" {
		t.Errorf("the fetch of /resource while offline: %q, want failed: TypeError", offline)
	}
	var logpoint struct {
		Bound  bool
		Logged string
	}
	stepValue(t, run.result, "set_logpoints", &logpoint)
	if !logpoint.Bound || logpoint.Logged != "logpoint 21" {
		t.Errorf("set_logpoints: %+v, want the logpoint bound and \"logpoint 21\" logged", logpoint)
	}
	// wait_for (Puppeteer's aria/ and text/ locators): text present from
	// the start, and a text and an accessible name (aria/ only) that a
	// timer adds after the call began; on frames.html, present text of the
	// main document and of its three same-origin frames.
	for _, name := range []string{"wait_for present", "wait_for late text", "wait_for late name", "wait_for frames main", "wait_for frames inner"} {
		var found string
		stepValue(t, run.result, name, &found)
		if found != "found" {
			t.Errorf("%s: %q, want found", name, found)
		}
	}
	// get_css_styles on css.html's button: the cascade of the fixture's
	// CSS, as chrome-devtools-mcp prints it from the endpoint's CSS domain
	// (the script checks every declaration; here the decisive ones).
	var css struct {
		Rules []struct {
			Selector, Source, In string
			Decls                []string
		}
	}
	stepValue(t, run.result, "get_css_styles", &css)
	cssHas := func(selector, in, source, decl string) {
		t.Helper()
		for _, r := range css.Rules {
			if r.Selector == selector && r.In == in && r.Source == source && slices.Contains(r.Decls, decl) {
				return
			}
		}
		t.Errorf("get_css_styles has no %q in %s %s (%s); rules %+v", decl, in, selector, source, css.Rules)
	}
	cssHas("#target", "", "<style>:4", "color: var(--brand); /* rgb(255, 0, 0) */")
	cssHas(".box", "", "<style>:3", "[overloaded] color: blue;")
	cssHas(".box", "", "<style>:3", "text-decoration: underline !important;")
	cssHas("#target", "", "<style>:4", "[overloaded] text-decoration: none;")
	cssHas("#target", "@media (min-width: 1px)", "<style>:5", "padding: 3px;")
	var url string
	if err := json.Unmarshal([]byte(cli(t, "--session", "chrome-devtools-mcp", "eval", "JSON.stringify(location.href)")), &url); err != nil || url != g.site.origin+"/console.html" {
		t.Errorf("the page after the scenario is %q (%v), want console.html", url, err)
	}
}

// featureState is the state of the features fixture
// (fixtures/features.html) as featuresState reads it.
type featureState struct {
	Title    string `json:"title"`
	ModLog   string `json:"modlog"`
	SizeLog  string `json:"sizelog"`
	Size     string `json:"size"`
	Rows     int    `json:"rows"`
	InnerLog string `json:"innerlog"`
}

// featuresState reads the features fixture through "agent-ios eval" in
// session: an oracle that does not depend on the client under test.
func featuresState(t *testing.T, session string) featureState {
	t.Helper()
	const expr = "JSON.stringify({title: document.title, modlog: document.getElementById('modlog').textContent, " +
		"sizelog: document.getElementById('sizelog').textContent, size: document.getElementById('size').value, " +
		"rows: document.querySelectorAll('li').length, " +
		"innerlog: document.getElementById('child').contentDocument.getElementById('innerlog').textContent})"
	var s featureState
	out := cli(t, "--session", session, "eval", expr)
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("eval of the features state in session %s printed %q: %v", session, out, err)
	}
	return s
}

// agentBrowserFeatures runs commands that agent-browser 0.42 to 0.45 added
// or changed, through --cdp with the binary bin, on the features fixture:
// click --modifiers, select --by, snapshot hints and the iframe label,
// diff snapshot, and frame with click and eval in the iframe. agent-ios
// eval in the same logical session checks each effect on the page.
func agentBrowserFeatures(t *testing.T, bin string) {
	g := startGTK(t, startPage)
	const session = "agent-browser"
	u := g.clientSession(t, session)
	name := strings.ReplaceAll(t.Name(), "/", "_")
	ab := g.agentBrowserSession(t, bin, name, u.WS)
	before := unknownCounts(t)
	run := func(step string, args ...string) (int, string) {
		t.Helper()
		return ab.run(t, name+"-"+step, args...)
	}
	must := func(step string, args ...string) string {
		t.Helper()
		code, out := run(step, args...)
		if code != 0 {
			t.Errorf("agent-browser %s: exit %d: %s", strings.Join(args, " "), code, out)
		}
		return out
	}
	features := g.site.origin + "/features.html"
	must("open", "open", features)
	if got := featuresState(t, session); got.Title != "CDP features" {
		t.Fatalf("after open: %+v, want the features page", got)
	}

	// click --modifiers (#2594): the modifiers reach the page's click
	// event, as in agent-browser's own e2e test (meta+shift).
	must("click-modifiers", "click", "#mod", "--modifiers", "meta,shift")
	if got := featuresState(t, session).ModLog; got != "click:b0:d1:c0s1a0m1" {
		t.Errorf("click #mod --modifiers meta,shift: the page saw %q, want click:b0:d1:c0s1a0m1", got)
	}

	// select --by (#2592): "m" is the value of one option and the label of
	// another; --by picks which.
	must("select-by-label", "select", "#size", "m", "--by", "label")
	if got := featuresState(t, session); got.Size != "x" || got.SizeLog != "x" {
		t.Errorf("select #size m --by label: value %q, change event %q; want x (the option labelled m)", got.Size, got.SizeLog)
	}
	must("select-by-value", "select", "#size", "m", "--by", "value")
	if got := featuresState(t, session); got.Size != "m" || got.SizeLog != "m" {
		t.Errorf("select #size m --by value: value %q, change event %q; want m (Medium)", got.Size, got.SizeLog)
	}

	// snapshot hints (#2578): the two Delete buttons share their label, so
	// each line gets a hint from its name and its row; the button with a
	// unique label gets none; the iframe line shows its title (#2739).
	snap := must("snapshot", "snapshot", "-i")
	for _, want := range []string{`hint="name:del-blue; in:Blue mug"`, `hint="name:del-red; in:Red mug"`} {
		if !strings.Contains(snap, want) {
			t.Errorf("agent-browser snapshot -i has no %s:\n%s", want, snap)
		}
	}
	if m := regexp.MustCompile(`button "Modifier probe" [^\n]*`).FindString(snap); m == "" || strings.Contains(m, "hint=") {
		t.Errorf("agent-browser snapshot -i: the button Modifier probe (a unique label) is %q, want a line without a hint:\n%s", m, snap)
	}
	if !regexp.MustCompile(`(?i)iframe "Child frame"`).MatchString(snap) {
		t.Errorf("agent-browser snapshot -i has no iframe labelled by its title \"Child frame\":\n%s", snap)
	}

	// diff snapshot (#2689): after Delete removed the red row, the diff
	// against the last snapshot (same options) shows the change.
	if m := regexp.MustCompile(`button "Delete" \[ref=(e\d+)\][^\n]*del-red`).FindStringSubmatch(snap); m == nil {
		t.Errorf("agent-browser snapshot -i has no red Delete button with a ref:\n%s", snap)
	} else {
		must("click-delete", "click", "@"+m[1])
	}
	if got := featuresState(t, session).Rows; got != 1 {
		t.Errorf("after the click on the red Delete the page has %d rows, want 1", got)
	}
	_, diffOut := run("diff-snapshot", "--json", "diff", "snapshot", "-i")
	var diff struct {
		Success bool
		Error   string
		Data    struct {
			Diff                string
			Additions, Removals int
			Changed             bool
		}
	}
	if err := json.Unmarshal([]byte(diffOut), &diff); err != nil {
		t.Errorf("agent-browser --json diff snapshot -i printed %q: %v", diffOut, err)
	} else if !diff.Success || !diff.Data.Changed || diff.Data.Removals == 0 || !strings.Contains(diff.Data.Diff, "- ") || !strings.Contains(diff.Data.Diff, "Red mug") {
		t.Errorf("agent-browser diff snapshot -i after the red row was removed: %+v, want a change that removes the red row's line", diff)
	}

	// The iframe commands (#2737, #2738): frame selects the iframe; click
	// and eval act in it; frame main returns to the page.
	must("frame", "frame", "#child")
	must("frame-click", "click", "#inner")
	if got := featuresState(t, session).InnerLog; got != "inner clicked" {
		t.Errorf("click #inner after frame #child: the iframe says %q, want inner clicked", got)
	}
	if out := strings.Trim(strings.TrimSpace(must("frame-eval", "eval", "document.title")), `"`); out != "CDP child frame" {
		t.Errorf("eval document.title after frame #child printed %q, want CDP child frame", out)
	}
	must("frame-main", "frame", "main")
	if out := strings.Trim(strings.TrimSpace(must("main-eval", "eval", "document.title")), `"`); out != "CDP features" {
		t.Errorf("eval document.title after frame main printed %q, want CDP features", out)
	}
	checkUnknown(t, "agent-browser", newUnknown(before, unknownCounts(t)))
}

// Design 7.8 test 12, the awaited intercepted fetch (2.10): Puppeteer's
// request interception and Playwright's page.route answer /resource, and
// page.evaluate(() => fetch('/resource').then(r => r.text())) returns "ok"
// within 2 s of a 10 s deadline (the scripts check both). The request never
// reaches the fixture server.
func TestAwaitedInterceptedFetch(t *testing.T) {
	requireNode(t)
	for _, c := range []struct{ name, client string }{{"puppeteer", "puppeteer-core"}, {"playwright", "playwright-core"}} {
		t.Run(c.name, func(t *testing.T) {
			g := startGTK(t, startPage)
			u := g.clientSession(t, c.name)
			run := g.runNode(t, c.name+"-fetch", c.client, "fetch", u.WS, g.site.origin)
			checkUnknown(t, c.client, run.unknown)
			var got struct {
				Body string
				MS   int
			}
			stepValue(t, run.result, "fetch", &got)
			t.Logf("%s: fetch returned %q after %d ms", c.client, got.Body, got.MS)
			if got.Body != "ok" || got.MS > 2000 {
				t.Errorf("%s: fetch returned %q after %d ms, want ok within 2000 ms", c.client, got.Body, got.MS)
			}
			if n := g.site.resourceRequests(); n != 0 {
				t.Errorf("%s: the fixture server got %d requests for /resource, want 0 (the client answers it)", c.client, n)
			}
		})
	}
}

var pressMeRef = regexp.MustCompile(`button "Press me" \[ref=(e\d+)\]`)

// leaseEpoch returns the lease epoch of session's one page.
func leaseEpoch(t *testing.T, session string) uint64 {
	t.Helper()
	var info struct {
		Pages []struct {
			Epoch uint64 `json:"epoch"`
		} `json:"pages"`
	}
	out := cli(t, "--session", session, "session", "info", "--json")
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info.Pages) != 1 {
		t.Fatalf("session info of %s: %q (%v); want one leased page", session, out, err)
	}
	return info.Pages[0].Epoch
}

// Design 7.8 test 11 (a) and (d) through Puppeteer (the Go-client form is
// cmd/agent-ios TestCDPSessionBindingThroughTheCLI):
//   - (a) after "agent-ios --session s1 snapshot -i" and "get cdp-url",
//     Puppeteer connects with s1's URL (tab, then page) and evaluates in the
//     page; s1's lease epoch is unchanged, and "click @eN" with the
//     snapshot's ref works through the CLI while Puppeteer stays connected
//     (the script sees the click).
//   - (d) "agent-ios --session s2 get title" exits 4 with the lease
//     message; a Puppeteer connection through s2's URL lists the page and
//     Target.attachToTarget fails with the lease message; s1's lease epoch
//     and its Puppeteer connection are unchanged.
func TestSessionBindingWithPuppeteer(t *testing.T) {
	requireNode(t)
	g := startGTK(t, startPage)
	cli(t, "--session", "s1", "open", g.site.origin+"/form.html")
	m := pressMeRef.FindStringSubmatch(cli(t, "--session", "s1", "snapshot", "-i"))
	if m == nil {
		t.Fatal("s1 snapshot -i has no Press me button with a ref")
	}
	epoch := leaseEpoch(t, "s1")
	s1 := g.clientSession(t, "s1")
	s2 := g.clientSession(t, "s2")
	checkpoints := map[string]func() error{
		"attached": func() error {
			if got := leaseEpoch(t, "s1"); got != epoch {
				return fmt.Errorf("s1's lease epoch went from %d to %d after the Puppeteer attach", epoch, got)
			}
			if code, _, errOut := cliExit(t, "--session", "s1", "click", "@"+m[1]); code != 0 {
				return fmt.Errorf("s1 click @%s: exit %d: %s", m[1], code, errOut)
			}
			if code, _, errOut := cliExit(t, "--session", "s2", "get", "title"); code != 4 || !strings.Contains(errOut, `leased by session "s1"`) {
				return fmt.Errorf("s2 get title: exit %d, stderr %q; want exit 4 with the lease message", code, errOut)
			}
			return nil
		},
		"refused": func() error {
			if got := leaseEpoch(t, "s1"); got != epoch {
				return fmt.Errorf("s1's lease epoch went from %d to %d after s2's attempts", epoch, got)
			}
			return nil
		},
	}
	var seen []string
	run := g.runNodeWith(t, "puppeteer-session", "puppeteer-core", "session", s1.WS, nodeOptions{
		args: []string{g.site.origin, s2.WS},
		checkpoint: func(name string) error {
			seen = append(seen, name)
			check, ok := checkpoints[name]
			if !ok {
				return fmt.Errorf("unknown checkpoint %q", name)
			}
			return check()
		},
	})
	checkUnknown(t, "puppeteer-core", run.unknown)
	if strings.Join(seen, ",") != "attached,refused" {
		t.Errorf("checkpoints %v, want attached, refused", seen)
	}
	var msg string
	stepValue(t, run.result, "s2 attach refused", &msg)
	t.Logf("s2's Target.attachToTarget: %s", msg)
}
