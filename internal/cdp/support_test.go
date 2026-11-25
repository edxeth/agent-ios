package cdp

import (
	"encoding/json"
	"strings"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Test support of this package's tests only: nothing in the endpoint uses
// these, so they live here and not in the production files.

// cookieID is a cookie's identity in the jar: name, path, and domain with
// its leading dot (the kind), case aside (W4, commit a6d3142:
// tools.SetCookieVerified, devtools.DeleteCookies).
type cookieID struct{ name, domain, path string }

// idOf is the identity of c.
func idOf(c webkit.Cookie) cookieID { return cookieID{c.Name, strings.ToLower(c.Domain), c.Path} }

// dispatch translates one event of a model (no connection): a target is
// provisional from its Target.targetCreated{isProvisional} to its commit
// or destruction.
func (ps *pageState) dispatch(target, method string, params json.RawMessage) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var p targetParams
	if json.Unmarshal(params, &p) == nil {
		switch method {
		case evTargetCreated:
			if p.TargetInfo.IsProvisional && p.TargetInfo.TargetID != "" {
				ps.modelProv[p.TargetInfo.TargetID] = true
			}
		case evTargetCommitted:
			delete(ps.modelProv, p.NewTargetID)
		case evTargetDestroyed:
			delete(ps.modelProv, p.TargetID)
		}
	}
	ps.applyLocked(wipEvent{target: target, method: method, params: params, provisional: target != "" && ps.modelProv[target], at: monotonic()})
}
