// Result file and method recording shared by the client scripts of the CDP
// compatibility suite (README.md, "Result format"). Each script runs one
// scenario with the exact URL it got on its command line and writes one
// JSON result file that the Go driver checks.
import { writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

// scriptLimit bounds one script run; the Go driver waits a little longer.
export const scriptLimit = 60_000;

// parseArgs reads "<scenario> <url> <result-file> [scenario arguments]".
// The scenario arguments are what a scenario needs besides the URL: the
// fixture site's origin, and for the session scenario the URL of the
// competing session.
export function parseArgs(usage) {
  const [scenario, url, file, ...rest] = process.argv.slice(2);
  if (!scenario || !url || !file) {
    console.error(`usage: ${usage} <scenario> <cdp-ws-url or cdp-http-url> <result-file> [scenario arguments]`);
    process.exit(64);
  }
  return { scenario, url, file, rest };
}

// newResult starts the result of one run. url is kept exactly as received,
// so the driver can check that the client got the printed URL unchanged.
export function newResult(client, scenario, url, file) {
  const out = {
    client,
    version: require(`${client}/package.json`).version,
    scenario,
    form: url.startsWith('ws://') || url.startsWith('wss://') ? 'ws' : 'http',
    url,
    ok: false,
    steps: [],
    sent: [],
  };
  const timer = setTimeout(() => {
    out.error = `no result within ${scriptLimit} ms`;
    finish(out, file, 2);
  }, scriptLimit);
  timer.unref();
  return out;
}

// record adds one CDP command that the client sent (its protocol hook).
export function record(out, method, sessionId) {
  out.sent.push({ method, sessionId: sessionId || '' });
}

// step runs one named step and records its outcome; it returns the value
// of fn, or undefined when fn threw.
export async function step(out, name, fn) {
  const start = Date.now();
  const s = { name, ok: false, ms: 0 };
  out.steps.push(s);
  try {
    const v = await fn();
    s.ok = true;
    return v;
  } catch (e) {
    s.error = String((e && e.message) || e);
    return undefined;
  } finally {
    s.ms = Date.now() - start;
  }
}

// observe runs one named step like step and also records the value that
// fn returns (JSON), so the driver can check it.
export async function observe(out, name, fn) {
  return step(out, name, async () => {
    const v = await fn();
    out.steps[out.steps.findLastIndex((s) => s.name === name)].value = v;
    return v;
  });
}

// canonical is the JSON text of v with object keys sorted.
function canonical(v) {
  return JSON.stringify(v, (_, x) =>
    x && typeof x === 'object' && !Array.isArray(x) ? Object.fromEntries(Object.entries(x).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) : x,
  );
}

// expect throws unless got equals want (compared as JSON, key order aside).
export function expect(what, got, want) {
  if (canonical(got) !== canonical(want)) {
    throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  }
  return got;
}

// allPassed reports whether every recorded step passed.
export function allPassed(out) {
  return out.steps.length > 0 && out.steps.every((s) => s.ok);
}

// pngSize returns the width and height of a PNG image (its IHDR chunk), and
// throws for anything that is not a PNG.
export function pngSize(buf) {
  const signature = '89504e470d0a1a0a';
  if (buf.length < 24 || buf.subarray(0, 8).toString('hex') !== signature || buf.subarray(12, 16).toString() !== 'IHDR') {
    throw new Error(`not a PNG image (${buf.length} bytes, starts with ${buf.subarray(0, 16).toString('hex')})`);
  }
  return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20), bytes: buf.length };
}

// viewportPixels runs in the page (page.evaluate): the size of the
// viewport in the pixels of a screenshot, the CSS viewport times the
// page's zoom (visualViewport.scale; on iOS 0.33 on a page without a meta
// viewport) times devicePixelRatio (3 on an iPhone X). On WebKitGTK both
// factors are 1.
export const viewportPixels = () => {
  const scale = (window.visualViewport ? window.visualViewport.scale : 1) * window.devicePixelRatio;
  return { width: innerWidth * scale, height: innerHeight * scale, cssWidth: innerWidth, cssHeight: innerHeight, scale };
};

// expectScreenshotSize throws unless the PNG size (pngSize) is the
// viewport's size in pixels (viewportPixels). WebKit rounds a fractional
// size, so a size less than 1 pixel apart counts; a whole size, as on
// WebKitGTK, must be exact.
export function expectScreenshotSize(size, viewport) {
  if (Math.abs(size.width - viewport.width) >= 1 || Math.abs(size.height - viewport.height) >= 1) {
    throw new Error(`screenshot size: got ${size.width}x${size.height}, want the viewport ${viewport.cssWidth}x${viewport.cssHeight} CSS pixels times ${viewport.scale} = ${viewport.width}x${viewport.height}`);
  }
}

// withDeadline resolves like promise, or rejects after ms with what in the
// message.
export function withDeadline(promise, ms, what) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: no answer within ${ms} ms`)), ms);
  });
  return Promise.race([promise, deadline]).finally(() => clearTimeout(timer));
}

// checkpoint hands control to the driver (the session scenario of design
// 7.8 test 11): it writes "@checkpoint <name>" on stdout, waits for the
// driver's answer line on stdin, and throws unless the answer is "ok".
let stdinLines;
export async function checkpoint(name) {
  if (!stdinLines) {
    const { createInterface } = await import('node:readline');
    stdinLines = createInterface({ input: process.stdin })[Symbol.asyncIterator]();
  }
  process.stdout.write(`@checkpoint ${name}\n`);
  const { value, done } = await stdinLines.next();
  if (done || value !== 'ok') {
    throw new Error(`checkpoint ${name}: the driver answered ${done ? 'nothing (stdin closed)' : JSON.stringify(value)}`);
  }
}

// finish writes the result file and exits.
export function finish(out, file, code) {
  writeFileSync(file, JSON.stringify(out, null, 2) + '\n');
  process.exit(code ?? (out.ok ? 0 : 1));
}
