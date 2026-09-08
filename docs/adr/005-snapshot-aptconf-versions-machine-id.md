# ADR-005: snapshot captures `apt.conf.d`, apt/dpkg versions, and a redactable machine-id

**Status:** Accepted — 2026-09-03
**Design reference:** the snapshot-capture section, experiments E2 and E4

## Context

The Bash prototype's snapshot already captured `status`, sources,
preferences and keyrings, and building from it worked — except when it
didn't: a target with `Install-Recommends "false"` or a `Default-Release`
pin in `apt.conf.d` is silently mis-resolved if the builder never learns
about it (experiment E4); Ubuntu's phased-update selection depends on the
target's own machine-id and cannot be reproduced without it (ADR-006); and a
resolving apt whose major.minor differs from the target's can silently
choose differently (experiment E2) with no way to detect it if the
target's own apt/dpkg versions were never recorded.

## Decision

`debark.snapshot/v1` captures three fields the prototype did not:
`apt.conf[]` (`apt.conf` and `apt.conf.d/*`, verbatim, proxy credentials
redacted at the byte level), `target.apt_version`/`target.dpkg_version`
(from `apt-get -v`/`dpkg --version`), and `target.machine_id` (from
`/etc/machine-id`) — the last one explicitly redactable, stripped by
`snapshot create --redact` along with proxy credentials and operator
labels.

## Consequences

- **Accepted happily.** `apt.conf.d` settings that change resolution are now
  visible to the builder instead of silently absent; solver-version
  divergence is now detectable instead of invisible; phased-update selection
  can be reproduced exactly (ADR-006) when the operator is willing to carry
  the machine-id.
- **Accepted unhappily.** A machine-id is machine-identifying information,
  and this design deliberately captures it by default — the redaction flag
  exists to make omitting it a first-class, easy operator choice, at the
  cost of falling back to the more conservative `never-include`
  phased-update policy (ADR-006) when redacted.
- `apt.conf[]` capture necessarily also captures proxy configuration, which
  requires redaction at the individual-file-byte level
  (`snapshot.File.Redacted: true` marks a file whose stored bytes differ
  from the target's for this reason) — a narrower, harder-to-implement
  redaction than simply omitting a whole field.

## Alternatives considered

- **Capture `apt.conf.d` but not machine-id**, treating phased updates as
  out of scope for v1. Rejected: Ubuntu ships phased updates on every
  `-updates` pocket; ignoring it is not a narrow edge case but a routine,
  silent wrong-bundle generator on Ubuntu targets specifically (risk
  table rates this "Medium likelihood / existential impact").
- **Make redaction the default** (opt-in machine-id capture) rather than
  opt-out. Rejected in favour of matching the rest of the snapshot's
  default posture: capture what a correct resolution needs by default, and
  let the audience that needs privacy ask for it explicitly with
  `--redact` — consistent with sources, preferences and keyrings already
  being captured verbatim by default.
