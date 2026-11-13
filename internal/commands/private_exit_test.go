package commands

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// Tests of the end of a private run (Outcome): reviewer findings F33 and
// F35 of the checker-trim closure review. Every exit path of private mode
// ends with exactly one status line on stderr, "<command> exit 0" for a
// success; stdout holds what the caller asked for, and help is asked for.
// The same lines without private mode print what they printed before: the
// help on stdout and nothing on stderr.

// runHelp runs args through RunCLIStarted; newEnv fails the test, because
// help never connects.
func runHelp(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunCLIStarted(context.Background(), args, &out, &errOut, nil, func(context.Context, *Invocation) (Env, error) {
		t.Fatalf("%v: help connected", args)
		return nil, nil
	})
	return code, out.String(), errOut.String()
}

// F35 (reviewer's repro TestReviewPrivateHelpStatus): a private help
// request succeeded without its status line.
func TestPrivateHelpEndsWithOneStatusLine(t *testing.T) {
	click, _ := Lookup([]string{"click"})
	group, _ := GroupHelp([]string{"network"})
	for _, tc := range []struct {
		private, plain []string // the line with and without the mode
		env            string   // IWDP_PRIVATE of the private line
		stdout, status string
	}{
		{[]string{"--private", "click", "--help"}, []string{"click", "--help"}, "", Help(click), "click exit 0\n"},
		{[]string{"click", "-h"}, []string{"click", "-h"}, "1", Help(click), "click exit 0\n"},
		{[]string{"--private", "network", "--help"}, []string{"network", "--help"}, "", group, "network exit 0\n"},
		{[]string{"network", "--help"}, []string{"network", "--help"}, "1", group, "network exit 0\n"},
		{[]string{"--help"}, []string{"--help"}, "1", Usage(), "iwdp-cli exit 0\n"},
	} {
		t.Setenv(privateEnv, "")
		code, out, errOut := runHelp(t, tc.plain...)
		if code != ExitOK || out != tc.stdout || errOut != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 0, the help and no stderr", tc.plain, code, out, errOut)
		}
		t.Setenv(privateEnv, tc.env)
		code, out, errOut = runHelp(t, tc.private...)
		if code != ExitOK || out != tc.stdout || errOut != tc.status {
			t.Errorf("%v with %s=%q: exit %d, stdout %q, stderr %q; want exit 0, the help and the status %q", tc.private, privateEnv, tc.env, code, out, errOut, tc.status)
		}
	}
}

// The status line of a group's help names only the group's words, also
// when the line has other arguments.
func TestPrivateGroupHelpStatusNamesTheGroup(t *testing.T) {
	code, out, errOut := runHelp(t, "--private", "--session", "s-"+privateMarker, "network", "har", "--help")
	if code != ExitOK || !strings.HasPrefix(out, "Usage: iwdp-cli network har <command>") || errOut != "network har exit 0\n" {
		t.Errorf("exit %d, stdout %q, stderr %q; want the group's help and \"network har exit 0\"", code, out, errOut)
	}
}

// The decision of the last stderr line (Outcome.Line): in private mode it
// is one line made of a registry name and the exit code, whatever the
// message; without the mode it is the message.
func TestOutcomeLineProperty(t *testing.T) {
	cmds := All()
	prop := func(pick uint, code int, message string, private bool) bool {
		o := Outcome{Command: cmds[pick%uint(len(cmds))].Name, Code: code, Private: private, Message: privateMarker + message}
		line := o.Line()
		if !private {
			return line == o.Message
		}
		head := o.Command + " exit " + strconv.Itoa(code)
		if code == ExitOK {
			return line == head
		}
		return strings.HasPrefix(line, head+": ") && !strings.Contains(line, "\n") && !strings.Contains(line, privateMarker)
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Error(err)
	}
	// A line without a command names the program.
	if got := (Outcome{Private: true}).Line(); got != "iwdp-cli exit 0" {
		t.Errorf("private success without a command: %q", got)
	}
}

// F40 (reviewer's TestReviewPrivateHelpWriteFailure): a help text that
// cannot be written on stdout ended the run with exit 0. It ends with
// exit 1: in private mode with the status line of that exit, without it
// with the error line of the write, as a result that cannot be written
// has ("Error: writing the result: ...").
func TestHelpThatCannotBeWrittenExits1(t *testing.T) {
	for _, tc := range []struct {
		args    []string // without private mode
		flag    bool     // private mode by --private, else by the variable
		command string   // of the private status line
	}{
		{[]string{"click", "--help"}, true, "click"},
		{[]string{"network", "--help"}, true, "network"},
		{[]string{"network", "--help"}, false, "network"},
		{[]string{"--help"}, false, ""}, // "--private --help" is a usage error
	} {
		for _, private := range []bool{false, true} {
			args := tc.args
			t.Setenv(privateEnv, "")
			switch {
			case private && tc.flag:
				args = append([]string{"--private"}, args...)
			case private:
				t.Setenv(privateEnv, "1")
			}
			o := RunCLIOutcome(context.Background(), args, failingWriter{}, nil, func(context.Context, *Invocation) (Env, error) {
				t.Fatalf("%v: help connected", args)
				return nil, nil
			})
			want := "Error: writing the help: write refused: " + privateMarker
			if private {
				want = privateErrorLine(tc.command, ExitError)
			}
			if o.Code != ExitError || o.Line() != want {
				t.Errorf("%q (private %v): exit %d, line %q; want exit %d and %q", args, private, o.Code, o.Line(), ExitError, want)
			}
		}
	}
}
