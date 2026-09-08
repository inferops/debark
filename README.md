# debark

**Prepare Debian and Ubuntu packages online. Install them offline.**

debark builds portable bundles of `.deb` packages and their dependencies for
machines without internet access. It resolves packages against a snapshot of
the target machine, or a selected baseline OS, using the target release's apt.
Each bundle includes an apt repository, an exact-version install plan, and an
optional signature that the offline machine verifies before installation.

Use the **CLI** for terminals, scripts, and CI, or the **[Debark desktop
app](gui/README.md)** to browse packages and prepare bundles on the online machine.

[Website](https://debark.dev) · [Get started](#quick-start) ·
[Downloads](https://github.com/inferops/debark/releases) ·
[Documentation](https://debark.dev/docs) · [Contributing](CONTRIBUTING.md) · [Get help](SUPPORT.md)

> **Pre-1.0:** debark is under active development. Minor releases may include
> breaking changes; published data formats have their own compatibility rules.
> Check [platform support](#platform-support), [known limitations](docs/status.md),
> and the [changelog](CHANGELOG.md) before adopting it.

## Why debark?

Running `apt-get install --download-only` on an online machine uses that
machine's installed packages. Dependencies it already has may be missing on
your offline target.

debark gives apt the **target's package state**, so the download plan accounts
for what the target needs. It also provides:

- **Vendor packages:** combine archive packages, local `.deb` files, and
  vendor URLs; apt resolves their dependencies together.
- **Locked versions:** record the selected versions and install from that plan.
- **Bundle verification:** sign with an operator key and check signatures and
  file digests before installation.
- **Incremental builds:** reuse downloaded content and refresh existing bundles.
- **Inspection and automation:** inspect plans, preview target changes, generate
  an optional CycloneDX SBOM, and consume JSON output.

No telemetry, analytics, crash uploads, or update checks. Online builds contact
the package sources, vendor URLs, and container registries needed for the request.

## How it works

```text
Offline target              Online builder              Offline target
--------------              --------------              --------------
snapshot create  ---------> build             --------> verify
                 snapshot   resolve with apt    bundle  install
                            download + sign
```

A captured snapshot includes the target's installed packages, repositories,
pins, and apt settings. If you cannot capture the machine first,
`build --base` uses a baseline OS instead. A baseline **assumes** an installed
package set; review differences on the target before installing.

## Install

Download the CLI for your machine from
[GitHub Releases](https://github.com/inferops/debark/releases):

| Platform | CLI archive | Other packages |
| --- | --- | --- |
| Linux amd64 | `debark_<version>_linux_amd64.tar.gz` | `.deb`, `.rpm` |
| Linux arm64 | `debark_<version>_linux_arm64.tar.gz` | `.deb`, `.rpm` |
| Windows amd64 | `debark_<version>_windows_amd64.zip` | — |

Extract the archive and put `debark` (or `debark.exe`) on your `PATH`.
CLI downloads use `debark_checksums.txt`; desktop downloads use
`debark-gui_checksums.txt`. Use the matching checksum file and its signature,
as described in the release notes.

With **Go 1.26.8 or newer**, you can install the CLI directly:

```sh
go install github.com/inferops/debark/cmd/debark@latest
debark version
```

Go installs executables into `GOBIN`, or `$(go env GOPATH)/bin` when
`GOBIN` is unset. Add that directory to your `PATH` if needed. Use a specific
tag in place of `@latest` when you need a pinned version.

To build from source:

```sh
git clone https://github.com/inferops/debark.git
cd debark
go build -o debark ./cmd/debark
```

On Windows, use `-o debark.exe`. The CLI is pure Go and needs no GUI libraries.

The online builder needs a compatible local apt or Docker/Podman running Linux
containers. Windows and macOS builders also need a Linux debark binary for the
container; see [builder setup](docs/platforms.md#container-builders).
The offline target needs a Linux debark binary, apt/dpkg, and root privileges
for installation. Snapshot capture and verification do not need root.

For the desktop app, follow [desktop installation and build instructions](gui/README.md).
Its platform requirements differ from the CLI's.

## Quick start

This example installs `jq` on an existing Debian or Ubuntu machine. Commands
use a POSIX shell; run each step on the machine named. Package counts and
versions depend on the target and the available repositories.

### 1. Create a signing key on the online builder

```sh
debark keygen --out operator.key
```

This writes `operator.key` and `operator.pub`. Keep the private key on the
builder. Provision the public key on the target through a trusted channel, or
check it against a fingerprint obtained independently of the bundle media.

### 2. Capture the offline target

Run on the target, then transfer the snapshot to the online builder:

```sh
debark snapshot create --out target.snapshot.tar.zst
```

Snapshots can contain sensitive machine and repository configuration.
`--redact` removes machine ID, proxy settings, and labels, but is not a
complete anonymizer; see [snapshot privacy](docs/faq.md#what-information-is-in-a-snapshot).

### 3. Build on the online builder

From the directory containing the snapshot and signing key:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./bundle --sign operator.key jq
```

Copy the entire `bundle` directory to the offline target. Include a trusted
Linux CLI binary separately, or use
[`--embed-binary`](docs/quickstart.md#carry-the-cli-in-the-bundle).

### 4. Verify and install on the offline target

From the directory containing the bundle and provisioned public key:

```sh
debark verify ./bundle --key operator.pub
debark install ./bundle --key operator.pub --status
sudo debark install ./bundle --key operator.pub --yes
```

Review the status output before the last command. `install` verifies the
bundle again before invoking apt and installs the locked versions.

See the [full walkthrough](docs/quickstart.md) for vendor packages, updates,
and transfer options.

### Build with a baseline OS

If you cannot capture the target first, choose its release and architecture:

```sh
debark snapshot list-bases
debark build --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key jq
```

Use the signing key created above. `minimal`, `server`, and `desktop`
describe what the target is assumed to have; they do not install an OS.
Use either `--base` or `--snapshot`. Baseline builds have
[additional validation limits](docs/status.md#baseline-os-builds).

### Use the interactive CLI

```sh
debark build --interactive
```

The prompts cover the target, package list, output, signing key, upgrades, and
SBOM. Create a key first with `debark keygen --out operator.key`.
The CLI saves entered packages to a list file and prints a command to reuse.
Interactive mode requires a terminal.

## Platform support

| Role | Supported path |
| --- | --- |
| Capture and install on a target | Debian 12/13 and Ubuntu 22.04/24.04/26.04; Linux with apt/dpkg |
| Build on Linux | Compatible local apt, otherwise a Linux container |
| Build on Windows | Docker or Podman with Linux containers and a Linux debark helper |
| Verify and inspect | Linux and Windows CLI binaries; portable Go code |
| Desktop builder | Linux amd64 on Ubuntu 24.04/26.04 and Debian 13 |

CLI binaries are released for Linux amd64/arm64 and Windows amd64. The nightly
integration workflow covers amd64; arm64 integration requires a manual run
with emulation enabled. macOS is a source-build path without CI or recorded
container validation. Derivative distributions are best effort.

See [platforms and prerequisites](docs/platforms.md) for baseline IDs,
container setup, and the distinction between available builds and tested paths.

## Status and known gaps

The captured-snapshot workflow has an end-to-end demo that installs packages
with networking disabled. Baseline builds, cross-platform container builds,
and the desktop app have different levels of validation. These are recorded
in [status and known limitations](docs/status.md), with links to the underlying
tests and reports.

A valid signature establishes who signed a bundle and whether its contents
changed. Packages can still contain vulnerabilities or maintainer scripts
that require the network. See the [security model](docs/security-model.md).

## Documentation

Browse the [online documentation](https://debark.dev/docs) for guides and
reference pages. The [repository documentation](docs/README.md) follows the
source in this checkout and can be read offline; use a release tag for the
docs that shipped with that version.

| I want to… | Online guide | Repository reference |
| --- | --- | --- |
| Build and install my first bundle | [Quick start](https://debark.dev/docs/get-started/quickstart) | [Quick start](docs/quickstart.md) |
| Find flags, package inputs, JSON output, or exit codes | [CLI reference](https://debark.dev/docs/reference/cli) | [CLI guide](docs/cli.md) |
| Set up Windows, containers, or a different target release | [Supported systems](https://debark.dev/docs/get-started/supported-systems) | [Platforms](docs/platforms.md) |
| Browse packages in the desktop app | [Desktop guide](https://debark.dev/docs/get-started/desktop) | [Desktop user guide](gui/docs/user-guide.md) |
| Diagnose a failure | [Troubleshooting](https://debark.dev/docs/operate/troubleshooting) | [Troubleshooting](docs/troubleshooting.md) |
| Understand snapshots, baselines, and trust | [How verification works](https://debark.dev/docs/trust/trust-model) | [FAQ](docs/faq.md) · [Security model](docs/security-model.md) |
| Read or integrate the file formats | [Bundle format](https://debark.dev/docs/reference/bundle-format) | [Formats](docs/formats.md) · [Schemas](api/schema/) |
| Find design decisions and validation reports | — | [Documentation index](docs/README.md) |

## Contributing and support

Contributions to code, documentation, tests, accessibility, and packaging are
welcome. Create a branch, make signed-off commits, and open a pull request
targeting `main`. Required checks must pass before a maintainer merges it;
maintainers use the same PR workflow. [CONTRIBUTING.md](CONTRIBUTING.md) walks
through fork setup, local checks, opening a PR, and updating it during review.
Commits require a [DCO sign-off](DCO); there is no CLA.

Use [GitHub Issues](https://github.com/inferops/debark/issues) for bugs,
questions, and feature requests. [SUPPORT.md](SUPPORT.md) explains what to
include. Report vulnerabilities privately through [SECURITY.md](SECURITY.md).

Participation follows the [Code of Conduct](CODE_OF_CONDUCT.md).
[GOVERNANCE.md](GOVERNANCE.md) describes project decisions and scope.

## License

Apache-2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and the
[trademark policy](TRADEMARK.md).

The CLI, desktop app, and capabilities needed to prepare, inspect, transfer,
verify, and install a bundle are community functionality. The
[free/paid policy](docs/free-paid-policy.md) records that commitment.
