# ADR-002: backend auto-selection with container fallback on host/target mismatch

**Status:** Accepted — 2026-09-03
**Design reference:** the supported-releases commitment, decision D3, experiment E2

## Context

Resolution must run using the target release's own apt (ADR-001), but the
machine running `debark build` is not always that release: it may be a
different Debian/Ubuntu version, a non-apt OS entirely (macOS, Windows), or
a CI runner. The platform matrix requires `build` to work on macOS
and Windows, where there is no host apt at all, and apt's own solver
behaviour can genuinely differ across major versions (`solver3` in Ubuntu
25.10+).

## Decision

`auto` backend selection: if the host has apt, its `distro_id`
matches the target's, and its apt major.minor matches the target's, resolve
locally. Otherwise, resolve inside a pinned container of the target release,
selected from a distro image table by digest (`core/distro`), run with
`--platform linux/<target arch>`. macOS and Windows are container-only by
construction — there is no local branch available to them. When no
container runtime is found, debark hard-fails with the exact install
hint; it never falls back to a second-class resolution path.

## Consequences

- **Accepted happily.** A Linux engineer on a matching distro gets fast,
  dependency-free local resolution; everyone else gets a correct answer via
  container, at the cost of requiring a container runtime.
- **Accepted unhappily.** Requires Docker or Podman for the majority of
  realistic builder machines — any Windows/macOS workstation, any Linux host
  on a different release than every one of its targets. Container image pull
  time, and possible bind-mount I/O overhead on Docker Desktop (experiment
  E5), are accepted costs.
- `auto` is request-time-only: once resolved, the lock records `local` or
  `container`, never `auto`. This shows up directly in the frozen
  `lock.Resolver.Backend` enum (`local`/`container` only) versus
  `buildjob.Options.Backend`, which still allows `auto` as a request-time
  input — the two schemas deliberately disagree on this one enum for
  exactly this reason (see `api/schema/lock.v1.schema.json` and
  `api/schema/buildjob.v1.schema.json`).

## Alternatives considered

- **Always resolve in a container, never locally, for uniformity.**
  Rejected: unnecessarily slow and heavyweight for the common case (a
  Debian/Ubuntu engineer building for their own release), discarding the
  prototype's proven local-root design where it applies cleanly.
- **Silently resolve with whatever local apt is available, even on a
  version mismatch.** Rejected: this is exactly how a wrong-bundle bug
  enters undetected — solver behaviour genuinely differs across apt
  major versions. A mismatch must be a loud decision (prefer container, or
  warn and record it), never a silent one.
