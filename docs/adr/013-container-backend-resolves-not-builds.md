# ADR-013: the container backend re-enters the CLI to resolve, not to build

**Status:** Accepted — 2026-09-03
**Design reference:** the backend-selection section (as refined during the initial build-out); relies on ADR-002, ADR-004, ADR-008

## Context

The design originally specified that the container backend runs
`debark build --backend=local` inside the container — i.e. the *entire*
build pipeline (resolve, fetch, index, sign, assemble) executing inside the
container whenever the host lacks a matching local apt. The build-out examined this
against two other already-frozen commitments: ADR-008's rule that
transfer-integrity signing must never be exposed to anything the operator
does not directly control, and principle 2's requirement that determinism
be controlled by exactly one code path. Running the *entire* build inside
the container would mean the signing key (the `Signer` — potentially a
local ed25519 key file, a `gpg` agent socket, or a plugin process) would
need to be mounted or forwarded into a container whose provenance the host
does not fully control, and would mean two different code paths (host-
native build, container-internal build) could each independently produce
"the deterministic bundle" — exactly the duplication principle 2 exists to
prevent.

## Decision

The container backend re-enters the CLI to **resolve**, not to build. When
resolution needs to run inside a container (ADR-002), the container runs
the hidden `debark resolve --backend=local` subcommand only, mounting the
snapshot (read-only), the content-addressed store, and a workspace
(read-write). It returns a `resolve.Plan` (the frozen `core/resolve.Plan`
type) plus the staged `.deb` files apt already downloaded, written to the
mounted workspace. Everything after that — store ingestion, repository
indexing, lock-writing, manifest-building, and signing — runs on the host,
through the single build code path used regardless of which backend
resolved.

Rationale, stated plainly: signing keys must never enter a container;
determinism is controlled by one code path on the host; the engine stays a
library whose backends only resolve.

## Consequences

- **Accepted happily.** A signing key never needs to be provisioned inside a
  container image or mount at all — it stays exactly where the operator
  already trusts it, on the host. Determinism now has exactly one
  implementation to get right, regardless of which backend resolved; a bug
  fixed there is fixed for both backends simultaneously. `core/engine`
  stays a library whose *backends* are the only thing that knows how to
  talk to a container at all — nothing about signing, indexing or bundle
  assembly needs to know containers exist.
- **Accepted unhappily.** This is a departure from the original design's
  literal text, discovered and corrected during the initial build-out rather
  than anticipated up front — exactly the kind of refinement this ADR exists to
  record faithfully rather than silently. The container image now needs a
  `resolve` subcommand surface to be hidden-but-present and independently
  testable, more heavily relied upon than the original design anticipated
  (already lists `resolve`/`fetch` as "exposed only as hidden
  debugging subcommands," which this decision leans on directly).
- The container's mounted workspace becomes a small but real inter-process
  contract — staged `.deb` files at agreed paths, matching
  `resolve.Plan.Selections[].StagedPath` — that both the container-internal
  `resolve` invocation and the host-side ingestion step must agree on. This
  seam did not need to exist under the original "container runs the whole
  build" design.

## Alternatives considered

- **Run the full `build` pipeline inside the container**, as the design
  originally specified, forwarding a signing key or agent socket into the
  container for the signing step. Rejected: this is precisely the trust
  boundary ADR-008 exists to keep intact — the manifest signature is what a
  target trusts *instead of* trusting the builder's disk in general, and
  mounting the signing key into a container (a pulled image, a container
  runtime, whatever else the image contains — a larger, less auditable
  dependency surface than the host) weakens exactly the guarantee the
  signature is supposed to provide, for no compensating benefit.
- **Run the full build inside the container, but sign on the host as a
  bolt-on second step** over whatever the container produced. Rejected:
  container-internal code would still determine the deterministic bundle
  contents (indexing, file selection, layout) that the host merely signs
  without having produced — two code paths for "assemble the bundle"
  (native and containerised) is the exact duplication principle 2 warns
  against, and a determinism bug in the container-only path would be
  invisible to any test that only exercises the host path.
- **Keep the engine ignorant of containers entirely**, shelling out to a
  container purely to fetch raw `.deb` bytes with no structured
  `resolve.Plan` returned. Rejected: this would push interpretation of
  apt's resolution output (reasons, origins, fingerprints — everything the
  lock needs) onto ad hoc parsing of container logs or a bespoke
  side-channel, instead of the same typed `resolve.Plan` the local backend
  already produces; `core/resolve/types.go` is frozen precisely so both
  backends can share one answer shape.
