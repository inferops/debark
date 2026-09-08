/**
 * screens/export.js — screen 5: save the bundle, copy it to a drive.
 *
 * What this screen is for
 * ----------------------
 * The bundle exists on the builder machine. Somebody now has to carry it to a
 * machine with no network. This screen does that carrying, and — because the
 * mistake is discovered on the side where it cannot be fixed — it spends most
 * of its pixels on three questions:
 *
 *   1. Is the destination the one you meant?  (the USB stick, not `/`)
 *   2. Will it actually fit?                  (refused up front, with numbers)
 *   3. Did every byte arrive?                 (verified, or honestly not)
 *
 * The safety boundary, stated in the UI
 * -------------------------------------
 * This is a plain filesystem copy onto an already-mounted volume. It never
 * writes a bootable USB, never opens a raw block device, never partitions or
 * formats, and never needs root. Operators arrive expecting "make me a bootable
 * stick" often enough that the banner at the top of the screen says so before
 * anything else does.
 *
 * Escaping
 * --------
 * Volume labels, device names and file paths come from the kernel and from the
 * operator's own filesystem. Nothing in this file ever builds HTML from them:
 * no assignment to `innerHTML` or `outerHTML` and no call to
 * `insertAdjacentHTML` is made anywhere in it. Every node is created with `document.createElement` (or
 * `createElementNS` for the icons) and filled with `textContent`, so there is
 * no string-to-markup path for a hostile label to travel down. The only
 * literal markup values in the file are the icon `d` attributes, which are
 * constants authored here.
 *
 * The one URL in the file is the SVG namespace, which is an XML identifier
 * rather than something that is fetched. This screen makes no network call.
 *
 * Ownership: the implementing package-screen. Owns this file and export.demo.html only.
 */

// The application's name and the name of the binary it drives are NOT the same
// word and neither is spelled out here: the UI says Debark, the wire says
// debark, and both live in exactly one constant so the eventual rename is a
// one-line change. Importing the shell for two constants is safe — shell.js
// has no module-scope side effects and loads screens dynamically, so the cycle
// resolves.
import { APP_NAME, CLI_BINARY } from '../shell/shell.js';
import { bindDisclosure } from '../shell/disclosure.js';

/* --------------------------------------------------------------------------
 * Nothing here mirrors the backend any more.
 *
 * This file used to carry three constants copied out of internal/export: the
 * incomplete marker's filename and the two sentences describing what
 * verification does and does not prove. All three now cross the binding
 * surface — `DestinationView.marker_name`, `ExportStatus.verify_method` and
 * `ExportStatus.verify_caveat` — and are rendered from it.
 *
 * That matters beyond tidiness. `verify_method` says something ELSE entirely
 * when skip_verify was set, which a constant cannot; a copy of those sentences
 * in JavaScript goes stale the moment the exporter changes what it checks, and
 * the screen then makes a claim that is no longer true.
 * ------------------------------------------------------------------------ */

/** How often the volume list is re-read. ListVolumes is one procfs read plus
 *  one statfs per candidate; the binding surface says polling this is fine. */
const VOLUME_POLL_MS = 1500;

const DEST_RADIO_NAME = 'df-export-destination';

/* --------------------------------------------------------------------------
 * DOM helpers. Everything user-visible goes through textContent.
 * ------------------------------------------------------------------------ */

function h(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined && text !== null && text !== '') node.textContent = String(text);
  return node;
}

function add(parent, ...children) {
  for (const c of children) if (c) parent.appendChild(c);
  return parent;
}

const SVG_NS = 'http://www.w3.org/2000/svg';

/**
 * Icon path data. Every shape is distinct, because rule 2 of the design system
 * forbids signalling state with colour alone: the banner variants must be
 * separable with the colour stripped out.
 */
const ICON_PATHS = {
  info: ['M12 2.6a9.4 9.4 0 1 1 0 18.8 9.4 9.4 0 0 1 0-18.8z', 'M12 11v5.5', 'M12 7.6h.01'],
  success: ['M12 2.6a9.4 9.4 0 1 1 0 18.8 9.4 9.4 0 0 1 0-18.8z', 'M7.8 12.2l2.9 2.9 5.5-5.9'],
  warning: ['M12 3.2 2.4 20.2h19.2z', 'M12 9.4v4.6', 'M12 17.2h.01'],
  danger: ['M8.6 2.8h6.8L21.2 8.6v6.8L15.4 21.2H8.6L2.8 15.4V8.6z', 'M9.4 9.4l5.2 5.2', 'M14.6 9.4l-5.2 5.2'],
  removable: ['M8.2 8.4h7.6v12.4H8.2z', 'M10.2 8.4V3.2h3.6v5.2', 'M10.4 12.4h3.2', 'M10.4 15.4h3.2'],
  fixed: ['M3.4 6.2c0-1.7 3.9-3 8.6-3s8.6 1.3 8.6 3-3.9 3-8.6 3-8.6-1.3-8.6-3z', 'M3.4 6.2v11.6c0 1.7 3.9 3 8.6 3s8.6-1.3 8.6-3V6.2', 'M3.4 12c0 1.7 3.9 3 8.6 3s8.6-1.3 8.6-3'],
  network: ['M12 2.6a9.4 9.4 0 1 1 0 18.8 9.4 9.4 0 0 1 0-18.8z', 'M2.8 12h18.4', 'M12 2.6c2.6 2.6 3.9 6 3.9 9.4S14.6 18.8 12 21.4C9.4 18.8 8.1 15.4 8.1 12S9.4 5.2 12 2.6z'],
  unknown: ['M12 2.6a9.4 9.4 0 1 1 0 18.8 9.4 9.4 0 0 1 0-18.8z', 'M9.6 9.4a2.5 2.5 0 1 1 3.3 2.4c-.6.2-.9.8-.9 1.4v.6', 'M12 17.2h.01'],
  folder: ['M3.2 6.8a2 2 0 0 1 2-2h3.9l2 2.2h7.7a2 2 0 0 1 2 2v8.6a2 2 0 0 1-2 2H5.2a2 2 0 0 1-2-2z'],
  refresh: ['M20.4 12a8.4 8.4 0 1 1-2.5-6', 'M20.4 3.2v5.4h-5.4'],
  shield: ['M12 2.8 20 5.8v6.1c0 4.6-3.3 8.1-8 9.3-4.7-1.2-8-4.7-8-9.3V5.8z', 'M8.6 11.9l2.4 2.4 4.4-4.6'],
  flush: ['M7.4 3h9.2', 'M7.4 21h9.2', 'M8.4 3v3.4c0 2 3.6 3.4 3.6 5.6s-3.6 3.6-3.6 5.6V21', 'M15.6 3v3.4c0 2-3.6 3.4-3.6 5.6s3.6 3.6 3.6 5.6V21'],
  close: ['M6 6l12 12', 'M18 6 6 18'],
  copy: ['M9 8.6h10.4V21H9z', 'M5.2 15.4V3h10.4v2.4'],
  reveal: ['M14 4h6v6', 'M20 4l-8.6 8.6', 'M19 14.4V19a1.6 1.6 0 0 1-1.6 1.6H5a1.6 1.6 0 0 1-1.6-1.6V6.6A1.6 1.6 0 0 1 5 5h4.6'],
  // The question mark of .df-help__btn, copied from design/gallery.html.
  help: ['M12 2.5a9.5 9.5 0 1 1 0 19 9.5 9.5 0 0 1 0-19z', 'M9.3 9.2a2.8 2.8 0 115.3 1.3c-.5.9-1.6 1.3-2.2 2.1-.3.4-.4.8-.4 1.4', 'M12 17.4v.1'],
};

/** Builds an inline SVG icon. `name` indexes ICON_PATHS; nothing else is
 *  interpolated, so no caller can inject markup here. */
function icon(name, cls) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  if (cls) svg.setAttribute('class', cls);
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of ICON_PATHS[name] || ICON_PATHS.unknown) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', d);
    svg.appendChild(p);
  }
  return svg;
}

/** Status pill: icon shape + words. Never colour alone. */
function status(role, label, iconName) {
  const el = h('span', 'df-status' + (role ? ' df-status--' + role : ''));
  add(el, icon(iconName || role || 'info'), h('span', null, label));
  return el;
}

function button(label, variant, onClick, opts) {
  const b = h('button', 'df-btn ' + (variant || 'df-btn--secondary'));
  b.type = 'button';
  if (opts && opts.iconName) add(b, icon(opts.iconName, 'df-btn__icon'));
  add(b, h('span', null, label));
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

/**
 * A design-system banner. `role` is one of info/success/warning/danger.
 * Danger gets role="alert", everything else role="status" (README §6.8).
 */
function banner(role, title, text, actions) {
  const el = h('div', 'df-banner df-banner--' + role);
  el.setAttribute('role', role === 'danger' ? 'alert' : 'status');
  add(el, icon(role, 'df-banner__icon'));
  const body = h('div', 'df-banner__body');
  if (title) add(body, h('p', 'df-banner__title', title));
  if (Array.isArray(text)) {
    for (const t of text) if (t) add(body, h('p', 'df-banner__text', t));
  } else if (text) {
    add(body, h('p', 'df-banner__text', text));
  }
  add(el, body);
  // The actions column holds a grid track even when empty — keep it.
  const acts = h('div', 'df-banner__actions');
  for (const a of actions || []) add(acts, a);
  add(el, acts);
  return el;
}

/** A monospace block for a path or a list. `.df-log` draws no border, so this
 *  costs no rectangle and sits happily inside a boxed list as a slab. */
function logBlock(lines) {
  const pre = h('pre', 'df-log');
  pre.tabIndex = 0;
  for (const line of Array.isArray(lines) ? lines : [lines]) {
    add(pre, h('span', 'df-log__line', String(line)));
  }
  return pre;
}

/** A collapsed details drawer. `body` is rendered as monospace log text. */
function drawer(summaryText, bodyText) {
  const d = h('details', 'df-disclosure');
  const s = h('summary', 'df-summary', summaryText);
  const b = h('div', 'df-disclosure__body');
  const pre = h('pre', 'df-log');
  add(pre, h('span', 'df-log__line', bodyText));
  add(b, pre);
  return bindDisclosure(add(d, s, b));
}

function progressBar(labelText, valueText, fraction, roleClass) {
  const wrap = h('div', 'df-stack df-stack--tight');
  const lab = h('div', 'df-progress-label');
  add(lab, h('span', null, labelText), h('span', 'df-progress-label__value', valueText || ''));
  const indeterminate = fraction === null || fraction === undefined || fraction < 0;
  const bar = h(
    'div',
    'df-progress' + (roleClass ? ' ' + roleClass : '') + (indeterminate ? ' df-progress--indeterminate' : '')
  );
  bar.setAttribute('role', 'progressbar');
  bar.setAttribute('aria-label', labelText);
  const fill = h('div', 'df-progress__bar');
  if (!indeterminate) {
    const pct = Math.max(0, Math.min(100, Math.round(fraction * 100)));
    fill.style.width = pct + '%';
    bar.setAttribute('aria-valuenow', String(pct));
    bar.setAttribute('aria-valuemin', '0');
    bar.setAttribute('aria-valuemax', '100');
  }
  add(bar, fill);
  return { el: add(wrap, lab, bar), label: lab.firstChild, value: lab.lastChild, bar, fill };
}

/* --------------------------------------------------------------------------
 * Formatting.
 *
 * `ctx.states.formatBytes` is the whole application's byte formatter and this
 * screen uses it. It renders BINARY units — KiB/MiB/GiB — because apt and dpkg
 * report Installed-Size in KiB and `df -h` is what an operator checks the same
 * drive with. This screen used to carry a private decimal one, which meant the
 * readiness screen and this screen rendered the SAME quantity — free space on
 * the chosen destination — in two different units.
 *
 * The fallback below is reached only in the standalone harness, where there is
 * no shell to ask, and produces the same strings.
 * ------------------------------------------------------------------------ */

function humanBytes(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return 'an unknown amount';
  const st = S.ctx && S.ctx.states;
  if (st && typeof st.formatBytes === 'function') {
    const out = st.formatBytes(n);
    if (out) return out;
  }
  if (n < 1024) return String(Math.round(n)) + ' B';
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
  return (v >= 100 ? String(Math.round(v)) : v.toFixed(1).replace(/\.0$/, '')) + ' ' + units[i];
}

function humanCount(n) {
  return Number(n || 0).toLocaleString();
}

function humanDuration(ms) {
  const total = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  if (m >= 60) {
    const hrs = Math.floor(m / 60);
    return hrs + 'h ' + (m % 60) + 'm';
  }
  return m + ':' + String(s).padStart(2, '0');
}

function humanRate(bytesPerSec) {
  if (!bytesPerSec || bytesPerSec <= 0) return '';
  return humanBytes(bytesPerSec) + '/s';
}

/* --------------------------------------------------------------------------
 * The screen.
 * ------------------------------------------------------------------------ */

const S = {
  ctx: null,
  root: null,
  mounted: false,
  visible: false,

  // Source
  bundlePath: '',
  bundleResolved: '',
  bundleSigned: null,
  bundleExitClass: '',
  sourceTarget: '',
  returnTo: 'build',
  entrySeq: 0,
  statusSeq: 0,
  lifecycleSeq: 0,
  statusError: '',
  acceptFinished: true,
  jobStartedAt: '',
  choosing: false,
  buildRunning: false,
  stopping: false,

  // Destinations
  volumes: [],
  fingerprint: null,
  volumesSupported: true,
  volumesError: null,
  volumesLoaded: false,
  folderDest: '',

  selectedPath: '',
  selectedIsVolume: false,
  selectedMissing: false,
  internalConfirmed: false,

  subdir: '',
  subdirError: '',
  skipVerify: false,

  // Plan
  planning: false,
  planToken: 0,
  inspectToken: 0,
  plan: null,
  planError: null,
  incompleteHere: false,

  // Run
  canStart: false,
  cancelling: false,
  running: false,
  progress: null,
  runStartedAt: 0,
  phaseStartedAt: 0,
  lastPhase: '',

  // Outcome
  finished: null, // ExportFinished
  verify: null, // VerifyStatus
  verifyRunning: false,
  verifyStarting: false,
  verifySeq: 0,
  verifyStartSeq: 0,
  verifyStartedCopy: '',

  // Destinations known this session to hold an interrupted export. Still kept
  // as a fallback: it is the only answer available against a backend with no
  // InspectDestination binding.
  incompleteDests: new Set(),

  // From DestinationView. `markerName` is the exporter's own filename and is
  // never used to build a path — only to name the file in a sentence, so an
  // operator can look for it themselves.
  markerName: '',
  incompleteMessage: '',

  // Timers / handles
  pollTimer: 0,
  tickTimer: 0,
  rafHandle: 0,

  // Nodes
  nodes: {},
};

function bindings() {
  const ctx = S.ctx;
  if (ctx && ctx.bindings) return ctx.bindings;
  if (window.go && window.go.app && window.go.app.App) return window.go.app.App;
  return null;
}

function toast(kind, message) {
  if (S.ctx && typeof S.ctx.toast === 'function') S.ctx.toast(kind, message);
}

/**
 * The shared loading / empty / error blocks (`ctx.states`, the implementing package).
 *
 * Screen-contract rule 2: a screen renders three states or it is not finished,
 * and these come from the shell so they look the same on every screen. The
 * local fallback below exists only so this module still renders when it is
 * driven without a shell — the standalone harness, or a shell that has not
 * finished booting — and it builds the same design-system markup.
 *
 * Options: { title, text, detail, error, actions: [{label, onClick, variant}],
 *            emptyKind }
 */
function statesApi() {
  const s = S.ctx && S.ctx.states;
  return s && typeof s.loadingState === 'function' && typeof s.errorState === 'function' ? s : null;
}

function stateBlock(kind, opts) {
  const st = statesApi();
  if (st) {
    try {
      if (kind === 'loading') {
        return st.loadingState({ label: opts.title, detail: opts.text }).el;
      }
      if (kind === 'empty') {
        return st.emptyState({
          kind: opts.emptyKind || 'collection-empty',
          title: opts.title,
          message: opts.text,
          actions: opts.actions || [],
        }).el;
      }
      return st.errorState({
        error: opts.error,
        summary: opts.title,
        hint: opts.text,
        detail: opts.detail,
        actions: opts.actions || [],
      }).el;
    } catch (_) {
      /* a shell helper that throws must not take the screen down with it */
    }
  }
  return localStateBlock(kind, opts);
}

function localStateBlock(kind, opts) {
  const err = opts.error || {};
  const title = opts.title || err.message || 'Something went wrong.';
  const text = opts.text || err.hint || '';
  const variant = kind === 'error' ? ' df-state--error' : kind === 'loading' ? ' df-state--loading' : '';
  const el = h('div', 'df-state' + variant);
  if (kind === 'loading') {
    add(el, h('span', 'df-spinner df-spinner--lg'));
  } else {
    add(el, icon(kind === 'error' ? 'danger' : 'info', 'df-state__icon'));
  }
  add(el, h('span', 'df-state__title', title));
  if (text) add(el, h('p', 'df-state__text', text));
  const actions = opts.actions || [];
  if (actions.length) {
    const acts = h('div', 'df-state__actions');
    for (let i = 0; i < actions.length; i++) {
      const a = actions[i];
      add(acts, button(a.label, 'df-btn--' + (a.variant || (i === 0 ? 'primary' : 'secondary')) + ' df-btn--sm', a.onClick));
    }
    add(el, acts);
  }
  const detail = opts.detail || detailsOf(err);
  if (detail) {
    const det = h('div', 'df-state__detail');
    add(det, drawer('What failed', detail));
    add(el, det);
  }
  return el;
}

/** The technical lines a UIError carries, for the collapsed drawer. */
function detailsOf(err) {
  if (!err) return '';
  const parts = [];
  if (err.details) parts.push(err.details);
  if (err.command && err.command.length) parts.push(err.command.join(' '));
  if (err.exit_class) parts.push('class: ' + err.exit_class);
  return parts.join('\n');
}

/* --------------------------------------------------------------------------
 * Section 1 — what is being copied.
 * ------------------------------------------------------------------------ */

function renderSource() {
  const box = S.nodes.sourceBox;
  const notes = S.nodes.sourceNotes;
  box.replaceChildren();
  notes.replaceChildren();

  const path = S.bundleResolved || S.bundlePath;
  add(box, boxRow({
    label: path ? baseName(path) : 'Choose a bundle',
    description: S.sourceTarget || (path ? 'Existing bundle' : 'Select the bundle folder to copy.'),
    control: S.running ? [] : [button(path ? 'Change…' : 'Choose bundle…', 'df-btn--secondary df-btn--sm', () => chooseBundle(false), { iconName: 'folder' })],
  }));
  if (path) add(box, slab(logBlock(path)));

  // Both of these are facts about the bundle that change what the copy means,
  // so they keep a banner. Neither is permanent: they appear only when true.
  if (S.bundleExitClass === 'incomplete') {
    add(
      notes,
      banner(
        'warning',
        'This bundle is itself incomplete',
        'Some requested packages are missing. A checked copy will still contain this incomplete bundle.'
      )
    );
  }
  if (S.bundleSigned === false) {
    add(
      notes,
      banner(
        'warning',
        'This bundle is unsigned',
        'Copying does not add a signature. The recipient cannot verify who produced this bundle.'
      )
    );
  }
  if (path && S.bundleSigned === null) add(notes, h('p', 'df-text-secondary', 'Signing and build completeness are unknown for this source. Check the bundle before installing.'));
}

/* --------------------------------------------------------------------------
 * Section 2 — the destination chooser.
 *
 * The failure that matters on this screen is picking the wrong destination, so
 * this table is built to make the USB stick and the root filesystem impossible
 * to mix up:
 *
 *  - the backend's order is preserved exactly (removable first, root last) and
 *    never re-sorted here;
 *  - two group headings split the list into "Removable media" and "Not
 *    removable", derived from that order rather than imposed on it;
 *  - every row carries a media icon SHAPE and the words "USB / removable",
 *    "Internal disk" or "Network share" — never a colour on its own;
 *  - a volume that cannot be written has no radio at all, and says why;
 *  - choosing a non-removable volume needs a second, explicit tick.
 * ------------------------------------------------------------------------ */

const KIND_LABEL = {
  removable: 'USB / removable',
  fixed: 'Internal disk',
  network: 'Network share',
  unknown: 'Unknown device',
};

const KIND_ICON = {
  removable: 'removable',
  fixed: 'fixed',
  network: 'network',
  unknown: 'unknown',
};

function volumeUsable(v) {
  return !v.read_only && v.writable !== false;
}

function volumeReason(v) {
  if (v.note) return v.note;
  if (v.read_only) return 'mounted read-only';
  if (v.writable === false) return 'no write permission for this user';
  return '';
}

function renderDestinations() {
  const box = S.nodes.destBox;
  const states = S.nodes.destStates;

  // Remember what has keyboard focus so a background poll cannot steal it.
  const active = document.activeElement;
  const focusedPath =
    active && box.contains(active) && active.dataset && active.dataset.destPath ? active.dataset.destPath : null;

  box.replaceChildren();
  states.replaceChildren();

  if (!S.volumesLoaded && !S.volumesError) {
    box.hidden = true;
    add(states, stateBlock('loading', { title: 'Looking for mounted drives', text: 'Reading the list of mounted filesystems.' }));
    return;
  }

  if (S.volumesError) {
    add(
      states,
      stateBlock('error', {
        error: S.volumesError,
        actions: [{ label: 'Try again', onClick: () => pollVolumes(true) }],
      })
    );
  }

  const rows = [];
  if (!S.volumesError && S.volumesSupported) {
    let seenNonRemovable = false;
    let seenRemovable = false;
    for (const v of S.volumes) {
      if (v.removable && !seenRemovable) {
        seenRemovable = true;
        rows.push(groupHeaderRow('Removable drives'));
      }
      if (!v.removable && !seenNonRemovable) {
        seenNonRemovable = true;
        rows.push(groupHeaderRow('Internal disks and network shares'));
      }
      rows.push(volumeRow(v));
    }
  }

  if (S.folderDest) {
    rows.push(groupHeaderRow('The folder you chose'));
    rows.push(folderRow(S.folderDest));
  }

  box.hidden = rows.length === 0;
  if (rows.length === 0) {
    if (!S.volumesSupported) {
      add(
        states,
        stateBlock('empty', {
          title: 'This platform cannot list mounted drives',
          text: 'That is not a failure — the copy itself works exactly the same way. Choose a folder to copy into and carry on.',
          actions: [{ label: 'Save to a folder…', onClick: chooseFolder }],
        })
      );
    } else if (!S.volumesError) {
      add(
        states,
        stateBlock('empty', {
          title: 'No drive is mounted',
          text: 'Plug in a USB stick and wait for the desktop to mount it — this list refreshes on its own. Or copy the bundle into a folder on this machine instead.',
          actions: [{ label: 'Save to a folder…', onClick: chooseFolder }],
        })
      );
    }
  } else {
    const table = h('table', 'df-table');
    const thead = h('thead');
    const htr = h('tr');
    add(
      htr,
      h('th', 'df-table__status', 'Use'),
      h('th', 'df-table__label', 'Drive'),
      h('th', 'df-table__detail', 'Location'),
      h('th', 'df-table__action', 'Available')
    );
    add(thead, htr);
    const tbody = h('tbody');
    for (const r of rows) add(tbody, r);
    add(table, thead, tbody);
    // A slab, so the table is full-bleed inside the box and loses its own
    // radius rather than drawing a second rectangle inside the first.
    add(box, tableSlab(table, 'Mounted drives'));
    // The folder-name field belongs to the destination, so it is a row of the
    // same box rather than a field floating underneath it.
  }

  // Restore focus if the poll rebuilt the row that had it.
  if (focusedPath) {
    for (const input of box.querySelectorAll('input[type="radio"]')) {
      if (input.dataset.destPath === focusedPath) {
        input.focus();
        break;
      }
    }
  }

  renderSelectionNotes();
}

function groupHeaderRow(text) {
  const tr = h('tr');
  const th = h('th', null, text);
  th.setAttribute('colspan', '4');
  th.setAttribute('scope', 'colgroup');
  return add(tr, th);
}

function destRadio(path, label) {
  const input = h('input', 'df-radio__input');
  input.type = 'radio';
  input.name = DEST_RADIO_NAME;
  input.dataset.destPath = path;
  input.setAttribute('aria-label', label);
  input.checked = S.selectedPath === path;
  input.disabled = S.running;
  input.addEventListener('change', () => {
    if (input.checked) selectDestination(path, true);
  });
  return input;
}

function volumeRow(v) {
  const tr = h('tr');
  const usable = volumeUsable(v);
  const kind = KIND_LABEL[v.kind] ? v.kind : 'unknown';

  // Column 1 — the radio, or an unmistakable "no".
  const useCell = h('td', 'df-table__status');
  if (usable) {
    add(useCell, destRadio(v.path, 'Copy to ' + v.label + ' at ' + v.path));
  } else {
    const no = status('danger', 'Cannot be used', 'danger');
    add(useCell, no);
  }
  add(tr, useCell);

  // Column 2 — media class, as a shape and as words.
  const kindCell = h('td', 'df-table__label');
  add(kindCell, status(v.removable ? 'success' : 'warning', KIND_LABEL[kind], KIND_ICON[kind]));

  // Column 3 — the name the operator recognises, in the primary text colour.
  const nameCell = h('td', 'df-table__label');
  add(nameCell, h('span', null, v.label || v.path));
  add(nameCell, status(v.removable ? 'info' : 'warning', KIND_LABEL[kind], KIND_ICON[kind]));
  add(tr, nameCell);

  // Column 4 — where it is mounted, which device it is, and the reason it
  // cannot be used if there is one.
  const whereCell = h('td', 'df-table__detail');
  const stack = h('div', 'df-stack df-stack--tight');
  const sub = [v.path];
  if (v.fs_type) sub.push(v.fs_type);
  add(stack, h('div', 'df-mono df-text-sm df-truncate', sub.join('  ·  ')));
  const reason = volumeReason(v);
  if (reason) {
    add(stack, status(usable ? 'info' : 'danger', reason, usable ? 'info' : 'danger'));
  }
  add(whereCell, stack);
  add(tr, whereCell);

  // Column 4 — filesystem type. It is the difference between "this works" and
  // a support ticket, so it is a column and not a tooltip.
  const fsCell = h('td', 'df-table__label');
  add(fsCell, h('span', 'df-mono df-text-sm', v.fs_type || '—'));

  // Column 5 — capacity.
  const capCell = h('td', 'df-table__action');
  const capStack = h('div', 'df-stack df-stack--tight');
  if (v.total_bytes > 0) {
    add(capStack, h('div', null, humanBytes(v.free_bytes) + ' free'));
    add(capStack, h('div', 'df-text-sm df-text-secondary', 'of ' + humanBytes(v.total_bytes)));
  } else {
    add(capStack, h('div', 'df-text-secondary', 'Capacity unknown'));
  }
  add(capCell, capStack);
  add(tr, capCell);

  if (usable) {
    tr.addEventListener('click', (ev) => {
      if (S.running) return;
      if (ev.target && ev.target.tagName === 'INPUT') return;
      selectDestination(v.path, true);
      renderDestinations();
    });
  }
  return tr;
}

function folderRow(path) {
  const tr = h('tr');
  const useCell = h('td', 'df-table__status');
  add(useCell, destRadio(path, 'Copy to the folder ' + path));
  add(tr, useCell);

  const kindCell = h('td', 'df-table__label');
  add(kindCell, status('info', 'Folder', 'folder'));

  const host = hostVolumeFor(path);
  const nameCell = h('td', 'df-table__label');
  add(nameCell, h('span', null, baseName(path)));
  add(nameCell, status('info', 'Folder', 'folder'));
  add(tr, nameCell);

  const whereCell = h('td', 'df-table__detail');
  const stack = h('div', 'df-stack df-stack--tight');
  add(stack, h('div', 'df-mono df-text-sm df-truncate', path));
  if (host) {
    add(
      stack,
      h(
        'div',
        'df-text-sm',
        'On ' + (host.label || host.path) + ' — ' + humanBytes(host.free_bytes) + ' free of ' + humanBytes(host.total_bytes)
      )
    );
  }
  add(whereCell, stack);
  add(tr, whereCell);

  const fsCell = h('td', 'df-table__label');
  add(fsCell, h('span', 'df-mono df-text-sm', (host && host.fs_type) || '—'));

  const capCell = h('td', 'df-table__action');
  add(capCell, h('span', 'df-text-secondary', host && host.total_bytes > 0 ? humanBytes(host.free_bytes) + ' free' : '—'));
  add(tr, capCell);

  tr.addEventListener('click', (ev) => {
    if (S.running) return;
    if (ev.target && ev.target.tagName === 'INPUT') return;
    selectDestination(path, false);
    renderDestinations();
  });
  return tr;
}

/** The last segment of a path, for a row that needs a short name. */
function baseName(path) {
  const parts = String(path || '').split(/[\\/]+/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : String(path || '');
}

/**
 * The listed volume whose mount point contains `path`, longest prefix first so
 * a nested mount wins over its parent. Display only: it is used to tell the
 * operator which drive a chosen folder actually sits on. The plan's own
 * free-space number always comes from the backend.
 */
function hostVolumeFor(path) {
  let best = null;
  for (const v of S.volumes) {
    if (!v.path) continue;
    const p = v.path.endsWith('/') || v.path.endsWith('\\') ? v.path : v.path + '/';
    const target = path.endsWith('/') || path.endsWith('\\') ? path : path + '/';
    if (target === p || target.startsWith(p)) {
      if (!best || v.path.length > best.path.length) best = v;
    }
  }
  return best;
}

function selectedVolume() {
  if (!S.selectedIsVolume) return null;
  for (const v of S.volumes) if (v.path === S.selectedPath) return v;
  return null;
}

function renderSelectionNotes() {
  const box = S.nodes.destNotes;
  box.replaceChildren();
  if (!S.selectedPath) return;

  if (S.selectedMissing) {
    add(
      box,
      banner(
        'danger',
        'The drive you chose is no longer connected',
        'It disappeared from the list of mounted filesystems. Plug it back in and wait for the desktop to mount it, or pick another destination.'
      )
    );
    return;
  }

  const vol = selectedVolume() || hostVolumeFor(S.selectedPath);
  if (vol && !vol.removable) {
    const label = h('label', 'df-check');
    const input = h('input', 'df-check__input');
    input.type = 'checkbox';
    input.checked = S.internalConfirmed;
    input.addEventListener('change', () => {
      S.internalConfirmed = input.checked;
      renderActions();
    });
    add(label, input, h('span', null, 'Yes, copy to this internal destination.'));
    add(
      box,
      banner(
        'warning',
        'This is not removable media',
        [
          (vol.label || vol.path) + ' is ' + (vol.kind === 'network' ? 'a network share' : 'a disk inside this machine') + '.',
          'Copying here puts nothing on a USB stick. If you meant the stick you just plugged in, pick a row from the "Removable media" group above.',
        ],
        [label]
      )
    );
  }

  if (S.incompleteHere || S.incompleteDests.has(destinationKey())) {
    add(box, incompleteWarningBanner(S.selectedPath));
  }
}

/** The marker named the way the exporter names it, or described when the
 *  backend has not told us its filename. */
function markerSentence(dest) {
  const where = dest || 'the destination folder';
  return S.markerName
    ? 'A file named ' + S.markerName + ' is in ' + where + '.'
    : 'The exporter’s incomplete marker is still in ' + where + '.';
}

function incompleteWarningBanner(dest) {
  return banner(
    'warning',
    'This destination already holds an interrupted export',
    [
      // The backend's own sentence when it sent one; it knows what it found.
      S.incompleteMessage ||
        (markerSentence(dest) + ' It was left behind by an export that did not finish and verify, which means whatever is in that folder is not a usable bundle.'),
      'Exporting again replaces it. Nothing else in this app will clear that mark.',
    ],
    [button('Show the folder', 'df-btn--secondary df-btn--sm', () => revealPath(dest), { iconName: 'reveal' })]
  );
}

/* --------------------------------------------------------------------------
 * Section 3 — the plan.
 * ------------------------------------------------------------------------ */

function renderPlan() {
  const box = S.nodes.planBox;
  const states = S.nodes.planStates;
  box.replaceChildren();
  states.replaceChildren();

  const finish = () => {};

  if (!S.selectedPath) {
    finish();
    return;
  }

  if (S.subdirError) {
    add(states, banner('danger', 'That folder name cannot be used', S.subdirError));
    finish();
    return;
  }

  if (S.planning) {
    add(states, stateBlock('loading', { title: 'Working out what would be copied', text: 'Counting the files in the bundle. Nothing is being written.' }));
    finish();
    return;
  }

  if (S.planError) {
    // A plan error means the copy must not start — including "it will not
    // fit", which carries the real numbers in its own sentence.
    add(
      states,
      stateBlock('error', {
        error: S.planError,
        actions: [{ label: 'Check again', onClick: () => runPlan() }],
      })
    );
    finish();
    return;
  }

  const p = S.plan;
  if (!p) { finish(); return; }

  // Rows of one box, not a table inside a card. The numbers are the reason
  // this group exists, so they sit in the row's trailing control where the eye
  // lands, and only the sentence beneath a number is secondary.
  const value = (text, mono) => h('span', mono ? 'df-mono' : null, text);

  add(box, boxRow({ label: 'Copy destination', description: p.dest_dir,
    control: [value(humanCount(p.total_files) + ' files · ' + humanBytes(p.total_bytes))] }));
  add(box, boxRow({
    label: 'Room needed',
    description: 'Includes space reserved for safe copying.',
    control: [value(humanBytes(p.required_bytes))],
  }));
  if (p.free_bytes >= 0) {
    const left = p.free_bytes - p.required_bytes;
    add(box, boxRow({
      label: 'Free on the destination',
      description: left >= 0 ? 'About ' + humanBytes(left) + ' would be left over.' : null,
      control: [value(humanBytes(p.free_bytes))],
    }));
  } else {
    add(box, boxRow({
      label: 'Free on the destination',
      description: 'The copy will be attempted anyway; it may fail part-way if the drive fills.',
      control: [value('Could not be measured')],
    }));
  }

  finish();

  const warnings = p.warnings || [];
  if (warnings.length) {
    add(
      states,
      banner(
        'warning',
        warnings.length === 1 ? 'Worth knowing before you start' : warnings.length + ' things worth knowing before you start',
        warnings
      )
    );
  }
}

/* --------------------------------------------------------------------------
 * Section 4 — the copy in flight.
 *
 * Phase labelling is the whole point of this section. The engine's `flush`
 * phase is an fsync of everything that has just been written; on a USB stick it
 * can sit for minutes moving no bytes, and it emits exactly ONE progress event
 * when it starts and nothing until it ends. A percentage bar parked at 100%
 * during that is precisely how an operator gets convinced the app has hung and
 * pulls the drive. So the flush renders as an indeterminate bar, a sentence
 * saying what fsync is, a locally-ticked elapsed clock proving the app is alive,
 * and the words "do not unplug the drive".
 * ------------------------------------------------------------------------ */

const PHASE_TEXT = {
  idle: { title: 'Waiting', note: '' },
  checking: {
    title: 'Checking the destination',
    note: 'Making sure the folder exists and can be written, before a single byte is copied.',
  },
  copying: { title: 'Copying files', note: '' },
  flushing: {
    title: 'Finishing writes',
    note:
      'Saving the remaining data to the drive. This may take several minutes. Keep the drive connected.',
  },
  verifying: {
    title: 'Checking copy',
    note: 'Reading files back from the drive and checking their digests.',
  },
  finished: { title: 'Finished', note: '' },
  failed: { title: 'Stopped', note: '' },
  cancelled: { title: 'Cancelled', note: '' },
};

function buildRunPanel() {
  const stack = h('div', 'df-stack');
  const bar = progressBar('Starting', '', -1);
  const meta = h('div', 'df-text-sm df-text-secondary');
  const file = h('div', 'df-mono df-text-sm df-text-secondary df-truncate');
  const note = h('div', 'df-banner df-banner--info');
  note.setAttribute('role', 'status');
  const noteIcon = icon('flush', 'df-banner__icon');
  const noteBody = h('div', 'df-banner__body');
  const noteTitle = h('p', 'df-banner__title', '');
  const noteText = h('p', 'df-banner__text', '');
  add(noteBody, noteTitle, noteText);
  add(note, noteIcon, noteBody, h('div', 'df-banner__actions'));
  note.hidden = true;

  bar.label.setAttribute('aria-live', 'polite');
  add(stack, bar.el, meta, file, note);
  S.nodes.run = { stack, bar, meta, file, note, noteTitle, noteText };
  return stack;
}

function renderRun() {
  const n = S.nodes.run;
  const p = S.progress;
  const phase = (p && p.phase) || 'checking';
  const text = PHASE_TEXT[phase] || PHASE_TEXT.checking;

  S.nodes.runPanel.hidden = !S.running;
  if (!S.running) return;

  const indeterminate = phase === 'flushing' || phase === 'checking' || !p || p.fraction < 0;
  n.bar.label.textContent = text.title;

  if (indeterminate) {
    if (!n.bar.bar.classList.contains('df-progress--indeterminate')) {
      n.bar.bar.classList.add('df-progress--indeterminate');
      n.bar.bar.removeAttribute('aria-valuenow');
      n.bar.fill.style.width = '';
    }
    n.bar.value.textContent = phase === 'flushing' ? 'still working' : '';
  } else {
    const pct = Math.max(0, Math.min(100, Math.round(p.fraction * 100)));
    n.bar.bar.classList.remove('df-progress--indeterminate');
    n.bar.bar.setAttribute('aria-valuenow', String(pct));
    n.bar.bar.setAttribute('aria-valuemin', '0');
    n.bar.bar.setAttribute('aria-valuemax', '100');
    n.bar.fill.style.width = pct + '%';
    n.bar.value.textContent = pct + '%';
  }
  n.bar.bar.setAttribute('aria-label', text.title);

  const bits = [];
  if (p && p.files_total) bits.push(humanCount(p.files_done) + ' of ' + humanCount(p.files_total) + ' files');
  if (p && p.bytes_total) bits.push(humanBytes(p.bytes_done) + ' of ' + humanBytes(p.bytes_total));
  const r = p ? humanRate(p.bytes_per_sec) : '';
  if (r && phase === 'copying') bits.push(r);
  if (S.runStartedAt) bits.push('elapsed ' + humanDuration(Date.now() - S.runStartedAt));
  if (phase === 'flushing' && S.phaseStartedAt) {
    bits.push('flushing for ' + humanDuration(Date.now() - S.phaseStartedAt));
  }
  n.meta.textContent = bits.join('  ·  ');

  n.file.textContent = p && p.file ? p.file : '';

  if (text.note) {
    n.note.hidden = false;
    n.noteTitle.textContent = phase === 'flushing' ? 'Why nothing appears to be happening' : text.title;
    n.noteText.textContent = text.note;
    n.note.className = 'df-banner ' + (phase === 'flushing' ? 'df-banner--warning' : 'df-banner--info');
  } else {
    n.note.hidden = true;
  }
}

/* --------------------------------------------------------------------------
 * Section 5 — the outcome.
 *
 * `Result.Complete` means verified: copied, flushed, re-read, digests matched
 * and the incomplete marker removed. It reaches the frontend as
 * ExportStatus.verified. Nothing else is success, and this section refuses to
 * render anything else as success.
 * ------------------------------------------------------------------------ */

function renderOutcome() {
  const panel = S.nodes.outcomePanel;
  const box = S.nodes.outcomeBody;
  box.replaceChildren();

  const f = S.finished;
  panel.hidden = !f;
  if (!f) return;

  const st = f.status || {};
  const dest = st.destination_path || st.destination || S.selectedPath;

  if (f.cancelled) {
    add(
      box,
      banner('warning', 'The copy was cancelled', [
        progressRecap(st) || 'The copy stopped before it finished, so the destination holds a partly written bundle.',
      ])
    );
    add(box, incompleteBlock(dest, 'cancelled'));
  } else if (f.error) {
    // The engine's own summary restates the error sentence and its hint
    // verbatim, so rendering both would say the same thing twice. What the
    // summary adds is how far the copy got, and that is what is kept.
    add(
      box,
      stateBlock('error', {
        error: f.error,
        actions: [{ label: 'Copy the message', variant: 'secondary', onClick: () => copyText(errorText(f.error)) }],
      })
    );
    const recap = progressRecap(st);
    if (recap) add(box, h('p', 'df-text-secondary', recap));
    // Which files, and how. Without this the operator cannot tell a bad drive
    // from a bad bundle, which is the whole decision in front of them.
    add(box, incompleteBlock(dest, 'failed'));
    const bad = mismatchBlock(st);
    if (bad) add(box, bad);
  } else if (st.verified) {
    add(
      box,
      banner(
        'success',
        'Copy integrity checked',
        ['The copied files match the source. This does not verify the bundle signature.']
      )
    );
    const detail = verificationDetail(st);
    if (detail) add(box, detail);
    add(box, postCopyActions(dest));
  } else if (st.verify_skipped) {
    add(
      box,
      banner('warning', 'Copy is unchecked — do not install it', [
        st.summary || 'The files were written, but verification was skipped, so nothing has been read back off the drive.',
        'This is not a checked bundle. A copy that silently went wrong would look exactly like this one.',
      ])
    );
    // verify_method says something different when the pass was skipped, which
    // is precisely why it is read off the status rather than hard-coded.
    const skippedDetail = verificationDetail(st);
    if (skippedDetail) add(box, skippedDetail);
    add(box, incompleteBlock(dest, 'unverified'));
    add(box, h('p', 'df-text-secondary', 'Copy again with verification enabled before using this destination.'));
  } else {
    // Belt and braces: ok without verified and without verify_skipped should
    // not happen, and if it does it is not success.
    add(
      box,
      banner('warning', 'The copy finished, but it was not confirmed as verified', [
        st.summary || 'The backend did not report a passing verification for this copy.',
        'Treat the destination as unchecked until you have verified it.',
      ])
    );
    const unmatched = mismatchBlock(st);
    if (unmatched) add(box, unmatched);
    add(box, incompleteBlock(dest, 'unverified'));
  }

  renderVerifySection();
}

/** How far the copy got, from the last progress sample. */
function progressRecap(st) {
  const p = st && st.progress;
  if (!p || !p.files_total) return '';
  // Only interesting while the copy was still short of the end; once every
  // file has been written the error itself says what went wrong.
  if (p.files_done >= p.files_total) return '';
  return (
    'It stopped after ' + humanCount(p.files_done) + ' of ' + humanCount(p.files_total) +
    ' files, ' + humanBytes(p.bytes_done) + ' of ' + humanBytes(p.bytes_total) + ' written.'
  );
}

/**
 * What verification actually checked, in the exporter's own words.
 *
 * `verify_method` and `verify_caveat` come off ExportStatus. They used to be
 * two constants at the top of this file, which is exactly the drift the view
 * layer exists to prevent: the moment the exporter changes what it checks, a
 * copy of its sentences in JavaScript is a claim that is no longer true. It
 * also says something else entirely when skip_verify was set, which a constant
 * cannot. If the backend sends neither, this renders nothing rather than
 * inventing a claim.
 */
function verificationDetail(st) {
  const method = (st && st.verify_method) || '';
  const caveat = (st && st.verify_caveat) || '';
  const files = st && typeof st.files_checked === 'number' ? st.files_checked : 0;
  const bytes = st && typeof st.bytes_checked === 'number' ? st.bytes_checked : 0;
  const counted = files > 0 || bytes > 0
    ? humanCount(files) + (files === 1 ? ' file' : ' files') + ' and ' + humanBytes(bytes) + ' were read back off the drive.'
    : '';
  if (!method && !caveat && !counted) return null;

  const d = h('details', 'df-disclosure');
  const sum = h('summary', 'df-summary', 'What was checked, and what that proves');
  const b = h('div', 'df-disclosure__body');
  const stack = h('div', 'df-stack');
  stack.style.padding = 'var(--space-4)';
  if (counted) add(stack, h('p', null, counted));
  if (method) add(stack, h('p', null, method));
  if (caveat) add(stack, h('p', 'df-text-secondary', caveat));
  add(b, stack);
  return bindDisclosure(add(d, sum, b));
}

/** What each mismatch reason means, in words rather than an enum. */
const MISMATCH_REASON = {
  missing: 'not on the drive',
  size: 'wrong length on the drive',
  content: 'right length, different bytes',
  not_copied: 'never written',
  unreadable: 'could not be read back',
};

/**
 * The files that failed verification.
 *
 * "The copy did not verify" without this is a wall: the operator's next
 * question is always which files and how, and `mismatches` is the only place
 * that detail exists. `reason: "content"` is the one that matters — right
 * length, wrong bytes, which a size check alone cannot see.
 *
 * `mismatch_count` is the true total and is what gets rendered; the list
 * itself is capped at 500 by the backend.
 */
function mismatchBlock(st) {
  const list = (st && st.mismatches) || [];
  const total = st && typeof st.mismatch_count === 'number' ? st.mismatch_count : list.length;
  if (!total) return null;

  const wrap = h('section', 'df-group');
  const headingID = 'df-export-mismatches';
  const heading = h('h2', 'df-group__title', total === 1 ? 'The file that did not match' : humanCount(total) + ' files did not match');
  heading.id = headingID;
  wrap.setAttribute('aria-labelledby', headingID);
  add(wrap, heading);
  add(wrap, h('p', 'df-group__description',
    'These are the files the drive gave back differently from the way they were written. A drive that does this to one file will usually do it to others.'));

  const box = boxedList();
  if (list.length) {
    const table = h('table', 'df-table');
    const thead = h('thead');
    const htr = h('tr');
    add(htr, h('th', 'df-table__label', 'File'), h('th', 'df-table__label', 'What went wrong'), h('th', 'df-table__detail', 'Detail'));
    add(thead, htr);
    const tbody = h('tbody');
    for (const m of list) {
      const tr = h('tr');
      add(tr, (() => { const td = h('td', 'df-table__label'); add(td, h('span', 'df-mono df-text-sm', m.path || '')); return td; })());
      add(tr, h('td', 'df-table__label', MISMATCH_REASON[m.reason] || m.reason || 'unknown'));
      const dc = h('td', 'df-table__detail');
      const stack = h('div', 'df-stack df-stack--tight');
      // A detail that only restates the reason is noise in a table that
      // already has a reason column.
      const reasonText = MISMATCH_REASON[m.reason] || m.reason || '';
      if (m.detail && m.detail !== reasonText) add(stack, h('div', null, m.detail));
      if (typeof m.want_bytes === 'number' && typeof m.got_bytes === 'number' && m.want_bytes !== m.got_bytes) {
        add(stack, h('div', 'df-text-sm', 'expected ' + humanBytes(m.want_bytes) + ', got ' + humanBytes(m.got_bytes)));
      }
      // The two digests are what make "this drive corrupted the copy" a fact
      // rather than a guess, so they are on screen and copyable.
      if (m.want_sha256 || m.got_sha256) {
        add(stack, logBlock(['expected ' + (m.want_sha256 || '(none)'), 'got      ' + (m.got_sha256 || '(none)')]));
      }
      add(dc, stack);
      add(tr, dc);
      add(tbody, tr);
    }
    add(table, thead, tbody);
    add(box, tableSlab(table, 'Files that did not match'));
  }

  const foot = h('div', 'df-boxed-list__footer');
  add(foot, h('span', null, st.mismatches_truncated
    ? 'Showing the first ' + humanCount(list.length) + ' of ' + humanCount(total) + '. The complete record is in the bundle’s evidence.'
    : humanCount(total) + (total === 1 ? ' file' : ' files') + ' listed.'));
  add(foot, h('div', 'df-spacer'));
  add(foot, button('Copy the list', 'df-btn--ghost df-btn--sm', () => copyText(
    list.map((m) => [m.path || '', m.reason || '', m.detail || ''].join('  ')).join(String.fromCharCode(10))
  ), { iconName: 'copy' }));
  add(box, foot);
  add(wrap, box);
  return wrap;
}


function postCopyActions(dest) {
  const wrap = h('div', 'df-stack');
  if (S.bundleExitClass === 'incomplete') {
    add(wrap, h('p', 'df-text-secondary', 'The source bundle is incomplete. Build the missing items before installing.'));
    return wrap;
  }
  if (S.finished?.status?.verified) {
    add(wrap, h('p', null, S.bundleSigned === false
      ? 'This source is unsigned. The offline commands require your explicit acceptance of an unauthenticated bundle.'
      : 'On the offline machine, verify the bundle with an independently trusted key before installing.'));
  }
  const details = h('details', 'df-disclosure');
  const body = h('div', 'df-disclosure__body df-stack');
  add(body, button('Copy destination path', 'df-btn--secondary df-btn--sm', () => copyText(dest)));
  add(body, button('Check bundle signature', 'df-btn--secondary df-btn--sm', () => startVerify(dest)));
  const commands = (S.bundleSigned === false ? [
    'debark verify /path/to/bundle --allow-unsigned',
    'debark install /path/to/bundle --allow-unsigned --yes',
  ] : [
    'debark verify /path/to/bundle --key /path/to/trusted-operator.pub',
    'debark install /path/to/bundle --key /path/to/trusted-operator.pub --yes',
  ]).join('\n');
  add(body, h('p', 'df-text-secondary', 'Replace the example paths on the offline machine. Obtain the public key separately from a trusted source; a key beside the bundle does not establish trust.'));
  if (S.bundleSigned === false) add(body, h('p', 'df-text-secondary', 'These commands deliberately allow an unsigned bundle. They check integrity without authenticating the producer. Only use them if you accept that risk.'));
  add(body, logBlock(commands), button('Copy installation commands', 'df-btn--secondary df-btn--sm', () => copyText(commands)));
  add(wrap, bindDisclosure(add(details, h('summary', 'df-summary', 'Installation and signature details'), body)));
  return wrap;
}

/**
 * The block that must be impossible to mistake for a good copy. It names the
 * marker file, names the folder, and says the one thing that matters: do not
 * carry this drive to the offline machine.
 */
function incompleteBlock(dest, why) {
  const el = banner(
    'danger',
    'Incomplete copy — do not install',
    'Copy again with verification enabled, or remove this destination folder before using the drive.'
  );
  el.querySelector('.df-banner__body').appendChild(drawer('Incomplete marker details', markerSentence(dest) + ' The marker remains because the copy ' + (why === 'cancelled' ? 'was cancelled.' : 'did not finish and verify.')));
  return el;
}

function renderVerifySection() {
  const box = S.nodes.verifyBody;
  box.replaceChildren();
  S.nodes.verifyPanel.hidden = !(S.verifyRunning || S.verify);

  if (S.verifyRunning) {
    add(
      box,
      stateBlock('loading', {
        title: `Running ${CLI_BINARY} verify on the copy`,
        text: 'This checks the bundle’s own manifest and signature, which is a different question from "did every byte arrive".',
      })
    );
    return;
  }
  const v = S.verify;
  if (!v) return;

  if (v.cancelled) {
    add(box, banner('warning', 'Verification was cancelled', 'Nothing was concluded about the bundle.'));
    return;
  }
  if (v.error) {
    add(box, stateBlock('error', { error: v.error,
      actions: [{ label: 'Retry verification status', variant: 'secondary', onClick: recoverVerifyStatus }] }));
  } else if (v.ok && v.signed) {
    add(box, banner('success', `${CLI_BINARY} verify: the bundle is intact and signed`, 'Its manifest matches its contents and it carries a signature.'));
  } else if (v.ok && !v.signed) {
    add(
      box,
      banner('warning', `${CLI_BINARY} verify: intact, but unsigned`, [
        'The bundle’s manifest matches its contents, so nothing is corrupt.',
        'It carries no signature, so the offline machine cannot check who produced it.',
      ])
    );
  } else {
    add(box, banner('danger', `${CLI_BINARY} verify: this bundle did not pass`, 'The findings below say what is wrong. Do not install it on the offline machine.'));
  }

  if (v.findings && v.findings.length) {
    const vbox = boxedList();
    const table = h('table', 'df-table');
    const tbody = h('tbody');
    for (const f of v.findings) {
      const tr = h('tr');
      const sc = h('td', 'df-table__status');
      const role = f.severity === 'error' ? 'danger' : f.severity === 'warning' ? 'warning' : 'info';
      const st = status(role, '', role);
      add(st, h('span', 'df-visually-hidden', f.severity || 'info'));
      add(sc, st);
      add(tr, sc);
      const dc = h('td');
      const stack = h('div', 'df-stack df-stack--tight');
      add(stack, h('div', null, f.message || ''));
      if (f.path) add(stack, h('div', 'df-mono df-text-sm df-text-secondary df-truncate', f.path));
      if (f.hint) add(stack, h('div', 'df-text-sm df-text-secondary', f.hint));
      add(dc, stack);
      add(tr, dc);
      add(tbody, tr);
    }
    add(table, tbody);
    add(vbox, tableSlab(table, 'Verification findings'));
    add(box, vbox);
  }
}

/* --------------------------------------------------------------------------
 * The action bar.
 * ------------------------------------------------------------------------ */

function renderActions() {
  const n = S.nodes;
  const canStart =
    !S.running && !S.buildRunning && !S.stopping && !S.choosing && !S.statusError &&
    !!S.bundlePath &&
    !!S.selectedPath &&
    !S.selectedMissing &&
    !S.planning &&
    !S.planError &&
    !S.subdirError &&
    !!S.plan &&
    (!needsInternalConfirm() || S.internalConfirmed);

  S.canStart = canStart;
  // aria-disabled, never `disabled`. A disabled primary action leaves the tab
  // order, so a keyboard operator never meets it and is never told it exists
  // or why it is unavailable. The action bar's status line is the reason, and
  // it reaches assistive technology twice: as a live region when it changes,
  // and as the button's own description when the button takes focus.
  n.startBtn.setAttribute('aria-disabled', canStart ? 'false' : 'true');
  n.startBtn.className = 'df-btn ' + (S.skipVerify ? 'df-btn--danger' : 'df-btn--primary');
  n.startLabel.textContent = S.skipVerify ? 'Copy WITHOUT verifying' : 'Copy and verify';
  n.startBtn.hidden = S.running || !!S.finished;

  n.cancelBtn.hidden = !S.running;
  if (n.openBtn) n.openBtn.hidden = !S.finished;
  if (n.anotherBtn) n.anotherBtn.hidden = !S.finished;
  if (n.setupPanel) n.setupPanel.hidden = S.running || !!S.finished;
  if (n.unsafeNote) n.unsafeNote.hidden = !S.skipVerify;
  if (n.heading) n.heading.textContent = S.running ? 'Copying bundle' : S.finished ? (S.finished.status?.verified && !S.finished.error ? 'Copy checked' : 'Copy needs attention') : 'Copy to drive';
  n.subdirInput.disabled = S.running || S.stopping;
  n.skipInput.disabled = S.running || S.stopping;

  n.statusText.textContent = actionStatusText();
}

/**
 * Move the operator to whatever is stopping the copy.
 *
 * Refusing a click and saying why is half the aria-disabled idiom; the other
 * half is putting focus on the control that will unblock it.
 */
function focusStartBlocker() {
  const pick = (el) => {
    if (!el || typeof el.focus !== 'function') return false;
    try { el.focus(); return true; } catch (_) { return false; }
  };
  if (S.subdirError) { S.nodes.advanced.open = true; pick(S.nodes.subdirInput); return; }
  if (needsInternalConfirm() && !S.internalConfirmed) {
    if (pick(S.nodes.destNotes.querySelector('input[type="checkbox"]'))) return;
  }
  if (!S.selectedPath || S.selectedMissing) {
    if (pick(S.nodes.destBox.querySelector('input[type="radio"]'))) return;
    if (pick(S.nodes.folderBtn)) return;
  }
  if (S.planError) { pick(S.nodes.planStates.querySelector('button')); return; }
  pick(S.nodes.page);
}

function needsInternalConfirm() {
  const v = selectedVolume() || hostVolumeFor(S.selectedPath);
  return !!(v && !v.removable);
}

function actionStatusText() {
  if (S.statusError) return S.statusError;
  if (S.stopping) return 'Stopping the active operation…';
  if (S.buildRunning) return 'A build is running. Wait for it to finish before copying.';
  if (S.choosing) return 'Choose the bundle folder.';
  if (!S.bundlePath) return 'Choose a bundle to copy.';
  if (S.finished) return S.finished.status?.destination_path || S.finished.status?.destination || '';
  if (S.running) {
    const p = S.progress;
    const t = PHASE_TEXT[(p && p.phase) || 'checking'] || PHASE_TEXT.checking;
    return t.title;
  }
  if (!S.selectedPath) return 'Choose where the bundle should go.';
  if (S.selectedMissing) return 'The chosen drive is no longer connected.';
  if (S.subdirError) return 'Fix the folder name before copying.';
  if (S.planning) return 'Working out what would be copied…';
  if (S.planError) return 'This copy cannot start — see above.';
  if (needsInternalConfirm() && !S.internalConfirmed) return 'Confirm you meant a destination that is not removable media.';
  if (S.plan) {
    return humanCount(S.plan.total_files) + ' files · ' + humanBytes(S.plan.total_bytes) + ' → ' + S.plan.dest_dir;
  }
  return '';
}

/* --------------------------------------------------------------------------
 * Behaviour.
 * ------------------------------------------------------------------------ */

function destinationKey() {
  return destinationKeyFor(S.plan ? S.plan.dest_dir : S.selectedPath);
}

function destinationKeyFor(p) {
  return String(p || '');
}

function selectDestination(path, isVolume) {
  if (S.running) return;
  if (S.selectedPath === path && S.selectedIsVolume === isVolume) return;
  S.selectedPath = path;
  S.selectedIsVolume = isVolume;
  S.selectedMissing = false;
  S.internalConfirmed = false;
  S.plan = null;
  S.planError = null;
  S.incompleteHere = false;
  S.incompleteMessage = '';
  S.finished = null;
  resetVerification();
  renderVerifySection();
  renderOutcome();
  renderSelectionNotes();
  renderPlan();
  renderActions();
  inspectDestination(path);
  runPlan();
}

/**
 * Warn about an interrupted export already sitting on the chosen destination.
 *
 * This is the one thing on this screen the operator cannot see for themselves:
 * a folder holding a half-copied bundle looks exactly like a finished one.
 * `InspectDestination` is two stat calls and writes nothing, so it runs the
 * moment a destination is chosen — before the bundle walk `PlanExport` pays
 * for, and well before anything is written.
 *
 * It is a warning, not a refusal. Exporting again is exactly the right fix:
 * the copy rewrites the marker and removes it only once every file has been
 * read back and matched.
 */
async function inspectDestination(path) {
  const b = bindings();
  const token = ++S.inspectToken;
  if (S.incompleteDests.has(destinationKeyFor(path))) S.incompleteHere = true;
  if (b && typeof b.InspectDestination === 'function') {
    try {
      const d = await b.InspectDestination(exportOptions());
      if (S.selectedPath !== path || token !== S.inspectToken || !S.mounted) return;
      if (d && !d.error) {
        if (d.marker_name) S.markerName = d.marker_name;
        S.incompleteHere = S.incompleteHere || !!d.incomplete;
        S.incompleteMessage = d.incomplete ? (d.message || '') : '';
      }
    } catch (_) {
      /* a failed probe is not a reason to block the screen */
    }
  }
  renderSelectionNotes();
}

async function chooseFolder() {
  const b = bindings();
  if (!b || S.running || S.stopping) return;
  const entry = S.entrySeq;
  try {
    const res = await b.ChooseDirectory('Choose a folder to copy the bundle into', S.folderDest || '');
    if (entry !== S.entrySeq || !S.mounted || S.running || S.stopping) return;
    if (!res || res.cancelled) return;
    if (res.error) {
      toast('danger', res.error.message || 'The folder picker failed.');
      return;
    }
    S.folderDest = res.path;
    renderDestinations();
    selectDestination(res.path, false);
    renderDestinations();
  } catch (e) {
    toast('danger', 'The folder picker could not be opened.');
  }
}

async function chooseBundle(returnOnCancel = false) {
  const b = bindings();
  if (!b || S.running || S.choosing || S.stopping) return;
  const entry = S.entrySeq;
  S.choosing = true;
  renderActions();
  try {
    const res = await b.ChooseDirectory('Choose the bundle folder to copy', S.bundleResolved || '');
    if (entry !== S.entrySeq || !S.mounted || S.running || S.stopping) return;
    if (!res || res.cancelled) {
      if (returnOnCancel) S.ctx.go(S.returnTo || 'target');
      return;
    }
    if (res.error) {
      toast('danger', res.error.message || 'The folder picker failed.');
      return;
    }
    setSource(res.path, null, '');
    await loadBundle();
    renderSource();
    if (S.selectedPath) runPlan();
  } catch (e) {
    toast('danger', 'The folder picker could not be opened.');
  } finally {
    if (entry === S.entrySeq && S.mounted) {
      S.choosing = false;
      renderActions();
    }
  }
}

function setSource(path, summary, target) {
  resetForAnotherCopy(false);
  S.statusSeq++;
  S.acceptFinished = false;
  S.jobStartedAt = '';
  S.bundlePath = path || '';
  S.bundleResolved = path || '';
  S.bundleSigned = summary && typeof summary.signed === 'boolean' ? summary.signed : null;
  S.bundleExitClass = summary?.exit_class || '';
  S.sourceTarget = target || '';
  renderSource();
  renderActions();
}

function exportOptions() {
  return {
    bundle_path: S.bundlePath || '',
    destination: S.selectedPath || '',
    subdir: S.subdir || '',
    skip_verify: !!S.skipVerify,
  };
}

function validateSubdir() {
  const v = S.subdir;
  S.subdirError = '';
  if (!v) return true;
  if (v.includes('/') || v.includes('\\')) {
    S.subdirError = 'It must be a single folder name, with no "/" or "\\" in it.';
  } else if (v === '.' || v === '..') {
    S.subdirError = '"." and ".." are not folder names.';
  }
  return !S.subdirError;
}

async function runPlan() {
  const b = bindings();
  if (!b || !S.selectedPath || !S.bundlePath || S.running) return;
  const token = ++S.planToken;
  if (!validateSubdir()) {
    S.planning = false;
    S.plan = null;
    renderPlan();
    renderActions();
    return;
  }
  S.planning = true;
  S.planError = null;
  renderPlan();
  renderActions();

  let res;
  try {
    res = await b.PlanExport(exportOptions());
  } catch (e) {
    res = { error: { code: 'export.failed', message: 'The plan could not be worked out.', hint: 'Check the drive is still connected, then try again.', details: String(e) } };
  }
  if (token !== S.planToken || !S.mounted || S.running) return;
  S.planning = false;
  if (res && res.error) {
    S.planError = res.error;
    S.plan = null;
  } else {
    S.plan = (res && res.plan) || null;
    S.planError = null;
    // ExportPlan carries the same fact, so a screen that only planned is not
    // left blind.
    if (S.plan && S.plan.destination_incomplete) S.incompleteHere = true;
  }
  renderPlan();
  renderActions();
}

async function startExport() {
  const b = bindings();
  if (!b) return;
  // The button stays focusable and clickable while it is gated, so the refusal
  // lives here. Rewriting the live region re-announces it — an unchanged live
  // region says nothing.
  if (!S.canStart || S.running) {
    const why = actionStatusText();
    S.nodes.statusText.textContent = '';
    S.nodes.statusText.textContent = why;
    toast('warning', why || 'This copy cannot start yet.');
    focusStartBlocker();
    return;
  }
  S.finished = null;
  S.progress = { phase: 'checking', fraction: -1, files_done: 0, files_total: 0, bytes_done: 0, bytes_total: 0 };
  resetVerification();
  renderVerifySection();
  S.running = true;
  S.acceptFinished = true;
  S.jobStartedAt = '';
  S.runStartedAt = Date.now();
  S.phaseStartedAt = Date.now();
  S.lastPhase = 'checking';
  stopPolling();
  renderOutcome();
  renderRun();
  renderActions();
  renderDestinations();
  startTicker();

  let res;
  try {
    res = await b.StartExport(exportOptions());
  } catch (e) {
    res = { ok: false, error: { code: 'export.failed', message: 'The copy could not be started.', details: String(e) } };
  }
  if (res?.ok) await recoverStatus();
  if (!res || !res.ok) {
    S.acceptFinished = false;
    S.statusSeq++;
    S.running = false;
    stopTicker();
    startPolling();
    if (res && res.error) {
      S.planError = res.error;
      S.plan = null;
      renderPlan();
    }
    renderRun();
    renderActions();
    renderDestinations();
  }
}

async function cancelExport() {
  const b = bindings();
  if (!b) return;
  if (S.cancelling) return;
  S.cancelling = true;
  S.nodes.cancelBtn.setAttribute('aria-disabled', 'true');
  S.nodes.cancelBtn.setAttribute('aria-busy', 'true');
  S.nodes.cancelLabel.textContent = 'Stopping…';
  try {
    const result = await b.CancelExport();
    if (result?.error) throw new Error(result.error.message + ' ' + (result.error.hint || ''));
    await recoverStatus();
  } catch (error) {
    S.cancelling = false;
    S.nodes.cancelBtn.setAttribute('aria-disabled', 'false');
    S.nodes.cancelBtn.removeAttribute('aria-busy');
    S.nodes.cancelLabel.textContent = 'Cancel';
    toast('danger', error.message || 'The copy could not be stopped. Try Cancel again.');
  }
}

async function startVerify(path) {
  const b = bindings();
  const context = verificationContext();
  if (!b || !context || path !== context.path || S.verifyRunning || S.verifyStarting || S.stopping) return;
  const sequence = ++S.verifyStartSeq;
  S.verifySeq++;
  S.verifyStartedCopy = context.key;
  S.verifyStarting = true;
  S.verifyRunning = true;
  S.verify = null;
  renderVerifySection();
  let result;
  try {
    result = await b.StartVerify(path);
  } catch (e) {
    result = { error: { message: 'Verification could not be started.', details: String(e) } };
  }
  if (!S.mounted || sequence !== S.verifyStartSeq || context.key !== verificationContext()?.key) return;
  S.verifyStarting = false;
  if (!result?.ok) {
    S.verifyStartedCopy = '';
    S.verifySeq++;
    S.verifyRunning = false;
    S.verify = { error: result?.error || { message: 'Verification could not be started.' } };
    renderVerifySection();
    return;
  }
  await recoverVerifyStatus();
}

function resetVerification() {
  S.verifySeq++;
  S.verifyStartSeq++;
  S.verifyStartedCopy = '';
  S.verify = null;
  S.verifyRunning = false;
  S.verifyStarting = false;
}

function verificationContext() {
  const copy = S.finished?.status;
  if (!copy?.verified || S.finished.error || S.finished.cancelled || !copy.destination_path) return null;
  return { path: copy.destination_path, finishedAt: copy.finished_at,
    key: JSON.stringify([copy.bundle_path, copy.destination_path, copy.started_at, copy.finished_at]) };
}

// Notifications carry no authority. A read belongs to this screen entry and
// completed copy; a newer notification or source reset invalidates it.
async function recoverVerifyStatus() {
  const sequence = ++S.verifySeq;
  const entry = S.entrySeq;
  const context = verificationContext();
  if (!S.mounted || !context) return;
  try {
    const status = await bindings().VerifyStatus();
    if (!S.mounted || entry !== S.entrySeq || sequence !== S.verifySeq || context.key !== verificationContext()?.key) return;
    if (!status || ['running', 'finished', 'cancelled', 'ok', 'signed'].some(key => typeof status[key] !== 'boolean')) {
      throw new Error('The backend did not return a valid verification status.');
    }
    // The StartVerify request may still be queued in the bridge. Its old idle
    // or terminal status cannot dismiss the pending action or admit a repeat.
    if (S.verifyStarting && !status.running) return;
    const started = Date.parse(status.started_at);
    const copied = Date.parse(context.finishedAt);
    // A check of this path before it was copied again says nothing about the
    // new contents. Native timestamps have second precision: equality can be
    // attributed to this copy only after its explicit Check action in session.
    const afterCopy = Number.isFinite(started) && Number.isFinite(copied) && started > copied;
    const requestedHere = S.verifyStartedCopy === context.key && (!Number.isFinite(copied) || !Number.isFinite(started) || started >= copied);
    const current = status.bundle_path === context.path && (afterCopy || requestedHere || status.running && started >= copied);
    S.verifyRunning = current && status.running;
    S.verify = current && !status.running && status.finished ? status : null;
  } catch (error) {
    if (!S.mounted || entry !== S.entrySeq || sequence !== S.verifySeq || context.key !== verificationContext()?.key) return;
    if (S.verifyStarting) return;
    S.verifyRunning = false;
    S.verify = { error: { message: 'Verification status is unavailable.', hint: 'Retry the status check before relying on this result.', details: String(error) } };
  }
  renderVerifySection();
}

function resetForAnotherCopy(refresh = true) {
  if (S.running || S.stopping) return;
  S.statusSeq++;
  S.planToken++;
  S.inspectToken++;
  S.acceptFinished = false;
  S.jobStartedAt = '';
  S.planning = false;
  S.skipVerify = false;
  S.subdir = '';
  S.subdirError = '';
  S.selectedMissing = false;
  S.incompleteHere = false;
  S.folderDest = '';
  if (S.nodes.skipInput) S.nodes.skipInput.checked = false;
  if (S.nodes.subdirInput) S.nodes.subdirInput.value = '';
  S.finished = null;
  resetVerification();
  S.progress = null;
  S.plan = null;
  S.planError = null;
  S.selectedPath = '';
  S.selectedIsVolume = false;
  S.internalConfirmed = false;
  renderOutcome();
  renderVerifySection();
  renderDestinations();
  renderPlan();
  renderActions();
  if (refresh) pollVolumes(true);
}

async function revealPath(path) {
  const b = bindings();
  if (!b || !path) return;
  try {
    const res = await b.RevealPath(path);
    if (res && res.error) toast('warning', res.error.message || 'That folder could not be opened.');
  } catch (_) {
    toast('warning', 'That folder could not be opened.');
  }
}

async function copyText(text) {
  const b = bindings();
  if (!b || !text) return;
  try {
    const res = await b.CopyToClipboard(text);
    if (res && res.ok !== false) toast('success', 'Copied.');
  } catch (_) {
    /* nothing to say */
  }
}

function errorText(err) {
  const parts = [err.message || ''];
  if (err.hint) parts.push(err.hint);
  if (err.details) parts.push(err.details);
  return parts.filter(Boolean).join('\n');
}

/* --------------------------------------------------------------------------
 * Polling the volume list.
 *
 * ListVolumes is cheap by design and the binding surface says to poll it and
 * compare `fingerprint`. The rule that matters here is that an unrelated drive
 * appearing must not disturb the operator: the table is only rebuilt when the
 * fingerprint moves, the selection is keyed on the destination path rather than
 * on a row index, and keyboard focus is put back on the row that had it.
 * ------------------------------------------------------------------------ */

function startPolling() {
  stopPolling();
  if (!S.visible || S.running) return;
  pollVolumes(true);
  S.pollTimer = window.setInterval(() => pollVolumes(false), VOLUME_POLL_MS);
}

function stopPolling() {
  if (S.pollTimer) window.clearInterval(S.pollTimer);
  S.pollTimer = 0;
}

async function pollVolumes(force) {
  if (S.running) return;
  const b = bindings();
  if (!b) {
    S.volumesLoaded = true;
    S.volumesError = { message: `${APP_NAME} has no backend attached.`, hint: `Restart ${APP_NAME}.` };
    renderDestinations();
    return;
  }
  let res;
  try {
    res = await b.ListVolumes();
  } catch (e) {
    res = { volumes: [], supported: true, error: { message: 'The list of mounted drives could not be read.', hint: 'Try again; if it keeps failing, choose a folder instead.', details: String(e) } };
  }
  if (!S.visible) return;

  // One signature covering every reason the panel would look different, so an
  // unchanged poll never rebuilds the table and never disturbs focus. The
  // backend's own fingerprint is used when it supplies one.
  const supported = res ? res.supported !== false : true;
  const sig = res && res.error
    ? 'err|' + (res.error.message || '')
    : (supported ? '' : 'unsupported|') +
      (res && res.fingerprint
        ? res.fingerprint
        : ((res && res.volumes) || [])
            .map((v) => [v.path, v.free_bytes, v.total_bytes, v.writable, v.read_only, v.note].join('~'))
            .join(','));
  const same = !force && sig === S.fingerprint;
  S.fingerprint = sig;
  S.volumesLoaded = true;
  S.volumesSupported = supported;
  S.volumesError = (res && res.error) || null;
  S.volumes = (res && res.volumes) || [];

  // The chosen drive going away is worth interrupting for; anything else is
  // not, so an unchanged fingerprint short-circuits the whole redraw.
  const missing =
    !!S.selectedPath &&
    S.selectedIsVolume &&
    S.volumesSupported &&
    !S.volumesError &&
    !S.volumes.some((v) => v.path === S.selectedPath);
  const missingChanged = missing !== S.selectedMissing;
  S.selectedMissing = missing;
  if (missingChanged && missing) {
    S.planToken++;
    S.inspectToken++;
    S.planning = false;
    S.plan = null;
    S.planError = null;
    renderPlan();
  }

  if (same && !missingChanged) return;
  renderDestinations();
  renderActions();
}

/* Local 1 Hz clock. It exists so the flush phase — which sends no events at
 * all while it runs — visibly keeps counting instead of looking frozen. */
function startTicker() {
  stopTicker();
  S.tickTimer = window.setInterval(() => {
    if (S.running) renderRun();
  }, 1000);
}

function stopTicker() {
  if (S.tickTimer) window.clearInterval(S.tickTimer);
  S.tickTimer = 0;
}

/* --------------------------------------------------------------------------
 * Events.
 * ------------------------------------------------------------------------ */

function onExportStarted(st) {
  S.running = true;
  S.finished = null;
  S.runStartedAt = Date.now();
  S.phaseStartedAt = Date.now();
  S.progress = (st && st.progress) || S.progress;
  S.lastPhase = (S.progress && S.progress.phase) || 'checking';
  stopPolling();
  startTicker();
  renderOutcome();
  renderRun();
  renderActions();
  renderDestinations();
}

function onExportProgress(p) {
  if (!p) return;
  S.progress = p;
  if (p.phase !== S.lastPhase) {
    S.lastPhase = p.phase;
    S.phaseStartedAt = Date.now();
  }
  // The engine emits at ~10 Hz. Coalesce to one paint per frame.
  if (S.rafHandle) return;
  S.rafHandle = window.requestAnimationFrame(() => {
    S.rafHandle = 0;
    renderRun();
    S.nodes.statusText.textContent = actionStatusText();
  });
}

function onExportFinished(f) {
  S.running = false;
  stopTicker();
  if (S.rafHandle) {
    window.cancelAnimationFrame(S.rafHandle);
    S.rafHandle = 0;
  }
  S.finished = f || { ok: false, cancelled: false, status: {} };
  S.progress = (f && f.status && f.status.progress) || S.progress;
  const st = (f && f.status) || {};
  const dest = st.destination_path || st.destination;
  if (dest && (f.cancelled || f.error || !st.verified)) S.incompleteDests.add(destinationKeyFor(dest));
  if (dest && st.verified) S.incompleteDests.delete(destinationKeyFor(dest));
  S.cancelling = false;
  S.nodes.cancelBtn.setAttribute('aria-disabled', 'false');
  S.nodes.cancelBtn.removeAttribute('aria-busy');
  S.nodes.cancelLabel.textContent = 'Cancel';
  renderRun();
  renderOutcome();
  renderActions();
  renderDestinations();
  startPolling();
}

/* --------------------------------------------------------------------------
 * Recovery — a screen that mounts late, or a window that reloaded mid-copy,
 * must not show an idle screen while a copy is running.
 * ------------------------------------------------------------------------ */

async function recoverStatus() {
  const b = bindings();
  if (!b) return;
  const sequence = ++S.statusSeq;
  try {
    const st = await b.ExportStatus();
    if (sequence !== S.statusSeq || !S.mounted) return;
    applyExportStatus(st);
  } catch (error) {
    if (sequence === S.statusSeq) S.nodes.statusText.textContent = 'Copy status is unavailable. Reopen this view to check it.';
  }
}

function applyExportStatus(st) {
  if (!st) return;
  if (st.running) {
    if (st.bundle_path && st.bundle_path !== S.bundlePath) {
      S.bundlePath = S.bundleResolved = st.bundle_path;
      S.bundleSigned = null;
      S.bundleExitClass = '';
      S.sourceTarget = '';
    }
    if (st.started_at !== S.jobStartedAt) {
      S.runStartedAt = Date.parse(st.started_at) || Date.now();
      S.phaseStartedAt = Date.now();
    }
    S.jobStartedAt = st.started_at || S.jobStartedAt;
    S.acceptFinished = true;
    S.running = true;
    S.finished = null;
    resetVerification();
    renderVerifySection();
    S.selectedPath = st.destination || S.selectedPath;
    onExportProgress(st.progress);
    stopPolling();
    if (S.visible) startTicker();
  } else if (st.finished && S.acceptFinished &&
    (!S.bundlePath || st.bundle_path === S.bundlePath) &&
    (!S.jobStartedAt || !st.started_at || st.started_at === S.jobStartedAt)) {
    S.bundlePath = S.bundleResolved = st.bundle_path || S.bundlePath;
    S.selectedPath = st.destination || S.selectedPath;
    S.jobStartedAt = st.started_at || S.jobStartedAt;
    onExportFinished({ ok: !st.error && !st.cancelled, cancelled: !!st.cancelled, status: st, error: st.error || null });
  }
  renderSource();
  renderRun();
  renderOutcome();
  renderActions();
}

function onLifecycle(view) {
  S.lifecycleSeq++;
  S.buildRunning = !!view?.build_running;
  S.stopping = !!view?.stopping;
  renderActions();
}

async function loadBundle() {
  const b = bindings();
  if (!b || !S.bundlePath) return;
  const path = S.bundlePath;
  const entry = S.entrySeq;
  try {
    const st = await b.BuildStatus();
    const sum = st && st.summary;
    if (entry !== S.entrySeq || path !== S.bundlePath) return;
    if (sum && sum.bundle_path === path) {
      S.bundleResolved = sum.bundle_path;
      S.bundleSigned = typeof sum.signed === 'boolean' ? sum.signed : null;
      S.bundleExitClass = sum.exit_class || '';
      S.sourceTarget = st.target_id || '';
    }
  } catch (_) {
    /* the plan resolves the bundle anyway; this is only for the header */
  }
  renderSource();
}

/* --------------------------------------------------------------------------
 * Mounting.
 * ------------------------------------------------------------------------ */

let sectionSeq = 0;

/**
 * One group: a real heading as PLAIN TEXT above whatever the group contains.
 *
 * This used to be a `.df-panel` per section — six bordered cards stacked down
 * one column, each with its title inside the box. Design rule 9: the title
 * goes above, the box below is one seamless surface, and 32px of space
 * separates groups instead of another border.
 *
 * The `<section>` still points at its own heading with aria-labelledby,
 * because a `<section>` with no accessible name is not a landmark and
 * assistive technology skips it. That defect predates the redesign.
 */
function section(title, description, opts) {
  const o = opts || {};
  const headingID = 'df-export-section-' + (++sectionSeq);
  const heading = h('h2', 'df-group__title', title);
  heading.id = headingID;
  const head = h('div', 'df-cluster');
  add(head, heading);
  for (const extra of o.head || []) add(head, extra);
  const panel = h('section', 'df-group');
  panel.setAttribute('aria-labelledby', headingID);
  add(panel, head);
  if (description) add(panel, h('p', 'df-group__description', description));
  const body = h('div', 'df-stack');
  add(panel, body);
  return { panel, body, head };
}

/** The box itself. One border for a whole group, never one per row. */
function boxedList() {
  return h('div', 'df-boxed-list');
}

/**
 * One row of a boxed list.
 *
 * `description` is the ONE line under a label that a row is allowed. Anything
 * longer belongs behind `helpButton` — permanent micro-copy under every
 * control is the SaaS-onboarding habit that made this screen dense.
 */
function boxRow(opts) {
  const o = opts || {};
  const text = h('span', 'df-boxed-list__text');
  if (o.label != null) {
    const lab = h(o.labelFor ? 'label' : 'span', 'df-boxed-list__label', o.label);
    if (o.labelFor) lab.setAttribute('for', o.labelFor);
    add(text, lab);
  }
  let desc = null;
  if (o.description || o.descriptionSlot) {
    desc = h('span', 'df-boxed-list__description', o.description || '');
    if (!o.description) desc.hidden = true;
    add(text, desc);
  }
  const row = h('div', 'df-boxed-list__row');
  add(row, text);
  if (o.control && o.control.length) {
    const ctl = h('div', 'df-boxed-list__control' + (o.field ? ' df-boxed-list__control--field' : ''));
    for (const c of o.control) add(ctl, c);
    add(row, ctl);
  }
  row.descEl = desc;
  return row;
}

/** A full-bleed child of the box: a table, a log. Keeps the box seamless. */
function slab(child) {
  const el = h('div', 'df-boxed-list__slab');
  add(el, child);
  return el;
}

/**
 * A table inside a boxed list, in its own horizontal scroller.
 *
 * `.df-boxed-list` is `overflow: hidden` so it can clip rows to its radius,
 * which means a table wider than the box is CLIPPED rather than scrollable and
 * its last columns become unreachable. Measured at a 320px container: the
 * volume table wants about 855px. `.df-scroller` is the design system's own
 * answer (README §6.13) and `tabindex` makes the region reachable by keyboard,
 * which a scrollable region has to be. WCAG 1.4.10.
 */
function tableSlab(table, label) {
  const scroller = h('div', 'df-scroller');
  scroller.tabIndex = 0;
  scroller.setAttribute('role', 'group');
  scroller.setAttribute('aria-label', label);
  add(scroller, table);
  return slab(scroller);
}

/* --------------------------------------------------------------------------
 * Contextual help.
 *
 * `.df-help__btn` plus a layered `.df-popover` is what replaced the paragraphs
 * that used to sit permanently under controls on this screen. Layered, never
 * an inline <details>: an inline disclosure above the volume table reflows
 * everything below it, which moves the operator's place in the list.
 * ------------------------------------------------------------------------ */

let openHelp = null;

function onHelpKey(ev) {
  if (ev.key === 'Escape' && openHelp) {
    ev.preventDefault();
    closeHelp(true);
  }
}

function onHelpDown(ev) {
  if (openHelp && !openHelp.anchor.contains(ev.target)) closeHelp(false);
}

function closeHelp(restoreFocus) {
  const cur = openHelp;
  if (!cur) return;
  openHelp = null;
  cur.pop.hidden = true;
  cur.btn.setAttribute('aria-expanded', 'false');
  document.removeEventListener('keydown', onHelpKey, true);
  document.removeEventListener('mousedown', onHelpDown, true);
  if (restoreFocus) {
    try { cur.btn.focus(); } catch (_) { /* a detached opener is not worth throwing over */ }
  }
}

function helpButton(ariaLabel, title, paragraphs) {
  const id = 'df-export-help-' + (++sectionSeq);
  const pop = h('div', 'df-popover df-popover--end');
  pop.id = id;
  if (title) add(pop, h('p', 'df-popover__title', title));
  for (const para of paragraphs) add(pop, h('p', 'df-popover__text', para));
  pop.hidden = true;
  const btn = h('button', 'df-help__btn');
  btn.type = 'button';
  btn.setAttribute('aria-expanded', 'false');
  btn.setAttribute('aria-controls', id);
  btn.setAttribute('aria-label', ariaLabel);
  add(btn, icon('help'));
  const anchor = h('span', 'df-popover-anchor');
  add(anchor, btn, pop);
  btn.addEventListener('click', () => {
    if (openHelp && openHelp.pop === pop) { closeHelp(true); return; }
    closeHelp(false);
    openHelp = { btn, pop, anchor };
    pop.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    document.addEventListener('keydown', onHelpKey, true);
    document.addEventListener('mousedown', onHelpDown, true);
  });
  return anchor;
}

function buildDOM(root) {
  const layout = h('div', 'df-screen-layout');
  const scroll = h('div', 'df-screen-content');
  // --wide, not the 720px default: the volume table carries six columns and
  // the mount-point column is the one an operator reads to tell two drives
  // apart. The shell hands a bare .df-app__pane with no padding and no
  // measure, on purpose, so the measure is the screen's to choose.
  // .df-app__content is a plain block on purpose. Adding .df-stack to it makes
  // it a flex column, and .df-stack sets `min-height: 0`, so every child inside
  // a bounded .df-app__pane shrinks below its content and the whole screen
  // collapses into a few overlapping pixels. The stack goes INSIDE the column.
  const page = h('div', 'df-app__content df-app__content--wide');
  page.tabIndex = -1;      // focus restoration must never land on <body>
  S.nodes.page = page;
  const column = h('div', 'df-stack df-stack--loose');
  add(page, column);

  /* The step's own heading, and the safety boundary as the screen's own
     opening statement rather than a permanent bordered alert. It is still the
     first thing read, it is no longer a rectangle competing with the real
     ones, and a banner that is always there is a banner nobody reads. */
  const lede = h('div', 'df-stack df-stack--tight');
  S.nodes.heading = h('h1', null, 'Copy to drive');
  add(lede, S.nodes.heading);
  add(column, lede);

  // .df-groups is the 32px of space between groups that replaced six borders.
  const groups = h('div', 'df-groups');
  add(column, groups);

  /* 1 — source */
  const src = section('Bundle to copy', null);
  S.nodes.sourceBox = boxedList();
  S.nodes.sourceNotes = h('div', 'df-stack');
  add(src.body, S.nodes.sourceBox, S.nodes.sourceNotes);
  add(groups, src.panel);
  const setup = h('div', 'df-stack');
  S.nodes.setupPanel = setup;
  add(groups, setup);

  /* 2 — destination */
  S.nodes.folderBtn = button('Choose folder…', 'df-btn--secondary df-btn--sm', chooseFolder, { iconName: 'folder' });
  const refreshBtn = button('Refresh', 'df-btn--ghost df-btn--sm', () => pollVolumes(true), { iconName: 'refresh' });
  const dest = section(
    'Destination',
    null,
    { head: [h('div', 'df-spacer'), S.nodes.folderBtn, refreshBtn] }
  );

  S.nodes.destBox = boxedList();
  S.nodes.destStates = h('div', 'df-stack');
  S.nodes.destNotes = h('div', 'df-stack');
  add(dest.body, S.nodes.destBox, S.nodes.destStates, S.nodes.destNotes);

  /* The folder to create on the destination is a ROW of the destination box:
     it is part of choosing where the bundle lands, not a field floating under
     the table. The hint kept here is the one that prevents an error the
     validator would otherwise have to report — a path where a name belongs. */
  const fieldId = 'df-export-subdir';
  const input = h('input', 'df-input');
  input.id = fieldId;
  input.type = 'text';
  input.autocomplete = 'off';
  input.spellcheck = false;
  const errNode = h('span', 'df-field__error');
  errNode.hidden = true;
  add(errNode, icon('danger'), h('span', null, ''));
  const subdirRow = boxRow({
    label: 'Destination folder name',
    labelFor: fieldId,
    description: 'Leave blank to use the bundle folder name.',
    field: true,
    control: [input],
  });
  add(subdirRow, errNode);
  input.addEventListener('input', () => {
    S.subdir = input.value.trim();
    const ok = validateSubdir();
    input.classList.toggle('is-error', !ok);
    input.setAttribute('aria-invalid', ok ? 'false' : 'true');
    errNode.hidden = ok;
    errNode.lastChild.textContent = S.subdirError;
    S.planToken++;
    S.planning = false;
    S.plan = null;
    renderActions();
  });
  input.addEventListener('change', () => {
    if (S.selectedPath) { runPlan(); inspectDestination(S.selectedPath); }
  });
  S.nodes.subdirRow = subdirRow;
  S.nodes.subdirInput = input;
  add(setup, dest.panel);

  /* 3 — plan */
  const plan = section('Copy details', null);
  S.nodes.planStates = h('div', 'df-stack');
  S.nodes.planBox = boxedList();
  add(plan.body, S.nodes.planStates, S.nodes.planBox);

  /* The verification switch. It used to sit behind an "Advanced" disclosure
     with a paragraph under it; it is now a row of the plan box with the
     paragraph behind contextual help, because that paragraph is an argument
     the operator needs once, not a caption they need every time. */
  const skipInput = h('input', 'df-check__input');
  skipInput.type = 'checkbox';
  skipInput.id = 'df-export-skip-verify';
  skipInput.addEventListener('change', () => {
    S.skipVerify = skipInput.checked;
    renderPlan();
    renderActions();
    if (S.selectedPath) runPlan();
  });
  S.nodes.skipRow = boxRow({
    label: 'Skip verification',
    labelFor: skipInput.id,
    control: [
      skipInput,
      helpButton('About skipping verification', 'Skipping verification', [
        'Verification is the step that makes this feature worth having: it is what catches a drive that accepted the write and gave back different bytes.',
        'With it off, the copy is never reported as complete, the incomplete marker is never removed, and you are carrying an unchecked bundle across the air gap.',
      ]),
    ],
  });
  S.nodes.skipInput = skipInput;
  add(setup, plan.panel);
  const unsafeNote = banner('warning', 'Copy will be unchecked', 'Verification is off. The destination will remain marked incomplete.');
  unsafeNote.hidden = true;
  S.nodes.unsafeNote = unsafeNote;
  add(setup, unsafeNote);
  const advanced = h('details', 'df-disclosure');
  const advancedBody = h('div', 'df-disclosure__body');
  add(advancedBody, subdirRow, S.nodes.skipRow);
  bindDisclosure(add(advanced, h('summary', 'df-summary', 'Advanced'), advancedBody));
  S.nodes.advanced = advanced;
  add(setup, advanced);

  /* 4 — run. No box: a progress bar and a phase note are not a list of rows,
     and .df-progress draws no border of its own. */
  const run = section('Progress', null);
  add(run.body, buildRunPanel());
  run.panel.hidden = true;
  S.nodes.runPanel = run.panel;
  add(groups, run.panel);

  /* 5 — outcome */
  const out = section('Result', null);
  const outBody = h('div', 'df-stack');
  add(out.body, outBody);
  out.panel.hidden = true;
  S.nodes.outcomePanel = out.panel;
  S.nodes.outcomeBody = outBody;
  add(groups, out.panel);

  /* 5b — debark verify, run against the copy on the drive */
  const ver = section('Check it with the command-line tool', 'A different question from "did every byte arrive": this checks the bundle’s own manifest and signature.');
  const verBody = h('div', 'df-stack');
  add(ver.body, verBody);
  ver.panel.hidden = true;
  S.nodes.verifyPanel = ver.panel;
  S.nodes.verifyBody = verBody;
  add(groups, ver.panel);

  /* action bar */
  const bar = h('div', 'df-actionbar');
  const statusText = h('span', 'df-actionbar__status', '');
  statusText.id = 'df-export-action-status';
  statusText.setAttribute('role', 'status');
  statusText.setAttribute('aria-live', 'polite');
  const cancelBtn = h('button', 'df-btn df-btn--ghost');
  cancelBtn.type = 'button';
  const cancelLabel = h('span', null, 'Cancel');
  add(cancelBtn, icon('close', 'df-btn__icon'), cancelLabel);
  cancelBtn.addEventListener('click', cancelExport);
  cancelBtn.hidden = true;
  const startBtn = h('button', 'df-btn df-btn--primary');
  startBtn.type = 'button';
  startBtn.setAttribute('aria-describedby', statusText.id);
  const startLabel = h('span', null, 'Copy and verify');
  add(startBtn, startLabel);
  startBtn.addEventListener('click', startExport);
  const backBtn = button('Back', 'df-btn--ghost', () => S.ctx.go(S.returnTo || 'target'));
  const openBtn = button('Open folder', 'df-btn--secondary', () => revealPath(S.finished?.status?.destination_path || S.finished?.status?.destination || S.selectedPath));
  const anotherBtn = button('Copy to another drive', 'df-btn--primary', resetForAnotherCopy);
  openBtn.hidden = true;
  anotherBtn.hidden = true;
  add(bar, backBtn, statusText, openBtn, anotherBtn, cancelBtn, startBtn);
  S.nodes.openBtn = openBtn;
  S.nodes.anotherBtn = anotherBtn;

  S.nodes.statusText = statusText;
  S.nodes.startBtn = startBtn;
  S.nodes.startLabel = startLabel;
  S.nodes.cancelBtn = cancelBtn;
  S.nodes.cancelLabel = cancelLabel;

  add(scroll, page);
  add(layout, scroll, bar);
  add(root, layout);
}

/* --------------------------------------------------------------------------
 * The screen module (docs/dev/screen-contract.md).
 * ------------------------------------------------------------------------ */

/**
 * Returns every piece of screen state to its initial value.
 *
 * This module holds its state at module scope — the shell registers exactly one
 * instance of each screen — so `destroy` emptying the DOM is not enough: a
 * stale selection, a stale outcome banner or a stale "skip verification" tick
 * would reappear the next time the screen mounted, which is how a screen ends
 * up telling an operator a drive is missing when it is sitting right there.
 */
function resetState() {
  S.bundlePath = '';
  S.bundleResolved = '';
  S.bundleSigned = null;
  S.bundleExitClass = '';
  S.sourceTarget = '';
  S.returnTo = 'build';
  S.entrySeq++;
  S.statusSeq++;
  S.lifecycleSeq++;
  S.statusError = '';
  S.acceptFinished = true;
  S.jobStartedAt = '';
  S.choosing = false;
  S.buildRunning = false;
  S.stopping = false;
  S.volumes = [];
  S.fingerprint = null;
  S.volumesSupported = true;
  S.volumesError = null;
  S.volumesLoaded = false;
  S.folderDest = '';
  S.selectedPath = '';
  S.selectedIsVolume = false;
  S.selectedMissing = false;
  S.internalConfirmed = false;
  S.subdir = '';
  S.subdirError = '';
  S.skipVerify = false;
  S.planning = false;
  S.planToken++;
  S.inspectToken++;
  S.plan = null;
  S.planError = null;
  S.incompleteHere = false;
  S.incompleteMessage = '';
  S.markerName = '';
  S.canStart = false;
  S.cancelling = false;
  S.running = false;
  S.progress = null;
  S.runStartedAt = 0;
  S.phaseStartedAt = 0;
  S.lastPhase = '';
  S.finished = null;
  resetVerification();
}

const screen = {
  id: 'export',
  title: 'Copy the bundle to a drive',

  mount(root, ctx) {
    S.ctx = ctx;
    S.root = root;
    S.mounted = true;
    buildDOM(root);

    // Events request authoritative status; late notifications cannot resurrect
    // a previous source or turn a terminal result back into running progress.
    ctx.on('export:started', recoverStatus);
    ctx.on('export:progress', recoverStatus);
    ctx.on('export:finished', recoverStatus);
    ctx.on('app:lifecycle', onLifecycle);
    ctx.on('verify:started', recoverVerifyStatus);
    ctx.on('verify:finished', recoverVerifyStatus);

    renderSource();
    renderDestinations();
    renderPlan();
    renderRun();
    renderOutcome();
    renderActions();
  },

  async show(ctx, params) {
    S.ctx = ctx || S.ctx;
    S.visible = true;
    const p = params || {};
    const entry = ++S.entrySeq;
    let status, lifecycle;
    S.statusError = '';
    // A status response begun before a newer event must never replace it.
    // Re-read both views until they belong to one uninterrupted entry epoch.
    try {
      while (S.mounted && S.visible && entry === S.entrySeq) {
        const statusSequence = ++S.statusSeq;
        const lifecycleSequence = S.lifecycleSeq;
        [status, lifecycle] = await Promise.all([bindings().ExportStatus(), bindings().LifecycleStatus()]);
        if (entry !== S.entrySeq || !S.mounted || !S.visible) return;
        if (statusSequence === S.statusSeq && lifecycleSequence === S.lifecycleSeq) break;
      }
    } catch (_) {
      if (entry !== S.entrySeq || !S.mounted) return;
      S.statusError = 'Could not check active jobs. Reopen Copy to drive to retry.';
      renderActions();
      return;
    }
    if (entry !== S.entrySeq || !S.mounted || !S.visible) return;
    onLifecycle(lifecycle);
    if (typeof lifecycle?.build_running !== 'boolean' || typeof lifecycle?.export_running !== 'boolean' || typeof lifecycle?.stopping !== 'boolean') {
      S.statusError = 'Could not check active jobs. Reopen Copy to drive to retry.';
      renderActions();
      return;
    }
    if (status?.running) {
      applyExportStatus(status);
      await loadBundle();
      return;
    }
    S.running = false;
    stopTicker();
    if (p.returnTo && ['target', 'picker', 'build', 'readiness'].includes(p.returnTo)) S.returnTo = p.returnTo;
    if (p.sourceMode === 'choose') {
      setSource('', null, '');
      startPolling();
      await chooseBundle(true);
      return;
    }
    const bundle = p.bundlePath || p.bundle_path;
    if (bundle && (bundle !== S.bundlePath || p.sourceMode === 'result')) setSource(bundle,
      p.bundleSummary?.bundle_path === bundle ? p.bundleSummary : null, p.sourceTarget || '');
    else applyExportStatus(status);
    renderSource();
    await loadBundle();
    await recoverVerifyStatus();
    if (S.running) startTicker();
    else startPolling();
  },

  hide() {
    S.visible = false;
    S.entrySeq++;
    S.choosing = false;
    stopPolling();
    stopTicker();
    // A help popover left open would outlive the screen it explains, and its
    // document-level Escape and outside-click handlers with it.
    closeHelp(false);
  },

  destroy() {
    S.visible = false;
    stopPolling();
    stopTicker();
    closeHelp(false);
    if (S.rafHandle) window.cancelAnimationFrame(S.rafHandle);
    S.rafHandle = 0;
    if (S.root) S.root.replaceChildren();
    resetState();
    S.nodes = {};
    S.mounted = false;
    S.ctx = null;
    S.root = null;
  },
};

export default screen;

// Destination free space and incomplete markers come from PlanExport/InspectDestination.
