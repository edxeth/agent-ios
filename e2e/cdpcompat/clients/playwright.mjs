// playwright-core client of the CDP compatibility suite. Usage:
//   node clients/playwright.mjs <scenario> <url> <result-file> [arguments]
// Scenarios (README.md, "Scenarios"):
//   resolve (design 7.8 test 10): chromium.connectOverCDP with the URL,
//     then close. A failed connect is recorded as a failed step; the
//     driver judges the requests that the endpoint saw.
//   scenario <origin> (test 8): connectOverCDP, goto, evaluate,
//     getByRole().click(), fill, a locator read through the utility world,
//     screenshot, ariaSnapshot.
//   fetch <origin> (test 12): an awaited fetch that page.route fulfills.
//   noCheckVisibility <origin> (docs/cdp.md "Page compatibility shims"):
//     on form-no-checkvisibility.html, which removed its native
//     Element.prototype.checkVisibility before the connect, and without a
//     navigation: the endpoint's shim is there, then getByRole().click(),
//     fill and ariaSnapshot.
// Method recording: Playwright logs every CDP message it sends to its
// "pw:protocol" debug logger ("SEND ► <message>"); this script turns that
// logger on and reads its lines from stderr.
import { allPassed, expect, expectScreenshotSize, finish, newResult, observe, parseArgs, pngSize, record, step, viewportPixels, withDeadline } from './result.mjs';

const { scenario, url, file, rest } = parseArgs('node clients/playwright.mjs');
const out = newResult('playwright-core', scenario, url, file);

const sendMark = 'SEND \u25BA ';
const recvMark = '\u25C0 RECV ';

// utilityWorlds are the execution contexts that the endpoint announced to
// Playwright under the name of Playwright's utility world
// (__playwright_utility_world_<page guid>), with their auxData.
const utilityWorlds = [];

// recvLine notes the utility-world contexts of one "◀ RECV" line; other
// received messages are not recorded.
function recvLine(line) {
  const msg = line.slice(line.indexOf(recvMark) + recvMark.length).trim();
  if (!msg.includes('"Runtime.executionContextCreated"')) {
    return;
  }
  const { params } = JSON.parse(msg);
  if (params.context.name.startsWith('__playwright_utility_world_')) {
    utilityWorlds.push({ id: params.context.id, auxData: params.context.auxData });
  }
}

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
    } else if (line.includes(' pw:protocol ') && line.includes(recvMark)) {
      recvLine(line);
    } else if (line.includes(' pw:protocol ')) {
      // other protocol lines
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

// origin is the fixture site of the scenarios that take it.
function origin() {
  if (!rest[0]) {
    throw new Error(`scenario ${scenario} needs the fixture origin as its first argument`);
  }
  return rest[0];
}

// connectPage connects and returns the browser and its one page: the
// default context's (design 5.5), and an endpoint record's scope is one
// page (5.2).
async function connectPage() {
  const browser = await step(out, 'connect', () => chromium.connectOverCDP(url, { timeout: 15_000 }));
  if (!browser) {
    return {};
  }
  const page = await step(out, 'page', () => {
    const contexts = browser.contexts();
    expect('contexts', contexts.length, 1);
    const pages = contexts[0].pages();
    expect('pages', pages.length, 1);
    return pages[0];
  });
  return { browser, page };
}

// gotoForm navigates to the form fixture and checks the response and the
// title.
function gotoForm(page) {
  return observe(out, 'goto', async () => {
    const response = await page.goto(origin() + '/form.html', { waitUntil: 'load', timeout: 15_000 });
    expect('goto response status', response && response.status(), 200);
    expect('url after goto', page.url(), origin() + '/form.html');
    return { status: response.status(), title: expect('title', await page.title(), 'CDP form') };
  });
}

const scenarios = {
  async resolve() {
    const browser = await step(out, 'connect', () => chromium.connectOverCDP(url, { timeout: 15_000 }));
    if (browser) {
      await step(out, 'close', () => browser.close());
    }
    return true;
  },

  async scenario() {
    const { browser, page } = await connectPage();
    if (!page) {
      return false;
    }
    page.setDefaultTimeout(10_000);
    await gotoForm(page);
    await observe(out, 'evaluate', async () => expect('evaluate', await page.evaluate(() => document.title + ':' + 6 * 7), 'CDP form:42'));
    await observe(out, 'getByRole().click()', async () => {
      await page.getByRole('button', { name: 'Press me' }).click();
      return expect('button text after the click', await page.evaluate(() => document.getElementById('change').textContent), 'Clicked 1');
    });
    await observe(out, 'fill', async () => {
      await page.getByLabel('Name').fill('Grace');
      return {
        value: expect('#name value', await page.evaluate(() => document.getElementById('name').value), 'Grace'),
        status: expect('#status after fill', await page.evaluate(() => document.getElementById('status').textContent), 'name: Grace'),
      };
    });
    // locator.textContent() runs in Playwright's utility world (frames
    // _waitForFunctionOnSelector without mainWorld), which Playwright finds
    // only through the named executionContextCreated (design 5.2). The
    // click added #late 300 ms later, so the read also waits.
    await observe(out, 'utility-world locator read', async () => {
      const text = expect('#late', await page.locator('#late').textContent(), 'late element');
      const worlds = utilityWorlds.filter((w) => w.auxData && w.auxData.isDefault === false && w.auxData.type === 'isolated');
      if (worlds.length === 0) {
        throw new Error(`the endpoint announced no isolated context named __playwright_utility_world_*: ${JSON.stringify(utilityWorlds)}`);
      }
      return { text, utilityContexts: worlds.length };
    });
    await observe(out, 'screenshot', async () => {
      const size = pngSize(await page.screenshot({ type: 'png' }));
      expectScreenshotSize(size, await page.evaluate(viewportPixels));
      return size;
    });
    await observe(out, 'ariaSnapshot', async () => {
      const snap = await page.locator('body').ariaSnapshot();
      for (const want of ['- heading "Sign up" [level=1]', '- button "Clicked 1"', '- textbox "Name": Grace', '- paragraph: late element']) {
        if (!snap.split('\n').some((l) => l.trim() === want)) {
          throw new Error(`ariaSnapshot has no line ${JSON.stringify(want)}:\n${snap}`);
        }
      }
      return snap;
    });
    await step(out, 'close', () => browser.close());
    return allPassed(out);
  },

  async noCheckVisibility() {
    const { browser, page } = await connectPage();
    if (!page) {
      return false;
    }
    page.setDefaultTimeout(10_000);
    await observe(out, 'checkVisibility shim', async () => {
      expect('url', page.url(), origin() + '/form-no-checkvisibility.html');
      const kind = await page.evaluate(() => {
        const f = Element.prototype.checkVisibility;
        return typeof f !== 'function' ? typeof f : f.__iwdpShim === true ? 'shim' : 'native';
      });
      return expect('Element.prototype.checkVisibility', kind, 'shim');
    });
    await observe(out, 'getByRole().click()', async () => {
      await page.getByRole('button', { name: 'Press me' }).click();
      return expect('button text after the click', await page.evaluate(() => document.getElementById('change').textContent), 'Clicked 1');
    });
    await observe(out, 'fill', async () => {
      await page.getByLabel('Name').fill('Grace');
      return expect('#status after fill', await page.evaluate(() => document.getElementById('status').textContent), 'name: Grace');
    });
    await observe(out, 'ariaSnapshot', async () => {
      await page.locator('#late').waitFor();
      const snap = await page.locator('body').ariaSnapshot();
      for (const want of ['- heading "Sign up" [level=1]', '- button "Clicked 1"', '- textbox "Name": Grace', '- paragraph: late element']) {
        if (!snap.split('\n').some((l) => l.trim() === want)) {
          throw new Error(`ariaSnapshot has no line ${JSON.stringify(want)}:\n${snap}`);
        }
      }
      return snap;
    });
    await step(out, 'close', () => browser.close());
    return allPassed(out);
  },

  // Design 7.8 test 12: the route fulfills /resource; the fetch must
  // return "ok" within 2 s of a 10 s deadline.
  async fetch() {
    const { browser, page } = await connectPage();
    if (!page) {
      return false;
    }
    await gotoForm(page);
    const answered = [];
    await step(out, 'route', () =>
      page.route('**/resource', (r) => {
        answered.push(r.request().url());
        return r.fulfill({ body: 'ok' });
      }),
    );
    await observe(out, 'fetch', async () => {
      const start = Date.now();
      const body = await withDeadline(page.evaluate(() => fetch('/resource').then((r) => r.text())), 10_000, 'fetch /resource');
      const ms = Date.now() - start;
      expect('fetch body', body, 'ok');
      expect('requests the route fulfilled', answered, [origin() + '/resource']);
      if (ms > 2_000) {
        throw new Error(`the intercepted fetch took ${ms} ms, want at most 2000`);
      }
      return { body, ms };
    });
    await step(out, 'close', () => browser.close());
    return allPassed(out);
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
