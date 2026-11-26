package cdp

import (
	"encoding/json"
	"testing"
)

// Parity 0.45 (chrome-devtools-mcp 1.14.0 console stack traces, feasibility
// 3g): WIP Console.StackTrace has parentStackTrace (async frames; WebKit
// main and 7615.3.12.11.2 Console.json); the bridge forwards it as CDP
// StackTrace.parent. A parent whose topCallFrameIsBoundary is true starts
// with the native function that scheduled the work (setTimeout), which
// becomes the parent's description, as Chrome names async boundaries.
func TestCDPStackForwardsParent(t *testing.T) {
	raw := json.RawMessage(`{"callFrames":[{"functionName":"inner","url":"https://a.example/a.js","scriptId":"7","lineNumber":3,"columnNumber":5}],
	"parentStackTrace":{"topCallFrameIsBoundary":true,"callFrames":[{"functionName":"setTimeout","url":"[native code]","scriptId":"0","lineNumber":0,"columnNumber":0},
	{"functionName":"outer","url":"https://a.example/a.js","scriptId":"7","lineNumber":10,"columnNumber":2}],
	"parentStackTrace":{"callFrames":[{"functionName":"","url":"https://a.example/a.js","scriptId":"7","lineNumber":20,"columnNumber":1}]}}}`)
	got, _ := json.Marshal(cdpStack(raw))
	want := `{"callFrames":[{"columnNumber":4,"functionName":"inner","lineNumber":2,"scriptId":"7","url":"https://a.example/a.js"}],` +
		`"parent":{"callFrames":[{"columnNumber":1,"functionName":"outer","lineNumber":9,"scriptId":"7","url":"https://a.example/a.js"}],"description":"setTimeout",` +
		`"parent":{"callFrames":[{"columnNumber":0,"functionName":"","lineNumber":19,"scriptId":"7","url":"https://a.example/a.js"}]}}}`
	if string(got) != want {
		t.Fatalf("cdpStack =\n%s\nwant\n%s", got, want)
	}
}
