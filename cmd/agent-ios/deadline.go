package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
)

// exitTimeout is the exit code for a command that hit its deadline. It
// matches timeout(1), so callers can tell a stall from an ordinary error.
const exitTimeout = 124

// watchdogGrace is how long after its deadline a command may still run
// before the watchdog ends the process. Most waits honor the context and stop
// at the deadline; the watchdog covers the few that do not.
const watchdogGrace = 2 * time.Second

// timeoutEnv names the environment variable that sets the default deadline.
const timeoutEnv = "AGENT_IOS_TIMEOUT"

// commandRun describes the running command for deadline errors.
type commandRun struct {
	mu     sync.Mutex
	name   string
	page   string
	start  time.Time
	budget time.Duration // the whole-command deadline
	ctx    context.Context
}

var current = &commandRun{}

// watch records a registry command that runs in this process (a Local
// iwdp command, or any command under AGENT_IOS_NO_DAEMON=1) under ctx, the
// context with the command's deadline, and arms the watchdog
// (armWatchdog) shortly after that deadline. Without a deadline it does
// nothing.
func (r *commandRun) watch(name string, ctx context.Context) {
	if budget, ok := r.started(name, ctx); ok {
		armWatchdog(budget+watchdogGrace, exits)
	}
}

// started records a registry command that runs under ctx, the context
// with the command's deadline, for its deadline message (timeoutMessage):
// its name, its start, its deadline (budget) and ctx. It arms no watchdog:
// a command in the daemon gets the daemon's deadline. Without a deadline
// it records nothing and returns false.
func (r *commandRun) started(name string, ctx context.Context) (budget time.Duration, ok bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	budget = time.Until(deadline)
	r.mu.Lock()
	r.name, r.start, r.budget, r.ctx = name, time.Now(), budget, ctx
	r.mu.Unlock()
	return budget, true
}

// DeadlineMessage is the deadline error of the recorded command for
// commands.DeadlineReporter: the CLI's message (timeoutMessage) when err,
// or the command's context, shows that the deadline ended the command.
func (r *commandRun) DeadlineMessage(err error) (string, bool) {
	if !r.deadlineHit(err) {
		return "", false
	}
	return r.timeoutMessage(err), true
}

// setPage records the page the command talks to, for deadline errors.
func (r *commandRun) setPage(page string) {
	r.mu.Lock()
	r.page = page
	r.mu.Unlock()
}

// deadlineHit reports whether err, or the command context, shows that the
// command ran out of time. A command context that nothing canceled counts
// once the clock has reached its deadline, before its deadline timer has
// run: an error caused by the deadline (an expiry in the in-process core,
// or the daemon's answer) can arrive in that instant, and the exit code
// must not depend on which of them ran first.
func (r *commandRun) deadlineHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	r.mu.Lock()
	ctx := r.ctx
	r.mu.Unlock()
	if ctx == nil {
		return false
	}
	switch cerr := ctx.Err(); {
	case errors.Is(cerr, context.DeadlineExceeded):
		return true
	case cerr != nil:
		return false // canceled before its deadline, for example by a signal
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
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
	var b strings.Builder
	fmt.Fprintf(&b, "Error: agent-ios %s timed out after %s (deadline %s).\n", r.name, time.Since(r.start).Round(100*time.Millisecond), r.budget.Round(time.Millisecond))
	fmt.Fprintf(&b, "Page: %s\n", page)
	if cause != nil {
		fmt.Fprintf(&b, "Last wait: %v\n", cause)
	}
	b.WriteString("Likely causes:\n")
	if errors.Is(cause, core.ErrActionTimeout) {
		// The page answered every attempt of the action; the attempts
		// stopped for the reason in "Last wait", so a stalled page or
		// another inspector is not the likely cause.
		b.WriteString("  - the action's target did not pass its checks before the deadline; 'Last wait' names the last reason (for example: no element matches, not visible, disabled, covered by another element, still moving)\n")
		b.WriteString("  - the selector or ref does not match the current page: take a new snapshot ('snapshot -i') and use its refs\n")
		b.WriteString("If the element appears late by design, raise the deadline with --timeout <duration> or " + timeoutEnv + ".")
		return b.String()
	}
	b.WriteString("  - another inspector session holds the page (Safari Web Inspector or another agent-ios such as 'console --follow')\n")
	b.WriteString("  - the page is stalled (long-running JavaScript, or an alert, confirm or prompt dialog is open)\n")
	b.WriteString("  - the device is locked or asleep, or Safari is in the background\n")
	b.WriteString("If the command is slow by design, raise the deadline with --timeout <duration> or " + timeoutEnv + ".")
	return b.String()
}

// errWatchdog is the cause reported when the watchdog ends a command.
var errWatchdog = errors.New("the command or its cleanup of the page state did not end by its deadline, so the watchdog ended it")

// armWatchdog runs p.deadlineExpired after d: the process exits with
// exitTimeout and the deadline error after the in-process cleanup. It backs
// up the context deadline for waits that do not honor a context. It is a
// variable so that a test can see which commands arm it.
var armWatchdog = func(d time.Duration, p *processExit) *time.Timer {
	return time.AfterFunc(d, p.deadlineExpired)
}
