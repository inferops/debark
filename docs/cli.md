# CLI guide

[Documentation index](README.md) · [Quick start](quickstart.md) ·
[Troubleshooting](troubleshooting.md)

Use `debark --help` for the command tree and `debark <command> --help` for
the flags in your installed version. Examples assume a captured
`target.snapshot.tar.zst` and an operator key created with
`debark keygen --out operator.key`.

## Commands

| Command | Purpose |
| --- | --- |
| `snapshot create` | Capture the target's installed packages and apt configuration |
| `snapshot inspect FILE` | Inspect a captured or synthesized snapshot |
| `snapshot list-bases` | List built-in baseline OS definitions |
| `snapshot from-base BASE` | Resolve a baseline and save it as a reusable snapshot |
| `build [pkg...]` | Build from `--snapshot` or `--base`; use `--interactive` for prompts |
| `verify BUNDLE` | Verify the signature, file digests, and bundle consistency |
| `install BUNDLE` | Verify, check the target, and install locked versions |
| `inspect BUNDLE` | Show manifest, lock, warnings, and sizes without verification |
| `doctor BUNDLE` | Report heuristics for potential offline installation problems |
| `keygen` | Generate an Ed25519 operator signing key |
| `store ls` / `store gc` | Inspect and prune the local content-addressed store |
| `config init` / `path` / `show` | Create, locate, and inspect configuration |
| `version` | Print version and build identity |
| `completion` | Generate Bash, Zsh, Fish, or PowerShell completions |

## Package inputs

You can mix archive package names and existing vendor files:

```sh
debark build --snapshot target.snapshot.tar.zst --sign operator.key \
  --out ./bundle jq tree ./vendor/agent.deb
```

Replace the vendor path with your actual file. Use `name=version` to request
an exact version available from the target's configured sources. apt handles
dependency resolution for both archive and vendor packages.

For repeatable requests, create `packages.txt`:

```text
# Blank lines and comments are ignored.
jq
tree
./vendor/agent.deb
```

```sh
debark build --snapshot target.snapshot.tar.zst \
  --list packages.txt --out ./bundle --sign operator.key
```

Local paths in a list are resolved relative to the list file. Supply multiple
lists with repeated `--list`, or use `--local-dir ./vendor` to scan a directory
of `.deb` files without recursion.

HTTPS URLs to vendor `.deb` files are also accepted. For stronger provenance,
provide an expected SHA-256 obtained from a trusted vendor source:

```sh
debark build --snapshot target.snapshot.tar.zst --out ./bundle \
  --sign operator.key "https://vendor.example.com/agent.deb" \
  --digest "https://vendor.example.com/agent.deb=REPLACE_WITH_64_HEX_SHA256"
```

The URL and digest above are placeholders. A list entry can alternatively use
`https://vendor.example.com/agent.deb sha256=REPLACE_WITH_64_HEX_SHA256`.

HTTPS protects transport; it does not establish a package's publisher.
The bundle signature records your approval of the assembled bundle. Vendor
provenance and redistribution warnings remain available in the bundle.

## Updates and storage

Repeat the **complete desired request** when rebuilding. Builds reuse downloaded
objects; `--update` refreshes indexes, resolves again, and prunes superseded
versions:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --list packages.txt --out ./bundle --update --sign operator.key
```

Without `--update`, existing pool files are retained. With `--update`,
versions selected by the new lock and protected vendor inputs remain.
`--no-prune` keeps superseded versions too.

Removing a package from your request does not necessarily remove its file from
an existing bundle. Pool files not indexed by the new repository appear in
`last-run-unreferenced.txt` and a `pool.unreferenced` warning. They remain
covered by the manifest, but apt cannot select them from that repository.
Build into a fresh output directory if you need a bundle without leftovers.

To reclaim unused objects in the local store, name every bundle you want to
retain:

```sh
debark store ls
debark store gc ./bundle ./other-bundle --dry-run
```

After reviewing the result, omit `--dry-run` to perform collection. debark does
not maintain a registry of every bundle you have built.

## Upgrades and recommendations

Add an upgrade plan for packages already installed on the target:

```sh
debark build --snapshot target.snapshot.tar.zst \
  --out ./bundle --sign operator.key --upgrades jq
```

Apply that upgrade set explicitly on the target:

```sh
sudo debark install ./bundle --key operator.pub --upgrade --yes
```

Build uses plural `--upgrades`; install uses singular `--upgrade`.

By default, captured-snapshot builds follow the target's `Install-Recommends`
setting. Override it with `--no-recommends` or `--recommends`. Excluding
recommendations can reduce bundle size but may omit optional functionality.

## Signing and trust

| Build option | Signing method |
| --- | --- |
| `--sign operator.key` | Ed25519 key from `debark keygen`; private key is unencrypted |
| `--sign gpg:KEY_ID` | GPG, including passphrase-protected keys |
| `--sign plugin:NAME` | External signer using the [plugin protocol](../examples/signer-plugin/README.md) |
| `--no-sign` | Explicitly create an unsigned bundle |

For Ed25519 signatures, supply trusted public keys on the target:

```sh
debark verify ./bundle --key operator.pub
debark verify ./bundle --keyring /etc/debark/keys
```

For a GPG-signed bundle, use a trusted GPG keyring:

```sh
debark verify ./bundle --gpg-keyring ./trusted-release-keys.gpg
```

The same trust options apply to `install`. Provision keys independently of
the bundle, or authenticate them through a separate trusted channel.
Do not infer trust from a key included in the bundle.

An intentionally unsigned bundle requires `--allow-unsigned` at verification
and installation. This weakens authentication and should be an explicit operator
decision. See the [security model](security-model.md) for exact verification
behavior and GPG trust boundaries.

## Policy and inspection

```sh
debark inspect ./bundle
debark doctor ./bundle
debark install ./bundle --key operator.pub --status
```

`inspect` does not verify. `doctor` reports heuristics such as network-related
maintainer scripts, snap shims, DKMS concerns, and redistribution notices.
Those findings are not proof that a package is safe or installable.

`install --status` checks the bundle and reports changes against the current
target without applying them. In the current CLI, `--dry-run` uses the same
planning path; it is not a full rehearsal of apt and maintainer scripts.

To enforce a local policy or restrict archive key fingerprints:

```sh
debark build --snapshot target.snapshot.tar.zst --out ./bundle \
  --sign operator.key --policy ./policy.yaml \
  --approved-keys ./approved-keys.txt jq
```

Create those files using the [format reference](formats.md). Policy and
approved-key violations use exit code 6. Redistribution findings remain recorded
when `--acknowledge-redistribution` suppresses an interactive prompt.

## Output formats

The default output is a directory. Use `--tar transfer` instead of `--out`
for `transfer.debark.tar.zst`. Add `--sbom` for a CycloneDX SBOM or
`--embed-binary PATH` to carry a target Linux executable.

A typical bundle contains:

```text
bundle/
├── debark.manifest.json        file sizes and SHA-256 digests
├── debark.manifest.sig         detached signature, when signed
├── lock.json                   selected versions and install plan
├── snapshot.json               target description
├── evidence.json               build evidence
├── README.txt                  generated summary
├── last-run-added.txt
├── last-run-removed.txt
├── last-run-unreferenced.txt
├── sbom.cdx.json                with --sbom
├── bin/                        with --embed-binary
└── repo/
    ├── Packages
    ├── Packages.gz
    ├── Release
    └── pool/                   .deb files
```

This is an overview; [formats](formats.md) defines the complete layout.

## JSON and progress events

Use `--json` for machine-readable command results. Commands that emit evidence
can stream NDJSON events with `--json-events PATH`. Keep the final result and
progress stream in separate files for straightforward parsing:

```sh
debark build --snapshot target.snapshot.tar.zst --out ./bundle \
  --sign operator.key --json --json-events ./build.events.ndjson jq \
  > build.result.json
```

A `--json-events` value of `-` streams to stdout. Avoid mixing that stream
with a final JSON object unless your consumer handles both. Always check the
process exit code.

See [schemas](../api/schema/) and the [format reference](formats.md).
Interactive mode cannot be combined with JSON modes.

## Configuration and profiles

```sh
debark config path
debark config init
debark config show
debark build --profile prod --snapshot target.snapshot.tar.zst \
  --out ./bundle --sign operator.key jq
```

Create the `prod` profile in your config before using the final example.
Precedence is **flags → environment (`DEBARK_*`) → config → defaults**.
`--config PATH` selects a file explicitly. Review effective configuration
before sharing it, since it can include local paths and environment details.

## Reproducibility

Set `SOURCE_DATE_EPOCH` to fix artifact timestamps. For repeatable output,
also hold the snapshot, repository indexes, package bytes, resolver version,
options, signing setup, and initial store/output state constant. A live archive
can change even when your package request is unchanged.

See [status](status.md#distribution-and-reproducibility) and the
[reproducible-build check](../hack/reproducible-check.sh) for project validation.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Usage or configuration error |
| 2 | Environment cannot perform the operation |
| 3 | Incomplete bundle or request |
| 4 | Verification failure |
| 5 | apt could not resolve the request |
| 6 | Policy or approved-key violation |
| 7 | Target mismatch, such as an incompatible architecture |

These classes are a public scripting contract. The error message provides the
specific cause and remedy. A release mismatch can be a warning rather than exit
7; review target warnings even when planning succeeds.
