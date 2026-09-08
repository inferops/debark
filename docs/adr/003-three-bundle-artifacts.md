# ADR-003: three separate bundle artefacts — repository, lock, signed manifest

**Status:** Accepted — 2026-09-03
**Design reference:** the bundle-artefacts section, decision D4

## Context

A bundle must simultaneously be (a) something apt can install directly, (b)
an exact, explainable record of what was chosen and why, and (c) something a
target can cryptographically trust before apt is ever pointed at it. A
single combined document cannot cleanly serve all three roles: apt needs a
real repository on disk in its own format, an auditor needs "why is this
package here" in a form suited to review and diffing, and a verifier needs
one small, rarely-changing signed object that provably covers everything
else.

## Decision

Every bundle carries three separate, purpose-built artefacts: a real
flat apt repository (`repo/` — installable, and the only thing apt itself
ever reads), a lock plan (`lock.json` — what was chosen, from where, and
why, plus the closed-world proof, ADR-007), and a signed manifest
(`debark.manifest.json` + `.sig` — digests of literally everything else,
cryptographically bound together, ADR-004/ADR-008). Each has its own schema
version and therefore evolves independently.

## Consequences

- **Accepted happily.** Clean separation of concerns: the repository stays
  boring, standard apt metadata; the lock carries all the provenance detail
  and is expected to be large and read/diffed routinely; the manifest stays
  small and singularly focused on being the signed root of trust.
- **Accepted unhappily.** Three files (plus the detached signature) must be
  kept consistent instead of one — the manifest's `snapshot_digest`,
  `lock_digest` and `repository.*` fields exist purely to bind them back
  together, and any code that builds a bundle must get the ordering right:
  repository and lock finalised, only then the manifest, only then the
  signature (see ADR-013, whose host/container split exists specifically to
  keep this ordering under one code path).

## Alternatives considered

- **One combined document that is both the lock and the manifest.**
  Rejected: the lock is large (one entry per package, with per-file origin
  detail) and read/diffed by humans and scripts routinely; the manifest is
  meant to be small and rarely change shape. Conflating them would make the
  signed object slow to review and force every lock-shape change to also be
  a manifest schema change.
- **Embed the lock and file list directly inside apt's own `Release` file**
  via extension fields. Rejected: `Release` is a deb822 format with its own
  narrow, apt-defined grammar; overloading it would be fragile against apt's
  parser and unreadable by generic JSON tooling — exactly the "auditor five
  years from now" failure principle 6 warns against.
