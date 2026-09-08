import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import { createFixture, DIGEST, parseBridgeSchema, validateDTO } from './fixtures.mjs';

const root = new URL('../../', import.meta.url);
const models = readFileSync(new URL('frontend/wailsjs/go/models.ts', root), 'utf8');
const declarations = readFileSync(new URL('frontend/wailsjs/go/app/App.d.ts', root), 'utf8');
const schema = parseBridgeSchema(models, declarations);
// The WebKit server and Node/hostile consumers must read the same generated DTOs.
const servedSchema = JSON.parse(execFileSync(process.env.PYTHON || 'python', ['-c',
  'import json, pathlib, runpy, sys; module = runpy.run_path(sys.argv[1]); print(json.dumps(module["bridge_schema"](pathlib.Path(sys.argv[2]))))',
  fileURLToPath(new URL('server.py', import.meta.url)), fileURLToPath(root)], { encoding: 'utf8' }));
assert.deepEqual(schema.classes, servedSchema.classes);
assert.deepEqual(schema.methods, servedSchema.methods);
assert.equal(servedSchema.source.models_sha256, createHash('sha256').update(models).digest('hex'));
assert.equal(servedSchema.source.declarations_sha256, createHash('sha256').update(declarations).digest('hex'));
assert.throws(() => parseBridgeSchema(models, declarations + '\nexport function Bad():string;'), /Incomplete/);
assert.throws(() => createFixture(), /schema is required/);

const fixture = createFixture({ scenario: 'picker-selected', schema, latency: 0 });
const b = fixture.bindings;
assert.equal((await b.Selection()).total, 3);
const removed = await b.RemovePackages(['gimp']);
assert.ok(removed.undo_token);
assert.equal((await b.UndoSelection(removed.undo_token)).total, 3);
await b.AddURLs([{ url: 'https://vendor.example/agent.deb', sha256: DIGEST }]);
const keyRevision = (await b.Selection()).revision;
const inputRevision = (await b.LifecycleStatus()).selection_revision;
await b.AddURLs([{ url: 'https://vendor.example/agent.deb', sha256: '7b'.repeat(32) }]);
assert.equal((await b.Selection()).revision,keyRevision,'digest-only edit preserves key revision');
assert.ok((await b.LifecycleStatus()).selection_revision > inputRevision,'digest-only edit moves build-input revision');
const before = await b.SelectionPage(0, 100);
const cleared = await b.ClearSelection();
assert.equal(cleared.total, 0);
await b.UndoSelection(cleared.undo_token);
assert.deepEqual((await b.SelectionPage(0, 100)).entries, before.entries);
const expired = await b.RemovePackages(['gimp']);
await b.AddPackages(['curl']);
assert.ok((await b.UndoSelection(expired.undo_token)).error);
const snapshot = await b.SelectionPage(0, 100);
snapshot.entries.length = 0;
assert.ok((await b.Selection()).total > 0, 'read result must not mutate fixture backend');
const started = await b.StartBuild({ output_dir: '/tmp/bundle', signer_ref: '/tmp/key', acknowledge_redistribution: true });
assert.equal(started.ok, true);
const count = (await b.Selection()).total;
assert.ok((await b.ClearSelection()).error, 'active job must refuse conflicting mutation');
assert.equal((await b.Selection()).total,count);
await b.CancelBuild();
assert.equal((await b.BuildStatus()).cancelled,true);
assert.equal((await b.LifecycleStatus()).unsaved_selection,true);
assert.throws(() => b.NotARealBinding, /Undeclared binding/);
assert.throws(() => b.Bind, /Missing fixture implementation/, 'Bind is native wiring, not an invented fixture success');
const strict = createFixture({ schema: { classes: schema.classes, methods: { Selection: schema.methods.Selection } }, latency: 0 });
assert.throws(() => strict.bindings.ChooseSigningKey, /Undeclared binding/);
assert.equal((await strict.bindings.Selection()).total,0);
fixture.destroy(); strict.destroy();

// Reject errors where they originate, including otherwise-hidden optional typos.
const summary = await b.Selection();
assert.throws(() => validateDTO({ ...summary, undo_lable: undefined }, 'app.SelectionSummary', schema), /undo_lable: unknown/);
assert.throws(() => validateDTO({ ...summary, total: '3' }, 'SelectionSummary', schema), /total: expected number/);
assert.throws(() => validateDTO({ ...summary, total: NaN }, 'SelectionSummary', schema), /total: expected number/);
const missing = { ...summary }; delete missing.revision;
assert.throws(() => validateDTO(missing, 'SelectionSummary', schema), /revision: missing required/);
assert.throws(() => validateDTO({ ...summary, error: { code: 'broken', message: 'bad', retryable: 'yes' } }, 'SelectionSummary', schema), /error.retryable: expected boolean/);
assert.throws(() => validateDTO({ ...summary, warnings: [{}] }, 'SelectionSummary', schema), /warnings\[0\].kind: missing required/);
assert.throws(() => validateDTO(null, 'BuildStatus', schema), /expected BuildStatus/);
assert.doesNotThrow(() => validateDTO({ ...summary, warnings: null, rejected: null }, 'SelectionSummary', schema));
assert.doesNotThrow(() => validateDTO({ entries: null, total: 0, offset: 0, limit: 100 }, 'SelectionPage', schema));
assert.doesNotThrow(() => validateDTO({ seq: 1, type: 'fixture', attrs: null }, 'BuildEvent', schema));
assert.doesNotThrow(() => validateDTO({ seq: 1, type: 'fixture', attrs: { arbitrary: [null, 'text'] } }, 'BuildEvent', schema));
const poisoned = createFixture({ schema, latency: 0 });
poisoned.state.build.progress.total = 'bad';
await assert.rejects(poisoned.bindings.BuildStatus(), /BuildStatus.progress.total: expected number/);
assert.match(poisoned.errors[0], /BuildStatus.progress.total/);
assert.throws(() => poisoned.emit('catalog:finished', { status: { ...poisoned.state.catalog, progress: { label: 'Incomplete fake status' } } }), /catalog:finished.status.progress.generation: missing required/);
poisoned.destroy();

const scenarios = ['target', 'target-missing-cli', 'startup', 'snapshot-invalid', 'snapshot-cancelled',
  'readiness-blocked', 'picker-empty', 'picker-selected', 'picker-undo', 'close-unsaved', 'selection-many320',
  'catalog-loading', 'catalog-failed', 'catalog-cancelled', 'catalog-missing',
  'build-running', 'build-signed', 'build-unsigned', 'build-incomplete', 'build-failed', 'build-cancelled',
  'copy-choose', 'copy-choose-cancelled', 'copy-verified', 'copy-skipped', 'copy-mismatch', 'copy-cancelled',
  'copy-removed', 'copy-incomplete', 'copy-no-space', 'copy-unknown-space'];
const reads = { AppInfo: [], Readiness: [], SupportedArchitectures: [], ListBases: ['amd64'], CurrentTarget: [],
  InspectSnapshot: ['/fixture/snapshot.tar.zst'], CatalogStatus: [], Categories: [], SearchPackages: [{}],
  GetPackage: ['gimp'], PackageRows: [['gimp', 'missing-package']], Selection: [], SelectionKeys: [], SelectionPage: [0, 100],
  ParsePackageList: ['gimp\nfirefox\nunknown-package\ninvalid package\ngimp\n# a comment'],
  PreviewCommand: [{ output_dir: '/fixture/output', no_sign: true }], BuildStatus: [], BuildLog: [0, 100],
  ListVolumes: [], InspectDestination: [{ destination: '/fixture/drive', bundle_path: '/fixture/bundle' }],
  PlanExport: [{ destination: '/fixture/drive', bundle_path: '/fixture/bundle' }], ExportStatus: [], VerifyStatus: [], LifecycleStatus: [] };
let responses = 0;
const covered = new Set();
for (const scenario of scenarios) {
  const f = createFixture({ scenario, schema, latency: 0 });
  try {
    for (const [method, args] of Object.entries(reads)) { await f.bindings[method](...args); covered.add(method); responses++; }
    const absent = await f.bindings.GetPackage('absent-package');
    assert.equal(absent.found, false); assert.equal(absent.package.name, '');
    if (scenario.startsWith('copy-choose')) {
      assert.equal((await f.bindings.BuildStatus()).finished, false);
      assert.equal((await f.bindings.CurrentTarget()).target.selected, false);
      assert.equal((await f.bindings.Selection()).total, 0);
    }
    assert.deepEqual(f.errors, [], scenario);
  } finally { f.destroy(); }
}

// Every UI-callable generated method has at least one checked concrete fixture response.
const actions = { StartReadinessCheck: [], RecheckReadiness: ['signing-key'], RunReadinessAction: ['signing-key'], CancelReadinessCheck: [],
  ChooseSnapshotFile: [], SelectTarget: [{ kind: 'base', base_id: 'debian:13/desktop', arch: 'amd64' }],
  ClearTarget: [], StartCatalogBuild: [true], CancelCatalogBuild: [],
  AddPackages: [['curl', 'new-package']], RemovePackages: [['gimp']], ClearSelection: [], UndoSelection: ['expired'],
  AddPackageList: ['curl\nnew-package\ninvalid package'], AddURLs: [[{ url: 'https://vendor.example/pkg.deb', sha256: DIGEST }]],
  AddLocalDebs: [['/fixture/one.deb', '/fixture/two.deb']], ChooseLocalDebs: [], ChooseDirectory: ['Destination', ''], ChooseSigningKey: [],
  StartBuild: [{ output_dir: '/fixture/output', no_sign: true, acknowledge_redistribution: true }], CancelBuild: [],
  StartExport: [{ bundle_path: '/fixture/bundle', destination: '/fixture/drive' }], CancelExport: [],
  StartVerify: ['/fixture/bundle'], CancelVerify: [], CopyToClipboard: ['report'], RevealPath: ['/fixture/bundle'], RequestClose: [] };
for (const [method, args] of Object.entries(actions)) {
  const f = createFixture({ scenario: 'picker-selected', schema, latency: 0 });
  try { await f.bindings[method](...args); covered.add(method); responses++; assert.deepEqual(f.errors, [], method); }
  finally { f.destroy(); }
}
assert.deepEqual([...covered].sort(), Object.keys(schema.methods).filter(name => name !== 'Bind').sort());

const transitions = createFixture({ scenario: 'picker-selected', schema, latency: 0 });
try {
  const t = transitions.bindings;
  const privateURL = 'https://operator:private-secret@vendor.example/pkg.deb?access_token=private-token';
  await t.AddURLs([{ url: privateURL, sha256: DIGEST }]);
  assert.equal((await t.Selection()).unknown_count, 0, 'external URLs are not unknown apt package names');
  const publicURL = (await t.SelectionPage(0, 100)).entries.find(e => e.source === 'url');
  assert.equal(publicURL.key, 'url:fixture-opaque-vendor-reference');
  assert.equal(publicURL.url, 'https://vendor.example/pkg.deb');
  const removal = await t.RemovePackages([publicURL.key]);
  assert.equal(removal.undo_label, 'Undo removal');
  await t.UndoSelection(removal.undo_token);
  assert.equal(transitions.state.entries.find(e => e.source === 'url').url, privateURL, 'Undo keeps the private original');
  assert.equal(transitions.state.entries.find(e => e.source === 'url').sha256, DIGEST);
  assert.equal((await t.ClearSelection()).undo_label, 'Undo clear');
  await t.AddPackageList('gimp\nfirefox\nnew-package\ninvalid package');
  assert.equal((await t.Selection()).unknown_count, 1);
  await t.SelectTarget({ kind: 'base', base_id: 'debian:13/desktop', arch: 'amd64' });
  assert.equal((await t.CatalogStatus()).state, 'not-built');
  await t.StartCatalogBuild(false);
  await new Promise(resolve => setTimeout(resolve, 450));
  assert.equal((await t.CatalogStatus()).ready, true);
  await t.StartBuild({ output_dir: '/fixture/output', no_sign: true, acknowledge_redistribution: true });
  transitions.finishBuild('signed');
  assert.equal((await t.BuildStatus()).summary.stats.bytes, 86400000);
  await t.StartExport({ bundle_path: '/fixture/bundle', destination: '/fixture/drive' });
  transitions.exportFinish('mismatch');
  assert.ok((await t.ExportStatus()).error);
  await t.StartExport({ bundle_path: '/fixture/bundle', destination: '/fixture/drive' });
  assert.equal((await t.ExportStatus()).error, undefined, 'new copy clears old terminal error');
  transitions.exportFinish('verified');
  await t.StartVerify('/fixture/bundle');
  await new Promise(resolve => setTimeout(resolve, 180));
  assert.equal((await t.VerifyStatus()).ok, true);
  assert.deepEqual(transitions.errors, []);
} finally { transitions.destroy(); }

const many = createFixture({ scenario: 'selection-many320', schema, latency: 0 });
try {
  const m = many.bindings;
  const original = (await m.SelectionKeys()).keys;
  assert.equal(original.length, 320);
  assert.equal(new Set(original).size, 320, 'Large selection keys are unique');
  assert.equal((await m.CatalogStatus()).package_count, 320);
  assert.equal((await m.Selection()).unknown_count, 0, 'All selected fixture packages exist in its catalogue');
  const pages = [];
  for (let offset = 0; offset < original.length; offset += 100) pages.push(...(await m.SelectionPage(offset, 100)).entries);
  assert.deepEqual(pages.map(entry => entry.key), original, 'Native-sized pages cover every key once in backend order');
  assert.equal((await m.SelectionPage(300, 100)).entries.length, 20);
  assert.equal((await m.SearchPackages({ text: 'fixture-package-0312', offset: 0, limit: 50 })).total, 1);
  const removed = await m.RemovePackages([original[157]]);
  assert.equal(removed.total, 319);
  assert.deepEqual((await m.SelectionKeys()).keys, original.filter((_, index) => index !== 157));
  await m.UndoSelection(removed.undo_token);
  assert.deepEqual((await m.SelectionPage(0, 500)).entries, pages, 'Large-selection Undo restores metadata and order');
  assert.deepEqual(many.errors, []);
} finally { many.destroy(); }
console.log(`Fixture checks passed: ${responses} schema-validated responses across ${scenarios.length} scenarios, all ${covered.size} UI methods, strict rejection/null-slice cases, and mutation/job transitions.`);
