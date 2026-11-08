package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// The MCP skill names only tools that the server registers (design 7.9:
// the skills match the product). A tool name is a snake_case word in a
// code span of skills/ios-safari-debug/SKILL.md.
func TestMCPSkillNamesRegisteredTools(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", "ios-safari-debug", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IWDP_MCP_TOOLS", "all")
	tools := toolNames(t)
	t.Setenv("IWDP_MCP_TOOLS", "core")
	core := toolNames(t)
	seen := 0
	for _, m := range regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`").FindAllStringSubmatch(string(data), -1) {
		seen++
		if !slices.Contains(tools, m[1]) {
			t.Errorf("the skill names %s, which is not a registered tool", m[1])
		}
	}
	if seen < 20 {
		t.Fatalf("found %d tool names in the skill; the pattern no longer matches how the skill writes them", seen)
	}
	// The agentic loop of the skill runs on the default profile.
	for _, n := range []string{"tab_list", "tab_select", "navigate", "snapshot", "fill", "click", "wait", "dialog", "close_session", "session_info", "take_screenshot", "get_cookies", "set_cookie"} {
		if !slices.Contains(core, n) {
			t.Errorf("%s is not in the core profile, but the skill uses it without naming a profile", n)
		}
	}
}
