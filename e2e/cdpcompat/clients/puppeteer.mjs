// puppeteer-core client of the CDP compatibility suite. Usage:
//   node clients/puppeteer.mjs <scenario> <url> <result-file>
// Scenarios: resolve (design 7.8 test 10): connect with the URL
// (browserWSEndpoint for the ws form, browserURL for the http form), then
// disconnect. A failed connect is recorded as a failed step; the driver
// judges the requests that the endpoint saw.
// Method recording: every CDP command goes through Connection._rawSend
// (one per browser and session command), which this script wraps.
import puppeteer, { Connection } from 'puppeteer-core';
import { finish, newResult, parseArgs, record, step } from './result.mjs';

const { scenario, url, file } = parseArgs('node clients/puppeteer.mjs');
const out = newResult('puppeteer-core', scenario, url, file);

const rawSend = Connection.prototype._rawSend;
Connection.prototype._rawSend = function (callbacks, method, params, sessionId, options) {
  record(out, method, sessionId);
  return rawSend.call(this, callbacks, method, params, sessionId, options);
};

const scenarios = {
  async resolve() {
    const target = out.form === 'ws' ? { browserWSEndpoint: url } : { browserURL: url };
    const browser = await step(out, 'connect', () => puppeteer.connect({ ...target, defaultViewport: null, protocolTimeout: 15_000 }));
    if (browser) {
      await step(out, 'disconnect', () => browser.disconnect());
    }
    return true;
  },
};

try {
  const run = scenarios[scenario];
  if (!run) {
    throw new Error(`unknown scenario ${scenario}; want one of ${Object.keys(scenarios).join(', ')}`);
  }
  out.ok = await run();
} catch (e) {
  out.error = String((e && e.stack) || e);
}
finish(out, file);
