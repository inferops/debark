/*
 * hostile.js — the fixture generator for the XSS pass.
 *
 * It reads the Wails-generated bridge description (every view class, every
 * field, every method's return type) and produces a `window.go.app.App` whose
 * every free-text field carries an injection payload. That is the point: a
 * hand-written fixture poisons the four fields the author thought of, and this
 * one poisons all 300-odd strings the bridge can carry.
 */
import { createFixture } from '../../../hack/ui-review/fixtures.mjs';

// Each payload, if it is ever parsed as markup rather than escaped as text,
// calls window.__xss(tag). Nothing else in the page defines an element with
// these tag names, so their mere presence in the DOM is also proof.
export const PAYLOADS = {
  // 1. a package Description with an image error handler
  desc:
    '<img src=x onerror="window.__xss(\'desc-img\')">DESCRIPTION_PAYLOAD' +
    '<svg/onload="window.__xss(\'desc-svg\')">',
  // 2. a volume label carrying a quote and an angle bracket
  label: 'MY USB" onmouseover="window.__xss(\'label-attr\')" x=\'<b>LABEL_PAYLOAD</b>',
  // 3. a stderr blob that closes a script element
  stderr:
    'debark: fatal\n</script><script>window.__xss("stderr-script")</script>\n' +
    "</textarea></title></style><img src=x onerror=window.__xss('stderr-img')>",
  // 4. a vendor URL with a javascript: scheme, and its friends
  url: 'javascript:window.__xss("url-js")//https://vendor.example/a.deb',
  dataurl: 'data:text/html;base64,PHNjcmlwdD53aW5kb3cuX194c3MoImRhdGEtdXJsIik8L3NjcmlwdD4=',
  // 5. a file path that is also markup
  path: '/mnt/<iframe src="javascript:window.__xss(\'path-iframe\')"></iframe>/bundle',
  // 6. CSS injection through a style-ish value: an off-origin beacon
  css: 'red; background-image: url("http://127.0.0.1:7299/css-beacon.png")',
  // 7. an off-origin image beacon, in case something builds an <img src>
  beacon: 'http://127.0.0.1:7299/src-beacon.png',
  // 8. structural noise: bidi override, and a very long run
  weird: 'A B‮C DE' + 'x'.repeat(4000),
};

// Fields the screens branch on. Poisoning these makes the screen render its
// "unknown" path and never reach the interesting markup, so in `realistic`
// mode they get a legal value and every other string gets a payload.
const ENUMS = {
  kind: 'removable',
  state: 'ready',
  phase: 'downloading',
  status: 'problem',
  severity: 'blocking',
  tier: 'application',
  source: 'url',
  exit_class: 'verification',
  origin_kind: 'captured',
  level: 'warning',
  format: 'dir',
  backend: 'auto',
  fs_type: 'vfat',
  arch: 'amd64',
  id: 'debark-binary',
  code: 'cli.verification',
  key: 'vendor-agent',
  type: 'fetch.file',
};

// Field names that must stay numeric-ish or the render short-circuits.
const NUMBERS = {
  fraction: 0.42,
  total: 60000,
  count: 3,
  offset: 0,
  limit: 50,
};

function payloadFor(field) {
  const n = field.toLowerCase();
  if (n.includes('url')) return PAYLOADS.url;
  if (n.includes('path') || n.includes('dir') || n.includes('file') || n.includes('device')) return PAYLOADS.path;
  if (n.includes('label')) return PAYLOADS.label;
  if (n.includes('detail') || n.includes('stderr') || n.includes('log') || n.includes('line')) return PAYLOADS.stderr;
  if (n.includes('icon')) return PAYLOADS.beacon;
  if (n.includes('colour') || n.includes('color') || n.includes('style')) return PAYLOADS.css;
  return PAYLOADS.desc;
}

export function buildValue(bridge, type, field, mode, depth) {
  if (depth > 4) return null;
  const arr = type.match(/^(.*)\[\]$/) || type.match(/^Array<(.*)>$/);
  if (arr) {
    const inner = arr[1].trim();
    const n = inner === 'string' ? 3 : 2;
    const out = [];
    for (let i = 0; i < n; i += 1) out.push(buildValue(bridge, inner, field, mode, depth + 1));
    return out;
  }
  if (type.startsWith('Record<')) return { [PAYLOADS.desc]: PAYLOADS.stderr, ok: PAYLOADS.label };
  if (type === 'number') {
    for (const k in NUMBERS) if (field.toLowerCase().includes(k)) return NUMBERS[k];
    return 1234567;
  }
  if (type === 'boolean') return !/^(?:running|checking|building|build_running|export_running|stopping|cancelled|truncated|read_only|elevated)$/.test(field);
  if (type === 'any') return PAYLOADS.desc;
  if (type === 'string') {
    if (mode !== 'poison' && Object.prototype.hasOwnProperty.call(ENUMS, field)) return ENUMS[field];
    // 'control' is the positive control: the SAME fields carry a legitimate
    // https URL, so a run that finds no anchor at all cannot be mistaken for a
    // run in which the javascript: URL was refused.
    if (mode === 'control' && field.toLowerCase().includes('url')) {
      return 'https://vendor.example/pool/CONTROL_URL.deb';
    }
    return payloadFor(field);
  }
  const cls = bridge.classes[type];
  if (!cls) return PAYLOADS.desc;
  return buildClass(bridge, type, mode, depth + 1);
}

export function buildClass(bridge, name, mode, depth = 0) {
  const fields = bridge.classes[name];
  if (!fields) return {};
  const out = {};
  for (const f of fields) out[f.name] = buildValue(bridge, f.type, f.name, mode, depth);
  // An `error` field set on every result would make every screen render its
  // error state and nothing else, so the happy path is exercised too.
  if ('error' in out && mode !== 'poison') out.error = null;
  if ('ok' in out) out.ok = true;
  if ('cancelled' in out) out.cancelled = false;
  if ('found' in out) out.found = true;
  if ('ready' in out) out.ready = true;
  if ('running' in out) out.running = false;
  if ('finished' in out) out.finished = true;
  return out;
}

// Reuse the tested fixture state machine. Its legal booleans, revisions,
// counts and identities let the operator journeys reach actual rendering.
// Only string presentation is hostile. The generated schema still decides
// which fields cross the bridge; no successful fallback for missing methods.
const IDENTITY = new Set(['id', 'key', 'name', 'keys', 'kind', 'state', 'phase',
  'status', 'severity', 'tier', 'source', 'exit_class', 'origin_kind', 'level',
  'format', 'backend', 'fs_type', 'arch', 'arches', 'default', 'distro_id',
  'version_id', 'codename', 'variant', 'schema_version', 'type', 'code',
  'target_id', 'target_kind', 'check_id', 'running_check', 'undo_token',
  'started_at', 'finished_at', 'created_at', 'checked_at', 'built_at', 'updated_at',
  'marker_name', 'bundle_id', 'section', 'categories']);
const ENUM_FIELDS = new Set(['kind', 'state', 'phase', 'status', 'severity',
  'tier', 'source', 'exit_class', 'origin_kind', 'level', 'type', 'code']);
export const EVENT_CLASSES = {
  'app:lifecycle': 'LifecycleView', 'app:error': 'UIError',
  'readiness:started': 'ReadinessReport', 'readiness:progress': 'ReadinessProgress', 'readiness:finished': 'ReadinessFinished',
  'target:changed': 'TargetView', 'catalog:started': 'CatalogStatus', 'catalog:progress': 'CatalogProgress', 'catalog:finished': 'CatalogFinished',
  'selection:changed': 'SelectionSummary', 'build:started': 'BuildStarted', 'build:progress': 'BuildProgress', 'build:event': 'BuildEvent', 'build:finished': 'BuildFinished',
  'export:started': 'ExportStatus', 'export:progress': 'ExportProgress', 'export:finished': 'ExportFinished',
  'verify:started': 'VerifyStatus', 'verify:finished': 'VerifyFinished',
};

export function makeBindings(bridge, mode, record = () => {}) {
  if (!['realistic', 'poison', 'control'].includes(mode)) throw new Error('Unknown hostile mode: ' + mode);
  const fixture = createFixture({ scenario: 'picker-selected', schema: bridge, latency: 0 });
  // A selected-row warning exercises both a rejected javascript: link and the
  // same positive-control https link through the real visible Details action.
  fixture.state.entries[0].warnings = [{ kind: 'external-url', package: 'gimp',
    message: PAYLOADS.desc, hint: PAYLOADS.stderr, doc_url: PAYLOADS.url }];
  const projectedFields = new Set();
  const handlers = new Map();
  const projectionMode = mode === 'control' ? 'control' : 'realistic';
  const strip = type => type.replace(/^app\./, '');
  function project(type, value, field = '', currentMode = projectionMode, trail = '') {
    if (value == null) return value;
    type = strip(type);
    // This is an echoed request identity, checked by the virtualizer before
    // accepting a page. Poisoning it tests stale-response rejection only.
    if (type === 'SearchQuery') return structuredClone(value);
    const array = type.match(/^(.*)\[\]$/) || type.match(/^Array<(.*)>$/);
    if (array) return value.map((item, index) => project(array[1], item, field, currentMode, `${trail}[${index}]`));
    if (type === 'string') {
      if (IDENTITY.has(field) && !(currentMode === 'poison' && ENUM_FIELDS.has(field))) return value;
      if (!value) return value;
      projectedFields.add(trail);
      return currentMode === 'control' && /url|homepage/i.test(field)
        ? 'https://vendor.example/pool/CONTROL_URL.deb' : payloadFor(field);
    }
    if (type === 'number' || type === 'boolean') return value;
    const fields = bridge.classes[type];
    // Wails generates bound result types, not every event-only envelope.
    // Preserve its legal shape while poisoning nested presentation strings.
    if (!fields) {
      if (Array.isArray(value)) return value.map((item, index) => project(typeof item, item, field, currentMode, `${trail}[${index}]`));
      if (typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) =>
        [key, project(typeof item, item, key, currentMode, trail + '.' + key)]));
      return value;
    }
    const result = {};
    for (const f of fields) if (Object.prototype.hasOwnProperty.call(value, f.name)) {
      result[f.name] = project(f.type, value[f.name], f.name, currentMode, trail + '.' + f.name);
    }
    return result;
  }
  function deliver(name, value, currentMode = projectionMode) {
    const cls = EVENT_CLASSES[name];
    if (!cls) throw new Error('Unknown event schema ' + name);
    const payload = project(cls, value, '', currentMode, name);
    for (const cb of handlers.get(name) || []) cb(payload);
  }
  const runtime = {
    on(name, cb) {
      if (!handlers.has(name)) {
        handlers.set(name, new Set());
        fixture.runtime.on(name, value => deliver(name, value));
      }
      handlers.get(name).add(cb);
      return () => handlers.get(name)?.delete(cb);
    },
    off(name) { handlers.delete(name); fixture.runtime.off(name); },
  };
  const bindings = new Proxy(Object.create(null), {
    get(_target, name) {
      if (name === 'then') return undefined;
      if (typeof name !== 'string') return undefined;
      const signature = bridge.methods[name];
      if (!signature) throw new Error('Undeclared hostile binding ' + name);
      return async (...args) => {
        record(name, args);
        const result = await fixture.bindings[name](...args);
        return project(signature.ret, result, '', projectionMode, name);
      };
    },
  });
  return {
    bindings, runtime, projectedFields, errors: fixture.errors,
    emitError() { deliver('app:error', buildClass(bridge, 'UIError', projectionMode)); },
    poisonEvents() {
      if (mode !== 'poison') return false;
      // Deliberately invalid enum/error data reaches error paths; then a real
      // status projection recovers edit availability. Do not mutate shell or
      // screen state and do not leave every lifecycle boolean set to true.
      deliver('app:error', buildClass(bridge, 'UIError', 'poison'), 'poison');
      deliver('app:lifecycle', { ...fixture.lifecycle(), error: buildClass(bridge, 'UIError', 'poison') }, 'poison');
      deliver('target:changed', fixture.state.target, 'poison');
      return true;
    },
    recover() {
      deliver('target:changed', fixture.state.target);
      deliver('selection:changed', fixture.summary());
      deliver('app:lifecycle', fixture.lifecycle());
    },
    finishBuild() { fixture.finishBuild('signed'); },
    destroy() { fixture.destroy(); handlers.clear(); },
  };
}
