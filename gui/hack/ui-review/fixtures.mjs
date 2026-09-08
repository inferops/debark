/* Deterministic bridge fixtures. This is test data, never a catalogue/resolver.
 * Unknown methods and responses outside the generated DTO schema throw and
 * enter the observable errors list. Validate before cloning or hostile edits.
 * Returned objects are cloned so consumers cannot mutate authoritative state.
 */
export const STAMP = '2026-09-07T12:00:00Z';
export const DIGEST = '8a'.repeat(32);
export const CAVEAT = 'A standard installation is an assumption about the target. Use a snapshot for a machine that has been changed.';
const clone = (x) => JSON.parse(JSON.stringify(x));
const error = (code, message, hint) => ({ code, message, hint, retryable: true });

export function parseBridgeSchema(models, declarations) {
  const classes = {}, methods = {};
  for (const [, name, body] of models.matchAll(/export class (\w+) \{([\s\S]*?)\n\s*static createFrom/g)) {
    classes[name] = [...body.matchAll(/^\s+(\w+)(\?)?:\s*(.+?);\s*$/gm)]
      .map(([, field, optional, type]) => ({ name: field, optional: !!optional, type: type.trim() }));
  }
  for (const [, name, args, ret] of declarations.matchAll(/^export function (\w+)\((.*)\):Promise<(.+)>;\s*$/gm)) {
    methods[name] = { args, ret: ret.trim() };
  }
  const classCount = [...models.matchAll(/export class /g)].length;
  const methodCount = [...declarations.matchAll(/^export function /gm)].length;
  if (!classCount || Object.keys(classes).length !== classCount || !methodCount || Object.keys(methods).length !== methodCount) {
    throw new Error('Incomplete generated bridge schema; rebuild bindings before review.');
  }
  return { classes, methods };
}

export function validateDTO(value, type, schema, path = type) {
  const fail = (expected) => { throw new TypeError(`${path}: expected ${expected}`); };
  if (typeof type !== 'string') throw new TypeError(`${path}: missing generated return type`);
  const name = type.trim().replace(/^app\./, '');
  if (name === 'any') return; // The generated BuildEvent.attrs is deliberately opaque.
  const array = name.match(/^(?:Array<(.+)>|(.+)\[\])$/);
  if (array) {
    if (value === null) return; // Go nil slices serialize as null, even without omitempty.
    if (!Array.isArray(value)) fail(name);
    value.forEach((item, i) => validateDTO(item, array[1] || array[2], schema, `${path}[${i}]`));
    return;
  }
  if (name === 'Record<string, any>') {
    if (value !== null && (typeof value !== 'object' || Array.isArray(value))) fail(name);
    return;
  }
  if (['string', 'number', 'boolean'].includes(name)) {
    if (typeof value !== name || (name === 'number' && !Number.isFinite(value))) fail(name);
    return;
  }
  const fields = schema.classes[name];
  if (!fields) throw new TypeError(`${path}: unsupported generated type ${name}`);
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(name);
  const known = new Set(fields.map(field => field.name));
  for (const key of Object.keys(value)) {
    if (!known.has(key)) throw new TypeError(`${path}.${key}: unknown field in ${name}`);
  }
  for (const field of fields) {
    if (!Object.hasOwn(value, field.name) || value[field.name] === undefined) {
      if (field.optional) continue; // JS fixture undefined models an omitted JSON property.
      throw new TypeError(`${path}.${field.name}: missing required field`);
    }
    validateDTO(value[field.name], field.type, schema, `${path}.${field.name}`);
  }
}

const row = (name, display_name, summary, extra = {}) => ({ name, display_name, summary,
  version: '1.2.3-1ubuntu1', arch: 'amd64', suite: 'noble', component: 'universe',
  section: 'utils', categories: ['Utility'], source: 'apt', is_app: !!display_name,
  selected: false, installed_size_bytes: 12480000, download_size_bytes: 4210000, ...extra });
export const PACKAGES = [
  row('gimp', 'GNU Image Manipulation Program', 'Create and edit images'),
  row('inkscape', 'Inkscape', 'Draw and edit vector graphics'),
  row('vlc', 'VLC media player', 'Play audio and video'),
  row('git', '', 'Fast, scalable distributed revision control system'),
  row('curl', '', 'Transfer data with URLs'),
  row('build-essential', '', 'Build tools and development essentials'),
  row('firefox', 'Firefox', 'Transitional package for the Firefox snap', { warnings: [{ kind: 'snap-transitional', package: 'firefox', message: 'This package installs a snap and will not work offline.' }] }),
  row('libexample-dev', '', 'Headers and development files for Example'),
];

// Explicit DTO construction is intentional: PackageRow is not SelectionEntry.
// Do not project arbitrary response objects to hide unexpected fixture fields.
const packageEntry = (p, known = true) => ({ key: p.name, name: p.name, display_name: p.display_name,
  version: p.version, summary: p.summary, source: 'apt', installed_size_bytes: p.installed_size_bytes,
  download_size_bytes: p.download_size_bytes, known, warnings: p.warnings });
const catalogProgress = (generation) => ({ generation, phase: '', phase_index: 0, phase_count: 0,
  label: '', current: 0, total: 0, bytes_done: 0, bytes_total: 0, fraction: -1, overall_fraction: -1, elapsed_ms: 0 });
const buildProgress = () => ({ phase: 'idle', current: 0, total: 0, fraction: -1, bytes_done: 0, bytes_total: 0 });
const idleCopy = () => ({ running: false, finished: false, cancelled: false, verified: false, verify_skipped: false,
  files_checked: 0, bytes_checked: 0, mismatch_count: 0, mismatches_truncated: false,
  progress: { phase: 'idle', fraction: -1, files_done: 0, files_total: 0, bytes_done: 0, bytes_total: 0, bytes_per_sec: 0 } });
const emptySnapshot = () => ({ path: '', distro_id: '', version_id: '', codename: '', arch: '', package_count: 0 });
const emptyPackage = () => ({ name: '', installed_size_bytes: 0, download_size_bytes: 0, is_app: false, selected: false, source: '' });
const emptyPlan = () => ({ source_dir: '', dest_dir: '', total_files: 0, total_bytes: 0, required_bytes: 0,
  free_bytes: 0, margin_bytes: 0, destination_incomplete: false });

export function createFixture({ scenario = 'target', schema, latency = 12 } = {}) {
  if (!schema?.classes || !schema?.methods) throw new Error('A generated bridge schema is required for review fixtures.');
  const calls = [], errors = [], handlers = new Map(), timers = new Set();
  const manySelection = scenario === 'selection-many320';
  const packages = manySelection ? [...PACKAGES, ...Array.from({ length: 320 - PACKAGES.length }, (_, i) =>
    row('fixture-package-' + String(i + 1).padStart(4, '0'), '', 'Additional package for the selection paging review.'))] : PACKAGES;
  const selected = !['target', 'target-missing-cli', 'snapshot-invalid', 'startup'].includes(scenario);
  const withItems = manySelection || /selected|build|copy|undo|close-unsaved/.test(scenario);
  const state = { generation: 1, revision: withItems ? 1 : 0, inputRevision: withItems ? 1 : 0, undo: null, clipboard: '', closed: false,
    target: { selected, kind: 'base', id: 'ubuntu:24.04/desktop', label: 'Ubuntu 24.04 desktop (amd64)',
      distro_id: 'ubuntu', version_id: '24.04', codename: 'noble', variant: 'desktop', arch: 'amd64',
      assumed: true, caveat: CAVEAT, components: ['main', 'universe'], suites: ['noble', 'noble-updates'],
      catalog_ready: selected, generation: 1 },
    entries: withItems ? (manySelection ? packages : packages.slice(0, 3)).map(p => packageEntry(p)) : [],
    catalog: { state: selected ? 'ready' : 'none', target_id: selected ? 'ubuntu:24.04/desktop' : '', ready: selected,
      building: false, stale: false, progress: catalogProgress(1),
      package_count: selected ? packages.length : 0, from_cache: selected, cancelled: false, built_at: selected ? STAMP : '', generation: 1 },
    build: { target_generation: 0, selection_revision: 0, running: false, finished: false, cancelled: false, item_count: 0, event_count: 0, progress: buildProgress() },
    copy: idleCopy(),
    verify: { running: false, finished: false, cancelled: false, ok: false, signed: false },
  };
  function emit(name, payload) {
    // Wails only generates bound types, not every event-only envelope. Validate
    // their generated members as well as directly generated event payloads.
    const direct = { 'app:lifecycle': 'LifecycleView', 'app:error': 'UIError', 'target:changed': 'TargetView',
      'selection:changed': 'SelectionSummary', 'catalog:started': 'CatalogStatus', 'catalog:progress': 'CatalogProgress',
      'build:progress': 'BuildProgress', 'build:event': 'BuildEvent', 'readiness:started': 'ReadinessReport',
      'export:started': 'ExportStatus', 'export:progress': 'ExportProgress', 'verify:started': 'VerifyStatus' };
    const nested = { 'catalog:finished': ['status', 'CatalogStatus'], 'build:finished': ['summary', 'BuildSummary'],
      'readiness:finished': ['report', 'ReadinessReport'], 'export:finished': ['status', 'ExportStatus'], 'verify:finished': ['status', 'VerifyStatus'] };
    try {
      if (direct[name]) validateDTO(payload, direct[name], schema, name);
      const member = nested[name];
      if (member && payload[member[0]] !== undefined) validateDTO(payload[member[0]], member[1], schema, `${name}.${member[0]}`);
      if (member && payload.error !== undefined) validateDTO(payload.error, 'UIError', schema, `${name}.error`);
    } catch (failure) { errors.push(String(failure.message || failure)); throw failure; }
    for (const cb of handlers.get(name) || []) cb(clone(payload));
  }
  function later(fn, ms = 150) { const t = setTimeout(() => { timers.delete(t); fn(); }, ms); timers.add(t); }
  function summary(extra = {}) {
    return { total: state.entries.length, package_count: state.entries.filter(e => e.source === 'apt').length,
      url_count: state.entries.filter(e => e.source === 'url').length, file_count: state.entries.filter(e => e.source === 'file').length,
      unknown_count: state.entries.filter(e => e.source === 'apt' && !e.known).length,
      installed_size_bytes: state.entries.reduce((n,e) => n + (e.installed_size_bytes || 0), 0),
      download_size_bytes: state.entries.reduce((n,e) => n + (e.download_size_bytes || 0), 0),
      warnings: state.entries.flatMap(e => e.warnings || []), rejected: [], added: 0, removed: 0,
      revision: state.revision, undo_token: state.undo?.token || '', undo_label: state.undo?.label || '', ...extra };
  }
  function lifecycle() { return { build_running: state.build.running, export_running: state.copy.running,
    stopping: false, unsaved_selection: state.entries.length > 0 && !(state.build.finished && state.build.summary?.exit_class === 'success' && state.build.target_generation === state.generation && state.build.selection_revision === state.inputRevision),
    target_generation: state.generation, selection_revision: state.inputRevision }; }
  function busy() { return state.build.running || state.copy.running; }
  function publicEntry(entry) {
    const out = clone(entry);
    if (entry.source === 'url') {
      const url = new URL(entry.url);
      if (url.username || url.password || url.search) {
        out.key = 'url:fixture-opaque-vendor-reference';
        url.username = ''; url.password = ''; url.search = '';
        out.url = url.toString();
      }
    }
    return out;
  }
  function parseNames(text) {
    const inputs = String(text).split(/\r?\n/).map(line => line.split('#')[0].trim()).filter(Boolean);
    const rejected = inputs.filter(name => !/^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9-]+)?$/.test(name)).map(value => ({ value, reason: 'Use a package name, one per line.' }));
    const names = [...new Set(inputs.filter(name => !rejected.some(r => r.value === name)))];
    const known = names.map(name => packages.find(p => p.name === name)).filter(Boolean);
    return { names, result: { count: names.length, new_count: names.filter(name => !state.entries.some(e => e.key === name)).length,
      known_count: known.length, unknown: names.filter(name => !packages.some(p => p.name === name)), unknown_count: names.length - known.length,
      duplicates: inputs.length - rejected.length - names.length, sample: known, rejected, warnings: known.flatMap(p => p.warnings || []) } };
  }
  function mutated(extra) { const s = summary(extra); emit('selection:changed', s); emit('app:lifecycle', lifecycle()); return s; }
  function mutate(fn, undoLabel = '') {
    if (busy()) return summary({ error: error('app.busy', 'A job is using this selection.', 'Wait for the job or cancel it before editing.') });
    const before = clone(state.entries), result = fn();
    if (JSON.stringify(before.map(e => e.key)) !== JSON.stringify(state.entries.map(e => e.key))) state.revision += 1;
    if (JSON.stringify(before) !== JSON.stringify(state.entries)) state.inputRevision += 1;
    state.undo = undoLabel ? { token: `undo-${state.inputRevision}`, label: undoLabel, entries: before } : null;
    return mutated(result);
  }
  function readiness() {
    const missing = scenario === 'target-missing-cli' || scenario === 'readiness-blocked';
    return { checks: [
      { id: 'debark-binary', title: 'debark command', status: missing ? 'problem' : 'ok', severity: missing ? 'blocking' : 'info',
        summary: missing ? 'The debark command is not installed.' : 'debark is available.', remedy: missing ? 'Install debark and check again.' : '', running: false, derived: false, duration_ms: 2 },
      { id: 'signing-key', title: 'Signing key', status: 'problem', severity: 'info', summary: 'Choose a signing key before building.',
        remedy: 'Choose an existing key or create one.', action: { label: 'Create key', command: ['debark', 'keygen'], display: 'debark keygen', elevated: false, runnable: true }, running: false, derived: false, duration_ms: 1 },
    ], can_build: !missing, blocking_count: missing ? 1 : 0, degraded_count: 0, platform: 'linux/amd64', checking: false, checked_at: STAMP, duration_ms: 3 };
  }
  const builtSummary = (ending = 'signed') => ({ bundle_path: '/home/operator/Bundles/ubuntu-desktop', bundle_id: 'desktop-20260907-120000',
    lock_ref: 'sha256:' + DIGEST, manifest_ref: 'sha256:' + DIGEST, signed: ending !== 'unsigned',
    stats: { package_count: 42, bytes: 86400000, downloaded_bytes: 86400000, added: 42, removed: 0, unchanged: 0, duration_seconds: 2.4 },
    warnings: ending === 'incomplete' ? ['A required file could not be downloaded.'] : [],
    unresolved: [], fetch_failed: ending === 'incomplete' ? ['vendor-agent.deb'] : [], truncated: false,
    exit_class: ending === 'incomplete' ? 'incomplete' : 'success' });
  function finishBuild(ending = 'signed') {
    state.build.running = false; state.build.finished = true; state.build.finished_at = STAMP;
    state.build.cancelled = ending === 'cancelled';
    state.build.progress = { ...state.build.progress, phase: ending === 'cancelled' ? 'cancelled' : 'finished', fraction: 1 };
    if (ending === 'failed') state.build.error = error('cli.failed', 'The signing key could not be read.', 'Choose another key and try again.');
    else if (ending !== 'cancelled') state.build.summary = builtSummary(ending);
    emit('build:finished', { ok: !state.build.error && !state.build.cancelled, cancelled: state.build.cancelled, summary: state.build.summary, error: state.build.error, duration_ms: 2400 });
    emit('app:lifecycle', lifecycle());
  }
  function exportFinish(ending = 'verified') {
    Object.assign(state.copy, { running: false, finished: true, cancelled: ending === 'cancelled',
      verified: ending === 'verified', verify_skipped: ending === 'skipped', finished_at: STAMP,
      summary: ending === 'verified' ? 'The copied files match the source bundle.' : 'This copy has not been verified.',
      verify_method: 'SHA-256 comparison of copied files against source files.',
      verify_caveat: 'Copy verification does not establish trust in the publisher.', files_checked: ending === 'verified' ? 48 : 0,
      bytes_checked: ending === 'verified' ? 86400000 : 0 });
    state.copy.progress.phase = ending === 'cancelled' ? 'cancelled' : ending === 'mismatch' ? 'failed' : 'finished';
    if (ending === 'mismatch') Object.assign(state.copy, { error: error('verify.failed', 'The copied bundle does not match.', 'Copy it again to a reliable drive.'), mismatch_count: 1,
      mismatches: [{ path: 'pool/gimp.deb', reason: 'content', detail: 'SHA-256 digest mismatch.', want_bytes: 4210000, got_bytes: 4210000, want_sha256: DIGEST, got_sha256: '7b'.repeat(32) }] });
    if (ending === 'removed') state.copy.error = error('export.failed', 'The destination drive was removed.', 'Reconnect the drive and copy the bundle again.');
    emit('export:finished', { ok: state.copy.verified, cancelled: state.copy.cancelled, status: state.copy, error: state.copy.error, duration_ms: 900 });
    emit('app:lifecycle', lifecycle());
  }
  if (/build-(signed|unsigned|incomplete|failed|cancelled)|copy/.test(scenario) && !scenario.startsWith('copy-choose')) {
    Object.assign(state.build, { target_id: state.target.id, item_count: 3, started_at: STAMP, target_generation: 1, selection_revision: 1 });
    finishBuild(scenario.startsWith('build-') ? scenario.slice(6) : 'signed');
  }
  if (scenario === 'build-running') Object.assign(state.build, { running: true, target_id: state.target.id, item_count: 3, started_at: new Date(Date.now() - 8000).toISOString(), target_generation: 1, selection_revision: 1,
    progress: { phase: 'downloading', message: 'Downloading packages', package: 'gimp', current: 18, total: 42, fraction: 0.42, bytes_done: 36288000, bytes_total: 86400000 } });
  if (scenario.startsWith('copy-choose')) {
    state.target.selected = false; state.target.catalog_ready = false; state.entries = [];
    state.revision = 0; state.inputRevision = 0;
    Object.assign(state.catalog, { state: 'none', target_id: '', ready: false, package_count: 0, built_at: '', from_cache: false });
  }
  if (/catalog-(loading|failed|cancelled|missing)/.test(scenario)) {
    const phase = scenario.slice(8); Object.assign(state.catalog, { ready: false, building: phase === 'loading', state: phase === 'loading' ? 'building' : phase === 'failed' ? 'failed' : phase === 'cancelled' ? 'cancelled' : 'not-built', cancelled: phase === 'cancelled', package_count: 0, from_cache: false, built_at: '',
      progress: { ...catalogProgress(1), phase: 'download', phase_index: 2, phase_count: 7, label: 'Downloading package indexes', item: 'noble/universe', fraction: 0.8, overall_fraction: 0.4 },
      error: phase === 'failed' ? error('catalog.failed', 'Package indexes could not be downloaded.', 'Check the connection, then retry.') : undefined });
    state.target.catalog_ready = false;
  }
  if (/copy-(verified|skipped|mismatch|cancelled|removed)/.test(scenario)) {
    Object.assign(state.copy, { bundle_path: state.build.summary.bundle_path, destination: '/media/operator/TRANSFER', destination_path: '/media/operator/TRANSFER/ubuntu-desktop', started_at: STAMP });
    exportFinish(scenario.slice(5));
  }
  const implementations = {
    AppInfo: () => ({ name: 'Debark', version: 'ui-review-fixture', platform: 'linux', arch: 'amd64', debark_path: '/usr/bin/debark', debark_version: 'fixture', progress_events: true, headless: false }),
    Readiness: readiness, StartReadinessCheck: () => { later(() => emit('readiness:finished', { ok: true, cancelled: false, action: false, report: readiness() })); return { ok: true }; },
    RecheckReadiness: () => implementations.StartReadinessCheck(),
    RunReadinessAction: (checkID) => { later(() => emit('readiness:finished', { ok: true, cancelled: false, action: true, check_id: checkID, report: readiness() })); return { ok: true }; },
    CancelReadinessCheck: () => ({ ok: true }),
    SupportedArchitectures: () => ({ arches: ['amd64', 'arm64'], default: 'amd64' }),
    ListBases: (arch) => ({ arch, caveat: CAVEAT, bases: [ ['debian','12','bookworm'], ['debian','13','trixie'], ['ubuntu','22.04','jammy'], ['ubuntu','24.04','noble'], ['ubuntu','26.04','resolute'] ].flatMap(([distro_id,version_id,codename]) => ['minimal','server','desktop'].map(variant => ({
      id: `${distro_id}:${version_id}/${variant}`, description: `${distro_id === 'ubuntu' ? 'Ubuntu' : 'Debian'} ${version_id} ${variant}`, distro_id, version_id, codename, variant, arch,
      seeds: [distro_id === 'ubuntu' ? `ubuntu-${variant}-minimal` : 'apt'], excludes: [], recommends: false, digest: DIGEST }))) }),
    CurrentTarget: () => ({ target: state.target }),
    SelectTarget: (value) => {
      if (busy()) return { target: state.target, error: error('app.busy', 'A job is using this target.', 'Wait for the job before changing target.') };
      const changed = state.target.id !== (value.base_id || 'snapshot-site-01') || state.target.arch !== (value.arch || 'amd64') || state.target.kind !== value.kind || !state.target.selected;
      if (changed) state.generation += 1;
      state.target = { ...state.target, selected: true, id: value.base_id || 'snapshot-site-01', arch: value.arch || 'amd64', kind: value.kind, generation: state.generation };
      if (changed) {
        state.target.catalog_ready = false;
        state.catalog = { generation: state.generation, state: 'not-built', target_id: state.target.id, ready: false,
          building: false, stale: false, progress: catalogProgress(state.generation), package_count: 0, from_cache: false, cancelled: false };
      }
      state.undo = null;
      emit('target:changed', state.target); emit('app:lifecycle', lifecycle()); return { target: state.target };
    },
    ClearTarget: () => {
      if (busy()) return { ok: false, error: error('app.busy', 'A job is using this target.', 'Wait for the job before changing target.') };
      state.generation++; state.target = { generation: state.generation, selected: false, assumed: false, catalog_ready: false };
      state.catalog = { generation: state.generation, state: 'none', ready: false, building: false, stale: false,
        progress: catalogProgress(state.generation), package_count: 0, from_cache: false, cancelled: false };
      state.undo = null; emit('target:changed', state.target); emit('app:lifecycle', lifecycle()); return { ok: true };
    },
    ChooseSnapshotFile: () => ({ path: '/media/operator/TRANSFER/site-01.snapshot.tar.zst', paths: [], cancelled: scenario === 'snapshot-cancelled' }),
    InspectSnapshot: (path) => scenario === 'snapshot-invalid' ? { snapshot: emptySnapshot(), error: error('target.invalid', 'This snapshot could not be read.', 'Choose a valid snapshot file.') } : ({ snapshot: { path, schema_version: 'debark.snapshot/v1', distro_id: 'ubuntu', version_id: '24.04', codename: 'noble', arch: 'amd64', origin_kind: 'captured', created_at: STAMP, package_count: 1847, foreign_archs: [] } }),
    CatalogStatus: () => state.catalog,
    StartCatalogBuild: (force = false) => { if (busy()) return { ok: false, error: error('app.busy', 'Another job is running.', 'Wait for it to finish.') };
      if (state.catalog.building || (state.catalog.ready && !force)) return { ok: true };
      if (!state.target.selected) return { ok: false, error: error('app.invalid_input', 'Choose a target first.', 'Choose a standard installation or snapshot.') };
      const generation = state.generation; state.catalog.building = true; state.catalog.state = 'building'; state.catalog.cancelled = false; state.catalog.error = undefined; emit('catalog:started', state.catalog);
      later(() => { if (generation !== state.generation || !state.catalog.building) return; Object.assign(state.catalog, { ready: true, building: false, state: 'ready', package_count: packages.length, built_at: STAMP }); state.target.catalog_ready = true;
        emit('catalog:finished', { ok: true, cancelled: false, status: state.catalog, duration_ms: 150 }); }, 400); return { ok: true }; },
    CancelCatalogBuild: () => { Object.assign(state.catalog, { building: false, cancelled: true, state: 'cancelled' }); emit('catalog:finished', { ok: false, cancelled: true, status: state.catalog, duration_ms: 100 }); return { ok: true }; },
    Categories: () => ({ categories: [{ id: 'section:utils', tier: 'section', name: 'Utilities', count: packages.length }], total: packages.length }),
    SearchPackages: (query = {}) => { const text = (query.text || '').toLowerCase(), offset = query.offset || 0, limit = Math.min(query.limit || 50, 500);
      const matches = packages.filter(p => (!text || `${p.name} ${p.display_name} ${p.summary}`.toLowerCase().includes(text)) && (!query.apps_only || p.is_app));
      return { query: { ...query, text: query.text || '', offset, limit }, rows: matches.slice(offset, offset+limit).map(p => ({ ...p, selected: state.entries.some(e => e.key === p.name) })), total: matches.length, offset, limit, truncated: false, took_ms: 1 }; },
    GetPackage: (name) => { const p = packages.find(p => p.name === name); return { found: !!p, package: p ? { ...p,
      selected: state.entries.some(e => e.key === name), description: 'Create and edit images with layers, painting tools and filters.\n\nThis extended description comes from the fixture package metadata.', description_truncated: false } : emptyPackage() }; },
    PackageRows: (names) => ({ rows: packages.filter(p => names.includes(p.name)), missing: names.filter(n => !packages.some(p => p.name === n)) }),
    Selection: summary, SelectionKeys: () => ({ keys: state.entries.map(e => publicEntry(e).key), total: state.entries.length, revision: state.revision, truncated: false }),
    SelectionPage: (offset = 0, limit = 200) => ({ entries: state.entries.slice(offset, offset + Math.min(limit,500)).map(publicEntry), total: state.entries.length, offset, limit }),
    AddPackages: (names) => mutate(() => { let added = 0; for (const name of names) if (!state.entries.some(e => e.key === name)) { const p = packages.find(p => p.name === name); state.entries.push(packageEntry(p || { name, summary: 'Not in this catalogue', installed_size_bytes: 0, download_size_bytes: 0 }, !!p)); added++; } return { added }; }),
    RemovePackages: (keys) => mutate(() => { const count = state.entries.length; state.entries = state.entries.filter(e => !keys.includes(publicEntry(e).key)); return { removed: count - state.entries.length }; }, 'Undo removal'),
    ClearSelection: () => mutate(() => { const removed = state.entries.length; state.entries = []; return { removed }; }, 'Undo clear'),
    UndoSelection: (token) => { if (!state.undo || state.undo.token !== token) return summary({ error: error('selection.undo_expired', 'This Undo is no longer available.', 'Add the packages again.') });
      if (busy()) return summary({ error: error('app.busy', 'A job is using this selection.', 'Wait for it to finish.') });
      state.entries = clone(state.undo.entries); state.revision++; state.inputRevision++; state.undo = null; return mutated({}); },
    ParsePackageList: (text) => parseNames(text).result,
    AddPackageList: (text) => { const parsed = parseNames(text); return { ...implementations.AddPackages(parsed.names), rejected: parsed.result.rejected }; },
    AddURLs: (inputs) => mutate(() => { let added = 0; for (const { url, sha256 = '' } of inputs) {
      const existing = state.entries.find(e => e.key === url);
      if (existing) existing.sha256 = sha256;
      else { state.entries.push({ key: url, url, name: new URL(url).pathname.split('/').pop(), source: 'url', known: false, sha256, installed_size_bytes: 0, download_size_bytes: 0,
        warnings: [{ kind: 'external-url', message: 'Vendor download outside the target archive. Check its source and redistribution terms.' }] }); added++; }
    } return { added }; }),
    AddLocalDebs: (paths) => mutate(() => { for (const path of paths) state.entries.push({ key: path, path, name: path.split('/').pop(), source: 'file', known: true, installed_size_bytes: 0, download_size_bytes: 0 }); return { added: paths.length }; }),
    ChooseLocalDebs: () => ({ paths: ['/home/operator/vendor/agent.deb', '/home/operator/vendor/support.deb'], path: '', cancelled: false }),
    ChooseDirectory: (title) => ({ path: /bundle.*copy|source|existing/i.test(title || '') ? '/home/operator/Bundles/existing-bundle' : /copy|drive|destination/i.test(title || '') ? '/media/operator/TRANSFER' : '/home/operator/Bundles', paths: [], cancelled: scenario === 'copy-choose-cancelled' }),
    ChooseSigningKey: () => ({ path: '/home/operator/.config/debark/operator.key', paths: [], cancelled: false }),
    PreviewCommand: (options = {}) => { if (!options.output_dir) return { argv: [], display: '', error: error('app.invalid_input', 'Choose an output folder.', 'Choose where the bundle should be saved.') };
      if (!options.no_sign && !options.signer_ref) return { argv: [], display: '', error: error('app.invalid_input', 'Choose a signing key.', 'Choose a key or explicitly choose an unsigned bundle.') };
      const argv = ['debark','build','--base',state.target.id,'--out', `${options.output_dir}/${options.output_name || 'automatic-name'}`, ...(options.no_sign ? ['--no-sign'] : ['--sign',options.signer_ref]),'--', ...state.entries.map(e => e.name)]; return { argv, display: argv.join(' ') }; },
    BuildStatus: () => state.build, BuildLog: () => ({ events: [], next_seq: 0, total: 0, dropped: 0 }),
    StartBuild: (options) => { const preview = implementations.PreviewCommand(options); if (preview.error) return { ok: false, error: preview.error };
      if (!options.acknowledge_redistribution) return { ok: false, error: error('app.invalid_input', 'Confirm the redistribution acknowledgement.', 'Review the acknowledgement before building.') };
      if (busy()) return { ok: false, error: error('app.busy', 'Another job is running.', 'Wait for it to finish.') };
      state.build = { running: true, finished: false, cancelled: false, command: preview.argv, item_count: state.entries.length, target_id: state.target.id, target_generation: state.generation, selection_revision: state.inputRevision,
        started_at: STAMP, event_count: 0, progress: { ...buildProgress(), phase: 'resolving' } };
      emit('build:started', { target_generation: state.generation, selection_revision: state.inputRevision, command: preview.argv,
        target_id: state.target.id, item_count: state.entries.length, started_at: STAMP, output_dir: options.output_dir }); emit('app:lifecycle', lifecycle()); return { ok: true }; },
    CancelBuild: () => { if (state.build.running) finishBuild('cancelled'); return { ok: true }; },
    ListVolumes: () => ({ supported: true, fingerprint: 'fixture-volume-v1', volumes: [{ path: '/media/operator/TRANSFER', label: 'TRANSFER', kind: 'removable', removable: true, writable: true, fs_type: 'ext4', free_bytes: 64000000000, total_bytes: 128000000000, read_only: false }] }),
    InspectDestination: (opts) => ({ destination: opts.destination, path: `${opts.destination}/ubuntu-desktop`, exists: scenario === 'copy-incomplete', incomplete: scenario === 'copy-incomplete', marker_name: '.debark-incomplete', message: scenario === 'copy-incomplete' ? 'An interrupted copy is here. Do not use it until copying completes and verifies.' : '' }),
    PlanExport: (opts) => scenario === 'copy-no-space' ? { plan: emptyPlan(), error: error('export.failed', 'There is not enough free space.', 'Choose a destination with at least 95 MB available.') } : ({ plan: { source_dir: opts.bundle_path || state.build.summary?.bundle_path || '', dest_dir: `${opts.destination}/${opts.subdir || 'ubuntu-desktop'}`, total_files: 48, total_bytes: 86400000, required_bytes: 95040000, free_bytes: scenario === 'copy-unknown-space' ? 0 : 64000000000, margin_bytes: 8640000, warnings: scenario === 'copy-unknown-space' ? ['Free space could not be measured. Copying will stop if the destination fills.'] : [], destination_incomplete: scenario === 'copy-incomplete' } }),
    ExportStatus: () => state.copy, StartExport: (opts) => { const plan = implementations.PlanExport(opts); if (plan.error) return { ok: false, error: plan.error }; if (busy()) return { ok: false, error: error('app.busy', 'Another job is running.', 'Wait for it to finish.') };
      state.copy = { ...idleCopy(), running: true, bundle_path: opts.bundle_path || state.build.summary?.bundle_path, destination: opts.destination, destination_path: plan.plan.dest_dir, verify_skipped: !!opts.skip_verify, started_at: STAMP }; state.copy.progress.phase = 'copying'; emit('export:started', state.copy); emit('app:lifecycle', lifecycle()); return { ok: true }; },
    CancelExport: () => { if (state.copy.running) exportFinish('cancelled'); return { ok: true }; },
    VerifyStatus: () => state.verify,
    StartVerify: (path) => {
      if (state.verify.running || busy()) return { ok: false, error: error('app.busy', 'Another job is running.', 'Wait for it to finish.') };
      state.verify = { running: true, finished: false, cancelled: false, bundle_path: path, ok: false, signed: false, started_at: STAMP };
      const current = state.verify;
      later(() => { if (state.verify !== current || !current.running) return;
        Object.assign(current, { running: false, finished: true, ok: true, signed: true, finished_at: STAMP, exit_class: 'success' });
        emit('verify:finished', { ok: true, cancelled: false, status: current, duration_ms: 150 }); });
      return { ok: true };
    },
    CancelVerify: () => { if (state.verify.running) {
      Object.assign(state.verify, { running: false, finished: true, cancelled: true, finished_at: STAMP });
      emit('verify:finished', { ok: false, cancelled: true, status: state.verify, duration_ms: 0 });
    } return { ok: true }; },
    CopyToClipboard: (value) => { state.clipboard = value; return { ok: true }; }, RevealPath: () => ({ ok: true }), LifecycleStatus: lifecycle,
    RequestClose: () => { if (!busy() && !lifecycle().unsaved_selection) state.closed = true; return { ok: true }; },
  };
  const bindings = new Proxy({}, { get(_target, name) {
    if (name === 'then') return undefined;
    if (typeof name !== 'string') return undefined;
    if (!schema.methods[name]) { const message = `Undeclared binding ${name}`; errors.push(message); throw new Error(message); }
    if (!implementations[name]) { const message = `Missing fixture implementation ${name}`; errors.push(message); throw new Error(message); }
    return async (...args) => {
      calls.push({ name, args: clone(args) });
      if (latency) await new Promise(r => setTimeout(r, latency));
      try {
        const result = implementations[name](...args);
        validateDTO(result, schema.methods[name].ret, schema, name);
        return clone(result);
      } catch (failure) { errors.push(String(failure.message || failure)); throw failure; }
    };
  } });
  const runtime = { on(name, cb) { if (!handlers.has(name)) handlers.set(name,new Set()); handlers.get(name).add(cb); return () => handlers.get(name)?.delete(cb); }, off(name) { handlers.delete(name); } };
  return { bindings, runtime, state, calls, errors, emit, summary, lifecycle, finishBuild, exportFinish,
    destroy() { for (const t of timers) clearTimeout(t); timers.clear(); handlers.clear(); } };
}
