# Status and known limitations

[Documentation index](README.md) · [Platform requirements](platforms.md)

This page summarizes the validation recorded in the repository as of
**2026-09-08**. It distinguishes available functionality from the tests and
manual runs that support it. It is not a report of the latest remote CI run.

debark is pre-1.0. Published schemas are versioned contracts; other behavior
may change in minor releases. See the [changelog](../CHANGELOG.md).

## Captured-snapshot builds

The [offline demo](../hack/demo-airgap.sh) captures a Debian 12 container,
builds a signed bundle, and verifies and installs it in a container with
`--network none`. Integration fixtures cover archive packages, vendor packages,
updates, trust failures, and other target conditions.

The [integration workflow](../.github/workflows/matrix.yml) schedules amd64
fixtures across Debian 12/13 and Ubuntu 22.04/24.04/26.04. arm64 execution is
opt-in through a manual dispatch that enables emulation. See the
[matrix documentation](../hack/matrix/README.md) for how skipped and failed
rows are reported.

A successful resolution checks the captured state. Later changes to the
target, unavailable package resources, or network-dependent maintainer scripts
can still prevent a successful offline installation.

## Baseline OS builds

`build --base` and `snapshot from-base` are implemented, with narrower
recorded validation than captured snapshots:

- The recorded Linux run used Ubuntu 24.04 amd64 with the local backend.
  It exercised listing, synthesis, inspection, bundle building, and
  `install --status`.
- A Windows container run exercised Ubuntu 24.04 minimal synthesis and a
  `jq` build. See the [Windows record](../gui/docs/windows.md).
- The original baseline validation did not complete a signed build followed
  by installation on a real target. An install plan is not evidence that
  package installation completed.
- [Experiment E8](experiments/E8-base-fidelity.md) measured the Ubuntu 24.04
  amd64 variants against image manifests. Equivalent fidelity measurements
  are not recorded for Debian 12/13, Ubuntu 22.04/26.04, or arm64.

A baseline is an assumption about the installed package set. Missing assumed
packages are reported as warnings on the target; the warning itself does not
block installation or repair the bundle. Prefer a captured snapshot when
available.

## Containers and other platforms

The container backend has unit tests and Linux integration fixtures.
The recorded Windows run covers one target release and architecture; it does
not establish coverage for every combination. Broader validation of the
container check that resolves using only the finished bundle remains useful.

macOS has no release artifact, CI job, or recorded container run. Derivative
distributions are best effort. See [platform support](platforms.md).

## Interactive CLI and desktop

Interactive prompts have unit coverage, but the original validation did not
record a complete `build --interactive` session. Contributions that exercise
the full guided workflow are welcome.

The desktop workflow is **Target → Packages → Bundle**. Linux is the supported
desktop platform. Its [accessibility](../gui/docs/accessibility.md),
[performance](../gui/docs/performance.md), [packaging](../gui/docs/packaging.md),
and [security](../gui/docs/security-review.md) reports describe what was tested
and which gaps remain. Windows is experimental; macOS is out of scope.

## Distribution and reproducibility

The release configuration builds CLI and desktop artifacts under a shared
version, with separate signed checksum files. Check the actual assets and
notes for the release you use. SLSA provenance jobs are conditional on repository
visibility; their configuration alone does not prove that an artifact has
provenance attached.

There is no project apt repository or dedicated published resolver image.
Use release downloads or a source build.

`SOURCE_DATE_EPOCH` fixes artifact timestamps. Reproducibility also requires
the same snapshot, package bytes and indexes, resolver, options, signing setup,
and initial output/store state. A live repository can change between runs.
The desktop's native libraries and compiler are additional build inputs; see
its [release guide](../gui/docs/release.md).

## Helping close gaps

Useful contributions include reproducible target fixtures, baseline fidelity
measurements, container runs on additional hosts, and desktop accessibility
testing. Report the source revision, host and target, commands, and results,
including skips. See [CONTRIBUTING.md](../CONTRIBUTING.md).
