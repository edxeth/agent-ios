//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
)

// pauseWait bounds the wait for Debugger.paused after the page calls the
// function with the breakpoint from a timer.
const pauseWait = 5 * time.Second

// W1b round 5, R5-2 (user decision: breakpoints as in Web Inspector): a
// breakpoint set through the tools pauses page script. WebKit starts every
// backend with breakpoints inactive and Debugger.enable does not activate
// them, so tools.DebuggerEnable also sends Debugger.setBreakpointsActive
// (true). Without that activation no Debugger.paused arrives.
func TestBreakpointSetThroughToolsPausesPageScript(t *testing.T) {
	navigate(t, "breakpoint.html", "typeof hit === 'function'")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	paused := make(chan json.RawMessage, 1)
	unsub := sess.Subscribe("Debugger.paused", func(_ string, p json.RawMessage) {
		select {
		case paused <- p:
		default:
		}
	})
	defer unsub()
	if err := tools.DebuggerEnable(ctx, sess); err != nil {
		t.Fatalf("debugger enable: %v", err)
	}
	t.Cleanup(func() {
		// Debugger.disable also resumes a page that is still paused and
		// removes the breakpoint.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tools.DebuggerDisable(ctx, sess); err != nil {
			t.Errorf("debugger disable: %v", err)
		}
	})
	id, _, err := tools.SetBreakpointByURL(ctx, sess, origin+"/breakpoint.js", 3, nil, "")
	if err != nil {
		t.Fatalf("set breakpoint: %v", err)
	}
	eval(t, "setTimeout(hit, 0), true")
	select {
	case p := <-paused:
		var ev struct {
			CallFrames []struct {
				FunctionName string `json:"functionName"`
				Location     struct {
					LineNumber int `json:"lineNumber"`
				} `json:"location"`
			} `json:"callFrames"`
		}
		if err := json.Unmarshal(p, &ev); err != nil || len(ev.CallFrames) == 0 {
			t.Fatalf("Debugger.paused without call frames: %s", p)
		}
		if f := ev.CallFrames[0]; f.FunctionName != "hit" || f.Location.LineNumber != 3 {
			t.Fatalf("paused in %q at line %d, want hit at line 3: %s", f.FunctionName, f.Location.LineNumber, p)
		}
	case <-time.After(pauseWait):
		t.Fatalf("no Debugger.paused within %s after the page called hit with breakpoint %s", pauseWait, id)
	}
	if err := tools.Resume(ctx, sess); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := tools.RemoveBreakpoint(ctx, sess, id); err != nil {
		t.Fatalf("remove breakpoint: %v", err)
	}
	if got := string(eval(t, "hit()")); strings.TrimSpace(got) != "42" {
		t.Fatalf("hit() after the removal = %s, want 42 without a pause", got)
	}
}
