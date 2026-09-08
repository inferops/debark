# Contract brief — read this before writing any code

**Desktop UX revision, 2026-09-07:** [ux-contract.md](ux-contract.md)
supersedes the presentation choices and the historical ownership table below.
All engine, security and product invariants below remain binding.

The GUI is a **separate Go module** from the engine it drives, deliberately:
it links GTK and WebKitGTK through CGO, and that dependency surface must never
reach the engine's own module, which stays pure Go. The two share this
repository so they version and release together.

The interfaces below define compatibility boundaries within the desktop module.
Use the current [UX contract](ux-contract.md) for interaction behavior and
[CONTRIBUTING.md](../../../CONTRIBUTING.md) for the contribution workflow.

## The one-paragraph product

A desktop app that runs on the **online builder machine**. The operator picks a
target — a stock base OS, or a snapshot file someone handed them — picks
packages from a catalogue derived from that target's own apt indexes, adds
vendor `.deb` URLs or local files, and builds. The output is a bundle folder,
optionally copied to a mounted USB drive. **Everything on the offline target
stays as it is today: the `debark` CLI, or plain apt.**

## Non-negotiable rules

1. **No engine logic in the GUI module.** No dependency resolution, no
   version comparison, no dependency reasoning, no "which package supersedes
   which". The GUI shells out to `debark` and reads `--json`. Two
   implementations that can disagree is two products. If you find yourself
   comparing two version strings to decide anything, stop — you are writing the
   engine.
2. **apt is the oracle, and `debark` is the only thing allowed to ask it.**
   Parsing a `Packages` index to *show the operator a list* is catalogue work
   and is fine (that is the catalogue packages' whole job). Parsing it to
   *decide what gets installed* is engine work and is forbidden.
3. **No network call to any debark-operated endpoint. Ever.** Not behind a
   flag, not for updates, not for a catalogue. The only hosts this app contacts
   are the distro archives named in the target's own sources, and vendor `.deb`
   URLs the operator typed. There is no hosted API, no catalogue service, no
   phone-home.
4. **No telemetry, no analytics, no crash reporting.** Architecturally absent,
   not merely disabled. No such library may appear in the tree.
5. **Zero runtime npm dependencies, and — as built — zero npm dependencies at
   all.** There is no bundler and no JavaScript build step; see *Frontend
   stack* below. Adding an npm package is a decision that needs a written
   justification in `docs/dependency-review.md`, not a `npm install`.
6. **No new Go dependency without reporting it first.** This project
   hand-reviews every dependency with "what its compromise would mean"
   (`../docs/security/dependency-review.md` is the house format). If
   you genuinely need something, stop and report it instead of running
   `go get`.
   **And do not run `go mod tidy` while packages are running.** `go.mod` is
   frozen and owned. Tidy reads the tree as it is *at that instant*, so while
   another package is mid-write it will happily prune
   `require github.com/inferops/debark` because nothing imports it yet — and
   every package that depends on the sibling module breaks at once. This has
   already happened twice. The core repo's own contract brief carries the same
   rule for the same reason.
7. **No caps, no metering, no trials, no entitlement checks.**
   `../docs/free-paid-policy.md` is a published promise. The GUI is
   community functionality.
8. **The CLI stays the complete interface.** Anything the GUI can do must be
   expressible as a command, and the GUI shows that command where it reasonably
   can. `CLIAdapter.Command` exists for exactly this and is part of the frozen
   contract.
9. **Nothing in this repo may be a GUI for the offline target.** This app runs
   on the builder. It never assumes a display exists on the other side.

## Frontend stack — and why there is no build step

Vanilla HTML, CSS and ES modules, plus Wails' generated bindings under
`frontend/wailsjs/`. The stock `wails init` template shipped Vite;
`frontend/package.json` was deleted at the outset.

The app is lists, a search box, a progress view and a file picker. A framework
buys nothing here and costs a dependency tree nobody will review. The "build"
is a plain recursive copy of `frontend/src/` and `frontend/index.html` into
`frontend/dist/`, which `main.go` embeds. The frontend targets only WebKit2GTK
and WebView2, so modern CSS and ES modules may be used directly — no
transpiling, no polyfills.

## Performance budgets — architecture, not optimisation

The catalogue is large: Ubuntu `main` + `universe` is roughly 70,000 binary
packages. A naive implementation is unusable, so these are requirements.

| Budget | Target |
|---|---|
| Cold start to interactive | < 1.5 s |
| Catalogue build, first time for a target | progress shown and cancellable; parse phase < 10 s after download completes |
| Catalogue load from cache | < 500 ms |
| Keystroke → rendered search results | < 100 ms p95, **including** the Wails bridge round-trip |
| Scrolling a 60,000-row result set | no dropped frames |
| Idle resident memory | < 400 MB |

Three rules make these achievable:

1. **Search runs in Go, never in JS.** The frontend never receives the full
   package set. It asks for a page and Go returns ~50 rows. Shipping 70,000
   records across the bridge is the single easiest way to make this app feel
   broken.
2. **The list is virtualised.** Render only visible rows, whatever the result
   count. Row height is a fixed token (`--row-height`); the virtualiser depends
   on it.
3. **The parsed index is cached on disk**, keyed by release, arch, components
   and the index digest. First run pays; later runs do not.

Budgets are **measured and recorded**, never asserted. `internal/catalog`
carries a benchmark; the number goes in `docs/performance.md`.

## Package ownership

Other packages are running **concurrently in this same working tree**. Writing
outside your files will collide with someone else's work. If two packages need
the same file, that is a missing contract — report it, do not coordinate around
it.

| Area | Owns (writes here only) |
|---|---|
| Build and tooling | `main.go`, `wails.json`, `Makefile`, `.golangci.yml`, `.gitignore`, `.github/`, `hack/`, `frontend/index.html` (placeholder), `frontend/dist/.gitkeep` |
| CLI adapter — contract | `internal/cliadapter/iface.go`, `internal/cliadapter/fake/` |
| CLI adapter — implementation | `internal/cliadapter/invoke.go`, `events.go`, `errors.go` + tests |
| Catalog — contract | `internal/catalog/iface.go`, `internal/catalog/fake/`, `docs/dev/cache-format.md` |
| Catalog — implementation | `internal/catalog/packages.go`, `dep11.go`, `cache.go`, `search.go`, `catalog.go`, `sources.go`, `resolve.go` + tests |
| Design tokens | `frontend/src/design/` |
| Bound surface | `internal/app/bindings.go`, `internal/app/types.go`, `docs/dev/binding-surface.md` |
| Target screen | `frontend/src/screens/target.js` |
| Packages screen | `frontend/src/screens/picker-list.js` (virtualiser), `picker-search.js`, `picker-tray.js` |
| Build screen | `frontend/src/screens/build.js` |
| Export | `internal/export/` |
| Export screen | `frontend/src/screens/export.js` |
| Readiness | `internal/readiness/` |
| Readiness screen | `frontend/src/screens/readiness.js` |
| Shell | `frontend/src/shell/shell.js`, `frontend/src/main.js`, `states.js`, `theme.js` |

`go.mod`, `go.sum` and `docs/dev/contract-brief.md` are frozen. Report a
problem with them; do not edit them.

**Ownership ends when an area reports done.** While an area is live its files
are its own: report a defect in them rather than fixing it, because a
concurrent write loses work and neither party can see the other's edit. Once
it has reported, its files pass to integration, and a defect found later is
fixed in place rather than left because nobody owns it any more.

This matters because the interesting defects surface *after* an area finishes,
found by whoever consumes its work — which is exactly when a strict reading of
the ownership table would freeze the tree. Two rules keep it honest: leave a
comment saying what changed and why, and if the original author is still
reachable, tell it, so a later pass does not silently revert the fix.

**`internal/app`'s bound surface is owned in one place.** The whole bound
surface lives in `bindings.go`, and nothing else may add an exported method to
`App`. Two packages adding a bound method is a compile error, and a surface
split across five files cannot be frozen — which is the entire point of
contract 3.
Subsystems plug in through the `Deps` seams (`Resolver`, `Policy`,
`Readiness`, `Runner`, `Emit`) instead. This corrects an earlier version of
the table above, which had four areas writing into `internal/app`.

## Frozen contract 1 — `internal/cliadapter`

The single seam through which **every** `debark` invocation passes. Every
other packages mocks this; nothing outside this package may call `exec.Command`
with `debark` in it.

```go
package cliadapter

// Adapter is the only way this application runs debark.
type Adapter interface {
	// Probe locates the binary and reports its version and capabilities.
	Probe(ctx context.Context) (Probe, error)

	// ListBases returns the base definitions compiled into the binary, for
	// one architecture. Wraps `snapshot list-bases --json`.
	ListBases(ctx context.Context, arch string) (*base.List, error)

	// InspectSnapshot reads a snapshot the operator chose from disk. Wraps
	// `snapshot inspect --json`. Nothing is uploaded anywhere.
	InspectSnapshot(ctx context.Context, path string) (SnapshotInfo, error)

	// Build runs a build to completion, delivering every NDJSON event to
	// sink as it arrives. Cancelling ctx cancels the build. Wraps `build`
	// with --json-events -.
	Build(ctx context.Context, spec BuildSpec, sink EventSink) (*buildjob.BuildResult, error)

	// Keygen creates an ed25519 operator signing key. Wraps `keygen`.
	Keygen(ctx context.Context, outPath string) (KeyInfo, error)

	// Verify checks a bundle. Wraps `verify --json`.
	Verify(ctx context.Context, bundlePath string) (VerifyReport, error)

	// Command returns the exact argv a spec would run, without running it,
	// so the UI can show the operator the command it is about to execute.
	// Rule 8 depends on this.
	Command(spec BuildSpec) []string
}

// EventSink receives one decoded debark.events/v1 event per call, in
// emission order, from the goroutine reading the stream. Implementations must
// not block: the build's output pipe is behind this call.
type EventSink func(Event)
```

`BuildSpec` is the GUI's own request shape; it is *translated* into flags, and
deliberately does not reuse `buildjob.BuildRequest`, because the CLI takes
flags, not a job document. Every field maps to exactly one documented flag.
`Build` returns debark's own `*buildjob.BuildResult` — **imported, never
mirrored.** Hand-written copies of those structs drift; typed imports cannot.

Errors returned by every method are `*cliadapter.Error`, carrying the
`dferr.Class` (the frozen 0–7 exit-code table), a human summary, a hint saying
what to do next, and the argv and captured stderr for the details drawer.
**No error path may surface a raw exit code alone** — that is a definition-of-
done item, and this package owns it.

The exact `--json` type for each command, every event `type` value, and the
exit-code table are recorded in `docs/dev/cli-surface.md`, verified against a
real binary. **That document, and the binary, outrank this brief** where they
disagree.

## Frozen contract 2 — `internal/catalog`

```go
package catalog

// Catalog answers "what can I install on this target?" It is a browsing
// index built from the target's own apt indexes. It decides nothing.
type Catalog interface {
	// Build fetches and parses the target's indexes, reporting progress.
	// Cancellable. Populates the on-disk cache.
	Build(ctx context.Context, t Target, progress func(Progress)) error

	// Ready reports whether a usable cache exists for this target, so the UI
	// can skip straight to the picker.
	Ready(t Target) (bool, error)

	// Search returns one page. Runs entirely in Go. Must meet the < 100 ms
	// p95 budget.
	Search(ctx context.Context, q Query) (Page, error)

	// Categories returns the two-tier grouping: DEP-11 freedesktop categories
	// for applications, apt Section: for everything else.
	Categories(ctx context.Context) ([]Category, error)

	// Get returns one entry by exact binary package name.
	Get(ctx context.Context, name string) (Entry, bool, error)

	Close() error
}
```

**Where the package list comes from is already decided.**
`docs/dev/catalogue-sourcing.md` is binding on all four catalogue packages
(`packages.go`, `dep11.go`, `cache.go`, `search.go` / `catalog.go`): the
catalogue fetches the target's `Packages` and DEP-11 indexes directly over
HTTPS, from the archives named in the target's own sources. Read that document
before writing catalogue code — it explains why this is not the second
implementation rule 1 forbids, what the scope limits are, and the one piece of
duplicated logic it does accept.

`Page` carries `Total` as well as the rows: the virtualiser needs the full
match count to size its scrollbar without ever receiving the rows.

`Entry` describes an apt binary package as the *target's own index* reports it
— name, candidate version, arch, `Section:`, installed size, a one-line
summary, and, when DEP-11 knows it, a human application name and freedesktop
categories. A version string in an `Entry` is **display only**; nothing in this
repository may compare two of them.

## Frozen contract 3 — the Wails binding surface

The exact Go methods the frontend may call and the exact event names the
backend may emit, written down before either side starts. Both are recorded in
`docs/dev/binding-surface.md` and implemented in `internal/app/bindings.go`.

Rules:
- A bound method returns a **view type** (`internal/app/types.go`), never a
  `core/` type and never an `internal/catalog` type directly. The bridge is a
  serialisation boundary; keeping a view layer means a backend refactor is not
  a frontend change.
- A bound method never blocks for longer than a frame. Anything slow — catalogue
  build, bundle build, drive copy — starts work and returns immediately;
  progress arrives as events.
- Event names are namespaced `noun:verb` (`catalog:progress`, `build:finished`).
- No bound method may take or return an unbounded slice of catalogue rows.
  Paging is not optional.

## Definition of done (the whole app)

- [ ] Search meets the < 100 ms p95 budget against a real `universe` index, and
      the number is **measured and recorded**, not asserted.
- [ ] A 60,000-row result set scrolls without dropped frames.
- [ ] The GUI contains no dependency resolution, version comparison, or
      dependency reasoning of its own. Verified by review, stated in the README.
- [ ] Zero runtime npm dependencies; every build-time tool justified in
      `docs/dependency-review.md`.
- [ ] No network call to any debark-operated endpoint exists in the tree.
- [ ] End-to-end: a GUI-built bundle verifies and installs on an offline target.
- [ ] Every error path renders an actionable message; none surface a raw exit
      code alone.
- [ ] Ships as a `.deb` that installs cleanly on the supported Ubuntu releases.
- [ ] The core repository is unchanged by this work, except where the CLI
      genuinely lacked a `--json` field the GUI needs — and each such change is
      made in the core repo, with tests, as its own commit.

## Scope discipline

This app exists to remove the blank-`packages.txt` problem, not to become a
platform. If a feature request would put engine logic in the frontend, add a
hosted service, or move functionality onto the offline target, it belongs in
the brief's Non-goals table rather than the backlog. The project-wide permanent
do-not-build list is in [`GOVERNANCE.md`](../../../GOVERNANCE.md), under "The
do-not-build list is product definition, not backlog" — it binds this half of
the repository as much as the engine, and it already forbids a GUI or
full-screen TUI as the *primary* interface. This application carries four
further entries of its own, permanent on the same footing: a GUI on the offline
target, one binary that is GUI-by-default, writing bootable USBs or any raw
block-device access, and any hosted catalogue.
