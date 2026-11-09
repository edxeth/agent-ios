#!/usr/bin/env bash
# Line coverage (LCOV) of the JavaScript files that the tests run, written
# to coverage-js.lcov at the repository root (docs/notes/w5.md,
# "JavaScript coverage of the page shims"):
#   - internal/cdp/page_shims.js, measured in WebKitGTK by JavaScriptCore's
#     control-flow profiler (e2e/webkitgtk TestCheckVisibilityShimMatchesNative,
#     converted by e2e/jscov);
#   - e2e/cdpcompat/clients/playwright.mjs, measured by V8
#     (CDPCOMPAT_V8_COVERAGE, the driver option of e2e/cdpcompat) and
#     reported by c8, pinned in e2e/cdpcompat/package-lock.json.
# Needs what the WebKitGTK suite and make test-cdpcompat need, with the
# Node project installed (make e2e/cdpcompat/node_modules/.package-lock.json)
# and, when agent-browser on PATH is a script, IWDP_CDPCOMPAT_PATH_AGENT_BROWSER
# (e2e/cdpcompat/README.md).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
c8=e2e/cdpcompat/node_modules/.bin/c8
if [ ! -x "$c8" ]; then
	echo "js-coverage: $c8 is missing; install the Node project: make e2e/cdpcompat/node_modules/.package-lock.json" >&2
	exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
IWDP_JSCOV_LCOV="$work/shim.lcov" go test -tags=webkitgtk -count=1 -run '^TestCheckVisibilityShimMatchesNative$' ./e2e/webkitgtk/
(cd e2e/cdpcompat && CDPCOMPAT_V8_COVERAGE="$work/v8" IWDP_CDP_COMPAT=1 go test -tags=cdpcompat -count=1 -timeout=900s \
	-run '^(TestPlaywrightLocatorsWithoutNativeCheckVisibility|TestClientScenarios|TestAwaitedInterceptedFetch)$/^playwright$' .)
"$c8" report --temp-directory "$work/v8" --reporter lcovonly --report-dir "$work/c8" --include e2e/cdpcompat/clients/playwright.mjs
cat "$work/shim.lcov" "$work/c8/lcov.info" >coverage-js.lcov
grep -E '^(SF|LF|LH):' coverage-js.lcov
