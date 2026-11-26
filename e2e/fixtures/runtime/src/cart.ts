// cart.ts: the original source of logpoint-mapped.js
interface Item { price: number }

export function total(items: Item[]): number {
  let sum = 0;
  for (const it of items) {
    sum += it.price;
  }
  return sum;
}
