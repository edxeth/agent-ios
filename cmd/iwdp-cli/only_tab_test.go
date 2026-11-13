package main

import (
	"strings"
	"testing"
)

// --only-tab through the CLI and its daemon (the request's OnlyTab): with
// two tabs a page command exits 5 with the two counts and no title; with
// one tab it runs there. With --page it is a usage error (exit 2).
func TestCLIOnlyTabRunsOnTheOnlyTab(t *testing.T) {
	p := newCLITwoPages(t)
	res := runCLI(t, "", p.env, "--only-tab", "eval", "1")
	wantExit(t, res, 5, "--only-tab needs a listing of exactly one tab and no hidden page; it shows 2 tab(s) and 0 hidden page(s)")
	if strings.Contains(res.stderr, "Fixture") || strings.Contains(res.stderr, "Other") {
		t.Fatalf("--only-tab with two tabs named a page: %q", res.stderr)
	}
	wantExit(t, runCLI(t, "", p.env, "--only-tab", "--page", "t1", "eval", "1"), 2, "--only-tab and --page")

	p.dev.RemovePage(p.otherListing)
	res = runCLI(t, "", p.env, "--only-tab", "eval", "1")
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("--only-tab eval 1 with one tab: exit %d, stdout %q, stderr %q; want 2", res.code, res.stdout, res.stderr)
	}
}
