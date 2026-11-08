// playwright-core client of the CDP compatibility suite. Usage:
//   node clients/playwright.mjs <scenario> <url> <result-file>
// Scenarios: resolve (design 7.8 test 10): chromium.connectOverCDP with
// the URL, then close. A failed connect is recorded as a failed step; the
// driver judges the requests that the endpoint saw.
// Method recording: Playwright logs every CDP message it sends to its
// "pw:protocol" debug logger ("SEND ► <message>"); this script turns that
// logger on and reads its lines from stderr.
import { finish, newResult, parseArgs, record, step } from './result.mjs';

const { scenario, url, file } = parseArgs('node clients/playwright.mjs');
const out = newResult('playwright-core', scenario, url, file);

const sendMark = 'SEND \u25BA ';

// recordLine records the command of one "SEND ►" line. Playwright writes
// {id, method, params, sessionId}; a long message is logged with its
// middle cut out, so it is not JSON, but it keeps its start (the method)
// and its end (the top-level sessionId, which follows the parameters).
function recordLine(line) {
  const msg = line.slice(line.indexOf(sendMark) + sendMark.length).trim();
  try {
    const m = JSON.parse(msg);
    record(out, m.method, m.sessionId);
    return;
  } catch {
    // cut message
  }
  const method = /"method":"([^"]+)"/.exec(msg);
  const sessions = [...msg.matchAll(/"sessionId":"([^"]+)"\}$/g)];
  if (!method) {
    throw new Error(`a pw:protocol SEND line without a method: ${msg.slice(0, 200)}`);
  }
  record(out, method[1], sessions.length ? sessions[sessions.length - 1][1] : '');
}

const stderrWrite = process.stderr.write.bind(process.stderr);
process.stderr.write = (chunk, ...rest) => {
  const other = [];
  for (const line of String(chunk).split('\n')) {
    if (line.includes(' pw:protocol ') && line.includes(sendMark)) {
      recordLine(line);
    } else if (line.includes(' pw:protocol ')) {
      // received messages: not recorded
    } else {
      other.push(line);
    }
  }
  const text = other.join('\n');
  return text === '' ? true : stderrWrite(text, ...rest);
};
// The debug logger writes plain lines ("<date> pw:protocol SEND ► ...")
// to stderr when colors are off. The driver sets this environment; the
// script sets it again, and drops every other DEBUG_* setting (DEBUG_FILE
// would send the lines to a file), so the record cannot be lost.
for (const k of Object.keys(process.env)) {
  if (/^debug_/i.test(k)) {
    delete process.env[k];
  }
}
process.env.DEBUG = 'pw:protocol';
process.env.DEBUG_COLORS = 'no';
const { chromium } = await import('playwright-core');

const scenarios = {
  async resolve() {
    const browser = await step(out, 'connect', () => chromium.connectOverCDP(url, { timeout: 15_000 }));
    if (browser) {
      await step(out, 'close', () => browser.close());
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
