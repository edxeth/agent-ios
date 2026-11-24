//go:build webkitgtk

package webkitgtk

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/webkitgtk"
)

// Design 7.9 acceptance 2: the command examples of the skills run against
// WebKitGTK. Every ```bash block of the guides skill-data/*/SKILL.md (what
// "iwdp-cli skills get" prints) is an example; the stubs in skills/ have
// no bash block (internal/skills TestSkillStubsPointAtTheirGuides). The
// blocks of one skill run in file order in one bash process (so an export
// in one block holds for the next), each block's output in its own files,
// under "set -eo pipefail": the first failing command fails the test and
// names its block.
//
// Markers are HTML comments on the line before a block:
//   - "<!-- doc-test: skip <rule>: <reason> -->" before a bash block: the
//     block is not run, by a rule of skipRules, and the test logs it;
//   - "<!-- doc-test: expect -->" before a ```text block: each of its
//     lines must appear in the output of the nearest bash block above it.
//     A status line of private mode (statusLineRE) must appear in its
//     stderr, every other line, a result, in its stdout: a shell consumer
//     reads results from stdout only, so neither stream stands in for the
//     other.
//
// The examples use the origin https://example.com; the test serves it
// from e2e/fixtures/skills. The environment gives EVIDENCE (a temporary
// folder), QA_PASSWORD (a stand-in) and TARGET_FILE (a file that holds
// the URL of /login). MiniBrowser starts on /login, and each guide starts
// there, so the device guide's target check, which comes before its first
// page action, reads that URL and finds the target; it never opens or
// navigates the tab.
var skipRules = map[string]string{
	"device":    "needs a USB-connected iPhone or iPad and ios_webkit_debug_proxy",
	"simulator": "needs a Mac over SSH with an iOS Simulator and its configuration (docs/simulator.md)",
	"w5b":       "runs agent-browser, which this test does not install; e2e/cdpcompat TestClientScenarios runs the agent-browser --cdp form",
}

const exampleOrigin = "https://example.com"

// statusLineRE matches a status line of private mode (privateResult and
// privateErrorLine in internal/commands/private.go): "<command> exit
// <code>", where <command> is a command of the registry or iwdp-cli for a
// line that names none, then the end of the line, or a space and the
// counts of an action, or ": " and the meaning of the code.
func statusLineRE() *regexp.Regexp {
	names := []string{"iwdp-cli"}
	for _, c := range commands.All() {
		names = append(names, regexp.QuoteMeta(c.Name))
	}
	return regexp.MustCompile(`^(` + strings.Join(names, "|") + `) exit [0-9]+($|[ :])`)
}

type docBlock struct {
	line   int    // line of the opening fence
	lang   string // info string of the fence
	body   string
	skip   string // rule of a skip marker
	why    string // reason of a skip marker
	expect bool   // text block with an expect marker
}

var markerRE = regexp.MustCompile(`^<!-- doc-test: (skip ([a-z0-9]+): (.*)|expect) -->$`)

// docBlocks returns the fenced blocks of a skill file with their markers.
func docBlocks(t *testing.T, path string) []docBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var out []docBlock
	var pending *docBlock
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if m := markerRE.FindStringSubmatch(l); m != nil {
			pending = &docBlock{line: i + 2, expect: m[1] == "expect", skip: m[2], why: strings.TrimSpace(m[3])}
			continue
		} else if strings.HasPrefix(l, "<!-- doc-test") {
			t.Fatalf("%s:%d: malformed doc-test marker %q", path, i+1, l)
		}
		if !strings.HasPrefix(l, "```") {
			if pending != nil && strings.TrimSpace(l) != "" {
				t.Fatalf("%s:%d: a doc-test marker must stand on the line before a code block", path, pending.line-1)
			}
			continue
		}
		b := docBlock{line: i + 1, lang: strings.TrimPrefix(l, "```")}
		if pending != nil {
			b.skip, b.why, b.expect = pending.skip, pending.why, pending.expect
			pending = nil
		}
		var body []string
		for i++; i < len(lines) && lines[i] != "```"; i++ {
			body = append(body, lines[i])
		}
		if i == len(lines) {
			t.Fatalf("%s:%d: code block without a closing fence", path, b.line)
		}
		b.body = strings.Join(body, "\n")
		switch {
		case b.skip != "" && b.lang != "bash":
			t.Fatalf("%s:%d: a skip marker stands before a %q block; it applies to bash blocks only", path, b.line, b.lang)
		case b.skip != "" && (skipRules[b.skip] == "" || b.why == ""):
			t.Fatalf("%s:%d: skip marker with rule %q and reason %q; want a rule of %v and a reason", path, b.line, b.skip, b.why, skipRules)
		case b.expect && b.lang != "text":
			t.Fatalf("%s:%d: an expect marker stands before a %q block; it applies to text blocks only", path, b.line, b.lang)
		}
		out = append(out, b)
	}
	return out
}

// skillsRoot is the folder of the skill guides, skill-data/.
func skillsRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "skill-data")
}

// skillsBrowser serves the skill fixtures as the example origin and starts
// MiniBrowser on /login, the target of the guide's target check. It returns the inspector address and the
// fixture origin.
func skillsBrowser(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(fixtureDir(), "..", "skills")
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "login.html")) })
	mux.HandleFunc("/second", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "second.html")) })
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"qa"}`))
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	origin := "http://" + l.Addr().String()
	shim, err := buildDPIShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	display := broadway(t)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "LD_PRELOAD=" + shim},
		miniBrowser, origin+"/login")
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(browserStart); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 && pages[0].Title != "" {
			return addr, origin
		}
	}
	t.Fatalf("MiniBrowser did not list the login page at %s within %v", addr, browserStart)
	return "", ""
}

// skillScript builds the bash script of the runnable blocks; block i
// writes its stdout and stderr to out/<i>.out and out/<i>.err.
func skillScript(blocks []docBlock, out, origin string) string {
	var b strings.Builder
	b.WriteString("set -eo pipefail\n")
	for i, bl := range blocks {
		if bl.lang != "bash" || bl.skip != "" {
			continue
		}
		fmt.Fprintf(&b, "exec >%q 2>%q\n", filepath.Join(out, fmt.Sprintf("%d.out", i)), filepath.Join(out, fmt.Sprintf("%d.err", i)))
		b.WriteString(strings.ReplaceAll(bl.body, exampleOrigin, origin))
		b.WriteString("\n")
	}
	return b.String()
}

func TestSkillExamplesRunOnWebKitGTK(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(skillsRoot(), "*", "SKILL.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no skill-data/*/SKILL.md: %v", err)
	}
	addr, origin := skillsBrowser(t)
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	for _, p := range paths {
		name := filepath.Base(filepath.Dir(p))
		t.Run(name, func(t *testing.T) {
			// Each guide starts on /login, the target of the device guide's
			// target check, as an agent that loads one guide finds the page:
			// the core guide's navigation example leaves it on /second.
			for _, args := range [][]string{{"open", origin + "/login"}, {"close"}} {
				if r := cli(t, append([]string{"--session", "qa-login"}, args...)...); r.code != 0 {
					t.Fatalf("%s: iwdp-cli %v before the examples: exit %d, stderr %q", name, args, r.code, r.stderr)
				}
			}
			blocks := docBlocks(t, p)
			runnable := 0
			for _, b := range blocks {
				switch {
				case b.lang == "bash" && b.skip != "":
					t.Logf("%s:%d: not run by rule %s (%s): %s", name, b.line, b.skip, skipRules[b.skip], b.why)
				case b.lang == "bash":
					runnable++
				}
			}
			if runnable == 0 {
				t.Fatalf("%s has no runnable bash example", name)
			}
			work := t.TempDir()
			out := filepath.Join(work, "out")
			evidence := filepath.Join(work, "evidence")
			target := filepath.Join(work, "target-url")
			if err := os.Mkdir(out, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(origin+"/login"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(work, "examples.sh")
			if err := os.WriteFile(script, []byte(skillScript(blocks, out, origin)), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script)
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(cliBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
				"EVIDENCE="+evidence, "QA_PASSWORD=stand-in-password", "TARGET_FILE="+target, "TMPDIR="+work, "TAB=t1")
			runErr := cmd.Run()
			// The last block with an output file is the one that ran last.
			last := -1
			for i := range blocks {
				if _, err := os.Stat(filepath.Join(out, fmt.Sprintf("%d.out", i))); err == nil {
					last = i
				}
			}
			read := func(i int, ext string) string {
				data, _ := os.ReadFile(filepath.Join(out, fmt.Sprintf("%d.%s", i, ext)))
				return string(data)
			}
			if runErr != nil {
				if last < 0 {
					t.Fatalf("%s: the example script failed before its first block: %v", name, runErr)
				}
				t.Fatalf("%s: the example block at line %d failed (%v):\n%s\nstdout:\n%s\nstderr:\n%s", name, blocks[last].line, runErr, blocks[last].body, read(last, "out"), read(last, "err"))
			}
			status := statusLineRE()
			ran := 0
			lastBash := -1
			for i, b := range blocks {
				if b.lang == "bash" && b.skip == "" {
					if _, err := os.Stat(filepath.Join(out, fmt.Sprintf("%d.out", i))); err != nil {
						t.Errorf("%s:%d: the block did not run", name, b.line)
					}
					ran++
					lastBash = i
				}
				if !b.expect {
					continue
				}
				if lastBash < 0 {
					t.Fatalf("%s:%d: an expect block without a runnable bash block above it", name, b.line)
				}
				for _, want := range strings.Split(b.body, "\n") {
					if strings.TrimSpace(want) == "" {
						continue
					}
					stream, ext := "stdout", "out"
					if status.MatchString(want) {
						stream, ext = "stderr", "err"
					}
					if got := read(lastBash, ext); !strings.Contains(got, want) {
						t.Errorf("%s:%d: the %s of the block at line %d lacks %q:\n%s", name, b.line, stream, blocks[lastBash].line, want, got)
					}
				}
			}
			t.Logf("%s: %d example blocks ran", name, ran)
		})
	}
}
