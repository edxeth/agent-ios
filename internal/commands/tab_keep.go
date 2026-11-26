package commands

import (
	"context"
	"fmt"

	"github.com/edxeth/agent-ios/internal/devtools"
)

// tab keep (agent-browser 0.45 "tab keep live|throttled|frozen"): Chrome
// keeps a background tab running (Emulation.setFocusEmulationEnabled) or
// freezes it (Page.setWebLifecycleState). The WebKit Inspector Protocol
// has neither: no focus emulation, no lifecycle state and no setting of
// background throttling (Page.overrideSetting has none) in the protocol of
// iOS 16 to 27 or of WebKit main. So the command exists and fails with
// that reason, as "set viewport" does; it touches no page.
func init() {
	Register(&Command{
		Name: "tab keep",
		Summary: "Not available on iOS WebKit: the inspector protocol cannot keep a background tab running, throttle it or freeze it " +
			"(Chrome's focus emulation and lifecycle freeze); Safari throttles background tabs itself.",
		Args: []Arg{
			{Name: "mode", Help: "live, throttled or frozen", Required: true},
			{Name: "tab", Help: "the tab (ignored: the command always fails)"},
		},
		Output: OutputText,
		Run: func(_ context.Context, _ Env, in Input) (Result, error) {
			// Checked here, not in Check: help parses the arguments too.
			switch arg(in, 0) {
			case "live", "throttled", "frozen":
			default:
				return Result{}, usagef("tab keep %q: want live, throttled or frozen", arg(in, 0))
			}
			return Result{}, fmt.Errorf("%w: tab keep %s: WebKit's inspector protocol has no focus emulation (Emulation.setFocusEmulationEnabled) and no lifecycle freeze (Page.setWebLifecycleState), "+
				"so agent-ios cannot keep a background tab live or frozen; Safari applies its own background throttling, and a tab runs at full speed only while Safari shows it",
				devtools.ErrUnsupported, arg(in, 0))
		},
	})
}
