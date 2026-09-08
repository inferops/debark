# Architecture decision records

Each ADR records one settled, consequential decision: the context that
forced it, the decision itself, its consequences (including the ones
accepted unhappily), and the alternatives that were considered and why they
lost. They are tight by design — an ADR that runs past a page is usually two
ADRs — and each records the decision it
came from. ADRs 001–012 are the project's original decision log; ADR-013 records a refinement made during the initial build-out itself, and
ADR-014 a deliberate addition to the design's scope made after it — both in
the same spirit, a decision an ADR should capture faithfully rather than let
go unrecorded.

None of these are open questions. Per the design's own framing,
"these are settled unless new evidence appears; do not reopen them in
implementation." A package that believes one is wrong should say so in an
issue or a new ADR, not silently diverge from it.

Each ADR opens with a **Design reference** line naming the part of the
project's original design document the decision came from. That document is
not published in this repository — it predates it — so those lines are
provenance, not links: an ADR's own Context section is the authoritative
statement of the problem it settles, and is written to stand alone.

| ADR | Title |
|---|---|
| [001](001-apt-in-private-root-no-solver.md) | apt in a private root is the sole resolver; no Go dependency solver |
| [002](002-backend-auto-selection-container-fallback.md) | Backend auto-selection with container fallback on host/target mismatch |
| [003](003-three-bundle-artifacts.md) | Three separate bundle artefacts — repository, lock, signed manifest |
| [004](004-canonical-json-versioned-schemas.md) | RFC 8785 canonical JSON and versioned, frozen schemas for every format |
| [005](005-snapshot-aptconf-versions-machine-id.md) | Snapshot captures `apt.conf.d`, apt/dpkg versions, and a redactable machine-id |
| [006](006-phased-update-policy.md) | Phased-update policy via `APT::Machine-ID` or a conservative never-include fallback |
| [007](007-closed-world-check-exact-version-install.md) | Closed-world check before export; exact-version install from the lock |
| [008](008-layered-trust-model.md) | Layered trust — archive keys as captured policy, operator key out-of-band, verify strictly before apt |
| [009](009-plugin-protocol-stdio-json.md) | Out-of-process plugin protocol over stdio JSON; no `.so` plugins; Signer is the only v1 capability |
| [010](010-licensing-governance-no-telemetry.md) | Apache-2.0, DCO, early trademark registration, and zero telemetry with no entitlement code in the community binary |
| [011](011-go-stack-cli-repository-writer.md) | Go stack — Cobra, koanf, mpb, slog, and a pure-Go repository writer |
| [012](012-exit-codes-json-contracts.md) | The 0–7 exit-code scheme and versioned `--json` object contracts |
| [013](013-container-backend-resolves-not-builds.md) | The container backend re-enters the CLI to resolve, not to build |
| [014](014-synthesized-snapshots.md) | A base definition produces a synthesized snapshot, not a second build path |
