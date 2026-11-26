// Package bundlegen builds the installed agent runtime from the readable
// sources in internal/runtime/js (design 3.1): the core from js/*.js and
// one feature module from each directory js/mod-<name>/ (parity-045
// decision D5). go generate in internal/runtime writes the core to
// bundle.js and each module to mod-<name>.js, which are checked in;
// internal/runtime's tests check that they match the sources. The package
// uses esbuild's Go API (decision D4, amended in docs/notes/w3b-actions.md)
// and is never linked into the binaries.
package bundlegen

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Header is the first line of the core bundle; it names the script in
// protocol logs.
const Header = "// agent-ios agent runtime bundle, generated from internal/runtime/js by go generate (design 3.1)\n"

// ModuleHeader is the first line of a module (%s: its name).
const ModuleHeader = "// agent-ios agent runtime module %s, generated from internal/runtime/js/mod-%s by go generate (design 3.1)\n"

// moduleName is the name of a module directory js/mod-<name>.
var moduleName = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// Build joins the files js/*.js of fsys in name order and minifies them
// as one ES2017 script: whitespace, syntax and local identifiers. Property
// names and strings stay, so the runtime's function names, the version
// placeholder and window.__iwdpBuilding are unchanged.
func Build(fsys fs.FS) (string, error) {
	src, err := join(fsys, "js")
	if err != nil {
		return "", err
	}
	out, err := minify(src)
	if err != nil {
		return "", err
	}
	return Header + out, nil
}

// Modules lists the module names of fsys: the directories js/mod-<name>.
func Modules(fsys fs.FS) ([]string, error) {
	dirs, err := fs.Glob(fsys, "js/mod-*")
	if err != nil {
		return nil, fmt.Errorf("listing the runtime modules: %w", err)
	}
	var names []string
	for _, d := range dirs {
		n := strings.TrimPrefix(path.Base(d), "mod-")
		if !moduleName.MatchString(n) {
			return nil, fmt.Errorf("runtime module directory %s: the name must match %s", d, moduleName)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// moduleWrapper installs a module's files on the core of the same version
// and marks the module installed; on another or no core it changes nothing
// and answers the missing sentinel. A second install of an installed
// module changes nothing. The files read the core from
// window.__iwdpBuilding, as the core's later files do.
const moduleWrapper = `(function () {
  var w = window, I = w.__iwdp, N = %q;
  if (!I || I.v !== "__IWDP_VERSION__" || typeof I.has !== "function") { return "__IWDP_MISSING__"; }
  if (I.has(N)) { return true; }
  Object.defineProperty(w, "__iwdpBuilding", { value: I, configurable: true, writable: true, enumerable: false });
  try {
%s
  } finally {
    try { delete w.__iwdpBuilding; } catch (e) {}
  }
  I._.mod(N);
  return true;
})();
`

// BuildModule builds module name from the files js/mod-<name>/*.js of
// fsys in name order, wrapped for its install and minified as Build does.
func BuildModule(fsys fs.FS, name string) (string, error) {
	if !moduleName.MatchString(name) {
		return "", fmt.Errorf("runtime module %q: the name must match %s", name, moduleName)
	}
	src, err := join(fsys, "js/mod-"+name)
	if err != nil {
		return "", err
	}
	out, err := minify(fmt.Sprintf(moduleWrapper, name, src))
	if err != nil {
		return "", fmt.Errorf("runtime module %s: %w", name, err)
	}
	return fmt.Sprintf(ModuleHeader, name, name) + out, nil
}

func join(fsys fs.FS, dir string) (string, error) {
	names, err := fs.Glob(fsys, dir+"/*.js")
	if err != nil {
		return "", fmt.Errorf("listing the runtime sources in %s: %w", dir, err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no runtime sources in %s/", dir)
	}
	sort.Strings(names)
	var src strings.Builder
	for _, n := range names {
		data, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", n, err)
		}
		src.Write(data)
		src.WriteString("\n")
	}
	return src.String(), nil
}

func minify(src string) (string, error) {
	r := api.Transform(src, api.TransformOptions{
		Loader:            api.LoaderJS,
		Target:            api.ES2017,
		MinifyWhitespace:  true,
		MinifySyntax:      true,
		MinifyIdentifiers: true,
		LegalComments:     api.LegalCommentsNone,
		Charset:           api.CharsetASCII,
	})
	if len(r.Errors) > 0 {
		var msgs []string
		for _, e := range r.Errors {
			msgs = append(msgs, e.Text)
		}
		return "", fmt.Errorf("minifying the runtime: %s", strings.Join(msgs, "; "))
	}
	return string(r.Code), nil
}
