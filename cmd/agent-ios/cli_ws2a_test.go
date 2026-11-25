package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// reservedPage is physicalPage with the device reserved for the default
// session, so that page commands run.
func reservedPage(t *testing.T) ([]string, *fakewebkit.Server) {
	t.Helper()
	env, f, _, _ := physicalPage(t, false, fakewebkit.Options{})
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	return env, f
}

// WS2a brief, section 1: "eval <expression>" runs the registry command in
// the daemon (--json works; a thrown value exits 1 with its message and
// stack). WS2a round 2 decision: the legacy eval is gone, so a trailing
// ws:// URL is a usage error (exit 2) that names --page, and nothing is
// evaluated.
func TestBareEvalRunsTheRegistryCommand(t *testing.T) {
	env, f := reservedPage(t)
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		switch p.Expression {
		case "6*7":
			return map[string]any{"result": map[string]any{"type": "number", "value": 42, "description": "42"}}, nil
		case "boom()":
			return map[string]any{"result": map[string]any{"type": "object", "subtype": "error", "description": "Error: boom", "objectId": "e1"}, "wasThrown": true}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0, "description": "0"}}, nil
	})
	f.Handle("Runtime.callFunctionOn", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), `"e1"`) {
			return map[string]any{"result": map[string]any{"type": "string", "value": "boom@https://fixture.example/app.js:3:9"}}, nil
		}
		return map[string]any{}, nil
	})
	if res := runCLI(t, "", env, "eval", "6*7"); res.code != 0 || res.stdout != "42\n" {
		t.Fatalf("eval 6*7: exit %d, stdout %q, stderr %q; want 42", res.code, res.stdout, res.stderr)
	}
	if res := runCLI(t, "", env, "--json", "eval", "6*7"); res.code != 0 || strings.TrimSpace(res.stdout) != `{"type":"number","value":42}` {
		t.Fatalf("--json eval 6*7: exit %d, stdout %q, stderr %q; want the registry JSON", res.code, res.stdout, res.stderr)
	}
	res := runCLI(t, "", env, "eval", "boom()")
	if res.code != 1 || !strings.Contains(res.stderr, "Error: boom") || !strings.Contains(res.stderr, "boom@https://fixture.example/app.js:3:9") || res.stdout != "" {
		t.Fatalf("eval boom(): exit %d, stdout %q, stderr %q; want exit 1 with the message and the stack", res.code, res.stdout, res.stderr)
	}
	evaluations := len(f.ReceivedMethod("Runtime.evaluate"))
	if res := runCLI(t, "", env, "eval", "6*7", f.URL); res.code != 2 || !strings.Contains(res.stderr, "--page") || res.stdout != "" {
		t.Fatalf("eval 6*7 <ws-url>: exit %d, stdout %q, stderr %q; want the usage error that names --page (exit 2)", res.code, res.stdout, res.stderr)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")); n != evaluations {
		t.Fatalf("eval 6*7 <ws-url> sent %d evaluations, want none", n-evaluations)
	}
}

// WS2a brief, section 2: bare "cookies" is the registry listing (values
// hidden unless --show-values). WS2a round 2 decision: the trailing ws://
// URL form is a usage error (exit 2) that names --page.
func TestBareCookiesRunsTheRegistryListing(t *testing.T) {
	env, f := reservedPage(t)
	f.Result("Page.getCookies", map[string]any{"cookies": []webkit.Cookie{
		{Name: "sid", Value: secretValue, Domain: ".example.com", Path: "/", HTTPOnly: true, Secure: true, Session: true, SameSite: "Lax"},
	}})
	res := runCLI(t, "", env, "cookies")
	if res.code != 0 || res.stdout != "sid=(hidden)  domain=.example.com path=/ sameSite=Lax httpOnly secure session\n" || strings.Contains(res.stdout+res.stderr, secretValue) {
		t.Fatalf("cookies: exit %d, stdout %q, stderr %q; want the registry listing without the value", res.code, res.stdout, res.stderr)
	}
	if res := runCLI(t, "", env, "cookies", "--show-values"); res.code != 0 || !strings.HasPrefix(res.stdout, "sid="+secretValue+"  domain=.example.com") {
		t.Fatalf("cookies --show-values: exit %d, stdout %q, stderr %q; want the value", res.code, res.stdout, res.stderr)
	}
	res = runCLI(t, "", env, "--json", "cookies")
	var got struct {
		Cookies     []map[string]any `json:"cookies"`
		ValuesShown *bool            `json:"valuesShown"`
	}
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &got) != nil || len(got.Cookies) != 1 || got.Cookies[0]["name"] != "sid" || got.ValuesShown == nil || *got.ValuesShown || strings.Contains(res.stdout, secretValue) {
		t.Fatalf("--json cookies: exit %d, stdout %q, stderr %q; want the registry data without the value", res.code, res.stdout, res.stderr)
	}
	if res := runCLI(t, "", env, "cookies", f.URL); res.code != 2 || !strings.Contains(res.stderr, "--page") || strings.Contains(res.stdout+res.stderr, "s3cr") {
		t.Fatalf("cookies <ws-url>: exit %d, stdout %q, stderr %q; want the usage error that names --page (exit 2), no value", res.code, res.stdout, res.stderr)
	}
}

// Review ws2a-r2-b F5: a bad global option is the registry's usage error
// (exit 2) before any connection, for every global option that can fail to
// parse.
func TestRegistryGlobalOptionErrorsExit2(t *testing.T) {
	env := privateDaemon(t)
	for _, tc := range []struct {
		env  []string
		args []string
		want string
	}{
		{env, []string{"eval", "1", "--timeout", "bogus"}, "--timeout"},
		{env, []string{"status", "--timeout"}, "--timeout needs a value"},
		{with(env, "AGENT_IOS_TIMEOUT=bogus"), []string{"eval", "1"}, "AGENT_IOS_TIMEOUT"},
		{env, []string{"cookies", "--transport", "bogus"}, "--transport"},
		{env, []string{"cookies", "--transport"}, "--transport needs a value"},
		{env, []string{"--wait-lease", "bogus", "eval", "1"}, "--wait-lease"},
		{env, []string{"eval", "1", "--wait-lease"}, "--wait-lease needs a value"},
		{env, []string{"devices", "--session"}, "--session needs a value"},
		{env, []string{"tab", "list", "--reservation"}, "--reservation needs a value"},
		{env, []string{"snapshot", "--page"}, "--page needs a value"},
		{env, []string{"snapshot", "--max-output", "many"}, "--max-output"},
		{env, []string{"screenshot", "--timeout", "bogus"}, "--timeout"},
	} {
		r := runCLI(t, "", tc.env, tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the registry usage error naming %s (exit 2)", tc.args, r.code, r.stdout, r.stderr, tc.want)
		}
	}
	dir := strings.TrimPrefix(env[0], "XDG_RUNTIME_DIR=")
	if _, err := os.Stat(filepath.Join(dir, "agent-ios", "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a usage error started the daemon: stat of its socket: %v", err)
	}
	// WS2b: no line is a removed command's any more. A removed command
	// and a screenshot line with a ws:// URL get the same usage error
	// (exit 2) for the bad global option.
	for _, args := range [][]string{{"query-selector", "body", "--timeout", "bogus"}, {"screenshot", "ws://127.0.0.1:9/devtools/page/1", "--timeout", "bogus"}} {
		if r := runCLI(t, "", env, args...); r.code != 2 || !strings.Contains(r.stderr, "--timeout") {
			t.Errorf("%v: exit %d, stderr %q; want the usage error naming --timeout (exit 2)", args, r.code, r.stderr)
		}
	}
}

// echoEval makes f answer Runtime.evaluate with the string
// "evaluated:<expression>", so a test sees which expression ran.
func echoEval(f *fakewebkit.Server) {
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		return map[string]any{"result": map[string]any{"type": "string", "value": "evaluated:" + p.Expression}}, nil
	})
}

// Review ws2a-a F1 and ws2a-b F1/F2 (WS2a round 2 decisions): every eval
// line goes to the registry. A --page URL after the command selects the
// page, a token such as -1 is the expression, not a flag, and a malformed
// line is the registry's usage error (exit 2), never a legacy run.
func TestEvalLinesAllGoToTheRegistry(t *testing.T) {
	env, f := reservedPage(t)
	echoEval(f)
	for _, tc := range []struct {
		name string
		args []string
		want string // the stdout of a run that exits 0
	}{
		{"page flag before the expression", []string{"eval", "--page", f.URL, "6*7"}, "evaluated:6*7\n"},
		{"page flag after the expression", []string{"eval", "6*7", "--page", f.URL, "--json"}, `{"type":"string","value":"evaluated:6*7"}` + "\n"},
		{"negative expression before --json", []string{"eval", "-1", "--json"}, `{"type":"string","value":"evaluated:-1"}` + "\n"},
		{"negative expression after --json", []string{"eval", "--json", "-1"}, `{"type":"string","value":"evaluated:-1"}` + "\n"},
		{"negative fraction", []string{"eval", "-.5"}, "evaluated:-.5\n"},
		// Review ws2a-r2-a F5: only known flags are flags (WS2a round 4).
		{"unary minus on a call", []string{"eval", "-Math.abs(2)"}, "evaluated:-Math.abs(2)\n"},
		{"unary minus on a property", []string{"eval", "-window.scrollY", "--json"}, `{"type":"string","value":"evaluated:-window.scrollY"}` + "\n"},
		{"expression after --", []string{"eval", "--", "--x"}, "evaluated:--x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := runCLI(t, "", env, tc.args...); r.code != 0 || r.stdout != tc.want {
				t.Errorf("%v: exit %d, stdout %q, stderr %q; want %q", tc.args, r.code, r.stdout, r.stderr, tc.want)
			}
		})
	}
	for _, args := range [][]string{{"eval", "6*7", "unexpected"}, {"eval", "--bogus", "6*7"}, {"eval"}} {
		before := len(f.ReceivedMethod("Runtime.evaluate"))
		if r := runCLI(t, "", env, args...); r.code != 2 || !strings.Contains(r.stderr, "eval --help") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the registry usage error (exit 2)", args, r.code, r.stdout, r.stderr)
		}
		if n := len(f.ReceivedMethod("Runtime.evaluate")); n != before {
			t.Errorf("%v sent %d evaluations, want none", args, n-before)
		}
	}
}

// Review ws2a-a F1 and ws2a-b F1/F2/F3 (WS2a round 2 decisions): every
// cookies line goes to the registry, so no form prints any part of a
// value without --show-values. AGENT_IOS_WS_URL no longer selects the page: it
// is a usage error (exit 2) that names --page, unless --page is given.
func TestCookiesLinesAllGoToTheRegistry(t *testing.T) {
	env, f := reservedPage(t)
	const value = "abcd-synthetic-secret-value"
	f.Result("Page.getCookies", map[string]any{"cookies": []webkit.Cookie{{Name: "sid", Value: value, Domain: "example.test", Path: "/", Session: true}}})
	const listing = "sid=(hidden)  domain=example.test path=/ session\n"
	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{"page flag", env, []string{"cookies", "--page", f.URL}},
		{"page flag with AGENT_IOS_WS_URL", with(env, "AGENT_IOS_WS_URL="+f.URL), []string{"cookies", "--page", f.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := runCLI(t, "", tc.env, tc.args...); r.code != 0 || r.stdout != listing || strings.Contains(r.stderr, "abcd") {
				t.Errorf("%v: exit %d, stdout %q, stderr %q; want the registry listing %q", tc.args, r.code, r.stdout, r.stderr, listing)
			}
		})
	}
	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want string // in stderr
	}{
		{"AGENT_IOS_WS_URL", with(env, "AGENT_IOS_WS_URL="+f.URL), []string{"cookies"}, "--page"},
		{"AGENT_IOS_WS_URL with --json", with(env, "AGENT_IOS_WS_URL="+f.URL), []string{"--json", "cookies"}, "--page"},
		{"unknown flag", env, []string{"cookies", "--bogus"}, "cookies --help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.ReceivedMethod("Page.getCookies"))
			r := runCLI(t, "", tc.env, tc.args...)
			if r.code != 2 || !strings.Contains(r.stderr, tc.want) || strings.Contains(r.stdout+r.stderr, "abcd") {
				t.Errorf("%v: exit %d, stdout %q, stderr %q; want the usage error naming %s (exit 2), no value", tc.args, r.code, r.stdout, r.stderr, tc.want)
			}
			if n := len(f.ReceivedMethod("Page.getCookies")); n != before {
				t.Errorf("%v read the cookies %d times, want none", tc.args, n-before)
			}
		})
	}
}

// WS2a round 2 decision: AGENT_IOS_WS_URL no longer selects the page of eval
// (exit 2 naming --page, nothing evaluated); an explicit --page wins over
// it (review ws2a-b F1, environment-json).
func TestEvalRefusesIWDPWSURL(t *testing.T) {
	env, f := reservedPage(t)
	echoEval(f)
	e := with(env, "AGENT_IOS_WS_URL="+f.URL)
	before := len(f.ReceivedMethod("Runtime.evaluate"))
	if r := runCLI(t, "", e, "eval", "1+1", "--json"); r.code != 2 || !strings.Contains(r.stderr, "AGENT_IOS_WS_URL") || !strings.Contains(r.stderr, "--page") || r.stdout != "" {
		t.Errorf("eval with AGENT_IOS_WS_URL: exit %d, stdout %q, stderr %q; want the usage error naming --page (exit 2)", r.code, r.stdout, r.stderr)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")); n != before {
		t.Errorf("eval with AGENT_IOS_WS_URL sent %d evaluations, want none", n-before)
	}
	if r := runCLI(t, "", e, "eval", "6*7", "--page", f.URL, "--json"); r.code != 0 || !strings.Contains(r.stdout, `"value":"evaluated:6*7"`) {
		t.Errorf("eval --page with AGENT_IOS_WS_URL: exit %d, stdout %q, stderr %q; want the registry JSON", r.code, r.stdout, r.stderr)
	}
}

// Review ws2a-a F2: an empty string prints one empty line, as the legacy
// eval's fmt.Println did; so does a string that ends with a newline get
// its own newline.
func TestEvalStringKeepsThePrintlnLine(t *testing.T) {
	env, f := reservedPage(t)
	for value, want := range map[string]string{"": "\n", "a\n": "a\n\n", "a": "a\n"} {
		f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "string", "value": value}})
		if r := runCLI(t, "", env, "eval", "s"); r.code != 0 || r.stdout != want {
			t.Errorf("eval of the string %q: exit %d, stdout %q, stderr %q; want %q", value, r.code, r.stdout, r.stderr, want)
		}
	}
}

// The deadline contract (CLAUDE.md, Deadlines) for a second registry page
// command: a deadline error exits 124 and names the command, the page, the
// elapsed time, the last wait and the likely causes. The page is the
// --page value, else the session's page that the daemon reports.
func TestRegistryDeadlineNamesCommandPageAndCauses(t *testing.T) {
	env, f := reservedPage(t)
	f.Stall("Page.getCookies")
	for _, tc := range []struct {
		name string
		args []string
		page string // in stderr
	}{
		{"page flag", []string{"cookies", "--page", f.URL, "--timeout", "1s"}, "Page: " + f.URL},
		// The title is the listing's, which can be older than the page's
		// document (device check of 2026-11-11): the message says so.
		{"session page", []string{"cookies", "--timeout", "1s"}, "Page: \"Fixture\" (title as listed; the page may have navigated since; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCLI(t, "", env, tc.args...)
			if r.code != exitTimeout {
				t.Fatalf("%v: exit %d, stderr %q; want %d", tc.args, r.code, r.stderr, exitTimeout)
			}
			for _, want := range []string{"Error: agent-ios cookies timed out after", "(deadline ", tc.page, "Last wait: ", "Page.getCookies", "another inspector session", "device is locked", "--timeout <duration>"} {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("%v: stderr lacks %q: %s", tc.args, want, r.stderr)
				}
			}
		})
	}
}

// Review ws2a-r2-c F5: the daemon gets the whole deadline for the
// operation. A wait of 4.7 s fits --timeout 5s, including wait's 150 ms
// reserve; the round 3 design cut the daemon's deadline by 200 ms, and
// the wait failed at 4.66 s.
func TestDaemonKeepsTheFullDeadline(t *testing.T) {
	env, _ := reservedPage(t)
	if r := runCLI(t, "", env, "eval", "1"); r.code != 0 {
		t.Fatalf("eval 1 (warms the page): exit %d, stderr %q", r.code, r.stderr)
	}
	if r := runCLI(t, "", env, "wait", "4700", "--timeout", "5s"); r.code != 0 {
		t.Fatalf("wait 4700 --timeout 5s: exit %d after %v, stderr %q; want the wait to fit the deadline", r.code, r.elapsed, r.stderr)
	}
}

// wantFullDeadlineMessage checks the CLI's deadline message of command
// (CLAUDE.md, Deadlines): exit 124, the command, the page line page, the
// elapsed time, the last wait and the likely causes.
func wantFullDeadlineMessage(t *testing.T, what string, code int, stderr, command, page string) {
	t.Helper()
	if code != exitTimeout {
		t.Errorf("%s: exit %d, stderr %q; want %d", what, code, stderr, exitTimeout)
		return
	}
	for _, want := range []string{"Error: agent-ios " + command + " timed out after", "(deadline ", "Page: " + page, "Last wait: ", "Likely causes:", "--timeout <duration>"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr lacks %q: %s", what, want, stderr)
		}
	}
}

// Review ws2a-r2-c F6: every deadline path prints the full message, also
// during the client steps: a read of the standard input that never ends
// (fill --stdin) and the wait for the daemon's start lock.
func TestSetupDeadlinePrintsTheFullMessage(t *testing.T) {
	t.Run("blocked stdin", func(t *testing.T) {
		cmd := cliCommand(t, "", privateDaemon(t), "fill", "input", "--stdin", "--page", "Fixture", "--timeout", "300ms")
		w, err := cmd.StdinPipe() // held open: the read never ends
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait() // the exit code is checked below
		wantFullDeadlineMessage(t, "fill --stdin with an open stdin", cmd.ProcessState.ExitCode(), stderr.String(), "fill", "Fixture")
		if !strings.Contains(stderr.String(), "standard input") {
			t.Errorf("fill --stdin: stderr %q; want the read of the standard input as the last wait", stderr.String())
		}
	})
	t.Run("daemon start lock", func(t *testing.T) {
		env := privateDaemon(t)
		dir := filepath.Join(strings.TrimPrefix(env[0], "XDG_RUNTIME_DIR="), "agent-ios")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		r := runCLI(t, "", env, "eval", "1", "--page", "ws://example.test/p", "--timeout", "300ms")
		wantFullDeadlineMessage(t, "eval while another process holds the daemon lock", r.code, r.stderr, "eval", "ws://example.test/p")
		if !strings.Contains(r.stderr, "daemon lock") {
			t.Errorf("eval: stderr %q; want the wait for the daemon lock as the last wait", r.stderr)
		}
	})
}

// Review ws2a-r2-c F7: the daemon reports the page as soon as the
// command's page is resolved, before the attach, the lease and the queue:
// a session that waits for another session's lease names the page. tab,
// tab new and tab close report the page they operate on.
func TestDeadlineNamesThePageDuringTheLeaseWait(t *testing.T) {
	env, _ := reservedPage(t)
	s2 := []string{"--reservation", "default", "--session", "s2"}
	run := func(session string, args ...string) cliResult {
		return runCLI(t, "", env, append([]string{"--reservation", "default", "--session", session}, args...)...)
	}
	if r := run("s2", "eval", "1"); r.code != 0 {
		t.Fatalf("s2 eval 1: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run("s2", "close"); r.code != 0 {
		t.Fatalf("s2 close: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run("s1", "eval", "1"); r.code != 0 {
		t.Fatalf("s1 eval 1 (takes the lease): exit %d, stderr %q", r.code, r.stderr)
	}
	for _, args := range [][]string{{"eval", "1"}, {"tab", "t1"}, {"tab", "new"}, {"tab", "close"}} {
		r := runCLI(t, "", env, append(append(append([]string{}, s2...), "--wait-lease", "3s", "--timeout", "1s"), args...)...)
		command := args[0]
		if args[0] == "tab" && args[1] != "t1" {
			command = "tab " + args[1]
		}
		wantFullDeadlineMessage(t, strings.Join(args, " ")+" during the lease wait", r.code, r.stderr, command, "\"Fixture\" (")
	}
}
