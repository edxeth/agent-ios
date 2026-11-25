package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nnemirovsky/agent-ios/internal/commands"
)

// referencePath is the checked-in reference, relative to this package.
var referencePath = filepath.Join("..", "..", "..", "docs", "commands.md")

func readReference(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("read the command reference: %v (run make docs)", err)
	}
	return string(b)
}

// TestReferenceListsEveryRegistryCommand is design 7.9 acceptance 1: every
// registry command appears in the checked-in reference.
func TestReferenceListsEveryRegistryCommand(t *testing.T) {
	ref := readReference(t)
	all := commands.All()
	if len(all) == 0 {
		t.Fatal("the registry is empty; refgen must import the packages that register commands")
	}
	for _, c := range all {
		if !strings.Contains(ref, "\n### `"+c.Name+"`\n") {
			t.Errorf("docs/commands.md has no section for registry command %q; run make docs", c.Name)
		}
	}
}

// TestReferenceIsCurrent fails when the checked-in reference differs from
// a fresh render of the registry, for example after a changed flag or
// summary.
func TestReferenceIsCurrent(t *testing.T) {
	ref := readReference(t)
	want := render(commands.All())
	if ref == want {
		return
	}
	got, exp := strings.Split(ref, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(got) || i < len(exp); i++ {
		var g, e string
		if i < len(got) {
			g = got[i]
		}
		if i < len(exp) {
			e = exp[i]
		}
		if g != e {
			t.Fatalf("docs/commands.md is stale; run make docs. First difference at line %d:\n file:   %q\n render: %q", i+1, g, e)
		}
	}
}

// TestRenderCommandSection pins the Markdown of one entry. The expected
// text follows CommonMark and GitHub rules: backslash escapes keep angle
// brackets and stars literal, a pipe inside a table cell is escaped, and
// the heading id of "`net work`" is "net-work".
func TestRenderCommandSection(t *testing.T) {
	c := &commands.Command{
		Name:    "net work",
		Aliases: []string{"nw"},
		Summary: "Route <url> with * and | in it.",
		Page:    commands.LeasedPage,
		Args: []commands.Arg{
			{Name: "target", Help: "a ref | selector", Required: true},
			{Name: "rest", Help: "more", Variadic: true},
		},
		Flags: []commands.Flag{
			{Name: "mode", Type: commands.FlagEnum, Enum: []string{"wip", "js"}, Default: "wip", Help: "how"},
			{Name: "delay", Short: "d", Type: commands.FlagDuration, Help: "wait"},
			{Name: "pause", Type: commands.FlagDuration, NeedsUnit: true, Help: "pause"},
			{Name: "quiet", Type: commands.FlagBool, Help: "less output"},
		},
		Run: func(context.Context, commands.Env, commands.Input) (commands.Result, error) {
			return commands.Result{}, nil
		},
	}
	local := &commands.Command{Name: "zz local", Summary: "Local thing.", Local: true, Run: c.Run}
	out := render([]*commands.Command{c, local})
	for _, want := range []string{
		"| [`net work`](#net-work) | Route \\<url\\> with \\* and \\| in it. |\n",
		"| [`zz local`](#zz-local) | Local thing. |\n",
		"\n### `net work`\n\n```\nagent-ios net work <target> [<rest>...] [flags]\n```\n\nRoute \\<url\\> with \\* and | in it.\n\n",
		"in it.\n\n- Aliases: `nw`\n- Page: leases the session's page\n",
		"| `<target>` | yes | a ref \\| selector |\n| `<rest>...` | no | more |\n",
		"| `--mode` | `wip`, `js` | how (default `wip`) |\n",
		"| `-d`, `--delay` | duration (40s, 1m30s) or seconds (40) | wait |\n",
		"| `--pause` | duration with a unit (50ms, 1s) | pause |\n",
		"| `--quiet` | none | less output |\n",
		"\n### `zz local`\n\n```\nagent-ios zz local\n```\n\nLocal thing.\n\n- Page: none; runs in the client process, not in the daemon\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n--- render ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "MCP") {
		t.Errorf("the reference must not name the removed MCP server:\n%s", out)
	}
	if strings.Contains(out, "Default deadline") {
		t.Errorf("a command without its own timeout must not show a default deadline:\n%s", out)
	}
}
