package core

import (
	"fmt"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/transport"
)

// Device check 3, P2-C: a daemon log line about a page names it by its
// page id (pageLogf), never by its title or URL. Errors name a page by its
// quoted title for the user ("connecting to page %q: ..."); such an error
// is logged through logsafe.Error, which writes no error text, only the
// name of the sentinel that core registers (errors.go); the log call's
// format names the operation.
func TestPageLogfNamesThePageByItsID(t *testing.T) {
	var lines []string
	m := &Manager{o: ManagerOptions{Logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}}
	tg := pageTarget{
		title: "Private Title", url: "https://private.example/account",
		page: transport.Page{ID: "ws://127.0.0.1:9224/devtools/page/3?x=1", Title: "Private Title"},
	}
	err := fmt.Errorf("connecting to page %q: %w", tg.title, ErrRecoveryFailed)
	m.pageLogf(tg, "background recovery failed: %v", logsafe.Error(err))
	want := "page ws://127.0.0.1:9224/devtools/page/3: background recovery failed: core.ErrRecoveryFailed"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("pageLogf wrote %q, want [%q]", lines, want)
	}
	if strings.Contains(lines[0], "Private") || strings.Contains(lines[0], "private.example") {
		t.Fatalf("pageLogf wrote the title or URL: %q", lines[0])
	}
}
