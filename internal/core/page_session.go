// Package core holds the per-page model of the daemon (design
// docs/design/agentic-parity.md, sections 2.4 to 2.10): the operation
// scheduler with its control lane, the desired state and the bootstrap parts.
package core

import (
	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// PageSession is everything the daemon keeps for one attached page.
type PageSession struct {
	Caller  webkit.Caller
	Device  transport.Device
	Page    transport.Page
	Runtime *runtime.Runtime // W3a
	Refs    *refs.Table      // W3a
	Sched   *Scheduler       // operations of every entry point (2.10)
	State   *DesiredState    // owned entries (2.4); sent to new and provisional targets, reduced on release
	Boot    *BootstrapParts  // named parts joined into one Page.setBootstrapScript (State.Boot)

	// DaemonID is the daemon of the runtime's owner (runtime.Owner.Daemon):
	// ManagerOptions.DaemonID, the domain in which lease epochs only grow.
	// "" means runtime.DaemonID (a page session made outside a manager).
	DaemonID string
}
