/* Compact native-framed desktop shell. Three stages, contextual utilities,
 * scoped screen lifecycles, authoritative background-job status and safe text.
 * The screen owns its bounded content and single bottom action area.
 */

import { bindDisclosure } from './disclosure.js';
import { buildFulfilsDraft, acceptSelectionSummary, createTerminalNoticeTracker } from './job-state.js';

import { createEventBus } from './bus.js';
import * as statesModule from './states.js';
import * as themeModule from './theme.js';

/* ==========================================================================
   Names.

   The product is Debark. The command-line tool it drives is debark. Those
   are two different names for two different things and the UI must not blur
   them: every sentence the operator reads says Debark, and every place the UI
   names the process it is about to run — contract rule 8 — says debark,
   because that is what they would type.

   Holding the binary name here means the day the CLI is renamed, this is the
   line that changes. Screens may import both.
   ========================================================================== */

/** The product name. Every operator-visible string uses this. */
export const APP_NAME = 'Debark';

/**
 * The command-line binary this application invokes. NOT a display name — it is
 * the literal argv[0] an operator would type, and it appears in error hints and
 * in every command preview.
 */
export const CLI_BINARY = 'debark';

/* ==========================================================================
   The frozen event surface — internal/app/events.go, EventNames().
   The shell subscribes to all names once and fans out to scoped listeners.
   ========================================================================== */

export const EVENT_NAMES = Object.freeze([
  'readiness:started',
  'readiness:progress',
  'readiness:finished',
  'target:changed',
  'catalog:started',
  'catalog:progress',
  'catalog:finished',
  'selection:changed',
  'build:started',
  'build:progress',
  'build:event',
  'build:finished',
  'export:started',
  'export:progress',
  'export:finished',
  'verify:started',
  'verify:finished',
  'app:error',
  'app:lifecycle',
]);

/* Internal routes remain stable; copy shares the visible Bundle stage. */

const SCREENS = [
  {
    id: 'readiness',
    title: 'System check',
    navLabel: 'System check',
    aside: true, // beside the stepper, not a step in it — see above
    load: () => import('../screens/readiness.js'),
  },
  {
    id: 'target',
    title: 'Choose a target',
    navLabel: 'Target',
    step: 1,
    load: () => import('../screens/target.js'),
  },
  {
    id: 'picker',
    title: 'Choose packages',
    navLabel: 'Packages',
    step: 2,
    needsTarget: true,
    lockedReason: 'Choose a target first. The catalogue is built from its own apt indexes.',
    // picker is three files by ownership but one screen; picker-list.js exports
    // the screen module and composes the other two.
    load: () => import('../screens/picker-list.js'),
  },
  {
    id: 'build',
    title: 'Review bundle',
    navLabel: 'Bundle',
    step: 3,
    needsTarget: true,
    lockedReason: 'Choose a target first, then pick the packages to include.',
    load: () => import('../screens/build.js'),
  },
  {
    id: 'export',
    title: 'Copy bundle',
    navLabel: 'Bundle',
    stage: 'build',
    load: () => import('../screens/export.js'),
  },
];

const STEPS = SCREENS.filter((s) => typeof s.step === 'number').sort((a, b) => a.step - b.step);

const TOAST_KINDS = new Set(['info', 'success', 'warning', 'danger']);
const HISTORY_MAX = 50;
/** Pending toasts. The DOM holds one; this is how many may wait behind it. */
const TOAST_QUEUE_MAX = 4;
const BANNERS_MAX = 4;

/* ==========================================================================
   Small DOM helpers. No framework, no template engine: dynamic text always
   goes in through textContent, static SVG through innerHTML on an element we
   just created.
   ========================================================================== */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

/**
 * svg builds one of the library's line icons. `strokeWidth` is an argument
 * because the tick glyph in a 20px stepper marker needs 3 to read at all,
 * while everything else is 2 — the gallery's own markup says so.
 */
function svg(markup, className, strokeWidth) {
  const holder = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  holder.setAttribute('viewBox', '0 0 24 24');
  holder.setAttribute('fill', 'none');
  holder.setAttribute('stroke', 'currentColor');
  holder.setAttribute('stroke-width', String(strokeWidth || 2));
  holder.setAttribute('stroke-linecap', 'round');
  holder.setAttribute('stroke-linejoin', 'round');
  holder.setAttribute('aria-hidden', 'true');
  if (className) holder.setAttribute('class', className);
  holder.innerHTML = markup;
  return holder;
}

/*
 * The icon shapes. Colour is never the only signal, so each of these is a
 * distinct outline: circle-i, circle-tick, triangle, octagon-x, padlock, tick.
 * The four alert shapes are copied from gallery.html so the toast and the
 * banner draw the same glyph as the design system's own examples.
 */
const ICON = {
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5"/><path d="M12 7.6v.1"/>',
  success: '<circle cx="12" cy="12" r="9"/><path d="M8 12.4l2.6 2.6L16 9.6"/>',
  warning: '<path d="M12 3.6l9 15.8H3z"/><path d="M12 9.6v4.2"/><path d="M12 16.6v.1"/>',
  danger: '<path d="M8.3 3h7.4L21 8.3v7.4L15.7 21H8.3L3 15.7V8.3z"/><path d="M15 9l-6 6M9 9l6 6"/>',
  close: '<path d="M6 6l12 12M18 6L6 18"/>',
  back: '<path d="M15 5l-7 7 7 7"/>',
  tick: '<path d="M5 12.6l4.5 4.5L19 7"/>',
  padlock:
    '<rect x="5" y="10.5" width="14" height="9.5" rx="1.6"/><path d="M8.4 10.5V7.8a3.6 3.6 0 017.2 0v2.7"/>',
};

/*
 * The Debark mark. ONE svg, not a light/dark pair: a single 32×32 drawing whose
 * camel has `fill` and `stroke` both `currentColor`, so it follows the theme
 * through `.df-brand-mark`, carrying a crate whose `rect` is the one pinned
 * part — filled with `var(--color-brand-amber)` through an inline `style`
 * attribute. `gallery.html` carries the exact same markup, and `BRAND_MARK`
 * below is that markup. Design README §6.22.
 *
 * `--color-brand-*` may appear here, in `.df-brand` and in `.df-about`, and
 * nowhere else in the application.
 */
const BRAND_MARK =
  '<g fill="currentColor" stroke="currentColor" stroke-width="1" stroke-linejoin="round" stroke-linecap="round">' +
  '<path d="M7.3 13.4 L4.9 17.3" fill="none" stroke-width="1.15"/>' +
  '<path d="M23.4 5.4 L22.9 3.5 L24.6 4.7 Z"/>' +
  '<path d="M7.4 13.6 C7.6 12.0 8.6 11.4 9.7 11.4 C10.1 6.9 11.5 4.6 13.6 4.6 C15.7 4.6 17.1 6.9 17.5 11.4 ' +
  'L18.5 11.5 C18.9 9.6 19.9 7.8 21.5 6.4 C22.2 5.8 23.1 5.4 24.0 5.4 L27.6 5.4 C28.6 5.4 29.0 6.0 29.0 6.6 ' +
  'C29.0 7.2 28.6 7.6 27.8 7.7 L25.2 8.1 C24.4 8.2 23.8 8.6 23.3 9.2 C22.3 10.5 21.7 12.3 21.5 14.6 ' +
  'L21.5 26.2 L19.5 26.2 L19.5 17.0 L12.4 17.0 L12.4 26.2 L10.4 26.2 L10.4 16.8 C8.7 16.5 7.4 15.4 7.4 13.9 Z"/>' +
  '</g>' +
  '<g stroke="currentColor" stroke-width="1" stroke-linejoin="round" stroke-linecap="round">' +
  '<rect x="10" y="4.2" width="8" height="4.8" rx="1.1" style="fill:var(--color-brand-amber)"/>' +
  '<path d="M14 4.2 L14 9"/>' +
  '</g>';

function brandMark(doc, { className = 'df-brand-mark', decorative = false } = {}) {
  const node = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
  node.setAttribute('class', className);
  node.setAttribute('viewBox', '0 0 32 32');
  if (decorative) {
    // Inside a button that already carries "About Debark", naming the mark as
    // well would read the identity out twice.
    node.setAttribute('aria-hidden', 'true');
  } else {
    // role="img" + a name: an unnamed logo is the one image in the application
    // worth naming.
    node.setAttribute('role', 'img');
    node.setAttribute('aria-label', APP_NAME);
  }
  node.innerHTML = BRAND_MARK;
  return node;
}

function normaliseKind(kind) {
  return TOAST_KINDS.has(kind) ? kind : 'info';
}

const NOOP = () => {};

/* ==========================================================================
   Bindings.

   The generated wrapper at ../wailsjs/go/app/App.js is produced by
   `wails generate module`; Wails injects the same object at window.go.app.App
   at runtime regardless, and the screen contract names that object, so that is
   what we resolve. When it is genuinely absent — a plain browser, a build where
   binding did not run — we install a stub that answers in the frozen error
   shape rather than throwing, so the app still boots and says why it cannot do
   anything.
   ========================================================================== */

const NO_BINDINGS_ERROR = Object.freeze({
  code: 'app.no_runtime',
  message: 'The desktop runtime is not attached, so the application cannot reach its backend.',
  hint: 'Run the packaged application rather than opening index.html directly. If this is a development build, run `wails dev` or `wails build` so the Go bindings are generated.',
  retryable: false,
});

function missingBindings(onCall) {
  const answer = () => ({ ok: false, error: { ...NO_BINDINGS_ERROR } });
  return new Proxy(Object.create(null), {
    get(_t, prop) {
      if (typeof prop !== 'string') return undefined;
      if (prop === 'then') return undefined; // never look thenable to await
      return (...args) => {
        if (onCall) onCall(prop, args);
        return Promise.resolve(answer());
      };
    },
    has: () => true,
  });
}

/**
 * resolveBindings waits briefly for Wails to install window.go.app.App.
 * Bounded, because a wait that never ends is the same bug as a white screen.
 */
function resolveBindings({ timeoutMS = 750, win = window } = {}) {
  const found = () => win.go && win.go.app && win.go.app.App;
  if (found()) return Promise.resolve(found());
  return new Promise((resolve) => {
    const deadline = Date.now() + timeoutMS;
    const tick = () => {
      const app = found();
      if (app) return resolve(app);
      if (Date.now() >= deadline) return resolve(null);
      win.setTimeout(tick, 25);
      return undefined;
    };
    win.setTimeout(tick, 0);
  });
}

/* ==========================================================================
   Theme and states adapters.

   `ctx.states` and `ctx.theme` are passed through exactly as their owning
   modules export them; the shell never wraps or reinterprets them. The only
   thing the shell needs from theme.js is a control to put in the header slot.
   ========================================================================== */

function unwrapModule(mod) {
  if (!mod) return null;
  return mod.default != null ? mod.default : mod;
}

/* ==========================================================================
   createShell
   ========================================================================== */

export function createShell(options = {}) {
  const doc = options.document || document;
  const win = options.window || window;
  const mountPoint = options.root || doc.getElementById('app') || doc.body;
  const debug = Boolean(options.debug);

  const states = unwrapModule(statesModule);
  const theme = unwrapModule(themeModule);

  /* --- diagnostics ---------------------------------------------------- */

  function report(err, where) {
    // console is the only sink. There is no crash reporting in this tree and
    // there never will be — rule 4.
    // eslint-disable-next-line no-console
    console.error(`[shell] ${where}:`, err);
  }

  function trace(...args) {
    // eslint-disable-next-line no-console
    if (debug) console.debug('[shell]', ...args);
  }

  /** True when the engine has been asked to stop animating. */
  function reducedMotion() {
    try {
      return Boolean(win.matchMedia && win.matchMedia('(prefers-reduced-motion: reduce)').matches);
    } catch (_) {
      return false;
    }
  }

  /* --- state ---------------------------------------------------------- */

  const registry = new Map();
  for (const def of SCREENS) {
    registry.set(def.id, {
      def,
      module: null, // the screen module, once imported
      pane: null, // the <div> the shell owns and the screen builds in
      scope: null, // its bus scope
      ctx: null,
      mounted: false,
      failed: false,
      visible: false,
    });
  }

  const shellState = {
    target: null, // TargetView
    appInfo: null,
    readiness: null,
    /* What the stepper needs to mark a step complete. All three are
       presentation only: nothing here changes what is reachable. */
    selectionTotal: 0,
    buildDone: false,
    selectionRevision: null,
    selectionKeyRevision: null,
    buildStatus: null,
    exportStatus: null,
    lifecycle: null,
    currentID: null,
    started: false,
    destroyed: false,
  };
  const terminalNotices = createTerminalNoticeTracker();

  /** @type {{id: string, params: any}[]} */
  const history = [];
  let navToken = 0;

  let bindings = missingBindings((method) => trace('call to missing bindings:', method));
  let bindingsReady = false;
  let bus = null;
  let shellScope = null;

  /* --- frame ---------------------------------------------------------- */

  const nodes = {};

  /**
   * buildFrame paints the window's own layout. Every class here is the design
   * system's — §6.24 for the frame, §6.19 for the stepper, §6.22 for the brand.
   * This function must never inject a stylesheet; if a rule is missing, that is
   * a report to the design work, not a `<style>` element.
   *
   *   .df-app
   *     a.df-app__skip
   *     header.df-headerbar.df-brand.df-headerbar--brand
   *       svg.df-brand-mark   span.df-wordmark
   *       .df-spacer   nav.df-stepper   .df-spacer
   *       target context   job return   Main menu
   *     .df-app__banners
   *     main#main.df-app__main
   *       .df-app__pane × n          .df-toast-host
   */
  function buildFrame() {
    const app = el('div', 'df-app');

    // Skip link — three lines, and it is the difference between a keyboard
    // operator reaching the package list in one key and in eleven.
    const skip = el('a', 'df-btn df-btn--secondary df-btn--sm df-app__skip', 'Skip to content');
    skip.href = '#main';
    skip.addEventListener('click', (ev) => {
      ev.preventDefault();
      focusCurrentPane();
    });
    app.appendChild(skip);

    const header = el('header', 'df-headerbar df-brand df-headerbar--brand df-desktop-toolbar');
    const identity = el('div', 'df-desktop-toolbar__identity');
    const brandBtn = el('button', 'df-btn df-btn--ghost');
    brandBtn.type = 'button';
    brandBtn.setAttribute('aria-label', `About ${APP_NAME}`);
    brandBtn.appendChild(brandMark(doc, { decorative: true }));
    brandBtn.appendChild(el('span', 'df-wordmark', APP_NAME));
    brandBtn.addEventListener('click', () => openAbout());
    const status = el('span', 'df-desktop-toolbar__target');
    identity.append(brandBtn, status);
    header.appendChild(identity);

    const stepper = el('nav', 'df-stepper');
    stepper.setAttribute('aria-label', 'Bundle stages');
    const stepperList = el('ol', 'df-stepper__list');
    stepper.appendChild(stepperList);
    header.appendChild(stepper);

    const utilities = el('div', 'df-desktop-toolbar__utilities');
    const jobButton = el('button', 'df-btn df-btn--ghost df-btn--sm df-desktop-toolbar__job');
    jobButton.type = 'button';
    jobButton.hidden = true;
    jobButton.addEventListener('click', () => go(activeJobRoute()));
    utilities.appendChild(jobButton);

    const menuAnchor = el('div', 'df-menu-anchor');
    const menuButton = el('button', 'df-btn df-btn--ghost df-btn--sm', 'Menu');
    menuButton.type = 'button';
    menuButton.setAttribute('aria-label', 'Main menu');
    menuButton.setAttribute('aria-haspopup', 'menu');
    menuButton.setAttribute('aria-expanded', 'false');
    menuButton.setAttribute('aria-controls', 'main-menu');
    const menu = el('div', 'df-menu');
    menu.id = 'main-menu';
    menu.setAttribute('role', 'menu');
    menu.setAttribute('aria-label', 'Main menu');
    menu.hidden = true;
    menuAnchor.append(menuButton, menu);
    utilities.appendChild(menuAnchor);
    header.appendChild(utilities);
    app.appendChild(header);

    /* banners — the global error channel lives here */
    const banners = el('div', 'df-app__banners');
    banners.id = 'shell-banners';
    app.appendChild(banners);

    /* screens */
    const main = el('main', 'df-app__main');
    main.id = 'main';
    app.appendChild(main);

    // The toast host is a child of .df-app__main, which the library makes
    // position: relative, so the pill rises from the content area and never
    // covers the action bar. It holds AT MOST ONE .df-toast.
    const toastHost = el('div', 'df-toast-host');
    main.appendChild(toastHost);

    /* Two live regions, doing two different jobs.

       `announcer` says where the operator now is, on every navigation.
       `gate` says why a refused control refused — §6.23 requires the reason to
       be rewritten on every refusal, because an unchanged live region says
       nothing at all the second time. */
    const announcer = el('div', 'df-visually-hidden');
    announcer.setAttribute('aria-live', 'polite');
    announcer.setAttribute('role', 'status');
    app.appendChild(announcer);

    const gate = el('div', 'df-live');
    gate.setAttribute('aria-live', 'polite');
    gate.setAttribute('role', 'status');
    app.appendChild(gate);

    mountPoint.textContent = '';
    mountPoint.appendChild(app);

    Object.assign(nodes, {
      app,
      header,
      brandBtn,
      stepper,
      stepperList,
      status,
      jobButton,
      menuAnchor,
      menuButton,
      menu,
      banners,
      main,
      toastHost,
      announcer,
      gate,
    });

    buildStepper();
    watchHeaderWidth();
    buildMenu();
    renderStatus();
    renderStepper();
    renderReadinessControl();
  }

  /* --- the stepper ----------------------------------------------------- */

  function buildMenu() {
    const addItem = (label, action) => {
      const button = el('button', 'df-btn df-btn--ghost df-menu__item', label);
      button.type = 'button';
      button.tabIndex = -1;
      button.setAttribute('role', 'menuitem');
      button.addEventListener('click', () => { closeMenu(true); action(); });
      nodes.menu.appendChild(button);
      return button;
    };
    addItem('Copy existing bundle…', openExistingCopy);
    const readinessBtn = addItem('System check', () => go('readiness', { returnTo: shellState.currentID || 'target' }));
    const readinessIcon = svg(ICON.info, 'df-btn__icon');
    const readinessNote = el('span', 'df-visually-hidden');
    readinessBtn.prepend(readinessIcon);
    readinessBtn.appendChild(readinessNote);
    Object.assign(nodes, { readinessBtn, readinessIcon, readinessNote });
    addItem('Appearance', () => openUtilityDialog('Appearance', (body) => {
      const label = el('p', 'df-text-secondary', 'Follow the system appearance, or choose a theme.');
      body.appendChild(label);
      if (!nodes.themeController) nodes.themeController = themeModule.createThemeController({ label: 'Appearance' });
      body.appendChild(nodes.themeController.el);
    }));
    addItem('Keyboard shortcuts', openShortcuts);
    addItem(`About ${APP_NAME}`, openAbout);
    addItem(`Quit ${APP_NAME}`, requestClose);
    nodes.menuButton.addEventListener('click', () => nodes.menu.hidden ? openMenu() : closeMenu(true));
    nodes.menuButton.addEventListener('keydown', (event) => {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        openMenu(event.key === 'ArrowUp');
      }
    });
    nodes.menu.addEventListener('keydown', (event) => {
      const buttons = Array.from(nodes.menu.querySelectorAll('[role="menuitem"]'));
      const current = buttons.indexOf(doc.activeElement);
      let next;
      if (event.key === 'ArrowDown') next = (current + 1) % buttons.length;
      if (event.key === 'ArrowUp') next = (current + buttons.length - 1) % buttons.length;
      if (event.key === 'Home') next = 0;
      if (event.key === 'End') next = buttons.length - 1;
      if (next !== undefined) { event.preventDefault(); buttons[next].focus(); }
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); closeMenu(true); }
      if (event.key === 'Tab') closeMenu(true);
    });
    doc.addEventListener('pointerdown', onMenuOutside);
  }

  function onMenuOutside(event) {
    if (!nodes.menuAnchor.contains(event.target)) closeMenu(false);
  }

  function openMenu(last = false) {
    nodes.menu.hidden = false;
    nodes.menuButton.setAttribute('aria-expanded', 'true');
    const items = nodes.menu.querySelectorAll('[role="menuitem"]');
    items[last ? items.length - 1 : 0]?.focus();
  }

  function closeMenu(restoreFocus = false) {
    if (!nodes.menu || nodes.menu.hidden) return;
    nodes.menu.hidden = true;
    nodes.menuButton.setAttribute('aria-expanded', 'false');
    if (restoreFocus) nodes.menuButton.focus({ preventScroll: true });
  }

  async function openExistingCopy() {
    if (nodes.copyOpening) return;
    nodes.copyOpening = true;
    const opener = shellState.currentID || 'target';
    try {
      const status = await bindings.ExportStatus();
      if (status?.error && typeof status.running !== 'boolean') {
        showError(status.error);
        return;
      }
      if (status?.running) await go('export');
      else await go('export', { sourceMode: 'choose', returnTo: opener });
    } catch (error) {
      toast('danger', error?.message || 'Could not check the current copy. Try again.');
    } finally { nodes.copyOpening = false; }
  }

  async function requestClose() {
    try {
      const result = await bindings.RequestClose();
      if (result?.error) showError(result.error);
    } catch (error) { report(error, 'RequestClose'); }
  }

  function openUtilityDialog(title, populate) {
    if (nodes.utilityDialog?.open) return;
    const dialog = el('dialog', 'df-dialog');
    dialog.setAttribute('aria-labelledby', 'utility-dialog-title');
    const inner = el('div', 'df-dialog__inner');
    const header = el('div', 'df-dialog__header');
    const heading = el('h2', 'df-dialog__title', title);
    heading.id = 'utility-dialog-title';
    header.appendChild(heading);
    const body = el('div', 'df-dialog__body');
    populate(body);
    const footer = el('div', 'df-dialog__footer');
    const close = el('button', 'df-btn df-btn--secondary', 'Close');
    close.type = 'button';
    close.addEventListener('click', () => dialog.close());
    footer.appendChild(close);
    inner.append(header, body, footer);
    dialog.appendChild(inner);
    dialog.addEventListener('close', () => {
      dialog.remove();
      nodes.utilityDialog = null;
      if (!shellState.destroyed) nodes.menuButton.focus({ preventScroll: true });
    }, { once: true });
    nodes.app.appendChild(dialog);
    nodes.utilityDialog = dialog;
    dialog.showModal();
    close.focus();
  }

  function openShortcuts() {
    openUtilityDialog('Keyboard shortcuts', (body) => {
      const list = el('dl', 'df-key-value');
      for (const [keys, action] of [
        ['Tab / Shift+Tab', 'Move between controls'],
        ['F10', 'Open Main menu'],
        ['Alt+Left', 'Go back'],
        ['Ctrl+W', 'Close Debark'],
        ['Space in packages', 'Select or remove a package'],
        ['Enter in packages', 'Open package details'],
        ['Escape', 'Close a menu or dialog'],
      ]) list.append(el('dt', null, keys), el('dd', null, action));
      body.appendChild(list);
    });
  }

  /**
   * buildStepper creates the three items once. Everything after this mutates in
   * place: rebuilding the list on every navigation would destroy focus, which
   * for a control the operator is tabbing through is a defect, not a redraw.
   *
   * Each item is
   *
   *   <li class="df-stepper__item">
   *     <button class="df-stepper__step" …>
   *       <span class="df-stepper__marker">…</span>
   *       <span class="df-stepper__label">Packages</span>
   *       <span class="df-visually-hidden">, completed</span>   (only when it is)
   *     </button>
   *     <span class="df-stepper__reason" role="tooltip">…</span> (only when locked)
   *   </li>
   *
   * The reason is a SIBLING of the button. As a child it would be swallowed into
   * the button's accessible name — "Build Choose at least one package first" —
   * and as an aria-describedby target it is announced even while it is off
   * screen, because the accessible-description computation includes a directly
   * referenced node regardless of its visibility.
   *
   * No roving tabindex: each of the three stages remains a Tab stop.
   * A compact stage list must keep every destination reachable, as a gated flow
   * needs. docs/accessibility.md §10 P3-13.
   */
  function buildStepper() {
    nodes.stepperList.textContent = '';
    nodes.steps = new Map();

    STEPS.forEach((def, index) => {
      const item = doc.createElement('li');
      item.className = 'df-stepper__item';

      const button = el('button', 'df-stepper__step');
      button.type = 'button';
      button.dataset.screen = def.id;

      const marker = el('span', 'df-stepper__marker');
      button.appendChild(marker);
      button.appendChild(el('span', 'df-stepper__label', def.navLabel));
      // Filled in only while the step is complete: `.is-complete` is a class,
      // and a class says nothing whatsoever to assistive technology.
      const completeNote = el('span', 'df-visually-hidden', '');
      button.appendChild(completeNote);
      // And the same treatment for "you are here", for the same reason one
      // step further along. `aria-current="step"` is correct ARIA and it does
      // reach AT-SPI — the attribute shows up on the accessible as
      // `current=page` — but Orca 46.1 on WebKit2GTK 2.52.6 does not say it.
      // Measured directly: standing on the target step, Orca announced
      // "1 Target push button" and nothing more, so the one thing a gated
      // stepper exists to tell an operator — where they are — was the one
      // thing only the picture carried. Same argument as `.is-complete`: the
      // attribute stays because it is right, and the sentence is added
      // because the attribute is not enough on the shipping stack.
      const currentNote = el('span', 'df-visually-hidden', '');
      button.appendChild(currentNote);

      const reason = el('span', 'df-stepper__reason');
      reason.id = `step-reason-${def.id}`;
      reason.setAttribute('role', 'tooltip');
      // The last item's tooltip would otherwise hang off the right of the
      // window; --end anchors it to the item's right edge instead.
      if (index === STEPS.length - 1) reason.classList.add('df-stepper__reason--end');
      reason.hidden = true;

      button.addEventListener('click', () => activateStep(def));

      item.appendChild(button);
      item.appendChild(reason);
      nodes.stepperList.appendChild(item);
      nodes.steps.set(def.id, { def, button, marker, completeNote, currentNote, reason });
    });
  }

  /** isLocked is the single answer to "may the operator go here yet". */
  function isLocked(def) {
    if (def?.id === 'build' && shellState.currentID === 'export') return false;
    if (!def || !def.needsTarget) return false;
    return !(shellState.target && shellState.target.selected);
  }

  /** isComplete drives the tick. Presentation only — never gates anything. */
  function isComplete(def) {
    switch (def.id) {
      case 'target':
        return Boolean(shellState.target && shellState.target.selected);
      case 'picker':
        return shellState.selectionTotal > 0;
      case 'build':
        return shellState.buildDone;
      default:
        return false;
    }
  }

  /**
   * activateStep is the refusal. It never uses `disabled`, so it is reached by
   * Tab and by click alike, and when it refuses it does the three things the
   * gating idiom requires: it does not navigate, it rewrites the live region so
   * the reason is announced again, and it moves focus to the control that will
   * unblock it — which for every locked step in this app is the Target step.
   */
  function activateStep(def) {
    if (def.id === 'build' && shellState.currentID === 'export') {
      go('export');
      return;
    }
    if (!isLocked(def)) {
      go(def.id);
      return;
    }
    const reason = def.lockedReason || 'This step is not available yet.';
    say(reason);
    const unblock = nodes.steps.get('target');
    if (unblock) {
      try {
        unblock.button.focus({ preventScroll: true });
      } catch (_) {
        unblock.button.focus();
      }
    }
  }

  /**
   * renderStepper reflects state onto the three stages. Each state carries at
   * least two signals so none of them depends on hue:
   *
   *   current    aria-current="step"    filled marker + semibold label
   *                                     + a visually-hidden ", current step"
   *   completed  .is-complete           tick glyph + tinted marker + ", completed"
   *   locked     aria-disabled="true"   padlock + muted label + dashed marker + reason
   *
   * The two hidden sentences are not belt-and-braces. `.is-complete` is a
   * class and says nothing to assistive technology at all; `aria-current` is
   * real ARIA and reaches AT-SPI, but Orca 46.1 on WebKit2GTK 2.52.6 does not
   * speak it. Both were measured on the shipping stack — see
   * docs/accessibility.md §5.
   */
  function renderStepper() {
    if (!nodes.steps) return;
    for (const def of STEPS) {
      const s = nodes.steps.get(def.id);
      if (!s) continue;

      const locked = isLocked(def);
      const complete = !locked && isComplete(def);
      const current = shellState.currentID === def.id || (def.id === 'build' && shellState.currentID === 'export');

      if (current) s.button.setAttribute('aria-current', 'step');
      else s.button.removeAttribute('aria-current');

      s.button.classList.toggle('is-complete', complete);
      s.button.setAttribute('aria-disabled', locked ? 'true' : 'false');

      // The marker is the step number, a tick, or a padlock — one element,
      // rewritten, so there is never a stale glyph underneath.
      s.marker.textContent = '';
      if (locked) {
        s.marker.appendChild(svg(ICON.padlock, 'df-stepper__glyph'));
      } else if (complete) {
        s.marker.appendChild(svg(ICON.tick, 'df-stepper__glyph', 3));
      } else {
        s.marker.textContent = String(def.step);
      }

      s.completeNote.textContent = complete ? ', completed' : '';
      // Read after ", completed" so a step that is both says
      // "Packages, completed, current step" in that order.
      s.currentNote.textContent = current ? ', current step' : '';

      if (locked) {
        s.reason.textContent = def.lockedReason || 'This step is not available yet.';
        s.reason.hidden = false;
        s.button.setAttribute('aria-describedby', s.reason.id);
      } else {
        // Hidden AND unreferenced: an empty tooltip box that appears on hover
        // is worse than no tooltip, and a description of "" is noise.
        s.reason.hidden = true;
        s.reason.textContent = '';
        s.button.removeAttribute('aria-describedby');
      }
    }
    scheduleCompactSync();
  }

  /**
   * The compact variant clips the labels off screen — a clip, not
   * `display: none`, because `display: none` would take the buttons'
   * accessible names with it. It is driven from a ResizeObserver rather than a
   * media query because what matters is whether these three labels fit next to
   * this wordmark and these utility controls, which no breakpoint can know.
   *
   * The observer watches `.df-app`, NOT the headerbar. Toggling the class
   * changes the headerbar's size, so observing the headerbar would feed its own
   * output back in and trip the engine's "ResizeObserver loop" guard; `.df-app`
   * is height:100% of a host that only the window resizes.
   */
  function watchHeaderWidth() {
    if (typeof win.ResizeObserver === 'function') {
      nodes.headerObserver = new win.ResizeObserver(() => scheduleCompactSync());
      try {
        nodes.headerObserver.observe(nodes.app);
      } catch (err) {
        report(err, 'observing the frame width');
      }
    } else {
      // Older WebKitGTK. One listener is a fair fallback for a control that
      // only ever changes on a window resize.
      nodes.onWinResize = () => scheduleCompactSync();
      win.addEventListener('resize', nodes.onWinResize);
    }
    scheduleCompactSync();
  }

  let compactPending = false;
  function scheduleCompactSync() {
    if (compactPending || shellState.destroyed) return;
    compactPending = true;
    const run = () => {
      compactPending = false;
      syncCompact();
    };
    if (typeof win.requestAnimationFrame === 'function') win.requestAnimationFrame(run);
    else win.setTimeout(run, 0);
  }

  /**
   * syncCompact measures rather than guesses. `.df-stepper__list` wraps, so a
   * stepper that does not fit gets taller instead of overflowing — comparing
   * the list's height against one step's height is therefore the honest test.
   *
   * Both branches run inside one frame, so the un-compacted measurement never
   * reaches the screen: `offsetHeight` forces layout, not a paint.
   */
  function syncCompact() {
    const list = nodes.stepperList;
    const stepper = nodes.stepper;
    if (!list || !stepper || !list.firstChild) return;
    const first = list.querySelector('.df-stepper__step');
    if (!first) return;

    stepper.classList.remove('df-stepper--compact');
    const rowHeight = first.offsetHeight;
    if (!rowHeight) return; // not laid out yet (display:none, detached)
    if (list.offsetHeight > rowHeight * 1.4) stepper.classList.add('df-stepper--compact');
  }

  /* --- about ------------------------------------------------------------ */

  /**
   * The about surface — the one place in this application allowed to look like
   * a poster, and the only place besides the headerbar and the mark where
   * `--color-brand-*` may appear (design README §6.22, §8.2).
   *
   * Built lazily, on first open: a dialog nobody has asked for should not be in
   * the DOM of a window budgeted at 1.5 s to interactive.
   *
   * The accessibility rules here are the ones docs/accessibility.md §10 found
   * broken elsewhere and would find broken again by default: a `<dialog>` gets
   * the dialog role from the engine and NO name, so `aria-labelledby` is not
   * optional; initial focus goes to the dismissive action; and focus is
   * restored from the `close` event, because Escape is the one path that does
   * not go through any button.
   */
  function buildAbout() {
    if (nodes.about) return nodes.about;

    const dialog = doc.createElement('dialog');
    dialog.className = 'df-dialog';
    dialog.id = 'about-dialog';
    dialog.setAttribute('aria-labelledby', 'about-dialog-title');

    const inner = el('div', 'df-dialog__inner');

    const head = el('div', 'df-dialog__header');
    const title = el('span', 'df-dialog__title', `About ${APP_NAME}`);
    title.id = 'about-dialog-title';
    head.appendChild(title);
    const closeIcon = el('button', 'df-dialog__close');
    closeIcon.type = 'button';
    closeIcon.setAttribute('aria-label', 'Close');
    closeIcon.appendChild(svg(ICON.close));
    closeIcon.addEventListener('click', () => closeAbout());
    head.appendChild(closeIcon);
    inner.appendChild(head);

    const body = el('div', 'df-dialog__body');
    const about = el('div', 'df-brand df-about');
    // The one rounding the library leaves to the caller, because .df-about is
    // also used full-bleed. gallery.html sets the same value the same way.
    about.style.borderRadius = 'var(--radius-lg)';
    about.appendChild(brandMark(doc, { className: 'df-brand-mark df-brand-mark--lg' }));
    about.appendChild(el('span', 'df-wordmark df-wordmark--lg', APP_NAME));
    const version = el('span', 'df-about__version', '');
    about.appendChild(version);
    about.appendChild(el('span', 'df-about__rule'));
    about.appendChild(
      el(
        'p',
        'df-about__text',
        'Builds an offline apt bundle from a target’s own indexes. No telemetry, no hosted service, and no network call to anything but the archives you named.',
      ),
    );
    body.appendChild(about);
    inner.appendChild(body);

    const foot = el('div', 'df-dialog__footer');
    const closeBtn = el('button', 'df-btn df-btn--secondary', 'Close');
    closeBtn.type = 'button';
    closeBtn.addEventListener('click', () => closeAbout());
    foot.appendChild(closeBtn);
    inner.appendChild(foot);

    dialog.appendChild(inner);
    // Restoring focus on `close` rather than in closeAbout() is deliberate:
    // Escape dismisses a modal dialog without going through any handler of
    // ours, and focus would otherwise be left on <body>.
    dialog.addEventListener('close', () => {
      const back = nodes.aboutOpener || nodes.brandBtn;
      if (back && doc.contains(back)) {
        try {
          back.focus({ preventScroll: true });
        } catch (_) {
          back.focus();
        }
      }
      nodes.aboutOpener = null;
    });

    nodes.app.appendChild(dialog);
    nodes.about = dialog;
    nodes.aboutVersion = version;
    nodes.aboutClose = closeBtn;
    renderAboutVersion();
    return dialog;
  }

  function renderAboutVersion() {
    if (!nodes.aboutVersion) return;
    const info = shellState.appInfo;
    const parts = [];
    parts.push(info && info.version ? info.version : 'development build');
    if (info && info.debark_version) parts.push(`${CLI_BINARY} ${info.debark_version}`);
    else parts.push(`${CLI_BINARY} not found`);
    if (info && info.platform) parts.push(`${info.platform}/${info.arch || ''}`.replace(/\/$/, ''));
    nodes.aboutVersion.textContent = parts.join(' · ');
  }

  function openAbout() {
    const dialog = buildAbout();
    renderAboutVersion();
    nodes.aboutOpener = doc.activeElement;
    if (typeof dialog.showModal === 'function') {
      dialog.showModal();
    } else {
      // No showModal: ::backdrop never renders, so the modifier paints one and
      // centres the dialog itself.
      dialog.classList.add('df-dialog--fallback');
      dialog.setAttribute('open', '');
    }
    // The dismissive action, never a destructive one — there is no destructive
    // one here, and this is the habit the rule exists to build.
    try {
      nodes.aboutClose.focus({ preventScroll: true });
    } catch (_) {
      nodes.aboutClose.focus();
    }
  }

  function closeAbout() {
    const dialog = nodes.about;
    if (!dialog) return;
    if (typeof dialog.close === 'function') {
      dialog.close();
    } else {
      dialog.removeAttribute('open');
      dialog.dispatchEvent(new Event('close'));
    }
  }

  /* --- the readiness health control ------------------------------------ */

  /**
   * Readiness is a health indicator, not a step — see the note on SCREENS. The
   * state is carried by the icon's SHAPE and by a visually-hidden sentence, so
   * it survives both a monochrome theme and a screen reader.
   */
  function renderReadinessControl() {
    const btn = nodes.readinessBtn;
    if (!btn) return;
    const rep = shellState.readiness;

    let shape = ICON.info;
    let note = '';

    if (rep && rep.checking) {
      note = ', checking';
    } else if (rep && Array.isArray(rep.checks) && rep.checks.length) {
      const blocking = Number(rep.blocking_count) || 0;
      const degraded = Number(rep.degraded_count) || 0;
      if (blocking > 0) {
        shape = ICON.danger;
        note = blocking === 1 ? ', one check is blocking a build' : `, ${blocking} checks are blocking a build`;
      } else if (degraded > 0) {
        shape = ICON.warning;
        note = degraded === 1 ? ', one check needs attention' : `, ${degraded} checks need attention`;
      } else {
        shape = ICON.success;
        note = ', all checks passed';
      }
    }

    btn.setAttribute('aria-busy', rep && rep.checking ? 'true' : 'false');
    nodes.readinessNote.textContent = note;

    const next = svg(shape, 'df-btn__icon');
    btn.replaceChild(next, nodes.readinessIcon);
    nodes.readinessIcon = next;

    if (shellState.currentID === 'readiness') btn.setAttribute('aria-current', 'page');
    else btn.removeAttribute('aria-current');
  }

  function renderStatus() {
    const target = shellState.target;
    nodes.status.textContent = target?.selected ? target.label || target.id || 'Target selected' : '';
    nodes.status.title = target?.selected ? target.label || target.id || '' : '';
    renderJobContext();
    renderAboutVersion();
  }

  function activeJobRoute() {
    if (shellState.exportStatus?.running || shellState.lifecycle?.export_running) return 'export';
    return 'build';
  }

  function renderJobContext() {
    const route = activeJobRoute();
    const status = route === 'export' ? shellState.exportStatus : shellState.buildStatus;
    const running = Boolean(status?.running || shellState.lifecycle?.build_running || shellState.lifecycle?.export_running);
    nodes.jobButton.hidden = !running || shellState.currentID === route;
    nodes.jobButton.textContent = shellState.lifecycle?.stopping ? 'Stopping…' : route === 'export' ? 'Return to copy' : 'Return to build';
    nodes.jobButton.title = status?.target_id || status?.bundle_path || '';
  }

  /* --- banners -------------------------------------------------------- */

  /** @type {Map<string, {node: Node}>} keyed so an error storm renders once */
  const banners = new Map();

  /**
   * banner renders a persistent notice in the frame. It is the global error
   * channel's surface: summary AND hint, always; the exit class and code live
   * inside the details drawer where they are supporting evidence rather than
   * the message.
   */
  function banner(spec) {
    const kind = normaliseKind(spec.kind);
    const key = spec.key || `${kind}:${spec.title || ''}:${spec.text || ''}`;
    if (banners.has(key)) return () => dismissBanner(key);

    const root = el('div', `df-banner df-banner--${kind}`);
    root.setAttribute('role', kind === 'danger' ? 'alert' : 'status');
    root.appendChild(svg(ICON[kind], 'df-banner__icon'));

    const body = el('div', 'df-banner__body');
    if (spec.title) body.appendChild(el('p', 'df-banner__title', spec.title));
    if (spec.text) body.appendChild(el('p', 'df-banner__text', spec.text));

    if (spec.details || (spec.command && spec.command.length)) {
      const details = doc.createElement('details');
      details.className = 'df-disclosure';
      const summary = doc.createElement('summary');
      summary.className = 'df-summary';
      summary.textContent = 'What failed';
      details.appendChild(summary);
      bindDisclosure(details);
      const inner = el('div', 'df-disclosure__body');
      if (spec.command && spec.command.length) {
        inner.appendChild(el('pre', 'df-log', spec.command.join(' ')));
      }
      if (spec.details) inner.appendChild(el('pre', 'df-log', spec.details));
      details.appendChild(inner);
      body.appendChild(details);
    }
    root.appendChild(body);

    const actions = el('div', 'df-banner__actions');
    for (const action of spec.actions || []) {
      const btn = el('button', 'df-btn df-btn--secondary df-btn--sm', action.label);
      btn.type = 'button';
      btn.addEventListener('click', () => {
        try {
          action.run();
        } catch (err) {
          report(err, 'banner action');
        }
        if (action.dismiss !== false) dismissBanner(key);
      });
      actions.appendChild(btn);
    }
    if (spec.dismissible !== false) {
      const close = el('button', 'df-icon-btn');
      close.type = 'button';
      close.setAttribute('aria-label', 'Dismiss this message');
      close.appendChild(svg(ICON.close));
      close.addEventListener('click', () => dismissBanner(key));
      actions.appendChild(close);
    }
    root.appendChild(actions);

    nodes.banners.appendChild(root);
    banners.set(key, { node: root });

    while (banners.size > BANNERS_MAX) {
      const oldest = banners.keys().next().value;
      dismissBanner(oldest);
    }
    return () => dismissBanner(key);
  }

  function dismissBanner(key) {
    const entry = banners.get(key);
    if (!entry) return;
    banners.delete(key);
    // A banner can hold focus (its dismiss button, a details drawer). Losing it
    // to <body> is the defect docs/accessibility.md §10 P1-6 names.
    if (entry.node.contains(doc.activeElement)) focusCurrentPane();
    if (entry.node.parentNode) entry.node.parentNode.removeChild(entry.node);
  }

  /**
   * showError renders a UIError from the frozen error contract. Message and
   * hint are both rendered — "every error says what to do next" — and an exit
   * code never appears alone.
   */
  function showError(uiError, opts = {}) {
    const e = uiError || {};
    const message = e.message || 'Something went wrong and the application could not say what.';
    const hint =
      e.hint ||
      `Try the action again. If it keeps failing, run the same step with the ${CLI_BINARY} command-line tool to see its output.`;
    const detailBits = [];
    if (e.details) detailBits.push(e.details);
    if (e.exit_class || typeof e.exit_code === 'number') {
      const cls = e.exit_class || 'unclassified';
      const code = typeof e.exit_code === 'number' ? ` (exit ${e.exit_code})` : '';
      detailBits.push(`${CLI_BINARY} reported: ${cls}${code}`);
    }
    if (e.code) detailBits.push(`code: ${e.code}`);
    return banner({
      kind: opts.kind || 'danger',
      key: opts.key || `err:${e.code || ''}:${message}`,
      title: message,
      text: hint,
      details: detailBits.join('\n\n'),
      command: e.command,
      actions: opts.actions,
    });
  }

  /* --- the toast ------------------------------------------------------- */

  /*
   * ONE pill, bottom centre, queued in JS.
   *
   * `.df-toast-host` holds at most one `.df-toast` — two in the DOM at once is
   * the bug the component exists to remove, because toasts stacking in the
   * bottom-right corner is a defining Electron signature and GNOME shows one.
   * So the queue lives here.
   *
   * Two rules that are easy to get wrong:
   *
   *   - The exit is finished by a TIMER, never by `animationend` alone. The
   *     design system's reduced-motion block collapses every animation, and an
   *     engine that decides there is nothing to animate fires no event at all —
   *     the toast would then sit there for the rest of the session.
   *   - A danger toast pre-empts a calmer one instead of waiting behind it. A
   *     build failure queued behind three "Copied." pills is a failure the
   *     operator sees twelve seconds late.
   */

  /** @type {Array<object>} specs waiting for the host to be free */
  const toastQueue = [];
  let activeToast = null;
  let toastExiting = false;

  function sameToast(a, b) {
    return a.kind === b.kind && a.text === b.text;
  }

  /** How long to wait before removing an exiting node. */
  function exitDelay() {
    // Under reduced motion `--duration-fast` is 1ms and there may be no
    // animation at all; waiting is then pure lag.
    return reducedMotion() ? 0 : 240;
  }

  function toast(kind, message, opts = {}) {
    if (!nodes.toastHost || shellState.destroyed) return NOOP;
    const kindName = normaliseKind(kind);
    const raw = String(message == null ? '' : message).trim();
    if (!raw) return NOOP;
    // There is one text slot, so a title is a sentence in front of the message
    // rather than a second line the component does not have.
    const title = opts.title ? String(opts.title).trim() : '';
    const text = title ? `${title} ${raw}` : raw;

    const spec = {
      kind: kindName,
      text,
      action: opts.action && typeof opts.action.run === 'function' ? opts.action : null,
      duration: typeof opts.duration === 'number' ? opts.duration : null,
    };
    spec.close = () => cancelToast(spec);

    // An error storm must not become a queue of identical pills.
    if (activeToast && sameToast(activeToast.spec, spec)) {
      activeToast.rearm();
      return activeToast.spec.close;
    }
    for (const queued of toastQueue) {
      if (sameToast(queued, spec)) return queued.close;
    }

    if (kindName === 'danger') {
      toastQueue.unshift(spec);
      if (activeToast && activeToast.spec.kind !== 'danger') dismissActiveToast();
    } else {
      toastQueue.push(spec);
    }
    // Past the cap the OLDEST pending notice is dropped: by the time five have
    // piled up, the first is describing something the operator has moved on
    // from.
    while (toastQueue.length > TOAST_QUEUE_MAX) toastQueue.shift();

    pumpToasts();
    return spec.close;
  }

  function cancelToast(spec) {
    if (activeToast && activeToast.spec === spec) {
      dismissActiveToast();
      return;
    }
    const i = toastQueue.indexOf(spec);
    if (i !== -1) toastQueue.splice(i, 1);
  }

  function pumpToasts() {
    if (activeToast || toastExiting || shellState.destroyed) return;
    const spec = toastQueue.shift();
    if (!spec) return;
    // showToast publishes `activeToast` itself, before it arms the dismissal
    // timer — see the note there.
    showToast(spec);
  }

  function showToast(spec) {
    const danger = spec.kind === 'danger';
    const root = el('div', `df-toast df-toast--${spec.kind}`);
    // Danger interrupts; everything else waits for a gap in the speech.
    root.setAttribute('role', danger ? 'alert' : 'status');
    root.setAttribute('aria-live', danger ? 'assertive' : 'polite');

    root.appendChild(svg(ICON[spec.kind], 'df-toast__icon'));
    root.appendChild(el('span', 'df-toast__text', spec.text));

    if (spec.action) {
      const btn = el(
        'button',
        'df-btn df-btn--secondary df-btn--sm df-toast__action',
        spec.action.label || 'Open',
      );
      btn.type = 'button';
      btn.addEventListener('click', () => {
        try {
          spec.action.run();
        } catch (err) {
          report(err, 'toast action');
        }
        dismissActiveToast();
      });
      root.appendChild(btn);
    }

    const closeBtn = el('button', 'df-icon-btn df-toast__close');
    closeBtn.type = 'button';
    closeBtn.setAttribute('aria-label', 'Dismiss this notice');
    closeBtn.appendChild(svg(ICON.close));
    closeBtn.addEventListener('click', () => dismissActiveToast());
    root.appendChild(closeBtn);

    // Reading time, floored and capped. A one-word "Copied" does not need ten
    // seconds; a two-line warning does not fit in three.
    const life =
      spec.duration != null
        ? spec.duration
        : Math.min(12000, Math.max(danger ? 8000 : 4000, 2000 + spec.text.length * 55));

    const entry = { spec, node: root, timer: null };

    entry.rearm = () => {
      if (activeToast !== entry || life <= 0) return;
      win.clearTimeout(entry.timer);
      entry.timer = win.setTimeout(() => dismissActiveToast(), life);
    };
    const disarm = () => win.clearTimeout(entry.timer);

    // A notice the operator is reading, or has tabbed into, must not evaporate
    // mid-sentence.
    root.addEventListener('pointerenter', disarm);
    root.addEventListener('pointerleave', () => entry.rearm());
    root.addEventListener('focusin', disarm);
    root.addEventListener('focusout', () => entry.rearm());

    // Publish BEFORE arming. `rearm` refuses to start a timer for a toast that
    // is not the active one — which is what stops a pointerleave on a node
    // already on its way out from resurrecting it — so arming first would mean
    // the guard rejected the toast being armed and nothing ever auto-dismissed.
    activeToast = entry;
    nodes.toastHost.appendChild(root);
    entry.rearm();
    return entry;
  }

  function dismissActiveToast() {
    const entry = activeToast;
    if (!entry) return;
    activeToast = null;
    win.clearTimeout(entry.timer);
    toastExiting = true;
    entry.node.classList.add('is-leaving');

    // The operator may be inside the pill — its action or its close button.
    // Focus must land somewhere real, never on <body>.
    if (entry.node.contains(doc.activeElement)) focusCurrentPane();

    let done = false;
    let timer = null;
    const drop = () => {
      if (done) return;
      done = true;
      win.clearTimeout(timer);
      if (entry.node.parentNode) entry.node.parentNode.removeChild(entry.node);
      toastExiting = false;
      pumpToasts();
    };
    // animationend is the fast path and NOTHING MORE. The timer is what
    // guarantees the node leaves.
    entry.node.addEventListener('animationend', drop, { once: true });
    timer = win.setTimeout(drop, exitDelay());
  }

  /* --- live regions ---------------------------------------------------- */

  /**
   * say rewrites the gate live region. The rewrite is the point: setting a live
   * region to the text it already holds announces nothing, so a second refusal
   * of the same control would be silent.
   */
  function say(text) {
    if (!nodes.gate) return;
    nodes.gate.textContent = '';
    win.setTimeout(() => {
      if (nodes.gate) nodes.gate.textContent = text;
    }, 30);
  }

  function announce(text) {
    if (!nodes.announcer) return;
    // Re-setting identical text does not re-announce; clear first.
    nodes.announcer.textContent = '';
    win.setTimeout(() => {
      if (nodes.announcer) nodes.announcer.textContent = text;
    }, 30);
  }

  /* --- panes and screen states ---------------------------------------- */

  /**
   * paneFor builds the `root` a screen mounts into: a bare `.df-app__pane`.
   *
   * The pane scrolls and has NO padding and NO measure, deliberately. Wrapping
   * the screen's content in `.df-app__content` is the screen's job, not the
   * shell's, because only the screen knows which variant it needs — a step that
   * is a form takes the 720px default, a wide table takes `--wide`, and the
   * virtualised catalogue takes `--bleed` and opts out of the measure
   * altogether. A shell that imposed the wrapper would put the picker in a
   * 720px column with no way out. Design README §6.24.
   */
  function paneFor(entry) {
    if (entry.pane) return entry.pane;
    const pane = el('div', 'df-app__pane');
    pane.id = `screen-${entry.def.id}`;
    pane.tabIndex = -1;
    pane.setAttribute('role', 'region');
    pane.setAttribute('aria-label', entry.def.title);
    pane.hidden = true;
    // Before the toast host, so the host stays the last child of .df-app__main
    // and the pill is painted over the pane rather than under it.
    nodes.main.insertBefore(pane, nodes.toastHost);
    entry.pane = pane;
    return pane;
  }

  function renderPaneLoading(pane, title) {
    pane.textContent = '';
    const state = el('div', 'df-state df-state--loading');
    const spinner = el('div', 'df-spinner df-spinner--lg');
    spinner.setAttribute('aria-hidden', 'true');
    state.appendChild(spinner);
    state.appendChild(el('span', 'df-state__title', `Opening ${title}`));
    state.appendChild(el('p', 'df-state__text', 'Preparing the screen.'));
    pane.appendChild(state);
  }

  /**
   * renderPaneError contains a screen that could not be loaded or that threw in
   * `mount`. One broken screen must not white-screen the app, so the failure
   * stays inside that screen's own pane and every other destination in the
   * stepper still works.
   */
  function renderPaneError(entry, err, phase) {
    const pane = paneFor(entry);
    pane.textContent = '';
    const state = el('div', 'df-state df-state--error');
    state.appendChild(svg(ICON.danger, 'df-state__icon'));
    state.appendChild(
      el('span', 'df-state__title', `The ${entry.def.navLabel.toLowerCase()} step could not be opened`),
    );
    state.appendChild(
      el(
        'p',
        'df-state__text',
        phase === 'load'
          ? 'This step failed to load. The rest of the application still works — use the steps in the header to go somewhere else, or try again.'
          : 'This step failed while building itself. The rest of the application still works — use the steps in the header to go somewhere else, or try again.',
      ),
    );

    const actions = el('div', 'df-state__actions');
    const retry = el('button', 'df-btn df-btn--primary df-btn--sm', 'Try again');
    retry.type = 'button';
    retry.addEventListener('click', () => {
      // Full reset: drop the half-built module, scope and DOM, then navigate
      // afresh so mount runs from a clean pane.
      const params = entry.lastParams;
      teardownScreen(entry);
      go(entry.def.id, params);
    });
    actions.appendChild(retry);
    if (history.length) {
      const backBtn = el('button', 'df-btn df-btn--secondary df-btn--sm', 'Back');
      backBtn.type = 'button';
      backBtn.addEventListener('click', () => back());
      actions.appendChild(backBtn);
    }
    state.appendChild(actions);

    const detail = el('div', 'df-state__detail');
    const details = doc.createElement('details');
    details.className = 'df-disclosure';
    const summary = doc.createElement('summary');
    summary.className = 'df-summary';
    summary.textContent = 'What failed';
    details.appendChild(summary);
    bindDisclosure(details);
    const inner = el('div', 'df-disclosure__body');
    inner.appendChild(el('pre', 'df-log', (err && (err.stack || err.message)) || String(err)));
    details.appendChild(inner);
    detail.appendChild(details);
    state.appendChild(detail);

    pane.appendChild(state);
    entry.failed = true;
    report(err, `screen "${entry.def.id}" ${phase}`);
  }

  /* --- ctx ------------------------------------------------------------ */

  /**
   * makeCtx builds exactly the object the screen contract specifies — eight
   * members, no more — and freezes it.
   *
   * Frozen because `ctx` is the contract's surface, not a scratch object: a
   * screen that stashes state on it would be inventing a channel other screens
   * cannot see and the shell cannot tear down. In a module (always strict mode) the
   * assignment throws, which is the failure you want.
   *
   * `on`/`off` are bound to this screen's own bus scope, which is the whole
   * point: there is no way for a screen to subscribe outside its own ledger,
   * because the runtime handle never reaches it.
   */
  function makeCtx(entry) {
    const scope = entry.scope;
    return Object.freeze({
      go: (id, params) => go(id, params),
      back: () => back(),
      on: (event, handler) => scope.on(event, handler),
      off: (event, handler) => scope.off(event, handler),
      get bindings() {
        return bindings;
      },
      toast: (kind, message, opts) => toast(kind, message, opts),
      states,
      theme,
    });
  }

  /* --- navigation ----------------------------------------------------- */

  async function ensureMounted(entry, token) {
    if (entry.mounted || entry.failed) return entry.mounted;

    const pane = paneFor(entry);
    renderPaneLoading(pane, entry.def.navLabel.toLowerCase());

    let mod;
    try {
      mod = await entry.def.load();
    } catch (err) {
      if (token !== navToken) return false;
      renderPaneError(entry, err, 'load');
      return false;
    }
    if (token !== navToken) return false;

    const screen = mod && (mod.default || mod);
    if (!screen || typeof screen.mount !== 'function') {
      renderPaneError(
        entry,
        new Error(
          `${entry.def.id}: the module has no default export with a mount(root, ctx) function (screen-contract.md §"The module shape")`,
        ),
        'load',
      );
      return false;
    }
    entry.module = screen;
    if (screen.id && screen.id !== entry.def.id) {
      report(
        new Error(`screen module declares id "${screen.id}" but is registered as "${entry.def.id}"`),
        'registry',
      );
    }

    entry.scope = bus.scope(entry.def.id);
    entry.ctx = makeCtx(entry);
    pane.textContent = '';
    pane.setAttribute('aria-label', screen.title || entry.def.title);

    try {
      screen.mount(pane, entry.ctx);
    } catch (err) {
      // The screen may have subscribed before it threw. Its listeners go now,
      // not later.
      try {
        entry.scope.dispose();
      } catch (_) {
        /* nothing useful to do */
      }
      entry.scope = null;
      entry.ctx = null;
      entry.module = null;
      renderPaneError(entry, err, 'mount');
      return false;
    }
    entry.mounted = true;
    return true;
  }

  function hideCurrent() {
    const current = shellState.currentID && registry.get(shellState.currentID);
    if (!current || !current.visible) return;
    current.visible = false;
    if (current.pane) current.pane.hidden = true;
    if (current.mounted && current.module && typeof current.module.hide === 'function') {
      try {
        current.module.hide();
      } catch (err) {
        report(err, `screen "${current.def.id}" hide`);
      }
    }
  }

  function focusCurrentPane() {
    const entry = shellState.currentID && registry.get(shellState.currentID);
    if (entry && entry.pane) {
      try {
        entry.pane.focus({ preventScroll: true });
      } catch (_) {
        entry.pane.focus();
      }
    }
  }

  /**
   * go navigates to a screen. Lazy-mounts on first visit, then show/hide.
   *
   * There is no transition. GTK applications essentially never animate a
   * main-content swap, and a step-based flow the operator moves through
   * dozens of times in a session is the last place to start.
   *
   * Re-entrancy: `go` awaits a dynamic import, so a second navigation can start
   * mid-flight. `navToken` makes the newest call win and the older one abandon
   * its work rather than paint over the new screen.
   */
  async function go(id, params, opts = {}) {
    if (shellState.destroyed) return false;
    const entry = registry.get(id);
    if (!entry) {
      report(new Error(`no screen registered as "${id}"`), 'navigation');
      toast('danger', `There is no step called “${id}”.`);
      return false;
    }

    closeMenu(false);
    const token = ++navToken;
    const previous = shellState.currentID;
    // Navigating to the screen you are already on is a re-show with new params,
    // not a departure and a return. `hide` means "stopped being visible", and
    // firing it here would make a screen tear down state it is about to need.
    const sameScreen = previous === id;

    if (previous && !sameScreen && opts.fromHistory !== true) {
      const prevEntry = registry.get(previous);
      history.push({ id: previous, params: prevEntry ? prevEntry.lastParams : undefined });
      while (history.length > HISTORY_MAX) history.shift();
    }

    if (id === 'readiness' && !params?.returnTo) params = { ...params, returnTo: previous && previous !== 'readiness' ? previous : 'target' };
    entry.lastParams = params;
    if (!sameScreen) hideCurrent();
    shellState.currentID = id;
    renderStepper();
    renderReadinessControl();
    renderStatus();

    const pane = paneFor(entry);
    pane.hidden = false;
    entry.visible = true;

    const ok = await ensureMounted(entry, token);
    if (token !== navToken) return false; // superseded; the winner owns the DOM

    if (ok && entry.module && typeof entry.module.show === 'function') {
      try {
        entry.module.show(entry.ctx, params);
      } catch (err) {
        report(err, `screen "${id}" show`);
        toast('danger', `The ${entry.def.navLabel.toLowerCase()} step failed to open.`);
      }
    }

    const title = (entry.module && entry.module.title) || entry.def.title;
    doc.title = `${APP_NAME} — ${title}`;
    pane.setAttribute('aria-label', title);
    // A stepper operator has no persistent nav to orient against, so the
    // announcement says where in the flow they are as well as what they are
    // looking at. docs/accessibility.md §10 P2-12.
    announce(
      typeof entry.def.step === 'number' || entry.def.stage === 'build'
        ? `${title}. Step ${entry.def.step || 3} of ${STEPS.length}.`
        : title,
    );
    if (opts.focus !== false) focusCurrentPane();
    watchActionArea(pane);
    renderStatus();
    scheduleJobsRefresh();
    trace('navigated to', id);
    return ok;
  }

  function watchActionArea(pane) {
    nodes.actionObserver?.disconnect();
    const actions = pane.querySelector('.df-screen-layout > .df-actionbar');
    const update = () => nodes.main.style.setProperty('--screen-actionbar-space', `${actions?.getBoundingClientRect().height || 56}px`);
    if (actions && typeof win.ResizeObserver === 'function') {
      nodes.actionObserver = new win.ResizeObserver(update);
      nodes.actionObserver.observe(actions);
    }
    update();
  }

  function back() {
    const current = registry.get(shellState.currentID);
    const returnTo = current?.lastParams?.returnTo;
    if ((shellState.currentID === 'readiness' || shellState.currentID === 'export') && returnTo && returnTo !== shellState.currentID && registry.has(returnTo)) {
      go(returnTo, undefined, { fromHistory: true });
      return true;
    }
    const prev = history.pop();
    renderStatus();
    if (!prev) return false;
    go(prev.id, prev.params, { fromHistory: true });
    return true;
  }

  function teardownScreen(entry) {
    if (entry.scope) {
      try {
        entry.scope.dispose();
      } catch (err) {
        report(err, `disposing subscriptions for "${entry.def.id}"`);
      }
      entry.scope = null;
    }
    if (entry.module && typeof entry.module.destroy === 'function') {
      try {
        entry.module.destroy();
      } catch (err) {
        report(err, `screen "${entry.def.id}" destroy`);
      }
    }
    if (entry.pane && entry.pane.parentNode) entry.pane.parentNode.removeChild(entry.pane);
    entry.pane = null;
    entry.module = null;
    entry.ctx = null;
    entry.mounted = false;
    entry.failed = false;
    entry.visible = false;
  }

  /* --- event routing --------------------------------------------------- */

  function wireEvents(runtime) {
    bus = createEventBus({
      names: EVENT_NAMES,
      subscribe: (name, cb) => runtime.on(name, cb),
      unsubscribe: runtime.off,
      onError: report,
    });
    shellScope = bus.scope('shell');

    // app:error is the global error channel: a failure that belongs to no call
    // the frontend made. It gets a banner with the backend's own summary and
    // hint, never a bare exit code.
    shellScope.on('app:error', (payload) => {
      showError(payload);
    });

    shellScope.on('target:changed', (payload) => {
      if (payload?.generation != null && shellState.target?.generation > payload.generation) return;
      shellState.target = payload || null;
      recomputeBuildCompletion();
      renderStatus();
      renderStepper();
    });
    shellScope.on('selection:changed', (payload) => {
      if (!acceptSelectionSummary(shellState, payload)) return;
      recomputeBuildCompletion();
      renderStepper();
    });
    shellScope.on('app:lifecycle', (payload) => {
      scheduleJobsRefresh();
      shellState.lifecycle = payload || null;
      if (typeof payload?.selection_revision === 'number' && payload.selection_revision >= (shellState.selectionRevision ?? 0)) shellState.selectionRevision = payload.selection_revision;
      recomputeBuildCompletion();
      renderStepper();
      renderJobContext();
    });
    for (const event of ['build:started', 'build:progress', 'export:started', 'export:progress', 'verify:started']) {
      shellScope.on(event, scheduleJobsRefresh);
    }
    shellScope.on('readiness:started', () => {
      shellState.readiness = { ...shellState.readiness, checking: true };
      renderReadinessControl();
    });
    shellScope.on('readiness:finished', (payload) => {
      shellState.readiness = payload?.report || null;
      renderReadinessControl();
    });

    // Identity-free event payloads can be late. Their only authority is to
    // request a status read; the accepted snapshot determines any notice.
    for (const route of ['build', 'export', 'verify']) {
      shellScope.on(`${route}:finished`, () => {
        terminalNotices.request(route);
        scheduleJobsRefresh();
      });
    }

    if (debug) {
      for (const name of EVENT_NAMES) {
        if (name === 'build:event') continue; // thousands per build
        shellScope.on(name, (payload) => trace('event', name, payload));
      }
    }
  }

  /* --- keyboard -------------------------------------------------------- */

  function onKeyDown(ev) {
    // Keep navigation and close shortcuts out of modal dialogs.
    if (doc.querySelector('dialog[open]')) return;
    if (ev.key === 'F10' && !ev.shiftKey) {
      ev.preventDefault();
      openMenu();
      return;
    }
    if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'w') {
      if (typeof bindings.RequestClose === 'function') {
        ev.preventDefault();
        requestClose();
      }
      return;
    }
    // Alt+Left is the platform-neutral Back on both WebKit2GTK and WebView2.
    if (ev.altKey && ev.key === 'ArrowLeft') {
      if (history.length) {
        ev.preventDefault();
        back();
      }
    }
  }

  /* --- lifecycle -------------------------------------------------------- */

  let readyPromise = null;

  /**
   * start resolves bindings, wires lifecycle events and loads About metadata.
   * It does not navigate: main.js owns the first-screen decision.
   *
   * Nothing here is on the first-paint path. buildFrame() has already run and
   * painted by the time this is called.
   */
  function start(opts = {}) {
    if (readyPromise) return readyPromise;
    shellState.started = true;

    readyPromise = (async () => {
      const resolved = await resolveBindings({ timeoutMS: opts.timeoutMS, win });
      if (resolved) {
        bindings = resolved;
        bindingsReady = true;
      } else {
        banner({
          kind: 'danger',
          key: 'no-bindings',
          title: NO_BINDINGS_ERROR.message,
          text: NO_BINDINGS_ERROR.hint,
          dismissible: false,
        });
      }

      const runtime = options.runtime || defaultRuntime(win);
      wireEvents(runtime);

      doc.addEventListener('keydown', onKeyDown);

      // AppInfo supplies About metadata after first paint.
      // Skipped entirely without bindings — the missing-bindings banner above
      // already says the one thing there is to say, and a second banner saying
      // it again is noise.
      if (bindingsReady) {
        Promise.resolve()
          .then(() => bindings.AppInfo())
          .then((info) => {
            shellState.appInfo = info || null;
            renderStatus();
            if (info && info.headless) {
              banner({
                kind: 'info',
                key: 'headless',
                title: 'Running without the desktop runtime',
                text: 'Progress events will not arrive, so long operations will look frozen. Run the packaged application for the full experience.',
              });
            }

          })
          .catch((err) => report(err, 'AppInfo'));

        // Seed the stepper's completed marks. All three are documented `fast` —
        // memory reads, no probe and no subprocess — and none of them is
        // awaited by anything, so they cost the boot path nothing but three
        // bridge round-trips after first paint. Without them the header lies
        // until the first event of each kind arrives, which for a session where
        // the operator changes nothing is never.
        seedStepperState();
      }

      return { bindings, bindingsReady };
    })();

    return readyPromise;
  }

  let jobsRequest = 0;
  let jobsTimer = null;
  function scheduleJobsRefresh() {
    jobsRequest++;
    if (jobsTimer !== null || shellState.destroyed) return;
    jobsTimer = win.setTimeout(() => { jobsTimer = null; refreshJobs(); }, 80);
  }

  async function refreshJobs() {
    if (!bindingsReady || shellState.destroyed) return;
    const request = ++jobsRequest;
    const [build, copy, lifecycle, verify] = await Promise.allSettled([
      Promise.resolve().then(() => bindings.BuildStatus()),
      Promise.resolve().then(() => bindings.ExportStatus()),
      Promise.resolve().then(() => typeof bindings.LifecycleStatus === 'function' ? bindings.LifecycleStatus() : null),
      terminalNotices.hasPending('verify') ? Promise.resolve().then(() => bindings.VerifyStatus()) : Promise.resolve(null),
    ]);
    if (request !== jobsRequest || shellState.destroyed) return;
    if (build.status === 'fulfilled' && build.value && typeof build.value.running === 'boolean') shellState.buildStatus = build.value;
    if (copy.status === 'fulfilled' && copy.value && typeof copy.value.running === 'boolean') shellState.exportStatus = copy.value;
    if (lifecycle.status === 'fulfilled' && lifecycle.value && typeof lifecycle.value.build_running === 'boolean') {
      shellState.lifecycle = lifecycle.value;
      if (lifecycle.value.selection_revision >= (shellState.selectionRevision ?? 0)) shellState.selectionRevision = lifecycle.value.selection_revision;
    }
    recomputeBuildCompletion();
    renderStepper();
    renderJobContext();
    for (const [route, result] of [['build', build], ['export', copy], ['verify', verify]]) {
      if (result.status !== 'fulfilled') continue;
      const notice = terminalNotices.accept(route, result.value);
      const screenID = route === 'verify' ? 'export' : route;
      if (notice && shellState.currentID !== screenID) {
        toast(notice.kind, notice.text, { action: { label: 'Open', run: () => go(screenID) } });
      }
    }
  }

  function recomputeBuildCompletion() {
    const status = shellState.buildStatus;
    const generation = shellState.target?.generation ?? shellState.lifecycle?.target_generation;
    shellState.buildDone = buildFulfilsDraft(status, generation, shellState.selectionRevision);
  }

  function seedStepperState() {
    Promise.resolve().then(() => bindings.Selection()).then((selection) => {
      if (!acceptSelectionSummary(shellState, selection)) return;
      recomputeBuildCompletion();
      renderStepper();
    }).catch(() => {});
    refreshJobs();
  }

  function destroy() {
    if (shellState.destroyed) return;
    shellState.destroyed = true;
    if (jobsTimer !== null) win.clearTimeout(jobsTimer);
    nodes.actionObserver?.disconnect();
    doc.removeEventListener('keydown', onKeyDown);
    doc.removeEventListener('pointerdown', onMenuOutside);
    closeMenu(false);
    nodes.themeController?.destroy();
    if (nodes.utilityDialog?.open) nodes.utilityDialog.close();
    if (nodes.headerObserver) {
      try {
        nodes.headerObserver.disconnect();
      } catch (err) {
        report(err, 'disconnecting the frame observer');
      }
      nodes.headerObserver = null;
    }
    if (nodes.onWinResize) {
      win.removeEventListener('resize', nodes.onWinResize);
      nodes.onWinResize = null;
    }
    // A modal dialog left open would keep the whole document inert after the
    // frame under it has been removed.
    if (nodes.about && nodes.about.open) {
      try {
        nodes.about.close();
      } catch (err) {
        report(err, 'closing the about dialog');
      }
    }
    for (const entry of registry.values()) teardownScreen(entry);
    toastQueue.length = 0;
    if (activeToast) {
      win.clearTimeout(activeToast.timer);
      if (activeToast.node.parentNode) activeToast.node.parentNode.removeChild(activeToast.node);
      activeToast = null;
    }
    if (bus) bus.close();
    bus = null;
    shellScope = null;
    if (theme && typeof theme.destroy === 'function') {
      try {
        theme.destroy();
      } catch (err) {
        report(err, 'theme destroy');
      }
    }
    mountPoint.textContent = '';
  }

  /* --- public surface --------------------------------------------------- */

  buildFrame();

  return {
    start,
    destroy,
    go,
    back,
    toast,
    banner,
    showError,
    ready: () => readyPromise || start(),

    /** setTarget lets main.js seed the header before the first event arrives. */
    setTarget(view) {
      if (view?.generation != null && shellState.target?.generation > view.generation) return;
      shellState.target = view || null;
      recomputeBuildCompletion();
      renderStatus();
      renderStepper();
    },
    setReadiness(report_) {
      shellState.readiness = report_ || null;
      renderReadinessControl();
    },

    get bindings() {
      return bindings;
    },
    get bindingsReady() {
      return bindingsReady;
    },
    /** AppInfo once it has landed, so a screen need not ask a second time. */
    get appInfo() {
      return shellState.appInfo;
    },
    get currentScreen() {
      return shellState.currentID;
    },
    get historyDepth() {
      return history.length;
    },
    /** subscriptionCount is a leak assertion for tests: 0 after destroy. */
    subscriptionCount: () => (bus ? bus.count() : 0),
    screenIDs: () => SCREENS.map((s) => s.id),
    /** stepIDs is the stepper's three, in order. Readiness is not among them. */
    stepIDs: () => STEPS.map((s) => s.id),
    /** pendingToasts is a test assertion: the DOM holds one, this is the rest. */
    pendingToasts: () => toastQueue.length,
  };
}

/* ==========================================================================
   The Wails runtime seam.

   The shell talks to exactly one runtime object with `on`/`off`. In the app
   that is window.runtime, installed by Wails before any module runs. Keeping it
   behind this shape means bus.js and the shell are testable with a two-method
   stub, and means nothing but this function knows the runtime exists.
   ========================================================================== */

export function defaultRuntime(win = window) {
  const rt = win.runtime;
  if (rt && typeof rt.EventsOnMultiple === 'function') {
    return {
      on: (name, cb) => rt.EventsOnMultiple(name, cb, -1),
      off: (name) => rt.EventsOff(name),
    };
  }
  // No runtime: subscriptions are recorded and never fire. The shell has
  // already raised a banner saying so; failing here would be worse.
  return { on: () => () => {}, off: () => {} };
}
