package logsafe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// F1: This is the constructor used by core.pageConn.probe for stallErr.
// The %v content is not a trusted operation name.
func TestR2FlattenedProtocolText(t *testing.T) {
	const title = "Private Untagged Page Title"
	pe := &webkit.ErrorData{Code: -32000, Message: title}
	err := fmt.Errorf("%w (%v)", webkit.ErrSessionStalled, pe)
	if got := Error(err); strings.Contains(got, title) {
		t.Fatalf("page text flattened into wrapper survives Error: %s", got)
	}
}

// AC1: localhost does not imply that a web page is an iwdp listing.
func TestR2LoopbackPageURL(t *testing.T) {
	const pageURL = "http://localhost:3000/json/private-account"
	err := fmt.Errorf("loading %s: %w", pageURL, context.DeadlineExceeded)
	if got := Error(err); strings.Contains(got, pageURL) || strings.Contains(got, "private-account") {
		t.Fatalf("page URL mistaken for an iwdp endpoint: %s", got)
	}
}

// F1 repair brief forbids page-supplied messages, not only string values.
func TestR2JSONTypeErrorContainsInput(t *testing.T) {
	const secret = "98765432123456789"
	var out int8
	err := json.Unmarshal([]byte(secret), &out)
	if err == nil {
		t.Fatal("fixture must overflow int8")
	}
	if got := Error(err); strings.Contains(got, secret) {
		t.Fatalf("JSON error classification retained the input value: %s", got)
	}
}

// Runtime/ref error codes are supplied in page replies. Syntax alone
// does not establish that one is a known classification.
func TestR2CodeShapedPageText(t *testing.T) {
	const title = "private_account_title"
	for _, err := range []error{
		&runtime.Error{Code: title, Message: "fixture"},
		&refs.Error{Code: title, Message: "fixture"},
	} {
		if got := Error(err); strings.Contains(got, title) {
			t.Errorf("page-controlled code retained as trusted classification: %s", got)
		}
	}
}
