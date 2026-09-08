# The Wails binding surface

**Desktop UX revision, 2026-09-07:** additive methods/DTOs, native lifecycle,
job/edit policy and build-to-copy metadata are frozen in
[ux-contract.md](ux-contract.md). That contract supersedes historical first-run
readiness, destructive Clear, and any presentation instructions below.
New methods: ChooseSigningKey, UndoSelection, LifecycleStatus, RequestClose.
SelectionSummary gains undo_token/undo_label; target/catalogue payloads gain
generation; BuildStatus gains draft checkpoint revisions. Private references
remain backend-owned. Generated bindings are the executable surface.

**Frozen contract 3.** This is the complete list of Go methods the frontend may
call and the complete list of events the backend may emit. It is written for
someone building a screen in JavaScript who should never have to read Go to
know what a method returns.

Implemented in `internal/app/bindings.go` (methods), `internal/app/types.go`
(the shapes) and `internal/app/events.go` (the names). If this document and
those files ever disagree, the code is right and this is stale — but they must
not disagree, because you are reading this instead of the code.

---

## How to call anything

Wails generates one JavaScript function per exported method on the bound
struct:

```js
import * as App from "../wailsjs/go/app/App.js";

const page = await App.SearchPackages({ text: "firefox", limit: 50 });
```

Every generated function returns a promise.

### Rule 1 — promises resolve, they do not reject

No method on this surface returns a Go `error`. Wails turns a Go error into a
promise rejection carrying only a string, which throws away the hint, the
command that ran, the captured stderr and the exit class — exactly the
information that makes a failure actionable. So every fallible method resolves
with an object carrying an `error` field instead:

```js
const r = await App.StartCatalogBuild(false);
if (r.error) { showError(r.error); return; }
```

A rejected promise means the bridge itself broke. Handle it as a bug, not as a
user-facing error.

### Rule 2 — the error object

```ts
{
  code:       string,    // stable id. Branch on this. Never parse `message`.
  message:    string,    // one plain sentence. Always set.
  hint?:      string,    // what to do next, imperative. Usually set.
  details?:   string,    // long/ugly raw text for the collapsed drawer
  command?:   string[],  // the exact argv that failed, when a process was run
  exit_class?: string,   // debark's class: "environment", "resolution", ...
  exit_code?: number|null,
  retryable:  boolean,   // may running the same thing again work?
  doc_url?:   string
}
```

**Render `message` as the banner and `hint` as the next line.** Put `details`
and `command` behind a disclosure. Never show `exit_code` on its own — "every
error path renders an actionable message; none surface a raw exit code alone"
is a definition-of-done item for the whole project.

Codes you will branch on:

| `code` | Means | The screen's move |
|---|---|---|
| `target.none` | No target chosen yet | Send them to target selection |
| `catalog.not_ready` | No catalogue for this target | Offer `StartCatalogBuild` — this is the normal first run, not a failure |
| `catalog.failed` | Catalogue build failed | Banner with retry |
| `selection.empty` | Build asked for with an empty tray | Disable the build button instead |
| `app.busy` | That operation is already running | Disable the control |
| `app.cancelled` | The operator cancelled | Not an error; do not show a banner |
| `app.too_many_items` | Batch above a documented limit | Split the batch |
| `app.invalid_input` | Bad or missing argument | Highlight the field |
| `cli.failed` | debark returned non-zero | Banner + details drawer + the command |
| `cli.missing` | debark could not be found | Send them to readiness |
| `export.failed` / `verify.failed` | The drive or the bundle | Banner + details |
| `app.not_implemented` | A stub that has not landed | Dev builds only |
| `app.no_runtime` | Headless (test harness) | You will not see this in the app |

### Rule 3 — three speed tiers

| Tier | Meaning | You may call it |
|---|---|---|
| **fast** | Pure memory. Microseconds. | On every render, in a loop, from a scroll handler |
| **quick** | Bounded work with a deadline: one subprocess, one page of an index, one directory walk. | On an interaction; show a spinner past ~200 ms |
| **fire-and-forget** | Starts work and returns immediately. Progress arrives as events. | Once, then listen |

Native file dialogs are the deliberate exception: they block until the operator
dismisses them, which is what a modal does.

### Rule 4 — events are not a source of truth

Every long-running operation has a **status** method as well as its events. A
screen that mounts after the events fired, a drawer that reopens, or a reloaded
window has missed everything. **On mount, read the status; then subscribe.**

```js
import { EventsOn } from "../wailsjs/runtime/runtime.js";

let status = await App.BuildStatus();   // recover
EventsOn("build:progress", p => { status.progress = p; render(); });
```

### Rule 5 — paging is not optional

No method takes or returns an unbounded list of catalogue rows. The limits:

| Constant | Value | Applies to |
|---|---|---|
| search page, default | 50 | `SearchPackages` with `limit: 0` |
| search page, max | 500 | `SearchPackages`; a larger `limit` is clamped and `truncated` is set |
| hydrate batch, max | 500 | `PackageRows` |
| tray page, max | 500 | `SelectionPage` |
| tray keys, max | 50 000 | `SelectionKeys`; past it `truncated` is set and **no** keys come back |
| add batch, max | 5000 | `AddPackages`, `AddURLs`, `AddLocalDebs` |
| pasted list, max | 4 MiB | `ParsePackageList`, `AddPackageList` |
| build log page, max | 500 | `BuildLog` |
| build log retained | 5000 events | the ring behind `BuildLog`; older ones are counted in `dropped` |

---

## Methods

Forty-eight. `Bind`, `Startup` and `Shutdown` are bound as well but are not
part of this contract: they exist for `main.go`. **Never call `Bind()` from
JavaScript**, even though Wails generates a stub for it.

### Application

| JS | Returns | Tier | Events |
|---|---|---|---|
| `AppInfo()` | `AppInfo` | quick (probes the binary once, then cached) | — |
| `CopyToClipboard(text)` | `Result` | fast | — |
| `RevealPath(path)` | `Result` | fast | — |

```ts
AppInfo = {
  name, version, platform, arch: string,
  debark_path?, debark_version?: string,
  headless: boolean,          // true only in a test harness
  progress_events: boolean,   // this debark can stream build progress
  error?: UIError             // set when the binary could not be probed;
                              // the rest of the object is still valid
}
Result = { ok: boolean, error?: UIError }
```

`RevealPath` opens the operator's file manager at that path. Use it for "open
the bundle folder" after a build.

**`progress_events: false` means the build screen must not draw a bar.** It is
debark's `--json-events` capability. An older binary does not have the flag,
so a build against it still runs and still finishes, but **no** `build:progress`
and **no** `build:event` will ever arrive: the details drawer stays empty and
`BuildProgress.phase` never leaves `"starting"`. Show an indeterminate spinner
and say the binary is too old to report progress. It is also `false` before the
binary has been probed and whenever probing failed, which is the safe reading —
a screen that assumed progress and got none has nothing to fall back to.

### Screen 1 — readiness and first run

| JS | Returns | Tier | Events |
|---|---|---|---|
| `Readiness()` | `ReadinessReport` | fast | — |
| `StartReadinessCheck()` | `Result` | fire-and-forget | `readiness:started`, `readiness:finished` |
| `RecheckReadiness(checkID)` | `Result` | fire-and-forget | `readiness:progress`, `readiness:finished` |
| `RunReadinessAction(checkID)` | `Result` | fire-and-forget | `readiness:progress`, `readiness:finished` |
| `CancelReadinessCheck()` | `Result` | fast | `readiness:finished` (with `cancelled: true`) |

```ts
ReadinessReport = {
  checks: ReadinessCheck[],
  can_build: boolean,          // nothing blocking
  blocking_count, degraded_count: number,
  platform?: string,           // "linux/amd64"
  checking: boolean,           // a full pass is in flight
  running_check?: string,      // one row is being re-checked or acted on
  checked_at?: string,         // RFC 3339 UTC
  duration_ms: number,
  remote_builder?: RemoteBuilderAdvice,   // present only when it applies
  error?: UIError
}

RemoteBuilderAdvice = {
  reason: "no-build-environment" | "local-options-blocked",
  message: string,             // what is true
  hint: string,                // what to do
  blocked?: string[]           // the check ids that led here
}

ReadinessCheck = {
  id: string,                  // "debark-binary" | "apt" | "container" |
                               // "wsl" | "self-binary" |
                               // "build-environment" | "signing-key" |
                               // "disk-space" | "archive-network"
  title: string,
  status: "ok" | "problem" | "skipped",
  severity: "blocking" | "degraded" | "info",
  summary: string,             // what is true. Always set.
  remedy?: string,             // what to do. Always set when status is "problem".
  action?: ReadinessAction,
  detail?: string,             // raw evidence for a drawer
  duration_ms: number,
  running: boolean,            // this one row is spinning
  derived: boolean             // computed from the other rows; see below
}

ReadinessAction = {
  label: string,               // button text
  command: string[],           // the exact argv
  display: string,             // that argv as a person would type it
  elevated: boolean,           // needs root/Administrator
  runnable: boolean,           // false whenever elevated is true
  note?: string
}
```

**`status` and `severity` are different questions.** `status` is "did this
pass"; `severity` is "how much does it matter that it did not". A passing check
is always `severity: "info"`. Colour the row by severity, but only when
`status === "problem"`.

**A row with `status: "problem"` always has a `remedy`.** That invariant is
what makes this a list of rows with actions instead of a wall. Show it.

**Show `remote_builder` above the rows, not inside one.** When it is present,
building on another machine is the way forward, and on a machine where nothing
local can run apt it is the only advice that helps — burying it under "install
WSL" wastes it. Do not compute this yourself from `platform` and a set of check
ids: it is a product decision and it now lives on the backend, where a rewording
of one row's `remedy` cannot silently change when the panel appears. `blocked`
names the rows to point at.

`reason: "no-build-environment"` is the derived row failing outright.
`reason: "local-options-blocked"` is the Windows case it does not always reach
in time: WSL and a container runtime are the only local routes, and on a managed
laptop policy blocks both.

**`derived: true` means do not offer "check again" on that row.** Exactly one
row is derived — `build-environment`, the answer to "is there any way at all to
run apt on this machine", read off the `apt` and `container` rows and gated by
`self-binary`. It has no probe, so `RecheckReadiness` refuses it outright. It is
also one of only two **blocking** rows, so a screen that offered the button
there offered it on the row where it matters most and does nothing.

**`wsl` is context, not a route, and this changed in the third round.** A build
runs apt natively or inside a container and nothing else: debark's `--backend` takes
`local`, `container` or `auto`, `core/lock` records only the first two, and no
code in either repository ever invokes `wsl.exe` to do work. On Windows the
`apt` row is not registered at all, so the container backend is the only route
there — and because a non-Linux host mounts and re-execs a Linux build of
debark, that route is gated by `self-binary`.

The derivation used to count a green `wsl` row as a way to build, and the
consequence was not cosmetic. The `self-binary` gate only fires when no other
route is `ok`, so a working WSL 2 installation silently defeated it:

| `wsl` | `container` | `self-binary` | → `build-environment` | `can_build` |
|---|---|---|---|---|
| ok | ok | **problem** | ok — "can run a build using WSL 2" | **true** |
| ok | problem | **problem** | ok — same | **true** |
| problem | ok | **problem** | blocking — gate fires | false |
| ok | ok | ok | ok | true |

Row 1 is an ordinary Windows machine with Docker Desktop, measured on real
hardware: `can_build: true`, a green "This machine can build bundles" banner,
and the build then failing. Row 3 is the only combination where the gate worked
and it needs a Windows machine with **no** WSL 2 — but Docker Desktop's default
backend *is* WSL 2 and `docker-desktop` registers itself as a WSL 2
distribution, so on the machines the gate was built for it could essentially
never fire.

The `wsl` row is still probed and still shown, because Docker Desktop normally
runs on it and a WSL fault is very often the explanation for the `container`
row failing beside it. Its summary no longer says builds can run without a
container. **The visible effect is that a Windows machine with WSL 2 and no
usable container now reports `can_build: false` where it used to report
`true`.** That machine could not build; saying so is the point.

Point the operator at the inputs instead. A derived row follows them: re-check
`container`, and `build-environment` is recomputed with it and arrives in the
same `readiness:finished` payload.

**`build-environment` is gated by `self-binary`.** A container runtime that
answers is not the same thing as a container backend debark can use: on a
non-Linux host debark mounts and re-execs a Linux build of itself, so a green
`container` row beside a failing `self-binary` row is exactly the combination
that used to report "this machine can build" and then fail every build. When
`self-binary` is a problem, the `container` row is still counted as considered
— the runtime exists — but not as a way to build, and `build-environment` says
so in its own words rather than sending the operator to install a runtime they
are already running. The row appears only where it applies; on Linux it is
never registered, and a `build-environment` computed from a set that does not
contain it is not gated by it.

`self-binary` also appears in `remote_builder.blocked` when it is the
obstacle, for the same reason: a panel naming only `container` would point at
the row that is green.

`reason: "local-options-blocked"` follows the same correction. Windows has one
local route, and it takes two rows to work — a container runtime that answers,
and a Linux build of debark to mount into it. Either failing breaks it, so
the panel now appears when **either** is a problem. It used to require `wsl`
*and* `container` to both be blocked, which meant a machine with healthy WSL 2
and a broken container route never saw the panel at all.

**`runnable: false` means show the command, do not offer the button.** The
application does not escalate privilege. Offer `CopyToClipboard(action.display)`
and a sentence telling the operator to run it in an elevated terminal, then
`RecheckReadiness(id)` when they come back.

### Screen 2 — target selection

| JS | Returns | Tier | Events |
|---|---|---|---|
| `SupportedArchitectures()` | `ArchesResult` | quick | — |
| `ListBases(arch)` | `BasesResult` | quick (one `snapshot list-bases --json`) | — |
| `ChooseSnapshotFile()` | `FilePickResult` | blocks on the dialog | — |
| `InspectSnapshot(path)` | `SnapshotResult` | quick | — |
| `SelectTarget(sel)` | `TargetResult` | quick | `target:changed` |
| `CurrentTarget()` | `TargetResult` | fast | — |
| `ClearTarget()` | `Result` | fast | `target:changed` (with `selected: false`) |

```ts
ArchesResult = { arches: string[], default: string, error?: UIError }

BasesResult = {
  arch: string,                // what the listing was actually rendered for.
                               // Read this; do not assume it echoes your input.
  bases: BaseView[],
  caveat: string,              // the assumption sentence — SHOW IT
  error?: UIError
}

BaseView = {
  id: string,                  // "ubuntu:24.04/desktop"
  description?: string,
  distro_id, version_id, codename, arch: string,
  variant?: string,
  seeds?: string[], excludes?: string[], recommends: boolean,
  digest: string
}

TargetSelection = {            // the argument to SelectTarget
  kind: "base" | "snapshot",
  base_id?: string, arch?: string,     // required when kind is "base"
  snapshot_path?: string               // required when kind is "snapshot"
}

TargetView = {
  selected: boolean,           // false before anything is chosen
  kind?: "base" | "snapshot",
  id?: string,                 // base id, or the snapshot's identity
  label?: string,              // "Ubuntu 24.04 (amd64)" — for a header
  distro_id?, version_id?, codename?, variant?, arch?: string,
  snapshot_path?: string,
  assumed: boolean,            // true for a stock base
  caveat?: string,             // set whenever assumed is true
  components?: string[],       // what the catalogue will cover — see below
  suites?: string[],
  catalog_ready: boolean       // a usable cache exists; skip to the picker
}

SnapshotView = {
  path: string, schema_version?: string,
  distro_id, version_id, codename, arch: string, variant?: string,
  foreign_archs?: string[],    // extra dpkg architectures the target enabled
  origin_kind?: "captured" | "synthesized",
  created_at?: string, package_count: number,
  // all three empty for a CAPTURED snapshot: they describe the base a
  // synthesized one was made from, and that emptiness is the signal
  base_id?: string,            // "ubuntu:26.04/desktop"
  origin_source?: string,      // "builtin", or the operator's file's base name
  origin_source_digest?: string
}
```

**`origin_source_digest` was called `digest`, and the rename is the point.** It
is the digest of the BASE DEFINITION a synthesized snapshot was made from, and
is empty for every captured snapshot — that is, for exactly the snapshots this
product tells operators to prefer. A screen written against the old name would
have rendered a blank identity field for the good case. There is no digest of
the snapshot itself on this surface.

**`components` and `suites` are on `TargetView`, not `SnapshotView`.** They only
exist after resolution: a snapshot captures its apt sources as *files*, so
`InspectSnapshot` has nothing parsed to report, and the stanzas are parsed when
the target is selected. Show them — `universe` present or absent is the
difference between 70,000 rows and 25,000, and an operator who cannot find a
package is usually looking at a target whose sources do not carry it, which the
picker itself can never say.

**Nothing is uploaded anywhere.** Say so on the snapshot half of this screen.
The path is read locally by the debark binary and by nothing else.

**Show the caveat whenever `assumed` is true.** A stock base is an assumption
about a default install, not a measurement of the machine. `origin_kind:
"synthesized"` on a snapshot means the same thing.

**Cancelling a dialog is not an error.** Check `cancelled` before `error`:

```ts
FilePickResult = {
  path: string,        // "" when cancelled
  paths: string[],     // single-select dialogs return 0 or 1 entry here
  cancelled: boolean,
  error?: UIError
}
```

### Screen 3 — the catalogue

| JS | Returns | Tier | Events |
|---|---|---|---|
| `CatalogStatus()` | `CatalogStatus` | fast | — |
| `StartCatalogBuild(force)` | `Result` | fire-and-forget | `catalog:started`, `catalog:progress`×n, `catalog:finished` |
| `CancelCatalogBuild()` | `Result` | fast | `catalog:finished` (with `cancelled: true`) |
| `SearchPackages(q)` | `SearchResult` | quick — **< 100 ms p95 including this round-trip** | — |
| `Categories()` | `CategoriesResult` | quick | — |
| `GetPackage(name)` | `PackageResult` | quick | — |
| `PackageRows(names)` | `PackageRowsResult` | quick | — |

```ts
CatalogStatus = {
  state: "none" | "not-built" | "building" | "ready" | "failed" | "cancelled",
  target_id?: string,
  ready: boolean,              // Search may be called
  building: boolean,
  stale: boolean,              // usable, but the archive has moved on
  progress: CatalogProgress,
  package_count: number,
  built_at?: string,
  from_cache: boolean,
  cancelled: boolean,
  error?: UIError
}

CatalogProgress = {
  phase: "resolve"|"release"|"download"|"parse"|"index"|"save"|"done",
  phase_index, phase_count: number,
  label: string,               // present-tense sentence, never empty
  item?: string,               // the specific index file, when there is one
  current, total: number,      // total 0 means "not yet known"
  bytes_done, bytes_total: number,
  fraction: number,            // 0..1 within the phase, or -1 = indeterminate
  overall_fraction: number,    // 0..1 across the whole build, or -1
  elapsed_ms: number
}
```

Draw the bar from `overall_fraction`; it always moves forwards and reaches the
end exactly when the build does. **`-1` means indeterminate** — show a spinner
or a barber-pole, never a bar stuck at zero. Show `label` large and `item`
small: a path is not an explanation.

`stale: true` is not an error. Offer a refresh (`StartCatalogBuild(true)`);
do not block on it. Nothing incorrect can follow from browsing a stale
catalogue, because the build re-resolves against the live archive regardless of
what was on screen.

**`stale` can only ever be true on the cache path**, and until the third round
nothing assigned it there — so the refresh banner could not appear for the only kind of
catalogue that can be out of date. `package_count` and `built_at` had the same
shape: the first was assigned nowhere at all, which is why `Categories().Total`
was always 0, and the second only on the build path.

All three now come off the catalogue through `CatalogInsight`, an **optional
interface `internal/app` type-asserts for** rather than a widening of frozen
contract 2. The contract answers "what can I install on this target?" with five
methods every implementation must supply; none of these three facts is needed
to answer it, and none is knowable to an implementation that holds no cache. A
`Catalog` that does not implement `CatalogInsight` behaves exactly as before,
with the fields at their documented not-ready values. Nothing about the shape
above changed — these are fields the frontend was already told it would get.

> **Open, and it blocks the banner in production.** `stale` is a comparison
> between the target's live index digest and the one recorded in the cache
> marker, and *nothing populates either side*. `catalog.Target.IndexDigest` is
> never set by the resolver, and `CacheInput.IndexDigest` is never set by a
> build. Both empty means "freshness unknown", and unknown is deliberately not
> stale — so on a real machine `stale` is false for every catalogue, however
> old. The plumbing above is correct and tested, and it has nothing to compare
> until `docs/dev/cache-format.md` §6.2 is implemented: fetch each suite's
> `Release`, hash its `SHA256:` section into a digest, record it on save, and
> re-derive it in the background after the picker opens. That is a network
> fetch, which `Ready()` is forbidden to do, so it is its own piece of work.

```ts
SearchQuery = {               // the argument to SearchPackages
  text: string,               // substring, case-insensitive, over name+summary
  category?: string,          // a CategoryView.id, tier-prefixed
  offset: number,             // may be large and may jump
  limit: number,              // 0 => 50; > 500 is clamped
  apps_only?: boolean
}

SearchResult = {
  rows: PackageRow[],
  total: number,              // matches, not rows returned — size the scrollbar
  offset, limit: number,      // the NORMALISED values; trust these, not yours
  query: SearchQuery,         // echoed — see below
  took_ms: number,            // backend time, excluding the bridge
  truncated: boolean,         // your limit was clamped
  error?: UIError
}
```

**There is no sort option and no "selected only" filter.** Ordering is part of
the contract, not the caller's choice — name/app-name matches first, then
summary matches, each group by name ascending — so every screen ranks
identically. The tray is its own list; read it with `SelectionPage`.

**Use the echoed `query` to drop stale pages.** A debounced search box fires
several requests over an asynchronous bridge and they can complete out of
order. Compare `result.query.text` against what is currently typed and discard
anything that does not match, or you will show results for "fire" after the
operator finished typing "firefox".

```ts
PackageRow = {
  name: string,
  display_name?: string,      // DEP-11 application name, "Firefox Web Browser"
  version?: string,           // DISPLAY ONLY — never compare two of these
  suite?, component?, arch?, section?, priority?: string,
  categories?: string[],
  summary?: string,
  installed_size_bytes: number,   // BYTES (converted from apt's KiB here)
  download_size_bytes: number,
  is_app: boolean,
  selected: boolean,          // may be one frame stale; the tray is authoritative
  source: "apt" | "url" | "file",
  warnings?: SelectionWarning[]
}
```

**`version` is a label, not a fact about what will be installed.** Nothing in
this application compares two version strings — that is engine work and apt is
the oracle. Do not sort by it, do not diff it, do not decide anything from it.

**There are no dependency fields, deliberately.** `Depends` and friends are the
raw material of dependency reasoning, and the surest way to stop this
application growing a second resolver is to never carry them across the bridge.
The authoritative answer to "what else will this pull in" is the build, arriving
as events.

```ts
PackageDetail = {  // same fields as PackageRow, plus:
  description?: string,          // plain text from supplied Packages metadata
  description_truncated?: boolean, // bounded/omitted/unreadable extended text
  homepage?: string, url?: string, path?: string
}
PackageResult = { package: PackageDetail, found: boolean, error?: UIError }
```

`found: false` with no error is the ordinary answer for a name the target's
indexes do not carry. It is not a failure.

`GetPackage` alone hydrates the extended description; `SearchPackages` and
`PackageRows` remain summary-only. The text comes from the supplied Packages
stream, including its continuation lines; this does not fetch Translation
indexes or add a dependency field. Render it as text. If the description is
absent, show the summary with an explicit summary-only explanation; when
`description_truncated` is true, keep that limitation visible in Details.

Backend bounds are 64 KiB of UTF-8 description per package and 64 MiB across
the parsed/merged catalogue. Cache format 2 stores descriptions compressed,
with 64 KiB + 1 KiB per compressed entry and 64 MiB aggregate compressed data.
Only the requested Details entry is decompressed, under the same 64 KiB
output limit. Invalid/oversized compressed text yields no extended text and
sets `description_truncated`; it must not erase the searchable summary.
Older cache versions are rejected for rebuild, rather than treated as format 2.

```ts
CategoriesResult = { categories: CategoryView[], total: number, error?: UIError }
CategoryView = {
  id: string,                 // "app:Graphics" | "section:net"
  tier: "application" | "section",
  name: string, count: number
}
```

The one listing that is not paged — there are tens of these, not tens of
thousands. Render the `application` tier first: it is what an operator who does
not know package names is looking for. Order is stable across calls, so the
sidebar does not reshuffle.

```ts
PackageRowsResult = { rows: PackageRow[], missing?: string[], error?: UIError }
```

> **There are no application icons on this surface, and there will not be.**
> `PackageIcon`, `IconResult` and `PackageRow.icon_ref` were removed rather than
> implemented. `internal/catalog` records an opaque `icon_ref` and deliberately
> never fetches the image behind it, so the method could only ever answer
> `found: false` — a documented handle with nothing to hand it to.
>
> The index research is what settles it rather than the missing accessor:
> fetching DEP-11 icons costs 7.9 MB to decorate the 3% of rows that have one,
> and about half of those are fonts. Every row needs a placeholder regardless,
> so the icons buy a nicer version of a list that already works, for a download
> the operator pays on first run. Distinguish applications from libraries with
> `is_app` and `categories`, which is what an operator was actually looking for.
> If icons are ever wanted, this comes back as a new method with a cache and a
> budget, not as a field on the row that crosses the bridge fifty at a time.

### Screen 3 — the selection tray

| JS | Returns | Tier | Events |
|---|---|---|---|
| `Selection()` | `SelectionSummary` | fast | — |
| `SelectionKeys()` | `SelectionKeysResult` | fast | — |
| `SelectionPage(offset, limit)` | `SelectionPage` | fast | — |
| `AddPackages(names)` | `SelectionSummary` | quick | `selection:changed` |
| `AddURLs(urls)` | `SelectionSummary` | quick | `selection:changed` |
| `AddLocalDebs(paths)` | `SelectionSummary` | quick | `selection:changed` |
| `ChooseLocalDebs()` | `FilePickResult` | blocks on the dialog | — |
| `RemovePackages(keys)` | `SelectionSummary` | fast | `selection:changed` |
| `ClearSelection()` | `SelectionSummary` | fast | `selection:changed` |
| `ParsePackageList(text)` | `ParsedList` | quick | — |
| `AddPackageList(text)` | `SelectionSummary` | quick | `selection:changed` |

```ts
SelectionSummary = {
  total, package_count, url_count, file_count, unknown_count: number,
  installed_size_bytes, download_size_bytes: number,
  warnings?: SelectionWarning[],
  rejected?: RejectedInput[],   // only on the mutating calls
  added, removed: number,       // what THIS call did
  revision: number,             // identity of the current KEY SET — see below
  error?: UIError
}

SelectionEntry = {
  key: string,        // identity: the name / the URL / the absolute path.
                      // This is what RemovePackages takes.
  name: string, display_name?, version?, summary?: string,
  source: "apt" | "url" | "file",
  url?, path?: string,
  installed_size_bytes, download_size_bytes: number,
  known: boolean,     // present in the catalogue
  warnings?: SelectionWarning[]
}

SelectionPage = { entries: SelectionEntry[], total, offset, limit: number, error?: UIError }

SelectionKeysResult = {
  keys: string[],     // every SelectionEntry.key, in tray order
  total: number,      // the true count, even when truncated
  revision: number,   // matches SelectionSummary.revision
  truncated: boolean, // > 50 000 entries: keys is EMPTY, not partial
  error?: UIError
}

SelectionWarning = {
  kind: "snap-transitional" | "external-url" | "large-download" |
        "unknown-package" | "arch-mismatch",
  package?: string, message: string, hint?: string, doc_url?: string
}

RejectedInput = { value: string, reason: string, hint?: string }
```

**Mark the virtualised list from `SelectionKeys`, never from `SelectionPage`.**
A recycled row has to know whether the package it is about to draw is selected,
and `PackageRow.selected` is a snapshot taken when that page was produced — it
may be one frame stale. `Selection()` returns counts, not keys. `SelectionKeys`
is the authoritative answer in one round trip:

```js
let keys = new Set(), rev = -1;

async function syncKeys(summary) {
  if (summary && summary.revision === rev) return;   // the key set did not move
  const r = await App.SelectionKeys();
  if (r.revision < rev) return;                      // a stale response; drop it
  keys = r.truncated ? null : new Set(r.keys);       // null => show the count, no ticks
  rev = r.revision;
}

await syncKeys(await App.Selection());
EventsOn("selection:changed", s => { syncKeys(s); renderTrayHeader(s); });

// drawing a recycled row
row.selected = keys ? keys.has(pkg.name) : false;
```

For an apt row the key **is** the package name, so marking a row is a set
membership test against `PackageRow.name`. URL rows key on the URL and file rows
on the absolute path, neither of which a catalogue row can collide with.

`revision` moves when a key enters or leaves the tray, and deliberately does
**not** move when an entry's contents change under an unchanged key — a tray row
hydrating against a freshly built catalogue, say. Those are not changes to what
is selected, and a cached key set stays correct across them. It only ever
increases, including across `ClearSelection`.

**Warnings are shown at selection time, not at build time.** The
`snap-transitional` case is why: on Ubuntu desktop the archive's `firefox` is a
stub that pulls in a snap, so a bundle containing it does not contain a browser.
Telling someone that after a twenty-minute build is telling them too late.

**`known: false` entries are kept, not dropped.** The operator may know
something the index does not; debark gives the authoritative answer at build
time. Mark them, do not remove them.

**Sizes are estimates, and the UI must say so.** They count nothing about
dependencies, which this application never sees and must never reason about.

**Paste-a-list is two calls.** `ParsePackageList` previews; `AddPackageList`
commits the same text. The preview never returns the whole parsed list:

```ts
ParsedList = {
  count: number,              // distinct names the text yielded
  new_count: number,          // not already in the tray
  known_count: number,        // found in the catalogue
  unknown?: string[],         // capped at 100 entries
  unknown_count: number,      // the true total
  duplicates: number,
  sample?: PackageRow[],      // up to 50 hydrated rows for a preview table
  rejected?: RejectedInput[],
  warnings?: SelectionWarning[],
  error?: UIError
}
```

A pasted list is read the way `packages.txt` is: one name per line, `#` starts
a comment, blanks ignored, duplicates collapsed.

### Screen 4 — build and progress

| JS | Returns | Tier | Events |
|---|---|---|---|
| `PreviewCommand(opts)` | `CommandPreview` | fast | — |
| `StartBuild(opts)` | `Result` | fire-and-forget | `build:started`, `build:progress`×n, `build:event`×n, `build:finished` |
| `CancelBuild()` | `Result` | fast | `build:finished` (with `cancelled: true`) |
| `BuildStatus()` | `BuildStatus` | fast | — |
| `BuildLog(sinceSeq, limit)` | `BuildLogPage` | fast | — |
| `ChooseDirectory(title, defaultDir)` | `FilePickResult` | blocks on the dialog | — |

**The target and the tray are not arguments.** They are application state, so
the tray stays the single source of truth for what gets built and cannot
disagree with what the operator is looking at.

```ts
BuildOptions = {
  output_dir: string,          // required
  output_name?: string,        // default: distro-version-arch-YYYYMMDD
  format?: "dir" | "tar",      // default "dir"
  signer_ref?: string,         // --sign; setting it makes signing REQUIRED
  no_sign?: boolean,           // --no-sign
  sbom?: boolean,
  recommends?: boolean | null, // three states — see below
  no_recommends?: boolean,     // DEPRECATED spelling of recommends: false
  upgrades?: boolean, update?: boolean, no_prune?: boolean,
  backend?: "auto" | "local" | "container",
  acknowledge_redistribution?: boolean
}
```

Every field maps to exactly one debark flag. There is no "dry run", because
debark has no such flag — the CLI stays the complete interface.

**`recommends` has three states, and a checkbox cannot express it.**

| Value | Flag | Means |
|---|---|---|
| absent / `null` | none | follow the target's own `apt.conf` |
| `false` | `--no-recommends` | exclude recommended packages: a smaller bundle |
| `true` | `--recommends` | pull recommended packages in |

Send `null`, not `false`, for "let the target decide" — they are different
builds. A target whose `apt.conf` turns recommends on gets them under `null` and
does not under `false`. A three-way control (follow / include / exclude) is the
honest widget; a two-state checkbox silently collapses the first two.

`no_recommends` is the older one-directional spelling, kept only because the
CLI had no `--recommends` when this surface was frozen and the current frontend
still sends it. `recommends` wins outright whenever it is set, so the two cannot
contradict each other. **Drop `no_recommends` in the redesign.**

**`acknowledge_redistribution` is a record that a person was asked.** Set it
only after the operator has actually seen and accepted the notice on screen.

```ts
CommandPreview = { argv: string[], display: string, error?: UIError }
```

**Show this.** Rule 8 of the project: anything the GUI can do must be
expressible as a command, and the GUI shows that command where it reasonably
can. `display` is shell-quoted for reading and copying; nothing in this
application ever passes a string to a shell. Put `CopyToClipboard(display)`
beside it.

**`argv` and `display` are redacted, and the copy beside them must say so.**
They are `cliadapter.RedactArgv`'s output, not the exact argv: a vendor URL's
credentials **and its entire query string** are replaced by the visible marker
`REDACTED`. The exact command is still what runs — only the text differs — so
a caption promising the shown line reproduces the build "exactly", or runs
as-is in a terminal, is false and must not be written. Redaction touches two
argv positions only, a `url:` positional and the URL half of a `--digest
URL=SHA` pair; the SHA-256 survives, because it is not a secret. It fires on
userinfo **or** a query string, so a secret-free query string is redacted too
— the parameter carrying a presigned credential has no standard name and
cannot be told apart. With no such URL in the tray, shown and executed are
identical, which is most builds. There is deliberately **no reveal control**:
the operator's own URL is in the selection tray where they typed it.

**`PreviewCommand` validates, in the same order `StartBuild` does.** On refusal
`argv` and `display` are empty and `error` carries the reason. So the frontend
must render that error, and **must not present a Ready state beside it** — a
screen that computes its own enablement from form state alone will say the
command is ready above an empty block.

```ts
BuildStatus = {
  running, finished, cancelled: boolean,
  progress: BuildProgress,
  command?: string[],          // what this build ran, redacted for display
  target_id?: string, item_count: number,
  started_at?, finished_at?: string,
  event_count: number,         // badge the drawer with this
  summary?: BuildSummary,      // null until it finishes
  error?: UIError
}

BuildProgress = {
  phase: "idle"|"starting"|"resolving"|"downloading"|"assembling"|
         "signing"|"verifying"|"finished"|"failed"|"cancelled",
  message?: string, package?: string,
  current, total: number,
  bytes_done, bytes_total: number,
  fraction: number             // 0..1, or -1 = indeterminate
}

BuildSummary = {
  bundle_path?, bundle_id?, lock_ref?, manifest_ref?: string,
  signed: boolean,
  stats: { added, removed, unchanged, package_count: number,
           bytes, downloaded_bytes: number, duration_seconds: number },
  warnings?, unresolved?, fetch_failed?: string[],   // each capped at 500
  truncated: boolean,
  exit_class?: string
}
```

**Progress is indeterminate by default. Drive the screen from `phase`.** A
build screen has to render correctly having received nothing but the phase, and
there are three independent reasons it will:

- The located debark has no `--json-events`. `AppInfo.progress_events` says so
  before a build starts, and then **no** `build:progress` or `build:event`
  arrives at all.
- The **container backend** emits about eleven events for a complete build — no
  `fetch.file`, no `progress`, not even `apt.resolve` — because the inner
  container process's events do not cross back out. Measured on a real run; it
  is the shipping behaviour, not a bug to wait out.
- On the local backend `apt.resolve`'s selection count is an upper bound on
  work rather than a file count, and arrives *after* apt has already downloaded
  everything inside one blocking exec.

So `fraction` is `-1` far more often than it is a number, `package` is
frequently empty, and a bar that only moves when counters do will sit still
through an entire successful build. Show an indeterminate indicator plus the
phase name, and treat every counter as a bonus.

**`exit_class: "incomplete"` is not success.** It means a bundle was written but
is short — a URL failed, or an external `.deb` has unsatisfiable dependencies —
and `unresolved` and `fetch_failed` are the only place that detail exists. The
screen must not present it as a clean build. A finished build can therefore
arrive with **both** a `summary` and an `error`.

**`signed: false` must be said out loud.** An unsigned bundle is a legitimate
outcome; a silent one is not.

```ts
BuildLogPage = {
  events: BuildEvent[],
  next_seq: number,            // pass back as sinceSeq
  total: number,               // how many the run has produced
  dropped: number,             // fell out of the 5000-event ring
  error?: UIError
}

BuildEvent = {
  seq: number,                 // 1-based position in the run
  ts?, type, level?, msg?: string,   // type is debark's: "apt.resolve", ...
  attrs?: Record<string, any>,
  raw?: string                 // the exact NDJSON line, for copy-paste
}
```

The details drawer is **collapsed by default** — this audience trusts what it
can inspect, and hiding the log entirely costs more than it gains. Two ways to
fill it, and you should use both:

- Live: subscribe to `build:event`. A build emits thousands, so do not render
  them unless the drawer is open.
- On open, or after a reload: page `BuildLog(0, 500)`, then `BuildLog(next_seq,
  500)` until `next_seq` stops moving.

When `dropped > 0`, say so. Do not imply the log is complete; the full record
is in the bundle's `evidence.json`.

### Screen 5 — export to a mounted volume

| JS | Returns | Tier | Events |
|---|---|---|---|
| `ListVolumes()` | `VolumesResult` | quick — cheap enough to poll | — |
| `InspectDestination(opts)` | `DestinationView` | fast | — |
| `PlanExport(opts)` | `ExportPlanResult` | quick (walks the bundle) | — |
| `StartExport(opts)` | `Result` | fire-and-forget | `export:started`, `export:progress`×n, `export:finished` |
| `CancelExport()` | `Result` | fast | `export:finished` (with `cancelled: true`) |
| `ExportStatus()` | `ExportStatus` | fast | — |
| `StartVerify(bundlePath)` | `Result` | fire-and-forget | `verify:started`, `verify:finished` |
| `CancelVerify()` | `Result` | fast | `verify:finished` |
| `VerifyStatus()` | `VerifyStatus` | fast | — |

**Mounted volumes only, and a plain file copy.** No block device is ever
opened, nothing is partitioned or formatted, no bootable medium is written, and
nothing needs root. That is a permanent product decision, not a missing feature.

```ts
VolumesResult = {
  volumes: VolumeView[],
  fingerprint?: string,        // changes when the set meaningfully changes
  supported: boolean,          // false => offer a plain folder picker instead
  error?: UIError
}

VolumeView = {
  path: string,                // the identity — pass it as `destination`
  label: string, device?, fs_type?: string,
  total_bytes, free_bytes: number,   // free is space available to THIS user
  kind: "removable" | "fixed" | "network" | "unknown",
  removable, read_only, writable: boolean,
  note?: string                // "mounted read-only", "capacity unavailable"
}
```

Poll on a 1 s timer if you want live drive detection; compare `fingerprint` and
skip the redraw when it has not moved. The list arrives with removable media
first and the machine's own root filesystem last, so a keyboard-driven operator
never lands on it by default. `supported: false` is not an error — the platform
simply cannot enumerate; offer `ChooseDirectory` instead.

```ts
ExportOptions = {
  bundle_path?: string,        // "" => the bundle the last build produced
  destination: string,         // a volume's `path`, or any directory
  subdir?: string,             // "" => the bundle folder's own name
  skip_verify?: boolean        // leave it false
}

ExportPlan = {
  source_dir, dest_dir: string,
  total_files: number, total_bytes: number,
  required_bytes: number,      // with allocation rounding and the margin
  free_bytes: number,          // -1 = unknown
  margin_bytes: number,
  warnings?: string[],         // FAT filename, >4 GiB file, unknown free space
  destination_incomplete: boolean   // see below
}
ExportPlanResult = { plan: ExportPlan, error?: UIError }

DestinationView = {
  destination: string,         // what the operator chose, echoed back
  path: string,                // where the bundle would land: destination + subdir
  exists: boolean,
  incomplete: boolean,         // an interrupted copy's marker is still there
  marker_name: string,         // the marker file's name; never build a path from it
  message?: string,            // the sentence to show; set only when incomplete
  error?: UIError
}
```

**Show the plan and let the operator confirm.** An error from `PlanExport` —
including the free-space refusal — means the copy must not start.

**Warn before the operator confirms, not after.** A destination that still
carries the marker an interrupted export left behind looks exactly like a
finished bundle and is not one — it is the only thing on this screen an operator
cannot see for themselves. Call `InspectDestination` the moment a destination is
chosen; it is two stat calls and writes nothing, so it can run before the bundle
walk `PlanExport` pays for:

```js
const d = await App.InspectDestination({ destination: vol.path });
if (d.incomplete) showWarning(d.message);   // not a refusal: copying again is the fix
```

`ExportPlan.destination_incomplete` carries the same fact, so a screen that only
plans is not left blind. Neither is a refusal — copying again is exactly the
right fix, and the copy rewrites the marker and only removes it once every file
has been read back and matched.

```ts
ExportStatus = {
  running, finished, cancelled: boolean,
  progress: ExportProgress,
  bundle_path?, destination?, destination_path?: string,
  started_at?, finished_at?: string,
  verified: boolean,           // the post-copy check passed
  verify_skipped: boolean,
  summary?: string,            // what verification did and did not prove
  verify_method?: string,      // one sentence: exactly what was checked
  verify_caveat?: string,      // one sentence: the honest limit of that check
  files_checked: number, bytes_checked: number,
  mismatches?: ExportMismatch[],     // capped at 500
  mismatch_count: number,            // the true total — render THIS
  mismatches_truncated: boolean,
  error?: UIError
}

ExportMismatch = {
  path: string,                // relative to the bundle folder
  reason: "missing" | "size" | "content" | "not_copied" | "unreadable",
  detail?: string,
  want_bytes, got_bytes: number,
  want_sha256?, got_sha256?: string
}

ExportProgress = {
  phase: "idle"|"checking"|"copying"|"flushing"|"verifying"|
         "finished"|"failed"|"cancelled",
  message?, file?: string,
  files_done, files_total: number,
  bytes_done, bytes_total: number,
  bytes_per_sec: number,
  fraction: number             // 0..1, or -1
}
```

**Never show an unverified copy as verified.** `verified` and `verify_skipped`
are separate for exactly that reason.

**Print `verify_method` and `verify_caveat`; never hard-code them.** They are
the exporter's own account of what it checked and what that does and does not
prove, and only the exporter knows it — a copy of those sentences in JavaScript
goes stale the moment the exporter changes what it verifies, and then the screen
is making a claim that is no longer true. `verify_method` says something else
entirely when `skip_verify` was set, which a constant cannot.

**A failed verification names the files.** `mismatches` is the only place that
detail exists; "the copy did not verify" without it is a wall, and the
operator's next question is always which files and how. `reason: "content"` is
the important one — right length, wrong bytes, which a size check alone cannot
see — and `want_sha256` / `got_sha256` are what make "this drive corrupted the
copy" a fact rather than a guess. Render `mismatch_count`, not
`mismatches.length`: the list is capped at 500.

**A cancelled copy leaves the drive marked bad, on purpose.** The exporter
writes an incomplete marker before its first byte and only a passing
verification removes it. Say so when the operator cancels: the drive holds a
partly copied bundle and must not be used.

```ts
VerifyStatus = {
  running, finished, cancelled: boolean,
  bundle_path?: string,
  ok: boolean,                 // meaningful only when finished
  signed: boolean,
  findings?: { severity: "error"|"warning"|"info",
               code?, message, hint?, path?: string }[],
  exit_class?: string,
  started_at?, finished_at?: string,
  error?: UIError
}
```

`ok: true, signed: false` is a real and distinct outcome: the bundle is intact
and unsigned. Distinguish it from `ok: true, signed: true`.

---

## Events

Subscribe with Wails' runtime:

```js
import { EventsOn, EventsOff } from "../wailsjs/runtime/runtime.js";
const off = EventsOn("catalog:progress", p => render(p));
// ... later
off();
```

Every payload is the **single argument** to the callback.

| Name | Payload | Fires |
|---|---|---|
| `readiness:started` | `ReadinessReport` | once, when a full pass begins |
| `readiness:progress` | `ReadinessProgress` | around a single re-check or action |
| `readiness:finished` | `ReadinessFinished` | exactly once per pass, re-check or action |
| `target:changed` | `TargetView` | whenever the target changes, including being cleared |
| `catalog:started` | `CatalogStatus` | once, when a catalogue build begins |
| `catalog:progress` | `CatalogProgress` | many times during a build |
| `catalog:finished` | `CatalogFinished` | exactly once per build |
| `selection:changed` | `SelectionSummary` | whenever the tray changes |
| `build:started` | `BuildStarted` | once, when a build begins |
| `build:progress` | `BuildProgress` | when the phase or the counters move |
| `build:event` | `BuildEvent` | once per raw debark NDJSON event |
| `build:finished` | `BuildFinished` | exactly once per build |
| `export:started` | `ExportStatus` | once, when a copy begins |
| `export:progress` | `ExportProgress` | as bytes move |
| `export:finished` | `ExportFinished` | exactly once per copy |
| `verify:started` | `VerifyStatus` | once, when a verification begins |
| `verify:finished` | `VerifyFinished` | exactly once per verification |
| `app:error` | `UIError` | a background failure belonging to no call |

That is the complete list. `internal/app/events.go` holds it as constants and
`EventNames()` returns it, so a test can prove this table and the code agree.

### The finish payloads

```ts
CatalogFinished = { ok, cancelled: boolean, status: CatalogStatus,
                    duration_ms: number, error?: UIError }
BuildStarted    = { command: string[], target_id: string, item_count: number,
                    started_at: string, output_dir: string }
                  // command is redacted for display, like CommandPreview.argv
BuildFinished   = { ok, cancelled: boolean, summary?: BuildSummary,
                    duration_ms: number, error?: UIError }
ExportFinished  = { ok, cancelled: boolean, status: ExportStatus,
                    duration_ms: number, error?: UIError }
VerifyFinished  = { ok, cancelled: boolean, status: VerifyStatus,
                    duration_ms: number, error?: UIError }
ReadinessProgress = { check_id?, title?: string, action: boolean,
                      message?: string, done, total: number }
ReadinessFinished = { ok, cancelled: boolean, check_id?: string,
                      action: boolean, report: ReadinessReport, error?: UIError }
```

### Cancellation — what you are promised

1. **Every `*:finished` event always arrives**, exactly once per run, whether
   the run succeeded, failed, or was cancelled. There is no path where a
   started operation never finishes.
2. **After you call `Cancel*`, a few `*:progress` and `build:event` messages
   already in flight may still arrive** before the finish. Do not treat a
   progress message after a cancel as a bug; just keep rendering until the
   finish lands.
3. **No event from a cancelled run ever arrives after the next run's
   `*:started`.** Each run carries a generation internally and a stale
   goroutine drops its results rather than writing over newer state.
4. **`Cancel*` is always safe**, including when nothing is running. It returns
   `{ ok: true }` and does nothing.
5. A cancelled run's finish carries `cancelled: true` and usually **no**
   `error`. Cancelling is something the operator did, not a failure — do not
   show a red banner for it.

### `app:error`

Deliberately rare. An error caused by a method call is returned *by that
method*; this is for a background failure that belongs to no call. Render it as
a dismissible banner in the shell.

---

## Typical flows

### First run

```js
let r = await App.Readiness();            // fast — render immediately
if (!r.checked_at) await App.StartReadinessCheck();

EventsOn("readiness:finished", f => {
  render(f.report);
  if (f.report.can_build) enableContinue();
});
```

A row the operator fixes:

```js
// runnable action
await App.RunReadinessAction(row.id);     // emits progress, then finished

// elevated action — do not run it
await App.CopyToClipboard(row.action.display);
// ...operator runs it elsewhere, comes back:
await App.RecheckReadiness(row.id);
```

### Choosing a target

```js
const arches = await App.SupportedArchitectures();
const bases  = await App.ListBases(arches.default);
showCaveat(bases.caveat);

const t = await App.SelectTarget({ kind: "base", base_id: id, arch: bases.arch });
if (t.error) return showError(t.error);

if (t.target.catalog_ready) goToPicker();
else                        offerCatalogBuild();
```

Or from a file:

```js
const pick = await App.ChooseSnapshotFile();
if (pick.cancelled) return;
const info = await App.InspectSnapshot(pick.path);
if (info.error) return showError(info.error);
confirm(info.snapshot);   // show origin_kind: captured vs synthesized
await App.SelectTarget({ kind: "snapshot", snapshot_path: pick.path });
```

### Building the catalogue

```js
let st = await App.CatalogStatus();       // recover on mount
if (!st.ready) {
  EventsOn("catalog:progress", p => bar(p.overall_fraction, p.label, p.item));
  EventsOn("catalog:finished", f => {
    if (f.cancelled) return showCancelled();
    if (f.error)     return showError(f.error);
    goToPicker();
  });
  const r = await App.StartCatalogBuild(false);
  if (r.error) showError(r.error);
}
```

### The picker

```js
// mount
const cats = await App.Categories();
let page = await App.SearchPackages({ text: "", offset: 0, limit: 50 });
virtualiser.setTotal(page.total);

// a keystroke, debounced 60 ms — see picker-search.js, and read the note below
let typed = input.value;
const res = await App.SearchPackages({ text: typed, category: cat, offset: 0, limit: 50 });
if (res.query.text !== input.value) return;     // stale — drop it
virtualiser.setTotal(res.total);
virtualiser.put(res.offset, res.rows);

// scrolled to a new window
const win = await App.SearchPackages({ text: typed, offset: firstVisible, limit: 60 });
virtualiser.put(win.offset, win.rows);

// selecting
const sel = await App.AddPackages([row.name]);
sel.warnings?.forEach(showWarning);   // snap-transitional lands here
```

**The debounce is 60 ms.** This document used to sketch "~120 ms", which cannot
hold: the budget is keystroke to rendered results in < 100 ms p95 *including*
the bridge round trip, so the debounce is spent out of that 100 ms rather than
added to it, and 120 ms alone is 120% of it before any work starts. The
implementation settled on 60 against a pessimistically summed fixed cost of
about 30–35 ms at p95 — the bridge for 50 rows, plus the virtualised render;
the backend search itself is under a millisecond on a real `universe` index
(`docs/performance.md`). 60 ms is the largest round number that fits with
margin, and it still coalesces everything that actually floods the bridge: key
auto-repeat, IME composition bursts, and the input event after a paste.

The number lives in one exported constant, `SEARCH_DEBOUNCE_MS` in
`picker-search.js`, whose header carries the full arithmetic. The code was right
and this document was stale.

### The tray

```js
EventsOn("selection:changed", s => renderTrayHeader(s));

const p = await App.SelectionPage(0, 200);
renderTrayRows(p.entries);

await App.RemovePackages([entry.key]);   // key, not name

// paste-a-list
const preview = await App.ParsePackageList(textarea.value);
showPreview(preview);                    // count, unknown, sample, rejected
if (confirmed) await App.AddPackageList(textarea.value);
```

### Building a bundle

```js
const dir = await App.ChooseDirectory("Where should the bundle go?", "");
if (dir.cancelled) return;

const opts = { output_dir: dir.path, no_sign: false };
const cmd = await App.PreviewCommand(opts);
if (cmd.error) {
  // PreviewCommand validates, so a refused spec has no command at all.
  // Render the error, and do not tell the operator anything is ready.
  showError(cmd.error);
} else {
  showCommand(cmd.display);              // rule 8 — always show it
}

// progress_events is false on an older debark: nothing below will fire.
const { progress_events } = await App.AppInfo();
if (!progress_events) showIndeterminateSpinner("This debark cannot report progress.");

EventsOn("build:progress", p => bar(p.fraction, p.phase, p.package));
EventsOn("build:event", e => { if (drawerOpen) appendLogRow(e); });
EventsOn("build:finished", f => {
  if (f.cancelled) return showCancelled();
  if (f.summary?.exit_class === "incomplete") return showIncomplete(f.summary);
  if (f.error)     return showError(f.error);
  showDone(f.summary);                   // say so if summary.signed is false
});

const r = await App.StartBuild(opts);
if (r.error) showError(r.error);
```

On reopening the drawer, or after a reload:

```js
let seq = 0, page;
do { page = await App.BuildLog(seq, 500); appendLogRows(page.events); seq = page.next_seq; }
while (page.events.length === 500);
if (page.dropped) showDroppedNotice(page.dropped);
```

### Exporting

```js
const vols = await App.ListVolumes();
// poll: if (v.fingerprint !== last) rerender

const plan = await App.PlanExport({ destination: vol.path });
if (plan.error) return showError(plan.error);   // includes "it will not fit"
showPlan(plan.plan);

EventsOn("export:progress", p => bar(p.fraction, p.phase, p.file, p.bytes_per_sec));
EventsOn("export:finished", f => {
  if (f.cancelled) return warnDriveIsIncomplete();
  if (f.error)     return showError(f.error);
  showDone(f.status.summary, f.status.verified);
});

await App.StartExport({ destination: vol.path });
```

---

## Deliberate omissions, and the gaps

Things a screen might reach for that are **not** here, on purpose:

- **No sort or filter beyond category + apps-only.** Ordering is contractual so
  every screen ranks identically; a second ordering is a second index.
- **No dependency fields on a package.** See screen 3.
- **No "will this fit / what will it pull in" estimate.** Only the build knows,
  because only apt knows.
- **No raw device access, partitioning or bootable-media writing.** Permanently
  out of scope.
- **No hosted anything.** No method on this surface contacts a
  debark-operated endpoint, because there is none.
- **No telemetry hooks.** Architecturally absent.

Gaps that are real, and who owns them:

1. **The stock-base half of `catalog.Target.Sources` is closed, but not by the
   CLI.** The catalogue fetches indexes directly and so needs each target's
   deb822 sources parsed into `catalog.Source`, and `catalog.ResolveTarget`
   supplies them today — in process, from `base.Resolve` for a stock base and
   `snapshot.Open` for a snapshot file, never through `snapshot list-bases
   --json`. That route stays closed on purpose: `base.ListEntry` deliberately
   omits `Sources`, so the CLI's own listing carries no archive URIs, suites or
   components at all. A GUI built against a debark it only ever shells out to
   would still have nothing to parse; this one links the same packages the CLI
   does. Worth knowing before anyone tries to route it through the adapter
   again.
2. **`readiness:progress` is coarse for a full pass.** The checks run
   concurrently and finish together, so a full pass emits started and finished
   and nothing between. Show an indeterminate state, not a bar.

---

## For the Go packages

If you are implementing a subsystem behind this surface rather than calling it:

- **Do not add exported methods to `App` from your own file.** The surface is
  frozen in `bindings.go`; two packages adding the same name is a compile error
  and a surface spread over five files cannot be frozen at all. Add your work
  behind the seam you own — `Deps.Resolver`, `Deps.Policy`,
  `Deps.Readiness`, `Deps.Runner` — or as unexported helpers.
- **Emit only through the typed helpers in `events.go`.** Never call
  `runtime.EventsEmit` with a literal name.
- **Return a `*UIError` with a `Message` and a `Hint`.** A `UIError` with an
  empty message is a bug; so is one that only says what failed.
- **Everything must work headless.** `App` runs with no Wails runtime attached
  — that is how the tests drive it — so events are dropped rather than emitted
  and the dialog methods return `app.no_runtime`. Never call a Wails runtime
  function without checking that the runtime is attached: `EventsEmit` calls
  `log.Fatalf` on a foreign context and takes the process with it.
