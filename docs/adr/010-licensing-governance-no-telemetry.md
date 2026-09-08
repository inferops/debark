# ADR-010: Apache-2.0, DCO, early trademark registration, and zero telemetry with no entitlement code in the community binary

**Status:** Accepted — 2026-09-03
**Design reference:** the do-not-build list, the "Entitlement" and "Telemetry" sections, decisions D7 and D8

## Context

The commercial model depends on a durable free/paid boundary the
market believes will hold — the named anti-template, Keryx, "went
commercial and died" by gating its core and hiding downloads behind a trial
wall. Both monetization research rounds independently converged on
the same licence, contribution and telemetry posture, and the
product's own audience — air-gapped, OT, classified and regulated
environments — treats any phone-home capability as disqualifying on sight,
not merely undesirable (do-not-build list).

## Decision

The community repository is Apache-2.0 in full, with DCO-signed
contributions and no CLA. The project name and logo are trademarked early,
with a published policy that permits forks, factual reference, and
unmodified redistribution while reserving "official" and "Enterprise"
branding. No analytics, crash-reporting or telemetry library appears
anywhere in the source tree — not merely disabled by default, but
structurally absent, so "no telemetry" is verifiable by reading the
dependency list rather than trusting a runtime flag. The community binary
contains **no entitlement-checking code whatsoever**; only the interface
seam (an edition string that is branding metadata, never a feature gate)
exists, and any future licence verification lives exclusively in a separate
commercial component.

## Consequences

- **Accepted happily.** The free/paid boundary is trustworthy precisely
  because it cannot be quietly moved — no capability in the shipped source
  could be "just turned on" to gate something free yesterday. A
  security-conscious downstream, exactly this product's target audience,
  can confirm the no-telemetry claim by source inspection in an afternoon.
- **Accepted unhappily.** Apache-2.0 with no CLA means anyone may fork and
  compete commercially with no relicensing recourse — accepted explicitly
  as the cost of avoiding the BUSL/SSPL pattern that "relicenses forks
  projects without proven revenue lift" (HashiCorp→OpenTofu, Redis→Valkey
  cited directly). The project competes on brand, support and
  execution, not licence leverage.
- No CLA also means the project can never relicense the accumulated
  contribution history later without tracking down every contributor — a
  one-way door, taken deliberately.

## Alternatives considered

- **BUSL, SSPL, Elastic License, or another source-available licence**, to
  prevent a hosted-competitor fork. Rejected explicitly: none is DFSG-free
  (closing the door to Debian/Ubuntu inclusion — one of this project's
  specific differentiators against Canonical), and the 2023–2026
  track record of exactly this move backfiring was treated as
  decisive evidence, not a risk to merely hedge against.
- **A CLA**, to keep relicensing optionality open. Rejected: the chosen
  model does not depend on relicensing for revenue, so a CLA has no
  offsetting benefit against its known chilling effect on the contributor
  base a single-maintainer project needs to survive ("Maintenance tax
  ... higher with no contributor growth → freeze features").
- **Ship telemetry disabled-by-default**, with an opt-in flag, as most
  developer tools do. Rejected outright, not merely defaulted off: the
  audience includes classified and OT environments where the
  mere *presence* of a phone-home code path fails a source audit regardless
  of its default state.
