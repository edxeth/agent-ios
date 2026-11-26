window.lpTotal2 = 0;
function total2(items) {
  var sum = 0;
  for (var i = 0; i < items.length; i++) {
    sum += items[i].price;
  }
  return sum;
}
setInterval(function () { window.lpTotal2 = total2([{price: 2}, {price: 3}]); }, 50);
//# sourceMappingURL=maps/logpoint-mapped2.js.map
