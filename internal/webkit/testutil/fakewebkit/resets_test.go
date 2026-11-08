package fakewebkit

import (
	"slices"
	"testing"
)

// WithoutAgentResets drops each <Domain>.disable that comes before the
// domain's first enable on its connection and target (the agent reset of
// webkit.SessionOptions.AgentReset) and keeps every later disable.
func TestWithoutAgentResets(t *testing.T) {
	recs := []Record{
		{Method: "Runtime.disable", Conn: 1, Target: "t1"}, // reset
		{Method: "Runtime.enable", Conn: 1, Target: "t1"},
		{Method: "CSS.disable", Conn: 1, Target: "t1"}, // reset of another domain
		{Method: "Runtime.disable", Conn: 1, Target: "t1"},
		{Method: "Runtime.disable", Conn: 2, Target: "t1"}, // reset on a new connection
		{Method: "Runtime.disable", Conn: 1, Target: "t2"}, // reset on another target
		{Method: "Runtime.evaluate", Conn: 2, Target: "t1"},
		{Method: "Runtime.enable", Conn: 2, Target: "t1"},
		{Method: "Runtime.disable", Conn: 2, Target: "t1"},
	}
	var got []string
	for _, r := range WithoutAgentResets(recs) {
		got = append(got, r.Method+"@"+string(rune('0'+r.Conn))+r.Target)
	}
	want := []string{"Runtime.enable@1t1", "Runtime.disable@1t1", "Runtime.evaluate@2t1", "Runtime.enable@2t1", "Runtime.disable@2t1"}
	if !slices.Equal(got, want) {
		t.Fatalf("WithoutAgentResets = %q, want %q", got, want)
	}
}
