package cdp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// The Log domain (design docs/design/agentic-parity.md, section 5.3):
// entryAdded from WIP console messages that are neither console API calls
// nor JavaScript errors (Runtime reports those), with source network,
// security or other; and the dialog policy's automatic answers as
// warnings (5.2 "Dialogs", acceptance 7.8 item 6).

func init() {
	register(
		Method{Name: "Log.enable", Levels: OnPage, Handle: logEnable},
		Method{Name: "Log.disable", Levels: OnPage, Handle: logDisable},
	)
	registerEvents("Log", "entryAdded")
}

// logEnable owns WIP Console for the attachment (its messages are the
// entries) and turns on the attachment's Log events. Console stays owned
// until the detach: Runtime and Page events need it too.
func logEnable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.owned(ctx, mo, "Console.enable", nil)
	})
	if err != nil {
		return nil, nil, err
	}
	s.page.setEnabled(domLog, true)
	return nil, nil, nil
}

func logDisable(_ context.Context, call *Call) (any, Later, error) {
	call.session.page.setEnabled(domLog, false)
	return nil, nil, nil
}

// logSource maps a WIP console message source to a CDP Log source.
func logSource(src string) string {
	switch src {
	case "network":
		return "network"
	case "security", "content-blocker":
		return "security"
	}
	return "other"
}

// logLevel maps a WIP console level to a CDP Log level.
func logLevel(level string) string {
	switch level {
	case "warning", "error":
		return level
	case "debug":
		return "verbose"
	}
	return "info"
}

// logEntryLocked sends m as Log.entryAdded.
func (ps *pageState) logEntryLocked(m wipConsoleMessage) {
	e := map[string]any{"source": logSource(m.Source), "level": logLevel(m.Level), "text": m.Text, "timestamp": nowMillis()}
	if m.URL != "" {
		e["url"] = m.URL
		e["lineNumber"] = max(m.Line-1, 0)
	}
	if m.RequestID != "" {
		e["networkRequestId"] = m.RequestID
	}
	if st := cdpStack(m.StackTrace); st != nil {
		e["stackTrace"] = st
	}
	ps.emitLocked(domLog, "Log.entryAdded", map[string]any{"entry": e})
}

// dialogEntryLocked reports a dialog that the session's dialog override
// answered (the page's marked message, 80-dialogs.js) as a Log warning:
// the bridge never emits javascriptDialogOpening (5.2).
func (ps *pageState) dialogEntryLocked(payload string) {
	var u struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Result  any    `json:"result"`
	}
	if json.Unmarshal([]byte(payload), &u) != nil {
		return
	}
	res, _ := json.Marshal(u.Result) // a JSON value
	if u.Type == "alert" {
		res = []byte("accepted")
	}
	by := "by the session's dialog policy"
	if u.ID != "" {
		by = "by the answer " + u.ID + " armed with iwdp-cli dialog"
	}
	text := fmt.Sprintf("dialog %s %q answered %s %s; %s", u.Type, u.Message, res, by, errDialogs.Message)
	ps.emitLocked(domLog, "Log.entryAdded", map[string]any{"entry": map[string]any{
		"source": "other", "level": "warning", "text": text, "timestamp": nowMillis(),
	}})
}
