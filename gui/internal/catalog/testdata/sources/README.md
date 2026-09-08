# apt sources fixtures

Input for `internal/catalog/sources_test.go`. The parser in `sources.go` is the
only duplicated logic in this repository (`docs/dev/catalogue-sourcing.md`), so
it is tested against files that came off real machines rather than against
hand-written shapes that only exercise what the author already thought of.

Each row says exactly where the bytes came from, because a fixture whose
provenance nobody recorded is a fixture nobody can trust.

| File | Provenance |
|---|---|
| `ubuntu-noble.sources` | Captured, verbatim. `/etc/apt/sources.list.d/ubuntu.sources` from `../testdata/real-targets/ubuntu-2404-state.tar.gz` — WSL2 Ubuntu 24.04.3 LTS, apt 2.8.3, amd64, captured 2026-09-03. |
| `ubuntu-noble-sources.list` | Captured, verbatim. The same machine's `/etc/apt/sources.list`: Ubuntu 24.04 moved its sources to deb822 and left this file as four comment lines. A real file with no entries in it at all. |
| `tailscale.list` | Captured, verbatim. The same machine's `/etc/apt/sources.list.d/tailscale.list` — a third-party vendor entry in the one-line format with `signed-by=`. |
| `debian-bookworm.sources` | Captured, verbatim. `/etc/apt/sources.list.d/debian.sources` from `../testdata/real-targets/debian-12-state.tar.gz` — `debian:bookworm-slim`, apt 2.6.1, amd64, captured 2026-09-03. Note the `#` comment *inside* each stanza: Debian's own image generator puts it there, and a parser that treats a comment as a field separator gets this file wrong. |
| `debian12-classic.list` | Copied from `../core/snapshot/testdata/debian12-list/etc/apt/sources.list`. The classic one-line layout, `deb-src` line included. |
| `flat-repo.list` | Real output. `hack/experiments/out/e3/aptroot-online/etc/apt/sources.list.d/e3-online.list` in the debark repository, written by experiment E3: two flat repositories (`./`) with `[trusted=yes]`. |
| `jammy-no-signed-by.sources` | Real output. `hack/experiments/out/e6/primary-ubuntu-24.04/aptroot/etc/apt/sources.list.d/jammy.sources` — deb822 with no `Signed-By:` at all, which apt accepts when the ambient `trusted.gpg.d` covers the archive. |
| `inline-armored.sources` | Copied from `../core/snapshot/testdata/inline-armored/etc/apt/sources.list.d/inline.sources`. A `Signed-By:` folded across a dozen continuation lines of base64 — colons, dots and all. None of it may be read as a field, and the stanza around it must survive. |
| `builtin-ubuntu-2604-amd64.sources` | `base.Builtin("amd64")` output for `ubuntu:26.04/*`, verbatim. This is the exact text production parses for a stock Ubuntu base. |
| `builtin-ubuntu-2604-arm64.sources` | `base.Builtin("arm64")` output for `ubuntu:26.04/*`: the ports archive, one stanza, `-security` folded in with the rest. Different in shape from the amd64 document, which is why both are here. |
| `builtin-debian-13-amd64.sources` | `base.Builtin("amd64")` output for `debian:13/*`, verbatim. |
| `vendor-arch-signed-by.list` | **Written for this test**, reproducing the line every mainstream vendor's install script writes (`[arch=amd64 signed-by=…]`, `[arch=amd64,arm64 …]`), plus a trailing `#` comment and a tab-separated line. |
| `mirrors.sources` | **Written for this test.** One stanza listing three mirrors, one restricted with `Architectures:`, one switched off with `Enabled: no`. |
| `malformed.sources` | **Written for this test.** Every way a stanza can be unreadable: a line that is not a field, a field given twice, a paragraph carrying none of the four fields, an unexpanded `${codename}`. Every one of them must be *reported*. |
| `not-sources.sources` | Real bytes, wrong file. Three stanzas lifted from `../testdata/ubuntu-noble-main-Packages.excerpt`. Feeding a `Packages` index to the sources parser must produce complaints, not silence and not sources. |
| `empty.sources` | Zero bytes. |

## What is deliberately not a file here

CRLF line endings and a UTF-8 byte-order mark are applied to a fixture **in the
test**, not committed as files. Git normalises line endings on checkout
depending on the developer's `core.autocrlf`, so a committed CRLF fixture would
be testing the checkout rather than the parser, and would pass or fail
depending on whose machine it ran on.
