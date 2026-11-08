package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-mcp/internal/skills"
)

// listDescriptionRunes is where "skills list" cuts a description, as
// agent-browser's list does; "skills get" prints the whole skill.
const listDescriptionRunes = 72

// skillsList and skillsAll read the embedded skills; tests replace them to
// reach the error paths, which an intact binary never takes.
var (
	skillsList = skills.List
	skillsAll  = skills.All
)

// The skills commands (design docs/design/agentic-parity.md, section 4.2
// "skills get|list|path", 7.9): the agent skills embedded in this binary,
// so an agent reads the instructions that match the installed version.
// They are Local (no daemon, no page) and have no MCP tool (design 4.2).
func init() {
	Register(&Command{
		Name: "skills list", Aliases: []string{"skills"}, Local: true,
		Summary: "List the agent skills built into this binary (name and description); 'skills get <name>' prints one.",
		Run: func(context.Context, Env, Input) (Result, error) {
			list, err := skillsList()
			if err != nil {
				return Result{}, err
			}
			width := 0
			for _, s := range list {
				width = max(width, len(s.Name))
			}
			var b strings.Builder
			for _, s := range list {
				fmt.Fprintf(&b, "  %-*s  %s\n", width, s.Name, shorten(s.Description, listDescriptionRunes))
			}
			return Result{Text: strings.TrimRight(b.String(), "\n"), Data: list}, nil
		},
	})
	Register(&Command{
		Name: "skills get", Local: true,
		Summary: "Print agent skills built into this binary (the whole SKILL.md); without a name, or with --all, every skill.",
		Args:    []Arg{{Name: "name", Help: "skill name from 'skills list'; several names print several skills", Variadic: true}},
		Flags:   []Flag{{Name: "all", Type: FlagBool, Help: "print every skill"}},
		Run: func(_ context.Context, _ Env, in Input) (Result, error) {
			var out []skills.Skill
			if len(in.Args) == 0 || flagBool(in, "all") {
				all, err := skillsAll()
				if err != nil {
					return Result{}, err
				}
				out = all
			} else {
				for _, n := range in.Args {
					s, err := skills.Get(n)
					if err != nil {
						return Result{}, err
					}
					out = append(out, s)
				}
			}
			texts := make([]string, len(out))
			for i, s := range out {
				texts[i] = strings.TrimRight(s.Content, "\n")
			}
			return Result{Text: strings.Join(texts, "\n\n"), Data: out}, nil
		},
	})
	Register(&Command{
		Name: "skills path", Local: true,
		Summary: "Write the built-in skills to the user cache directory (once per skills version, mode 0600) and print the directory of one skill or of all.",
		Args:    []Arg{{Name: "name", Help: "skill name; without it, the directory that holds every skill"}},
		Run: func(_ context.Context, _ Env, in Input) (Result, error) {
			cache, err := os.UserCacheDir()
			if err != nil {
				return Result{}, fmt.Errorf("finding the user cache directory for the skills: %w", err)
			}
			dir, err := skills.Install(filepath.Join(cache, "iwdp", "skills"))
			if err != nil {
				return Result{}, err
			}
			if len(in.Args) == 1 {
				if _, err := skills.Get(in.Args[0]); err != nil {
					return Result{}, err
				}
				dir = filepath.Join(dir, in.Args[0])
			}
			return Result{Text: dir, Data: map[string]string{"path": dir}}, nil
		},
	})
}

// shorten cuts s to at most n runes at a word boundary and adds "...".
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cut := string([]rune(s)[:n])
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:.") + "..."
}
