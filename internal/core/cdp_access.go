package core

import (
	"context"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// This file is the core API of the CDP endpoint (design
// docs/design/agentic-parity.md, section 5.2): page resolution without a
// lease, the page listing of a transport, operations under a lease that a
// CDP attachment already holds, and calls and releases with an
// attachment's own desired-state owner.

// ListedPage is one page as page resolution sees it: its record key (the
// key of the Page the daemon attaches for it), its device and its
// transport page.
type ListedPage struct {
	Key    string
	Title  string
	URL    string
	Device transport.Device
	Page   transport.Page
}

func listedOf(t pageTarget) ListedPage {
	return ListedPage{Key: t.key, Title: t.title, URL: t.url, Device: t.device, Page: t.page}
}

// ResolvePage resolves req as the page of a command (design 5.2,
// selectedPage) without attaching it and without a lease, so it also
// resolves a page that another session holds. An explicit ws:// URL gives
// the record of that URL.
func (m *Manager) ResolvePage(ctx context.Context, req PageRequest) (ListedPage, error) {
	t, err := m.resolve(ctx, req)
	if err != nil {
		return ListedPage{}, err
	}
	return listedOf(t), nil
}

// ListPages lists every page of req's transport (req.Page is ignored),
// with the record key that page resolution gives each page. A device whose
// page list does not answer is left out and logged, as in page resolution.
func (m *Manager) ListPages(ctx context.Context, req PageRequest) ([]ListedPage, error) {
	kind := req.Transport
	if kind == "" {
		kind = "auto"
	}
	if m.o.Transport == nil {
		return nil, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("%w: no transport is configured", ErrNoPage)}
	}
	tr, err := m.o.Transport(ctx, kind, req.Addr)
	if err != nil {
		return nil, &CodedError{Code: CodeNoPage, Err: err}
	}
	devs, err := tr.Devices(ctx)
	if err != nil {
		return nil, &CodedError{Code: CodeNoPage, Err: fmt.Errorf("listing devices (%s): %w", tr.Kind(), err)}
	}
	var out []ListedPage
	for _, d := range devs {
		pages, err := tr.Pages(ctx, d.ID)
		if err != nil {
			m.o.Logf("listing pages of %s: %v", d.Label, err)
			continue
		}
		for _, p := range pages {
			// The same key as findListed gives the page.
			key := string(tr.Kind()) + "|" + d.ID + "|" + p.ID
			out = append(out, ListedPage{Key: key, Title: p.Title, URL: p.URL, Device: d, Page: p})
		}
	}
	return out, nil
}

// LeaseOf reports the session and epoch of the lease on the page record
// key; ok is false when the daemon has no such page or no lease on it.
func (m *Manager) LeaseOf(key string) (session string, epoch uint64, ok bool) {
	pg := m.lookup(key)
	if pg == nil {
		return "", 0, false
	}
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.lease == nil {
		return "", 0, false
	}
	return pg.lease.session, pg.lease.epoch, true
}

// BeginLeased opens an operation of class on pg under the lease (session,
// epoch) that the caller already holds, for example a CDP attachment that
// took it with Touch (design 5.2). It never grants a lease: when that lease
// ended or changed, the operation is refused with the lease error, so a
// late message of a detached attachment cannot lease the page again. ctx
// must have the operation deadline.
func (m *Manager) BeginLeased(ctx context.Context, pg *Page, session string, epoch uint64, name string, class Class) (*ManagedOp, error) {
	now := time.Now()
	pg.mu.Lock()
	l := pg.lease
	if l == nil || l.session != session || l.epoch != epoch || pg.releasing || pg.PS == nil {
		pg.mu.Unlock()
		return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("%w: the lease of session %q (epoch %d) on page %q ended, so %s was not sent", ErrLeased, session, epoch, pg.target.title, name)}
	}
	l.lastUsed, pg.lastUsed = now, now
	pg.running++
	pg.mu.Unlock()
	notRunning := func() {
		pg.mu.Lock()
		pg.running--
		pg.mu.Unlock()
	}
	// As Begin: a failed provisional setup of the active target is applied
	// again before the operation queues, since the session refuses every
	// gated send to that target until a setup succeeds.
	if err := m.recoverSetup(ctx, pg); err != nil {
		notRunning()
		return nil, fmt.Errorf("%s was not sent: %w", name, err)
	}
	op, err := pg.PS.Sched.Begin(ctx, OpSpec{Name: name, Class: class, Session: session, Epoch: epoch})
	if err != nil {
		notRunning()
		return nil, err
	}
	m.mu.Lock()
	m.nextOp++
	mo := &ManagedOp{ID: fmt.Sprintf("o%d", m.nextOp), Op: op, Page: pg, Session: session, Epoch: epoch}
	m.ops[mo.ID] = mo
	m.mu.Unlock()
	return mo, nil
}

// ReleaseOwner removes the desired-state entries of one owner (a CDP
// attachment, design 5.2) inside operation mo and applies the result, as
// a lease release does for each owner of its session (design 2.4): a
// domain that only owner held is disabled, and a header that loses its
// last owner while other Network owners remain runs the Network
// reinitialization barrier. mo should be exclusive.
//
// As a lease release (release_lifecycle.go), owner leaves the model
// whatever fails here, also when the change lock is not free before ctx
// ends, and its removal increases the release generation, so no later
// setup applies its state again. A cleanup that was not sent or failed
// stays pending on the live connection (forgetOwner).
func (m *Manager) ReleaseOwner(ctx context.Context, mo *ManagedOp, owner Owner) (err error) {
	ps := mo.Page.PS
	var failed []string // domains whose cleanup send failed
	defer func() { m.forgetOwner(mo.Page, owner, failed) }()
	ctx = asCleanup(ctx) // a forwarded send that fails stays pending too
	cs, end, err := ps.State.beginChange(ctx, mo.Op.Caller())
	if err != nil {
		return err
	}
	defer func() { err = end(err) }()
	res, err := ps.State.Release(ctx, owner, cs)
	failed = res.Failed
	if err == nil && res.NeedsNetworkReset {
		if err = NetworkReset(ctx, ps.Sched, cs, ps.State); err != nil {
			failed = append(failed, "Network")
		}
	}
	return err
}

// ForgetOwner removes owner from pg's desired state without an operation
// and without sending anything, for a detach that could not open its
// operation (BeginLeased) before its deadline: the cleanup of the domains
// owner touched stays pending on the live connection (forgetOwner).
func (m *Manager) ForgetOwner(pg *Page, owner Owner) {
	m.forgetOwner(pg, owner, nil)
}

// forgetOwner removes owner from the model, as forgetSession does for a
// session: on a live connection the domains of failed and those whose
// cleanup owner never sent become unknown, and the background reset
// starts; on any other connection the next connection's setup resets them.
//
// The sample of the connection state and the removal run under pg.mu, the
// lock under which reattach and recover check the release generation and
// install a new connection. So the removal comes before that check, and
// the installer sets up again, or after the swap, and then it sees the
// installed connection live and keeps the cleanup pending on it.
func (m *Manager) forgetOwner(pg *Page, owner Owner, failed []string) {
	pg.mu.Lock()
	live := pg.conn.State() == ConnOK
	pending := pg.PS.State.forgetOwners(func(o Owner) bool { return o == owner }, live, failed)
	pg.mu.Unlock()
	if pending {
		m.o.Logf("page %s: the state of %s was released; its page-side cleanup was not sent or failed and is pending", pg.Title(), owner)
		m.resetUnknownAgents(pg)
	}
}
