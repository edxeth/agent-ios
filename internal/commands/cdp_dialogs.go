package commands

import (
	"context"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp"
)

// CDPDialogs is the dialog bridge of CDP Input events (cdp.Options.Dialogs;
// design docs/design/agentic-parity.md 5.2 "Dialogs" and 3.7). A CDP
// attachment acts for its logical session, so an Input event uses that
// session's bridge state exactly as a W3b action does (w3bPage.run): the
// same activation and issuance before the steps (dialogsBefore) and the
// same acknowledgment of the answered dialogs after them, so the
// session's armed answers and its "dialog default" policy answer dialogs
// that a CDP click opens, and "dialog status" lists them.
var CDPDialogs cdp.Dialogs = cdpDialogs{}

type cdpDialogs struct{}

// page is the w3bPage of the CDP operation's page work.
func (cdpDialogs) page(p cdp.DialogPage) *w3bPage {
	return &w3bPage{
		pageRuntime: &pageRuntime{rt: p.Runtime, refs: p.PS.Refs, session: p.Session},
		ps:          p.PS, caller: p.Caller, session: p.Session, epoch: p.Epoch,
		st: stateFor(p.PS, p.Session, p.Epoch), end: func() {},
	}
}

func (d cdpDialogs) Before(ctx context.Context, p cdp.DialogPage) (string, error) {
	return d.page(p).dialogsBefore(ctx)
}

func (cdpDialogs) After(p cdp.DialogPage, uses []cdp.DialogUse) {
	l := make([]dialogUse, len(uses))
	for i, u := range uses {
		l[i] = dialogUse(u)
	}
	st := stateFor(p.PS, p.Session, p.Epoch)
	st.mu.Lock()
	st.dialogs.acknowledge(l)
	st.mu.Unlock()
}
