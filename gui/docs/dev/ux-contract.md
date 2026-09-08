# Desktop UX contract — 2026-09-07

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

This contract and the implementation plan
supersede historical presentation and ownership choices.
Engine/security/product invariants in contract-brief.md remain binding.

## Frame and layout

Internal routes stay `target`, `picker`, `build`, `export`, `readiness`.
Visible stages are Target (target), Packages (picker), Bundle (build/export).
Readiness is a utility view outside the count. Copying an external bundle
does not complete the current draft. Start on Target; check readiness async.
Use native frame; proposed client 1040x720, minimum 960x640.
No shell footer or shell Back. Each screen uses `.df-screen-layout` containing
`.df-screen-content` (bounded scroll) and one sibling `.df-actionbar`.
Shared CSS owner supplies these classes and `.df-disclosure`, `.df-menu`.
Existing lifecycle/scoped events/native disclosure workaround are preserved.
Picker rows remain fixed 32px. Selection pane 260px at client widths >=1024;
below 1024 (including effective width at enlarged text) use a named dialog.
Confirm this breakpoint at foundation checkpoint before screen integration.

## Routes and immutable jobs

`ctx.go('picker', { prepare: true })` follows successful target Continue.
Picker alone owns automatic preparation: one attempt per target generation;
ready cache wins, pending calls deduplicate, cancellation/failure waits for
explicit Retry. Read status on entry and on events; ignore stale generations.
Unchanged target revisit retains selection/query/scroll/cache. Target change
revalidates retained selections in Go, invalidates Undo, never zeros the UI count.

Build/export jobs are mutually exclusive. Bindings reject conflicting target,
selection and catalogue mutations while either job runs or close is stopping.
Views/navigation remain available. UI explains disabled edits. Status is
authoritative; job IDs/generation/event sequence protect terminal states.
BuildStatus target_id/item_count/started_at and summary are immutable run data.
Add `target_generation` and `selection_revision` checkpoint fields to status.
Build completion fulfils the draft only for a complete successful build matching
both captured revisions. Later edits restore the unsaved-selection close warning.

Build -> copy route is:
`ctx.go('export', { bundlePath: summary.bundle_path, bundleSummary: summary,
sourceTarget: status.target_id, sourceMode: 'result', returnTo: 'build' })`.
Summary is the backend BuildSummary; signature/completeness comes from this
object, never the current draft. Export also retrieves BuildStatus and accepts
its metadata only for an exact matching bundle path. Unknown external signing
or completeness is explicitly unknown until authoritatively inspected.
Menu existing-copy route: `{sourceMode:'choose', returnTo: opener}`. Export
clears stale source/outcome, invokes ChooseDirectory for source, and returns
to opener on cancellation. Existing running copy takes precedence over route.
Back from result copy goes to build; another-copy resets skip_verify=false.

## Additive native bindings (B owns implementation and generated surface)

* `ChooseSigningKey() FilePickResult`: native file chooser, reference only.
* `UndoSelection(token string) SelectionSummary`: atomic one-step restore.
* SelectionSummary adds `undo_token` string and `undo_label` string; empty token
  means unavailable. Keep existing `revision` and paged SelectionPage/keys.
* TargetView and CatalogStatus add `generation` uint64; catalogue progress and
  finished payloads carry target generation (additive) where needed.
* `LifecycleStatus() LifecycleView`: `build_running`, `export_running`,
  `stopping`, `unsaved_selection`, `target_generation`, `selection_revision`,
  and optional `error` UIError. Event `app:lifecycle` uses this same projection.
* `RequestClose() Result` invokes the same native close policy as window Close.
  OnBeforeClose returns promptly; native confirmation runs asynchronously.
  Keep working is default; Stop and close cancels and waits bounded cleanup.
  Timeout keeps app open with an actionable error. No force-close or second
  unsaved prompt after deliberate Stop and close. Wire OnShutdown.

Undo saves original values/order/digests only in Go, never redacted URL copies.
Remove/Clear create one eligible token; intervening mutation or target change
invalidates it. Invalid token returns an actionable error without mutation.
Close warns on nonempty unbuilt selection; there is no persistence/recovery.
Key creation uses existing RunReadinessAction. Its Result stays unchanged;
after completion return to Choose key with a precise instruction. Never send
private key contents into webview, silently fall back to unsigned, or add keys.

## Picker factory signatures

`createSearchField({onQueryChange,onCategoryChange})` keeps full query snapshots
`{text,category,appsOnly}` and actual returned methods `el,focus,setCategories,
setBusy,setResultCount,clear,clearFilters,getQuery,destroy`.
Actual callback arguments are `(text, snapshot)` and `(category, snapshot)`;
consumers use the complete second argument. All packages remain default.
`setQuery(snapshot)` is an additive silent setter for category shortcuts.

`createTray({bindings,toast,onRemove,onClear,onPaste,onAddURL,onAddFile,
onChange,onUndoState})` retains injected bindings, revision guards and paging.
`onAddURL(url,sha256)` MUST retain digest. Existing mutation callbacks retain
their actual return shape. `onChange(summary)` informs parent of authoritative
mutations; `onUndoState({token,label})` updates the screen action-area Undo.
Return existing `el,render,setBusy,destroy,focus,openPasteDialog,
openURLDialog,openClearDialog`, plus `openAddMenu(trigger)`, `undo(token)`.
D owns always-visible Add button and action-area Undo button; E owns Add menu,
dialogs and backend Undo calls. E's dialogs must work even with pane hidden.
No duplicated selection counts. Enter opens GetPackage details, Space selects;
closing restores same logical row focus through virtualization.

`PackageDetail` adds optional plain-text `description` and boolean
`description_truncated`. GetPackage hydrates only that entry's bounded
extended text; search/hydration rows remain summary-only. Description text is
limited to 64 KiB per entry and 64 MiB across the parsed/merged catalogue;
cache format 2 stores bounded compressed descriptions and rejects older cache
versions for rebuild. No Translation fetch or dependency interpretation is
added. Details labels missing or truncated text explicitly and preserves the
summary; render all package text safely. Exact compressed-data bounds are in
binding-surface.md and cache-format.md.

## Ownership and acceptance

Coordinator owns contracts, Makefile/CI, container operations and integration.
A owns main.js, shell/*, design/*, frontend/index.html only.
B owns main.go, internal/app/{bindings,types,events}.go, ux_*_test.go,
surface_test.go and generated frontend/wailsjs/go only.
H owns hack/ui-review/*, docs/dev/ui-review.md, shared fixtures and the UX
screenshot evidence tree kept outside this repository; approved hostile harness
files only.
Then C owns target/readiness modules+demos; D picker-list/search modules+demos;
E picker-tray module+demo; F build module+demo; X export module+demo.
Only A edits shared CSS; screen workers report CSS requests. No Docker by
workers, no go mod tidy, no drive D writes, no engine/website changes.
Do not commit another worker's files; explicit paths and staged review required.

Acceptance follows all P0/P1 cases in the outer plan. Screenshots S0-S4 use
actual modules, a linked manifest with revisions/dirty identity, fixture/live,
engine, dimensions/theme/scaling, and inspected images. Final native evidence
must be actual rebuilt Wails Linux/WebKitGTK, with separate binding-to-offline
installation proof. Unknown/blocked checks are never passes.
