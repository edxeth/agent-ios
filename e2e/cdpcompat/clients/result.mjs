// Result file and method recording shared by the client scripts of the CDP
// compatibility suite (README.md, "Result format"). Each script runs one
// scenario with the exact URL it got on its command line and writes one
// JSON result file that the Go driver checks.
import { writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

// scriptLimit bounds one script run; the Go driver waits a little longer.
export const scriptLimit = 60_000;

// parseArgs reads "<scenario> <url> <result-file>".
export function parseArgs(usage) {
  const [scenario, url, file] = process.argv.slice(2);
  if (!scenario || !url || !file) {
    console.error(`usage: ${usage} <scenario> <cdp-ws-url or cdp-http-url> <result-file>`);
    process.exit(64);
  }
  return { scenario, url, file };
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

// finish writes the result file and exits.
export function finish(out, file, code) {
  writeFileSync(file, JSON.stringify(out, null, 2) + '\n');
  process.exit(code ?? (out.ok ? 0 : 1));
}
