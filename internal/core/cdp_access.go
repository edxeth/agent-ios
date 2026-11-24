package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
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

// ResolvePage resolves req as the page of a command of session (design
// 5.2, selectedPage) without attaching it and without a lease, so it also
// resolves a page that another session holds. It applies the session's
// page choice as every command does (sessionPage): a tab id in req.Page,
// else the page chosen with "tab <id>", else the session's default page
// while it is attached (without a listing read), else the listing's first
// page. An explicit ws:// URL gives the record of that URL. Unlike a
// command, it records no default page: issuing a URL does not act on the
// page.
//
// listing is the transport and listing address that the resolved page
// was found through (Page empty), taken from the page itself: the
// session's tab, or an attached page named by a tab id, label, key or
// ws:// URL, can be on another transport than req, and resolve returns an
// attached page without reading its listing. A caller that lists or
// attaches the page later (a CDP endpoint record) must use listing, not
// req.
//
// A simulator page is resolved under the simulator's turn rules for the
// reservation owner (simulator_turns.go), as for a command.
func (m *Manager) ResolvePage(ctx context.Context, session, owner string, req PageRequest) (lp ListedPage, listing PageRequest, err error) {
	ao := AcquireOptions{Session: session, Reservation: owner}.normalized()
	sreq := m.sessionPage(ao.Session, req)
	t, err := m.resolve(ctx, sreq)
	if err != nil {
		return ListedPage{}, PageRequest{}, err
	}
	if err := m.admitSimulator(ctx, sreq, t, ao.Reservation); err != nil {
		return ListedPage{}, PageRequest{}, err
	}
	return listedOf(t), t.listing, nil
}

// ListPages lists every page of req's transport (req.Page is ignored),
// with the record key that page resolution gives each page. A device whose
// page list fails is left out and logged, as in page resolution, unless
// the daemon holds an open page connection to it (pageListFailed).
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
		return nil, &CodedError{Code: CodeNoPage, Err: m.listingFailed(err)}
	}
	devs, err := tr.Devices(ctx)
	if err != nil {
		return nil, &CodedError{Code: CodeNoPage, Err: m.listingFailed(&ListingError{Kind: tr.Kind(), Err: fmt.Errorf("listing devices (%s): %w", tr.Kind(), err)})}
	}
	var out []ListedPage
	for _, d := range devs {
		pages, err := tr.Pages(ctx, d.ID)
		if err != nil {
			if err := m.pageListFailed(tr, d, err); err != nil {
				return nil, err
			}
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
	// An attachment admitted before another owner reserved the simulator
	// sends nothing in that owner's turn (simulator_turns.go).
	pg.mu.Lock()
	simulator, resOwner := pg.target.device.ID == SimulatorDevice && !pg.target.physical, ""
	if pg.lease != nil {
		resOwner = pg.lease.resOwner
	}
	pg.mu.Unlock()
	if simulator {
		if err := m.SimulatorAdmits(resOwner); err != nil {
			return nil, fmt.Errorf("%s was not sent: %w", name, err)
		}
	}
	now := time.Now()
	pg.mu.Lock()
	l := pg.lease
	if l == nil || l.session != session || l.epoch != epoch || pg.releasing || pg.PS == nil {
		pg.mu.Unlock()
		return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("%w: the lease of session %q (epoch %d) on page %q ended, so %s was not sent", ErrLeased, session, epoch, pg.target.title, name)}
	}
	l.lastUsed, pg.lastUsed = now, now
	l.shared = true // an operation without the claim of the grant uses it
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
		m.pageLogf(pg.targetNow(), "the state of %s was released; its page-side cleanup was not sent or failed and is pending", logsafe.ID(owner))
		m.resetUnknownAgents(pg)
	}
}

// SubscribeTarget is Subscribe with each event's source target
// (webkit.TargetEventHandler): a CDP page attachment keys its document
// generations by target and main-frame loader id (design 2.7, 5.2), and a
// WIP loader id is unique only within its target's process. Events of
// every target of the page's connection arrive, also those of a
// provisional target before its commit; webkit.AllEvents subscribes to
// every event in arrival order. The subscription follows reconnects.
func (m *Manager) SubscribeTarget(pg *Page, method string, h webkit.TargetEventHandler) (unsub func()) {
	return pg.conn.SubscribeTarget(method, h)
}

// EventPosition is where a connection delivered an event (SubscribeOrdered)
// or read a reply (WithReplyPosition): the connection, its active target
// right after it read the message, so with the event already applied (a
// commit's own event names the new target), and the message's place in
// the connection's stream.
type EventPosition struct {
	Conn         webkit.Caller // the connection that delivered the event
	ActiveTarget string        // "" on a connection without targets
	// Seq is the number of messages Conn had read up to and including this
	// one (webkit.Session.ReadPosition): the events and replies of one
	// connection are ordered by it. 0 on a connection that does not count.
	Seq uint64
}

// OrderedHandler handles one event of an ordered subscription. It runs in
// the delivering connection's read loop, or in the swap that installs a
// connection whose events were held until then (pageConn.swap), so it must
// not block, send, subscribe or unsubscribe.
type OrderedHandler func(pos EventPosition, target, method string, params json.RawMessage)

// SubscribeOrdered registers h for the events of method (webkit.AllEvents:
// every event) of each connection of the page, as SubscribeTarget does,
// but in the read loop and with each event's position: a CDP page
// attachment classifies an event of a provisional target by the target
// standing at that event, not at the time a goroutine of its own gets to
// it (design 2.7). The events of one connection arrive in its order; the
// events that a new connection delivered before a reconnect installed it
// arrive at the swap, before its later ones, with the position at which
// they were read. It returns the connection installed when the
// subscription started; a later connection is a reconnect.
func (m *Manager) SubscribeOrdered(pg *Page, method string, h OrderedHandler) (initial webkit.Caller, unsub func()) {
	return pg.conn.subscribeOrdered(method, h)
}

// WithReplyPosition returns ctx for a call whose reply must be placed among
// the page's ordered events (SubscribeOrdered). When the page's connection
// reads the call's successful reply, it takes the reply's position in its
// read loop, before it reads the next message (the same connection value
// as the events' positions, and the reply's own Seq), and runs h with it,
// the target that sent the reply and its result, in the order in which
// the ordered subscriptions get that connection's events: at once, or,
// while the connection's events are held for a hand-over (pageConn.prepare
// and swap), held with them in read order and run at their delivery
// (pageConn.replyRead). WebKit sends a target's events and the replies to
// its commands in one ordered stream, so h runs after the handlers of the
// events the page sent before the reply and before those of the later
// ones. When the call returns its reply without an error, h has run (a
// held h first runs at the delivery; a call whose context ends before it
// fails). h runs at most once, never for an error reply, a call without a
// reply or a call that does not go through the page's connection (the
// callers of a Page); it must not block, send, subscribe or unsubscribe.
func WithReplyPosition(ctx context.Context, h func(pos EventPosition, target string, result json.RawMessage)) context.Context {
	return context.WithValue(ctx, replyPositionKey{}, h)
}

type replyPositionKey struct{}
