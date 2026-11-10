package core

import (
	"context"
	"encoding/json"
	"fmt"
)

// Raw sends one protocol call inside the open operation id through RawAs,
// with the session's owner of kind "legacy" as the desired-state owner.
// The tests use it as a second owner kind next to the registry command and
// CDP owners. Production code calls RawAs (the CDP endpoint) or the
// command paths; the old-command bridge that called Raw is removed.
func (m *Manager) Raw(ctx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
	m.mu.Lock()
	mo := m.ops[id]
	m.mu.Unlock()
	if mo == nil {
		return nil, fmt.Errorf("operation %q is not open", id)
	}
	return m.RawAs(ctx, mo, SessionOwner("legacy", mo.Session), method, params)
}
