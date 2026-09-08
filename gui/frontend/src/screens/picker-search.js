// picker-search.js — the picker's one row of chrome: a search box, a single
// filter control, and a result count.
//
// This module owns nothing but its own subtree.
// `picker-list.js` composes it, owns the fetching, and owns the results list;
// this file never calls a Wails binding, never touches another screen's DOM,
// and makes no network request of any kind. It describes intent; the list acts
// on it.
//
// ---------------------------------------------------------------------------
// What this file used to be, and why it is not that any more
// ---------------------------------------------------------------------------
//
// Before the redesign this component stacked five rows above a 60,000-row
// virtualised list, four of which were independent ways to narrow the same set:
//
//   1. the search box (plus a permanent hint paragraph under it)
//   2. a two-option "All packages / Applications" segmented control
//      (plus a two-sentence description paragraph under *that*)
//   3. a curated application-category chip cloud with a "Show N more" expander
//   4. a <details> disclosure holding a SECOND filter input and a SECOND chip
//      cloud — a search box whose only job was to filter the thing three rows
//      below the first search box
//   5. an "active filters" chip row echoing the state of 2 and 3
//
// All four filtering mechanisms are now ONE control: a `.df-popover` behind a
// single button that sits next to the search box and is closed by default. Its
// own label states the current filter — "All packages", "Applications only",
// "Section: devel" — which is why the "active filters" row could be deleted
// outright rather than moved. A trigger that states its own value *is* the
// active-filter indicator, and a separate echo row is redundant the moment it
// does.
//
// The disclosure became a popover for CORRECTNESS, not for looks. An inline
// <details> reflows everything below it on every open and close. What was
// below it is a virtualised list whose scroll position and keyboard cursor are
// computed from `scrollTop`; reflowing it moves both, every time somebody
// touches a filter. `.df-popover` layers over the content instead
// (design/README.md section 6.21, and rule 7 of section 11).
//
// The nested "filter the filter" is gone as a concept. A search box that
// filters a chip cloud is a tell that the cloud has too many members to browse
// as chips; the honest form is a searchable LIST, which is what the popover
// holds — one type-ahead over one `role="listbox"` carrying both category
// tiers. There is exactly one such input in the component and it is inside the
// popover, never on screen next to the real search box.
//
// ---------------------------------------------------------------------------
// The frozen factory (docs/dev/screen-contract.md)
//
//   createSearchField({ onQueryChange, onCategoryChange })
//     -> { el, focus, setCategories, setBusy }
//
// Those four members are the contract and behave exactly as specified. Three
// things did not fit inside it and are added *additively*, so a host written
// against the frozen shape alone still works:
//
//   1. There is no channel for `SearchQuery.AppsOnly`. It is neither the text
//      nor the category, and the factory has only two callbacks. So both
//      callbacks are invoked with a SECOND argument — the complete query state
//      `{ text, category, appsOnly }` — and `getQuery()` returns the same
//      snapshot on demand. An apps-only change is reported through
//      `onQueryChange`, because that is what it is: a change to the query.
//
//   2. There is no way for the list to tell this component how many rows a
//      query matched. `setBusy` therefore takes an optional second argument
//      and `setResultCount()` exists as the explicit form. The count is not
//      only announced now — it is the visible result count in the toolbar,
//      which the redesign brief keeps.
//
//   3. The empty-results state in `picker-list.js` offers "clear the filters"
//      and the frozen shape gives it nothing to call. `clearFilters()` emits;
//      `clear()` is the silent form for a host that has already reset its own
//      query and only needs the controls brought back into line.
//
// `destroy()` is also exported so the composing screen can cancel this
// module's timers and its two document-level listeners on teardown.
//
// ---------------------------------------------------------------------------
// The debounce, and why it is 30 ms
//
// The budget (docs/dev/contract-brief.md) is: keystroke -> rendered results
// < 100 ms p95, INCLUDING the Wails bridge round trip. The debounce is spent
// from that budget before any work starts, so it is not a free knob.
//
// This used to be 60 ms, justified by a sum written at a desk: bridge
// "~8 ms typical, ~20-25 ms p95", render "~5 ms typical, ~10-12 ms p95",
// "~30-35 ms at p95", "that leaves ~65 ms". The conclusion was right by
// accident and every term in it was wrong. A later latency pass measured the
// real app on WebKit2GTK with real xdotool keystrokes, using the same definition of
// "rendered" the 103 ms figure used:
//
//   backend search        < 1 ms   (measured in Go; SearchResult.took_ms)
//   Wails bridge          9-10 ms  (not 20-25)
//   DOM write             2-3 ms   (not 10-12)
//   waiting for a frame   22-31 ms (not accounted for at all)
//
// The curve it produced, p95 on a quiet machine, against the 100 ms budget:
//
//   debounce   p95     margin
//    0 ms      50 ms   50 ms spare
//   30 ms      70 ms   30 ms spare   <- chosen
//   45 ms      86 ms   14 ms spare
//   60 ms     102 ms   MISSED by 2
//
// The finding that settles it: at any realistic typing speed the debounce
// coalesces nothing. 210 characters typed at 6.6 chars/s (about 79 wpm)
// produce 210 queries at every value from 0 to 60, with the Go work identical
// to within 3%; coalescing only begins above ~16 chars/s, roughly twice a fast
// typist's sustained rate. Key auto-repeat and a paste's single input event
// are still absorbed, which is what a debounce is actually for here. And the
// query it would have saved is Go against an in-memory index -- about 3.6 ms
// of local CPU and no network at all -- so "archive load" was never on this
// trade.
//
// Do not re-derive this from a sum. It cost a dedicated package and a quiet
// machine, and the arithmetic above is what a sum gets you. The number lives
// in one exported constant.
//
// ---------------------------------------------------------------------------
// Accessibility notes that are load-bearing
//
//   * The opener owns `aria-expanded` and `aria-controls`; the popover is
//     toggled with `el.hidden`, never `style.display` (section 6.17). Escape
//     and an outside click close it and focus returns to the opener.
//   * The category list is ONE tab stop: a `role="listbox"` with a roving
//     tabindex, arrow keys inside, manual activation. Seventy categories must
//     never become seventy tab stops (docs/accessibility.md section 10, P3.13).
//   * Selected state is driven from `aria-selected`, which is what the design
//     system's `.df-boxed-list__row[aria-selected="true"]` rule keys off, so
//     the visual state cannot drift from the accessible one. A selected row
//     also gains a leading tick: tint, a 3px rule and a glyph — three signals,
//     only one of which is colour.
//   * Nothing is `disabled`. A filter control with nothing to offer is
//     removed, not greyed out.
//
// Security: every string that reaches the DOM from the backend (a category
// name is archive metadata) is written with textContent or setAttribute. There
// is no innerHTML in this file at all, and no SVG is built from a string;
// icons are constructed with createElementNS. That is a property a reviewer
// can check with grep rather than by reading.

'use strict';

// ---------------------------------------------------------------------------
// Tunables. All timings live here.
// ---------------------------------------------------------------------------

/** Trailing debounce on the search input, in milliseconds. See the header. */
export const SEARCH_DEBOUNCE_MS = 30;

/**
 * Quiet period before the live region speaks. Long enough that typing a word
 * produces one announcement rather than one per letter; short enough that a
 * deliberate filter click is confirmed while the operator still cares.
 */
const ANNOUNCE_DEBOUNCE_MS = 600;

/**
 * Busy is shown only if the work outlasts this, and then stays for at least
 * BUSY_MIN_VISIBLE_MS. A spinner that flashes for 20 ms is noise, and a
 * spinner that flickers on every keystroke is worse than none.
 */
const BUSY_SHOW_DELAY_MS = 180;
const BUSY_MIN_VISIBLE_MS = 320;

/**
 * Above this many categories the popover grows a type-ahead over its list.
 * Ubuntu noble main+universe yields ~58 sections plus ~13 freedesktop main
 * categories, so it is on in practice; a target whose indexes carry a handful
 * of sections gets a plain list and no box to ignore.
 */
const TYPEAHEAD_THRESHOLD = 12;

const APP_PREFIX = 'app:';
const SECTION_PREFIX = 'section:';
const SVG_NS = 'http://www.w3.org/2000/svg';

const STYLE_ID = 'picker-search-layout';

/**
 * Layout only. Every component here comes from the design system; what follows
 * arranges them and defines no colour beyond one token-valued hairline that
 * separates the popover's two category groups. Injected once, keyed by id,
 * because `frontend/src/design/` is not this package to write.
 *
 * `.df-toolbar` does not wrap — reported to the design work. Reflow at a
 * 320px container is a WCAG 1.4.10 requirement, so the wrap is applied here.
 */
const LAYOUT_CSS = [
  '.picker-search { flex-wrap: wrap; row-gap: var(--space-2); }',
  '.picker-search__box { flex: 1 1 20ch; min-width: 0; }',
  '.picker-search__count { flex: 0 0 auto; margin-left: auto;',
  '  font-variant-numeric: tabular-nums; white-space: nowrap; }',
  '.picker-search__pop { display: flex; flex-direction: column;',
  '  gap: var(--space-3); max-height: min(70vh, 30rem); }',
  '.picker-search__list { flex: 0 1 auto; overflow: auto;',
  '  overscroll-behavior: contain; max-height: min(45vh, 17rem); }',
  // A group header strip inside a .df-boxed-list. The library has
  // .df-row--header for .df-list but nothing for a boxed list; reported.
  '.picker-search__grouphead { padding: var(--space-1) var(--space-4);',
  '  border-top: var(--border-width-hairline) solid var(--color-border-subtle); }',
  '.picker-search__tick { flex: none; }',
  '.picker-search__rowcount { margin-left: auto; flex: none; }',
  '.picker-search__note { margin: 0; }',
].join('\n');

function installLayoutCSS() {
  if (typeof document === 'undefined' || !document.head) return;
  if (document.getElementById(STYLE_ID)) return;
  const style = document.createElement('style');
  style.id = STYLE_ID;
  style.textContent = LAYOUT_CSS;
  document.head.appendChild(style);
}

let instanceSeq = 0;

let numberFormat = null;
try {
  numberFormat = new Intl.NumberFormat();
} catch (_) {
  numberFormat = null;
}

function fmtCount(n) {
  const v = Number(n);
  if (!isFinite(v)) return '0';
  return numberFormat ? numberFormat.format(v) : String(v);
}

function plural(n, one, many) {
  return n === 1 ? one : many;
}

// ---------------------------------------------------------------------------
// DOM helpers. Nothing here interpolates a string into markup.
// ---------------------------------------------------------------------------

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = String(text);
  return node;
}

/**
 * Build an inline SVG icon from literal path data. Sized with presentation
 * attributes so it renders correctly whether or not a component rule sizes it
 * — the design system ships no icons, only slots.
 */
function icon(paths, opts) {
  const o = opts || {};
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', o.strokeWidth || '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  if (o.className) svg.setAttribute('class', o.className);
  if (o.size) {
    svg.setAttribute('width', String(o.size));
    svg.setAttribute('height', String(o.size));
  }
  for (let i = 0; i < paths.length; i++) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', paths[i]);
    svg.appendChild(p);
  }
  return svg;
}

const ICON_SEARCH = ['M10.5 3a7.5 7.5 0 1 0 0 15 7.5 7.5 0 0 0 0-15Z', 'M21 21l-5.2-5.2'];
const ICON_CLOSE = ['M6 6l12 12', 'M18 6L6 18'];
const ICON_TICK = ['M20 6L9 17l-5-5'];
const ICON_FILTER = ['M3.5 6h17', 'M6.5 12h11', 'M10 18h4'];
const ICON_WARNING = ['M12 3.5 1.7 21h20.6L12 3.5Z', 'M12 10v4.5', 'M12 17.6v.01'];

// ---------------------------------------------------------------------------
// Category normalisation.
// ---------------------------------------------------------------------------

/**
 * Ids are opaque strings. The prefix is read only to decide which of the two
 * groups an entry belongs to and to label it; it is never rebuilt, split for
 * meaning, or compared to anything the backend did not send.
 */
function tierOf(cat) {
  if (cat.tier === 'application' || cat.tier === 'section') return cat.tier;
  if (typeof cat.id === 'string' && cat.id.indexOf(APP_PREFIX) === 0) return 'application';
  if (typeof cat.id === 'string' && cat.id.indexOf(SECTION_PREFIX) === 0) return 'section';
  // Unknown tier: file it with the complete grouping rather than dropping it.
  return 'section';
}

function nameOf(cat) {
  if (typeof cat.name === 'string' && cat.name !== '') return cat.name;
  const id = String(cat.id || '');
  if (id.indexOf(APP_PREFIX) === 0) return id.slice(APP_PREFIX.length);
  if (id.indexOf(SECTION_PREFIX) === 0) return id.slice(SECTION_PREFIX.length);
  return id;
}

function byName(a, b) {
  if (a.name === b.name) return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  return a.name < b.name ? -1 : 1;
}

// ---------------------------------------------------------------------------
// The factory.
// ---------------------------------------------------------------------------

/**
 * @param {object} handlers
 * @param {(text: string, query: {text: string, category: string, appsOnly: boolean}) => void} [handlers.onQueryChange]
 * @param {(category: string, query: {text: string, category: string, appsOnly: boolean}) => void} [handlers.onCategoryChange]
 */
export function createSearchField(handlers) {
  installLayoutCSS();

  const h = handlers || {};
  const onQueryChange = typeof h.onQueryChange === 'function' ? h.onQueryChange : null;
  const onCategoryChange = typeof h.onCategoryChange === 'function' ? h.onCategoryChange : null;

  const uid = 'df-picker-search-' + ++instanceSeq;

  // -- state ----------------------------------------------------------------

  const state = { text: '', category: '', appsOnly: false };
  /** The last triple actually handed to a callback, for de-duplication. */
  let lastEmitted = { text: '', category: '', appsOnly: false };

  let catalogueTotal = null; // CategoriesResult.total, when the host supplies it
  let appsTotal = null; // optional; not part of the documented result shape
  let appCats = [];
  let sectionCats = [];
  const catsById = new Map();
  /** Every option button in the list, in DOM order. */
  let optionEls = [];
  let categoriesLoaded = false;
  let categoryError = null;

  let resultTotal = null; // last count the host reported, if any
  let lastAnnounced = '';

  let open = false;
  let debounceTimer = 0;
  let announceTimer = 0;
  let busyShowTimer = 0;
  let busyHideTimer = 0;
  let busyWanted = false;
  let busyShown = false;
  let busyShownAt = 0;
  let destroyed = false;

  // -- root: ONE toolbar row ------------------------------------------------

  const root = el('div', 'df-toolbar picker-search');
  root.setAttribute('role', 'search');
  root.setAttribute('aria-label', 'Search and filter the catalogue');

  // -- 1. the search box ----------------------------------------------------

  const searchBox = el('div', 'df-search picker-search__box');
  searchBox.appendChild(icon(ICON_SEARCH, { className: 'df-search__icon' }));

  const input = document.createElement('input');
  input.id = uid + '-input';
  input.className = 'df-search__input';
  input.type = 'search';
  input.placeholder = 'Search packages';
  // A real label gives native screen readers an explicit label relationship.
  const inputLabel = el('label', 'df-visually-hidden', 'Search packages by name or summary');
  inputLabel.id = uid + '-label';
  inputLabel.htmlFor = input.id;
  input.setAttribute('aria-labelledby', inputLabel.id);
  searchBox.appendChild(inputLabel);
  input.autocomplete = 'off';
  input.spellcheck = false;
  input.setAttribute('autocorrect', 'off');
  input.setAttribute('autocapitalize', 'off');
  searchBox.appendChild(input);

  // Busy indicator. A wrapping span with no display-setting class, so the
  // [hidden] attribute is not overridden by a component rule.
  const busyWrap = el('span');
  busyWrap.hidden = true;
  const spinner = el('span', 'df-spinner');
  spinner.setAttribute('aria-hidden', 'true');
  busyWrap.appendChild(spinner);
  busyWrap.appendChild(el('span', 'df-visually-hidden', 'Searching'));
  searchBox.appendChild(busyWrap);

  const clearBtn = el('button', 'df-search__clear');
  clearBtn.type = 'button';
  clearBtn.hidden = true; // .df-search__clear[hidden] is handled by the library
  clearBtn.setAttribute('aria-label', 'Clear the search text');
  clearBtn.appendChild(icon(ICON_CLOSE, { size: 16 }));
  searchBox.appendChild(clearBtn);

  root.appendChild(searchBox);

  // -- 2. the ONE filter control -------------------------------------------

  const popId = uid + '-pop';
  const anchor = el('span', 'df-popover-anchor');

  const filterBtn = el('button', 'df-btn df-btn--secondary picker-search__filter');
  filterBtn.type = 'button';
  filterBtn.id = uid + '-filter';
  filterBtn.setAttribute('aria-expanded', 'false');
  filterBtn.setAttribute('aria-controls', popId);
  filterBtn.appendChild(icon(ICON_FILTER, { className: 'df-btn__icon' }));
  // The visible label IS the state. The hidden prefix makes the accessible
  // name say what the control is without breaking WCAG 2.5.3 (label in name):
  // the visible string is still a substring of the accessible name.
  filterBtn.appendChild(el('span', 'df-visually-hidden', 'Filter, currently '));
  const filterLabel = el('span', null, 'All packages');
  filterBtn.appendChild(filterLabel);
  anchor.appendChild(filterBtn);

  const pop = el('div', 'df-popover df-popover--end df-popover--wide picker-search__pop');
  pop.id = popId;
  pop.hidden = true;

  const popTitle = el('p', 'df-popover__title', 'Filter the catalogue');
  popTitle.id = uid + '-pop-title';
  pop.appendChild(popTitle);

  // 2a. the applications scope, at the top of the popover.
  const scopeWrap = el('div');
  const scopeLabel = el('label', 'df-check');
  const scopeInput = document.createElement('input');
  scopeInput.type = 'checkbox';
  scopeInput.className = 'df-check__input';
  scopeInput.id = uid + '-apps';
  scopeLabel.appendChild(scopeInput);
  scopeLabel.appendChild(el('span', 'df-check__label', 'Applications only'));
  const scopeCount = el('span', 'df-badge df-badge--count');
  scopeCount.setAttribute('aria-hidden', 'true');
  scopeCount.hidden = true;
  scopeLabel.appendChild(scopeCount);
  scopeWrap.appendChild(scopeLabel);
  const scopeNote = el(
    'p',
    'df-popover__text picker-search__note',
    'The packages the archive publishes desktop metadata for — a labelled '
      + 'subset with a human name and a category. Libraries, headers and '
      + 'command-line tools are outside it.',
  );
  scopeWrap.appendChild(scopeNote);
  pop.appendChild(scopeWrap);

  // 2b. the type-ahead over the category list. This is the searchable list's
  // own type-ahead, inside the popover — not a second filter box on screen.
  const typeWrap = el('div');
  typeWrap.hidden = true;
  const typeBox = el('div', 'df-search');
  typeBox.appendChild(icon(ICON_SEARCH, { className: 'df-search__icon' }));
  const typeInput = document.createElement('input');
  typeInput.id = uid + '-typeahead';
  typeInput.className = 'df-search__input';
  typeInput.type = 'search';
  typeInput.placeholder = 'Filter categories';
  typeInput.setAttribute('aria-label', 'Filter the category list by name');
  typeInput.setAttribute('aria-controls', uid + '-list');
  typeInput.autocomplete = 'off';
  typeInput.spellcheck = false;
  typeBox.appendChild(typeInput);
  typeWrap.appendChild(typeBox);
  pop.appendChild(typeWrap);

  // 2c. the list itself.
  const listEl = el('div', 'df-boxed-list picker-search__list');
  listEl.id = uid + '-list';
  listEl.setAttribute('role', 'listbox');
  listEl.setAttribute('aria-label', 'Category filter');
  pop.appendChild(listEl);

  const listEmpty = el('p', 'df-popover__text picker-search__note', 'No category name matches that.');
  listEmpty.hidden = true;
  pop.appendChild(listEmpty);

  // 2d. the one line that explains why the list is empty or unavailable.
  const statusWrap = el('div');
  statusWrap.hidden = true;
  const statusLine = el('p', 'df-popover__text picker-search__note');
  statusWrap.appendChild(statusLine);
  pop.appendChild(statusWrap);

  anchor.appendChild(pop);
  root.appendChild(anchor);

  // -- 3. the visible result count -----------------------------------------
  //
  // Not a live region: it updates on every keystroke and would chatter. The
  // announcement is the debounced `.df-live` below, which says the same thing
  // in a sentence.
  const countEl = el('span', 'df-text-sm df-text-secondary picker-search__count');
  root.appendChild(countEl);

  // -- 4. the live region ---------------------------------------------------
  //
  // Created empty and up front: a live region inserted at the same moment as
  // its text is unreliable across screen readers.
  const liveRegion = el('p', 'df-live');
  liveRegion.id = uid + '-live';
  liveRegion.setAttribute('role', 'status');
  liveRegion.setAttribute('aria-live', 'polite');
  liveRegion.setAttribute('aria-atomic', 'true');
  root.appendChild(liveRegion);

  // -------------------------------------------------------------------------
  // Emitting
  // -------------------------------------------------------------------------

  function snapshot() {
    return { text: state.text, category: state.category, appsOnly: state.appsOnly };
  }

  function sameAsEmitted() {
    return (
      lastEmitted.text === state.text
      && lastEmitted.category === state.category
      && lastEmitted.appsOnly === state.appsOnly
    );
  }

  function cancelDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer);
      debounceTimer = 0;
    }
  }

  /**
   * @param {'query'|'category'|'both'} kind which callback(s) to invoke.
   * Both callbacks receive the full query snapshot as their second argument.
   */
  function commit(kind) {
    cancelDebounce();
    if (sameAsEmitted()) return;
    const snap = snapshot();
    lastEmitted = snap;
    if (kind === 'category' || kind === 'both') {
      if (onCategoryChange) onCategoryChange(snap.category, snapshot());
    }
    if (kind === 'query' || kind === 'both') {
      if (onQueryChange) onQueryChange(snap.text, snapshot());
    }
  }

  /**
   * A filter changed. If the text debounce is still pending, the host has not
   * seen the newest text either, so tell it about both in one go rather than
   * letting the pending timer fire a second, superseded request.
   */
  function commitFilterChange(kindWhenTextIsCurrent) {
    const textPending = lastEmitted.text !== state.text;
    commit(textPending ? 'both' : kindWhenTextIsCurrent);
  }

  // -------------------------------------------------------------------------
  // The filter button's label — this is the active-filter indicator
  // -------------------------------------------------------------------------

  function categoryText(id) {
    const c = catsById.get(id);
    if (!c) return 'Category filter';
    return (c.tier === 'application' ? 'Application: ' : 'Section: ') + c.name;
  }

  /**
   * "All packages" | "Applications only" | "Section: devel" |
   * "Applications only · Section: devel".
   *
   * The two are independent in `SearchQuery`, so both can be on and the label
   * says so rather than picking a winner.
   */
  function filterLabelText() {
    const bits = [];
    if (state.appsOnly) bits.push('Applications only');
    if (state.category) bits.push(categoryText(state.category));
    return bits.length ? bits.join(' · ') : 'All packages';
  }

  function renderFilterButton() {
    const text = filterLabelText();
    if (filterLabel.textContent !== text) filterLabel.textContent = text;
    // A control with nothing to offer is removed, never greyed out: no
    // category data and no applications tier means this button cannot change
    // anything. It stays if a filter is somehow still set, so the operator can
    // always get back out of one.
    const usable = !categoriesLoaded
      || !!categoryError
      || appCats.length > 0
      || sectionCats.length > 0
      || state.appsOnly
      || state.category !== '';
    anchor.hidden = !usable;
    if (!usable && open) closePopover(false);
  }

  // -------------------------------------------------------------------------
  // The live region and the visible count
  // -------------------------------------------------------------------------

  function filterPhrase() {
    const bits = [];
    if (state.text) bits.push('matching “' + state.text + '”');
    if (state.category) {
      const c = catsById.get(state.category);
      if (c) bits.push('in ' + (c.tier === 'application' ? 'application category ' : 'section ') + c.name);
    }
    if (state.appsOnly) bits.push('in applications only');
    return bits.join(' ');
  }

  function currentMessage() {
    const phrase = filterPhrase();
    if (resultTotal != null) {
      if (resultTotal === 0) {
        return 'No packages ' + (phrase || 'in the catalogue')
          + '. Clear the filters to see the whole catalogue.';
      }
      const head = fmtCount(resultTotal) + plural(resultTotal, ' package', ' packages');
      return head + (phrase ? ' ' + phrase : ' in the catalogue') + '.';
    }
    if (!phrase) return 'Showing all packages.';
    const c = state.category ? catsById.get(state.category) : null;
    const extra = c
      ? ' ' + fmtCount(c.count) + plural(c.count, ' package', ' packages') + ' in that category.'
      : '';
    return 'Filtering ' + phrase + '.' + extra;
  }

  function renderCount() {
    const text = resultTotal == null
      ? ''
      : fmtCount(resultTotal) + plural(resultTotal, ' package', ' packages');
    if (countEl.textContent !== text) countEl.textContent = text;
  }

  function scheduleAnnounce(retry) {
    if (destroyed) return;
    const attempt = retry || 0;
    if (announceTimer) clearTimeout(announceTimer);
    announceTimer = setTimeout(function () {
      announceTimer = 0;
      // A search is still in flight and no count has landed: the useful answer
      // is probably milliseconds away, so wait for it rather than announcing
      // the filter and then the count. Bounded, so a host that never clears
      // busy still gets told something.
      if (busyWanted && resultTotal == null && attempt < 4) {
        scheduleAnnounce(attempt + 1);
        return;
      }
      const msg = currentMessage();
      if (msg === lastAnnounced) return;
      lastAnnounced = msg;
      liveRegion.textContent = msg;
    }, ANNOUNCE_DEBOUNCE_MS);
  }

  // -------------------------------------------------------------------------
  // Busy
  // -------------------------------------------------------------------------

  function renderBusy() {
    busyWrap.hidden = !busyShown;
    searchBox.setAttribute('aria-busy', busyShown ? 'true' : 'false');
  }

  function applyBusy() {
    if (destroyed) return;
    if (busyWanted) {
      if (busyHideTimer) {
        clearTimeout(busyHideTimer);
        busyHideTimer = 0;
      }
      if (!busyShown && !busyShowTimer) {
        busyShowTimer = setTimeout(function () {
          busyShowTimer = 0;
          if (!busyWanted) return;
          busyShown = true;
          busyShownAt = Date.now();
          renderBusy();
        }, BUSY_SHOW_DELAY_MS);
      }
      return;
    }
    if (busyShowTimer) {
      clearTimeout(busyShowTimer);
      busyShowTimer = 0;
    }
    if (busyShown && !busyHideTimer) {
      const remaining = Math.max(0, BUSY_MIN_VISIBLE_MS - (Date.now() - busyShownAt));
      busyHideTimer = setTimeout(function () {
        busyHideTimer = 0;
        if (busyWanted) return;
        busyShown = false;
        renderBusy();
      }, remaining);
    }
  }

  // -------------------------------------------------------------------------
  // The popover
  // -------------------------------------------------------------------------

  function onDocumentPointerDown(ev) {
    if (!open) return;
    if (anchor.contains(ev.target)) return;
    closePopover(false);
  }

  function onDocumentKeyDown(ev) {
    if (!open || ev.key !== 'Escape') return;
    ev.preventDefault();
    ev.stopPropagation();
    closePopover(true);
  }

  /** Focus left the control entirely (Tab out): close, but do not steal focus. */
  function onAnchorFocusOut(ev) {
    if (!open) return;
    const next = ev.relatedTarget;
    if (next && anchor.contains(next)) return;
    // relatedTarget is null when focus went to the document body or the window
    // lost focus altogether; closing then would fight the operator. A real Tab
    // out of the popover always names its destination.
    if (next === null) return;
    closePopover(false);
  }

  function openPopover() {
    if (open) return;
    open = true;
    pop.hidden = false;
    filterBtn.setAttribute('aria-expanded', 'true');
    document.addEventListener('pointerdown', onDocumentPointerDown, true);
    document.addEventListener('keydown', onDocumentKeyDown, true);
    // Land where the operator can act: the type-ahead when there is one,
    // otherwise the applications toggle, otherwise the list.
    if (!typeWrap.hidden) typeInput.focus();
    else if (!scopeWrap.hidden) scopeInput.focus();
    else {
      const items = visibleOptions();
      if (items.length) moveTo(items, 0);
    }
  }

  function closePopover(restoreFocus) {
    if (!open) return;
    open = false;
    pop.hidden = true;
    filterBtn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onDocumentPointerDown, true);
    document.removeEventListener('keydown', onDocumentKeyDown, true);
    if (restoreFocus && filterBtn.isConnected) filterBtn.focus();
  }

  filterBtn.addEventListener('click', function () {
    if (open) closePopover(true);
    else openPopover();
  });
  anchor.addEventListener('focusout', onAnchorFocusOut);

  // -------------------------------------------------------------------------
  // The category list
  // -------------------------------------------------------------------------

  /**
   * One option row. `.df-boxed-list__row` is the design system's activatable
   * row; `aria-selected` drives both the semantics and the tint plus the 3px
   * leading rule, and the tick is the third, non-colour signal.
   *
   * `role="option"` on a real `<button>` keeps native Enter/Space activation
   * (a UA behaviour of the element, not of the role) while giving the listbox
   * the child role it requires.
   */
  function makeOption(cat) {
    const btn = el('button', 'df-boxed-list__row');
    btn.type = 'button';
    btn.setAttribute('role', 'option');
    btn.setAttribute('aria-selected', 'false');
    btn.tabIndex = -1; // roving; the listbox is one tab stop
    btn.dataset.categoryId = cat.id;
    btn.dataset.categoryName = cat.name;

    const tick = icon(ICON_TICK, { size: 16, strokeWidth: '3', className: 'picker-search__tick' });
    tick.setAttribute('data-tick', '');
    tick.style.visibility = 'hidden';
    btn.appendChild(tick);

    const text = el('span', 'df-boxed-list__text');
    text.appendChild(el('span', 'df-boxed-list__label', cat.name));
    btn.appendChild(text);

    if (cat.count > 0) {
      const count = el('span', 'df-badge df-badge--count picker-search__rowcount', fmtCount(cat.count));
      count.setAttribute('aria-hidden', 'true'); // already in the accessible name
      btn.appendChild(count);
    }

    const kind = cat.id === '' ? '' : (cat.tier === 'application' ? ', application category' : ', section');
    btn.setAttribute(
      'aria-label',
      cat.name + kind
        + (cat.count > 0 ? ', ' + fmtCount(cat.count) + plural(cat.count, ' package', ' packages') : ''),
    );

    btn.addEventListener('click', function () {
      chooseCategory(cat.id);
    });
    return btn;
  }

  function groupHead(id, text) {
    const node = el('div', 'picker-search__grouphead df-text-sm df-text-secondary', text);
    node.id = id;
    return node;
  }

  /** Rebuild the list. Only called when the category set itself changed. */
  function renderList() {
    // Every option node is about to be discarded. If the operator is arrowing
    // through them at this moment — a catalogue rebuild can land while the
    // popover is open — focus would go to <body>. Park it somewhere real.
    const hadFocus = listEl.contains(document.activeElement);

    while (listEl.firstChild) listEl.removeChild(listEl.firstChild);
    optionEls = [];

    const all = makeOption({
      id: '',
      tier: 'section',
      name: 'All packages',
      count: catalogueTotal != null ? catalogueTotal : 0,
    });
    all.setAttribute(
      'aria-label',
      catalogueTotal != null
        ? 'All packages, ' + fmtCount(catalogueTotal) + ' in the catalogue'
        : 'All packages',
    );
    listEl.appendChild(all);
    optionEls.push(all);

    const apps = appCats.slice().sort(byName);
    const sections = sectionCats.slice().sort(byName);

    // The application tier first: it is what an operator who does not know
    // package names is looking for (docs/dev/binding-surface.md).
    if (apps.length) {
      const gid = uid + '-g-app';
      const group = el('div');
      group.setAttribute('role', 'group');
      group.setAttribute('aria-labelledby', gid);
      group.appendChild(groupHead(gid, 'Applications, by what a program does'));
      for (let i = 0; i < apps.length; i++) {
        const o = makeOption(apps[i]);
        group.appendChild(o);
        optionEls.push(o);
      }
      listEl.appendChild(group);
    }
    if (sections.length) {
      const gid = uid + '-g-sec';
      const group = el('div');
      group.setAttribute('role', 'group');
      group.setAttribute('aria-labelledby', gid);
      group.appendChild(groupHead(gid, 'Sections, from the target’s own indexes'));
      for (let i = 0; i < sections.length; i++) {
        const o = makeOption(sections[i]);
        group.appendChild(o);
        optionEls.push(o);
      }
      listEl.appendChild(group);
    }

    const many = apps.length + sections.length > TYPEAHEAD_THRESHOLD;
    typeWrap.hidden = !many;
    if (!many) typeInput.value = '';
    listEl.hidden = !(apps.length || sections.length);

    applyTypeahead();

    if (hadFocus) {
      const items = visibleOptions();
      if (items.length) moveTo(items, 0);
      else if (!typeWrap.hidden) typeInput.focus();
      else if (filterBtn.isConnected) filterBtn.focus();
    }
  }

  function visibleOptions() {
    const out = [];
    if (listEl.hidden) return out; // no categories at all: nothing to arrow to
    for (let i = 0; i < optionEls.length; i++) {
      if (optionEls[i].hidden) continue;
      out.push(optionEls[i]);
    }
    return out;
  }

  /** Selection changed but the list did not: repaint, keeping the nodes. */
  function paintOptions() {
    for (let i = 0; i < optionEls.length; i++) {
      const btn = optionEls[i];
      const on = (btn.dataset.categoryId || '') === state.category;
      btn.setAttribute('aria-selected', on ? 'true' : 'false');
      const tick = btn.querySelector('[data-tick]');
      if (tick) tick.style.visibility = on ? '' : 'hidden';
    }
    syncRoving();
  }

  /**
   * One tab stop for the whole listbox. Wherever the operator has arrowed to
   * stays the entry point; otherwise it is the selected option, otherwise the
   * first visible one.
   */
  function syncRoving() {
    const items = visibleOptions();
    if (!items.length) return;
    let target = null;
    if (items.indexOf(document.activeElement) >= 0) target = document.activeElement;
    for (let i = 0; !target && i < items.length; i++) {
      if (items[i].getAttribute('aria-selected') === 'true') target = items[i];
    }
    if (!target) target = items[0];
    for (let i = 0; i < optionEls.length; i++) {
      optionEls[i].tabIndex = optionEls[i] === target ? 0 : -1;
    }
  }

  function moveTo(items, index) {
    if (!items.length) return;
    const i = Math.min(Math.max(0, index), items.length - 1);
    for (let k = 0; k < optionEls.length; k++) {
      optionEls[k].tabIndex = optionEls[k] === items[i] ? 0 : -1;
    }
    items[i].focus();
  }

  listEl.addEventListener('keydown', function (ev) {
    if (ev.altKey || ev.ctrlKey || ev.metaKey) return;
    const items = visibleOptions();
    const at = items.indexOf(document.activeElement);
    if (at < 0) return;
    switch (ev.key) {
      case 'ArrowDown':
        ev.preventDefault();
        moveTo(items, at + 1);
        break;
      case 'ArrowUp':
        ev.preventDefault();
        if (at === 0 && !typeWrap.hidden) typeInput.focus();
        else moveTo(items, at - 1);
        break;
      case 'Home':
        ev.preventDefault();
        moveTo(items, 0);
        break;
      case 'End':
        ev.preventDefault();
        moveTo(items, items.length - 1);
        break;
      default:
    }
  });

  function applyTypeahead() {
    const needle = typeInput.value.trim().toLowerCase();
    let shown = 0;
    for (let i = 0; i < optionEls.length; i++) {
      const btn = optionEls[i];
      // "All packages" is the way out of a filter; a type-ahead over category
      // names never hides it.
      const always = (btn.dataset.categoryId || '') === '';
      const name = String(btn.dataset.categoryName || '').toLowerCase();
      const match = always || needle === '' || name.indexOf(needle) !== -1;
      btn.hidden = !match;
      if (match && !always) shown++;
    }
    // A group whose every option is hidden should not announce its heading.
    const groups = listEl.querySelectorAll('[role="group"]');
    for (let i = 0; i < groups.length; i++) {
      const opts = groups[i].querySelectorAll('[role="option"]');
      let any = false;
      for (let k = 0; k < opts.length && !any; k++) any = !opts[k].hidden;
      groups[i].hidden = !any;
    }
    listEmpty.hidden = shown !== 0 || needle === '';
    syncRoving();
  }

  typeInput.addEventListener('input', applyTypeahead);
  typeInput.addEventListener('keydown', function (ev) {
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      moveTo(visibleOptions(), 0);
      return;
    }
    if (ev.key === 'Escape' && typeInput.value !== '') {
      // Clear the type-ahead first; a second Escape closes the popover.
      ev.preventDefault();
      ev.stopPropagation();
      typeInput.value = '';
      applyTypeahead();
    }
  });

  // -------------------------------------------------------------------------
  // Interaction
  // -------------------------------------------------------------------------

  function updateClearButton() {
    clearBtn.hidden = input.value === '';
  }

  input.addEventListener('input', function () {
    state.text = input.value;
    updateClearButton();
    cancelDebounce();
    debounceTimer = setTimeout(function () {
      debounceTimer = 0;
      commit('query');
    }, SEARCH_DEBOUNCE_MS);
    // A new query invalidates the count the host last reported.
    resultTotal = null;
    renderCount();
    scheduleAnnounce();
  });

  input.addEventListener('keydown', function (event) {
    if (event.key === 'Escape') {
      if (input.value !== '') {
        // Consume it: the operator meant "clear this box", not "leave".
        event.preventDefault();
        event.stopPropagation();
        clearText();
      }
      return; // empty already: let it bubble, so the screen above can act
    }
    if (event.key === 'Enter') {
      // A deliberate "go". Do not wait out the debounce.
      event.preventDefault();
      state.text = input.value;
      commit('query');
    }
  });

  clearBtn.addEventListener('click', function () {
    clearText();
  });

  function clearText() {
    input.value = '';
    state.text = '';
    updateClearButton();
    resultTotal = null;
    renderCount();
    // Clearing is a discrete act, not a stream: no debounce.
    commit('query');
    scheduleAnnounce();
    // Pointer users clicked the button; put the caret back where they type.
    input.focus();
  }

  function setAppsOnly(next, announce) {
    next = !!next;
    if (next === state.appsOnly) return;
    state.appsOnly = next;
    if (scopeInput.checked !== next) scopeInput.checked = next;
    renderFilterButton();
    commitFilterChange('query');
    if (announce !== false) scheduleAnnounce();
  }

  scopeInput.addEventListener('change', function () {
    setAppsOnly(scopeInput.checked);
  });

  /**
   * Choosing a category is a terminal act, so the popover closes and focus
   * goes back to the button — whose label now states what was chosen. That
   * label is the whole reason there is no "active filters" row any more.
   */
  function chooseCategory(id) {
    const next = typeof id === 'string' ? id : '';
    if (next !== state.category) {
      state.category = next;
      resultTotal = null;
      renderCount();
      paintOptions();
      renderFilterButton();
      commitFilterChange('category');
      scheduleAnnounce();
    }
    closePopover(true);
  }

  // -------------------------------------------------------------------------
  // Rendering the popover's own state
  // -------------------------------------------------------------------------

  function renderStatusLine() {
    while (statusLine.firstChild) statusLine.removeChild(statusLine.firstChild);
    statusLine.className = 'df-popover__text picker-search__note';
    if (categoryError) {
      // Colour is never the only signal: a triangle icon and words as well.
      statusLine.className = 'df-status df-status--warning picker-search__note';
      statusLine.appendChild(icon(ICON_WARNING, { size: 16 }));
      statusLine.appendChild(el('span', null,
        'Categories are unavailable — ' + categoryError + ' Searching by name still works.'));
      statusWrap.hidden = false;
      return;
    }
    if (!categoriesLoaded) {
      statusLine.textContent = 'Loading categories…';
      statusWrap.hidden = false;
      return;
    }
    if (!appCats.length && !sectionCats.length) {
      statusLine.textContent =
        'This target’s indexes carry no category data. Search by package name instead.';
      statusWrap.hidden = false;
      return;
    }
    if (!appCats.length) {
      statusLine.textContent =
        'This target’s indexes carry no desktop application metadata, so there is no '
        + 'applications tier.';
      statusWrap.hidden = false;
      return;
    }
    statusWrap.hidden = true;
  }

  function renderScope() {
    if (scopeInput.checked !== state.appsOnly) scopeInput.checked = state.appsOnly;
    const known = appsTotal != null;
    scopeCount.hidden = !known;
    if (known) scopeCount.textContent = fmtCount(appsTotal);
    // No application metadata means the apps-only scope can only ever be
    // empty. Take the row away rather than offering a dead control.
    const possible = !categoriesLoaded || appCats.length > 0 || known || state.appsOnly;
    scopeWrap.hidden = !possible;
  }

  function renderAll() {
    renderList();
    paintOptions();
    renderScope();
    renderFilterButton();
    renderStatusLine();
  }

  // -------------------------------------------------------------------------
  // Public surface
  // -------------------------------------------------------------------------

  /**
   * CONTRACT. Focus the search input, caret at the end of whatever is there.
   *
   * Deliberately a no-op when focus is already inside this component: `show()`
   * runs on every navigation back to the picker, and stealing focus out of the
   * open filter popover is precisely the behaviour the brief calls infuriating.
   */
  function focus() {
    if (root.contains(document.activeElement)) return;
    input.focus();
    const n = input.value.length;
    try {
      input.setSelectionRange(n, n);
    } catch (_) {
      /* some engines refuse on type=search; the caret default is fine */
    }
  }

  /**
   * CONTRACT. Accepts any of the three shapes a caller might reasonably use:
   *
   *   setCategories(categoriesResult)          // the whole binding result
   *   setCategories(categoryArray, total)      // the array plus the count
   *   setCategories(categoryArray)             // the array alone
   *
   * Empty categories are dropped rather than offered. If the current filter
   * names a category that is no longer in the list it is cleared and
   * `onCategoryChange('')` fires — the host would otherwise be left paging a
   * filter that can never match.
   */
  function setCategories(incoming, totalArg) {
    let list = [];
    categoryError = null;
    catalogueTotal = null;
    appsTotal = null;

    // No `> 0` guard. CatalogStatus.PackageCount is assigned on both the cache
    // and the build path now, so `total: 0` is a fact — an empty catalogue —
    // rather than the "never assigned" it used to mean. The guard, and the
    // section-sum fallback that used to fire behind it, were a screen standing
    // in for an engine gap; the brief forbids that, and the sum was not even
    // equivalent, because a package with no `Section:` is in the catalogue and
    // not in any section count.
    if (typeof totalArg === 'number' && isFinite(totalArg)) {
      catalogueTotal = totalArg;
    }

    if (Array.isArray(incoming)) {
      list = incoming;
    } else if (incoming && typeof incoming === 'object') {
      list = Array.isArray(incoming.categories) ? incoming.categories : [];
      if (typeof incoming.total === 'number') catalogueTotal = incoming.total;
      if (typeof incoming.apps_total === 'number') appsTotal = incoming.apps_total;
      else if (typeof incoming.appsTotal === 'number') appsTotal = incoming.appsTotal;
      if (incoming.error) {
        categoryError =
          (typeof incoming.error.summary === 'string' && incoming.error.summary)
          || (typeof incoming.error.message === 'string' && incoming.error.message)
          || 'the catalogue did not report them.';
      }
    }

    appCats = [];
    sectionCats = [];
    catsById.clear();

    for (let i = 0; i < list.length; i++) {
      const raw = list[i];
      if (!raw || typeof raw.id !== 'string' || raw.id === '') continue;
      const count = Number(raw.count) || 0;
      if (count <= 0) continue; // an empty category is not an offer
      const cat = { id: raw.id, tier: tierOf(raw), name: nameOf(raw), count: count };
      catsById.set(cat.id, cat);
      if (cat.tier === 'application') appCats.push(cat);
      else sectionCats.push(cat);
    }

    categoriesLoaded = true;
    // A filter that survived a catalogue rebuild but no longer exists would
    // leave the host paging a query that can never match. Drop it and say so.
    const droppedCategory = state.category !== '' && !catsById.has(state.category);
    if (droppedCategory) state.category = '';
    const droppedApps = appCats.length === 0 && state.appsOnly;
    if (droppedApps) state.appsOnly = false;

    renderAll();
    if (droppedCategory || droppedApps) {
      resultTotal = null;
      renderCount();
      commit(droppedCategory && droppedApps ? 'both' : droppedCategory ? 'category' : 'query');
      scheduleAnnounce();
    }
  }

  /**
   * CONTRACT: `setBusy(busy)` shows that the field is working. It never
   * disables the input, never clears it, and never moves focus — typing
   * continues through a search exactly as it does between searches.
   *
   * ADDITIVE: an optional second argument carries the result count, since the
   * frozen shape has no other channel for it. Accepts a number, or a
   * SearchResult-ish object with `total` (and optionally `text` or
   * `query.text`, which is used to drop a stale count).
   */
  function setBusy(busy, info) {
    if (info !== undefined && info !== null) setResultCount(info);
    const next = !!busy;
    if (next !== busyWanted) {
      busyWanted = next;
      applyBusy();
    }
  }

  /**
   * ADDITIVE. Tell the component how many rows the current query matched. This
   * is both the toolbar's visible count and what the live region announces.
   */
  function setResultCount(info) {
    if (info == null) { resultTotal = null; renderCount(); return; }
    let total = null;
    if (typeof info === 'number') {
      total = info;
    } else if (info && typeof info === 'object') {
      if (typeof info.total === 'number') total = info.total;
      else if (typeof info.count === 'number') total = info.count;
      const echoed = typeof info.text === 'string'
        ? info.text
        : info.query && typeof info.query.text === 'string'
          ? info.query.text
          : null;
      // The bridge is asynchronous and pages can land out of order; a count
      // for text the operator has already moved past must not be shown.
      if (echoed != null && echoed !== state.text) return;
    }
    if (total == null || !isFinite(total)) return;
    resultTotal = total;
    renderCount();
    scheduleAnnounce();
  }

  /**
   * ADDITIVE. Clear the category and the apps-only scope. This is what the
   * empty-results state in `picker-list.js` calls from its "clear the filters"
   * action; the frozen shape gives it nothing else to call.
   *
   * The search text is left alone unless `includeText` is set, because
   * "no results for `libssl`" is usually a filter problem, not a typo, and
   * throwing away what someone typed is not a recovery.
   */
  function clearFilters(opts) {
    const includeText = !!(opts && opts.includeText);
    const had = state.category !== '' || state.appsOnly || (includeText && state.text !== '');
    if (!had) return;
    const categoryChanged = state.category !== '';
    state.category = '';
    state.appsOnly = false;
    if (includeText) {
      state.text = '';
      input.value = '';
      updateClearButton();
    }
    resultTotal = null;
    renderCount();
    if (typeInput.value !== '') {
      typeInput.value = '';
      applyTypeahead();
    }
    paintOptions();
    renderScope();
    renderFilterButton();
    commit(categoryChanged ? 'both' : 'query');
    scheduleAnnounce();
  }

  /**
   * ADDITIVE, and deliberately silent.
   *
   * `picker-list.js` resets its own `_query` to the empty query and then calls
   * this to bring the visible controls back into line. It fetches immediately
   * afterwards, so emitting here would only produce a second, superseded
   * request — and a control still labelled "Section: devel" while the list
   * shows everything is exactly the ambiguity this component exists to prevent.
   */
  function clear() {
    state.text = '';
    state.category = '';
    state.appsOnly = false;
    input.value = '';
    lastEmitted = snapshot();
    resultTotal = null;
    updateClearButton();
    renderCount();
    if (typeInput.value !== '') {
      typeInput.value = '';
      applyTypeahead();
    }
    paintOptions();
    renderScope();
    renderFilterButton();
    scheduleAnnounce();
  }

  /** ADDITIVE. The complete query state, for a host that wants to pull it. */
  function getQuery() {
    return snapshot();
  }

  /** Silent full-state setter for category shortcuts and target changes. */
  function setQuery(query) {
    cancelDebounce();
    state.text = query?.text == null ? '' : String(query.text);
    state.category = query?.category == null ? '' : String(query.category);
    state.appsOnly = !!query?.appsOnly;
    input.value = state.text;
    lastEmitted = snapshot();
    resultTotal = null;
    updateClearButton();
    paintOptions();
    renderScope();
    renderFilterButton();
    renderCount();
    scheduleAnnounce();
  }

  /** ADDITIVE. Cancel this module's timers and its document listeners. */
  function destroy() {
    destroyed = true;
    closePopover(false);
    cancelDebounce();
    if (announceTimer) clearTimeout(announceTimer);
    if (busyShowTimer) clearTimeout(busyShowTimer);
    if (busyHideTimer) clearTimeout(busyHideTimer);
    announceTimer = busyShowTimer = busyHideTimer = 0;
  }

  // Initial paint.
  renderAll();
  updateClearButton();
  renderBusy();
  renderCount();

  return {
    setQuery,
    // The frozen four.
    el: root,
    focus: focus,
    setCategories: setCategories,
    setBusy: setBusy,
    // Additive; see the note at the top of the file.
    setResultCount: setResultCount,
    clear: clear,
    clearFilters: clearFilters,
    getQuery: getQuery,
    destroy: destroy,
  };
}
