package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state group through the real CLI and its daemon (WP-DIFFSTATE,
// agent-browser 0.45 contract section 7): named states live in
// $XDG_STATE_HOME/agent-ios/states of the daemon's environment; list,
// show, rename and clear need no page.
func TestCLIStateGroup(t *testing.T) {
	home := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + home}
	dir := filepath.Join(home, "agent-ios", "states")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"version":1,"cookies":[{"name":"sid","value":"s3cret","domain":"app.test","path":"/","expires":-1,"httpOnly":false,"secure":false,"sameSite":"Lax"}],"origins":[{"origin":"https://app.test","localStorage":[],"sessionStorage":[]}]}`
	if err := os.WriteFile(filepath.Join(dir, "login.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", env, "--json", "state", "list")
	var list struct {
		Files []struct {
			Filename string `json:"filename"`
			Path     string `json:"path"`
		} `json:"files"`
		Directory string `json:"directory"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &list) != nil || list.Directory != dir || len(list.Files) != 1 || list.Files[0].Filename != "login.json" {
		t.Fatalf("state list --json: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, "", env, "state", "show", "login")
	if r.code != 0 || r.stdout != "State file summary:\n  Cookies: 1\n  Origins: 1\n" || strings.Contains(r.stdout+r.stderr, "s3cret") {
		t.Fatalf("state show login: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	if r = runCLI(t, "", env, "state", "rename", "login", "old"); r.code != 0 || r.stdout != "Renamed login -> old\n" {
		t.Fatalf("state rename: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	if r = runCLI(t, "", env, "state", "clear"); r.code != 2 {
		t.Fatalf("state clear without a name: exit %d, %q", r.code, r.stderr)
	}
	if r = runCLI(t, "", env, "state", "clear", "--all"); r.code != 0 || r.stdout != "Cleared 1 state file in "+dir+"\n" {
		t.Fatalf("state clear --all: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	if r = runCLI(t, "", env, "state", "show", "old"); r.code != 1 || !strings.Contains(r.stderr, filepath.Join(dir, "old.json")) {
		t.Fatalf("state show after clear: exit %d, %q", r.code, r.stderr)
	}
}

// tab keep and diff url --full fail as the contract says, through the CLI.
func TestCLITabKeepAndDiffURLUsage(t *testing.T) {
	r := runCLI(t, "", nil, "tab", "keep", "frozen")
	if r.code != 1 || !strings.Contains(r.stderr, "unsupported: tab keep frozen:") {
		t.Fatalf("tab keep frozen: exit %d, %q", r.code, r.stderr)
	}
	r = runCLI(t, "", nil, "diff", "url", "http://a.test/", "http://b.test/", "--full")
	if r.code != 2 || !strings.Contains(r.stderr, "--full only applies to the screenshot diff; add --screenshot") {
		t.Fatalf("diff url --full: exit %d, %q", r.code, r.stderr)
	}
}
