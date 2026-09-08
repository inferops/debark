# Frequently asked questions

[Documentation index](README.md) · [Quick start](quickstart.md) ·
[Troubleshooting](troubleshooting.md)

## Why capture the target instead of just downloading packages?

apt normally resolves against the installed package database of the machine
where it runs. An online builder may already have dependencies that the offline
target lacks.

A snapshot lets debark give apt the target's installed packages, sources, pins,
and relevant configuration. The resulting bundle is planned against that state.
Capture again after target changes; a snapshot is not a live connection to the
machine.

## Do I need access to the offline machine first?

You can start without it using `--base`:

```sh
debark build --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key jq
```

Create the key with `debark keygen --out operator.key` first.
The baseline describes packages assumed to be installed. It is useful for
preparing new machines, but a captured snapshot is more specific.

If the target has fewer packages than assumed, the bundle may omit needed
dependencies. `install --status` reports missing assumed packages as warnings;
it does not repair the bundle or automatically refuse all baseline differences.
See [baseline validation limits](status.md#baseline-os-builds).

## Does a desktop baseline install a desktop?

No. `ubuntu:24.04/desktop` tells the resolver what a desktop target is assumed
to have already. The packages you request are what debark prepares for
installation. A baseline is neither an OS installer nor a disk image.

Each built-in release has `minimal`, `server`, and `desktop` variants.
Omitting the variant selects `minimal`. See the [baseline list](platforms.md#baseline-os-list).

## What information is in a snapshot?

A snapshot includes installed package state and apt configuration, including
sources, pins, relevant keyrings, and a machine ID when available. Operator
labels can add identifying metadata. Repository URLs and configuration may
reveal internal infrastructure or credentials.

`snapshot create --redact` removes machine ID, proxy settings, and labels.
It does **not** promise to remove every sensitive value from arbitrary
configuration. Inspect the archive before sharing it publicly; prefer synthetic
fixtures in bug reports.

See [snapshot formats](formats.md) and [the security model](security-model.md)
for the captured fields and trust boundaries.

## How do phased updates work?

For automatic upgrades requested with `--upgrades`, the captured machine ID
helps reproduce apt's phased-update selection. A redacted snapshot uses a
conservative fallback.

Explicitly named package requests behave differently: apt can bypass phasing
for those requests, so redaction does not guarantee that every explicitly
requested version has completed rollout. See [ADR-006](adr/006-phased-update-policy.md)
and [experiment E1](experiments/E1-phased-updates.md).

## Which operations need the internet?

`build` and `snapshot from-base` need access to package repositories and any
vendor URLs in the request; container resolution may also pull an image.
The desktop catalogue downloads package metadata from the selected sources.

`snapshot create`, `snapshot inspect`, `verify`, and `inspect` work
locally. Normal target installation downloads packages from the bundle.
Package maintainer scripts can still try to access the network; debark does
not rewrite or sandbox them. `doctor` offers heuristics, and an isolated test
of your package set gives stronger evidence.

debark has no telemetry, analytics, crash uploads, or automatic update checks.

## Does the target need debark installed system-wide?

No. You can run a trusted Linux executable from a local directory or transfer
media. `--embed-binary` includes one in the bundle. Snapshot capture also
needs a debark executable if you use that workflow.

Authenticate an embedded executable before running it; it cannot independently
establish trust in its own media. The [quick start](quickstart.md#carry-the-cli-in-the-bundle)
explains the bootstrap step.

The bundle also contains an ordinary apt repository. Using it manually requires
your own verification and installation procedure and bypasses debark's normal
verification gate and locked install plan.

## Does a valid signature mean the packages are safe?

It means a trusted key signed the manifest and the covered contents have not
changed, subject to the verifier's checks. It does not prove the packages are
free of vulnerabilities or that their maintainer scripts will work offline.

Trust the operator key through a channel independent of the bundle. Archive
signatures, operator signatures, and vendor digests answer different questions;
see the [security model](security-model.md).

## Can I use Windows or macOS as the builder?

Windows uses a Linux container for apt resolution and needs a static Linux
debark helper matching the target architecture. Docker or Podman must be
available and able to run Linux containers.

macOS has a source-build path, but no published binaries, CI coverage, or
recorded container validation. See [platform setup](platforms.md#container-builders).
The desktop app has its own [platform limits](../gui/README.md#platforms).

## Why does an updated bundle still contain an old package?

`--update` prunes superseded versions, while protecting locked versions and
certain vendor inputs. It does not delete every package removed from your
request. Unindexed pool files are recorded in `last-run-unreferenced.txt`;
apt cannot select those files through the bundle's repository.

Use a fresh output directory for a bundle without retained pool files. See
[updates and storage](cli.md#updates-and-storage).

## What does the project support beyond deb packages?

debark focuses on Debian/Ubuntu `.deb` transfers. It delegates dependency
resolution to apt and does not manage Snap, Flatpak, pip, npm, Cargo, OCI, or
Helm packages. It is not a mirror manager, daemon, or general vulnerability
scanner. The optional desktop app prepares bundles on the online machine;
the CLI remains the complete interface.

See [governance and scope](../GOVERNANCE.md).

## Is debark open source? Is there a paid edition?

The CLI and desktop app are Apache-2.0. There is no commercial edition today.
Preparing, inspecting, transferring, verifying, and installing a bundle are
community capabilities. The [free/paid policy](free-paid-policy.md) defines
the boundary for any future organizational features.

Contributors sign off commits under the [DCO](../DCO); no CLA is required.
