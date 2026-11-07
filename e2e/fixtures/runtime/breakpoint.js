// Breakpoint fixture (e2e/webkitgtk/debugger_test.go): the test sets a
// breakpoint on line index 3 (zero-based), the body of hit, and calls hit.
function hit() {
  return 41 + 1;
}
