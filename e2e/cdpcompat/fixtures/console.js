// chrome-devtools-mcp features scenario: the line numbers are part of it.
function logFromFixture(n) {
  console.log('fixture log', n);
}
function doubled(n) {
  return n * 2;
}
logFromFixture(1);
