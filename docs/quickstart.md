# Quick start

Build a signed bundle on an online computer, then install it on a Debian or
Ubuntu machine without network access.

For a shorter guide with snapshot and baseline choices and optional signing,
see the [README quick start](../README.md#quick-start).

[Overview](../README.md) · [Prerequisites](platforms.md) ·
[CLI reference](cli.md) · [Troubleshooting](troubleshooting.md)

## Before you start

You need:

- An **offline target** running Debian or Ubuntu with apt/dpkg and a Linux
  debark binary. Capturing a snapshot needs no root; installation does.
- An **online builder** with debark and access to the target's repositories.
  It needs a compatible local apt, or Docker/Podman and a Linux debark helper.
  See [container setup](platforms.md#container-builders) for Windows,
  macOS, or builds for another release.
- A way to transfer files and establish which public signing key the target
  trusts.

Use a captured snapshot when you can access the target. If you cannot, use
the [baseline workflow](#build-with-a-baseline-os).

Commands below use a POSIX shell, with debark on `PATH`. They are examples,
not recorded transcripts; versions and download sizes vary by target and date.

## Build for an existing target

### 1. Create a signing key on the builder

Run once in a directory where you can keep the key:

```sh
debark keygen --out operator.key
```

This creates an Ed25519 private key, `operator.key`, and the corresponding
public key, `operator.pub`. The private key is unencrypted. Restrict access
to it and keep it off the transfer media. For passphrase-protected signing,
see [GPG signing](cli.md#signing-and-trust).

Provision `operator.pub` on the target through a trusted channel. If you
carry the public key on the bundle media, authenticate it against a fingerprint
you obtained independently. The media alone cannot establish trust in its
own signing key.

### 2. Capture the offline target

On the target:

```sh
debark snapshot create --out target.snapshot.tar.zst
debark snapshot inspect target.snapshot.tar.zst
```

Capture reads installed package state, sources, pins, keyrings, and apt
configuration. It needs no network or root access and writes the output
archive. Treat the snapshot as sensitive inventory.

To omit machine ID, proxy settings, and operator labels, add `--redact`.
Review the remaining contents before sharing outside your environment.
Redaction also affects phased-update selection; see the [FAQ](faq.md).

Transfer `target.snapshot.tar.zst` to the builder.

### 3. Resolve and download on the builder

From the directory containing the snapshot and signing key:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./bundle --sign operator.key jq tree
```

apt resolves the requested packages against the captured target state.
debark downloads the selected packages, checks the plan against the bundle's
repository, and writes the lock, manifest, and signature.

Inspect and verify the result before transferring it:

```sh
debark inspect ./bundle
debark doctor ./bundle
debark verify ./bundle --key operator.pub
```

`inspect` displays information without verifying it. `doctor` reports
heuristics, including packages whose maintainer scripts may need the network.
Review warnings and any nonzero build exit code before shipping.

### 4. Transfer the bundle

Copy the **entire** `bundle` directory, including its manifest, signature,
lock, and `repo/` subdirectory. A folder containing only the `.deb` files
does not preserve debark's verification and install workflow.

For a single archive, use `--tar` instead of `--out` when building:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --tar transfer --sign operator.key jq tree
```

The archive is named `transfer.debark.tar.zst`. `verify`, `inspect`,
and `install` accept a bundle directory or archive.

### 5. Verify, review, and install on the target

From the directory containing the transferred bundle and the trusted public key:

```sh
debark verify ./bundle --key operator.pub
debark install ./bundle --key operator.pub --status
```

Check the target identity, package changes, and warnings. If the machine's
installed packages or repository configuration changed since capture, take a
fresh snapshot and rebuild.

When the plan matches your intent:

```sh
sudo debark install ./bundle --key operator.pub --yes
```

`install` repeats verification before invoking apt. The normal install path
uses a temporary apt configuration pointing at the bundle, and applies exact
versions from the lock.

The bundle supplies package downloads offline. Package maintainer scripts
still run as part of installation and may need the network or other resources.
Test the actual package set in an isolated target environment when that matters.

## Build with a baseline OS

Choose this when you cannot capture the target first:

```sh
debark snapshot list-bases
debark build --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key jq
```

This uses the signing key from step 1. See [baseline IDs](platforms.md#baseline-os-list)
for releases and variants. `--arch` is the target architecture; if omitted,
it defaults to the builder's architecture. `--base` and `--snapshot` are
mutually exclusive.

A baseline describes packages **assumed to be installed already**. It does
not install an OS, and selecting `desktop` does not add a desktop to the bundle.
A machine missing assumed packages may need dependencies the bundle omits.

After transfer, run the same verification and status commands as above.
The status report identifies missing assumed packages. These findings warn;
they do not by themselves block installation. Capture the real target and
rebuild if the assumption does not fit.

To save a baseline for reuse:

```sh
debark snapshot from-base ubuntu:24.04/minimal \
  --arch amd64 --out base.snapshot.tar.zst
```

Later builds can use `--snapshot base.snapshot.tar.zst`. Saving the file does
not turn an assumption into a captured machine. Both synthesis and building
need network access. Read [baseline validation limits](status.md#baseline-os-builds)
before relying on this path.

## Use guided prompts

On the builder:

```sh
debark build --interactive
```

The CLI asks for missing target, package, output, signing, upgrade, and SBOM
choices, then shows a review before starting the build. You can provide known
choices with flags:

```sh
debark build --interactive --base ubuntu:24.04/minimal --arch amd64 \
  --out ./bundle --sign operator.key
```

Entered packages are saved to `packages.txt` in the working directory, or an
unused numbered filename. The file is written **before final confirmation**.
After the build attempt, the CLI prints a reusable command; retain any additional
backend, policy, or profile options you supplied.

Interactive commands require a terminal and cannot be combined with `--json`
or `--json-events`. For automation, use explicit flags and `--list`.

## Include a vendor package

Supply an existing local `.deb` alongside archive package names:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./vendor-bundle --sign operator.key ./vendor/agent.deb jq
```

Replace `./vendor/agent.deb` with your package's path. apt resolves its
dependencies against the target's configured repositories. URL inputs and
operator-supplied digests are covered in the [CLI guide](cli.md#package-inputs).

## Refresh a bundle

Capture a fresh snapshot after changes to the target. Then rerun the complete
package request:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./bundle --update --sign operator.key jq tree
```

Downloaded content is reused. `--update` refreshes resolution and prunes
superseded versions; it does not remove every package you stopped requesting.
See [updates and storage](cli.md#updates-and-storage) for retained pool files
and garbage collection.

## Carry the CLI in the bundle

Supply a trusted Linux CLI binary matching the target architecture:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./bundle --sign operator.key \
  --embed-binary ./debark-linux-amd64 jq
```

On an amd64 target, run it from the bundle:

```sh
./bundle/bin/debark-linux-amd64 verify ./bundle --key operator.pub
sudo ./bundle/bin/debark-linux-amd64 install ./bundle --key operator.pub --yes
```

Embedding saves a separate installation step. Establish trust in the executable
before running it: a binary from untrusted media cannot independently prove
that the same media is safe. Provision a trusted verifier ahead of time, or
authenticate the executable through your release-verification process.

## Try the automated offline demo

For a disposable demonstration using Docker, see
[`hack/demo-airgap.sh`](../hack/demo-airgap.sh). It creates container targets,
builds a signed bundle, and verifies and installs with the target container's
network disabled.

For failures, start with [troubleshooting](troubleshooting.md). For automation
and less common options, use the [CLI guide](cli.md).
