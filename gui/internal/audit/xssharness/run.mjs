// Serve current real modules, run one security mode, and fail closed when the
// browser, fixture, journey, positive control or injection checks fail.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import url from 'node:url';
import { createHash } from 'node:crypto';

const SEC = path.dirname(url.fileURLToPath(import.meta.url));
const REPO = path.resolve(SEC, '..', '..', '..');
const CHROME = process.env.DEBARK_CHROME || (process.platform === 'win32' ? 'C:/Program Files/Google/Chrome/Application/chrome.exe' : 'google-chrome');
const MODE = process.argv[2] || 'realistic';
if (!['realistic', 'poison', 'control'].includes(MODE)) throw new Error('Mode must be realistic, poison, or control');
const PORT = Number(process.env.DEBARK_XSS_PORT || 7251);
if (!Number.isInteger(PORT) || PORT < 1024 || PORT > 65535) throw new Error('Invalid loopback port');
const PROFILE = fs.mkdtempSync(path.join(os.tmpdir(), 'debark-xss-'));
let server, chrome, browserTimer, exitCode = 1;
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
function done(child) {
  return new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', (code, signal) => resolve({ code, signal })); });
}
async function stop(child) {
  if (!child?.pid || child.exitCode !== null || child.signalCode) return;
  child.kill();
  await Promise.race([done(child), pause(1500)]);
  if (child.exitCode === null && !child.signalCode) child.kill('SIGKILL');
}
function decodeHTML(text) {
  return text.replace(/&#(x[0-9a-f]+|\d+);/gi, (_match, value) => String.fromCodePoint(value[0].toLowerCase() === 'x' ? parseInt(value.slice(1), 16) : Number(value)))
    .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&apos;/g, "'").replace(/&amp;/g, '&');
}
function sourceIdentity() {
  const files = [];
  function walk(directory) {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      const full = path.join(directory, entry.name);
      if (entry.isDirectory()) walk(full); else if (entry.isFile()) files.push(full);
    }
  }
  walk(path.join(REPO, 'frontend', 'src'));
  for (const file of ['harness.html', 'harness.js', 'hostile.js', 'run.mjs', 'genfixture.mjs', 'serve.mjs']) files.push(path.join(SEC, file));
  files.push(path.join(REPO, 'hack', 'ui-review', 'fixtures.mjs'));
  files.push(path.join(REPO, 'frontend', 'wailsjs', 'go', 'models.ts'), path.join(REPO, 'frontend', 'wailsjs', 'go', 'app', 'App.d.ts'));
  const hashes = Object.fromEntries(files.sort().map(file => [path.relative(REPO, file).split(path.sep).join('/'), createHash('sha256').update(fs.readFileSync(file)).digest('hex')]));
  return { sha256: createHash('sha256').update(JSON.stringify(hashes)).digest('hex'), files: hashes };
}
try {
  const generate = spawn(process.execPath, [path.join(SEC, 'genfixture.mjs')], { stdio: 'inherit', windowsHide: true });
  const generated = await done(generate);
  if (generated.code !== 0) throw new Error('Bridge schema generation failed');
  const source = sourceIdentity();
  const started = new Date().toISOString();
  server = spawn(process.execPath, [path.join(SEC, 'serve.mjs')], { stdio: 'inherit', windowsHide: true });
  let serverFailure;
  server.on('error', error => { serverFailure = error; });
  const deadline = Date.now() + 5000;
  let ready = false;
  while (Date.now() < deadline) {
    if (serverFailure || server.exitCode !== null) throw serverFailure || new Error('Fixture server exited');
    try { const response = await fetch(`http://127.0.0.1:${PORT}/sec/bridge.json`); if (response.ok) { ready = true; break; } } catch { /* wait for listening */ }
    await pause(50);
  }
  if (!ready) throw new Error('Fixture server did not become ready');
  await pause(100); // An already occupied port must not look like our server.
  if (server.exitCode !== null) throw new Error('Fixture server exited before browser launch');
  const args = ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-default-apps',
    '--window-size=1040,900', '--user-data-dir=' + PROFILE, '--virtual-time-budget=60000', '--dump-dom'];
  // The documented Linux audit container runs as root; its browser profile is
  // disposable. Host desktop runs retain Chrome's normal sandbox.
  if (process.platform !== 'win32' && process.getuid?.() === 0) args.push('--no-sandbox');
  args.push(`http://127.0.0.1:${PORT}/sec/harness.html?mode=${MODE}`);
  chrome = spawn(CHROME, args, { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  let dom = '', stderr = '';
  chrome.stdout.on('data', data => { dom += data; });
  chrome.stderr.on('data', data => { stderr = (stderr + data).slice(-12000); });
  browserTimer = setTimeout(() => chrome.kill(), 60000);
  browserTimer.unref();
  const result = await done(chrome);
  clearTimeout(browserTimer);
  if (result.code !== 0) throw new Error('Chrome failed (' + result.code + ', ' + result.signal + '): ' + stderr);
  const match = dom.match(/<pre id="results">([\s\S]*?)<\/pre>/);
  if (!match) throw new Error('No harness results block; DOM length ' + dom.length);
  const output = decodeHTML(match[1]);
  const jsonLine = output.split('\n').find(line => line.startsWith('REPORT JSON: '));
  if (!jsonLine || !output.trimEnd().endsWith('DONE')) throw new Error('Harness did not complete or omitted structured results');
  const report = JSON.parse(jsonLine.slice('REPORT JSON: '.length));
  if (report.mode !== MODE || report.done !== true || !Array.isArray(report.problems) || report.totalProblems !== report.problems.length) throw new Error('Invalid security result accounting');
  if (!report.calls?.length || !report.journeys?.length || !report.projectedFields?.length) throw new Error('Harness did not exercise bridge rendering');
  if (sourceIdentity().sha256 !== source.sha256) throw new Error('Source changed during the browser run; rerun a stable snapshot');
  report.execution = { started_at: started, platform: process.platform, arch: process.arch,
    node: process.version, browser: CHROME, source };
  const fullReport = JSON.stringify(report);
  console.log(output.replace(jsonLine, 'REPORT JSON: ' + fullReport));
  if (process.env.DEBARK_XSS_REPORT) fs.writeFileSync(process.env.DEBARK_XSS_REPORT, JSON.stringify(report, null, 2) + '\n');
  exitCode = report.totalProblems === 0 ? 0 : 1;
} catch (error) {
  console.error('SECURITY HARNESS FAILED: ' + (error.stack || error.message));
} finally {
  clearTimeout(browserTimer);
  await stop(chrome); await stop(server);
  // PROFILE was created by mkdtemp directly under OS temp. Verify that exact
  // boundary before recursively deleting it, including on Windows.
  const resolved = path.resolve(PROFILE), temp = path.resolve(os.tmpdir());
  if (path.dirname(resolved) !== temp || !path.basename(resolved).startsWith('debark-xss-')) throw new Error('Unexpected browser profile path: ' + resolved);
  fs.rmSync(resolved, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  process.exitCode = exitCode;
}
