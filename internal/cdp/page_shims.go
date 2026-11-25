package cdp

import (
	"context"
	_ "embed" // the page shims
	"fmt"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// Page compatibility shims (docs/cdp.md, "Page compatibility shims"): CDP
// clients believe they drive Chromium and call newer Web APIs without a
// check, which Safari 16.6 (iOS 16.7) lacks; Playwright 1.65's injected
// script calls Element.prototype.checkVisibility (Safari 17.4) on every
// visibility check. page_shims.js adds each missing API, and only a
// missing one, in the main world, where the utility-world aliases run too
// (worlds.go). Only CDP attachments install it; the CLI runtime bundle
// does not change.
//
//go:embed page_shims.js
var pageShimsJS string

// pageShimsPart is the name of a page attachment's bootstrap part with
// the shims.
func pageShimsPart(owner core.Owner) string { return "cdp-shims:" + string(owner) }

// installPageShims installs the shims for page attachment s: as a
// bootstrap part of its desired-state owner (rank core.RankShim, so they
// run before the client's Page.addScriptToEvaluateOnNewDocument scripts,
// which are part of the owner's rank core.RankInit part), so every new
// document gets them until the detach releases the owner; and in the
// current document. A detach keeps the shims in the current document: a
// page script may already hold or use the method, and the shim answers as
// the native method of later WebKit versions does.
func (s *session) installPageShims(ctx context.Context) error {
	return s.do(ctx, "page compatibility shims", core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		st := s.pg.PS.State
		err := st.Change(ctx, mo.Op.Caller(), func(c webkit.Caller) error {
			first := !st.Needs("Page")
			if err := st.SetPart(s.owner, pageShimsPart(s.owner), core.RankShim, pageShimsJS); err != nil {
				return err
			}
			if first {
				if _, err := c.Send(ctx, "Page.enable", nil); err != nil {
					return fmt.Errorf("sending Page.enable: %w", err)
				}
			}
			if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": st.Boot.Script()}); err != nil {
				return fmt.Errorf("Page.setBootstrapScript: %w", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := evaluateValue(ctx, mo.Op.Caller(), pageShimsJS); err != nil {
			return fmt.Errorf("installing them in the current document: %w", err)
		}
		return nil
	})
}
