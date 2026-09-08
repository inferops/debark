# ADR-008: layered trust — archive keys as captured policy, operator key out-of-band, verify strictly before apt

**Status:** Accepted — 2026-09-03
**Design reference:** principle 4, decision D6

## Context

Three different trust questions arise across a debark bundle's life:
"was this package authentically published by its archive," "did these
exact bytes cross the air gap unmodified," and "should this target's apt
trust this local repository." Conflating them is a real security failure
mode: a tampered snapshot could carry both a malicious source *and* the
very key that would authenticate it, so the snapshot's captured keyrings
cannot be treated as an independent root of trust.

## Decision

Three layers, kept strictly separate:

1. **Archive authenticity** is apt's own job, verified during
   `apt-get update`/fetch against keys the snapshot *records as captured
   policy*, never auto-imported as a replacement, with an optional
   organisation-approved fingerprint allowlist (`--approved-keys`)
   resolution must satisfy.
2. **Transfer integrity** is the manifest signature (ADR-004's canonical
   digest, produced by a `Signer`) — mandatory for `verify` to pass unless
   `--allow-unsigned` is explicitly given, and the operator's public key
   must reach the target *independently of the bundle's media*; debark
   refuses to trust a key found on the same medium it is verifying, with no
   default override.
3. **Local repository trust** is the target's own apt configuration,
   granted only after layer 2 has already passed.

`debark install` calls `verify` first and refuses to touch the system on
failure (exit 4) or target mismatch (exit 7).

## Consequences

- **Accepted happily.** A compromised or malicious snapshot cannot forge
  trust in layer 2 no matter what it claims in layer 1's captured
  keyrings — the layers cannot be laundered into each other. This is the
  property a security reviewer checks first, and it is checkable purely by
  reading the manifest/signature format (`docs/formats.md` §1.5), with no
  need to trust debark's implementation.
- **Accepted unhappily.** Operator key provisioning is manual and
  out-of-band by design — no "first-use trust," no same-media key. This is
  deliberate friction, accepted as the price of the guarantee.
  `--allow-unsigned` exists as an explicit, loudly-recorded escape hatch,
  never a quiet default.
- `verify` must fully re-derive trust from scratch on every run — it cannot
  cache or shortcut based on "this bundle passed before," because the whole
  point is that the medium between the last check and now is exactly what
  might have changed.

## Alternatives considered

- **Trust an operator public key shipped alongside the bundle on the same
  medium** (a common "payload + detached signature + certificate" scheme).
  Rejected outright: the entire point of the manifest signature is to
  detect *media* tampering, and a key travelling on the same tamperable
  medium can be swapped by the same attacker who would tamper with the
  payload — it verifies nothing.
- **Treat the snapshot's captured archive keyrings as sufficient
  authorisation on their own**, skipping the manifest signature when the
  archive-fetched packages are already apt-verified. Rejected: this is
  exactly the "tampered snapshot carries its own malicious key" hole
  described above, and it would leave vendor `.deb` files and any
  locally-supplied file with no verification story at all.
