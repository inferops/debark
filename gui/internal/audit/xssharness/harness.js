/* Real modules, hostile bridge text, visible sequential operator journeys.
 * Fixture evidence only: no native chooser, keyboard or Wails claim.
 * Four injection signals plus required visible verbatim text/control coverage.
 */
import { makeBindings, PAYLOADS } from './hostile.js';

const MODE = new URLSearchParams(location.search).get('mode') || 'realistic';
const report = { evidence: 'fixture-chrome', mode: MODE, engine: navigator.userAgent,
  viewport: { width: innerWidth, height: innerHeight, devicePixelRatio },
  done: false, problems: [], journeys: [], calls: [], seen: [], controlAnchor: false, projectedFields: [] };
window.__XSS_REPORT__ = report;
window.__xss_hits = [];
window.__xss = tag => window.__xss_hits.push(tag);
window.__calls = [];
window.__net = [];
const problemKeys = new Set();
function fail(message) {
  if (!problemKeys.has(message)) { problemKeys.add(message); report.problems.push(message); }
}
window.addEventListener('error', event => fail('JS ERROR: ' + (event.message || 'resource/handler failure')));
window.addEventListener('unhandledrejection', event => fail('UNHANDLED REJECTION: ' + String(event.reason?.stack || event.reason)));
const origFetch = window.fetch;
window.fetch = function (...args) { window.__net.push('fetch:' + String(args[0])); return origFetch.apply(this, args); };
const origOpen = XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open = function (method, address, ...rest) { window.__net.push('xhr:' + String(address)); return origOpen.call(this, method, address, ...rest); };
window.WebSocket = function (address) { window.__net.push('ws:' + String(address)); throw new Error('Blocked hostile WebSocket'); };
window.EventSource = function (address) { window.__net.push('sse:' + String(address)); throw new Error('Blocked hostile EventSource'); };

const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const nameOf = node => (node.getAttribute('aria-label') || node.textContent || '').replace(/\s+/g, ' ').trim();
function visible(node) {
  if (!node || node.closest('[hidden],dialog:not([open])')) return false;
  for (let parent = node.parentElement; parent; parent = parent.parentElement) {
    if (parent.tagName === 'DETAILS' && !parent.open && !parent.querySelector(':scope > summary')?.contains(node)) return false;
  }
  const style = getComputedStyle(node), box = node.getBoundingClientRect();
  return style.display !== 'none' && style.visibility !== 'hidden' && box.width > 0 && box.height > 0;
}
function activePane() {
  const panes = [...document.querySelectorAll('.df-app__pane')].filter(visible);
  if (panes.length !== 1) throw new Error('Expected one visible screen, found ' + panes.length);
  return panes[0];
}
function modal() {
  const dialogs = [...document.querySelectorAll('dialog[open]')].filter(visible);
  if (dialogs.length !== 1) throw new Error('Expected one visible dialog, found ' + dialogs.length);
  return dialogs[0];
}
async function until(read, label, timeout = 4500) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) { const value = read(); if (value) return value; await pause(25); }
  throw new Error('Timed out: ' + label);
}
function enabled(node) { return !node.disabled && node.getAttribute('aria-disabled') !== 'true'; }
async function activate(node, label = nameOf(node)) {
  if (!visible(node) || !enabled(node)) throw new Error('Control unavailable: ' + label);
  node.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  await pause(15);
  const rect = node.getBoundingClientRect();
  if (rect.bottom <= 0 || rect.top >= innerHeight || rect.right <= 0 || rect.left >= innerWidth) throw new Error('Control outside viewport: ' + label);
  node.focus(); node.click();
  report.journeys.push({ action: 'click', label });
  await pause(90);
  scan('after ' + label);
}
async function click(pattern, scope = activePane()) {
  const nodes = [...scope.querySelectorAll('button,summary,[role="menuitem"]')].filter(node => visible(node) && pattern.test(nameOf(node)));
  if (nodes.length !== 1) throw new Error('Expected one control ' + pattern + ', found ' + nodes.length);
  return activate(nodes[0]);
}
async function input(selector, value, scope = activePane()) {
  const nodes = [...scope.querySelectorAll(selector)].filter(visible);
  if (nodes.length !== 1 || !enabled(nodes[0])) throw new Error('Input unavailable/ambiguous: ' + selector);
  nodes[0].scrollIntoView({ block: 'nearest' }); nodes[0].focus(); nodes[0].value = value;
  nodes[0].dispatchEvent(new Event('input', { bubbles: true }));
  report.journeys.push({ action: 'input', selector });
  await pause(260);
  return nodes[0];
}
async function inputLabel(pattern, value, scope = activePane()) {
  const labels = [...scope.querySelectorAll('label[for]')].filter(node => visible(node) && pattern.test(nameOf(node)));
  if (labels.length !== 1) throw new Error('Label unavailable/ambiguous: ' + pattern);
  return input('#' + CSS.escape(labels[0].htmlFor), value, scope);
}
async function openDisclosures(scope) {
  // Reach each disclosure by its visible summary. Never force .open or mount
  // hidden dialogs; nested disclosures become available after their parent.
  for (let count = 0; count < 40; count++) {
    const next = [...scope.querySelectorAll('details > summary')].find(node => visible(node) && !node.parentElement.open);
    if (!next) return;
    await activate(next, 'Disclosure: ' + nameOf(next));
  }
  throw new Error('Disclosure walk exceeded its bound');
}
async function mainMenu(pattern) {
  await click(/^Main menu$/, document.querySelector('.df-desktop-toolbar'));
  await click(pattern, document.getElementById('main-menu'));
}
async function selectionDetails() {
  const selection = activePane().querySelector('.df-picker-selection');
  if (!visible(selection)) await click(/^Selected \(/);
  await openDisclosures(visible(selection) ? selection : modal());
  if (document.querySelector('dialog[open]')) await click(/^Close selected packages$/, modal());
}
async function reached(method, count = 1) {
  await until(() => report.calls.filter(call => call.name === method).length >= count, 'binding ' + method);
  report.journeys.push({ assertion: 'Reached ' + method, passed: true });
}

const ALLOWED = new Set(('DIV SPAN P BUTTON A MAIN HEADER NAV UL OL LI PRE CODE INPUT LABEL SELECT OPTION TEXTAREA H1 H2 H3 H4 H5 H6 STRONG EM SMALL TABLE THEAD TBODY TR TD TH CAPTION COLGROUP COL DL DT DD DETAILS SUMMARY DIALOG FORM FIELDSET LEGEND HR BR FOOTER SECTION ARTICLE ASIDE PROGRESS METER OUTPUT TIME KBD SAMP VAR ABBR STYLE').split(' '));
const SVG_OK = new Set('svg path circle rect line polyline polygon g ellipse title desc use defs'.split(' '));
const bootstrap = document.querySelector('script[src="/sec/harness.js"]');
const seen = new Set();
function scan(where) {
  // Body scanning includes Add/menu/file-result portals outside #app. The
  // immutable harness script and results node are the only exclusions.
  for (const node of document.body.querySelectorAll('*')) {
    if (node === bootstrap || node.closest('#results')) continue;
    const svg = node.namespaceURI === 'http://www.w3.org/2000/svg';
    const tag = svg ? node.tagName.toLowerCase() : node.tagName;
    if (svg ? !SVG_OK.has(tag) : !ALLOWED.has(tag)) fail('FOREIGN ELEMENT: ' + tag + ' at ' + where);
    for (const attr of node.attributes) {
      if (/^on/i.test(attr.name)) fail('ON-ATTR: ' + tag + '.' + attr.name);
      if (/^(href|src|xlink:href)$/.test(attr.name) && attr.value && !/^(https?:|#|\/|\.\/|mailto:)/i.test(attr.value.trim())) fail('DANGEROUS URL: ' + attr.value);
      if (attr.name === 'style' && /url\s*\(/i.test(attr.value)) fail('STYLE URL: ' + attr.value);
    }
    if (node.tagName === 'A' && node.getAttribute('href')) {
      const raw = node.getAttribute('href');
      if (!raw.startsWith('#') && !['http:', 'https:', 'mailto:'].includes(node.protocol)) fail('ANCHOR PROTOCOL: ' + node.protocol);
      if (visible(node) && node.href === 'https://vendor.example/pool/CONTROL_URL.deb') report.controlAnchor = true;
    }
  }
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  let textNode;
  while ((textNode = walker.nextNode())) {
    if (textNode.parentElement?.closest('#results,script,style') || !visible(textNode.parentElement)) continue;
    for (const [key, payload] of Object.entries(PAYLOADS)) if (textNode.textContent.includes(payload)) seen.add(key);
    if (textNode.textContent.includes('CONTROL_URL')) seen.add('controlURL');
  }
  for (const hit of window.__xss_hits) fail('SCRIPT EXECUTED: ' + hit);
  for (const entry of performance.getEntriesByType('resource')) {
    if (new URL(entry.name, location.href).origin !== location.origin) fail('OFF-ORIGIN RESOURCE: ' + entry.name);
  }
  for (const call of window.__net) {
    const address = call.slice(call.indexOf(':') + 1);
    if (new URL(address, location.href).origin !== location.origin) fail('OFF-ORIGIN JS REQUEST: ' + call);
  }
}

let hostile;
async function journey(label, work) {
  try { await work(); report.journeys.push({ journey: label, passed: true }); }
  catch (error) { fail('JOURNEY ' + label + ': ' + (error.stack || error.message)); report.journeys.push({ journey: label, passed: false }); error.harnessRecorded = true; throw error; }
}
async function run() {
  const bridge = await (await fetch('/sec/bridge.json')).json();
  report.schema = bridge.source;
  hostile = makeBindings(bridge, MODE, (name, args) => { window.__calls.push(name); report.calls.push({ name, args }); });
  window.go = { app: { App: hostile.bindings } };
  const { createShell } = await import('/frontend/src/shell/shell.js');
  const shell = createShell({ root: document.getElementById('app'), runtime: hostile.runtime });
  await shell.start({ timeoutMS: 200 });
  await reached('LifecycleStatus');
  await pause(250);
  scan('startup');

  await journey('System check through Main menu', async () => {
    await mainMenu(/^System check/);
    await reached('Readiness');
    await openDisclosures(activePane());
    await click(/^Back$/);
  });
  await journey('Target snapshot chooser and Continue', async () => {
    await until(() => [...activePane().querySelectorAll('input[value="snapshot"]')].find(node => visible(node) && enabled(node)), 'snapshot choice');
    await activate(activePane().querySelector('input[value="snapshot"]'), 'Snapshot file');
    await click(/^Choose snapshot file/);
    await reached('ChooseSnapshotFile'); await reached('InspectSnapshot');
    await openDisclosures(activePane());
    await click(/^Continue$/);
    await reached('SelectTarget');
    await until(() => activePane().querySelector('input[type="search"]'), 'Packages search');
  });
  await journey('Search and explicit Details', async () => {
    await input('input[type="search"]', 'gimp');
    await reached('SearchPackages');
    const list = await until(() => [...activePane().querySelectorAll('[role="listbox"]')].find(node => visible(node) && [...node.querySelectorAll('[role="option"]')].some(visible)), 'loaded package list');
    list.focus(); list.dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true }));
    list.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await reached('GetPackage'); await pause(100); scan('GetPackage details');
    await click(/^Close package details$/, modal());
    if (document.activeElement !== list) throw new Error('Details did not restore list focus');
    report.journeys.push({ assertion: 'Details restores logical list focus', passed: true });
    await selectionDetails();
  });
  await journey('Paste preview and add', async () => {
    await click(/^Add…$/); await click(/^Paste list…$/, document.querySelector('.df-menu--anchored'));
    await input('textarea', 'gimp\nunknown-tool', modal());
    await reached('ParsePackageList');
    await until(() => [...modal().querySelectorAll('button')].find(node => /^Add \d+ packages?$/.test(nameOf(node)) && enabled(node)), 'paste ready');
    await click(/^Add \d+ packages?$/, modal()); await reached('AddPackageList');
  });
  await journey('URL validation, digest and add', async () => {
    await click(/^Add…$/); await click(/^Add URL…$/, document.querySelector('.df-menu--anchored'));
    await inputLabel(/^Download address$/, PAYLOADS.url, modal());
    const submit = [...modal().querySelectorAll('button')].find(node => nameOf(node) === 'Add URL');
    if (enabled(submit)) throw new Error('javascript: input was not rejected');
    await inputLabel(/^Download address$/, 'https://vendor.example/pool/agent.deb', modal());
    await inputLabel(/^Expected SHA-256/, '8a'.repeat(32), modal());
    await click(/^Add URL$/, modal()); await reached('AddURLs');
    const added = report.calls.find(call => call.name === 'AddURLs');
    if (added.args[0][0].sha256 !== '8a'.repeat(32)) throw new Error('Digest lost before AddURLs');
  });
  await journey('Native multi-file chooser reference path', async () => {
    await click(/^Add…$/); await click(/^Add local .deb…$/, document.querySelector('.df-menu--anchored'));
    await reached('ChooseLocalDebs'); await reached('AddLocalDebs');
    const added = report.calls.filter(call => call.name === 'AddLocalDebs');
    if (added.length !== 1 || added[0].args[0].length !== 2) throw new Error('Files not handed to AddLocalDebs exactly once');
    await selectionDetails();
  });
  await journey('Bundle output, signing and command validation', async () => {
    await click(/^Review bundle$/);
    await click(/^Choose folder…$/); await reached('ChooseDirectory');
    await click(/^Choose key…$/); await reached('ChooseSigningKey');
    await openDisclosures(activePane()); await reached('PreviewCommand');
    const ack = [...activePane().querySelectorAll('input[type="checkbox"]')].find(node => /responsible for redistribution/.test(node.closest('label')?.textContent || node.parentElement.textContent));
    if (!ack) throw new Error('Redistribution acknowledgement missing');
    if (!ack.checked) await activate(ack, 'Redistribution acknowledgement');
    await until(() => [...activePane().querySelectorAll('button')].find(node => nameOf(node) === 'Build bundle' && enabled(node)), 'validated Build bundle');
    await click(/^Build bundle$/); await reached('StartBuild');
    await openDisclosures(activePane());
    // Fixture job completion follows a user-started job; it is not a hidden
    // screen-state mutation. Subsequent view recovery still reads status.
    hostile.finishBuild(); await pause(180); scan('completed build');
    await click(/^Copy to drive…$/);
  });
  await journey('Copy destination plan', async () => {
    await reached('ListVolumes');
    const destination = await until(() => [...activePane().querySelectorAll('input[type="radio"][data-dest-path]')].find(node => visible(node) && enabled(node)), 'mounted destination');
    await activate(destination, 'Mounted destination'); await reached('PlanExport'); await reached('InspectDestination');
    await openDisclosures(activePane());
    scan('copy plan');
    await click(/^Back$/);
  });
  await journey('Existing-copy source chooser through Main menu', async () => {
    const before = report.calls.filter(call => call.name === 'ChooseDirectory').length;
    await mainMenu(/^Copy existing bundle/);
    await reached('ChooseDirectory', before + 1);
    await reached('ExportStatus');
  });
  await journey('Hostile error and poison recovery', async () => {
    hostile.emitError(); await pause(80);
    await openDisclosures(document.getElementById('shell-banners'));
    scan('error details');
    if (hostile.poisonEvents()) {
      await pause(120); scan('poisoned enum/error phase');
      hostile.recover(); await pause(120); scan('authoritative recovery');
    }
    const before = report.calls.filter(call => call.name === 'ChooseDirectory').length;
    await click(/^Change…$/); await reached('ChooseDirectory', before + 1);
    if (MODE === 'poison') report.poisonRecovery = true;
  });
  await pause(200);
}
function finish() {
  scan('final');
  for (const error of hostile?.errors || []) fail('FIXTURE: ' + error);
  const required = ['Readiness', 'ChooseSnapshotFile', 'InspectSnapshot', 'SelectTarget', 'SearchPackages', 'GetPackage', 'SelectionPage', 'ParsePackageList', 'AddPackageList', 'AddURLs', 'ChooseLocalDebs', 'AddLocalDebs', 'ChooseDirectory', 'ChooseSigningKey', 'PreviewCommand', 'StartBuild', 'BuildStatus', 'ListVolumes', 'InspectDestination', 'PlanExport', 'ExportStatus', 'LifecycleStatus'];
  for (const method of required) if (!report.calls.some(call => call.name === method)) fail('UNREACHED REQUIRED BINDING: ' + method);
  for (const payload of ['desc', 'stderr', 'label', 'path']) if (!seen.has(payload)) fail('PAYLOAD NEVER VISIBLE VERBATIM: ' + payload);
  if (MODE !== 'control' && !seen.has('url')) fail('HOSTILE URL NEVER VISIBLE VERBATIM');
  if (MODE === 'control' && (!report.controlAnchor || !seen.has('controlURL'))) fail('HTTPS POSITIVE CONTROL NOT VISIBLE AS LINK AND TEXT');
  if (MODE === 'poison' && !report.poisonRecovery) fail('POISON PHASE DID NOT RECOVER TO A REACHED CHOOSER');
  report.seen = [...seen]; report.projectedFields = [...(hostile?.projectedFields || [])]; report.done = true;
  report.totalProblems = report.problems.length;
  hostile?.destroy();
  const lines = [
    'MODE: ' + MODE,
    ...report.problems.map(problem => 'FAIL: ' + problem),
    'PAYLOADS RENDERED VERBATIM AS VISIBLE TEXT: ' + report.seen.join(','),
    'CONTROL HTTPS ANCHOR: ' + report.controlAnchor,
    'XSS HITS: ' + (window.__xss_hits.join(',') || 'none'),
    'BINDINGS CALLED: ' + [...new Set(report.calls.map(call => call.name))].sort().join(' '),
    'PROJECTED TEXT FIELDS: ' + report.projectedFields.length,
    'TOTAL PROBLEMS: ' + report.totalProblems,
    'REPORT JSON: ' + JSON.stringify(report),
    'DONE',
  ];
  document.getElementById('results').textContent = lines.join('\n');
}
run().catch(error => { if (!error.harnessRecorded) fail('HARNESS FAILED: ' + (error.stack || error.message)); }).finally(finish);
