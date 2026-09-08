# Platforms and prerequisites

[Documentation index](README.md) · [Quick start](quickstart.md)

The builder downloads packages online. The target receives and installs them
offline. They can use different operating systems, but apt resolution must
match the target.

## CLI support by role

| Role | Debian/Ubuntu Linux | Other Linux | Windows | macOS |
| --- | --- | --- | --- | --- |
| `snapshot create` | Yes, on an apt/dpkg system | Only on compatible apt/dpkg systems | No | No |
| `build` and `snapshot from-base` | Compatible local apt or container | Container | Linux container | Linux container; unvalidated |
| `verify` and `inspect` | Yes | Yes | Yes | Source build; no CI |
| `install` | Yes | Requires a compatible apt/dpkg target | No | No |

Release configuration provides CLI binaries for **Linux amd64**, **Linux
arm64**, and **Windows amd64**, with `.deb` and `.rpm` CLI packages for
Linux. An RPM installs the builder/verifier tool; it does not add RPM bundle
support. macOS has no published binary or CI coverage.

The target release matrix includes Debian 12/13 and Ubuntu 22.04/24.04/26.04.
The [nightly workflow](../.github/workflows/matrix.yml) runs amd64 fixtures.
arm64 fixtures require a manual dispatch with QEMU emulation enabled; a release
binary's availability does not imply equivalent integration coverage.

Derivative distributions such as Mint, Pop!_OS, Kali, Raspberry Pi OS, and
Proxmox are best effort. Include the exact release, architecture, and sources
when [reporting a result](../SUPPORT.md).

## Local Linux builders

The CLI selects `--backend auto` by default. It uses local apt when its
compatibility checks match the target; otherwise it selects a container.
Having apt installed is not enough to build for every release.

The CLI build needs Go 1.26+ but no C compiler or desktop libraries.
Snapshot capture and verification need no root privileges. Installing packages
on the target requires root.

## Container builders

Use Docker or Podman configured for **Linux containers** and make sure its
daemon/runtime is available to your user. Container builds need access to the
target's archives and may pull a distribution image.

The container mounts and executes a **static Linux debark binary for the target
architecture**. A Windows `.exe` or macOS executable cannot serve as that
helper. Obtain the matching Linux CLI release, or cross-compile from the same
checkout as the builder.

In a POSIX shell, from the repository root:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o bin/debark-linux-amd64 ./cmd/debark
```

In PowerShell, the equivalent scoped command restores your environment afterward:

```powershell
$savedDebarkEnv = @($env:CGO_ENABLED, $env:GOOS, $env:GOARCH)
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    go build -o bin/debark-linux-amd64 ./cmd/debark
} finally {
    $env:CGO_ENABLED, $env:GOOS, $env:GOARCH = $savedDebarkEnv
}
```

Use `arm64` in both places for an arm64 target. Cross-architecture execution
also needs container emulation or a native host of that architecture.

Point the builder at the helper explicitly:

```sh
debark build --backend container --self-binary ./bin/debark-linux-amd64 \
  --snapshot target.snapshot.tar.zst --out ./bundle --sign operator.key jq
```

Alternatively, place it at `bin/debark-linux-<arch>` or
`debark-linux-<arch>` beside the **running executable**. Discovery is relative
to that executable, not the current working directory. You can also set
`DEBARK_SELF_BINARY` or `self_binary:` in the config; precedence is flag,
environment, then config.

A WSL installation alone is not a backend for the native Windows CLI.
You may run the Linux CLI inside a suitable WSL distribution, or use the native
Windows CLI with a Linux container and helper binary.

## Baseline OS list

Every release below offers `minimal`, `server`, and `desktop` variants.
Omitting the variant selects `minimal`.

| Target release | Baseline prefix | Example |
| --- | --- | --- |
| Debian 12 | `debian:12` | `debian:12/minimal` |
| Debian 13 | `debian:13` | `debian:13/server` |
| Ubuntu 22.04 | `ubuntu:22.04` | `ubuntu:22.04/minimal` |
| Ubuntu 24.04 | `ubuntu:24.04` | `ubuntu:24.04/server` |
| Ubuntu 26.04 | `ubuntu:26.04` | `ubuntu:26.04/desktop` |

List the definitions included in your binary:

```sh
debark snapshot list-bases --arch amd64
debark snapshot list-bases --arch arm64 --json
```

The variants describe assumed installed packages. They do not install the
selected OS or profile. Set `--arch` explicitly when it differs from the
builder. See [formats](formats.md) for custom baseline definition files and
[status](status.md#baseline-os-builds) for measured fidelity.

## Desktop application

The published Linux desktop package targets **amd64** on Ubuntu 24.04/26.04
and Debian 13. Its GTK/GLib dependencies exclude the packaged build from
Ubuntu 22.04 and Debian 12. Those releases can still use the CLI or receive
bundles from a newer builder.

The desktop app requires the `debark` CLI separately. Windows desktop builds
are available with limited validation and no support commitment. There is no
macOS desktop build.

See the [desktop README](../gui/README.md), [packaging record](../gui/docs/packaging.md),
and [Windows record](../gui/docs/windows.md).
