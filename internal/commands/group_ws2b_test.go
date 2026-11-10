package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// WS2b-1b-b item 2 at the parser: command words that start registry
// commands but name none are a group. With --help the invocation carries
// the group's list and RunCLIStarted prints it with exit 0; without, the
// usage error (exit 2) carries the list. Other words keep the plain
// unknown-command error. The commands "zzgroup alpha" and "zzgroup beta"
// below make "zzgroup" such a group.
func TestParseListsTheCommandsOfAGroup(t *testing.T) {
	run := func(context.Context, Env, Input) (Result, error) {
		t.Error("a group's list ran a command")
		return Result{}, nil
	}
	group := []*Command{
		{Name: "zzgroup alpha", Summary: "First command of the group.", Run: run},
		{Name: "zzgroup beta", Summary: "Second command of the group.", Run: run},
	}
	for _, c := range group {
		Register(c)
	}
	t.Cleanup(func() {
		for _, c := range group {
			Unregister(c)
		}
	})
	wantList := func(what, s string) {
		t.Helper()
		for _, n := range []string{"zzgroup alpha", "zzgroup beta"} {
			c, _ := Lookup(strings.Fields(n))
			if !strings.Contains(s, n+" ") || !strings.Contains(s, c.Summary) {
				t.Errorf("%s: %q lacks %q with its summary", what, s, n)
			}
		}
	}
	inv, err := Parse([]string{"zzgroup", "--help"}, noEnv)
	if err != nil || !inv.Help || inv.Cmd != nil {
		t.Fatalf("zzgroup --help: %+v, %v; want a help invocation without a command", inv, err)
	}
	wantList("Parse zzgroup --help", inv.Group)

	code, out, errOut, env := runCLI("zzgroup", "-h")
	if code != ExitOK || errOut != "" || env != 0 {
		t.Errorf("zzgroup -h: exit %d, stderr %q, env calls %d; want exit 0 without a connection", code, errOut, env)
	}
	wantList("zzgroup -h", out)

	_, err = Parse([]string{"zzgroup", "--bogus"}, noEnv)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), `"zzgroup" is a group of commands`) {
		t.Fatalf("zzgroup --bogus: %v; want the group's usage error", err)
	}
	wantList("Parse zzgroup --bogus", err.Error())
	code, out, errOut, env = runCLI("zzgroup")
	if code != ExitUsage || out != "" || env != 0 {
		t.Errorf("zzgroup: exit %d, stdout %q, env calls %d; want exit 2 without a connection", code, out, env)
	}
	wantList("zzgroup", errOut)

	for _, args := range [][]string{{"zz-no-group"}, {"zz-no-group", "--help"}, {"zzgroup", "zz-none"}, {"zzgroup", "zz-none", "-h"}} {
		_, err := Parse(args, noEnv)
		if !errors.As(err, &ue) || !strings.HasPrefix(err.Error(), "unknown command ") || strings.Contains(err.Error(), "zzgroup alpha") {
			t.Errorf("%v: %v; want the plain unknown-command error", args, err)
		}
	}
	if _, ok := GroupHelp([]string{"zz-no-group"}); ok {
		t.Error("GroupHelp(zz-no-group) reports a group")
	}
}
