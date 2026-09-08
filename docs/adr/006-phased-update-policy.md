# ADR-006: phased-update policy via `APT::Machine-ID` or a conservative never-include fallback

**Status:** Accepted — 2026-09-03
**Design reference:** the phased-update policy and the risk table, decision D12, experiment E1

## Context

Ubuntu's `Phased-Update-Percentage` mechanism selects a package version by
hashing the machine-id together with the package — deterministic per
machine, not random per run. A builder resolving with its *own* machine-id
can therefore select a version the target's apt would never have chosen,
producing a bundle the target's own `apt-get upgrade` would disagree with —
a silent, hard-to-detect wrong-bundle bug (risk table: "Medium
likelihood / existential impact").

## Decision

When the snapshot carries the target's `machine_id`, the private apt root
sets `APT::Machine-ID` to that value, so the resolving apt makes exactly
the phasing decision the target's own apt would make. When the snapshot was
captured with `--redact` (no machine-id available), resolution instead sets
`APT::Get::Never-Include-Phased-Updates=true` — the conservative choice,
since a fully-phased version is one every target accepts regardless of its
own machine-id. Which policy applied is recorded in the lock
(`resolver.phased_updates`), never silently. The bundle's own flat
repository carries no phasing field at all, so the target installs exactly
what the lock says regardless of its local apt's phasing state. Validated
by experiment E1.

## Consequences

- **Accepted happily.** The common case (an unredacted snapshot) reproduces
  the target's exact phasing decision; the redacted case degrades to
  "conservative, not wrong" rather than "silently wrong."
- **Accepted unhappily.** A redacted snapshot may miss a version that is
  actively phasing in for the target specifically, producing a bundle with
  an older version than the target's own apt would eventually settle on — a
  real, if bounded and honestly recorded (in the lock's
  `resolver.phased_updates` field), capability loss traded for privacy.
- This is one of the concrete reasons machine-id capture exists in the
  snapshot at all (ADR-005), whose own unhappy consequence (capturing
  machine-identifying information by default) is priced in there.

## Alternatives considered

- **Always use `Never-Include-Phased-Updates`**, regardless of whether a
  machine-id is available. Rejected: discards a real, verifiable capability
  (exact phasing reproduction, confirmed by experiment E1) for no benefit
  when the operator never asked for redaction.
- **Have the operator supply a phasing percentage or target version
  manually.** Rejected as unnecessary manual burden when the mechanically
  correct answer — use the target's own machine-id — is already available
  from the snapshot in the common case.
