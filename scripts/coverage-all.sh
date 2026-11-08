#!/usr/bin/env bash
# coverage-all.sh writes one merged Go coverage profile (go tool cover text
# format) from the unit tests and the WebKitGTK suites (e2e/webkitgtk,
# build tag webkitgtk). Most command run functions execute only against a
# real WebKit page, so a profile of the unit tests alone misses them.
#
# Usage: scripts/coverage-all.sh [output]   (default: coverage.out)
#
# Coverage is collected with -coverpkg over the whole module. Binaries that
# the tests start as subprocesses are covered too: the unit tests re-run
# their own test binary, and the WebKitGTK suite builds iwdp-cli with go
# build, which takes -cover from GOFLAGS; both write to GOCOVERDIR. The
# WebKitGTK suites need MiniBrowser, broadwayd, gcc and pkg-config gdk-3.0
# (see e2e/webkitgtk); the script fails when any test fails.
set -euo pipefail

out=$(realpath -m "${1:-coverage.out}")
cd "$(dirname "$0")/.."
mod=$(go list -m)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/unit" "$work/webkitgtk" "$work/merged"

echo "==> unit tests" >&2
GOCOVERDIR="$work/unit" go test ./... -count=1 -coverpkg=./... -args -test.gocoverdir="$work/unit"

echo "==> WebKitGTK suites" >&2
GOFLAGS="${GOFLAGS:+$GOFLAGS }-cover -coverpkg=$mod/..." GOCOVERDIR="$work/webkitgtk" \
	go test -tags webkitgtk -count=1 -timeout 30m ./e2e/webkitgtk/ -args -test.gocoverdir="$work/webkitgtk"

go tool covdata merge -i="$work/unit,$work/webkitgtk" -o="$work/merged"
go tool covdata textfmt -i="$work/merged" -o="$out"
echo "==> wrote $out" >&2
