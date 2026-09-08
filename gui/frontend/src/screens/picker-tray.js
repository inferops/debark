/** Compact selected items, Add dialogs, and revision-safe backend Undo. */
import { bindDisclosure } from '../shell/disclosure.js';

const SELECTION_PAGE_MAX = 500;
/** How many tray entries are drawn before the operator asks for more. */
const PAGE_SIZE = 100;
/** Debounce before a paste is sent to ParsePackageList for preview. */
const PARSE_DEBOUNCE_MS = 300;
/** Longest rejected/unknown list rendered before it is summarised. */
const LIST_RENDER_CAP = 50;

let uidCounter = 0;
function uid(prefix) { return prefix + '-' + (++uidCounter); }

// Icons. Author-owned path data, drawn with createElementNS so that no icon
// ever travels through innerHTML.
// ---------------------------------------------------------------------------

const SVG_NS = 'http://www.w3.org/2000/svg';

const ICONS = {
  close: ['M18 6L6 18', 'M6 6l12 12'],
  plus: ['M12 5v14', 'M5 12h14'],
  warning: [
    'M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z',
    'M12 9v4',
    'M12 17h.01',
  ],
  info: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M12 11v5', 'M12 8h.01'],
  success: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M8.5 12.2l2.5 2.5 4.5-5.4'],
  danger: ['M8 2h8l6 6v8l-6 6H8l-6-6V8z', 'M15 9l-6 6', 'M9 9l6 6'],
  apt: ['M21 8v8l-9 5-9-5V8l9-5z', 'M3 8l9 5 9-5', 'M12 13v8'],
  url: [
    'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z',
    'M3 12h18',
    'M12 3c2.5 2.4 3.8 5.4 3.8 9S14.5 18.6 12 21c-2.5-2.4-3.8-5.4-3.8-9S9.5 5.4 12 3z',
  ],
  file: ['M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z', 'M14 3v5h5'],
  paste: [
    'M9 4h6v3H9z',
    'M15 5.5h2A2 2 0 0 1 19 7.5v11a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2v-11a2 2 0 0 1 2-2h2',
  ],
  link: [
    'M10.5 13.5a4.5 4.5 0 0 0 6.4 0l2.6-2.6a4.5 4.5 0 0 0-6.4-6.4l-1 1',
    'M13.5 10.5a4.5 4.5 0 0 0-6.4 0l-2.6 2.6a4.5 4.5 0 0 0 6.4 6.4l1-1',
  ],
  trash: [
    'M4 7h16',
    'M10 11v6',
    'M14 11v6',
    'M6.5 7l.9 12.1A2 2 0 0 0 9.4 21h5.2a2 2 0 0 0 2-1.9L17.5 7',
    'M9.5 7V5.5a2 2 0 0 1 2-2h1a2 2 0 0 1 2 2V7',
  ],
  tray: ['M3 13.5h5l2 3h4l2-3h5', 'M5 4.5h14l2 9v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5z'],
  copy: ['M9.5 9.5h10v10h-10z', 'M4.5 14.5v-10h10'],
  shield: ['M12 3l8 3v6c0 5-3.4 8.2-8 9-4.6-.8-8-4-8-9V6z', 'M9 12l2 2 4-4.5'],
};

/**
 * Build one inline SVG icon. `name` must be a key of ICONS; the caller owns it,
 * never the backend.
 */
function icon(name, className) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  if (className) svg.setAttribute('class', className);
  const paths = ICONS[name] || ICONS.info;
  for (let i = 0; i < paths.length; i += 1) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', paths[i]);
    svg.appendChild(p);
  }
  return svg;
}

// ---------------------------------------------------------------------------
// DOM helpers. Everything that touches backend data goes through textContent.
// ---------------------------------------------------------------------------

function h(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

function empty(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function button(label, className, onClick, iconName) {
  const b = h('button', className);
  b.type = 'button';
  if (iconName) b.appendChild(icon(iconName, 'df-btn__icon'));
  b.appendChild(document.createTextNode(label));
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

function hidden(text) {
  return h('span', 'df-visually-hidden', text);
}

/** Only http(s) may become an href. A doc_url is backend data. */
function safeHref(raw) {
  if (typeof raw !== 'string' || raw === '') return null;
  let u;
  try {
    u = new URL(raw);
  } catch (_) {
    return null;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  return u.href;
}

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];

/**
 * Format a byte count the backend supplied. This is presentation, not
 * arithmetic on the operator's behalf: no size is derived, added or predicted
 * here, only rendered.
 */
function formatBytes(n) {
  if (typeof n !== 'number' || !isFinite(n) || n <= 0) return '';
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = value >= 100 || unit === 0 ? 0 : 1;
  return value.toFixed(digits) + ' ' + UNITS[unit];
}

function plural(n, one, many) {
  return n === 1 ? one : many;
}

function formatCount(n) {
  const v = typeof n === 'number' && isFinite(n) ? n : 0;
  try {
    return v.toLocaleString();
  } catch (_) {
    return String(v);
  }
}

// ---------------------------------------------------------------------------
// Metadata for the frozen enums in internal/app/types.go.
//
// Provenance is a quiet badge on an ordinary row, never a separate section:
// the operator should not have to think about which mechanism delivers a
// thing. The glyph carries the same information as a shape, so the badge is
// never colour alone.
// ---------------------------------------------------------------------------

/**
 * How each frozen SelectionWarning.Kind is presented. The kinds are constants
 * in types.go; the *content* — message, hint, affected package — always comes
 * from the backend. Nothing here hardcodes a package name, and there is no
 * list of "packages that are snaps" anywhere in this file.
 */
const WARNING_META = {
  'snap-transitional': { variant: 'warning', glyph: 'warning', label: 'Snap shim', order: 0 },
  'arch-mismatch': { variant: 'danger', glyph: 'danger', label: 'Architecture mismatch', order: 1 },
  'large-download': { variant: 'warning', glyph: 'warning', label: 'Large download', order: 2 },
  'unknown-package': { variant: 'info', glyph: 'info', label: 'Not in the catalogue', order: 3 },
  'external-url': { variant: 'info', glyph: 'info', label: 'External download', order: 4 },
};

function warningMeta(kind) {
  return (
    WARNING_META[kind] || { variant: 'info', glyph: 'info', label: 'Notice', order: 9 }
  );
}

// ---------------------------------------------------------------------------
// Banners.
// ---------------------------------------------------------------------------

function makeBanner(variant, glyph, title, text, alert) {
  const b = h('div', 'df-banner df-banner--' + variant);
  b.setAttribute('role', alert ? 'alert' : 'status');
  b.appendChild(icon(glyph, 'df-banner__icon'));
  const body = h('div', 'df-banner__body');
  if (title) body.appendChild(h('p', 'df-banner__title', title));
  if (text) body.appendChild(h('p', 'df-banner__text', text));
  b.appendChild(body);
  // The actions cell holds the grid column even when empty (design §6.8).
  b.appendChild(h('div', 'df-banner__actions'));
  return b;
}

function bannerBody(banner) {
  return banner.querySelector('.df-banner__body');
}

/**
 * A dismiss/act control for a banner, placed in the banner body rather than in
 * `.df-banner__actions`. That slot is the banner grid's third column, and at
 * `--tray-width` a button in it squeezes the text to about a hundred pixels.
 * The empty slot stays in the markup to hold the column, as the design system
 * requires.
 */
function bannerAction(banner, label, className, onClick) {
  const cluster = h('div', 'df-cluster');
  cluster.appendChild(button(label, className, onClick));
  bannerBody(banner).appendChild(cluster);
}

/** Render a UIError the way the contract requires: message, hint, evidence. */
function errorBanner(err) {
  const b = makeBanner(
    'danger',
    'danger',
    err && err.message ? err.message : 'Something went wrong.',
    err && err.hint ? err.hint : '',
    true
  );
  const body = bannerBody(b);
  if (err && (err.code || err.details || err.exit_class || typeof err.retryable === 'boolean' || (err.command && err.command.length))) {
    const details = h('details', 'df-disclosure');
    details.appendChild(h('summary', 'df-summary', 'What failed'));
    bindDisclosure(details);
    const drawer = h('div', 'df-disclosure__body');
    const pre = h('pre', 'df-log');
    if (err.code) pre.appendChild(h('span', 'df-log__line df-mono', 'Code: ' + err.code));
    if (typeof err.retryable === 'boolean') pre.appendChild(h('span', 'df-log__line', 'Retryable: ' + (err.retryable ? 'yes' : 'no')));
    if (err.command && err.command.length) {
      pre.appendChild(h('span', 'df-log__line df-mono', err.command.join(' ')));
    }
    if (err.details) {
      pre.appendChild(h('span', 'df-log__line df-log__line--error', err.details));
    }
    if (err.exit_class) {
      pre.appendChild(h('span', 'df-log__line df-log__line--muted', 'class: ' + err.exit_class));
    }
    drawer.appendChild(pre);
    details.appendChild(drawer);
    body.appendChild(details);
  }
  return b;
}

function jsError(e) {
  return {
    code: 'frontend.exception',
    message: 'The selection action could not be completed.',
    hint: 'Try again. If this persists, restart Debark; Details has the error to report.',
    details: e && e.message ? String(e.message) : String(e),
  };
}

// ---------------------------------------------------------------------------
// Validation. Immediate feedback only — the backend stays the authority.
// ---------------------------------------------------------------------------

function validateURL(raw) {
  const v = String(raw == null ? '' : raw).trim();
  if (v === '') {
    return { ok: false, message: 'Enter the download address of the .deb.' };
  }
  let u;
  try {
    u = new URL(v);
  } catch (_) {
    return {
      ok: false,
      message:
        'That is not a URL. It should look like ' +
        'https://vendor.example/pool/agent_2.1.0_amd64.deb.',
    };
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') {
    return { ok: false, message: 'Only http:// and https:// downloads are accepted.' };
  }
  if (!u.hostname) {
    return { ok: false, message: 'That URL has no host name in it.' };
  }
  const warn =
    u.protocol === 'http:'
      ? 'This is plain http, so the download is not authenticated in transit. ' +
        'An expected SHA-256 is worth supplying here.'
      : '';
  return { ok: true, value: v, warn: warn };
}

/**
 * Normalise an operator-supplied digest. A `sha256:` prefix and internal
 * whitespace are accepted because that is how vendors publish them.
 */
function normaliseDigest(raw) {
  let v = String(raw == null ? '' : raw)
    .trim()
    .toLowerCase()
    .replace(/\s+/g, '');
  if (v.indexOf('sha256:') === 0) v = v.slice(7);
  if (v === '') return { ok: true, value: '' };
  if (!/^[0-9a-f]{64}$/.test(v)) {
    return {
      ok: false,
      message:
        'That is not a SHA-256. It should be 64 hexadecimal characters; that is ' +
        v.length +
        '.',
    };
  }
  return { ok: true, value: v };
}

/**
 * Whether a SelectionEntry carries an expected digest.
 *
 * SelectionEntry has no digest field today (see the report on the binding
 * surface), so this is written to notice one the moment the contract grows it,
 * and to tell the truth in the meantime rather than claiming an attestation
 * the bundle will not contain.
 */
function entryDigest(entry) {
  if (!entry) return '';
  return entry.sha256 || entry.expected_sha256 || entry.digest || '';
}

// ---------------------------------------------------------------------------
// Modal dialogs. <dialog>.showModal() supplies the focus trap and Escape; the
// fallback below exists so a missing modal is never a silent no-op.
// ---------------------------------------------------------------------------

const FOCUSABLE =
  'a[href],button:not([disabled]),input:not([disabled]),textarea:not([disabled]),' +
  'select:not([disabled]),[tabindex]:not([tabindex="-1"])';

/**
 * gate marks a dialog's primary action unavailable without taking it out of
 * the tab order.
 *
 * docs/accessibility.md §10 P1-4: never `disabled` on a gated action. A
 * `disabled` button is not focusable, so a keyboard or screen-reader operator
 * walking the dialog never meets it, is never told a primary action exists,
 * and is certainly never told why it will not work. Both of these dialogs did
 * exactly that: "Add packages" and "Add URL" started `disabled` — which is the
 * state the dialog OPENS in — so the control the dialog exists to reach was
 * invisible to the keyboard until the operator happened to type something
 * valid.
 *
 * `aria-disabled` keeps it focusable and the design system already styles
 * `.df-btn[aria-disabled="true"]` exactly like `:disabled`, so nothing about
 * the picture changes. The refusal moves into the click handler.
 *
 * `reason` is written into the button's own `aria-describedby` target, and
 * that is the load-bearing half on this platform rather than a nicety: the
 * accessibility pass measured that live regions are inert under Orca on
 * WebKit2GTK, while a
 * rewritten `aria-describedby` target IS re-read with its new text when focus
 * next arrives. So the description is a pull channel that works here, and an
 * `aria-live` announcement is one that does not.
 */
function gate(btn, blocked, describer, reason) {
  btn.setAttribute('aria-disabled', blocked ? 'true' : 'false');
  if (describer) describer.textContent = blocked ? reason || '' : '';
}

/** True when a gated control should refuse rather than act. */
function gated(btn) {
  return btn.getAttribute('aria-disabled') === 'true';
}

function focusables(root) {
  const out = [];
  const found = root.querySelectorAll(FOCUSABLE);
  for (let i = 0; i < found.length; i += 1) {
    const n = found[i];
    if (n.offsetParent !== null || n === document.activeElement) out.push(n);
  }
  return out;
}

function makeDialog(wide) {
  const dlg = document.createElement('dialog');
  dlg.className = 'df-dialog' + (wide ? ' df-dialog--wide' : '');
  const inner = h('div', 'df-dialog__inner');
  const header = h('div', 'df-dialog__header');
  const title = h('span', 'df-dialog__title');
  title.id = uid('pt-dlg-title');
  const close = h('button', 'df-dialog__close');
  close.type = 'button';
  close.setAttribute('aria-label', 'Close');
  close.appendChild(icon('close'));
  header.appendChild(title);
  header.appendChild(h('span', 'df-spacer'));
  header.appendChild(close);
  const body = h('div', 'df-dialog__body');
  const footer = h('div', 'df-dialog__footer');
  inner.appendChild(header);
  inner.appendChild(body);
  inner.appendChild(footer);
  dlg.appendChild(inner);
  dlg.setAttribute('aria-labelledby', title.id);

  close.addEventListener('click', function () {
    closeDialog(dlg);
  });

  return { el: dlg, title: title, body: body, footer: footer, close: close };
}

function trapKeydown(ev) {
  const dlg = ev.currentTarget;
  if (ev.key === 'Escape') {
    ev.preventDefault();
    closeDialog(dlg);
    return;
  }
  if (ev.key !== 'Tab') return;
  const items = focusables(dlg);
  if (items.length === 0) return;
  const first = items[0];
  const last = items[items.length - 1];
  if (ev.shiftKey && document.activeElement === first) {
    ev.preventDefault();
    last.focus();
  } else if (!ev.shiftKey && document.activeElement === last) {
    ev.preventDefault();
    first.focus();
  }
}

function openDialog(dlg, trigger, initialFocus, selectText) {
  dlg.__ptTrigger =
    trigger || (document.activeElement instanceof HTMLElement ? document.activeElement : null);
  if (typeof dlg.showModal === 'function') {
    if (!dlg.open) dlg.showModal();
  } else {
    // Fallback: no native top layer on this engine. `.df-dialog--fallback` is
    // the design system's modifier for exactly this — it fixes and centres the
    // dialog and paints the backdrop itself, because `::backdrop` only renders
    // for `showModal()`, which is the case this branch is not for. The focus
    // trap and Escape are supplied below, since those come from the top layer
    // too. Unreachable on both target engines (WebKitGTK 2.36+ and WebView2
    // both have `showModal`); kept so a missing modal is never a silent no-op.
    dlg.__ptFallback = true;
    dlg.classList.add('df-dialog--fallback');
    dlg.setAttribute('open', '');
    dlg.setAttribute('role', 'dialog');
    dlg.setAttribute('aria-modal', 'true');
    dlg.addEventListener('keydown', trapKeydown);
  }
  const target = initialFocus || focusables(dlg)[0];
  if (target) {
    try {
      target.focus();
      if (selectText && target.select) target.select();
    } catch (_) {
      /* focusing a detached node is not worth a crash */
    }
  }
}

function closeDialog(dlg) {
  if (typeof dlg.close === 'function' && dlg.open && !dlg.__ptFallback) {
    dlg.close();
    return;
  }
  if (dlg.__ptFallback) {
    dlg.__ptFallback = false;
    dlg.classList.remove('df-dialog--fallback');
    dlg.removeAttribute('open');
    dlg.removeEventListener('keydown', trapKeydown);
    dlg.dispatchEvent(new Event('close'));
  }
}

/** Restore focus to whatever opened the dialog. Native restore is unreliable. */
function wireFocusRestore(dlg) {
  dlg.addEventListener('close', function () {
    const t = dlg.__ptTrigger;
    dlg.__ptTrigger = null;
    if (t && typeof t.focus === 'function' && t.isConnected) {
      try {
        t.focus();
      } catch (_) {
        /* the trigger may have been re-rendered away */
      }
    }
  });
}

// ---------------------------------------------------------------------------
// The tray.
// ---------------------------------------------------------------------------

const EMPTY_SUMMARY = {
  total: 0,
  package_count: 0,
  url_count: 0,
  file_count: 0,
  unknown_count: 0,
  installed_size_bytes: 0,
  download_size_bytes: 0,
  warnings: [],
  rejected: [],
  added: 0,
  removed: 0,
};

/**
 * @param {object} options
 * @param {function(string[]):*} [options.onRemove]  remove these keys
 * @param {function():*}         [options.onClear]   empty the tray
 * @param {function(string):*}   [options.onPaste]   commit a pasted list
 * @param {function(string,string):*} [options.onAddURL] add one URL (+digest)
 * @param {function():*}         [options.onAddFile] open the native picker
 * @param {object} [options.bindings] optional injection; defaults to window.go.app.App
 * @param {function(string,string):void} [options.toast] optional ctx.toast
 * @returns {{el: HTMLElement, render: function(object):void, setBusy: function(boolean):void,
 *            destroy: function():void, focus: function():void}}
 */
export function createTray(options) {
  const opts = options || {};

  const state = {
    summary: EMPTY_SUMMARY,
    entries: [],
    shown: 0,
    pageSeq: 0,
    pageError: null,
    hostBusy: false,
    opBusy: false,
    readBusy: false,
    mutationSeq: 0,
    undoToken: null,
    addTrigger: null,
    activeIndex: 0,
    destroyed: false,
    // Await entry rendering before restoring focus after a removal.
    pending: null,
  };

  function api() {
    if (opts.bindings) return opts.bindings;
    const g = typeof window !== 'undefined' ? window.go : null;
    return (g && g.app && g.app.App) || null;
  }

  function toast(kind, message) {
    if (typeof opts.toast === 'function') opts.toast(kind, message);
  }

  // -- structure -----------------------------------------------------------

  const el = h('section', 'df-panel df-panel--flush pt');
  el.setAttribute('aria-label', 'Selection');

  // Header.
  const header = h('div', 'df-toolbar');
  const titleEl = h('span', 'pt__title', 'Selection');
  const busyEl = h('span', 'pt__busy');
  busyEl.hidden = true;
  busyEl.appendChild(h('span', 'df-spinner'));
  busyEl.appendChild(h('span', null, 'Updating…'));
  const clearBtn = button('Clear all', 'df-btn df-btn--ghost df-btn--sm', function (ev) {
    openClearDialog(ev.currentTarget);
  }, 'trash');
  header.appendChild(titleEl);
  header.appendChild(h('span', 'df-spacer'));
  header.appendChild(busyEl);
  header.appendChild(clearBtn);
  el.appendChild(header);

  // The picker toolbar owns Add and the sole selection count. Estimates
  // belong inside this collapsed disclosure, not in a permanent lower strip.
  const statsWrap = h('details', 'df-disclosure');
  statsWrap.appendChild(h('summary', 'df-summary', 'Size estimates'));
  bindDisclosure(statsWrap);
  const statsBody = h('div', 'df-disclosure__body');
  const statsEl = h('div', 'pt__stats');
  const statsNote = h('p', 'pt__note');
  statsBody.appendChild(statsEl);
  statsBody.appendChild(statsNote);
  statsWrap.appendChild(statsBody);

  // Notices: errors, rejected inputs, selection-wide warnings.
  const noticeWrap = h('div', 'pt__pad--tight');
  const noticeEl = h('div', 'df-stack df-stack--tight');
  noticeWrap.appendChild(noticeEl);

  // The entries themselves.
  const scroll = h('div', 'pt__scroll');
  const cloud = h('div', 'df-selection-list');
  cloud.setAttribute('role', 'list');
  cloud.setAttribute('aria-label', 'Selected items');
  const cloudHint = hidden(
    'Use the arrow keys to move between selected items. ' +
      'Press Delete or Backspace to remove the focused item.'
  );
  cloudHint.id = uid('pt-cloud-hint');
  cloud.setAttribute('aria-describedby', cloudHint.id);
  scroll.appendChild(noticeWrap);
  scroll.appendChild(cloudHint);
  scroll.appendChild(cloud);
  scroll.appendChild(statsWrap);
  el.appendChild(scroll);

  // Paging footer.
  const foot = h('div', 'pt__foot');
  const footStatus = h('span', 'df-text-sm df-text-secondary');
  const moreBtn = button('Show more', 'df-btn df-btn--secondary df-btn--sm', function () {
    state.shown = Math.min((state.shown || 0) + PAGE_SIZE, state.summary.total || 0);
    refreshEntries();
  });
  const allBtn = button('Show all', 'df-btn df-btn--ghost df-btn--sm', function () {
    state.shown = state.summary.total || 0;
    refreshEntries();
  });
  foot.appendChild(footStatus);
  foot.appendChild(h('span', 'df-spacer'));
  foot.appendChild(moreBtn);
  foot.appendChild(allBtn);
  foot.hidden = true;
  scroll.insertBefore(foot, statsWrap);

  // Screen-reader announcements for things that happen without a banner.
  const announcer = h('p', 'df-visually-hidden');
  announcer.setAttribute('role', 'status');
  announcer.setAttribute('aria-live', 'polite');
  document.body.appendChild(announcer);

  function announce(text) {
    announcer.textContent = text;
  }

  // -- busy ----------------------------------------------------------------

  function applyBusy() {
    const busy = state.hostBusy || state.opBusy;
    el.setAttribute('aria-busy', busy || state.readBusy ? 'true' : 'false');
    busyEl.hidden = !state.opBusy;
    clearBtn.disabled = busy || (state.summary.total || 0) === 0;
    for (const node of removeButtons()) node.disabled = busy;
    for (const node of menu.querySelectorAll('button')) node.disabled = busy;
    moreBtn.disabled = state.readBusy;
    allBtn.disabled = state.readBusy;
    pasteArea.disabled = busy;
    urlInput.disabled = busy;
    digestInput.disabled = busy;
    pasteCommit.disabled = busy;
    urlCommit.disabled = busy;
    busyNotice.hidden = !state.hostBusy;
    busyNotice.textContent = state.hostBusy ? 'Selection changes are unavailable while a build or copy is active.' : '';
    for (const note of [pasteLock, urlLock]) {
      note.hidden = !state.hostBusy;
      note.textContent = busyNotice.textContent;
    }
  }

  function setOpBusy(v) { state.opBusy = !!v; applyBusy(); }

  function operationError(error) {
    state.operationError = error;
    for (const pair of [[pasteDialog, pasteError], [urlDialog, urlActionError]]) {
      if (pair[0].el.open) {
        empty(pair[1]); pair[1].appendChild(errorBanner(error)); pair[1].hidden = false;
        return;
      }
    }
    renderNotices();
  }

  function showFileResult(summary, trigger) {
    if (!summary || (!summary.error && !(summary.rejected || []).length)) return;
    empty(fileResult.body);
    fileResult.title.textContent = summary.error ? 'Files could not be added' : 'Some files were not added';
    if (summary.error) fileResult.body.appendChild(errorBanner(summary.error));
    if ((summary.rejected || []).length) fileResult.body.appendChild(rejectedTable(summary.rejected));
    openDialog(fileResult.el, trigger, fileResult.close);
  }

  function acceptMutation(summary) {
    if (summary && summary.error && typeof summary.total !== 'number') {
      if (!state.destroyed) operationError(summary.error);
      return summary;
    }
    if (!summary || typeof summary.total !== 'number') {
      throw new Error('The selection update returned no selection status. Recheck the selection and try again.');
    }
    if (state.destroyed) return summary;
    const stale = typeof summary.revision === 'number' && summary.revision < (state.summary.revision || 0);
    if (stale) return summary;
    state.operationError = summary.error || null;
    render(summary);
    if (summary.error) operationError(summary.error);
    else if (typeof opts.onChange === 'function') opts.onChange(summary);
    return summary;
  }

  // A supplied callback is invoked once. Its undefined return never triggers
  // a second mutation; recover authoritative status through Selection instead.
  async function mutate(hook, hookArgs, bindingName, bindingArgs) {
    if (state.destroyed || state.hostBusy || state.opBusy) return null;
    const request = ++state.mutationSeq;
    setOpBusy(true);
    try {
      const fn = opts[hook];
      const b = api();
      let summary;
      if (typeof fn === 'function') summary = await fn.apply(null, hookArgs);
      else {
        if (!b || typeof b[bindingName] !== 'function') throw new Error('This selection action is unavailable. Restart Debark and try again.');
        summary = await b[bindingName].apply(b, bindingArgs);
      }
      if (summary === undefined && b && typeof b.Selection === 'function') summary = await b.Selection();
      if (state.destroyed || request !== state.mutationSeq) return summary || null;
      return acceptMutation(summary);
    } catch (error) {
      const projected = jsError(error);
      if (!state.destroyed && request === state.mutationSeq) operationError(projected);
      return Object.assign({}, state.summary, { error: projected });
    } finally {
      if (!state.destroyed && request === state.mutationSeq) setOpBusy(false);
    }
  }

  async function refreshEntries() {
    const total = state.summary.total || 0;
    const revision = state.summary.revision;
    const seq = (state.pageSeq += 1);

    if (total === 0) {
      state.entries = [];
      state.shown = 0;
      state.readBusy = false;
      state.pageError = null;
      applyBusy();
      renderCloud();
      return;
    }

    const want = Math.min(Math.max(state.shown || 0, PAGE_SIZE), total);
    const b = api();
    if (!b || typeof b.SelectionPage !== 'function') {
      state.entries = [];
      state.readBusy = false;
      applyBusy();
      state.pageError = {
        code: 'frontend.no_bindings',
        message: 'The selection could not be read.',
        hint: 'Wait for the window to finish starting, then try again.',
      };
      renderCloud();
      renderNotices();
      return;
    }

    state.readBusy = true;
    applyBusy();
    if (state.entries.length === 0) renderLoadingEntries();
    const collected = [];
    let err = null;
    try {
      for (let off = 0; off < want; off += SELECTION_PAGE_MAX) {
        const limit = Math.min(SELECTION_PAGE_MAX, want - off);
        /* eslint-disable no-await-in-loop */
        const page = await b.SelectionPage(off, limit);
        /* eslint-enable no-await-in-loop */
        if (seq !== state.pageSeq || state.destroyed) return;
        if (page && page.error) {
          err = page.error;
          break;
        }
        if (!page || !Array.isArray(page.entries)) throw new Error('The selection could not be read. Try again.');
        const rows = page.entries;
        for (let i = 0; i < rows.length; i += 1) collected.push(rows[i]);
        if (rows.length < limit) break;
      }
      // Pages do not carry a revision. A status read after all pages ensures
      // that an intervening mutation cannot produce a mixed list.
      if (!err && typeof revision === 'number') {
        if (typeof b.Selection !== 'function') throw new Error('Selection status is unavailable. Restart Debark and try again.');
        const current = await b.Selection();
        if (seq !== state.pageSeq || state.destroyed) return;
        if (current && current.error) err = current.error;
        else if (!current || typeof current.revision !== 'number') throw new Error('Selection status could not be read. Try again.');
        else if (current.revision !== revision) {
          if (current.revision > revision) return render(current);
          throw new Error('Selection status changed while loading. Try again.');
        }
      }
    } catch (e) {
      err = jsError(e);
    }
    if (seq !== state.pageSeq || state.destroyed) return;
    state.entries = collected;
    state.shown = collected.length;
    state.pageError = err;
    state.readBusy = false;
    applyBusy();
    renderCloud();
    renderNotices();
  }

  // -- selected rows ----------------------------------------------------------

  function chipFor(entry, index) {
    const row = h('div', 'df-selection-item');
    row.setAttribute('role', 'listitem');
    row.dataset.selectionKey = entry.key;
    const head = h('div', 'df-selection-item__header');
    const label = entry.name || displayKey(entry.path || entry.url) || '(unnamed)';
    const labelEl = h('span', 'df-selection-item__label df-mono', label);
    head.appendChild(labelEl);
    const remove = button('', 'df-btn df-btn--ghost df-btn--icon df-btn--sm df-selection-remove', function () {
      state.activeIndex = index;
      removeKeys([entry.key], index);
    }, 'close');
    remove.setAttribute('aria-label', 'Remove ' + label);
    remove.setAttribute('tabindex', index === state.activeIndex ? '0' : '-1');
    remove.disabled = state.hostBusy || state.opBusy || !entry.key;
    remove.dataset.index = String(index);
    remove.addEventListener('focus', function () { state.activeIndex = index; updateRovingTabindex(); });
    head.appendChild(remove);
    row.appendChild(head);

    const warnings = entry.warnings || [];
    const material = warnings.filter(w => w.kind !== 'external-url');
    for (const warning of material) {
      row.appendChild(h('p', 'df-selection-item__warning', warning.message || warningMeta(warning.kind).label));
    }
    if (entry.source === 'apt' && entry.known === false && !material.length) {
      row.appendChild(h('p', 'df-selection-item__warning', 'Not in this target’s catalogue; retained for the build.'));
    }
    if (entry.source === 'url') row.appendChild(h('p', 'pt__note', entryDigest(entry) ? 'Vendor URL · expected SHA-256 supplied' : 'Vendor URL · unverified download'));
    else if (entry.source === 'file') row.appendChild(h('p', 'pt__note', 'Local .deb'));

    const detail = h('details', 'df-disclosure df-selection-item__details');
    detail.appendChild(h('summary', 'df-summary', 'Source details'));
    bindDisclosure(detail);
    const body = h('div', 'df-disclosure__body');
    const identity = entry.path || entry.url || entry.name || '';
    if (identity) body.appendChild(h('p', 'pt__prose df-mono', identity));
    if (entry.version) body.appendChild(h('p', 'pt__note', 'Version: ' + entry.version));
    if (entry.summary) body.appendChild(h('p', 'pt__note', entry.summary));
    if (entryDigest(entry)) body.appendChild(h('p', 'pt__note df-mono', 'Expected SHA-256: ' + entryDigest(entry)));
    for (const warning of warnings) {
      if (warning.kind === 'external-url') body.appendChild(h('p', 'pt__note', warning.message || ''));
      if (warning.hint) body.appendChild(h('p', 'pt__note', warning.hint));
      const href = safeHref(warning.doc_url);
      if (href) { const link = h('a', 'df-link', 'What this means'); link.href = href; link.rel = 'noopener noreferrer'; body.appendChild(link); }
    }
    detail.appendChild(body);
    if (entry.source !== 'apt' || warnings.some(w => w.hint || w.doc_url)) row.appendChild(detail);
    return row;
  }

  function removeButtons() {
    return cloud.querySelectorAll('.df-selection-remove');
  }

  function updateRovingTabindex() {
    const buttons = removeButtons();
    if (buttons.length === 0) return;
    if (state.activeIndex >= buttons.length) state.activeIndex = buttons.length - 1;
    if (state.activeIndex < 0) state.activeIndex = 0;
    for (let i = 0; i < buttons.length; i += 1) {
      buttons[i].setAttribute('tabindex', i === state.activeIndex ? '0' : '-1');
    }
  }

  function focusChip(index) {
    const buttons = removeButtons();
    if (buttons.length === 0) {
      focusFallback();
      return;
    }
    const i = Math.max(0, Math.min(index, buttons.length - 1));
    state.activeIndex = i;
    updateRovingTabindex();
    buttons[i].focus();
  }

  cloud.addEventListener('keydown', function (ev) {
    if (!ev.target.classList.contains('df-selection-remove')) return;
    const buttons = removeButtons();
    if (buttons.length === 0) return;
    const current = state.activeIndex;
    let next = null;
    switch (ev.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        next = current + 1;
        break;
      case 'ArrowLeft':
      case 'ArrowUp':
        next = current - 1;
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = buttons.length - 1;
        break;
      case 'Delete':
      case 'Backspace': {
        ev.preventDefault();
        const entry = state.entries[current];
        if (entry) removeKeys([entry.key], current);
        return;
      }
      default:
        return;
    }
    if (next === null) return;
    ev.preventDefault();
    focusChip(Math.max(0, Math.min(next, buttons.length - 1)));
  });

  function focusFallback() {
    const trigger = state.addTrigger;
    if (trigger && trigger.isConnected && trigger.getClientRects().length) trigger.focus();
    else if (el.getClientRects().length) { el.tabIndex = -1; el.focus(); }
  }

  async function removeKeys(keys, focusAfter) {
    if (!keys.length || keys.some(key => typeof key !== 'string' || !key)) return;
    const summary = await mutate('onRemove', [keys.slice()], 'RemovePackages', [keys.slice()]);
    if (!summary || summary.error || state.destroyed) return;
    if (summary.removed > 0) announce('Selection updated. ' + (summary.undo_token ? (summary.undo_label || 'Undo') + ' is available.' : ''));
    if (typeof focusAfter === 'number') {
      await state.pending;
      if (!state.destroyed) focusChip(focusAfter);
    }
  }

  function renderHeader() { applyBusy(); }

  function stat(text, statusVariant, glyph) {
    if (!statusVariant) return h('span', null, text);
    const s = h('span', 'df-status df-status--' + statusVariant);
    s.appendChild(icon(glyph || statusVariant, 'pt__glyph'));
    s.appendChild(h('span', null, text));
    return s;
  }

  function renderStats() {
    const s = state.summary;
    empty(statsEl);
    statsNote.textContent = '';

    const total = s.total || 0;
    if (total === 0) {
      statsWrap.hidden = true;
      return;
    }
    statsWrap.hidden = false;

    const vendor = (s.url_count || 0) + (s.file_count || 0);
    const installed = formatBytes(s.installed_size_bytes);
    const download = formatBytes(s.download_size_bytes);
    if (installed) statsEl.appendChild(stat('~' + installed + ' installed'));
    if (download) statsEl.appendChild(stat('~' + download + ' to download'));

    const notes = [];
    if (installed || download) {
      notes.push(
        'Selected-package estimates exclude dependencies.'
      );
    } else {
      notes.push('No size estimate is available for this selection yet.');
    }
    if (vendor > 0) {
      notes.push('Vendor .deb sizes are not known until the build fetches or reads them.');
    }
    statsNote.textContent = notes.join(' ');
  }

  function displayKey(value) {
    if (typeof value !== 'string') return '';
    if (!/[/\\]/.test(value)) return value;
    const stripped = value.split('?')[0].replace(/[/\\]+$/, '');
    const parts = stripped.split(/[/\\]/);
    return parts[parts.length - 1] || value;
  }

  function renderNotices() {
    empty(noticeEl);

    // A failed SelectionPage read is rendered as the entries region's own
    // error state, with a retry, rather than a second banner up here.
    if (state.summary.error) noticeEl.appendChild(errorBanner(state.summary.error));

    // Rejected inputs from the last mutating call. Never silently dropped.
    const rejected = state.summary.rejected || [];
    if (rejected.length) {
      const b = makeBanner(
        'warning',
        'warning',
        rejected.length +
          ' ' +
          plural(rejected.length, 'entry was', 'entries were') +
          ' not added',
        ''
      );
      bannerBody(b).appendChild(rejectedTable(rejected));
      bannerAction(b, 'Dismiss', 'df-btn df-btn--ghost df-btn--sm', function () {
        state.summary = Object.assign({}, state.summary, { rejected: [] });
        renderNotices();
      });
      noticeEl.appendChild(b);
    }

    if (state.operationError && state.operationError !== state.summary.error) noticeEl.appendChild(errorBanner(state.operationError));
    // Entry-specific warnings stay beside that row. Global warnings have no
    // key and cannot be used as removal identities.
    for (const warning of state.summary.warnings || []) {
      if (warning.package) continue;
      const meta = warningMeta(warning.kind);
      noticeEl.appendChild(makeBanner(meta.variant, meta.glyph, warning.message || meta.label, warning.hint || '', meta.variant === 'danger'));
    }

    noticeWrap.hidden = noticeEl.childNodes.length === 0;
  }

  function rejectedTable(rejected) {
    const wrap = h('div', 'pt__scrollbox');
    // Auto table layout, not `--fixed`: with `table-layout: fixed` the
    // design system's `.df-table__detail { width: 100% }` takes the whole
    // width and the label columns collapse to zero, overlapping their text.
    const table = h('table', 'df-table');
    const thead = document.createElement('thead');
    const hr = document.createElement('tr');
    hr.appendChild(h('th', 'df-table__label', 'Line'));
    hr.appendChild(h('th', 'df-table__detail', 'Why it was not added'));
    thead.appendChild(hr);
    table.appendChild(thead);
    const tbody = document.createElement('tbody');
    const shown = Math.min(rejected.length, LIST_RENDER_CAP);
    for (let i = 0; i < shown; i += 1) {
      const r = rejected[i];
      const tr = document.createElement('tr');
      const td1 = h('td', 'df-table__label pt__wrap');
      td1.appendChild(h('span', 'df-mono', r.value || ''));
      const td2 = h('td', 'df-table__detail pt__wrap');
      td2.appendChild(document.createTextNode(r.reason || ''));
      if (r.hint) {
        td2.appendChild(document.createElement('br'));
        td2.appendChild(h('span', 'df-text-secondary', r.hint));
      }
      tr.appendChild(td1);
      tr.appendChild(td2);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    wrap.appendChild(table);
    if (rejected.length > shown) {
      wrap.appendChild(
        h('p', 'pt__note', 'and ' + (rejected.length - shown) + ' more not shown.')
      );
    }
    return wrap;
  }

  function renderEmptyState() {
    const emptyState = h('div', 'df-state');
    emptyState.appendChild(h('span', 'df-text-secondary', 'Nothing selected.'));
    return emptyState;
  }

  /** Drop whichever of the three whole-region states is currently shown. */
  function clearRegionState() {
    const stale = scroll.querySelector('.df-state');
    if (stale) scroll.removeChild(stale);
  }

  function renderLoadingEntries() {
    empty(cloud);
    cloud.hidden = true;
    foot.hidden = true;
    clearRegionState();
    const loading = h('div', 'df-state df-state--loading');
    loading.appendChild(h('span', 'df-spinner df-spinner--lg'));
    loading.appendChild(h('span', 'df-state__title', 'Reading the selection…'));
    scroll.appendChild(loading);
  }

  /** The entries region could not be read. Say what and what to do next. */
  function renderPageError(err) {
    const s = h('div', 'df-state df-state--error');
    s.appendChild(icon('danger', 'df-state__icon'));
    s.appendChild(
      h('span', 'df-state__title', err.message || 'The selection could not be read.')
    );
    if (err.hint) s.appendChild(h('p', 'df-state__text', err.hint));
    const actions = h('div', 'df-state__actions pt__actions');
    actions.appendChild(
      button('Try again', 'df-btn df-btn--primary df-btn--sm', function () {
        state.pageError = null;
        renderNotices();
        state.pending = refreshEntries();
      })
    );
    s.appendChild(actions);
    if (err.code || err.details || err.exit_class || typeof err.retryable === 'boolean' || (err.command && err.command.length)) {
      const detail = h('div', 'df-state__detail');
      const details = h('details', 'df-disclosure');
      details.appendChild(h('summary', 'df-summary', 'What failed'));
      bindDisclosure(details);
      const drawer = h('div', 'df-disclosure__body');
      const pre = h('pre', 'df-log');
      if (err.code) pre.appendChild(h('span', 'df-log__line df-mono', 'Code: ' + err.code));
      if (err.exit_class) pre.appendChild(h('span', 'df-log__line', 'Class: ' + err.exit_class));
      if (typeof err.retryable === 'boolean') pre.appendChild(h('span', 'df-log__line', 'Retryable: ' + (err.retryable ? 'yes' : 'no')));
      if (err.command && err.command.length) {
        pre.appendChild(h('span', 'df-log__line df-mono', err.command.join(' ')));
      }
      if (err.details) pre.appendChild(h('span', 'df-log__line df-log__line--error', err.details));
      drawer.appendChild(pre);
      details.appendChild(drawer);
      detail.appendChild(details);
      s.appendChild(detail);
    }
    return s;
  }

  function renderCloud() {
    const previousRows = Array.from(cloud.children);
    const opened = new Set(previousRows.filter(row => row.querySelector('details')?.open).map(row => row.dataset.selectionKey));
    const focusedRow = previousRows.find(row => row.contains(document.activeElement));
    const focusSelector = document.activeElement?.classList.contains('df-selection-remove') ? '.df-selection-remove' : document.activeElement?.tagName === 'SUMMARY' ? 'summary' : '';
    empty(cloud);
    const total = state.summary.total || 0;
    clearRegionState();

    if (state.pageError) {
      cloud.hidden = true;
      foot.hidden = true;
      scroll.appendChild(renderPageError(state.pageError));
      return;
    }

    if (total === 0) {
      cloud.hidden = true;
      foot.hidden = true;
      scroll.appendChild(renderEmptyState());
      return;
    }

    cloud.hidden = false;

    for (let i = 0; i < state.entries.length; i += 1) {
      const row = chipFor(state.entries[i], i);
      if (opened.has(row.dataset.selectionKey) && row.querySelector('details')) row.querySelector('details').open = true;
      cloud.appendChild(row);
      if (focusedRow && focusedRow.dataset.selectionKey === row.dataset.selectionKey && focusSelector && row.getClientRects().length) row.querySelector(focusSelector)?.focus();
    }
    updateRovingTabindex();

    const shown = state.entries.length;
    if (shown < total) {
      foot.hidden = false;
      footStatus.textContent = formatCount(shown) + ' items shown';
      const remaining = total - shown;
      moreBtn.textContent = 'Show ' + formatCount(Math.min(PAGE_SIZE, remaining)) + ' more';
      // "Show all" disappears above SelectionPageMax*2: the add cap is 5,000,
      // and drawing five thousand chips in one frame is a stall, not a view.
      // Paging by hundreds stays available for as long as it takes.
      allBtn.hidden = remaining <= PAGE_SIZE || total > SELECTION_PAGE_MAX * 2;
    } else {
      foot.hidden = true;
    }
  }

  /**
   * The frozen render entry point. `summary` is a SelectionSummary; the entries
   * are then re-read from the backend, so this component and the list can never
   * hold different ideas of what is selected.
   */
  function render(summary) {
    if (state.destroyed || !summary) return state.pending;
    if (typeof summary.revision === 'number' && summary.revision < (state.summary.revision || 0)) return state.pending;
    if (summary === state.renderedSummary) return state.pending;
    state.renderedSummary = summary;
    state.summary = Object.assign({}, EMPTY_SUMMARY, summary);
    const token = state.summary.undo_token || '';
    if (token !== state.undoToken) {
      state.undoToken = token;
      if (typeof opts.onUndoState === 'function') opts.onUndoState({ token, label: state.summary.undo_label || 'Undo' });
    }
    renderHeader();
    renderStats();
    renderNotices();
    state.pending = refreshEntries();
    return state.pending;
  }

  // Kept as an API name for hosts. Clear is reversible, so no confirmation.
  async function openClearDialog(trigger) {
    if (!state.summary.total) return null;
    const summary = await mutate('onClear', [], 'ClearSelection', []);
    if (!summary || summary.error || state.destroyed) return summary;
    if (summary.removed > 0) announce('Selection cleared. ' + (summary.undo_token ? 'Undo clear is available.' : ''));
    if (trigger && trigger.isConnected && trigger.getClientRects().length) trigger.focus();
    else focusFallback();
    return summary;
  }

  async function undo(token) {
    const summary = await mutate('', [], 'UndoSelection', [token]);
    if (summary && !summary.error && !state.destroyed) announce('Selection restored.');
    return summary;
  }

  // -- paste a list ---------------------------------------------------------

  const pasteDialog = makeDialog(true);
  wireFocusRestore(pasteDialog.el);
  pasteDialog.title.textContent = 'Paste a package list';
  const pasteLock = h('p', 'df-text-secondary'); pasteLock.hidden = true; pasteDialog.body.appendChild(pasteLock);
  const pasteError = h('div'); pasteError.hidden = true; pasteDialog.body.appendChild(pasteError);

  const pasteStack = h('div', 'df-stack');
  const pasteField = h('div', 'df-field');
  const pasteLabel = h('label', 'df-field__label', 'One package name per line');
  const pasteArea = h('textarea', 'df-input pt__textarea');
  pasteArea.id = uid('pt-paste');
  pasteLabel.setAttribute('for', pasteArea.id);
  pasteArea.setAttribute('spellcheck', 'false');
  pasteArea.setAttribute('autocapitalize', 'off');
  pasteArea.setAttribute(
    'placeholder',
    'nginx-full\npostgresql-16\n# comments and blank lines are ignored\nbuild-essential'
  );
  const pasteHint = h(
    'span',
    'df-field__hint',
    'Use package names or name=version. Comments (#), blank lines and duplicates are ignored.'
  );
  pasteField.appendChild(pasteLabel);
  pasteField.appendChild(pasteArea);
  pasteField.appendChild(pasteHint);
  pasteStack.appendChild(pasteField);

  const previewWrap = h('div', 'df-stack df-stack--tight');
  previewWrap.setAttribute('aria-live', 'polite');
  pasteStack.appendChild(previewWrap);
  pasteDialog.body.appendChild(pasteStack);

  const pasteCancel = button('Cancel', 'df-btn df-btn--ghost', function () {
    closeDialog(pasteDialog.el);
  });
  const pasteCommit = button('Add packages', 'df-btn df-btn--primary', async function () {
    if (gated(pasteCommit)) {
      // Refuse, and send them to the thing that would unblock it.
      pasteArea.focus();
      return;
    }
    const text = pasteArea.value;
    const s = await mutate('onPaste', [text], 'AddPackageList', [text]);
    if (s && !s.error) {
      if ((s.rejected || []).length) {
        empty(pasteError);
        pasteError.appendChild(makeBanner('warning', 'warning', 'Some lines were not added.', 'Accepted package names are now selected.'));
        pasteError.appendChild(rejectedTable(s.rejected));
        pasteError.hidden = false;
        schedulePreview();
      } else closeDialog(pasteDialog.el);
      announce(
        'Added ' + formatCount(s.added || 0) + '. ' + formatCount(s.total || 0) + ' selected.'
      );
    }
  });
  // The reason this action is unavailable, read out when focus reaches the
  // button. Visually hidden: the dialog already shows the same information as
  // a preview panel, and repeating it in the footer would be noise on screen
  // and silence off it.
  const pasteWhy = document.createElement('span');
  pasteWhy.className = 'df-visually-hidden';
  pasteWhy.id = uid('pt-why');
  pasteCommit.setAttribute('aria-describedby', pasteWhy.id);
  gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
  pasteDialog.footer.appendChild(pasteWhy);
  pasteDialog.footer.appendChild(pasteCancel);
  pasteDialog.footer.appendChild(pasteCommit);
  document.body.appendChild(pasteDialog.el);

  let parseSeq = 0;
  let parseTimer = null;

  function schedulePreview() {
    parseSeq += 1;
    if (parseTimer) window.clearTimeout(parseTimer);
    parseTimer = window.setTimeout(runPreview, PARSE_DEBOUNCE_MS);
    gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
    if (pasteArea.value.trim() === '') {
      previewWrap.removeAttribute('aria-busy');
      empty(previewWrap);
      return;
    }
    // Keep the previous preview on screen while the next one is computed —
    // wiping it on every keystroke makes the dialog jump under the operator.
    previewWrap.setAttribute('aria-busy', 'true');
    if (previewWrap.childNodes.length === 0) renderPreviewBusy();
  }

  function renderPreviewBusy() {
    empty(previewWrap);
    const s = h('div', 'df-cluster');
    s.appendChild(h('span', 'df-spinner'));
    s.appendChild(
      h('span', 'df-text-sm df-text-secondary', 'Checking this list against the catalogue…')
    );
    previewWrap.appendChild(s);
  }

  async function runPreview() {
    parseTimer = null;
    const text = pasteArea.value;
    if (text.trim() === '') {
      empty(previewWrap);
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      pasteCommit.textContent = 'Add packages';
      return;
    }
    const seq = (parseSeq += 1);
    const b = api();
    if (!b || typeof b.ParsePackageList !== 'function') {
      empty(previewWrap);
      previewWrap.appendChild(
        errorBanner({
          code: 'frontend.no_bindings',
          message: 'The list could not be checked.',
          hint: 'Wait for the window to finish starting, then try again.',
        })
      );
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      return;
    }
    let parsed;
    try {
      parsed = await b.ParsePackageList(text);
    } catch (e) {
      if (seq !== parseSeq || state.destroyed || text !== pasteArea.value || !pasteDialog.el.open) return;
      empty(previewWrap);
      previewWrap.appendChild(errorBanner(jsError(e)));
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      return;
    }
    if (seq !== parseSeq || state.destroyed || text !== pasteArea.value || !pasteDialog.el.open) return;
    renderPreview(parsed || {});
  }

  function previewGroup(title, count, glyph, variant) {
    const group = h('div', 'pt__group');
    const head = h('div', 'pt__grouphead');
    if (glyph) {
      const st = h('span', 'df-status' + (variant ? ' df-status--' + variant : ''));
      st.appendChild(icon(glyph, 'pt__glyph'));
      st.appendChild(h('span', null, title + ' (' + formatCount(count) + ')'));
      head.appendChild(st);
    } else {
      head.appendChild(h('span', null, title + ' (' + formatCount(count) + ')'));
    }
    group.appendChild(head);
    return group;
  }

  function renderPreview(parsed) {
    empty(previewWrap);
    previewWrap.removeAttribute('aria-busy');

    if (parsed.error) {
      previewWrap.appendChild(errorBanner(parsed.error));
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      pasteCommit.textContent = 'Add packages';
      return;
    }

    const count = parsed.count || 0;
    const newCount = parsed.new_count || 0;
    const known = parsed.known_count || 0;
    const unknownCount = parsed.unknown_count || 0;
    const rejected = parsed.rejected || [];
    const duplicates = parsed.duplicates || 0;
    const already = Math.max(0, count - newCount);

    // The headline: what this paste would actually do.
    const stats = h('div', 'pt__stats');
    stats.appendChild(stat(formatCount(count) + ' ' + plural(count, 'name', 'names') + ' recognised'));
    if (newCount !== count) stats.appendChild(stat(formatCount(newCount) + ' new'));
    if (already > 0) stats.appendChild(stat(formatCount(already) + ' already selected'));
    if (known > 0) stats.appendChild(stat(formatCount(known) + ' in the catalogue', 'success', 'success'));
    if (unknownCount > 0) {
      stats.appendChild(stat(formatCount(unknownCount) + ' not in the catalogue', 'warning', 'warning'));
    }
    if (duplicates > 0) {
      stats.appendChild(stat(formatCount(duplicates) + ' duplicate ' + plural(duplicates, 'line', 'lines') + ' collapsed'));
    }
    if (rejected.length > 0) {
      stats.appendChild(
        stat(
          formatCount(rejected.length) + ' ' + plural(rejected.length, 'line', 'lines') + ' not a package name',
          'danger',
          'danger'
        )
      );
    }
    previewWrap.appendChild(stats);

    // Backend warnings about the paste as a whole.
    const warns = parsed.warnings || [];
    for (let i = 0; i < warns.length; i += 1) {
      const meta = warningMeta(warns[i].kind);
      const banner = makeBanner(
        meta.variant,
        meta.glyph,
        warns[i].message || meta.label,
        warns[i].hint || '',
        meta.variant === 'danger'
      );
      if (warns[i].package) {
        const p = h('p', 'df-banner__text');
        p.appendChild(h('span', 'df-mono', warns[i].package));
        bannerBody(banner).appendChild(p);
      }
      previewWrap.appendChild(banner);
    }

    // 1. Recognised, with a bounded sample so a 5,000-line paste is honest
    //    without being unreadable.
    const sample = parsed.sample || [];
    if (sample.length > 0) {
      const g = previewGroup('Recognised', known, 'success', 'success');
      const wrap = h('div', 'pt__scrollbox');
      const table = h('table', 'df-table');
      const thead = document.createElement('thead');
      const hr = document.createElement('tr');
      hr.appendChild(h('th', 'df-table__label', 'Package'));
      hr.appendChild(h('th', 'df-table__label', 'Version'));
      hr.appendChild(h('th', 'df-table__detail', 'Summary'));
      thead.appendChild(hr);
      table.appendChild(thead);
      const tbody = document.createElement('tbody');
      for (let i = 0; i < sample.length; i += 1) {
        const row = sample[i];
        const tr = document.createElement('tr');
        const c1 = h('td', 'df-table__label');
        c1.appendChild(h('span', 'df-mono', row.name || ''));
        const c2 = h('td', 'df-table__label');
        c2.appendChild(h('span', 'df-mono', row.version || '—'));
        const c3 = h('td', 'df-table__detail pt__wrap', row.summary || '');
        tr.appendChild(c1);
        tr.appendChild(c2);
        tr.appendChild(c3);
        tbody.appendChild(tr);
      }
      table.appendChild(tbody);
      wrap.appendChild(table);
      g.appendChild(wrap);
      if (known > sample.length) {
        g.appendChild(
          h(
            'p',
            'pt__note',
            'Showing the first ' + formatCount(sample.length) + ' of ' + formatCount(known) + '.'
          )
        );
      }
      previewWrap.appendChild(g);
    }

    // 2. Not in the catalogue — kept, not dropped.
    if (unknownCount > 0) {
      const g = previewGroup('Not in the catalogue', unknownCount, 'warning', 'warning');
      const names = parsed.unknown || [];
      const cluster = h('div', 'df-cluster');
      const shown = Math.min(names.length, LIST_RENDER_CAP);
      for (let i = 0; i < shown; i += 1) {
        const c = h('span', 'df-chip df-chip--warning');
        c.appendChild(h('span', 'df-chip__label df-mono', names[i]));
        cluster.appendChild(c);
      }
      g.appendChild(cluster);
      const overflow = unknownCount - shown;
      g.appendChild(
        h(
          'p',
          'pt__note',
          (overflow > 0 ? 'and ' + formatCount(overflow) + ' more. ' : '') +
            'These are still added and marked unknown — they are kept, not dropped. ' +
            'You may know something the index does not, and debark gives the ' +
            'authoritative answer at build time. If the catalogue for this target ' +
            'has not been built yet, everything appears here.'
        )
      );
      previewWrap.appendChild(g);
    }

    // 3. Not package names at all. Never silently dropped.
    if (rejected.length > 0) {
      const g = previewGroup('Not package names', rejected.length, 'danger', 'danger');
      g.appendChild(rejectedTable(rejected));
      const note = h(
        'p',
        'pt__note',
        'These lines will not be added. If one of them is a download address, ' +
          'add it with "Add URL" instead; if it is a file on this machine, use ' +
          '"Add local .deb".'
      );
      g.appendChild(note);
      const b = api();
      if (b && typeof b.CopyToClipboard === 'function') {
        const cluster = h('div', 'df-cluster');
        cluster.appendChild(
          button(
            'Copy the rejected lines',
            'df-btn df-btn--secondary df-btn--sm',
            function () {
              const lines = rejected
                .map(function (r) {
                  return r.value || '';
                })
                .join('\n');
              Promise.resolve(b.CopyToClipboard(lines)).then(
                function () {
                  toast('success', 'Copied.');
                  announce('Rejected lines copied to the clipboard.');
                },
                function () {
                  toast('warning', 'Could not copy.');
                }
              );
            },
            'copy'
          )
        );
        g.appendChild(cluster);
      }
      previewWrap.appendChild(g);
    }

    if (count === 0) {
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      pasteCommit.textContent = 'Add packages';
      if (rejected.length === 0) {
        previewWrap.appendChild(
          makeBanner('info', 'info', 'Nothing in this text looks like a package name.', '')
        );
      }
      return;
    }

    if (newCount === 0) {
      gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
      pasteCommit.textContent = 'Add packages';
      previewWrap.appendChild(
        makeBanner(
          'info',
          'info',
          'Everything in this list is already selected.',
          'Nothing would change. Close this dialog, or paste a different list.'
        )
      );
      return;
    }

    gate(pasteCommit, false, pasteWhy);
    pasteCommit.textContent =
      'Add ' + formatCount(newCount) + ' ' + plural(newCount, 'package', 'packages');
  }

  pasteArea.addEventListener('input', schedulePreview);
  pasteDialog.el.addEventListener('close', () => { parseSeq++; if (parseTimer) window.clearTimeout(parseTimer); parseTimer = null; });

  function openPasteDialog(trigger, prefill) {
    if (state.hostBusy || state.opBusy || state.destroyed) return;
    pasteError.hidden = true;
    closeAddMenu(false);
    if (typeof prefill === 'string') pasteArea.value = prefill;
    empty(previewWrap);
    gate(pasteCommit, true, pasteWhy, 'Unavailable: paste at least one package name above.');
    pasteCommit.textContent = 'Add packages';
    openDialog(pasteDialog.el, trigger, pasteArea);
    if (pasteArea.value.trim() !== '') schedulePreview();
  }

  // -- add a URL ------------------------------------------------------------

  const urlDialog = makeDialog(false);
  wireFocusRestore(urlDialog.el);
  urlDialog.title.textContent = 'Add URL';
  const urlLock = h('p', 'df-text-secondary'); urlLock.hidden = true; urlDialog.body.appendChild(urlLock);
  const urlActionError = h('div'); urlActionError.hidden = true; urlDialog.body.appendChild(urlActionError);

  const urlStack = h('div', 'df-stack');

  const urlField = h('div', 'df-field');
  const urlLabel = h('label', 'df-field__label', 'Download address');
  const urlInput = h('input', 'df-input df-mono');
  urlInput.id = uid('pt-url');
  urlInput.type = 'text';
  urlInput.setAttribute('spellcheck', 'false');
  urlInput.setAttribute('autocapitalize', 'off');
  urlInput.setAttribute('placeholder', 'https://vendor.example/pool/agent_2.1.0_amd64.deb');
  urlLabel.setAttribute('for', urlInput.id);
  const urlHint = h(
    'span',
    'df-field__hint',
    'The file is downloaded on this machine during the build.'
  );
  const urlError = h('span', 'df-field__error');
  urlError.hidden = true;
  urlField.appendChild(urlLabel);
  urlField.appendChild(urlInput);
  urlField.appendChild(urlHint);
  urlField.appendChild(urlError);
  urlStack.appendChild(urlField);

  const digestField = h('div', 'df-field');
  const digestLabel = h('label', 'df-field__label', 'Expected SHA-256 (optional)');
  const digestInput = h('input', 'df-input df-mono');
  digestInput.id = uid('pt-digest');
  digestInput.type = 'text';
  digestInput.setAttribute('spellcheck', 'false');
  digestInput.setAttribute('autocapitalize', 'off');
  digestInput.setAttribute('placeholder', '64 hex characters, or sha256:…');
  digestLabel.setAttribute('for', digestInput.id);
  const digestHint = h('span', 'df-field__hint', 'Use the vendor’s published digest. Debark checks the downloaded file against it during the build.');
  const digestError = h('span', 'df-field__error');
  digestError.hidden = true;
  digestField.appendChild(digestLabel);
  digestField.appendChild(digestInput);
  digestField.appendChild(digestHint);
  digestField.appendChild(digestError);
  urlStack.appendChild(digestField);

  const urlNotices = h('div', 'df-stack df-stack--tight');
  urlStack.appendChild(urlNotices);
  urlDialog.body.appendChild(urlStack);

  function setFieldError(input, errEl, message) {
    if (message) {
      input.classList.add('is-error');
      input.setAttribute('aria-invalid', 'true');
      empty(errEl);
      errEl.appendChild(icon('danger'));
      errEl.appendChild(document.createTextNode(message));
      errEl.hidden = false;
    } else {
      input.classList.remove('is-error');
      input.removeAttribute('aria-invalid');
      empty(errEl);
      errEl.hidden = true;
    }
  }

  function validateURLFields(showEmpty) {
    empty(urlNotices);
    const raw = urlInput.value;
    const urlRes = raw.trim() === '' && !showEmpty ? { ok: false, quiet: true } : validateURL(raw);
    setFieldError(urlInput, urlError, urlRes.ok || urlRes.quiet ? '' : urlRes.message);

    const digRes = normaliseDigest(digestInput.value);
    setFieldError(digestInput, digestError, digRes.ok ? '' : digRes.message);

    if (urlRes.ok && urlRes.warn) {
      urlNotices.appendChild(makeBanner('warning', 'warning', urlRes.warn, ''));
    }
    if (urlRes.ok && digRes.ok && digRes.value === '') {
      urlNotices.appendChild(
        makeBanner(
          'info',
          'info',
          'This download will be recorded as unverified.',
          'Supply the vendor’s SHA-256 to check that the file matches the expected download.'
        )
      );
    }
    if (urlRes.ok && digRes.ok && digRes.value !== '') {
      const b = makeBanner(
        'success',
        'success',
        'Expected SHA-256 supplied.',
        'A digest mismatch will stop the build.'
      );
      urlNotices.appendChild(b);
    }

    const bad = !(urlRes.ok && digRes.ok);
    gate(urlCommit, bad, urlWhy,
      !urlRes.ok ? 'Unavailable: the download address is not usable yet.'
        : 'Unavailable: the expected SHA-256 is not usable yet.');
    return { url: urlRes, digest: digRes };
  }

  const urlCancel = button('Cancel', 'df-btn df-btn--ghost', function () {
    closeDialog(urlDialog.el);
  });
  const urlCommit = button('Add URL', 'df-btn df-btn--primary', async function () {
    if (gated(urlCommit)) {
      urlInput.focus();
      return;
    }
    const res = validateURLFields(true);
    if (!res.url.ok || !res.digest.ok) return;
    const url = res.url.value;
    const digest = res.digest.value;
    const s = await mutate('onAddURL', [url, digest], 'AddURLs', [[{ url, sha256: digest || '' }]]);
    if (s && !s.error) {
      if ((s.rejected || []).length) {
        empty(urlActionError); urlActionError.appendChild(rejectedTable(s.rejected)); urlActionError.hidden = false;
        return;
      }
      closeDialog(urlDialog.el);
      urlInput.value = '';
      digestInput.value = '';
      announce(s.added > 0 ? 'Vendor URL added.' : 'Selection unchanged.');
    }
  });
  const urlWhy = document.createElement('span');
  urlWhy.className = 'df-visually-hidden';
  urlWhy.id = uid('pt-why');
  urlCommit.setAttribute('aria-describedby', urlWhy.id);
  gate(urlCommit, true, urlWhy, 'Unavailable: type the download address above.');
  urlDialog.footer.appendChild(urlWhy);
  urlDialog.footer.appendChild(urlCancel);
  urlDialog.footer.appendChild(urlCommit);
  document.body.appendChild(urlDialog.el);

  urlInput.addEventListener('input', function () {
    validateURLFields(false);
  });
  digestInput.addEventListener('input', function () {
    validateURLFields(false);
  });
  urlInput.addEventListener('keydown', function (ev) {
    if (ev.key === 'Enter' && !gated(urlCommit)) {
      ev.preventDefault();
      urlCommit.click();
    }
  });
  digestInput.addEventListener('keydown', function (ev) {
    if (ev.key === 'Enter' && !gated(urlCommit)) {
      ev.preventDefault();
      urlCommit.click();
    }
  });

  function openURLDialog(trigger, prefill, prefillDigest) {
    if (state.hostBusy || state.opBusy || state.destroyed) return;
    urlActionError.hidden = true;
    closeAddMenu(false);
    if (typeof prefill === 'string') urlInput.value = prefill;
    if (typeof prefillDigest === 'string') digestInput.value = prefillDigest;
    setFieldError(urlInput, urlError, '');
    setFieldError(digestInput, digestError, '');
    empty(urlNotices);
    gate(urlCommit, true, urlWhy, 'Unavailable: type the download address above.');
    openDialog(urlDialog.el, trigger, urlInput, true);
    if (urlInput.value.trim() !== '') validateURLFields(false);
  }

  // -- add local .deb files -------------------------------------------------

  /**
   * The native picker path. `onAddFile` may do the whole job (returning a
   * SelectionSummary) or only the picking (returning a FilePickResult); with
   * no hook at all the tray drives ChooseLocalDebs + AddLocalDebs itself. It
   * is called exactly once either way.
   *
   * Nothing is uploaded. The chosen paths are handed to the backend as paths,
   * and debark reads the files from this machine during the build.
   */
  async function addLocalFiles(trigger) {
    if (state.hostBusy || state.opBusy || state.destroyed) return;
    setOpBusy(true);
    try {
      const b = api();
      const pick = typeof opts.onAddFile === 'function' ? opts.onAddFile : b && b.ChooseLocalDebs;
      if (typeof pick !== 'function') throw new Error('The file picker is unavailable. Restart Debark and try again.');
      const picked = await pick.call(b);
      if (state.destroyed) return;
      if (picked && typeof picked.total === 'number') { acceptMutation(picked); showFileResult(picked, trigger); return; }
      if (!picked) throw new Error('The file picker returned no result. Try again.');
      if (picked.error) { operationError(picked.error); showFileResult(picked, trigger); return; }
      if (picked.cancelled) return;
      const paths = picked.paths || (picked.path ? [picked.path] : []);
      if (!paths.length) return;
      // A native chooser may stay open while a job starts. Recheck before
      // handing the references to the guarded backend mutation.
      if (state.hostBusy) return;
      if (!b || typeof b.AddLocalDebs !== 'function') throw new Error('Local files cannot be added. Restart Debark and try again.');
      const result = await b.AddLocalDebs(paths);
      if (state.destroyed) return;
      const summary = acceptMutation(result);
      showFileResult(summary, trigger);
      if (!summary.error) announce('Local file selection updated.');
    } catch (error) { if (!state.destroyed) { const projected = jsError(error); operationError(projected); showFileResult({ error: projected }, trigger); } }
    finally {
      if (!state.destroyed) setOpBusy(false);
      if (!fileResult.el.open && trigger && trigger.isConnected) trigger.focus();
    }
  }

  // This portal is independent of the pane. It also works while the picker
  // hides an empty selection or keeps the selection dialog closed.
  const menu = h('div', 'df-menu df-menu--anchored');
  menu.id = uid('pt-add-menu');
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', 'Add packages');
  menu.hidden = true;
  for (const [label, action] of [
    ['Paste list…', openPasteDialog], ['Add URL…', openURLDialog], ['Add local .deb…', addLocalFiles],
  ]) {
    const item = button(label, 'df-btn df-btn--ghost df-menu__item', () => {
      const trigger = state.addTrigger;
      closeAddMenu(false);
      action(trigger);
    });
    item.setAttribute('role', 'menuitem'); item.tabIndex = -1; menu.appendChild(item);
  }
  const busyNotice = h('p', 'pt__note');
  busyNotice.hidden = true;
  scroll.insertBefore(busyNotice, noticeWrap);
  document.body.appendChild(menu);
  const fileResult = makeDialog(false);
  wireFocusRestore(fileResult.el);
  fileResult.footer.appendChild(button('Close', 'df-btn df-btn--primary', () => closeDialog(fileResult.el)));
  document.body.appendChild(fileResult.el);

  function closeAddMenu(restore) {
    if (menu.hidden) return;
    menu.hidden = true;
    const trigger = state.addTrigger;
    if (trigger) trigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', outsideMenu);
    window.removeEventListener('resize', closeOnResize);
    if (restore && trigger && trigger.isConnected) trigger.focus();
  }
  function outsideMenu(event) {
    if (!menu.contains(event.target) && !(state.addTrigger && state.addTrigger.contains(event.target))) closeAddMenu(false);
  }
  function closeOnResize() { closeAddMenu(true); }
  function openAddMenu(trigger) {
    if (state.hostBusy || state.opBusy || state.destroyed || !trigger) return;
    if (!menu.hidden && state.addTrigger === trigger) { closeAddMenu(true); return; }
    closeAddMenu(false);
    state.addTrigger = trigger;
    trigger.setAttribute('aria-haspopup', 'menu');
    trigger.setAttribute('aria-controls', menu.id);
    trigger.setAttribute('aria-expanded', 'true');
    menu.hidden = false;
    const rect = trigger.getBoundingClientRect();
    const box = menu.getBoundingClientRect();
    menu.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - box.width - 8)) + 'px';
    menu.style.top = Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - box.height - 8)) + 'px';
    document.addEventListener('pointerdown', outsideMenu);
    window.addEventListener('resize', closeOnResize);
    menu.querySelector('button').focus();
  }
  menu.addEventListener('keydown', event => {
    const items = Array.from(menu.querySelectorAll('button:not([disabled])'));
    const current = items.indexOf(document.activeElement);
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); closeAddMenu(true); }
    else if (event.key === 'Tab') { closeAddMenu(true); }
    else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 : (current + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
      if (items[next]) items[next].focus();
    }
  });

  // -- lifecycle ------------------------------------------------------------

  function setBusy(busy) {
    state.hostBusy = !!busy;
    if (state.hostBusy) closeAddMenu(true);
    applyBusy();
  }

  function destroy() {
    state.destroyed = true;
    state.pageSeq += 1;
    parseSeq += 1;
    state.mutationSeq += 1;
    closeAddMenu(false);
    if (parseTimer) window.clearTimeout(parseTimer);
    parseTimer = null;
    closeDialog(pasteDialog.el);
    closeDialog(urlDialog.el);
    pasteDialog.el.remove();
    urlDialog.el.remove();
    closeDialog(fileResult.el);
    fileResult.el.remove();
    menu.remove();
    announcer.remove();
    empty(el);
  }

  function focus() {
    const buttons = removeButtons();
    if (buttons.length) focusChip(state.activeIndex);
    else focusFallback();
  }

  // First paint: the empty state, until the host says otherwise.
  render(EMPTY_SUMMARY);

  return {
    el: el,
    render: render,
    setBusy: setBusy,
    destroy: destroy,
    focus: focus,
    openPasteDialog: openPasteDialog,
    openURLDialog: openURLDialog,
    openClearDialog: openClearDialog,
    openAddMenu: openAddMenu,
    undo: undo,
  };
}

export default createTray;
