//go:build cdpcompat

package cdpcompat

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// F1 of the W5c-2a review: a client script that fails after it made the
// expected requests fails the run. The fault injection makes both Node
// clients exit 7 with ok:false and an error (testdata/fatal-result.mjs);
// execNode and judgeNodeRun must report each, for both URL forms. The
// table also covers each part of the verdict on its own.
func TestNodeRunFailureIsReported(t *testing.T) {
	requireNode(t)
	e := startDaemon(t)
	u := e.getCDPURL(t, "cdpcompat", fakePage(t).URL)
	inject, err := filepath.Abs(filepath.Join("testdata", "fatal-result.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"playwright-core", "puppeteer-core"} {
		for form, url := range map[string]string{"ws": u.WS, "http": u.HTTP} {
			code, data, err := e.execNode(t, "fatal-"+client+"-"+form, client, "resolve", url, nodeOptions{preload: inject})
			if err != nil {
				t.Fatalf("%s %s: %v", client, form, err)
			}
			if _, err := judgeNodeRun(code, data, client, "resolve", url); err == nil || !strings.Contains(err.Error(), "exit 7, ok false") {
				t.Errorf("%s %s with an injected failure: %v, want the failed run reported", client, form, err)
			}
		}
	}

	// The result names the pinned version: judgeNodeRun checks it.
	ok := []byte(`{"client":"puppeteer-core","version":"` + puppeteerPin + `","scenario":"resolve","form":"ws","url":"ws://x","ok":true,"steps":[{"name":"connect","ok":false,"ms":1,"error":"refused"}],"sent":[]}`)
	if _, err := judgeNodeRun(0, ok, "puppeteer-core", "resolve", "ws://x"); err != nil {
		t.Fatalf("a run whose failed connect step the scenario tolerates: %v", err)
	}
	for _, c := range []struct {
		name string
		code int
		data string
	}{
		{"nonzero exit", 1, string(ok)},
		{"ok false", 0, strings.Replace(string(ok), `"ok":true`, `"ok":false`, 1)},
		{"script error", 0, strings.Replace(string(ok), `"ok":true`, `"ok":true,"error":"boom"`, 1)},
		{"other URL", 0, strings.Replace(string(ok), "ws://x", "ws://y", 1)},
		{"other version", 0, strings.Replace(string(ok), puppeteerPin, "0.0.0", 1)},
		{"malformed", 0, "{"},
	} {
		if _, err := judgeNodeRun(c.code, []byte(c.data), "puppeteer-core", "resolve", "ws://x"); err == nil {
			t.Errorf("%s: judgeNodeRun passed", c.name)
		}
	}
}

// F2 of the W5c-2a review: client runs do not depend on the user's client
// settings. The test process gets a hostile environment: a HOME whose
// agent-browser configuration turns on autoConnect (which refuses --cdp),
// agent-browser and proxy variables, debug settings that would send
// Playwright's protocol log to a file or hide it, and (round 2) a
// NODE_OPTIONS preload that makes every Node client fail (exit 7, ok
// false). The pinned agent-browser still resolves the ws form, both Node
// clients still pass and Playwright still records its methods, the
// user's HOME is neither read for configuration nor changed, and the
// debug file is never written.
func TestClientsIgnoreUserSettings(t *testing.T) {
	requireNode(t)
	pinned := pinnedAgentBrowser(t)
	hostilePreload, err := filepath.Abs(filepath.Join("testdata", "fatal-result.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	userHome := t.TempDir()
	cfg := filepath.Join(userHome, ".agent-browser", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"autoConnect": true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	debugFile := filepath.Join(t.TempDir(), "pw-protocol.log")
	for k, v := range map[string]string{
		"HOME": userHome, "XDG_CONFIG_HOME": filepath.Join(userHome, ".config"),
		"AGENT_BROWSER_AUTO_CONNECT": "1", "AGENT_BROWSER_CONFIG": cfg,
		"DEBUG_FILE": debugFile, "DEBUG": "nothing", "DEBUG_COLORS": "yes", "DEBUG_HIDE_DATE": "yes",
		"HTTP_PROXY": "http://127.0.0.1:1", "ALL_PROXY": "http://127.0.0.1:1", "NO_PROXY": "",
		"NODE_OPTIONS": "--import=" + hostilePreload,
	} {
		t.Setenv(k, v)
	}
	before := treeOf(t, userHome)
	e := startDaemon(t)
	u := e.getCDPURL(t, "cdpcompat", fakePage(t).URL)
	wsPath := strings.TrimPrefix(u.WS, "ws://"+u.Host)

	mark := e.rec.mark()
	code, out, _ := e.runAgentBrowser(t, pinned.Binary, "isolation-agent-browser-ws", u.WS, "get", "url")
	got := e.rec.since(mark)
	// agent-browser 0.45.0 opens the URL twice: its attach probe, then the
	// attach (TestEndpointResolution).
	onlyPrinted := len(got) == 2
	for _, r := range got {
		onlyPrinted = onlyPrinted && r.URI == wsPath && r.Upgrade && r.Status == 101
	}
	if strings.Contains(out, "auto-connect") || !onlyPrinted {
		t.Errorf("agent-browser with a hostile user configuration: exit %d, output %q, requests %s; want two upgrades of the printed ws path (probe and attach)", code, e.redact(out), e.redact(lines(got)))
	}
	// runNode fails the test when a client run fails, as the hostile
	// preload would make it.
	run := e.runNode(t, "isolation-playwright-ws", "playwright-core", "resolve", u.WS)
	if !slices.Contains(run.result.methods(), "Browser.getVersion") {
		t.Errorf("Playwright recorded %v with DEBUG_FILE and DEBUG set by the user, want its commands (Browser.getVersion first)", run.result.methods())
	}
	e.runNode(t, "isolation-puppeteer-ws", "puppeteer-core", "resolve", u.WS)
	if _, err := os.Stat(debugFile); err == nil {
		t.Errorf("Playwright wrote the user's DEBUG_FILE %s", debugFile)
	}
	if after := treeOf(t, userHome); after != before {
		t.Errorf("the user's HOME changed during the runs:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// F2, round 2: the client environment drops every inherited variable
// that changes how Node or npm load code (userSetting), and keeps the
// others.
func TestClientEnvDropsCodeLoadingSettings(t *testing.T) {
	dropped := []string{
		"NODE_OPTIONS", "NODE_PATH", "NODE_PRESERVE_SYMLINKS", "NODE_PRESERVE_SYMLINKS_MAIN", "NODE_COMPILE_CACHE",
		"NODE_REPL_EXTERNAL_MODULE", "NODE_ICU_DATA", "NODE_EXTRA_CA_CERTS", "NODE_V8_COVERAGE",
		"OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_MODULES", "OPENSSL_ENGINES",
		"npm_config_node_options", "NPM_CONFIG_USERCONFIG",
		// chrome-devtools-mcp settings: a category switch, a crash mode.
		"CHROME_DEVTOOLS_MCP_CATEGORY_EXPERIMENTAL_THIRD_PARTY", "CHROME_DEVTOOLS_MCP_CRASH_ON_UNCAUGHT",
	}
	for _, k := range dropped {
		t.Setenv(k, "/hostile")
	}
	t.Setenv("CDPCOMPAT_KEPT", "kept")
	env := clientEnv(nil)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(dropped, k) {
			t.Errorf("the client environment inherits %s", kv)
		}
	}
	if !slices.Contains(env, "CDPCOMPAT_KEPT=kept") {
		t.Errorf("the client environment lost an unrelated variable: %q", env)
	}
}

// V8 coverage of the client scripts comes only from the driver option
// (v8CoverageVar): an inherited NODE_V8_COVERAGE is dropped, and the
// option adds NODE_V8_COVERAGE to every Node client's environment.
func TestNodeV8CoverageOnlyFromTheDriverOption(t *testing.T) {
	t.Setenv("NODE_V8_COVERAGE", "/hostile")
	saved := v8CoverageDir
	t.Cleanup(func() { v8CoverageDir = saved })
	coverage := func(client string) []string {
		var out []string
		for _, kv := range clientEnv(nodeClientEnv(client)) {
			if strings.HasPrefix(kv, "NODE_V8_COVERAGE=") {
				out = append(out, kv)
			}
		}
		return out
	}
	v8CoverageDir = ""
	for client := range nodeScripts {
		if got := coverage(client); len(got) != 0 {
			t.Errorf("%s without the driver option: %q, want no NODE_V8_COVERAGE", client, got)
		}
	}
	v8CoverageDir = "/cov/dir"
	for client := range nodeScripts {
		if got := coverage(client); !slices.Equal(got, []string{"NODE_V8_COVERAGE=/cov/dir"}) {
			t.Errorf("%s with the driver option: %q, want only NODE_V8_COVERAGE=/cov/dir", client, got)
		}
	}
	if got := nodeEnv["playwright-core"]; slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "NODE_V8_COVERAGE=") }) {
		t.Errorf("the option changed the shared nodeEnv table: %q", got)
	}
}

// treeOf lists the files below dir with their sizes and modes.
func treeOf(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(p + " " + fi.Mode().String() + " " + strconv.FormatInt(fi.Size(), 10) + " " + fi.ModTime().String() + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
