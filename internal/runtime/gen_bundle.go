//go:build ignore

// gen_bundle writes bundle.js, the minified runtime bundle, from js/*.js
// (go generate in internal/runtime; see package bundlegen).
package main

import (
	"log"
	"os"

	"github.com/nnemirovsky/iwdp-mcp/internal/runtime/bundlegen"
)

func main() {
	out, err := bundlegen.Build(os.DirFS("."))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("bundle.js", []byte(out), 0o644); err != nil {
		log.Fatal(err)
	}
}
