package core

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
)

// Page resolution without the listing (docs/known-limits.md, "iwdp does
// not list pages while a page is connected"). The iwdp of the second
// device check closed every listing request without a reply while any
// WebSocket to one of its pages was open; the cause is unknown. So:
//   - a request for a page that the daemon holds an open connection to
//     (its exact record key, or no page in a session whose default page it
//     is) resolves to that record without the listing: the connection is
//     the evidence that the page exists. A lost or closed connection falls
//     back to the listing.
//   - a listing read that fails while the daemon holds an iwdp page
//     connection gets advice instead of a dead-iwdp message.
//   - a device's page list that fails while the device list answers is
//     the error only when the daemon holds an open connection to a page of
//     that device (pageListFailed): then the request may need the device,
//     and its page list fails because of that connection. Any other device
//     whose page list fails is left out and logged, as before, so that it
//     hides no other device of a healthy iwdp.

// ListingError is a failed read of a transport's page listing, as a
// TransportFunc reports it (the daemon's check of iwdp's device list
// before a listing). Its text is that of Err.
type ListingError struct {
	Kind transport.Kind
	Err  error
}

func (e *ListingError) Error() string { return e.Err.Error() }
func (e *ListingError) Unwrap() error { return e.Err }

// openPage returns the record of key when the daemon holds an open
// connection to its page (attached, or stalled with a recovery that may
// run), else nil.
func (m *Manager) openPage(key string) *Page {
	pg := m.lookup(key)
	if pg == nil {
		return nil
	}
	switch pg.conn.State() {
	case ConnOK, ConnStalled:
		return pg
	}
	return nil
}

// attachedTarget resolves req to an open page record without the listing:
// req.Page is the record's key, or req names no page and its session's
// default page (PageRequest.prefer) is open. A request for another
// transport kind than the record's is not answered here.
func (m *Manager) attachedTarget(req PageRequest) (pageTarget, bool) {
	key := req.Page
	if key == "" {
		key = req.prefer
	}
	if key == "" {
		return pageTarget{}, false
	}
	if k := req.Transport; k != "" && k != "auto" && !strings.HasPrefix(key, k+"|") {
		return pageTarget{}, false
	}
	pg := m.openPage(key)
	if pg == nil {
		return pageTarget{}, false
	}
	return pg.targetNow(), true
}

// pageListFailed handles err, the failed page list of device d of tr. When
// tr is iwdp and the daemon holds an open connection to a page of d, it
// returns the coded listing error with the advice (listingFailed): a
// title selection that matches no other device, a tab list and the CDP
// page list report it instead of "no page" or a list without d. Otherwise
// it logs err and returns nil: d is left out.
func (m *Manager) pageListFailed(tr transport.Transport, d transport.Device, err error) error {
	kind := tr.Kind()
	if kind == transport.KindIWDP {
		prefix := string(kind) + "|" + d.ID + "|"
		for _, pg := range m.allPages() {
			if strings.HasPrefix(pg.Key, prefix) && m.openPage(pg.Key) != nil {
				return &CodedError{Code: CodeNoPage, Err: m.listingFailed(&ListingError{Kind: kind, Err: fmt.Errorf("listing the pages of %s (%s): %w", d.Label, kind, err)})}
			}
		}
	}
	m.o.Logf("listing pages of %s: %v", logsafe.ID(d.Label), logsafe.Error(err))
	return nil
}

// listingFailed is err, a failed listing read, with advice when err is an
// iwdp listing error (ListingError) while the daemon holds an open iwdp
// page connection; otherwise err unchanged.
func (m *Manager) listingFailed(err error) error {
	var le *ListingError
	if !errors.As(err, &le) || le.Kind != transport.KindIWDP {
		return err
	}
	n := 0
	for _, pg := range m.allPages() {
		if strings.HasPrefix(pg.Key, string(transport.KindIWDP)+"|") && m.openPage(pg.Key) != nil {
			n++
		}
	}
	if n == 0 {
		return err
	}
	conns := "1 page connection"
	if n > 1 {
		conns = fmt.Sprintf("%d page connections", n)
	}
	return fmt.Errorf("iwdp did not answer the page list while the daemon has %s open; commands on an attached page still work (in its session without --page, or with its ws:// URL), and the daemon does not start or restart iwdp for this. To list or select pages, run 'iwdp-cli daemon stop' to close the page connections, or have ios_webkit_debug_proxy restarted: 'iwdp-cli restart-iwdp' restarts only an iwdp that iwdp-cli started and recorded; for another iwdp, only whoever started that iwdp, and only when nobody else uses it, may stop it, with the procedure of docs/device-runbook.md (Stopping an iwdp by hand). Cause: %w", conns, err)
}
