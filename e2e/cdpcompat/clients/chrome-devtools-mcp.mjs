// chrome-devtools-mcp client of the CDP compatibility suite. Usage:
//   node clients/chrome-devtools-mcp.mjs scenario <ws-url> <result-file> <origin>
// The script starts the pinned chrome-devtools-mcp as an MCP server with
// --wsEndpoint set to the URL exactly as received (design 5.5) and drives
// it through its MCP stdio protocol (newline-delimited JSON-RPC 2.0), as an
// agent's MCP host does.
// Scenarios (README.md, "Scenarios"):
//   scenario <origin> (design 7.8 test 8): list_pages (the page id that
//     the page tools need: the server routes by page id by default),
//     navigate_page to the form, take_snapshot, click by uid (with the
//     snapshot that click returns), list_network_requests,
//     take_screenshot.
//   features <origin> (the tools that chrome-devtools-mcp 1.14.0 added or
//     changed; the server runs with --categoryNetworkMocks):
//     navigate_page to css.html, take_snapshot and get_css_styles on its
//     button (the winning and the overloaded declarations, the <style>
//     source lines, a var() value), navigate_page to console.html,
//     list_console_messages with includeStackTraces (the message of console.js has its frame),
//     get_console_message, emulate (colorScheme dark, geolocation,
//     cpuThrottlingRate 1; the page sees dark and the position), emulate
//     with networkConditions Slow 3G (the step records the endpoint's
//     refusal: the preset has an upload limit, which WebKit cannot
//     emulate), emulate with networkConditions Offline (a fetch of
//     /resource fails) and without it (offline ends), mock_network on
//     /resource with a body file (a fetch gets the file),
//     remove_network_mock (a fetch reaches the server), and set_logpoints
//     (the logpoint binds, and doubled(21) logs "logpoint 21").
// Method recording: the server's bundled Puppeteer logs every CDP command
// it sends ("puppeteer:protocol:SEND ►", enabled by NODE_DEBUG) to the
// server's --logFile; the script reads the commands from that file when
// the server has ended.
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { allPassed, expect, finish, newResult, observe, parseArgs, pngSize, record, step, withDeadline } from './result.mjs';

const require = createRequire(import.meta.url);
const { scenario, url, file, rest } = parseArgs('node clients/chrome-devtools-mcp.mjs');
const out = newResult('chrome-devtools-mcp', scenario, url, file);

// The server's entry point, from the package's bin map.
const pkgDir = dirname(require.resolve('chrome-devtools-mcp/package.json'));
const serverBin = join(pkgDir, require('chrome-devtools-mcp/package.json').bin['chrome-devtools-mcp']);
// logFile receives the server's debug log, the protocol lines among them.
const logFile = join(dirname(file), 'chrome-devtools-mcp.log');
// sendPrefix starts the log text of one sent CDP command (the bundled
// puppeteer-core's DEBUG_PREFIXES.cdpSend); the JSON message follows it.
const sendPrefix = 'puppeteer:protocol:SEND ► ';
// callLimit bounds one MCP request; the script's own limit is 60 s.
const callLimit = 30_000;
// exitLimit bounds the server's exit after its stdin closed.
const exitLimit = 10_000;
// protocolVersion is the MCP revision that the client asks for.
const protocolVersion = '2025-06-18';

// McpServer is the server process and its JSON-RPC requests.
class McpServer {
  #child;
  #nextId = 1;
  #pending = new Map();
  #exited;

  // args are server flags after --wsEndpoint and --logFile.
  constructor(args = []) {
    // The server's stderr goes to the script's stderr, which the driver
    // keeps in the run's log. NODE_DEBUG only selects debug logs: it loads
    // no code (the driver drops every inherited NODE_* variable).
    this.#child = spawn(process.execPath, [serverBin, '--wsEndpoint', url, '--logFile', logFile, ...args], {
      stdio: ['pipe', 'pipe', 'inherit'],
      env: { ...process.env, NODE_DEBUG: 'puppeteer:protocol:SEND*' },
    });
    this.#exited = new Promise((resolve) => {
      this.#child.on('exit', (code, signal) => resolve({ code, signal }));
    });
    this.#exited.then(({ code, signal }) => {
      for (const { reject } of this.#pending.values()) {
        reject(new Error(`the MCP server ended (exit ${code}, signal ${signal}) before it answered`));
      }
      this.#pending.clear();
    });
    createInterface({ input: this.#child.stdout }).on('line', (line) => this.#onLine(line));
  }

  #onLine(line) {
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      console.error(`the MCP server wrote a line that is not JSON: ${line}`);
      return;
    }
    const p = msg.id !== undefined && this.#pending.get(msg.id);
    if (!p) {
      return; // a notification or a request of the server; the scenario needs none
    }
    this.#pending.delete(msg.id);
    if (msg.error) {
      p.reject(new Error(`${p.method}: JSON-RPC error ${msg.error.code}: ${msg.error.message}`));
    } else {
      p.resolve(msg.result);
    }
  }

  #write(msg) {
    this.#child.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...msg }) + '\n');
  }

  request(method, params) {
    const id = this.#nextId++;
    const answer = new Promise((resolve, reject) => this.#pending.set(id, { method, resolve, reject }));
    this.#write({ id, method, params });
    return withDeadline(answer, callLimit, method);
  }

  notify(method, params) {
    this.#write({ method, params });
  }

  // callTool calls the tool name and returns the text of its answer and
  // its images; an answer with isError throws.
  async callTool(name, args) {
    const result = await this.request('tools/call', { name, arguments: args });
    const text = result.content
      .filter((c) => c.type === 'text')
      .map((c) => c.text)
      .join('\n');
    if (result.isError) {
      throw new Error(`${name} answered with an error: ${text}`);
    }
    return { text, images: result.content.filter((c) => c.type === 'image') };
  }

  // close ends the server the way an MCP host does: it closes stdin. A
  // server that does not end within exitLimit is killed, and close throws.
  async close() {
    this.#child.stdin.end();
    const exit = await Promise.race([this.#exited, new Promise((resolve) => setTimeout(() => resolve(null), exitLimit).unref())]);
    if (exit === null) {
      this.#child.kill('SIGKILL');
      await this.#exited;
      throw new Error(`the MCP server did not end within ${exitLimit} ms after its stdin closed`);
    }
    return exit;
  }
}

// recordSent adds the CDP commands of the server's log file to the result.
function recordSent() {
  for (const line of readFileSync(logFile, 'utf8').split('\n')) {
    const at = line.indexOf(sendPrefix);
    if (at >= 0) {
      const msg = JSON.parse(line.slice(at + sendPrefix.length));
      record(out, msg.method, msg.sessionId);
    }
  }
}

// origin is the fixture site.
function origin() {
  if (!rest[0]) {
    throw new Error(`scenario ${scenario} needs the fixture origin as its first argument`);
  }
  return rest[0];
}

// uidOf returns the uid of the snapshot line with role and name, such as
// `uid=1_4 button "Press me"`.
function uidOf(snapshot, role, name) {
  const line = snapshot.split('\n').find((l) => l.includes(` ${role} ${JSON.stringify(name)}`));
  const m = line && /uid=(\S+)/.exec(line);
  if (!m) {
    throw new Error(`the snapshot has no ${role} ${JSON.stringify(name)} with a uid:\n${snapshot}`);
  }
  return m[1];
}

// pagesOf returns the page lines of a "## Pages" list, such as
// `1: CDP second page (http://127.0.0.1:4711/second.html) [selected]`.
function pagesOf(text) {
  const lines = text.split('\n');
  const start = lines.indexOf('## Pages');
  if (start < 0) {
    throw new Error(`the answer has no "## Pages" list:\n${text}`);
  }
  const pages = [];
  for (const l of lines.slice(start + 1)) {
    const m = /^(\d+): (.*)$/.exec(l);
    if (!m) {
      break;
    }
    pages.push({ id: Number(m[1]), label: m[2] });
  }
  return pages;
}

// toolsUsed are the tools of the scenario; the server must list them.
const toolsUsed = ['list_pages', 'navigate_page', 'take_snapshot', 'click', 'list_network_requests', 'take_screenshot'];

// featureTools are the tools of the features scenario.
const featureTools = ['list_pages', 'navigate_page', 'take_snapshot', 'get_css_styles', 'list_console_messages', 'get_console_message', 'emulate', 'evaluate_script', 'mock_network', 'remove_network_mock', 'set_logpoints'];

// start runs initialize and tools/list on server, checks that it lists
// tools, and returns the id of its one page, the start page; undefined
// when a step failed.
async function start(server, tools) {
  const init = await observe(out, 'initialize', async () => {
    const r = await server.request('initialize', { protocolVersion, capabilities: {}, clientInfo: { name: 'agent-ios-cdpcompat', version: '1' } });
    server.notify('notifications/initialized', {});
    return { protocolVersion: r.protocolVersion, server: r.serverInfo };
  });
  if (!init) {
    return undefined;
  }
  await observe(out, 'tools/list', async () => {
    const { tools: listed } = await server.request('tools/list', {});
    const names = listed.map((t) => t.name);
    const missing = tools.filter((n) => !names.includes(n));
    if (missing.length) {
      throw new Error(`the server does not list ${missing.join(', ')}; it lists ${names.join(', ')}`);
    }
    return tools;
  });
  // The first tool call connects the server's Puppeteer to the URL. An
  // endpoint record's scope is one page (design 5.2): MiniBrowser's
  // start page.
  return await observe(out, 'list_pages', async () => {
    const { text } = await server.callTool('list_pages', {});
    const pages = pagesOf(text);
    expect('pages', pages.length, 1);
    if (!pages[0].label.includes(`(${origin()}/second.html)`)) {
      throw new Error(`the page is ${pages[0].label}, want the start page ${origin()}/second.html`);
    }
    return pages[0].id;
  });
}

// navigate opens target in the page and checks the server's answer.
async function navigate(server, pageId, target) {
  return await observe(out, 'navigate_page', async () => {
    const { text } = await server.callTool('navigate_page', { pageId, type: 'url', url: target });
    const line = `Successfully navigated to ${target}.`;
    if (!text.includes(line)) {
      throw new Error(`navigate_page answered without "${line}":\n${text}`);
    }
    return line;
  });
}

// evaluate runs fn (a function declaration) in the page with
// evaluate_script and returns the JSON value it answered.
async function evaluate(server, pageId, fn) {
  const { text } = await server.callTool('evaluate_script', { pageId, function: fn });
  const m = /```json\n([\s\S]*?)\n```/.exec(text);
  if (!m) {
    throw new Error(`evaluate_script answered without a JSON block:\n${text}`);
  }
  return JSON.parse(m[1]);
}

// cssRules returns the rules of a get_css_styles answer: for each rule
// line such as `  #target (<style>:4) {`, its selector, its source and
// the declaration lines up to its closing brace, trimmed. A rule inside a
// wrapper (`@media ... {`) gets the wrapper's text in `in`.
function cssRules(text) {
  const lines = text.split('\n');
  const rules = [];
  const wrappers = [];
  for (let i = 0; i < lines.length; i++) {
    const wrapper = /^(\s*)(@\S.*) \{$/.exec(lines[i]);
    if (wrapper) {
      wrappers.push({ indent: wrapper[1], text: wrapper[2] });
      continue;
    }
    if (wrappers.length && lines[i] === `${wrappers.at(-1).indent}}`) {
      wrappers.pop();
      continue;
    }
    const rule = /^(\s*)(\S.*) \(([^()]+)\) \{$/.exec(lines[i]);
    if (rule) {
      const decls = [];
      let j = i + 1;
      for (; j < lines.length && lines[j] !== `${rule[1]}}`; j++) {
        decls.push(lines[j].trim());
      }
      rules.push({ selector: rule[2], source: rule[3], in: wrappers.at(-1)?.text, decls });
      i = j;
    }
  }
  return rules;
}

// refusal calls a tool that the endpoint must refuse and returns the
// error of its answer; a tool that answers without an error throws, with
// its text.
async function refusal(server, name, args) {
  let text;
  try {
    ({ text } = await server.callTool(name, args));
  } catch (e) {
    return String(e.message || e);
  }
  throw new Error(`${name} answered without an error:\n${text}`);
}

const scenarios = {
  async scenario() {
    const form = origin() + '/form.html';
    const server = new McpServer();
    try {
      const pageId = await start(server, toolsUsed);
      if (pageId === undefined || !(await navigate(server, pageId, form))) {
        return false;
      }
      const uids = await observe(out, 'take_snapshot', async () => {
        const { text } = await server.callTool('take_snapshot', { pageId });
        if (!/ RootWebArea "CDP form"/.test(text)) {
          throw new Error(`the snapshot has no RootWebArea "CDP form":\n${text}`);
        }
        uidOf(text, 'heading', 'Sign up');
        return { button: uidOf(text, 'button', 'Press me'), textbox: uidOf(text, 'textbox', 'Name') };
      });
      if (!uids) {
        return false;
      }
      // includeSnapshot: the answer has a fresh snapshot, in which the
      // button has its new name.
      await observe(out, 'click', async () => {
        const { text } = await server.callTool('click', { pageId, uid: uids.button, includeSnapshot: true });
        if (!text.includes('Successfully clicked on the element')) {
          throw new Error(`click answered without "Successfully clicked on the element":\n${text}`);
        }
        return { uid: uids.button, button: uidOf(text, 'button', 'Clicked 1') };
      });
      // The requests since the last navigation: the form document itself.
      await observe(out, 'list_network_requests', async () => {
        const { text } = await server.callTool('list_network_requests', { pageId });
        const requests = text.split('\n').filter((l) => l.startsWith('reqid='));
        const line = requests.find((l) => / GET (\S+) \[(\d+)\]$/.exec(l)?.[1] === form);
        if (!line) {
          throw new Error(`list_network_requests has no GET ${form}:\n${text}`);
        }
        expect(`status of ${form}`, / \[(\d+)\]$/.exec(line)[1], '200');
        return requests.map((l) => l.replace(/^reqid=\S+ /, ''));
      });
      await observe(out, 'take_screenshot', async () => {
        const { images } = await server.callTool('take_screenshot', { pageId, format: 'png' });
        expect('images of take_screenshot', images.map((i) => i.mimeType), ['image/png']);
        return pngSize(Buffer.from(images[0].data, 'base64'));
      });
    } finally {
      await step(out, 'close', async () => {
        const exit = await server.close();
        if (exit.code !== 0) {
          throw new Error(`the MCP server exited with ${exit.code} (signal ${exit.signal})`);
        }
      });
      await step(out, 'record sent commands', async () => recordSent());
    }
    return allPassed(out);
  },
};

scenarios.features = async function features() {
  const consolePage = origin() + '/console.html';
  const resource = origin() + '/resource';
  // mock_network reads the body from a file below a root the server
  // allows; without MCP roots that is the temporary directory.
  const bodyDir = mkdtempSync(join(tmpdir(), 'cdpcompat-mock-'));
  const bodyFile = join(bodyDir, 'mocked.txt');
  writeFileSync(bodyFile, 'mocked by chrome-devtools-mcp');
  const server = new McpServer(['--categoryNetworkMocks']);
  try {
    const pageId = await start(server, featureTools);
    if (pageId === undefined) {
      return false;
    }
    // get_css_styles through the endpoint's CSS domain on css.html, whose
    // cascade is known from its CSS: #target's color wins over .box's,
    // .box's text-decoration wins by !important, the @media #target
    // padding wins over .box's. Lines count inside the <style> element
    // (docs/cdp.md): .box is its line 2, printed 1-based as 3.
    if (await navigate(server, pageId, origin() + '/css.html')) {
      await observe(out, 'get_css_styles', async () => {
        const { text: snap } = await server.callTool('take_snapshot', { pageId });
        const uid = uidOf(snap, 'button', 'Styled');
        const { text } = await server.callTool('get_css_styles', { pageId, uid });
        if (!text.includes('Styles for ')) {
          throw new Error(`get_css_styles answered without "Styles for":\n${text}`);
        }
        const rules = cssRules(text);
        const find = (selector, within) => {
          const r = rules.find((x) => x.selector === selector && x.in === within);
          if (!r) {
            throw new Error(`get_css_styles has no rule ${selector}${within ? ' in ' + within : ''}:\n${text}`);
          }
          return r;
        };
        const want = [
          ['.box', undefined, '<style>:3', ['[overloaded] color: blue;', '[overloaded] padding: 1px;', 'text-decoration: underline !important;']],
          ['#target', undefined, '<style>:4', ['color: var(--brand); /* rgb(255, 0, 0) */', '[overloaded] text-decoration: none;']],
          ['#target', '@media (min-width: 1px)', '<style>:5', ['padding: 3px;']],
        ];
        for (const [selector, within, source, decls] of want) {
          const r = find(selector, within);
          expect(`source of ${selector}`, r.source, source);
          expect(`declarations of ${selector}${within ? ' in ' + within : ''}`, r.decls, decls);
        }
        return { uid, rules: rules.filter((r) => r.source.startsWith('<style>')) };
      });
    }
    if (!(await navigate(server, pageId, consolePage))) {
      return false;
    }
    // console.js logged "fixture log 1" from logFromFixture (line 3).
    const msgid = await observe(out, 'list_console_messages', async () => {
      const { text } = await server.callTool('list_console_messages', { pageId, includeStackTraces: true });
      const lines = text.split('\n');
      const at = lines.findIndex((l) => /^msgid=\d+ \[log\] fixture log 1 /.test(l));
      if (at < 0) {
        throw new Error(`list_console_messages has no "fixture log 1":\n${text}`);
      }
      const frame = lines.slice(at + 1).find((l) => l.includes('at logFromFixture'));
      if (!frame || !frame.includes('console.js:3')) {
        throw new Error(`the stack trace of "fixture log 1" has no frame logFromFixture at console.js:3:\n${text}`);
      }
      return { msgid: Number(/^msgid=(\d+)/.exec(lines[at])[1]), frame: frame.trim() };
    });
    if (msgid) {
      await observe(out, 'get_console_message', async () => {
        const { text } = await server.callTool('get_console_message', { pageId, msgid: msgid.msgid });
        for (const want of [`ID: ${msgid.msgid}`, 'Message: log> fixture log 1', 'at logFromFixture']) {
          if (!text.includes(want)) {
            throw new Error(`get_console_message has no "${want}":\n${text}`);
          }
        }
        return text.split('\n').filter((l) => l.startsWith('ID:') || l.startsWith('Message:'));
      });
    }
    // emulate replaces every setting on each call: color scheme,
    // geolocation and a CPU rate of 1 (no throttling) in one call.
    await observe(out, 'emulate', async () => {
      const { text } = await server.callTool('emulate', { pageId, colorScheme: 'dark', geolocation: '52.52,13.405', cpuThrottlingRate: 1 });
      if (!text.includes('Emulation configured successfully')) {
        throw new Error(`emulate answered without "Emulation configured successfully":\n${text}`);
      }
      const seen = await evaluate(server, pageId, `async () => ({
        dark: matchMedia('(prefers-color-scheme: dark)').matches,
        position: await new Promise((resolve) => navigator.geolocation.getCurrentPosition(
          (p) => resolve([p.coords.latitude, p.coords.longitude]),
          (e) => resolve('error ' + e.code + ': ' + e.message),
          { timeout: 5000 })),
      })`);
      expect('prefers-color-scheme: dark', seen.dark, true);
      expect('the position', seen.position, [52.52, 13.405]);
      return seen;
    });
    // Network throttling: every preset of the tool (Puppeteer's
    // PredefinedNetworkConditions) has an upload limit, which WebKit cannot
    // emulate, so the endpoint refuses it before any change; the step
    // records the error and the driver checks its reason (README.md,
    // "Scenarios"). A success text alone would prove nothing: Puppeteer
    // also answers success when the endpoint says "Not supported".
    await observe(out, 'emulate networkConditions', async () => ({
      error: await refusal(server, 'emulate', { pageId, networkConditions: 'Slow 3G' }),
    }));
    // Offline: the endpoint fails every request through interception, so a
    // fetch of /resource rejects and never reaches the fixture server (the
    // driver checks its count); emulate without networkConditions ends it
    // (the fetch of remove_network_mock reaches the server).
    await observe(out, 'emulate Offline', async () => {
      const { text } = await server.callTool('emulate', { pageId, networkConditions: 'Offline' });
      if (!text.includes('Emulation configured successfully')) {
        throw new Error(`emulate answered without "Emulation configured successfully":\n${text}`);
      }
      const fetched = await evaluate(server, pageId, `async () => {
        try {
          return 'fetched: ' + (await (await fetch('/resource')).text());
        } catch (e) {
          return 'failed: ' + e.name;
        }
      }`);
      expect('the fetch of /resource while offline', fetched, 'failed: TypeError');
      const cleared = await server.callTool('emulate', { pageId });
      if (!cleared.text.includes('Emulation configured successfully')) {
        throw new Error(`emulate without settings answered without "Emulation configured successfully":\n${cleared.text}`);
      }
      return fetched;
    });
    await observe(out, 'mock_network', async () => {
      const { text } = await server.callTool('mock_network', { pageId, route: resource, body: bodyFile });
      if (!text.includes('Network mock configured.')) {
        throw new Error(`mock_network answered without "Network mock configured.":\n${text}`);
      }
      const body = await evaluate(server, pageId, `async () => (await fetch('/resource')).text()`);
      expect('the mocked /resource', body, 'mocked by chrome-devtools-mcp');
      return body;
    });
    await observe(out, 'remove_network_mock', async () => {
      const { text } = await server.callTool('remove_network_mock', { pageId, route: resource });
      if (!text.includes('Removed 1 network mock.')) {
        throw new Error(`remove_network_mock answered without "Removed 1 network mock.":\n${text}`);
      }
      const body = await evaluate(server, pageId, `async () => (await fetch('/resource')).text()`);
      expect('/resource without the mock', body, 'from the server');
      return body;
    });
    // set_logpoints goes through the endpoint's Debugger subset: the
    // logpoint binds, and a call of doubled logs its message. Any error
    // fails the step.
    await observe(out, 'set_logpoints', async () => {
      const { text: answer } = await server.callTool('set_logpoints', { pageId, logpoints: [`${origin()}/console.js:6 'logpoint', n`] });
      if (!answer.includes('[bound]')) {
        throw new Error(`set_logpoints answered without a bound logpoint:\n${answer}`);
      }
      expect('doubled(21)', await evaluate(server, pageId, '() => doubled(21)'), 42);
      const { text } = await server.callTool('list_console_messages', { pageId });
      if (!/\] logpoint 21 /.test(text)) {
        throw new Error(`the bound logpoint logged nothing:\n${text}`);
      }
      return { bound: true, logged: 'logpoint 21' };
    });
  } finally {
    await step(out, 'close', async () => {
      const exit = await server.close();
      if (exit.code !== 0) {
        throw new Error(`the MCP server exited with ${exit.code} (signal ${exit.signal})`);
      }
    });
    await step(out, 'record sent commands', async () => recordSent());
    rmSync(bodyDir, { recursive: true, force: true });
  }
  return allPassed(out);
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
