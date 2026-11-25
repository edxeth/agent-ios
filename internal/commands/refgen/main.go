// refgen writes docs/commands.md, the command reference, from the command
// registry (internal/commands): every registry command of agent-ios with its
// usage, arguments and flags. It registers the
// same commands as the binaries: those of internal/commands and the daemon
// commands of internal/daemon.
//
//	go run ./internal/commands/refgen docs/commands.md   (make docs)
//
// The tests in this package fail when a registry command is missing from
// the checked-in file or when the file differs from a fresh render.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/commands"
	// The daemon lifecycle commands register in internal/daemon, because
	// that package imports internal/commands.
	_ "github.com/nnemirovsky/agent-ios/internal/daemon"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/commands/refgen <output file, for example docs/commands.md>")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[1], []byte(render(commands.All())), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "refgen: write %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// header is the fixed start of the page.
var header = strings.Join([]string{
	"# Command reference",
	"",
	"<!-- Generated from the command registry by \"make docs\" (go run ./internal/commands/refgen docs/commands.md). Do not edit by hand. -->",
	"",
	"This page lists every registry command of `agent-ios`: the commands that run in the agent-ios daemon and share sessions, leases and refs. For each command it gives the usage, the arguments and the flags.",
	"",
	"- `agent-ios` runs only these commands. [The migration notes](migration.md) map each removed command of earlier versions (for example `pages`, `set-breakpoint`, `get-matched-styles`) to its registry command, or say that it was removed.",
	"- `agent-ios <command> --help` prints the same usage and never connects to a page.",
	"",
	"",
}, "\n")

// render returns the reference page for cmds (sorted by name, as
// commands.All returns them).
func render(cmds []*commands.Command) string {
	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, "Global flags (any position): %s.\n\n", strings.Join(globalFlags(), ", "))

	b.WriteString("## Index\n\n| Command | Summary |\n|---|---|\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "| [%s](#%s) | %s |\n", code(c.Name), anchor(c.Name), cell(c.Summary))
	}

	b.WriteString("\n## Commands\n")
	for _, c := range cmds {
		b.WriteString(section(c))
	}
	return b.String()
}

// section is the reference entry of one command.
func section(c *commands.Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s\n\n", code(c.Name))
	fmt.Fprintf(&b, "```\n%s\n```\n\n", synopsis(c))
	fmt.Fprintf(&b, "%s\n\n", text(c.Summary))
	if len(c.Aliases) > 0 {
		quoted := make([]string, len(c.Aliases))
		for i, a := range c.Aliases {
			quoted[i] = code(a)
		}
		fmt.Fprintf(&b, "- Aliases: %s\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "- Page: %s\n", pageNeed(c))
	if c.Timeout > 0 {
		fmt.Fprintf(&b, "- Default deadline: %s (`--timeout` overrides it)\n", c.Timeout)
	}
	if len(c.Args) > 0 {
		b.WriteString("\n| Argument | Required | Description |\n|---|---|---|\n")
		for _, a := range c.Args {
			name := "<" + a.Name + ">"
			if a.Variadic {
				name += "..."
			}
			req := "no"
			if a.Required {
				req = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", code(name), req, cell(a.Help))
		}
	}
	if len(c.Flags) > 0 {
		b.WriteString("\n| Flag | Value | Description |\n|---|---|---|\n")
		for _, f := range c.Flags {
			name := code("--" + f.Name)
			if f.Short != "" {
				name = code("-"+f.Short) + ", " + name
			}
			help := cell(f.Help)
			if f.Default != "" {
				help += " (default " + code(f.Default) + ")"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", name, flagValue(f), help)
		}
	}
	return b.String()
}

// synopsis is the usage line of the command's --help without "Usage: ",
// so that the reference and the help text cannot disagree.
func synopsis(c *commands.Command) string {
	first, _, _ := strings.Cut(commands.Help(c), "\n")
	return strings.TrimPrefix(first, "Usage: ")
}

// globalFlags are the global flags that every command's --help lists.
func globalFlags() []string {
	_, list, ok := strings.Cut(commands.Help(&commands.Command{Name: "x", Summary: "x"}), "Global flags:")
	if !ok {
		panic("refgen: commands.Help has no \"Global flags:\" line")
	}
	var out []string
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, code(f))
		}
	}
	return out
}

func pageNeed(c *commands.Command) string {
	switch {
	case c.Local:
		return "none; runs in the client process, not in the daemon"
	case c.Page == commands.LeasedPage:
		return "leases the session's page"
	case c.Page == commands.AnyPage:
		return "uses the session's page"
	}
	return "none"
}

func flagValue(f commands.Flag) string {
	switch f.Type {
	case commands.FlagBool:
		return "none"
	case commands.FlagInt:
		return "integer"
	case commands.FlagDuration:
		if f.NeedsUnit {
			return "duration with a unit (50ms, 1s)"
		}
		return "duration (40s, 1m30s) or seconds (40)"
	case commands.FlagPath:
		return "file path"
	case commands.FlagEnum:
		vals := make([]string, len(f.Enum))
		for i, v := range f.Enum {
			vals[i] = code(v)
		}
		return strings.Join(vals, ", ")
	}
	return "text"
}

// anchor is the GitHub heading id of a "### `name`" heading: lower
// case, spaces as hyphens, other characters than letters, digits, hyphens
// and underscores removed.
func anchor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// code is s as a Markdown code span. Registry names, flags and enum values
// contain no backticks.
func code(s string) string { return "`" + s + "`" }

// mdEscaper escapes the Markdown characters of help texts that would
// change the rendering: backslashes, backticks, emphasis stars and angle
// brackets (which would start HTML tags).
var mdEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "<", `\<`, ">", `\>`)

// text is help text as Markdown paragraph text.
func text(s string) string { return mdEscaper.Replace(s) }

// cell is help text inside a table cell: pipes are escaped too, and line
// breaks become spaces.
func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text(s), "|", `\|`), "\n", " ")
}
