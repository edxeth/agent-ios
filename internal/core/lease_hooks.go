package core

import (
	"context"
	"sync"

	"github.com/nnemirovsky/iwdp-cli/internal/logsafe"
)

// LeaseHook runs when a session's lease on a page starts: a new grant or a
// takeover (design 4.2 "network requests": the capture starts with the
// lease, MAP 1.8). It runs once per lease epoch, in the first call under
// that lease whose context carries WithLeaseHooks, before that call's
// operation is queued, so that what it sets up sees the operation's page
// traffic. ctx carries that call's deadline. A hook that needs the page opens its own operation with
// ps.Sched.Begin under session and epoch. An error is logged and the
// acquisition goes on.
type LeaseHook func(ctx context.Context, ps *PageSession, session string, epoch uint64) error

type leaseHooksKey struct{}

// WithLeaseHooks marks ctx as the context of a registry command (design
// 2.10): a lease that such a call acquires or uses first runs the lease
// hooks. Calls without the mark (a CDP attachment's) do not run them.
func WithLeaseHooks(ctx context.Context) context.Context {
	return context.WithValue(ctx, leaseHooksKey{}, true)
}

var leaseHooks struct {
	mu   sync.Mutex
	list []LeaseHook
}

// OnLeaseStart adds a hook that runs at the start of every lease of every
// manager of this process (packages call it from init).
func OnLeaseStart(h LeaseHook) {
	leaseHooks.mu.Lock()
	defer leaseHooks.mu.Unlock()
	leaseHooks.list = append(leaseHooks.list, h)
}

// leaseHookState is the last lease epoch of a page whose hooks ran. Its
// mutex makes a second caller of the same new epoch wait until the hooks
// ran, so that its operation, too, starts after them.
type leaseHookState struct {
	mu    sync.Mutex
	epoch uint64
}

var leaseHookStates sync.Map // *Page -> *leaseHookState

// leaseStarted runs the lease hooks for session's lease epoch on pg when
// ctx carries WithLeaseHooks, unless they ran for that epoch already.
// Epochs only grow (Manager.epochs), so a new epoch is a new lease.
func (m *Manager) leaseStarted(ctx context.Context, pg *Page, session string, epoch uint64) {
	if ctx.Value(leaseHooksKey{}) == nil {
		return
	}
	leaseHooks.mu.Lock()
	hooks := append([]LeaseHook(nil), leaseHooks.list...)
	leaseHooks.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	v, loaded := leaseHookStates.LoadOrStore(pg, &leaseHookState{})
	if !loaded {
		// The page record ends with its page session's connection.
		go func() {
			<-pg.PS.Caller.Done()
			leaseHookStates.Delete(pg)
		}()
	}
	st := v.(*leaseHookState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.epoch >= epoch {
		return
	}
	st.epoch = epoch
	// The hooks open their operations with Scheduler.Begin, not with
	// Manager.Begin, which applies a failed setup of the active target
	// again before it queues an operation (recoverSetup). Do that here
	// too, so that the session refuses none of the hooks' sends.
	if err := m.recoverSetup(ctx, pg); err != nil {
		m.pageLogf(pg.targetNow(), "lease start of session %q (epoch %d): %v", logsafe.ID(session), epoch, logsafe.Error(err))
	}
	for _, h := range hooks {
		if err := h(ctx, pg.PS, session, epoch); err != nil {
			m.pageLogf(pg.targetNow(), "lease start of session %q (epoch %d): %v", logsafe.ID(session), epoch, logsafe.Error(err))
		}
	}
}
