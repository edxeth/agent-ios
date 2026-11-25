package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/edxeth/agent-ios/internal/core"
)

// The reply of an action whose attempts the deadline ended carries that
// fact, so that the CLI's deadline message names the action's checks
// (cmd/agent-ios TestDeadlineMessageOfAnActionNamesItsChecks); another
// deadline does not.
func TestErrorReplyMarksAnActionTimeout(t *testing.T) {
	action := toErrorReply(fmt.Errorf("click stopped before step 1 of 1: no element matches: %w", core.ErrActionTimeout))
	if !action.Timeout || !action.ActionTimeout || action.Code != core.CodeTimeout {
		t.Fatalf("reply of an action timeout = %+v, want a timeout (code %d) marked as an action's", action, core.CodeTimeout)
	}
	if err := error(&RemoteError{E: *action}); !errors.Is(err, core.ErrActionTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the remote error of %+v is not core.ErrActionTimeout and context.DeadlineExceeded", action)
	}
	other := toErrorReply(fmt.Errorf("Runtime.evaluate: %w", context.DeadlineExceeded))
	if !other.Timeout || other.ActionTimeout {
		t.Fatalf("reply of another deadline = %+v, want a timeout that is not an action's", other)
	}
	if errors.Is(&RemoteError{E: *other}, core.ErrActionTimeout) {
		t.Fatalf("the remote error of %+v is core.ErrActionTimeout", other)
	}
}
