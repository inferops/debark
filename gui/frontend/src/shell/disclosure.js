/*
 * disclosure.js — make a `<details>` drawer readable on the shipping engine.
 *
 * Why this module exists, in one measurement.
 *
 * WebKit2GTK 2.52.6 maps `<details>` and `<summary>` to AT-SPI role `unknown`.
 * Not "disclosure triangle", not "push button", not "expandable" — `unknown`,
 * with no EXPANDED state and no EXPANDABLE state. Orca 46.1, driven against
 * the real Debark binary in a container, announced the readiness screen's
 * drawer as:
 *
 *     "Details for debark command-line tool."
 *
 * and nothing else. No role, no "collapsed", no "expanded" when it opened. An
 * operator hears a phrase and has no way to know it is a control, let alone
 * what pressing it did. There are fourteen of these drawers across the app.
 *
 * Three markups were driven side by side on that engine, with Orca reading
 * each one (measured directly against that engine):
 *
 *   plain `<summary>`                       -> "A plain summary."
 *   `<summary aria-expanded="false">`       -> "B summary with aria-expanded."
 *   `<summary role="button" aria-expanded>` -> "C summary role=button
 *                                              collapsed push button."
 *
 * `aria-expanded` on its own does nothing: WebKit's `unknown` role swallows
 * it. Only the explicit `role="button"` reaches Orca, and once the role is
 * there the state comes with it. So that is what this does — and it has to be
 * kept in step with the element's own `open` property, because the engine
 * toggles that itself and never touches the attribute.
 *
 * This is deliberately NOT in components.css: CSS cannot add ARIA, and the
 * design system's §6.12 markup is correct HTML. The gap is in one engine's
 * mapping, and the repair belongs next to the code that builds the drawer.
 *
 * Chromium/WebView2 maps `<summary>` to a disclosure-triangle role and
 * announces the state without help. `role="button"` there replaces one correct
 * announcement with another correct one, so this is safe on both targets.
 */

/**
 * bindDisclosure gives a `<details>`' `<summary>` an explicit button role and
 * an `aria-expanded` that tracks `open`.
 *
 * Idempotent, and safe to call on a detached node — the `toggle` event fires
 * whether or not the element is in the document.
 *
 * @param {HTMLElement|null|undefined} details a `<details>` element
 * @returns {HTMLElement|null|undefined} the same element, so it can be used
 *   inline in a builder: `bindDisclosure(h('details', …))`
 */
export function bindDisclosure(details) {
  if (!details || details.nodeType !== 1) return details;
  const summary = details.querySelector(':scope > summary') || details.querySelector('summary');
  if (!summary) return details;
  if (summary.dataset.dfDisclosureBound === '1') return details;
  summary.dataset.dfDisclosureBound = '1';

  // role="button" and not role="group"/"region" on the <details>: the thing an
  // operator needs named is the control they are standing on, and the body
  // that appears is read as ordinary content underneath it.
  summary.setAttribute('role', 'button');
  const sync = () => summary.setAttribute('aria-expanded', details.open ? 'true' : 'false');
  sync();
  // `toggle` is the one event both the engine's own click handling and a
  // programmatic `details.open = true` go through.
  details.addEventListener('toggle', sync);
  return details;
}

export default { bindDisclosure };
