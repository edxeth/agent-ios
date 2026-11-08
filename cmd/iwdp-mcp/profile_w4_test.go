package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Design 4.1 and D2: IWDP_MCP_TOOLS or --tools selects any subset of the
// profiles core, devtools and legacy; "core,devtools,legacy" registers the
// same tools as "all".
func TestCombinedProfiles(t *testing.T) {
	profile := func(v string) []string {
		t.Helper()
		t.Setenv("IWDP_MCP_TOOLS", v)
		return toolNames(t)
	}
	core, dev, legacy := profile("core"), profile("devtools"), profile("legacy")
	coreDev, every, all := profile("core,devtools"), profile("devtools,legacy,core"), profile("all")
	cases := []struct {
		name     string
		tools    []string
		has, not []string
	}{
		{"core", core, []string{"evaluate_script", "snapshot", "session_info"}, []string{"set_media", "heap_snapshot"}},
		{"devtools", dev, []string{"set_media", "network_route", "set_emulated_conditions", "set_credentials"}, []string{"evaluate_script", "snapshot", "heap_snapshot"}},
		{"legacy", legacy, []string{"evaluate_script", "heap_snapshot", "get_shader_source"}, []string{"snapshot", "set_media", "session_info"}},
		{"core,devtools", coreDev, []string{"evaluate_script", "snapshot", "set_media"}, []string{"heap_snapshot"}},
	}
	for _, c := range cases {
		for _, n := range c.has {
			if !slices.Contains(c.tools, n) {
				t.Errorf("profile %s lacks %s", c.name, n)
			}
		}
		for _, n := range c.not {
			if slices.Contains(c.tools, n) {
				t.Errorf("profile %s has %s", c.name, n)
			}
		}
	}
	if len(legacy) < 116 {
		t.Errorf("profile legacy has %d tools, want every legacy tool (116+)", len(legacy))
	}
	if !slices.Equal(every, all) {
		t.Errorf("devtools,legacy,core registers %d tools, all %d; want the same set", len(every), len(all))
	}
	for _, n := range append(slices.Clone(core), dev...) {
		if !slices.Contains(coreDev, n) {
			t.Errorf("core,devtools lacks %s", n)
		}
	}
}

// The --tools flag overrides IWDP_MCP_TOOLS; an unknown or missing value
// is an error that names the problem.
func TestToolsFlag(t *testing.T) {
	cases := []struct {
		args []string
		env  string // IWDP_MCP_TOOLS afterwards
		bad  string // text of the error
	}{
		{args: nil, env: "core"},
		{args: []string{"--tools", "core,devtools"}, env: "core,devtools"},
		{args: []string{"--tools=all"}, env: "all"},
		{args: []string{"--tools", "core,bogus"}, bad: `unknown profile "bogus"`},
		{args: []string{"--tools"}, bad: "--tools needs a value"},
	}
	for _, c := range cases {
		t.Setenv("IWDP_MCP_TOOLS", "core")
		err := selectProfile(c.args)
		switch {
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
			t.Errorf("%v: %v; want an error with %q", c.args, err, c.bad)
		case c.bad == "" && (err != nil || os.Getenv("IWDP_MCP_TOOLS") != c.env):
			t.Errorf("%v: %v, IWDP_MCP_TOOLS=%q; want %q", c.args, err, os.Getenv("IWDP_MCP_TOOLS"), c.env)
		}
	}
	t.Setenv("IWDP_MCP_TOOLS", "nope")
	if err := selectProfile(nil); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("IWDP_MCP_TOOLS=nope: %v; want an error naming it", err)
	}
}
