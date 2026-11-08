//go:build webkitgtk

package webkitgtk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// W1c T04 (the W1b and W3b harness rules): every helper of this package
// that starts MiniBrowser puts it on a display of the shared broadway
// helper (broadway or startBroadway: a random free display, retried when
// broadwayd does not come up) and waits for its page at most browserStart
// (60 s). A helper with a fixed display range collides with the suites of
// other test processes on the host, and a 15 s start limit failed on a
// loaded host; both end as "MiniBrowser did not list the page". Only
// runBroadwayd runs broadwayd.
//
// The check reads the package's Go files with go/ast and inspects the
// expressions that do the work: the display that MiniBrowser gets, the
// deadline of the loop that polls the inspector's page list, and the
// value of browserStart. A single owning wait function would not cover
// every copy: newNavEnv (navigation_harness_test.go) belongs to another
// work item, and runSuite runs without a *testing.T. The file uses only
// the standard library, so it also runs alone on other sources.
func TestBrowserHelpersUseTheSharedBroadwayAndStartGuard(t *testing.T) {
	fset, files := parsePackage(t)
	findings, helpers := checkBrowserHelpers(fset, files)
	for _, f := range findings {
		t.Error(f)
	}
	// The helpers of this package when W1c added the check: it must see
	// each of them (a check that sees none passes for nothing).
	for _, want := range []string{"browser", "runSuite", "runtimeBrowser", "devtoolsBrowser", "managedBrowser", "newNavEnv", "backAndForward"} {
		if !slices.Contains(helpers, want) {
			t.Errorf("the check did not see the MiniBrowser helper %s (saw %v)", want, helpers)
		}
	}
}

// browserStart is 60 s: the guard against a hang of MiniBrowser, never a
// short start limit.
func TestBrowserStartIsTheSixtySecondGuard(t *testing.T) {
	_, files := parsePackage(t)
	if err := checkBrowserStart(files); err != nil {
		t.Fatal(err)
	}
}

// The check finds each way a helper can lose the shared guard or display,
// also when its failure message still names browserStart (review F1).
func TestBrowserHelperCheckFindsEachDefect(t *testing.T) {
	const good = `
	display := broadway(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display}, miniBrowser, url)
	for end := time.Now().Add(browserStart); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if pages, err := tr.Pages(ctx, id); err == nil && len(pages) > 0 {
			return
		}
	}
	t.Fatalf("MiniBrowser did not list the page within %v", browserStart)
`
	cases := []struct {
		name, body, want string // want "" = no finding
	}{
		{"current helper", good, ""},
		{"wait ended by the deadline in the body", strings.NewReplacer("time.Now().Before(end); time.Sleep", "; time.Sleep", "\t\tif pages", "\t\tif time.Now().After(end) {\n\t\t\tt.Fatal()\n\t\t}\n\t\tif pages").Replace(good), ""},
		{"15 s deadline with the browserStart message", strings.Replace(good, "Add(browserStart)", "Add(15 * time.Second)", 1), "with [end = time.Now().Add(15 * time.Second)]"},
		{"another constant", strings.Replace(good, "Add(browserStart)", "Add(cdpStart)", 1), "with [end = time.Now().Add(cdpStart)]"},
		{"browserStart deadline that the wait ignores", strings.Replace(good, "time.Now().Before(end)", "time.Now().Before(stop)", 1), "with [stop]"},
		{"context deadline", strings.Replace(good, "for end := time.Now().Add(browserStart); time.Now().Before(end); time.Sleep(200 * time.Millisecond)", "ctx, cancel := context.WithTimeout(ctx, 15*time.Second)\n\tdefer cancel()\n\tfor ctx.Err() == nil", 1), "not bounded by a browserStart deadline"},
		{"no page wait", "\tdisplay := broadway(t)\n\tstartProcess(t, []string{\"BROADWAY_DISPLAY=\" + display}, miniBrowser, url)\n", "no loop that polls the page list"},
		{"fixed display", strings.Replace(good, "\"BROADWAY_DISPLAY=\" + display", "\"BROADWAY_DISPLAY=:250\"", 1), "display of broadway"},
		{"display not from broadway", strings.Replace(good, "display := broadway(t)", "display := fmt.Sprintf(\":%d\", 250)", 1), "display of broadway"},
		{"own broadwayd", strings.Replace(good, "\tdisplay := broadway(t)", "\tdisplay := \":250\"\n\tstartProcess(t, nil, \"broadwayd\", display)", 1), "starts broadwayd itself"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package webkitgtk\n\nfunc helper(t *testing.T) {\n" + tc.body + "\n}\n"
			f, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("fixture: %v\n%s", err, src)
			}
			findings, helpers := checkBrowserHelpers(fset, []*ast.File{f})
			if !slices.Equal(helpers, []string{"helper"}) {
				t.Fatalf("the check saw the helpers %v in the fixture, want [helper]", helpers)
			}
			got := strings.Join(findings, "\n")
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("findings %q, want %q\nfixture:\n%s", got, tc.want, src)
			}
		})
	}
	for _, c := range []struct{ value, want string }{
		{"60 * time.Second", ""},
		{"15 * time.Second", "browserStart is 15 * time.Second"},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package webkitgtk\n\nconst browserStart = "+c.value+"\n", 0)
		if err != nil {
			t.Fatal(err)
		}
		err = checkBrowserStart([]*ast.File{f})
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("checkBrowserStart(%s) = %v, want %q", c.value, err, c.want)
		}
	}
}

func parsePackage(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return fset, files
}

// checkBrowserStart reports an error unless the files declare browserStart
// once, as 60 * time.Second.
func checkBrowserStart(files []*ast.File) error {
	var values []string
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if n.Name == "browserStart" && i < len(vs.Values) {
						values = append(values, types.ExprString(vs.Values[i]))
					}
				}
			}
		}
	}
	if len(values) != 1 {
		return fmt.Errorf("browserStart is declared %d times (%v), want once", len(values), values)
	}
	if values[0] != "60 * time.Second" {
		return fmt.Errorf("browserStart is %s, want 60 * time.Second", values[0])
	}
	return nil
}

// checkBrowserHelpers returns the findings in files and the names of the
// functions that start MiniBrowser. Each such function must
//   - give MiniBrowser BROADWAY_DISPLAY=<v>, v assigned from broadway(...)
//     or startBroadway(...);
//   - poll the page list (a call of Pages) in a loop that compares
//     time.Now() (Before or After) only with variables assigned
//     time.Now().Add(browserStart), at least once.
//
// No function but runBroadwayd starts broadwayd.
func checkBrowserHelpers(fset *token.FileSet, files []*ast.File) (findings, helpers []string) {
	report := func(pos token.Pos, fn, format string, args ...any) {
		findings = append(findings, fmt.Sprintf("%s: %s: %s", fset.Position(pos), fn, fmt.Sprintf(format, args...)))
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			startsBrowser := false
			displays := map[string]bool{} // variables assigned broadway(...) or startBroadway(...)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					if len(n.Rhs) != 1 || len(n.Lhs) == 0 {
						return true
					}
					id, ok := n.Lhs[0].(*ast.Ident)
					if !ok {
						return true
					}
					if c, ok := n.Rhs[0].(*ast.CallExpr); ok && (calleeName(c) == "broadway" || calleeName(c) == "startBroadway") {
						displays[id.Name] = true
					}
				case *ast.CallExpr:
					if !startsProcess(calleeName(n)) {
						return true
					}
					for _, a := range n.Args {
						if isIdent(a, "miniBrowser") {
							startsBrowser = true
						}
						if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, _ := strconv.Unquote(lit.Value); s == "broadwayd" && name != "runBroadwayd" {
								report(n.Pos(), name, "starts broadwayd itself; use broadway(t)")
							}
						}
					}
				}
				return true
			})
			if !startsBrowser {
				continue
			}
			helpers = append(helpers, name)
			checkDisplay(fn, displays, func(pos token.Pos, msg string) { report(pos, name, "%s", msg) })
			checkPageWait(fn, func(pos token.Pos, msg string) { report(pos, name, "%s", msg) })
		}
	}
	return findings, helpers
}

// checkDisplay: every BROADWAY_DISPLAY setting of fn is "BROADWAY_DISPLAY="
// plus a variable in displays, and there is at least one.
func checkDisplay(fn *ast.FuncDecl, displays map[string]bool, report func(token.Pos, string)) {
	settings := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var lit *ast.BasicLit
		var operand ast.Expr
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if l, ok := n.X.(*ast.BasicLit); ok && n.Op == token.ADD {
				lit, operand = l, n.Y
			}
		case *ast.BasicLit:
			lit = n
		}
		if lit == nil || lit.Kind != token.STRING {
			return true
		}
		s, _ := strconv.Unquote(lit.Value)
		if !strings.HasPrefix(s, "BROADWAY_DISPLAY=") {
			return true
		}
		settings++
		if id, ok := operand.(*ast.Ident); !ok || s != "BROADWAY_DISPLAY=" || !displays[id.Name] {
			report(n.Pos(), "MiniBrowser's BROADWAY_DISPLAY is not the display of broadway(t) or startBroadway()")
		}
		return false // the literal inside a checked BinaryExpr is not a second setting
	})
	if settings == 0 {
		report(fn.Pos(), "starts MiniBrowser without BROADWAY_DISPLAY from the display of broadway(t) or startBroadway()")
	}
}

// checkPageWait: fn has a loop that polls the page list, and every such
// loop compares time.Now() only with browserStart deadlines, at least once.
// A deadline is a variable assigned time.Now().Add(d) in the loop's init
// or, outside it, in fn's own body; function literals have their own
// variables (a closure of backAndForward reuses the name end) and are
// skipped.
func checkPageWait(fn *ast.FuncDecl, report func(token.Pos, string)) {
	outer := deadlineVars(fn.Body)
	loops := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		loop, ok := n.(*ast.ForStmt)
		if !ok || !callsMethod(loop.Body, "Pages") {
			return true
		}
		loops++
		deadlines := outer
		if loop.Init != nil {
			deadlines = map[string]string{}
			for k, v := range outer {
				deadlines[k] = v
			}
			for k, v := range deadlineVars(loop.Init) {
				deadlines[k] = v
			}
		}
		bounded := 0
		var other []string // the other times the loop compares with
		for _, part := range []ast.Node{loop.Cond, loop.Body} {
			if part == nil {
				continue
			}
			ast.Inspect(part, func(m ast.Node) bool {
				for _, cmp := range []string{"Before", "After"} {
					if arg, ok := timeNowCall(m, cmp); ok {
						id, ok := arg.(*ast.Ident)
						switch {
						case ok && deadlines[id.Name] == "browserStart":
							bounded++
						case ok && deadlines[id.Name] != "":
							other = append(other, fmt.Sprintf("%s = time.Now().Add(%s)", id.Name, deadlines[id.Name]))
						default:
							other = append(other, types.ExprString(arg))
						}
					}
				}
				return true
			})
		}
		if bounded == 0 || len(other) > 0 {
			report(loop.Pos(), fmt.Sprintf("the loop that polls the page list is not bounded by a browserStart deadline (time.Now().Add(browserStart)); it compares time.Now() with %v", other))
		}
		return true
	})
	if loops == 0 {
		report(fn.Pos(), "starts MiniBrowser with no loop that polls the page list")
	}
}

// deadlineVars maps the variables that n assigns time.Now().Add(d) to d,
// outside function literals.
func deadlineVars(n ast.Node) map[string]string {
	out := map[string]string{}
	ast.Inspect(n, func(m ast.Node) bool {
		if _, ok := m.(*ast.FuncLit); ok {
			return false
		}
		a, ok := m.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
			return true
		}
		if id, ok := a.Lhs[0].(*ast.Ident); ok {
			if arg, ok := timeNowCall(a.Rhs[0], "Add"); ok {
				out[id.Name] = types.ExprString(arg)
			}
		}
		return true
	})
	return out
}

// timeNowCall reports whether n is time.Now().<method>(arg) and returns arg.
func timeNowCall(n ast.Node, method string) (ast.Expr, bool) {
	c, ok := n.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return nil, false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return nil, false
	}
	now, ok := sel.X.(*ast.CallExpr)
	if !ok || len(now.Args) != 0 {
		return nil, false
	}
	nsel, ok := now.Fun.(*ast.SelectorExpr)
	if !ok || nsel.Sel.Name != "Now" || !isIdent(nsel.X, "time") {
		return nil, false
	}
	return c.Args[0], true
}

// callsMethod reports whether n contains a call of x.<method>(...).
func callsMethod(n ast.Node, method string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if c, ok := m.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				found = true
			}
		}
		return !found
	})
	return found
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// calleeName is the name of the function a call calls: f for f(...),
// Command for exec.Command(...).
func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// startsProcess reports whether a call of callee starts a process of this
// harness: startProcess, exec.Command and runSuite's start.
func startsProcess(callee string) bool {
	switch callee {
	case "startProcess", "Command", "CommandContext", "start":
		return true
	}
	return false
}
