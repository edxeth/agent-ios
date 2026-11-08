package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Session commands (design 4.2 row "session", decision D8).
func init() {
	Register(&Command{
		Name: "session", Summary: "Print the current session name (--session or IWDP_SESSION; default \"default\").",
		Run: func(_ context.Context, _ Env, in Input) (Result, error) {
			return Result{Text: in.Global.Session, Data: map[string]string{"session": in.Global.Session}}, nil
		},
	})
	Register(&Command{
		Name: "session list", Summary: "List the sessions that hold page leases in the daemon.",
		Run: func(_ context.Context, env Env, _ Input) (Result, error) {
			m, err := managerOf(env)
			if err != nil {
				return Result{}, err
			}
			counts := map[string]int{}
			for _, p := range m.Status() {
				if p.Session != "" {
					counts[p.Session]++
				}
			}
			names := make([]string, 0, len(counts))
			for n := range counts {
				names = append(names, n)
			}
			sort.Strings(names)
			var b strings.Builder
			for _, n := range names {
				fmt.Fprintf(&b, "%s\t%d page(s)\n", n, counts[n])
			}
			if len(names) == 0 {
				b.WriteString("No session holds a page lease.")
			}
			return Result{Text: b.String(), Data: counts}, nil
		},
	})
	Register(&Command{
		Name: "session info", MCPName: "session_info", Profile: ProfileCore,
		Summary: "Show this session's leased pages: page, lease state (active or lost), epoch and last use.",
		Run: func(_ context.Context, env Env, in Input) (Result, error) {
			m, err := managerOf(env)
			if err != nil {
				return Result{}, err
			}
			pages := sessionPages(m, in.Global.Session)
			var b strings.Builder
			fmt.Fprintf(&b, "Session %q: %d leased page(s)\n", in.Global.Session, len(pages))
			for _, p := range pages {
				fmt.Fprintf(&b, "  %s  %q  lease %s (epoch %d), connection %s, last command %s\n",
					p.Key, p.Title, p.Lease, p.Epoch, p.State, p.LastUsed.Format("15:04:05"))
			}
			return Result{Text: b.String(), Data: map[string]any{"session": in.Global.Session, "pages": pages}}, nil
		},
	})
	Register(&Command{
		Name: "session release", Summary: "Release this session's page leases and run their cleanup (same as close).",
		Run: closeSession,
	})
}

// closeSession releases every lease of the caller's session with its
// cleanup (design 2.5); other sessions are never touched. The session's
// CDP endpoint records are revoked first (design 5.2), so their URLs get
// 404 and no CDP connection acts for the session during the release.
func closeSession(ctx context.Context, env Env, in Input) (Result, error) {
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	revokeCDP(env, in.Global.Session)
	n, err := m.ReleaseSession(ctx, in.Global.Session)
	if err != nil {
		return Result{}, fmt.Errorf("releasing session %q: %w", in.Global.Session, err)
	}
	return Result{
		Text: fmt.Sprintf("Session %q closed: released %d page lease(s).", in.Global.Session, n),
		Data: map[string]any{"session": in.Global.Session, "released": n},
	}, nil
}
