# ADR-009: out-of-process plugin protocol over stdio JSON; no `.so` plugins; Signer is the only v1 capability

**Status:** Accepted — 2026-09-03
**Design reference:** the extension-seams section, Appendix A ("Plugin handshake"), Appendix G ("Plugin transport"), decision D9

## Context

The design commits to extension seams that "cost nothing extra" today but
keep real options open later — specifically an HSM/KMS/Sigstore
signer as a commercial or community plugin, without coupling the community
binary's Go ABI to anyone else's code. The two commissioned research rounds
disagreed on the transport: one favoured `hashicorp/go-plugin` (gRPC), the
other stdio with JSON or CBOR framing (Appendix G).

## Decision

`debark.plugin/v1` is a language-agnostic, out-of-process protocol: a
plugin is any executable that prints a `Handshake` on start and then
exchanges single-line JSON `Request`/`Response` objects over stdio until its
stdin closes. No Go plugin ABI, no `.so` files, no gRPC dependency in the
community binary. Only the `sign` capability is defined in v1; the handshake
carries a capability list specifically so the host can refuse to call a
method a plugin never announced, and a future capability is additive to the
protocol rather than a breaking change.

## Consequences

- **Accepted happily.** A plugin can be written in any language with a JSON
  encoder and stdio, is crash-isolated from the host process (a plugin
  panic cannot bring down a build), and needs no shared-library loading or
  Go version/ABI compatibility with debark's own binary at all.
- **Accepted unhappily.** Stdio JSON is a lower-throughput, less strongly
  typed transport than gRPC — acceptable here because signing calls are
  rare (one manifest per bundle, plus at most one apt-repository signature)
  and the payloads are small (a canonical-JSON document's digest and a
  signature, not a data-plane stream). This would be the wrong tradeoff for
  a high-frequency or high-volume protocol, which this explicitly is not.
- The protocol carries its own version (`debark.plugin/v1`) independent
  of any specific capability's shape, so `SignParams`/`SignResult`'s own
  fields can only change via a new protocol version — a real constraint on
  future flexibility, accepted deliberately so a v1 plugin can never
  silently misinterpret a v2 host or vice versa.

## Alternatives considered

- **`hashicorp/go-plugin` over gRPC.** Rejected (Appendix G): pulls a
  substantial dependency and a network-protocol stack into the community
  binary for a capability that does not need gRPC's streaming or
  strong-typing benefits. Stdio JSON is the smallest, most language-agnostic,
  most crash-isolated option for this specific job; gRPC remains available
  later behind the same protocol-version scheme if a future capability
  genuinely needs it.
- **Go's `plugin` package (`.so` files).** Rejected outright and early:
  requires matching Go toolchain versions and build flags between host and
  plugin, is Linux-only in practice, and cannot be written in another
  language — none of which is acceptable for a project explicitly aiming to
  let a commercial HSM/KMS vendor or a community contributor ship a signer
  independently.
