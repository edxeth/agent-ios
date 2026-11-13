package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Exit codes of registry commands (design 2.10).
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitTimeout     = 124 // like timeout(1); decision D9
	ExitBusy        = 4   // page busy or lost
	ExitNoPage      = 5   // no device or page
	ExitReservation = 6   // device reservation missing
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

// parseTimeout parses the value of --timeout, IWDP_TIMEOUT or --wait-lease
// (name): a Go duration ("30s", "1m30s") or a bare whole number of seconds
// ("30"; strconv.Atoi syntax), greater than zero. Earlier iwdp-cli versions
// accepted both forms. A second count too large for a time.Duration is
// refused, as time.ParseDuration refuses the same count with an "s".
func parseTimeout(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if secs, aerr := strconv.Atoi(v); aerr == nil {
		d, err = time.Duration(secs)*time.Second, nil
		if secs <= 0 || secs > math.MaxInt64/int(time.Second) {
			d = 0 // not positive, or too large for a Duration
		}
	}
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q: want a positive duration such as 30s, or a whole number of seconds such as 30", name, v)
	}
	return d, nil
}

var globalSpecs = []globalSpec{
	{"--session", true, func(g *Globals, v string) error { g.Session = v; return nil }},
	{"--page", true, func(g *Globals, v string) error { g.Page = v; return nil }},
	{"--only-tab", false, boolGlobal("--only-tab", func(g *Globals, b bool) { g.OnlyTab = b })},
	{"--timeout", true, func(g *Globals, v string) (err error) {
		g.Timeout, err = parseTimeout("--timeout", v)
		return err
	}},
	{"--json", false, boolGlobal("--json", func(g *Globals, b bool) { g.JSON = b })},
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
	{"--content-boundaries", false, boolGlobal("--content-boundaries", func(g *Globals, b bool) { g.ContentBoundaries = b })},
	{"--reservation", true, func(g *Globals, v string) error { g.Reservation = v; return nil }},
	{"--wait-lease", true, func(g *Globals, v string) (err error) {
		g.WaitLease, err = parseTimeout("--wait-lease", v)
		return err
	}},
	{"--takeover", false, boolGlobal("--takeover", func(g *Globals, b bool) { g.Takeover = b })},
	{"--private", false, boolGlobal("--private", func(g *Globals, b bool) { g.Private = b })},
}

// boolGlobal is the setter of the boolean global option name: "--name"
// alone sets true (splitGlobals passes "true"), "--name=<v>" takes exactly
// the values of strconv.ParseBool, and any other value is an error that
// names the option and the value. An explicit false is honored, so
// --takeover=false never authorizes a takeover.
func boolGlobal(name string, set func(g *Globals, b bool)) func(*Globals, string) error {
	return func(g *Globals, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s %q: want true or false (or 1, t, T, TRUE, True, 0, f, F, FALSE, False)", name, v)
		}
		set(g, b)
		return nil
	}
}

// CheckPageGlobals refuses --only-tab together with --page: both name the
// page of the command. Parse and the daemon (for a request that carries
// both) return its usage error (exit 2) before any page work.
func CheckPageGlobals(g Globals) error {
	if g.OnlyTab && g.Page != "" {
		return &UsageError{fmt.Sprintf("--only-tab and --page %q both name the page; pass one of them", g.Page)}
	}
	return nil
}

// CheckPageArgs refuses --only-tab together with an argument of c that
// names a page (Arg.Page): both name the page, and the argument would
// override the one tab that --only-tab checked. Parse and the daemon (for
// a request with --only-tab, which is not in its Argv) return its usage
// error (exit 2) before any page work.
func CheckPageArgs(c *Command, in Input) error {
	if !in.Global.OnlyTab {
		return nil
	}
	for i, a := range c.Args {
		if a.Page && i < len(in.Args) {
			return &UsageError{fmt.Sprintf("%s: --only-tab and the <%s> argument %q both name the page; pass one of them", c.Name, a.Name, in.Args[i])}
		}
	}
	return nil
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
	// Group, with Help and no Cmd, is the help of the command group that
	// the command words name (GroupHelp).
	Group string
}

// Parse parses a registry command line: global flags anywhere before "--",
// the command path, then its flags and arguments. It never connects to
// anything. env supplies IWDP_SESSION, IWDP_TRANSPORT, IWDP_TIMEOUT and
// IWDP_PRIVATE defaults; the flags override them.
func Parse(args []string, env func(string) string) (*Invocation, error) {
	g := Globals{Session: "default", Transport: "auto"}
	if v := env("IWDP_SESSION"); v != "" {
		g.Session = v
	}
	if v := env("IWDP_TRANSPORT"); v != "" {
		g.Transport = v
	}
	g.Reservation = env("IWDP_DEVICE_RESERVATION")
	if v := env("IWDP_TIMEOUT"); v != "" {
		d, err := parseTimeout("IWDP_TIMEOUT", v)
		if err != nil {
			return nil, &UsageError{err.Error()}
		}
		g.Timeout = d
	}
	if v := env(privateEnv); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, &UsageError{fmt.Sprintf("%s %q: want true or false (or 1, t, T, TRUE, True, 0, f, F, FALSE, False)", privateEnv, v)}
		}
		g.Private = b
	}
	rest, help, err := splitGlobals(args, &g)
	if err != nil {
		return nil, err
	}
	if err := CheckPageGlobals(g); err != nil {
		return nil, err
	}
	// An unknown long option or an unknown command is a usage error also
	// with --help or -h; help never hides a malformed line.
	words := commandWords(rest)
	c, n := Lookup(words)
	if c == nil {
		if len(words) == 0 {
			if help && onlyHelp(args) {
				// "--help" or "-h" alone: the overview.
				return &Invocation{Help: true}, nil
			}
			if name := unknownLongFlag(rest); name != "" {
				return nil, &UsageError{fmt.Sprintf("no command in %q: unknown flag %s (run 'iwdp-cli --help')", strings.Join(args, " "), name)}
			}
			// Only global options, or a flag before any command word.
			return nil, &UsageError{fmt.Sprintf("no command in %q (run 'iwdp-cli --help')", strings.Join(args, " "))}
		}
		if list, ok := GroupHelp(words); ok {
			if help {
				// A group has no flags of its own.
				if name := unknownLongFlag(rest[len(words):]); name != "" {
					group := strings.Join(words, " ")
					return nil, &UsageError{fmt.Sprintf("%s: unknown flag %s (run 'iwdp-cli %s --help')", group, name, group)}
				}
				return &Invocation{Help: true, Group: list}, nil
			}
			return nil, &UsageError{fmt.Sprintf("%q is a group of commands, not a command.\n%s", strings.Join(words, " "), strings.TrimSuffix(list, "\n"))}
		}
		return nil, &UsageError{fmt.Sprintf("unknown command %q", strings.Join(words, " "))}
	}
	inv := &Invocation{Cmd: c, Help: help, Input: Input{Global: g, Flags: map[string]any{}}}
	if help {
		// Help needs none of the command's arguments, but its flags must
		// be the command's, with valid values (Check).
		in := Input{Global: g, Flags: map[string]any{}}
		if err := parseFlags(c, rest[n:], &in); err != nil {
			return nil, &UsageError{fmt.Sprintf("%s: %v (run 'iwdp-cli %s --help')", c.Name, err, c.Name)}
		}
		if err := checkValues(c, in); err != nil {
			return nil, err
		}
		return inv, nil
	}
	if err := parseCommand(c, rest[n:], &inv.Input); err != nil {
		return nil, &UsageError{fmt.Sprintf("%s: %v (run 'iwdp-cli %s --help')", c.Name, err, c.Name)}
	}
	if err := CheckPageArgs(c, inv.Input); err != nil {
		return nil, err
	}
	if err := checkValues(c, inv.Input); err != nil {
		return nil, err
	}
	return inv, nil
}

// checkValues runs c.Check, when set, and returns its error as a usage
// error that names c. Parse calls it also for a help request, whose
// arguments can be missing, so a Check checks values only, never that an
// argument is given.
func checkValues(c *Command, in Input) error {
	if c.Check == nil {
		return nil
	}
	if err := c.Check(in); err != nil {
		return &UsageError{fmt.Sprintf("%s: %v (run 'iwdp-cli %s --help')", c.Name, err, c.Name)}
	}
	return nil
}

// splitGlobals takes the global flags and --help (-h) out of args, up to
// "--", and returns the other arguments in their order. It sets the flags
// on g; with g nil it checks no value, so it only finds the command words
// (CommandWords). On an error, rest holds the arguments before the bad
// flag.
func splitGlobals(args []string, g *Globals) (rest []string, help bool, err error) {
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
		if !gs.value && !hasVal {
			val = "true" // a boolean option alone
		}
		if gs.value && !hasVal {
			if i+1 >= len(args) {
				return rest, help, &UsageError{fmt.Sprintf("%s needs a value", name)}
			}
			i++
			val = args[i]
		}
		if g == nil {
			continue
		}
		if err := gs.set(g, val); err != nil {
			return rest, help, &UsageError{err.Error()}
		}
	}
	return rest, help, nil
}

// onlyHelp reports whether every argument is --help or -h.
func onlyHelp(args []string) bool {
	for _, a := range args {
		if a != "--help" && a != "-h" {
			return false
		}
	}
	return true
}

// unknownLongFlag returns the name of the first argument before "--" that
// starts with "--" (its text before any "="), or "". It checks the
// arguments of a line without a command, and those after a group's words,
// where no long flag is known.
func unknownLongFlag(args []string) string {
	for _, a := range args {
		if a == "--" {
			return ""
		}
		if strings.HasPrefix(a, "--") {
			name, _, _ := strings.Cut(a, "=")
			return name
		}
	}
	return ""
}

// commandWords are the leading arguments of rest that are not flags.
func commandWords(rest []string) []string {
	var words []string
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	return words
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

// parseCommand parses the flags and arguments of c (parseFlags) and
// checks the number of arguments.
func parseCommand(c *Command, args []string, in *Input) error {
	if err := parseFlags(c, args, in); err != nil {
		return err
	}
	return checkArgs(c, in)
}

// parseFlags sets the flags of c in in.Flags, defaults first, and collects
// the other arguments in in.Args.
func parseFlags(c *Command, args []string, in *Input) error {
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
	// Only exact, known flags are flags. Any other token that starts with
	// "--" is a usage error; every other token, also one with a single
	// leading dash that names no short flag of c (-1, -.5, -Math.abs(2),
	// -window.scrollY), is an argument. "--" ends the flags.
	positional := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if positional {
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
			if strings.HasPrefix(a, "--") {
				return fmt.Errorf("unknown flag %s", name)
			}
			in.Args = append(in.Args, a)
			continue
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
	return nil
}

// checkArgs checks the number of arguments that parseFlags collected.
func checkArgs(c *Command, in *Input) error {
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
		extra := in.Args[max]
		if isPageURL(extra) {
			// Earlier iwdp-cli versions took the page URL as the last
			// argument.
			return fmt.Errorf("unexpected argument %q: a page URL is not an argument; pass it with --page <ws:// URL>", extra)
		}
		return fmt.Errorf("unexpected argument %q", extra)
	}
	return nil
}

// isPageURL reports whether an argument is a page's WebSocket URL.
func isPageURL(a string) bool {
	return strings.HasPrefix(a, "ws://") || strings.HasPrefix(a, "wss://")
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
	b.WriteString("\nGlobal flags: --session, --page, --only-tab, --timeout, --json, --transport, --max-output,\n  --content-boundaries, --reservation, --wait-lease, --takeover, --private\n")
	return b.String()
}

// GroupHelp lists the commands of the group that the command words name:
// each command whose name or alias starts with these words and has more
// words, with its summary. It reports false when the words start no
// command name.
func GroupHelp(words []string) (string, bool) {
	prefix := strings.Join(words, " ") + " "
	var b strings.Builder
	for _, c := range All() {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if strings.HasPrefix(n, prefix) {
				fmt.Fprintf(&b, "  %-24s %s\n", n, c.Summary)
			}
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	group := strings.TrimSpace(prefix)
	return fmt.Sprintf("Usage: iwdp-cli %s <command>\n\nCommands of %q:\n%sRun 'iwdp-cli %s <command> --help' for the help of one.\n", group, group, b.String(), group), true
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

// ErrInterrupted is the cause with which the caller of RunCLIStarted
// cancels ctx when a signal (SIGINT or SIGTERM) ends the command. The
// caller exits ExitInterrupted for the signal; RunCLIStarted prints no
// error for a command that the signal ended.
var ErrInterrupted = errors.New("interrupted by a signal")

// ExitInterrupted is the exit code of a command that a signal (SIGINT or
// SIGTERM) ended: 128 + SIGINT, as shells report it.
const ExitInterrupted = 130

// Outcome is how one run of iwdp-cli ends: its exit code and the text
// that stderr gets last. Every exit path of the CLI ends with one
// Outcome, and Line is the one decision of that text, so that private
// mode prints exactly one status line on every path: a registry command
// (RunCLIOutcome), help and usage errors, and the ends that the process
// decides itself (a signal, the deadline watchdog; cmd/iwdp-cli).
type Outcome struct {
	// Command is the command, or the words of the command group, that the
	// line names; "" when it names none (the overview, a line that Parse
	// refused). It is a registry name, never an argument.
	Command string
	Code    int
	// Private is private mode (--private, IWDP_PRIVATE; privateRequested
	// for a line that Parse refused).
	Private bool
	// Status is the status line of a command that succeeded in private
	// mode (privateResult); "" means "<Command> exit 0".
	Status string
	// Message is what stderr gets last without private mode: the error,
	// which can have several lines, or "" for nothing. It can name a page
	// or repeat an argument, so private mode never prints it.
	Message string
}

// Line returns the text that stderr gets last, "" for none. In private
// mode it is exactly one status line, made of the command and the exit
// code only: Status or "<command> exit 0" for a success, privateErrorLine
// for a failure. Without private mode it is Message.
func (o Outcome) Line() string {
	switch {
	case !o.Private:
		return o.Message
	case o.Code != ExitOK:
		return privateErrorLine(o.Command, o.Code)
	case o.Status != "":
		return o.Status
	case o.Command != "":
		return o.Command + " exit 0"
	}
	return "iwdp-cli exit 0"
}

// Print writes Line and a newline on w; it writes nothing for an empty
// Line.
func (o Outcome) Print(w io.Writer) {
	if line := o.Line(); line != "" {
		_, _ = fmt.Fprintln(w, line) // nothing to do when stderr fails
	}
}

// WriteFailed returns the end of the run o whose output what ("result",
// "help", "output") could not be written on stdout (err): a success
// becomes exit 1 with the line "Error: writing the <what>: <err>"; a
// failure keeps its exit code, and that line comes first in its message.
// In private mode Line prints only the status line of the exit code.
func (o Outcome) WriteFailed(what string, err error) Outcome {
	line := fmt.Sprintf("Error: writing the %s: %v", what, err)
	if o.Code == ExitOK {
		return Outcome{Command: o.Command, Code: ExitError, Private: o.Private, Message: line}
	}
	o.Message = strings.TrimSuffix(line+"\n"+o.Message, "\n")
	return o
}

// DaemonRun is the command that runs the daemon in the foreground
// (internal/daemon registers it). Private mode never applies to it
// (privateRun).
const DaemonRun = "daemon run"

// PrivateRequested reports whether the run of the command line args, with
// the environment of this process, is in private mode (privateRun). main
// calls it first, before any output or work, to install what private mode
// needs at the process boundary.
func PrivateRequested(args []string) bool {
	inv, err := Parse(args, os.Getenv)
	if err != nil {
		inv = nil
	}
	return privateRun(args, inv)
}

// privateRun decides private mode for the run of the command line args,
// which Parse read as inv (nil: Parse refused the line). Every run gets
// its mode here: the process boundary (PrivateRequested) and
// RunCLIOutcome.
//
// A line that Parse refused, and a help request, have no parsed options:
// privateRequested reads them as Parse does. A command runs in the mode of
// its options (--private, IWDP_PRIVATE), with one rule:
//
// Private mode is a mode of the output of a client command, and "daemon
// run" is not a client command: it is the daemon itself. A client starts
// it on demand with the client's environment, so IWDP_PRIVATE=1 reaches
// it, and its stderr is the daemon's log, which has its own policy of
// safe arguments (internal/logsafe). So private mode never applies to a
// run of "daemon run": with --private or IWDP_PRIVATE=1 it logs on
// stderr, prints no status line and exits as without them. Its help, and
// a line of its words that Parse refuses, are output of the client and
// stay in private mode.
func privateRun(args []string, inv *Invocation) bool {
	switch {
	case inv == nil || inv.Help || inv.Cmd == nil:
		return privateRequested(args, os.Getenv)
	case inv.Cmd.Name == DaemonRun:
		return false
	}
	return inv.Input.Global.Private
}

// RunCLIStarted runs the command line args (RunCLIOutcome), prints its
// end on stderr (Outcome.Print) and returns its exit code.
func RunCLIStarted(ctx context.Context, args []string, stdout, stderr io.Writer, start func(context.Context, *Invocation) DeadlineReporter, newEnv func(context.Context, *Invocation) (Env, error)) int {
	o := RunCLIOutcome(ctx, args, stdout, start, newEnv)
	o.Print(stderr)
	return o.Code
}

// RunCLIOutcome parses args and runs the command. It prints the result on
// stdout and returns how the run ends; it prints nothing on stderr, so
// that the caller prints the end once (Outcome.Print), after anything
// else that it does at the exit. Help and usage errors are handled before
// newEnv is called, so they never connect to anything. The command
// deadline (commandTimeout; an earlier deadline of ctx stays) is fixed
// before newEnv, so it covers building the environment and the run.
// start, when not nil, is called once the command's deadline is fixed,
// before the command's ClientStep and newEnv. The DeadlineReporter that
// start returns (nil: none) explains a deadline error of every later
// stage: the ClientStep (for example a read of the standard input),
// building the environment (for example connecting to or starting the
// daemon) and the run.
func RunCLIOutcome(ctx context.Context, args []string, stdout io.Writer, start func(context.Context, *Invocation) DeadlineReporter, newEnv func(context.Context, *Invocation) (Env, error)) Outcome {
	inv, err := Parse(args, os.Getenv)
	if err != nil {
		// A usage error can repeat an argument, and an argument can be a
		// URL: the line names no command in private mode.
		return Outcome{Code: ExitCodeOf(err), Private: privateRun(args, nil), Message: fmt.Sprintf("Error: %v", err)}
	}
	if inv.Help || inv.Cmd == nil {
		// A help request without a command has no parsed options. Parse
		// accepted the line, so privateRequested reads its options as
		// Parse did (TestPrivateRequestedAgreesWithTheParser), also for
		// the help of a command.
		o := Outcome{Private: privateRun(args, inv)}
		var help string
		switch {
		case inv.Cmd != nil:
			o.Command, help = inv.Cmd.Name, Help(inv.Cmd)
		case inv.Group != "":
			o.Command, help = groupWords(args), inv.Group
		default:
			help = Usage()
		}
		if _, err := io.WriteString(stdout, help); err != nil {
			return o.WriteFailed("help", err)
		}
		return o
	}
	private := privateRun(args, inv)
	if err := refusePageURLEnv(inv); err != nil {
		return Outcome{Command: inv.Cmd.Name, Code: ExitCodeOf(err), Private: private, Message: fmt.Sprintf("Error: %v", err)}
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout(inv.Cmd, inv.Input.Global))
	defer cancel()
	var reporter DeadlineReporter
	if start != nil {
		reporter = start(ctx, inv)
	}
	// fail is the end of a stage that failed with err: its exit code and
	// line (the error line), the reporter's deadline message in its place
	// when the deadline ended the stage, and no message after a signal's
	// cancellation. Private mode prints neither message (Outcome.Line):
	// the deadline message names the page, and an error line can.
	fail := func(err error, line string) Outcome {
		o := Outcome{Command: inv.Cmd.Name, Code: ExitCodeOf(err), Private: private, Message: line}
		if errors.Is(context.Cause(ctx), ErrInterrupted) {
			o.Message = ""
			return o
		}
		if reporter != nil {
			if msg, hit := reporter.DeadlineMessage(err); hit {
				o.Code, o.Message = ExitTimeout, msg
			}
		}
		return o
	}
	// write prints the result on stdout as this mode prints it: in private
	// mode what privateResult allows. It returns the status line of
	// private mode for a command that succeeded; it prints nothing on
	// stderr.
	write := func(res Result) (status string, err error) {
		if private {
			res, status = privateResult(inv.Cmd, res)
		}
		return status, writeResult(stdout, res, inv.Input.Global)
	}
	if inv.Cmd.ClientStep != nil {
		if err := inv.Cmd.ClientStep(ctx, &inv.Input, os.Stdin); err != nil {
			return fail(err, fmt.Sprintf("Error: %s: %v", inv.Cmd.Name, err))
		}
	}
	env, err := newEnv(ctx, inv)
	if err != nil {
		return fail(err, fmt.Sprintf("Error: %v", err))
	}
	res, err := run(ctx, env, inv.Cmd, inv.Input)
	hasResult := err == nil
	var wr *withResult
	if errors.As(err, &wr) {
		res, err, hasResult = wr.Result, wr.Err, true
	}
	var status string
	var werr error
	if hasResult {
		status, werr = write(res)
	}
	// The error of the command decides the exit code, then a result that
	// could not be written (exit 1), then the success; without private
	// mode the error of the write comes first in the message.
	o := Outcome{Command: inv.Cmd.Name, Code: ExitOK, Private: private, Status: status}
	if err != nil {
		o = fail(err, fmt.Sprintf("Error: %s: %v", inv.Cmd.Name, err))
	}
	if werr != nil {
		o = o.WriteFailed("result", werr)
	}
	return o
}

// groupWords returns the command words of args, a line that Parse read as
// the help of a command group: the words of that group (GroupHelp).
func groupWords(args []string) string {
	rest, _, _ := splitGlobals(args, nil)
	return strings.Join(commandWords(rest), " ")
}

// withResult is the error of a command that fails with a result to show
// (status: what it found and the iwdp log). RunCLIStarted writes Result
// like the result of a successful command, so the output flags apply,
// then reports Err with Err's exit code. Only a Local command may return
// it: the daemon sends no result with an error.
type withResult struct {
	Result Result
	Err    error
}

func (e *withResult) Error() string { return e.Err.Error() }
func (e *withResult) Unwrap() error { return e.Err }

// pageURLEnv is the environment variable with which earlier iwdp-cli
// versions selected the page of a command by its WebSocket URL.
const pageURLEnv = "IWDP_WS_URL"

// refusePageURLEnv refuses a page command (one whose Page is not NoPage)
// without --page while IWDP_WS_URL is set: earlier versions ran it on the
// page that the variable names, so running it on the session's page
// instead could act on another page than the caller expects. The usage
// error (exit 2) names --page; an explicit --page or --only-tab wins over
// the variable.
func refusePageURLEnv(inv *Invocation) error {
	if inv.Cmd.Page == NoPage || inv.Input.Global.Page != "" || inv.Input.Global.OnlyTab || os.Getenv(pageURLEnv) == "" {
		return nil
	}
	return &UsageError{Msg: fmt.Sprintf("%s: %s no longer selects the page; pass the page with --page <tab id or ws:// URL>, or unset %s", inv.Cmd.Name, pageURLEnv, pageURLEnv)}
}

// DeadlineReporter explains a deadline error of a command run by
// RunCLIStarted (the CLI's deadline message: the command, its page, the
// elapsed time, the last wait and the likely causes). When
// DeadlineMessage reports that the command's deadline ended a stage
// (hit), RunCLIStarted prints msg instead of its own error line and
// returns ExitTimeout.
type DeadlineReporter interface {
	DeadlineMessage(err error) (msg string, hit bool)
}

// readWithin runs read, which may block without a deadline (a terminal,
// or a pipe whose writer stays open), in a goroutine, and returns its
// result; when ctx ends first, it returns ctx's error and drops what was
// read. The goroutine ends when read returns, at the latest when the
// process exits.
func readWithin(ctx context.Context, read func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1) // the goroutine never blocks on the send
	go func() {
		s, err := read()
		done <- result{s, err}
	}()
	select {
	case res := <-done:
		return res.s, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// run runs c in env: through RunRemote when env is a RemoteEnv and c is
// not Local, else in this process.
func run(ctx context.Context, env Env, c *Command, in Input) (Result, error) {
	if re, ok := env.(RemoteEnv); ok && !c.Local {
		return re.RunRemote(ctx, c, in)
	}
	return c.Run(ctx, env, in)
}

// Argv renders a parsed command back into arguments that Parse accepts
// (global flags excluded): the command words, each flag as --name=value in
// name order, then "--" and the positional arguments. The daemon parses it
// with the same spec (design 2.2, /v1/run).
func Argv(c *Command, in Input) []string {
	out := strings.Fields(c.Name)
	names := make([]string, 0, len(in.Flags))
	for n := range in.Flags {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, fmt.Sprintf("--%s=%v", n, in.Flags[n]))
	}
	if len(in.Args) > 0 {
		out = append(append(out, "--"), in.Args...)
	}
	return out
}

// writeResult prints the result: its JSON with --json, else its text,
// truncated to --max-output. With --content-boundaries the output is marked
// as page content (output.go); the markers go around the truncated text.
func writeResult(w io.Writer, res Result, g Globals) error {
	out := res.Text
	var mark boundary
	if g.ContentBoundaries {
		mark = newBoundary(res.Data)
	}
	if g.JSON {
		v := res.Data
		if v == nil {
			v = map[string]string{"text": res.Text}
		}
		if g.ContentBoundaries {
			v = mark.markJSON(v)
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
	if g.ContentBoundaries && !g.JSON && out != "" {
		out = mark.wrapText(out)
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err := io.WriteString(w, out)
	return err
}
