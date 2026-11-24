package skills_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	_ "github.com/nnemirovsky/iwdp-cli/internal/daemon" // registers the daemon commands, which a guide names
	"github.com/nnemirovsky/iwdp-cli/internal/skills"
)

// The guides skill-data/*/SKILL.md are what "iwdp-cli skills get" prints.
// Private output is a mode of the CLI (internal/commands/private.go, with
// its own tests), so a guide holds plain command lines. These tests check
// what a guide can still get wrong: a command line that the parser
// refuses, a block that runs outside private mode or in the default
// session, and a core guide that grows back into one long file.
//
// The WebKitGTK doc test (e2e/webkitgtk/skills_doc_test.go) runs the same
// blocks against a real page.

// maxCoreBytes bounds the core guide. Its model, the core guide of
// agent-browser 0.41, has 15 KB; the guide of this repository had 42 KB
// before it was split, most of it shell that the CLI now does itself.
// What only some tasks need goes into a guide of its own, which the core
// guide names.
const maxCoreBytes = 20 << 10

// startLine is the line that every example block starts with: a shell
// call that lacks it runs in the default session and prints titles and
// URLs.
const startLine = "export IWDP_SESSION=qa-login IWDP_PRIVATE=1"

// stubCLI stands in for iwdp-cli: it records the two variables and its
// arguments (fields end with 0x1f, a call with 0x1e) and prints what the
// next line of a block reads.
const stubCLI = `#!/bin/sh
printf '%s\037' "$IWDP_SESSION" "$IWDP_PRIVATE" "$@" >>"$STUB_LOG"
printf '\036' >>"$STUB_LOG"
case "$*" in
*"tab find"*) echo t1 ;;
*"get cdp-url"*) echo ws://127.0.0.1:9230/devtools/browser/stand-in ;;
esac
`

func guides(t *testing.T) []skills.Skill {
	t.Helper()
	all, err := skills.All()
	if err != nil {
		t.Fatal(err)
	}
	return all
}

var bashBlockRE = regexp.MustCompile("(?ms)^```bash\n(.*?)^```$")

// Every command line of every example block is one that the parser takes,
// and it runs in private mode in a session of its own.
func TestEveryGuideCommandParsesAndRunsInPrivateMode(t *testing.T) {
	for _, s := range guides(t) {
		t.Run(s.Name, func(t *testing.T) {
			blocks := bashBlockRE.FindAllStringSubmatch(s.Content, -1)
			if len(blocks) == 0 {
				t.Fatal("the guide has no example block")
			}
			dir := t.TempDir()
			for name, script := range map[string]string{"iwdp-cli": stubCLI, "agent-browser": "#!/bin/sh\n", "curl": "#!/bin/sh\necho '[]'\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(dir, "target-url")
			if err := os.WriteFile(target, []byte("https://example.com/login\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for i, b := range blocks {
				body := b[1]
				if !strings.HasPrefix(body, startLine) {
					t.Errorf("block %d does not start with %q:\n%s", i+1, startLine, body)
				}
				log := filepath.Join(dir, "calls.log")
				_ = os.Remove(log)
				// Each block runs as its own shell call, as a harness
				// runs it.
				cmd := exec.Command("bash", "-eo", "pipefail", "-c", body)
				cmd.Dir = dir
				cmd.Env = []string{
					"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "STUB_LOG=" + log,
					"EVIDENCE=" + filepath.Join(dir, "evidence"), "TARGET_FILE=" + target, "QA_PASSWORD=stand-in",
					"TAB=t1", "PORT=41873", // what sim up printed, and the agent's server
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("block %d failed against the stand-in: %v\n%s\n%s", i+1, err, body, out)
					continue
				}
				data, _ := os.ReadFile(log)
				calls := strings.Split(strings.TrimSuffix(string(data), "\x1e"), "\x1e")
				if len(data) == 0 {
					t.Errorf("block %d runs no iwdp-cli command:\n%s", i+1, body)
					continue
				}
				for _, call := range calls {
					f := strings.Split(strings.TrimSuffix(call, "\x1f"), "\x1f")
					session, private, args := f[0], f[1], f[2:]
					inv, err := commands.Parse(args, func(name string) string {
						return map[string]string{"IWDP_SESSION": session, "IWDP_PRIVATE": private}[name]
					})
					switch {
					case err != nil:
						t.Errorf("block %d: iwdp-cli %s: %v", i+1, strings.Join(args, " "), err)
					case inv.Cmd == nil || inv.Help:
						t.Errorf("block %d: iwdp-cli %s runs no command", i+1, strings.Join(args, " "))
					case !inv.Input.Global.Private || inv.Input.Global.Session == "default":
						t.Errorf("block %d: iwdp-cli %s runs with private %v in session %q", i+1, strings.Join(args, " "), inv.Input.Global.Private, inv.Input.Global.Session)
					}
				}
			}
		})
	}
}

var inlineCommandRE = regexp.MustCompile("`(iwdp-cli [^`\n]*)`")

// A command that the text names is a command of this binary too: an
// inline span that starts with iwdp-cli and holds no placeholder parses.
func TestEveryCommandThatAGuideNamesParses(t *testing.T) {
	none := func(string) string { return "" }
	for _, s := range guides(t) {
		named := 0
		for _, m := range inlineCommandRE.FindAllStringSubmatch(s.Content, -1) {
			if strings.ContainsAny(m[1], "<>") {
				continue // a form with a placeholder, such as iwdp-cli <command> --help
			}
			named++
			args := strings.Fields(strings.NewReplacer("\"", "", "'", "").Replace(m[1]))[1:]
			if inv, err := commands.Parse(args, none); err != nil || (inv.Cmd == nil && inv.Group == "") {
				t.Errorf("skill %s names `%s`: %v", s.Name, m[1], err)
			}
		}
		if named == 0 {
			t.Errorf("skill %s names no iwdp-cli command in its text", s.Name)
		}
	}
}

// The core guide stays short, names every other guide with the command
// that prints it, and every other guide points back at the rules of the
// core guide.
func TestGuidesAreLayered(t *testing.T) {
	var core string
	all := guides(t)
	for _, s := range all {
		if s.Name == "iwdp" {
			core = s.Content
		}
	}
	if core == "" {
		t.Fatal("no core guide iwdp")
	}
	if len(core) > maxCoreBytes {
		t.Errorf("the core guide has %d bytes, over %d: move what only some tasks need into a guide of its own", len(core), maxCoreBytes)
	}
	for _, s := range all {
		if s.Name == "iwdp" {
			continue
		}
		if !strings.Contains(core, "`iwdp-cli skills get "+s.Name+"`") {
			t.Errorf("the core guide does not name `iwdp-cli skills get %s`", s.Name)
		}
		if !strings.HasPrefix(s.Name, "iwdp-") {
			t.Errorf("guide %s: a guide beside the core guide is named iwdp-<topic>", s.Name)
		}
		if !strings.Contains(s.Content, "The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide.") {
			t.Errorf("guide %s does not point at the safety rules and private mode of the core guide", s.Name)
		}
	}
}

// standIns stands in for every command that a block runs, on a failure
// path. bash reads it from BASH_ENV before the block, so the block runs
// as it is written. Each stand-in is a function: it records its call as
// one line "<name> <arguments>" in $STUB_LOG and counts it; it returns
// $FAIL_CODE for call number $FAIL_AT, and for the iwdp-cli call whose
// arguments start with $FAIL. Otherwise iwdp-cli and agent-browser print
// or write what the next line of a block needs, and mkdir, rm and [ run
// the real command. The function does not use [ itself: it would call
// its own stand-in.
const standIns = `call() {
	local name=$1 n
	shift
	printf '%s\n' "$name $*" >>"$STUB_LOG"
	read -r n <"$COUNT"
	n=$((n + 1))
	echo "$n" >"$COUNT"
	if ((n == FAIL_AT)); then
		return "$FAIL_CODE"
	fi
	if [[ $name == iwdp-cli && -n $FAIL && "$*" == "$FAIL"* ]]; then
		return "$FAIL_CODE"
	fi
	case $name in
	iwdp-cli)
		case "$*" in
		"tab find"*) echo t1 ;;
		"get cdp-url"*) echo ws://127.0.0.1:9230/devtools/browser/stand-in ;;
		"state save "*) : >"$3" ;;
		esac
		;;
	agent-browser) ;;
	*) command "$name" "$@" ;;
	esac
}
iwdp-cli() { call iwdp-cli "$@"; }
agent-browser() { call agent-browser "$@"; }
mkdir() { call mkdir "$@"; }
rm() { call rm "$@"; }
[() { call [ "$@"; }
`

// standInNames are the commands that standIns stands in for. A block runs
// with a PATH that holds only mkdir and rm, so a command that a block
// adds without a stand-in fails the block instead of running.
var standInNames = map[string]bool{"iwdp-cli": true, "agent-browser": true, "mkdir": true, "rm": true, "[": true}

// guideBlock returns the one bash block of a guide that holds line.
func guideBlock(t *testing.T, guide, line string) string {
	t.Helper()
	for _, s := range guides(t) {
		if s.Name != guide {
			continue
		}
		var found []string
		for _, b := range bashBlockRE.FindAllStringSubmatch(s.Content, -1) {
			if strings.Contains(b[1], line) {
				found = append(found, b[1])
			}
		}
		if len(found) != 1 {
			t.Fatalf("guide %s has %d bash blocks with %q, want 1", guide, len(found), line)
		}
		return found[0]
	}
	t.Fatalf("no guide %s", guide)
	return ""
}

// blockRun is what a block did against standIns.
type blockRun struct {
	calls    []string // the arguments of each iwdp-cli call, and each agent-browser call with its name, joined by spaces
	names    []string // the name of every command that ran, in order
	exit     int      // the exit status of the block
	output   string   // stdout and stderr of the block
	evidence string   // the evidence folder
}

// runFailing runs a block as an agent runs it: in a new plain bash, with
// no -e, against standIns, which fail the iwdp-cli call that starts with
// fail with exit code (no call fails when fail is empty).
func runFailing(t *testing.T, block, fail string, code int) blockRun {
	t.Helper()
	return runBlock(t, block, "FAIL="+fail, "FAIL_AT=0", "FAIL_CODE="+strconv.Itoa(code))
}

// runFailingAt runs a block as runFailing does, but the command that
// fails is the one that runs as number at (from 1) of the block.
func runFailingAt(t *testing.T, block string, at, code int) blockRun {
	t.Helper()
	return runBlock(t, block, "FAIL=", "FAIL_AT="+strconv.Itoa(at), "FAIL_CODE="+strconv.Itoa(code))
}

// runFailingAtWithE is runFailingAt in a bash with -e, as a harness or a
// script with set -e runs a block.
func runFailingAtWithE(t *testing.T, block string, at, code int) blockRun {
	t.Helper()
	return runBlockIn(t, []string{"-e"}, block, "FAIL=", "FAIL_AT="+strconv.Itoa(at), "FAIL_CODE="+strconv.Itoa(code))
}

func runBlock(t *testing.T, block string, failure ...string) blockRun {
	t.Helper()
	return runBlockIn(t, nil, block, failure...)
}

// runBlockIn runs a block in bash with the options opts.
func runBlockIn(t *testing.T, opts []string, block string, failure ...string) blockRun {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mkdir", "rm"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	env := filepath.Join(dir, "stand-ins.sh")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(env, []byte(standIns), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(count, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence")
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	cmd := exec.Command("bash", append(append([]string(nil), opts...), "-c", block)...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + bin, "BASH_ENV=" + env, "COUNT=" + count, "HOME=" + dir, "STUB_LOG=" + log,
		"EVIDENCE=" + evidence, "TARGET_FILE=" + filepath.Join(dir, "target-url"), "QA_PASSWORD=stand-in",
		"TAB=t1", "PORT=41873", // what sim up printed, and the agent's server
	}, failure...)
	out, err := cmd.CombinedOutput()
	r := blockRun{output: string(out), evidence: evidence}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		r.exit = exit.ExitCode()
	case err != nil:
		t.Fatalf("bash did not run the block: %v", err)
	}
	data, _ := os.ReadFile(log)
	if s := strings.TrimSuffix(string(data), "\n"); s != "" {
		for _, call := range strings.Split(s, "\n") {
			name, args, _ := strings.Cut(call, " ")
			r.names = append(r.names, name)
			switch name {
			case "iwdp-cli":
				r.calls = append(r.calls, args)
			case "agent-browser":
				r.calls = append(r.calls, call)
			}
		}
	}
	return r
}

// A block stops before a command whose effect depends on a command that
// failed, and ends with the exit code of the failure: the tab of the
// task is selected only after tab find found it (no "tab" with an empty
// id, which lists the tabs and exits 0), a click does not submit a form
// that a failed fill left incomplete, no click runs without its armed
// dialog answer, and no navigation leaves the page after a failed link.
// The cookie block changes nothing when state save fails, and keeps the
// state file when state load fails. A cleanup step runs after a failed
// step, and the block still fails: the restore after a failed cookie
// change, the device release after a failed close, the simulator's
// release after a failed tab close (goesOn). The cookie
// block reads before it saves, so a failed read changes nothing and
// leaves no state file. Each block runs in a new plain bash, without
// -e, as a harness runs it; zsh, which agents also use, runs these
// command lists the same way, and the hosts of the suite need not have
// it (review F24, F25).
func TestGuideBlocksStopAtAFailedStep(t *testing.T) {
	const save, load = "state save", "state load"
	for _, c := range []struct {
		name, guide, line string // the guide and a line that only the block holds
		fail              string // the start of the arguments of the call that fails
		code              int
		calls             []string // the calls that run, in order, each by the start of its arguments
		exit              int
		stateFile         bool // the state file of the cookie block is there after the block
	}{
		{"tab find: no tab shows the target", "iwdp-device", "tab find", "tab find", 7, []string{"tab find"}, 7, false},
		{"tab find: a tab could not be read", "iwdp-device", "tab find", "tab find", 4, []string{"tab find"}, 4, false},
		{"tab: the selection fails", "iwdp-device", "tab find", "tab t1", 4, []string{"tab find", "tab t1"}, 4, false},
		{"tab find: found", "iwdp-device", "tab find", "", 0, []string{"tab find", "tab t1"}, 0, false},
		{"device reserve fails", "iwdp-device", "device reserve", "device reserve", 6, []string{"device reserve"}, 6, false},
		{"fill fails", "iwdp", "fill @e1", "fill @e1", 1, []string{"fill @e1"}, 1, false},
		{"password fill fails", "iwdp", "fill @e1", "fill @e2", 1, []string{"fill @e1", "fill @e2"}, 1, false},
		{"sign-in click fails", "iwdp", "fill @e1", "click @e3", 1, []string{"fill @e1", "fill @e2", "click @e3"}, 1, false},
		{"sign-in wait fails", "iwdp", "fill @e1", "wait --text", 3, []string{"fill @e1", "fill @e2", "click @e3", "wait --text"}, 3, false},
		{"dialog answer fails", "iwdp", "dialog accept", "dialog accept", 4, []string{"dialog accept"}, 4, false},
		{"link click fails", "iwdp", "click @e6", "click @e6", 1, []string{"click @e6"}, 1, false},
		{"url wait fails", "iwdp", "click @e6", "wait --url", 3, []string{"click @e6", "wait --url"}, 3, false},
		{"back fails", "iwdp", "click @e6", "back", 1, []string{"click @e6", "wait --url", "back"}, 1, false},
		{"cookie read fails", "iwdp-devtools", save, "cookies", 4, []string{"cookies"}, 4, false},
		{"state save fails", "iwdp-devtools", save, save, 4, []string{"cookies", "storage local", save}, 4, false},
		{"cookie change fails", "iwdp-devtools", save, "cookies set", 4, []string{"cookies", "storage local", save, "cookies set", load}, 1, false},
		{"state load fails", "iwdp-devtools", save, load, 4, []string{"cookies", "storage local", save, "cookies set", load}, 4, true},
		{"cookie block succeeds", "iwdp-devtools", save, "", 0, []string{"cookies", "storage local", save, "cookies set", load}, 0, false},
		{"close fails", "iwdp-device", "device release", "close", 4, []string{"close", "device release"}, 1, false},
		{"sim reserve fails", "iwdp", "sim reserve", "sim reserve", 3, []string{"sim reserve"}, 3, false},
		{"tab close fails", "iwdp", "sim release", "tab close", 1, []string{"tab close", "sim release"}, 1, false},
		{"cdp url fails before a CDP client", "iwdp-cdp", "agent-browser", "get cdp-url", 5, []string{"get cdp-url"}, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := runFailing(t, guideBlock(t, c.guide, c.line), c.fail, c.code)
			ok := len(r.calls) == len(c.calls)
			for i := 0; ok && i < len(c.calls); i++ {
				ok = r.calls[i] == c.calls[i] || strings.HasPrefix(r.calls[i], c.calls[i]+" ")
			}
			if !ok {
				t.Errorf("calls %q, want %q", r.calls, c.calls)
			}
			if r.exit != c.exit {
				t.Errorf("the block exits %d, want %d; output:\n%s", r.exit, c.exit, r.output)
			}
			if _, err := os.Stat(filepath.Join(r.evidence, "state-before.json")); (err == nil) != c.stateFile {
				t.Errorf("state file there: %v, want %v", err == nil, c.stateFile)
			}
		})
	}
}

// A guide selects the tab of the task before any page command. In the
// core guide, in file order, the only calls before the selection of the
// tab that sim up printed are the simulator's reservation and sim up. In
// the device guide, the only calls before tab find are the checks that
// start nothing, the reservation and the tab listing, which name no page.
func TestGuideFindsTheTargetBeforeAnyPageCommand(t *testing.T) {
	for _, c := range []struct {
		guide, selection string   // the guide and the start of the call that selects the tab
		before           []string // the calls that may run before it, each by its arguments or their start
	}{
		{"iwdp", "tab t", []string{"sim reserve", "sim up"}},
		{"iwdp-device", "tab find", []string{"status", "session info", "device reserve", "tab list"}},
	} {
		t.Run(c.guide, func(t *testing.T) {
			var content string
			for _, s := range guides(t) {
				if s.Name == c.guide {
					content = s.Content
				}
			}
			for _, b := range bashBlockRE.FindAllStringSubmatch(content, -1) {
				for _, call := range runFailing(t, b[1], "", 0).calls {
					if strings.HasPrefix(call, c.selection) {
						return
					}
					allowed := false
					for _, p := range c.before {
						allowed = allowed || call == p || strings.HasPrefix(call, p+" ")
					}
					if !allowed {
						t.Fatalf("iwdp-cli %s runs before %s:\n%s", call, c.selection, b[1])
					}
				}
			}
			t.Fatalf("guide %s runs no %s", c.guide, c.selection)
		})
	}
}

// stopSuffix ends a command that stops its block when it fails: the block
// ends with the exit code of the command, and the status line of private
// mode names the command.
const stopSuffix = `|| exit "$?"`

// goesOn lists the commands that do not stop their block when they fail,
// each with its reason. Each records its status with captureSuffix
// (|| closed=$?), so that a shell with -e also goes on to the cleanup. In every bash block of every guide, every other
// command except the last of its block ends with stopSuffix. A read whose
// result no later command uses stops too: after a failed read the page or
// the session is in a state that the agent must look at. A block that goes
// on after a failed command still ends with a nonzero status.
var goesOn = []struct {
	guide   string
	block   string // a text that only the block holds
	command string // the command, without its comment
	reason  string
}{
	{
		"iwdp-devtools", "state save", "iwdp-cli cookies set session_hint abc123 --http-only --same-site Lax --expires 0",
		"state load restores the saved cookies and storage also after a failed change (safety rule 4)",
	},
	{
		"iwdp-device", "device release", "iwdp-cli close",
		"device release frees the shared device also after a failed close (safety rule 6)",
	},
	{
		"iwdp", "sim release", `iwdp-cli tab close "${TAB:-}"`,
		"sim release frees the shared simulator also after a failed tab close, which Safari can refuse (safety rule 6)",
	},
}

// goesOnRow returns the index of the row of goesOn that names command c
// of a block of a guide, or -1.
func goesOnRow(guide, block string, c blockCommand) int {
	for i, row := range goesOn {
		if row.guide == guide && strings.Contains(block, row.block) && row.command == c.text {
			return i
		}
	}
	return -1
}

// blockCommand is one command of a bash block.
type blockCommand struct {
	line     int    // the line of the command in the block, from 1
	name     string // the command word, such as iwdp-cli or [
	text     string // the line without its stop or capture and its comment
	stops    bool   // the line ends with stopSuffix
	captures bool   // the line ends with captureSuffix
}

// captureSuffix ends a command of goesOn: it records the status of the
// command in a variable, which a shell with -e does not stop at.
var captureSuffix = regexp.MustCompile(`\|\| [A-Za-z_][A-Za-z0-9_]*=\$\?$`)

// blockCommands returns the commands of a bash block in order, after its
// first line (startLine). It knows the forms that the guides use and
// refuses every other form, so that no command hides from the stop rule:
// one command per line (a pipeline or a list hides the status of a
// command), a line continuation, a comment, a redirection, a here-string,
// a heredoc (its body holds no command), an assignment without a command
// substitution (not a command: it cannot fail), and a command
// substitution only as the whole value of an assignment (the status of
// the line is then that of the command; in the arguments of a command its
// failure is lost).
func blockCommands(body string) ([]blockCommand, error) {
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) == 0 || lines[0] != startLine {
		return nil, fmt.Errorf("the block does not start with %q", startLine)
	}
	var cmds []blockCommand
	for i := 1; i < len(lines); i++ {
		at := i + 1
		line := lines[i]
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + " " + lines[i]
		}
		code, heredocs, _, err := scanShellLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d %q: %w", at, line, err)
		}
		for _, d := range heredocs {
			for i++; i < len(lines) && strings.TrimLeft(lines[i], "\t") != d; i++ {
			}
			if i == len(lines) {
				return nil, fmt.Errorf("line %d: the heredoc does not end with %q", at, d)
			}
		}
		if code == "" {
			continue
		}
		c := blockCommand{line: at, text: code}
		if rest, ok := strings.CutSuffix(code, stopSuffix); ok {
			c.text, c.stops = strings.TrimSpace(rest), true
		} else if m := captureSuffix.FindStringIndex(code); m != nil {
			c.text, c.captures = strings.TrimSpace(code[:m[0]]), true
		}
		if c.name, err = commandName(c.text); err != nil {
			return nil, fmt.Errorf("line %d %q: %w", at, line, err)
		}
		if c.name != "" {
			cmds = append(cmds, c)
		}
	}
	return cmds, nil
}

var (
	assignWordRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	substAssignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=(?:"\$\((.*)\)"|\$\((.*)\))$`)
)

// commandName returns the command word of one command, or "" for an
// assignment without a command substitution.
func commandName(text string) (string, error) {
	_, _, substs, err := scanShellLine(text)
	if err != nil {
		return "", err
	}
	if substs > 0 {
		m := substAssignRE.FindStringSubmatch(text)
		if m == nil {
			return "", errors.New("a command substitution stands in the arguments of a command, where its failure is lost: assign it to a variable on a line of its own")
		}
		inner := m[1] + m[2]
		if substs > 1 {
			return "", errors.New("more than one command substitution, or one inside another")
		}
		return commandName(inner)
	}
	for _, w := range shellWords(text) {
		if !assignWordRE.MatchString(w) {
			return strings.NewReplacer("'", "", "\"", "").Replace(w), nil
		}
	}
	return "", nil
}

// shellWords splits a command at blanks outside quotes.
func shellWords(s string) []string {
	var words []string
	var w strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == ' ' || ch == '\t':
			if w.Len() > 0 {
				words = append(words, w.String())
				w.Reset()
			}
			continue
		}
		w.WriteByte(ch)
	}
	if w.Len() > 0 {
		words = append(words, w.String())
	}
	return words
}

// scanShellLine returns a line without its comment, the delimiters of
// its heredocs and the number of its command substitutions. It refuses
// a control operator other than the one of stopSuffix or captureSuffix
// at the end of the line, a subshell, a backquote and an open quote.
func scanShellLine(line string) (string, []string, int, error) {
	var heredocs []string
	var quote byte // the open quote, ' or "
	depth := 0     // the depth of $( inside the line
	substs := 0
	end := len(line)
	ops := 0 // the control operators outside quotes
	opAt := -1
	for i := 0; i < len(line); i++ {
		ch := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		switch {
		case quote == '\'':
			if ch == '\'' {
				quote = 0
			}
			continue
		case ch == '\\':
			i++
			continue
		case ch == '`':
			return "", nil, 0, errors.New("a backquote: use $(...) as the guides do")
		case ch == '$' && next == '(':
			depth++
			substs++
			i++
			continue
		case ch == ')' && depth > 0:
			depth--
			continue
		case quote == '"':
			if ch == '"' {
				quote = 0
			}
			continue
		case ch == '\'' || ch == '"':
			quote = ch
			continue
		}
		// Outside quotes.
		switch {
		case ch == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			end = i
			i = len(line)
		case ch == '<' && next == '<':
			if strings.HasPrefix(line[i:], "<<<") {
				i += 2
				continue
			}
			rest := strings.TrimLeft(strings.TrimPrefix(line[i+2:], "-"), " \t")
			d, _, _ := strings.Cut(rest, " ")
			d = strings.NewReplacer("'", "", "\"", "").Replace(d)
			if d == "" {
				return "", nil, 0, errors.New("a heredoc without a delimiter")
			}
			heredocs = append(heredocs, d)
			i++
		case ch == '&' && (next == '>' || (i > 0 && (line[i-1] == '>' || line[i-1] == '<'))):
			// a redirection: &>, >&2, <&0
		case ch == '|' && i > 0 && line[i-1] == '>':
			// the redirection >|
		case ch == ';' || ch == '&' || ch == '|':
			ops++
			opAt = i
			if next == ch {
				i++
			}
		case ch == '(' || ch == ')':
			return "", nil, 0, errors.New("a subshell or a group: write one command per line")
		}
	}
	if quote != 0 || depth != 0 {
		return "", nil, 0, errors.New("a quote or a command substitution that the line does not close")
	}
	code := strings.TrimSpace(line[:end])
	lead := len(line[:end]) - len(strings.TrimLeft(line[:end], " \t"))
	suffixAt := -1
	if strings.HasSuffix(code, stopSuffix) {
		suffixAt = len(code) - len(stopSuffix)
	} else if m := captureSuffix.FindStringIndex(code); m != nil {
		suffixAt = m[0]
	}
	if ops > 1 || (ops == 1 && (suffixAt < 0 || opAt != lead+suffixAt)) {
		return "", nil, 0, fmt.Errorf("a pipeline or a list: write one command per line, with %s or || <name>=$? as its only operator", stopSuffix)
	}
	return code, heredocs, substs, nil
}

// blockCommands finds every command of a block in the forms that a guide
// can use, and refuses a form in which a command could hide from the stop
// rule.
func TestBlockCommandsFindsEveryCommand(t *testing.T) {
	type cmd struct {
		name, text string
		stops      bool
	}
	for _, c := range []struct {
		name, body string
		want       []cmd
	}{
		{"a stop and a last command", "iwdp-cli a || exit \"$?\"\niwdp-cli b", []cmd{{"iwdp-cli", "iwdp-cli a", true}, {"iwdp-cli", "iwdp-cli b", false}}},
		{"a comment, also with # in quotes", "iwdp-cli eval 'a # b' || exit \"$?\"   # why\n# a comment line\n\niwdp-cli c # \"x", []cmd{{"iwdp-cli", "iwdp-cli eval 'a # b'", true}, {"iwdp-cli", "iwdp-cli c", false}}},
		{"a command substitution", "tab=\"$(iwdp-cli tab find --url-file \"$F\")\" || exit \"$?\"\nx=$(iwdp-cli get cdp-url)", []cmd{{"iwdp-cli", "tab=\"$(iwdp-cli tab find --url-file \"$F\")\"", true}, {"iwdp-cli", "x=$(iwdp-cli get cdp-url)", false}}},
		{"a here-string and redirections", "iwdp-cli fill @e2 --stdin <<<\"$P\" || exit \"$?\"\nagent-browser snapshot >\"$E/x\" 2>&1 || exit \"$?\"\niwdp-cli a >&2", []cmd{{"iwdp-cli", "iwdp-cli fill @e2 --stdin <<<\"$P\"", true}, {"agent-browser", "agent-browser snapshot >\"$E/x\" 2>&1", true}, {"iwdp-cli", "iwdp-cli a >&2", false}}},
		{"a heredoc", "iwdp-cli eval --stdin <<'EOF' || exit \"$?\"\na; b | c\nEOF\niwdp-cli b <<-END\n\tx\n\tEND\nrm f", []cmd{{"iwdp-cli", "iwdp-cli eval --stdin <<'EOF'", true}, {"iwdp-cli", "iwdp-cli b <<-END", false}, {"rm", "rm f", false}}},
		{"a line continuation", "iwdp-cli a \\\n  --b || exit \"$?\"\nmkdir d", []cmd{{"iwdp-cli", "iwdp-cli a    --b", true}, {"mkdir", "mkdir d", false}}},
		{"$( in single quotes is no substitution", "iwdp-cli eval '$(\"#a\").length' || exit \"$?\"", []cmd{{"iwdp-cli", "iwdp-cli eval '$(\"#a\").length'", true}}},
		{"assignments", "closed=$?\nX=1 iwdp-cli a || exit \"$?\"\n[ \"$closed\" = 0 ]", []cmd{{"iwdp-cli", "X=1 iwdp-cli a", true}, {"[", "[ \"$closed\" = 0 ]", false}}},
		{"a capture", "closed=0\niwdp-cli tab close \"${TAB:?}\" || closed=$?   # why\n[ \"$closed\" = 0 ]", []cmd{{"iwdp-cli", "iwdp-cli tab close \"${TAB:?}\"", false}, {"[", "[ \"$closed\" = 0 ]", false}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := blockCommands(startLine + "\n" + c.body + "\n")
			if err != nil {
				t.Fatal(err)
			}
			var have []cmd
			for _, g := range got {
				have = append(have, cmd{g.name, g.text, g.stops})
			}
			if fmt.Sprint(have) != fmt.Sprint(c.want) {
				t.Errorf("commands %+v, want %+v", have, c.want)
			}
		})
	}
	for _, body := range []string{
		"iwdp-cli a | iwdp-cli b",
		"iwdp-cli a; iwdp-cli b",
		"iwdp-cli a && iwdp-cli b",
		"iwdp-cli a || iwdp-cli b",
		"iwdp-cli a || exit \"$?\" || true",
		"iwdp-cli a || closed=$? || exit \"$?\"",
		"iwdp-cli a || closed=1",
		"iwdp-cli a && closed=$?",
		"iwdp-cli a &",
		"(exit 1)",
		"{ iwdp-cli a; }",
		"iwdp-cli fill @e1 \"$(cat f)\" || exit \"$?\"",
		"x=\"$(iwdp-cli a \"$(cat f)\")\"",
		"x=\"$(iwdp-cli a | cat)\"",
		"x=`iwdp-cli a`",
		"iwdp-cli eval 'a",
		"iwdp-cli eval --stdin <<EOF\nx",
		"iwdp-cli a <<",
	} {
		if cmds, err := blockCommands(startLine + "\n" + body); err == nil {
			t.Errorf("blockCommands takes %q as %v", body, cmds)
		}
	}
	if _, err := blockCommands("iwdp-cli a\n"); err == nil {
		t.Error("blockCommands takes a block without the start line")
	}
}

// The rule of the blocks: in every bash block of every guide, every
// command except the last stops the block when it fails, or goesOn names
// it with its reason. A row of goesOn that names no such command is
// stale. An agent copies the blocks into a plain shell without -e, so a
// command that goes on after a failure acts on a page or a session that
// is not in the state the block expects (review F24, F25, F43).
func TestEveryGuideCommandStopsItsBlockOrGoesOnForAReason(t *testing.T) {
	used := make([]int, len(goesOn))
	for _, s := range guides(t) {
		for i, b := range bashBlockRE.FindAllStringSubmatch(s.Content, -1) {
			cmds, err := blockCommands(b[1])
			if err != nil {
				t.Errorf("guide %s, block %d: %v", s.Name, i+1, err)
				continue
			}
			if len(cmds) == 0 {
				t.Errorf("guide %s, block %d runs no command", s.Name, i+1)
			}
			for j, c := range cmds {
				row := goesOnRow(s.Name, b[1], c)
				if row >= 0 {
					used[row]++
				}
				switch {
				case row >= 0 && !c.captures:
					t.Errorf("guide %s, block %d, line %d: goesOn row %d names %q, which does not record its status: end it with || <name>=$?, or a shell with -e stops before the cleanup", s.Name, i+1, c.line, row, c.text)
				case row < 0 && c.captures:
					t.Errorf("guide %s, block %d, line %d: %q records its status and goes on: add a row with the reason to goesOn", s.Name, i+1, c.line, c.text)
				case row >= 0 && (c.stops || j == len(cmds)-1):
					t.Errorf("guide %s, block %d, line %d: goesOn row %d names %q, which stops the block or ends it: delete the row", s.Name, i+1, c.line, row, c.text)
				case row < 0 && !c.stops && j < len(cmds)-1:
					t.Errorf("guide %s, block %d, line %d: %q goes on when it fails: end it with %s, or add a row with the reason to goesOn", s.Name, i+1, c.line, c.text, stopSuffix)
				}
			}
		}
	}
	for i, n := range used {
		if n != 1 {
			t.Errorf("goesOn row %d (%s, %q) names %d commands of its guide, want 1", i, goesOn[i].guide, goesOn[i].command, n)
		}
	}
}

// Every block, run in a plain bash without -e and in a bash with -e, once
// with no failure and once per command with a stand-in that fails that
// command: after a failure no later command runs, except after a command
// of goesOn, and the block ends with the exit code of the failure, or
// after a command of goesOn with a nonzero status. Without a failure
// every command runs and the block ends with 0. The cases come from the
// blocks, so a new block or a new line gets its cases. With -e too, a
// failed close or cookie change still runs the release or the restore
// (closure review of the skill rewrite, 2026-11-24).
func TestEveryGuideBlockStopsAtEveryFailedCommand(t *testing.T) {
	const code = 8 // no exit code of iwdp-cli, so the status is the stand-in's
	for _, s := range guides(t) {
		for i, b := range bashBlockRE.FindAllStringSubmatch(s.Content, -1) {
			cmds, err := blockCommands(b[1])
			if err != nil {
				t.Errorf("guide %s, block %d: %v (TestEveryGuideCommandStopsItsBlockOrGoesOnForAReason)", s.Name, i+1, err)
				continue
			}
			for _, c := range cmds {
				if !standInNames[c.name] {
					t.Fatalf("guide %s, block %d, line %d runs %s, which has no stand-in: add one to standIns", s.Name, i+1, c.line, c.name)
				}
			}
			for k := 0; k <= len(cmds); k++ {
				name := fmt.Sprintf("%s/block %d/no failure", s.Name, i+1)
				if k > 0 {
					name = fmt.Sprintf("%s/block %d/line %d fails", s.Name, i+1, cmds[k-1].line)
				}
				for _, mode := range []struct {
					name string
					run  func(*testing.T, string, int, int) blockRun
				}{{"plain", runFailingAt}, {"set -e", runFailingAtWithE}} {
					t.Run(name+"/"+mode.name, func(t *testing.T) {
						r := mode.run(t, b[1], k, code)
						ran, exit := len(cmds), 0
						switch {
						case k > 0 && goesOnRow(s.Name, b[1], cmds[k-1]) >= 0:
							exit = -1 // any status but 0
						case k > 0:
							ran, exit = k, code
						}
						var want []string
						for _, c := range cmds[:ran] {
							want = append(want, c.name)
						}
						if fmt.Sprint(r.names) != fmt.Sprint(want) {
							t.Errorf("the commands %q ran, want %q", r.names, want)
						}
						if (exit < 0 && r.exit == 0) || (exit >= 0 && r.exit != exit) {
							t.Errorf("the block exits %d, want %d (-1: not 0); output:\n%s", r.exit, exit, r.output)
						}
					})
				}
			}
		}
	}
}

// The repro of review F43: in the DevTools block a failed HAR start, route
// or trace start does not go on to the reload, the unroute or the trace
// stop, and in the navigation block a failed forward does not go on to
// the reload.
func TestReviewDevtoolsStopsAtFailedPrerequisite(t *testing.T) {
	block := guideBlock(t, "iwdp-devtools", "network har start")
	for _, tc := range []struct{ fail, forbid string }{
		{"network har start", "reload"},
		{"network route", "network unroute"},
		{"trace start", "trace stop"},
	} {
		r := runFailing(t, block, tc.fail, 4)
		for _, c := range r.calls {
			if c == tc.forbid || strings.HasPrefix(c, tc.forbid+" ") {
				t.Errorf("%s failed, but %s ran; exit %d calls %q", tc.fail, c, r.exit, r.calls)
			}
		}
	}
	core := guideBlock(t, "iwdp", "click @e6")
	r := runFailing(t, core, "forward", 4)
	for _, c := range r.calls {
		if c == "reload" {
			t.Errorf("forward failed, but reload ran; exit %d calls %q", r.exit, r.calls)
		}
	}
}
