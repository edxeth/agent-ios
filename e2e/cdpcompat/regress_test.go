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
			code, data, err := e.execNode(t, "fatal-"+client+"-"+form, client, "resolve", url, inject)
			if err != nil {
				t.Fatalf("%s %s: %v", client, form, err)
			}
			if _, err := judgeNodeRun(code, data, client, "resolve", url); err == nil || !strings.Contains(err.Error(), "exit 7, ok false") {
				t.Errorf("%s %s with an injected failure: %v, want the failed run reported", client, form, err)
			}
		}
	}

	ok := []byte(`{"client":"puppeteer-core","version":"25.16.0","scenario":"resolve","form":"ws","url":"ws://x","ok":true,"steps":[{"name":"connect","ok":false,"ms":1,"error":"refused"}],"sent":[]}`)
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
		{"other version", 0, strings.Replace(string(ok), "25.16.0", "25.15.0", 1)},
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
	if strings.Contains(out, "auto-connect") || len(got) != 1 || got[0].URI != wsPath || got[0].Status != 101 {
		t.Errorf("agent-browser with a hostile user configuration: exit %d, output %q, requests %s; want one upgrade of the printed ws path", code, e.redact(out), e.redact(lines(got)))
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
