package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/nnemirovsky/agent-ios/internal/runtime"
)

// The action part of find (design docs/design/agentic-parity.md, section
// 4.2: "find <kind> <value> [action [args]]"; agent-browser "find ...
// click"): the locator must match exactly one element, and the action runs
// on its new ref through the action command itself, in the same operation.

// findActions maps an action word to its command and its number of
// arguments after the target.
var findActions = map[string]struct {
	cmd  string
	args int
}{
	"click": {"click", 0}, "dblclick": {"dblclick", 0}, "tap": {"tap", 0}, "hover": {"hover", 0},
	"focus": {"focus", 0}, "check": {"check", 0}, "uncheck": {"uncheck", 0},
	"scrollintoview": {"scrollintoview", 0}, "fill": {"fill", 1}, "type": {"type", 1},
}

const findActionList = "click, dblclick, tap, hover, focus, check, uncheck, scrollintoview, fill <text>, type <text>"

type findAction struct {
	word string
	cmd  *Command
	args []string
}

// parseFindAction reads the words after the locator: none, or an action
// and its arguments.
func parseFindAction(words []string) (*findAction, error) {
	if len(words) == 0 {
		return nil, nil
	}
	a, ok := findActions[words[0]]
	if !ok {
		return nil, usagef("find: unknown action %q; want one of %s", words[0], findActionList)
	}
	if len(words)-1 != a.args {
		return nil, usagef("find ... %s takes %d argument(s) after the locator, got %d", words[0], a.args, len(words)-1)
	}
	c, _ := Lookup([]string{a.cmd})
	if c == nil || c.Name != a.cmd {
		return nil, fmt.Errorf("find ... %s: the %s command is not registered", words[0], a.cmd)
	}
	return &findAction{word: words[0], cmd: c, args: words[1:]}, nil
}

// run runs the action on the single match of the locator.
func (a *findAction) run(ctx context.Context, env Env, in Input, matches []runtime.Node, warnings []string) (Result, error) {
	var refs []string
	var walk func([]runtime.Node)
	walk = func(ns []runtime.Node) {
		for _, n := range ns {
			if n.Ref != "" {
				refs = append(refs, n.Ref)
			}
			walk(n.Children)
		}
	}
	walk(matches)
	switch {
	case len(refs) == 0:
		return Result{}, &runtime.Error{Code: "not_found", Message: fmt.Sprintf("find found no element to %s", a.word)}
	case len(refs) > 1:
		return Result{}, &runtime.Error{Code: "ambiguous", Message: fmt.Sprintf("find matched %d elements (%s); %s needs exactly one: add --name or --exact, or use find nth", len(refs), "@"+strings.Join(refs, ", @"), a.word)}
	}
	sub := Input{Args: append([]string{"@" + refs[0]}, a.args...), Flags: map[string]any{}, Global: in.Global}
	r, err := a.cmd.Run(ctx, env, sub)
	if err != nil {
		return r, fmt.Errorf("find ... %s on @%s: %w", a.word, refs[0], err)
	}
	r.Text = withWarnings(fmt.Sprintf("@%s: %s", refs[0], r.Text), warnings)
	return r, nil
}
