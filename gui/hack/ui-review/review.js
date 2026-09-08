import { createFixture } from './fixtures.mjs';
import { observeAppearance } from './appearance.js';

const params = new URLSearchParams(location.search);
const scenario = params.get('scenario') || 'target';
const theme = params.get('theme') || 'light';
const observed = { scenario, theme, ready: false, errors: [], journey: [], calls: [], measurements: null };
window.__UI_REVIEW__ = observed;
window.addEventListener('error', e => observed.errors.push(e.message));
window.addEventListener('unhandledrejection', e => observed.errors.push(String(e.reason?.stack || e.reason)));
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const visible = el => {
  if (!el || el.closest('[hidden]')) return false;
  if (el.closest('dialog:not([open])')) return false;
  const closedDetails = el.closest('details:not([open])');
  if (closedDetails && !el.closest('summary')) return false;
  const style = getComputedStyle(el), r = el.getBoundingClientRect();
  return style.display !== 'none' && style.visibility !== 'hidden' && r.width > 0 && r.height > 0;
};

function inspect() {
  const content = document.querySelector('.df-app__pane') || document.getElementById('app');
  const controls = Array.from(document.querySelectorAll('button,input,select,textarea,[role="button"],summary'))
    .filter(visible).map(el => { const r = el.getBoundingClientRect(); return {
      tag: el.tagName, role: el.getAttribute('role'), name: el.getAttribute('aria-label') || el.textContent.trim() || el.id,
      disabled: !!el.disabled, rect: { x: r.x,y:r.y,width:r.width,height:r.height },
      inViewport: r.top >= 0 && r.left >= 0 && r.bottom <= innerHeight + 1 && r.right <= innerWidth + 1,
    }; });
  const helperNodes = Array.from(content.querySelectorAll('p,.df-field__hint,.df-boxed-list__description,.df-help,.df-note'))
    .filter(visible).filter(el => !el.closest('[role="option"],pre,code,.df-mono,[role="listbox"]'))
    .filter(el => !Array.from(el.children).some(child => child.matches('p,.df-field__hint,.df-boxed-list__description,.df-help,.df-note') && visible(child)));
  const helperText = helperNodes.map(el => el.textContent.trim()).filter(Boolean);
  observed.measurements = { viewport: { width: innerWidth, height: innerHeight }, devicePixelRatio,
    fontSize: getComputedStyle(document.documentElement).fontSize,
    horizontalOverflow: document.documentElement.scrollWidth > innerWidth + 1,
    activeElement: { tag: document.activeElement?.tagName, id: document.activeElement?.id, role: document.activeElement?.getAttribute('role') },
    actionbars: Array.from(document.querySelectorAll('.df-actionbar')).filter(visible).length,
    controls, helperText, helperWords: helperText.join(' ').split(/\s+/).filter(Boolean).length,
    text: content.innerText,
  };
  observed.appearance = observeAppearance(document);
  return observed;
}

// Journeys may only interact through visible DOM controls. Backend transitions
// are deliberately separate evidence: seed setup is never called a UI journey.
function button(name) {
  const matches = Array.from(document.querySelectorAll('button,[role="button"],summary')).filter(visible)
    .filter(el => name instanceof RegExp ? name.test(el.getAttribute('aria-label') || el.textContent.trim()) : (el.getAttribute('aria-label') || el.textContent.trim()) === name);
  if (matches.length !== 1) throw new Error(`Expected one visible control ${name}; found ${matches.length}`);
  if (matches[0].disabled) throw new Error(`Visible control ${name} is disabled`);
  return matches[0];
}
async function click(name) { const el = button(name); el.click(); observed.journey.push({ action: 'click', name: String(name) }); await sleep(150); }
async function search(text) {
  const el = Array.from(document.querySelectorAll('input[type="search"]')).find(visible);
  if (!el) throw new Error('No visible search field');
  el.focus(); el.value = text; el.dispatchEvent(new Event('input', { bubbles:true }));
  observed.journey.push({ action: 'search', text }); await sleep(180);
}
function assert(condition, message) {
  if (!condition) throw new Error(message);
  observed.journey.push({ assertion: message, passed: true });
}
async function input(selector, value) {
  const el = Array.from(document.querySelectorAll(selector)).find(visible);
  if (!el) throw new Error(`No visible field ${selector}`);
  el.focus(); el.value = value; el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  observed.journey.push({ action: 'input', selector, value }); await sleep(200);
}
async function chooseDestination() {
  const radio = Array.from(document.querySelectorAll('input[name="df-export-destination"]')).find(visible);
  if (!radio) throw new Error('No visible destination radio');
  radio.focus(); radio.click(); await sleep(220);
}

async function boot() {
  localStorage.setItem('debark.theme', theme);
  document.documentElement.dataset.theme = theme;
  const schema = await (await fetch('./schema.json')).json();
  const fixture = createFixture({ scenario, schema });
  observed.calls = fixture.calls; observed.fixtureErrors = fixture.errors;
  window.go = { app: { App: fixture.bindings } };
  window.runtime = { EventsOnMultiple: fixture.runtime.on, EventsOff: fixture.runtime.off, EventsEmit() {}, LogPrint() {}, BrowserOpenURL() {} };
  let shell;
  if (scenario === 'startup') {
    await import('/frontend/src/main.js');
    shell = window.__debarkShell;
    await sleep(500);
  } else {
    const { createShell } = await import('/frontend/src/shell/shell.js');
    shell = createShell({ root: document.getElementById('app'), runtime: fixture.runtime });
    await shell.start({ timeoutMS: 200 });
    shell.setTarget(fixture.state.target); shell.setReadiness(await fixture.bindings.Readiness());
    const route = scenario.startsWith('build') ? 'build' : scenario.startsWith('copy') ? 'export' :
      /picker|catalog|undo|close-unsaved|selection-many/.test(scenario) ? 'picker' : scenario.startsWith('readiness') ? 'readiness' : 'target';
    const routeParams = route === 'export' ? (scenario.startsWith('copy-choose') ? { sourceMode: 'choose', returnTo: 'target' } :
      fixture.state.copy.finished || fixture.state.copy.running ? undefined : { bundlePath: fixture.state.build.summary.bundle_path,
      bundleSummary: fixture.state.build.summary, sourceTarget: fixture.state.build.target_id, sourceMode: 'result', returnTo: 'build' }) : undefined;
    await shell.go(route, routeParams, { focus: false });
    await sleep(450);
  }
  if (scenario.startsWith('picker') && !scenario.includes('empty')) await search(params.get('query') || '');
  const journey = params.get('journey');
  const journeys = new Set(['', 'none', 'target-continue', 'snapshot', 'search-gimp', 'main-menu',
    'clear-undo', 'selection-paging', 'package-details', 'details-close', 'build-advanced',
    'selection-drawer', 'add-menu', 'add-url', 'add-files', 'paste-list', 'url-undo',
    'build-keygen', 'catalog-retry', 'catalog-cancel', 'copy-choose-check', 'copy-plan',
    'copy-skip', 'copy-cancel', 'copy-subdir', 'copy-another']);
  if (journey && !journeys.has(journey)) throw new Error(`Unknown visible-control journey: ${journey}`);
  if (journey === 'target-continue') {
    await click(/^Continue/); await sleep(550);
    assert(fixture.calls.filter(c => c.name === 'SelectTarget').length === 1, 'Continue commits one target');
    assert(fixture.calls.filter(c => c.name === 'StartCatalogBuild').length === 1, 'Continue prepares packages once');
  }
  if (journey === 'snapshot') {
    const radio = document.querySelector('input[type="radio"][value="snapshot"]');
    if (!radio) throw new Error('Snapshot source control missing');
    radio.click(); await sleep(100); await click(/^Choose snapshot file/);
    assert(fixture.calls.some(c => c.name === 'InspectSnapshot'), 'Chosen snapshot is inspected');
    if (scenario === 'snapshot-invalid') assert(button(/^Continue/).getAttribute('aria-disabled') === 'true', 'Invalid snapshot cannot continue');
  }
  if (params.get('journey') === 'search-gimp') await search('gimp');
  if (params.get('journey') === 'main-menu') await click(/Main menu/);
  if (params.get('journey') === 'clear-undo') {
    const selected = Array.from(document.querySelectorAll('button')).find(el => visible(el) && /^Selected \(/.test(el.textContent.trim()));
    if (selected && selected.getAttribute('aria-haspopup') === 'dialog') { selected.click(); await sleep(150); }
    await click(/^Clear( selection| all)?$/);
    const close = Array.from(document.querySelectorAll('button')).find(el => visible(el) && /Close selection/.test(el.getAttribute('aria-label') || ''));
    if (close) { close.click(); await sleep(100); }
    await click(/^Undo/);
    if (fixture.summary().total !== 3) throw new Error('Undo did not restore the initial 3 selections');
  }
  if (journey === 'selection-paging') {
    const expected = fixture.state.entries.map(entry => entry.key);
    assert(expected.length === 320, 'Large selection fixture contains 320 ordered entries');
    const selected = button(/^Selected \(/);
    const drawer = selected.getAttribute('aria-haspopup') === 'dialog';
    if (drawer) await click(/^Selected \(/);
    const host = document.querySelector('.df-picker-selection');
    assert(visible(host), 'Selected packages are visible in the pane or named drawer');
    const keys = () => Array.from(host.querySelectorAll('.df-selection-item')).map(row => row.dataset.selectionKey);
    const waitForCount = async count => {
      const deadline = performance.now() + 2500;
      while (keys().length !== count && performance.now() < deadline) await sleep(40);
      assert(keys().length === count, `Selection renders ${count} rows`);
    };
    // Paging actions live after the rows, so scroll them into the actual client
    // viewport before activating. Offscreen DOM existence is not interaction.
    const scrollClick = async name => {
      const deadline = performance.now() + 2500;
      let control = null, attempts = 0;
      while (!control && performance.now() < deadline) {
        // Undo can cause several authoritative row reads. A late focus restore
        // may scroll the old row back into view after our first scroll request.
        // Focus the intended control as a real user would, and recheck after
        // the read/focus work settles rather than activating an offscreen node.
        if (host.querySelector('.pt')?.getAttribute('aria-busy') === 'true') { await sleep(40); continue; }
        const candidate = button(name);
        attempts++;
        candidate.focus({ preventScroll: true });
        candidate.scrollIntoView({ block: 'nearest', inline: 'nearest' }); await sleep(80);
        const rect = candidate.getBoundingClientRect();
        const center = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
        if (candidate.isConnected && document.activeElement === candidate && !candidate.disabled &&
            host.querySelector('.pt')?.getAttribute('aria-busy') !== 'true' &&
            rect.width > 0 && rect.height > 0 && rect.left >= 0 && rect.top >= 0 &&
            rect.right <= innerWidth + 1 && rect.bottom <= innerHeight + 1 && candidate.contains(center)) control = candidate;
      }
      assert(!!control,
        `${String(name)} is reachable in the client viewport`);
      control.click();
      observed.journey.push({ action: 'click', name: String(name), scrolledIntoView: true, settledAttempts: attempts }); await sleep(150);
    };
    await waitForCount(100);
    assert(JSON.stringify(keys()) === JSON.stringify(expected.slice(0, 100)), 'Initial bounded page preserves the selection order');
    const initialReads = fixture.calls.filter(call => call.name === 'SelectionPage');
    assert(initialReads.length > 0 && initialReads.every(call => call.args[0] === 0 && call.args[1] <= 100), 'Initial render reads only the first bounded selection page');
    await scrollClick(/^Show 100 more$/); await waitForCount(200);
    assert(JSON.stringify(keys()) === JSON.stringify(expected.slice(0, 200)), 'Show more extends the ordered selection');
    await scrollClick(/^Show all$/); await waitForCount(320);
    assert(JSON.stringify(keys()) === JSON.stringify(expected), 'Show all renders the complete selection once in order');
    const removedKey = expected[157];
    await scrollClick('Remove ' + removedKey); await waitForCount(319);
    assert(JSON.stringify(keys()) === JSON.stringify(expected.filter(key => key !== removedKey)), 'Removal changes only the chosen middle entry');
    assert(fixture.calls.filter(call => call.name === 'RemovePackages').length === 1, 'The visible remove control mutates once');
    if (drawer) await click(/^Close selected packages$/);
    await scrollClick(/^Undo/);
    assert(JSON.stringify(fixture.state.entries.map(entry => entry.key)) === JSON.stringify(expected), 'Undo restores all 320 backend entries in their original order');
    assert(fixture.calls.filter(call => call.name === 'UndoSelection').length === 1, 'The action-area Undo mutates once');
    if (drawer) await click(/^Selected \(/);
    const undoDeadline = performance.now() + 2500;
    while ((!keys().includes(removedKey) || keys().length < 319) && performance.now() < undoDeadline) await sleep(40);
    assert(keys().includes(removedKey) && keys().length >= 319, 'Undo refreshes the selected rows before the next paging action');
    // The tray retains its shown-page size across Undo; the restored last row
    // may need one explicit paging action to become visible again.
    if (keys().length < 320) await scrollClick(/^Show \d+ more$/);
    await waitForCount(320);
    assert(JSON.stringify(keys()) === JSON.stringify(expected), 'Undo and paging restore every rendered entry without duplicates');
    assert(fixture.calls.filter(call => call.name === 'SelectionPage').every(call => call.args[1] > 0 && call.args[1] <= 500), 'All selection reads obey the native page limit');
    host.querySelector('.df-selection-item').scrollIntoView({ block: 'start', inline: 'nearest' });
    await sleep(100);
  }
  if (journey === 'package-details' || journey === 'details-close') {
    await search('gimp');
    const list = Array.from(document.querySelectorAll('[role="listbox"]')).find(visible);
    assert(!!list, 'Package list is available');
    list.focus(); list.dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true }));
    list.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); await sleep(220);
    assert(fixture.calls.some(c => c.name === 'GetPackage' && c.args[0] === 'gimp'), 'Enter opens authoritative package details');
    if (journey === 'details-close') {
      await click(/^Close package details$/);
      assert(document.activeElement === list || !!document.activeElement.closest('[role="listbox"]'), 'Details close restores list focus');
      list.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true })); await sleep(200);
      assert(fixture.calls.some(c => c.name === 'AddPackages' || c.name === 'RemovePackages'), 'Space changes selection separately');
    }
  }
  if (journey === 'build-advanced') await click(/^Advanced options/);
  if (journey === 'selection-drawer') await click(/^Selected \(/);
  if (journey === 'add-menu' || journey === 'add-url' || journey === 'add-files' || journey === 'paste-list' || journey === 'url-undo') {
    await click(/^Add…$/);
    if (journey === 'add-url' || journey === 'url-undo') {
      await click(/^Add URL…$/);
      const url = 'https://operator:synthetic-secret@vendor.example/agent.deb';
      await input('dialog[open] input[id^="pt-url"]', url);
      const digest = '8a'.repeat(32);
      const digestField = Array.from(document.querySelectorAll('dialog[open] input[id^="pt-digest"]')).find(visible);
      assert(!!digestField, 'URL dialog retains SHA-256 input');
      digestField.value = digest; digestField.dispatchEvent(new Event('input', { bubbles: true })); await sleep(80);
      await click(/^Add URL$/);
      const call = fixture.calls.find(c => c.name === 'AddURLs');
      assert(call?.args[0][0].url === url && call.args[0][0].sha256 === digest, 'URL and digest reach backend unchanged');
      assert(!document.body.innerText.includes('synthetic-secret'), 'Selection presentation does not expose URL credentials');
      if (journey === 'url-undo') {
        await click(/^Remove agent.deb$/); await click(/^Undo/);
        assert(fixture.state.entries.some(e => e.url === url && e.sha256 === digest), 'Undo restores original credentialed URL and digest');
        assert(fixture.calls.some(c => c.name === 'RemovePackages' && c.args[0][0] === 'url:fixture-opaque-vendor-reference'), 'Removal uses opaque stable key');
      }
    }
    if (journey === 'add-files') {
      await click(/^Add local .deb…$/);
      assert(fixture.calls.filter(c => c.name === 'ChooseLocalDebs').length === 1, 'Local multi-file chooser runs once');
      assert(fixture.calls.filter(c => c.name === 'AddLocalDebs').length === 1 && fixture.calls.find(c => c.name === 'AddLocalDebs').args[0].length === 2, 'Both chosen files are added once');
    }
    if (journey === 'paste-list') {
      await click(/^Paste list…$/);
      await input('dialog[open] textarea', 'firefox\nunknown-tool\nbad;name'); await sleep(350);
      assert(fixture.calls.some(c => c.name === 'ParsePackageList'), 'Paste previews package names before adding');
      assert(document.body.innerText.includes('unknown-tool') && document.body.innerText.includes('bad;name'), 'Paste preview shows unknown and rejected inputs');
    }
  }
  if (journey === 'build-keygen') await click(/^Create key/);
  if (journey === 'catalog-retry') {
    await click(/^Retry/); await sleep(550);
    assert(fixture.calls.filter(c => c.name === 'StartCatalogBuild').length === 1 && fixture.state.catalog.ready, 'Explicit Retry prepares the failed catalogue once');
  }
  if (journey === 'catalog-cancel') {
    await click(/^Cancel/); await sleep(600);
    assert(fixture.state.catalog.cancelled && !fixture.state.catalog.building, 'Preparation cancellation remains stopped');
    assert(!fixture.calls.some(c => c.name === 'StartCatalogBuild'), 'Cancellation does not restart preparation');
  }
  if (journey === 'copy-choose-check') {
    assert(fixture.calls.some(c => c.name === 'ChooseDirectory'), 'Existing bundle route invokes source chooser without a target');
    assert(!fixture.calls.some(c => c.name === 'SelectTarget'), 'Existing copy does not require target selection');
    if (scenario.endsWith('cancelled')) assert(document.body.innerText.includes('Choose target'), 'Cancelled source chooser returns to opener');
    else assert(document.body.innerText.includes('existing-bundle') && document.body.innerText.includes('unknown for this source'), 'External source retains explicit path and unknown metadata');
  }
  if (journey === 'copy-plan' || journey === 'copy-skip' || journey === 'copy-cancel' || journey === 'copy-subdir') {
    await chooseDestination();
    assert(fixture.calls.some(c => c.name === 'PlanExport'), 'Chosen destination is planned');
    if (journey === 'copy-skip' || journey === 'copy-subdir') await click(/^Advanced$/);
    if (journey === 'copy-skip') {
      const skip = document.getElementById('df-export-skip-verify'); skip.click(); await sleep(150);
      assert(button(/Copy WITHOUT/).getAttribute('aria-disabled') === 'false', 'Unchecked copy is a deliberate enabled choice');
      assert(document.body.innerText.includes('Copy will be unchecked'), 'Unchecked warning remains outside Advanced');
    }
    if (journey === 'copy-subdir') {
      await input('#df-export-subdir', '../bad');
      assert(button(/^Copy and verify/).getAttribute('aria-disabled') === 'true', 'Invalid destination name cannot copy');
      await input('#df-export-subdir', 'second-copy');
      assert(fixture.calls.filter(c => c.name === 'PlanExport').at(-1).args[0].subdir === 'second-copy', 'Corrected folder name is replanned');
    }
    if (journey === 'copy-cancel') {
      await click(/^Copy and verify/); await click(/^Cancel$/);
      assert(fixture.state.copy.cancelled && !fixture.state.copy.verified, 'Cancelled copy remains unchecked');
      assert(/incomplete/i.test(document.body.innerText), 'Cancellation leaves visible incomplete warning');
    }
  }
  if (journey === 'copy-another') {
    await click(/^Copy to another drive$/);
    assert(document.getElementById('df-export-skip-verify')?.checked === false, 'Another copy resets verification to enabled');
    assert(document.body.innerText.includes('Choose where the bundle should go'), 'Another copy requires explicit destination');
  }
  await sleep(200);
  observed.ready = true;
  inspect();
  // Read-only observation for capture tools. No shell/store mutation API.
  window.__UI_REVIEW_INSPECT__ = inspect;
}
boot().catch(err => { observed.errors.push(String(err.message || err) + '\n' + (err.stack || '')); observed.ready = true; inspect(); });
