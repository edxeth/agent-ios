package cdp

import (
	"errors"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/logsafe"
)

// Review devfix-R1 F1: an error that the CDP endpoint logs (the cleanup of
// a session's state, conn.cleanup; an auto-attach, autoAttachLocked) can
// carry the page's title and URL in any form. The endpoint renders it with
// logsafe.Error, which TestLogCallsWriteOnlySafeKinds enforces for every
// log call, so neither reaches the daemon log.
func TestReviewCDPLogRetainsPageURL(t *testing.T) {
	tg := target{title: "Private Review Title", url: "https://private-review.example/account", pageID: "A1"}
	text := logsafe.Error(errors.New("cleanup failed for https://private-review.example/account (Private Review Title)"))
	if strings.Contains(text, tg.url) || strings.Contains(text, tg.title) {
		t.Fatalf("page title and URL remain in CDP log text: %s", text)
	}
}
