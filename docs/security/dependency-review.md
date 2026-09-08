# Dependency review

Every third-party dependency in `go.mod`, as of **2026-09-03T21:11Z**: what
it is, why it is there, its licence (identified by reading that module's own
`LICENSE`/`COPYING` file in the local module cache — not inferred from its
`go.sum` entry or assumed from its ecosystem reputation), and what its
compromise would mean for debark specifically.

**A note on timing.** `go.mod` changed twice in the course of writing this
document, both times growing (dependencies were promoted from indirect to
direct, and a CLI/config dependency stack was added). That is expected during
the build-out of a contract-first codebase (`docs/dev/contract-brief.md`)
— it is not evidence of anything improper — but it means this document is a
snapshot, not a standing guarantee. Re-run `go list -m all` and re-check
licences before every release; do not assume this table is still exhaustive
by the time you read it.

## Direct dependencies

These are imported directly by debark's own packages (confirmed by `grep`
against `core/`, `api/`, `internal/`, `cmd/`, not inferred from `go.mod`'s own
direct/indirect markers, which lag behind actual imports because packages are
instructed not to run `go mod tidy` mid-build — `docs/dev/contract-brief.md`).

| Module | Version | Licence | Used for | Where | If compromised |
|---|---|---|---|---|---|
| `github.com/gowebpki/jcs` | v1.0.1 | Apache-2.0 | RFC 8785 JSON Canonicalization — the transform every digest and signature in the product is computed over | `core/canonical/canonical.go` | The single highest-leverage dependency in the tree. Every "the digest matches what was signed" claim in `docs/threat-model.md` §4 ultimately rests on this library producing the same canonical bytes on every machine, every time. A bug here (not necessarily malicious — a canonicalisation *inconsistency* would do just as much damage) could make a tampered document look untampered, or make a legitimate document fail verification. Small, focused, single-purpose library — a reasonable trust concentration for what it's asked to do, but worth naming explicitly rather than burying in a table row. |
| `pault.ag/go/debian` | v0.21.0 | MIT-style (Paul R. Tagliamonte) | `.deb` control-data parsing (`deb.LoadFile`), used when reading a `.deb`'s control stanza for repository-index generation and pool identity (`core/repository/api.go`, `core/bundle/assemble.go`'s `readDebIdentity`) | `core/repository`, `core/bundle` | Sits directly on the untrusted-input path: every vendor/external `.deb` a build processes gets parsed by this library. Debian-authored, in the Debian archive itself, actively maintained. A compromise (or a parsing bug) here is a plausible route to the path-traversal finding in `docs/security/review-findings.md` F1 — this library is *where* an attacker-controlled `Package:` field first enters the process, even though the actual missing check is debark's own, not this library's. |
| `github.com/ProtonMail/go-crypto` (`openpgp` subpackage) | v1.4.1 | BSD-3-Clause (Go Authors-derived) | Parsing captured OpenPGP keyrings during `snapshot create` — fingerprint and user-ID enumeration only | `core/snapshot/pgp.go` | Explicitly **not** used for any trust decision: no signature verification, no revocation checking is ever performed with it (the package doc comment in `pgp.go` is specific about this, matching the "captured policy, not an independent root of trust"). A widely-used, actively-maintained fork of `golang.org/x/crypto/openpgp` (ProtonMail's own mail client depends on it). Worst case of a compromise here: a snapshot's recorded keyring fingerprints are wrong or incomplete — a correctness/audit problem, not a verification bypass, since nothing downstream trusts this output as an authorization decision. |
| `github.com/klauspost/compress` (`zstd` subpackage) | v1.18.0 | BSD-3-Clause (Go Authors + Klaus Post) | Compressing/decompressing `snapshot.tar.zst` and `<name>.debark.tar.zst` | `core/snapshot/zstd.go`, `core/bundle/tar.go` | Also on the untrusted-input path: both a malicious snapshot archive and malicious bundle media are zstd streams this library decompresses before anything else sees them. A memory-safety bug in the decompressor would be reachable by an attacker who controls either input. High-profile, widely-audited, and the standard choice for zstd in Go — a reasonable dependency to carry, but the one most worth watching for CVEs given where it sits. |

## Indirect (transitive) dependencies

Not imported by any debark package directly (confirmed by `grep`); present
because a direct dependency needs them. Go only compiles what is actually
imported, so an unused transitive dependency (`stretchr/testify`, currently)
does not affect the shipped binary regardless of what `go.sum` lists.

| Module | Version | Licence | Pulled in by | Note |
|---|---|---|---|---|
| `github.com/cloudflare/circl` | v1.6.2 | BSD-3-Clause | `ProtonMail/go-crypto` | Elliptic-curve / post-quantum primitives for OpenPGP parsing. |
| `github.com/kjk/lzma` | (pseudo-version) | BSD-style (Andrei Vieru) | `pault.ag/go/debian` | Legacy LZMA decompression support for older `.deb`/control formats. |
| `github.com/xi2/xz` | (pseudo-version) | Public domain | `pault.ag/go/debian` | XZ decompression. Explicitly public-domain per its own `LICENSE` file — no compatibility concern with Apache-2.0. |
| `golang.org/x/crypto` | v0.41.0 | BSD-3-Clause | `ProtonMail/go-crypto` | Standard Go team supplementary crypto package. |
| `golang.org/x/sys` | v0.35.0 | BSD-3-Clause | multiple | Low-level OS syscall bindings. |
| `pault.ag/go/topsort` | v0.1.1 | MIT-style (Paul R. Tagliamonte) | `pault.ag/go/debian` | Topological sort, used internally by `pault.ag/go/debian`'s own relationship modelling. **Not** used by debark's own code and **not** a dependency solver of any kind — flagging this explicitly because its name alone could look, out of context, like exactly the "pure-Go apt dependency solver" the project's permanent do-not-build list forbids. It isn't one; debark never calls it. |
| `github.com/stretchr/testify` | v1.8.4 | MIT | (unresolved — present in `go.sum` but not imported by any `.go` file in this tree as of this review) | Confirmed by `grep -rl "stretchr/testify" --include="*.go"` returning nothing. Likely a transitive test-dependency requirement surfacing in the module graph from another module's own `go.mod`, not something debark's test suite currently uses (the tests read so far all use stdlib `testing` directly). Even if it starts being used, it is test-only and never links into a release binary. |
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 | direct CLI use (already imported by `internal/cli/command.go`, despite `go.mod`'s stale `// indirect` marker) | CLI command framework. |
| `github.com/spf13/pflag` | v1.0.10 | BSD-3-Clause (Alex Ogier / Go Authors) | `cobra` | POSIX/GNU-style flag parsing. |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | Apache-2.0 | `cobra` | Detects a Windows binary launched by double-click (so it can print guidance instead of a window that instantly closes). No security surface. |
| `github.com/knadh/koanf/v2` + `koanf/maps`, `koanf/parsers/yaml`, `koanf/providers/{env/v2,file,posflag}` | v2.3.6 / v0.1.2 / v1.1.1 / v2.0.1 / v1.2.1 / v1.0.2 | MIT (Kailash Nadh) | direct CLI use (already imported by `internal/cli/config/config.go`) | Config loading with flag > env > file precedence. |
| `github.com/go-viper/mapstructure/v2` | v2.4.0 | MIT (Mitchell Hashimoto) | `koanf` | Struct decoding for config values. |
| `github.com/mitchellh/copystructure`, `github.com/mitchellh/reflectwalk` | v1.2.0 / v1.0.2 | MIT (Mitchell Hashimoto) | `koanf` / `mapstructure` | Structural copy/walk helpers. |
| `github.com/fsnotify/fsnotify` | v1.9.0 | BSD-style (Go Authors + fsnotify Authors) | `koanf` | Filesystem-change notification. `koanf` can use this for live config-file reload; whether debark's own CLI code actually enables that feature was outside this package's reading scope to confirm — worth a one-line check before release, since a tool marketed as having no unexpected background behaviour should not be silently watching a config path unless that is an intended, documented feature. |
| `go.yaml.in/yaml/v3` | v3.0.4 | Dual MIT / Apache-2.0 | `koanf`'s YAML parser | Successor project to `gopkg.in/yaml.v3`. |

## The one dependency debark deliberately does *not* have: apt/dpkg

`apt-get`, `dpkg`, `dpkg-deb` and `gpg` are all invoked as **external
processes** via `os/exec` — never linked, vendored, or built from source into
the debark binary (ADR-001; `.github/workflows/licence-scan.yml`'s own
comment states this explicitly, and `SECURITY.md` repeats it as the answer to
"is debark GPL"). apt and dpkg are GPL-licensed; running them as a separate
program the same way a shell script would is the standard, well-established
boundary that keeps their licence from attaching to this binary — no
different from a Go program that shells out to `git` or `curl`. This matters
enough to the project's own commercial and distribution strategy that it has its own CI check (`licence-scan.yml`) rather than resting
on this document alone.

The same reasoning applies to `docker`/`podman` (container backend, exec'd,
never linked) and `gpg` (GPG signer/verifier, exec'd — see
`core/sign/gpg.go`).

## No telemetry, no analytics, anywhere in the dependency tree

Checked directly: `go.sum` (110 lines as of this review) contains no package
whose name matches any known analytics, telemetry, crash-reporting, or
error-tracking SDK (searched for `analytics`, `telemetry`, `sentry`,
`bugsnag`, `segment`, `mixpanel`, `amplitude`, `rollbar`, `datadog`,
`newrelic`, `honeycomb`, `posthog`, `crashlytics`, `raygun`, `honeybadger` —
zero matches). Every dependency in both tables above is either a data-format
library (canonicalisation, compression, `.deb`/OpenPGP parsing) or a CLI/config
framework. This property is architecturally load-bearing,
not incidental: "no such library in the source
tree at all — its mere presence fails source audits in classified
environments." As of this review, that claim holds, and it is checkable by
anyone with `go.sum` and five minutes, which is the point.

## Licence summary

Every direct and transitive dependency identified above is Apache-2.0, MIT,
BSD-2/3-Clause, or public domain. None is copyleft (GPL/AGPL/LGPL/MPL family).
This is consistent with `.github/workflows/licence-scan.yml`'s stated policy
("fails the build if a runtime Go dependency carries a copyleft licence") and
with Apache-2.0 being debark's own licence (`LICENSE`) — nothing in the
dependency tree constrains that choice.
