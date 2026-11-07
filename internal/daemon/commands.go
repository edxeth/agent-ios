package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
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
		return nil, errors.New("the daemon commands run in iwdp-cli and iwdp-mcp")
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
		{Name: "idle", Type: commands.FlagDuration, Help: "exit after this long without requests and leases (default IWDP_DAEMON_IDLE, else 15m)"},
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
				return commands.Result{Text: "The iwdp daemon is not running."}, nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			defer c.Close()
			if err := c.Shutdown(ctx, false); err != nil {
				return commands.Result{}, err
			}
			if err := c.WaitGone(ctx, stopWait); err != nil {
				return commands.Result{}, err
			}
			return commands.Result{Text: fmt.Sprintf("Stopped the iwdp daemon (pid %d).", c.Hello.PID)}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon status", Local: true,
		Summary: "Show the daemon, its pages, leases and device reservations.",
		Run: func(ctx context.Context, env commands.Env, _ commands.Input) (commands.Result, error) {
			c, err := connectRunning(ctx, env)
			if errors.Is(err, ErrNotRunning) {
				return commands.Result{Text: "The iwdp daemon is not running.", Data: map[string]bool{"running": false}}, nil
			}
			if err != nil {
				return commands.Result{}, err
			}
			defer c.Close()
			st, err := c.Status(ctx)
			if err != nil {
				return commands.Result{}, err
			}
			return commands.Result{Text: statusText(c, st), Data: st}, nil
		},
	})
	commands.Register(&commands.Command{
		Name: "daemon logs", Local: true,
		Flags:   []commands.Flag{{Name: "lines", Short: "n", Type: commands.FlagInt, Default: logTail, Help: "number of lines from the end"}},
		Summary: "Print the end of the daemon log ($XDG_STATE_HOME/iwdp/daemon.log).",
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
	return ro
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
	return fmt.Sprintf("iwdp daemon %s (protocol %d) pid %d, started %s, socket %s", h.Version, h.Protocol, h.PID, h.StartedAt.Format(time.RFC3339), SocketPath(c.Dir()))
}

func statusText(c *Client, st Status) string {
	var b strings.Builder
	b.WriteString(helloText(c) + "\n")
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
	return b.String()
}
