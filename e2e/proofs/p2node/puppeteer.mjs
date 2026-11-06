// Proof P2 (design 7.1): pinned puppeteer-core connect completes
// page.waitForSelector('#target') through the bridge, reads the element in
// the main world (handle transfer through DOM.describeNode and
// DOM.resolveNode), and waits for an element added 500 ms later. Usage:
//   node puppeteer.mjs <cdp-ws-url or cdp-http-url> <expected text>
// Prints one JSON line; exits 0 only when every step passed.
import puppeteer from 'puppeteer-core';
import { createRequire } from 'node:module';

const [url, expected] = process.argv.slice(2);
const version = createRequire(import.meta.url)('puppeteer-core/package.json').version;
const out = { client: 'puppeteer-core', version, form: url.startsWith('ws') ? 'ws' : 'http', ok: false };
const timer = setTimeout(() => {
  out.error = 'no result within 90 s';
  console.log(JSON.stringify(out));
  process.exit(2);
}, 90_000);
try {
  const target = url.startsWith('ws') ? { browserWSEndpoint: url } : { browserURL: url };
  const browser = await puppeteer.connect({ ...target, defaultViewport: null, protocolTimeout: 30_000 });
  const pages = await browser.pages();
  out.pages = pages.length;
  const page = pages[0];
  out.url = page.url();
  const handle = await page.waitForSelector('#target', { timeout: 15_000 });
  out.text = await handle.evaluate((e) => e.textContent);
  await page.evaluate(() => {
    document.getElementById('late')?.remove();
    setTimeout(() => {
      const d = document.createElement('div');
      d.id = 'late';
      d.textContent = 'late element';
      document.body.appendChild(d);
    }, 500);
  });
  const late = await page.waitForSelector('#late', { timeout: 15_000 });
  out.late = await late.evaluate((e) => e.textContent);
  await page.evaluate(() => document.getElementById('late')?.remove());
  out.ok = out.text === expected && out.late === 'late element';
  await browser.disconnect();
} catch (e) {
  out.error = String((e && e.stack) || e);
}
clearTimeout(timer);
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
