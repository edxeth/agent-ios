package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edxeth/agent-ios/internal/commands"
)

// Host is what a binary tells the daemon commands: how to reach and start
// its daemon. The CLI's Env for Local commands implements it. (These
// commands live here, not in internal/commands, because this package
// imports commands.)
type Host interface {
	ClientOptions() Options
	RunOptions() RunOptions
}

func hostOf(env commands.Env) (Host, error) {
	h, ok := env.(Host)
	if !ok {
		return nil, errors.New("the daemon commands run in agent-ios")
	}
	return h, nil
}

// logTail is the default number of lines of daemon logs.
const logTail = "50"

// The daemon lifecycle commands (design 2.3). They are Local: they run in
// the client, since they start, inspect and stop the daemon itself.
func init() {
	// The global --transport (iwdp, direct or webkitgtk; auto = iwdp) picks
	// the transport for pages requested with --transport auto.
	transportFlags := []commands.Flag{
		{Name: "addr", Type: commands.FlagString, Help: "listing or inspector address of the --transport (webkitgtk: the WEBKIT_INSPECTOR_HTTP_SERVER address)"},
		{Name: "idle", Type: commands.FlagDuration, Help: "exit after this long without requests and leases (default AGENT_IOS_DAEMON_IDLE, else 15m)"},
		{Name: "cdp-port", Type: commands.FlagInt, Help: "start the CDP endpoint on 127.0.0.1:<port> with the daemon, and use the port for later starts (default: the endpoint starts only on 'cdp start' or 'get cdp-url', on port 9230)"},
	}
	commands.Register(&commands.Command{
		Name: "daemon run", Local: true, Flags: transportFlags,
		Summary: "Run the daemon in the foreground until SIGTERM, 'daemon stop' or the idle timeout (clients start it on demand).",
		Run: func(ctx context.Context, env commands.Env, in commands.Input) (commands.Result, error) {
			h, err := hostOf(env)
			if err != nil {
				return commands.Result{}, err
			}
			ro := runOptions(h.RunOptions(), in)
			// The daemon outlives any command deadline; signals end it.
			sctx, stop := signal.NotifyContext(context.WithoutCancel(ctx), syscall.SIGTERM, os.Interrupt)
			defer stop()
			if err := Run(sctx, ro); err != nil {
				return commands.Result{}, err
			}
			return commands.Result{}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon start", Local: true, Flags: transportFlags,
		Summary: "Start the daemon in the background unless it runs.",
		Run: func(ctx context.Context, env commands.Env, in commands.Input) (commands.Result, error) {
			h, err := hostOf(env)
			if err != nil {
				return commands.Result{}, err
			}
			o := h.ClientOptions()
			o.Args = []string{"daemon", "run", "--transport", daemonTransport(in)}
			if a, _ := in.Flags["addr"].(string); a != "" {
				o.Args = append(o.Args, "--addr", a)
			}
			if d, ok := in.Flags["idle"].(time.Duration); ok {
				o.Args = append(o.Args, "--idle", d.String())
			}
			if p, ok := in.Flags["cdp-port"].(int); ok {
				o.Args = append(o.Args, "--cdp-port", strconv.Itoa(p))
			}
			c, err := Connect(ctx, o)
			if err != nil {
				return commands.Result{}, err
			}
			defer c.Close()
			return commands.Result{Text: helloText(c), Data: c.Hello}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon stop", Local: true,
		Summary: "Stop the daemon after the cleanup of every page (5 s at most).",
		Run: func(ctx context.Context, env commands.Env, _ commands.Input) (commands.Result, error) {
			c, err := connectRunning(ctx, env)
			if errors.Is(err, ErrNotRunning) {
				return commands.Result{Text: "The agent-ios daemon is not running."}, nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			defer c.Close()
			// A request that the daemon closes without an answer
			// (notRunning) means it is going away already: wait for it
			// as after its answer.
			if err := c.Shutdown(ctx, false); err != nil && !notRunning(err) {
				return commands.Result{}, err
			}
			if err := c.WaitGone(ctx, stopWait); err != nil {
				return commands.Result{}, err
			}
			return commands.Result{Text: fmt.Sprintf("Stopped the agent-ios daemon (pid %d).", c.Hello.PID)}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon status", Local: true,
		Summary: "Show the daemon, its pages, leases and device reservations.",
		Run: func(ctx context.Context, env commands.Env, _ commands.Input) (commands.Result, error) {
			c, err := connectRunning(ctx, env)
			if errors.Is(err, ErrNotRunning) {
				return notRunningStatus(), nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			defer c.Close()
			st, err := c.Status(ctx)
			if notRunning(err) { // the daemon went away after its hello
				return notRunningStatus(), nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			return commands.Result{Text: statusText(c, st), Data: st}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon logs", Local: true,
		Flags:   []commands.Flag{{Name: "lines", Short: "n", Type: commands.FlagInt, Default: logTail, Help: "number of lines from the end"}},
		Summary: "Print the end of the daemon log ($XDG_STATE_HOME/agent-ios/daemon.log).",
		Run: func(_ context.Context, _ commands.Env, in commands.Input) (commands.Result, error) {
			p, err := LogPath()
			if err != nil {
				return commands.Result{}, err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return commands.Result{}, fmt.Errorf("reading the daemon log: %w", err)
			}
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if n := in.Flags["lines"].(int); n >= 0 && len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			return commands.Result{Text: strings.Join(lines, "\n")}, nil
		},
	})
}

// daemonTransport is the daemon's transport for "auto" requests.
func daemonTransport(in commands.Input) string {
	if t := in.Global.Transport; t != "" && t != "auto" {
		return t
	}
	return "iwdp"
}

func runOptions(ro RunOptions, in commands.Input) RunOptions {
	ro.Transport = daemonTransport(in)
	if a, _ := in.Flags["addr"].(string); a != "" {
		ro.Addr = a
	}
	if d, ok := in.Flags["idle"].(time.Duration); ok {
		ro.Idle = d
	}
	if p, ok := in.Flags["cdp-port"].(int); ok {
		ro.CDPPort = p
	}
	return ro
}

// notRunningStatus is the result of 'daemon status' when no daemon answers.
func notRunningStatus() commands.Result {
	return commands.Result{Text: "The agent-ios daemon is not running.", Data: map[string]bool{"running": false}}
}

func connectRunning(ctx context.Context, env commands.Env) (*Client, error) {
	h, err := hostOf(env)
	if err != nil {
		return nil, err
	}
	o := h.ClientOptions()
	o.NoStart = true
	return Connect(ctx, o)
}

func helloText(c *Client) string {
	h := c.Hello
	return fmt.Sprintf("agent-ios daemon %s (protocol %d) pid %d, started %s, socket %s", h.Version, h.Protocol, h.PID, h.StartedAt.Format(time.RFC3339), SocketPath(c.Dir()))
}

func statusText(c *Client, st Status) string {
	var b strings.Builder
	b.WriteString(helloText(c) + "\n")
	if p := c.Hello.Protocol; p != Protocol {
		fmt.Fprintf(&b, "This daemon is older: it speaks protocol %d, this agent-ios protocol %d. Other commands replace it when it has no leases; 'agent-ios daemon stop' stops it.\n", p, Protocol)
	}
	fmt.Fprintf(&b, "Pages: %d\n", len(st.Pages))
	for _, p := range st.Pages {
		lease := p.Lease
		if p.Session != "" {
			lease += " by session " + p.Session
		}
		fmt.Fprintf(&b, "  %s  %q  connection %s, lease %s\n", p.Key, p.Title, p.State, lease)
	}
	fmt.Fprintf(&b, "Device reservations: %d\n", len(st.Reservations))
	for _, r := range st.Reservations {
		fmt.Fprintf(&b, "  %s by %q until %s\n", r.Device, r.Owner, r.Expires.Format(time.RFC3339))
	}
	if st.ReservationsError != "" {
		fmt.Fprintf(&b, "  (reservations unreadable: %s)\n", st.ReservationsError)
	}
	if st.CDP.Listening {
		fmt.Fprintf(&b, "CDP endpoint: listening on %s, %d endpoint record(s), %d connection(s)\n", st.CDP.Addr, len(st.CDP.Endpoints), st.CDP.Connections)
	} else {
		b.WriteString("CDP endpoint: not running\n")
	}
	return b.String()
}
