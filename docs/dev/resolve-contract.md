# The `debark resolve` contract (frozen)

`debark resolve` is a hidden debugging command **and** the interface the
container backend uses to re-enter this binary inside an image of the target
release (ADR-013). Two callers implement against it — the CLI command and the
container driver that invokes it — so it is frozen here rather than
in either of their heads.

The container resolves and **only** resolves. Indexing, manifest generation and
signing stay on the host: signing keys must never enter a container, and one
host-side code path is what makes bundles byte-identical.

## Command line

```
debark resolve --snapshot FILE --archives DIR --work DIR
                 [--plan-out FILE]
                 [--backend local|auto|container]      default: auto
                 [--package NAME[=VERSION]]...          repeatable
                 [--external-repo DIR]                  staging repo of vendor .debs
                 [--external-name NAME]...              repeatable
                 [--recommends=true|false]              default: from the snapshot
                 [--upgrades]
                 [--arch ARCH]
                 [--approved-key FINGERPRINT]...        repeatable
                 [--json]
```

| Flag | Meaning |
|---|---|
| `--snapshot` | Path to a `snapshot.tar.zst` **or** an extracted snapshot directory. Required. |
| `--archives` | Where apt must leave downloaded `.deb` files (`Dir::Cache::archives`). Required; created if absent. |
| `--work` | Scratch directory for the private apt root and captured output. Required; created if absent. |
| `--plan-out` | Where to write the plan envelope. Default: stdout. |
| `--backend` | Inside a container this is always `local`. `auto` outside. |
| `--package` | An apt package name, optionally `name=version`. |
| `--external-repo` | A directory of vendor `.deb` files already indexed with a `Packages` file, added to the private root as `deb [trusted=yes] file:///… ./`. |
| `--external-name` | A package name provided by `--external-repo`, to be installed alongside `--package`. |
| `--recommends` | Overrides the target's effective `APT::Install-Recommends`. |
| `--upgrades` | Adds the `full-upgrade --download-only` pass. |
| `--arch` | Resolve for a different architecture than the snapshot's. |
| `--approved-key` | Restricts which archive key fingerprints resolution may trust. |

## Output envelope

One JSON object, to `--plan-out` or stdout. Nothing else is written to stdout —
diagnostics go to stderr, and `--json-events` NDJSON (when requested) also goes
to stderr so it can never corrupt the envelope.

```json
{
  "schema_version": "debark.resolveplan/v1",
  "plan": { "...": "the resolve.Plan value, JSON-encoded field for field" },
  "backend": {
    "kind": "local",
    "apt_version": "2.6.1",
    "dpkg_version": "1.21.22",
    "distro_id": "debian",
    "version_id": "12"
  },
  "archives_dir": "/archives",
  "files": [
    { "name": "jq", "arch": "amd64", "version": "1.6-2.1+deb12u2",
      "filename": "jq_1.6-2.1+deb12u2_amd64.deb", "sha256": "…", "size": 63984 }
  ]
}
```

`files` lists what is actually on disk in `--archives` when the command exits,
with digests the caller re-verifies. It is deliberately redundant with `plan` —
the caller checks the two agree, which catches a truncated mount or a partial
download without trusting either side alone.

## Exit codes

The standard table (`core/dferr`): `0` success, `1` usage, `2` environment,
`3` incomplete (some input unresolved — **the plan is still written**), `5`
resolution failed, `6` policy. The caller must read the envelope on `3` as well
as `0`.

## How the container backend invokes it

Mounts, all explicit:

| Host | Container | Mode |
|---|---|---|
| the debark binary for the target arch | `/debark` | ro |
| the snapshot archive | `/snapshot.tar.zst` | ro |
| the work dir | `/work` | rw |
| the archives dir | `/archives` | rw |
| the external staging repo, when present | `/external` | ro |

```
docker run --rm --platform linux/<target arch> \
  -v <binary>:/debark:ro -v <snapshot>:/snapshot.tar.zst:ro \
  -v <work>:/work -v <archives>:/archives \
  <image pinned by digest> \
  /debark resolve --backend local --snapshot /snapshot.tar.zst \
                    --archives /archives --work /work --plan-out /work/plan.json \
                    --package … 
```

Rules the driver must hold to:

- The binary mounted in must be a **static linux binary for the target
  architecture** (`CGO_ENABLED=0`). The driver checks this before running and
  fails with a clear environment error rather than producing an exec-format
  error from inside the container.
- The image is pinned by digest from `core/distro`'s table. A tag-only image
  produces a warning recorded in the lock's resolver block.
- No network restriction is applied: resolution must reach the archives. The
  **closed-world check** is the step that runs with no network, and it runs on
  the host.
- Nothing is written to the image; every mutable path is a mount.
- The container never receives a signing key, a private key path, or the
  operator's keyring.
- `--json-events -` is added to the inner argv **when, and only when, the
  driver has somewhere to forward the stream** (`ContainerOptions.Events`).
  See below.

## The inner process's event stream

The driver adds `--json-events -` to the inner invocation so that what happens
inside the container is visible while it is happening. Without it a container
build reports nothing between `backend.selected` and the run finishing:
measured from Windows against a real daemon, a 61 s `build --backend
container` emitted four events, every one of them raised by the host.

Three rules, and they are the contract:

1. **The stream is untrusted input.** It comes from the same process, over the
   same pipe, as the envelope this document already says the caller must
   cross-check rather than believe. The driver accepts only lines whose
   `schema` is exactly `debark.events/v1`, whose `type` is one core/evidence
   declares, and whose `level` is one of the three defined; it strips control
   characters from `msg`, bounds its length, and refuses attribute values that
   are not scalars. Anything else is dropped and counted, and the count is
   reported by the host as a `warning` event of its own.
2. **Event lines are removed from the captured diagnostics.** The driver shows
   the last few lines of captured output in its error messages, and NDJSON
   would otherwise push the inner process's real failure off the top.
3. **Forwarded events reach the operator's live stream and never the
   bundle.** The host stamps `attrs.forwarded_from: "container"` on every one
   of them, and `core/engine` drops events carrying that key before they reach
   the collector that becomes `evidence.json` — which the manifest hashes and
   the signature covers. How many events an inner process emits, and in what
   words, is a property of the binary that was mounted and of what its apt met
   that afternoon, not of the request; and a claim by a process this driver
   spends three functions refusing to trust does not belong in a signed
   record. Nothing is lost that was ever there: before this, no inner event
   crossed the boundary at all.

`snapshot from-base` run in a container (`core/apt/basecontainer.go`) does the
same. Its `-` is stdout rather than stderr, because that command has no
envelope on stdout to protect; the driver watches both streams so neither
caller has to encode the difference.

The **closed-world check deliberately does not do this.** Its captured output
is digested into `lock.ClosedWorld`, which reaches `lock.json`, the lock
digest, the bundle id and the signature, and a second process's narration has
no business in that path. It also has no progress to report.
