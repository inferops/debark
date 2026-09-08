// picker-list.js — the `picker` screen module, and the virtualiser it is built
// around. 
//
// Two exports:
//
//   createVirtualList(options)  the virtualiser. Knows nothing about Wails,
//                               nothing about the catalogue, and nothing about
//                               the selection tray. It is handed a `search`
//                               function and a `renderRow` function and it
//                               scrolls. `picker-list.demo.html` drives it
//                               against a synthetic 60,000-row backend to
//                               measure the frame budget; the screen below
//                               drives it against `SearchPackages`.
//
//   default                     the `picker` screen module (screen-contract.md),
//                               which composes `picker-search.js` and
//                               `picker-tray.js` around the list.
//
// ---------------------------------------------------------------------------
// The performance promise, and how this file keeps it
// ---------------------------------------------------------------------------
//
// The budget is: a 60,000-row result set scrolls with no dropped frames, and a
// keystroke reaches rendered results in under 100 ms p95 including the bridge.
// Five properties of this file are load-bearing for that and none of them are
// optimisations that can be dropped later:
//
//  1. **The frontend never holds the result set.** Only `total` crosses the
//     bridge in full; rows arrive ~50 at a time and a bounded number of pages
//     is retained (`maxCachedPages`). Scrolling from one end of a 60,000-row
//     result to the other allocates no more memory at the end than at the
//     start. Page eviction is not a nicety — without it this is a slow leak
//     that reintroduces exactly the problem virtualising was meant to solve.
//
//  2. **Row elements are recycled, never created during scroll.** A pool of
//     `viewport + 2*overscan` elements is allocated once per resize. A scroll
//     pass reassigns indexes; it does not touch `createElement`. Allocation
//     during scroll is what produces a GC pause, and a GC pause is a dropped
//     frame.
//
//  3. **Every write to the DOM is guarded by a comparison.** A recycled row
//     that lands on the same index with the same data is not rewritten. This
//     is what makes "rows rendered per frame" a number worth measuring: during
//     a steady scroll it is the two or three rows that actually entered the
//     window, not the forty that are in it.
//
//  4. **One read, then only writes.** `scrollTop` is read once at the top of
//     an update pass. The viewport height comes from a `ResizeObserver`, not
//     from `clientHeight` on every frame, so a pass never interleaves reads
//     and writes and never forces a synchronous layout.
//
//  5. **Rendering is a pure function of (index, cache, generation).** A slot
//     never keeps data from the index it used to show. When the page covering
//     an index is not resident the slot renders a skeleton at the right height
//     and the row fills in later. There is no code path that can paint row N's
//     content at position M — which is the bug that makes a virtualiser feel
//     haunted, and the reason this file is written the way it is rather than
//     as a diff against the previous scroll position.
//
// Out-of-order responses are dropped twice over: by a monotonic generation
// counter bumped on every query change, and by comparing the query the backend
// echoes back (`SearchResult.Query`) against the query that is current at the
// moment the response lands. The echo exists in the binding surface for
// exactly this and it is not optional — a debounced search box fires several
// requests and they complete in whatever order the catalogue felt like.
//
// Escaping: nothing from the backend is ever concatenated into markup. Every
// backend string reaches the DOM through `textContent` or `setAttribute`.
// There is no `innerHTML` in this file at all; the icons are built with
// `createElementNS`.

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/** Fallback row height, used only if `--row-height` cannot be read. Kept equal
 *  to the token's value so a missing stylesheet degrades rather than tears. */
import { bindDisclosure } from '../shell/disclosure.js';
import { observeSelectionLayout } from '../shell/layout.js';
import { createSearchField } from './picker-search.js';
import { createTray } from './picker-tray.js';

const ROW_HEIGHT_FALLBACK = 32;

/** Page size. Matches `app.SearchPageDefault`; `SearchPageMax` is 500. */
const PAGE_SIZE = 50;

/** Rows rendered above and below the viewport. Six rows is ~190 px of buffer:
 *  enough to cover a scroll delta between two frames at ordinary wheel speed,
 *  small enough that the pool stays around forty elements. */
const OVERSCAN = 6;

/** Pages retained. 24 * 50 = 1200 rows ≈ 1200 small objects. The window needs
 *  two or three; the rest is so that a short scroll back does not refetch. */
const MAX_CACHED_PAGES = 24;

/** Above this many pixels of scroll between two frames we are in a fling and
 *  stop issuing page requests: the pages we would ask for will be off screen
 *  before they arrive. Requests resume when the scroll settles. */
const FLING_PX_PER_FRAME = 320;

/** How long after the last scroll event we consider the scroll settled. */
const SETTLE_MS = 90;

/** Attempts before a page that keeps coming back short is left alone. */
const MAX_PAGE_ATTEMPTS = 3;

/** A response that is dropped is a page nobody is going to deliver: nothing
 *  else will ask for it again, because the request completed. These two bound
 *  the re-ask. Without them a single dropped first page leaves the list
 *  showing skeletons for ever, which the harness's chaotic-backend mode found
 *  and which a real backend can produce with a timeout or a stale echo. */
const RECOVERY_MS = 120;
const MAX_RECOVERY_ATTEMPTS = 8;

/** Retained page-latency samples, for `stats()`. Bounded so the harness cannot
 *  turn the instrumentation itself into the leak it is looking for. */
const LATENCY_RING = 512;

/** Browsers clamp the height of a single element. Chromium's limit is around
 *  33.5 M px and WebKit's is in the same range; 15 M px is comfortably under
 *  both and is 468,750 rows at a 32 px row — two orders of magnitude past the
 *  ~70,000 packages in Ubuntu main + universe. If a result set ever exceeds
 *  it the list still scrolls, it just stops being able to reach the last rows,
 *  so we warn rather than fail silently. */
const MAX_SIZER_PX = 15000000;

const WARN_SNAP_TRANSITIONAL = 'snap-transitional';

// ---------------------------------------------------------------------------
// Small DOM helpers. No innerHTML anywhere.
// ---------------------------------------------------------------------------

const SVG_NS = 'http://www.w3.org/2000/svg';

function el(tag, className, attrs) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (attrs) for (const k in attrs) node.setAttribute(k, attrs[k]);
  return node;
}

/** An inline SVG icon from a static path string. The `d` values below are
 *  literals in this file, never backend data. */
function icon(className, paths, opts) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', (opts && opts.width) || '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  if (className) svg.setAttribute('class', className);
  for (const d of paths) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', d);
    svg.appendChild(p);
  }
  return svg;
}

const ICON_TRIANGLE = ['M12 3.5 22 20H2Z', 'M12 10v4', 'M12 17.2v.1'];
const ICON_BOX = ['M21 8 12 3 3 8v8l9 5 9-5Z', 'M3 8l9 5 9-5', 'M12 13v8'];
const ICON_OCTAGON = ['M8 2h8l6 6v8l-6 6H8l-6-6V8Z', 'M9 9l6 6', 'M15 9l-6 6'];

/** Set `textContent` only when it actually changed. Every row write goes
 *  through this: a recycled row that lands on identical data costs nothing. */
function setText(node, value) {
  const v = value == null ? '' : String(value);
  if (node.textContent !== v) node.textContent = v;
  return node;
}

function setAttr(node, name, value) {
  if (value == null || value === false) {
    if (node.hasAttribute(name)) node.removeAttribute(name);
    return;
  }
  const v = value === true ? 'true' : String(value);
  if (node.getAttribute(name) !== v) node.setAttribute(name, v);
}

function setHidden(node, hidden) {
  if (node.hidden !== hidden) node.hidden = hidden;
}

/**
 * Read `--row-height` from CSS rather than hardcoding it, so the token and the
 * virtualiser cannot drift. The token is the contract (design/README.md §6.5):
 * the virtualiser computes `floor(scrollTop / rowHeight)` and sizes a spacer
 * from it, and a rendered row box that stops matching the token tears the list.
 *
 * `context` is any element inside the document so that a per-screen density
 * override, if one is ever added, is picked up.
 */
export function readRowHeight(context) {
  const probe = context && context.nodeType === 1 ? context : document.documentElement;
  let raw = '';
  try {
    raw = getComputedStyle(probe).getPropertyValue('--row-height');
  } catch (_) {
    raw = '';
  }
  const n = parseFloat(String(raw).trim());
  if (Number.isFinite(n) && n > 0) return Math.round(n);
  return ROW_HEIGHT_FALLBACK;
}

/** Normalise a query from either side of the bridge. The backend echoes json
 *  tags (`apps_only`); this module speaks `appsOnly` internally. */
function normQuery(q) {
  if (!q) return { text: '', category: '', appsOnly: false };
  const apps = q.appsOnly !== undefined ? q.appsOnly : q.apps_only;
  return {
    text: q.text == null ? '' : String(q.text),
    category: q.category == null ? '' : String(q.category),
    appsOnly: !!apps,
  };
}

function sameQuery(a, b) {
  return a.text === b.text && a.category === b.category && a.appsOnly === b.appsOnly;
}

function quantile(sorted, p) {
  if (!sorted.length) return 0;
  const i = Math.min(sorted.length - 1, Math.max(0, Math.ceil(p * sorted.length) - 1));
  return sorted[i];
}

/** Summarise a sample set. Used for both frame timings and page latencies, so
 *  the harness and the app report the same shape. */
export function summarise(samples) {
  const s = Array.prototype.slice.call(samples).sort((a, b) => a - b);
  const n = s.length;
  if (!n) return { n: 0, mean: 0, p50: 0, p95: 0, p99: 0, max: 0 };
  let sum = 0;
  for (let i = 0; i < n; i++) sum += s[i];
  return {
    n,
    mean: sum / n,
    p50: quantile(s, 0.5),
    p95: quantile(s, 0.95),
    p99: quantile(s, 0.99),
    max: s[n - 1],
  };
}

// ---------------------------------------------------------------------------
// The virtualiser
// ---------------------------------------------------------------------------

/**
 * @typedef {Object} VirtualListOptions
 * @property {(req: {text:string, category:string, apps_only:boolean, offset:number, limit:number}) => Promise<Object>} search
 *   Fetches one page. Must resolve to something shaped like `app.SearchResult`:
 *   `{rows, total, offset, limit, query, error}`. This is the only seam between
 *   the virtualiser and the backend — `picker-list.demo.html` swaps a synthetic
 *   implementation in here and nothing else changes.
 * @property {string} [label]              accessible name for the listbox
 * @property {number} [pageSize]
 * @property {number} [overscan]
 * @property {number} [maxCachedPages]
 * @property {() => Element} [createSkeleton]  one `--row-height` placeholder row
 * @property {(row:Object) => boolean} [isSelected]  selection is backend truth;
 *   the list asks rather than remembering
 * @property {(index:number, row:Object) => void} [onToggle]
 * @property {(index:number, row:Object) => void} [onActivate]
 * @property {(index:number, row:Object|undefined) => void} [onFocusRow]
 * @property {(total:number, result:Object) => void} [onTotal]
 * @property {(err:Object) => void} [onError]     a `UIError` from the backend
 * @property {(sample:Object) => void} [onMetrics] one call per update pass
 */

/**
 * Build a virtualised list.
 *
 * The DOM is exactly the structure `design/components.css` documents:
 *
 *     .df-list                       bounded box
 *       .df-vlist.df-scroller        the scroll port, role=listbox
 *         .df-vlist__sizer           height = total * rowHeight
 *           .df-vlist__window        transform: translateY(first * rowHeight)
 *             .df-row | .df-skeleton-row   x (viewport + 2*overscan)
 *
 * @param {VirtualListOptions} options
 */
export function createVirtualList(options) {
  const opts = options || {};
  const search = opts.search;
  if (typeof search !== 'function') {
    throw new TypeError('createVirtualList: options.search is required');
  }

  const pageSize = opts.pageSize > 0 ? Math.floor(opts.pageSize) : PAGE_SIZE;
  const overscan = opts.overscan >= 0 ? Math.floor(opts.overscan) : OVERSCAN;
  const maxCachedPages = opts.maxCachedPages > 0 ? Math.floor(opts.maxCachedPages) : MAX_CACHED_PAGES;
  const isSelected = typeof opts.isSelected === 'function' ? opts.isSelected : (row) => !!(row && row.selected);
  const createSkeleton = typeof opts.createSkeleton === 'function' ? opts.createSkeleton : defaultSkeleton;

  // -- DOM ------------------------------------------------------------------

  const listEl = el('div', 'df-list picker-list');
  const port = el('div', 'df-vlist df-scroller', {
    role: 'listbox',
    'aria-multiselectable': 'true',
    'aria-label': opts.label || 'Package catalogue',
    tabindex: '0',
  });
  // The sizer and window are layout, not structure: marking them presentational
  // keeps the role=option rows owned by the role=listbox port.
  const sizer = el('div', 'df-vlist__sizer', { role: 'presentation' });
  const windowEl = el('div', 'df-vlist__window', { role: 'presentation' });
  sizer.appendChild(windowEl);
  port.appendChild(sizer);
  listEl.appendChild(port);

  // -- state ----------------------------------------------------------------

  let rowHeight = ROW_HEIGHT_FALLBACK;
  let viewportH = 0;
  let total = -1; // -1 = not known yet
  let generation = 0;
  let query = normQuery(null);
  let destroyed = false;

  /** pageIndex -> {rows, complete, attempts, lru} */
  const pages = new Map();
  /** pageIndex -> generation of the request in flight */
  const inflight = new Map();
  let lruClock = 0;

  /** Every slot ever allocated. Each slot owns one `.df-row` and one
   *  `.df-skeleton-row`; exactly one of the two is in the document at any
   *  moment. Swapping the two is one `replaceChild`, and only happens when a
   *  slot changes kind. The pool only ever grows, and only to the size the
   *  viewport demands. */
  const pool = [];
  /** The mounted slots, in DOM order: `order[i]` shows row `firstIndex + i`. */
  const order = [];
  let liveSlots = 0;

  let firstIndex = 0;
  /** What `order` was last assigned, so a scroll can be applied as a rotation
   *  rather than as a full reassignment. NaN forces the next pass to reassign. */
  let assignedFirst = NaN;
  let focusIndex = -1;

  let scheduled = false;
  let focusRefreshFrame = 0;
  let focusRefreshSequence = 0;
  let focusRefreshCleared = false;
  let lastScrollTop = 0;
  let settleTimer = 0;
  let pendingFetch = false;
  let recoveryTimer = 0;
  let recoveryAttempts = 0;
  let recoveryReported = false;
  /** True once a query has actually been asked for. The list never searches on
   *  its own: the screen decides when the catalogue is ready to be asked. */
  let started = false;
  let sizerPx = 0;
  let sizerClamped = false;

  const stats = {
    updates: 0,
    rowsRendered: 0,
    skeletonsRendered: 0,
    kindSwaps: 0,
    nodeMoves: 0,
    fullReassigns: 0,
    pageRequests: 0,
    pagesDropStale: 0,
    pagesDropEcho: 0,
    pagesDropMisaligned: 0,
    pagesEvicted: 0,
    pageErrors: 0,
    latencies: [],
  };

  function recordLatency(ms) {
    stats.latencies.push(ms);
    if (stats.latencies.length > LATENCY_RING) stats.latencies.shift();
  }

  // -- skeleton -------------------------------------------------------------

  function defaultSkeleton() {
    const node = el('div', 'df-skeleton-row');
    node.appendChild(el('span', 'df-skeleton df-skeleton--check'));
    // Fixed widths per slot so a recycled skeleton does not shimmer-shift, and
    // varied across slots so a loading list does not look like a barcode.
    const widths = ['78%', '64%', '88%'];
    for (const w of widths) {
      const s = el('span', 'df-skeleton');
      s.style.width = w;
      node.appendChild(s);
    }
    const tail = el('span', 'df-skeleton');
    tail.style.width = '44px';
    node.appendChild(tail);
    return node;
  }

  function makeSkeleton(slotNo) {
    let node;
    try {
      node = createSkeleton(slotNo);
    } catch (_) {
      node = null;
    }
    if (!node || node.nodeType !== 1) node = defaultSkeleton();
    // Whatever `shell/states.js` hands back, the ARIA and the identity are
    // ours: the list owns the listbox and a placeholder still occupies a
    // position in it.
    node.setAttribute('role', 'option');
    node.setAttribute('aria-busy', 'true');
    node.setAttribute('aria-selected', 'false');
    node.setAttribute('aria-label', 'Loading');
    return node;
  }

  // -- row elements ---------------------------------------------------------

  function makeSlot(slotNo) {
    // NO `tabindex` on the row, deliberately.
    //
    // This listbox drives its cursor with `aria-activedescendant`, so DOM
    // focus belongs on the port and nowhere else, and `.df-row.is-focused`
    // draws the ring. `tabindex="-1"` does not only keep an element out of the
    // tab order — it makes it *click*-focusable. A click on a row therefore
    // moved DOM focus into the recycled pool, and the next scroll moved that
    // node with `appendChild`, which blurs it: focus landed on <body> and the
    // arrow keys stopped working entirely. That is the classic recycling
    // focus-loss bug, and not having a tabindex here is the fix. `onPointerDown`
    // below keeps focus on the port for the checkbox, which is natively
    // click-focusable whatever we do, and `update()` carries a last-resort
    // guard.
    //
    // components.css's own comment at section 12 shows `tabindex="-1"` on a
    // `.df-row`; README section 6.5 does not. Reported to the design work.
    const row = el('div', 'df-row df-picker-row', {
      role: 'option',
      'aria-selected': 'false',
      id: 'picker-row-slot-' + slotNo,
    });

    const check = el('input', 'df-check__input df-row__check');
    check.type = 'checkbox';
    // The row carries the state via aria-selected; the box is the visual
    // signal. Hiding it from the tree avoids a screen reader announcing the
    // same fact twice, and tabindex -1 keeps Tab from walking forty checkboxes
    // instead of moving to the next control.
    check.tabIndex = -1;
    check.setAttribute('aria-hidden', 'true');

    const identity = el('span', 'df-picker-row-identity');
    const name = el('span', 'df-row__name df-mono');
    const displayName = el('span', 'df-picker-row-display');
    identity.appendChild(displayName);
    identity.appendChild(name);
    const summary = el('span', 'df-picker-row-summary');
    const badges = el('span', 'df-row__badge');
    const component = el('span', 'df-badge');
    const snap = el('span', 'df-badge df-badge--warning');
    component.hidden = true;
    snap.hidden = true;
    badges.appendChild(component);
    badges.appendChild(snap);

    row.appendChild(check);
    row.appendChild(identity);
    row.appendChild(summary);
    row.appendChild(badges);
    const details = el('button', 'df-btn df-btn--ghost df-btn--sm df-picker-row-details', { type: 'button', tabindex: '-1', 'data-action': 'details' });
    setText(details, 'Details');
    row.appendChild(details);

    const skeleton = makeSkeleton(slotNo);
    skeleton.id = 'picker-row-skel-' + slotNo;

    return {
      no: slotNo,
      row,
      skeleton,
      check,
      name,
      displayName,
      details,
      summary,
      component,
      snap,
      // What is currently in the document for this slot, and what it shows.
      mounted: null,
      mountedIn: false,
      kind: '',
      index: -1,
      data: null,
      selected: null,
      focused: null,
    };
  }

  /** Grow or shrink the mounted set to `count`, always at the tail, so that
   *  `order[i]` keeps meaning "the i-th row from the top of the window". */
  function ensurePool(count) {
    while (order.length > count) {
      const s = order.pop();
      if (s.mounted && s.mounted.parentNode === windowEl) windowEl.removeChild(s.mounted);
      s.mounted = null;
      s.kind = '';
      s.index = -1;
      s.data = null;
      s.selected = null;
      s.focused = null;
      s.mountedIn = false;
    }
    while (order.length < count) {
      let s = null;
      for (let i = 0; i < pool.length; i++) {
        if (!pool[i].mountedIn) { s = pool[i]; break; }
      }
      if (!s) {
        s = makeSlot(pool.length);
        pool.push(s);
      }
      // Mount as a skeleton; the render pass immediately after swaps it if the
      // page is resident. Appending keeps DOM order == window order.
      s.mounted = s.skeleton;
      s.kind = 'skeleton';
      s.index = -1;
      s.data = null;
      s.selected = null;
      s.focused = null;
      s.mountedIn = true;
      windowEl.appendChild(s.mounted);
      order.push(s);
    }
    liveSlots = order.length;
  }

  /**
   * Apply a scroll as a rotation of the mounted set.
   *
   * A recycled pool held in flow order has a hidden cost: when the window
   * moves down by two rows, every slot's index shifts by two and all forty
   * rows have to be rewritten. Moving the elements instead — the two that fell
   * off the top go to the bottom of the window — leaves thirty-eight slots
   * holding the index and the content they already had, so the work per frame
   * is proportional to the rows that actually entered the viewport rather than
   * to the size of the viewport. Flow order is preserved, so the DOM stays
   * exactly the structure design/components.css documents.
   *
   * Returns true if the caller still has to reassign every slot.
   */
  function rotateTo(first, count) {
    if (!Number.isFinite(assignedFirst) || count === 0) {
      stats.fullReassigns++;
      return true;
    }
    const shift = first - assignedFirst;
    if (shift === 0) return false;
    if (Math.abs(shift) >= count) {
      // A jump, not a scroll: nothing on screen survives it.
      stats.fullReassigns++;
      return true;
    }
    if (shift > 0) {
      for (let k = 0; k < shift; k++) {
        const s = order.shift();
        windowEl.appendChild(s.mounted);
        order.push(s);
        stats.nodeMoves++;
      }
    } else {
      for (let k = 0; k < -shift; k++) {
        const s = order.pop();
        windowEl.insertBefore(s.mounted, windowEl.firstChild);
        order.unshift(s);
        stats.nodeMoves++;
      }
    }
    return false;
  }

  function swapKind(slot, kind) {
    const next = kind === 'row' ? slot.row : slot.skeleton;
    if (slot.mounted === next) return;
    if (slot.mounted && slot.mounted.parentNode === windowEl) {
      windowEl.replaceChild(next, slot.mounted);
    } else {
      windowEl.appendChild(next);
    }
    slot.mounted = next;
    slot.kind = kind;
    stats.kindSwaps++;
  }

  // -- data access ----------------------------------------------------------

  function pageOf(index) {
    return Math.floor(index / pageSize);
  }

  function rowAt(index) {
    const p = pages.get(pageOf(index));
    if (!p) return undefined;
    p.lru = ++lruClock;
    return p.rows[index - pageOf(index) * pageSize];
  }

  // -- rendering ------------------------------------------------------------

  function renderSlot(slot, index) {
    const row = rowAt(index);
    // A slot owns two elements and they carry their own attributes, so a slot
    // that keeps its index while changing kind still has to stamp the index
    // onto the element that just came into the document. Missing this is not
    // cosmetic: `data-index` is how a click finds its row.
    const wasKind = slot.kind;

    if (row === undefined) {
      // No data for this index. A skeleton at the right height, never the row
      // that used to live in this slot.
      swapKind(slot, 'skeleton');
      slot.data = null;
      slot.selected = null;
      slot.focused = null;
      if (slot.index !== index || wasKind !== 'skeleton') {
        slot.skeleton.dataset.index = String(index);
        setAttr(slot.skeleton, 'aria-posinset', index + 1);
        setAttr(slot.skeleton, 'aria-setsize', total > 0 ? total : null);
        slot.index = index;
        stats.skeletonsRendered++;
      }
      if (index === focusIndex && !focusRefreshCleared) setAttr(port, 'aria-activedescendant', slot.skeleton.id);
      return;
    }

    const selected = !!isSelected(row);
    const focused = index === focusIndex;
    const wasRow = wasKind === 'row';
    swapKind(slot, 'row');

    if (wasRow && slot.index === index && slot.data === row && slot.selected === selected && slot.focused === focused) {
      if (focused && !focusRefreshCleared) setAttr(port, 'aria-activedescendant', slot.row.id);
      return; // nothing changed — do not touch the DOM
    }

    const node = slot.row;

    if (slot.index !== index || !wasRow) {
      node.dataset.index = String(index);
      setAttr(node, 'aria-posinset', index + 1);
    }
    setAttr(node, 'aria-setsize', total > 0 ? total : null);

    if (slot.data !== row) {
      setText(slot.name, row.name || '');
      const display = row.display_name || row.displayName || '';
      setText(slot.displayName, display !== row.name ? display : '');
      setHidden(slot.displayName, !display || display === row.name);
      const summaryText = row.summary || '';
      setText(slot.summary, summaryText);
      setAttr(node, 'aria-label', [display !== row.name ? display : '', row.name, summaryText,
        ...(row.warnings || []).map((warning) => [warning.message, warning.hint].filter(Boolean).join(' '))].filter(Boolean).join('. '));
      setAttr(slot.details, 'aria-label', 'Details for ' + row.name);

      // One catalogue row exists per exact package name. Only a backend
      // disambiguator needs a visible source label; routine metadata is in Details.
      const comp = row.disambiguation || '';
      setHidden(slot.component, !comp);
      if (comp) setText(slot.component, comp);

      const snapWarn = snapWarningOf(row);
      setHidden(slot.snap, !snapWarn);
      if (snapWarn) {
        setText(slot.snap, 'Installs snap');
        setAttr(slot.snap, 'title', snapWarn.message || 'This package installs a snap.');
      }
    }

    if (slot.selected !== selected) {
      setAttr(node, 'aria-selected', selected ? 'true' : 'false');
      if (slot.check.checked !== selected) slot.check.checked = selected;
    }

    if (slot.focused !== focused) {
      if (focused) node.classList.add('is-focused');
      else node.classList.remove('is-focused');
    }
    if (focused && !focusRefreshCleared) setAttr(port, 'aria-activedescendant', node.id);

    slot.index = index;
    slot.data = row;
    slot.selected = selected;
    slot.focused = focused;
    stats.rowsRendered++;
  }

  function update() {
    scheduled = false;
    if (destroyed) return;

    // The only timestamp the virtualiser takes. It is here because the frame
    // budget has to be a measured number rather than an assertion, and the
    // only place that number exists is around this function.
    const t0 = performance.now();

    // One read, at the top. Everything below this line is a write.
    const scrollTop = port.scrollTop;
    const delta = Math.abs(scrollTop - lastScrollTop);
    lastScrollTop = scrollTop;

    // FOCUS SURVIVES RECYCLING. `rotateTo` moves row elements with
    // `appendChild`/`insertBefore`, and `ensurePool`/`swapKind` remove and
    // replace them. Every one of those detaches the node for an instant, and a
    // node that holds DOM focus when it is detached blurs to <body> — after
    // which the port's keydown handler never fires again and the list is dead
    // to the keyboard. Rows carry no tabindex precisely so this cannot happen,
    // and `onPointerDown` keeps the checkbox from taking focus either; this is
    // the last-resort guard that makes the property true regardless of how
    // focus got in there. Reading `activeElement` is not a layout read, so it
    // does not break the one-read-then-writes rule.
    const focusWasInside = windowEl.contains(document.activeElement);

    if (!viewportH) viewportH = port.clientHeight || 0;

    const known = total >= 0;
    // While the very first page is in flight we do not know `total`. Render a
    // viewport of skeletons rather than an empty box, so the list appears at
    // the right size immediately instead of popping in.
    const effectiveTotal = known ? total : Math.ceil(viewportH / rowHeight) + overscan;

    const before = stats.rowsRendered + stats.skeletonsRendered;

    const visible = Math.max(1, Math.ceil(viewportH / rowHeight) + 1);
    let first = Math.floor(scrollTop / rowHeight) - overscan;
    if (first < 0) first = 0;
    let count = Math.min(effectiveTotal - first, visible + overscan * 2);
    if (count < 0) count = 0;
    if (first > 0 && count === 0) {
      // Scrolled past the end after `total` shrank; clamp back.
      first = Math.max(0, effectiveTotal - 1);
      count = Math.min(effectiveTotal - first, visible + overscan * 2);
    }

    firstIndex = first;
    ensurePool(count);
    rotateTo(first, count);
    assignedFirst = first;

    const offsetPx = first * rowHeight;
    const transform = 'translateY(' + offsetPx + 'px)';
    if (windowEl.style.transform !== transform) windowEl.style.transform = transform;

    // Every slot is asked what it should show. The ones that already show it
    // return on four comparisons and touch no DOM; that guard is what makes
    // "rows rendered per frame" a number worth measuring.
    for (let i = 0; i < count; i++) renderSlot(order[i], first + i);

    // The keyboard cursor can now be outside the rendered window: the operator
    // scrolled with the wheel or the scrollbar and `focusIndex` stayed put.
    // The slot the cursor used to occupy has been recycled onto a different
    // package, and `renderSlot` only ever *sets* aria-activedescendant — so
    // without this the listbox goes on naming a live element that is now some
    // unrelated row, and a screen reader reads that row out as the current
    // option while Space would toggle the one the operator actually chose.
    // No active option is honest; a wrong one is not. The next arrow key calls
    // ensureVisible, which brings the cursor back on screen and re-points it.
    // Found by the accessibility keyboard pass.
    if (focusIndex < first || focusIndex >= first + count) {
      setAttr(port, 'aria-activedescendant', null);
    }

    // See `focusWasInside`. If a node we just moved took focus with it, put it
    // back on the port so the cursor and the key handlers keep working.
    if (focusWasInside && !windowEl.contains(document.activeElement)) {
      try {
        port.focus({ preventScroll: true });
      } catch (_) {
        port.focus();
      }
    }

    stats.updates++;

    // Fetching is decided after rendering so that a fling always paints first.
    const flinging = delta > FLING_PX_PER_FRAME;
    if (known) {
      if (flinging) {
        pendingFetch = true;
      } else {
        requestPagesFor(first, count, /* prefetch */ true);
      }
    } else if (started && inflight.size === 0) {
      // The page that would have told us the total was dropped or failed.
      scheduleRecovery();
    }
    // Only arm the settle timer when something moved. Arming it every pass
    // would make the timer's own repaint re-arm it, and the list would run a
    // frame every SETTLE_MS forever with nothing to do.
    if (delta > 0 || pendingFetch) armSettleTimer();

    if (typeof opts.onMetrics === 'function') {
      opts.onMetrics({
        firstIndex: first,
        slotCount: count,
        rendered: stats.rowsRendered + stats.skeletonsRendered - before,
        updateMs: performance.now() - t0,
        scrollTop,
        delta,
        flinging,
        cachedPages: pages.size,
        inflight: inflight.size,
        poolSize: pool.length,
      });
    }
  }

  function schedule() {
    if (scheduled || destroyed) return;
    scheduled = true;
    requestAnimationFrame(update);
  }

  function armSettleTimer() {
    if (settleTimer) clearTimeout(settleTimer);
    settleTimer = setTimeout(() => {
      settleTimer = 0;
      if (destroyed) return;
      pendingFetch = false;
      lastScrollTop = port.scrollTop;
      // The fling is over; ask for what is actually on screen now. Arriving
      // pages schedule their own repaint, so nothing is scheduled from here —
      // a settle with nothing to fetch costs one timer and no frame.
      if (total >= 0) requestPagesFor(firstIndex, liveSlots, true);
    }, SETTLE_MS);
  }

  /**
   * Re-ask for what the window needs, after a response was dropped rather than
   * delivered. Rate-limited and bounded: a backend that answers every request
   * with something unusable is a failure the operator has to be told about,
   * not something to hammer.
   */
  function scheduleRecovery() {
    if (recoveryTimer || destroyed) return;
    if (recoveryAttempts >= MAX_RECOVERY_ATTEMPTS) {
      if (total < 0 && !recoveryReported) {
        recoveryReported = true;
        reportError({
          code: 'app.internal',
          message: 'The catalogue stopped answering searches.',
          hint: 'Try the search again, or rebuild the catalogue for this target.',
        });
      }
      return;
    }
    recoveryAttempts++;
    recoveryTimer = setTimeout(() => {
      recoveryTimer = 0;
      if (destroyed) return;
      if (total < 0) fetchPage(pageOf(Math.floor(port.scrollTop / rowHeight)));
      else requestPagesFor(firstIndex, Math.max(1, liveSlots), false);
      schedule();
    }, RECOVERY_MS);
  }

  // -- paging ---------------------------------------------------------------

  function requestPagesFor(first, count, prefetch) {
    if (count <= 0) return;
    const p0 = pageOf(first);
    const p1 = pageOf(first + count - 1);
    const lo = prefetch ? Math.max(0, p0 - 1) : p0;
    const hi = prefetch ? p1 + 1 : p1;
    const lastPage = total > 0 ? pageOf(total - 1) : hi;
    for (let p = lo; p <= Math.min(hi, lastPage); p++) {
      const have = pages.get(p);
      if (have && have.complete) continue;
      if (have && have.attempts >= MAX_PAGE_ATTEMPTS) continue;
      if (inflight.has(p)) continue;
      fetchPage(p);
    }
  }

  function fetchPage(pageIndex) {
    const myGen = generation;
    const myQuery = query;
    const offset = pageIndex * pageSize;

    inflight.set(pageIndex, myGen);
    stats.pageRequests++;
    const t0 = performance.now();

    let promise;
    try {
      promise = Promise.resolve(search({
        text: myQuery.text,
        category: myQuery.category,
        apps_only: myQuery.appsOnly,
        offset,
        limit: pageSize,
      }));
    } catch (err) {
      inflight.delete(pageIndex);
      stats.pageErrors++;
      reportError(err);
      scheduleRecovery();
      return;
    }

    promise.then((res) => {
      recordLatency(performance.now() - t0);
      if (inflight.get(pageIndex) === myGen) inflight.delete(pageIndex);
      if (destroyed) return;

      // Guard 1 — generation. The query changed while this was in flight.
      if (myGen !== generation) {
        stats.pagesDropStale++;
        return;
      }
      if (!res) return;
      if (res.error) {
        stats.pageErrors++;
        reportError(res.error);
        scheduleRecovery();
        return;
      }

      // Guard 2 — the echoed query. The backend returns the query it actually
      // ran (SearchResult.Query) for exactly this: two keystrokes can share a
      // generation only if they produced the same query, so a mismatch here is
      // a response that belongs to a search the operator has already replaced.
      const echoed = res.query !== undefined ? normQuery(res.query) : myQuery;
      if (!sameQuery(echoed, query)) {
        // Not this search's answer. Nothing else is going to deliver the page
        // we asked for, so ask again rather than leaving a skeleton.
        stats.pagesDropEcho++;
        scheduleRecovery();
        return;
      }

      // Guard 3 — alignment. Store the page where the backend says it starts,
      // not where we asked for it, and refuse anything that is not on a page
      // boundary rather than guessing an index for it.
      const echoOffset = Number.isInteger(res.offset) ? res.offset : offset;
      if (echoOffset < 0 || echoOffset % pageSize !== 0) {
        stats.pagesDropMisaligned++;
        scheduleRecovery();
        return;
      }
      const key = echoOffset / pageSize;
      const rows = Array.isArray(res.rows) ? res.rows : [];

      if (Number.isInteger(res.total)) applyTotal(res.total, res);

      const complete = rows.length >= pageSize
        || (total >= 0 && echoOffset + rows.length >= total);
      const prev = pages.get(key);
      pages.set(key, {
        rows,
        complete,
        attempts: (prev ? prev.attempts : 0) + 1,
        lru: ++lruClock,
      });
      recoveryAttempts = 0;
      recoveryReported = false;
      evict();
      schedule();
    }, (err) => {
      if (inflight.get(pageIndex) === myGen) inflight.delete(pageIndex);
      if (destroyed || myGen !== generation) return;
      stats.pageErrors++;
      reportError(err);
      scheduleRecovery();
    });
  }

  /**
   * Evict least-recently-used pages down to `maxCachedPages`, never evicting a
   * page the window currently needs. This is the bound that keeps memory flat
   * across a full scroll of a 60,000-row result: without it the cache grows to
   * the whole result set and the app has simply moved the problem.
   */
  function evict() {
    if (pages.size <= maxCachedPages) return;
    const lo = Math.max(0, pageOf(firstIndex) - 1);
    const hi = pageOf(firstIndex + Math.max(1, liveSlots) - 1) + 1;
    const keys = [];
    for (const [k, v] of pages) {
      if (k >= lo && k <= hi) continue;
      keys.push([k, v.lru]);
    }
    keys.sort((a, b) => a[1] - b[1]);
    let over = pages.size - maxCachedPages;
    for (let i = 0; i < keys.length && over > 0; i++, over--) {
      pages.delete(keys[i][0]);
      stats.pagesEvicted++;
    }
  }

  function applyTotal(next, res) {
    if (!Number.isInteger(next) || next < 0) return;
    const changed = next !== total;
    total = next;
    if (changed) {
      applySizer();
      // `aria-setsize` carries the true match count, and a slot that keeps
      // both its index and its data takes the early return in `renderSlot`
      // and would never restamp it. Invalidating the slots makes the next
      // pass rewrite the attribute so a screen reader never says "1 of 264"
      // about a set that is now 75,176. docs/accessibility.md section 10, P0.3.
      for (let i = 0; i < liveSlots; i++) order[i].index = -1;
      if (typeof opts.onTotal === 'function') opts.onTotal(total, res || null);
    }
    if (focusIndex >= total) focusIndex = total - 1;
  }

  function applySizer() {
    const wanted = Math.max(0, total) * rowHeight;
    const clamped = Math.min(wanted, MAX_SIZER_PX);
    if (clamped !== wanted && !sizerClamped) {
      sizerClamped = true;
      // eslint-disable-next-line no-console
      console.warn(
        'picker-list: result set of ' + total + ' rows exceeds the maximum ' +
        'scrollable height; the scrollbar is clamped at ' + MAX_SIZER_PX + 'px.',
      );
    }
    if (clamped !== sizerPx) {
      sizerPx = clamped;
      sizer.style.height = clamped + 'px';
    }
  }

  function resetRecovery() {
    if (recoveryTimer) clearTimeout(recoveryTimer);
    recoveryTimer = 0;
    recoveryAttempts = 0;
    recoveryReported = false;
  }

  function reportError(err) {
    if (typeof opts.onError === 'function') opts.onError(err);
    else console.error('picker-list: page request failed', err); // eslint-disable-line no-console
  }

  function snapWarningOf(row) {
    const list = row && row.warnings;
    if (!Array.isArray(list)) return null;
    for (const w of list) {
      if (w && w.kind === WARN_SNAP_TRANSITIONAL) return w;
    }
    return null;
  }

  // -- measurement ----------------------------------------------------------

  let resizeObserver = null;

  function measure() {
    const next = readRowHeight(port);
    const changed = next !== rowHeight;
    rowHeight = next;
    viewportH = port.clientHeight || viewportH;
    if (changed) applySizer();
    return { rowHeight, viewportH };
  }

  /**
   * Development check: the token says one thing, the rendered box says another.
   * That is the failure mode §6.5 of the design README warns about — the list
   * tears while scrolling and nothing else looks wrong — so we say so loudly
   * once rather than letting someone hunt it.
   */
  let driftChecked = false;
  function checkRowHeightDrift() {
    if (driftChecked || !liveSlots) return;
    const s = order[0];
    if (!s || s.kind !== 'row' || !s.row.isConnected) return;
    driftChecked = true;
    const measured = s.row.offsetHeight;
    if (measured && Math.abs(measured - rowHeight) > 0.5) {
      // eslint-disable-next-line no-console
      console.warn(
        'picker-list: --row-height is ' + rowHeight + 'px but .df-row renders ' +
        measured + 'px. The virtualiser and the design token have drifted; the ' +
        'list will tear while scrolling.',
      );
    }
  }

  // -- interaction ----------------------------------------------------------

  function indexFromEvent(ev) {
    let node = ev.target;
    while (node && node !== windowEl) {
      if (node.dataset && node.dataset.index !== undefined) {
        const i = parseInt(node.dataset.index, 10);
        return Number.isFinite(i) ? i : -1;
      }
      node = node.parentNode;
    }
    return -1;
  }

  function toggleIndex(index) {
    const row = rowAt(index);
    if (!row) return;
    if (typeof opts.onToggle === 'function') opts.onToggle(index, row);
  }

  function setFocusIndex(index, opt) {
    if (total === 0) return;
    const max = total > 0 ? total - 1 : 0;
    const next = Math.min(Math.max(0, index), max);
    if (next === focusIndex) {
      if (!opt || opt.ensureVisible !== false) ensureVisible(next);
      return;
    }
    focusIndex = next;
    if (!opt || opt.ensureVisible !== false) ensureVisible(next);
    schedule();
    if (typeof opts.onFocusRow === 'function') opts.onFocusRow(next, rowAt(next));
  }

  function ensureVisible(index) {
    const top = index * rowHeight;
    const bottom = top + rowHeight;
    const st = port.scrollTop;
    const vh = viewportH || port.clientHeight;
    if (top < st) port.scrollTop = top;
    else if (bottom > st + vh) port.scrollTop = bottom - vh;
  }

  function onScroll() {
    schedule();
  }

  function onKeyDown(ev) {
    if (ev.altKey || ev.ctrlKey || ev.metaKey) return;
    const vh = viewportH || port.clientHeight;
    const pageStep = Math.max(1, Math.floor(vh / rowHeight) - 1);
    const cur = focusIndex < 0 ? Math.floor(port.scrollTop / rowHeight) : focusIndex;

    switch (ev.key) {
      case 'ArrowDown':
        setFocusIndex(cur + 1);
        break;
      case 'ArrowUp':
        setFocusIndex(cur - 1);
        break;
      case 'PageDown':
        setFocusIndex(cur + pageStep);
        break;
      case 'PageUp':
        setFocusIndex(cur - pageStep);
        break;
      case 'Home':
        setFocusIndex(0);
        break;
      case 'End':
        setFocusIndex(total > 0 ? total - 1 : 0);
        break;
      case ' ':
      case 'Spacebar':
        if (focusIndex >= 0) toggleIndex(focusIndex);
        break;
      case 'Enter':
        if (focusIndex >= 0) {
          const row = rowAt(focusIndex);
          if (row && typeof opts.onActivate === 'function') opts.onActivate(focusIndex, row);
        }
        break;
      default:
        return;
    }
    ev.preventDefault();
  }

  /**
   * Keep DOM focus on the port when a row is clicked.
   *
   * The row itself is not focusable any more, but the checkbox inside it is
   * natively focusable and `tabindex="-1"` does not change that. Suppressing
   * the default focus move on pointer-down — and then focusing the port
   * explicitly — means DOM focus never enters the recycled pool, so no
   * subsequent `appendChild` in `rotateTo` can blur it onto <body>. It also
   * stops a drag inside a listbox selecting text, which is correct anyway.
   */
  function onPointerDown(ev) {
    if (indexFromEvent(ev) < 0) return;
    ev.preventDefault();
    try {
      port.focus({ preventScroll: true });
    } catch (_) {
      port.focus();
    }
  }

  function onClick(ev) {
    const index = indexFromEvent(ev);
    if (index < 0) return;
    setFocusIndex(index);
    if (ev.target.closest('[data-action="details"]')) {
      const row = rowAt(index);
      if (row && typeof opts.onActivate === 'function') opts.onActivate(index, row);
      return;
    }
    toggleIndex(index);
  }

  function onDblClick(ev) {
    const index = indexFromEvent(ev);
    if (index < 0) return;
    const row = rowAt(index);
    if (row && typeof opts.onActivate === 'function') opts.onActivate(index, row);
  }

  function onFocus() {
    if (focusIndex < 0 && total !== 0) {
      setFocusIndex(Math.floor(port.scrollTop / rowHeight), { ensureVisible: false });
    }
    // Native focus may announce the first selected option, although the
    // logical cursor is elsewhere. Refresh its relation after focus settles.
    // Separate frames prevent WebKit from coalescing clear and restore.
    const sequence = ++focusRefreshSequence;
    const focusGeneration = generation;
    const stillFocused = () => !destroyed && sequence === focusRefreshSequence &&
      generation === focusGeneration && port.isConnected && document.activeElement === port;
    if (focusRefreshFrame) cancelAnimationFrame(focusRefreshFrame);
    focusRefreshCleared = false;
    focusRefreshFrame = requestAnimationFrame(() => {
      focusRefreshFrame = 0;
      if (!stillFocused()) return;
      focusRefreshCleared = true;
      setAttr(port, 'aria-activedescendant', null);
      focusRefreshFrame = requestAnimationFrame(() => {
        focusRefreshFrame = 0;
        focusRefreshCleared = false;
        if (!stillFocused()) return;
        const slot = order.find((item) => item.index === focusIndex);
        const node = slot?.kind === 'row' ? slot.row : slot?.skeleton;
        if (node?.isConnected && windowEl.contains(node) && Number(node.dataset.index) === focusIndex) {
          setAttr(port, 'aria-activedescendant', node.id);
        }
      });
    });
  }

  port.addEventListener('scroll', onScroll, { passive: true });
  port.addEventListener('keydown', onKeyDown);
  port.addEventListener('mousedown', onPointerDown);
  port.addEventListener('click', onClick);
  port.addEventListener('dblclick', onDblClick);
  port.addEventListener('focus', onFocus);

  // ResizeObserver rather than reading clientHeight on every frame: the pool
  // only needs resizing when the viewport does, and a read inside the scroll
  // pass would force a layout in the middle of a set of writes.
  if (typeof ResizeObserver === 'function') {
    resizeObserver = new ResizeObserver((entries) => {
      const entry = entries[entries.length - 1];
      const h = entry && entry.contentRect ? entry.contentRect.height : port.clientHeight;
      if (h && Math.abs(h - viewportH) >= 1) {
        viewportH = h;
        schedule();
      }
    });
    resizeObserver.observe(port);
  }

  // -- public surface -------------------------------------------------------

  const api = {
    /** The `.df-list` element. Put it in a bounded flex parent. */
    el: listEl,
    /** The scroll port, for tests and for focus management. */
    port,

    /**
     * Point the list at a different query. Bumps the generation, which is what
     * makes every response already in flight unrenderable.
     *
     * @param {{text?:string, category?:string, appsOnly?:boolean}} next
     * @param {{keepScroll?:boolean}} [o]
     */
    setQuery(next, o) {
      const q = normQuery(next);
      const keepScroll = !!(o && o.keepScroll);
      if (sameQuery(q, query) && total >= 0) return;
      query = q;
      generation++;
      pages.clear();
      inflight.clear();
      total = -1;
      sizerClamped = false;
      focusIndex = -1;
      setAttr(port, 'aria-activedescendant', null);
      if (!keepScroll) {
        port.scrollTop = 0;
        lastScrollTop = 0;
      }
      applySizer();
      resetRecovery();
      started = true;
      // Ask for the first page immediately — the keystroke budget is measured
      // from here — and paint skeletons in the same frame.
      fetchPage(pageOf(keepScroll ? Math.floor(port.scrollTop / rowHeight) : 0));
      schedule();
    },

    /** Current query, normalised. */
    getQuery() {
      return Object.assign({}, query);
    },

    /** Invalidate pending pages without fetching an empty query. */
    reset() {
      generation++;
      started = false;
      pages.clear();
      inflight.clear();
      total = -1;
      focusIndex = -1;
      setAttr(port, 'aria-activedescendant', null);
      port.scrollTop = lastScrollTop = 0;
      resetRecovery();
      applySizer();
      schedule();
    },

    focusedRow() { return focusIndex >= 0 ? rowAt(focusIndex) : undefined; },

    /**
     * Throw the cache away and refetch, keeping the query. Use after the
     * catalogue is rebuilt. Selection changes do not need this — use
     * `repaint()`, which costs no round-trip.
     */
    refresh(o) {
      const keepScroll = !!(o && o.keepScroll);
      generation++;
      pages.clear();
      inflight.clear();
      total = -1;
      if (!keepScroll) {
        port.scrollTop = 0;
        lastScrollTop = 0;
      }
      applySizer();
      resetRecovery();
      started = true;
      fetchPage(pageOf(Math.floor(port.scrollTop / rowHeight)));
      schedule();
    },

    /**
     * Re-evaluate `isSelected` for every visible row and repaint. Called when
     * the backend says the selection changed: the rows themselves are not
     * refetched, because the row's `selected` field is a hint and the resolver
     * the screen supplies is the truth.
     */
    repaint() {
      for (let i = 0; i < liveSlots; i++) order[i].selected = null;
      schedule();
    },

    /** Scroll so `index` is visible, and focus it. */
    scrollToIndex(index) {
      setFocusIndex(index);
    },

    focusIndex() {
      return focusIndex;
    },

    /** Move DOM focus into the list. */
    focus() {
      port.focus();
    },

    total() {
      return total;
    },

    rowHeight() {
      return rowHeight;
    },

    /** Re-read `--row-height` and the viewport. Call from `show()`. */
    measure() {
      const m = measure();
      schedule();
      requestAnimationFrame(checkRowHeightDrift);
      return m;
    },

    /**
     * Counters and page latencies. This is what `picker-list.demo.html`
     * reports and what makes the budget a measured number rather than a claim.
     */
    stats() {
      return {
        updates: stats.updates,
        rowsRendered: stats.rowsRendered,
        skeletonsRendered: stats.skeletonsRendered,
        kindSwaps: stats.kindSwaps,
        nodeMoves: stats.nodeMoves,
        fullReassigns: stats.fullReassigns,
        pageRequests: stats.pageRequests,
        pagesDropStale: stats.pagesDropStale,
        pagesDropEcho: stats.pagesDropEcho,
        pagesDropMisaligned: stats.pagesDropMisaligned,
        pagesEvicted: stats.pagesEvicted,
        pageErrors: stats.pageErrors,
        recoveryAttempts,
        cachedPages: pages.size,
        inflight: inflight.size,
        poolSize: pool.length,
        liveSlots,
        generation,
        latency: summarise(stats.latencies),
      };
    },

    resetStats() {
      stats.updates = 0;
      stats.rowsRendered = 0;
      stats.skeletonsRendered = 0;
      stats.kindSwaps = 0;
      stats.nodeMoves = 0;
      stats.fullReassigns = 0;
      stats.pageRequests = 0;
      stats.pagesDropStale = 0;
      stats.pagesDropEcho = 0;
      stats.pagesDropMisaligned = 0;
      stats.pagesEvicted = 0;
      stats.pageErrors = 0;
      stats.latencies.length = 0;
    },

    /**
     * What every slot currently shows, as `{index, name}`. The demo's
     * correctness checks compare this against the synthetic backend to prove
     * that no slot ever paints one index's data at another index's position.
     */
    inspect() {
      const out = [];
      for (let i = 0; i < liveSlots; i++) {
        const s = order[i];
        out.push({
          slot: s.no,
          index: s.index,
          kind: s.kind,
          name: s.kind === 'row' ? s.name.textContent : null,
          selected: s.selected,
        });
      }
      return out;
    },

    destroy() {
      destroyed = true;
      if (focusRefreshFrame) cancelAnimationFrame(focusRefreshFrame);
      focusRefreshFrame = 0;
      focusRefreshCleared = false;
      if (settleTimer) clearTimeout(settleTimer);
      settleTimer = 0;
      if (recoveryTimer) clearTimeout(recoveryTimer);
      recoveryTimer = 0;
      if (resizeObserver) resizeObserver.disconnect();
      resizeObserver = null;
      port.removeEventListener('scroll', onScroll);
      port.removeEventListener('keydown', onKeyDown);
      port.removeEventListener('mousedown', onPointerDown);
      port.removeEventListener('click', onClick);
      port.removeEventListener('dblclick', onDblClick);
      port.removeEventListener('focus', onFocus);
      pages.clear();
      inflight.clear();
      pool.length = 0;
      order.length = 0;
      liveSlots = 0;
      if (listEl.parentNode) listEl.parentNode.removeChild(listEl);
    },
  };

  measure();
  applySizer();

  return api;
}

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/**
 * Layout only. Every component on this screen comes from the design system
 * (`df-*`); what is below arranges those components inside the screen's root
 * and defines no colour, no border and no type. It is injected once, keyed by
 * id, because a screen has nowhere else to put its own layout and
 * `frontend/src/design/` belongs to W0-D.
 */
const SCREEN_CSS = `
.picker-screen {
  display: flex;
  flex-direction: column;
  flex: 1 1 auto;
  height: 100%;
  min-height: 0;
}
.picker-screen__top,
.picker-screen__notices,
.picker-screen__foot { flex: 0 0 auto; }
/* The one row under the header is a .df-toolbar (picker-search.js owns it) and
   sits flush against the header: its own hairline is the separator, so this
   host adds no padding and no gap of its own. */
.picker-screen__top { display: flex; flex-direction: column; min-width: 0; }
.picker-screen__notices {
  display: flex;
  flex-direction: column;
  gap: var(--space-2, 8px);
  padding: var(--space-3, 12px) var(--space-4, 16px) 0;
}
.picker-screen__notices:empty { display: none; }
.picker-screen__body {
  flex: 1 1 auto;
  min-height: 0;
  display: flex;
  gap: var(--space-3, 12px);
  padding: var(--space-3, 12px) var(--space-4, 16px);
}
.picker-screen__main {
  flex: 1 1 auto;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.picker-screen__list { flex: 1 1 auto; min-height: 0; display: flex; }
.picker-screen__list > .df-list { flex: 1 1 auto; min-height: 0; }
.picker-screen__tray {
  flex: 0 1 320px;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.picker-screen__tray:empty { display: none; }
.picker-screen__state {
  flex: 1 1 auto;
  min-height: 0;
  display: flex;
  padding: var(--space-3, 12px) var(--space-4, 16px);
}
.picker-screen__state > * { flex: 1 1 auto; }
/* .df-actionbar does not wrap; at a 320px container its three children run off
   the edge (WCAG 1.4.10). Reported to the design work, wrapped here. */
.picker-screen__foot { flex-wrap: wrap; }
.picker-screen[data-mode="list"] .picker-screen__state { display: none; }
.picker-screen[data-mode="state"] .picker-screen__body { display: none; }
.picker-screen[data-mode="state"] .picker-screen__top { opacity: 0.5; pointer-events: none; }
@media (max-width: 900px) {
  .picker-screen__body { flex-direction: column; }
  .picker-screen__tray { flex: 0 1 auto; max-height: 38%; }
}
`;

function injectScreenCSS() {
  if (document.getElementById('picker-screen-layout')) return;
  const style = document.createElement('style');
  style.id = 'picker-screen-layout';
  style.textContent = SCREEN_CSS;
  document.head.appendChild(style);
}

/**
 * Adapter over `ctx.states` (`frontend/src/shell/states.js`).
 *
 * The shared helpers are the contract and are used whenever they are there:
 * `loadingState`, `emptyState`, `errorState` and `skeletonRows`, each returning
 * a `{ el, destroy }` handle. This screen must still render a state when it is
 * mounted in a harness that has none — `picker-list.demo.html` drives the
 * virtualiser with no shell at all — so every call falls back to the design
 * system's own `.df-state` markup. Only this function knows the difference.
 *
 * Every wrapper returns `{ el, destroy }`, and the screen destroys the handle
 * it is replacing: `loadingState` can own an elapsed-time timer.
 */
function statesAdapter(ctx) {
  const s = (ctx && ctx.states) || null;

  function pick() {
    for (let i = 0; i < arguments.length; i++) {
      const n = arguments[i];
      if (s && typeof s[n] === 'function') return s[n].bind(s);
    }
    return null;
  }

  const fLoading = pick('loadingState', 'loading');
  const fEmpty = pick('emptyState', 'empty');
  const fError = pick('errorState', 'error');
  const fSkeleton = pick('skeletonRows', 'skeletonRow');

  function handle(node) {
    return { el: node, destroy() {} };
  }

  function fallbackState(variant, spec) {
    const node = el('div', 'df-state' + (variant ? ' df-state--' + variant : ''));
    if (variant === 'loading') {
      node.appendChild(el('span', 'df-spinner df-spinner--lg', { 'aria-hidden': 'true' }));
    } else {
      node.appendChild(icon('df-state__icon', variant === 'error' ? ICON_OCTAGON : ICON_BOX));
    }
    node.appendChild(setText(el('span', 'df-state__title'), spec.title || ''));
    if (spec.text) node.appendChild(setText(el('p', 'df-state__text'), spec.text));

    // A wait with no bar and no words is the "nothing appears frozen" rule
    // broken (screen-contract.md rule 3), so the fallback renders the bar too
    // rather than degrading to a bare spinner.
    if (variant === 'loading' && spec.progress !== undefined) {
      const determinate = typeof spec.progress === 'number' && spec.progress >= 0;
      const pct = determinate ? Math.round(spec.progress * 100) : -1;
      const label = el('div', 'df-progress-label');
      label.appendChild(setText(el('span'), spec.title || 'Working'));
      label.appendChild(setText(el('span', 'df-progress-label__value'), pct >= 0 ? pct + '%' : ''));
      const bar = el('div', 'df-progress' + (pct < 0 ? ' df-progress--indeterminate' : ''), {
        role: 'progressbar',
        'aria-label': spec.title || 'Working',
      });
      if (pct >= 0) {
        bar.setAttribute('aria-valuenow', String(pct));
        bar.setAttribute('aria-valuemin', '0');
        bar.setAttribute('aria-valuemax', '100');
      }
      const fill = el('div', 'df-progress__bar');
      if (pct >= 0) fill.style.width = pct + '%';
      bar.appendChild(fill);
      const stack = el('div', 'df-stack df-stack--tight');
      stack.appendChild(label);
      stack.appendChild(bar);
      node.appendChild(stack);
    }

    const actions = (spec.actions || []).slice();
    if (typeof spec.onCancel === 'function') {
      actions.push({ label: spec.cancelLabel || 'Cancel', variant: 'secondary', onClick: spec.onCancel });
    }
    if (typeof spec.onClear === 'function' && !actions.length) {
      actions.push({ label: 'Clear filters', variant: 'primary', onClick: spec.onClear });
    }
    if (actions.length) {
      const row = el('div', 'df-state__actions');
      for (let i = 0; i < actions.length; i++) {
        const a = actions[i];
        const variantClass = 'df-btn--' + (a.variant || (i === 0 ? 'primary' : 'secondary'));
        const btn = el('button', 'df-btn ' + variantClass + ' df-btn--sm', { type: 'button' });
        setText(btn, a.label);
        btn.addEventListener('click', a.onClick);
        row.appendChild(btn);
      }
      node.appendChild(row);
    }
    if (spec.detail) {
      const wrap = el('div', 'df-state__detail');
      const details = el('details', 'df-disclosure');
      const summary = el('summary', 'df-summary');
      setText(summary, spec.detailLabel || 'What failed');
      const body = el('div', 'df-disclosure__body');
      const pre = el('pre', 'df-log');
      setText(pre, spec.detail);
      body.appendChild(pre);
      details.appendChild(summary);
      bindDisclosure(details);
      details.appendChild(body);
      wrap.appendChild(details);
      node.appendChild(wrap);
    }
    return handle(node);
  }

  function tryHandle(fn, args, variant, spec) {
    if (fn) {
      try {
        const h = fn.apply(null, args);
        if (h && h.el && h.el.nodeType === 1) return h;
        if (h && h.nodeType === 1) return handle(h);
      } catch (err) {
        console.warn('picker: ctx.states rejected a ' + variant + ' state', err); // eslint-disable-line no-console
      }
    }
    return fallbackState(variant, spec);
  }

  return {
    /**
     * One `--row-height` placeholder row for the virtualiser, taken from
     * `states.js`'s `skeletonRows` so that the placeholder and the real row
     * are the same height by construction rather than by two copies of the
     * number.
     */
    skeletonRow() {
      if (!fSkeleton) return null;
      let got;
      try {
        got = fSkeleton(1);
      } catch (_) {
        return null;
      }
      const container = got && got.el ? got.el : got;
      if (!container) return null;
      if (container.nodeType === 1) {
        const row = container.classList && container.classList.contains('df-skeleton-row')
          ? container
          : container.querySelector('.df-skeleton-row');
        if (row) {
          // states.js marks its placeholders aria-hidden because they are
          // decoration in an ordinary list. Inside this listbox a placeholder
          // occupies a real position, so the ARIA is re-stated by makeSlot.
          row.removeAttribute('aria-hidden');
          return row;
        }
      }
      if (Array.isArray(container) && container[0] && container[0].nodeType === 1) return container[0];
      return null;
    },

    /** spec: { title, text, progress, onCancel } */
    loading(spec) {
      return tryHandle(fLoading, [{
        label: spec.title,
        item: spec.text,
        progress: spec.progress,
        onCancel: spec.onCancel,
      }], 'loading', spec);
    },

    /** spec: { kind, title, text, actions, onClear } */
    empty(spec) {
      return tryHandle(fEmpty, [{
        kind: spec.kind,
        title: spec.title,
        message: spec.text,
        actions: spec.actions,
        onClear: spec.onClear,
      }], '', spec);
    },

    /** spec: { error, title, text, detail, detailLabel, actions } */
    error(spec) {
      return tryHandle(fError, [{
        error: spec.error,
        summary: spec.title,
        hint: spec.text,
        detail: spec.detail,
        detailLabel: spec.detailLabel,
        actions: spec.actions,
      }], 'error', spec);
    },
  };
}

/** A `.df-banner`, built without markup interpolation. */
function banner(variant, title, text, actions) {
  const node = el('div', 'df-banner df-banner--' + variant, {
    role: variant === 'danger' ? 'alert' : 'status',
  });
  node.appendChild(icon('df-banner__icon', variant === 'danger' ? ICON_OCTAGON : ICON_TRIANGLE));
  const body = el('div', 'df-banner__body');
  body.appendChild(setText(el('p', 'df-banner__title'), title));
  if (text) body.appendChild(setText(el('p', 'df-banner__text'), text));
  node.appendChild(body);
  // The actions column is kept even when empty: it holds a grid column.
  const acts = el('div', 'df-banner__actions');
  for (const a of actions || []) {
    const btn = el('button', 'df-btn df-btn--secondary df-btn--sm', { type: 'button' });
    setText(btn, a.label);
    btn.addEventListener('click', a.onClick);
    acts.appendChild(btn);
  }
  node.appendChild(acts);
  return node;
}

function formatBytes(n) {
  if (!n || n < 0) return '';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return (v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
}

/**
 * The `picker` screen.
 *
 * Composition: `picker-search.js` and `picker-tray.js` are loaded with dynamic
 * `import()` from `mount`, not with a static import at the top of the file, for
 * two reasons. It keeps `mount` synchronous, which is what the screen contract
 * promises the shell. And it lets this module — and therefore the virtualiser
 * inside it — load in `picker-list.demo.html` and in any harness where the
 * other two packages' files are not present yet, without the whole screen failing
 * to parse. The factory signatures used are exactly the frozen ones.
 */
const pickerScreen = {
  id: 'picker',
  title: 'Choose packages',

  mount(root, ctx) {
    this._ctx = ctx;
    this._root = root;
    this._states = statesAdapter(ctx);
    this._visible = false;
    this._destroyed = false;
    this._query = { text: '', category: '', appsOnly: false };
    this._browseAll = false;
    this._generation = -1;
    this._readyGeneration = -1;
    this._statusSequence = 0;
    this._selectionSequence = 0;
    this._detailSequence = 0;
    this._selectionRevision = -1;
    this._selected = new Set();
    this._pendingToggles = new Set();
    this._selectionComplete = false;
    this._attempted = new Set();
    this._pendingPreparation = new Map();
    this._cancelledPreparation = new Set();
    this._preparationErrors = new Map();
    this._categories = [];
    this._summary = null;
    this._lifecycle = null;
    this._mode = '';
    this._listRevealPending = false;
    this._statusPoll = 0;

    const screen = el('div', 'df-picker-screen df-screen-layout');
    const content = el('div', 'df-picker-content df-screen-content');
    const heading = el('h1', 'df-visually-hidden');
    setText(heading, 'Choose packages');
    const toolbar = el('div', 'df-picker-toolbar');
    const notices = el('div', 'df-picker-notices');
    const workspace = el('div', 'df-picker-workspace');
    const results = el('div', 'df-picker-results');
    const listHost = el('div', 'df-picker-list-host');
    const stateHost = el('div', 'df-picker-state');
    const trayHost = el('aside', 'df-picker-selection df-selection-pane', { 'aria-label': 'Selected packages' });
    const foot = el('div', 'df-actionbar');
    const gate = el('p', 'df-actionbar__status', { id: 'picker-build-gate', role: 'status', 'aria-live': 'polite' });
    const back = this._button('Back', () => ctx.go('target'), 'ghost');
    const undo = this._button('Undo', () => this._undo(), 'ghost');
    undo.hidden = true;
    const next = this._button('Review bundle', () => this._review(), 'primary');
    next.setAttribute('aria-describedby', gate.id);
    const add = this._button('Add…', () => this._openAddMenu(), 'secondary');
    add.setAttribute('aria-haspopup', 'menu');
    add.setAttribute('aria-describedby', gate.id);
    const selected = this._button('Selected (0)', () => this._openSelection(), 'ghost');
    selected.setAttribute('aria-controls', 'picker-selection-dialog');
    const details = this._button('Details', () => {
      const row = this._list.focusedRow();
      if (row) this._openDetails(this._list.focusIndex(), row);
      else { this._toast('info', 'Focus a package row to open its details.'); this._list.focus(); }
    }, 'ghost');
    details.setAttribute('aria-describedby', 'picker-list-help');
    const help = el('span', 'df-visually-hidden', { id: 'picker-list-help' });
    setText(help, 'Use arrow keys to move through packages. Space selects a package. Enter opens Details.');

    this._screen = screen;
    this._content = content;
    this._toolbar = toolbar;
    this._notices = notices;
    this._workspace = workspace;
    this._listHost = listHost;
    this._stateHost = stateHost;
    this._trayHost = trayHost;
    this._gate = gate;
    this._nextBtn = next;
    this._addBtn = add;
    this._selectedBtn = selected;
    this._undoBtn = undo;
    this._detailsBtn = details;

    this._search = createSearchField({
      onQueryChange: (_text, query) => this._setQuery(query),
      onCategoryChange: (_category, query) => this._setQuery(query),
    });
    this._list = createVirtualList({
      label: 'Package catalogue',
      search: (request) => ctx.bindings.SearchPackages(request),
      createSkeleton: () => this._states.skeletonRow(),
      isSelected: (row) => this._selectionComplete ? this._selected.has(row.name) : this._selected.has(row.name) || !!row.selected,
      onToggle: (_index, row) => this._toggle(row),
      onActivate: (index, row) => this._openDetails(index, row),
      onTotal: (total) => this._onTotal(total),
      onError: (error) => this._onSearchError(error),
      onFocusRow: () => this._applyGate(),
    });
    this._list.port.setAttribute('aria-describedby', help.id);
    this._tray = createTray({
      bindings: ctx.bindings,
      toast: (kind, message) => this._toast(kind, message),
      onRemove: (keys) => this._mutate('RemovePackages', [[].concat(keys)]),
      onClear: () => this._mutate('ClearSelection', []),
      onPaste: (text) => this._mutate('AddPackageList', [String(text || '')]),
      onAddURL: (url, sha256) => this._mutate('AddURLs', [[{ url: String(url || ''), sha256: String(sha256 || '') }]]),
      onAddFile: () => this._mutate('ChooseLocalDebs', []),
      onChange: (summary) => this._onSelectionChanged(summary),
      onUndoState: (state) => this._setUndo(state),
    });
    trayHost.appendChild(this._tray.el);
    listHost.appendChild(this._list.el);
    results.append(stateHost, listHost);
    workspace.append(results, trayHost);
    toolbar.append(this._search.el, add, selected, details, help);
    foot.append(back, gate, undo, next);
    content.append(heading, toolbar, notices, workspace);
    screen.append(content, foot);
    root.appendChild(screen);

    this._selectionDialog = this._makeDialog('Selected packages', 'picker-selection-dialog', 'df-selection-dialog');
    this._selectionDialog.el.addEventListener('close', () => {
      this._workspace.appendChild(this._trayHost);
      this._updateSelectionLayout();
      if (this._visible) this._selectedBtn.focus();
    });
    this._detailsDialog = this._makeDialog('Package details', 'picker-details-dialog');
    this._detailsDialog.el.addEventListener('close', () => {
      this._detailSequence++;
      const saved = this._detailFocus;
      if (this._visible && saved && saved.generation === this._generation && sameQuery(saved.query, this._query)) {
        this._list.scrollToIndex(saved.index);
        this._list.focus();
      } else if (this._visible) this._search.focus();
    });
    this._stopLayoutObserver = observeSelectionLayout((wide) => {
      this._wide = wide;
      this._updateSelectionLayout();
    });

    for (const name of ['catalog:started', 'catalog:progress', 'catalog:finished']) {
      ctx.on(name, (payload) => {
        const generation = payload?.generation ?? payload?.status?.generation;
        if (generation != null && Number(generation) < this._generation) return;
        this._refreshStatus();
      });
    }
    ctx.on('target:changed', () => { this._refreshStatus(); this._reloadSelection(); });
    ctx.on('selection:changed', (summary) => this._onSelectionChanged(summary));
    ctx.on('app:lifecycle', () => this._refreshLifecycle());
    this._showState(this._states.loading({ title: 'Loading packages…', text: 'Checking the selected target.' }));
    this._applyGate();
  },

  show(ctx, params = {}) {
    this._ctx = ctx || this._ctx;
    this._visible = true;
    if (params.prepare) this._prepareOnEntry = true;
    this._refreshStatus();
    this._refreshLifecycle();
    this._reloadSelection();
    this._reconnectList();
    this._list.measure();
    this._search.focus();
  },

  hide() {
    this._visible = false;
    clearTimeout(this._statusPoll);
    this._statusPoll = 0;
    this._selectionDialog?.el.close();
    this._detailsDialog?.el.close();
  },

  destroy() {
    this._destroyed = true;
    this.hide();
    this._statusSequence++;
    this._detailSequence++;
    this._selectionSequence++;
    this._stopLayoutObserver?.();
    this._releaseState();
    this._list.destroy();
    this._search.destroy();
    this._tray.destroy();
    this._selectionDialog.el.remove();
    this._detailsDialog.el.remove();
    this._root.replaceChildren();
  },

  _button(label, action, variant = 'secondary') {
    const button = el('button', 'df-btn df-btn--' + variant, { type: 'button' });
    setText(button, label);
    button.addEventListener('click', action);
    return button;
  },

  _toast(kind, message) { this._ctx?.toast?.(kind, message); },
  _toastError(error) { this._toast('danger', [error?.message || String(error), error?.hint].filter(Boolean).join(' ')); },

  async _call(method, args = []) {
    try {
      const binding = this._ctx.bindings[method];
      if (typeof binding !== 'function') throw new Error(method + ' is unavailable. Restart the current desktop build.');
      const result = await binding(...args);
      if (!result) throw new Error(method + ' did not return a result.');
      return result;
    } catch (error) {
      const failure = { code: 'app.binding', message: error.message || String(error), hint: 'Try again, or restart Debark.' };
      return { error: failure };
    }
  },

  _editReason() {
    if (this._lifecycleError) return 'Package changes are unavailable until job status can be checked. Reopen Packages to retry.';
    if (!this._lifecycle) return 'Checking whether package changes are available…';
    if (this._lifecycle.stopping) return 'Debark is stopping. Package changes are unavailable.';
    if (this._lifecycle.build_running || this._lifecycle.export_running) return 'Package changes are unavailable while a build or copy is running.';
    return '';
  },

  async _mutate(method, args) {
    const reason = this._editReason();
    if (reason) { this._toast('info', reason); return { error: { code: 'app.busy', message: reason } }; }
    const result = await this._call(method, args);
    if (result.error) this._toastError(result.error);
    if (typeof result.total === 'number') this._onSelectionChanged(result);
    return result;
  },

  async _refreshLifecycle() {
    const sequence = this._lifecycleSequence = (this._lifecycleSequence || 0) + 1;
    const result = await this._call('LifecycleStatus');
    if (this._destroyed || sequence !== this._lifecycleSequence) return;
    const before = this._editReason();
    this._lifecycleError = typeof result.build_running !== 'boolean';
    if (!this._lifecycleError) this._lifecycle = result;
    this._tray.setBusy(!!this._editReason());
    this._applyGate();
    this._renderNotices();
    if (before && !this._editReason()) this._refreshStatus();
  },

  _applyGate() {
    const reason = this._editReason();
    const hasSelection = (this._summary?.total || 0) > 0;
    this._addBtn.setAttribute('aria-disabled', String(!!reason));
    this._undoBtn.setAttribute('aria-disabled', String(!!reason));
    this._selectedBtn.setAttribute('aria-disabled', String(!hasSelection));
    this._nextBtn.setAttribute('aria-disabled', String(!hasSelection));
    this._detailsBtn.hidden = this._mode !== 'list';
    setText(this._gate, reason || (hasSelection ? '' : 'Choose packages, or use Add to enter a name, URL or file.'));
  },

  _review() {
    if (this._summary?.total > 0) { this._ctx.go('build'); return; }
    this._toast('info', 'Choose at least one package, or use Add.');
    this._search.focus();
  },

  async _openAddMenu() {
    if (this._editReason()) { this._toast('info', this._editReason()); return; }
    try { await this._tray.openAddMenu(this._addBtn); }
    catch (error) { this._toastError(error); }
  },

  _setUndo(state) {
    const wasFocused = document.activeElement === this._undoBtn;
    this._undoToken = state?.token || '';
    this._undoBtn.hidden = !this._undoToken;
    setText(this._undoBtn, state?.label || 'Undo');
    this._applyGate();
    if (wasFocused && !this._undoToken && this._visible) this._addBtn.focus();
  },

  async _undo() {
    if (!this._undoToken || this._editReason()) { if (this._editReason()) this._toast('info', this._editReason()); return; }
    try {
      const result = await this._tray.undo(this._undoToken);
      if (result?.error) this._toastError(result.error);
      await this._reloadSelection();
    } catch (error) { this._toastError(error); }
  },

  _updateSelectionLayout() {
    if (!this._trayHost) return;
    const hasSelection = (this._summary?.total || 0) > 0;
    if ((!hasSelection || this._wide) && this._selectionDialog?.el.open) this._selectionDialog.el.close();
    this._workspace.classList.toggle('is-selection-dialog', !this._wide);
    const inDialog = !!this._selectionDialog?.el.open;
    this._trayHost.hidden = !hasSelection || (!this._wide && !inDialog);
    this._selectedBtn.setAttribute('aria-expanded', String(inDialog || (!!this._wide && hasSelection)));
    this._selectedBtn.setAttribute('aria-haspopup', this._wide ? 'false' : 'dialog');
    this._list?.measure();
  },

  _openSelection() {
    if (!(this._summary?.total > 0)) { this._toast('info', 'No packages selected yet. Use search or Add.'); this._search.focus(); return; }
    if (this._wide) { this._tray.focus(); return; }
    this._selectionDialog.body.appendChild(this._trayHost);
    this._trayHost.hidden = false;
    this._selectionDialog.el.showModal();
    this._selectedBtn.setAttribute('aria-expanded', 'true');
    this._selectionDialog.close.focus();
  },

  _makeDialog(title, id, extraClass = '') {
    const dialog = el('dialog', 'df-dialog df-dialog--wide ' + extraClass, { id, 'aria-labelledby': id + '-title' });
    const inner = el('div', 'df-dialog__inner');
    const header = el('div', 'df-dialog__header');
    const heading = el('h2', 'df-dialog__title', { id: id + '-title' });
    setText(heading, title);
    const close = this._button('Close', () => dialog.close(), 'ghost');
    close.setAttribute('aria-label', 'Close ' + title.toLowerCase());
    const body = el('div', 'df-dialog__body');
    header.append(heading, el('span', 'df-spacer'), close);
    inner.append(header, body);
    dialog.appendChild(inner);
    this._root.appendChild(dialog);
    return { el: dialog, title: heading, body, close };
  },

  async _openDetails(index, row) {
    if (!row?.name) return;
    const sequence = ++this._detailSequence;
    const generation = this._generation;
    this._detailFocus = { index, name: row.name, generation, query: { ...this._query } };
    const dialog = this._detailsDialog;
    setText(dialog.title, row.display_name || row.name);
    setText(dialog.body, 'Loading package details…');
    if (!dialog.el.open) dialog.el.showModal();
    dialog.close.focus();
    const result = await this._call('GetPackage', [row.name]);
    if (this._destroyed || sequence !== this._detailSequence || generation !== this._generation || !dialog.el.open) return;
    dialog.body.replaceChildren();
    if (result.error || !result.found) {
      const error = result.error || { message: 'This package is no longer in the catalogue.', hint: 'Close Details and refresh the package list.' };
      const box = this._states.error({ title: error.message, text: error.hint, error, detail: error.details || '' });
      dialog.body.appendChild(box.el);
      return;
    }
    const detail = result.package;
    const description = el('p', 'df-picker-description');
    setText(description, detail.description || detail.summary || 'No description is available for this package.');
    dialog.body.appendChild(description);
    if (detail.description_truncated || (!detail.description && detail.summary)) {
      const note = el('p', 'df-muted');
      setText(note, detail.description_truncated
        ? 'Some description text is unavailable because it exceeds the catalogue metadata limit.'
        : 'Only a summary is available in this package list.');
      dialog.body.appendChild(note);
    }
    const facts = el('dl', 'df-picker-detail-facts');
    const fields = [
      ['Package', detail.name], ['Version', detail.version], ['Architecture', detail.arch],
      ['Source', detail.source], ['Archive', [detail.suite, detail.component].filter(Boolean).join(' / ')],
      ['Section', detail.section], ['Categories', (detail.categories || []).join(', ')],
      ['Download size', detail.download_size_bytes ? formatBytes(detail.download_size_bytes) : 'Unknown'],
      ['Installed size', detail.installed_size_bytes ? formatBytes(detail.installed_size_bytes) : 'Unknown'],
      ['Homepage', detail.homepage],
    ];
    for (const [label, value] of fields) {
      if (value == null || value === '') continue;
      const term = el('dt'); setText(term, label);
      const definition = el('dd'); setText(definition, value);
      facts.append(term, definition);
    }
    dialog.body.appendChild(facts);
    for (const warning of detail.warnings || []) {
      dialog.body.appendChild(banner('warning', warning.message || 'Package warning', warning.hint || ''));
    }
  },

  _releaseState() { this._stateHandle?.destroy?.(); this._stateHandle = null; },
  _showState(handle) {
    this._releaseState();
    this._stateHandle = handle;
    this._stateHost.replaceChildren();
    if (handle?.el) this._stateHost.appendChild(handle.el);
    this._stateHost.hidden = false;
    this._listHost.hidden = true;
    this._mode = 'state';
    this._applyGate();
  },
  _showList() {
    if (this._listHost.hidden) this._listRevealPending = true;
    this._releaseState();
    this._stateHost.replaceChildren();
    this._stateHost.hidden = true;
    this._listHost.hidden = false;
    this._reconnectList();
    this._mode = 'list';
    this._list.measure();
    this._applyGate();
  },

  _reconnectList() {
    if (!this._listRevealPending || !this._visible || this._listHost.hidden) return;
    this._listRevealPending = false;
    // Reconnect only after both the pane and list are visible, so native
    // accessibility can recompute ancestry. Retain the virtualiser and cursor.
    const list = this._list;
    const scrollTop = list.port.scrollTop;
    const hadFocus = list.el.contains(document.activeElement);
    list.el.remove();
    this._listHost.appendChild(list.el);
    list.port.scrollTop = scrollTop;
    if (hadFocus) list.port.focus({ preventScroll: true });
  },

  _hasQuery() { return !!(this._query.text || this._query.category || this._query.appsOnly || this._browseAll); },
  _setQuery(query) {
    this._query = normQuery(query);
    this._browseAll = false;
    this._search.setQuery(this._query);
    if (!this._catalogStatus?.ready) return;
    this._renderDiscovery();
  },
  _renderDiscovery(refresh = false) {
    if (!this._hasQuery()) {
      this._list.reset();
      this._search.setBusy(false);
      this._search.setResultCount(null);
      const handle = this._states.empty({
        kind: 'nothing-yet', title: 'Find packages for your target',
        text: 'Search by name or description, or choose a category.',
        actions: [{ label: 'Browse all packages', variant: 'secondary', onClick: () => {
          this._browseAll = true; this._renderDiscovery();
        } }],
      });
      const shortcuts = el('div', 'df-picker-categories', { role: 'group', 'aria-label': 'Package categories' });
      for (const category of this._categories.filter((item) => item.count > 0).slice(0, 8)) {
        shortcuts.appendChild(this._button(category.name || category.id, () => {
          this._setQuery({ text: '', category: category.id, appsOnly: false });
          this._search.focus();
        }, 'secondary'));
      }
      if (shortcuts.childElementCount) handle.el.appendChild(shortcuts);
      this._showState(handle);
      return;
    }
    this._showList();
    this._search.setBusy(true);
    if (refresh && sameQuery(this._list.getQuery(), this._query)) this._list.refresh({ keepScroll: true });
    else this._list.setQuery(this._query);
  },

  _onTotal(total) {
    if (!this._catalogStatus?.ready || !this._hasQuery()) return;
    this._search.setBusy(false);
    this._search.setResultCount(total);
    if (total > 0) { this._showList(); return; }
    this._showState(this._states.empty({
      kind: 'no-matches', title: 'No packages match',
      text: this._query.text ? 'Try another name or description, or clear the filters.' : 'Try another category or clear the filters.',
      actions: [{ label: 'Clear filters', variant: 'secondary', onClick: () => this._setQuery({}) }],
    }));
  },

  _onSearchError(error) {
    if (!this._catalogStatus?.ready || !this._hasQuery()) return;
    this._search.setBusy(false);
    if (error?.code === 'catalog.not_ready') { this._refreshStatus(); return; }
    this._showState(this._states.error({
      title: error?.message || 'Packages could not be searched',
      text: error?.hint || 'Try again after checking the selected target.',
      error, detail: error?.details || '',
      actions: [{ label: 'Retry', variant: 'primary', onClick: () => this._renderDiscovery(true) }],
    }));
  },

  async _refreshStatus() {
    if (this._destroyed) return;
    const sequence = ++this._statusSequence;
    const [targetResult, status] = await Promise.all([this._call('CurrentTarget'), this._call('CatalogStatus')]);
    if (this._destroyed || sequence !== this._statusSequence) return;
    if (targetResult.error || status.error && !status.state) {
      const error = targetResult.error || status.error;
      this._showState(this._states.error({ title: error.message, text: error.hint, error,
        actions: [{ label: 'Retry', variant: 'primary', onClick: () => this._refreshStatus() }] }));
      return;
    }
    const target = targetResult.target;
    if (Number(status.generation) !== Number(target.generation)) { this._pollStatus(40); return; }
    const generation = Number(target.generation);
    if (generation < this._generation) return;
    if (generation !== this._generation) {
      this._generation = generation;
      this._target = target;
      this._readyGeneration = -1;
      this._categories = [];
      this._list.reset();
      this._detailsDialog.el.close();
      this._onSelectionChanged(await this._call('Selection'));
      if (this._destroyed || sequence !== this._statusSequence) return;
    }
    if (this._prepareOnEntry && target.selected) {
      this._prepareGeneration = generation;
      this._prepareOnEntry = false;
    }
    this._applyStatus(status, target);
  },

  _pollStatus(delay = 350) {
    clearTimeout(this._statusPoll);
    if (this._visible) this._statusPoll = setTimeout(() => { this._statusPoll = 0; this._refreshStatus(); }, delay);
  },

  _applyStatus(status, target = this._target) {
    if (!status.ready && !status.building && this._preparationErrors.has(this._generation)) {
      status = { ...status, state: 'failed', cancelled: false, error: this._preparationErrors.get(this._generation) };
    }
    this._catalogStatus = status;
    this._target = target;
    if (!target?.selected || status.state === 'none') {
      this._showState(this._states.empty({
        title: 'Choose a target first', text: 'Packages are listed for your target system.',
        actions: [{ label: 'Choose target', variant: 'primary', onClick: () => this._ctx.go('target') }],
      }));
      this._renderNotices();
      return;
    }
    if (status.ready && !status.building) {
      const firstReady = this._readyGeneration !== this._generation;
      this._readyGeneration = this._generation;
      if (firstReady) { this._loadCategories(); this._renderDiscovery(true); }
      this._renderNotices();
      return;
    }
    if (status.building || status.state === 'building' || this._pendingPreparation.has(this._generation)) {
      this._attempted.add(this._generation);
      const progress = status.progress || {};
      this._showState(this._states.loading({
        title: this._cancelledPreparation.has(this._generation) ? 'Stopping package preparation…' : 'Loading packages…',
        text: [progress.label, progress.item].filter(Boolean).join(' · '),
        progress: progress.overall_fraction >= 0 ? progress.overall_fraction : null,
        onCancel: this._cancelledPreparation.has(this._generation) ? undefined : () => this._cancelPreparation(),
      }));
      this._pollStatus();
      return;
    }
    if (status.cancelled || status.state === 'cancelled') {
      this._attempted.add(this._generation);
      this._showState(this._states.empty({
        title: 'Package loading cancelled', text: 'Your selection is unchanged. Retry when you are ready.',
        actions: [{ label: 'Retry', variant: 'primary', onClick: () => this._startPreparation(false, true) }],
      }));
    } else if (status.error || status.state === 'failed') {
      this._attempted.add(this._generation);
      this._showState(this._states.error({
        title: status.error?.message || 'Packages could not be loaded',
        text: status.error?.hint || 'Check your connection and try again.',
        error: status.error, detail: status.error?.details || '',
        actions: [{ label: 'Retry', variant: 'primary', onClick: () => this._startPreparation(true, true) }],
      }));
    } else if (this._prepareGeneration === this._generation && !this._attempted.has(this._generation)) {
      this._startPreparation(false, false);
    } else {
      this._showState(this._states.empty({
        title: 'Packages are not loaded yet', text: 'Load packages to search this target.',
        actions: [{ label: 'Load packages', variant: 'primary', onClick: () => this._startPreparation(false, true) }],
      }));
    }
    this._renderNotices();
  },

  async _startPreparation(force, explicit) {
    const generation = this._generation;
    if (this._pendingPreparation.has(generation) || this._catalogStatus?.building) return;
    if (!explicit && this._attempted.has(generation)) return;
    if (this._editReason()) {
      this._showState(this._states.empty({ title: 'Package preparation is unavailable', text: this._editReason(),
        actions: [{ label: 'Retry', variant: 'secondary', onClick: () => this._startPreparation(force, true) }] }));
      return;
    }
    this._attempted.add(generation);
    this._cancelledPreparation.delete(generation);
    this._preparationErrors.delete(generation);
    this._readyGeneration = -1;
    this._list.reset();
    const pending = this._call('StartCatalogBuild', [!!force]);
    this._pendingPreparation.set(generation, pending);
    this._applyStatus({ ...this._catalogStatus, ready: false, building: true, cancelled: false, error: null, state: 'building', progress: {} });
    const result = await pending;
    this._pendingPreparation.delete(generation);
    if (this._destroyed || generation !== this._generation) return;
    if (result.error) {
      this._preparationErrors.set(generation, result.error);
      this._applyStatus({ ...this._catalogStatus, ready: false, building: false, state: 'failed', error: result.error });
      return;
    }
    await this._refreshStatus();
  },

  async _cancelPreparation() {
    const generation = this._generation;
    this._attempted.add(generation);
    this._cancelledPreparation.add(generation);
    this._applyStatus(this._catalogStatus);
    const result = await this._call('CancelCatalogBuild');
    if (result.error) { this._cancelledPreparation.delete(generation); this._toastError(result.error); }
    if (!this._destroyed && generation === this._generation) await this._refreshStatus();
  },

  async _loadCategories() {
    const generation = this._generation;
    const result = await this._call('Categories');
    if (this._destroyed || generation !== this._generation) return;
    if (result.error) { this._search.setCategories(result); return; }
    this._categories = Array.isArray(result.categories) ? result.categories : [];
    this._search.setCategories(result);
    if (!this._hasQuery() && this._catalogStatus?.ready) this._renderDiscovery();
  },

  _renderNotices() {
    this._notices.replaceChildren();
    if (this._catalogStatus?.stale) {
      this._notices.appendChild(banner('info', 'Package list may be out of date', '',
        [{ label: 'Refresh', onClick: () => this._startPreparation(true, true) }]));
    }
    if (this._summary?.unknown_count > 0 && this._catalogStatus?.ready) {
      this._notices.appendChild(banner('warning', 'Some selected packages are unavailable in this catalogue',
        'They remain selected. The bundle build checks whether they can be installed.',
        [{ label: 'Review selection', onClick: () => this._openSelection() }]));
    }
  },

  async _toggle(row) {
    const name = row?.name;
    if (!name || this._pendingToggles.has(name)) return;
    if (this._editReason()) { this._toast('info', this._editReason()); return; }
    const wasSelected = this._selected.has(name) || (!this._selectionComplete && row.selected);
    this._pendingToggles.add(name);
    if (wasSelected) this._selected.delete(name); else this._selected.add(name);
    this._list.repaint();
    const summary = await this._mutate(wasSelected ? 'RemovePackages' : 'AddPackages', [[name]]);
    this._pendingToggles.delete(name);
    if (summary.error || !(wasSelected ? summary.removed : summary.added)) {
      if (wasSelected) this._selected.add(name); else this._selected.delete(name);
      this._list.repaint();
      this._reloadSelection();
    }
    if (!summary.error) {
      for (const rejected of summary.rejected || []) this._toast('warning', [rejected.reason, rejected.hint].filter(Boolean).join(' '));
    }
  },

  _onSelectionChanged(summary) {
    if (!summary || summary.error || typeof summary.total !== 'number') return;
    if (typeof this._summary?.revision === 'number' && summary.revision < this._summary.revision) return;
    const focusWasInTray = this._trayHost.contains(document.activeElement);
    this._summary = summary;
    this._tray.render(summary);
    this._setUndo({ token: summary.undo_token, label: summary.undo_label });
    setText(this._selectedBtn, 'Selected (' + summary.total + ')');
    this._updateSelectionLayout();
    if (focusWasInTray && this._trayHost.hidden && this._visible) {
      (this._undoToken ? this._undoBtn : this._addBtn).focus();
    }
    this._applyGate();
    this._renderNotices();
    if (summary.revision !== this._selectionRevision || !this._selectionComplete) this._reloadSelection(true);
  },

  async _reloadSelection(haveSummary = false) {
    const sequence = ++this._selectionSequence;
    const result = await this._call('SelectionKeys');
    if (this._destroyed || sequence !== this._selectionSequence) return;
    if (result.error) this._toastError(result.error);
    if (!result.error && !(result.revision < this._selectionRevision)) {
      this._selectionRevision = result.revision;
      this._selectionComplete = !result.truncated;
      if (!result.truncated) this._selected = new Set(result.keys || []);
      this._list.repaint();
    }
    if (!haveSummary) {
      const summary = await this._call('Selection');
      if (!this._destroyed && sequence === this._selectionSequence) this._onSelectionChanged(summary);
    }
  },
};

export default pickerScreen;
