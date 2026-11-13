package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Tests of private mode (private.go): --private and IWDP_PRIVATE print no
// page title, URL, page key, device, session name or error text. The
// marker stands in every such place of the fakes; it must never reach the
// output.
const privateMarker = "PRIVATE-MARK"

// remoteStub runs commands as the daemon does and returns the result as
// the daemon sends it: the data as raw JSON.
type remoteStub struct {
	mgrEnv
	t       *testing.T
	session string
	fail    error // returned in place of running the command
}

func (e *remoteStub) RunRemote(_ context.Context, c *Command, in Input) (Result, error) {
	if e.fail != nil {
		return Result{}, e.fail
	}
	argv := Argv(c, in)
	if in.Global.Page != "" {
		argv = append([]string{"--page", in.Global.Page}, argv...)
	}
	res, err := runIn(e.t, e.m, e.session, argv...)
	if err != nil || res.Data == nil {
		return res, err
	}
	data, err := json.Marshal(res.Data)
	if err != nil {
		e.t.Fatal(err)
	}
	return Result{Text: res.Text, Data: json.RawMessage(data)}, nil
}

// runPrivate runs a command line through RunCLIStarted against stub.
func runPrivate(t *testing.T, stub *remoteStub, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunCLIStarted(context.Background(), args, &out, &errOut, nil, func(context.Context, *Invocation) (Env, error) { return stub, nil })
	return code, out.String(), errOut.String()
}

func TestPrivateModePrintsIDsAndCountsOnly(t *testing.T) {
	m, f := newTabManager(t)
	f.add("One "+privateMarker, "http://one.test/"+privateMarker+"?q="+privateMarker)
	f.add("Two "+privateMarker, "http://two.test/"+privateMarker)
	stub := &remoteStub{mgrEnv: mgrEnv{m: m}, t: t, session: "a"}
	for _, tc := range []struct {
		name string
		args []string
		code int
		out  string
		err  string
	}{
		// The session holds no page yet, and the command then sends "pages": null
		// (found by e2e/webkitgtk TestSkillExamplesRunOnWebKitGTK, iwdp-device).
		{"a session without a page", []string{"--private", "session", "info"}, 0, "pages 0\n", "session info exit 0\n"},
		{"the tab listing", []string{"--private", "tab", "list"}, 0, "  t1 debuggable\n  t2 debuggable\nhidden 0\n", "tab list exit 0\n"},
		{"tab without an id lists too", []string{"--private", "tab"}, 0, "  t1 debuggable\n  t2 debuggable\nhidden 0\n", "tab exit 0\n"},
		{"a tab selection", []string{"--private", "tab", "t2"}, 0, "", "tab exit 0\n"},
		{"the listing marks the current tab", []string{"--private", "tab", "list"}, 0, "  t1 debuggable\n* t2 debuggable\nhidden 0\n", "tab list exit 0\n"},
		{
			"the listing as JSON",
			[]string{"--private", "--json", "tab", "list"},
			0,
			`{"hidden":0,"tabs":[{"current":false,"debuggable":true,"id":"t1"},{"current":true,"debuggable":true,"id":"t2"}]}` + "\n", "tab list exit 0\n",
		},
		{"the pages of the session", []string{"--private", "session", "info"}, 0, "pages 1\npage 1 lease active epoch 1 connection attached\n", "session info exit 0\n"},
		{"a status as JSON", []string{"--private", "--json", "tab", "t1"}, 0, `{"command":"tab","exit":0}` + "\n", "tab exit 0\n"},
		{"a tab that is not listed", []string{"--private", "tab", "t9"}, ExitNoPage, "", "tab exit 5: no device or page (private mode hides the message)\n"},
		{"the variable turns the mode on", []string{"tab", "list"}, 0, "* t1 debuggable\n  t2 debuggable\nhidden 0\n", "tab list exit 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "the variable turns the mode on" {
				t.Setenv(privateEnv, "1")
			}
			code, out, errOut := runPrivate(t, stub, tc.args...)
			if code != tc.code || out != tc.out || errOut != tc.err {
				t.Fatalf("%v: exit %d\nstdout %q\nstderr %q\nwant exit %d\nstdout %q\nstderr %q", tc.args, code, out, errOut, tc.code, tc.out, tc.err)
			}
			if strings.Contains(out+errOut, privateMarker) {
				t.Fatalf("%v printed a private value:\n%s%s", tc.args, out, errOut)
			}
		})
	}
	// Without the mode the same listing prints the titles: the fakes do
	// carry the marker.
	if code, out, _ := runPrivate(t, stub, "tab", "list"); code != 0 || !strings.Contains(out, privateMarker) {
		t.Fatalf("tab list without --private: exit %d, %q; want the titles", code, out)
	}
}

// An error can name the page by its title. Private mode prints the
// command, the exit code and what the code means, for every exit code.
func TestPrivateModeNeverPrintsTheTextOfAnError(t *testing.T) {
	text := errors.New("page \"Title " + privateMarker + "\": refused")
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{text, ExitError, "click exit 1: the command failed (private mode hides the message)\n"},
		{&core.CodedError{Code: ExitWaitTimeout, Err: text}, ExitWaitTimeout, "click exit 3: a wait timed out (private mode hides the message)\n"},
		{&core.CodedError{Code: core.CodeBusy, Err: text}, ExitBusy, "click exit 4: the page is leased by another session, or its connection was lost (private mode hides the message)\n"},
		{&core.CodedError{Code: core.CodeNoPage, Err: text}, ExitNoPage, "click exit 5: no device or page (private mode hides the message)\n"},
		{&core.CodedError{Code: core.CodeReservation, Err: text}, ExitReservation, "click exit 6: the device reservation is missing (private mode hides the message)\n"},
		{&core.CodedError{Code: core.CodeTimeout, Err: text}, ExitTimeout, "click exit 124: the command deadline passed (private mode hides the message)\n"},
	} {
		code, out, errOut := runPrivate(t, &remoteStub{t: t, fail: tc.err}, "--private", "click", "@e1")
		if code != tc.code || out != "" || errOut != tc.want {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and %q", code, out, errOut, tc.code, tc.want)
		}
		// The same error without the mode prints its text.
		if _, _, errOut := runPrivate(t, &remoteStub{t: t, fail: tc.err}, "click", "@e1"); !strings.Contains(errOut, privateMarker) {
			t.Errorf("without --private the error line is %q; want the error's text", errOut)
		}
	}
}

// A usage error can repeat an argument, and an argument can be a URL.
func TestPrivateModeHidesTheArgumentsOfAUsageError(t *testing.T) {
	secret := "https://example.test/" + privateMarker
	for _, args := range [][]string{
		{"--private", "open", secret, "--no-such-flag"},
		{"--private", "open", secret, "extra"},
		{"--private=maybe", "open", secret, "--no-such-flag"},
		{"--private", "no-such-command", secret},
		{"--private", "get", "url"},
		{"--private", "get", "title"},
	} {
		code, out, errOut := runPrivate(t, &remoteStub{t: t, fail: errors.New("must not run")}, args...)
		if code != ExitUsage || out != "" || strings.Contains(errOut, privateMarker) || !strings.Contains(errOut, "exit 2: usage error") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 2 and a line without the argument", args, code, out, errOut)
		}
	}
	t.Setenv(privateEnv, "1")
	if code, _, errOut := runPrivate(t, &remoteStub{t: t, fail: errors.New("must not run")}, "open", secret, "--no-such-flag"); code != ExitUsage || strings.Contains(errOut, privateMarker) {
		t.Errorf("with %s=1: exit %d, stderr %q", privateEnv, code, errOut)
	}
}

func TestPrivateFlagAndVariable(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == privateEnv {
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		args []string
		env  string
		want bool
	}{
		{[]string{"close"}, "", false},
		{[]string{"--private", "close"}, "", true},
		{[]string{"close", "--private"}, "", true},
		{[]string{"close"}, "1", true},
		{[]string{"close"}, "true", true},
		{[]string{"close"}, "0", false},
		{[]string{"--private=false", "close"}, "1", false},
		{[]string{"--private=true", "close"}, "0", true},
		// The value of another option is not the flag.
		{[]string{"--private", "--session", "--private=false", "close"}, "", true},
		{[]string{"--session", "--private", "close"}, "", false},
	} {
		inv, err := Parse(tc.args, env(tc.env))
		if err != nil || inv.Input.Global.Private != tc.want {
			t.Errorf("%v with %s=%q: private %v, err %v; want %v", tc.args, privateEnv, tc.env, inv != nil && inv.Input.Global.Private, err, tc.want)
		}
		if got := privateRequested(tc.args, env(tc.env)); got != tc.want {
			t.Errorf("privateRequested(%v, %q) = %v, want %v", tc.args, tc.env, got, tc.want)
		}
	}
	var ue *UsageError
	if _, err := Parse([]string{"close"}, env("yes please")); !errors.As(err, &ue) {
		t.Errorf("%s=yes please: %v, want a usage error", privateEnv, err)
	}
	// A malformed request for the mode keeps the line private.
	if !privateRequested([]string{"--private=maybe", "close"}, env("")) || !privateRequested([]string{"close"}, env("yes please")) {
		t.Error("a malformed --private or variable must count as private")
	}
	// After "--" the word is an argument, not the flag.
	if privateRequested([]string{"eval", "--", "--private"}, env("")) {
		t.Error("--private after -- is an argument")
	}
}

func TestGetURLAndTitleAreRefusedInPrivateModeOnly(t *testing.T) {
	none := func(string) string { return "" }
	for _, what := range []string{"url", "title"} {
		if _, err := Parse([]string{"--private", "get", what}, none); err == nil || !strings.Contains(err.Error(), "private mode hides") {
			t.Errorf("--private get %s: %v, want a refusal", what, err)
		}
		if _, err := Parse([]string{"get", what}, none); err != nil {
			t.Errorf("get %s: %v", what, err)
		}
	}
	if _, err := Parse([]string{"--private", "get", "text", "#a"}, none); err != nil {
		t.Errorf("--private get text: %v", err)
	}
}

// The classes of private.go: an action prints counts, never text; a read
// prints its result; every other command prints its status line.
func TestPrivateResultByClass(t *testing.T) {
	cmd := func(name string) *Command {
		c, _ := Lookup(strings.Fields(name))
		if c == nil {
			t.Fatalf("no command %q", name)
		}
		return c
	}
	raw := func(s string) json.RawMessage { return json.RawMessage(strings.ReplaceAll(s, "MARK", privateMarker)) }
	action := raw(`{"trusted":false,"steps":1,"acknowledged":1,"emulated":["x MARK"],"dialogs":[{"message":"MARK"}],"warnings":["MARK","b"]}`)
	two := 2
	for _, tc := range []struct {
		name   string
		res    Result
		want   string // stdout
		status string // stderr
	}{
		{"click", Result{Text: "clicked " + privateMarker, Data: action}, "", "click exit 0 warnings 2 dialogs 1"},
		{"click", Result{Text: "clicked", Data: &actResult{Steps: 1}}, "", "click exit 0 warnings 0 dialogs 0"},
		{"find", Result{Text: "clicked " + privateMarker, Data: action}, "", "find exit 0 warnings 2 dialogs 1"},
		{"find", Result{Text: "- button [ref=e1]", Data: raw(`{"matches":[{"ref":"e1","role":"button"}],"warnings":[]}`)}, "- button [ref=e1]", "find exit 0"},
		{"snapshot", Result{Text: "- heading", Data: raw(`{"snapshot":"- heading","url":"https://x.test/MARK","title":"MARK"}`)}, "- heading", "snapshot exit 0"},
		// A read without data is not a result of the command: it prints
		// as a status line.
		{"snapshot", Result{Text: "- heading " + privateMarker}, "", "snapshot exit 0"},
		{"tab find", Result{Text: "t2", Data: raw(`{"tab":"t2"}`)}, "t2", "tab find exit 0"},
		{"open", Result{Text: "navigated to " + privateMarker, Data: raw(`{"url":"MARK","title":"t"}`)}, "", "open exit 0"},
		{"wait", Result{Text: privateMarker, Data: raw(`{"ok":true,"state":"MARK"}`)}, "", "wait exit 0"},
		{"dialog", Result{Text: privateMarker, Data: raw(`{"log":[{"message":"MARK"}]}`)}, "", "dialog exit 0"},
		{"device reserve", Result{Text: privateMarker}, "", "device reserve exit 0"},
		{"devices", Result{Text: privateMarker, Data: raw(`[{"udid":"MARK"}]`)}, "", "devices exit 0"},
		{"session list", Result{Text: privateMarker}, "", "session list exit 0"},
		{"status", Result{Text: privateMarker, Data: statusData{Running: true, Log: "/home/" + privateMarker}}, "iwdp answers: true", "status exit 0"},
		{"status", Result{Text: privateMarker, Data: statusData{Running: true, Devices: &two, Log: "/home/" + privateMarker}}, "iwdp answers: true\ndevices 2", "status exit 0"},
		{"session info", Result{Text: privateMarker, Data: raw(`{"session":"s","pages":null}`)}, "pages 0", "session info exit 0"},
	} {
		got, status := privateResult(cmd(tc.name), tc.res)
		data, err := json.Marshal(got.Data)
		if err != nil {
			t.Fatal(err)
		}
		if got.Text != tc.want || status != tc.status || strings.Contains(string(data), privateMarker) {
			t.Errorf("%s: stdout %q, status %q, data %s; want %q, %q and no private value", tc.name, got.Text, status, data, tc.want, tc.status)
		}
	}
	// A read prints the value that the caller asked for, also when that
	// value is a URL: get cdp-url exists to print one, which the caller
	// keeps in a variable.
	// Its data also names the page by its title (cdp.Endpoint), which the
	// caller did not ask for.
	ep := cdp.Endpoint{WS: "ws://127.0.0.1:9230/cdp/token", HTTP: "http://127.0.0.1:9230/cdp/token", Scope: "page", Page: "Title " + privateMarker}
	got, _ := privateResult(cmd("get cdp-url"), Result{Text: ep.WS, Data: ep})
	data, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != ep.WS || string(data) != `{"http":"http://127.0.0.1:9230/cdp/token","scope":"page","ws":"ws://127.0.0.1:9230/cdp/token"}` {
		t.Errorf("get cdp-url in private mode prints %q and %s; want the URL that the caller asked for, and no title", got.Text, data)
	}
}

// Every class names a command of the registry, with a reason; a renamed
// command must not fall out of its class unnoticed.
func TestPrivateClassesNameRegistryCommands(t *testing.T) {
	names := map[string]string{}
	for name, read := range privateReads {
		names[name] = read.what
		if read.data == nil || read.text == nil {
			t.Errorf("private read %q says nothing about what of its result prints", name)
		}
	}
	for name := range privateSummaries {
		if _, both := privateReads[name]; both {
			t.Errorf("%q is a read and a summary", name)
		}
		names[name] = "a summary"
	}
	for name, why := range names {
		if c, n := Lookup(strings.Fields(name)); c == nil || c.Name != name || n != len(strings.Fields(name)) {
			t.Errorf("private class of %q: no such registry command", name)
		}
		if len(why) < 5 {
			t.Errorf("private read %q has no reason", name)
		}
	}
}

func TestURLFileGivesTheURLWithoutTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	none := func(string) string { return "" }
	secret := "https://example.test/r/" + privateMarker + "?sig=" + privateMarker + "#k"
	for _, name := range []string{"open", "tab find"} {
		inv, err := Parse(append(strings.Fields(name), "--url-file", write("ok", secret+"\n")), none)
		if err != nil {
			t.Fatal(err)
		}
		if err := inv.Cmd.ClientStep(context.Background(), &inv.Input, nil); err != nil {
			t.Fatalf("%s --url-file: %v", name, err)
		}
		// The flag stays: it says where the URL came from (urlFile).
		if len(inv.Input.Args) != 1 || inv.Input.Args[0] != secret || flagString(inv.Input, "url-file") != filepath.Join(dir, "ok") {
			t.Errorf("%s --url-file: args %v, flags %v; want the URL as the argument and the flag with the file's path", name, inv.Input.Args, inv.Input.Flags)
		}
		for _, bad := range []struct{ file, want string }{
			{write("empty", "\n"), "is empty"},
			{write("two", secret+"\n"+secret+"\n"), "one URL on one line"},
			{filepath.Join(dir, "missing"), "--url-file"},
		} {
			inv, err := Parse(append(strings.Fields(name), "--url-file", bad.file), none)
			if err != nil {
				t.Fatal(err)
			}
			err = inv.Cmd.ClientStep(context.Background(), &inv.Input, nil)
			if err == nil || !strings.Contains(err.Error(), bad.want) || strings.Contains(err.Error(), privateMarker) {
				t.Errorf("%s --url-file %s: %v; want an error with %q and without the content", name, filepath.Base(bad.file), err, bad.want)
			}
		}
		// A URL argument and the file together, and neither of them.
		inv, err = Parse(append(strings.Fields(name), secret, "--url-file", write("ok", secret)), none)
		if err != nil {
			t.Fatal(err)
		}
		var ue *UsageError
		if err := inv.Cmd.ClientStep(context.Background(), &inv.Input, nil); !errors.As(err, &ue) || strings.Contains(err.Error(), privateMarker) {
			t.Errorf("%s <url> --url-file: %v, want a usage error without the URL", name, err)
		}
		inv, err = Parse(strings.Fields(name), none)
		if err != nil {
			t.Fatal(err)
		}
		if err := needURL(inv.Input, name); !errors.As(err, &ue) {
			t.Errorf("%s without a URL: %v, want a usage error", name, err)
		}
	}
}

// The value of another global option is not the flag: the mode stays on,
// and the usage error prints no argument (review of 2026-11-13, F12).
func TestPrivateModeIsNotTurnedOffByTheValueOfAnotherOption(t *testing.T) {
	args := []string{"--private", "--session", "--private=false", "unknown-" + privateMarker}
	code, out, errOut := runPrivate(t, &remoteStub{t: t, fail: errors.New("must not run")}, args...)
	if code != ExitUsage || out != "" || strings.Contains(errOut, privateMarker) {
		t.Fatalf("%v: exit %d, stdout %q, stderr %q; want exit 2 without the argument", args, code, out, errOut)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write refused: " + privateMarker)
}

// A result that cannot be written is an error like any other: private
// mode prints no text of it (review of 2026-11-13, F12).
func TestPrivateModePrintsNoTextOfAWriteFailure(t *testing.T) {
	var errOut bytes.Buffer
	code := RunCLIStarted(context.Background(), []string{"--private", "skills", "list"}, failingWriter{}, &errOut, nil,
		func(context.Context, *Invocation) (Env, error) { return nil, nil })
	if want := "skills list exit 1: the command failed (private mode hides the message)\n"; code != ExitError || errOut.String() != want {
		t.Fatalf("exit %d, stderr %q; want exit 1 and %q", code, errOut.String(), want)
	}
	errOut.Reset()
	code = RunCLIStarted(context.Background(), []string{"skills", "list"}, failingWriter{}, &errOut, nil,
		func(context.Context, *Invocation) (Env, error) { return nil, nil })
	if code != ExitError || !strings.Contains(errOut.String(), "Error: writing the result: write refused") {
		t.Fatalf("without --private: exit %d, stderr %q; want the error of the write", code, errOut.String())
	}
}

// privateModel is the mode of a line of global options by the rules of
// the parser, written apart from it: a boolean option alone is true, with
// "=value" it takes a ParseBool value, an option with a value takes the
// next word, "--" ends the options, and the last --private wins over the
// variable. ok is false for a line whose global options are malformed.
func privateModel(args []string, env string) (private, ok bool) {
	private = env == "1"
	takesValue := map[string]bool{"--session": true, "--page": true, "--timeout": true, "--transport": true, "--max-output": true, "--reservation": true, "--wait-lease": true}
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch {
		case args[i] == "--":
			return private, true
		case takesValue[name] && !hasVal:
			i++
			if i >= len(args) {
				return private, false
			}
		case name == "--private" && !hasVal:
			private = true
		case name == "--private":
			if val != "true" && val != "false" {
				return private, false
			}
			private = val == "true"
		}
	}
	return private, true
}

// For every line of global options and words, privateRequested gives the
// mode that the parser's rules give; for a malformed line it is on when
// anything asks for it. When Parse takes the line, both agree with it.
func TestPrivateRequestedAgreesWithTheParser(t *testing.T) {
	words := []string{"--private", "--private=false", "--private=true", "--private=maybe", "--session", "--page", "--json", "--", "close", "x"}
	property := func(picks []uint8, envOn bool) bool {
		if len(picks) > 7 {
			picks = picks[:7]
		}
		args := make([]string, len(picks))
		for i, p := range picks {
			args[i] = words[int(p)%len(words)]
		}
		value := ""
		if envOn {
			value = "1"
		}
		env := func(name string) string {
			if name == privateEnv {
				return value
			}
			return ""
		}
		got := privateRequested(args, env)
		want, ok := privateModel(args, value)
		if !ok {
			asked := envOn
			for _, a := range args {
				asked = asked || strings.HasPrefix(a, "--private")
			}
			want = asked
		}
		if got != want {
			t.Logf("%v with %s=%q: %v, the model gives %v", args, privateEnv, value, got, want)
			return false
		}
		if inv, err := Parse(args, env); err == nil && inv.Input.Global.Private != got {
			t.Logf("%v: Parse gives %v, privateRequested %v", args, inv.Input.Global.Private, got)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
}

// For any content of the file, --url-file gives exactly the content
// without the white space around it, or an error that does not repeat the
// content.
func TestURLFileProperty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "url")
	property := func(body string, pad uint8) bool {
		content := strings.Repeat(" ", int(pad%3)) + body + strings.Repeat("\n", int(pad%2))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		in := Input{Flags: map[string]any{"url-file": path}}
		err := urlFromFile(context.Background(), &in, nil)
		trimmed := strings.TrimSpace(content)
		switch {
		case err == nil:
			return len(in.Args) == 1 && in.Args[0] == trimmed && trimmed != "" && !strings.ContainsAny(trimmed, " \t\r\n")
		case len(trimmed) >= 8 && strings.Contains(err.Error(), trimmed):
			t.Logf("the error repeats the content: %v", err)
			return false
		}
		return trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n") || len(content) > maxURLFileBytes
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// The data of a snapshot names the page's title and URL, and its tree
// the URLs of links. Without -u the caller asked for neither, so private
// mode prints neither, also with --json (review of 2026-11-13, F23).
func TestPrivateSnapshotJSONHasNoTitleOrURL(t *testing.T) {
	h := newHarness(t)
	reply := snapshotReply(3, "doc-1")
	data := reply["value"].(map[string]any)
	data["url"] = "https://x.test/reset?token=" + privateMarker
	data["title"] = "Title " + privateMarker
	data["tree"].([]any)[1].(map[string]any)["url"] = "https://x.test/link-" + privateMarker
	h.page.set("snapshot", reply)
	code, out, errOut := h.run("--private", "--json", "snapshot", "-i")
	if code != 0 || errOut != "snapshot exit 0\n" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(out, privateMarker) {
		t.Fatalf("--private --json snapshot -i printed the page's title or a URL: %s", out)
	}
}

// The content boundary names no origin in private mode: the marker says
// origin=unknown, as for a result without a page (review of 2026-11-13,
// F23).
func TestPrivateSnapshotBoundaryNamesNoOrigin(t *testing.T) {
	h := newHarness(t)
	reply := snapshotReply(3, "doc-1")
	reply["value"].(map[string]any)["url"] = "https://" + strings.ToLower(privateMarker) + ".test/"
	h.page.set("snapshot", reply)
	code, out, errOut := h.run("--private", "--content-boundaries", "snapshot", "-i")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(strings.ToLower(out), strings.ToLower(privateMarker)) || !strings.Contains(out, " origin=unknown ---\n") {
		t.Fatalf("--private --content-boundaries snapshot printed %q; want the marker with origin=unknown", out)
	}
}

// Storage set and clear and state save and load change something: in
// private mode they print only their status line. A storage listing
// prints keys and values, never the origin, also when it is empty
// (review of 2026-11-13, F23).
func TestPrivateStorageAndStateNameNoOrigin(t *testing.T) {
	origin := "https://" + strings.ToLower(privateMarker) + ".example"
	p := devtoolstest.New(t, origin)
	ps := &core.PageSession{Caller: p.Conn}
	dir := t.TempDir()
	for _, tc := range []struct {
		args   []string
		out    string
		status string
	}{
		{[]string{"storage", "local"}, "localStorage is empty.", "storage exit 0\n"},
		{[]string{"storage", "local", "set", "theme", "dark"}, "", "storage exit 0\n"},
		{[]string{"storage", "local"}, "theme=dark", "storage exit 0\n"},
		{[]string{"storage", "local", "theme"}, "dark", "storage exit 0\n"},
		{[]string{"storage", "local", "clear"}, "", "storage exit 0\n"},
		{[]string{"state", "save", "state.json"}, "", "state save exit 0\n"},
		{[]string{"state", "load", "state.json"}, "", "state load exit 0\n"},
	} {
		code, out, errOut := spRun(ps, dir, append([]string{"--private"}, tc.args...)...)
		if code != 0 || out != tc.out || errOut != tc.status {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 0, %q and %q", tc.args, code, out, errOut, tc.out, tc.status)
		}
	}
}

// Markers of the end-to-end tests of private mode: linkMarker stands in
// the URLs of links, askedMarker in content that the caller asks for (an
// element's text, a value, a message, a request URL).
const (
	linkMarker  = "LINK-MARK"
	askedMarker = "ASKED-MARK"
)

// privateCase is a command line of a read in private mode, with the
// markers of what the caller asked for (they print, in the text and in
// the --json data) and of what it did not ask for (they never print in
// private mode; without it, each prints in some output mode, so the
// fixture carries it).
type privateCase struct {
	args   []string
	shown  []string
	hidden []string
	status bool // the command changes something: it prints only its status line
	// textDiffers: the command's text holds what private mode does not
	// print (an origin, the URL and stack of a source, the size of an
	// image); without it the text in private mode is the command's text.
	textDiffers bool
}

// boundaryStartLine is the start marker of a content boundary in private
// mode: it names no origin.
var boundaryStartLine = regexp.MustCompile(`^--- AGENT_BROWSER_PAGE_CONTENT nonce=[0-9a-f]{32} origin=unknown ---\n`)

// checkPrivateRead runs tc with run in the four output modes (text,
// --json, --content-boundaries and both), with and without --private.
// Markers are compared without case: a host is written in lower case.
func checkPrivateRead(t *testing.T, run func(args ...string) (int, string, string), tc privateCase) {
	t.Helper()
	inv, err := Parse(tc.args, func(string) string { return "" })
	if err != nil {
		t.Fatalf("%v: %v", tc.args, err)
	}
	has := func(out, marker string) bool { return strings.Contains(strings.ToLower(out), strings.ToLower(marker)) }
	var plain strings.Builder // what the same command prints without the mode
	var privText string
	for _, mode := range [][]string{nil, {"--json"}, {"--content-boundaries"}, {"--json", "--content-boundaries"}} {
		args := append(append([]string{"--private"}, mode...), tc.args...)
		code, out, errOut := run(args...)
		if code != 0 || errOut != inv.Cmd.Name+" exit 0\n" {
			t.Errorf("%v: exit %d, stderr %q; want exit 0 and the status line %q", args, code, errOut, inv.Cmd.Name+" exit 0")
			continue
		}
		if mode == nil {
			privText = out
		}
		for _, m := range tc.hidden {
			if has(out, m) {
				t.Errorf("%v printed %s, which the caller did not ask for:\n%s", args, m, out)
			}
		}
		if !tc.status {
			for _, m := range tc.shown {
				if !has(out, m) {
					t.Errorf("%v did not print %s, which the caller asked for:\n%s", args, m, out)
				}
			}
		}
		asJSON, bounded := len(mode) > 0 && mode[0] == "--json", len(mode) > 0 && mode[len(mode)-1] == "--content-boundaries"
		switch {
		case asJSON:
			var list []json.RawMessage
			if !bounded && json.Unmarshal([]byte(out), &list) == nil {
				break // a list (skills list); --content-boundaries puts it under "data"
			}
			var v map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Errorf("%v: stdout is not a JSON object: %v\n%s", args, err, out)
				continue
			}
			var b struct{ Origin string }
			if _, marked := v["_boundary"]; bounded && (json.Unmarshal(v["_boundary"], &b) != nil || b.Origin != "unknown") || !bounded && marked {
				t.Errorf("%v: the member _boundary is %s; want origin unknown with --content-boundaries only", args, v["_boundary"])
			}
			delete(v, "_boundary")
			if data, _ := json.Marshal(v); tc.status && string(data) != `{"command":`+strconv.Quote(inv.Cmd.Name)+`,"exit":0}` {
				t.Errorf("%v printed %s; want only the status as JSON", args, data)
			}
		case tc.status && out != "":
			t.Errorf("%v printed %q; want only the status line", args, out)
		case bounded && out != "" && !boundaryStartLine.MatchString(out):
			t.Errorf("%v: the content boundary is not the one without an origin:\n%s", args, out)
		}
		if code, out, errOut := run(append(mode, tc.args...)...); code != 0 {
			t.Errorf("%v without --private: exit %d, %q", tc.args, code, errOut)
		} else {
			plain.WriteString(out + "\n")
			if mode == nil && !tc.status && !tc.textDiffers && out != privText {
				t.Errorf("%v: private mode printed %q, the command's text is %q; want the same text", tc.args, privText, out)
			}
		}
	}
	for _, m := range tc.hidden {
		if !has(plain.String(), m) {
			t.Errorf("%v without --private never printed %s: the fixture does not test it", tc.args, m)
		}
	}
}

// The reads of the page runtime: the page's title and URL and the URLs of
// links are in their data, and print only where the caller asks for them
// (snapshot -u prints link URLs).
func TestPrivateReadsOfThePageRuntime(t *testing.T) {
	h := newHarness(t)
	reply := snapshotReply(3, "doc-1")
	page := reply["value"].(map[string]any)
	page["url"] = "https://" + strings.ToLower(privateMarker) + ".test/reset?token=" + privateMarker
	page["title"] = "Title " + privateMarker
	page["tree"].([]any)[1].(map[string]any)["url"] = "https://link.test/" + linkMarker
	h.page.set("snapshot", reply)
	h.page.set("find", okEnv(map[string]any{
		"nodes":   []any{map[string]any{"ref": "e4", "role": "link", "name": "Go", "url": "https://link.test/" + linkMarker}},
		"refs":    []any{map[string]any{"ref": "e4", "role": "link", "name": "Go", "unique": true}},
		"nextRef": 5, "doc": "doc-1",
	}))
	h.page.set("hitTest", okEnv(map[string]any{
		"nodes":   []any{map[string]any{"ref": "e5", "role": "link", "name": "Go", "url": "https://link.test/" + linkMarker}},
		"refs":    []any{map[string]any{"ref": "e5", "role": "link", "name": "Go", "unique": true}},
		"nextRef": 6, "doc": "doc-1", "element": "<a>", "box": map[string]any{"x": 1, "y": 2, "width": 3, "height": 4},
	}))
	h.page.set("get", okEnv("Text "+askedMarker))
	h.page.set("is", okEnv(true))
	h.page.set("read", okEnv("Page text "+askedMarker))
	for _, tc := range []privateCase{
		{args: []string{"snapshot"}, hidden: []string{privateMarker, linkMarker}},
		{args: []string{"snapshot", "-i"}, hidden: []string{privateMarker, linkMarker}},
		{args: []string{"snapshot", "-u"}, shown: []string{linkMarker}, hidden: []string{privateMarker}},
		{args: []string{"diff", "snapshot"}},
		{args: []string{"find", "role", "link"}, hidden: []string{linkMarker}},
		{args: []string{"hit-test", "1", "2"}, hidden: []string{linkMarker}},
		{args: []string{"get", "text", "@e1"}, shown: []string{askedMarker}},
		{args: []string{"is", "visible", "@e1"}},
		{args: []string{"read"}, shown: []string{askedMarker}},
	} {
		checkPrivateRead(t, h.run, tc)
	}
}

// screenshot prints the caller's path, the size and the labels of
// --annotate (roles and names of the page's elements).
func TestPrivateScreenshot(t *testing.T) {
	s := newShotFake(t)
	path := filepath.Join(t.TempDir(), "shot.png")
	checkPrivateRead(t, s.h.run, privateCase{args: []string{"screenshot", "--annotate", path}, shown: []string{"shot.png", "Save"}, textDiffers: true})
	checkPrivateRead(t, s.h.run, privateCase{args: []string{"screenshot", "--if-changed", path}})
}

// The reads and actions of cookies, storage and state on a page whose
// origin carries the marker; eval prints the page's URL when the caller
// asks for it.
func TestPrivateReadsOfCookiesStorageAndState(t *testing.T) {
	p := devtoolstest.New(t, "https://"+strings.ToLower(privateMarker)+".example")
	p.Cookies = []webkit.Cookie{{Name: "c-" + askedMarker, Value: "v", Domain: "shop.test", Path: "/", Session: true, SameSite: "Lax"}}
	ps := &core.PageSession{Caller: p.Conn}
	dir := t.TempDir()
	run := func(args ...string) (int, string, string) { return spRun(ps, dir, args...) }
	if code, _, errOut := run("state", "save", "state.json"); code != 0 {
		t.Fatalf("state save: exit %d, %q", code, errOut)
	}
	for _, tc := range []privateCase{
		{args: []string{"storage", "local"}, hidden: []string{privateMarker}, textDiffers: true},
		{args: []string{"storage", "local", "set", "k", "v " + askedMarker}, hidden: []string{privateMarker}, status: true},
		{args: []string{"storage", "local"}, shown: []string{"v " + askedMarker}, hidden: []string{privateMarker}},
		{args: []string{"storage", "local", "k"}, shown: []string{askedMarker}, hidden: []string{privateMarker}},
		{args: []string{"storage", "local", "get", "k"}, shown: []string{askedMarker}, hidden: []string{privateMarker}},
		{args: []string{"storage", "local", "clear"}, hidden: []string{privateMarker}, status: true},
		{args: []string{"state", "save", "state.json"}, hidden: []string{privateMarker}, status: true},
		{args: []string{"state", "load", "state.json"}, hidden: []string{privateMarker}, status: true},
		{args: []string{"cookies"}, shown: []string{"c-" + askedMarker}},
		{args: []string{"eval", "location.href"}, shown: []string{privateMarker}},
	} {
		checkPrivateRead(t, run, tc)
	}
}

// Console messages and errors print without the URL and the stack of
// their source, which name the page; requests print their URLs.
func TestPrivateReadsOfTheDevToolsLogs(t *testing.T) {
	h := newDTHarness(t)
	pageURL := "https://" + strings.ToLower(privateMarker) + ".test/page?token=" + privateMarker
	h.mustRun(t, "errors") // the first command starts the lease, the console capture and the error hooks
	h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "javascript", "level": "error", "text": "Error: " + askedMarker, "url": pageURL, "line": 3, "column": 4, "timestamp": 1.0,
		"stackTrace": map[string]any{"callFrames": []any{map[string]any{"functionName": "f", "url": pageURL, "lineNumber": 3, "columnNumber": 4}}},
	}})
	waitFor(t, "the error", func() bool { return strings.Contains(h.mustRun(t, "errors"), askedMarker) })
	h.f.Emit("Network.requestWillBeSent", map[string]any{
		"requestId": "r1", "frameId": "f1", "loaderId": "l1", "timestamp": 1.0, "walltime": 1.7e9, "type": "Fetch",
		"request": map[string]any{"url": "https://api.test/" + askedMarker, "method": "GET", "headers": map[string]string{"Accept": "*/*"}},
	})
	h.f.Emit("Network.loadingFinished", map[string]any{"requestId": "r1", "timestamp": 1.2})
	waitFor(t, "the request", func() bool { return strings.Contains(h.mustRun(t, "network", "requests"), askedMarker) })
	for _, tc := range []privateCase{
		{args: []string{"console"}, shown: []string{askedMarker}, hidden: []string{privateMarker}, textDiffers: true},
		{args: []string{"errors"}, shown: []string{askedMarker}, hidden: []string{privateMarker}, textDiffers: true},
		{args: []string{"network", "requests"}, shown: []string{askedMarker}},
		{args: []string{"network", "request", "page-1:r1"}, shown: []string{askedMarker}},
	} {
		checkPrivateRead(t, h.run, tc)
	}
	// har stop needs a recording for each run.
	har := func(args ...string) (int, string, string) {
		h.mustRun(t, "network", "har", "start")
		return h.run(args...)
	}
	checkPrivateRead(t, har, privateCase{args: []string{"network", "har", "stop", "out.har"}, shown: []string{"out.har"}})
}

// Reads that print numbers and the caller's paths: vitals, diff
// screenshot, trace stop and profiler stop.
func TestPrivateReadsOfNumbersAndFiles(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "innerWidth") {
			return map[string]any{"result": map[string]any{"type": "string", "value": `{"width":3,"height":2}`}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p"}}, nil
	})
	f.Handle("Runtime.awaitPromise", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": `{"ttfb":{"value":20},"fcp":{"value":30},"lcp":{"unavailable":"none"},"cls":{"unavailable":"none"},"inp":{"unavailable":"none"},"domContentLoaded":{"value":40},"load":{"value":50}}`}}, nil
	})
	shot := pngOf(t, color.Black)
	f.Handle("Page.snapshotRect", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(shot)}, nil
	})
	ps := &core.PageSession{Caller: spDial(t, f)}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.png"), shot, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) { return spRun(ps, dir, args...) }
	checkPrivateRead(t, run, privateCase{args: []string{"vitals"}, shown: []string{"TTFB"}})
	checkPrivateRead(t, run, privateCase{args: []string{"diff", "screenshot", "--baseline", "base.png"}})

	r := newSPRig(t)
	r.f.Handle("Timeline.stop", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("Timeline.recordingStopped", map[string]any{"endTime": 11.0})
		return nil, nil
	})
	r.f.Handle("ScriptProfiler.startTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("ScriptProfiler.trackingStart", map[string]any{"timestamp": 5.0})
		return nil, nil
	})
	r.f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.f.Emit("ScriptProfiler.trackingComplete", map[string]any{"timestamp": 5.003, "samples": map[string]any{"stackTraces": []any{}}})
		return nil, nil
	})
	for _, rec := range []string{"trace", "profiler"} {
		stop := func(args ...string) (int, string, string) {
			if code, _, errOut := r.run(t, dir, rec, "start"); code != 0 {
				t.Fatalf("%s start: exit %d, %q", rec, code, errOut)
			}
			return r.run(t, dir, args...)
		}
		checkPrivateRead(t, stop, privateCase{args: []string{rec, "stop", rec + ".out"}, shown: []string{rec + ".out"}})
	}
}

// Reads without a page: tab find, session, the skills and get cdp-url,
// whose data names the page by its title.
func TestPrivateReadsWithoutAPage(t *testing.T) {
	m, f := newTabManager(t)
	var reads atomic.Int32
	addAt(f, "Title "+privateMarker, "https://app.test/a", &reads, false)
	stub := &remoteStub{mgrEnv: mgrEnv{m: m}, t: t, session: "s-" + askedMarker}
	run := func(args ...string) (int, string, string) { return runPrivate(t, stub, args...) }
	checkPrivateRead(t, run, privateCase{args: []string{"tab", "find", "https://app.test/a"}, shown: []string{"t1"}})
	checkPrivateRead(t, run, privateCase{args: []string{"session"}, shown: []string{askedMarker}})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // os.UserCacheDir reads XDG_CACHE_HOME first
	checkPrivateRead(t, run, privateCase{args: []string{"skills", "list"}, shown: []string{"iwdp"}})
	checkPrivateRead(t, run, privateCase{args: []string{"skills", "get", "iwdp"}, shown: []string{"iwdp-cli"}})
	checkPrivateRead(t, run, privateCase{args: []string{"skills", "path"}, shown: []string{"skills"}})

	srv := cdp.New(cdp.Options{Manager: m})
	t.Cleanup(func() { srv.Stop() })
	if _, err := srv.Start(0, nil); err != nil { // port 0: a free port, never the daemon's
		t.Fatal(err)
	}
	env := &cdpPageEnv{mgrEnv: mgrEnv{m: m}, srv: srv}
	cdpRun := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunCLIStarted(context.Background(), args, &out, &errOut, nil, func(context.Context, *Invocation) (Env, error) { return env, nil })
		return code, out.String(), errOut.String()
	}
	checkPrivateRead(t, cdpRun, privateCase{args: []string{"get", "cdp-url"}, shown: []string{"ws://127.0.0.1:"}, hidden: []string{privateMarker}})
}

// cdpPageEnv is the daemon's Env with a CDP endpoint, for get cdp-url.
type cdpPageEnv struct {
	mgrEnv
	srv *cdp.Server
}

func (e *cdpPageEnv) CDP() *cdp.Server { return e.srv }

// A read prints only the members of its data that privateReads names: a
// member that a command adds to its data later, at any depth, or a value
// of another kind than its shape names (an object or an array where it
// names a scalar), does not print in private mode until it is named.
// Each sample is the data of the command with such members, which carry
// the marker; every read needs a sample. The command's own text carries
// the marker too: private mode makes the text from the data that it
// prints, so what the JSON does not print, the text does not print.
func TestPrivateReadsPrintOnlyNamedMembers(t *testing.T) {
	samples := map[string]string{
		"snapshot":         `{"snapshot":"- link","tree":[{"role":"link","name":"Go","url":"MARK","href":"MARK","new":{"x":"MARK"},"cursor":{"kind":"clickable","hints":["h",{"new":"MARK"},["MARK"]],"text":"t","new":"MARK"},"children":[{"role":"img","src":"MARK","children":[{"role":"text","name":"n","new":"MARK"}]}]}],"generation":1,"refs":{"e1":{"role":"link","name":"Go","href":"MARK","new":{"x":"MARK"}},"new":"MARK"},"warnings":["w",{"new":"MARK"},["MARK"]],"url":"MARK","title":"MARK","new":"MARK"}`,
		"diff snapshot":    `{"changed":true,"additions":1,"removals":0,"unchanged":1,"diff":"+ x","new":{"x":"MARK"}}`,
		"diff screenshot":  `{"width":1,"height":1,"total":1,"different":0,"ratio":0,"threshold":0.1,"match":true,"new":"MARK"}`,
		"get":              `{"value":"x","warnings":["w",{"new":"MARK"}],"new":"MARK"}`,
		"is":               `{"value":true,"warnings":[{"new":"MARK"}],"new":"MARK"}`,
		"eval":             `{"type":"object","value":{"a":1},"new":"MARK","other":{"x":"MARK"}}`,
		"console":          `{"messages":[{"id":1,"source":"console-api","level":"log","text":"hi","url":"MARK","line":1,"stack":"MARK","repeatCount":2,"new":{"x":"MARK"}}],"dropped":0,"new":"MARK"}`,
		"errors":           `{"errors":[{"id":1,"kind":"error","message":"m","url":"MARK","line":1,"stack":"MARK","new":{"x":"MARK"}}],"new":"MARK"}`,
		"read":             `{"text":"t","warnings":[{"new":"MARK"}],"url":"MARK","new":"MARK"}`,
		"find":             `{"matches":[{"role":"link","name":"Go","url":"MARK","cursor":{"kind":"clickable","hints":["h",{"new":"MARK"}],"new":"MARK"},"children":[{"role":"img","new":"MARK"}],"new":"MARK"}],"warnings":["w",{"new":"MARK"}],"new":"MARK"}`,
		"hit-test":         `{"match":[{"role":"link","url":"MARK","new":"MARK"}],"element":"<a>","box":{"x":1,"y":1,"width":1,"height":1,"new":"MARK"},"warnings":[{"new":"MARK"}],"new":"MARK"}`,
		"vitals":           `{"lcp":{"value":1,"new":"MARK"},"cls":{"unavailable":"none","new":{"x":"MARK"}},"new":"MARK"}`,
		"cookies":          `{"cookies":[{"name":"c","value":"v","domain":"d","path":"/","new":"MARK"}],"valuesShown":true,"new":"MARK"}`,
		"storage":          `{"origin":"MARK","area":"local","items":[{"name":"k","value":"v","new":"MARK"}],"new":"MARK"}`,
		"network requests": `{"requests":[{"id":"r","url":"u","method":"GET","state":"failed","error":"MARK","target":"MARK","frameId":"MARK","loaderId":"MARK","requestHeaders":{"Accept":"*/*","X":{"new":"MARK"}},"responseHeaders":{"Y":["MARK"]},"timing":{"requestTime":1,"new":"MARK"},"new":"MARK"}],"gaps":[{"start":"2026-11-13T00:00:00Z","end":"2026-11-13T00:00:01Z","afterSeq":1,"closed":["r",{"new":"MARK"}],"new":"MARK"}],"started":"2026-11-13T00:00:00Z","new":"MARK"}`,
		"network request":  `{"request":{"id":"r","url":"u","method":"GET","state":"failed","error":"MARK","requestHeaders":{"X":{"new":"MARK"}},"timing":{"requestTime":1,"new":"MARK"},"new":"MARK"},"body":"b","base64Encoded":false,"new":"MARK"}`,
		"screenshot":       `{"path":"p","sha256":"s","labels":[{"n":1,"ref":"e1","role":"r","name":"n","new":"MARK"}],"new":"MARK"}`,
		"network har stop": `{"path":"p","entries":1,"new":{"x":"MARK"}}`,
		"trace stop":       `{"path":"p","records":1,"events":1,"new":"MARK"}`,
		"profiler stop":    `{"path":"p","samples":1,"nodes":1,"new":"MARK"}`,
		"get cdp-url":      `{"ws":"w","http":"h","scope":"page","page":"MARK","new":"MARK"}`,
		"tab find":         `{"tab":"t1","new":"MARK"}`,
		"session":          `{"session":"s","new":"MARK"}`,
		"skills list":      `[{"name":"n","description":"d","new":"MARK"}]`,
		"skills get":       `[{"name":"n","description":"d","content":"c","new":{"x":"MARK"}}]`,
		"skills path":      `{"path":"p","new":"MARK"}`,
	}
	for name := range privateReads {
		sample, ok := samples[name]
		if !ok {
			t.Errorf("private read %q has no sample here", name)
			continue
		}
		checkPrivateSample(t, name, sample)
	}
	// The summaries make their values from named members too.
	for name, sample := range map[string]string{
		"tab list":     `{"tabs":[{"id":"t1","debuggable":true,"current":true,"title":"MARK","url":"MARK","new":{"x":"MARK"}}],"hidden":0,"new":"MARK"}`,
		"tab":          `{"tabs":[{"id":"t1","debuggable":false,"current":false,"key":"MARK","new":"MARK"}],"hidden":1,"new":"MARK"}`,
		"session info": `{"session":"MARK","pages":[{"key":"MARK","title":"MARK","lease":"active","epoch":1,"state":"attached","new":{"x":"MARK"}}],"new":"MARK"}`,
		"status":       `{"running":true,"devices":1,"log":"MARK","new":{"x":"MARK"}}`,
	} {
		checkPrivateSample(t, name, sample)
	}
}

// checkPrivateSample runs privateResult for the command name on sample,
// whose members that private mode does not print carry MARK, with a
// command text that carries the marker too.
func checkPrivateSample(t *testing.T, name, sample string) {
	t.Helper()
	c, _ := Lookup(strings.Fields(name))
	data := json.RawMessage(strings.ReplaceAll(sample, "MARK", privateMarker))
	got, _ := privateResult(c, Result{Text: "text of " + name + " " + privateMarker, Data: data})
	out, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if json.Unmarshal(out, &v) == nil && v["command"] != nil {
		t.Errorf("%s printed only its status for %s; the sample is not a result of it", name, data)
	}
	if strings.Contains(got.Text+string(out), privateMarker) {
		t.Errorf("%s printed a member that private mode does not name:\n%s\n%s", name, got.Text, out)
	}
}

// Every shape of privateReads, walked to every depth: a member that is not
// content never prints an object or an array, an object never prints a
// member that it does not name, and the places where free content prints
// and where every member of an object prints ("*") are exactly the ones
// listed here, so that a new one is a decision that a reader sees.
func TestPrivateShapesPrintObjectsOnlyByName(t *testing.T) {
	wantContent := []string{
		"console: messages[].text",
		"cookies: cookies[].value",
		"diff snapshot: diff",
		"errors: errors[].message",
		"eval: description",
		"eval: value",
		"get: value",
		"network request: body",
		"network request: request.postData",
		"network requests: requests[].postData",
		"read: text",
		"skills get: [].content",
		"snapshot: snapshot",
		"storage: items[].value",
		"storage: value",
	}
	wantEvery := []string{
		"network request: request.requestHeaders",
		"network request: request.responseHeaders",
		"network requests: requests[].requestHeaders",
		"network requests: requests[].responseHeaders",
		"snapshot: refs",
	}
	mark := strconv.Quote(privateMarker)
	var gotContent, gotEvery []string
	for name, read := range privateReads {
		seen := map[*shape]bool{}
		// prints reports whether the value v at path prints.
		prints := func(path []string, v string) bool {
			for i := len(path) - 1; i >= 0; i-- {
				switch path[i] {
				case "[]":
					v = "[" + v + "]"
				case "*":
					v = `{"k":` + v + "}"
				default:
					v = "{" + strconv.Quote(path[i]) + ":" + v + "}"
				}
			}
			got, _ := project(json.RawMessage(v), read.data)
			return strings.Contains(string(got), privateMarker)
		}
		var walk func(path []string, s *shape)
		walk = func(path []string, s *shape) {
			where := name + ": " + strings.ReplaceAll(strings.TrimPrefix(strings.Join(path, "."), "."), ".[]", "[]")
			if s == nil {
				t.Errorf("%s has no shape", where)
				return
			}
			switch s.kind {
			case contentKind:
				gotContent = append(gotContent, where)
			case scalarKind:
				for _, v := range []string{`{"x":` + mark + "}", "[" + mark + "]"} {
					if prints(path, v) {
						t.Errorf("%s is not content, but prints %s", where, v)
					}
				}
			case listKind:
				walk(append(slices.Clone(path), "[]"), s.elem)
			case objectKind:
				if _, every := s.members["*"]; every {
					gotEvery = append(gotEvery, where)
				} else if prints(path, `{"unknown-member":`+mark+"}") {
					t.Errorf("%s prints a member that it does not name", where)
				}
				if seen[s] {
					return
				}
				seen[s] = true
				for k, sub := range s.members {
					walk(append(slices.Clone(path), k), sub)
				}
			default:
				t.Errorf("%s has no kind", where)
			}
		}
		walk(nil, read.data)
	}
	slices.Sort(gotContent)
	slices.Sort(gotEvery)
	if !slices.Equal(gotContent, wantContent) {
		t.Errorf("free content prints at\n%s\nwant\n%s", strings.Join(gotContent, "\n"), strings.Join(wantContent, "\n"))
	}
	if !slices.Equal(gotEvery, wantEvery) {
		t.Errorf("every member prints at\n%s\nwant\n%s", strings.Join(gotEvery, "\n"), strings.Join(wantEvery, "\n"))
	}
}

// A result whose named members have another type than the command gives
// them (a number where it gives text) is not a result of the read: private
// mode prints its status line, no text and no data. Every read needs a
// case.
func TestPrivateReadOfAnotherShapePrintsItsStatus(t *testing.T) {
	cases := map[string][]string{
		"snapshot":         {`{"snapshot":1}`},
		"diff snapshot":    {`{"diff":1}`},
		"diff screenshot":  {`{"total":"x"}`},
		"get":              {`{"value":"x","warnings":[1]}`},
		"is":               {`{"value":"x"}`, `{"warnings":[]}`},
		"eval":             {`{"type":1}`},
		"console":          {`{"messages":[],"dropped":"x"}`, `{"messages":[]}`},
		"errors":           {`{"errors":[{"id":"x"}]}`, `{"other":[]}`},
		"read":             {`{"text":1}`},
		"find":             {`{"matches":[{"role":1}]}`},
		"hit-test":         {`{"element":1}`},
		"vitals":           {`{"lcp":{"value":"x"}}`},
		"cookies":          {`{"cookies":[{"name":1}]}`},
		"storage":          {`{"area":"local","items":[{"name":1}]}`, `{"area":1}`, `{"area":"local","found":true,"key":1}`},
		"network requests": {`{"requests":[{"seq":"x"}]}`},
		"network request":  {`{"request":{"seq":"x"}}`, `{"request":{"id":"r"},"body":"%%","base64Encoded":true}`, `{"body":"b"}`},
		"screenshot":       {`{"path":1}`},
		"network har stop": {`{"entries":"x"}`},
		"trace stop":       {`{"records":"x"}`},
		"profiler stop":    {`{"samples":"x"}`},
		"get cdp-url":      {`{"ws":1}`},
		"tab find":         {`{"tab":1}`},
		"session":          {`{"session":1}`},
		"skills list":      {`[{"name":1}]`, `{"name":"n"}`},
		"skills get":       {`[{"content":1}]`},
		"skills path":      {`{"path":1}`},
	}
	for name := range privateReads {
		if len(cases[name]) == 0 {
			t.Errorf("private read %q has no case here", name)
		}
		c, _ := Lookup(strings.Fields(name))
		for _, data := range cases[name] {
			got, _ := privateResult(c, Result{Text: "text of " + name, Data: json.RawMessage(data)})
			out, err := json.Marshal(got.Data)
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"command":` + strconv.Quote(name) + `,"exit":0}`; got.Text != "" || string(out) != want {
				t.Errorf("%s of %s: text %q, data %s; want no text and %s", name, data, got.Text, out, want)
			}
		}
	}
}

// A member that is added later inside a nested object of fixed shape (the
// box of hit-test, the timing of a request) does not print (final review
// of 2026-11-13, F39).
func TestPrivateNestedAddedMembers(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"hit-test", `{"box":{"x":1,"y":2,"width":3,"height":4,"newOrigin":"MARK"},"match":[]}`},
		{"network requests", `{"requests":[{"id":"r1","timing":{"requestTime":1,"newOrigin":"MARK"}}],"gaps":[],"dropped":0}`},
		{"network request", `{"request":{"id":"r1","timing":{"requestTime":1,"newOrigin":"MARK"}}}`},
	} {
		c, _ := Lookup(strings.Fields(tc.name))
		out, _ := privateResult(c, Result{Text: "safe", Data: json.RawMessage(strings.ReplaceAll(tc.data, "MARK", privateMarker))})
		b, err := json.Marshal(out.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.Text+string(b), privateMarker) {
			t.Errorf("%s printed a nested member that private mode does not name: %s\n%s", tc.name, out.Text, b)
		}
	}
}

// WebKit's text of a failed request (Network.loadingFailed errorText) can
// name what the caller did not ask for: the network reads print that the
// request failed, never the text, in the text and in the JSON (final review
// of 2026-11-13, F38). The request URL, which the caller asked for, prints.
func TestPrivateNetworkReadsPrintNoErrorText(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "network", "requests")
	h.f.Emit("Network.requestWillBeSent", map[string]any{
		"requestId": "r1", "frameId": "f1", "loaderId": "l1", "timestamp": 1.0, "walltime": 1.7e9, "type": "Fetch",
		"request": map[string]any{"url": "https://api.test/" + askedMarker, "method": "GET", "headers": map[string]string{}},
	})
	h.f.Emit("Network.loadingFailed", map[string]any{"requestId": "r1", "timestamp": 1.2, "errorText": "failure " + privateMarker})
	waitFor(t, "the failed request", func() bool { return strings.Contains(h.mustRun(t, "network", "requests"), privateMarker) })
	for _, args := range [][]string{{"network", "requests"}, {"--json", "network", "requests"}, {"network", "request", "page-1:r1"}, {"--json", "network", "request", "page-1:r1"}} {
		args = append([]string{"--private"}, args...)
		code, out, errOut := h.run(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d, %s", args, code, errOut)
		}
		if strings.Contains(out, privateMarker) {
			t.Errorf("%v printed the error text of the request: %s", args, out)
		}
		if !strings.Contains(out, askedMarker) || !strings.Contains(out, devtools.StateFailed) {
			t.Errorf("%v: %s; want the URL and the state %q", args, out, devtools.StateFailed)
		}
	}
}

// The content boundary of private data names no origin, also when the
// data has a member from which the boundary would take one: the boundary
// does not depend on what the projections of privateReads keep.
func TestPrivateDataNamesNoOrigin(t *testing.T) {
	data := map[string]string{"url": "https://" + strings.ToLower(privateMarker) + ".test/a"}
	if got := resultOrigin(data); got != "https://"+strings.ToLower(privateMarker)+".test" {
		t.Fatalf("the boundary of ordinary data names %q; want the origin of its url", got)
	}
	if got := newBoundary(privateData{data}).origin; got != unknownOrigin {
		t.Fatalf("the boundary of private data names %q; want %q", got, unknownOrigin)
	}
}

// The texts that private mode makes from data say what the commands' own
// texts say (console.go, errors.go, storage_cmd.go, get.go,
// get_cdp_url.go, har.go, network_cmds.go), without the source's URL and
// stack, without the origin and without the error text of a request. The
// command's own text only chooses among texts made from the data (the
// form of get cdp-url, the note of har stop and of network requests).
func TestPrivateTextsMadeFromData(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(strings.ReplaceAll(s, "MARK", privateMarker)) }
	for _, tc := range []struct {
		name string
		data json.RawMessage
		cmd  string // the command's own text
		want string
	}{
		{
			"console", raw(`{"messages":[{"id":4,"level":"log","text":"hi","url":"MARK","stack":"MARK","repeatCount":3}],"dropped":2}`), "",
			fmt.Sprintf("(2 older messages were dropped; the buffer keeps the last %d)\n#4 [log] hi x3", devtools.DefaultMaxMessages),
		},
		{"console", raw(`{"messages":[],"dropped":0}`), "", "No console messages."},
		{"errors", raw(`{"errors":[]}`), "", "No errors."},
		{"errors", raw(`{"errors":[{"id":2,"kind":"unhandledrejection","message":"boom","url":"MARK","stack":"MARK"}]}`), "", "#2 unhandledrejection: boom"},
		{"storage", raw(`{"origin":"MARK","area":"local","key":"password","found":true,"value":"` + devtools.Masked + `"}`), "", devtools.Masked + "\n" + maskedHint},
		{"storage", raw(`{"origin":"MARK","area":"session","items":[{"name":"token","value":"` + devtools.Masked + `"},{"name":"a","value":"b"}]}`), "", "token=" + devtools.Masked + "\na=b\n" + maskedHint},
		{"storage", raw(`{"origin":"MARK","area":"session","items":null}`), "", "sessionStorage is empty."},
		// get prints the box as JSON, the styles as lines, a count as a
		// number and a missing attribute as nothing.
		{"get", raw(`{"value":{"x":1,"y":2,"width":3,"height":4},"warnings":[]}`), `{"x":1,"y":2,"width":3,"height":4}`, `{"x":1,"y":2,"width":3,"height":4}`},
		{"get", raw(`{"value":{"display":"block","color":"red"},"warnings":["w"]}`), "display: block\ncolor: red\nwarning: w", "color: red\ndisplay: block\nwarning: w"},
		{"get", raw(`{"value":3}`), "3", "3"},
		{"get", raw(`{"value":null}`), "", ""},
		{"get cdp-url", raw(`{"ws":"ws://h/t","http":"http://h/t","scope":"page"}`), "http://h/t", "http://h/t"},
		{"get cdp-url", raw(`{"ws":"ws://h/t","http":"http://h/t","scope":"page"}`), "MARK", "ws://h/t"},
		{"network har stop", raw(`{"path":"a.har","entries":2}`), "Wrote 2 entries to a.har (secret values kept).", "Wrote 2 entries to a.har (secret values kept)."},
		{"network har stop", raw(`{"path":"a.har","entries":2}`), "MARK", "Wrote 2 entries to a.har (secret values masked; --show-secrets keeps them)."},
		{
			"network requests",
			raw(`{"requests":[{"id":"page-1:r1","seq":1,"url":"https://a.test/x","method":"GET","type":"Fetch","state":"failed","error":"MARK"}],"started":"2026-11-13T00:00:00Z"}`),
			netlogStartedNote + "\npage-1:r1 GET failed (MARK) Fetch https://a.test/x",
			netlogStartedNote + "\npage-1:r1 GET failed Fetch https://a.test/x",
		},
		{
			"network requests",
			raw(`{"requests":[{"id":"page-1:r1","seq":1,"url":"https://a.test/x","method":"GET","type":"Fetch","state":"failed","error":"MARK"}],"started":"2026-11-13T00:00:00Z"}`),
			"page-1:r1 GET failed (MARK) Fetch https://a.test/x",
			"page-1:r1 GET failed Fetch https://a.test/x",
		},
		{
			"network requests",
			raw(`{"requests":[{"id":"page-1:r2","seq":2,"url":"https://a.test/y","method":"GET","type":"Fetch","status":404,"state":"finished"}],"gaps":[{"start":"2026-11-13T10:00:00.5Z","end":"2026-11-13T10:00:01Z","afterSeq":1,"closed":["page-1:r1"]}],"dropped":3,"started":"2026-11-13T00:00:00Z"}`),
			"",
			fmt.Sprintf("(3 older requests were dropped; the log keeps the last %d)\n", devtools.DefaultMaxEntries) +
				"-- network reset 10:00:00.500 to 10:00:01.000: 1 request(s) in flight closed as " + devtools.StateCaptureReset + "; earlier response bodies are gone\n" +
				"page-1:r2 GET 404 Fetch https://a.test/y",
		},
		{
			"network request",
			raw(`{"request":{"id":"page-1:r2","url":"https://a.test/y","method":"POST","type":"Fetch","mimeType":"text/plain","status":200,"statusText":"OK","state":"finished","postData":"a=1","bodyGone":"gone"},"body":"//4=","base64Encoded":true}`),
			"",
			"POST https://a.test/y\nStatus: 200 OK (finished)\nType: Fetch  MIME: text/plain\nRequest body:\na=1\nNote: gone\nBody: 2 bytes of binary data (use --json for base64)",
		},
		{"cookies", raw(`{"cookies":[],"valuesShown":false}`), "", "No cookies."},
		{
			"network request",
			raw(`{"request":{"id":"page-1:r1","url":"https://a.test/x","method":"POST","type":"Fetch","state":"failed","error":"MARK","requestHeaders":{"A":"b"}},"body":"aGk=","base64Encoded":true}`),
			"MARK",
			"POST https://a.test/x\nStatus: failed\nType: Fetch  MIME: \nRequest headers:\n  A: b\nBody:\nhi",
		},
	} {
		text, value, ok := privateReads[tc.name].show(tc.cmd, tc.data)
		if !ok || text != tc.want || strings.Contains(text+string(value), privateMarker) {
			t.Errorf("%s of %s: %q, %s, ok %v; want %q and no private value", tc.name, tc.data, text, value, ok, tc.want)
		}
	}
	// Set and clear are actions: the status line only.
	for _, data := range []string{`{"origin":"o","area":"local","key":"k"}`, `{"origin":"o","area":"local","cleared":1}`} {
		if _, _, ok := privateReads["storage"].show("", json.RawMessage(data)); ok {
			t.Errorf("storage result %s prints as a read; want its status line", data)
		}
	}
}

// project prints a value only by its shape: a scalar never as an object or
// an array, an object with the members that the shape names, an array by
// the element shape, content as it is, null as null; a value of another
// kind does not print, and an incomplete shape prints nothing.
func TestPrivateProjectByKind(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(strings.ReplaceAll(s, "MARK", privateMarker)) }
	s := object(fields{
		"a": object(fields{"x": scalar}), "b": object(fields{"x": scalar}), "c": listOf(object(fields{"d": scalar})),
		"s1": scalar, "s2": scalar, "s3": scalar, "l": listOf(scalar), "o": object(fields{"x": scalar}), "free": content,
		"zero": {}, "nolist": {kind: listKind}, "nilmember": object(fields{"x": nil}),
	})
	got, ok := project(raw(`{"a":"MARK","b":null,"c":[{"d":1,"e":"MARK"},"MARK"],"s1":{"x":"MARK"},"s2":["MARK"],"s3":"v","l":{"x":"MARK"},"o":["MARK"],"free":{"y":[1]},"zero":"MARK","nolist":["MARK"],"nilmember":{"x":"MARK"},"new":"MARK"}`), s)
	if want := `{"b":null,"c":[{"d":1}],"free":{"y":[1]},"nilmember":{},"nolist":[],"s3":"v"}`; !ok || string(got) != want {
		t.Errorf("project = %s, %v; want %s", got, ok, want)
	}
}
