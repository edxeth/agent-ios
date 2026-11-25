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
// Method recording: the server's bundled Puppeteer logs every CDP command
// it sends ("puppeteer:protocol:SEND ►", enabled by NODE_DEBUG) to the
// server's --logFile; the script reads the commands from that file when
// the server has ended.
import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
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

  constructor() {
    // The server's stderr goes to the script's stderr, which the driver
    // keeps in the run's log. NODE_DEBUG only selects debug logs: it loads
    // no code (the driver drops every inherited NODE_* variable).
    this.#child = spawn(process.execPath, [serverBin, '--wsEndpoint', url, '--logFile', logFile], {
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

const scenarios = {
  async scenario() {
    const form = origin() + '/form.html';
    const server = new McpServer();
    try {
      const init = await observe(out, 'initialize', async () => {
        const r = await server.request('initialize', { protocolVersion, capabilities: {}, clientInfo: { name: 'agent-ios-cdpcompat', version: '1' } });
        server.notify('notifications/initialized', {});
        return { protocolVersion: r.protocolVersion, server: r.serverInfo };
      });
      if (!init) {
        return false;
      }
      await observe(out, 'tools/list', async () => {
        const { tools } = await server.request('tools/list', {});
        const names = tools.map((t) => t.name);
        const missing = toolsUsed.filter((n) => !names.includes(n));
        if (missing.length) {
          throw new Error(`the server does not list ${missing.join(', ')}; it lists ${names.join(', ')}`);
        }
        return toolsUsed;
      });
      // The first tool call connects the server's Puppeteer to the URL. An
      // endpoint record's scope is one page (design 5.2): MiniBrowser's
      // start page.
      const pageId = await observe(out, 'list_pages', async () => {
        const { text } = await server.callTool('list_pages', {});
        const pages = pagesOf(text);
        expect('pages', pages.length, 1);
        if (!pages[0].label.includes(`(${origin()}/second.html)`)) {
          throw new Error(`the page is ${pages[0].label}, want the start page ${origin()}/second.html`);
        }
        return pages[0].id;
      });
      if (pageId === undefined) {
        return false;
      }
      const navigated = await observe(out, 'navigate_page', async () => {
        const { text } = await server.callTool('navigate_page', { pageId, type: 'url', url: form });
        const line = `Successfully navigated to ${form}.`;
        if (!text.includes(line)) {
          throw new Error(`navigate_page answered without "${line}":\n${text}`);
        }
        return line;
      });
      if (!navigated) {
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
