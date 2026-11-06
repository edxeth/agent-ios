// Proof P2 (design 7.1): pinned playwright-core connectOverCDP reads
// locator('#target').textContent() through the bridge. Usage:
//   node playwright.mjs <cdp-ws-url or cdp-http-url> <expected text>
// Prints one JSON line; exits 0 only when the text matches.
import { chromium } from 'playwright-core';
import { createRequire } from 'node:module';

const [url, expected] = process.argv.slice(2);
const version = createRequire(import.meta.url)('playwright-core/package.json').version;
const out = { client: 'playwright-core', version, form: url.startsWith('ws') ? 'ws' : 'http', ok: false };
const timer = setTimeout(() => {
  out.error = 'no result within 90 s';
  console.log(JSON.stringify(out));
  process.exit(2);
}, 90_000);
try {
  const browser = await chromium.connectOverCDP(url, { timeout: 30_000 });
  const contexts = browser.contexts();
  out.contexts = contexts.length;
  const pages = contexts[0].pages();
  out.pages = pages.length;
  out.url = pages[0].url();
  out.text = await pages[0].locator('#target').textContent({ timeout: 15_000 });
  out.ok = out.text === expected;
  await browser.close();
} catch (e) {
  out.error = String((e && e.stack) || e);
}
clearTimeout(timer);
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
