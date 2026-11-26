// logpoint fixture: tick runs every 50 ms (e2e/webkitgtk/logpoint045_test.go)
window.lpCount = 0;
function tick(step) {
  var doubled = step * 2;

  window.lpCount = window.lpCount + 1;
  return doubled;
}
setInterval(function () { tick(window.lpCount); }, 50);
