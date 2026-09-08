# Screen contract

**Desktop UX revision, 2026-09-07:** [ux-contract.md](ux-contract.md) is the
current presentation, routing, job/edit, picker-factory and handoff contract.
The lifecycle conventions below remain binding.

**Frozen at the outset.** Every screen module in `frontend/src/screens/` implements
this, and `frontend/src/shell/shell.js` is the only thing that calls it. It
exists so that ten screens can be written concurrently without ten different
ideas about how a screen gets on the page.

## The module shape

A screen is an ES module with a default export:

```js
export default {
  id: 'target',                  // stable, unique, used in navigation
  title: 'Choose a target',      // shown in the header
  mount(root, ctx) {},           // once, on first navigation. Build the DOM here.
  show(ctx, params) {},          // every time this screen becomes visible
  hide() {},                     // every time it stops being visible
  destroy() {},                  // on shutdown. Release listeners, timers, observers.
}
```

`mount` is called **once**, lazily, the first time a screen is navigated to —
cold start budget is 1.5 s, so screens must not all build their DOM up front.
`show` may be called many times; it must be cheap and idempotent.

`root` is an empty `<div>` the shell owns. Build inside it. Never reach outside
it, never touch `document.body`, and never assume another screen's DOM exists.

## `ctx` — everything a screen may use

```js
ctx = {
  go(id, params),        // navigate to another screen
  back(),                // navigation history
  on(event, handler),    // subscribe to a backend event; auto-removed on destroy
  off(event, handler),
  bindings,              // window.go.app.App — the generated Wails bindings
  toast(kind, message),  // transient notice: 'info' | 'success' | 'warning' | 'danger'
  states,                // the shared loading/empty/error helpers (shell/states.js)
  theme,                 // the theme controller (shell/theme.js)
}
```

**Use `ctx.on`, never `runtime.EventsOn` directly.** The shell tracks
subscriptions and tears them down on `destroy`; a screen that subscribes behind
the shell's back leaks a listener that fires against a dead DOM. `ctx` is
frozen, so a screen cannot stash state on it — that would invent a channel
other screens cannot see and the shell cannot tear down.

### What the shell modules export

Named here because the first version of this document did not, and the shell
ended up guessing — with a probe list that did not include the real name, so
the theme control silently never appeared in the header.

```js
// shell/theme.js
export function createThemeController(opts?) → { el, get, set, effective, onChange, destroy }
// shell/states.js — the helpers reached as ctx.states
export function loadingState(...)  → { el, update, destroy }
export function emptyState(...)    → { el }
export function errorState(...)    → { el }
export function skeletonRows(n)    → { el }
export function inlineBanner(...)  → { el, dismiss }
```

### One screen-owned action area

There is no shell footer or shell Back. Each screen owns a bounded
`.df-screen-content` and one sibling `.df-actionbar` inside
`.df-screen-layout`. The shell supplies available height. `ctx` has no
`setActions`; actions stay with their screen's state. Utility Back destinations
are explicit. Target/Packages/Bundle are the three visible stages.

### `show`, `hide` and re-navigation

Navigating to the screen already showing is a **re-show with new params** —
`show(ctx, params)` is called again and `hide()` is **not**. `hide` means
"stopped being visible", and firing it here makes a screen tear down state it
is about to need.

### When `mount` throws

The shell contains it: the failure renders an error state in that screen's own
pane with a Retry that fully resets the module, its subscription scope and its
DOM, and every other destination keeps working. **Screens therefore do not need
their own try/catch around `mount`** — let it throw and the shell will render
it properly.

## Rules

1. **A screen never calls a Wails binding that blocks.** Every long operation
   is start-and-subscribe: call `Start…`, then render from events. The bound
   methods are designed for this — see `binding-surface.md`.
2. **A screen renders three states or it is not finished**: loading, empty, and
   error. `ctx.states` provides them so they look the same everywhere. A screen
   that only handles the happy path will be sent back.
3. **Nothing appears frozen.** Every wait shows what it is waiting on — the
   backend's progress events carry a human label precisely so a spinner never
   has to stand alone.
4. **Every error says what to do next.** The backend's error shape already
   carries a summary and a hint; render both. Never render an exit code alone.
5. **No screen holds the full catalogue.** Search is paged and virtualised;
   ask for what is visible.
6. **No engine logic.** No version comparison, no dependency reasoning, no
   sorting by "newest". If you are tempted, the answer is a backend call.
7. **Markup uses the design system's classes** (`frontend/src/design/`). Do not
   write bespoke component CSS in a screen; if a component is missing, say so
   rather than inventing a one-off.
8. **Keyboard reachable.** Every control is tabbable and has a visible
   `:focus-visible`. This is a definition-of-done item, not a polish pass.

## Registration

`shell.js` imports each screen module and registers it. Screens do **not**
register themselves and do not import each other. If two screens need to share
something, it belongs in `frontend/src/shell/` or in the backend.

## The screens

| id | file | purpose |
|---|---|---|
| `readiness` | `screens/readiness.js` | contextual System check utility |
| `target` | `screens/target.js` | stock base, or a snapshot file |
| `picker` | `screens/picker-*.js` | the main screen: list, search, tray |
| `build` | `screens/build.js` | progress over the event stream, log drawer |
| `export` | `screens/export.js` | save the bundle, copy to a drive |

`picker` is three files by ownership (`picker-list.js`, `picker-search.js`,
`picker-tray.js`) but **one screen**. `picker-list.js` exports the screen
module; the other two export components it composes. They agree on this:

```js
// picker-search.js
export function createSearchField({ onQueryChange, onCategoryChange }) → { el, focus, setCategories, setBusy, setResultCount, clear, clearFilters, getQuery, destroy }
// picker-tray.js
export function createTray({ bindings, toast, onRemove, onClear, onPaste, onAddURL, onAddFile, onChange, onUndoState }) → { el, render, setBusy, destroy, focus, openPasteDialog, openURLDialog, openClearDialog, openAddMenu, undo }
```

Neither reaches into the other's DOM. State lives in the backend
(`Selection()`, `SearchPackages()`); the tray renders what the backend reports
rather than keeping its own copy, so two views can never disagree.

Package Details uses `GetPackage` for the selected logical row. Show its
plain-text `description` when available; otherwise identify the summary-only
fallback. Keep `description_truncated` visible when extended text is bounded
or unavailable. Search rows and selection hydration stay summary-only, and
the backend owns description/cache bounds; see binding-surface.md. Enter or
the explicit Details action opens this view independently of Space selection.
Closing restores focus to the same logical package through virtualization.
