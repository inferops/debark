# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0.0 is tagged. Before 1.0.0, minor versions may include breaking
changes; the `api/` schemas are frozen regardless (see
[CONTRIBUTING.md](CONTRIBUTING.md)).

## [Unreleased]

Nothing yet.

## [0.1.0] - 2026-09-08

The first tagged release. This project is under active development: the
public schemas and package interfaces are frozen and compile, and work
continues behind them. Pre-1.0, a minor version may include breaking changes;
the `api/` schemas are frozen regardless.

**This file is behind the implementation, and reading it as a summary of what
exists would mislead you.** It was written during the repository-foundations
work and records that only: the entries in 0.1.0 are about CI, licensing
and release engineering, and not one of `snapshot`, `build`, `verify` or
`install` — all of which are implemented and run end to end — appears in it.
Backfilling it is a job in its own right, and adding features to it one at a
time in the meantime would be worse than leaving it plainly stale, because
whichever feature went in first would read as the project's headline.

Until that backfill happens, [the README's Status and known gaps
section](README.md#status-and-known-gaps) is the authority on what works and
what does not. It is treated as load-bearing in this project, and it is kept
current.

### Added

- Repository foundations: licence, governance, security, trademark and
  free/paid policy documents.
- CI: lint, build, vet, unit test matrix (Linux and Windows; macOS is
  deliberately absent), Debian/Ubuntu container test job, determinism check,
  `gofmt` check, DCO check, dependency licence scan.
- Nightly integration matrix workflow, which builds and runs `hack/matrix`
  over the fixture matrix and publishes its JSON result.
- Release engineering: `goreleaser` configuration (`linux_amd64`,
  `linux_arm64` and `windows_amd64` archives plus `.deb`/`.rpm` packages,
  cosign keyless signing of `debark_checksums.txt`, SBOM, `nfpm` packaging),
  reproducible-build check script run as a release gate.
- `Makefile` with `build`, `test`, `test-linux`, `lint`, `fmt`, `vet`, `cover`,
  `man`, `completions`, `snapshot`, `clean` targets.

### Changed

- Nothing yet.

### Deprecated

- Nothing yet.

### Removed

- Nothing yet.

### Fixed

- Nothing yet.

### Security

- Nothing yet.

[Unreleased]: https://github.com/inferops/debark/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/inferops/debark/releases/tag/v0.1.0
