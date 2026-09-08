# Debark

A desktop app for preparing air-gapped Debian/Ubuntu software transfers.

Pick a target OS, pick some packages, get a signed bundle. It exists to remove
the blank-`packages.txt` problem — the moment where you know what you want the
offline machine to have, and have to work out what to type.

It runs on the **online builder machine**. Nothing about the offline target
changes: over there it is still the `debark` CLI, or still plain apt.

The product is **Debark**; the module, the executable and the `.deb` are all
`debark-gui`, and the engine it drives is `debark`. Both live in this one
repository — the engine at the root, this app under `gui/`. You will see both names
— the window says Debark, the command line it runs says `debark` — because
the engine is a real command-line tool people use directly, on the far side and
in CI, and it keeps its own name.

**Using it: [`docs/user-guide.md`](docs/user-guide.md)** — what it is for, the
four steps, and what to do when a check fails.

## What it does

1. **Pick a target.** A stock base OS (`ubuntu:26.04/desktop`) for a machine
   that does not exist yet, or a snapshot file someone handed you from a
   machine that does. Nothing is uploaded anywhere.
2. **Pick packages.** A searchable catalogue built from that target's own apt
   indexes, grouped by application category where the archive publishes
   AppStream data and by apt `Section:` everywhere else. Add vendor `.deb`
   URLs or local files alongside; they appear as ordinary rows.
3. **Build.** `debark` resolves the exact missing closure with the target
   release's own apt, fetches it, builds a real flat apt repository, and signs
   a manifest — or records, deliberately and visibly, that you chose not to
   sign it. You watch it happen by phase rather than by a fabricated
   percentage, and can read the raw event log if you want to.
4. **Export.** Save the bundle folder, or copy it to a mounted drive — with a
   free-space check first, progress during, and a verification pass after.

## What it is not

This app is a front end. **It contains no dependency resolution, no version
comparison, and no dependency reasoning of its own**, and it never will. Every
decision about what goes into a bundle is made by `debark`, which makes it by
asking the target release's own apt. Two implementations that can disagree is
two products.

The catalogue is the one place that reads apt data directly, and it reads it
only to *show you a list*. If it is ever wrong, the cost is a missing row in a
picker — you can still add that package by name or by URL, and the build still
resolves against real apt with real sources. A catalogue bug cannot produce a
wrong bundle. See [`docs/dev/catalogue-sourcing.md`](docs/dev/catalogue-sourcing.md)
for why that asymmetry is what makes the design safe.

Also permanently out of scope, each for a reason:

| Not built | Why |
|---|---|
| A GUI that runs on the offline target | The target side must work with nothing but the open binary — or nothing but apt. Targets are frequently headless, serial-console, or approval-gated. |
| One binary that is GUI-by-default, CLI-when-flagged | Ambiguous over SSH and in CI, and it drags a UI toolkit into a binary that must stay small, static and distro-packageable. Two binaries, two packages. |
| Writing bootable USBs, raw block devices, `.img`/`.iso` export | The most dangerous component in the product. It forces root, and drags in ISO verification, autoinstall, Secure Boot and a boot-test matrix. Bundle export plus a plain file copy delivers the value. |
| Any hosted API, catalogue service, or phone-home | The catalogue comes from the distro archive. There is no debark-operated endpoint, and there never will be. |
| Telemetry, crash reporting, analytics | Architecturally absent, not merely disabled. No such library appears in the tree. |
| Caps, metering, trials, entitlement checks | [`free-paid-policy.md`](../docs/free-paid-policy.md) is a published promise. This app is community functionality. |

The CLI stays the complete interface: anything this app can do is expressible
as a command, and it shows you that command where it reasonably can.

## Dependencies

**Zero npm dependencies. No JavaScript build step, no bundler, no framework.**
The frontend is vanilla HTML, CSS and ES modules plus Wails' generated
bindings; the "build" is a recursive file copy. This is deliberate: the project
hand-reviews every dependency with a written note on what its compromise would
mean, and that culture cannot absorb a transitive npm tree.

The Go side is Wails v2 plus `debark` itself — imported as a module so that
`--json` output is parsed with **debark's own types**, never hand-written
mirrors that would drift.

Every dependency is reviewed in [`docs/dependency-review.md`](docs/dependency-review.md).

## Platforms

**Linux is the shipping target.** It is the only platform where
`debark build` runs with zero prerequisites — native apt, no container, no
WSL2 — and it is where these users are. Ships as a `debark-gui` `.deb` with
`libwebkit2gtk-4.1-0` and its GTK stack as normal `Depends`, derived from the
binary by `dpkg-shlibdeps` rather than written down, and with no maintainer
scripts at all. It installs on **Ubuntu 24.04 and newer and Debian 13 and
newer**; Ubuntu 22.04 and Debian 12 are excluded by the `t64` transition, not
by WebKit, and fail cleanly at `apt install`. [`docs/packaging.md`](docs/packaging.md)
is the measured record.

**Windows builds, but is not supported.** The UI is nearly free because Wails
uses the system WebView2. The obstacle is not the UI: `build` needs a
container, and on the locked-down laptops this audience carries a container
runtime is frequently blocked by policy. The readiness screen reports what it
can probe rather than promising a build will work; it no longer counts WSL 2
as a build route, because nothing in either repository ever invokes one, so a
Windows machine's only route is a container plus a Linux `debark` for that
container to mount. [`docs/windows.md`](docs/windows.md) is the measured record
of an end-to-end run there — its §5 and §6 predate that correction and still
describe the old derivation. No support commitment until someone asks.

macOS is out of scope.

## Building

Requires Go 1.26+, the [Wails v2 CLI](https://wails.io), and on Linux
`libgtk-3-dev` and `libwebkit2gtk-4.1-dev`.

```sh
make build     # build the app
make dev       # run with live reload
make check     # fmt-check + vet + lint + test — what CI runs
```

Every Go command carries `-tags webkit2_41`, because Debian 13, Ubuntu 24.04
and everything newer ship only the 4.1 ABI. There is no release this project
targets where clearing it is required — Ubuntu 22.04 and Debian 12 ship 4.1
as well as 4.0, measured — so `make build WEBKIT_TAG=` is kept only for a host
that genuinely has nothing but the 4.0 headers installed. See the comment on
`WEBKIT_TAG` in the `Makefile`.

`debark` is currently resolved from the engine module via a `replace`
directive in `go.mod`, because it is not yet published to a module proxy. Clone
both modules side by side:

```
parent/
  debark/
  debark-gui/
```

## Development

Start with [`docs/dev/contract-brief.md`](docs/dev/contract-brief.md) — the
frozen interfaces, the ownership map, the performance budgets, and the rules
that are not negotiable. Then:

- [`docs/dev/cli-surface.md`](docs/dev/cli-surface.md) — the verified `debark`
  command surface, its `--json` types and its exit codes. This document and the
  real binary outrank everything else where they disagree.
- [`docs/dev/binding-surface.md`](docs/dev/binding-surface.md) — the exact Go
  methods the frontend may call and the events the backend may emit.
- [`docs/dev/catalogue-sourcing.md`](docs/dev/catalogue-sourcing.md) — where the
  package list comes from, and why.
- [`docs/dev/index-formats.md`](docs/dev/index-formats.md) — what apt indexes
  and DEP-11 actually contain, measured against the real archive rather than
  read off a spec. Several of its findings are load-bearing.
- [`docs/dev/cache-format.md`](docs/dev/cache-format.md) — the on-disk catalogue
  cache and its invalidation rule.
- [`docs/dev/screen-contract.md`](docs/dev/screen-contract.md) — the shape every
  screen module has, and what the shell gives it.
- [`docs/dev/error-catalogue.md`](docs/dev/error-catalogue.md) — 21 real
  failures, caused on purpose, with the error each produces and what a person
  must see. A specification, not a report.
- [`frontend/src/design/README.md`](frontend/src/design/README.md) — the design
  system: tokens, components, and the theming contract.

`docs/performance.md`, `docs/accessibility.md`, `docs/security-review.md`,
`docs/dependency-review.md`, `docs/packaging.md` and `docs/windows.md` are the
measurement records: what was tested, with what, what it found, and what was
**not** done.

## Licence

Apache-2.0, matching `debark` itself.
