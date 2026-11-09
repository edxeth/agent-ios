// puppeteer-core client of the CDP compatibility suite. Usage:
//   node clients/puppeteer.mjs <scenario> <url> <result-file> [arguments]
// Scenarios (README.md, "Scenarios"):
//   resolve (design 7.8 test 10): connect with the URL (browserWSEndpoint
//     for the ws form, browserURL for the http form), then disconnect. A
//     failed connect is recorded as a failed step; the driver judges the
//     requests that the endpoint saw.
//   scenario <origin> (test 8): connect, goto, $eval, click,
//     waitForSelector, type, screenshot, accessibility.snapshot.
//   fetch <origin> (test 12): an awaited fetch that a request interception
//     handler answers.
//   session <origin> <url of s2> (test 11 (a) and (d)): the URL is s1's;
//     the script stops at two checkpoints for the driver's CLI checks.
// Method recording: every CDP command goes through Connection._rawSend
// (one per browser and session command), which this script wraps.
import puppeteer, { Connection } from 'puppeteer-core';
import { allPassed, checkpoint, expect, finish, newResult, observe, parseArgs, pngSize, record, step, withDeadline } from './result.mjs';

const { scenario, url, file, rest } = parseArgs('node clients/puppeteer.mjs');
const out = newResult('puppeteer-core', scenario, url, file);

// connections are the Connection objects that sent a command, in order.
const connections = [];
const rawSend = Connection.prototype._rawSend;
Connection.prototype._rawSend = function (callbacks, method, params, sessionId, options) {
  record(out, method, sessionId);
  if (!connections.includes(this)) {
    connections.push(this);
  }
  return rawSend.call(this, callbacks, method, params, sessionId, options);
};

// connect connects with either URL form; defaultViewport null keeps
// Puppeteer from sending Emulation.setDeviceMetricsOverride (design 5.5).
function connect(u) {
  const target = u.startsWith('ws://') ? { browserWSEndpoint: u } : { browserURL: u };
  return puppeteer.connect({ ...target, defaultViewport: null, protocolTimeout: 15_000 });
}

// onlyPage returns the one page of the browser: an endpoint record's scope
// is one page (design 5.2).
async function onlyPage(browser) {
  const pages = await browser.pages();
  expect('pages', pages.length, 1);
  return pages[0];
}

// origin is the fixture site of the scenarios that take it.
function origin() {
  if (!rest[0]) {
    throw new Error(`scenario ${scenario} needs the fixture origin as its first argument`);
  }
  return rest[0];
}

// gotoForm navigates to the form fixture and checks the response and the
// title.
async function gotoForm(page) {
  return observe(out, 'goto', async () => {
    const response = await page.goto(origin() + '/form.html', { waitUntil: 'load', timeout: 15_000 });
    expect('goto response status', response && response.status(), 200);
    expect('url after goto', page.url(), origin() + '/form.html');
    return { status: response.status(), title: expect('title', await page.title(), 'CDP form') };
  });
}

// axFind returns the nodes of an accessibility snapshot with role.
function axFind(node, role, found = []) {
  if (node) {
    if (node.role === role) {
      found.push({ role: node.role, name: node.name, value: node.value });
    }
    for (const c of node.children || []) {
      axFind(c, role, found);
    }
  }
  return found;
}

const scenarios = {
  async resolve() {
    const browser = await step(out, 'connect', () => connect(url));
    if (browser) {
      await step(out, 'disconnect', () => browser.disconnect());
    }
    return true;
  },

  async scenario() {
    const browser = await step(out, 'connect', () => connect(url));
    if (!browser) {
      return false;
    }
    const page = await step(out, 'page', () => onlyPage(browser));
    if (!page) {
      return false;
    }
    await gotoForm(page);
    await observe(out, '$eval', async () => expect('h1', await page.$eval('h1', (e) => e.textContent), 'Sign up'));
    await observe(out, 'click', async () => {
      await page.click('#change');
      return expect('button text after the click', await page.$eval('#change', (e) => e.textContent), 'Clicked 1');
    });
    // The click adds #late 300 ms later: the wait polls in Puppeteer's
    // utility world, and the handle moves to the main world for the read
    // (DOM.describeNode, then DOM.resolveNode; P2 item 5).
    await observe(out, 'waitForSelector', async () => {
      const late = await page.waitForSelector('#late', { timeout: 10_000 });
      return expect('#late', await late.evaluate((e) => e.textContent), 'late element');
    });
    await observe(out, 'type', async () => {
      await page.type('#name', 'Ada');
      return {
        value: expect('#name value', await page.$eval('#name', (e) => e.value), 'Ada'),
        status: expect('#status after typing', await page.$eval('#status', (e) => e.textContent), 'name: Ada'),
      };
    });
    await observe(out, 'screenshot', async () => {
      const size = pngSize(Buffer.from(await page.screenshot({ type: 'png' })));
      const viewport = await page.evaluate(() => ({ width: innerWidth, height: innerHeight }));
      expect('screenshot size', { width: size.width, height: size.height }, viewport);
      return size;
    });
    await observe(out, 'accessibility.snapshot', async () => {
      const snap = await page.accessibility.snapshot();
      expect('snapshot root', snap && [snap.role, snap.name], ['RootWebArea', 'CDP form']);
      const buttons = axFind(snap, 'button').map((n) => n.name);
      const boxes = axFind(snap, 'textbox').map((n) => [n.name, n.value]);
      if (!buttons.includes('Clicked 1')) {
        throw new Error(`no button "Clicked 1" in the snapshot: buttons ${JSON.stringify(buttons)}`);
      }
      if (!boxes.some(([name, value]) => name === 'Name' && value === 'Ada')) {
        throw new Error(`no textbox "Name" with the value Ada in the snapshot: ${JSON.stringify(boxes)}`);
      }
      return { buttons, textboxes: boxes };
    });
    await step(out, 'disconnect', () => browser.disconnect());
    return allPassed(out);
  },

  // Design 7.8 test 12: the handler answers /resource; every other request
  // continues. The fetch must return "ok" within 2 s of a 10 s deadline.
  async fetch() {
    const browser = await step(out, 'connect', () => connect(url));
    if (!browser) {
      return false;
    }
    const page = await step(out, 'page', () => onlyPage(browser));
    if (!page) {
      return false;
    }
    await gotoForm(page);
    const answered = [];
    await step(out, 'setRequestInterception', async () => {
      page.on('request', (r) => {
        if (new URL(r.url()).pathname === '/resource') {
          answered.push(r.url());
          r.respond({ status: 200, contentType: 'text/plain', body: 'ok' });
        } else {
          r.continue();
        }
      });
      await page.setRequestInterception(true);
    });
    await observe(out, 'fetch', async () => {
      const start = Date.now();
      const body = await withDeadline(page.evaluate(() => fetch('/resource').then((r) => r.text())), 10_000, 'fetch /resource');
      const ms = Date.now() - start;
      expect('fetch body', body, 'ok');
      expect('requests the handler answered', answered, [origin() + '/resource']);
      if (ms > 2_000) {
        throw new Error(`the intercepted fetch took ${ms} ms, want at most 2000`);
      }
      return { body, ms };
    });
    await step(out, 'disconnect', () => browser.disconnect());
    return allPassed(out);
  },

  // Design 7.8 test 11 (a) and (d) through Puppeteer. The URL is s1's,
  // rest[1] the URL that get cdp-url issued for s2 on the same page.
  async session() {
    const s2 = rest[1];
    if (!s2) {
      throw new Error('scenario session needs the URL of session s2 as its second argument');
    }
    const browser = await step(out, 'connect s1', () => connect(url));
    if (!browser) {
      return false;
    }
    const page = await step(out, 'page', () => onlyPage(browser));
    if (!page) {
      return false;
    }
    const s1Conn = connections[0];
    const evaluate = () => page.evaluate(() => document.title + ':' + 6 * 7);
    await observe(out, 'evaluate', async () => expect('evaluate in the page', await evaluate(), 'CDP form:42'));
    // The driver checks s1's lease epoch, clicks with the snapshot's ref
    // through the CLI, and runs s2's "get title".
    await step(out, 'checkpoint attached', () => checkpoint('attached'));
    await observe(out, 'CLI click seen', async () => expect('button text after the CLI click', await page.$eval('#change', (e) => e.textContent), 'Clicked 1'));
    const other = await step(out, 'connect s2', () => connect(s2));
    if (!other) {
      return false;
    }
    const s2Conn = connections.find((c) => c !== s1Conn);
    await observe(out, 's2 lists the page', async () => {
      const { targetInfos } = await s2Conn.send('Target.getTargets');
      const pages = targetInfos.filter((ti) => ti.type === 'page');
      expect('page targets of s2', pages.map((ti) => ti.url), [origin() + '/form.html']);
      return pages.map((ti) => ti.targetId);
    });
    await observe(out, 's2 attach refused', async () => {
      const { targetInfos } = await s2Conn.send('Target.getTargets');
      const target = targetInfos.find((ti) => ti.type === 'page');
      try {
        await s2Conn.send('Target.attachToTarget', { targetId: target.targetId, flatten: true });
      } catch (e) {
        const msg = String(e && e.message);
        if (!msg.includes('leased by session "s1"')) {
          throw new Error(`Target.attachToTarget failed with ${msg}, want the lease message`);
        }
        return msg;
      }
      throw new Error('Target.attachToTarget through s2 succeeded on a page that s1 leases');
    });
    await observe(out, 's1 unchanged', async () => {
      expect('s1 connected', browser.connected, true);
      return expect('evaluate in the page after s2', await evaluate(), 'CDP form:42');
    });
    // The driver checks that s1's lease epoch did not change.
    await step(out, 'checkpoint refused', () => checkpoint('refused'));
    await step(out, 'disconnect', async () => {
      await other.disconnect();
      await browser.disconnect();
    });
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
