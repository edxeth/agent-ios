package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
)

// defaultCommandTimeout is the deadline of one CLI command when neither
// --timeout nor IWDP_TIMEOUT is set.
const defaultCommandTimeout = 30 * time.Second

// exitTimeout is the exit code for a command that hit its deadline. It
// matches timeout(1), so callers can tell a stall from an ordinary error.
const exitTimeout = 124

// watchdogGrace is how long after its deadline a command may still run
// before the watchdog ends the process. Most waits honor the context and stop
// at the deadline; the watchdog covers the few that do not.
const watchdogGrace = 2 * time.Second

// timeoutEnv names the environment variable that sets the default deadline.
const timeoutEnv = "IWDP_TIMEOUT"

// recordingDefaults lists the commands that record for a duration (-d or
// --duration) and their default duration. Their deadline is the timeout plus
// the recording duration.
var recordingDefaults = map[string]time.Duration{
	"console":          10 * time.Second,
	"network":          10 * time.Second,
	"console-messages": 3 * time.Second,
	"timeline-record":  3 * time.Second,
	"memory-track":     3 * time.Second,
	"heap-track":       3 * time.Second,
	"cpu-profile":      3 * time.Second,
	"script-profile":   3 * time.Second,
	"animation-track":  3 * time.Second,
}

// followCommands stream until interrupted when given --follow.
var followCommands = map[string]bool{"console": true, "network": true}

// globalOptions holds the flags that apply to every command.
type globalOptions struct {
	timeout    time.Duration
	timeoutSet bool // set by --timeout or IWDP_TIMEOUT, not the default
}

// parseGlobalOptions removes --timeout <d> and --timeout=<d> from args (at any
// position) and resolves the timeout: flag, then IWDP_TIMEOUT (envTimeout),
// then defaultCommandTimeout.
func parseGlobalOptions(args []string, envTimeout string) ([]string, globalOptions, error) {
	opts := globalOptions{timeout: defaultCommandTimeout}
	if envTimeout != "" {
		d, err := parsePositiveDuration(envTimeout)
		if err != nil {
			return nil, opts, fmt.Errorf("%s: %w", timeoutEnv, err)
		}
		opts.timeout, opts.timeoutSet = d, true
	}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		var value string
		switch {
		case a == "--timeout":
			if i+1 >= len(args) {
				return nil, opts, errors.New("--timeout needs a value, for example --timeout 10s")
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(a, "--timeout="):
			value = strings.TrimPrefix(a, "--timeout=")
		default:
			rest = append(rest, a)
			continue
		}
		d, err := parsePositiveDuration(value)
		if err != nil {
			return nil, opts, fmt.Errorf("--timeout: %w", err)
		}
		opts.timeout, opts.timeoutSet = d, true
	}
	return rest, opts, nil
}

// parsePositiveDuration accepts a Go duration ("10s", "1m30s") or a whole
// number of seconds ("45").
func parsePositiveDuration(v string) (time.Duration, error) {
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if parsed, err := time.ParseDuration(v); err == nil {
		d = parsed
	} else {
		return 0, fmt.Errorf("invalid duration %q: use a Go duration such as 10s or 2m, or whole seconds", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be greater than zero", v)
	}
	return d, nil
}

// recordingDuration removes -d <d>, --duration <d> and --duration=<d> from
// args and returns the duration, or the command's default from
// recordingDefaults.
func recordingDuration(command string, args []string) (time.Duration, []string, error) {
	d, ok := recordingDefaults[command]
	if !ok {
		return 0, nil, fmt.Errorf("%s does not record for a duration", command)
	}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		var value string
		switch {
		case a == "-d" || a == "--duration":
			if i+1 >= len(args) {
				return 0, nil, fmt.Errorf("%s needs a value, for example %s 10s", a, a)
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(a, "--duration="):
			value = strings.TrimPrefix(a, "--duration=")
		default:
			rest = append(rest, a)
			continue
		}
		parsed, err := parsePositiveDuration(value)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %w", a, err)
		}
		d = parsed
	}
	return d, rest, nil
}

// recordingDurationOrExit is recordingDuration for command handlers: a bad
// value is a usage error.
func recordingDurationOrExit(command string, args []string) (time.Duration, []string) {
	d, rest, err := recordingDuration(command, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return d, rest
}

// hasFlag reports whether args contain one of names.
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// commandBudget returns how long a command may run. unlimited is true only
// for console and network with --follow, which stream until interrupted;
// their connect and enable steps still use opts.timeout.
func commandBudget(command string, args []string, opts globalOptions) (budget time.Duration, unlimited bool, err error) {
	budget = opts.timeout
	if command == "heap-snapshot" && !opts.timeoutSet && tools.HeapSnapshotTimeout > budget {
		// Heap snapshots of heavy pages take minutes; see tools.HeapSnapshotTimeout.
		budget = tools.HeapSnapshotTimeout
	}
	if followCommands[command] && hasFlag(args, "--follow", "-f") {
		return 0, true, nil
	}
	if _, ok := recordingDefaults[command]; ok {
		d, _, err := recordingDuration(command, args)
		if err != nil {
			return 0, false, err
		}
		budget += d
	}
	return budget, false, nil
}

// commandRun describes the running command for deadline errors.
type commandRun struct {
	mu      sync.Mutex
	name    string
	page    string
	start   time.Time
	timeout time.Duration // the --timeout value
	budget  time.Duration // the whole-command deadline; 0 when unlimited
	ctx     context.Context
}

var current = &commandRun{}

// begin records the command and returns its context. With a budget, the
// context ends at the deadline and a watchdog ends the process shortly after
// if the command is still running.
func (r *commandRun) begin(name string, timeout, budget time.Duration, unlimited bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if !unlimited {
		ctx, cancel = context.WithTimeout(ctx, budget)
		armWatchdog(budget+watchdogGrace, os.Stderr, os.Exit)
	}
	r.mu.Lock()
	r.name, r.start, r.timeout, r.ctx = name, time.Now(), timeout, ctx
	if unlimited {
		r.budget = 0
	} else {
		r.budget = budget
	}
	r.mu.Unlock()
	return ctx, cancel
}

// setPage records the page the command talks to, for deadline errors.
func (r *commandRun) setPage(page string) {
	r.mu.Lock()
	r.page = page
	r.mu.Unlock()
}

// setupTimeout is the deadline for the connect and enable steps of a
// streaming command.
func (r *commandRun) setupTimeout() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timeout <= 0 {
		return defaultCommandTimeout
	}
	return r.timeout
}

// deadlineHit reports whether err, or the command context, shows that the
// command ran out of time.
func (r *commandRun) deadlineHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	r.mu.Lock()
	ctx := r.ctx
	r.mu.Unlock()
	return ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// timeoutMessage explains a deadline error: the command, the page, the
// elapsed time, the last wait and the likely causes.
func (r *commandRun) timeoutMessage(cause error) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	page := r.page
	if page == "" {
		page = "(none selected yet: still discovering pages through iwdp)"
	}
	deadline := "no whole-command deadline; step timeout " + r.timeout.String()
	if r.budget > 0 {
		deadline = "deadline " + r.budget.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Error: iwdp-cli %s timed out after %s (%s).\n", r.name, time.Since(r.start).Round(100*time.Millisecond), deadline)
	fmt.Fprintf(&b, "Page: %s\n", page)
	if cause != nil {
		fmt.Fprintf(&b, "Last wait: %v\n", cause)
	}
	b.WriteString("Likely causes:\n")
	b.WriteString("  - another inspector session holds the page (Safari Web Inspector, iwdp-mcp, or another iwdp-cli such as 'console --follow')\n")
	b.WriteString("  - the page is stalled (long-running JavaScript, or an alert, confirm or prompt dialog is open)\n")
	b.WriteString("  - the device is locked or asleep, or Safari is in the background\n")
	b.WriteString("If the command is slow by design, raise the deadline with --timeout <duration> or " + timeoutEnv + ".")
	return b.String()
}

// errWatchdog is the cause reported when the watchdog ends a command.
var errWatchdog = errors.New("the command did not stop at its deadline, so the watchdog ended it")

// armWatchdog ends the process with exitTimeout after d, printing the
// deadline error to w. It backs up the context deadline for waits that do
// not honor a context.
func armWatchdog(d time.Duration, w io.Writer, exit func(int)) *time.Timer {
	return time.AfterFunc(d, func() {
		fmt.Fprintln(w, current.timeoutMessage(errWatchdog))
		exit(exitTimeout)
	})
}
