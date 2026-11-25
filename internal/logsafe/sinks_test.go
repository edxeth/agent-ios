package logsafe

import (
	"bytes"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// sinkPackages are the packages whose code runs in the daemon and writes
// its log.
var sinkPackages = []string{
	"internal/cdp", "internal/commands", "internal/core", "internal/daemon",
	"internal/devtools", "internal/proxy", "internal/webkit",
}

// logFuncs are the names of the log functions of sinkPackages: Logf of
// the options of core, CDP and the WebKit session, the daemon's logf, and
// the page log helpers (core pageLogf, the page connection's logf, the
// devtools and CDP page logf).
var logFuncs = map[string]bool{"Logf": true, "logf": true, "pageLogf": true}

const (
	module      = "github.com/nnemirovsky/agent-ios/"
	logsafePath = module + "internal/logsafe"
)

// checkedPackage is a type-checked package of sinkPackages, without its
// tests.
type checkedPackage struct {
	path  string
	files []*ast.File
	info  *types.Info
}

var (
	loadOnce sync.Once
	loaded   []checkedPackage
	loadFset *token.FileSet
	loadErr  string
)

// sinkCode type-checks every non-test file of sinkPackages (for the
// default build tags), once for all the tests here.
func sinkCode(t *testing.T) ([]checkedPackage, *token.FileSet) {
	t.Helper()
	loadOnce.Do(func() { loaded, loadFset, loadErr = load() })
	if loadErr != "" {
		t.Fatal(loadErr)
	}
	return loaded, loadFset
}

func load() ([]checkedPackage, *token.FileSet, string) {
	var paths []string
	for _, p := range sinkPackages {
		paths = append(paths, module+p)
	}
	lines, errText := goList(append([]string{"-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}, paths...))
	if errText != "" {
		return nil, nil, errText
	}
	exports := map[string]string{}
	for _, line := range lines {
		path, file, _ := strings.Cut(line, "\t")
		exports[path] = file
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
	lines, errText = goList(append([]string{"-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \" \"}}"}, paths...))
	if errText != "" {
		return nil, nil, errText
	}
	var pkgs []checkedPackage
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		p := checkedPackage{path: fields[0], info: &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{},
			Uses:  map[*ast.Ident]types.Object{},
			Defs:  map[*ast.Ident]types.Object{},
		}}
		for _, name := range strings.Fields(fields[2]) {
			f, err := parser.ParseFile(fset, filepath.Join(fields[1], name), nil, 0)
			if err != nil {
				return nil, nil, err.Error()
			}
			p.files = append(p.files, f)
		}
		if _, err := (&types.Config{Importer: imp}).Check(p.path, fset, p.files, p.info); err != nil {
			return nil, nil, "type-checking " + p.path + ": " + err.Error()
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, fset, ""
}

// The sink check of review devfix-R2 F5: each argument of a log call in
// the daemon's packages is of a safe kind, so the daemon log gets no
// page-supplied text. The format is a constant; every other argument is a
// constant, a number (counts, epochs, codes, durations and states, whose
// types are numbers), a logsafe.ID or logsafe.Label, the result of a
// logsafe function (Error), or the arguments of a log helper forwarded as
// they are (fmt.Sprintf(format, args...) of its own parameters). A raw
// error, a string variable, an interface value and a formatted string all
// fail.
//
// Limits: the check is static and local to the call. It trusts every
// conversion to logsafe.ID (each is a reviewed statement that the value is
// an identifier), a one-argument filter of forwarded text (the CDP
// server's redact), and the forwarded arguments of a helper, whose own
// callers it checks only when they are named Logf, logf or pageLogf. It
// sees the default build only (not the webkitgtk files), and log calls
// through other names (a logger stored under another name) are not found.
func TestLogCallsWriteOnlySafeKinds(t *testing.T) {
	pkgs, fset := sinkCode(t)
	var bad []string
	for _, p := range pkgs {
		for _, f := range p.files {
			forEachCall(f, func(call *ast.CallExpr, encl *ast.FuncType) {
				if !logFuncs[funcName(call.Fun)] || len(call.Args) == 0 {
					return
				}
				if call.Ellipsis.IsValid() {
					// A helper forwards its own format and arguments; its
					// callers are checked.
					at := formatIndex(call, p.info)
					if at < 0 || at != len(call.Args)-2 || !forwards(call.Args[at], call.Args[at+1], encl, p.info) {
						bad = append(bad, fset.Position(call.Pos()).String()+": forwards arguments that are not its own format and arguments")
					}
					return
				}
				// The format is the parameter before the variadic one;
				// the arguments before it (the page of pageLogf) are the
				// helper's own context, which it renders.
				at := formatIndex(call, p.info)
				if at < 0 || at >= len(call.Args) {
					bad = append(bad, fset.Position(call.Pos()).String()+": no format parameter before the variadic arguments")
					return
				}
				if tv := p.info.Types[call.Args[at]]; tv.Value == nil || tv.Value.Kind() != constant.String {
					bad = append(bad, fset.Position(call.Args[at].Pos()).String()+": the format is not a constant string")
				}
				for _, arg := range call.Args[at+1:] {
					if why := unsafeArg(arg, encl, p.info); why != "" {
						bad = append(bad, fset.Position(arg.Pos()).String()+": "+why)
					}
				}
			})
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("log calls with an argument that is not of a safe kind (render errors with logsafe.Error; name a page by its id, a logsafe.ID; name an operation in the format or with a constant logsafe.Label):\n%s", strings.Join(bad, "\n"))
	}
}

// Every conversion to logsafe.Label in the daemon's packages converts a
// constant, so a Label is always a text of the program (review devfix-R2
// F5: operation labels only from constant strings).
func TestLabelsAreConstants(t *testing.T) {
	pkgs, fset := sinkCode(t)
	var bad []string
	for _, p := range pkgs {
		for _, f := range p.files {
			forEachCall(f, func(call *ast.CallExpr, _ *ast.FuncType) {
				tv, ok := p.info.Types[call.Fun]
				if !ok || !tv.IsType() || !isLogsafe(tv.Type, "Label") || len(call.Args) != 1 {
					return
				}
				if p.info.Types[call.Args[0]].Value == nil {
					bad = append(bad, fset.Position(call.Pos()).String())
				}
			})
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("conversions of a value that is not a constant to logsafe.Label:\n%s", strings.Join(bad, "\n"))
	}
}

// Review devfix-R2 F5 item 4: no error constructor of the daemon's
// packages flattens an error into text (an error formatted with a verb
// other than %w, or the text of an Error method), because Error
// classifies only the errors that stay in the chain. An error that
// explains another without joining its chain is built with
// logcause.Errorf.
//
// Limits: only fmt.Errorf calls with a constant format are checked; an
// error text built with fmt.Sprintf or string concatenation is not found.
func TestErrorConstructorsKeepTheChain(t *testing.T) {
	pkgs, fset := sinkCode(t)
	errorType := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	var bad []string
	for _, p := range pkgs {
		for _, f := range p.files {
			forEachCall(f, func(call *ast.CallExpr, _ *ast.FuncType) {
				if !isFunc(call.Fun, p.info, "fmt", "Errorf") || len(call.Args) == 0 {
					return
				}
				tv := p.info.Types[call.Args[0]]
				if tv.Value == nil || tv.Value.Kind() != constant.String {
					return
				}
				verbs := formatVerbs(constant.StringVal(tv.Value))
				for i, arg := range call.Args[1:] {
					if i < len(verbs) && verbs[i] == 'w' {
						continue
					}
					if inner, ok := arg.(*ast.CallExpr); ok && len(inner.Args) == 0 && funcName(inner.Fun) == "Error" {
						bad = append(bad, fset.Position(arg.Pos()).String()+": the text of an Error method")
						continue
					}
					if at, ok := p.info.Types[arg]; ok && at.Type != nil && types.Implements(at.Type, errorType) {
						bad = append(bad, fset.Position(arg.Pos()).String()+": an error formatted with a verb other than %w")
					}
				}
			})
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("error constructors that flatten an error into text (wrap it with %%w, or build it with logcause.Errorf):\n%s", strings.Join(bad, "\n"))
	}
}

// unsafeArg says why arg, an argument of a log call after its format, is
// not of a safe kind, or returns "".
func unsafeArg(arg ast.Expr, encl *ast.FuncType, info *types.Info) string {
	tv, ok := info.Types[arg]
	if !ok || tv.Type == nil {
		return "an argument of unknown type"
	}
	if tv.Value != nil {
		return ""
	}
	if b, ok := tv.Type.Underlying().(*types.Basic); ok && b.Info()&(types.IsNumeric|types.IsBoolean) != 0 {
		return ""
	}
	if isLogsafe(tv.Type, "ID") || isLogsafe(tv.Type, "Label") {
		return ""
	}
	if call, ok := ast.Unparen(arg).(*ast.CallExpr); ok {
		if fn := calledFunc(call.Fun, info); fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == logsafePath {
			return ""
		}
		if forwardedText(call, encl, info) {
			return ""
		}
		// A filter of the forwarded text (the CDP server's redact).
		if sig, ok := info.Types[call.Fun].Type.(*types.Signature); ok && len(call.Args) == 1 && sig.Params().Len() == 1 && sig.Results().Len() == 1 {
			if inner, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr); ok && forwardedText(inner, encl, info) {
				return ""
			}
		}
	}
	return "a value of type " + tv.Type.String()
}

// formatIndex is the index of the format argument of call, a call of a
// log function: the parameter before its variadic one, or -1.
func formatIndex(call *ast.CallExpr, info *types.Info) int {
	sig, ok := info.Types[call.Fun].Type.(*types.Signature)
	if !ok || !sig.Variadic() || sig.Params().Len() < 2 {
		return -1
	}
	return sig.Params().Len() - 2
}

// forwardedText reports whether call is fmt.Sprintf(format, args...) of
// the format and arguments of the log helper encl that holds it.
func forwardedText(call *ast.CallExpr, encl *ast.FuncType, info *types.Info) bool {
	return isFunc(call.Fun, info, "fmt", "Sprintf") && call.Ellipsis.IsValid() && len(call.Args) == 2 && forwards(call.Args[0], call.Args[1], encl, info)
}

// forwards reports whether format and args are the string and variadic
// parameters of the function encl that holds the call.
func forwards(format, args ast.Expr, encl *ast.FuncType, info *types.Info) bool {
	if encl == nil || encl.Params == nil {
		return false
	}
	var params []types.Object
	for _, field := range encl.Params.List {
		for _, name := range field.Names {
			params = append(params, info.Defs[name])
		}
	}
	if len(params) < 2 {
		return false
	}
	return identIs(format, params[len(params)-2], info) && identIs(args, params[len(params)-1], info)
}

func identIs(e ast.Expr, obj types.Object, info *types.Info) bool {
	id, ok := e.(*ast.Ident)
	return ok && obj != nil && info.Uses[id] == obj
}

func isLogsafe(t types.Type, name string) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == logsafePath && n.Obj().Name() == name
}

func calledFunc(fun ast.Expr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

func isFunc(fun ast.Expr, info *types.Info, pkg, name string) bool {
	fn := calledFunc(fun, info)
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == pkg && fn.Name() == name
}

// forEachCall calls visit for every call in f with the type of the
// innermost function (declaration or literal) that holds it.
func forEachCall(f *ast.File, visit func(*ast.CallExpr, *ast.FuncType)) {
	var walk func(n ast.Node, encl *ast.FuncType)
	walk = func(n ast.Node, encl *ast.FuncType) {
		ast.Inspect(n, func(c ast.Node) bool {
			switch c := c.(type) {
			case *ast.FuncDecl:
				if c.Body != nil && c != n {
					walk(c.Body, c.Type)
					return false
				}
			case *ast.FuncLit:
				if c != n {
					walk(c.Body, c.Type)
					return false
				}
			case *ast.CallExpr:
				visit(c, encl)
			}
			return true
		})
	}
	walk(f, nil)
}

// formatVerbs returns the verb of each argument that format consumes, in
// order ('*' for a width or precision argument).
func formatVerbs(format string) []rune {
	var verbs []rune
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		for i++; i < len(format); i++ {
			c := format[i]
			if c == '*' {
				verbs = append(verbs, '*')
				continue
			}
			if strings.IndexByte("+-# 0123456789.[]", c) >= 0 {
				continue
			}
			if c != '%' {
				verbs = append(verbs, rune(c))
			}
			break
		}
	}
	return verbs
}

func funcName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func goList(args []string) ([]string, string) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, "go list " + strings.Join(args, " ") + ": " + err.Error() + "\n" + stderr.String()
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), ""
}
