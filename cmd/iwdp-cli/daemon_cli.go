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

// globals are the global options of the running legacy command.
var globals globalOptions

// owner is the session and ownership options sent with every page request.
func (o globalOptions) owner() daemon.Owner {
	return daemon.Owner{Session: o.session, Reservation: o.reservation, WaitLeaseMs: int64(o.waitLease / time.Millisecond), Takeover: o.takeover}
}

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

// processExit is the one way out of the process for main, fatal, the signal
// handler of cleanupOnSignal and the deadline watchdog (armWatchdog). The
// cleanup runs once; whoever starts it, every other path waits for it, so no
// path exits while another one still sends the page resets
// (daemon.CloseInProcess takes the core, and a second call would return at
// once). The wait is bounded: budget is the cleanup's context deadline, and
// a cleanup that ignores it is given up budget+grace after it started.
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
	// timedOut is set by the watchdog: the command or its cleanup ran past
	// the command's deadline plus watchdogGrace.
	timedOut atomic.Bool

	once     sync.Once
	done     chan struct{} // closed when the cleanup ended
	deadline time.Time     // when waiters give up on the cleanup

	mu     sync.Mutex // held by the exiting path; os.Exit never returns it
	exited bool       // only an exit of a test returns
}

// awaitCleanup starts the cleanup unless it started, then waits until it
// ended or its bounded wait is over. It reports whether the cleanup ended.
func (p *processExit) awaitCleanup() bool {
	p.once.Do(func() {
		p.done = make(chan struct{})
		p.deadline = time.Now().Add(p.budget + p.grace)
		go func() {
			defer close(p.done)
			ctx, cancel := context.WithTimeout(context.Background(), p.budget)
			defer cancel()
			if err := p.cleanup(ctx); err != nil {
				_, _ = fmt.Fprintf(p.stderr, "warning: cleanup of the page state: %v\n", err)
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
		_, _ = fmt.Fprintf(p.stderr, "warning: the cleanup of the page state did not end within %s; exiting without it\n", p.budget+p.grace)
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
	p.finish(func() int { return exitInterrupted })
}

// streamWaiting is set when a streaming command starts to wait for its end
// and stays set until the process exits: from then on the stream handles
// SIGINT and SIGTERM itself, ends with its own disable call and returns
// normally, and main exits through exits.finish. It is never cleared, so a
// signal that the stream already took cannot also start the cleanup of
// cleanupOnSignal while the stream still ends (that cleanup would close
// the core under the stream's disable call).
var streamWaiting atomic.Bool

// cleanupOnSignal makes SIGINT or SIGTERM end a command of
// IWDP_NO_DAEMON=1 through exits.signalled: the cleanup, then exit 130. A
// streaming command that waits (or waited) ends on the signal by itself,
// and main exits after it.
func cleanupOnSignal() {
	if !daemon.NoDaemon() {
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sig {
			if streamWaiting.Load() {
				continue
			}
			exits.signalled()
		}
	}()
}

// exitInterrupted is the exit code of a command ended by SIGINT or SIGTERM
// (128 + SIGINT, as shells report it).
const exitInterrupted = 130

// isRegistryCommand reports whether the command words of args name a
// registry command (design 2.11).
func isRegistryCommand(args []string) bool {
	c, _ := reg.Lookup(reg.CommandWords(args))
	return c != nil
}

// runRegistry runs a registry command: Local commands (the daemon
// lifecycle) here, every other one in the daemon through /v1/run.
func runRegistry(argv []string) int {
	return reg.RunCLI(context.Background(), argv, os.Stdout, os.Stderr, func(ctx context.Context, inv *reg.Invocation) (reg.Env, error) {
		if inv.Cmd.Local {
			return cliHost{}, nil
		}
		if daemon.NoDaemon() {
			// The command's page state lives in this process: it gets the
			// signal cleanup and the watchdog of legacy commands. Local
			// commands never get them ("daemon run" ends on signals itself).
			cleanupOnSignal()
			current.watch(inv.Cmd.Name, ctx)
		}
		c, err := daemon.Connect(ctx, daemonOptions())
		if err != nil {
			return nil, err
		}
		return &remoteEnv{c: c}, nil
	})
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

// remoteEnv runs registry commands in the daemon (commands.RemoteEnv).
type remoteEnv struct {
	cliHost
	c *daemon.Client
}

func (e *remoteEnv) RunRemote(ctx context.Context, c *reg.Command, in reg.Input) (reg.Result, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return reg.Result{}, fmt.Errorf("current directory: %w", err)
	}
	g := in.Global
	res, err := e.c.Run(ctx, daemon.RunRequest{
		Argv: reg.Argv(c, in), Cwd: cwd, JSON: g.JSON,
		PageReq: daemon.PageReq{Transport: g.Transport, Page: g.Page},
		Owner:   daemon.Owner{Session: g.Session, Reservation: g.Reservation, WaitLeaseMs: int64(g.WaitLease / time.Millisecond), Takeover: g.Takeover},
	}, func(line string) { fmt.Println(line) })
	if err != nil {
		return reg.Result{}, err
	}
	out := reg.Result{Text: res.Text}
	if len(res.Data) > 0 {
		out.Data = res.Data
	}
	return out, nil
}
