package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/edxeth/agent-ios/internal/skills"
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
// They are Local (no daemon, no page).
func init() {
	Register(&Command{
		Name: "skills list", Aliases: []string{"skills"}, Local: true,
		Summary: "List the agent skills built into this binary (name and description); 'skills get <name>' prints one.",
		Run: func(context.Context, Env, Input) (Result, error) {
			list, err := skillsList()
			if err != nil {
				return Result{}, err
			}
			return Result{Text: skillsListText(list), Data: list}, nil
		},
	})
	Register(&Command{
		Name: "skills get", Local: true,
		Summary: "Print agent skills built into this binary (each guide without its front matter; --json gives the whole SKILL.md); without a name, or with --all, every skill.",
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
			return Result{Text: skillsGetText(out), Data: out}, nil
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
			dir, err := skills.Install(filepath.Join(cache, "agent-ios", "skills"))
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

// skillsListText is the text of skills list: one line per skill with its
// name and its shortened description. Private mode makes it from the
// members that it prints (privateReads).
func skillsListText(list []skills.Skill) string {
	width := 0
	for _, s := range list {
		width = max(width, len(s.Name))
	}
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, s.Name, shorten(s.Description, listDescriptionRunes))
	}
	return strings.TrimRight(b.String(), "\n")
}

// skillsGetText is the text of skills get: the content of each skill, an
// empty line between two. Private mode makes it from the members that it
// prints (privateReads).
func skillsGetText(list []skills.Skill) string {
	texts := make([]string, len(list))
	for i, s := range list {
		texts[i] = strings.TrimRight(skillBody(s.Content), "\n")
	}
	return strings.Join(texts, "\n\n")
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

// skillBody is a SKILL.md without its front matter: the name and the
// description are for 'skills list' and for an agent choosing a skill; an
// agent that runs 'skills get' has chosen it already, so the text output
// prints the guide alone.
func skillBody(content string) string {
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return content
	}
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return strings.TrimLeft(rest[i+len("\n---\n"):], "\n")
	}
	return content
}
