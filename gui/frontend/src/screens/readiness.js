/* System check is an advisory utility, outside the three bundle stages.
 * Problems and remedies remain visible; healthy/optional checks and technical
 * commands live in Details. Native consent is required before an action runs,
 * and elevated actions remain copy-only. All backend text uses textContent.
 */

// The application's name and the name of the binary it drives are NOT the same
// word and neither is spelled out here: the UI says Debark, the wire says
// debark, and both live in exactly one constant so the eventual rename is a
// one-line change. Importing the shell for two constants is safe — shell.js
// has no module-scope side effects and loads screens dynamically, so the cycle
// resolves.
import { APP_NAME, CLI_BINARY } from '../shell/shell.js';
import { bindDisclosure } from '../shell/disclosure.js';

// ---------------------------------------------------------------------------
// Stable identifiers.
//
// These mirror internal/app/types.go, which mirrors internal/readiness. Both
// document them as stable precisely so the frontend may key its copy off them.
// ---------------------------------------------------------------------------

const CHECK_BUILD_ENVIRONMENT = 'build-environment';

/**
 * Rows that cannot be re-checked on their own.
 *
 * `ReadinessCheck.derived` says this now, so the frontend no longer keeps a
 * list of ids that has to be maintained in step with the engine. Exactly one
 * row is derived — `build-environment`, the answer to "is there any way at all
 * to run apt on this machine", computed from the apt, WSL and container rows —
 * and `Checker.RunOne` refuses it by name. Offering the control would be
 * offering a button whose only outcome is an error, and it is one of only two
 * BLOCKING rows, so it would be offered where it matters most and does
 * nothing. The cell says what actually refreshes it instead.
 *
 * The id constant survives only as the fallback for a backend older than the
 * field.
 */
function isDerived(check) {
  if (check && typeof check.derived === 'boolean') return check.derived;
  return !!check && check.id === CHECK_BUILD_ENVIRONMENT;
}

/** How long a running action may go quiet before we reassure the operator. */
const SLOW_ACTION_MS = 6000;

/**
 * How long to wait before showing a loading state on a cold screen.
 *
 * The whole report completes in roughly 230 ms because the checks run
 * concurrently, so a spinner drawn immediately is a flash and nothing else.
 * We only draw one if the report is genuinely slow — a wedged container socket
 * pushes it to the engine's five-second per-check ceiling, and that is the case
 * worth showing something for.
 */
const LOADING_DELAY_MS = 260;

// ---------------------------------------------------------------------------
// DOM helpers.
//
// Everything below builds real nodes. `innerHTML` appears nowhere in this file:
// backend text carries paths, usernames and command output.
// ---------------------------------------------------------------------------

const SVG_NS = 'http://www.w3.org/2000/svg';

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null && text !== '') node.textContent = String(text);
  return node;
}

function setText(node, text) {
  node.textContent = text === undefined || text === null ? '' : String(text);
}

/**
 * Hide or show a node.
 *
 * `node.hidden` alone, per design rule 8. This used to set `style.display` as
 * well, because components.css carried no global `[hidden]` rule and an author
 * `display: flex` on `.df-cluster` beat the user agent's. The library now
 * ships `[hidden] { display: none !important }` — one of only two `!important`
 * declarations in it, and there for exactly this reason — so the inline style
 * is not only unnecessary, it destroys the component's own layout mode and
 * makes showing the element again depend on remembering what it was.
 */
function show(node, visible) {
  node.hidden = !visible;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/**
 * Icon shapes, as data.
 *
 * The design system ships no icons on purpose — no icon font, no remote asset —
 * and states the expected shape per role: circle-tick for success, triangle for
 * warning, octagon-x for danger, circle-i for info. Those exact paths are taken
 * from design/gallery.html so this screen and the gallery cannot drift.
 *
 * The shapes matter more than the colours: rule 2 of the design system is that
 * no state is signalled by hue alone, and the measured deuteranopia numbers in
 * its README are why.
 */
const ICONS = {
  success: [
    { t: 'circle', cx: 12, cy: 12, r: 9 },
    { t: 'path', d: 'M8 12.4l2.6 2.6L16 9.6' },
  ],
  warning: [
    { t: 'path', d: 'M12 4.2L2.6 20.2h18.8z' },
    { t: 'path', d: 'M12 10v4.4' },
    { t: 'circle', cx: 12, cy: 17.6, r: 1, fill: 'currentColor', stroke: 'none' },
  ],
  danger: [
    { t: 'path', d: 'M8.3 3h7.4L21 8.3v7.4L15.7 21H8.3L3 15.7V8.3z' },
    { t: 'path', d: 'M15 9l-6 6M9 9l6 6' },
  ],
  info: [
    { t: 'circle', cx: 12, cy: 12, r: 9 },
    { t: 'path', d: 'M12 11.2V16' },
    { t: 'circle', cx: 12, cy: 7.9, r: 1, fill: 'currentColor', stroke: 'none' },
  ],
  pending: [
    { t: 'circle', cx: 12, cy: 12, r: 9 },
    { t: 'path', d: 'M12 6.8V12l3.4 2' },
  ],
  refresh: [
    { t: 'path', d: 'M20 12a8 8 0 1 1-2.6-5.9' },
    { t: 'path', d: 'M20 4v4.6h-4.6' },
  ],
  copy: [
    { t: 'rect', x: 9, y: 9, width: 11, height: 11, rx: 2 },
    { t: 'path', d: 'M5 15V5a2 2 0 0 1 2-2h8' },
  ],
  play: [{ t: 'path', d: 'M8 5.5v13l11-6.5z' }],
  close: [{ t: 'path', d: 'M6 6l12 12M18 6L6 18' }],
  shield: [
    { t: 'path', d: 'M12 3l7 3v5.5c0 4.2-2.9 7.6-7 9.5-4.1-1.9-7-5.3-7-9.5V6z' },
    { t: 'path', d: 'M12 10.5v3.2' },
    { t: 'circle', cx: 12, cy: 16.6, r: 1, fill: 'currentColor', stroke: 'none' },
  ],
  machine: [
    { t: 'rect', x: 3, y: 4, width: 18, height: 12, rx: 2 },
    { t: 'path', d: 'M8 20h8M12 16v4' },
  ],
};

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
  for (const shape of ICONS[name] || []) {
    const child = document.createElementNS(SVG_NS, shape.t);
    for (const key of Object.keys(shape)) {
      if (key !== 't') child.setAttribute(key, String(shape[key]));
    }
    svg.appendChild(child);
  }
  return svg;
}

/** A `.df-status` unit: icon shape, colour and a word. Never colour alone. */
function statusEl(variant, label, hiddenLabel) {
  const wrap = el('span', 'df-status df-status--' + variant);
  wrap.appendChild(icon(variant));
  wrap.appendChild(el('span', hiddenLabel ? 'df-visually-hidden' : null, label));
  return wrap;
}

function button(label, className, iconName) {
  const btn = el('button', 'df-btn ' + className);
  btn.type = 'button';
  if (iconName) btn.appendChild(icon(iconName, 'df-btn__icon'));
  btn.appendChild(el('span', null, label));
  return btn;
}

/**
 * The exact argv, verbatim, monospace and selectable.
 *
 * No `.df-panel` around it. `.df-log` paints an inset background and draws no
 * border of its own, so the wrapper was adding one bordered rectangle per
 * action row — up to eight of them on a machine with problems, inside a table
 * that was itself inside a card.
 */
function commandBlock(display) {
  const pre = el('pre', 'df-log');
  pre.tabIndex = 0;
  const line = el('span', 'df-log__line');
  setText(line, display);
  pre.appendChild(line);
  return { el: pre, line };
}

/**
 * Gate a control without taking it out of the tab order.
 *
 * `disabled` removes a control from the tab order entirely, so a keyboard or
 * screen-reader operator never meets it and is never told it exists, let alone
 * why it is unavailable. Every "you cannot do this yet" on this screen is a
 * concurrency gate — the backend refuses a second check while one is running —
 * and there are up to sixteen of them on a machine with problems.
 *
 * The library styles `.df-btn[aria-disabled="true"]` exactly like `:disabled`
 * and suppresses its hover and press states, so this costs nothing visually.
 * The click handler does the refusing.
 */
const GATE_REASON_ID = 'df-readiness-gate';

/** The one thing that gates every control on this screen. */
const RUNNING_REASON =
  'A check is already running. The backend refuses a second one, so wait for it to finish or cancel it.';

function setGated(btn, gated, reason) {
  btn.setAttribute('aria-disabled', gated ? 'true' : 'false');
  if (gated) {
    btn.setAttribute('aria-describedby', GATE_REASON_ID);
    btn.dataset.gateReason = reason || '';
  } else {
    btn.removeAttribute('aria-describedby');
    delete btn.dataset.gateReason;
  }
}

function isGated(btn) {
  return btn.getAttribute('aria-disabled') === 'true';
}

function disclosure(summaryText) {
  const details = el('details', 'df-disclosure');
  const summary = el('summary', 'df-summary', summaryText);
  const body = el('div', 'df-disclosure__body');
  details.appendChild(summary);
  details.appendChild(body);
  bindDisclosure(details);
  return { el: details, body, summary };
}

// ---------------------------------------------------------------------------
// Presentation rules.
// ---------------------------------------------------------------------------

/**
 * How one row is presented.
 *
 * `status` and `severity` are different questions and this is the one place
 * that combines them. A passing or skipped check is never coloured by the
 * severity it *would* have carried; only a problem is.
 *
 * The words are chosen so a workable machine does not read as a broken one.
 * "Limited" is the engine's `degraded`: it narrows what is possible without
 * stopping anything, and the operator may never reach the limit. "Optional" is
 * an info-severity problem — a missing signing key is a choice debark
 * supports, not a fault.
 */
function present(check, running) {
  if (running) return { variant: 'pending', label: 'Checking…' };
  switch (check.status) {
    case 'ok':
      return { variant: 'success', label: 'Ready' };
    case 'skipped':
      return { variant: 'info', label: 'Skipped' };
    default:
      break;
  }
  switch (check.severity) {
    case 'blocking':
      return { variant: 'danger', label: 'Blocking' };
    case 'degraded':
      return { variant: 'warning', label: 'Limited' };
    default:
      return { variant: 'info', label: 'Optional' };
  }
}

function problems(report, severity) {
  return (report.checks || []).filter((c) => c.status === 'problem' && c.severity === severity);
}

/**
 * Whether the "build somewhere else" advice earns its place at the top.
 *
 * `ReadinessReport.remote_builder` is structured data now and this screen
 * reads it. It used to pattern-match on `platform` plus a set of check ids and
 * reconstruct the product decision here, which meant a rewording of one row's
 * remedy could silently change when the advice appeared. It is a product
 * decision and it lives on the backend.
 */
function remoteBuilderAdvice(report) {
  const a = report && report.remote_builder;
  return a && (a.message || a.hint) ? a : null;
}

function plural(n, one, many) {
  return n === 1 ? one : many;
}

function formatWhen(iso) {
  if (!iso) return '';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '';
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (secs < 10) return 'just now';
  if (secs < 60) return secs + ' seconds ago';
  const mins = Math.round(secs / 60);
  if (mins < 60) return mins + ' ' + plural(mins, 'minute', 'minutes') + ' ago';
  const hours = Math.round(mins / 60);
  if (hours < 24) return hours + ' ' + plural(hours, 'hour', 'hours') + ' ago';
  return new Date(then).toLocaleString();
}

/** A plain-text rendering of the report, for the clipboard. */
function reportAsText(report) {
  const lines = [];
  lines.push(`${CLI_BINARY}-gui readiness report`);
  if (report.platform) lines.push('platform: ' + report.platform);
  if (report.checked_at) lines.push('checked:  ' + report.checked_at);
  lines.push('build:    ' + (report.can_build ? 'possible' : 'blocked'));
  lines.push('');
  for (const c of report.checks || []) {
    const p = present(c, false);
    lines.push('[' + p.label + '] ' + c.title);
    lines.push('  ' + c.summary);
    if (c.remedy) lines.push('  ' + c.remedy);
    if (c.action) lines.push('  $ ' + c.action.display + (c.action.elevated ? '   (needs elevation)' : ''));
    if (c.action && c.action.note) lines.push('  note: ' + c.action.note);
    if (c.detail) lines.push('  detail: ' + c.detail);
    lines.push('');
  }
  return lines.join('\n');
}

// ---------------------------------------------------------------------------
// Shared state renderings.
//
// `ctx.states` (`shell/states.js`) owns the loading / empty / error look so all
// nine screens agree, and its `loadingState` / `emptyState` / `errorState` are
// used exactly as it exports them — including the required `label`, `title` and
// `message`, which it throws without and is right to.
//
// Each call is guarded and falls back to the design system's own documented
// `.df-state` markup, for two reasons: a shared helper that throws must not
// take the first-run screen down with it, and this screen has to render in a
// harness that has no shell attached at all.
// ---------------------------------------------------------------------------

function buildState(kind, opts) {
  const variant = kind === 'error' ? ' df-state--error' : kind === 'loading' ? ' df-state--loading' : '';
  const wrap = el('div', 'df-state' + variant);
  if (kind === 'error') wrap.setAttribute('role', 'alert');
  if (kind === 'loading') {
    const spinner = el('span', 'df-spinner df-spinner--lg');
    spinner.setAttribute('aria-hidden', 'true');
    wrap.setAttribute('aria-busy', 'true');
    wrap.appendChild(spinner);
  } else {
    const glyph = icon(kind === 'error' ? 'danger' : 'info');
    glyph.setAttribute('class', 'df-state__icon');
    wrap.appendChild(glyph);
  }
  const title = el('span', 'df-state__title');
  setText(title, opts.title);
  wrap.appendChild(title);
  if (opts.message) {
    const text = el('p', 'df-state__text');
    setText(text, opts.message);
    wrap.appendChild(text);
  }
  const actions = el('div', 'df-state__actions');
  let n = 0;
  // `action` and `actions` are both accepted, the way ctx.states accepts them.
  const wanted = (opts.action ? [opts.action] : []).concat(opts.actions || []);
  for (const a of wanted) {
    if (!a || !a.label) continue;
    const btn = button(a.label, 'df-btn--' + (a.variant || (n === 0 ? 'primary' : 'secondary')) + ' df-btn--sm');
    btn.addEventListener('click', a.onClick);
    actions.appendChild(btn);
    n += 1;
  }
  show(actions, n > 0);
  wrap.appendChild(actions);
  if (opts.detail) {
    const slot = el('div', 'df-state__detail');
    const d = disclosure('What failed');
    const pre = el('pre', 'df-log');
    setText(pre, opts.detail);
    d.body.appendChild(pre);
    slot.appendChild(d.el);
    wrap.appendChild(slot);
  }
  return wrap;
}

function fromStates(ctx, fn, args, fallback) {
  const states = ctx && ctx.states;
  if (states && typeof states[fn] === 'function') {
    try {
      const handle = states[fn].apply(states, args);
      const node = handle && handle.el ? handle.el : handle;
      if (node && typeof node.nodeType === 'number') return node;
    } catch (err) {
      console.error('readiness: ctx.states.' + fn + ' failed', err);
    }
  }
  return fallback();
}

function loadingRegion(ctx, opts) {
  return fromStates(ctx, 'loadingState', [opts], () =>
    buildState('loading', {
      title: opts.label,
      message: opts.detail,
      actions: opts.onCancel ? [{ label: opts.cancelLabel || 'Cancel', onClick: opts.onCancel }] : [],
    })
  );
}

function emptyRegion(ctx, opts) {
  return fromStates(ctx, 'emptyState', [opts], () => buildState('empty', opts));
}

function errorRegion(ctx, opts) {
  const uerr = opts.error || {};
  return fromStates(ctx, 'errorState', [opts], () =>
    buildState('error', {
      title: uerr.message || 'The checks could not run.',
      message: uerr.hint || '',
      detail: uerr.details || '',
      actions: opts.onRetry ? [{ label: opts.retryLabel || 'Try again', onClick: opts.onRetry }] : [],
    })
  );
}

/**
 * A UIError rendered whole: the sentence, the next action, the argv that
 * failed and the raw evidence. Never an exit code on its own — that is a
 * project definition-of-done item.
 */
function errorBanner(uerr, onCopy) {
  const banner = el('div', 'df-banner df-banner--danger');
  banner.setAttribute('role', 'alert');
  const glyph = icon('danger');
  glyph.setAttribute('class', 'df-banner__icon');
  banner.appendChild(glyph);

  const body = el('div', 'df-banner__body');
  const title = el('p', 'df-banner__title');
  setText(title, uerr.message || 'Something went wrong.');
  body.appendChild(title);
  if (uerr.hint) {
    const hint = el('p', 'df-banner__text');
    setText(hint, uerr.hint);
    body.appendChild(hint);
  }
  if (uerr.command && uerr.command.length) {
    const cmd = commandBlock(uerr.command.join(' '));
    body.appendChild(cmd.el);
  }
  if (uerr.details) {
    const d = disclosure('What the command said');
    const pre = el('pre', 'df-log');
    setText(pre, uerr.details);
    d.body.appendChild(pre);
    body.appendChild(d.el);
  }
  banner.appendChild(body);

  const actions = el('div', 'df-banner__actions');
  if (uerr.command && uerr.command.length && onCopy) {
    const copy = button('Copy command', 'df-btn--secondary df-btn--sm', 'copy');
    copy.addEventListener('click', () => onCopy(uerr.command.join(' ')));
    actions.appendChild(copy);
  }
  banner.appendChild(actions);
  return banner;
}

// ---------------------------------------------------------------------------
// One row.
//
// The row is built once and updated in place. Re-rendering by replacement
// would move focus out from under a keyboard operator every time a check
// finished, and "I started Docker, check again" is the most-used control here.
// ---------------------------------------------------------------------------

function createRow(id, handlers) {
  const tr = el('article', 'df-readiness-check');
  tr.setAttribute('data-check-id', id);
  // Programmatically focusable but not in the tab order: the headline's "Show
  // me" has to land somewhere even on a blocking row that offers no control,
  // and build-environment is exactly such a row.
  tr.setAttribute('tabindex', '-1');

  const header = el('div', 'df-readiness-check__header');
  const statusCell = el('span');
  const labelCell = el('h3');
  const detailCell = el('div');
  const actionCell = el('div');
  header.append(statusCell, labelCell, actionCell);
  tr.append(header, detailCell);

  const body = el('div', 'df-stack df-stack--tight');
  detailCell.appendChild(body);

  // Summary and remedy are two sentences on purpose: one says what is true,
  // the other says what to do. Merging them states neither clearly.
  const summaryLine = el('div');
  const summaryText = el('strong');
  summaryLine.appendChild(summaryText);
  body.appendChild(summaryLine);

  const remedyLine = el('div');
  body.appendChild(remedyLine);

  // --- the action block: badge, exact argv, note, buttons -------------------
  const actionBlock = el('div', 'df-stack df-stack--tight');
  const badges = el('div', 'df-cluster');
  const elevatedBadge = el('span', 'df-badge df-badge--warning', 'Needs elevation');
  badges.appendChild(elevatedBadge);
  actionBlock.appendChild(badges);

  const command = commandBlock('');
  actionBlock.appendChild(command.el);

  const elevationNote = el('div', 'df-text-sm');
  actionBlock.appendChild(elevationNote);

  // The note gets a banner rather than a line of small print. An operator who
  // runs usermod, sees nothing change and concludes it failed is the exact
  // outcome this sentence exists to prevent, so it is given weight.
  const noteLine = el('div', 'df-banner df-banner--info');
  noteLine.setAttribute('role', 'status');
  const noteIcon = icon('info');
  noteIcon.setAttribute('class', 'df-banner__icon');
  noteLine.appendChild(noteIcon);
  const noteBody = el('div', 'df-banner__body');
  const noteText = el('p', 'df-banner__text');
  noteBody.appendChild(noteText);
  noteLine.appendChild(noteBody);
  noteLine.appendChild(el('div', 'df-banner__actions'));
  actionBlock.appendChild(noteLine);

  const actionButtons = el('div', 'df-cluster');
  const copyBtn = button('Copy command', 'df-btn--secondary df-btn--sm', 'copy');
  const runBtn = button('Run', 'df-btn--secondary df-btn--sm', 'play');
  actionButtons.appendChild(copyBtn);
  actionButtons.appendChild(runBtn);
  actionBlock.appendChild(actionButtons);
  body.appendChild(actionBlock);

  // --- running feedback -----------------------------------------------------
  const progressBlock = el('div', 'df-stack df-stack--tight');
  const progressLabel = el('div', 'df-progress-label');
  const progressText = el('span', null, 'Working…');
  progressLabel.appendChild(progressText);
  progressBlock.appendChild(progressLabel);
  const progressBar = el('div', 'df-progress df-progress--indeterminate');
  progressBar.setAttribute('role', 'progressbar');
  progressBar.setAttribute('aria-label', 'Working');
  progressBar.appendChild(el('div', 'df-progress__bar'));
  progressBlock.appendChild(progressBar);
  const slowLine = el('div', 'df-text-sm');
  progressBlock.appendChild(slowLine);
  const cancelRow = el('div', 'df-cluster');
  const cancelBtn = button('Cancel', 'df-btn--ghost df-btn--sm');
  cancelRow.appendChild(cancelBtn);
  progressBlock.appendChild(cancelRow);
  body.appendChild(progressBlock);

  // --- aftermath ------------------------------------------------------------
  const aftermath = el('div', 'df-text-sm');
  body.appendChild(aftermath);

  // --- evidence -------------------------------------------------------------
  const detail = disclosure('Details');
  const detailPre = el('pre', 'df-log');
  detail.body.append(actionBlock, detailPre);
  body.appendChild(detail.el);

  // --- the right-hand column: re-check, always in the same place ------------
  const actionCluster = el('div', 'df-cluster');
  const recheckBtn = button('Re-check', 'df-btn--secondary df-btn--sm', 'refresh');
  actionCluster.append(runBtn, recheckBtn);
  const derivedNote = el('span', 'df-text-sm', 'Derived');
  actionCluster.appendChild(derivedNote);
  actionCell.appendChild(actionCluster);

  // An aria-disabled button still receives clicks and Enter — that is the
  // trade for keeping it in the tab order — so each handler refuses.
  const guarded = (btn, fn) => btn.addEventListener('click', () => {
    if (isGated(btn)) { handlers.onRefused(btn.dataset.gateReason || ''); return; }
    fn();
  });
  guarded(copyBtn, () => handlers.onCopy(id));
  guarded(runBtn, () => handlers.onRun(id));
  guarded(recheckBtn, () => handlers.onRecheck(id));
  cancelBtn.addEventListener('click', () => handlers.onCancel(id));

  let slowTimer = 0;

  function stopSlowTimer() {
    if (slowTimer) {
      clearTimeout(slowTimer);
      slowTimer = 0;
    }
  }

  function update(check, opts) {
    const running = Boolean(check.running) || opts.runningCheck === check.id;
    // Every row's controls go quiet while anything is running: the backend
    // refuses a second concurrent check, so leaving them live would offer a
    // button whose only outcome is "a readiness check is already running".
    const busy = running || opts.busy;
    const p = present(check, running);

    clear(statusCell);
    statusCell.appendChild(statusEl(p.variant, p.label));

    setText(labelCell, check.title);
    setText(summaryText, check.summary);

    setText(remedyLine, check.remedy || '');
    show(remedyLine, Boolean(check.remedy));

    const action = check.action;
    show(actionBlock, Boolean(action));
    if (!action) show(runBtn, false);
    if (action) {
      show(elevatedBadge, Boolean(action.elevated));
      show(badges, Boolean(action.elevated));
      setText(command.line, action.display || (action.command || []).join(' '));

      // An elevated action says so *before* it runs, not after — and this
      // application never escalates. The binding layer refuses these outright,
      // so the honest rendering is the command plus the reason.
      if (action.elevated) {
        setText(
          elevationNote,
          'This needs administrator rights, so this application will not run it. ' +
            'Copy it into a terminal you have elevated yourself, then re-check this row.'
        );
        show(elevationNote, true);
      } else {
        show(elevationNote, false);
      }

      setText(noteText, action.note || '');
      show(noteLine, Boolean(action.note));

      setText(runBtn.lastChild, action.label || 'Run this command');
      runBtn.setAttribute('aria-label', (action.label || 'Run this command') + ' — ' + (action.display || ''));
      show(runBtn, Boolean(action.runnable) && !action.elevated);
      setGated(runBtn, busy || opts.editLocked, opts.editLocked ? opts.editReason : RUNNING_REASON);
      setGated(copyBtn, false);
      copyBtn.setAttribute('aria-label', 'Copy the command for ' + check.title);
    }

    show(progressBlock, running);
    if (running) {
      // The backend's message for a plain re-check is the bare word "Checking";
      // naming the row makes it a sentence rather than a status code.
      let label = opts.progressMessage || '';
      if (!label || label === 'Checking') label = 'Checking ' + check.title + '…';
      setText(progressText, label);
      progressBar.setAttribute('aria-label', label);
      if (!slowTimer) {
        setText(slowLine, '');
        slowTimer = setTimeout(() => {
          setText(
            slowLine,
            'Still running. Installers, wsl --install and a first container start take minutes rather ' +
              'than seconds; this window stays usable while it works, and Cancel stops waiting on it.'
          );
        }, SLOW_ACTION_MS);
      }
    } else {
      stopSlowTimer();
      setText(slowLine, '');
    }

    setText(aftermath, opts.aftermath || '');
    show(aftermath, Boolean(opts.aftermath));

    setText(detailPre, check.detail || '');
    show(detail.el, Boolean(check.detail || check.action));
    show(detailPre, Boolean(check.detail));
    detail.summary.setAttribute('aria-label', 'Details for ' + check.title);

    const derived = isDerived(check);
    show(recheckBtn, !derived);
    show(derivedNote, derived);
    if (derived) {
      derivedNote.setAttribute(
        'title',
        'This row is worked out from the rows above it. Re-check one of those and it follows.'
      );
    }
    setGated(recheckBtn, busy, RUNNING_REASON);
    recheckBtn.setAttribute('aria-label', 'Re-check ' + check.title);
  }

  return {
    el: tr,
    update,
    destroy: stopSlowTimer,
    focus(scroll) {
      const target = [runBtn, copyBtn, recheckBtn].find((b) => !b.hidden && !isGated(b));
      if (target && detail.el.contains(target)) detail.el.open = true;
      (target || tr).focus();
      if (scroll) tr.scrollIntoView({ block: 'center' });
    },
  };
}

// ---------------------------------------------------------------------------
// The screen.
// ---------------------------------------------------------------------------

function createScreen() {
  let ctx = null;
  let root = null;
  let mounted = false;
  let visible = false;
  let returnTo = 'target';
  let focusCheck = '';
  let lifecycle = null;
  let lifecycleEpoch = 0;
  let lifecycleRequest = 0;
  let refreshRequest = 0;
  let reportEpoch = 0;

  /** The last report we rendered. Never mutated in place. */
  let report = emptyReport();
  /** Set while a full pass or a single row is in flight. */
  let progressMessage = '';
  /** A per-row sentence shown after an action finished. */
  const aftermath = new Map();
  let lastError = null;
  let cancelled = false;
  /** The check the consent dialog is currently asking about. */
  let pendingConsent = null;
  /** Where focus goes when it closes, and the row to fall back to. */
  let consentReturnFocus = null;
  let consentReturnID = '';

  let loadingTimer = 0;
  let clockTimer = 0;
  /** True once a full pass has finished at least once. */
  let started = false;
  /**
   * True while a pass is expected but the backend has not confirmed it yet.
   *
   * Without it the screen renders "this machine has not been checked yet" for
   * the one frame between `show()` and `readiness:started`, which is a flash of
   * the wrong answer on every cold start — worse than the spinner flash the
   * delay exists to avoid.
   */
  let starting = false;

  const rows = new Map();

  // DOM handles, filled by mount().
  const dom = {};

  function emptyReport() {
    return { checks: [], can_build: false, blocking_count: 0, degraded_count: 0, checking: false, duration_ms: 0 };
  }

  // --- backend plumbing -----------------------------------------------------

  function bindings() {
    return ctx && ctx.bindings;
  }

  async function call(name, ...args) {
    const b = bindings();
    if (!b || typeof b[name] !== 'function') {
      return {
        ok: false,
        error: {
          code: 'app.no_binding',
          message: `${APP_NAME} did not expose ` + name + '.',
          hint: 'Restart the application. If it happens again this build is incomplete — report it.',
        },
      };
    }
    try {
      const res = await b[name](...args);
      return res || { ok: false, error: { code: 'app.bridge', message: 'Debark returned no result.', hint: 'Try again.' } };
    } catch (err) {
      return {
        ok: false,
        error: {
          code: 'app.bridge',
          message: `${APP_NAME} did not answer.`,
          hint: `Try again. If it keeps happening, restart ${APP_NAME}.`,
          details: String(err),
        },
      };
    }
  }

  function toast(kind, message) {
    if (ctx && typeof ctx.toast === 'function') ctx.toast(kind, message);
  }

  function announce(message) {
    if (dom.live) setText(dom.live, message);
  }

  async function copyText(text) {
    const res = await call('CopyToClipboard', text);
    if (res && res.ok === false) {
      lastError = res.error;
      render();
      toast('warning', 'The clipboard could not be written. Select the command and copy it by hand.');
      return;
    }
    toast('success', 'Copied to clipboard.');
    announce('Copied to the clipboard.');
  }

  // --- actions --------------------------------------------------------------

  function checkById(id) {
    return (report.checks || []).find((c) => c.id === id);
  }

  function onCopy(id) {
    const check = checkById(id);
    if (!check || !check.action) return;
    copyText(check.action.display || (check.action.command || []).join(' '));
  }

  async function onRecheck(id) {
    if (report.checking || report.running_check || starting) { onRefused(RUNNING_REASON); return; }
    const check = checkById(id);
    if (!check) return;
    aftermath.delete(id);
    lastError = null;
    cancelled = false;
    progressMessage = 'Checking ' + check.title + '…';
    setRunning(id);
    const res = await call('RecheckReadiness', id);
    if (res && res.ok === false) failedStart(res.error, id);
  }

  function onRun(id) {
    if (editLocked()) { onRefused(editReason()); return; }
    if (report.checking || report.running_check || starting) { onRefused(RUNNING_REASON); return; }
    const check = checkById(id);
    if (!check || !check.action || !check.action.runnable || check.action.elevated) return;
    openConsent(check);
  }

  async function runConsented(check) {
    if (editLocked()) { onRefused(editReason()); return; }
    if (check.action?.elevated || !check.action?.runnable) return;
    aftermath.delete(check.id);
    lastError = null;
    cancelled = false;
    progressMessage = check.action.label || 'Running the command';
    setRunning(check.id);
    const res = await call('RunReadinessAction', check.id);
    if (res && res.ok === false) failedStart(res.error, check.id);
  }

  async function onCancel() {
    const result = await call('CancelReadinessCheck');
    if (result.error) { lastError = result.error; render(); }
  }

  /**
   * A gated control was pressed anyway.
   *
   * Rewrite the live region so it is re-announced — an unchanged live region
   * says nothing — and move focus to the control that will unblock it, which
   * here is always Cancel.
   */
  function onRefused(reason) {
    const why = reason || RUNNING_REASON;
    // An unchanged live region says nothing, so it is emptied and rewritten.
    if (dom.gate) {
      setText(dom.gate, '');
      setText(dom.gate, why);
    }
    toast('info', why);
    if (dom.cancelAll && !dom.cancelAll.hidden) {
      try { dom.cancelAll.focus(); } catch (e) { /* nothing to focus is not fatal */ }
    }
  }

  async function onRecheckAll() {
    if (report.checking || report.running_check || starting) { onRefused(RUNNING_REASON); return; }
    aftermath.clear();
    lastError = null;
    cancelled = false;
    progressMessage = 'Checking this machine…';
    starting = true;
    refreshLifecycle();
    report = Object.assign({}, report, { checking: true, running_check: '' });
    render();
    const res = await call('StartReadinessCheck');
    if (res && res.ok === false) failedStart(res.error, '');
  }

  function setRunning(id) {
    report = Object.assign({}, report, {
      running_check: id,
      checks: (report.checks || []).map((c) => (c.id === id ? Object.assign({}, c, { running: true }) : c)),
    });
    render();
  }

  function failedStart(uerr, id) {
    // The call was refused before any work began, so nothing is running.
    starting = false;
    report = Object.assign({}, report, {
      checking: false,
      running_check: '',
      checks: (report.checks || []).map((c) => (c.running ? Object.assign({}, c, { running: false }) : c)),
    });
    lastError = uerr || null;
    if (id) aftermath.delete(id);
    render();
    if (uerr && uerr.message) announce(uerr.message);
  }

  // --- consent dialog -------------------------------------------------------
  //
  // The engine never runs an action and the binding layer refuses every
  // elevated one, so the only commands that can reach here are unprivileged.
  // They still get an explicit yes: this audience installs a desktop tool on
  // the machine that builds their air-gapped media, and a program that runs
  // commands on its own initiative is the thing they will uninstall.

  function openConsent(check) {
    const action = check.action;
    setText(dom.consentTitle, 'Run this command?');
    clear(dom.consentBody);

    const stack = el('div', 'df-stack df-stack--tight');
    const lead = el('p');
    setText(lead, check.title + ' — ' + (action.label || 'run the command below'));
    stack.appendChild(lead);

    stack.appendChild(commandBlock(action.display || (action.command || []).join(' ')).el);

    const explain = el('p', 'df-text-sm');
    setText(
      explain,
      'It runs exactly as shown, as you, without administrator rights. ' +
        'Nothing else on this screen runs until it finishes, and you can cancel it.'
    );
    stack.appendChild(explain);

    if (action.note) {
      const note = el('div', 'df-banner df-banner--info');
      note.setAttribute('role', 'status');
      const glyph = icon('info');
      glyph.setAttribute('class', 'df-banner__icon');
      note.appendChild(glyph);
      const body = el('div', 'df-banner__body');
      const text = el('p', 'df-banner__text');
      setText(text, action.note);
      body.appendChild(text);
      note.appendChild(body);
      note.appendChild(el('div', 'df-banner__actions'));
      stack.appendChild(note);
    }

    dom.consentBody.appendChild(stack);
    setText(dom.consentRun.lastChild, action.label || 'Run it');
    pendingConsent = check;
    // Remember the trigger ourselves: the engine restores focus on close only
    // while the previously focused element is still focusable, and a finished
    // pass replaces every row underneath an open dialog.
    consentReturnFocus = document.activeElement;
    consentReturnID = check.id;

    if (typeof dom.consent.showModal === 'function') {
      // Focus trapping and Escape-to-close come free with the native element.
      dom.consent.showModal();
    } else {
      // An older system WebKitGTK without <dialog> loses the modality, not the
      // consent step: the card renders in flow, keyboard-reachable, and Escape
      // is wired by hand. Degrading the presentation is acceptable here;
      // degrading to "run it anyway" is not.
      dom.consent.setAttribute('open', '');
      document.addEventListener('keydown', onFallbackKey, true);
      dom.consent.scrollIntoView({ block: 'center' });
    }
    // The dismissive action, never the one that acts. ARIA practices, and the
    // right default when a program is asking to run a command on your machine.
    dom.consentCancel.focus();
  }

  function onFallbackKey(ev) {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      closeConsent();
    }
  }

  function closeConsent() {
    pendingConsent = null;
    if (!dom.consent) return;
    document.removeEventListener('keydown', onFallbackKey, true);
    if (typeof dom.consent.close === 'function' && dom.consent.open) {
      dom.consent.close();   // the `close` listener restores focus
      return;
    }
    dom.consent.removeAttribute('open');
    restoreConsentFocus();
  }

  // --- events ---------------------------------------------------------------

  function onStarted(payload) {
    reportEpoch++;
    cancelled = false;
    lastError = null;
    starting = true;
    progressMessage = 'Checking this machine…';
    report = Object.assign({}, report, (payload && payload.checks ? payload : {}), { checking: true });
    scheduleLoading();
    render();
  }

  function onProgress(payload) {
    reportEpoch++;
    if (!payload) return;
    if (payload.message) progressMessage = payload.message;
    if (payload.check_id) {
      report = Object.assign({}, report, {
        running_check: payload.done >= payload.total ? '' : payload.check_id,
      });
    }
    render();
  }

  function onFinished(payload) {
    reportEpoch++;
    if (!payload) return;
    // A confirmation must close when the thing it asks about is over. "Run
    // this command?" for a check that has just been re-run — or has vanished
    // from the report entirely — is worse than no confirmation: pressing Run
    // would act on a row that is no longer the one being described.
    if (pendingConsent && (!payload.check_id || payload.check_id === pendingConsent.id)) {
      closeConsent();
    }
    clearLoadingTimer();
    started = true;
    starting = false;
    cancelled = Boolean(payload.cancelled);
    lastError = payload.error || null;

    const next = payload.report || report;
    // Note what an action's aftermath was before we replace the rows, so the
    // usermod case — the command succeeded, the row still says no — reads as
    // "log out and back in", not as a failure.
    if (payload.check_id && payload.action) {
      const after = (next.checks || []).find((c) => c.id === payload.check_id);
      if (payload.cancelled) {
        aftermath.set(payload.check_id, 'The command was cancelled. Check its details before trying again.');
      } else if (payload.error) {
        aftermath.set(payload.check_id, 'The command did not finish cleanly. The message is below.');
      } else if (after && after.status === 'problem') {
        aftermath.set(
          payload.check_id,
          'The command finished, but this check still reports a problem. Some fixes only take effect in a ' +
            'new session or after a restart — try that, then re-check this row.'
        );
      } else {
        // The row itself has just turned green, which says it better than a
        // sentence would.
        aftermath.delete(payload.check_id);
        toast('success', 'Fixed.');
      }
    }

    report = Object.assign({}, next, { checking: false, running_check: '' });
    report.checks = (report.checks || []).map((c) => (c.running ? Object.assign({}, c, { running: false }) : c));
    if (payload.action && payload.check_id === 'signing-key' && !payload.error && !payload.cancelled) {
      const instruction = 'Key created. In Bundle, use Choose key and select the key file shown in this command.';
      aftermath.set('signing-key', instruction);
      toast('success', instruction);
    }
    progressMessage = '';
    render();

    if (payload.cancelled) {
      announce('The check was cancelled.');
      return;
    }
    if (payload.check_id) {
      const after = checkById(payload.check_id);
      if (after) {
        const p = present(after, false);
        announce(after.title + ': ' + p.label + '. ' + after.summary);
      }
    } else {
      announce(headline(report).announcement);
    }
  }

  function scheduleLoading() {
    // Only a cold screen ever shows a loading state, and only if the report is
    // genuinely slow. ~230 ms is the normal cost of a whole pass.
    if ((report.checks || []).length > 0) return;
    clearLoadingTimer();
    loadingTimer = setTimeout(() => {
      loadingTimer = 0;
      render();
    }, LOADING_DELAY_MS);
  }

  function clearLoadingTimer() {
    if (loadingTimer) {
      clearTimeout(loadingTimer);
      loadingTimer = 0;
    }
  }

  // --- headline -------------------------------------------------------------

  /**
   * The one sentence at the top.
   *
   * This is where the severity hierarchy is either told honestly or flattened
   * into a wall. A machine with no Docker and no signing key is *workable*, and
   * the headline says so first and counts the caveats second. Only a genuine
   * blocker gets the danger treatment.
   */
  function validLifecycle() { return lifecycle && ['build_running', 'export_running', 'stopping'].every(key => typeof lifecycle[key] === 'boolean'); }
  function editLocked() { return !validLifecycle() || Boolean(lifecycle.build_running || lifecycle.export_running || lifecycle.stopping); }
  function editReason() {
    if (!validLifecycle()) return 'Checking whether actions can run. Recheck the system if this persists.';
    return lifecycle.stopping ? 'Debark is stopping the current job.' : 'System changes are unavailable while a build or copy is running.';
  }
  function refreshLifecycle() {
    const epoch = lifecycleEpoch;
    const request = ++lifecycleRequest;
    call('LifecycleStatus').then(status => { if (mounted && epoch === lifecycleEpoch && request === lifecycleRequest) { lifecycle = status; render(); } });
  }
  function headline(rep) {
    const count = problems(rep, 'blocking').length;
    const text = count ? `${count} ${count === 1 ? 'host problem needs' : 'host problems need'} attention before building.` : 'No blocking host problems found.';
    return { announcement: text, title: text };
  }
  function render() {
    if (!mounted) return;
    const checks = report.checks || [];
    const inFlight = Boolean(report.checking) || starting;
    const busy = inFlight || Boolean(report.running_check);
    setText(dom.headline, checks.length ? headline(report).title : '');
    show(dom.headline, checks.length > 0 && !inFlight);
    setText(dom.toolbarStatus, busy ? progressMessage || 'Checking this machine...' : report.checked_at ? 'Checked ' + formatWhen(report.checked_at) : 'Not checked yet');
    setText(dom.reportMetadata, [report.platform, report.duration_ms ? report.duration_ms + ' ms' : '', report.checked_at].filter(Boolean).join(' / '));
    show(dom.toolbarSpinner, busy);
    setGated(dom.recheckAll, busy, RUNNING_REASON);
    setText(dom.gate, busy ? RUNNING_REASON : editLocked() ? editReason() : '');
    setText(dom.jobStatus, editLocked() ? editReason() : '');
    show(dom.jobStatus, editLocked());
    show(dom.cancelAll, busy);
    setGated(dom.copyReport, checks.length === 0, 'Run a system check before copying the report.');
    clear(dom.errorSlot);
    if (lastError) dom.errorSlot.appendChild(errorBanner(lastError, copyText));
    else if (cancelled) dom.errorSlot.appendChild(el('p', 'df-text-secondary', 'The check was cancelled. Recheck when ready.'));
    if (lifecycle?.error) dom.errorSlot.appendChild(errorBanner(lifecycle.error, copyText));
    show(dom.errorSlot, Boolean(lastError || cancelled || lifecycle?.error));
    clear(dom.stateSlot);
    if (!checks.length) {
      show(dom.tableWrap, false);
      show(dom.otherDetails.el, false);
      show(dom.stateSlot, true);
      if (inFlight && loadingTimer) show(dom.stateSlot, false);
      else if (inFlight) dom.stateSlot.appendChild(loadingRegion(ctx, { label: 'Checking this machine', detail: 'Looking for build tools, a signing key and available space.' }));
      else if (!lastError) dom.stateSlot.appendChild(emptyRegion(ctx, { kind: 'nothing-yet', title: started ? 'No system checks were returned.' : 'No system check yet.', message: 'Use Recheck to check this machine.' }));
    } else { show(dom.stateSlot, false); renderRows(checks); }
    renderRemoteBuilder(remoteBuilderAdvice(report));
    setText(dom.footerStatus, '');
    setText(dom.continueBtn.lastChild, 'Back');
    dom.continueBtn.className = 'df-btn df-btn--ghost df-actionbar__back';
  }
  function renderRows(checks) {
    const busy = Boolean(report.checking) || starting || Boolean(report.running_check);
    const ids = new Set(checks.map((check) => check.id));
    for (const [id, row] of rows) { if (!ids.has(id)) { row.destroy(); row.el.remove(); rows.delete(id); } }
    let problemsCount = 0, otherCount = 0;
    for (const check of checks) {
      let row = rows.get(check.id);
      if (!row) { row = createRow(check.id, { onCopy, onRun, onRecheck, onCancel, onRefused }); rows.set(check.id, row); }
      const attention = check.running || report.running_check === check.id || (check.status === 'problem' && check.severity !== 'info');
      const parent = attention ? dom.tbody : dom.otherRows;
      if (attention) problemsCount++; else otherCount++;
      if (row.el.parentNode !== parent) {
        if (!attention && row.el.contains(document.activeElement)) dom.otherDetails.el.open = true;
        parent.appendChild(row.el);
      }
      row.update(check, { busy, editLocked: editLocked(), editReason: editReason(), runningCheck: report.running_check || '', progressMessage, aftermath: aftermath.get(check.id) || '' });
    }
    show(dom.tableWrap, problemsCount > 0);
    show(dom.otherDetails.el, otherCount > 0);
    setText(dom.otherDetails.summary, `Details: healthy and optional checks (${otherCount})`);
    if (focusCheck && rows.has(focusCheck)) {
      const row = rows.get(focusCheck);
      if (dom.otherRows.contains(row.el)) dom.otherDetails.el.open = true;
      if (visible) { row.focus(true); focusCheck = ''; }
    }
  }

  // --- mount ----------------------------------------------------------------

  function mountInto(mountRoot, screenCtx) {
    root = mountRoot; ctx = screenCtx;
    const layout = el('section', 'df-screen-layout');
    const content = el('div', 'df-screen-content');
    const column = el('div', 'df-app__content'); column.tabIndex = -1;
    const stack = el('div', 'df-stack');
    column.appendChild(stack); content.appendChild(column); layout.appendChild(content); root.appendChild(layout); dom.page = column;
    dom.live = el('div', 'df-live'); dom.live.setAttribute('role', 'status'); dom.live.setAttribute('aria-live', 'polite'); stack.appendChild(dom.live);
    dom.gate = el('p', 'df-live'); dom.gate.id = GATE_REASON_ID; dom.gate.setAttribute('role', 'status'); stack.appendChild(dom.gate);
    stack.appendChild(el('h1', null, 'System check'));
    stack.appendChild(el('p', 'df-text-secondary', 'These checks describe this builder. Each target still has its own build requirements.'));
    dom.headline = el('p', 'df-text-secondary'); stack.appendChild(dom.headline);
    dom.jobStatus = el('p', 'df-text-secondary'); stack.appendChild(dom.jobStatus);
    dom.errorSlot = el('div'); stack.appendChild(dom.errorSlot);
    dom.alternative = buildRemoteBuilderPanel(); stack.appendChild(dom.alternative);
    dom.tableWrap = el('div', 'df-boxed-list'); dom.tbody = dom.tableWrap; stack.appendChild(dom.tableWrap);
    dom.otherDetails = disclosure('Details: healthy and optional checks');
    dom.reportMetadata = el('p', 'df-text-sm df-text-secondary'); dom.otherDetails.body.appendChild(dom.reportMetadata);
    dom.otherRows = el('div', 'df-boxed-list'); dom.otherDetails.body.appendChild(dom.otherRows); stack.appendChild(dom.otherDetails.el);
    dom.stateSlot = el('div'); stack.appendChild(dom.stateSlot);
    dom.toolbarStatus = el('p', 'df-text-sm df-text-secondary'); stack.appendChild(dom.toolbarStatus);
    const footer = el('div', 'df-actionbar');
    dom.continueBtn = button('Back', 'df-btn--ghost df-actionbar__back');
    dom.continueBtn.addEventListener('click', () => ctx.go(returnTo)); footer.appendChild(dom.continueBtn);
    dom.footerStatus = el('span', 'df-actionbar__status'); footer.appendChild(dom.footerStatus);
    dom.toolbarSpinner = el('span', 'df-spinner'); dom.toolbarSpinner.setAttribute('aria-hidden', 'true'); footer.appendChild(dom.toolbarSpinner);
    dom.copyReport = button('Copy report', 'df-btn--ghost', 'copy');
    dom.copyReport.addEventListener('click', () => { if (isGated(dom.copyReport)) { onRefused(dom.copyReport.dataset.gateReason); return; } copyText(reportAsText(report)); }); footer.appendChild(dom.copyReport);
    dom.cancelAll = button('Cancel', 'df-btn--ghost'); dom.cancelAll.addEventListener('click', onCancel); footer.appendChild(dom.cancelAll);
    dom.recheckAll = button('Recheck', 'df-btn--secondary', 'refresh');
    dom.recheckAll.addEventListener('click', () => { if (isGated(dom.recheckAll)) { onRefused(dom.recheckAll.dataset.gateReason); return; } onRecheckAll(); }); footer.appendChild(dom.recheckAll);
    layout.appendChild(footer); root.appendChild(buildConsentDialog());
    ctx.on('readiness:started', onStarted); ctx.on('readiness:progress', onProgress); ctx.on('readiness:finished', onFinished);
    ctx.on('app:lifecycle', (status) => { lifecycleEpoch++; lifecycle = status; if (editLocked()) closeConsent(); render(); });
    mounted = true; render();
  }

  function buildConsentDialog() {
    const dialog = el('dialog', 'df-dialog');
    // A <dialog> gets role="dialog" from the engine but NO name unless one is
    // given: without aria-labelledby a screen reader announces "dialog" and
    // nothing else, which is exactly the moment the operator most needs to
    // know what they are being asked.
    dialog.setAttribute('aria-labelledby', 'df-readiness-consent-title');
    dialog.setAttribute('aria-describedby', 'df-readiness-consent-body');
    const inner = el('div', 'df-dialog__inner');
    const header = el('div', 'df-dialog__header');
    dom.consentTitle = el('span', 'df-dialog__title', 'Run this command?');
    dom.consentTitle.id = 'df-readiness-consent-title';
    header.appendChild(dom.consentTitle);
    const close = el('button', 'df-dialog__close');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close without running anything');
    close.appendChild(icon('close'));
    close.addEventListener('click', closeConsent);
    header.appendChild(close);
    inner.appendChild(header);

    dom.consentBody = el('div', 'df-dialog__body');
    dom.consentBody.id = 'df-readiness-consent-body';
    inner.appendChild(dom.consentBody);

    const footer = el('div', 'df-dialog__footer');
    const cancel = button('Cancel', 'df-btn--ghost');
    cancel.addEventListener('click', closeConsent);
    dom.consentCancel = cancel;
    footer.appendChild(cancel);
    dom.consentRun = button('Run it', 'df-btn--primary', 'play');
    dom.consentRun.addEventListener('click', () => {
      const check = pendingConsent;
      closeConsent();
      if (check) runConsented(check);
    });
    footer.appendChild(dom.consentRun);
    inner.appendChild(footer);

    dialog.appendChild(inner);
    dialog.addEventListener('cancel', () => closeConsent());
    // `close` is the one path every dismissal goes through, including Escape,
    // which the engine handles without telling us. Focus restoration has to
    // live here or Escape drops it on <body>.
    dialog.addEventListener('close', restoreConsentFocus);
    dom.consent = dialog;
    return dialog;
  }

  /**
   * Put focus back somewhere real after the consent dialog closes.
   *
   * The row that opened it may have been rebuilt underneath — a finished pass
   * replaces every row — so the fallback walks to the row for that check id,
   * then to the screen's own column. Never <body>.
   */
  function restoreConsentFocus() {
    const target = consentReturnFocus;
    const id = consentReturnID;
    consentReturnFocus = null;
    consentReturnID = '';
    if (!target && !id) return;
    const usable = (n) => !!(n && n.isConnected && !n.hidden && !n.disabled &&
      n.getAttribute && n.getAttribute('aria-disabled') !== 'true' && !n.closest('[hidden]'));
    if (usable(target)) {
      try { target.focus(); return; } catch (_) { /* fall through */ }
    }
    const row = id && rows.get(id);
    if (row) { row.focus(false); return; }
    try { if (dom.page) dom.page.focus(); } catch (_) { /* nothing left to focus */ }
  }

  /**
   * "Build on a Linux machine you can reach."
   *
   * Rendered from `ReadinessReport.remote_builder` — the message, the hint and
   * the rows that led there — rather than from this screen's own reading of
   * the platform and a set of check ids. Given real weight rather than a
   * footnote, because on a managed Windows laptop where WSL and a container
   * runtime are both blocked by policy it is the only route that will ever
   * work. A bundle is an ordinary folder: there is nothing special about the
   * machine that made it.
   *
   * A group, not a `.df-panel` card: the title is plain text above one
   * seamless box.
   */
  function buildRemoteBuilderPanel() {
    const section = el('section', 'df-group');
    section.setAttribute('aria-labelledby', 'df-readiness-remote-title');

    const head = el('div', 'df-cluster');
    const glyph = icon('machine');
    glyph.setAttribute('class', 'df-banner__icon');
    head.appendChild(glyph);
    const title = el('h2', 'df-group__title', 'You can build this on another machine');
    title.id = 'df-readiness-remote-title';
    head.appendChild(title);
    section.appendChild(head);

    dom.remoteMessage = el('p', 'df-group__description');
    section.appendChild(dom.remoteMessage);

    const box = el('div', 'df-boxed-list');

    dom.remoteHintRow = el('div', 'df-boxed-list__row');
    const hintText = el('span', 'df-boxed-list__text');
    dom.remoteHint = el('span', 'df-boxed-list__label');
    hintText.appendChild(dom.remoteHint);
    dom.remoteHintRow.appendChild(hintText);
    box.appendChild(dom.remoteHintRow);

    dom.remoteBlockedFoot = el('div', 'df-boxed-list__footer');
    box.appendChild(dom.remoteBlockedFoot);
    section.appendChild(box);
    section.hidden = true;
    return section;
  }

  /** Fills the advice from the report, and points at the rows that led there. */
  function renderRemoteBuilder(advice) {
    show(dom.alternative, Boolean(advice));
    if (!advice) return;
    setText(dom.remoteMessage, advice.message || '');
    setText(dom.remoteHint, advice.hint || '');
    show(dom.remoteHintRow, Boolean(advice.hint));

    clear(dom.remoteBlockedFoot);
    const blocked = (advice.blocked || []).filter((id) => rows.has(id));
    show(dom.remoteBlockedFoot, blocked.length > 0);
    if (!blocked.length) return;
    dom.remoteBlockedFoot.appendChild(
      el('span', null, blocked.length === 1 ? 'One local option is blocked:' : blocked.length + ' local options are blocked:')
    );
    for (const id of blocked) {
      const check = checkById(id);
      const jump = button((check && check.title) || id, 'df-btn--ghost df-btn--sm');
      jump.addEventListener('click', () => {
        const row = rows.get(id);
        if (row) row.focus(true);
      });
      dom.remoteBlockedFoot.appendChild(jump);
    }
  }

  // --- lifecycle ------------------------------------------------------------

  async function refreshFromBackend() {
    const request = ++refreshRequest, epoch = reportEpoch;
    const b = bindings();
    // No screen-level banner for a missing backend. The shell raises that at
    // application level, and this screen used to raise it again as a banner
    // AND as an error state, so an operator with no runtime attached read the
    // same sentence three times. Render the empty screen and let the shell say
    // it once.
    if (!b || typeof b.Readiness !== 'function') {
      render();
      return;
    }
    let current = null;
    try {
      current = await b.Readiness();
    } catch (err) {
      lastError = {
        code: 'app.bridge',
        message: `${APP_NAME} did not answer.`,
        hint: `Try again. If it keeps happening, restart ${APP_NAME}.`,
        details: String(err),
      };
      render();
      return;
    }
    if (!mounted || request !== refreshRequest || epoch !== reportEpoch) return;
    if (current) {
      report = current;
      lastError = current.error || null;
      started = Boolean(current.checked_at);
      if (started || (current.checks || []).length > 0) starting = Boolean(current.checking);
      if (report.checking) scheduleLoading();
      render();
    }
    // Readiness() never probes, so an empty report is the cue to run a pass —
    // unless the shell already started one at boot, which is the usual case.
    if (!started && !cancelled && (!current || (!current.checked_at && !current.checking && !current.running_check && !(current.checks || []).length))) {
      progressMessage = 'Checking this machine…';
      starting = true;
      scheduleLoading();
      render();
      const res = await call('StartReadinessCheck');
      if (res && res.ok === false && res.error && res.error.code !== 'app.busy') failedStart(res.error, '');
    }
  }

  return {
    id: 'readiness',
    title: 'System check',

    mount(mountRoot, screenCtx) {
      if (mounted) return;
      mountInto(mountRoot, screenCtx);
    },

    show(screenCtx, params) {
      if (screenCtx) ctx = screenCtx;
      if (params?.returnTo && params.returnTo !== 'readiness') returnTo = params.returnTo;
      focusCheck = params?.focusCheck || '';
      refreshLifecycle();
      visible = true;
      // A cold screen is one frame away from a pass, either the shell's or the
      // one refreshFromBackend is about to ask for. Rendering "not checked yet"
      // in the meantime would be a flash of the wrong answer.
      if (!started && (report.checks || []).length === 0) {
        starting = true;
        scheduleLoading();
      }
      render();
      // Cheap and idempotent: Readiness() never probes.
      refreshFromBackend();
      if (!clockTimer) {
        clockTimer = setInterval(() => {
          if (visible && mounted && !report.checking) render();
        }, 30000);
      }
    },

    hide() {
      visible = false;
      if (clockTimer) {
        clearInterval(clockTimer);
        clockTimer = 0;
      }
      closeConsent();
    },

    destroy() {
      visible = false;
      mounted = false;
      refreshRequest++;
      starting = false;
      clearLoadingTimer();
      if (clockTimer) {
        clearInterval(clockTimer);
        clockTimer = 0;
      }
      for (const row of rows.values()) row.destroy();
      rows.clear();
      aftermath.clear();
      if (dom.consent) closeConsent();
      if (root) clear(root);
      root = null;
      ctx = null;
    },
  };
}

export default createScreen();
