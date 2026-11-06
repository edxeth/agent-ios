package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

// checkES2017 fails when src does not parse or uses syntax newer than
// ES2017: esbuild lowers newer syntax for an ES2017 target, so the ES2017
// output differs from the ESNext output (decision D4: a syntax guard only,
// not an API check).
func checkES2017(src string) error {
	transform := func(target api.Target) (string, error) {
		r := api.Transform(src, api.TransformOptions{Target: target, Loader: api.LoaderJS})
		if len(r.Errors) > 0 {
			var msgs []string
			for _, e := range r.Errors {
				msgs = append(msgs, e.Text)
			}
			return "", fmt.Errorf("JavaScript does not parse: %s", strings.Join(msgs, "; "))
		}
		return string(r.Code), nil
	}
	next, err := transform(api.ESNext)
	if err != nil {
		return err
	}
	es2017, err := transform(api.ES2017)
	if err != nil {
		return err
	}
	if next != es2017 {
		return fmt.Errorf("syntax newer than ES2017; esbuild lowered it to:\n%s", es2017)
	}
	return nil
}

func TestRuntimeBundleIsES2017Syntax(t *testing.T) {
	if err := checkES2017(Source()); err != nil {
		t.Fatalf("runtime bundle: %v", err)
	}
}

func TestES2017GuardDetectsNewerSyntax(t *testing.T) {
	for _, src := range []string{
		"var a = b?.c;",
		"var a = b ?? c;",
		"class A { x = 1 }",
		"try { f() } catch { g() }",
		"var o = {...p};",
	} {
		if err := checkES2017(src); err == nil || !strings.Contains(err.Error(), "newer than ES2017") {
			t.Fatalf("guard result for %q = %v, want newer syntax reported", src, err)
		}
	}
	if err := checkES2017("async function f() { await g(); return [1, 2].includes(1) ** 2; }"); err != nil {
		t.Fatalf("guard rejected ES2017 syntax: %v", err)
	}
}

// The guard is a syntax check: JavaScript that does not parse fails it,
// even when both targets report the same parse error.
func TestES2017GuardRejectsInvalidSyntax(t *testing.T) {
	if err := checkES2017("const ="); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("guard result for invalid JavaScript = %v, want a parse failure", err)
	}
}
