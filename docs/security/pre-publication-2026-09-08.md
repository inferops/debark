# Pre-publication review — 2026-09-08

Reviewed the CLI, engine, desktop module, repository history, contributor
documentation, and GitHub workflows from `a3d7c14`, then made the changes below.
This is a focused engineering review with executable regression coverage,
not a claim that every possible defect has been excluded.

## Fixed findings

| Area | Previous behavior | Change and evidence |
| --- | --- | --- |
| Desktop export: directory aliases | A destination symlink could point back into the source. Opening the destination truncated the source file. | Resolve directory aliases before comparing paths. Linux regression tests reproduced both same-directory and nested-directory aliases. |
| Desktop export: existing links | Destination symlinks and hardlinks could overwrite unrelated files or source data, including through the incomplete marker. | Refuse destination symlinks and replace regular-file directory entries using exclusive creation instead of truncating shared inodes. Regression coverage includes external hardlinks and file/directory/marker symlinks. |
| Desktop export: stale contents | Copy verification could succeed while files from an older bundle remained, causing offline bundle verification to fail. | Refuse unexpected destination entries before planning/copying. Recheck before Run. Existing interrupted copies of the same bundle still pass. |
| Signing key generation | An existing public-key path left a new, unmatched private key after failure. Multiline comments generated unreadable keys. | Remove this attempt's private key when public-key creation fails, reject multiline comments, and check flush/close errors. Tests verify existing public data survives and retry succeeds. |
| DCO workflow | A failed `git rev-list` inside process substitution could become a successful empty check. A sign-off quoted in a message body was accepted. | Check history errors directly and inspect parsed Git trailers. A temporary-repository harness covers signed, unsigned, unavailable-history, and quoted-sign-off cases. |
| Air-gap demo | `DEBARK_DEMO_OUT` was recursively deleted, and a custom destination did not receive the binary compiled into `.demo/debark`. | Use a fresh directory by default, refuse nonempty custom destinations, and move the build output into the selected directory. |

Export still assumes the source and destination are not concurrently replaced
by another process during copying. The destination preflight is not a general
filesystem sandbox against a hostile local process racing directory changes.

## Dependencies and automated checks

The original Windows scan reported 20 affected vulnerabilities in the root
module's analyzed code, including standard-library findings. The updated
Linux and Windows scans of both modules report **0 affected vulnerabilities
and 0 findings in imported packages**.

| Component | Previous pin | New pin |
| --- | --- | --- |
| Go, both modules | 1.26.0 | 1.26.8 |
| `github.com/cloudflare/circl` | v1.6.2 | v1.6.3 |
| `github.com/klauspost/compress` | v1.18.0 | v1.18.7 |
| `golang.org/x/crypto` | v0.41.0 | v0.56.0 |
| `golang.org/x/text` | v0.28.0 | v0.41.0 |
| `golang.org/x/net`, desktop | v0.42.0 | v0.57.0 |

The Go patch line includes the security fixes recorded in the
[Go release history](https://go.dev/doc/devel/release#go1.26.0).
The CIRCL update addresses
[GO-2026-4550](https://pkg.go.dev/vuln/GO-2026-4550).
The Go supplementary-module versions include security fixes and satisfy their
shared minimum-version requirements. No new runtime dependency was introduced.
Both module files were tidied; unused manpage-library requirements were removed.
The updated modules' top-level license files were checked in the module cache.

One module-level advisory remains:
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), about the deprecated
`golang.org/x/crypto/openpgp` package. Debark does not import that package; its
snapshot parser imports `github.com/ProtonMail/go-crypto/openpgp`. The scanner
does not report an affected imported package or call path for this advisory.

`vulnerabilities.yml` now scans both modules on Linux and Windows for pushes,
pull requests, weekly schedules, and manual dispatches. It uses the
`GOVULNCHECK_VERSION` pin in the root Makefile. `make vulncheck` runs the root
scan locally. Scanning contacts the public Go vulnerability database; no
runtime application networking behavior was added.

## Validation

Follow-up: the first pushed CI run exposed a Windows-only test assumption in
the marker-path assertion. The local run used matching path spelling; the
runner's resolved path differed from its temporary-directory spelling. The
failure was reproduced with a differently cased Windows temp path. The test
now checks directory identity with `os.SameFile`, and Windows coverage exports
through case aliases into both existing and new destinations.

- Full portable unit suites passed for both modules on Windows and Linux;
  the Linux desktop run used `desktop,production,webkit2_41`.
- Build, vet, and formatting checks passed in a source-only review copy,
  excluding the developer's ignored binaries, cached frontend, and scratch Go
  programs. Desktop assets were generated from their committed sources.
- The configured golangci-lint checks passed for both modules with zero issues.
- Linux race-detector checks passed for desktop export and application state.
- govulncheck passed for both modules on Linux and Windows, with the
  module-only advisory described above.
- All GitHub workflows passed actionlint. The DCO regression harness passed.
- The complete air-gap demo built and signed a Debian 12 bundle, verified it,
  and installed three packages in a container with `--network none`. The
  installed `jq` printed `jq-1.6` and successfully processed the sample input.
- The demo refused a nonempty custom output directory and preserved its
  sentinel file. Changed shell scripts passed ShellCheck.
- `go mod tidy -diff`, module checksum verification, and `git diff --check`
  were checked as part of dependency and source cleanup.

The full release/architecture matrix, arm64 execution, physical USB behavior,
and signed release packaging were not rerun by this review. Existing broader
validation limits remain recorded in [status](../status.md).

## Publication and repository hygiene

Pattern-based credential checks across reachable local Git history found
synthetic redaction-test values, with no identified live credentials. Both
committed target-state archives were inspected for sensitive paths and
credential patterns; neither produced a match. This is not an exhaustive
secret-detection guarantee. The fixtures still intentionally disclose their
documented OS/package inventories.

Common private-key files, local environment files, bundle outputs, and editor
artifacts are now ignored. Ignore rules do not remove anything from history.
The local `master` and `pre-public-history` branches retain older development
history, including generated binaries; `main` and `v0.1.0` use the prepared
public history. Push intended branches/tags explicitly rather than publishing
all local refs with `--all` or `--mirror`.

GitHub was confirmed **private**, with `main` as the default branch and an
existing published `v0.1.0` release. No visibility, release, tag, or remote branch
was changed by this review. Before presenting the downloads as current, ship a
new patch release built from these fixes and the updated toolchain. Existing
v0.1.0 assets do not acquire source fixes automatically.

After changing visibility, enable private vulnerability reporting and branch
protection/required status checks, including the new vulnerability jobs. The
private-reporting API returned 404 during this review, and the branch-protection
API explicitly required GitHub Pro or public visibility; those settings could
not be confirmed as enabled while the repository remained private.
