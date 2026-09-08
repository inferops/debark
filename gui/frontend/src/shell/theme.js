/**
 * Theme controller — light, dark, and follow-the-system.
 *
 * This module owns `ctx.theme` (docs/dev/screen-contract.md). It does not
 * define the theming contract: that lives in frontend/src/design/tokens.css
 * and is documented in frontend/src/design/README.md §3. This file is only the
 * switch that drives it.
 *
 *     data-theme absent    -> follow the system (prefers-color-scheme)
 *     data-theme="light"   -> light, even on a dark system
 *     data-theme="dark"    -> dark, even on a light system
 *
 * There is deliberately no `data-theme="system"`. The CSS does not know that
 * value, and setting it would leave the app pinned to the light palette on a
 * dark desktop. "System" is the *absence* of the attribute.
 *
 * No CSS is added here. The control is built from library components
 * (`.df-cluster`, `.df-chip`), and the only class it toggles is `.is-active`,
 * which the library already defines. Per README §5 a forced-state class
 * conveys nothing to assistive technology, so `.is-active` always travels with
 * the matching `aria-checked`.
 */

/** localStorage key. Exported so an early bootstrap can read the same key. */
export const THEME_STORAGE_KEY = 'debark.theme';

/** Valid modes, in the order the control renders them. */
export const THEME_MODES = ['system', 'light', 'dark'];

const LABELS = { system: 'System', light: 'Light', dark: 'Dark' };

const DARK_QUERY = '(prefers-color-scheme: dark)';

/* ==========================================================================
   Pure helpers — usable before a controller exists.
   ========================================================================== */

/** Coerce anything to a valid mode. Unknown input degrades to 'system'. */
export function normalizeMode(value) {
  const v = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return THEME_MODES.indexOf(v) === -1 ? 'system' : v;
}

/**
 * Read the persisted choice. Storage can throw (a webview with site data
 * disabled, a sandboxed origin) or come back empty; either way we degrade to
 * following the system rather than failing.
 */
export function readStoredMode() {
  try {
    return normalizeMode(window.localStorage.getItem(THEME_STORAGE_KEY));
  } catch (err) {
    return 'system';
  }
}

/** Persist the choice. Returns false if storage refused; never throws. */
function writeStoredMode(mode) {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, mode);
    return true;
  } catch (err) {
    return false;
  }
}

/**
 * Put a mode on the root element. This is the whole mechanism — everything
 * else in this file exists to decide which mode to pass here.
 */
export function applyTheme(mode) {
  const m = normalizeMode(mode);
  const root = document.documentElement;
  if (m === 'system') {
    root.removeAttribute('data-theme');
  } else {
    root.setAttribute('data-theme', m);
  }
  return m;
}

/**
 * Apply the persisted choice. Idempotent and cheap — safe to call from an
 * inline bootstrap in index.html and again from module code.
 */
export function applyStoredTheme() {
  return applyTheme(readStoredMode());
}

/** The `prefers-color-scheme: dark` query, or null without matchMedia. */
function darkQuery() {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return null;
  try {
    return window.matchMedia(DARK_QUERY);
  } catch (err) {
    return null;
  }
}

/**
 * Subscribe to a MediaQueryList. Returns an unsubscribe function.
 * `addEventListener` is the modern spelling; `addListener` is kept because the
 * shipping target is the *system* WebKitGTK on whatever the oldest supported
 * Debian/Ubuntu ships, not an evergreen engine.
 */
function watchMedia(mql, onChange) {
  if (!mql) return function () {};
  if (typeof mql.addEventListener === 'function') {
    mql.addEventListener('change', onChange);
    return function () { mql.removeEventListener('change', onChange); };
  }
  if (typeof mql.addListener === 'function') {
    mql.addListener(onChange);
    return function () { mql.removeListener(onChange); };
  }
  return function () {};
}

/* ==========================================================================
   The controller.
   ========================================================================== */

/**
 * Build the theme control the shell puts in its header.
 *
 * @param {{label?: string}} [options] `label` names the radio group for
 *   assistive technology; defaults to "Theme".
 * @returns {{
 *   el: HTMLElement,
 *   get: () => string,
 *   set: (mode: string) => string,
 *   effective: () => string,
 *   onChange: (handler: Function) => Function,
 *   destroy: () => void
 * }}
 *
 * ARIA: a radio group (WAI-ARIA APG), not three toggle buttons. A three-way
 * choice announced as "System, radio button, checked, 1 of 3" is correct;
 * three independent `aria-pressed` buttons would announce three unrelated
 * on/off states and never say which of them is in force. Roving tabindex, so
 * the group is one Tab stop and the arrow keys move within it.
 */
export function createThemeController(options) {
  const opts = options || {};
  const label = typeof opts.label === 'string' && opts.label ? opts.label : 'Theme';

  const handlers = [];
  const buttons = [];
  const mql = darkQuery();

  let mode = readStoredMode();
  let systemDark = !!(mql && mql.matches);
  let destroyed = false;

  // --- markup -------------------------------------------------------------

  const el = document.createElement('div');
  el.className = 'df-cluster';
  el.setAttribute('role', 'radiogroup');
  el.setAttribute('aria-label', label);
  // A hook for the shell to find or place this control. Deliberately not a
  // `df-` class: inventing a library class name for something the library does
  // not style would be a lie about where its CSS lives.
  el.setAttribute('data-theme-switch', '');

  for (let i = 0; i < THEME_MODES.length; i++) {
    const m = THEME_MODES[i];
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'df-chip';
    button.setAttribute('role', 'radio');
    button.setAttribute('aria-checked', 'false');
    button.setAttribute('data-theme-mode', m);
    button.tabIndex = -1;

    const text = document.createElement('span');
    text.className = 'df-chip__label';
    text.textContent = LABELS[m];
    button.appendChild(text);

    el.appendChild(button);
    buttons.push(button);
  }

  // --- state --------------------------------------------------------------

  function effective() {
    if (mode === 'light' || mode === 'dark') return mode;
    return systemDark ? 'dark' : 'light';
  }

  let lastMode = mode;
  let lastEffective = effective();

  function notify() {
    const next = { mode: mode, effective: effective() };
    if (next.mode === lastMode && next.effective === lastEffective) return;
    lastMode = next.mode;
    lastEffective = next.effective;
    // Copy first: a handler may unsubscribe itself while we iterate.
    const list = handlers.slice();
    for (let i = 0; i < list.length; i++) {
      try {
        list[i](next);
      } catch (err) {
        // One bad subscriber must not stop the others, or the theme.
        console.error('theme: onChange handler threw', err);
      }
    }
  }

  /**
   * Reflect `mode` onto the three radios. `aria-checked` is the meaning,
   * `.is-active` is the appearance (tint + heavier border + medium weight, so
   * the selected state is never signalled by colour alone), and the roving
   * tabindex keeps the group a single Tab stop.
   */
  function syncButtons() {
    for (let i = 0; i < buttons.length; i++) {
      const button = buttons[i];
      const on = button.getAttribute('data-theme-mode') === mode;
      button.setAttribute('aria-checked', on ? 'true' : 'false');
      if (on) {
        button.classList.add('is-active');
      } else {
        button.classList.remove('is-active');
      }
      button.tabIndex = on ? 0 : -1;
    }
  }

  function set(next) {
    const m = normalizeMode(next);
    mode = m;
    applyTheme(m);
    writeStoredMode(m); // best effort: a refusal costs persistence, not the app
    syncButtons();
    notify();
    return m;
  }

  // --- interaction --------------------------------------------------------

  function selectAndFocus(index) {
    const n = THEME_MODES.length;
    const i = ((index % n) + n) % n;
    set(THEME_MODES[i]);
    buttons[i].focus();
  }

  function onClick(event) {
    let node = event.target;
    while (node && node !== el) {
      if (node.nodeType === 1 && node.hasAttribute('data-theme-mode')) {
        set(node.getAttribute('data-theme-mode'));
        return;
      }
      node = node.parentNode;
    }
  }

  // Arrow keys move *and* check, per the APG radio-group pattern. Space and
  // Enter need no handling: these are real <button>s, so both fire click.
  function onKeydown(event) {
    if (event.altKey || event.ctrlKey || event.metaKey) return;
    const current = THEME_MODES.indexOf(mode);
    let handled = true;
    switch (event.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        selectAndFocus(current + 1);
        break;
      case 'ArrowLeft':
      case 'ArrowUp':
        selectAndFocus(current - 1);
        break;
      case 'Home':
        selectAndFocus(0);
        break;
      case 'End':
        selectAndFocus(THEME_MODES.length - 1);
        break;
      default:
        handled = false;
    }
    if (handled) event.preventDefault();
  }

  function onSystemChange(event) {
    systemDark = event && typeof event.matches === 'boolean'
      ? event.matches
      : !!(mql && mql.matches);
    // Only the effective theme moved. `data-theme` is already correct in every
    // mode — absent for system, pinned otherwise — so there is nothing to
    // re-apply; just tell anyone who renders differently per theme.
    notify();
  }

  el.addEventListener('click', onClick);
  el.addEventListener('keydown', onKeydown);
  const unwatchMedia = watchMedia(mql, onSystemChange);

  // Apply straight away: the module-level bootstrap at the foot of this file
  // may not have run (a test harness, a re-created document), and applying the
  // same mode twice is harmless.
  applyTheme(mode);
  syncButtons();

  // --- public surface -----------------------------------------------------

  return {
    el: el,

    /** 'light' | 'dark' | 'system' — what the user chose. */
    get: function () { return mode; },

    /** Choose a mode. Unknown input degrades to 'system'. */
    set: set,

    /** 'light' | 'dark' — what is actually showing right now. */
    effective: effective,

    /**
     * Subscribe to theme changes. The handler is called with
     * `{ mode, effective }` whenever either value changes — including when the
     * desktop theme flips while the app is open and the mode is 'system'.
     * Returns an unsubscribe function.
     */
    onChange: function (handler) {
      if (typeof handler !== 'function') return function () {};
      handlers.push(handler);
      return function () {
        const i = handlers.indexOf(handler);
        if (i !== -1) handlers.splice(i, 1);
      };
    },

    /** Release the media-query listener, the DOM listeners and the control. */
    destroy: function () {
      if (destroyed) return;
      destroyed = true;
      unwatchMedia();
      el.removeEventListener('click', onClick);
      el.removeEventListener('keydown', onKeydown);
      handlers.length = 0;
      if (el.parentNode) el.parentNode.removeChild(el);
    },
  };
}

/*
 * Applied at module-evaluation time, which is the earliest this file can act.
 * index.html loads src/main.js as a deferred module, so this still runs after
 * the first paint, and on its own it would let a stored non-system choice flash
 * the system palette for a frame.
 *
 * That flash is gone: frontend/index.html now carries a classic, non-deferred
 * script in <head>, ahead of the stylesheets, that reads the same key and
 * stamps the same attribute. This call stays because it is what makes the
 * module correct in isolation — a harness page, a re-created document, a build
 * whose index.html regressed — and applying the same mode twice costs nothing.
 *
 * THE KEY AND THE VALUES ARE DUPLICATED IN index.html BY NECESSITY: the
 * bootstrap must run before any module. Changing THEME_STORAGE_KEY or
 * THEME_MODES here without changing that snippet leaves the two disagreeing and
 * brings the flash back. index.html belongs to another package; report the change
 * rather than making it.
 */
if (typeof document !== 'undefined' && document.documentElement) {
  applyStoredTheme();
}
