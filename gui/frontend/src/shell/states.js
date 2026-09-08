// ---------------------------------------------------------------------------
// states.js — the shared loading, empty and error states.
//
// Every screen in this app renders three states or it is not finished
// (docs/dev/screen-contract.md, rule 2). This module is what makes that cheap:
// nine screens import these helpers instead of each inventing a spinner.
//
// The rules it enforces, so a screen author does not have to remember them:
//
//   * A spinner with no label is a bug. `loadingState` throws without one.
//     The backend's progress events carry a human label precisely so this
//     never happens — `progressFrom(event)` extracts it.
//   * Indeterminate is a first-class path, not a fallback. `total_bytes` is
//     -1/0 whenever an archive sends no Content-Length, which is common.
//   * An error is two sentences: what is wrong (`summary`) and what to do
//     (`hint`). Raw stderr goes in a collapsed drawer, never inline.
//   * A bare exit code is never the message. This module is the last line of
//     defence for that definition-of-done item: `errorState` detects a summary
//     that is only a number, demotes it into the detail drawer, and puts a
//     real sentence in its place.
//   * "No results" with no next step is a defect (design system rule 7), so
//     `emptyState` requires a message, and a filtered-empty state must offer a
//     way to clear the filter.
//
// Constraints this file obeys:
//
//   * No npm dependency, no build step, no framework. A vanilla ES module.
//   * No network call of any kind.
//   * No bespoke component CSS. Everything below is markup for components that
//     already ship in frontend/src/design/components.css; the only inline
//     styles are layout percentages and design tokens.
//   * **No innerHTML, anywhere.** Every node is built with createElement /
//     createElementNS and every string lands in `textContent`. Backend text
//     includes package names, file paths and raw stderr, and stderr can contain
//     anything at all; there is no interpolation path for it to escape from.
//
// Every helper returns `{ el, ... , destroy() }`. `el` is yours: insert it,
// move it, and call `destroy()` when you are done. `destroy()` stops every
// timer this module started and detaches the element. Nothing is left running.
// ---------------------------------------------------------------------------

import { bindDisclosure } from './disclosure.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

// Announcements are rate-limited: a screen reader should hear state
// transitions, not every byte. See `makeAnnouncer` below.
const ANNOUNCE_MIN_GAP_MS = 1500;
const ANNOUNCE_DEFER_MS = 150;
const ANNOUNCE_PERCENT_STEP = 10;

// ---------------------------------------------------------------------------
// DOM helpers. Text only ever reaches the document through textContent.
// ---------------------------------------------------------------------------

function mk(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (typeof text === 'string' && text !== '') node.textContent = text;
  return node;
}

function setText(node, text) {
  const next = typeof text === 'string' ? text : '';
  if (node.textContent !== next) node.textContent = next;
}

// Several design-system components set `display: flex`, which beats the UA's
// `[hidden] { display: none }`. An inline style is the unambiguous toggle, and
// `display: none` removes the node from the accessibility tree as well.
function setShown(node, visible) {
  const next = visible ? '' : 'none';
  if (node.style.display !== next) node.style.display = next;
}

function has(obj, key) {
  return Object.prototype.hasOwnProperty.call(obj, key);
}

function text(value) {
  return typeof value === 'string' ? value.trim() : '';
}

function require_(value, fn, field, why) {
  const s = text(value);
  if (!s) throw new TypeError(fn + ': `' + field + '` is required — ' + why);
  return s;
}

// ---------------------------------------------------------------------------
// Icons. Inline SVG, authored here as data, never as markup: the design system
// ships no icon library and forbids a webfont. Shapes match the ones the
// gallery documents, because a state is never signalled by colour alone.
// ---------------------------------------------------------------------------

const ICONS = {
  // circle-i — a neutral fact.
  info: [
    ['circle', { cx: '12', cy: '12', r: '9' }],
    ['path', { d: 'M12 11.2V16' }],
    ['circle', { cx: '12', cy: '7.9', r: '1', fill: 'currentColor', stroke: 'none' }],
  ],
  // circle-tick — it worked.
  success: [
    ['circle', { cx: '12', cy: '12', r: '9' }],
    ['path', { d: 'M8 12.4l2.6 2.6L16 9.6' }],
  ],
  // triangle-! — it will work, but not the way you assume.
  warning: [
    ['path', { d: 'M12 4.2L2.6 20.2h18.8z' }],
    ['path', { d: 'M12 10v4.4' }],
    ['circle', { cx: '12', cy: '17.6', r: '1', fill: 'currentColor', stroke: 'none' }],
  ],
  // octagon-x — it failed.
  danger: [
    ['path', { d: 'M8.3 3h7.4L21 8.3v7.4L15.7 21H8.3L3 15.7V8.3z' }],
    ['path', { d: 'M15 9l-6 6M9 9l6 6' }],
  ],
  // magnifier — a filter matched nothing.
  search: [
    ['circle', { cx: '11', cy: '11', r: '7' }],
    ['path', { d: 'M20 20l-3.6-3.6' }],
  ],
  // tray — a collection that is genuinely empty.
  tray: [
    ['path', { d: 'M4 5.5h16l1.5 8v5.5a1 1 0 01-1 1H3.5a1 1 0 01-1-1V13.5z' }],
    ['path', { d: 'M2.5 13.5h5l1.5 3h6l1.5-3h5' }],
  ],
  // box — nothing has been built yet.
  box: [
    ['path', { d: 'M12 2.6l8.2 4.7v9.4L12 21.4 3.8 16.7V7.3z' }],
    ['path', { d: 'M3.8 7.3L12 12l8.2-4.7M12 12v9.4' }],
  ],
  // x — dismiss.
  close: [['path', { d: 'M18 6L6 18M6 6l12 12' }]],
};

function icon(name, className) {
  const spec = has(ICONS, name) ? ICONS[name] : ICONS.info;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  // An <svg>'s .className is a read-only SVGAnimatedString.
  if (className) svg.setAttribute('class', className);
  for (const [tag, attrs] of spec) {
    const child = document.createElementNS(SVG_NS, tag);
    for (const key of Object.keys(attrs)) child.setAttribute(key, attrs[key]);
    svg.appendChild(child);
  }
  return svg;
}

// A caller may pass an icon name from the table above, or an <svg> element it
// built itself. It may never pass markup.
function resolveIcon(value, fallbackName, className) {
  if (value instanceof SVGElement) {
    value.setAttribute('aria-hidden', 'true');
    if (className) value.setAttribute('class', className);
    return value;
  }
  const name = typeof value === 'string' && has(ICONS, value) ? value : fallbackName;
  return icon(name, className);
}

// ---------------------------------------------------------------------------
// Actions. `{ label, onClick, variant, disabled }` — one, or an array.
// ---------------------------------------------------------------------------

function collectActions(opts) {
  const out = [];
  if (opts.action) out.push(opts.action);
  if (Array.isArray(opts.actions)) {
    for (const a of opts.actions) if (a) out.push(a);
  }
  return out;
}

function actionButton(action, defaultVariant) {
  const label = text(action.label);
  if (!label) return null;
  const variant = text(action.variant) || defaultVariant;
  const btn = mk('button', 'df-btn df-btn--' + variant + ' df-btn--sm', label);
  btn.type = 'button';
  if (action.disabled) btn.disabled = true;
  if (typeof action.onClick === 'function') {
    btn.addEventListener('click', action.onClick);
  }
  return btn;
}

function actionsRow(actions) {
  const row = mk('div', 'df-state__actions');
  let n = 0;
  for (const action of actions) {
    const btn = actionButton(action, n === 0 ? 'primary' : 'secondary');
    if (btn) {
      row.appendChild(btn);
      n += 1;
    }
  }
  setShown(row, n > 0);
  return row;
}

// ---------------------------------------------------------------------------
// Formatting. Exported because nine screens would otherwise write nine of
// these, and they would not agree with each other.
// ---------------------------------------------------------------------------

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/**
 * formatBytes(1484212) === '1.4 MiB'. Binary units, because that is what apt,
 * dpkg and this app's own copy use. Returns '' for anything not a finite,
 * non-negative number — including the -1 an archive with no Content-Length
 * produces.
 */
export function formatBytes(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return '';
  if (n < 1024) return String(Math.round(n)) + ' B';
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const rounded = value >= 100 ? String(Math.round(value)) : value.toFixed(1).replace(/\.0$/, '');
  return rounded + ' ' + BYTE_UNITS[unit];
}

/**
 * formatDuration(83000) === '1:23'. Under a minute it reads '12s', because
 * '0:12' looks like a stalled clock.
 */
export function formatDuration(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '';
  const total = Math.floor(ms / 1000);
  if (total < 60) return String(total) + 's';
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  if (minutes < 60) return minutes + ':' + String(seconds).padStart(2, '0');
  const hours = Math.floor(minutes / 60);
  return hours + ':' + String(minutes % 60).padStart(2, '0') + ':' + String(seconds).padStart(2, '0');
}

function percentOf(fraction) {
  return Math.max(0, Math.min(100, Math.round(fraction * 100)));
}

function isFraction(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1;
}

// ---------------------------------------------------------------------------
// progressFrom — the bridge between a backend progress payload and
// `loadingState`. This is the reason an unlabelled spinner is hard to build:
// the natural way to drive a loading state already carries a label.
//
// It accepts CatalogProgress, BuildProgress, ExportProgress and
// ReadinessProgress (docs/dev/binding-surface.md) without the caller caring
// which is which, and it never returns an empty label.
// ---------------------------------------------------------------------------

const PHASE_LABELS = {
  // catalogue build
  resolve: 'Working out which package lists to fetch',
  release: 'Checking the archive',
  download: 'Downloading package lists',
  parse: 'Reading package lists',
  index: 'Building the search index',
  save: 'Saving the catalogue',
  done: 'Finished',
  // bundle build
  idle: 'Waiting to start',
  starting: 'Starting debark',
  resolving: 'Asking apt to resolve the selection',
  downloading: 'Downloading packages',
  assembling: 'Assembling the bundle',
  signing: 'Signing the manifest',
  verifying: 'Verifying the bundle',
  finished: 'Finished',
  failed: 'Failed',
  cancelled: 'Cancelled',
  // export / copy
  scanning: 'Scanning the bundle',
  copying: 'Copying to the drive',
  flushing: 'Flushing writes to the drive',
};

function firstText(...values) {
  for (const value of values) {
    const s = text(value);
    if (s) return s;
  }
  return '';
}

function num(value) {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

/**
 * progressFrom(payload) → a patch for `loadingState` / `loading.update()`.
 *
 *   const loading = states.loadingState(progressFrom(status.progress));
 *   ctx.on('catalog:progress', (p) => loading.update(progressFrom(p)));
 *
 * `progress` is always present in the result: a number in [0,1] for a
 * determinate bar, or `null` for an indeterminate one. A payload with no
 * usable total therefore renders an honest indeterminate bar rather than an
 * empty determinate one that never moves.
 */
export function progressFrom(payload) {
  const p = payload && typeof payload === 'object' ? payload : {};
  const phase = text(p.phase);
  const label = firstText(
    p.label,
    p.message,
    p.title,
    has(PHASE_LABELS, phase) ? PHASE_LABELS[phase] : '',
    // Last resort only. Reached when the backend sent neither a label nor a
    // phase we recognise — a bug, but throwing here would freeze the screen,
    // which is the one thing this app must never do.
    'Working…'
  );

  const current = num(p.current) || num(p.files_done) || num(p.done);
  const total = num(p.total) || num(p.files_total);
  const bytes = num(p.bytes_done);
  const bytesTotal = num(p.bytes_total);

  let fraction = null;
  if (isFraction(p.overall_fraction)) fraction = p.overall_fraction;
  else if (isFraction(p.fraction)) fraction = p.fraction;
  else if (total > 0 && current >= 0) fraction = Math.min(1, current / total);
  else if (bytesTotal > 0 && bytes >= 0) fraction = Math.min(1, bytes / bytesTotal);

  return {
    label: label,
    item: firstText(p.item, p.package, p.file, p.check_id),
    progress: fraction,
    current: current,
    total: total,
    bytes: bytes,
    // A -1 or 0 total means "no Content-Length" — normalised to 0, which the
    // loading state renders as "4.2 MiB so far" plus an indeterminate bar.
    bytesTotal: bytesTotal > 0 ? bytesTotal : 0,
    elapsed: num(p.elapsed_ms),
  };
}

// ---------------------------------------------------------------------------
// The announcer. One visually-hidden polite live region per loading state.
//
// The visible label and percentage are NOT inside it, so a screen reader is
// not read a new number four times a second. Instead a composed sentence is
// pushed into the region on a state transition only: the phase label changed,
// the bar changed between determinate and indeterminate, the percentage
// crossed a 10% boundary, or a cancel was requested. Announcements are
// additionally deferred (so the region is in the document before its text
// changes, which is what makes an aria-live update fire at all) and spaced at
// least 1.5 s apart, with the last one always flushed.
// ---------------------------------------------------------------------------

function makeAnnouncer(region) {
  let timer = null;
  let pending = '';
  let lastSpoken = '';
  let lastAt = 0;

  function flush() {
    timer = null;
    if (pending === lastSpoken) return;
    lastSpoken = pending;
    lastAt = Date.now();
    setText(region, pending);
  }

  return {
    say(message) {
      const next = text(message);
      if (!next || next === lastSpoken) return;
      pending = next;
      if (timer !== null) return;
      const gap = ANNOUNCE_MIN_GAP_MS - (Date.now() - lastAt);
      timer = setTimeout(flush, Math.max(ANNOUNCE_DEFER_MS, gap));
    },
    stop() {
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    },
  };
}

// ---------------------------------------------------------------------------
// loadingState
// ---------------------------------------------------------------------------

/**
 * loadingState({ label, item, detail, progress, current, total, bytes,
 *                bytesTotal, elapsed, onCancel, cancelLabel })
 *   → { el, update(patch), destroy() }
 *
 * `label` is REQUIRED and this function throws without one. A spinner with no
 * label tells an operator nothing, and under `prefers-reduced-motion` the
 * spinner does not even move — the words are all that is left.
 *
 * The bar:
 *   * omit `progress` entirely      → spinner and words, no bar
 *   * `progress: 0.42`              → determinate bar at 42%
 *   * `progress: null` (or -1, NaN) → indeterminate bar
 *
 * Indeterminate is a real, common case, not a fallback: `total_bytes` is -1
 * whenever a mirror sends no Content-Length.
 *
 * `elapsed: true` starts a one-second clock that this handle owns and
 * `destroy()` stops; `elapsed: <ms>` renders a number the backend supplied and
 * starts no timer.
 *
 * `onCancel` renders a Cancel button. Pressing it disables the button and
 * relabels it "Cancelling…" before calling back, so the operator can see that
 * the request landed — a cancel that looks ignored is the same defect as a
 * frozen window.
 */
export function loadingState(options) {
  const opts = options && typeof options === 'object' ? options : {};

  const state = {
    label: require_(
      opts.label,
      'loadingState',
      'label',
      'a spinner with no label tells an operator nothing. Every backend ' +
        'progress event carries a human label: pass it, or build the whole ' +
        'patch with progressFrom(event).'
    ),
    item: text(opts.item),
    detail: text(opts.detail),
    hasBar: has(opts, 'progress') || num(opts.total) > 0 || num(opts.bytesTotal) > 0 || num(opts.bytes) > 0,
    progress: isFraction(opts.progress) ? opts.progress : null,
    current: num(opts.current),
    total: num(opts.total),
    bytes: num(opts.bytes),
    bytesTotal: num(opts.bytesTotal),
    elapsedMs: typeof opts.elapsed === 'number' && Number.isFinite(opts.elapsed) ? opts.elapsed : null,
    cancelling: Boolean(opts.cancelling),
  };

  const ticking = opts.elapsed === true;
  const startedAt = Date.now();
  let tickTimer = null;

  const el = mk('div', 'df-state df-state--loading');
  el.setAttribute('aria-busy', 'true');

  const spinner = mk('span', 'df-spinner df-spinner--lg');
  spinner.setAttribute('aria-hidden', 'true');
  el.appendChild(spinner);

  const title = mk('span', 'df-state__title', state.label);
  el.appendChild(title);

  // The specific thing being waited on. Monospace, because it is a path.
  const itemLine = mk('p', 'df-state__text df-mono df-truncate');
  el.appendChild(itemLine);

  const detailLine = mk('p', 'df-state__text');
  el.appendChild(detailLine);

  // The design system has no `.df-state__progress` slot; this wrapper is the
  // same shape `.df-state__detail` uses, expressed with tokens.
  const bar = mk('div', 'df-stack df-stack--tight');
  bar.style.width = '100%';
  bar.style.maxWidth = 'var(--measure-prose)';

  const barLabel = mk('div', 'df-progress-label');
  const barCounts = mk('span', 'df-truncate');
  const barValue = mk('span', 'df-progress-label__value');
  barLabel.appendChild(barCounts);
  barLabel.appendChild(barValue);
  bar.appendChild(barLabel);

  const track = mk('div', 'df-progress');
  track.setAttribute('role', 'progressbar');
  track.setAttribute('aria-valuemin', '0');
  track.setAttribute('aria-valuemax', '100');
  const fill = mk('div', 'df-progress__bar');
  track.appendChild(fill);
  bar.appendChild(track);
  el.appendChild(bar);

  let cancelBtn = null;
  const actions = mk('div', 'df-state__actions');
  if (typeof opts.onCancel === 'function') {
    cancelBtn = mk('button', 'df-btn df-btn--secondary df-btn--sm', text(opts.cancelLabel) || 'Cancel');
    cancelBtn.type = 'button';
    cancelBtn.addEventListener('click', () => {
      if (state.cancelling) return;
      state.cancelling = true;
      render();
      opts.onCancel();
    });
    actions.appendChild(cancelBtn);
  }
  setShown(actions, cancelBtn !== null);
  el.appendChild(actions);

  const live = mk('span', 'df-visually-hidden');
  live.setAttribute('role', 'status');
  live.setAttribute('aria-live', 'polite');
  live.setAttribute('aria-atomic', 'true');
  el.appendChild(live);

  const announcer = makeAnnouncer(live);
  let spoken = { label: '', determinate: null, bucket: -1, cancelling: false };

  function elapsedText() {
    if (ticking) return formatDuration(Date.now() - startedAt);
    if (state.elapsedMs === null) return '';
    return formatDuration(state.elapsedMs);
  }

  function countsText() {
    const parts = [];
    if (state.bytesTotal > 0) {
      parts.push(formatBytes(state.bytes) + ' of ' + formatBytes(state.bytesTotal));
    } else if (state.bytes > 0) {
      // No Content-Length. Say what is known and nothing more.
      parts.push(formatBytes(state.bytes) + ' so far');
    } else if (state.total > 0) {
      parts.push(String(state.current) + ' of ' + String(state.total));
    } else if (state.current > 0) {
      parts.push(String(state.current) + ' done');
    }
    const elapsed = elapsedText();
    if (elapsed) parts.push(elapsed + ' elapsed');
    return parts.join(' · ');
  }

  function render() {
    const determinate = state.progress !== null;
    const pct = determinate ? percentOf(state.progress) : -1;

    setText(title, state.label);

    setText(itemLine, state.item);
    setShown(itemLine, state.item !== '');

    setText(detailLine, state.detail);
    setShown(detailLine, state.detail !== '');

    setShown(bar, state.hasBar);
    // The bar's accessible name is the phase, never a bare number. Set even
    // while the bar is hidden, so it is never momentarily anonymous.
    track.setAttribute('aria-label', state.label);
    if (state.hasBar) {
      const counts = countsText();
      setText(barCounts, counts);
      setText(barValue, determinate ? String(pct) + '%' : '');
      setShown(barValue, determinate);

      if (determinate) {
        track.classList.remove('df-progress--indeterminate');
        track.setAttribute('aria-valuenow', String(pct));
        fill.style.width = String(pct) + '%';
      } else {
        track.classList.add('df-progress--indeterminate');
        track.removeAttribute('aria-valuenow');
        fill.style.width = '';
      }
    }

    if (cancelBtn) {
      setText(cancelBtn, state.cancelling ? 'Cancelling…' : text(opts.cancelLabel) || 'Cancel');
      cancelBtn.disabled = state.cancelling;
      if (state.cancelling) cancelBtn.setAttribute('aria-busy', 'true');
      else cancelBtn.removeAttribute('aria-busy');
    }

    // Announce transitions, not bytes.
    const bucket = determinate ? Math.floor(pct / ANNOUNCE_PERCENT_STEP) : -1;
    const labelChanged = state.label !== spoken.label;
    const changed =
      labelChanged ||
      determinate !== spoken.determinate ||
      bucket !== spoken.bucket ||
      state.cancelling !== spoken.cancelling;

    if (changed) {
      const bits = [state.label];
      // The item is a path; it is worth hearing when the phase changes and
      // pure noise on every one of the thousands of items within a phase.
      if (labelChanged && state.item) bits.push(state.item);
      if (determinate) bits.push(String(pct) + '%');
      if (state.cancelling) bits.push('cancelling');
      announcer.say(bits.join(' — '));
      spoken = {
        label: state.label,
        determinate: determinate,
        bucket: bucket,
        cancelling: state.cancelling,
      };
    }
  }

  if (ticking) tickTimer = setInterval(render, 1000);
  render();

  return {
    el: el,

    /**
     * update(patch) — merge and re-render in place. Any key accepted by
     * `loadingState` may be patched, including a whole `progressFrom(event)`
     * result. Passing `label: ''` is refused: the label cannot be taken away
     * once it exists.
     */
    update(patch) {
      const p = patch && typeof patch === 'object' ? patch : {};
      if (has(p, 'label')) {
        const next = text(p.label);
        if (next) state.label = next;
      }
      if (has(p, 'item')) state.item = text(p.item);
      if (has(p, 'detail')) state.detail = text(p.detail);
      if (has(p, 'progress')) {
        state.progress = isFraction(p.progress) ? p.progress : null;
        state.hasBar = true;
      }
      if (has(p, 'current')) state.current = num(p.current);
      if (has(p, 'total')) state.total = num(p.total);
      if (has(p, 'bytes')) state.bytes = num(p.bytes);
      if (has(p, 'bytesTotal')) state.bytesTotal = num(p.bytesTotal);
      if (has(p, 'elapsed') && typeof p.elapsed === 'number' && Number.isFinite(p.elapsed)) {
        state.elapsedMs = p.elapsed;
      }
      if (has(p, 'cancelling')) state.cancelling = Boolean(p.cancelling);
      if (state.total > 0 || state.bytesTotal > 0 || state.bytes > 0) state.hasBar = true;
      render();
    },

    destroy() {
      announcer.stop();
      if (tickTimer !== null) {
        clearInterval(tickTimer);
        tickTimer = null;
      }
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

// ---------------------------------------------------------------------------
// emptyState
// ---------------------------------------------------------------------------

const EMPTY_KINDS = {
  // Nothing exists yet. The operator has not done the thing that creates it.
  'nothing-yet': { icon: 'box' },
  // A filter or a query matched nothing. Offer to clear it.
  'no-matches': { icon: 'search' },
  // A genuinely empty collection: an empty selection tray, no drives mounted.
  'collection-empty': { icon: 'tray' },
};

/**
 * emptyState({ kind, title, message, action, actions, onClear, clearLabel, icon })
 *   → { el, destroy() }
 *
 * Three kinds, because they need different words and "No results" for all
 * three throws away the chance to tell someone what to do:
 *
 *   'nothing-yet'      no catalogue has been built for this target yet
 *   'no-matches'       a search or filter matched nothing — offer to clear it
 *   'collection-empty' the selection tray is empty, no drives are mounted
 *
 * `title` and `message` are both required: a headline names what is missing,
 * a sentence says what to do next. "Nothing here" with no next step is a
 * defect, not a state (design system rule 7).
 *
 * A 'no-matches' state must additionally offer a way out — pass `onClear`, or
 * an action of your own — because the operator's most likely next move is to
 * widen the query, and hunting for the clear button is not a feature.
 */
export function emptyState(options) {
  const opts = options && typeof options === 'object' ? options : {};
  const kind = has(EMPTY_KINDS, opts.kind) ? opts.kind : 'nothing-yet';

  const title = require_(
    opts.title,
    'emptyState',
    'title',
    'name what is missing, in the operator\'s terms.'
  );
  const message = require_(
    opts.message,
    'emptyState',
    'message',
    'say what to do next. "No results" with no next step is a defect ' +
      '(design system rule 7).'
  );

  const actions = collectActions(opts);
  if (typeof opts.onClear === 'function') {
    actions.push({
      label: text(opts.clearLabel) || 'Clear filters',
      onClick: opts.onClear,
      variant: actions.length === 0 ? 'primary' : 'secondary',
    });
  }
  if (kind === 'no-matches' && actions.length === 0) {
    throw new TypeError(
      'emptyState: kind "no-matches" needs a way out — pass onClear, or an ' +
        'action. A filter that matched nothing must offer to clear itself.'
    );
  }

  const el = mk('div', 'df-state');
  el.appendChild(resolveIcon(opts.icon, EMPTY_KINDS[kind].icon, 'df-state__icon'));
  el.appendChild(mk('span', 'df-state__title', title));
  el.appendChild(mk('p', 'df-state__text', message));
  el.appendChild(actionsRow(actions));

  return {
    el: el,
    destroy() {
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

// ---------------------------------------------------------------------------
// errorState
// ---------------------------------------------------------------------------

// The frozen 0–7 exit-code table, as sentences rather than numbers. Used only
// when the backend handed us nothing usable to show; the adapter's own summary
// is always preferred. This is a label table, not engine logic.
const EXIT_CLASS_SUMMARY = {
  usage: 'debark rejected the command it was given.',
  environment: 'The builder machine is missing something debark needs.',
  incomplete: 'The bundle was built, but it is incomplete.',
  verification: 'Verification failed: a signature, digest or index did not match.',
  resolution: 'apt could not satisfy the request.',
  policy: 'A policy rule blocked this.',
  'target-mismatch': 'The bundle does not match the target it was checked against.',
};

const LAST_RESORT_SUMMARY = 'Something went wrong, and debark did not say what.';
const LAST_RESORT_HINT = 'Open the details below for the raw output, or run the command yourself.';

// A message that is only a number, or only "exit status 3", is not a message.
// Catching it here is the last line of defence for the project's "no error
// path may surface a raw exit code alone" rule.
const BARE_CODE_PATTERNS = [
  /^-?\d+$/,
  /^(?:process\s+)?(?:exit(?:ed)?|status|code|error|errno|rc)\b[\s:=]*-?\d+\.?$/i,
  /^exit\s+status\s+-?\d+\.?$/i,
  /^signal\s+-?\d+\.?$/i,
];

function isBareCode(s) {
  const trimmed = s.trim().replace(/[.\s]+$/, '');
  if (!trimmed) return true;
  for (const re of BARE_CODE_PATTERNS) {
    if (re.test(trimmed)) return true;
  }
  return false;
}

// argv → a line an operator can paste. Display only.
function formatCommand(command) {
  if (typeof command === 'string') return command.trim();
  if (!Array.isArray(command)) return '';
  return command
    .map((arg) => {
      const s = String(arg);
      return /[\s"'\\$`|&;<>()*?![\]{}]/.test(s) ? "'" + s.replace(/'/g, "'\\''") + "'" : s;
    })
    .join(' ')
    .trim();
}

/**
 * errorState({ summary, hint, detail, command, exitClass, exitCode,
 *              onRetry, retryLabel, retryable, actions, detailLabel, error })
 *   → { el, destroy() }
 *
 * `summary` and `hint` are separate fields on purpose. The backend already
 * produces two sentences — what is wrong, and what to do about it — and
 * rendering them as one blob throws that away. The summary is the headline;
 * the hint is the sentence under it.
 *
 * `detail` is the raw material: captured stderr, a wrapped error chain, a
 * parser's complaint. It goes in a disclosure that is collapsed by default,
 * never inline. This audience wants to be able to read stderr; they do not
 * want it in their face. The failing argv and the exit class go in there too,
 * so the operator can run the command themselves.
 *
 * Pass a whole `UIError` from the bridge as `error` and every field is mapped
 * for you; anything you also pass explicitly wins:
 *
 *   states.errorState({ error: result.error, onRetry: () => start() });
 *
 * This function never throws. Throwing while rendering an error is the worst
 * possible moment, so a missing or useless summary degrades to a truthful
 * sentence instead — and the useless original is demoted into the details,
 * where a bare exit code belongs.
 */
export function errorState(options) {
  const opts = options && typeof options === 'object' ? options : {};
  const src = opts.error && typeof opts.error === 'object' ? opts.error : {};

  const hint = firstText(opts.hint, src.hint);
  const detail = firstText(opts.detail, src.details, src.detail);
  const command = formatCommand(has(opts, 'command') ? opts.command : src.command);
  const exitClass = firstText(opts.exitClass, src.exit_class, src.exitClass);
  const exitCode = (function () {
    const raw = has(opts, 'exitCode') ? opts.exitCode : src.exit_code;
    return typeof raw === 'number' && Number.isFinite(raw) ? raw : null;
  })();

  let summary = firstText(opts.summary, src.message, src.summary);
  let demoted = '';
  let substituted = false;
  if (isBareCode(summary)) {
    demoted = summary;
    substituted = true;
    summary =
      (exitClass && has(EXIT_CLASS_SUMMARY, exitClass) ? EXIT_CLASS_SUMMARY[exitClass] : '') ||
      LAST_RESORT_SUMMARY;
  }

  const el = mk('div', 'df-state df-state--error');
  el.setAttribute('role', 'alert');

  el.appendChild(icon('danger', 'df-state__icon'));
  el.appendChild(mk('span', 'df-state__title', summary));

  // A substituted summary always gets a next step, because the sentence that
  // replaced the useless one deliberately says nothing specific.
  const shownHint = hint || (substituted ? LAST_RESORT_HINT : '');
  if (shownHint) el.appendChild(mk('p', 'df-state__text', shownHint));

  const retryable = has(opts, 'retryable') ? Boolean(opts.retryable) : src.retryable !== false;
  const actions = [];
  if (typeof opts.onRetry === 'function' && retryable) {
    actions.push({
      label: text(opts.retryLabel) || 'Retry',
      onClick: opts.onRetry,
      variant: 'primary',
    });
  }
  for (const a of collectActions(opts)) actions.push(a);
  el.appendChild(actionsRow(actions));

  // The details drawer: collapsed, verbatim, and never the primary message.
  if (detail || command || exitClass || exitCode !== null || demoted) {
    const wrap = mk('div', 'df-state__detail');
    const disclosure = mk('details', 'df-disclosure');
    const summaryEl = mk('summary', 'df-summary', text(opts.detailLabel) || 'What failed');
    disclosure.appendChild(summaryEl);
    bindDisclosure(disclosure);

    const body = mk('div', 'df-disclosure__body');
    const log = mk('pre', 'df-log');

    if (command) log.appendChild(mk('span', 'df-log__line df-log__line--muted', '$ ' + command));
    if (exitClass || exitCode !== null) {
      const bits = [];
      if (exitClass) bits.push(exitClass);
      if (exitCode !== null) bits.push('exit code ' + String(exitCode));
      log.appendChild(mk('span', 'df-log__line df-log__line--muted', bits.join(' · ')));
    }
    if (demoted) {
      log.appendChild(mk('span', 'df-log__line df-log__line--muted', 'reported as: ' + demoted));
    }
    if (detail) {
      // One text node. `.df-log` is `white-space: pre-wrap`, so newlines,
      // tabs and every byte of stderr survive intact — as text, never markup.
      log.appendChild(document.createTextNode(detail));
    }

    body.appendChild(log);
    disclosure.appendChild(body);
    wrap.appendChild(disclosure);
    el.appendChild(wrap);
  }

  return {
    el: el,
    destroy() {
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

// ---------------------------------------------------------------------------
// skeletonRows
// ---------------------------------------------------------------------------

// Deterministic, so a re-render does not reshuffle the placeholders and the
// page does not shimmer differently every time it is opened.
const SKELETON_WIDTHS = [
  ['78%', '64%', '88%'],
  ['62%', '74%', '70%'],
  ['86%', '56%', '94%'],
  ['70%', '80%', '62%'],
  ['54%', '66%', '82%'],
];

const MAX_SKELETON_ROWS = 200;

/**
 * skeletonRows(count, { label }) → { el, setCount(n), destroy() }
 *
 * Placeholder rows for a list that is loading. Each row is the design
 * system's own `.df-skeleton-row`, whose height is `--skeleton-row-height` —
 * documented as, and required to be, equal to `--row-height`. That is what
 * stops the list jumping when real rows replace these, and it is why this
 * function writes no height of its own: matching the token by construction
 * beats matching it by copying the number.
 *
 * The rows themselves are `aria-hidden`: they are decoration, and a screen
 * reader reading twelve rows of nothing is worse than silence. The optional
 * `label` becomes a polite status line instead.
 */
export function skeletonRows(count, options) {
  const opts = options && typeof options === 'object' ? options : {};
  const el = mk('div');
  el.setAttribute('aria-busy', 'true');

  const live = mk('span', 'df-visually-hidden', text(opts.label));
  live.setAttribute('role', 'status');
  live.setAttribute('aria-live', 'polite');

  const rows = [];

  function addRow(index) {
    const widths = SKELETON_WIDTHS[index % SKELETON_WIDTHS.length];
    const row = mk('div', 'df-skeleton-row');
    row.setAttribute('aria-hidden', 'true');

    const check = mk('span', 'df-skeleton df-skeleton--check');
    row.appendChild(check);
    for (const width of widths) {
      const cell = mk('span', 'df-skeleton');
      cell.style.width = width;
      row.appendChild(cell);
    }
    const badge = mk('span', 'df-skeleton');
    badge.style.width = '44px';
    row.appendChild(badge);

    el.insertBefore(row, live);
    rows.push(row);
  }

  function setCount(next) {
    const want = Math.max(0, Math.min(MAX_SKELETON_ROWS, Math.floor(num(next))));
    while (rows.length > want) {
      const row = rows.pop();
      if (row.parentNode) row.parentNode.removeChild(row);
    }
    while (rows.length < want) addRow(rows.length);
  }

  el.appendChild(live);
  setCount(typeof count === 'number' ? count : 8);

  return {
    el: el,
    setCount: setCount,
    destroy() {
      setCount(0);
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

// ---------------------------------------------------------------------------
// inlineBanner
// ---------------------------------------------------------------------------

const BANNER_KINDS = {
  info: { icon: 'info', role: 'status' },
  success: { icon: 'success', role: 'status' },
  warning: { icon: 'warning', role: 'status' },
  danger: { icon: 'danger', role: 'alert' },
};

/**
 * inlineBanner({ kind, title, message, action, actions, dismissible,
 *                onDismiss, dismissLabel })
 *   → { el, update(patch), dismiss(), destroy() }
 *
 * A message that belongs beside the content rather than in front of it: a
 * stale catalogue, an unsigned bundle, a partial result. `danger` gets
 * `role="alert"`; everything else gets `role="status"`, per the design
 * system. Each kind carries a distinct icon shape as well as its colour, so
 * the kind survives without hue.
 *
 * At least one of `title` and `message` is required — a banner with no words
 * is a coloured stripe.
 */
export function inlineBanner(options) {
  const opts = options && typeof options === 'object' ? options : {};

  const state = {
    kind: has(BANNER_KINDS, opts.kind) ? opts.kind : 'info',
    title: text(opts.title),
    message: text(opts.message),
  };
  if (!state.title && !state.message) {
    throw new TypeError(
      'inlineBanner: `message` (or at least `title`) is required — a banner ' +
        'with no words is a coloured stripe.'
    );
  }

  const el = mk('div', 'df-banner df-banner--' + state.kind);
  el.setAttribute('role', BANNER_KINDS[state.kind].role);

  let iconEl = resolveIcon(opts.icon, BANNER_KINDS[state.kind].icon, 'df-banner__icon');
  el.appendChild(iconEl);

  const body = mk('div', 'df-banner__body');
  const titleEl = mk('p', 'df-banner__title');
  const messageEl = mk('p', 'df-banner__text');
  body.appendChild(titleEl);
  body.appendChild(messageEl);
  el.appendChild(body);

  // Kept even when empty: it holds the grid column.
  const actionsEl = mk('div', 'df-banner__actions');
  for (const action of collectActions(opts)) {
    const btn = actionButton(action, 'secondary');
    if (btn) actionsEl.appendChild(btn);
  }

  let dismissed = false;

  function dismiss() {
    if (dismissed) return;
    dismissed = true;
    if (el.parentNode) el.parentNode.removeChild(el);
    if (typeof opts.onDismiss === 'function') opts.onDismiss();
  }

  if (opts.dismissible) {
    const close = mk('button', 'df-icon-btn');
    close.type = 'button';
    close.appendChild(icon('close'));
    close.appendChild(mk('span', 'df-visually-hidden', text(opts.dismissLabel) || 'Dismiss this message'));
    close.addEventListener('click', dismiss);
    actionsEl.appendChild(close);
  }
  el.appendChild(actionsEl);

  function render() {
    setText(titleEl, state.title);
    setShown(titleEl, state.title !== '');
    setText(messageEl, state.message);
    setShown(messageEl, state.message !== '');
  }
  render();

  return {
    el: el,

    update(patch) {
      const p = patch && typeof patch === 'object' ? patch : {};
      if (has(p, 'kind') && has(BANNER_KINDS, p.kind) && p.kind !== state.kind) {
        el.classList.remove('df-banner--' + state.kind);
        state.kind = p.kind;
        el.classList.add('df-banner--' + state.kind);
        el.setAttribute('role', BANNER_KINDS[state.kind].role);
        const next = icon(BANNER_KINDS[state.kind].icon, 'df-banner__icon');
        el.replaceChild(next, iconEl);
        iconEl = next;
      }
      if (has(p, 'title')) state.title = text(p.title);
      if (has(p, 'message')) state.message = text(p.message);
      render();
    },

    dismiss: dismiss,

    destroy() {
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

// ---------------------------------------------------------------------------
// The object the shell hands screens as `ctx.states`.
// ---------------------------------------------------------------------------

export default {
  loadingState,
  emptyState,
  errorState,
  skeletonRows,
  inlineBanner,
  progressFrom,
  formatBytes,
  formatDuration,
};
