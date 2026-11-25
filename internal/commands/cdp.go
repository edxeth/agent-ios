package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/edxeth/agent-ios/internal/cdp"
)

// cdp start|stop|status|log: the CDP endpoint of the daemon (design
// docs/design/agentic-parity.md, sections 4.2 and 5.1). The endpoint is
// opt-in (decision D3). No output of these commands carries the token or
// an endpoint id; only get cdp-url hands out URLs.
func init() {
	Register(&Command{
		Name:    "cdp start",
		Summary: "Start the CDP endpoint on 127.0.0.1 (a running endpoint is kept); then run 'get cdp-url' for a client URL.",
		Flags: []Flag{
			{Name: "port", Type: FlagInt, Help: "port on 127.0.0.1 (default: the daemon's --cdp-port, else 9230; 0 picks a free port)"},
			{Name: "allow-origin", Type: FlagString, Help: "comma-separated Origin header values to accept (default: none, every request with an Origin header is refused)"},
		},
		Run: runCDPStart,
	})
	Register(&Command{
		Name:    "cdp stop",
		Summary: "Stop the CDP endpoint: close its client connections (each releases its state) and revoke every CDP URL.",
		Run: func(_ context.Context, env Env, _ Input) (Result, error) {
			srv, err := cdpOf(env)
			if err != nil {
				return Result{}, err
			}
			if !srv.Running() {
				return Result{Text: "The CDP endpoint is not running.", Data: map[string]any{"stopped": false}}, nil
			}
			records, conns := srv.Stop()
			return Result{
				Text: fmt.Sprintf("Stopped the CDP endpoint: %d endpoint record(s) revoked, %d connection(s) closed.", records, conns),
				Data: map[string]any{"stopped": true, "records": records, "connections": conns},
			}, nil
		},
	})
	Register(&Command{
		Name:    "cdp status",
		Summary: "Show the CDP endpoint: address, allowed origins and, per endpoint record, its session, page, scope, connections and attachments.",
		Run: func(_ context.Context, env Env, _ Input) (Result, error) {
			srv, err := cdpOf(env)
			if err != nil {
				return Result{}, err
			}
			st := srv.Status()
			return Result{Text: cdpStatusText(st), Data: st}, nil
		},
	})
	Register(&Command{
		Name:    "cdp log",
		Summary: "Show how often CDP clients sent methods that the endpoint does not translate (answered \"wasn't found\", never forwarded).",
		Run: func(_ context.Context, env Env, _ Input) (Result, error) {
			srv, err := cdpOf(env)
			if err != nil {
				return Result{}, err
			}
			counts := srv.Unknown()
			if len(counts) == 0 {
				return Result{Text: "No unknown CDP methods were received.", Data: counts}, nil
			}
			lines := []string{"Unknown CDP methods (count, method):"}
			for _, u := range counts {
				lines = append(lines, fmt.Sprintf("%6d  %s", u.Count, u.Method))
			}
			return Result{Text: strings.Join(lines, "\n"), Data: counts}, nil
		},
	})
}

// cdpOf returns the daemon's CDP endpoint from env.
func cdpOf(env Env) (*cdp.Server, error) {
	if ce, ok := env.(interface{ CDP() *cdp.Server }); ok && ce.CDP() != nil {
		return ce.CDP(), nil
	}
	return nil, errNeedsDaemon
}

// revokeCDP ends the CDP endpoint records of session when the session
// ends (close, session release; design 5.2): their URLs get 404 and their
// connections close after releasing their state. It returns the number of
// records; outside the daemon there are none.
func revokeCDP(env Env, session string) int {
	srv, err := cdpOf(env)
	if err != nil {
		return 0
	}
	return srv.RevokeSession(session)
}

func runCDPStart(_ context.Context, env Env, in Input) (Result, error) {
	srv, err := cdpOf(env)
	if err != nil {
		return Result{}, err
	}
	var origins []string
	if s, _ := in.Flags["allow-origin"].(string); s != "" {
		origins = strings.Split(s, ",")
	}
	var addr string
	if port, ok := in.Flags["port"].(int); ok {
		addr, err = srv.Start(port, origins)
	} else {
		addr, err = srv.StartDefault(origins)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{
		Text: fmt.Sprintf("CDP endpoint listening on %s. Run 'agent-ios get cdp-url' for a client URL.", addr),
		Data: map[string]any{"addr": addr},
	}, nil
}

func cdpStatusText(st cdp.Status) string {
	if !st.Listening {
		return "The CDP endpoint is not running; start it with 'agent-ios cdp start' or 'agent-ios get cdp-url'."
	}
	origins := "none"
	if len(st.AllowOrigins) > 0 {
		origins = strings.Join(st.AllowOrigins, ", ")
	}
	lines := []string{
		fmt.Sprintf("CDP endpoint listening on %s (allowed origins: %s), %d connection(s)", st.Addr, origins, st.Connections),
		fmt.Sprintf("Endpoint records: %d", len(st.Endpoints)),
	}
	for _, e := range st.Endpoints {
		lines = append(lines, fmt.Sprintf("  session %q  page %q  scope %s  %d connection(s), %d attachment(s)", e.Session, e.Page, e.Scope, e.Connections, e.Attachments))
	}
	return strings.Join(lines, "\n")
}
