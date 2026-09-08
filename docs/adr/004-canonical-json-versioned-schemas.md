# ADR-004: RFC 8785 canonical JSON and versioned, frozen schemas for every format

**Status:** Accepted — 2026-09-03
**Design reference:** principle 2 (determinism), decisions D4 and D12

## Context

Every debark document that is hashed or signed must produce byte-identical
output for byte-identical meaning, on any platform, in any language — both
a determinism requirement (principle 2: "two builds of the same
request must produce byte-identical manifests") and a prerequisite for a
third party to independently verify a signature without running debark's
own code (decision D12: schemas are "versioned, canonical, public, and frozen
before the first alpha").

## Decision

Any object that is hashed or signed is first canonicalised per RFC 8785
(JCS): object keys sorted by UTF-16 code unit, no insignificant whitespace,
ES6 number formatting, UTF-8 output — implemented once in `core/canonical`
and used by every package that signs or digests anything. A digest is always
lowercase hex SHA-256 of the canonical bytes. Every document format carries
an explicit `schema_version` (or `schema`) constant as its first logical
field. A published schema version is never mutated; a shape change is a new
version with a documented migration, not an in-place edit.

## Consequences

- **Accepted happily.** A third party can verify a debark signature with
  an off-the-shelf JCS library in any language — no dependency on
  debark's Go code or its exact `encoding/json` behaviour (see
  `docs/formats.md` §1 for the full recipe).
- **Accepted unhappily.** Two different, correct-looking digests can exist
  for the same logical document: a canonical-content digest (for
  cross-referencing between documents, e.g. `manifest.lock_digest`) and a
  whole-file digest (for on-disk tamper evidence, e.g.
  `manifest.files[].sha256`). Confusing them is the easiest mistake a new
  implementer can make; `docs/formats.md` §1.3 exists specifically to
  prevent it.
- Every format is frozen the moment it ships: `api/schema/` is the source of
  truth, checked against the Go types by a reflection-based drift test
  (`api/schema/schema_test.go`) precisely so this promise cannot be broken
  silently by an unrelated later change.

## Alternatives considered

- **Hash `json.MarshalIndent` output directly** (Go's default indented
  encoding). Rejected explicitly by the project's own engineering rules
  ("Never hash `json.MarshalIndent` output") — field order is not guaranteed
  stable across languages or even across Go versions, so this could never be
  reproduced by independent tooling and would not even guarantee
  determinism within debark across a future toolchain upgrade.
- **A binary or self-describing format** (CBOR, Protobuf) instead of
  canonical JSON. Rejected: JSON keeps every format trivially
  human-readable (principle 6: "record as if an auditor will read it in
  five years") and toolable with nothing more than a text editor and `jq`;
  the audience most likely to need independent verification — regulated and
  OT operators — is exactly the audience least likely to want to
  stand up a Protobuf toolchain first.
