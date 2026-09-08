# ADR-007: closed-world check before export; exact-version install from the lock

**Status:** Accepted — 2026-09-03
**Design reference:** the closed-world-check requirement, experiment E3

## Context

A bundle is a single synthetic flat repository whose `Release` does not
carry the target's original archive origins, labels or releases. A target
with origin/release apt preferences (pins) could therefore resolve the
*same* package set differently against the bundle's flat repo than the
online build did — meaning "it built successfully online" would not
actually guarantee "it installs correctly offline", precisely the
failure category (resolves online, fails offline) this project's kill
criteria treat as existential.

## Decision

Two independent defences, applied on every build:

1. **Closed-world check.** Before export, the private apt root's sources are
   swapped for the finished bundle *only*, and `apt-get -s install <lock's
   install set>` (plus `-s full-upgrade` if applicable) is run — it must
   succeed with zero network fetches. The result and a digest of its output
   are recorded in `lock.json`'s `closed_world_check`.
2. **Exact-version install.** `debark install` never asks apt to resolve
   the bundle freely — it always requests `pkg=version` for the exact set
   the lock recorded, never "whatever is newest in the bundle."

A third, weaker defence applies unconditionally too: `Release` carries
`Origin: debark`, `Suite: bundle`, and the target's own codename.
Full per-origin repository partitioning is deliberately left as a v1.x
option, to be built only if experiment E3 evidence from a real pinned
target demands it.

## Consequences

- **Accepted happily.** A build that reaches `closed_world_check.result:
  "ok"` has *proven*, offline, before ever leaving the builder, that the
  exact install the lock describes actually works against the bundle as
  shipped — the strongest correctness claim this design can make without a
  live target.
- **Accepted unhappily.** The closed-world check adds real build time (a
  second apt simulation pass) to every build; it is deliberately not
  skippable except via an explicitly-named debugging flag
  (`buildjob.Options.ClosedWorldCheck` defaults to true), so builders
  cannot casually disable the safety net.
- Per-origin `Release` partitioning — the strongest possible defence against
  pin confusion — was deliberately deferred rather than built speculatively;
  a target with unusually aggressive pins may still see a
  `closed_world_check` failure that a partitioned repository would have
  avoided, until E3 evidence justifies building it.

## Alternatives considered

- **Let `install` run a plain `apt-get install <package names>`** (no
  version pins) against the bundle repo, trusting apt to reach the same
  conclusion online and offline. Rejected: this is exactly the failure mode
  this ADR exists to close — a different resolution on the target side is
  possible whenever pins are in play, and "trust apt to agree with itself
  across two different repository views" is not a proof, only a hope.
- **Partition the bundle repository by origin from day one,
  unconditionally.** Rejected as speculative complexity: deferred explicitly
  to "if a real pin case demands it" (experiment E3), preferring the two
  cheaper defences above as the v1 answer.
