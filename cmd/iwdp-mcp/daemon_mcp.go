package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// pageClient is the selected page: the daemon-backed caller of
// select_page (design 2.10, decision D1), or a *webkit.Client in tests.
type pageClient interface {
	webkit.Caller
	Close() error
}

// daemonSpawnEnv is added to the environment of a daemon this process
// starts. Tests use it; it is empty otherwise.
var daemonSpawnEnv []string

// daemonStartIWDP starts ios_webkit_debug_proxy from the daemon when iwdp
// is selected and not running (decision D10, as in iwdp-cli). Tests set it
// to nil.
var daemonStartIWDP = proxy.Start

func mcpDaemonOptions() daemon.Options {
	return daemon.Options{Version: daemon.BuildVersion(), Env: daemonSpawnEnv, StartIWDP: daemonStartIWDP}
}

// mcpOwner is the MCP server's session and device reservation: IWDP_SESSION
// (default "default", D8) and IWDP_DEVICE_RESERVATION, read per call.
func mcpOwner() daemon.Owner {
	return daemon.Owner{Session: os.Getenv("IWDP_SESSION"), Reservation: os.Getenv("IWDP_DEVICE_RESERVATION")}
}

// attachPage connects to the daemon (started on demand) and leases the
// page of wsURL for the MCP session without holding an operation; each
// tool call then runs in its own operation (toolScope). The lease stays
// while the caller lives (HoldLease), as the direct connection did.
func attachPage(ctx context.Context, wsURL string) (pageClient, error) {
	c, err := daemon.Connect(ctx, mcpDaemonOptions())
	if err != nil {
		return nil, err
	}
	c.Logf = func(f string, a ...any) { log.Printf("warning: "+f, a...) }
	pc, err := c.AttachPage(ctx, daemon.PageOptions{
		Page:  daemon.PageReq{Transport: os.Getenv("IWDP_TRANSPORT"), Page: wsURL},
		Owner: mcpOwner(), Name: "select_page",
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	pc.HoldLease()
	return pc, nil
}

// dropEndedSelectionLocked drops the selected daemon page when it ended
// (lost, lease ended or released, daemon shut down or gone; design 2.6) and
// returns why, so the next tool call asks for select_page instead of
// using a caller without its heartbeat. The collectors of the page go with
// it, as on select_page. sess.mu is held.
func dropEndedSelectionLocked() error {
	pc, ok := sess.client.(*daemon.PageCaller)
	if !ok {
		return nil
	}
	select {
	case <-pc.Done():
	default:
		return nil
	}
	reason := pc.Err()
	_ = pc.Close()
	sess.client = nil
	sess.networkMonitor = nil
	sess.consoleCollector = nil
	sess.timelineCollector = nil
	sess.interceptionCollector = nil
	return fmt.Errorf("the selected page ended (%v); use select_page again", reason)
}

// toolScope runs one tool call as one exclusive operation on the selected
// page when it is daemon-backed (design 2.10); end closes it.
func toolScope(ctx context.Context) (context.Context, func()) {
	sess.mu.Lock()
	_ = dropEndedSelectionLocked() // getClient reports it
	pc, ok := sess.client.(*daemon.PageCaller)
	sess.mu.Unlock()
	if !ok {
		return ctx, func() {}
	}
	return pc.Scope(ctx)
}

// runDaemonCommand runs "iwdp-mcp daemon <run|start|stop|status|logs>":
// clients start the daemon as "<own executable> daemon run" (design 2.1).
func runDaemonCommand(argv []string) int {
	return commands.RunCLI(context.Background(), argv, os.Stdout, os.Stderr, func(_ context.Context, inv *commands.Invocation) (commands.Env, error) {
		if !inv.Cmd.Local {
			return nil, fmt.Errorf("iwdp-mcp runs only the daemon commands; use iwdp-cli %s", inv.Cmd.Name)
		}
		return mcpHost{}, nil
	})
}

// mcpHost is the Env of the daemon commands (daemon.Host).
type mcpHost struct{}

func (mcpHost) Page(context.Context) (*core.PageSession, error) {
	return nil, errors.New("this command runs without a page")
}
func (mcpHost) Emit(line string)              { fmt.Println(line) }
func (mcpHost) ResolvePath(p string) string   { return p }
func (mcpHost) ClientOptions() daemon.Options { return mcpDaemonOptions() }
func (mcpHost) RunOptions() daemon.RunOptions {
	return daemon.RunOptions{Version: daemon.BuildVersion(), StartIWDP: daemonStartIWDP}
}

// daemonCommands are the registry commands of this binary when it starts.
// They are the daemon's commands, since the daemon is this binary as
// "daemon run"; a command registered later (a test fixture) is unknown to
// the daemon.
var daemonCommands = commands.All()

// registryEnv runs the MCP server's registry tools in the daemon
// (commands.RemoteEnv) for the MCP session and its selected page.
type registryEnv struct{ mcpHost }

func (e registryEnv) RunRemote(ctx context.Context, c *commands.Command, in commands.Input) (commands.Result, error) {
	if !slices.Contains(daemonCommands, c) {
		return c.Run(ctx, e.mcpHost, in) // unknown to the daemon: runs here
	}
	if pc := directPage(); pc != nil {
		return c.Run(ctx, directPageEnv{mcpHost: e.mcpHost, caller: pc}, in)
	}
	dc, err := daemon.Connect(ctx, mcpDaemonOptions())
	if err != nil {
		return commands.Result{}, err
	}
	defer dc.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return commands.Result{}, fmt.Errorf("current directory: %w", err)
	}
	owner := mcpOwner()
	owner.WaitLeaseMs = int64(in.Global.WaitLease / time.Millisecond)
	owner.Takeover = in.Global.Takeover
	res, err := dc.Run(ctx, daemon.RunRequest{
		Argv: commands.Argv(c, in), Cwd: cwd, JSON: in.Global.JSON,
		PageReq: selectedPage(), Owner: owner,
	}, func(line string) { log.Print(line) })
	if err != nil {
		return commands.Result{}, err
	}
	out := commands.Result{Text: res.Text}
	if len(res.Data) > 0 {
		out.Data = res.Data
	}
	return out, nil
}

// directPage returns the selected page when it is a direct connection
// instead of a daemon page. select_page always attaches through the
// daemon; only the unit tests select a mock page directly (connectMCP).
// A registry tool that takes over a legacy tool (set_cookie) then runs in
// this process on that connection, as the legacy handler did.
func directPage() webkit.Caller {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.client == nil {
		return nil
	}
	if _, ok := sess.client.(*daemon.PageCaller); ok {
		return nil
	}
	return sess.client
}

// directPageEnv is the Env of a registry command on a directly connected
// page (directPage): the page session has only the caller, with no
// scheduler, desired state or runtime.
type directPageEnv struct {
	mcpHost
	caller webkit.Caller
}

func (e directPageEnv) Page(context.Context) (*core.PageSession, error) {
	return &core.PageSession{Caller: e.caller}, nil
}

// selectedPage is the page request of the selected page, or the
// transport's default page when none is selected.
func selectedPage() daemon.PageReq {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	_ = dropEndedSelectionLocked() // an ended page is no selection
	if pc, ok := sess.client.(*daemon.PageCaller); ok {
		return pc.PageReq()
	}
	return daemon.PageReq{Transport: os.Getenv("IWDP_TRANSPORT")}
}
