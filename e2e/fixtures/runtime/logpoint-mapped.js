window.lpTotal = 0;
function total(items) {
  var sum = 0;
  for (var i = 0; i < items.length; i++) {
    sum += items[i].price;
  }
  return sum;
}
setInterval(function () { window.lpTotal = total([{price: 2}, {price: 3}]); }, 50);
//# sourceMappingURL=data:application/json;charset=utf-8;base64,eyJ2ZXJzaW9uIjozLCJzb3VyY2VzIjpbInNyYy9jYXJ0LnRzIl0sInNvdXJjZXNDb250ZW50IjpbIi8vIGNhcnQudHM6IHRoZSBvcmlnaW5hbCBzb3VyY2Ugb2YgbG9ncG9pbnQtbWFwcGVkLmpzXG5pbnRlcmZhY2UgSXRlbSB7IHByaWNlOiBudW1iZXIgfVxuXG5leHBvcnQgZnVuY3Rpb24gdG90YWwoaXRlbXM6IEl0ZW1bXSk6IG51bWJlciB7XG4gIGxldCBzdW0gPSAwO1xuICBmb3IgKGNvbnN0IGl0IG9mIGl0ZW1zKSB7XG4gICAgc3VtICs9IGl0LnByaWNlO1xuICB9XG4gIHJldHVybiBzdW07XG59XG4iXSwibmFtZXMiOltdLCJtYXBwaW5ncyI6IjtBQUdBO0VBQ0U7RUFDQTtJQUNFOztFQUVGIn0=
