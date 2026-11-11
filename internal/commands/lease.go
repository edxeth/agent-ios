package commands

import (
	"context"
	"fmt"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// Lease commands (design 2.5).
func init() {
	Register(&Command{
		Name: "lease break", Summary: "End the lease on a page whoever holds it, with its cleanup.",
		Args: []Arg{{Name: "page", Help: "page key, ws:// URL, or part of its title or URL", Required: true}},
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			m, err := managerOf(env)
			if err != nil {
				return Result{}, err
			}
			pg := m.FindAttached(in.Args[0])
			if pg == nil {
				return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("%w: no attached page matches %q (see 'iwdp-cli daemon status')", core.ErrNoPage, in.Args[0])}
			}
			if err := m.Break(ctx, pg.Key); err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Lease on %q broken.", pg.Title()), Data: map[string]string{"page": pg.Key}}, nil
		},
	})
	Register(&Command{
		Name: "lease takeover", Summary: "Reattach a lost page and take its lease for this session (needs the device reservation and the device owner's approval).",
		Args: []Arg{{Name: "page", Help: "page key, ws:// URL, or part of its title or URL; default: the first page"}},
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			m, err := managerOf(env)
			if err != nil {
				return Result{}, err
			}
			req := core.PageRequest{Transport: in.Global.Transport, Page: in.Global.Page}
			if len(in.Args) > 0 {
				req.Page = in.Args[0]
			}
			pg, epoch, err := m.Touch(ctx, req, core.AcquireOptions{Session: in.Global.Session, Reservation: in.Global.Reservation, Takeover: true})
			if err != nil {
				return Result{}, err
			}
			return Result{
				Text: fmt.Sprintf("Session %q holds %q (epoch %d).", in.Global.Session, pg.Title(), epoch),
				Data: map[string]any{"page": pg.Key, "epoch": epoch},
			}, nil
		},
	})
}
