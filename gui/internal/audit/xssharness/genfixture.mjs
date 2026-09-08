// Parse the Wails-generated models.ts and App.d.ts into a machine description
// of the whole bridge surface, so the hostile fixture can poison EVERY string
// field that can cross it rather than the handful a human would think of.
import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';
import { createHash } from 'node:crypto';

// internal/audit/xssharness -> the repository root, three levels up.
const HERE = path.dirname(url.fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, '..', '..', '..');
const models = fs.readFileSync(REPO + '/frontend/wailsjs/go/models.ts', 'utf8');
const decls = fs.readFileSync(REPO + '/frontend/wailsjs/go/app/App.d.ts', 'utf8');

// --- classes -------------------------------------------------------------
const classes = {};
const classRe = /export class (\w+) \{([\s\S]*?)\n\t\}/g;
let m;
while ((m = classRe.exec(models)) !== null) {
  const name = m[1];
  const body = m[2];
  const fields = [];
  // stop at the first `static createFrom`
  const head = body.split('static createFrom')[0];
  for (const line of head.split('\n')) {
    const f = line.match(/^\s+(\w+)(\?)?:\s*(.+?);\s*$/);
    if (!f) continue;
    fields.push({ name: f[1], optional: !!f[2], type: f[3].trim() });
  }
  classes[name] = fields;
}

// --- method return types --------------------------------------------------
const methods = {};
// Anchor the complete declaration line: Array<T> and nested generic returns
// contain '>' themselves and must not silently disappear from this audit.
const mre = /^export function (\w+)\((.*)\):Promise<(.+)>;\s*$/gm;
while ((m = mre.exec(decls)) !== null) {
  methods[m[1]] = { args: m[2], ret: m[3].trim() };
}

const declarationCount = (decls.match(/^export function /gm) || []).length;
if (!Object.keys(classes).length || Object.keys(methods).length !== declarationCount) {
  throw new Error(`Incomplete bridge schema: ${Object.keys(classes).length} classes, ${Object.keys(methods).length}/${declarationCount} methods`);
}
for (const required of ['LifecycleStatus', 'GetPackage', 'ChooseSigningKey', 'UndoSelection']) {
  if (!methods[required]) throw new Error(`Generated bridge is missing ${required}`);
}

fs.writeFileSync(
  process.argv[2] || path.join(HERE, 'bridge.json'),
  JSON.stringify({ classes, methods, source: {
    models_sha256: createHash('sha256').update(models).digest('hex'),
    declarations_sha256: createHash('sha256').update(decls).digest('hex'),
  } }, null, 2)
);
console.log('classes:', Object.keys(classes).length, 'methods:', Object.keys(methods).length);
