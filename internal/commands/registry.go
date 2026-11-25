package commands

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	regMu    sync.Mutex
	registry = map[string]*Command{} // by name and alias
	ordered  []*Command
)

// Register adds a command. It is called from init() in each command file
// and panics on a description that breaks a registry rule: empty name or
// summary, a name or alias already taken, a flag or argument without help,
// a duplicate flag, an enum flag without values, or a Run of nil.
func Register(c *Command) {
	if err := validate(c); err != nil {
		panic("commands.Register: " + err.Error())
	}
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if prev, ok := registry[n]; ok {
			panic(fmt.Sprintf("commands.Register: %q is already used by command %q", n, prev.Name))
		}
	}
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		registry[n] = c
	}
	ordered = append(ordered, c)
}

func validate(c *Command) error {
	if c == nil || strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("command without a name")
	}
	if c.Summary == "" {
		return fmt.Errorf("command %q has no summary", c.Name)
	}
	if c.Run == nil {
		return fmt.Errorf("command %q has no Run", c.Name)
	}
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if n != strings.Join(strings.Fields(n), " ") || strings.HasPrefix(n, "-") {
			return fmt.Errorf("command %q: name or alias %q must be space-separated words", c.Name, n)
		}
	}
	seen := map[string]bool{}
	for _, f := range c.Flags {
		if f.Name == "" || f.Help == "" {
			return fmt.Errorf("command %q: flag %q has no help text", c.Name, f.Name)
		}
		for _, n := range []string{"--" + f.Name, "-" + f.Short} {
			if n == "-" {
				continue
			}
			if seen[n] || globalFlag(n) != nil || n == "--help" || n == "-h" {
				return fmt.Errorf("command %q: flag %s is defined twice or shadows a global flag", c.Name, n)
			}
			seen[n] = true
		}
		if f.Type == FlagEnum && len(f.Enum) == 0 {
			return fmt.Errorf("command %q: enum flag %q has no values", c.Name, f.Name)
		}
	}
	for i, a := range c.Args {
		if a.Name == "" || a.Help == "" {
			return fmt.Errorf("command %q: argument %d has no name or help text", c.Name, i+1)
		}
		if a.Variadic && i != len(c.Args)-1 {
			return fmt.Errorf("command %q: only the last argument can be variadic", c.Name)
		}
	}
	return nil
}

// All returns the registered commands sorted by name.
func All() []*Command {
	regMu.Lock()
	defer regMu.Unlock()
	out := append([]*Command(nil), ordered...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds the command whose name or alias matches the longest prefix
// of words. It returns the command and the number of words it used.
func Lookup(words []string) (*Command, int) {
	regMu.Lock()
	defer regMu.Unlock()
	for n := len(words); n > 0; n-- {
		if c, ok := registry[strings.Join(words[:n], " ")]; ok {
			return c, n
		}
	}
	return nil, 0
}

// Unregister removes a command. It is test support: no production code
// calls it, and the tests of this package and of other packages
// (internal/daemon, cmd/agent-ios) use it to clean up the commands they
// register.
func Unregister(c *Command) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if registry[n] == c {
			delete(registry, n)
		}
	}
	for i, o := range ordered {
		if o == c {
			ordered = append(ordered[:i], ordered[i+1:]...)
			break
		}
	}
}
