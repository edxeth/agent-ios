package commands

import (
	"context"
	"fmt"
	"testing"
)

// Decision D9: an expired command deadline exits 124 (like timeout(1)) for
// legacy and registry commands alike; design 7.3 item 3 is amended from 3.
func TestDeadlineExitCodeIs124(t *testing.T) {
	err := fmt.Errorf("eval: %w", context.DeadlineExceeded)
	if code := ExitCodeOf(err); code != 124 {
		t.Fatalf("ExitCodeOf(deadline) = %d, want 124 (decision D9)", code)
	}
}
