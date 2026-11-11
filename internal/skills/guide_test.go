package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// The tests of this file run the guide skill-data/iwdp/SKILL.md, as
// "iwdp-cli skills get iwdp" prints it, against a fake iwdp-cli. The guide's
// discovery, reservation and diagnostic commands print allowlisted fields
// only, and the exact exit status on success and failure, also under
// "set -eo pipefail" (the shell of the WebKitGTK doc test). The fake's
// private values (page titles, URLs, page keys with the device identifier,
// labels, other sessions' names, error messages) must never reach the
// output.

const guidePath = "skill-data/iwdp/SKILL.md"

// guideBashBlocks returns the bodies of the ```bash blocks of the guide.
func guideBashBlocks(t *testing.T) []string {
	t.Helper()
	var bodies []string
	for _, b := range guideBlocks(t) {
		bodies = append(bodies, b.body)
	}
	return bodies
}

// bashBlock is one ```bash block of the guide; skip is true when a
// "<!-- doc-test: skip" marker stands on the line before it (the
// WebKitGTK doc test does not run it).
type bashBlock struct {
	body string
	skip bool
}

func guideBlocks(t *testing.T) []bashBlock {
	t.Helper()
	var blocks []bashBlock
	var cur []string
	in, skip, prev := false, false, ""
	for _, line := range strings.Split(readRepo(t, guidePath), "\n") {
		switch {
		case !in && strings.TrimSpace(line) == "```bash":
			in, cur, skip = true, nil, strings.HasPrefix(prev, "<!-- doc-test: skip")
		case in && strings.TrimSpace(line) == "```":
			in = false
			blocks = append(blocks, bashBlock{strings.Join(cur, "\n"), skip})
		case in:
			cur = append(cur, line)
		}
		prev = line
	}
	return blocks
}

// guideSpans returns the inline code spans of the guide's text outside
// fenced blocks, each with its line number.
func guideSpans(t *testing.T) []guideSpan {
	t.Helper()
	span := regexp.MustCompile("`([^`]+)`")
	var out []guideSpan
	in := false
	for i, line := range strings.Split(readRepo(t, guidePath), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if in {
			continue
		}
		for _, m := range span.FindAllStringSubmatch(line, -1) {
			out = append(out, guideSpan{i + 1, m[1]})
		}
	}
	return out
}

type guideSpan struct {
	line int
	code string
}

// guideBlock returns the one bash block of the guide that contains marker.
func guideBlock(t *testing.T, marker string) string {
	t.Helper()
	var found []string
	for _, b := range guideBashBlocks(t) {
		if strings.Contains(b, marker) {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s has %d bash blocks with %q, want 1", guidePath, len(found), marker)
	}
	return found[0]
}

// privateMark is in every private value of the fake; none may be printed.
const privateMark = "PRIVATE"

// fakeCLI is an iwdp-cli whose output follows the real formats. It
// appends the arguments of each call as a JSON line to FAKE_LOG and, like
// splitGlobals (internal/commands/cli.go), takes the global options and
// their values out of the arguments; --json selects the JSON result
// (Result.Data, writeResult).
//   - "session info --json" and "tab list --json" print FAKE_SESSION_JSON
//     and FAKE_TABS_JSON.
//   - "tab <id>" prints the selection line and, with FAKE_WARNING, the
//     warning of releaseWarning that names a prior page by its quoted
//     title; its JSON has only "tab" and "key" (internal/commands/tab.go).
//   - Element and key actions print their done line, the emulated default
//     actions (FAKE_EMULATED, page text that can hold newlines), the dialog
//     lines of dialogLines with FAKE_DIALOG as the message, FAKE_WARNING
//     lines and the synthetic-input note (w3bPage.text); their JSON is
//     actResult (internal/commands/w3b_page.go) with FAKE_DIALOGS dialogs
//     and FAKE_WARNINGS warnings (default 1 each when FAKE_DIALOG or
//     FAKE_WARNING is set, else 0).
//   - Navigations (open, navigate, back, forward, reload) print the URL and
//     a text-only warning; their JSON has "url" and "title" (open.go).
//     "wait" prints its done line; its JSON has "ok" and "state" (wait.go).
//   - "dialog" prints its armed-answer or status lines; its JSON has the
//     policy, the answer ids and the log of answered dialogs (dialog.go).
//   - "eval" of the target check's expression prints whether the target
//     equals FAKE_PAGE_URL, the page's full URL; another "eval" prints
//     content.
//   - "get cdp-url" prints FAKE_CDP_URL, a URL with a secret token.
//   - The content reads, evidence, DevTools, cookie and close commands
//     print a content line; "snapshot" prints a tree.
//   - Every other command prints private text.
//
// Every call prints FAKE_STDERR on stderr. The command whose arguments
// contain FAKE_FAIL, or the call with number FAKE_FAIL_AT (from 1), exits
// FAKE_EXIT; when that is not 0, it prints only an error with private text
// on stderr, as iwdp-cli's errors that name the page or the device do.
const fakeCLI = `import json, os, re, sys
argv = sys.argv[1:]
with open(os.environ["FAKE_LOG"], "a") as f:
    f.write(json.dumps(argv) + "\n")
with open(os.environ["FAKE_LOG"]) as f:
    call = sum(1 for _ in f)
args = " ".join(argv)
private = "PRIVATE-TITLE https://private.example/PRIVATE-PATH?token=PRIVATE-TOKEN iwdp|PRIVATE-UDID|1 PRIVATE-SESSION"
if os.environ.get("FAKE_STDERR"):
    print(os.environ["FAKE_STDERR"], file=sys.stderr)
fail, code = os.environ.get("FAKE_FAIL"), int(os.environ.get("FAKE_EXIT", "0"))
if code and ((fail and fail in args) or os.environ.get("FAKE_FAIL_AT") == str(call)):
    print("PRIVATE-ERR", private, file=sys.stderr)
    sys.exit(code)
valued = ("--session", "--page", "--timeout", "--transport", "--max-output", "--reservation", "--wait-lease")
boolean = ("--json", "--content-boundaries", "--takeover")
# As splitGlobals and boolGlobal: options up to "--", a boolean alone is
# true, "--name=<v>" takes the values of strconv.ParseBool, and the last
# occurrence wins.
truth = {"1": True, "t": True, "T": True, "TRUE": True, "true": True, "True": True, "0": False, "f": False, "F": False, "FALSE": False, "false": False, "False": False}
a, as_json, i = [], False, 0
while i < len(argv):
    if argv[i] == "--":
        a += argv[i:]
        break
    name = argv[i].split("=", 1)[0]
    if name in valued:
        i += 1 if "=" in argv[i] else 2
        continue
    if name in boolean:
        v = argv[i].split("=", 1)[1] if "=" in argv[i] else "true"
        if v not in truth:
            print("Error: %s %s: want true or false" % (name, json.dumps(v)), file=sys.stderr)
            sys.exit(2)
        if name == "--json":
            as_json = truth[v]
        i += 1
        continue
    a.append(argv[i])
    i += 1
warning, dialog = os.environ.get("FAKE_WARNING"), os.environ.get("FAKE_DIALOG")
n_warn = int(os.environ.get("FAKE_WARNINGS", "1" if warning else "0"))
n_dlg = int(os.environ.get("FAKE_DIALOGS", "1" if dialog else "0"))
uses = [{"id": "d1", "type": "confirm", "message": dialog or "", "result": True}] * n_dlg
emulated = [os.environ.get("FAKE_EMULATED", "implicit submission: clicked <button id=\"go\">")]
q = lambda v: json.dumps(v, ensure_ascii=False)
out = lambda v: print(json.dumps(v, ensure_ascii=False))
actions = ("click", "dblclick", "tap", "fill", "type", "press", "hover", "check", "uncheck", "select", "drag", "find")
navigations = ("open", "navigate", "back", "forward", "reload")
content = ("get", "is", "hit-test", "read", "console", "errors", "network", "vitals", "screenshot", "set", "trace", "cookies", "storage", "state", "close")
if as_json and a[:2] == ["session", "info"]:
    print(os.environ["FAKE_SESSION_JSON"])
elif as_json and a[:2] == ["tab", "list"]:
    print(os.environ["FAKE_TABS_JSON"])
elif a[:1] == ["tab"]:
    if as_json:
        out({"tab": a[1], "key": "iwdp|PRIVATE-UDID|" + private})
    else:
        print("tab %s is now the current page of session %s" % (a[1], q("qa")))
        if warning:
            print("warning: releasing the lease of %s: %s" % (q(warning), warning.replace("\n", " ")))
elif a[:1] == ["eval"]:
    m = re.fullmatch(r"location\.href === new URL\((.*)\)\.href", a[1], re.S)
    print(("true" if json.loads(m.group(1)) == os.environ["FAKE_PAGE_URL"] else "false") if m else "content: eval")
elif a[:1] == ["snapshot"]:
    print('- button "Sign in" [ref=e3]')
elif a[:1] == ["dialog"]:
    if as_json:
        out({"policy": "dismiss", "active": True, "log": uses, "queued": [], "issued": ["d1"] if a[1] == "accept" else []})
    elif a[1] in ("accept", "dismiss"):
        print("armed dialog answer d1: " + a[1])
    elif a[1] == "default":
        print("dialogs without an armed answer: " + a[2])
    else:
        print("dialog override: on")
        print("default: dismiss")
        print("answers waiting in the page: 0; queued in the daemon: 0")
        for u in uses:
            print("dialog %s %s answered true (answer d1)" % (u["type"], q(u["message"])))
elif a[:1] and a[0] in actions:
    if as_json:
        out({"trusted": False, "steps": 1, "acknowledged": 1, "emulated": emulated, "value": private, "dialogs": uses, "warnings": [warning or ""] * n_warn})
    else:
        print("done: " + a[0])
        for e in emulated:
            print("emulated default action: " + e)
        for u in uses:
            print("dialog %s %s answered true (answer d1)" % (u["type"], q(u["message"])))
        for _ in range(n_warn):
            print("warning: " + (warning or ""))
        print("note: synthetic event (isTrusted=false); behavior that checks isTrusted or needs a user gesture may differ")
elif a[:1] and a[0] in navigations:
    if as_json:
        out({"url": "https://private.example/" + private, "title": private})
    else:
        print("navigated to " + private)
        if warning:
            print("warning: issuing the armed dialog answers into the new document: " + warning)
elif a[:1] == ["wait"]:
    if as_json:
        out({"ok": True, "state": "visible"})
    else:
        print("done: " + private)
elif a[:2] == ["get", "cdp-url"]:
    print(os.environ.get("FAKE_CDP_URL", "ws://127.0.0.1:9230/devtools/browser/PRIVATE-TOKEN"))
elif a[:1] and a[0] in content:
    print("content: " + a[0])
else:
    print("PRIVATE-OUT", private)
`

const fakeSessionJSON = `{"session":"PRIVATE-SESSION","pages":[{"key":"iwdp|PRIVATE-UDID|ws://127.0.0.1:9224/devtools/page/1","title":"PRIVATE-TITLE","url":"https://private.example/PRIVATE-PATH","device":"PRIVATE-DEVICE","transport":"iwdp","state":"attached","session":"PRIVATE-SESSION","lease":"active","epoch":3,"since":"PRIVATE-SINCE","lastUsed":"PRIVATE-LAST-USED"}]}`

const fakeTabsJSON = `{"tabs":[` +
	`{"id":"t1","label":"PRIVATE-LABEL","key":"iwdp|PRIVATE-UDID|1","title":"PRIVATE-TITLE","url":"https://private.example/PRIVATE-PATH","device":"PRIVATE-DEVICE","debuggable":true,"current":true,"leasedBy":"PRIVATE-SESSION"},` +
	`{"id":"t2","key":"iwdp|PRIVATE-UDID|2","title":"PRIVATE-OTHER-TITLE","url":"https://private.example/PRIVATE-OTHER","device":"PRIVATE-DEVICE","debuggable":false,"current":false,"leasedBy":"PRIVATE-OTHER-SESSION"}` +
	`],"hidden":2}`

// fakeListingJSON is the device listing of port 9221 for the device check.
const fakeListingJSON = `[{"deviceId":"PRIVATE-UDID","deviceName":"PRIVATE-NAME","deviceOSVersion":"PRIVATE-OS","url":"PRIVATE-HOST:9224"},{"deviceId":"SIMULATOR","deviceName":"PRIVATE-SIM-NAME","url":"PRIVATE-HOST:9225"}]`

// fakeAgentBrowser is a CDP client: "agent-browser --cdp <url> snapshot -i"
// prints its snapshot (page content); it fails when the URL is not the one
// that "get cdp-url" printed, and never prints the URL.
const fakeAgentBrowser = `import os, sys
a = sys.argv[1:]
if a[:1] != ["--cdp"] or len(a) < 3 or a[1] != os.environ.get("FAKE_CDP_URL", "ws://127.0.0.1:9230/devtools/browser/PRIVATE-TOKEN"):
    print("agent-browser: no CDP endpoint", file=sys.stderr)
    sys.exit(1)
print("- button \"Sign in\" [ref=e1]")
`

// fakeCurl answers the device check with FAKE_LISTING_JSON.
const fakeCurl = `import os, sys
fail, code = os.environ.get("FAKE_FAIL"), int(os.environ.get("FAKE_EXIT", "0"))
if fail == "curl" and code:
    print("curl: (7) PRIVATE-ERR", file=sys.stderr)
    sys.exit(code)
print(os.environ["FAKE_LISTING_JSON"])
`

// hiddenHint is the fixed sentence of the listing for hidden pages: it
// names neither the owner nor a device.
const hiddenHint = "Pages of physical devices without a reservation of your owner are not listed: reserve the device as in Start a run, or ask the device owner."

type shellRun struct {
	code           int
	stdout, stderr string
	calls          [][]string // the arguments of each iwdp-cli call
}

// guideShell runs guide blocks with bash -eo pipefail and a PATH of the
// fakes (iwdp-cli, curl, agent-browser) and of python3, mkdir, cat and rm only: tools
// that macOS has too; GNU coreutils such as timeout are not on it. noPython
// leaves python3 off the PATH (the fakes still run).
type guideShell struct {
	noPython bool
	env      []string
}

func (g guideShell) run(t *testing.T, script string, env ...string) shellRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the guide examples need bash: %v", err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("the guide examples need python3: %v", err)
	}
	dir := t.TempDir()
	bin, tools := filepath.Join(dir, "bin"), filepath.Join(dir, "tools")
	for _, d := range []string{bin, tools} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, src := range map[string]string{"iwdp-cli": fakeCLI, "curl": fakeCurl, "agent-browser": fakeAgentBrowser} {
		// -S skips the site module: the fakes need only the standard
		// library, and each call starts faster.
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!"+python+" -S\n"+src), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"mkdir", "cat", "rm"}
	if !g.noPython {
		names = append(names, "python3")
	}
	for _, name := range names {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("the guide examples need %s: %v", name, err)
		}
		if err := os.Symlink(p, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls.log")
	cmd := exec.Command(bash, "-eo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + tools,
		"HOME=" + dir,
		"FAKE_LOG=" + log,
		"FAKE_PAGE_URL=" + targetURL,
		"EVIDENCE=" + filepath.Join(dir, "evidence"),
		"FAKE_SESSION_JSON=" + fakeSessionJSON,
		"FAKE_TABS_JSON=" + fakeTabsJSON,
		"FAKE_LISTING_JSON=" + fakeListingJSON,
	}, g.env...), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	r := shellRun{stdout: stdout.String(), stderr: stderr.String()}
	switch {
	case errors.As(err, &exit):
		r.code = exit.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if l == "" {
			continue
		}
		var call []string
		if err := json.Unmarshal([]byte(l), &call); err != nil {
			t.Fatalf("fake iwdp-cli log line %q: %v", l, err)
		}
		r.calls = append(r.calls, call)
	}
	return r
}

// runGuideShell runs script in the default guideShell.
func runGuideShell(t *testing.T, script string, env ...string) shellRun {
	t.Helper()
	return guideShell{}.run(t, script, env...)
}

// checkRun compares a run with the wanted exit code and stdout, and checks
// that stderr is empty and nothing private was printed.
func checkRun(t *testing.T, r shellRun, code int, stdout string) {
	t.Helper()
	if r.code != code || r.stdout != stdout || r.stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, stdout %q, empty stderr", r.code, r.stdout, r.stderr, code, stdout)
	}
	if strings.Contains(r.stdout+r.stderr, privateMark) {
		t.Errorf("private data printed: stdout %q, stderr %q", r.stdout, r.stderr)
	}
}

// checkNoPython is checkRun for a run without python3 on the PATH: bash's
// own "command not found" line on stderr names no page and is allowed.
func checkNoPython(t *testing.T, r shellRun, code int, stdout string) {
	t.Helper()
	var keep []string
	for _, l := range strings.SplitAfter(r.stderr, "\n") {
		if !strings.HasSuffix(strings.TrimSuffix(l, "\n"), "python3: command not found") {
			keep = append(keep, l)
		}
	}
	r.stderr = strings.Join(keep, "")
	checkRun(t, r, code, stdout)
}

// targetURL is the task's target, and the full URL of the fake's page
// unless FAKE_PAGE_URL names another.
const targetURL = "https://private.example/PRIVATE-PATH?token=PRIVATE-TOKEN"

// targetFile writes the task's target URL to a file, as the guide reads it.
func targetFile(t *testing.T) string {
	t.Helper()
	return writeTarget(t, targetURL)
}

func writeTarget(t *testing.T, url string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(p, []byte(url), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Safety rule 3's health probe prints the lease, epoch and connection state
// of each page and the exact status, never a title, URL or page key, and
// needs no timeout command. A filter failure is reported as the filter's
// (review WS6-A F1, F7, F8, F12).
func TestGuideHealthProbePrintsOnlyStateAndStatus(t *testing.T) {
	probe := guideBlock(t, "session info --json")
	for _, code := range []int{0, 4, 124} {
		t.Run("exit "+strconv.Itoa(code), func(t *testing.T) {
			want := "session info exit " + strconv.Itoa(code) + "\n"
			if code == 0 {
				want = "page 1 lease active epoch 3 connection attached\n" + want
			}
			checkRun(t, runGuideShell(t, probe, "FAKE_FAIL=session info", "FAKE_EXIT="+strconv.Itoa(code)), code, want)
		})
	}
	t.Run("no python3", func(t *testing.T) {
		checkNoPython(t, guideShell{noPython: true}.run(t, probe), 127, "filter exit 127\n")
	})
}

// The start of a run reserves the device and lists the tabs with
// allowlisted fields only: per tab the current mark, the id and whether it
// is debuggable, no target claim (tab list drops query and fragment); then
// the hidden count and, when pages are hidden, a fixed hint (review WS6-A
// F4, F5, F9, F11, F12).
func TestGuideStartOfRunPrintsOnlyAllowlistedFields(t *testing.T) {
	start := guideBlock(t, "iwdp-cli device reserve")
	run := func(t *testing.T, fail string, code int) shellRun {
		t.Helper()
		return runGuideShell(t, start, "FAKE_FAIL="+fail, "FAKE_EXIT="+strconv.Itoa(code))
	}
	t.Run("success", func(t *testing.T) {
		checkRun(t, run(t, "", 0), 0, "device reserve exit 0\n* t1 debuggable\n  t2 not-debuggable\nhidden 2\n"+hiddenHint+"\ntab list exit 0\n")
	})
	for _, code := range []int{4, 124} {
		t.Run("tab list exit "+strconv.Itoa(code), func(t *testing.T) {
			checkRun(t, run(t, "tab list", code), code, "device reserve exit 0\ntab list exit "+strconv.Itoa(code)+"\n")
		})
	}
	for _, code := range []int{6, 124} {
		t.Run("device reserve exit "+strconv.Itoa(code), func(t *testing.T) {
			checkRun(t, run(t, "device reserve", code), code, "device reserve exit "+strconv.Itoa(code)+"\n")
		})
	}
	t.Run("no python3", func(t *testing.T) {
		checkNoPython(t, guideShell{noPython: true}.run(t, start), 127, "device reserve exit 0\nfilter exit 127\n")
	})
}

// The device check prints the number of devices only, with the statuses of
// curl and of its filter apart.
func TestGuideDeviceCheckPrintsOnlyTheDeviceCount(t *testing.T) {
	check := guideBlock(t, "curl -fsS")
	checkRun(t, runGuideShell(t, check), 0, "devices 1\ncurl exit 0\n")
	checkRun(t, runGuideShell(t, check, "FAKE_FAIL=curl", "FAKE_EXIT=7"), 7, "curl exit 7\n")
	checkNoPython(t, guideShell{noPython: true}.run(t, check), 127, "filter exit 127\n")
}

// evalTarget is the argument of the target check's eval for targetURL.
const evalTarget = `location.href === new URL("` + targetURL + `").href`

// The target check's first command on the chosen tab is a read-only eval
// with --page that compares the page's full URL with the target; it never
// opens or navigates the tab, a mismatch (also a URL that differs only in
// its query) stops it with "target false" and exit 1, and it selects the
// tab only after a match. Selection prints only its status: its JSON has
// no warnings or dialogs (review WS6-A F10, F11, F12, F16; criterion 9a).
func TestGuideTargetCheckIsReadOnlyAndFirst(t *testing.T) {
	check := guideBlock(t, "echo \"target true\"")
	g := guideShell{env: []string{"TARGET_FILE=" + targetFile(t), "FAKE_WARNING=PRIVATE-PRIOR-TITLE"}}
	eval := []string{"--page", "t1", "eval", evalTarget}
	sel := []string{"--json", "tab", "t1"}
	for _, c := range []struct {
		name  string
		env   []string
		code  int
		want  string
		calls [][]string
	}{
		{"match", nil, 0, "eval exit 0\ntarget true\ntab exit 0\n", [][]string{eval, sel}},
		{"unrelated tab", []string{"FAKE_PAGE_URL=https://private.example/PRIVATE-UNRELATED"}, 1, "eval exit 0\ntarget false\n", [][]string{eval}},
		{"other query", []string{"FAKE_PAGE_URL=https://private.example/PRIVATE-PATH"}, 1, "eval exit 0\ntarget false\n", [][]string{eval}},
		{"eval fails", []string{"FAKE_FAIL=eval", "FAKE_EXIT=4"}, 4, "eval exit 4\n", [][]string{eval}},
		{"eval deadline", []string{"FAKE_FAIL=eval", "FAKE_EXIT=124"}, 124, "eval exit 124\n", [][]string{eval}},
		{"tab fails", []string{"FAKE_FAIL=tab t1", "FAKE_EXIT=4"}, 4, "eval exit 0\ntarget true\ntab exit 4\n", [][]string{eval, sel}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := g.run(t, check, c.env...)
			checkRun(t, r, c.code, c.want)
			if !reflect.DeepEqual(r.calls, c.calls) {
				t.Errorf("iwdp-cli calls %q, want %q", r.calls, c.calls)
			}
		})
	}
	t.Run("no python3", func(t *testing.T) {
		g.noPython = true
		r := g.run(t, check)
		checkNoPython(t, r, 127, "python3 exit 127\n")
		if len(r.calls) != 0 {
			t.Errorf("iwdp-cli calls %q, want none", r.calls)
		}
	})
}

// pageCommands are the commands that act on, read or select a page: none
// may run before the target check's eval.
var pageCommands = map[string]bool{
	"open": true, "goto": true, "navigate": true, "back": true, "forward": true, "reload": true, "pushstate": true,
	"click": true, "dblclick": true, "tap": true, "fill": true, "type": true, "press": true, "wait": true,
	"dialog": true, "hover": true, "check": true, "uncheck": true, "select": true, "drag": true, "scroll": true,
	"swipe": true, "find": true, "frame": true, "snapshot": true, "get": true, "is": true, "hit-test": true,
	"read": true, "eval": true, "console": true, "errors": true, "network": true, "vitals": true,
	"screenshot": true, "set": true, "trace": true, "cookies": true, "storage": true, "state": true,
	"close": true,
}

// walkthrough is the script of the WebKitGTK doc test: every block that it
// runs, in file order, in one bash process (skillScript in
// e2e/webkitgtk/skills_doc_test.go).
func walkthrough(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, bl := range guideBlocks(t) {
		if !bl.skip {
			b.WriteString(bl.body + "\n")
		}
	}
	return b.String()
}

// The executable walkthrough runs the target check before its first page
// action: when the current tab shows another page, the whole flow stops
// at the check with "target false" and exit 1, after only the health
// probe, the reservation, the tab listing and the read-only eval; no
// selection, navigation, action or read runs. On the target it runs to
// the end, and the eval is the first page command (review WS6-A F20;
// criterion 9a).
func TestGuideWalkthroughStopsAtTheTargetCheck(t *testing.T) {
	g := guideShell{env: []string{"TARGET_FILE=" + targetFile(t), "QA_PASSWORD=PRIVATE-PASSWORD", "FAKE_WARNING=PRIVATE-WARNING", "FAKE_DIALOG=PRIVATE-DIALOG"}}
	flow := walkthrough(t)
	r := g.run(t, flow, "FAKE_PAGE_URL=https://private.example/PRIVATE-UNRELATED")
	if r.code != 1 || !strings.HasSuffix(r.stdout, "eval exit 0\ntarget false\n") || r.stderr != "" || strings.Contains(r.stdout, privateMark) {
		t.Errorf("unrelated tab: exit %d, stdout %q, stderr %q; want exit 1 after \"target false\"", r.code, r.stdout, r.stderr)
	}
	var cmds []string
	for _, c := range r.calls {
		cmds = append(cmds, command(c))
	}
	if want := []string{"session", "device", "tab", "eval"}; !slices.Equal(cmds, want) || !reflect.DeepEqual(r.calls[3], []string{"--page", "t1", "eval", evalTarget}) {
		t.Errorf("unrelated tab: iwdp-cli calls %q, want only %q ending with the read-only eval", r.calls, want)
	}
	ok := g.run(t, flow)
	if ok.code != 0 || ok.stderr != "" {
		t.Fatalf("target tab: exit %d, stdout %q, stderr %q; want the whole walkthrough", ok.code, ok.stdout, ok.stderr)
	}
	first := -1
	for i, c := range ok.calls {
		if pageCommands[command(c)] || (command(c) == "tab" && !slices.Contains(c, "list")) {
			first = i
			break
		}
	}
	if first < 0 || !reflect.DeepEqual(ok.calls[first], []string{"--page", "t1", "eval", evalTarget}) {
		t.Errorf("target tab: the first page command is %q, want the target check's eval; calls %q", ok.calls[max(first, 0)], ok.calls)
	}
	if last := command(ok.calls[len(ok.calls)-1]); last != "device" {
		t.Errorf("target tab: the last call is %q, want device release", last)
	}
}

// Page actions print the exact status and, for element and key actions,
// the lengths of the warnings and dialogs arrays of their JSON result,
// never their text. Commands whose result has no such arrays (wait,
// navigations, dialog, tab) print only their status. Page text that
// forges a dialog line or a warning line (an element id with newlines in
// an emulated action) counts as nothing; a filter failure is reported as
// the filter's (review WS6-A F6, F10, F16; criteria 10, 13).
func TestGuideActionsPrintOnlyCounts(t *testing.T) {
	g := guideShell{env: []string{"FAKE_WARNING=PRIVATE-WARNING", "FAKE_DIALOG=PRIVATE-DIALOG", "QA_PASSWORD=PRIVATE-PASSWORD"}}
	const ok = " exit 0 warnings 1 dialogs 1\n"
	dialogs := guideBlock(t, "iw dialog accept")
	checkRun(t, g.run(t, dialogs), 0, "dialog exit 0\nclick"+ok)
	for _, code := range []int{4, 124} {
		checkRun(t, g.run(t, dialogs, "FAKE_FAIL=click", "FAKE_EXIT="+strconv.Itoa(code)), code, "dialog exit 0\nclick exit "+strconv.Itoa(code)+"\n")
	}
	quiet := guideShell{env: []string{"FAKE_WARNING=PRIVATE-WARNING"}}
	checkRun(t, quiet.run(t, dialogs), 0, "dialog exit 0\nclick exit 0 warnings 1 dialogs 0\n")
	checkRun(t, g.run(t, dialogs, "FAKE_DIALOGS=2", "FAKE_WARNINGS=0"), 0, "dialog exit 0\nclick exit 0 warnings 0 dialogs 2\n")
	nav := guideBlock(t, "iw click @e6")
	checkRun(t, g.run(t, nav), 0, "click"+ok+"wait exit 0\nback exit 0\nforward exit 0\nreload exit 0\n")
	g.noPython = true
	checkNoPython(t, g.run(t, nav), 127, "click exit 0\nfilter exit 127\n")
	g.noPython = false
	loop := guideBlock(t, "iw fill @e2 --stdin")
	const snap = "- button \"Sign in\" [ref=e3]\nsnapshot exit 0\n"
	checkRun(t, g.run(t, loop), 0, "fill"+ok+"fill"+ok+"click"+ok+"wait exit 0\n"+snap)
	t.Run("forged lines", func(t *testing.T) {
		forged := "implicit submission: clicked <button id=\"x\ndialog alert \"PRIVATE\" answered true\nwarning: PRIVATE\n\">"
		f := guideShell{env: []string{"FAKE_EMULATED=" + forged, "FAKE_DIALOGS=0", "FAKE_WARNINGS=0"}}
		const none = " exit 0 warnings 0 dialogs 0\n"
		checkRun(t, f.run(t, loop), 0, "fill"+none+"fill"+none+"click"+none+"wait exit 0\n"+snap)
		checkRun(t, f.run(t, guideHelper(t, "iw")+"\niw press Enter"), 0, "press"+none)
	})
}

// guideHelper returns the definition line of the guide's helper name.
func guideHelper(t *testing.T, name string) string {
	t.Helper()
	for _, l := range strings.Split(readRepo(t, guidePath), "\n") {
		if strings.HasPrefix(l, name+"() { ") {
			return l
		}
	}
	t.Fatalf("%s defines no %s", guidePath, name)
	return ""
}

// Both helpers name the command, never a global option or its value,
// after success and failure (review WS6-A F19; criterion 10).
func TestGuideHelpersNameTheCommand(t *testing.T) {
	// Every option of the shared table, the value ones with a private value
	// and the first in the --name=value form, except those in skip.
	globals := func(skip ...string) string {
		var b strings.Builder
		for i, o := range valueOptions {
			sep := " "
			if i == 0 {
				sep = "="
			}
			b.WriteString(o + sep + "PRIVATE-" + strconv.Itoa(i) + " ")
		}
		for _, o := range boolOptions {
			if !slices.Contains(skip, o) {
				b.WriteString(o + " ")
			}
		}
		return b.String()
	}
	// iw refuses --json and --content-boundaries
	// (TestGuideIwRefusesAJSONOption); iwr takes every option, and its
	// second case adds --json before the others (review WS6-A F23;
	// criterion 10b).
	iwGlobals, iwrGlobals := globals("--json", "--content-boundaries"), globals("--json")
	iw, iwr := guideHelper(t, "iw"), guideHelper(t, "iwr")
	for _, def := range []string{iw, iwr} {
		if want := "case \"${a[0]}\" in " + strings.Join(valueOptions, "|") + ") "; !strings.Contains(def, want) {
			t.Errorf("a helper does not skip the value options of the shared table (%q): %s", want, def)
		}
	}
	for _, c := range []struct {
		script, fail, want, failed string
	}{
		{iw + "\niw " + iwGlobals + "press Enter", "press", "press exit 0 warnings 0 dialogs 0\n", "press exit 4\n"},
		{iw + "\niw " + iwGlobals + "open https://private.example/PRIVATE", "open", "open exit 0\n", "open exit 4\n"},
		{iwr + "\niwr " + iwrGlobals + "get text body", "get", "content: get\nget exit 0\n", "get exit 4\n"},
		{iwr + "\niwr --json " + iwrGlobals + "cookies", "cookies", "content: cookies\ncookies exit 0\n", "cookies exit 4\n"},
	} {
		checkRun(t, runGuideShell(t, c.script), 0, c.want)
		checkRun(t, runGuideShell(t, c.script, "FAKE_FAIL="+c.fail, "FAKE_EXIT=4"), 4, c.failed)
	}
}

// jsonForms are a caller's --json options in the forms of the real parser
// (splitGlobals, boolGlobal in internal/commands/cli.go): alone, with a
// strconv.ParseBool value, before or after the command.
var jsonForms = [][]string{
	{"--json", "press", "Enter"},
	{"--json=true", "press", "Enter"},
	{"--json=false", "press", "Enter"},
	{"--json=0", "press", "Enter"},
	{"--page", "t1", "--json=F", "press", "Enter"},
	{"press", "Enter", "--json"},
	{"press", "Enter", "--json=false"},
	{"press", "--json=False", "Enter"},
	// After "--", and where the CLI reads the word as an option's value
	// (review WS6-A round 7: the CLI takes "--" as the session value and
	// then applies --json=false).
	{"press", "--", "--json"},
	{"--session", "--", "press", "Enter", "--json=false"},
	{"--session", "--json=false", "press", "Enter"},
	{"--page", "--json", "press", "Enter"},
	// --content-boundaries changes the shape of the JSON (markJSON).
	{"--content-boundaries", "press", "Enter"},
	{"--content-boundaries=true", "press", "Enter"},
	{"press", "Enter", "--content-boundaries=0"},
	{"--session", "--content-boundaries", "press", "Enter"},
}

// refusal is iw's usage line for a word that sets the output format.
const refusal = "iw: usage: iw sets the output format itself; remove --json and --content-boundaries\n"

// iw refuses every word that sets the output format (--json and
// --content-boundaries, alone or with "="), at any position, also after
// "--" and where the CLI reads it as an option's value, before it calls
// iwdp-cli: such a word would run the action in text mode or change the
// shape of its JSON, after which the filter fails behind a completed
// action. The refusal names no value and exits 2 (review WS6-A F22;
// criterion 13b).
func TestGuideIwRefusesAJSONOption(t *testing.T) {
	iw := guideHelper(t, "iw")
	for _, form := range jsonForms {
		r := runGuideShell(t, iw+"\niw "+strings.Join(form, " "))
		checkRun(t, r, 2, refusal)
		if len(r.calls) != 0 {
			t.Errorf("iw %s: iwdp-cli calls %q, want none", strings.Join(form, " "), r.calls)
		}
	}
	// A line without such a word passes every word on unchanged, "--"
	// included.
	r := runGuideShell(t, iw+"\niw --session=x press -- Enter")
	checkRun(t, r, 0, "press exit 0 warnings 0 dialogs 0\n")
	if want := [][]string{{"--json", "--session=x", "press", "--", "Enter"}}; !reflect.DeepEqual(r.calls, want) {
		t.Errorf("iw --session=x press -- Enter: calls %q, want %q", r.calls, want)
	}
}

// helpForms are help switches (-h, --help; splitGlobals in
// internal/commands/cli.go) before, after and inside a command line, after
// "--" and where the CLI reads them as an option's value.
var helpForms = [][]string{
	{"-h", "press", "Enter"},
	{"--help", "press", "Enter"},
	{"press", "Enter", "-h"},
	{"--page", "t1", "--help", "get", "text", "body"},
	{"--session", "-h", "press", "Enter"},
	{"--page", "--help", "get", "text", "body"},
	{"get", "text", "--", "-h"},
	{"--", "--help"},
}

// Both helpers refuse every help switch at any position before they call
// iwdp-cli: the CLI would print help instead of running the command, and
// the helper would label it with the switch. The refusal names no value,
// points to iwdp-cli <command> --help and exits 2 (review WS6-A F24;
// criterion 10a).
func TestGuideHelpersRefuseHelpSwitches(t *testing.T) {
	for _, name := range []string{"iw", "iwr"} {
		def := guideHelper(t, name)
		for _, form := range helpForms {
			r := runGuideShell(t, def+"\n"+name+" "+strings.Join(form, " "))
			checkRun(t, r, 2, name+": usage: "+name+" runs no help; read it with iwdp-cli <command> --help\n")
			if len(r.calls) != 0 {
				t.Errorf("%s %s: iwdp-cli calls %q, want none", name, strings.Join(form, " "), r.calls)
			}
		}
	}
}

// The fake iwdp-cli reads --json as the real parser does: the last
// occurrence wins, with the values of strconv.ParseBool, up to "--"
// (internal/commands TestGuideHelperRefusesJSONOverrides runs the same
// forms through the real parser).
func TestGuideFakeTakesTheLastJSONValue(t *testing.T) {
	for _, c := range []struct {
		args string
		json bool
	}{
		{"--json press Enter", true},
		{"--json --json=false press Enter", false},
		{"--json press Enter --json=false", false},
		{"--json=false press Enter --json", true},
		{"--json=0 press Enter", false},
		{"--json=T press Enter", true},
		{"press Enter", false},
		{"--json press -- --json=false", true},
	} {
		r := runGuideShell(t, "iwdp-cli "+c.args)
		if r.code != 0 || strings.HasPrefix(r.stdout, "{") != c.json {
			t.Errorf("fake iwdp-cli %s: exit %d, stdout %q; want JSON %v", c.args, r.code, r.stdout, c.json)
		}
	}
}

// statusLine is a status line of the guide's blocks: "<command> exit <n>",
// with an optional second word ("tab list", "session info") and the
// counts of iw after it.
var statusLine = regexp.MustCompile(`^([a-z-]+)( [a-z]+)? exit (\d+)( warnings \d+ dialogs \d+)?$`)

// Every block that the WebKitGTK doc test runs reports one status line
// per iwdp-cli call, in order, and no private value. When any one call
// fails with an error that names the page, the block prints that call's
// status last, never the error, runs no later call, and ends with the
// call's status (review WS6-A F18; criteria 5a, 10, 15).
func TestGuideBlocksHideErrorsAndReportEveryStatus(t *testing.T) {
	g := guideShell{env: []string{"TARGET_FILE=" + targetFile(t), "QA_PASSWORD=PRIVATE-PASSWORD", "IWDP_SESSION=qa", "FAKE_WARNING=PRIVATE-WARNING", "FAKE_DIALOG=PRIVATE-DIALOG", "FAKE_STDERR=PRIVATE-NOTE"}}
	ran := 0
	for i, b := range guideBlocks(t) {
		if b.skip {
			continue
		}
		t.Run("block "+strconv.Itoa(i+1), func(t *testing.T) {
			t.Parallel()
			r := g.run(t, b.body)
			checkRun(t, r, 0, r.stdout)
			var cmds []string
			for _, l := range strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n") {
				if m := statusLine.FindStringSubmatch(l); m != nil && m[1] != "curl" && m[1] != "filter" && m[1] != "python3" {
					cmds = append(cmds, m[1])
				}
			}
			var want []string
			for _, c := range r.calls {
				want = append(want, command(c))
			}
			if !slices.Equal(cmds, want) {
				t.Errorf("status lines of %q, want one per iwdp-cli call %q:\n%s", cmds, want, r.stdout)
			}
			for k := range r.calls {
				f := g.run(t, b.body, "FAKE_FAIL_AT="+strconv.Itoa(k+1), "FAKE_EXIT=4")
				lines := strings.Split(strings.TrimSuffix(f.stdout, "\n"), "\n")
				last := statusLine.FindStringSubmatch(lines[len(lines)-1])
				if last == nil || last[1] != command(r.calls[k]) || last[3] != "4" || last[4] != "" {
					t.Errorf("call %d (%s) fails: last line %q, want its status, exit 4", k+1, command(r.calls[k]), lines[len(lines)-1])
				}
				if len(f.calls) != k+1 {
					t.Errorf("call %d (%s) fails: %d calls ran, want %d", k+1, command(r.calls[k]), len(f.calls), k+1)
				}
				checkRun(t, f, 4, f.stdout)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("no runnable bash block in the guide")
	}
}

// command returns the command word of an iwdp-cli call, after the global
// options and the values of those that take one.
func command(call []string) string { return commandWord(call) }

// Every status read in the guide is "cmd || s=$?" or "cmd || { s=$?; ...}",
// which errexit does not end; a status read after ";" never runs under
// set -e.
func TestGuideStatusLinesSurviveErrexit(t *testing.T) {
	for i, line := range strings.Split(readRepo(t, guidePath), "\n") {
		if strings.Count(line, "$?") != strings.Count(line, "|| s=$?")+strings.Count(line, "|| { s=$?;") {
			t.Errorf("%s:%d reads $? outside || s=$?: %s", guidePath, i+1, line)
		}
	}
}

// rawCommands are the discovery, reservation, diagnostic, selection and
// page-action commands whose own output the guide never prints (criterion
// 8); safeForms are the only lines that may run one through iwdp-cli
// directly. All others go through iw.
var (
	rawCommands = map[string]bool{
		"tab": true, "session": true, "devices": true, "doctor": true, "status": true, "device": true,
		"open": true, "goto": true, "navigate": true, "back": true, "forward": true, "reload": true, "pushstate": true,
		"click": true, "dblclick": true, "tap": true, "fill": true, "type": true, "press": true, "wait": true,
		"dialog": true, "hover": true, "check": true, "uncheck": true, "select": true, "drag": true,
		"scroll": true, "swipe": true, "find": true, "frame": true, "window": true,
	}
	safeForms = []string{
		"iwdp-cli session info --json 2>/dev/null)\" || { s=$?;",
		"iwdp-cli tab list --json 2>/dev/null)\" || { s=$?;",
		"iwdp-cli device reserve --owner \"$IWDP_SESSION\" --ttl 1h >/dev/null 2>&1 || s=$?;",
		"iwdp-cli device release --owner \"$IWDP_SESSION\" >/dev/null 2>&1 || s=$?;",
	}
	valueFlags = func() map[string]bool {
		m := map[string]bool{}
		for _, o := range valueOptions {
			m[o] = true
		}
		return m
	}()
)

// valueOptions and boolOptions are the global options of iwdp-cli
// (globalSpecs, internal/commands/cli.go): those that take a value, in the
// order of the helpers' case list, and the boolean ones. Every guard that
// finds a command name and the helpers' label test use this one table.
var (
	valueOptions = []string{"--page", "--session", "--timeout", "--transport", "--max-output", "--reservation", "--wait-lease"}
	boolOptions  = []string{"--json", "--content-boundaries", "--takeover"}
)

// commandWord returns the command name of the words after iwdp-cli, iw or
// iwr, as the helpers take it: after the leading global options, the
// values of those that take one ("--page t1"), the "--name=value" forms
// and the boolean options; "" when no word is left.
func commandWord(words []string) string {
	for len(words) > 0 && strings.HasPrefix(words[0], "--") {
		if valueFlags[words[0]] && len(words) > 1 {
			words = words[1:]
		}
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	return strings.Trim(words[0], "`'\"")
}

// The whole guide, prose and blocks, never tells to run one of rawCommands
// through iwdp-cli outside a safe form (review WS6-A F13).
func TestGuideRunsNoRawDiscoveryOrAction(t *testing.T) {
	for i, line := range strings.Split(readRepo(t, guidePath), "\n") {
		rest := line
		for {
			j := strings.Index(rest, "iwdp-cli ")
			if j < 0 {
				break
			}
			at := rest[j:]
			rest = rest[j+len("iwdp-cli "):]
			words := strings.Fields(rest)
			cmd := commandWord(words)
			if cmd == "" {
				continue
			}
			for len(words) > 0 && strings.HasPrefix(words[0], "--") {
				if valueFlags[words[0]] && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
			if cmd == "daemon" && len(words) > 1 && strings.Trim(words[1], "`") != "status" {
				continue
			}
			if !rawCommands[cmd] && cmd != "daemon" {
				continue
			}
			safe := false
			for _, f := range safeForms {
				safe = safe || strings.HasPrefix(at, f)
			}
			if !safe {
				t.Errorf("%s:%d runs iwdp-cli %s outside iw or a filtered form: %s", guidePath, i+1, cmd, line)
			}
		}
	}
}

// The non-sensitive exception covers get title, get url and open only: no
// sentence that names it invites dialog output, and no block or inline
// instruction runs a discovery, diagnostic, selection or action command
// through iwr, which prints the command's output (review WS6-A F21;
// criterion 8a).
func TestGuideOffersNoUnfilteredDialogOutput(t *testing.T) {
	text := readRepo(t, guidePath)
	for _, sentence := range regexp.MustCompile(`[.!?](\s|$)`).Split(text, -1) {
		if strings.Contains(sentence, "non-sensitive") && strings.Contains(sentence, "dialog") {
			t.Errorf("%s: a sentence on non-sensitive pages names dialogs: %q", guidePath, strings.TrimSpace(sentence))
		}
	}
	for i, line := range strings.Split(text, "\n") {
		for _, cmd := range iwrRawCommands(line) {
			t.Errorf("%s:%d: iwr runs %s, whose output the guide never prints: %s", guidePath, i+1, cmd, line)
		}
	}
}

// iwrCall matches a call of iwr as a shell command, in a block line or an
// inline code span.
var iwrCall = regexp.MustCompile(`(^|[;|&(]\s*|\$\(\s*|\x60)iwr `)

// iwrRawCommands returns the rawCommands that line runs through iwr, with
// the command name taken as the helpers take it (commandWord).
func iwrRawCommands(line string) []string {
	var out []string
	for _, loc := range iwrCall.FindAllStringIndex(line, -1) {
		rest := line[loc[1]:]
		if j := strings.IndexByte(rest, '`'); j >= 0 {
			rest = rest[:j] // the end of an inline code span
		}
		if cmd := commandWord(strings.Fields(rest)); rawCommands[cmd] {
			out = append(out, cmd)
		}
	}
	return out
}

// The guard of raw commands through iwr skips global options and their
// values as the helpers do; an option before the command never hides it
// (round 7b, item 11).
func TestGuideIwrGuardSkipsGlobalOptions(t *testing.T) {
	for _, line := range []string{
		"iwr --page t1 dialog status",
		"iwr --session=x dialog status",
		"iwr --timeout 5s --page t1 dialog status",
		"iwr --json --takeover dialog status",
		"`iwr --page \"$tab\" tab list`",
		"x; iwr --wait-lease 3s --reservation r open https://example.com",
	} {
		if got := iwrRawCommands(line); len(got) != 1 {
			t.Errorf("%q: raw commands %q, want one", line, got)
		}
	}
	for _, line := range []string{"iwr get text @e4", "iwr --page t1 get text @e4", "iwr --session=x cookies", "iwr --timeout 5s snapshot -i"} {
		if got := iwrRawCommands(line); len(got) != 0 {
			t.Errorf("%q: raw commands %q, want none", line, got)
		}
	}
	for _, c := range []struct {
		words []string
		want  string
	}{
		{[]string{"--page", "t1", "--session=x", "--timeout", "5s", "--json", "press", "Enter"}, "press"},
		{[]string{"--wait-lease=3s", "--max-output", "100", "--transport", "iwdp", "--content-boundaries", "tab", "list"}, "tab"},
		{[]string{"--page"}, ""},
	} {
		if got := commandWord(c.words); got != c.want {
			t.Errorf("commandWord(%q) = %q, want %q", c.words, got, c.want)
		}
	}
}

// helperDef matches the definition line of a shell helper of the guide.
var helperDef = regexp.MustCompile(`^(iw|iwr)\(\) \{ `)

// helperUse matches a call of a guide helper as a shell command.
func helperUse(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[;|&(]\s*|\$\(\s*)` + name + ` `)
}

// The bash blocks run no GNU timeout command (macOS has none; iwdp-cli has
// its own deadline). Every block that calls iw or iwr defines it before
// the first call, with the same definition in every block, because each
// shell call of an agent starts a new shell; an inline instruction
// cannot define one, so it never runs one (review WS6-A F15; criterion
// 12).
func TestGuideBashBlocksArePortableAndSelfContained(t *testing.T) {
	timeoutCmd := regexp.MustCompile(`(^|[;|&(]|\$\()\s*timeout\s`)
	defs := map[string]string{}
	for _, b := range guideBashBlocks(t) {
		lines := strings.Split(b, "\n")
		defined := map[string]bool{}
		for _, line := range lines {
			if timeoutCmd.MatchString(line) {
				t.Errorf("%s: a bash block runs timeout, which macOS lacks: %s", guidePath, line)
			}
			if m := helperDef.FindStringSubmatch(line); m != nil {
				defined[m[1]] = true
				if d, ok := defs[m[1]]; ok && d != line {
					t.Errorf("%s: two definitions of %s differ:\n%s\n%s", guidePath, m[1], d, line)
				}
				defs[m[1]] = line
				continue
			}
			for _, h := range []string{"iw", "iwr"} {
				if helperUse(h).MatchString(line) && !defined[h] {
					t.Errorf("%s: a bash block calls %s before it defines it: %s", guidePath, h, line)
				}
			}
		}
	}
	for _, h := range []string{"iw", "iwr"} {
		if defs[h] == "" {
			t.Errorf("%s defines no %s", guidePath, h)
		}
	}
	for _, s := range guideSpans(t) {
		for _, h := range []string{"iw", "iwr"} {
			if helperUse(h).MatchString(s.code) && !strings.Contains(s.code, h+"() { ") {
				t.Errorf("%s:%d: an inline instruction runs %s, which only a block defines: %s", guidePath, s.line, h, s.code)
			}
		}
	}
}

// iwdpCall matches a call of iwdp-cli as a shell command.
var iwdpCall = regexp.MustCompile(`(^|[;|&(]\s*)iwdp-cli\s`)

// statusRead is what follows "||" after a call that reports its status:
// the status of the call in s and an echo of it, or the same with an exit.
var statusRead = regexp.MustCompile(`^\|\| (s=\$\?; echo "[^"]* exit \$s"|\{ s=\$\?; echo "[^"]* exit \$s"; exit "\$s"; \})`)

// pageFreeMentions are the inline mentions of iwdp-cli commands that
// connect to no page, so that no error of theirs can name one: help and
// the guide itself, and the two commands of safety rule 7 that the guide
// names only to forbid or to assign to the device owner.
var pageFreeMentions = regexp.MustCompile(`^iwdp-cli (\S+ )*--help$|^iwdp-cli skills get [a-z]+$|^iwdp-cli restart-iwdp$|^iwdp-cli daemon stop$`)

// Every iwdp-cli call of the guide, in a block or inline, discards stderr
// (an error can name the page) and reports its exact status: "2>/dev/null"
// or ">/dev/null 2>&1" before the "||" that reads the status into s and
// echoes it. The helpers
// iw and iwr are such calls (review WS6-A F18; criteria 5a, 15).
func TestGuideIwdpCliCallsDiscardStderrAndReportStatus(t *testing.T) {
	check := func(where, code string) {
		for _, loc := range iwdpCall.FindAllStringIndex(code, -1) {
			rest := code[loc[1]:]
			k := strings.Index(rest, "||")
			discarded := strings.Contains(rest[:k+1], " 2>/dev/null") || strings.Contains(rest[:k+1], " >/dev/null 2>&1")
			if k < 0 || !discarded || !statusRead.MatchString(rest[k:]) {
				t.Errorf("%s: an iwdp-cli call without stderr discarded and its status reported: %s", where, code)
			}
		}
	}
	for i, b := range guideBashBlocks(t) {
		for _, line := range strings.Split(b, "\n") {
			check("bash block "+strconv.Itoa(i+1), line)
		}
	}
	for _, s := range guideSpans(t) {
		if !pageFreeMentions.MatchString(s.code) {
			check(guidePath+":"+strconv.Itoa(s.line), s.code)
		}
	}
}

// privateFields are the values that no block may print: every field of
// the filtered JSON that is not allowlisted (session info: the top-level
// session and each page's key, title, url, device, transport, session,
// since and lastUsed; tab list: each tab's label, key, title, url, device
// and leasedBy; the device listing of port 9221: deviceId, deviceName,
// deviceOSVersion and url of each device and of the SIMULATOR entry), an
// extra field that a newer iwdp-cli could add, the target URL, the CDP
// URL with its token, the text of
// warnings, dialog messages and emulated actions of page actions, and
// stderr. The other fields are the allowlisted facts that the blocks
// print: lease, epoch and connection state, tab ids, debuggable and
// current marks, the hidden count, the device count, and the numbers of
// warnings and dialogs of an action.
type privateFields struct {
	Session, PageKey, Title, URL, Device, Transport, PageSession, Since, LastUsed string
	Label, TabKey, LeasedBy                                                       string
	DeviceID, DeviceName, OSVersion, ListingURL, SimName, SimURL                  string
	ExtraKey, ExtraVal, Target, Warning, Dialog, Emulated, Stderr, CDP            string
	Lease, State, Hidden, Devices, Warnings, Dialogs                              uint8
	Epoch                                                                         uint16
	Debuggable                                                                    bool
}

// Property (criteria 11, 14 and 14a): with arbitrary private values in
// every non-allowlisted field and text, and a private CDP URL, every bash
// block of the guide prints exactly its allowed output: the allowlisted
// facts, the statuses, and the content of content reads (a fixed line of
// the fake). The table of expected outputs covers every block, whatever the
// skip rules of the WebKitGTK runner: the device check runs with a fake
// curl and the CDP block with a fake agent-browser. A new block fails the
// test until it is added.
func TestGuidePrivateValuesNeverReachTheOutput(t *testing.T) {
	// first returns the first line of a block that is not a helper
	// definition: the key of the block in the table below.
	first := func(body string) string {
		for _, l := range strings.Split(body, "\n") {
			if !helperDef.MatchString(l) {
				return l
			}
		}
		return ""
	}
	blocks := map[string]string{}
	// Every block, also those that the WebKitGTK runner skips: they run
	// with the fake clients (curl, agent-browser).
	for _, b := range guideBlocks(t) {
		blocks[first(b.body)] = b.body
	}
	rd := func(cmds ...string) string {
		var b strings.Builder
		for _, c := range cmds {
			b.WriteString("content: " + c + "\n" + c + " exit 0\n")
		}
		return b.String()
	}
	const snap = "- button \"Sign in\" [ref=e3]\nsnapshot exit 0\n"
	// clean keeps a value usable in an environment variable (no NUL) and
	// marks it private; the suffix tells two entries apart.
	clean := func(v, suffix string) string { return privateMark + strings.ReplaceAll(v, "\x00", "?") + suffix }
	leases, states := []string{"none", "active", "lost"}, []string{"attached", "connecting", "lost"}
	// keys are the first lines of the blocks that the property runs.
	keys := []string{"probe=", "export IWDP_SESSION=", "TARGET=", "iwr snapshot -i", "iw fill @e1", "iw wait @e5", "iwr get text @e4", "iw dialog accept", "iwr screenshot \"$EVIDENCE/after-sign-in.png\"", "iw click @e6", "iwr network har start", "iwr state save", "iwr close", "listing=", "CDP="}
	bodies := map[string]string{}
	for k, b := range blocks {
		n := 0
		for _, key := range keys {
			if strings.HasPrefix(k, key) {
				bodies[key] = b
				n++
			}
		}
		if n != 1 {
			t.Errorf("the property runs the block of %s that starts with %q %d times, want once", guidePath, k, n)
		}
	}
	// f runs the cases of the block key with the values p.
	f := func(t *testing.T, key string, p privateFields) bool {
		lease, state, epoch := leases[int(p.Lease)%len(leases)], states[int(p.State)%len(states)], int(p.Epoch)+1
		var pages, tabs, listing []any
		for i, n := range []string{"1", "2"} {
			pages = append(pages, map[string]any{"key": clean(p.PageKey, n), "title": clean(p.Title, n), "url": clean(p.URL, n), "device": clean(p.Device, n), "transport": clean(p.Transport, n), "state": state, "session": clean(p.PageSession, n), "lease": lease, "epoch": epoch, "since": clean(p.Since, n), "lastUsed": clean(p.LastUsed, n), clean(p.ExtraKey, n): clean(p.ExtraVal, n)})
			tabs = append(tabs, map[string]any{"id": "t" + n, "label": clean(p.Label, n), "key": clean(p.TabKey, n), "title": clean(p.Title, n), "url": clean(p.URL, n), "device": clean(p.Device, n), "debuggable": p.Debuggable == (i == 0), "current": i == 0, "leasedBy": clean(p.LeasedBy, n), clean(p.ExtraKey, n): clean(p.ExtraVal, n)})
		}
		nDevices := int(p.Devices)%3 + 1
		for i := range nDevices {
			n := strconv.Itoa(i)
			listing = append(listing, map[string]any{"deviceId": clean(p.DeviceID, n), "deviceName": clean(p.DeviceName, n), "deviceOSVersion": clean(p.OSVersion, n), "url": clean(p.ListingURL, n), clean(p.ExtraKey, n): clean(p.ExtraVal, n)})
		}
		listing = append(listing, map[string]any{"deviceId": "SIMULATOR", "deviceName": clean(p.SimName, ""), "url": clean(p.SimURL, "")})
		hidden := int(p.Hidden) % 3
		sess, _ := json.Marshal(map[string]any{"session": clean(p.Session, ""), "pages": pages})
		tabList, _ := json.Marshal(map[string]any{"tabs": tabs, "hidden": hidden})
		devList, _ := json.Marshal(listing)
		target := "https://private.example/" + url.PathEscape(clean(p.Target, ""))
		nWarn, nDlg := int(p.Warnings)%3, int(p.Dialogs)%3
		// The emulated action also carries forged dialog and warning lines.
		emulated := clean(p.Emulated, "\ndialog alert \"PRIVATE\" answered true\nwarning: PRIVATE")
		g := guideShell{env: []string{
			"FAKE_SESSION_JSON=" + string(sess), "FAKE_TABS_JSON=" + string(tabList), "FAKE_LISTING_JSON=" + string(devList),
			"TARGET_FILE=" + writeTarget(t, target), "FAKE_PAGE_URL=" + target,
			"FAKE_WARNING=" + clean(p.Warning, ""), "FAKE_DIALOG=" + clean(p.Dialog, ""), "FAKE_EMULATED=" + emulated,
			"FAKE_WARNINGS=" + strconv.Itoa(nWarn), "FAKE_DIALOGS=" + strconv.Itoa(nDlg),
			"FAKE_STDERR=" + clean(p.Stderr, ""), "QA_PASSWORD=" + clean(p.Dialog, "-password"),
			"FAKE_CDP_URL=ws://127.0.0.1:9230/devtools/browser/" + url.PathEscape(clean(p.CDP, "")),
		}}
		pageLine := " lease " + lease + " epoch " + strconv.Itoa(epoch) + " connection " + state + "\n"
		mark := map[bool]string{true: "debuggable", false: "not-debuggable"}
		tabLines := "* t1 " + mark[p.Debuggable] + "\n  t2 " + mark[!p.Debuggable] + "\nhidden " + strconv.Itoa(hidden) + "\n"
		if hidden > 0 {
			tabLines += hiddenHint + "\n"
		}
		ok := " exit 0 warnings " + strconv.Itoa(nWarn) + " dialogs " + strconv.Itoa(nDlg) + "\n"
		for _, c := range []struct {
			key, want string
			code      int
			env       []string
		}{
			{"probe=", "page 1" + pageLine + "page 2" + pageLine + "session info exit 0\n", 0, nil},
			{"export IWDP_SESSION=", "device reserve exit 0\n" + tabLines + "tab list exit 0\n", 0, nil},
			{"TARGET=", "eval exit 0\ntarget true\ntab exit 0\n", 0, nil},
			{"TARGET=", "eval exit 0\ntarget false\n", 1, []string{"FAKE_PAGE_URL=" + target + "-other"}},
			{"iwr snapshot -i", snap, 0, nil},
			{"iw fill @e1", "fill" + ok + "fill" + ok + "click" + ok + "wait exit 0\n" + snap, 0, nil},
			{"iw wait @e5", strings.Repeat("wait exit 0\n", 4), 0, nil},
			{"iwr get text @e4", rd("get", "get", "is", "get", "get", "hit-test", "read", "eval"), 0, nil},
			{"iw dialog accept", "dialog exit 0\nclick" + ok, 0, nil},
			{"iwr screenshot \"$EVIDENCE/after-sign-in.png\"", rd("screenshot", "screenshot", "screenshot", "console", "errors", "network", "vitals"), 0, nil},
			{"iw click @e6", "click" + ok + "wait exit 0\nback exit 0\nforward exit 0\nreload exit 0\n", 0, nil},
			{"iwr network har start", rd("network") + "reload exit 0\n" + rd("network", "network", "network", "set", "set", "trace", "trace"), 0, nil},
			{"iwr state save", rd("state", "cookies", "cookies", "storage", "state"), 0, nil},
			{"iwr close", rd("close") + "device release exit 0\n", 0, nil},
			{"listing=", "devices " + strconv.Itoa(nDevices) + "\ncurl exit 0\n", 0, nil},
			{"CDP=", "get cdp-url exit 0\n- button \"Sign in\" [ref=e1]\n", 0, nil},
		} {
			if c.key != key {
				continue
			}
			r := g.run(t, bodies[key], c.env...)
			if r.code != c.code || r.stdout != c.want || r.stderr != "" {
				t.Logf("block %q: exit %d, stdout %q, stderr %q; want exit %d, %q", c.key, r.code, r.stdout, r.stderr, c.code, c.want)
				return false
			}
		}
		return true
	}
	for _, key := range keys {
		if bodies[key] == "" {
			t.Errorf("no block of %s starts with %q", guidePath, key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			if err := quick.Check(func(p privateFields) bool { return f(t, key, p) }, &quick.Config{MaxCount: 20}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
