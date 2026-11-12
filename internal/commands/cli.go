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
	g.Reservation = env("IWDP_DEVICE_RESERVATION")
	if v := env("IWDP_TIMEOUT"); v != "" {
		d, err := parseTimeout("IWDP_TIMEOUT", v)
		if err != nil {
			return nil, &UsageError{err.Error()}
		}
		g.Timeout = d
	}
	rest, help, err := splitGlobals(args, &g)
	if err != nil {
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
	b.WriteString("\nGlobal flags: --session, --page, --timeout, --json, --transport, --max-output, --content-boundaries,\n  --reservation, --wait-lease, --takeover\n")
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
// caller exits 130 for the signal; RunCLIStarted prints no error for a
// command that the signal ended.
var ErrInterrupted = errors.New("interrupted by a signal")

// RunCLIStarted parses args and runs the command. Help and usage errors
// are handled before newEnv is called, so they never connect to anything.
// The command deadline (commandTimeout; an earlier deadline of ctx stays)
// is fixed before newEnv, so it covers building the environment and the
// run. start, when not nil, is called once the command's deadline is
// fixed, before the command's ClientStep and newEnv. The DeadlineReporter that start returns (nil: none) explains a
// deadline error of every later stage: the ClientStep (for example a
// read of the standard input), building the environment (for example
// connecting to or starting the daemon) and the run.
func RunCLIStarted(ctx context.Context, args []string, stdout, stderr io.Writer, start func(context.Context, *Invocation) DeadlineReporter, newEnv func(context.Context, *Invocation) (Env, error)) int {
	inv, err := Parse(args, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
		return ExitCodeOf(err)
	}
	if inv.Help || inv.Cmd == nil {
		if inv.Cmd != nil {
			_, _ = fmt.Fprint(stdout, Help(inv.Cmd))
		} else if inv.Group != "" {
			_, _ = fmt.Fprint(stdout, inv.Group)
		} else {
			_, _ = fmt.Fprint(stdout, Usage())
		}
		return ExitOK
	}
	if err := refusePageURLEnv(inv); err != nil {
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err) // nothing to do when stderr fails
		return ExitCodeOf(err)
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout(inv.Cmd, inv.Input.Global))
	defer cancel()
	var reporter DeadlineReporter
	if start != nil {
		reporter = start(ctx, inv)
	}
	// fail prints the error of a stage and returns its exit code: nothing
	// after a signal's cancellation, the reporter's deadline message when
	// the deadline ended the stage, else line (the error line).
	fail := func(err error, line string) int {
		if errors.Is(context.Cause(ctx), ErrInterrupted) {
			return ExitCodeOf(err)
		}
		if reporter != nil {
			if msg, hit := reporter.DeadlineMessage(err); hit {
				_, _ = fmt.Fprintln(stderr, msg) // nothing to do when stderr fails
				return ExitTimeout
			}
		}
		_, _ = fmt.Fprintln(stderr, line) // nothing to do when stderr fails
		return ExitCodeOf(err)
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
	var wr *withResult
	if errors.As(err, &wr) {
		if werr := writeResult(stdout, wr.Result, inv.Input.Global); werr != nil {
			_, _ = fmt.Fprintf(stderr, "Error: writing the result: %v\n", werr) // nothing to do when stderr fails; Err follows
		}
		err = wr.Err
	}
	if err != nil {
		return fail(err, fmt.Sprintf("Error: %s: %v", inv.Cmd.Name, err))
	}
	if err := writeResult(stdout, res, inv.Input.Global); err != nil {
		_, _ = fmt.Fprintf(stderr, "Error: writing the result: %v\n", err)
		return ExitError
	}
	return ExitOK
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
// error (exit 2) names --page; an explicit --page wins over the variable.
func refusePageURLEnv(inv *Invocation) error {
	if inv.Cmd.Page == NoPage || inv.Input.Global.Page != "" || os.Getenv(pageURLEnv) == "" {
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
