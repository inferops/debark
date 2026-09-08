/*
 * main.js — the entry point frontend/index.html loads as a module.
 *
 * There is no bundler. This file runs as authored, on WebKit2GTK and WebView2.
 * Its whole job is three steps, in this order and for this reason:
 *
 *   1. Attach the design system's stylesheets. They are in <head> before the
 *      first paint, so nothing flashes unstyled.
 *   2. Build and paint the frame — synchronously, with no backend call in the
 *      way. Cold start to interactive is budgeted at 1.5 s, and the way to
 *      spend it is on the frame the operator can already see and use.
 *   3. Only then talk to the backend: resolve the bindings, wire the events,
 *      and decide which screen to land on.
 *
 * Nothing here builds a screen's DOM. Screens are dynamically imported on first
 * navigation, so boot pays for this file, the shell and the CSS — not for five
 * screens' worth of markup the operator has not asked for.
 *
 * Ownership: the shell — this file and `shell/shell.js`.
 */

import { createShell, CLI_BINARY } from './shell/shell.js';

/* ==========================================================================
   1. Stylesheets.

   index.html links tokens.css and then components.css directly, which is
   where they belong: this module is deferred, so anything injected from here
   lands after first paint and the operator sees one unstyled frame. The load
   order is not optional — components.css reads tokens tokens.css defines.

   This block is the fallback for the case where the document did not link
   them (a harness page, or an index.html that regressed). It is idempotent:
   a stylesheet already present is not added twice.
   ========================================================================== */

function addStylesheet(href) {
  // Compare the RESOLVED url, not the attribute: index.html links these
  // relative to itself ("./src/design/tokens.css") while this module resolves
  // against its own location, so the two attribute strings never match even
  // when they name the same file. The .href property is absolute on both.
  for (const link of document.querySelectorAll('link[rel="stylesheet"]')) {
    if (link.href === href) return link;
  }

  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = href;
  link.addEventListener('error', () => {
    // eslint-disable-next-line no-console
    console.error('[main] stylesheet failed to load:', href);
  });
  document.head.appendChild(link);
  return link;
}

for (const file of ['./design/tokens.css', './design/components.css']) {
  addStylesheet(new URL(file, import.meta.url).href);
}

/* ==========================================================================
   2. The frame.
   ========================================================================== */

const shell = createShell({
  root: document.getElementById('app') || document.body,
  debug: new URLSearchParams(location.search).has('debug'),
});

// A screen that throws inside an event handler or a promise it forgot to catch
// would otherwise fail silently and look like a hang. It gets the same banner
// treatment as a backend failure: what happened, and what to do next.
window.addEventListener('error', (ev) => {
  shell.showError(
    {
      code: 'app.internal',
      message: 'Something in the interface failed unexpectedly.',
      hint: 'The rest of the application should still work. If this step is stuck, use the steps in the header to go somewhere else.',
      details: (ev.error && (ev.error.stack || ev.error.message)) || ev.message || '',
    },
    { key: 'window-error' },
  );
});

window.addEventListener('unhandledrejection', (ev) => {
  const reason = ev.reason;
  shell.showError(
    {
      code: 'app.internal',
      message: 'A background task in the interface failed.',
      hint: 'The rest of the application should still work. If a step is stuck, use the steps in the header to go somewhere else, or reopen it.',
      details: (reason && (reason.stack || reason.message)) || String(reason),
    },
    { key: 'window-rejection' },
  );
});

/* Target is always the first task. Context reads and host checks run in the
 * background, and never move the operator away from the current screen. */
async function loadContext(bindings) {
  const [targetResult, report] = await Promise.all([
    Promise.resolve().then(() => bindings.CurrentTarget()).catch(() => null),
    Promise.resolve().then(() => bindings.Readiness()).catch(() => null),
  ]);
  if (targetResult?.target) shell.setTarget(targetResult.target);
  shell.setReadiness(report);
  const checked = Boolean(report?.checked_at || report?.checks?.length);
  if (report && !checked && !report.checking) {
    Promise.resolve().then(() => bindings.StartReadinessCheck()).catch(() => {});
  }
}

/* ==========================================================================
   Boot.

   The frame is already in the DOM by the time createShell returned. Handing the
   engine one frame before starting the backend work means the operator sees the
   window painted rather than white while the bindings resolve.
   ========================================================================== */

function boot() {
  shell
    .start()
    .then(({ bindings }) => {
      loadContext(bindings);
      return shell.go('target', undefined, { focus: false });
    })
    .catch((err) => {
      // Boot itself failing is the one case where there is no screen to fall
      // back to, so it gets an undismissable banner and an offer of the one
      // screen that diagnoses this class of problem.
      // eslint-disable-next-line no-console
      console.error('[main] boot failed:', err);
      shell.showError(
        {
          code: 'app.internal',
          message: 'The application could not finish starting.',
          hint: `Close and reopen the window. If it keeps happening, run \`${CLI_BINARY} --version\` in a terminal to check the command-line tool is installed.`,
          details: (err && (err.stack || err.message)) || String(err),
        },
        { key: 'boot' },
      );
      shell.go('target', undefined, { focus: false });
    });
}

if (typeof requestAnimationFrame === 'function') {
  // One frame: paint what is built, then start the work.
  requestAnimationFrame(() => boot());
} else {
  boot();
}

// Exposed for a smoke test and for the debug console. Not an API: nothing in
// frontend/src may reach for it. `__debarkShell` is the old spelling, kept as
// an alias so a harness written against it still resolves; the product is
// Debark and new code uses that name.
window.__debarkShell = shell;
window.__debarkShell = shell;

export { shell };
