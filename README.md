# debark

**Download Debian and Ubuntu packages online, then install them offline.**

Choose a baseline OS with `build --base`, or capture the target's actual package
state with a snapshot. debark downloads the requested packages and dependencies
into a bundle you can copy over and install. You can sign the bundle with your
own key so the target can verify its source and contents.

For guided questions about the target, packages, and output, start with
`debark build --interactive`.

> **Pre-1.0.** The release pipeline is built and wired, but no version has
> been tagged yet, so today you install with `go install` or one `go build`
> from a checkout (see [Install](#install)). The pipeline below is real and
> runs end to end today. [Status and known gaps](#status-and-known-gaps)
> lists what isn't finished.

---

## Contents

- [What is in this repository](#what-is-in-this-repository)
- [Why not just `apt-get --download-only`?](#why-not-just-apt-get---download-only)
- [How it works](#how-it-works)
- [Install](#install)
- [Quick start](#quick-start)
- [Build with a baseline OS](#build-with-a-baseline-os)
- [Use the interactive CLI](#use-the-interactive-cli)
- [Usage](#usage)
- [What's in a bundle](#whats-in-a-bundle)
- [Command reference](#command-reference)
- [Exit codes](#exit-codes)
- [Platform support](#platform-support)
- [Troubleshooting](#troubleshooting)
- [Status and known gaps](#status-and-known-gaps)
- [Design boundaries](#design-boundaries)
- [Documentation](#documentation)
- [License](#license)

---

## What is in this repository

Two Go modules that ship together:

| Path | Module | What it is |
|---|---|---|
| `.` | `github.com/inferops/debark` | The `debark` CLI and the engine behind it. Pure Go, `CGO_ENABLED=0`, no GUI dependencies. |
| [`gui/`](gui/) | `github.com/inferops/debark/gui` | **Debark**, the desktop app that drives the CLI on the online builder machine. Wails + WebKitGTK, so it links C libraries. |

They are separate modules on purpose: the GUI's GTK/WebKit dependency surface
must never reach the engine's, which stays auditable and pure Go. They share a
repository so they version and release together — `gui/go.mod` resolves the
engine with `replace github.com/inferops/debark => ../`, so a checkout always
builds the GUI against exactly the engine beside it.

If you only want the command-line tool, everything below is about `.`; you can
ignore `gui/` entirely.

## Why not just `apt-get --download-only`?

Because it downloads what's missing **on the machine you run it on**, not on
the machine you're building for.

Every dependency your internet-connected box already has installed gets
silently skipped. The bundle looks complete. You carry it across the gap, and
the offline machine fails with unmet dependencies — usually for a library you
never thought about, and usually at the worst possible moment.

debark inverts it. The *target's* installed state comes to the builder, and
resolution runs against that state using the target release's own `apt` in a
private apt root. What you get is the exact missing closure: nothing forgotten,
nothing redundant.

It also handles things the usual workarounds don't:

| | |
|---|---|
| **Vendor `.deb` files** | Hand it a third-party `.deb` and its `Depends` are resolved from the archive too, not just checked. |
| **Exact versions** | The bundle carries a lock. The target installs the versions that were planned, not "whatever's newest". |
| **Tamper evidence** | The manifest is signed and verified before apt is allowed near the repository. |
| **Incremental rebuilds** | A content-addressed store means next month's bundle only fetches what actually changed. |

The other open tool in this space, `apt-offline`, resolves *on the target*
instead. That works, but a machine that has never been networked has no
package indexes to resolve against, and it has no answer for a vendor `.deb`
with dependencies of its own.

## How it works

```text
   offline target                    online builder                   offline target
 ┌────────────────┐   snapshot    ┌─────────────────────┐   bundle    ┌───────────────┐
 │ debark         │ ────────────► │ debark build        │ ──────────► │ debark        │
 │ snapshot create│  (small file) │ apt in private root │ (USB/media) │ verify        │
 │                │               │ fetch → repo → lock │             │ install       │
 └────────────────┘               │ → signed manifest   │             └───────────────┘
                                  └─────────────────────┘
```

The diagram shows a build from a captured snapshot. You can also start on the
online computer with `build --base`, choosing a baseline OS instead of capturing
the target first. A baseline assumes a stock installed package set. Use a
snapshot for the actual machine's packages, repositories, and settings.

Three rules the implementation holds to:

1. **apt is the oracle.** debark never re-derives or second-guesses a
   dependency decision. There is no hand-written solver, and there never will
   be — see [Design boundaries](#design-boundaries).
2. **Verify before apt.** The signature and every file digest are checked
   before the repository is exposed to apt, so a tampered bundle is refused
   rather than parsed.
3. **Nothing leaves the machine.** No telemetry, no analytics, no update
   check. A snapshot is a detailed inventory of a real host, and it is treated
   that way.

## Install

**GitHub Releases.** Each `vX.Y.Z` tag publishes one release at
<https://github.com/inferops/debark/releases> carrying prebuilt CLI archives
for `linux_amd64`, `linux_arm64` and `windows_amd64`, plus `.deb` and `.rpm`
packages — and, on the same release page, the desktop application from
`gui/`. Every CLI artefact's SHA-256 is listed in `debark_checksums.txt`
(the desktop app has its own `debark-gui_checksums.txt`; check each artefact
against the file for its own product). `debark_checksums.txt` is signed with
cosign in keyless mode, so `debark_checksums.txt.sig` and
`debark_checksums.txt.pem` sit beside it.

**Nothing has been tagged yet**, so that page is empty today. The pipeline
exists, runs, and is gated on a reproducible-build check
(`hack/reproducible-check.sh`), but the first release has not been cut. Until
it is, the two channels below are the ones that work.

There is **no macOS build**: no `darwin` archive is published and macOS is
not in the CI matrix. Building from source is the macOS path — see
[Platform support](#platform-support).

**`go install`** — you need Go 1.26 or newer:

```sh
go install github.com/inferops/debark/cmd/debark@latest
```

**From a checkout** — same Go version:

```sh
git clone https://github.com/inferops/debark
cd debark
go build ./cmd/debark
```

That produces a `debark` binary. Put it on your `PATH`.

You need a binary on **both** sides of the gap. For the offline target,
either build for it directly:

```sh
GOOS=linux GOARCH=amd64 go build -o debark-linux-amd64 ./cmd/debark
```

…or let the builder put one inside the bundle for you:

```sh
debark build --embed-binary ./debark-linux-amd64 ...
# lands at bin/debark-linux-amd64 inside the bundle
```

`snapshot create`, `verify` and `install` need no network. `build` needs one —
as does `snapshot from-base`, which resolves a stock release against that
distribution's own archive and nothing else.

## Quick start

The whole loop, in four commands. The transcripts in those four steps are real
output from [`hack/demo-airgap.sh`](hack/demo-airgap.sh), which snapshots a
Debian 12 container, builds a signed bundle, then verifies and installs it in
a second container started with `--network none`. The base subsection that
follows them quotes a different run, and says so.

**1. Make a signing key** (once, on the builder — keep the private key safe):

```console
$ debark keygen --out operator.key --comment "release engineering, 2026"
generated ed25519 key dce3567ab0d4ab5a
  private: /home/you/keys/operator.key
  public:  /home/you/keys/operator.pub
```

The public key is what the target needs, and it must get there **independently
of the media** — pre-provisioned, in config, or over a channel the media
doesn't control. A key that travels alongside the bundle it signs proves
nothing.

**2. On the offline target** — no root, no network, writes nothing but the
output file:

```console
$ debark snapshot create --out target.snapshot.tar.zst
snapshot created: target.snapshot.tar.zst
  target:    debian 12 (bookworm) amd64
  installed: 88 packages
  digest:    sha256:fb02144688b0193f07ef946efc45942f940d78500a4aa21940dcfedf2dd56d3f
```

Copy `target.snapshot.tar.zst` to the online machine.

**3. On the online builder** — ask for what you want:

```console
$ debark build --snapshot target.snapshot.tar.zst --out ./bundle --sign operator.key jq
OK  ./bundle
  target:     debian 12 (bookworm) amd64
  packages:   3 packages in the bundle (3 added, 0 unchanged, 0 removed this run)
  size:       387.5 kB total, 387.5 kB fetched this run
  trust:      signed
```

You asked for `jq`; you got three packages, because `libjq1` and `libonig5`
were missing on the target and nothing else was.

Copy `./bundle` to your media and carry it across.

**4. Back on the target**, network off:

```console
$ debark verify ./bundle --key operator.pub
OK  ./bundle
  target:  debian 12 (bookworm) amd64
  signed:  valid by 47110757540aeb75 (ed25519-file) [ed25519]
  checked: 12 files (417.2 kB)

$ debark install ./bundle --key operator.pub --yes
OK  installed ./bundle
  verify:    OK
  target:    debian 12 (bookworm) amd64
  to install: 3
  to upgrade: 0
  unchanged:  0
```

Installing packages needs root, so on a real machine that last command is
`sudo debark install …` — the transcript above is from the demo, which runs
as root inside a container.

`install` verifies again itself and refuses on failure, so the standalone
`verify` step is there for when you want to check the media *before*
committing to it.

<a id="if-there-is-no-machine-to-snapshot-yet"></a>

### Build with a baseline OS

Use a baseline OS to build without a captured snapshot. Choose the target's
release, installation variant, and architecture on the online computer:

```sh
debark snapshot list-bases
debark build --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key jq
```

This example assumes you created `operator.key` in the quick start. Reuse an
existing signing key, or create one with `debark keygen --out operator.key`.

Built-in baselines cover Debian 12 and 13, and Ubuntu 22.04, 24.04, and 26.04.
Each offers `minimal`, `server`, and `desktop` variants. The variant describes
what is assumed to be installed already; it does not install an OS. Omitting
the variant selects `minimal`. Set `--arch` for the target; otherwise it
defaults to the builder's architecture.

See the [supported systems and baseline IDs](https://debark.dev/docs/get-started/supported-systems#baseline-os-list)
for the complete list of release/variant IDs and values for `--arch`.

Use either `--base` or `--snapshot`, not both. `build --base` needs internet
access and a compatible local apt or container backend, just like other builds.

To save a baseline as a reusable file, optionally run:

```sh
debark snapshot from-base ubuntu:24.04/minimal \
  --arch amd64 --out base.tar.zst
```

Later builds can use `--snapshot base.tar.zst`. The file still describes the
baseline's assumptions, rather than a captured machine. You do not need this
separate step when using `build --base`.

After transferring the bundle and providing its public key separately, preview
it on the target:

```sh
debark install ./bundle --key operator.pub --status
```

The report warns if packages assumed by the baseline are missing on the real
machine. Review those differences before installing. Use a captured snapshot
and rebuild if the baseline does not fit. Baseline builds have less end-to-end
testing than captured-snapshot builds; see [Status and known gaps](#status-and-known-gaps).

### Use the interactive CLI

On the online computer, run:

```sh
debark build --interactive
```

The CLI asks whether you have a snapshot. If not, choose a baseline's
distribution, release, variant, and architecture. Enter packages one per line,
then submit a blank line. You can enter names, `name=version` requests, URLs,
and local `.deb` paths.

Next, choose the output folder, an existing signing key, upgrades, and an
optional SBOM. Review the choices before confirming the build. Create a signing
key beforehand with `debark keygen` if you need one.

You can supply known choices directly, then answer the remaining questions:

```sh
debark build --interactive --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key
```

Entered packages are saved in `packages.txt` in the working directory, or an
unused numbered name if it already exists. The list is saved before final
confirmation. After the build attempt, the CLI prints a command you can reuse.
Keep any extra backend, policy, or profile options with that saved command.

To choose and save only a baseline, use:

```sh
debark snapshot from-base --interactive
```

Both guided commands require a terminal on standard input and cannot be
combined with `--json` or `--json-events`. Interactive mode starts only when
requested. For CI, use explicit options and the saved package list instead.

## Usage

### Asking for packages

Inputs can be apt package names, pinned versions, `https://` URLs to `.deb`
files, local paths, or any mix:

```sh
debark build --snapshot t.tar.zst --sign operator.key \
  jq tree vlc=3.0.21-1build1 ./vendor/acme-agent_2.1_amd64.deb
```

For anything more than a handful, use a list file:

```text
# packages.txt — blank lines and # comments are ignored
jq
tree
vlc=3.0.21-1build1

# a vendor .deb sitting next to this file
./local-debs/acme-agent_2.1_amd64.deb

# a URL, with the digest you expect it to have
https://downloads.example.com/acme/acme-tools_4.2_amd64.deb sha256=9f2b...c41e
```

```sh
debark build --snapshot t.tar.zst --list packages.txt --sign operator.key
```

Or point at a directory of vendor `.deb` files (scanned, not recursively):

```sh
debark build --snapshot t.tar.zst --local-dir ./local-debs --sign operator.key
```

### Vendor `.deb` files with their own dependencies

This is the case most workarounds can't handle. Hand debark a third-party
package and its `Depends` are resolved out of the archive like any other:

```console
$ dpkg-deb -f jq_1.6-2.1+deb12u2_amd64.deb Depends
libjq1 (= 1.6-2.1+deb12u2), libc6 (>= 2.34)

$ debark build --snapshot target.snapshot.tar.zst --out bundle \
    --no-sign ./jq_1.6-2.1+deb12u2_amd64.deb
OK  bundle
  packages:   3 packages in the bundle (3 added, 0 unchanged, 0 removed this run)
  size:       387.5 kB total, 323.5 kB fetched this run

$ find bundle/repo/pool -name '*.deb'
bundle/repo/pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb
bundle/repo/pool/libj/libjq1/libjq1_1.6-2.1+deb12u2_amd64.deb
bundle/repo/pool/libo/libonig5/libonig5_6.9.8-1_amd64.deb
```

`libjq1` and `libonig5` were never mentioned. The lock records why:
`"reason": "dependency-of:jq"`.

### Upgrading what's already installed

By default a bundle carries only what's *missing*. To also refresh packages
the target already has:

```sh
debark build --snapshot t.tar.zst --upgrades --sign operator.key jq
```

Then on the target, opt in at install time — the upgrade set is applied only
if you ask for it:

```sh
sudo debark install ./bundle --key operator.pub --upgrade --yes
```

### Refreshing last month's bundle

Builds are incremental. Re-run into the same directory and only what changed
is fetched; superseded files are pruned:

```sh
debark build --snapshot t.tar.zst --out ./bundle --update --sign operator.key --list packages.txt
```

Use `--no-prune` to keep superseded versions. Files left in the pool that this
run's repository no longer indexes are reported in
`last-run-unreferenced.txt` — they're on the media and covered by the
signature, but the target's apt cannot select them.

### Signing and trust

Three signing options:

```sh
--sign operator.key      # ed25519 key from `debark keygen`
--sign gpg:<keyid>       # gpg, if you need a passphrase-protected key
--sign plugin:<name>     # external signer plugin
```

An unsigned bundle has to be an explicit choice on both sides — `--no-sign`
at build, `--allow-unsigned` at verify/install. There is no silent fallback.

On the target, trust comes from a key you supply, never from the bundle:

```sh
debark verify ./bundle --key operator.pub            # one key
debark verify ./bundle --keyring /etc/debark/keys  # a directory of them
```

To constrain which *archive* keys resolution is allowed to trust, point at a
file listing the approved fingerprints:

```sh
debark build --snapshot t.tar.zst --approved-keys ./approved-keys.txt jq
```

A source whose `Signed-By` doesn't resolve to one of those keys fails the
build with exit 6 rather than quietly resolving against it.

### Checking a bundle before you ship it

```sh
debark inspect ./bundle     # manifest, lock, warnings, sizes (does not verify)
debark doctor ./bundle      # what's likely to go wrong on the far side
```

`doctor` reports heuristics — maintainer scripts that look like they want the
network, snap shim packages, DKMS packages with no matching headers, and
redistribution terms (multiverse/restricted/non-free). It says "no obvious
network action found", never "proven safe", and it never fails a build on its
own. A policy file does that:

```sh
debark build --snapshot t.tar.zst --policy ./policy.yaml jq
```

### Checking before you commit on the target

```sh
debark install ./bundle --key operator.pub --status    # what would change; changes nothing
debark install ./bundle --key operator.pub --dry-run   # simulate the install
```

### Building from macOS or Windows

Resolution needs the target release's own apt. When the host can't provide it,
debark runs it in a container (Docker or Podman) automatically:

```sh
debark build --backend container --snapshot t.tar.zst --sign operator.key jq
```

`auto` is the default and picks the container backend whenever host apt isn't
a match for the target. Everything else — fetching, indexing, signing,
assembly, verification — is pure Go and runs natively anywhere.

The container backend mounts a debark binary into the container and
re-enters it, so it needs a **static Linux build** of debark for the
target's architecture. On Linux the running binary usually is one. On macOS
and Windows it never is (a Mach-O or PE executable is not a Linux ELF binary),
so build one and put it beside the debark you run:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/debark-linux-amd64 ./cmd/debark
```

`bin/debark-linux-<arch>` next to the running executable is found with no
flag at all. To keep it somewhere else, name it with `--self-binary PATH`, the
`DEBARK_SELF_BINARY` environment variable, or `self_binary:` in the config
file — flag first, then environment, then file. The same file works for
`--embed-binary`, which is why it uses the same name.

### Machine-readable output

Every command takes `--json` for a documented JSON object, and `--json-events`
to stream NDJSON progress events:

```sh
debark build --snapshot t.tar.zst --json --json-events - jq
```

Schemas live in [`api/`](api/).

### Config and profiles

If you build for the same targets repeatedly:

```sh
debark config init          # write a default config file
debark config path          # where it lives (XDG-correct)
debark config show          # the effective config, after all precedence
debark build --profile prod --snapshot t.tar.zst jq
```

Precedence is flag → env (`DEBARK_*`) → config file → defaults.

### Reproducible builds

Set `SOURCE_DATE_EPOCH` and two builds of the same request produce a
byte-identical bundle:

```sh
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) debark build --snapshot t.tar.zst jq
```

## What's in a bundle

```text
bundle/
├── debark.manifest.json        every file, with size and sha256
├── debark.manifest.sig         detached signature over the manifest
├── lock.json                   exact versions to install, and why each is here
├── snapshot.json               the target state this was resolved against
├── evidence.json               what happened during the build
├── README.txt                  human-readable summary, generated
├── last-run-added.txt          what this run added
├── last-run-removed.txt        what this run removed
├── last-run-unreferenced.txt   pool files this run's repo does not index
├── sbom.cdx.json               CycloneDX SBOM (with --sbom)
├── bin/                        embedded debark binary (with --embed-binary)
└── repo/                       a real flat apt repository
    ├── Packages
    ├── Packages.gz
    ├── Release
    └── pool/…                  the .deb files
```

`repo/` is an ordinary apt repository. If debark itself ever fails you, the
packages are still right there and installable by hand.

Ship it as a directory, or as a single archive with `--tar name`.

## Command reference

| Command | What it does |
|---|---|
| `snapshot create` | Capture this machine's package state. No root, no network. |
| `snapshot inspect` | Show what a snapshot contains, and which of the two kinds it is. |
| `snapshot from-base` | Save a baseline OS as a reusable snapshot. Use `--interactive` for guided choices. |
| `snapshot list-bases` | List the stock releases this binary can synthesize a snapshot from. |
| `build` | Build from a captured `--snapshot` or a baseline OS selected with `--base`. Use `--interactive` for guided setup. |
| `verify` | Check a bundle before apt sees it. |
| `install` | Verify, check preconditions, install exact locked versions. |
| `inspect` | Show a bundle's manifest, lock, warnings and sizes. |
| `doctor` | Heuristics for what may break on the far side. |
| `keygen` | Generate an ed25519 signing key. |
| `store ls` / `store gc` | Inspect and prune the content-addressed store. |
| `config init/path/show` | Manage config and profiles. |
| `version` | Print the build identity. |

`debark <command> --help` for the full flag list on any of them.

## Exit codes

Every failure carries a class, and the table is frozen — safe to branch on in
scripts:

| Code | Meaning |
|---|---|
| 0 | Success — the bundle matches the request |
| 1 | Usage or configuration error |
| 2 | Environment — this machine can't do the job (no apt, no container runtime) |
| 3 | Incomplete — a bundle was built, but something is missing |
| 4 | Verification — signature, digest or repository metadata mismatch |
| 5 | Resolution — apt could not satisfy the request |
| 6 | Policy — a local policy or approved-keys violation |
| 7 | Target mismatch — wrong architecture or release for this machine |

## Platform support

| Role | Linux (Debian/Ubuntu) | Other Linux | macOS | Windows |
|---|---|---|---|---|
| `snapshot create` | native | — (must be an apt system) | — | — |
| `build` — resolution | native apt when host matches target, else container | container | container | container |
| `build` — fetch, index, sign, assemble | pure Go | pure Go | pure Go | pure Go |
| `verify`, `inspect` | pure Go | pure Go | pure Go | pure Go |
| `install` | yes | — | — | — |

**No macOS binary is published, and macOS is not in the CI matrix.** The
table above is about where debark *runs*, and a macOS builder resolving
through a container is a supported path — but `.github/workflows/ci.yml`
tests Linux and Windows only, and the release publishes `linux_amd64`,
`linux_arm64` and `windows_amd64` and nothing else. On macOS you build from
source, and nobody is watching for regressions on your behalf. See
[Status and known gaps](#status-and-known-gaps): the container backend has
still never been run on macOS at all.

**CI-tested:** Debian 12, Debian 13, Ubuntu 22.04, 24.04, 26.04 on **amd64**,
nightly, through the full fixture matrix. The **arm64** rows of that matrix
are skipped on the nightly schedule: `hack/matrix` probes container emulation
once per non-native (release, arch) pair and marks every row that needed it
`skipped` when no QEMU `binfmt_misc` is registered, and the nightly job does
not register it. arm64 is covered only when
`.github/workflows/matrix.yml` is dispatched manually with its `arm64` input
checked.

**Derivatives** — Mint, Pop!\_OS, Kali, Raspberry Pi OS, Proxmox and similar —
are best effort. They talk to their own apt so they may well work, but nobody
is watching for regressions on your behalf. If you depend on one, open an
issue; a maintained fixture is what turns "best effort" into "CI-tested".

## Troubleshooting

**`exit 5` — apt could not satisfy the request.** The package genuinely isn't
available for that release, or a version pin can't be met. The message names
what apt refused. Check the name exists for the target's release.

**`exit 7` — target mismatch.** The bundle is for a different architecture, or
needs a foreign architecture the target hasn't enabled. If it names one, the
fix is on the target:

```sh
sudo dpkg --add-architecture i386 && sudo apt-get update
```

**`exit 4` — verification failed.** Do not install it. Either the media is
damaged or the bundle was modified after signing. Rebuild rather than
investigate the copy on the media.

**`exit 2` on a macOS or Windows builder.** No container runtime. Start Docker
or Podman, or run the build on a Linux host matching the target.

**The bundle is bigger than expected.** Almost always `Recommends`. Build with
`--no-recommends`, or check `inspect` output for what pulled things in — the
lock records a reason for every package.

**The build refuses over redistribution terms.** The bundle contains packages
from multiverse/restricted/non-free, and moving those may be your call to make
rather than debark's. `--acknowledge-redistribution` proceeds; the warnings
are still recorded in the lock.

## Status and known gaps

Pre-1.0 and under active development. The full pipeline runs end to end — the
transcripts above are real — and the integration matrix (32 rows across five
releases and two architectures) passes. Baseline OS support (`build --base`,
`snapshot from-base`) is new: it closes the gap that made debark impossible
to try without physical access to the offline machine first, and it is the
fallback rather than the default, because it assumes a stock release where a
captured snapshot measures a real one. It arrives with limits of its own,
named below alongside everything else that is not done.

- **No version has been tagged yet.** The release pipeline is built and
  wired — one `vX.Y.Z` tag publishes a single GitHub release carrying the
  CLI archives (`linux_amd64`, `linux_arm64`, `windows_amd64`), `.deb` and
  `.rpm` packages, cosign-signed checksums and an SBOM, alongside the
  desktop application from `gui/` — but nothing has been cut from it, so
  there is no published artefact to install today. `go install` and a source
  build are the two channels that work now. Still intended and not built: a
  project apt repository and a container image for the resolver backend,
  with Debian ITP pursued last. A macOS tap was on that list once and is
  not any more: no macOS artefact is published for one to point at.
- **Baseline OS support has been run for real on exactly one release, and
  stops short of installing.** On 2026-09-06, on a real Ubuntu 24.04 amd64
  system with `--backend local`, `snapshot list-bases`, `snapshot from-base`,
  `snapshot inspect`, `build --base` and `install --status` all ran against
  the live Ubuntu archive, and the base transcripts above are that run.
  `install` proper is the step not taken: **no bundle built from a
  synthesized snapshot has been installed on a real machine.** The plan was
  checked; nothing was committed. No base build has been signed either.
- **The container backend has now run `from-base`, once, from Windows.** On
  2026-09-06, on Windows 11 with Docker 29.6.2 and a
  `bin/debark-linux-amd64` beside `debark.exe`, `snapshot from-base
  ubuntu:24.04/minimal --backend container` and then `build --backend
  container ... jq` both ran against the live Ubuntu archive and exited 0 —
  147 packages assumed, 3 packages and 379.9 kB in the bundle, closed-world
  check passed. That is one release, one architecture, one host OS, and the
  bundle was neither signed nor installed. macOS has still never been tried,
  and the backend's own unit tests still drive a fake.
- **Only Ubuntu 24.04 amd64 has been run at all.** Debian 12 and 13, Ubuntu
  22.04 and 26.04, and arm64 are untried on this path, including whether
  Debian's seeds (`apt`, `init`, `task-gnome-desktop`) resolve. Experiment E8
  confirmed that Ubuntu 24.04's three (`ubuntu-minimal`,
  `ubuntu-server-minimal`, `ubuntu-desktop-minimal`) resolve in noble; the
  builtin table's other twelve bases are unverified. A missing seed fails
  loudly — apt says `Unable to locate package` — so it costs a failed build
  rather than a wrong bundle, which is why this ranks below the fidelity gap
  rather than above it.
- **Base fidelity is measured for Ubuntu 24.04 amd64 and reasoned everywhere
  else.** `docs/experiments/E8-base-fidelity.md` compared each variant's seed
  closure against Canonical's published image manifests: zero packages assumed
  present that a real install lacks, and 441/431/1007 in the harmless
  direction. That pass needed a fix first, which is the part worth knowing —
  `ubuntu-desktop-minimal` resolved from an empty root pulls in `lsb-base`, a
  transitional package the desktop image does not install, and a base claiming
  it would have made every desktop bundle short by one dependency. Debian 12
  and 13, Ubuntu 22.04 and 26.04, and arm64 are **not** measured; for those the
  subset claim is still reasoned, and careful seed choices demonstrably do not
  guarantee it.
- **The guided flow has no captured run of its own.** `snapshot from-base
  --interactive` has been driven through a real pty, and that is how the base
  picker was caught offering `desktop` as its default — the largest of the
  three claims, and the exact inversion of the rule the feature rests on;
  fixed now. What is missing is a recorded run to quote, and the fuller
  `build --interactive` flow — package entry, output path, signing key, the
  review step — has not been driven end to end at all. Its refusals, without
  a TTY and under `--json`, are unit-tested rather than demonstrated.
- **The container backend's closed-world check is not fully proven.** It may
  be structurally unable to pass in one configuration; measuring that is the
  next task on the list.
- **Derivative distributions are untested**, as above.
- **No head-to-head performance benchmark** against the Bash prototype this
  replaces. The prototype installed 74 packages / 40 MB in about 6.5 seconds;
  that number is the *prototype's*, not debark's, and is quoted only as
  evidence that the install path isn't the bottleneck worth optimising.

If something below the fold reads like a claim that a thing works today, this
section wins.

## Design boundaries

Treated as product definition, not backlog. debark will not:

| Never | Why |
|---|---|
| Write its own apt dependency solver | Wrong bundles that fail on the far side, and unbounded maintenance. This is the existential risk the whole design avoids. |
| Manage snap, Flatpak, pip, npm, cargo, OCI or Helm | Different closure and trust semantics; each already has a tool. |
| Become a mirror manager or artifact repository | aptly, Pulp, Katello, Artifactory and Nexus own that problem. |
| Ship a daemon, scheduler or server | cron and CI already do this. |
| Ship a GUI or full-screen TUI as the primary interface | Has to work over SSH, serial consoles and CI logs. `--interactive` is line-oriented prompts that scroll like any other output, and it ends by printing the equivalent command. |
| Self-update | Conflicts with distro packaging and controlled environments. |
| Collect telemetry, analytics or crash reports | Disqualifying for this audience. A snapshot is a sensitive inventory of a real machine. |
| Add licence checks, caps or metering | The bundle and the target-side verifier never need to know whether anyone paid. |
| Become a CVE scanner or licence adjudicator | debark records what it finds; it does not become Trivy or a lawyer. |

## Documentation

- [docs/quickstart.md](docs/quickstart.md) — the workflows walked through in full
- [docs/faq.md](docs/faq.md) — common questions
- [docs/formats.md](docs/formats.md) — snapshot, lock, manifest and bundle formats
- [docs/security-model.md](docs/security-model.md) — the trust chain in plain language
- [docs/security/verification-guide.md](docs/security/verification-guide.md) — verifying a bundle by hand
- [docs/threat-model.md](docs/threat-model.md) — adversaries, trust boundaries, where each attack fails
- [CONTRIBUTING.md](CONTRIBUTING.md) — how to build, test and contribute
- [SECURITY.md](SECURITY.md) — vulnerability disclosure
- [GOVERNANCE.md](GOVERNANCE.md) · [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) · [CHANGELOG.md](CHANGELOG.md) · [TRADEMARK.md](TRADEMARK.md)

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). This covers the whole
community codebase and is not going to change.

Everything needed to prepare, understand, transfer, verify and install a
bundle is free forever. A future commercial edition could add organisational
features — fleet coordination, centrally administered policy, HSM/KMS-backed
signing — but never anything security-relevant, and never a crippled version
of what's here. No commercial edition exists today. See
[docs/free-paid-policy.md](docs/free-paid-policy.md).

No telemetry. No analytics. No update check. Not behind a flag, not in any
edition, ever.
