// Package bundlegen builds the installed agent runtime bundle from the
// readable sources in internal/runtime/js (design 3.1). go generate in
// internal/runtime writes its output to bundle.js, which is checked in;
// internal/runtime's tests check that bundle.js matches the sources. The
// package uses esbuild's Go API (decision D4, amended in
// docs/notes/w3b-actions.md) and is never linked into the binaries.
package bundlegen

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Header is the first line of the bundle; it names the script in protocol
// logs.
const Header = "// iwdp-mcp agent runtime bundle, generated from internal/runtime/js by go generate (design 3.1)\n"

// Build joins the files js/*.js of fsys in name order and minifies them
// as one ES2017 script: whitespace, syntax and local identifiers. Property
// names and strings stay, so the runtime's function names, the version
// placeholder and window.__iwdpBuilding are unchanged.
func Build(fsys fs.FS) (string, error) {
	names, err := fs.Glob(fsys, "js/*.js")
	if err != nil {
		return "", fmt.Errorf("listing the runtime sources: %w", err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no runtime sources in js/")
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
	r := api.Transform(src.String(), api.TransformOptions{
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
	return Header + string(r.Code), nil
}
