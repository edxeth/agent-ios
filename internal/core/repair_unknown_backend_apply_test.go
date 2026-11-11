package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// An enable in Apply's pipelined batch that the page refuses fails the
// apply with the page's error (only a reset may be refused as not enabled).
func TestApplyFailsOnARefusedEnable(t *testing.T) {
	_, c, _ := ownersPage(t)
	d := NewDesiredState()
	d.Acquire("o", "Console")
	refused := refusingConn{c, "Console.enable", &webkit.ErrorData{Code: -32000, Message: "refused by the test"}}
	err := d.Apply(context.Background(), refused)
	var pe *webkit.ErrorData
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "applying desired state") {
		t.Fatalf("Apply with a refused Console.enable: %v, want the refusal", err)
	}
}
