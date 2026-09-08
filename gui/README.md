# Debark desktop

**Browse Debian and Ubuntu packages and prepare bundles for offline machines.**

Debark is the desktop companion to the [debark CLI](../README.md). It runs on
the **online builder**, where you choose a target, select packages, and build
a bundle to transfer. The offline target uses the CLI to verify and install.

[User guide](docs/user-guide.md) ·
[Downloads](https://github.com/inferops/debark/releases) ·
[Get help](../SUPPORT.md) · [Contributing](../CONTRIBUTING.md)

## Workflow

1. **Target:** choose a captured snapshot or a baseline OS. A captured snapshot
   describes the real machine; a baseline assumes an installed package set.
2. **Packages:** search the target's package catalogue and select packages.
   Add vendor URLs or local `.deb` files when needed.
3. **Bundle:** review the request, choose signing and output options, build,
   and copy the result to a folder or mounted drive.

The app delegates builds and verification to the CLI. apt resolves dependencies;
the catalogue is for browsing. The CLI remains available for scripts and for
options beyond the graphical workflow.

See the [user guide](docs/user-guide.md) for system checks, keyboard workflows,
signing, and export.

## Platforms

| Platform | Desktop status |
| --- | --- |
| Ubuntu 24.04/26.04 amd64 | Supported Linux package |
| Debian 13 amd64 | Supported Linux package |
| Ubuntu 22.04 / Debian 12 | Published desktop package cannot satisfy GTK/GLib dependencies; use the CLI |
| Windows amd64 | Experimental build; needs a Linux container and Linux CLI helper for builds |
| Linux arm64 / macOS | No desktop release |

These are desktop host requirements. A supported builder can prepare a bundle
for a different CLI target release using the appropriate backend.
See [packaging validation](docs/packaging.md), the [Windows record](docs/windows.md),
and [CLI platform requirements](../docs/platforms.md).

## Install

Download the matching `debark-gui` package and the `debark` CLI from the
same [release](https://github.com/inferops/debark/releases). The CLI is required
separately and must be discoverable on `PATH`.

Desktop assets use `debark-gui_checksums.txt` and its signature.
CLI assets use `debark_checksums.txt`. Follow the release verification
instructions for each product.

On a supported Linux host, install the downloaded desktop `.deb` with apt.
For example, after replacing the filename with the asset you downloaded:

```sh
sudo apt install ./debark-gui_VERSION_amd64.deb
```

apt installs GTK and WebKitGTK runtime dependencies. Launch **Debark** from
the desktop menu or run `debark-gui`. If a prerequisite is missing, open
**System check** and follow the reported remedy.

## Building from source

Both modules live in **one repository**:

```text
debark/
├── go.mod          CLI and engine
└── gui/
    └── go.mod      desktop app; engine resolved with replace => ../
```

For Linux development, install Go 1.26+, GNU Make, Bash, a C compiler,
`pkg-config`, GTK 3 headers, and WebKitGTK 4.1 headers:

```sh
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
git clone https://github.com/inferops/debark.git
cd debark
go build -o debark ./cmd/debark
```

Put the newly built CLI on `PATH` before running the desktop app. Then build
the GUI with the pinned Wails CLI:

```sh
cd gui
go install github.com/wailsapp/wails/v2/cmd/wails@$(make -s print-wails-version)
make build
```

Make sure Go's executable directory is also on `PATH`. The build output is
under `gui/build/bin/`. `make build` runs the frontend copy step through
`wails.json` and uses the `webkit2_41` build tag on Linux.

For live reload and checks:

```sh
make dev
make frontend
make check
```

`make check` runs formatting checks, vet, lint, and Go tests. Install the
linter version pinned in [Makefile](Makefile) before running it. Root-module
Go commands do not automatically test this module.

The frontend uses HTML, CSS, vanilla JavaScript ES modules, and generated
Wails bindings. There are no npm dependencies or bundler. Dependencies and
their trust impact are recorded in [the dependency review](docs/dependency-review.md).

## Development

Read [CONTRIBUTING.md](../CONTRIBUTING.md) and use these references:

| Guide | Purpose |
| --- | --- |
| [Engineering contract](docs/dev/contract-brief.md) | Interfaces, boundaries, and performance budgets |
| [CLI surface](docs/dev/cli-surface.md) | Commands, JSON types, and exit codes consumed by the app |
| [Binding surface](docs/dev/binding-surface.md) | Frontend/backend methods and events |
| [Catalogue sourcing](docs/dev/catalogue-sourcing.md) | Package metadata sources and browsing behavior |
| [Index formats](docs/dev/index-formats.md) | apt and AppStream inputs |
| [Cache format](docs/dev/cache-format.md) | Catalogue cache and invalidation |
| [Screen contract](docs/dev/screen-contract.md) | Screen module interfaces |
| [UX contract](docs/dev/ux-contract.md) | Current navigation and interaction rules |
| [UI review](docs/dev/ui-review.md) | Visual and native review procedures |
| [Design system](frontend/src/design/README.md) | Tokens, components, and themes |

Validation records cover [accessibility](docs/accessibility.md),
[performance](docs/performance.md), [security](docs/security-review.md),
[packaging](docs/packaging.md), and [releases](docs/release.md).
Their dated measurements are scoped to the runs they describe.

## Scope and license

The desktop app prepares bundles; it does not write bootable media, raw block
devices, or OS images. It has no hosted catalogue service, telemetry, crash
uploads, or update checks. Package metadata comes from the selected repositories.

Apache-2.0, matching the CLI. See [LICENSE](LICENSE), [NOTICE](NOTICE), and the
project's [free/paid policy](../docs/free-paid-policy.md).
