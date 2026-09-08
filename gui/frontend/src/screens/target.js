// Backend order is preserved. Resolution, caching and selection revalidation
// stay in Go; this view owns only a session's target-choice controls.
import { bindDisclosure } from '../shell/disclosure.js';
import { CLI_BINARY } from '../shell/shell.js';

const BASE_CAVEAT = 'A stock base is an assumption about a default install, not a measurement of the machine you are building for. If the target has packages a default install does not, the bundle may be short. Take a snapshot on the real machine when you can.';
const STANDARD_INSTALLATION_NOTE = 'A standard installation assumes the target has its default packages. Changed packages may leave the bundle short; use a snapshot for that machine.';
const SNAPSHOT_COMMAND = `${CLI_BINARY} snapshot create --out target.tar.zst`;
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = String(text);
  return node;
}
function button(text, kind, action) {
  const node = el('button', `df-btn df-btn--${kind}`, text);
  node.type = 'button'; node.addEventListener('click', action); return node;
}
function notice(kind, message, hint = '') {
  const box = el('div', `df-banner df-banner--${kind}`);
  box.setAttribute('role', kind === 'danger' ? 'alert' : 'status');
  const glyph = el('span', 'df-banner__icon', kind === 'info' ? 'i' : '!');
  glyph.setAttribute('aria-hidden', 'true'); box.appendChild(glyph);
  const body = el('div', 'df-banner__body');
  body.appendChild(el('p', 'df-banner__text', message));
  if (hint) body.appendChild(el('p', 'df-banner__text', hint));
  box.appendChild(body); return box;
}
function disclosure(title) {
  const details = el('details', 'df-disclosure');
  details.appendChild(el('summary', null, title));
  const body = el('div', 'df-stack df-stack--tight');
  details.appendChild(body); bindDisclosure(details); return { el: details, body };
}
function keyValues(entries) {
  const list = el('dl', 'df-key-value');
  for (const [name, value] of entries) {
    if (value == null || value === '' || (Array.isArray(value) && !value.length)) continue;
    list.append(el('dt', null, name), el('dd', null, Array.isArray(value) ? value.join(', ') : value));
  }
  return list;
}
function errorView(error) {
  const box = notice('danger', error.message || 'The target could not be read.', error.hint || 'Try again.');
  if (error.details || error.command?.length || error.code) {
    const more = disclosure('Error details');
    more.body.appendChild(el('pre', 'df-log', [error.command?.join(' '), error.details, error.code].filter(Boolean).join('\n')));
    box.querySelector('.df-banner__body').appendChild(more.el);
  }
  return box;
}
function unique(rows, key, label) {
  const seen = new Set();
  return rows.filter((row) => { const value = row[key] || ''; if (seen.has(value)) return false; seen.add(value); return true; })
    .map((row) => ({ value: row[key] || '', label: label(row) }));
}
function distro(id) { return id === 'ubuntu' ? 'Ubuntu' : id === 'debian' ? 'Debian' : id; }
function filename(path) { return String(path || '').split(/[\\/]/).filter(Boolean).pop() || 'Snapshot file'; }

function createScreen() {
  let ctx, root, dom;
  let mounted = false, visible = false, busy = false;
  let mode = 'base', touched = false, initialized = false;
  let arches = [], arch = '', bases = [], caveat = BASE_CAVEAT;
  let basePhase = 'idle', baseError = null, archError = null;
  let pick = { distro: '', version: '', edition: '' };
  let snapshot = null, snapshotPath = '', snapshotPhase = 'idle', snapshotError = null;
  let snapshotChanged = false, current = null, sourceProblems = [], commitError = null;
  let lifecycle = null, lifecycleError = null;
  let baseRequest = 0, snapshotRequest = 0, contextRequest = 0, lifecycleEpoch = 0, screenEpoch = 0;
  const toast = (kind, text) => ctx.toast(kind, text);
  async function call(method, ...args) {
    try { return await ctx.bindings[method](...args); }
    catch (error) { return { error: { code: 'app.bridge', message: 'Debark could not read the target information.', hint: 'Try again. If this continues, restart Debark.', details: String(error) } }; }
  }
  function locked() { return Boolean(lifecycleError || !lifecycle || lifecycle.build_running || lifecycle.export_running || lifecycle.stopping); }
  function acceptLifecycle(status) {
    const valid = status && ['build_running', 'export_running', 'stopping'].every(key => typeof status[key] === 'boolean');
    lifecycle = valid ? status : null;
    lifecycleError = valid ? null : status?.error || { message: 'Active job status could not be read.' };
  }
  function lockReason() {
    if (lifecycleError) return 'Could not check active jobs. Retry before editing the target.';
    if (!lifecycle) return 'Checking active jobs…';
    if (lifecycle.stopping) return 'Debark is stopping the current job. Target edits are unavailable.';
    return lifecycle.export_running ? 'Target edits are unavailable while a copy is running.' : 'Target edits are unavailable while a build is running.';
  }
  function chosenBase() { return bases.find((base) => base.distro_id === pick.distro && base.version_id === pick.version && (base.variant || '') === pick.edition); }
  function draft() {
    if (mode === 'base' && basePhase === 'ready') {
      const base = chosenBase(); if (base) return { kind: 'base', base_id: base.id, arch: base.arch || arch };
    }
    if (mode === 'snapshot' && snapshotPhase === 'ready' && snapshot) return { kind: 'snapshot', snapshot_path: snapshot.path || snapshotPath };
    return null;
  }
  function unchanged(selection) {
    if (!current?.selected || !selection || current.kind !== selection.kind) return false;
    return selection.kind === 'base' ? current.id === selection.base_id && current.arch === selection.arch
      : !snapshotChanged && current.snapshot_path === selection.snapshot_path;
  }
  function normalize() {
    const distributions = unique(bases, 'distro_id', (base) => distro(base.distro_id));
    if (!distributions.some((item) => item.value === pick.distro)) pick.distro = distributions[0]?.value || '';
    const versions = unique(bases.filter((base) => base.distro_id === pick.distro), 'version_id', (base) => base.version_id);
    if (!versions.some((item) => item.value === pick.version)) pick.version = versions[0]?.value || '';
    const editions = unique(bases.filter((base) => base.distro_id === pick.distro && base.version_id === pick.version), 'variant', (base) => base.variant || 'default');
    if (!editions.some((item) => item.value === pick.edition)) pick.edition = editions[0]?.value || '';
  }
  function fillSelect(select, items, value) {
    select.replaceChildren(...items.map((item) => { const option = el('option', null, item.label); option.value = item.value; return option; }));
    select.value = value; select.disabled = busy || locked() || !items.length;
  }
  function markChanged() { touched = true; commitError = null; sourceProblems = []; }
  function chooseMode(next) { if (busy || locked()) return; markChanged(); mode = next; render(); }
  async function loadBases() {
    const request = ++baseRequest;
    basePhase = 'loading'; baseError = null; render();
    const result = await call('ListBases', arch);
    if (!mounted || request !== baseRequest) return;
    if (result?.error) { baseError = result.error; basePhase = 'error'; }
    else {
      bases = result.bases || []; caveat = result.caveat || BASE_CAVEAT; arch = result.arch || arch;
      if (arch && !arches.includes(arch)) arches.push(arch);
      basePhase = bases.length ? 'ready' : 'empty'; normalize();
    }
    render();
  }
  async function loadInitial() {
    const request = ++contextRequest, epoch = lifecycleEpoch;
    const [architecture, target, jobs] = await Promise.all([call('SupportedArchitectures'), call('CurrentTarget'), call('LifecycleStatus')]);
    if (!mounted || request !== contextRequest) return;
    if (epoch === lifecycleEpoch) acceptLifecycle(jobs);
    archError = architecture?.error || null; arches = architecture?.arches || []; arch = arch || architecture?.default || arches[0] || '';
    if (target?.target?.selected && (!current || target.target.generation >= current.generation)) current = target.target;
    sourceProblems = target?.source_problems || [];
    if (target?.error) commitError = target.error;
    if (!touched && current?.selected) {
      mode = current.kind;
      if (mode === 'base') { arch = current.arch; pick = { distro: current.distro_id, version: current.version_id, edition: current.variant || '' }; }
    }
    initialized = true;
    const listing = loadBases();
    if (!touched && current?.kind === 'snapshot' && current.snapshot_path) await inspectSnapshot(current.snapshot_path, false);
    await listing; render();
  }
  async function refreshContext() {
    const request = ++contextRequest, epoch = lifecycleEpoch;
    const [target, jobs] = await Promise.all([call('CurrentTarget'), call('LifecycleStatus')]);
    if (!mounted || request !== contextRequest) return;
    if (target?.target && (!current || target.target.generation >= current.generation)) current = target.target;
    if (epoch === lifecycleEpoch) acceptLifecycle(jobs);
    if (target?.error) commitError = target.error;
    render();
  }
  async function chooseSnapshot() {
    if (busy || locked() || snapshotPhase === 'picking' || snapshotPhase === 'loading') return;
    const request = ++snapshotRequest;
    snapshotPhase = 'picking'; snapshotError = null; render();
    const result = await call('ChooseSnapshotFile');
    if (!mounted || request !== snapshotRequest) return;
    if (result?.cancelled) { snapshotPhase = snapshot ? 'ready' : 'idle'; render(); dom.chooseSnapshot.focus(); return; }
    if (result?.error) { snapshotError = result.error; snapshotPhase = 'error'; render(); return; }
    if (!result?.path) { snapshotPhase = snapshot ? 'ready' : 'idle'; render(); return; }
    markChanged(); await inspectSnapshot(result.path, true);
    if (visible && mounted) dom.chooseSnapshot.focus();
  }
  async function inspectSnapshot(path, changed) {
    const request = ++snapshotRequest;
    snapshotPath = path; snapshot = null; snapshotChanged = changed; snapshotPhase = 'loading'; snapshotError = null; render();
    const result = await call('InspectSnapshot', path);
    if (!mounted || request !== snapshotRequest) return;
    if (result?.error) { snapshotError = result.error; snapshotPhase = 'error'; }
    else if (result?.snapshot?.path && result.snapshot.arch) { snapshot = result.snapshot; snapshotPhase = 'ready'; }
    else { snapshotPhase = 'error'; snapshotError = { message: 'This file did not describe a target.', hint: 'Choose a target snapshot, rather than a bundle archive.' }; }
    render();
  }
  async function continueToPackages() {
    const selection = draft();
    if (busy) return;
    if (!selection) { dom.live.textContent = mode === 'snapshot' ? 'Choose a valid snapshot file first.' : 'Choose a standard installation first.'; return; }
    // Unchanged-target navigation remains available during a job; mutations
    // are correctly rejected by the backend and need not be attempted here.
    if (unchanged(selection)) { ctx.go('picker', { prepare: true }); return; }
    if (locked()) { dom.live.textContent = lockReason(); return; }
    const epoch = screenEpoch;
    busy = true; commitError = null; render();
    const result = await call('SelectTarget', selection);
    if (!mounted || epoch !== screenEpoch) return;
    busy = false; sourceProblems = result?.source_problems || [];
    if (result?.target?.selected) {
      current = result.target; snapshotChanged = false;
      if (result.error) toast('warning', [result.error.message, result.error.hint].filter(Boolean).join(' '));
      const unexpected = sourceProblems.filter((problem) => !problem.deliberate);
      if (unexpected.length) toast('warning', `${unexpected.length} package source${unexpected.length === 1 ? ' was' : 's were'} unavailable. ${unexpected[0].reason} Review Sources in Target for details.`);
      render(); if (visible) ctx.go('picker', { prepare: true });
    } else {
      commitError = result?.error || { message: 'The target could not be selected.', hint: 'Try again.' };
      render(); dom.error.scrollIntoView({ block: 'nearest' });
    }
  }
  function renderSources(container) {
    for (const problem of sourceProblems.filter((item) => !item.deliberate)) container.appendChild(notice('warning', problem.reason, [problem.file, problem.line ? `line ${problem.line}` : ''].filter(Boolean).join(': ')));
    if (!sourceProblems.length) return;
    const details = disclosure('Sources');
    for (const problem of sourceProblems) details.body.appendChild(keyValues([['Source', problem.file], ['Line', problem.line || ''], ['Status', problem.deliberate ? 'Skipped by catalogue scope' : 'Unavailable'], ['Reason', problem.reason], ['Source text', problem.text]]));
    container.appendChild(details.el);
  }
  function render() {
    if (!mounted || !dom) return;
    const disabled = busy || locked();
    for (const [key, input] of Object.entries(dom.mode)) { input.checked = mode === key; input.disabled = disabled; input.closest('label').classList.toggle('is-selected', mode === key); }
    dom.base.hidden = mode !== 'base'; dom.snapshot.hidden = mode !== 'snapshot';
    dom.job.replaceChildren();
    dom.job.hidden = !locked() && !lifecycle?.error;
    if (locked()) { dom.job.appendChild(notice('info', lockReason())); if (lifecycleError) dom.job.appendChild(button('Retry job check', 'secondary', refreshContext)); }
    if (lifecycle?.error) dom.job.appendChild(errorView(lifecycle.error));
    dom.error.replaceChildren(); if (commitError) dom.error.appendChild(errorView(commitError)); renderSources(dom.error);
    dom.error.hidden = !commitError && !sourceProblems.length;
    dom.current.textContent = current?.selected ? `Current target: ${current.label || current.id}. Selected packages are kept when the target changes.` : '';
    dom.current.hidden = !current?.selected;
    fillSelect(dom.distro, unique(bases, 'distro_id', (base) => distro(base.distro_id)), pick.distro);
    fillSelect(dom.version, unique(bases.filter((base) => base.distro_id === pick.distro), 'version_id', (base) => [base.version_id, base.codename].filter(Boolean).join(' ')), pick.version);
    fillSelect(dom.edition, unique(bases.filter((base) => base.distro_id === pick.distro && base.version_id === pick.version), 'variant', (base) => base.variant || 'default'), pick.edition);
    fillSelect(dom.arch, arches.map((value) => ({ value, label: value })), arch);
    dom.baseState.replaceChildren();
    dom.baseState.hidden = !archError && basePhase === 'ready';
    if (archError) dom.baseState.appendChild(errorView(archError));
    if (basePhase === 'loading' || basePhase === 'idle') dom.baseState.appendChild(el('p', 'df-text-secondary', 'Loading standard installations…'));
    if (basePhase === 'error') dom.baseState.append(errorView(baseError), button('Retry installation list', 'secondary', () => archError ? loadInitial() : loadBases()), button('System check', 'ghost', () => ctx.go('readiness', { returnTo: 'target' })));
    else if (archError) dom.baseState.appendChild(button('Retry architecture list', 'secondary', loadInitial));
    if (basePhase === 'empty') dom.baseState.append(notice('info', 'No standard installations are available for this architecture.', 'Choose a snapshot file or check the installation list again.'), button('Retry installation list', 'secondary', loadBases));
    const base = chosenBase();
    dom.baseSummary.textContent = base ? `${distro(base.distro_id)} ${base.version_id} ${base.variant || ''} (${base.arch || arch})` : '';
    dom.baseSummary.hidden = !base;
    dom.baseCaveat.textContent = STANDARD_INSTALLATION_NOTE; dom.baseTech.body.replaceChildren();
    if (base) dom.baseTech.body.appendChild(keyValues([['Installation assumption', caveat], ['Base identity', base.id], ['Description', base.description], ['Definition digest', base.digest], ['Seeds', base.seeds], ['Exclusions', base.excludes], ['Recommends for seed resolution', base.recommends ? 'Included' : 'Excluded']]));
    if (mode === 'base' && unchanged(draft())) addResolvedDetails(dom.baseTech.body);
    dom.baseTech.el.hidden = !base;
    dom.chooseSnapshot.textContent = snapshot ? 'Change…' : snapshotPhase === 'picking' ? 'Choosing…' : 'Choose snapshot file…';
    dom.chooseSnapshot.disabled = disabled || snapshotPhase === 'picking' || snapshotPhase === 'loading';
    dom.snapshotState.replaceChildren();
    dom.snapshotState.hidden = snapshotPhase !== 'loading' && snapshotPhase !== 'error';
    if (snapshotPhase === 'loading') dom.snapshotState.appendChild(el('p', 'df-text-secondary', 'Reading snapshot…'));
    if (snapshotPhase === 'error') {
      dom.snapshotState.appendChild(errorView(snapshotError));
      const help = disclosure('Create a snapshot on the target');
      help.body.append(el('p', null, 'Run this on the offline target, then bring the file to this machine.'), el('pre', 'df-log', SNAPSHOT_COMMAND));
      help.body.appendChild(button('Copy command', 'secondary', async () => { const result = await call('CopyToClipboard', SNAPSHOT_COMMAND); toast(result?.error ? 'warning' : 'success', result?.error?.message || 'Snapshot command copied.'); }));
      dom.snapshotState.appendChild(help.el);
    }
    dom.snapshotSummary.replaceChildren(); dom.snapshotTech.body.replaceChildren(); dom.snapshotTech.el.hidden = !snapshot;
    dom.snapshotSummary.hidden = !snapshot;
    if (snapshot) {
      const captured = snapshot.origin_kind === 'captured', synthesized = snapshot.origin_kind === 'synthesized';
      dom.snapshotSummary.appendChild(keyValues([['File', filename(snapshot.path || snapshotPath)], ['Operating system', `${distro(snapshot.distro_id)} ${snapshot.version_id}${snapshot.variant ? ` ${snapshot.variant}` : ''}`], ['Architecture', snapshot.arch], [captured ? 'Captured at' : 'Created at', snapshot.created_at || 'Not recorded']]));
      dom.snapshotSummary.appendChild(notice(synthesized ? 'warning' : 'info', captured
        ? 'This records the target when it was captured. Later changes to that machine are not included.'
        : synthesized ? 'This snapshot was made from a standard installation, not captured on a machine. Its assumptions may leave the bundle short.'
          : 'This snapshot does not record how it was made. Confirm its origin before relying on it.'));
      dom.snapshotTech.body.appendChild(keyValues([['Path', snapshot.path || snapshotPath], ['Origin', snapshot.origin_kind || 'Not recorded'], ['Schema', snapshot.schema_version], ['Recorded packages', snapshot.package_count], ['Additional architectures', snapshot.foreign_archs], ['Codename', snapshot.codename], ['Base identity', snapshot.base_id], ['Source', snapshot.origin_source], ['Source digest', snapshot.origin_source_digest]]));
      if (unchanged(draft())) addResolvedDetails(dom.snapshotTech.body);
    }
    const selection = draft();
    const reason = busy ? 'Selecting target…' : !selection ? mode === 'snapshot' ? 'Choose a valid snapshot file.' : 'Choose a standard installation.' : locked() && !unchanged(selection) ? lockReason() : '';
    dom.continue.setAttribute('aria-disabled', reason ? 'true' : 'false'); dom.continue.setAttribute('aria-busy', busy ? 'true' : 'false');
    dom.continue.textContent = busy ? 'Selecting…' : 'Continue'; dom.actionStatus.textContent = reason;
  }
  function addResolvedDetails(body) { body.appendChild(keyValues([['Catalogue suites', current.suites], ['Catalogue components', current.components]])); }
  function field(label, onChange) {
    const wrapper = el('label', 'df-field'); wrapper.appendChild(el('span', 'df-field__label', label));
    const select = el('select', 'df-input'); select.setAttribute('aria-label', label);
    select.addEventListener('change', () => { if (busy || locked()) return; markChanged(); onChange(select.value); });
    wrapper.appendChild(select); return { wrapper, select };
  }
  function mount(mountRoot, screenCtx) {
    ctx = screenCtx; root = mountRoot; mounted = true;
    const layout = el('section', 'df-screen-layout'), content = el('div', 'df-screen-content'), page = el('div', 'df-app__content'), stack = el('div', 'df-stack');
    page.appendChild(stack); content.appendChild(page); layout.appendChild(content); root.appendChild(layout);
    stack.appendChild(el('h1', null, 'Choose target'));
    const job = el('div', 'df-stack df-stack--tight'); stack.appendChild(job);
    const currentNode = el('p', 'df-text-sm df-text-secondary'); stack.appendChild(currentNode);
    const choiceBox = el('div', 'df-boxed-list'); choiceBox.setAttribute('role', 'radiogroup'); choiceBox.setAttribute('aria-label', 'Target source');
    const modeInputs = {};
    for (const [value, title, description] of [['base', 'Standard installation', 'Choose the system and edition installed on the target.'], ['snapshot', 'Snapshot file', 'Use a saved record of the target, read locally.']]) {
      const row = el('label', 'df-boxed-list__row'), radio = el('input', 'df-radio__input'); radio.type = 'radio'; radio.name = 'df-target-kind'; radio.value = value;
      radio.addEventListener('change', () => { if (radio.checked) chooseMode(value); });
      const text = el('span', 'df-boxed-list__text'); text.append(el('span', 'df-boxed-list__label', title), el('span', 'df-boxed-list__description', description));
      row.append(radio, text); choiceBox.appendChild(row); modeInputs[value] = radio;
    }
    stack.appendChild(choiceBox);
    const error = el('div', 'df-stack df-stack--tight'); stack.appendChild(error);
    const base = el('div', 'df-stack'); stack.appendChild(base);
    const fields = el('div', 'df-target-fields'); base.appendChild(fields);
    const distributionField = field('Distribution', (value) => { pick.distro = value; normalize(); render(); });
    const versionField = field('Version', (value) => { pick.version = value; normalize(); render(); });
    const editionField = field('Edition', (value) => { pick.edition = value; render(); });
    const archField = field('Architecture', (value) => { arch = value; loadBases(); });
    for (const entry of [distributionField, versionField, editionField, archField]) fields.appendChild(entry.wrapper);
    const baseState = el('div', 'df-stack df-stack--tight'); base.appendChild(baseState);
    const baseSummary = el('p', 'df-text-secondary'); base.appendChild(baseSummary);
    const caveatBox = notice('warning', ''), baseCaveat = caveatBox.querySelector('.df-banner__text'); base.appendChild(caveatBox);
    const baseTech = disclosure('Technical details'); base.appendChild(baseTech.el);
    const snap = el('div', 'df-stack'); stack.appendChild(snap);
    const choose = button('Choose snapshot file…', 'secondary', chooseSnapshot); snap.appendChild(choose);
    const snapshotState = el('div', 'df-stack df-stack--tight'); snap.appendChild(snapshotState);
    const snapshotSummary = el('div', 'df-stack df-stack--tight'); snap.appendChild(snapshotSummary);
    const snapshotTech = disclosure('Technical details'); snap.appendChild(snapshotTech.el);
    const actions = el('div', 'df-actionbar'), actionStatus = el('span', 'df-actionbar__status'); actionStatus.id = 'df-target-action-status'; actions.appendChild(actionStatus);
    const next = button('Continue', 'primary', continueToPackages); next.setAttribute('aria-describedby', actionStatus.id); actions.appendChild(next); layout.appendChild(actions);
    const live = el('p', 'df-live'); live.setAttribute('role', 'status'); live.setAttribute('aria-live', 'polite'); layout.appendChild(live);
    dom = { layout, content, page, job, current: currentNode, mode: modeInputs, error, base, distro: distributionField.select, version: versionField.select, edition: editionField.select, arch: archField.select, baseState, baseSummary, baseCaveat, baseTech,
      snapshot: snap, chooseSnapshot: choose, snapshotState, snapshotSummary, snapshotTech, continue: next, actionStatus, live };
    ctx.on('target:changed', (target) => { if (target && (!current || target.generation >= current.generation)) { current = target; render(); } });
    ctx.on('app:lifecycle', (status) => { lifecycleEpoch++; acceptLifecycle(status); render(); });
    render(); loadInitial();
  }
  return {
    id: 'target', title: 'Choose target', mount,
    show(screenCtx, params) { if (screenCtx) ctx = screenCtx; visible = true; if (params?.tab === 'base' || params?.tab === 'snapshot') { mode = params.tab; touched = true; } render(); if (initialized) refreshContext(); },
    hide() { visible = false; },
    destroy() {
      mounted = false; visible = false; baseRequest++; snapshotRequest++; contextRequest++; screenEpoch++;
      root?.replaceChildren(); dom = null; ctx = null;
      initialized = false; touched = false; busy = false; mode = 'base';
      arches = []; arch = ''; bases = []; basePhase = 'idle'; baseError = null; archError = null;
      pick = { distro: '', version: '', edition: '' }; snapshot = null; snapshotPath = ''; snapshotPhase = 'idle'; snapshotError = null;
      snapshotChanged = false; current = null; sourceProblems = []; commitError = null; lifecycle = null; lifecycleError = null;
    },
  };
}

export default createScreen();
