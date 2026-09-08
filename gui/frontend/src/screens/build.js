// Screen 4 — build and progress.
//
// This screen shows the command that is about to run, runs it, and reports what
// came out. Three things about it are deliberate and should not be "improved"
// without reading docs/dev/cli-surface.md §5.2 first.
//
// 1. THERE IS NO DENOMINATOR. Nothing on debark's wire says how many files a
//    build will fetch. `apt.resolve` carries `selections`, but a selection that
//    is already in the local store downloads nothing, so it is an upper bound
//    on work, not a file count. Therefore the top-level bar is a PHASE
//    indicator and is indeterminate for the whole run. The only honest fraction
//    in this screen is per-file byte progress, and it is drawn per file, next to
//    the file's name, so it can never be mistaken for "the build is 62% done".
//
// 2. `total_bytes` is -1 whenever the archive sent no Content-Length. That is
//    common, not exceptional. A file in that state gets an indeterminate bar and
//    a "size not sent by the archive" label, never a guessed total.
//
// 3. A GAP IS NORMAL. `progress` is throttled to about one event per 250 ms per
//    file, the writer may silently drop events, and four declared event types
//    are never emitted at all (`build.started`, `build.finished`,
//    `snapshot.created`, `verify.result`). Nothing here waits for a particular
//    event, and an unknown `type` is folded in harmlessly.
//
// SECURITY: this module never assigns innerHTML, outerHTML or insertAdjacentHTML,
// and never builds markup by string concatenation. Every value that came off the
// event stream, out of stderr, or out of a summary reaches the DOM as a text
// node via textContent or createTextNode. Icons are built with createElementNS
// from a constant table in this file. Raw event text can contain anything at
// all; this is the highest-risk injection surface in the application, and the
// mitigation is structural rather than a call to an escape() function that
// somebody later forgets.

// The application's name and the name of the binary it drives are NOT the same
// word and neither is spelled out here: the UI says Debark, the wire says
// debark, and both live in exactly one constant so the eventual rename is a
// one-line change. Importing the shell for two constants is safe — shell.js
// has no module-scope side effects and loads screens dynamically, so the cycle
// resolves.
import { APP_NAME, CLI_BINARY } from '../shell/shell.js';
import { bindDisclosure } from '../shell/disclosure.js';

/* ------------------------------------------------------------------ limits */

// How many log lines live in the DOM at once. Equal to BuildLogPageMax, so one
// backend page fills the window exactly. A build can emit tens of thousands of
// events; the drawer shows the most recent MAX_LOG_LINES and says so.
const MAX_LOG_LINES = 500;
const LOG_PAGE = 500;

// Warnings are deduplicated, so this cap is generous in practice.
const MAX_WARNINGS_RENDERED = 50;

// Concurrent downloads shown with their own bar. The rest are counted.
const MAX_INFLIGHT_ROWS = 5;

// Progress is throttled to ~250 ms per file, so silence below a few seconds is
// normal. Past this we say how long it has been rather than looking frozen.
const STALL_AFTER_MS = 8000;

const PREVIEW_DEBOUNCE_MS = 180;

/* ---------------------------------------------------- the frozen exit table */

// core/dferr, frozen by ADR-012; descriptions verbatim from Class.Description()
// as recorded in docs/dev/cli-surface.md §6. Used to turn a class name into a
// sentence so that no failure is ever rendered as a bare number.
const EXIT_CLASSES = {
  success: { code: 0, text: 'success; bundle matches the request' },
  usage: { code: 1, text: 'usage or configuration error' },
  environment: { code: 2, text: 'environment: no apt, no container runtime, no disk, no permission' },
  incomplete: { code: 3, text: 'bundle built but incomplete: a URL failed or an external .deb has unsatisfiable dependencies' },
  verification: { code: 4, text: 'verification failed: signature, digest or repository metadata mismatch' },
  resolution: { code: 5, text: 'resolution failed: apt could not satisfy the request' },
  policy: { code: 6, text: 'policy violation (local policy or approved-keys)' },
  'target-mismatch': { code: 7, text: 'target mismatch on install: architecture or release' },
};

const PHASES = [
  ['starting', 'Starting'],
  ['resolving', 'Resolving'],
  ['downloading', 'Downloading'],
  ['assembling', 'Assembling'],
  ['signing', 'Signing'],
  ['verifying', 'Verifying'],
];

/* -------------------------------------------------------------- DOM helpers */

function h(tag, opts, children) {
  const node = document.createElement(tag);
  const o = opts || {};
  if (o.class) node.className = o.class;
  if (o.text != null) node.textContent = String(o.text);
  if (o.attrs) {
    for (const k in o.attrs) {
      if (o.attrs[k] === null || o.attrs[k] === undefined) continue;
      node.setAttribute(k, String(o.attrs[k]));
    }
  }
  if (o.style) {
    for (const k in o.style) node.style.setProperty(k, o.style[k]);
  }
  if (o.on) {
    for (const k in o.on) node.addEventListener(k, o.on[k]);
  }
  appendAll(node, children);
  return node;
}

function appendAll(node, children) {
  if (children == null) return;
  const list = Array.isArray(children) ? children : [children];
  for (const c of list) {
    if (c == null || c === false) continue;
    node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

const SVG_NS = 'http://www.w3.org/2000/svg';

// Constant icon geometry. Nothing here is ever derived from input, which is why
// it is safe to build these elements at all.
const ICONS = {
  info: [['circle', { cx: 12, cy: 12, r: 9 }], ['path', { d: 'M12 11.2V16' }], ['circle', { cx: 12, cy: 7.9, r: 1, fill: 'currentColor', stroke: 'none' }]],
  success: [['circle', { cx: 12, cy: 12, r: 9 }], ['path', { d: 'M8 12.4l2.6 2.6L16 9.6' }]],
  warning: [['path', { d: 'M12 4.2L2.6 20.2h18.8z' }], ['path', { d: 'M12 10v4.4' }], ['circle', { cx: 12, cy: 17.6, r: 1, fill: 'currentColor', stroke: 'none' }]],
  danger: [['path', { d: 'M8.3 3h7.4L21 8.3v7.4L15.7 21H8.3L3 15.7V8.3z' }], ['path', { d: 'M15 9l-6 6M9 9l6 6' }]],
  pending: [['circle', { cx: 12, cy: 12, r: 9 }], ['path', { d: 'M12 6.8V12l3.4 2' }]],
  copy: [['path', { d: 'M9 9h10v10H9z' }], ['path', { d: 'M5 15V5h10' }]],
  folder: [['path', { d: 'M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2z' }]],
  stop: [['path', { d: 'M7 7h10v10H7z' }]],
  arrow: [['path', { d: 'M5 12h14' }], ['path', { d: 'M13 6l6 6-6 6' }]],
  dot: [['circle', { cx: 12, cy: 12, r: 4 }]],
  chevron: [['path', { d: 'M9 5l7 7-7 7' }]],
  lock: [['path', { d: 'M6.5 10.5h11v9h-11z' }], ['path', { d: 'M9 10.5V7.5a3 3 0 016 0v3' }]],
  // The question mark of .df-help__btn, copied from design/gallery.html so the
  // two cannot drift.
  help: [
    ['circle', { cx: 12, cy: 12, r: 9.5 }],
    ['path', { d: 'M9.3 9.2a2.8 2.8 0 115.3 1.3c-.5.9-1.6 1.3-2.2 2.1-.3.4-.4.8-.4 1.4' }],
    ['path', { d: 'M12 17.4v.1' }],
  ],
};

function icon(name, cls) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  if (cls) svg.setAttribute('class', cls);
  const parts = ICONS[name] || ICONS.info;
  for (const [tag, attrs] of parts) {
    const el = document.createElementNS(SVG_NS, tag);
    for (const k in attrs) el.setAttribute(k, String(attrs[k]));
    svg.appendChild(el);
  }
  return svg;
}

function banner(role, title, text, actions) {
  const body = h('div', { class: 'df-banner__body' });
  if (title) body.appendChild(h('p', { class: 'df-banner__title', text: title }));
  if (text != null) {
    const p = h('p', { class: 'df-banner__text' });
    appendAll(p, text);
    body.appendChild(p);
  }
  const acts = h('div', { class: 'df-banner__actions' });
  appendAll(acts, actions || null);
  return h('div', {
    class: 'df-banner df-banner--' + role,
    attrs: { role: role === 'danger' ? 'alert' : 'status' },
  }, [icon(role, 'df-banner__icon'), body, acts]);
}

function status(role, label) {
  return h('span', { class: 'df-status df-status--' + role }, [icon(role), h('span', { text: label })]);
}

function mono(text) {
  return h('span', { class: 'df-mono', text: text });
}

function btn(label, variant, onClick, opts) {
  const o = opts || {};
  const cls = ['df-btn', 'df-btn--' + (variant || 'secondary')];
  if (o.small) cls.push('df-btn--sm');
  const children = [];
  if (o.icon) children.push(icon(o.icon, 'df-btn__icon'));
  children.push(label);
  return h('button', {
    class: cls.join(' '),
    attrs: { type: 'button' },
    on: { click: onClick },
  }, children);
}

/* ------------------------------------------------- grouping: the boxed list */

// This screen used to stack SEVEN `.df-panel` cards down one column, each with
// its own title rendered inside the box. Peripheral vision counts rectangles
// before it reads a label, and seven of them is what made the longest form in
// the application read as an admin dashboard rather than a settings page.
//
// Design system rule 9 and §6.18: one seamless `.df-boxed-list` per group, the
// group's title as PLAIN TEXT ABOVE the box, and 32px of space between groups
// instead of another border. `.df-panel` is now reserved for things that
// genuinely float over content, and nothing on this screen does.

let uid = 0;
function nextId(prefix) { uid += 1; return prefix + '-' + uid; }

/**
 * One group: a real heading above one box.
 *
 * The heading is an `<h2>` and the `<section>` points at it with
 * aria-labelledby, because a `<section>` with no accessible name is not a
 * landmark and a bold paragraph is not a heading. Both were defects here.
 */
function group(titleText, opts) {
  const o = opts || {};
  const id = nextId('df-build-g');
  const head = h('div', { class: 'df-cluster' }, [
    h('h2', { class: 'df-group__title', text: titleText, attrs: { id: id } }),
  ]);
  if (o.help) head.appendChild(o.help);
  appendAll(head, o.actions || null);
  const section = h('section', { class: 'df-group', attrs: { 'aria-labelledby': id } }, [head]);
  if (o.description) section.appendChild(h('p', { class: 'df-group__description', text: o.description }));
  const box = h('div', { class: 'df-boxed-list' });
  section.appendChild(box);
  return { el: section, box: box, head: head, titleId: id };
}

/**
 * A titled block with NO box: a real heading and prose beneath it.
 *
 * Not everything that needs a title needs a border. Three of this screen's
 * `.df-panel` cards held nothing but sentences and a copyable list, and a
 * `.df-log` draws no border of its own, so these cost no rectangles at all.
 */
function titledBlock(titleText) {
  const id = nextId('df-build-g');
  const body = h('div', { class: 'df-stack df-stack--tight' });
  const el = h('section', { class: 'df-stack df-stack--tight', attrs: { 'aria-labelledby': id } }, [
    h('h2', { class: 'df-group__title', text: titleText, attrs: { id: id } }),
    body,
  ]);
  return { el: el, body: body };
}

/**
 * The label + optional one-line description column of a row.
 *
 * `descriptionSlot` makes an empty description that a render can fill later,
 * which is how a consequence that depends on the current choice reaches the
 * screen without another bordered banner appearing under the box.
 */
function rowText(labelText, description, labelFor, descriptionSlot) {
  const text = h('span', { class: 'df-boxed-list__text' });
  if (labelText != null) {
    text.appendChild(labelFor
      ? h('label', { class: 'df-boxed-list__label', text: labelText, attrs: { for: labelFor } })
      : h('span', { class: 'df-boxed-list__label', text: labelText }));
  }
  if (description || descriptionSlot) {
    const d = h('span', { class: 'df-boxed-list__description', text: description || '' });
    if (!description) d.hidden = true;
    text.appendChild(d);
    text.descEl = d;
  }
  return text;
}

/**
 * Stops Page Up and Page Down changing a `<select>`'s value.
 *
 * A closed `<select>` keeps keyboard focus after a choice is made, and both
 * WebKit2GTK and WebView2 treat Page Up / Page Down on a focused select as
 * "jump to the first / last option" rather than passing them to the scroller.
 * On this screen that is not a harmless surprise: the documentation pass drove
 * it and watched
 * "How to sign" move from *Use debark's configured default key* to *Write an
 * unsigned bundle, deliberately* with no interaction beyond an attempt to
 * scroll the page. The neighbouring value is a signing decision.
 *
 * So the two keys are taken back and given to the scroller, which is what the
 * operator asked for. Every other key a select handles — Up, Down, Home, End,
 * type-ahead, Alt+Down to open the list — is untouched, so nothing about
 * choosing an option by keyboard changes. Applied to all four selects rather
 * than only the signing one: the gesture and the surprise are identical, and a
 * rule that holds on one control and not its neighbours is a worse rule.
 */
function guardSelectPaging(sel) {
  sel.addEventListener('keydown', (e) => {
    if (e.key !== 'PageUp' && e.key !== 'PageDown') return;
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    e.preventDefault();
    // Scroll the nearest scrolling ancestor by a page, in the direction asked
    // for. `.df-app__pane` is the scroll port the shell gives every screen.
    let node = sel.parentNode;
    while (node && node.nodeType === 1) {
      if (node.scrollHeight - node.clientHeight > 1) break;
      node = node.parentNode;
    }
    if (!node || node.nodeType !== 1) return;
    const page = Math.max(1, node.clientHeight - 40);
    node.scrollTop += e.key === 'PageDown' ? page : -page;
  });
  return sel;
}

/** A row carrying a trailing control: an input, a select, a button. */
function boxRow(opts) {
  const o = opts || {};
  const text = rowText(o.label, o.description, o.labelFor, o.descriptionSlot);
  const kids = [text];
  if (o.control) {
    const cls = 'df-boxed-list__control' + (o.field ? ' df-boxed-list__control--field' : '');
    const ctl = h('div', { class: cls });
    appendAll(ctl, o.control);
    kids.push(ctl);
  }
  const row = h('div', { class: 'df-boxed-list__row' }, kids);
  row.descEl = text.descEl || null;
  return row;
}

/** Sets a row's description, hiding it when there is nothing to say. */
function setRowDescription(row, textValue) {
  if (!row || !row.descEl) return;
  row.descEl.textContent = textValue || '';
  row.descEl.hidden = !textValue;
}

/** A row that is itself a checkbox. Clicking anywhere on it toggles. */
function checkRow(labelText, description, checked, onChange) {
  const input = h('input', { class: 'df-check__input', attrs: { type: 'checkbox' }, on: { change: onChange } });
  input.checked = !!checked;
  const row = h('label', { class: 'df-boxed-list__row' }, [
    rowText(labelText, description),
    h('span', { class: 'df-boxed-list__control' }, [input]),
  ]);
  row.inputEl = input;
  return row;
}

/* ------------------------------------------------------- contextual help */

// `.df-help__btn` is why `.df-field__hint` no longer appears under every
// control on this screen. A permanent paragraph under each field is a
// SaaS-onboarding habit and half the density the owner complained about; the
// hints that genuinely prevent an error stay on the row, and the paragraphs
// that explain a consequence move in here.
//
// Layered, never inline: an inline <details> reflows everything below it.

let openPop = null;

function onPopKey(ev) {
  if (ev.key === 'Escape' && openPop) {
    ev.preventDefault();
    closePopover(true);
  }
}

function onPopDown(ev) {
  if (openPop && !openPop.anchor.contains(ev.target)) closePopover(false);
}

function closePopover(restoreFocus) {
  const cur = openPop;
  if (!cur) return;
  openPop = null;
  cur.pop.hidden = true;
  cur.btn.setAttribute('aria-expanded', 'false');
  document.removeEventListener('keydown', onPopKey, true);
  document.removeEventListener('mousedown', onPopDown, true);
  if (restoreFocus) {
    try { cur.btn.focus(); } catch (_) { /* a detached opener is not worth throwing over */ }
  }
}

function togglePopover(btn, pop, anchor) {
  if (openPop && openPop.pop === pop) { closePopover(true); return; }
  closePopover(false);
  openPop = { btn: btn, pop: pop, anchor: anchor };
  pop.hidden = false;
  btn.setAttribute('aria-expanded', 'true');
  document.addEventListener('keydown', onPopKey, true);
  document.addEventListener('mousedown', onPopDown, true);
}

/** `label` is the button's accessible name, e.g. "About the bundle name". */
function help(label, title, paragraphs) {
  const id = nextId('df-build-h');
  const pop = h('div', { class: 'df-popover df-popover--end', attrs: { id: id } });
  if (title) pop.appendChild(h('p', { class: 'df-popover__title', text: title }));
  for (const p of paragraphs) pop.appendChild(h('p', { class: 'df-popover__text', text: p }));
  pop.hidden = true;
  const btn = h('button', {
    class: 'df-help__btn',
    attrs: { type: 'button', 'aria-expanded': 'false', 'aria-controls': id, 'aria-label': label },
  }, [icon('help')]);
  const anchor = h('span', { class: 'df-popover-anchor' }, [btn, pop]);
  btn.addEventListener('click', () => togglePopover(btn, pop, anchor));
  return anchor;
}

/* --------------------------------------------------------- state components */

// The shell's shared state helpers (`shell/states.js`) are used when they are
// present and expose a factory for the kind we want; otherwise we build the same
// markup the design system documents in §6.15. The screen contract asks for
// ctx.states, and this screen must still render correctly in the standalone
// harness where the shell does not exist.
function stateEl(ctx, kind, title, text, actions, detail) {
  const s = ctx && ctx.states;
  if (s && typeof s[kind] === 'function') {
    try {
      const el = s[kind]({ title: title, text: text, actions: actions, detail: detail });
      if (el && el.nodeType === 1) return el;
    } catch (_) { /* fall through to the local rendering */ }
  }
  const cls = kind === 'error' ? 'df-state df-state--error'
    : kind === 'loading' ? 'df-state df-state--loading'
      : 'df-state';
  const kids = [
    icon(kind === 'error' ? 'danger' : kind === 'loading' ? 'pending' : 'info', 'df-state__icon'),
    h('span', { class: 'df-state__title', text: title }),
  ];
  if (text) kids.push(h('p', { class: 'df-state__text', text: text }));
  if (actions && actions.length) kids.push(h('div', { class: 'df-state__actions' }, actions));
  if (detail) kids.push(h('div', { class: 'df-state__detail' }, detail));
  return h('div', { class: cls }, kids);
}

/* ------------------------------------------------------------- formatting */

// One byte formatter for the whole application, reached through the shell.
// `ctx.states.formatBytes` is binary — KiB/MiB/GiB — because apt and dpkg
// report Installed-Size in KiB and `df -h` is what an operator checks the same
// drive with. Two screens rendering the same quantity in different units is
// exactly the drift a shared helper exists to stop, so this screen no longer
// carries its own. The fallback below is reached only in the standalone
// harness, where there is no shell to ask, and produces the same strings.
let sharedStates = null;

function formatBytes(n) {
  if (typeof n !== 'number' || !isFinite(n) || n < 0) return 'unknown';
  if (sharedStates && typeof sharedStates.formatBytes === 'function') {
    const s = sharedStates.formatBytes(n);
    if (s) return s;
  }
  if (n < 1024) return String(Math.round(n)) + ' B';
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v >= 100 ? String(Math.round(v)) : v.toFixed(1).replace(/\.0$/, '')) + ' ' + units[i];
}

function formatCount(n) {
  if (typeof n !== 'number' || !isFinite(n)) return '0';
  return n.toLocaleString();
}

function formatDuration(ms) {
  if (!isFinite(ms) || ms < 0) ms = 0;
  const total = Math.floor(ms / 1000);
  const h2 = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h2 > 0) return h2 + ' h ' + m + ' min';
  if (m > 0) return m + ' min ' + s + ' s';
  return s + ' s';
}

function fileNameOf(url) {
  if (typeof url !== 'string' || url === '') return '';
  const cut = url.split(/[?#]/)[0];
  const parts = cut.split('/');
  return parts[parts.length - 1] || cut;
}

// Display-only POSIX quoting, for showing the argv of a build that is already
// running. Nothing in this application ever hands a string to a shell; this
// exists so the operator can copy the line and run it themselves. The backend's
// CommandPreview.Display is preferred wherever it is available.
function shellQuote(token) {
  const s = String(token);
  if (s !== '' && /^[A-Za-z0-9_@%+=:,./-]+$/.test(s)) return s;
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

function argvDisplay(argv) {
  if (!argv || !argv.length) return '';
  return argv.map(shellQuote).join(' ');
}

/* ------------------------------------------------------- event accumulation */

function newAcc() {
  return {
    lastSeq: 0,
    lastEventAt: 0,
    lastMsg: '',
    eventCount: 0,

    filesDone: 0,
    bytesFetched: 0,
    storeHits: 0,
    storeHitBytes: 0,
    retries: 0,

    // apt.resolve's `selections` is an UPPER BOUND on the work, not a file
    // count: a selection already in the local store downloads nothing. It is
    // never used as a denominator. -1 means "not reported yet".
    selections: -1,
    // True when `selections` came from an apt.resolve carrying `packages`
    // rather than `selections` -- the base's own closure, not the operator's
    // choices. The two are different numbers and were sharing one label.
    selectionsAreBaseClosure: false,
    unresolvedReported: 0,

    inflight: new Map(), // url -> { url, name, bytes, total, attempt, attempts }
    inflightOrder: [],

    warnings: [],
    warningsByKey: new Map(),
    warningTotal: 0,

    // The reason each external input failed, keyed by the operator's own input
    // string so it can be shown BESIDE that input in the missing list rather
    // than only as a warning further down the page.
    //
    // docs/dev/error-catalogue.md 3.3-3.5 is one row precisely because an
    // unreachable host, a SHA-256 that did not match and a certificate this
    // machine does not trust used to be indistinguishable. They are three
    // different problems needing three different actions, and the sentence
    // that tells them apart arrives only in the `error` attribute of a
    // warn-level `input.external` event. This map is where it is kept.
    //
    // It is read out of the event, never derived: nothing here matches on the
    // text. `BuildResult.fetch_failures[]` carries a classified reason as of
    // core 1c767f7, but internal/app's BuildSummary does not project it yet,
    // so the event is the only route the frontend has today. See the note in
    // renderMissing.
    fetchReasons: new Map(), // input string -> reason sentence

    backend: '',
    externalNames: 0,
    assembledCount: -1,
    signedKeyID: '',
  };
}

function attrNum(attrs, key) {
  if (!attrs) return null;
  const v = attrs[key];
  return typeof v === 'number' && isFinite(v) ? v : null;
}

function attrStr(attrs, key) {
  if (!attrs) return '';
  const v = attrs[key];
  return typeof v === 'string' ? v : '';
}

function joinList(v) {
  if (Array.isArray(v)) return v.map((x) => String(x)).join(', ');
  if (v === null || v === undefined) return '';
  return String(v);
}

function touchInflight(acc, url) {
  let rec = acc.inflight.get(url);
  if (!rec) {
    rec = { url: url, name: fileNameOf(url), bytes: 0, total: -1, attempt: 0, attempts: 0 };
    acc.inflight.set(url, rec);
    acc.inflightOrder.push(url);
  }
  return rec;
}

function dropInflight(acc, url) {
  if (!acc.inflight.has(url)) return;
  acc.inflight.delete(url);
  const i = acc.inflightOrder.indexOf(url);
  if (i >= 0) acc.inflightOrder.splice(i, 1);
}

function addWarning(acc, severity, title, text, seq) {
  const key = severity + '|' + title + '|' + text;
  const existing = acc.warningsByKey.get(key);
  acc.warningTotal++;
  if (existing) {
    existing.count++;
    existing.dirty = true;
    return existing;
  }
  const rec = { key: key, severity: severity, title: title, text: text, count: 1, seq: seq || 0, node: null, dirty: true };
  acc.warningsByKey.set(key, rec);
  acc.warnings.push(rec);
  return rec;
}

// foldEvent is the whole accumulation. It is used identically for live events
// and for events replayed out of BuildLog when the screen remounts, so the two
// paths can never disagree.
//
// It reads a label off a stream. It decides nothing about the build: no version
// comparison, no dependency reasoning, no "which package supersedes which".
function foldEvent(acc, ev) {
  if (!ev || typeof ev !== 'object') return;
  if (typeof ev.seq === 'number' && ev.seq > acc.lastSeq) acc.lastSeq = ev.seq;
  acc.eventCount++;
  acc.lastEventAt = Date.now();
  if (ev.msg) acc.lastMsg = ev.msg;

  const a = ev.attrs || {};
  const type = typeof ev.type === 'string' ? ev.type : '';
  const level = typeof ev.level === 'string' ? ev.level : '';
  const before = acc.warningTotal;

  switch (type) {
    case 'progress': {
      const url = attrStr(a, 'url');
      const attempt = attrNum(a, 'attempt');
      if (attempt !== null) {
        // Retry notice. A retry RESTARTS the byte count for this file, so the
        // per-file bar goes backwards. That is the truth; do not clamp it.
        acc.retries++;
        const rec = touchInflight(acc, url);
        rec.attempt = attempt;
        rec.attempts = attrNum(a, 'attempts') || 0;
        rec.bytes = 0;
        addWarning(acc, 'warning', 'Retrying a download',
          (rec.name || url) + ' — attempt ' + attempt + (rec.attempts ? ' of ' + rec.attempts : ''), ev.seq);
        break;
      }
      if (!url) break;
      const rec = touchInflight(acc, url);
      const b = attrNum(a, 'bytes');
      const t = attrNum(a, 'total_bytes');
      if (b !== null) rec.bytes = b;
      // -1 means the archive sent no Content-Length. Keep it as -1; never guess.
      if (t !== null) rec.total = t;
      break;
    }
    case 'fetch.file': {
      acc.filesDone++;
      const size = attrNum(a, 'size');
      if (size !== null && size > 0) acc.bytesFetched += size;
      const url = attrStr(a, 'url');
      const name = attrStr(a, 'filename') || fileNameOf(url);
      if (url) dropInflight(acc, url);
      acc.lastFile = name;
      const verification = attrStr(a, 'verification');
      if (verification && /fail|unverified|none/i.test(verification)) {
        addWarning(acc, 'warning', 'Unverified download', name + ' — verification: ' + verification, ev.seq);
      }
      break;
    }
    case 'store.hit': {
      acc.storeHits++;
      const size = attrNum(a, 'size');
      if (size !== null && size > 0) acc.storeHitBytes += size;
      break;
    }
    case 'apt.resolve': {
      const sel = attrNum(a, 'selections');
      const pkgs = attrNum(a, 'packages'); // the base-synthesis shape
      if (sel !== null) {
        acc.selections = sel;
        acc.selectionsAreBaseClosure = false;
      } else if (pkgs !== null) {
        // Two different numbers were being shown under one label. On a stock
        // base the first apt.resolve is the BASE's own closure —
        // {"packages":147,"seeds":1} on a run whose operator had chosen two
        // items — and calling that "Selections resolved" tells them their two
        // selections came to 147. Photographed twice while driving
        // docs/dev/error-catalogue.md §3.2 and §3.8, on runs that never
        // reached the operator's own packages at all.
        acc.selections = pkgs;
        acc.selectionsAreBaseClosure = true;
      }
      const un = attrNum(a, 'unresolved');
      if (un !== null) acc.unresolvedReported = un;
      break;
    }
    case 'apt.update': {
      // `failed` is apt's own verdict and it is almost never true: ADR-001
      // makes apt the oracle, so `apt-get update` failing to fetch a source is
      // not treated as failing the run. Driven with `--network none` before
      // core d5f31cb, this event read {"entries":12,"failed":false} on a
      // machine with no network at all — docs/dev/error-catalogue.md §3.1's
      // core gap, and the reason the branch below could never fire.
      //
      // core d5f31cb added sources_fetched/sources_failed, so the wire now
      // says it. Re-driven in a `--network none` container against that
      // binary: {"entries":16,"failed":false,"sources_failed":4,
      // "sources_fetched":0}, against {"entries":19,"failed":false,
      // "sources_failed":0,"sources_fetched":4} on the same build with a
      // network. Nothing here parses prose; both are integers off the event.
      //
      // This matters most on the run that does NOT fail. A build that resolves
      // from indexes apt could not refresh succeeds, and every version in it
      // is whatever was last cached; without this the screen said nothing at
      // all. When zero sources were fetched the machine is offline, so that
      // case is raised as danger and the resolution failure that usually
      // follows (§3.1) has a companion saying why.
      const srcFailed = attrNum(a, 'sources_failed');
      const srcFetched = attrNum(a, 'sources_fetched');
      const none = srcFetched === 0 && srcFailed !== null && srcFailed > 0;
      if (none) {
        addWarning(acc, 'danger', 'No package index could be fetched',
          `None of the ${formatCount(srcFailed)} configured sources answered, so nothing about the target's archive is newer than the last time this machine reached it. That is the network, not the packages.`, ev.seq);
      } else if (a.failed === true || (srcFailed !== null && srcFailed > 0)) {
        addWarning(acc, 'warning', 'Some package indexes could not be refreshed',
          `${srcFailed ? formatCount(srcFailed) + ' of the configured sources did not answer. ' : ''}${CLI_BINARY} carried on with the indexes it has, so the plan may be built against stale metadata.`, ev.seq);
      }
      break;
    }
    case 'backend.selected': {
      acc.backend = attrStr(a, 'backend') || acc.backend;
      break;
    }
    case 'input.external': {
      if (level === 'warn' || level === 'error') {
        // A warn-level input.external is EITHER a local .deb that could not be
        // read (attrs.path) OR a vendor URL that would not download
        // (attrs.url). This used to read `path` only and title everything "A
        // local .deb could not be read", so every failed download — a refused
        // connection, an untrusted certificate, a digest mismatch — was
        // announced as a local file problem at "unknown path".
        const url = attrStr(a, 'url');
        const path = attrStr(a, 'path');
        const input = url || path;
        const reason = attrStr(a, 'error') || ev.msg || 'no reason given';
        if (input) acc.fetchReasons.set(input, reason);
        addWarning(acc, level === 'error' ? 'danger' : 'warning',
          url ? 'A vendor download failed' : 'A local .deb could not be read',
          (input || 'unknown input') + ' — ' + reason, ev.seq);
      } else if (Array.isArray(a.names)) {
        acc.externalNames += a.names.length;
      }
      break;
    }
    case 'policy.finding': {
      const sev = attrStr(a, 'severity').toLowerCase();
      addWarning(acc, sev === 'error' || sev === 'critical' ? 'danger' : sev === 'info' ? 'info' : 'warning',
        'Policy: ' + (attrStr(a, 'rule') || 'a rule matched'),
        (ev.msg ? ev.msg + ' ' : '') + (a.packages ? 'Packages: ' + joinList(a.packages) : ''), ev.seq);
      break;
    }
    case 'doctor.finding': {
      const sev = attrStr(a, 'severity').toLowerCase();
      const bits = [];
      if (attrStr(a, 'package')) bits.push(attrStr(a, 'package'));
      if (attrStr(a, 'flag')) bits.push(attrStr(a, 'flag'));
      addWarning(acc, sev === 'error' || sev === 'critical' ? 'danger' : sev === 'info' ? 'info' : 'warning',
        'Check: ' + (attrStr(a, 'check') || 'a check reported something'),
        (ev.msg || '') + (bits.length ? (ev.msg ? ' — ' : '') + bits.join(' · ') : ''), ev.seq);
      break;
    }
    case 'warning': {
      const code = attrStr(a, 'code');
      addWarning(acc, level === 'error' ? 'danger' : 'warning',
        code ? 'Warning: ' + code : 'Warning',
        ev.msg || describeAttrs(a), ev.seq);
      break;
    }
    case 'bundle.assembled': {
      const c = attrNum(a, 'package_count');
      if (c !== null) acc.assembledCount = c;
      break;
    }
    case 'manifest.signed': {
      acc.signedKeyID = attrStr(a, 'key_id');
      break;
    }
    default:
      // Unknown types — including the four that are declared and never emitted,
      // and anything a future debark starts sending — land here and are
      // harmless. Never require a known type.
      break;
  }

  // Anything that arrived at warn or error level and was not already turned into
  // a warning above still gets surfaced, whatever its type.
  if ((level === 'warn' || level === 'error') && acc.warningTotal === before) {
    addWarning(acc, level === 'error' ? 'danger' : 'warning',
      type || 'Event', ev.msg || describeAttrs(a), ev.seq);
  }
}

function describeAttrs(attrs) {
  if (!attrs) return '';
  const out = [];
  for (const k in attrs) {
    const v = attrs[k];
    out.push(k + '=' + (typeof v === 'object' ? JSON.stringify(v) : String(v)));
    if (out.length >= 8) break;
  }
  return out.join(' ');
}

/* ------------------------------------------------------- the progress model */

// describeProgress turns the accumulator into what the top bar says. It never
// returns a run-level fraction, because there is not one to compute.
function describeProgress(acc, phase, running) {
  const live = acc.inflightOrder.map((u) => acc.inflight.get(u)).filter(Boolean);

  if (phase === 'downloading' || live.length > 0) {
    const parts = [];
    parts.push(formatCount(acc.filesDone) + (acc.filesDone === 1 ? ' file fetched' : ' files fetched'));
    if (acc.bytesFetched > 0) parts.push(formatBytes(acc.bytesFetched));
    if (acc.storeHits > 0) parts.push(formatCount(acc.storeHits) + ' already in the local store');
    if (live.length > 0) {
      return {
        label: live.length === 1
          ? 'Downloading ' + (live[0].name || live[0].url)
          : 'Downloading ' + live.length + ' files',
        value: parts.join(' · '),
      };
    }
    return { label: 'Downloading', value: parts.join(' · ') };
  }

  const labels = {
    idle: 'Waiting to start',
    starting: `Starting ${CLI_BINARY}`,
    resolving: 'Resolving with apt',
    assembling: 'Assembling the bundle',
    signing: 'Signing the manifest',
    verifying: 'Checking the bundle',
    finished: 'Finished',
    failed: 'Stopped on an error',
    cancelled: 'Stopped',
  };
  let label = labels[phase] || (running ? 'Working' : 'Idle');
  if (acc.lastMsg && running) label = label + ' — ' + acc.lastMsg;
  let value = '';
  if (phase === 'resolving' && acc.selections >= 0) {
    value = formatCount(acc.selections) + (acc.selectionsAreBaseClosure ? ' base packages' : ' selections');
  } else if (acc.filesDone > 0) {
    value = formatCount(acc.filesDone) + ' files · ' + formatBytes(acc.bytesFetched);
  }
  return { label: label, value: value };
}

// classifyOutcome names which of the four endings a build:finished payload is.
// It is one function so the header chip, the bar and the outcome panel cannot
// disagree about whether a run succeeded.
//
// The subtle one is `incomplete`: debark returns a POPULATED result AND an
// error, so a bundle exists but is short. Treating a result with a bundle path
// alongside an error as incomplete also covers the case where the class string
// is missing, which matters because getting this wrong in either direction
// misleads the operator about whether their media is usable.
function classifyOutcome(f) {
  if (f.cancelled) return 'cancelled';
  const summary = f.summary || null;
  const err = f.error || null;
  const cls = (summary && summary.exit_class) || (err && err.exit_class) || '';
  if (cls === 'incomplete') return 'incomplete';
  if (cls && cls !== 'success') return 'failed';
  if (f.ok === false && summary && summary.bundle_path) return 'incomplete';
  if (f.ok === false || err) return 'failed';
  return 'finished';
}

const OUTCOME_LABELS = {
  finished: 'Finished',
  incomplete: 'Finished — but the bundle is incomplete',
  failed: 'Stopped on an error',
  cancelled: 'Stopped by you',
};

/* ------------------------------------------------------------- the screen */

const screen = {
  id: 'build',
  title: 'Bundle',

  ctx: null,
  root: null,
  els: null,
  opts: null,
  sign: 'key',
  ack: false,
  // AppInfo.progress_events. False means no build:progress and no build:event
  // will EVER arrive — an older debark with no --json-events — so the screen
  // must not draw a bar that is waiting for one. `progressEventsKnown` keeps
  // the honest "not asked yet" apart from the honest "asked, and no".
  progressEvents: true,
  progressEventsKnown: false,
  // True between pressing Build and StartBuild answering. The button stays in
  // the tab order throughout, so something has to refuse the second press.
  starting: false,
  acc: newAcc(),
  phase: 'idle',
  // One of '', 'finished', 'incomplete', 'failed', 'cancelled'.
  outcome: '',
  running: false,
  finished: false,
  cancelRequested: false,
  startedAtMs: 0,
  finishedAtMs: 0,
  command: [],
  commandDisplay: '',
  // The UIError PreviewCommand last came back with, or null. It gates the
  // primary action and is named in the setup bar's status line; see
  // syncBuildEnablement.
  previewError: null,
  logOpen: false,
  logSeq: 0,
  logLines: [],
  logDropped: 0,
  logTotal: 0,
  pendingLogEvents: [],
  rafPending: false,
  renderQueued: false,
  tickTimer: 0,
  previewTimer: 0,
  rehydrating: false,
  visible: false,

  mount(root, ctx) {
    this.ctx = ctx;
    this.root = root;
    sharedStates = (ctx && ctx.states) || null;
    // The module is a singleton, so a remount must not inherit the render
    // guards of the DOM it just threw away.
    this.statusKey = '';
    this.phaseRowKey = '';
    this.outcome = '';
    // A remount builds a fresh, closed drawer.
    this.logOpen = false;
    this.logLines = [];
    this.logSeq = 0;
    this.pendingLogEvents = [];
    // Bumped whenever an event moves the build's lifecycle. A BuildStatus()
    // reply that was in flight across a build:started or build:finished is
    // stale by the time it resolves, and applying it would put the screen back
    // into "Building" after the build had already finished.
    this.epoch = 0;
    this.statusReadSeq = 0;
    this.statusPending = false;
    this.statusReadError = null;
    this.rehydrateSeq = 0;
    this.rehydrating = false;
    this.rehydrateAgain = false;
    this.liveLogTimer = 0;
    this.progressStatusTimer = 0;
    this.previewEpoch = 0;
    this.lifecycleEpoch = 0;
    this.lifecycle = null;
    this.lifecycleReadError = null;
    this.sign = 'key';
    this.ack = false;
    this.runStatus = null;
    this.resultSummary = null;
    this.createKeyPending = false;
    this.opts = {
      output_dir: '',
      output_name: '',
      format: 'dir',
      signer_ref: '',
      no_sign: false,
      sbom: true,
      // Three states, not a checkbox. `null` follows the target's own apt.conf,
      // `false` is --no-recommends, `true` is --recommends; the first two are
      // DIFFERENT builds and a two-state control silently collapses them.
      // `no_recommends` is the deprecated one-directional spelling and is not
      // sent at all any more.
      recommends: null,
      upgrades: false,
      update: false,
      no_prune: false,
      backend: 'auto',
      acknowledge_redistribution: false,
    };
    this.buildDOM();

    ctx.on('build:started', (p) => this.onStarted(p));
    ctx.on('build:progress', (p) => this.onProgress(p));
    ctx.on('build:event', (e) => this.onEvent(e));
    ctx.on('build:finished', (f) => this.onFinished(f));
    ctx.on('app:lifecycle', (status) => { this.lifecycleEpoch++; this.applyLifecycle(status); });
    ctx.on('readiness:finished', (f) => {
      if (!this.els) return;
      this.applyReadiness(f && f.report);
      if (this.createKeyPending && f && f.action && f.check_id === 'signing-key') {
        this.createKeyPending = false;
        this.els.createKey.textContent = 'Create key…';
        this.els.createKey.disabled = false;
        clear(this.els.signNotice);
        if (f.error || f.ok === false) this.els.signNotice.appendChild(this.errorBanner(f.error, 'The key was not created.'));
        else {
          this.els.signNotice.appendChild(banner('info', 'Key created', 'Choose key… and select the private key file saved by this command:'));
          this.els.signNotice.appendChild(h('p', { class: 'df-mono df-text-sm', text: this.keyActionDisplay || 'debark keygen' }));
        }
      }
    });
    this.refreshLifecycle();
    this.refreshReadiness();
    this.probeProgressEvents();
  },

  show(ctx, params) {
    this.ctx = ctx || this.ctx;
    sharedStates = (this.ctx && this.ctx.states) || sharedStates;
    this.visible = true;
    if (params && params.outputDir && !this.opts.output_dir) {
      this.opts.output_dir = String(params.outputDir);
      this.els.outputDir.value = this.opts.output_dir;
    }
    this.refreshHeader();
    this.syncFromStatus();
    this.refreshLifecycle();
    this.refreshReadiness();
    this.schedulePreview(0);
    this.startTicker();
  },

  hide() {
    this.visible = false;
    this.stopTicker();
    // A help popover left open would outlive the screen it explains, and its
    // document-level Escape and outside-click handlers with it.
    closePopover(false);
    if (this.els?.keyDialog?.open) this.els.keyDialog.close();
  },

  destroy() {
    this.stopTicker();
    closePopover(false);
    if (this.previewTimer) clearTimeout(this.previewTimer);
    if (this.liveLogTimer) clearTimeout(this.liveLogTimer);
    if (this.progressStatusTimer) clearTimeout(this.progressStatusTimer);
    this.previewTimer = 0;
    this.pendingLogEvents = [];
    this.els = null;
    this.root = null;
    this.ctx = null;
    sharedStates = null;
  },

  /* ---------------------------------------------------- capability probe */

  // AppInfo.progress_events is false when the located debark has no
  // --json-events: the build still runs and still finishes, but not one
  // build:progress or build:event will arrive. A bar waiting for those would
  // sit at "Starting debark" for the whole run and look wedged, so the
  // screen says what it does and does not know instead.
  probeProgressEvents() {
    const api = this.api();
    if (!api || typeof api.AppInfo !== 'function') return;
    Promise.resolve(api.AppInfo()).then((info) => {
      if (!this.els || !info) return;
      this.progressEventsKnown = true;
      this.progressEvents = info.progress_events !== false;
      this.renderProgress();
    }).catch(() => { /* an unprobed binary is read as "no progress", below */ });
  },

  /* --------------------------------------------------------------- plumbing */

  api() {
    return (this.ctx && this.ctx.bindings) || null;
  },

  toast(kind, message) {
    if (this.ctx && typeof this.ctx.toast === 'function') this.ctx.toast(kind, message);
  },

  copy(text, what) {
    const api = this.api();
    const done = () => this.toast('success', what + ' copied to the clipboard.');
    if (api && typeof api.CopyToClipboard === 'function') {
      Promise.resolve(api.CopyToClipboard(text)).then((res) => {
        if (res && res.ok === false && res.error) this.toast('danger', res.error.message || 'Could not copy.');
        else done();
      }).catch(() => this.toast('danger', 'Could not copy.'));
      return;
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(() => this.toast('danger', 'Could not copy.'));
    }
  },

  startTicker() {
    if (this.tickTimer) return;
    this.tickTimer = setInterval(() => {
      if (this.running) this.renderRunHeader();
    }, 1000);
  },

  stopTicker() {
    if (this.tickTimer) clearInterval(this.tickTimer);
    this.tickTimer = 0;
  },

  /* ------------------------------------------------------------ DOM: build */

  buildDOM() {
    const els = {};
    this.els = els;

    /* ---------- compact review ---------- */
    const idSetupHeading = nextId('df-build-h');
    els.setupHeading = h('h1', { class: 'df-focus-target', text: 'Review bundle', attrs: { id: idSetupHeading } });
    els.targetLine = h('span', { class: 'df-text-sm' });
    els.selectionSummary = h('span', { class: 'df-text-sm', text: 'Reading selection…' });
    els.editTarget = btn('Edit target', 'ghost', () => this.ctx.go('target'), { small: true });
    els.editSelection = btn('Edit packages', 'ghost', () => this.ctx.go('picker'), { small: true });
    const setupLede = h('div', { class: 'df-stack df-stack--tight' }, [els.setupHeading,
      h('div', { class: 'df-cluster' }, [els.targetLine, els.editTarget, els.selectionSummary, els.editSelection]),
    ]);
    els.environmentNotice = h('div');
    els.jobNotice = h('div');

    const idDir = nextId('df-build-f'), idName = nextId('df-build-f');
    els.outputDir = h('input', { class: 'df-input df-mono', attrs: { id: idDir, type: 'text', spellcheck: 'false', placeholder: 'Choose a folder' },
      on: { input: e => { this.opts.output_dir = e.target.value.trim(); this.schedulePreview(); } } });
    els.outputName = h('input', { class: 'df-input', attrs: { id: idName, type: 'text', spellcheck: 'false', placeholder: 'Automatic name' },
      on: { input: e => { this.opts.output_name = e.target.value.trim(); this.schedulePreview(); } } });
    els.chooseFolder = btn('Choose folder…', 'secondary', () => this.chooseDirectory(), { small: true, icon: 'folder' });
    els.destination = h('p', { class: 'df-text-sm df-text-secondary', text: 'Choose where to save the bundle.' });
    const outputGroup = h('div', { class: 'df-boxed-list' }, [
      boxRow({ label: 'Output folder', labelFor: idDir, field: true, control: [els.outputDir, els.chooseFolder] }),
      boxRow({ label: 'Bundle name', labelFor: idName, field: true, control: [els.outputName] }),
    ]);

    const idSignMode = nextId('df-build-f'), idSigner = nextId('df-build-f');
    els.signSelect = guardSelectPaging(h('select', { class: 'df-input', attrs: { id: idSignMode }, on: { change: e => this.setSign(e.target.value) } }, [
      h('option', { attrs: { value: 'key' }, text: 'Sign with a key' }),
      h('option', { attrs: { value: 'default' }, text: 'Use configured default' }),
      h('option', { attrs: { value: 'unsigned' }, text: 'Unsigned bundle' }),
    ]));
    els.signerRef = h('input', { class: 'df-input df-mono', attrs: { id: idSigner, type: 'text', spellcheck: 'false', placeholder: 'gpg:<keyid> or plugin:<name>' },
      on: { input: e => { this.opts.signer_ref = e.target.value.trim(); this.renderSignNotice(); this.schedulePreview(); } } });
    els.keyLabel = h('span', { class: 'df-text-sm df-truncate', text: 'No key chosen' });
    els.chooseKey = btn('Choose key…', 'secondary', () => this.chooseSigningKey(), { small: true });
    els.createKey = btn('Create key…', 'ghost', () => this.requestKeyCreation(), { small: true });
    els.createKey.hidden = true;
    els.manualSigner = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'Manual key reference' }),
      h('div', { class: 'df-disclosure__body' }, [boxRow({ label: 'Key reference', labelFor: idSigner, field: true, control: [els.signerRef] })]),
    ]));
    els.signNotice = h('div');
    els.signModeRow = boxRow({ label: 'Signing', labelFor: idSignMode, control: [els.signSelect] });
    els.signerRow = h('div', { class: 'df-stack df-stack--tight' }, [
      h('div', { class: 'df-cluster' }, [els.keyLabel, els.chooseKey, els.createKey]), els.manualSigner,
    ]);
    const signGroup = h('div', { class: 'df-stack df-stack--tight' }, [
      h('div', { class: 'df-boxed-list' }, [els.signModeRow]), els.signerRow, els.signNotice,
    ]);

    const idFormat = nextId('df-build-f');
    els.formatSelect = guardSelectPaging(h('select', { class: 'df-input', attrs: { id: idFormat }, on: { change: e => this.setFormat(e.target.value) } }, [
      h('option', { attrs: { value: 'dir' }, text: 'Bundle folder' }),
      h('option', { attrs: { value: 'tar' }, text: 'Compressed archive (.debark.tar.zst)' }),
    ]));
    els.optSbom = checkRow('Include a software inventory (SBOM)', null, true, e => { this.opts.sbom = e.target.checked; this.schedulePreview(); });
    const idRec = nextId('df-build-f');
    els.recommendsSelect = guardSelectPaging(h('select', { class: 'df-input', attrs: { id: idRec }, on: { change: e => this.setRecommends(e.target.value) } }, [
      h('option', { attrs: { value: 'target' }, text: 'Follow target settings' }),
      h('option', { attrs: { value: 'include' }, text: 'Include recommended packages' }),
      h('option', { attrs: { value: 'exclude' }, text: 'Exclude recommended packages' }),
    ]));
    els.optUpgrades = checkRow('Upgrade installed packages', null, false, e => { this.opts.upgrades = e.target.checked; this.schedulePreview(); });
    els.optUpdate = checkRow('Refresh indexes and re-resolve', 'Also upgrades installed packages.', false, e => {
      this.opts.update = e.target.checked; this.syncOptionEnablement(); this.schedulePreview();
    });
    els.optNoPrune = checkRow('Keep superseded files', null, false, e => { this.opts.no_prune = e.target.checked; this.schedulePreview(); });
    const idBackend = nextId('df-build-f');
    els.backendSelect = guardSelectPaging(h('select', { class: 'df-input', attrs: { id: idBackend }, on: { change: e => { this.opts.backend = e.target.value; this.schedulePreview(); } } }, [
      h('option', { attrs: { value: 'auto' }, text: 'Automatic' }), h('option', { attrs: { value: 'local' }, text: 'Local apt' }), h('option', { attrs: { value: 'container' }, text: 'Container' }),
    ]));
    els.advancedSummary = h('span', { text: 'Advanced options' });
    els.advanced = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary' }, [els.advancedSummary]),
      h('div', { class: 'df-disclosure__body' }, [h('div', { class: 'df-boxed-list' }, [
        boxRow({ label: 'Format', labelFor: idFormat, control: [els.formatSelect] }), els.optSbom,
        boxRow({ label: 'Recommended packages', labelFor: idRec, control: [els.recommendsSelect] }),
        els.optUpgrades, els.optUpdate, els.optNoPrune,
        boxRow({ label: 'Backend', labelFor: idBackend, control: [els.backendSelect] }),
      ])]),
    ]));
    els.advancedWarning = h('p', { class: 'df-text-sm', attrs: { role: 'status' } });
    els.advancedWarning.hidden = true;

    els.ackCheck = checkRow('I am responsible for redistribution of these packages.', null, false, e => {
      this.ack = e.target.checked; this.opts.acknowledge_redistribution = this.ack; this.schedulePreview();
    });
    const redistGroup = h('div', { class: 'df-stack df-stack--tight' }, [
      h('p', { class: 'df-text-sm df-text-secondary', text: 'Some packages restrict redistribution. Check their terms before sharing this bundle.' }), els.ackCheck,
    ]);
    els.commandPre = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
    els.commandError = h('div');
    els.commandCopy = btn('Copy command', 'ghost', () => {
      if (!this.commandDisplay) { this.toast('info', 'There is no runnable command yet.'); return; }
      this.copy(this.commandDisplay, 'Command');
    }, { small: true, icon: 'copy' });
    els.commandCopy.setAttribute('aria-disabled', 'true');
    els.commandDetails = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'Command' }),
      h('div', { class: 'df-disclosure__body df-stack df-stack--tight' }, [els.commandPre,
        h('p', { class: 'df-text-sm df-text-secondary', text: 'REDACTED replaces URL credentials and query strings. Restore those values before running a copied command.' }), els.commandCopy,
      ]),
    ]));
    els.startError = h('div');
    els.buildBtn = btn('Build bundle', 'primary', () => this.startBuild());
    const setupStatusID = nextId('df-build-setup-status');
    els.setupStatus = h('span', { class: 'df-actionbar__status', attrs: { id: setupStatusID, role: 'status', 'aria-live': 'polite' } });
    els.buildBtn.setAttribute('aria-describedby', setupStatusID);
    const setupBar = h('div', { class: 'df-actionbar' }, [els.setupStatus,
      btn('Back', 'ghost', () => this.ctx.go('picker')), els.buildBtn,
    ]);
    els.setupForm = h('div', { class: 'df-app__content df-stack df-stack--tight' }, [
      setupLede, els.jobNotice, els.environmentNotice, outputGroup, els.destination, signGroup, redistGroup,
      els.advanced, els.advancedWarning, els.commandDetails, els.commandError, els.startError,
    ]);
    els.setup = h('div', { class: 'df-screen-layout', attrs: { tabindex: '-1', role: 'region', 'aria-labelledby': idSetupHeading } }, [
      h('div', { class: 'df-screen-content' }, [els.setupForm]), setupBar,
    ]);

    /* ---------- run view ---------- */

    els.runStatus = h('div', { class: 'df-cluster', attrs: { 'aria-live': 'polite' } });
    els.runMeta = h('span', { class: 'df-text-sm df-text-secondary' });
    els.phaseRow = h('div', { class: 'df-cluster' });

    els.progressLabel = h('span', { text: `Starting ${CLI_BINARY}` });
    els.progressValue = h('span', { class: 'df-progress-label__value' });
    els.progressBar = h('div', { class: 'df-progress__bar' });
    els.progressTrack = h('div', {
      class: 'df-progress df-progress--indeterminate',
      attrs: { role: 'progressbar', 'aria-label': 'Build progress' },
    }, [els.progressBar]);
    // What the bar can and cannot say. Rewritten per run in renderProgress(),
    // because the honest sentence is different when AppInfo says this debark
    // cannot report progress at all.
    els.progressNote = h('p', { class: 'df-text-sm df-text-secondary', style: { margin: '0' } });
    els.progressSpinner = h('span', { class: 'df-spinner', attrs: { 'aria-hidden': 'true' } });
    els.progressSpinner.hidden = true;

    els.counters = h('div', { class: 'df-cluster' });
    // Built once and re-attached, never rebuilt: a popover that is open when
    // its anchor is thrown away leaves its document listeners behind.
    els.selectionsHelp = help('About the selection count', 'Selections resolved', [
      'An upper bound on the work, not a file count: a selection that is already in the local store downloads nothing.',
      'On the local apt backend this number arrives only after apt has finished downloading, inside one blocking step, so it is a summary rather than something to watch move.',
    ]);
    els.inflight = h('div', { class: 'df-stack df-stack--tight' });
    // No box: a stack of progress bars is not a list of rows, and .df-progress
    // draws no border, so this costs no rectangles.
    els.inflightWrap = h('div', { class: 'df-stack df-stack--tight' }, [
      h('h2', { class: 'df-group__title', text: 'Downloading now' }),
      els.inflight,
    ]);
    els.inflightWrap.hidden = true;

    // The progress block is deliberately unboxed. It was a .df-panel wrapping
    // a status chip, a phase row, a bar and a row of badges — none of which
    // needs a border to be read as one thing.
    const idRunHeading = nextId('df-build-h');
    els.runHeading = h('h1', { class: 'df-focus-target', text: 'Building the bundle', attrs: { id: idRunHeading } });
    els.progressBody = h('div', { class: 'df-stack df-stack--tight' }, [
      h('div', { class: 'df-progress-label' }, [els.progressSpinner, els.progressLabel, els.progressValue]),
      els.progressTrack, els.progressNote,
    ]);
    const progressBlock = h('div', { class: 'df-stack' }, [
      els.runHeading,
      h('div', { class: 'df-cluster' }, [els.runStatus, h('div', { class: 'df-spacer' }), els.runMeta]),
      els.phaseRow,
      els.progressBody,
      els.counters,
      els.inflightWrap,
    ]);

    els.outcome = h('div', { class: 'df-stack' });
    els.warnings = h('div', { class: 'df-stack df-stack--tight' });
    els.warningsMore = h('p', { class: 'df-text-sm df-text-secondary' });
    els.warningsMore.hidden = true;
    const warningsTitleId = nextId('df-build-g');
    els.warningsWrap = h('section', {
      class: 'df-stack df-stack--tight',
      attrs: { 'aria-labelledby': warningsTitleId },
    }, [
      h('h2', { class: 'df-group__title', text: 'Warnings, as they arrive', attrs: { id: warningsTitleId } }),
      els.warnings,
      els.warningsMore,
    ]);
    els.warningsWrap.hidden = true;

    els.runCommandPre = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
    const runCommandPanel = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'The command that is running' }),
      h('div', { class: 'df-disclosure__body df-stack df-stack--tight', style: { padding: 'var(--space-3)' } }, [
        els.runCommandPre,
        h('div', { class: 'df-cluster' }, [
          btn('Copy the command', 'secondary', () => this.copy(argvDisplay(this.command) || this.commandDisplay, 'The command'), { small: true, icon: 'copy' }),
        ]),
      ]),
    ]));

    els.logCount = h('span', { class: 'df-badge df-badge--count', text: '0' });
    els.logPre = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
    els.logNotice = h('p', { class: 'df-text-sm df-text-secondary' });
    els.logDrawer = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary' }, [
        'Raw event log',
        h('span', { class: 'df-spacer' }),
        els.logCount,
      ]),
      h('div', { class: 'df-disclosure__body' }, [
        els.logPre,
        h('div', { class: 'df-stack df-stack--tight', style: { padding: 'var(--space-3)' } }, [
          els.logNotice,
          h('div', { class: 'df-cluster' }, [
            btn('Copy the visible log', 'secondary', () => this.copyLog(), { small: true, icon: 'copy' }),
            btn('Refresh', 'ghost', () => this.reloadLog(), { small: true }),
          ]),
        ]),
      ]),
    ]));
    els.logDrawer.addEventListener('toggle', () => {
      this.logOpen = els.logDrawer.open;
      if (this.logOpen) this.reloadLog();
    });

    els.cancelBtn = btn('Cancel', 'danger', () => this.confirmCancel(), { icon: 'stop' });
    els.exportBtn = btn('Copy to drive…', 'primary', () => this.copyResult(), { icon: 'arrow' });
    els.exportBtn.hidden = true;
    els.againBtn = btn('Edit bundle', 'ghost', () => this.backToSetup());
    els.againBtn.hidden = true;

    // Two actions the error catalogue asks for by name and this bar did not
    // have. Every failure used to offer "Change the options" and nothing else,
    // so a row whose remedy is on another screen — install a container runtime
    // (2.4), create a signing key (3.2), fix a package name (3.8) — named that
    // remedy in its hint and then left the operator to find it.
    //
    // routeBtn is one contextual route, labelled and wired per outcome by
    // renderOutcome. rebuildBtn is "Build again", which the catalogue asks for
    // on the cancelled state (3.7) and on the incomplete one (3.3-3.5, where
    // it also says not to label it "Retry" alone, because for a digest
    // mismatch retrying is the wrong instinct).
    els.routeBtn = btn('', 'secondary', () => {
      if (this.routeAction) this.routeAction();
    });
    els.routeBtn.hidden = true;
    els.rebuildBtn = btn('Build again', 'secondary', () => {
      this.backToSetup();
      this.startBuild();
    });
    els.rebuildBtn.hidden = true;

    els.runStatusText = h('span', { class: 'df-actionbar__status' });
    const runBar = h('div', { class: 'df-actionbar' }, [
      els.runStatusText, els.routeBtn, els.againBtn, els.rebuildBtn, els.cancelBtn, els.exportBtn,
    ]);

    // The two drawers were two independently bordered rectangles. As slabs in
    // one box they are one, and .df-boxed-list__slab strips a nested
    // .df-disclosure's own border and radius so the box stays seamless.
    const detailGroup = group('Details', {});
    detailGroup.box.appendChild(h('div', { class: 'df-boxed-list__slab' }, [runCommandPanel]));
    detailGroup.box.appendChild(h('div', { class: 'df-boxed-list__slab' }, [els.logDrawer]));

    const runDetails = bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'Details' }),
      h('div', { class: 'df-disclosure__body df-stack df-stack--tight' }, [els.phaseRow, els.counters, detailGroup.el]),
    ]));
    els.runIdentity = h('p', { class: 'df-text-sm df-text-secondary' });
    els.runStatusNotice = h('div');
    els.run = h('div', {
      class: 'df-screen-layout',
      attrs: { tabindex: '-1', role: 'region', 'aria-labelledby': idRunHeading },
    }, [
      h('div', { class: 'df-screen-content' }, [h('div', { class: 'df-app__content df-stack df-stack--tight' }, [
        progressBlock, els.runIdentity, els.runStatusNotice, els.outcome, els.warningsWrap, runDetails,
      ])]),
      runBar,
    ]);
    els.run.hidden = true;

    /* ---------- cancel dialog ---------- */

    els.cancelConfirm = btn('Stop the build', 'danger', () => this.doCancel());
    els.cancelKeep = btn('Keep building', 'ghost', () => this.closeCancelDialog());
    // A <dialog> gets role="dialog" from the engine but no NAME unless one is
    // given: without aria-labelledby a screen reader announces "dialog" and
    // nothing else, which is precisely the case where the operator most needs
    // to know what they are being asked. Found by the accessibility pass.
    const cancelTitleID = 'df-build-cancel-title';
    const cancelBodyID = 'df-build-cancel-body';
    els.cancelDialog = h('dialog', {
      class: 'df-dialog',
      attrs: { 'aria-labelledby': cancelTitleID, 'aria-describedby': cancelBodyID },
    }, [
      h('div', { class: 'df-dialog__inner' }, [
        h('div', { class: 'df-dialog__header' }, [
          h('span', { class: 'df-dialog__title', text: 'Stop this build?', attrs: { id: cancelTitleID } }),
          h('button', {
            class: 'df-dialog__close', attrs: { type: 'button', 'aria-label': 'Close' },
            on: { click: () => this.closeCancelDialog() },
          }, [icon('danger')]),
        ]),
        h('div', { class: 'df-dialog__body df-stack df-stack--tight', attrs: { id: cancelBodyID } }, [
          h('p', { text: 'Completed downloads are kept for the next build. The output may be partial and must not be installed.' }),
          h('ul', { class: 'df-text-md', style: { 'padding-left': 'var(--space-5)', margin: '0' } }, [
            h('li', { text: 'Build again to finish the bundle.' }),
          ]),
        ]),
        h('div', { class: 'df-dialog__footer' }, [
          els.cancelKeep,
          els.cancelConfirm,
        ]),
      ]),
    ]);
    // The `close` event is the one path every dismissal goes through — the
    // buttons, and Escape, which the engine handles without telling us. Focus
    // restoration has to live here or Escape drops it on the floor.
    els.cancelDialog.addEventListener('close', () => {
      els.cancelDialog.classList.remove('df-dialog--fallback');
      this.restoreCancelFocus();
    });
    const keyTitle = nextId('df-build-key-title');
    els.keyCommand = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
    els.keyKeep = btn('Cancel', 'ghost', () => els.keyDialog.close());
    els.keyDialog = h('dialog', { class: 'df-dialog', attrs: { 'aria-labelledby': keyTitle } }, [
      h('div', { class: 'df-dialog__inner' }, [
        h('div', { class: 'df-dialog__header' }, [h('h2', { class: 'df-dialog__title', text: 'Create a signing key?', attrs: { id: keyTitle } })]),
        h('div', { class: 'df-dialog__body df-stack df-stack--tight' }, [
          h('p', { text: 'Run the existing system-check action below. Keep the private key on this machine; recipients need its public key through a trusted channel.' }), els.keyCommand,
        ]),
        h('div', { class: 'df-dialog__footer' }, [els.keyKeep, btn('Create key', 'primary', () => { els.keyDialog.close(); this.createSigningKey(); })]),
      ]),
    ]);
    els.keyDialog.addEventListener('close', () => {
      if (!this.visible) return;
      (els.createKey.hidden ? els.chooseKey : els.createKey).focus();
    });

    // tabindex="-1" so focus restoration always has somewhere real to land.
    // "Focus must never fall on <body>" is a P1 requirement, and the last
    // resort in that chain has to be focusable for the chain to end.
    // .df-app__content is the screen's own readable column. The shell hands a
    // bare .df-app__pane with no padding and no measure, on purpose: a step is
    // a form, not a dashboard, and four controls stretched across 1600px is
    // what a dashboard looks like.
    // .df-app__content is a plain block on purpose. Adding .df-stack to it
    // makes it a flex column, and .df-stack sets `min-height: 0`, so every
    // child inside a bounded .df-app__pane shrinks below its content and the
    // whole screen collapses into a few overlapping pixels. The stack goes
    // INSIDE the column instead.
    els.page = h('div', {
      class: 'df-screen-layout',
      attrs: { tabindex: '-1' },
    }, [els.setup, els.run, els.cancelDialog, els.keyDialog]);
    this.root.appendChild(els.page);
    this.syncOptionEnablement();
    this.renderSignNotice();
    this.applyLifecycle(null);
    this.renderPhaseRow();
  },

  /* ---------------------------------------------------------- setup: state */

  refreshLifecycle() {
    const epoch = this.lifecycleEpoch;
    Promise.resolve().then(() => this.api().LifecycleStatus()).then(status => {
      if (this.els && epoch === this.lifecycleEpoch) this.applyLifecycle(status);
    }).catch(err => {
      if (!this.els || epoch !== this.lifecycleEpoch) return;
      this.applyLifecycle(null, { message: 'Job status could not be checked.', hint: 'Check again before editing the bundle.', details: String(err) });
    });
  },

  applyLifecycle(status, readError = null) {
    if (!this.els) return;
    if (status && ['build_running', 'export_running', 'stopping'].some(key => typeof status[key] !== 'boolean')) {
      readError = { message: 'Job status could not be checked.', hint: 'Check again before editing the bundle.' };
      status = null;
    }
    this.lifecycle = status;
    this.lifecycleReadError = readError;
    // A valid close-timeout notification can remain after cleanup has ended.
    // Its state flags are authoritative; only an unreadable status fails closed.
    const locked = !status || !!(readError || status.build_running || status.export_running || status.stopping);
    for (const control of this.els.setupForm.querySelectorAll('input,select,button')) control.disabled = locked;
    // View/navigation and explanations remain available. Backend guards still
    // reject a stale view that races the lifecycle event.
    this.els.editTarget.disabled = false;
    this.els.editSelection.disabled = false;
    this.els.commandCopy.disabled = false;
    this.els.createKey.disabled = locked || this.createKeyPending;
    clear(this.els.jobNotice);
    if (readError) {
      this.els.jobNotice.appendChild(banner('warning', readError.message, readError.hint, [
        btn('Check job status', 'secondary', () => this.refreshLifecycle(), { small: true }),
      ]));
    } else if (status?.error) {
      this.els.jobNotice.appendChild(this.errorBanner(status.error, 'The app could not close.'));
    }
    this.syncBuildEnablement();
  },

  refreshReadiness() {
    Promise.resolve().then(() => this.api().Readiness()).then(report => {
      if (this.els) this.applyReadiness(report);
    }).catch(() => {});
  },

  applyReadiness(report) {
    if (!this.els || !report) return;
    this.readiness = report;
    const keyCheck = (report.checks || []).find(c => c.id === 'signing-key');
    this.keyAction = keyCheck && keyCheck.action;
    this.els.createKey.hidden = !(this.keyAction && this.keyAction.runnable && !this.keyAction.elevated);
    clear(this.els.environmentNotice);
    this.els.systemCheck = null;
    if (report.can_build === false) {
      const blocker = (report.checks || []).find(c => c.status === 'problem' && c.severity === 'blocking');
      this.els.systemCheck = btn('System check', 'secondary', () => this.ctx.go('readiness', { returnTo: 'build', focusCheck: blocker?.id }), { small: true });
      this.els.environmentNotice.appendChild(banner('warning', blocker?.summary || 'A prerequisite is missing.', blocker?.remedy || 'Open System check to resolve it.', [this.els.systemCheck]));
    }
    this.syncBuildEnablement();
  },

  async chooseSigningKey() {
    if (this.lifecycle && (this.lifecycle.build_running || this.lifecycle.export_running || this.lifecycle.stopping)) return;
    try {
      const picked = await this.api().ChooseSigningKey();
      if (!this.els || picked.cancelled) return;
      if (picked.error) { clear(this.els.signNotice); this.els.signNotice.appendChild(this.errorBanner(picked.error, 'The key could not be chosen.')); return; }
      if (!picked.path) throw new Error('The key picker returned no key reference.');
      this.opts.signer_ref = picked.path;
      this.els.signerRef.value = picked.path;
      this.renderSignNotice();
      this.schedulePreview(0);
    } catch (err) {
      if (!this.els) return;
      clear(this.els.signNotice);
      this.els.signNotice.appendChild(banner('danger', 'The key picker failed.', String(err.message || err)));
    }
  },

  async createSigningKey() {
    if (this.createKeyPending || !this.keyAction?.runnable || this.keyAction.elevated) return;
    if (this.lifecycle && (this.lifecycle.build_running || this.lifecycle.export_running || this.lifecycle.stopping)) return;
    this.createKeyPending = true;
    this.els.createKey.disabled = true;
    this.els.createKey.textContent = 'Creating key…';
    try {
      const result = await this.api().RunReadinessAction('signing-key');
      if (!result || result.ok !== true) throw result?.error || new Error('The key action was not accepted.');
      // Accepted means started. The matching finished event supplies success.
    } catch (err) {
      this.createKeyPending = false;
      if (!this.els) return;
      this.els.createKey.disabled = false;
      this.els.createKey.textContent = 'Create key…';
      clear(this.els.signNotice);
      this.els.signNotice.appendChild(this.errorBanner(err, 'The key was not created.'));
    }
  },

  requestKeyCreation() {
    if (!this.keyAction?.runnable || this.keyAction.elevated || this.createKeyPending) return;
    this.keyActionDisplay = this.keyAction.display || argvDisplay(this.keyAction.command || []);
    this.els.keyCommand.textContent = this.keyActionDisplay;
    this.els.keyDialog.showModal();
    this.els.keyKeep.focus();
  },

  renderOptionSummary() {
    if (!this.els) return;
    const o = this.opts, changed = [];
    if (o.format !== 'dir') changed.push('archive');
    if (!o.sbom) changed.push('no SBOM');
    if (o.recommends !== null) changed.push(o.recommends ? 'include recommends' : 'exclude recommends');
    if (o.update) changed.push('refresh + upgrades');
    else if (o.upgrades) changed.push('upgrades');
    if (o.update && o.no_prune) changed.push('keep superseded files');
    if (o.backend !== 'auto') changed.push(o.backend + ' backend');
    this.els.advancedSummary.textContent = 'Advanced options' + (changed.length ? ' · ' + changed.join(', ') : '');
    const warnings = [];
    if (o.upgrades || o.update) warnings.push('Installed packages will also be upgraded.');
    if (o.update && o.no_prune) warnings.push('Superseded files will remain in the bundle.');
    if (o.backend === 'local') warnings.push('Local apt must support the selected target; the build checks this.');
    this.els.advancedWarning.hidden = !warnings.length;
    this.els.advancedWarning.textContent = warnings.join(' ');
    this.els.destination.textContent = o.output_dir
      ? (o.output_name ? 'Destination: ' + o.output_dir.replace(/[\\/]+$/, '') + '/' + o.output_name : 'Output folder: ' + o.output_dir + ' · Automatic name')
      : 'Choose where to save the bundle.';
  },

  setFormat(v) {
    this.opts.format = v;
    this.schedulePreview();
  },

  setSign(mode) {
    this.sign = mode;
    this.opts.no_sign = mode === 'unsigned';
    this.opts.signer_ref = mode === 'key' ? this.els.signerRef.value.trim() : '';
    // The key field is REMOVED in the other two modes rather than disabled: a
    // disabled control is a control the operator meets, cannot use and is
    // never told why. There is nothing to say here — the field simply does not
    // apply — so it goes.
    this.els.signerRow.hidden = mode !== 'key';
    this.renderSignNotice();
    this.syncBuildEnablement();
    this.schedulePreview();
  },

  // Three states, expressed by a three-option control. `null` is not `false`:
  // a target whose apt.conf turns recommends on gets them under `null` and
  // does not under `false`, so they are different builds.
  setRecommends(choice) {
    this.opts.recommends = choice === 'include' ? true : choice === 'exclude' ? false : null;
    this.schedulePreview();
  },

  renderSignNotice() {
    const slot = this.els.signNotice;
    clear(slot);
    // The default-key trap is one line on the row itself, not a bordered
    // banner. The unsigned choice keeps a banner: it is the one that changes
    // what the bundle can prove about itself.
    this.els.keyLabel.textContent = this.opts.signer_ref || 'No key chosen';
    this.els.keyLabel.title = this.opts.signer_ref || '';
    if (this.sign === 'default') slot.appendChild(banner('warning', 'Default key is not confirmed',
      'If no default key is configured, the bundle will be unsigned. Choose a key explicitly to require signing.'));
    if (this.sign === 'unsigned') {
      slot.appendChild(banner('warning', 'This bundle will be unsigned',
        'Recipients cannot authenticate its publisher. Verification requires an explicit unsigned override.'));
    }
  },

  // --no-prune means nothing without --update, so the row is not there without
  // it. A row that is absent needs no sentence explaining why it is greyed out,
  // and no disabled control for a keyboard operator to walk into.
  syncOptionEnablement() {
    const on = !!this.opts.update;
    this.els.optNoPrune.hidden = !on;
    if (!on && this.els.optNoPrune.inputEl.checked) {
      this.els.optNoPrune.inputEl.checked = false;
      this.opts.no_prune = false;
    }
  },

  syncBuildEnablement() {
    const els = this.els;
    if (!els) return;
    const reasons = [];
    /** The control to send the operator to for the first unmet requirement. */
    let firstFix = null;
    const lifecycle = this.lifecycle;
    if (this.lifecycleReadError) reasons.push(this.lifecycleReadError.message || 'job status is unavailable');
    else if (!lifecycle) reasons.push('checking active jobs');
    else if (lifecycle.stopping) reasons.push('waiting for cleanup');
    else if (lifecycle.build_running || lifecycle.export_running) reasons.push('wait for the active job or cancel it');
    if (this.readiness && this.readiness.can_build === false) { reasons.push('resolve the system check problem'); firstFix = firstFix || els.systemCheck; }
    if (this.targetSelected === false) { reasons.push('choose a target'); firstFix = firstFix || els.editTarget; }
    if (this.selectionTotal === 0) { reasons.push('choose packages'); firstFix = firstFix || els.editSelection; }
    if (!this.opts.output_dir) {
      reasons.push('choose an output folder');
      firstFix = firstFix || els.outputDir;
    }
    if (this.sign === 'key' && !this.opts.signer_ref) {
      reasons.push('choose a signing key');
      firstFix = firstFix || els.chooseKey;
    }
    if (!this.ack) {
      reasons.push('acknowledge redistribution');
      firstFix = firstFix || (els.ackCheck && els.ackCheck.inputEl) || null;
    }
    // The preview's own refusal counts as a reason, and did not before.
    //
    // PreviewCommand validates the spec now, so it can come back with an error
    // where it used to come back with a command. When it does, refreshPreview
    // clears the command block and puts the refusal in its place — but this
    // function computed `blocked` from local form state only, so the status
    // line went on saying "Ready. The command above is exactly what will run"
    // over an empty block, and the primary action stayed aria-disabled="false".
    // startBuild refuses it, so nothing dangerous happened; the one sentence
    // that exists to say why the action is unavailable was saying the
    // opposite. Photographed as 21-preview-refused.png.
    //
    // Validation failures and unavailable validation stay actionable even
    // while Command is closed. StartBuild still validates authoritatively.
    if (this.previewError) {
      reasons.push(this.previewError.message || 'review the validation error');
      firstFix = firstFix || els.commandError.querySelector('button') || els.advanced.querySelector('summary');
    }
    const blocked = reasons.length > 0;
    this.buildBlocked = blocked;
    this.buildBlockedFocus = firstFix;
    // aria-disabled, not `disabled`. `disabled` removes the screen's primary
    // action from the tab order, so a keyboard or screen-reader operator walks
    // straight past it and is never told it exists, let alone why it is
    // unavailable. The design system already styles
    // `.df-btn[aria-disabled="true"]` exactly like `:disabled`, so this costs
    // nothing visually; startBuild() enforces the refusal.
    els.buildBtn.setAttribute('aria-disabled', blocked ? 'true' : 'false');
    // "exactly what will run" was the load-bearing word and it is no longer
    // true: the line above is the command with any vendor-URL credentials and
    // query string replaced by REDACTED. What runs is unchanged; what is shown
    // is not, and this sentence is the one place the difference is stated at
    // the moment of committing to the build.
    els.setupStatus.textContent = blocked
      ? 'To build: ' + reasons[0] + '.'
      : 'Ready to build.';
  },

  chooseDirectory() {
    const api = this.api();
    if (!api || typeof api.ChooseDirectory !== 'function') {
      this.toast('warning', 'The folder picker is not available in this build.');
      return;
    }
    Promise.resolve(api.ChooseDirectory('Where should the bundle go?', this.opts.output_dir || ''))
      .then((res) => {
        if (!res) return;
        if (res.error) { this.toast('danger', res.error.message || 'The folder picker failed.'); return; }
        if (res.cancelled || !res.path) return;
        this.opts.output_dir = res.path;
        this.els.outputDir.value = res.path;
        this.syncBuildEnablement();
        this.schedulePreview(0);
      })
      .catch(() => this.toast('danger', 'The folder picker failed.'));
  },

  /* -------------------------------------------------------- setup: preview */

  schedulePreview(delay) {
    this.previewEpoch++;
    this.renderOptionSummary();
    this.syncBuildEnablement();
    if (this.previewTimer) clearTimeout(this.previewTimer);
    const ms = delay === undefined ? PREVIEW_DEBOUNCE_MS : delay;
    this.previewTimer = setTimeout(() => { this.previewTimer = 0; this.refreshPreview(); }, ms);
  },

  refreshPreview() {
    const api = this.api();
    const els = this.els;
    if (!els) return;
    if (!api || typeof api.PreviewCommand !== 'function') {
      els.commandPre.textContent = '';
      this.previewError = { message: 'Command validation is unavailable.', hint: 'Reopen the application and try again.' };
      this.syncBuildEnablement();
      return;
    }
    const epoch = this.previewEpoch;
    Promise.resolve(api.PreviewCommand(this.currentOptions()))
      .then((p) => {
        if (!this.els || this.previewEpoch !== epoch) return;
        clear(this.els.commandError);
        if (!p) return;
        if (p.error) {
          this.commandDisplay = '';
          this.els.commandPre.textContent = '';
          // Local required-field hints already have a focus path in the action
          // area. Backend errors for otherwise complete inputs stay outside
          // Command, with a direct route to any collapsed expert controls.
          if (this.opts.output_dir && (this.sign !== 'key' || this.opts.signer_ref) && this.ack) {
            this.els.commandError.appendChild(banner('warning', p.error.message || 'Review these options.', p.error.hint || null,
              [btn('Review options', 'ghost', () => { this.els.advanced.open = true; this.els.advanced.querySelector('summary').focus(); }, { small: true })]));
            this.els.commandError.appendChild(this.detailsDrawer(p.error));
          }
          this.els.commandCopy.setAttribute('aria-disabled', 'true');
          // Gate the primary action on the refusal, and say so in the status
          // line. See the note in syncBuildEnablement.
          this.previewError = p.error;
          this.syncBuildEnablement();
          return;
        }
        this.previewError = null;
        this.commandDisplay = p.display || argvDisplay(p.argv);
        this.els.commandPre.textContent = this.commandDisplay;
        this.els.commandCopy.setAttribute('aria-disabled', this.commandDisplay ? 'false' : 'true');
        this.syncBuildEnablement();
      })
      .catch((err) => {
        if (!this.els || this.previewEpoch !== epoch) return;
        clear(this.els.commandError);
        this.previewError = { message: 'Command validation failed.', hint: 'Change an option or reopen Bundle to retry.' };
        this.commandDisplay = '';
        this.els.commandPre.textContent = '';
        this.els.commandCopy.setAttribute('aria-disabled', 'true');
        this.els.commandError.appendChild(banner('danger', 'The command preview failed.', String(err && err.message ? err.message : err)));
        this.syncBuildEnablement();
      });
  },

  // `no_recommends` is deliberately absent. It is the deprecated
  // one-directional spelling; `recommends` wins outright wherever it is set,
  // and sending both is how the two eventually contradict each other.
  currentOptions() {
    const o = this.opts;
    return {
      output_dir: o.output_dir,
      output_name: o.output_name,
      format: o.format,
      signer_ref: this.sign === 'key' ? o.signer_ref : '',
      no_sign: this.sign === 'unsigned',
      sbom: !!o.sbom,
      recommends: o.recommends === true ? true : o.recommends === false ? false : null,
      upgrades: !!o.upgrades,
      update: !!o.update,
      no_prune: !!(o.update && o.no_prune),
      backend: o.backend,
      acknowledge_redistribution: !!this.ack,
    };
  },

  /* ------------------------------------------------------------ header data */

  refreshHeader() {
    const api = this.api();
    if (!api) return;
    if (typeof api.CurrentTarget === 'function') {
      Promise.resolve(api.CurrentTarget()).then((res) => {
        if (!this.els) return;
        const t = (res && res.target) || {};
        this.targetSelected = !!t.selected;
        clear(this.els.targetLine);
        if (t.selected) {
          appendAll(this.els.targetLine, ['Target: ', mono(t.label || t.id || 'unknown')]);
        } else {
          this.els.targetLine.textContent = 'No target has been chosen yet.';
        }
        this.syncBuildEnablement();
      }).catch(() => {});
    }
    if (typeof api.Selection === 'function') {
      Promise.resolve(api.Selection()).then((s) => {
        if (!this.els || !s) return;
        const bits = [];
        bits.push(formatCount(s.total) + (s.total === 1 ? ' item' : ' items'));
        if (s.url_count) bits.push(formatCount(s.url_count) + ' vendor URLs');
        if (s.file_count) bits.push(formatCount(s.file_count) + ' local .deb files');
        this.els.selectionSummary.textContent = bits.join(' · ');
        this.selectionTotal = s.total;
        this.syncBuildEnablement();
        if (s.total === 0 && !this.running && !this.finished) this.renderEmptySelection();
      }).catch(() => {});
    }
  },

  renderEmptySelection() {
    clear(this.els.startError);
    this.els.startError.appendChild(stateEl(this.ctx, 'empty',
      'Nothing is selected to build',
      'A bundle needs at least one package, vendor URL or local .deb. Pick what the offline machine needs, then come back.',
        [btn('Choose packages', 'secondary', () => this.ctx && this.ctx.go && this.ctx.go('picker'), { small: true })]));
  },

  /* -------------------------------------------------- status reconciliation */

  syncFromStatus() {
    const api = this.api();
    if (!api || typeof api.BuildStatus !== 'function') return;
    const epoch = this.epoch;
    const sequence = ++this.statusReadSeq;
    this.statusPending = true;
    if (this.els) this.els.exportBtn.hidden = true;
    return Promise.resolve().then(() => api.BuildStatus()).then((st) => {
      if (!this.els || this.epoch !== epoch || sequence !== this.statusReadSeq) return;
      if (!st || typeof st.running !== 'boolean' || typeof st.finished !== 'boolean') throw new Error('The build status response was incomplete.');
      const identity = status => JSON.stringify([status?.started_at, status?.target_id, status?.target_generation, status?.selection_revision]);
      if ((st.running || st.finished) && identity(st) !== identity(this.runStatus)) {
        // The log and summary belong to this captured run, even when a late
        // notification came from the preceding process.
        this.resetRunState();
      }
      this.statusPending = false;
      this.statusReadError = null;
      clear(this.els.runStatusNotice);
      this.runStatus = st;
      this.startedItems = st.item_count || 0;
      this.startedTarget = st.target_id || '';
      this.resultSummary = st.summary || null;
      this.running = !!st.running;
      this.finished = !!st.finished;
      this.phase = (st.progress && st.progress.phase) || (this.running ? 'starting' : 'idle');
      if (st.progress?.message) this.acc.lastMsg = st.progress.message;
      this.command = st.command || this.command;
      this.logTotal = st.event_count || 0;

      if (st.started_at) {
        const t = Date.parse(st.started_at);
        if (!isNaN(t)) this.startedAtMs = t;
      }
      if (st.finished_at) {
        const t = Date.parse(st.finished_at);
        if (!isNaN(t)) this.finishedAtMs = t;
      }

      const asFinished = {
        ok: !st.error,
        cancelled: !!st.cancelled,
        summary: st.summary || null,
        error: st.error || null,
        duration_ms: this.startedAtMs && this.finishedAtMs ? this.finishedAtMs - this.startedAtMs : 0,
      };
      this.outcome = this.finished ? classifyOutcome(asFinished) : '';
      if (this.finished) {
        this.phase = this.outcome === 'cancelled' ? 'cancelled'
          : this.outcome === 'failed' ? 'failed' : 'finished';
        this.acc.inflight.clear();
        this.acc.inflightOrder = [];
        if (this.els.cancelDialog?.open) this.closeCancelDialog();
        this.stopTicker();
      }

      if (this.running || this.finished) {
        this.showRun();
        // The accumulator cannot be reconstructed from BuildStatus — it holds
        // no per-file detail — so replay the retained ring instead. This is the
        // path taken when the operator navigates away and back mid-build.
        if (this.logTotal > this.acc.lastSeq) this.rehydrate(this.acc.lastSeq);
        else this.renderAll();
      }
      if (this.finished) {
        this.els.cancelBtn.hidden = true;
        this.els.againBtn.hidden = false;
        this.renderOutcome(asFinished);
      } else if (this.running) {
        this.els.exportBtn.hidden = true;
        this.els.cancelBtn.hidden = false;
        this.els.againBtn.hidden = true;
        this.startTicker();
      }
      this.renderRunCommand();
      this.refreshLifecycle();
    }).catch(error => {
      if (!this.els || this.epoch !== epoch || sequence !== this.statusReadSeq) return;
      this.statusPending = false;
      this.statusReadError = error;
      this.els.exportBtn.hidden = true;
      clear(this.els.runStatusNotice);
      this.els.runStatusNotice.appendChild(banner('warning', 'Build status could not be checked.', 'Check again before using this result.', [
        btn('Check build status', 'secondary', () => this.syncFromStatus(), { small: true }),
      ]));
    });
  },

  rehydrate(since = 0) {
    const api = this.api();
    if (!api || typeof api.BuildLog !== 'function') return;
    if (this.rehydrating) { this.rehydrateAgain = true; return; }
    this.rehydrating = true;
    const epoch = this.epoch;
    const sequence = ++this.rehydrateSeq;
    const step = (since) => {
      Promise.resolve(api.BuildLog(since, LOG_PAGE)).then((page) => {
        if (!this.els || sequence !== this.rehydrateSeq) return;
        if (!page || this.epoch !== epoch) { this.rehydrating = false; return; }
        this.logDropped = page.dropped || 0;
        this.logTotal = page.total || this.logTotal;
        for (const ev of page.events || []) {
          foldEvent(this.acc, ev);
          if (this.logOpen && (!ev.seq || ev.seq > this.logSeq)) this.pendingLogEvents.push(ev);
        }
        const next = page.next_seq || since;
        if (next > since && (page.events || []).length > 0) { step(next); return; }
        this.rehydrating = false;
        if (this.finished) { this.acc.inflight.clear(); this.acc.inflightOrder = []; }
        this.renderAll();
        this.flushLog();
        if (this.rehydrateAgain) {
          this.rehydrateAgain = false;
          this.rehydrate(this.acc.lastSeq);
        }
      }).catch(() => { if (sequence === this.rehydrateSeq) this.rehydrating = false; });
    };
    step(since);
  },

  /* ------------------------------------------------------------ build start */

  startBuild() {
    const api = this.api();
    // An aria-disabled button still receives clicks and Enter — that is the
    // trade for keeping it in the tab order. Refuse here, say why out loud, and
    // put the operator on the control that will unblock it.
    if (this.buildBlocked) {
      const why = this.els.setupStatus.textContent || 'Something above still needs filling in.';
      this.toast('warning', why);
      if (this.buildBlockedFocus && typeof this.buildBlockedFocus.focus === 'function') {
        try {
          this.buildBlockedFocus.focus();
        } catch (_) {
          /* a control that has since been removed is not worth throwing over */
        }
      }
      return;
    }
    // The button stays focusable while StartBuild is in flight (aria-busy, not
    // `disabled`), so the second press has to be refused here instead.
    if (this.starting || this.running) return;
    this.starting = true;
    this.epoch++;
    clear(this.els.startError);
    if (!api || typeof api.StartBuild !== 'function') {
      this.starting = false;
      this.els.startError.appendChild(banner('danger', `This build of ${APP_NAME} cannot run ${CLI_BINARY}.`,
        'That is a packaging fault, not something you did. Report it.'));
      return;
    }
    // aria-busy, not `disabled`: the screen's primary action must never leave
    // the tab order, not even for the second it takes StartBuild to answer.
    this.els.buildBtn.classList.add('is-loading');
    this.els.buildBtn.setAttribute('aria-busy', 'true');
    this.els.buildBtn.setAttribute('aria-disabled', 'true');
    this.resetRunState();
    // StartBuild/BuildStatus supply the immutable run identity. Never derive
    // it from a later selection read or a mutable target label.
    this.showRun();
    clear(this.els.outcome);
    this.els.outcome.appendChild(stateEl(this.ctx, 'loading', `Starting ${CLI_BINARY}`,
      'The first event usually arrives within a second or two.'));

    Promise.resolve(api.StartBuild(this.currentOptions()))
      .then((res) => {
        if (!this.els) return;
        this.starting = false;
        this.els.buildBtn.classList.remove('is-loading');
        this.els.buildBtn.removeAttribute('aria-busy');
        this.syncBuildEnablement();
        if (!res || res.ok !== true) {
          this.running = false;
          this.finished = false;
          this.showSetup();
          clear(this.els.outcome);
          this.els.startError.appendChild(this.errorBanner(res && res.error, 'The build did not start.'));
          this.refreshLifecycle();
          return;
        }
        clear(this.els.outcome);
        this.syncFromStatus();
      })
      .catch((err) => {
        if (!this.els) return;
        this.starting = false;
        this.running = false;
        this.finished = false;
        this.els.buildBtn.classList.remove('is-loading');
        this.els.buildBtn.removeAttribute('aria-busy');
        this.syncBuildEnablement();
        this.showSetup();
        clear(this.els.outcome);
        this.els.startError.appendChild(banner('danger', 'The build did not start.',
          String(err && err.message ? err.message : err)));
        this.refreshLifecycle();
      });
  },

  resetRunState() {
    this.rehydrateSeq++;
    this.rehydrating = false;
    this.rehydrateAgain = false;
    this.acc = newAcc();
    this.statusKey = '';
    this.phaseRowKey = '';
    this.outcome = '';
    this.resultSummary = null;
    this.runStatus = null;
    this.phase = 'starting';
    this.running = true;
    this.finished = false;
    this.cancelRequested = false;
    this.startedAtMs = Date.now();
    this.finishedAtMs = 0;
    this.logLines = [];
    this.logSeq = 0;
    this.logDropped = 0;
    this.logTotal = 0;
    this.pendingLogEvents = [];
    clear(this.els.logPre);
    clear(this.els.warnings);
    clear(this.els.outcome);
    this.els.warningsWrap.hidden = true;
    this.els.exportBtn.hidden = true;
    this.els.exportBtn.textContent = '';
    appendAll(this.els.exportBtn, [icon('arrow', 'df-btn__icon'), 'Copy to drive…']);
    this.els.againBtn.hidden = true;
    this.els.routeBtn.hidden = true;
    this.els.rebuildBtn.hidden = true;
    this.routeAction = null;
    this.els.cancelBtn.hidden = false;
    this.els.cancelBtn.setAttribute('aria-disabled', 'false');
    this.els.cancelBtn.removeAttribute('aria-busy');
    this.els.cancelBtn.textContent = '';
    appendAll(this.els.cancelBtn, [icon('stop', 'df-btn__icon'), 'Cancel']);
    this.startTicker();
  },

  /**
   * swapPane hides one half of this screen and shows the other.
   *
   * It exists because `hidden = true` on a subtree containing the focused
   * element blurs that element to `<body>`. Measured on WebKit2GTK 2.52.6 with
   * the real engine: pressing Enter on "Build the bundle" left
   * `document.activeElement === document.body`, so the next Tab started again
   * at the top of the document and a screen reader was told nothing about the
   * build having started. Coming back with "Change the options" did the same
   * thing in reverse. docs/accessibility.md §10 P1-6 is the same rule found in
   * a dialog; this is the second place it applies, and a step-based flow
   * creates it by construction — a pane swap IS a step transition.
   *
   * Focus goes to the incoming pane, which is a `role="region"` named by its
   * own `<h1>` through `aria-labelledby` — NOT to the heading itself, which is
   * what this did first and what the correction below is about.
   *
   * The severe half of the original fix was right and stands: focus no longer
   * ends on `<body>`. The half that was wrong was verified with a DOM readback,
   * which says where focus is and cannot say whether anyone was told. The
   * accessibility pass put four focus-target shapes side by side under Orca
   * on WebKit2GTK and
   * transcribed what was spoken: a named `<section>` and a `role="region"` div
   * both announce as a landmark; a plain button announces; **a bare
   * `<h1 tabindex="-1">` is silent**. The heading's own text appears 33 times
   * in that log, so Orca was seeing it and declining to speak it, not missing
   * it. Landing focus there moved the caret and told the operator nothing.
   *
   * The pattern is `shell.js`'s `paneFor()`, which has always made each screen
   * a named `role="region"` with `tabIndex = -1` — which is exactly why a
   * *step* transition speaks on this engine and a *pane swap* did not. It is
   * the same fix, applied one level down, and it is proven on this engine
   * rather than reasoned about.
   *
   * It only moves focus if focus was inside the pane being hidden — otherwise
   * it would yank the operator out of whatever they were doing when a
   * background event arrived.
   */
  swapPane(show, hide, heading) {
    const active = document.activeElement;
    const wasInside = !!(active && hide && hide.contains(active));
    hide.hidden = true;
    show.hidden = false;
    if (!wasInside) return;
    // `heading` is kept as the second choice rather than dropped: it is still
    // focusable, still inside the incoming pane, and still better than
    // `<body>` if a caller ever passes a pane that is not a region.
    const target = (show && show.isConnected) ? show
      : (heading && heading.isConnected) ? heading : this.els.page;
    if (!target) return;
    try {
      target.focus({ preventScroll: false });
    } catch (_) {
      try { target.focus(); } catch (__) { /* nothing left to focus */ }
    }
  },

  showRun() {
    this.swapPane(this.els.run, this.els.setup, this.els.runHeading);
  },

  showSetup() {
    this.swapPane(this.els.setup, this.els.run, this.els.setupHeading);
  },

  backToSetup() {
    this.showSetup();
    this.refreshHeader();
    this.refreshLifecycle();
    this.schedulePreview(0);
  },

  /* ----------------------------------------------------------- cancellation */

  confirmCancel() {
    const dlg = this.els.cancelDialog;
    // Remember the trigger ourselves. The engine restores focus on close only
    // while the previously focused element is still focusable, and this one is
    // not: a build that finishes while the dialog is open hides "Stop the
    // build", and focus then lands on <body> — no screen, no announcement, and
    // a Tab that starts again from the top of the document. Found by the
    // accessibility pass.
    this.cancelReturnFocus = document.activeElement;
    if (typeof dlg.showModal === 'function') {
      try {
        dlg.showModal();
        // showModal() focuses the first focusable descendant, which is the
        // header's close button. Put it on the dismissive action instead —
        // the same place the non-modal fallback below puts it, and the one
        // the ARIA practices call for on a destructive confirmation.
        this.els.cancelKeep.focus();
        return;
      } catch (_) { /* fall through */ }
    }
    // df-dialog--fallback, not df-dialog-overlay: the latter is a WRAPPER that
    // paints the backdrop around a dialog, and putting it on the dialog turned
    // the dialog itself into a full-screen backdrop.
    dlg.setAttribute('open', '');
    dlg.classList.add('df-dialog--fallback');
    this.els.cancelKeep.focus();
  },

  closeCancelDialog() {
    const dlg = this.els.cancelDialog;
    if (typeof dlg.close === 'function' && dlg.open) {
      try {
        dlg.close(); // the `close` listener restores focus
        return;
      } catch (_) { /* fall through to the attribute form */ }
    }
    dlg.removeAttribute('open');
    dlg.classList.remove('df-dialog--fallback');
    this.restoreCancelFocus();
  },

  /**
   * restoreCancelFocus puts focus back somewhere real after the confirmation
   * closes: the control that opened it if it is still there, else whichever
   * action has replaced it in the run bar, else the screen's own pane. Never
   * <body> — that is how a keyboard operator loses their place entirely.
   */
  restoreCancelFocus() {
    // Only ever moves focus for a dismissal we opened. Without this guard a
    // stray `close` event would yank focus out from under the operator.
    if (!this.cancelReturnFocus) return;
    const usable = (el) => !!(el && el.isConnected && !el.hidden && !el.disabled &&
      el.getAttribute('aria-disabled') !== 'true' && !el.closest('[hidden]'));
    const target = [this.cancelReturnFocus, this.els.cancelBtn, this.els.againBtn, this.els.exportBtn, this.els.page]
      .find((el) => usable(el));
    this.cancelReturnFocus = null;
    if (!target) return;
    try {
      target.focus();
    } catch (_) {
      /* nothing left to focus is not worth throwing over */
    }
  },

  doCancel() {
    this.closeCancelDialog();
    if (this.cancelRequested) return;
    const api = this.api();
    this.cancelRequested = true;
    // aria-disabled rather than `disabled`: "Stopping…" is the only thing on
    // screen saying what happened to the press, and a control that leaves the
    // tab order takes that with it.
    this.els.cancelBtn.setAttribute('aria-disabled', 'true');
    this.els.cancelBtn.textContent = 'Stopping…';
    this.els.cancelBtn.setAttribute('aria-busy', 'true');
    Promise.resolve().then(() => api.CancelBuild()).then(result => {
      if (!result || result.ok !== true) throw result?.error || new Error('Cancellation was not accepted.');
    }).catch(err => {
      if (!this.els || !this.running) return;
      this.cancelRequested = false;
      this.els.cancelBtn.setAttribute('aria-disabled', 'false');
      this.els.cancelBtn.removeAttribute('aria-busy');
      this.els.cancelBtn.textContent = 'Cancel';
      this.els.outcome.appendChild(this.errorBanner(err, 'The build could not be stopped.'));
      this.syncFromStatus();
    });
    this.renderRunHeader();
  },

  /* ---------------------------------------------------------- event handlers */

  onStarted() {
    if (!this.els) return;
    this.epoch++;
    this.rehydrateSeq++;
    this.rehydrating = false;
    this.rehydrateAgain = false;
    this.syncFromStatus();
  },

  onProgress(p) {
    if (!this.els || !p || !this.running || this.finished) return;
    // Progress notifications have no run identity either. Coalesce reads of
    // the authoritative phase instead of accepting a preceding run's payload.
    if (this.progressStatusTimer) return;
    this.progressStatusTimer = setTimeout(() => {
      this.progressStatusTimer = 0;
      if (this.els && this.running && !this.finished) this.syncFromStatus();
    }, 120);
  },

  onEvent(ev) {
    if (!this.els || !ev || this.finished) return;
    if (this.liveLogTimer) return;
    this.liveLogTimer = setTimeout(() => {
      this.liveLogTimer = 0;
      if (this.els && !this.statusPending) this.rehydrate(this.acc.lastSeq);
    }, 120);
  },

  onFinished() {
    this.onStarted();
  },

  /* ------------------------------------------------------------- rendering */

  queueRender() {
    if (this.renderQueued) return;
    this.renderQueued = true;
    const run = () => {
      this.renderQueued = false;
      if (!this.els) return;
      this.renderAll();
      this.flushLog();
    };
    if (typeof requestAnimationFrame === 'function') requestAnimationFrame(run);
    else setTimeout(run, 16);
  },

  renderAll() {
    this.renderRunHeader();
    this.renderPhaseRow();
    this.renderProgress();
    this.renderCounters();
    this.renderInflight();
    this.renderWarnings();
    this.renderLogCount();
  },

  renderRunHeader() {
    const els = this.els;
    if (!els) return;
    // runStatus is an aria-live region, so it is rebuilt only when the state it
    // announces actually changes. Rebuilding it on the one-second tick would
    // have a screen reader announce "Building" once a second, forever.
    const key = (this.cancelRequested && this.running) ? 'stopping'
      : this.running ? 'building'
        : this.finished && this.outcome === 'finished' && this.resultSummary && !this.resultSummary.signed ? 'unsigned'
          : this.finished ? (this.outcome || this.phase) : this.phase;
    if (key !== this.statusKey) {
      this.statusKey = key;
      clear(els.runStatus);
      let chip;
      if (key === 'stopping') chip = status('pending', 'Stopping');
      else if (key === 'building') chip = status('pending', 'Building');
      else if (key === 'cancelled') chip = status('warning', 'Stopped');
      else if (key === 'failed') chip = status('danger', 'Failed');
      else if (key === 'incomplete') chip = status('warning', 'Built, but incomplete');
      else if (key === 'unsigned') chip = status('warning', 'Unsigned');
      else if (key === 'finished') chip = status('success', 'Finished');
      else chip = status('info', 'Idle');
      els.runStatus.appendChild(chip);
    }

    const bits = [];
    const ms = (this.finishedAtMs || Date.now()) - (this.startedAtMs || Date.now());
    bits.push(formatDuration(ms) + (this.running ? ' elapsed' : ''));
    if (this.startedItems) bits.push(formatCount(this.startedItems) + ' items requested');
    if (this.acc.backend) bits.push('backend: ' + this.acc.backend);
    if (this.running) {
      const gap = this.acc.lastEventAt ? Date.now() - this.acc.lastEventAt : 0;
      if (gap > STALL_AFTER_MS) {
        // Not an error. Progress is throttled to ~250 ms per file and events can
        // be dropped outright, so silence is normal; say how long it has been
        // rather than freezing or pretending something is wrong.
        bits.push('no events for ' + Math.round(gap / 1000) + ' s');
      }
    }
    els.runMeta.textContent = bits.join(' · ');
    els.runIdentity.textContent = this.startedTarget ? this.startedTarget + (this.startedItems ? ' · ' + formatCount(this.startedItems) + ' items requested' : '') : '';
    els.runHeading.textContent = this.running ? (this.cancelRequested ? 'Stopping build…' : 'Building bundle')
      : this.outcome === 'incomplete' ? 'Bundle incomplete'
        : this.outcome === 'cancelled' ? 'Build cancelled'
          : this.outcome === 'failed' ? 'Build failed' : 'Bundle ready';
    els.runStatus.hidden = this.finished;

    els.runStatusText.textContent = this.running
      ? (this.cancelRequested ? 'Waiting for cleanup…' : 'Completed downloads are kept if you cancel.')
      : this.outcome === 'incomplete' ? 'Incomplete — do not install.' : '';
  },

  renderPhaseRow() {
    const els = this.els;
    if (!els) return;
    if (this.phaseRowKey === this.phase) return;
    this.phaseRowKey = this.phase;
    const order = PHASES.map((p) => p[0]);
    let idx = order.indexOf(this.phase);
    if (this.phase === 'finished') idx = order.length;
    clear(els.phaseRow);
    for (let i = 0; i < PHASES.length; i++) {
      const [id, label] = PHASES[i];
      let node;
      if (idx > i) node = status('success', label);
      else if (idx === i) node = status('pending', label);
      else {
        node = h('span', { class: 'df-status' }, [icon('dot'), h('span', { text: label }),
          h('span', { class: 'df-visually-hidden', text: ' — not reached yet' })]);
      }
      els.phaseRow.appendChild(node);
    }
    if (this.phase === 'failed' || this.phase === 'cancelled') {
      els.phaseRow.appendChild(h('span', {
        class: 'df-text-sm df-text-secondary',
        text: this.phase === 'failed' ? 'stopped here' : 'stopped by you here',
      }));
    }
  },

  // THERE IS NO DENOMINATOR, and on two of the three backends there are barely
  // any events either. This is the whole reason the top bar is a phase
  // indicator: it is indeterminate for the entire run and says so, and the
  // only honest fraction on the screen is per-file byte progress drawn next to
  // the file's own name.
  //
  // Three honest states:
  //
  //   progress_events false  — no bar at all. Nothing will ever arrive to move
  //                            it. A spinner and a sentence naming the reason.
  //   running, events        — an indeterminate bar, the phase, and whatever
  //                            counters have turned up so far.
  //   finished               — full, coloured by the outcome.
  renderProgress() {
    const els = this.els;
    if (!els) return;
    els.progressBody.hidden = this.finished;
    const p = describeProgress(this.acc, this.phase, this.running);
    if (this.finished && OUTCOME_LABELS[this.outcome]) p.label = OUTCOME_LABELS[this.outcome];
    const blind = this.running && this.progressEventsKnown && !this.progressEvents;
    els.progressLabel.textContent = blind ? 'Building — no progress is reported' : p.label;
    els.progressValue.textContent = blind ? '' : (p.value || '');
    els.progressSpinner.hidden = !blind;

    const track = els.progressTrack;
    const outcome = this.finished ? (this.outcome || this.phase) : '';
    track.hidden = blind;
    if (blind) {
      els.progressNote.hidden = false;
      els.progressNote.textContent = 'This engine reports no live progress. The result will appear when the build finishes.';
      return;
    }
    if (!this.running && outcome === 'finished') {
      track.className = 'df-progress df-progress--success';
      track.setAttribute('aria-valuenow', '100');
      track.setAttribute('aria-valuemin', '0');
      track.setAttribute('aria-valuemax', '100');
      els.progressBar.style.width = '100%';
      els.progressNote.hidden = true;
    } else if (!this.running) {
      track.className = 'df-progress ' + (outcome === 'failed' ? 'df-progress--danger' : 'df-progress--warning');
      track.removeAttribute('aria-valuenow');
      track.removeAttribute('aria-valuemin');
      track.removeAttribute('aria-valuemax');
      els.progressBar.style.width = '100%';
      els.progressNote.hidden = true;
    } else {
      track.className = 'df-progress df-progress--indeterminate';
      track.removeAttribute('aria-valuenow');
      track.removeAttribute('aria-valuemin');
      track.removeAttribute('aria-valuemax');
      els.progressBar.style.width = '';
      els.progressNote.hidden = false;
      // A container build emits about eleven events for a whole run — no
      // fetch.file, no progress, not even apt.resolve — because the inner
      // process's events never cross back out. Silence there is normal and the
      // counters below simply stay empty; saying so is better than a bar that
      // implies something is being counted.
      els.progressNote.textContent = this.acc.eventCount === 0
        ? 'Waiting for progress from the build. Some phases report only when they finish.'
        : 'Overall progress is by phase; download progress is shown when available.';
    }
    track.setAttribute('aria-label', els.progressLabel.textContent);
  },

  renderCounters() {
    const els = this.els;
    if (!els) return;
    const acc = this.acc;
    const items = [];
    items.push(['Files fetched', formatCount(acc.filesDone)]);
    items.push(['Downloaded', formatBytes(acc.bytesFetched)]);
    if (acc.storeHits > 0) items.push(['Reused from the store', formatCount(acc.storeHits) + ' (' + formatBytes(acc.storeHitBytes) + ')']);
    if (acc.selections >= 0) {
      items.push([acc.selectionsAreBaseClosure ? 'Base packages resolved' : 'Selections resolved',
        formatCount(acc.selections)]);
    }
    if (acc.retries > 0) items.push(['Retries', formatCount(acc.retries)]);
    if (acc.warningTotal > 0) items.push(['Warnings', formatCount(acc.warningTotal)]);
    items.push(['Events', formatCount(Math.max(this.logTotal, acc.eventCount))]);

    // The help anchor is detached first so clearing the cluster never orphans
    // an open popover.
    if (els.selectionsHelp.parentNode === els.counters) els.counters.removeChild(els.selectionsHelp);
    clear(els.counters);
    for (const [k, v] of items) {
      els.counters.appendChild(h('span', { class: 'df-badge' }, [k + ': ', h('span', { class: 'df-mono', text: v })]));
    }
    // The caveat that used to be a permanent sentence under the badges.
    if (acc.selections >= 0) els.counters.appendChild(els.selectionsHelp);
  },

  renderInflight() {
    const els = this.els;
    if (!els) return;
    const live = this.acc.inflightOrder.map((u) => this.acc.inflight.get(u)).filter(Boolean);
    els.inflightWrap.hidden = live.length === 0;
    clear(els.inflight);
    const shown = live.slice(0, MAX_INFLIGHT_ROWS);
    for (const rec of shown) {
      const known = typeof rec.total === 'number' && rec.total > 0;
      const frac = known ? Math.max(0, Math.min(1, rec.bytes / rec.total)) : -1;
      const name = rec.name || rec.url;
      const valueText = known
        ? formatBytes(rec.bytes) + ' of ' + formatBytes(rec.total) + ' · ' + Math.round(frac * 100) + '%'
        : formatBytes(rec.bytes) + ' · size not sent by the archive';
      const bar = h('div', { class: 'df-progress__bar' });
      const track = h('div', {
        class: known ? 'df-progress' : 'df-progress df-progress--indeterminate',
        attrs: { role: 'progressbar', 'aria-label': 'Downloading ' + name },
      }, [bar]);
      if (known) {
        bar.style.width = (frac * 100).toFixed(1) + '%';
        track.setAttribute('aria-valuemin', '0');
        track.setAttribute('aria-valuemax', '100');
        track.setAttribute('aria-valuenow', String(Math.round(frac * 100)));
      }
      const label = h('div', { class: 'df-progress-label' }, [
        h('span', { class: 'df-mono df-truncate', text: name, attrs: { title: rec.url } }),
        h('span', { class: 'df-progress-label__value', text: valueText }),
      ]);
      const kids = [label, track];
      if (rec.attempt > 1) {
        kids.push(h('span', {
          class: 'df-text-sm df-text-secondary',
          text: 'Retrying — attempt ' + rec.attempt + (rec.attempts ? ' of ' + rec.attempts : '') + '. The byte count restarts on a retry.',
        }));
      }
      els.inflight.appendChild(h('div', { class: 'df-stack df-stack--tight', style: { 'min-width': '0' } }, kids));
    }
    if (live.length > shown.length) {
      els.inflight.appendChild(h('p', {
        class: 'df-text-sm df-text-secondary',
        text: 'and ' + (live.length - shown.length) + ' more downloading concurrently',
      }));
    }
  },

  renderWarnings() {
    const els = this.els;
    if (!els) return;
    const list = this.acc.warnings;
    els.warningsWrap.hidden = list.length === 0;
    const limit = Math.min(list.length, MAX_WARNINGS_RENDERED);
    for (let i = 0; i < limit; i++) {
      const w = list[i];
      if (!w.dirty && w.node) continue;
      const text = w.count > 1 ? w.text + ' (seen ' + w.count + ' times)' : w.text;
      const role = w.severity === 'danger' ? 'danger' : w.severity === 'info' ? 'info' : 'warning';
      const node = banner(role, w.title, text);
      if (w.node && w.node.parentNode === els.warnings) els.warnings.replaceChild(node, w.node);
      else els.warnings.appendChild(node);
      w.node = node;
      w.dirty = false;
    }
    if (list.length > MAX_WARNINGS_RENDERED) {
      els.warningsMore.hidden = false;
      els.warningsMore.textContent = 'and ' + (list.length - MAX_WARNINGS_RENDERED) +
        ' more distinct warnings — open the raw event log to read them all.';
    } else {
      els.warningsMore.hidden = true;
    }
  },

  /* ------------------------------------------------------------- the drawer */

  renderLogCount() {
    if (!this.els) return;
    this.els.logCount.textContent = formatCount(Math.max(this.logTotal, this.acc.eventCount));
  },

  // A BuildEvent's `raw` field is the exact NDJSON line, when the backend has
  // one. The current projection in internal/app does not populate it, so this
  // falls back to re-encoding the decoded fields in the same shape. Either way
  // the result is set as TEXT, never as markup.
  lineOf(ev) {
    if (ev && typeof ev.raw === 'string' && ev.raw !== '') return ev.raw;
    const out = { ts: ev.ts || '', type: ev.type || '', level: ev.level || undefined, msg: ev.msg || undefined, attrs: ev.attrs || undefined };
    try { return JSON.stringify(out); } catch (_) { return String(ev && ev.type); }
  },

  appendLogLine(ev) {
    const text = this.lineOf(ev);
    const level = ev && ev.level;
    const cls = 'df-log__line' + (level === 'error' ? ' df-log__line--error' : level === 'warn' ? ' df-log__line--warn' : '');
    const span = h('span', { class: cls, text: text });
    this.els.logPre.appendChild(span);
    this.logLines.push(text);
    while (this.logLines.length > MAX_LOG_LINES) {
      this.logLines.shift();
      if (this.els.logPre.firstChild) this.els.logPre.removeChild(this.els.logPre.firstChild);
    }
  },

  flushLog() {
    if (!this.logOpen || !this.els || this.pendingLogEvents.length === 0) return;
    const pre = this.els.logPre;
    const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
    const batch = this.pendingLogEvents;
    this.pendingLogEvents = [];
    for (const ev of batch) {
      if (ev.seq && ev.seq <= this.logSeq) continue;
      if (ev.seq) this.logSeq = ev.seq;
      this.appendLogLine(ev);
    }
    this.renderLogNotice();
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  },

  // The drawer never holds the whole log. It asks the backend for the tail —
  // MAX_LOG_LINES lines — and then follows live events from there.
  reloadLog() {
    const api = this.api();
    if (!api || typeof api.BuildLog !== 'function' || !this.els) return;
    const epoch = this.epoch;
    Promise.resolve(api.BuildLog(0, 1)).then((head) => {
      if (!this.els || !head || this.epoch !== epoch) return;
      const total = head.total || 0;
      this.logTotal = Math.max(this.logTotal, total);
      this.logDropped = head.dropped || 0;
      const from = Math.max(0, total - MAX_LOG_LINES);
      return Promise.resolve(api.BuildLog(from, MAX_LOG_LINES)).then((page) => {
        if (!this.els || !page || this.epoch !== epoch) return;
        clear(this.els.logPre);
        this.logLines = [];
        this.logSeq = from;
        for (const ev of page.events || []) {
          if (ev.seq) this.logSeq = Math.max(this.logSeq, ev.seq);
          this.appendLogLine(ev);
        }
        this.logDropped = page.dropped || this.logDropped;
        this.logTotal = Math.max(this.logTotal, page.total || 0);
        this.renderLogNotice();
        this.renderLogCount();
        this.els.logPre.scrollTop = this.els.logPre.scrollHeight;
      });
    }).catch(() => {});
  },

  renderLogNotice() {
    const bits = [];
    const shown = this.logLines.length;
    bits.push('Showing the most recent ' + formatCount(shown) + ' of ' + formatCount(Math.max(this.logTotal, shown)) + ' events.');
    if (this.logDropped > 0) {
      bits.push(formatCount(this.logDropped) + ' older events have fallen out of the in-memory ring and are not here. ' +
        'The complete record is in the bundle’s evidence.json.');
    }
    bits.push('Events are re-encoded from the engine stream; JSON key order may differ.');
    this.els.logNotice.textContent = bits.join(' ');
  },

  copyLog() {
    if (!this.logLines.length) { this.toast('info', 'There is nothing in the log yet.'); return; }
    const header = '# ' + this.logLines.length + ' of ' + Math.max(this.logTotal, this.logLines.length) +
      ' events' + (this.logDropped > 0 ? ', ' + this.logDropped + ' older events dropped from the ring' : '');
    this.copy(header + '\n' + this.logLines.join('\n') + '\n', 'The visible log');
  },

  renderRunCommand() {
    if (!this.els) return;
    const text = argvDisplay(this.command) || this.commandDisplay || '';
    this.els.runCommandPre.textContent = text;
  },

  /* -------------------------------------------------------------- outcomes */

  errorBanner(err, fallbackTitle) {
    const e = err || {};
    const title = e.message || fallbackTitle || 'Something went wrong.';
    const kids = [];
    if (e.hint) kids.push(h('span', { text: e.hint }));
    const cls = e.exit_class && EXIT_CLASSES[e.exit_class];
    if (cls) {
      kids.push(h('br'));
      kids.push(h('span', {
        class: 'df-text-sm',
        text: `${CLI_BINARY} class ` + e.exit_class + ' (exit ' + cls.code + '): ' + cls.text,
      }));
    }
    return banner('danger', title, kids.length ? kids : null);
  },

  /** Labels and wires the single contextual route, or hides it. */
  setRoute(label, run) {
    const btnEl = this.els && this.els.routeBtn;
    if (!btnEl) return;
    this.routeAction = run || null;
    btnEl.hidden = !label;
    if (label) btnEl.textContent = label;
  },

  /**
   * routeForError puts the remedy the message names one click away.
   *
   * Every row in docs/dev/error-catalogue.md whose hint points at another
   * screen was, before this, a sentence with no route: 2.4 names the readiness
   * screen's container-runtime row and asks for "open readiness" and "change
   * target"; 3.2 names the readiness screen's signing-key action; 3.8 says to
   * check the names against the catalogue, which is the picker.
   *
   * Routing is on the exit CLASS, never on the message text — the class is the
   * frozen 0-7 table in docs/dev/cli-surface.md 6, and a rule that read the
   * sentence would break the first time a wording changed. There is one route,
   * not three: a bar of five buttons is how a screen stops being read.
   */
  routeForError(err, summary) {
    const go = (id) => () => { if (this.ctx && this.ctx.go) this.ctx.go(id); };
    const cls = (err && err.exit_class) || '';
    if (cls === 'environment') {
      // 2.4 and 3.9: neither backend can run here. Readiness carries the
      // container-runtime row and its remedy; 3.2's missing signing key is
      // usage-class and handled below.
      this.setRoute('Open readiness', go('readiness'));
      return;
    }
    if (cls === 'usage') {
      // 3.2, and every other "you asked for something that is not there".
      // Readiness is where a signing key gets created.
      this.setRoute('Open readiness', go('readiness'));
      return;
    }
    if (cls === 'resolution') {
      // 3.8: a name the target's archive does not carry. The picker is where
      // it is fixed, and the tray already marks an unknown name.
      this.setRoute('Back to packages', go('picker'));
      return;
    }
    if (cls === 'incomplete' || (summary && summary.fetch_failed && summary.fetch_failed.length)) {
      // 3.3-3.5: the inputs that failed are vendor URLs, added in the picker.
      this.setRoute('Back to packages', go('picker'));
      return;
    }
    this.setRoute(null, null);
  },

  // Four outcomes, and the third one is the one that gets misread.
  //
  //   finished     — a clean bundle
  //   incomplete   — a bundle exists but is SHORT (exit class 3). The result
  //                  object is populated AND an error was returned. It is
  //                  neither a success nor a failure and is rendered as its own
  //                  thing, naming exactly what is missing.
  //   failed       — no usable bundle
  //   stopped      — the operator cancelled; debark has no class for this
  renderOutcome(f) {
    const els = this.els;
    if (!els) return;
    clear(els.outcome);

    const summary = f.summary || null;
    this.resultSummary = summary;
    const err = f.error || null;
    const kind = this.outcome || classifyOutcome(f);
    const cancelled = kind === 'cancelled';
    const incomplete = kind === 'incomplete';

    this.setRoute(null, null);
    els.rebuildBtn.hidden = true;

    if (cancelled) {
      els.outcome.appendChild(banner('warning', 'Build cancelled',
        'The output may be partial. Do not install it. Completed downloads are kept for the next build.'));
      els.outcome.appendChild(this.leftBehindPanel());
      els.againBtn.hidden = false;
      // "Offer build again" — docs/dev/error-catalogue.md 3.7. Restarting is
      // cheap: everything already downloaded is in the local store.
      els.rebuildBtn.hidden = false;
      return;
    }

    if (incomplete) {
      els.outcome.appendChild(banner('warning', 'Incomplete — do not install',
        'Required items are missing. Correct the inputs below and build again.'));
      els.outcome.appendChild(this.missingPanel(summary));
      if (summary) els.outcome.appendChild(this.summaryPanel(summary, 'incomplete'));
      if (err) els.outcome.appendChild(this.detailsDrawer(err));
      els.exportBtn.hidden = !(summary && summary.bundle_path);
      if (!els.exportBtn.hidden) {
        els.exportBtn.textContent = '';
        appendAll(els.exportBtn, [icon('arrow', 'df-btn__icon'), 'Copy incomplete bundle…']);
      }
      els.againBtn.hidden = false;
      // Not labelled "Retry" on purpose: for a digest mismatch retrying is the
      // wrong instinct, and the list above now says which failure this was.
      els.rebuildBtn.hidden = false;
      this.routeForError(err, summary);
      return;
    }

    if (kind === 'failed') {
      els.outcome.appendChild(this.errorBanner(err, 'The build failed.'));
      // For exit 5 and 6 the result document is the only place the detail
      // exists, because a failing build writes nothing useful to stderr.
      if (summary && ((summary.unresolved && summary.unresolved.length) || (summary.fetch_failed && summary.fetch_failed.length))) {
        els.outcome.appendChild(this.missingPanel(summary));
      }
      if (err) els.outcome.appendChild(this.detailsDrawer(err));
      els.exportBtn.hidden = true;
      els.againBtn.hidden = false;
      els.rebuildBtn.hidden = !(err && err.retryable);
      this.routeForError(err, summary);
      return;
    }

    if (!summary || !summary.bundle_path) {
      els.outcome.appendChild(banner('warning', 'No bundle path was reported', 'Review Details before using this result.'));
    } else if (!summary.signed) {
      els.outcome.appendChild(banner('warning', 'This bundle is unsigned', 'Recipients cannot authenticate its publisher. Verification requires an explicit unsigned override.'));
    }
    if (summary) els.outcome.appendChild(this.summaryPanel(summary, 'success'));
    els.exportBtn.hidden = !(summary && summary.bundle_path);
    els.againBtn.hidden = false;
  },

  leftBehindPanel() {
    const b = titledBlock('What the stopped run left behind');
    b.body.appendChild(h('ul', { class: 'df-text-md', style: { 'padding-left': 'var(--space-5)', margin: '0' } }, [
      h('li', { text: 'Downloads that completed are in the local store and will be reused, so restarting is cheap.' }),
      h('li', { text: 'The output folder may hold a partial, unsigned bundle. Do not copy it to a target; build over it or delete it.' }),
      h('li', { text: 'Nothing was written to the target. This machine is the only one that changed.' }),
    ]));
    return b.el;
  },

  missingPanel(summary) {
    const s = summary || {};
    const rows = [];
    const unresolved = s.unresolved || [];
    const fetchFailed = s.fetch_failed || [];

    if (unresolved.length) {
      rows.push(h('h3', { class: 'df-group__title', text: 'Could not be resolved (' + formatCount(unresolved.length) + ')' }));
      rows.push(this.stringList(unresolved));
      rows.push(h('p', {
        class: 'df-text-sm df-text-secondary',
        text: 'apt could not satisfy these. An external .deb with a dependency the target does not have lands here.',
      }));
    }
    if (fetchFailed.length) {
      rows.push(h('h3', { class: 'df-group__title', text: 'Could not be downloaded (' + formatCount(fetchFailed.length) + ')' }));
      rows.push(this.failedInputList(fetchFailed));
      rows.push(h('p', {
        class: 'df-text-sm df-text-secondary',
        text: 'Correct the input, or add the file from disk instead, and build again — whatever already '
          + 'downloaded is kept in the local store and is not fetched twice.',
      }));
    }
    if (!rows.length) {
      rows.push(h('p', { class: 'df-text-md', text: `${CLI_BINARY} reported the bundle as incomplete but named nothing specific. The raw event log below is the place to look.` }));
    }
    if (s.truncated) {
      rows.push(h('p', {
        class: 'df-text-sm df-text-secondary',
        text: 'These lists are clamped at 500 entries. The complete record is in the bundle’s evidence.json.',
      }));
    }
    const block = titledBlock('What is missing from this bundle');
    appendAll(block.body, rows);
    return block.el;
  },

  /**
   * failedInputList is stringList with the reason attached to each entry.
   *
   * docs/dev/error-catalogue.md 3.3-3.5 is the most demanding row in that
   * document and this is the part of it a bare list cannot satisfy: three
   * inputs can fail in one build for three unrelated reasons — the host was
   * not there, the certificate is not trusted, the SHA-256 did not match —
   * and "these URLs failed after their retries" is true of all three and
   * actionable for none. The reason has to sit beside the input it belongs to,
   * because that is the unit the operator acts on.
   *
   * The reason comes from `acc.fetchReasons`, read verbatim out of the `error`
   * attribute of the warn-level `input.external` event that named this input.
   * Nothing here parses it. When the event did not arrive — a summary replayed
   * without its log, or a debark that did not emit one — the entry renders
   * exactly as it did before and the sentence below the list still applies.
   */
  failedInputList(items) {
    const reasons = (this.acc && this.acc.fetchReasons) || new Map();
    const box = h('div', { class: 'df-boxed-list' });
    const lines = [];
    for (const item of items) {
      const input = String(item);
      const reason = reasons.get(input) || '';
      lines.push(reason ? input + '\n    ' + reason : input);
      const text = h('span', { class: 'df-boxed-list__text' }, [
        h('span', { class: 'df-boxed-list__label df-mono df-truncate', text: input }),
      ]);
      if (reason) {
        text.appendChild(h('span', { class: 'df-boxed-list__description', text: reason }));
      }
      box.appendChild(h('div', { class: 'df-boxed-list__row' }, [text]));
    }
    const copy = btn('Copy this list', 'secondary', () => this.copy(lines.join('\n') + '\n', 'The list'), { small: true, icon: 'copy' });
    return h('div', { class: 'df-stack df-stack--tight' }, [box, h('div', { class: 'df-cluster' }, [copy])]);
  },

  stringList(items) {
    const pre = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
    for (const item of items) {
      pre.appendChild(h('span', { class: 'df-log__line', text: String(item) }));
    }
    const copy = btn('Copy this list', 'secondary', () => this.copy(items.map(String).join('\n') + '\n', 'The list'), { small: true, icon: 'copy' });
    return h('div', { class: 'df-stack df-stack--tight' }, [pre, h('div', { class: 'df-cluster' }, [copy])]);
  },

  // One box, not a card full of cards: the path, the signature and the numbers
  // are three rows of the same object.
  summaryPanel(summary, kind) {
    const s = summary, stats = s.stats || {};
    const block = h('div', { class: 'df-stack df-stack--tight' });
    block.appendChild(h('p', { class: 'df-mono', text: s.bundle_path || 'No output path was reported.' }));
    block.appendChild(h('div', { class: 'df-cluster' }, [
      status(kind === 'incomplete' ? 'warning' : 'success', kind === 'incomplete' ? 'Incomplete' : 'Complete'),
      status(s.signed ? 'success' : 'warning', s.signed ? 'Signed' : 'Unsigned'),
      btn('Open folder', 'secondary', () => this.reveal(s.bundle_path), { small: true, icon: 'folder' }),
      btn('Copy path', 'ghost', () => this.copy(s.bundle_path || '', 'Path'), { small: true, icon: 'copy' }),
    ]));
    const facts = [];
    if (s.bundle_id) facts.push(['Bundle ID', s.bundle_id]);
    if (stats.package_count) facts.push(['Packages', formatCount(stats.package_count)]);
    if (stats.bytes) facts.push(['Bundle size', formatBytes(stats.bytes)]);
    if (stats.downloaded_bytes) facts.push(['Downloaded', formatBytes(stats.downloaded_bytes)]);
    if (stats.added) facts.push(['Added', formatCount(stats.added)]);
    if (stats.removed) facts.push(['Removed', formatCount(stats.removed)]);
    if (stats.unchanged) facts.push(['Unchanged', formatCount(stats.unchanged)]);
    if (s.lock_ref) facts.push(['Lock', s.lock_ref]);
    if (s.manifest_ref) facts.push(['Manifest', s.manifest_ref]);
    const detailBody = h('div', { class: 'df-disclosure__body df-stack df-stack--tight' });
    for (const [label, value] of facts) detailBody.appendChild(h('p', { class: 'df-text-sm' }, [label + ': ', mono(value)]));
    detailBody.appendChild(h('p', { class: 'df-text-sm', text: s.signed
      ? 'On the offline target, verify with a public key trusted independently of this bundle, then install using debark.'
      : 'Recipients must explicitly allow unsigned verification. File integrity checks cannot authenticate an unsigned publisher.' }));
    block.appendChild(bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'Bundle details and installation' }), detailBody,
    ])));
    if (s.warnings && s.warnings.length) {
      const warnings = titledBlock('Warnings'); warnings.body.appendChild(this.stringList(s.warnings)); block.appendChild(warnings.el);
    }
    return block;
  },

  copyResult() {
    const summary = this.resultSummary;
    if (!summary || !summary.bundle_path || !this.runStatus || this.running || this.statusPending || this.statusReadError) {
      this.toast('warning', 'The bundle result is not available yet.');
      this.syncFromStatus();
      return;
    }
    this.ctx.go('export', {
      bundlePath: summary.bundle_path,
      bundleSummary: summary,
      sourceTarget: this.runStatus.target_id,
      sourceMode: 'result',
      returnTo: 'build',
    });
  },

  detailsDrawer(err) {
    const e = err || {};
    const body = h('div', { class: 'df-stack df-stack--tight', style: { padding: 'var(--space-3)' } });
    if (e.command && e.command.length) {
      body.appendChild(h('h3', { class: 'df-group__title', text: 'The command that failed' }));
      body.appendChild(h('pre', { class: 'df-log', attrs: { tabindex: '0' } }, [
        h('span', { class: 'df-log__line', text: argvDisplay(e.command) }),
      ]));
      // UIError.Command is cliadapter.RedactArgv's output, not the argv that
      // ran: a vendor URL's credentials and its whole query string become the
      // marker REDACTED (internal/app's uiErrorFrom). Before that landed this
      // drawer's copy button put a vendor credential on the clipboard in plain
      // text, twice. The drawer is still where the raw bytes live and it is
      // still worth saying so — but "verbatim" now belongs to the stderr
      // block, which is untouched, and not to this one.
      body.appendChild(h('p', {
        class: 'df-text-sm df-text-secondary',
        text: 'Shown the same way as everywhere else in this window: any vendor-URL credentials and query strings are replaced by REDACTED. The command that ran carried them.',
      }));
    }
    if (e.details) {
      body.appendChild(h('h3', { class: 'df-group__title', text: `What ${CLI_BINARY} wrote to stderr, verbatim` }));
      const pre = h('pre', { class: 'df-log', attrs: { tabindex: '0' } });
      // Raw stderr. Split into lines so long output stays scannable; every line
      // is a text node.
      for (const line of String(e.details).split('\n')) {
        pre.appendChild(h('span', { class: 'df-log__line', text: line }));
      }
      body.appendChild(pre);
    }
    const facts = [];
    if (e.code) facts.push('code: ' + e.code);
    if (e.exit_class) facts.push('class: ' + e.exit_class);
    if (typeof e.exit_code === 'number') facts.push('process exit status: ' + e.exit_code);
    facts.push(e.retryable ? 'running this again could plausibly work' : 'running this again will not change the outcome');
    body.appendChild(h('p', { class: 'df-text-sm df-text-secondary', text: facts.join(' · ') }));
    const copyText = [
      e.message || '', e.hint || '',
      e.command && e.command.length ? argvDisplay(e.command) : '',
      e.details || '',
    ].filter(Boolean).join('\n');
    body.appendChild(h('div', { class: 'df-cluster' }, [
      btn('Copy the failure', 'secondary', () => this.copy(copyText + '\n', 'The failure detail'), { small: true, icon: 'copy' }),
    ]));

    // Not "What failed, verbatim" any more. The stderr inside is verbatim and
    // says so; the argv beside it is redacted and says so. A summary that
    // promised verbatim over both was making a claim one of them cannot keep.
    return bindDisclosure(h('details', { class: 'df-disclosure' }, [
      h('summary', { class: 'df-summary', text: 'What failed' }),
      h('div', { class: 'df-disclosure__body' }, [body]),
    ]));
  },

  reveal(path) {
    const api = this.api();
    if (!path) return;
    if (api && typeof api.RevealPath === 'function') {
      Promise.resolve(api.RevealPath(path)).then((r) => {
        if (r && r.ok === false && r.error) this.toast('warning', r.error.message || 'Could not open the folder.');
      }).catch(() => this.toast('warning', 'Could not open the folder.'));
    } else {
      this.copy(path, 'The path');
    }
  },
};

export default screen;
