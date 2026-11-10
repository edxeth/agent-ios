package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
)

// daemonSpawnEnv is added to the environment of a daemon this process
// starts. Tests use it; it is empty otherwise.
var daemonSpawnEnv []string

// daemonStartIWDP starts ios_webkit_debug_proxy from the daemon when iwdp
// is selected and not running (decision D10: as the legacy auto-start, with
// the first UDID pinned to 9224; a running iwdp is never stopped or
// restarted). Tests set it to nil.
var daemonStartIWDP = proxy.Start

func daemonOptions() daemon.Options {
	return daemon.Options{Version: daemon.BuildVersion(), Env: daemonSpawnEnv, StartIWDP: daemonStartIWDP}
}

// cleanupGrace is how long past daemon.ShutdownBudget an exit path waits
// for a cleanup that does not end at its context's deadline.
const cleanupGrace = time.Second

// exits ends this process. Under IWDP_NO_DAEMON=1 it first runs the cleanup
// of the in-process core (design 2.4): leases end with the reset of the
// page state the command set, within daemon.ShutdownBudget. Without an
// in-process core the cleanup does nothing.
var exits = &processExit{cleanup: daemon.CloseInProcess, budget: daemon.ShutdownBudget, grace: cleanupGrace, stderr: os.Stderr, exit: os.Exit}

// processExit is the one way out of the process for main, the signal
// handler of cleanupOnSignal and the deadline watchdog (armWatchdog). The
// cleanup runs once; whoever starts it, every other path waits for it, so no
// path exits while another one still sends the page resets
// (daemon.CloseInProcess takes the core, and a second call would return at
// once). The wait is bounded (window): budget is the cleanup's context
// deadline, and a cleanup that ignores it is given up budget+grace after it
// started. A command deadline (bound) shortens both, so that the process
// ends within that deadline plus deadlineGrace.
//
// The exit code is decided after that wait, by the first path that gets
// there: 130 when a signal arrived (interrupted), else 124 when the watchdog
// fired (timedOut), else the path's own code.
type processExit struct {
	cleanup       func(context.Context) error
	budget, grace time.Duration
	stderr        io.Writer
	exit          func(int)

	// interrupted is set by the signal handler before it starts the
	// cleanup. Starting it ends the command's calls in flight ("context
	// canceled"), so the command can fail or end while the cleanup runs;
	// its exit is then still 130.
	interrupted atomic.Bool
	// cancelRun, when set, cancels the context of the running registry
	// command with commands.ErrInterrupted after a signal, so that
	// commands.RunCLIStarted prints no error for the call that the signal ended.
	// It is set before the signal handler starts (runRegistry).
	cancelRun context.CancelCauseFunc
	// timedOut is set by the watchdog: the command or its cleanup ran past
	// the command's deadline plus watchdogGrace.
	timedOut atomic.Bool

	// limit is the command's deadline in Unix nanoseconds (bound); 0
	// when none is set.
	limit atomic.Int64

	once     sync.Once
	done     chan struct{} // closed when the cleanup ended
	started  time.Time     // when the cleanup started
	deadline time.Time     // when waiters give up on the cleanup

	mu     sync.Mutex // held by the exiting path; os.Exit never returns it
	exited bool       // only an exit of a test returns
}

// closeWait is the part of deadlineGrace that the cleanup leaves for
// closing the page connections and exiting (window), and how long an exit
// path that comes after the command's deadline plus deadlineGrace (the
// watchdog) still waits for the cleanup, whose context has ended then.
const closeWait = 100 * time.Millisecond

// bound makes the cleanup and the waits for it end by the command's
// deadline plus deadlineGrace (window). The CLI sets it for a command of
// IWDP_NO_DAEMON=1 (startRegistry).
func (p *processExit) bound(deadline time.Time) { p.limit.Store(deadline.UnixNano()) }

// window returns when the cleanup's context ends (ctxEnd) and when the exit
// paths stop waiting for it (giveUp), for a cleanup that starts at now.
// Without a command deadline the cleanup gets budget and the waiters
// budget+grace. With one (bound), the cleanup gets the rest of the
// command's budget and the first part of deadlineGrace (up to closeWait
// before its end), so that a command that ran to its deadline still sends
// its resets; the waiters give up at the deadline plus deadlineGrace
// (closeWait after now when that has passed).
//
// When the cleanup's context ends, daemon.CloseInProcess stops sending
// resets and closes the page connections, and the exit closes them in
// any case. Closing the connection is expected to end the inspector
// session; whether iwdp passes the close on to WebKit as a frontend
// disconnect was not checked. When the last inspector frontend
// disconnects, current WebKit disables the Console, Network and Page
// agents and resets the Page overrides; this was not checked in the
// WebKit of an iOS release. Sources, WebKit main at commit adfcb278:
// PageInspectorController::disconnectFrontend
// (Source/WebCore/inspector/PageInspectorController.cpp) and
// WebPageInspectorController::disconnectFrontend
// (Source/WebKit/UIProcess/Inspector/WebPageInspectorController.cpp) call
// willDestroyFrontendAndBackend on the agents only when the last frontend
// leaves, so a page with another inspector attached keeps the domains;
// InspectorConsoleAgent, InspectorNetworkAgent and InspectorPageAgent
// disable themselves there, and InspectorPageAgent::disable resets the
// user agent, emulated media, color scheme and settings overrides.
// Changes that the commands made to the document itself (an evaluated
// script, the injected runtime) stay until the page reloads.
func (p *processExit) window(now time.Time) (ctxEnd, giveUp time.Time) {
	ctxEnd, giveUp = now.Add(p.budget), now.Add(p.budget+p.grace)
	if n := p.limit.Load(); n != 0 {
		limit := time.Unix(0, n)
		last := limit.Add(deadlineGrace)
		if end := last.Add(-closeWait); end.Before(ctxEnd) {
			ctxEnd = end
		}
		if last.Before(now.Add(closeWait)) {
			last = now.Add(closeWait)
		}
		if last.Before(giveUp) {
			giveUp = last
		}
	}
	return ctxEnd, giveUp
}

// awaitCleanup starts the cleanup unless it started, then waits until it
// ended or its bounded wait (window) is over. It reports whether the
// cleanup ended.
func (p *processExit) awaitCleanup() bool {
	p.once.Do(func() {
		p.done = make(chan struct{})
		p.started = time.Now()
		var ctxEnd time.Time
		ctxEnd, p.deadline = p.window(p.started)
		go func() {
			defer close(p.done)
			ctx, cancel := context.WithDeadline(context.Background(), ctxEnd)
			defer cancel()
			if err := p.cleanup(ctx); err != nil {
				note := ""
				if errors.Is(err, context.DeadlineExceeded) {
					note = " (it did not finish within its budget; the page connection closes)"
				}
				_, _ = fmt.Fprintf(p.stderr, "warning: cleanup of the page state: %v%s\n", err, note)
			}
		}()
	})
	t := time.NewTimer(time.Until(p.deadline))
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

// finish waits for the cleanup (awaitCleanup) and exits: 130 after a
// signal, 124 with the deadline error after the watchdog fired, else with
// the code that report returns after it printed the path's own outcome.
func (p *processExit) finish(report func() int) {
	ended := p.awaitCleanup()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	p.exited = true
	if !ended {
		_, _ = fmt.Fprintf(p.stderr, "warning: the cleanup of the page state did not end within %s; exiting without it\n", p.deadline.Sub(p.started).Round(time.Millisecond))
	}
	switch {
	case p.interrupted.Load():
		p.exit(exitInterrupted)
	case p.timedOut.Load():
		_, _ = fmt.Fprintln(p.stderr, current.timeoutMessage(errWatchdog))
		p.exit(exitTimeout)
	default:
		p.exit(report())
	}
}

// deadlineExpired is the watchdog: the command did not end by its deadline
// plus watchdogGrace. It never preempts a running cleanup: it starts the
// cleanup unless another path did, waits for it and exits 124 (130 when a
// signal arrived meanwhile).
func (p *processExit) deadlineExpired() {
	p.timedOut.Store(true)
	p.finish(func() int { return exitTimeout })
}

// signalled ends the process for SIGINT or SIGTERM: the cleanup, then 130.
func (p *processExit) signalled() {
	p.interrupted.Store(true) // before the cleanup ends any call
	if p.cancelRun != nil {
		p.cancelRun(reg.ErrInterrupted)
	}
	p.finish(func() int { return exitInterrupted })
}

// cleanupOnSignal makes SIGINT or SIGTERM end a command of
// IWDP_NO_DAEMON=1 through exits.signalled: the cleanup, then exit 130.
func cleanupOnSignal() {
	if !daemon.NoDaemon() {
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sig {
			exits.signalled()
		}
	}()
}

// exitInterrupted is the exit code of a command ended by SIGINT or SIGTERM
// (128 + SIGINT, as shells report it).
const exitInterrupted = 130

// runRegistry runs a registry command: Local commands (the daemon
// lifecycle) here, every other one in the daemon through /v1/run.
func runRegistry(argv []string) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	exits.cancelRun = cancel
	return reg.RunCLIStarted(ctx, argv, os.Stdout, os.Stderr, startRegistry, func(ctx context.Context, inv *reg.Invocation) (reg.Env, error) {
		if inv.Cmd.Local {
			return cliHost{}, nil
		}
		c, err := daemon.Connect(ctx, daemonOptions())
		if err != nil {
			return nil, err
		}
		return &remoteEnv{c: c}, nil
	})
}

// startRegistry records a registry command for the CLI's deadline message
// (timeoutMessage) as soon as its deadline is fixed, so that a deadline
// error of every stage prints it: reading the standard input (fill
// --stdin), connecting to or starting the daemon, and the run. Every
// command gets it except "daemon run", which outlives any command
// deadline.
func startRegistry(ctx context.Context, inv *reg.Invocation) reg.DeadlineReporter {
	c := inv.Cmd
	switch {
	case c.Name == daemonRun:
		return nil
	case c.Local && iwdpCommands[c.Name]:
		// The iwdp commands get the deadline error and the watchdog:
		// their iwdp calls honor ctx, and the watchdog ends a call that
		// does not.
		current.watch(c.Name, ctx)
	case c.Local:
		// The other Local commands (the daemon lifecycle, the skills)
		// get the deadline error; every wait of theirs honors ctx, so
		// they need no watchdog.
		current.started(c.Name, ctx)
	case daemon.NoDaemon():
		// The command's page state lives in this process: it gets the
		// signal cleanup and the watchdog. Local commands never get them
		// ("daemon run" ends on signals itself).
		cleanupOnSignal()
		current.watch(c.Name, ctx)
		// The cleanup at the exit ends by the command's deadline plus
		// deadlineGrace.
		if dl, ok := ctx.Deadline(); ok {
			exits.bound(dl)
		}
	default:
		// The daemon applies the deadline to the operation; this process
		// only records the command.
		current.started(c.Name, ctx)
	}
	// The deadline message names the page: the --page value, else the
	// page that the daemon reports for the operation (RunRemote).
	current.setPage(inv.Input.Global.Page)
	return current
}

// cliHost is the Env of Local commands (daemon.Host).
type cliHost struct{}

func (cliHost) Page(context.Context) (*core.PageSession, error) {
	return nil, errors.New("this command runs without a page")
}
func (cliHost) Emit(line string)              { fmt.Println(line) }
func (cliHost) ResolvePath(p string) string   { return p }
func (cliHost) ClientOptions() daemon.Options { return daemonOptions() }
func (cliHost) RunOptions() daemon.RunOptions {
	return daemon.RunOptions{Version: daemon.BuildVersion(), StartIWDP: daemonStartIWDP}
}

// daemonRun is the one registry command without a command deadline: the
// daemon in the foreground, which ends on signals (startRegistry).
const daemonRun = "daemon run"

// iwdpCommands are the Local registry commands that talk to
// ios_webkit_debug_proxy; besides the deadline message they get the
// watchdog (startRegistry).
var iwdpCommands = map[string]bool{"status": true, "devices": true, "restart-iwdp": true}

// remoteEnv runs registry commands in the daemon (commands.RemoteEnv).
type remoteEnv struct {
	cliHost
	c *daemon.Client
}

// deadlineGrace is how long after the command's deadline the CLI still
// waits for the daemon's answer. It only delivers an answer: the daemon
// runs the operation under the command's absolute deadline
// (daemon.RunRequest.Deadline), so no page work starts after it. A result
// that the daemon finished before the deadline counts as success. The
// daemon's deadline error, which names what the command waited for (for
// example "Runtime.evaluate: no response"), arrives a moment after the
// CLI's deadline; the CLI prints it in its deadline message. When it does
// not arrive within deadlineGrace, the message names the CLI's own wait
// for the daemon.
const deadlineGrace = 200 * time.Millisecond

func (e *remoteEnv) RunRemote(ctx context.Context, c *reg.Command, in reg.Input) (reg.Result, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return reg.Result{}, fmt.Errorf("current directory: %w", err)
	}
	g := in.Global
	req := daemon.RunRequest{
		Argv: reg.Argv(c, in), Cwd: cwd, JSON: g.JSON,
		PageReq: daemon.PageReq{Transport: g.Transport, Page: g.Page},
		Owner:   daemon.Owner{Session: g.Session, Reservation: g.Reservation, WaitLeaseMs: int64(g.WaitLease / time.Millisecond), Takeover: g.Takeover},
	}
	if dl, ok := ctx.Deadline(); ok {
		// The daemon runs the operation under the command's own deadline.
		req.Deadline = dl
		// The request outlives the deadline by deadlineGrace; a cancel of
		// the command's context (a signal) still ends it at once.
		cmdCtx := ctx
		runCtx, cancel := context.WithDeadline(context.WithoutCancel(cmdCtx), dl.Add(deadlineGrace))
		defer cancel()
		stop := context.AfterFunc(cmdCtx, func() {
			if !errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
				cancel()
			}
		})
		defer stop()
		ctx = runCtx
	}
	onPage := func(p daemon.PageInfo) {
		if g.Page == "" {
			// The page title goes only into error messages, never on
			// every command: titles must not land in agent transcripts
			// and logs.
			current.setPage(fmt.Sprintf("%q (%s)", p.Title, p.Key))
		}
	}
	res, err := e.c.RunWithPage(ctx, req, func(line string) { fmt.Println(line) }, onPage)
	if err != nil {
		return reg.Result{}, err
	}
	out := reg.Result{Text: res.Text}
	if len(res.Data) > 0 {
		out.Data = res.Data
	}
	return out, nil
}
