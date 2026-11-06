package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Exit codes of registry commands (design 2.10).
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitTimeout     = 3
	ExitBusy        = 4 // page busy or lost
	ExitNoPage      = 5 // no device or page
	ExitReservation = 6 // device reservation missing
)

// DefaultTimeout is the command deadline when neither --timeout,
// IWDP_TIMEOUT nor the command sets one (design 2.6, W0 default).
const DefaultTimeout = 30 * time.Second

// commandTimeout is the deadline of one run of c (design 2.6): --timeout or
// IWDP_TIMEOUT, else the command's own timeout, else DefaultTimeout.
func commandTimeout(c *Command, g Globals) time.Duration {
	switch {
	case g.Timeout > 0:
		return g.Timeout
	case c.Timeout > 0:
		return c.Timeout
	}
	return DefaultTimeout
}

// UsageError is a command line error. It exits with ExitUsage before any
// connection.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// ExitCoder lets a command error choose its exit code.
type ExitCoder interface{ ExitCode() int }

// ExitCodeOf maps an error to an exit code.
func ExitCodeOf(err error) int {
	var ec ExitCoder
	var ue *UsageError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ue):
		return ExitUsage
	case errors.As(err, &ec):
		return ec.ExitCode()
	case errors.Is(err, context.DeadlineExceeded):
		return ExitTimeout
	}
	return ExitError
}

type globalSpec struct {
	name  string
	value bool // takes a value
	set   func(g *Globals, v string) error
}

// parseTimeout parses the value of --timeout or IWDP_TIMEOUT (name).
func parseTimeout(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q: want a positive duration such as 30s", name, v)
	}
	return d, nil
}

var globalSpecs = []globalSpec{
	{"--session", true, func(g *Globals, v string) error { g.Session = v; return nil }},
	{"--page", true, func(g *Globals, v string) error { g.Page = v; return nil }},
	{"--timeout", true, func(g *Globals, v string) (err error) {
		g.Timeout, err = parseTimeout("--timeout", v)
		return err
	}},
	{"--json", false, func(g *Globals, _ string) error { g.JSON = true; return nil }},
	{"--transport", true, func(g *Globals, v string) error {
		switch v {
		case "auto", "iwdp", "direct", "webkitgtk":
			g.Transport = v
			return nil
		}
		return fmt.Errorf("--transport %q: want auto, iwdp, direct or webkitgtk", v)
	}},
	{"--max-output", true, func(g *Globals, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("--max-output %q: want a byte count", v)
		}
		g.MaxOutput = n
		return nil
	}},
	{"--content-boundaries", false, func(g *Globals, _ string) error { g.ContentBoundaries = true; return nil }},
}

func globalFlag(name string) *globalSpec {
	for i := range globalSpecs {
		if globalSpecs[i].name == name {
			return &globalSpecs[i]
		}
	}
	return nil
}

// Invocation is a parsed command line.
type Invocation struct {
	Cmd   *Command
	Input Input
	Help  bool // --help or -h: print help, run nothing
}

// Parse parses a registry command line: global flags anywhere before "--",
// the command path, then its flags and arguments. It never connects to
// anything. env supplies IWDP_SESSION, IWDP_TRANSPORT and IWDP_TIMEOUT
// defaults; the flags override them.
func Parse(args []string, env func(string) string) (*Invocation, error) {
	g := Globals{Session: "default", Transport: "auto"}
	if v := env("IWDP_SESSION"); v != "" {
		g.Session = v
	}
	if v := env("IWDP_TRANSPORT"); v != "" {
		g.Transport = v
	}
	if v := env("IWDP_TIMEOUT"); v != "" {
		d, err := parseTimeout("IWDP_TIMEOUT", v)
		if err != nil {
			return nil, &UsageError{err.Error()}
		}
		g.Timeout = d
	}
	help := false
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if a == "--help" || a == "-h" {
			help = true
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		gs := globalFlag(name)
		if gs == nil {
			rest = append(rest, a)
			continue
		}
		if gs.value && !hasVal {
			if i+1 >= len(args) {
				return nil, &UsageError{fmt.Sprintf("%s needs a value", name)}
			}
			i++
			val = args[i]
		}
		if err := gs.set(&g, val); err != nil {
			return nil, &UsageError{err.Error()}
		}
	}
	var words []string
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	c, n := Lookup(words)
	if c == nil {
		if len(words) == 0 {
			return &Invocation{Help: help}, nil
		}
		return nil, &UsageError{fmt.Sprintf("unknown command %q", strings.Join(words, " "))}
	}
	inv := &Invocation{Cmd: c, Help: help, Input: Input{Global: g, Flags: map[string]any{}}}
	if help {
		return inv, nil
	}
	if err := parseCommand(c, rest[n:], &inv.Input); err != nil {
		return nil, &UsageError{fmt.Sprintf("%s: %v (run 'iwdp-cli %s --help')", c.Name, err, c.Name)}
	}
	return inv, nil
}

func findFlag(c *Command, name string) *Flag {
	for i := range c.Flags {
		f := &c.Flags[i]
		if name == "--"+f.Name || (f.Short != "" && name == "-"+f.Short) {
			return f
		}
	}
	return nil
}

func parseCommand(c *Command, args []string, in *Input) error {
	for _, f := range c.Flags {
		if f.Default == "" {
			continue
		}
		v, err := parseFlagValue(f, f.Default)
		if err != nil {
			return fmt.Errorf("default of --%s: %w", f.Name, err)
		}
		in.Flags[f.Name] = v
	}
	positional := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if positional || !strings.HasPrefix(a, "-") || a == "-" {
			in.Args = append(in.Args, a)
			continue
		}
		if a == "--" {
			positional = true
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		f := findFlag(c, name)
		if f == nil {
			return fmt.Errorf("unknown flag %s", name)
		}
		if f.Type == FlagBool {
			if !hasVal {
				val = "true"
			}
		} else if !hasVal {
			if i+1 >= len(args) {
				return fmt.Errorf("flag --%s needs a value", f.Name)
			}
			i++
			val = args[i]
		}
		v, err := parseFlagValue(*f, val)
		if err != nil {
			return err
		}
		in.Flags[f.Name] = v
	}
	need, max := 0, len(c.Args)
	for _, a := range c.Args {
		if a.Required {
			need++
		}
		if a.Variadic {
			max = -1
		}
	}
	if len(in.Args) < need {
		return fmt.Errorf("missing argument <%s>", c.Args[len(in.Args)].Name)
	}
	if max >= 0 && len(in.Args) > max {
		return fmt.Errorf("unexpected argument %q", in.Args[max])
	}
	return nil
}

func parseFlagValue(f Flag, v string) (any, error) {
	switch f.Type {
	case FlagInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: %q is not an integer", f.Name, v)
		}
		return n, nil
	case FlagBool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: %q is not true or false", f.Name, v)
		}
		return b, nil
	case FlagDuration:
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("flag --%s: %q is not a duration such as 300ms", f.Name, v)
		}
		return d, nil
	case FlagEnum:
		for _, e := range f.Enum {
			if v == e {
				return v, nil
			}
		}
		return nil, fmt.Errorf("flag --%s: %q is not one of %s", f.Name, v, strings.Join(f.Enum, ", "))
	}
	return v, nil
}

// Help returns the help text of c.
func Help(c *Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: iwdp-cli %s", c.Name)
	for _, a := range c.Args {
		n := "<" + a.Name + ">"
		if a.Variadic {
			n += "..."
		}
		if !a.Required {
			n = "[" + n + "]"
		}
		b.WriteString(" " + n)
	}
	if len(c.Flags) > 0 {
		b.WriteString(" [flags]")
	}
	fmt.Fprintf(&b, "\n\n%s\n", c.Summary)
	if len(c.Aliases) > 0 {
		fmt.Fprintf(&b, "\nAliases: %s\n", strings.Join(c.Aliases, ", "))
	}
	if len(c.Args) > 0 {
		b.WriteString("\nArguments:\n")
		for _, a := range c.Args {
			fmt.Fprintf(&b, "  %-22s %s\n", a.Name, a.Help)
		}
	}
	if len(c.Flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, f := range c.Flags {
			n := "--" + f.Name
			if f.Short != "" {
				n = "-" + f.Short + ", " + n
			}
			h := f.Help
			if f.Type == FlagEnum {
				h += " (" + strings.Join(f.Enum, "|") + ")"
			}
			if f.Default != "" {
				h += " (default " + f.Default + ")"
			}
			fmt.Fprintf(&b, "  %-22s %s\n", n, h)
		}
	}
	b.WriteString("\nGlobal flags: --session, --page, --timeout, --json, --transport, --max-output, --content-boundaries\n")
	return b.String()
}

// Usage lists every registered command.
func Usage() string {
	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, c := range All() {
		fmt.Fprintf(&b, "  %-24s %s\n", c.Name, c.Summary)
	}
	return b.String()
}

// RunCLI parses args and runs the command. Help and usage errors are
// handled before newEnv is called, so they never connect to anything. The
// command deadline (commandTimeout; an earlier deadline of ctx stays) is
// fixed before newEnv, so it covers building the environment and the run.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer, newEnv func(context.Context, *Invocation) (Env, error)) int {
	inv, err := Parse(args, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return ExitCodeOf(err)
	}
	if inv.Help || inv.Cmd == nil {
		if inv.Cmd != nil {
			fmt.Fprint(stdout, Help(inv.Cmd))
		} else {
			fmt.Fprint(stdout, Usage())
		}
		return ExitOK
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout(inv.Cmd, inv.Input.Global))
	defer cancel()
	env, err := newEnv(ctx, inv)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return ExitCodeOf(err)
	}
	res, err := inv.Cmd.Run(ctx, env, inv.Input)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %s: %v\n", inv.Cmd.Name, err)
		return ExitCodeOf(err)
	}
	if err := writeResult(stdout, res, inv.Input.Global); err != nil {
		fmt.Fprintf(stderr, "Error: writing the result: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func writeResult(w io.Writer, res Result, g Globals) error {
	out := res.Text
	if g.JSON {
		v := res.Data
		if v == nil {
			v = map[string]string{"text": res.Text}
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = string(data)
	}
	// Truncation applies to text only; cutting JSON would make it unparsable.
	if !g.JSON && g.MaxOutput > 0 && len(out) > g.MaxOutput {
		out = fmt.Sprintf("%s\n[output truncated: %d of %d bytes shown]", out[:g.MaxOutput], g.MaxOutput, len(out))
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err := io.WriteString(w, out)
	return err
}
