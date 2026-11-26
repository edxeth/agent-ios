//go:build ignore

// gen_bundle writes bundle.js, the minified core of the runtime, from
// js/*.js, and mod-<name>.js, one minified module, from each js/mod-<name>/
// (go generate in internal/runtime; see package bundlegen). It removes
// the files of modules whose sources are gone.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/edxeth/agent-ios/internal/runtime/bundlegen"
)

func main() {
	fsys := os.DirFS(".")
	out, err := bundlegen.Build(fsys)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("bundle.js", []byte(out), 0o644); err != nil {
		log.Fatal(err)
	}
	names, err := bundlegen.Modules(fsys)
	if err != nil {
		log.Fatal(err)
	}
	keep := map[string]bool{}
	for _, n := range names {
		out, err := bundlegen.BuildModule(fsys, n)
		if err != nil {
			log.Fatal(err)
		}
		f := "mod-" + n + ".js"
		keep[f] = true
		if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	old, err := filepath.Glob("mod-*.js")
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range old {
		if !keep[f] {
			if err := os.Remove(f); err != nil {
				log.Fatal(err)
			}
		}
	}
}
