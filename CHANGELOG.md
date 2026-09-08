# Changelog

Notable changes to debark are recorded here. Release downloads and notes are
available on [GitHub Releases](https://github.com/inferops/debark/releases).

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Before 1.0, minor releases may include breaking changes. Published data formats
have separate [compatibility rules](CONTRIBUTING.md#public-contracts-and-schemas).

## [Unreleased]

### Changed

- Reorganized the README and user documentation around installation, signed
  bundle workflows, platform setup, CLI reference, and troubleshooting.
- Updated desktop build instructions for the single repository containing both
  Go modules, and clarified desktop versus CLI requirements.
- Refreshed contribution and support guidance, issue forms, and pull request
  expectations; collected validation gaps in a dedicated status page.

## [0.1.0] - 2026-09-08

First release of the CLI, engine, and desktop app. Validation coverage and
remaining gaps are described in [status and known limitations](docs/status.md).

### Added

- Target snapshots capturing installed packages and apt configuration, with
  inspection and optional redaction.
- Built-in baseline OS definitions for Debian 12/13 and Ubuntu
  22.04/24.04/26.04, plus snapshot synthesis and guided CLI workflows.
- Bundle builds using apt in a private root, with automatic selection between
  compatible local apt and a container backend.
- Archive package, pinned version, vendor URL, local `.deb`, and package-list
  inputs; incremental storage and bundle refreshes.
- Flat apt repositories, exact-version locks, manifests, build evidence,
  optional CycloneDX SBOMs, and embedded target binaries.
- Ed25519, GPG, and external-plugin signing; bundle verification before
  installation and exact-version target install plans.
- Inspection, offline-installation diagnostics, local policy evaluation,
  configuration profiles, JSON results, and progress events.
- Desktop package browsing, target selection, bundle building, and copy/export.
- CLI release archives for Linux amd64/arm64 and Windows amd64, Linux
  `.deb`/`.rpm` packages, and desktop release artifacts.
- Separate cosign-signed checksum files for CLI and desktop artifacts,
  release SBOM generation, and reproducible-build checks.
- Linux and Windows CI, apt-backed integration tests, a nightly fixture matrix,
  DCO checks, and dependency license scanning.
- Apache-2.0 license, DCO, governance, security, conduct, trademark, and
  community/commercial boundary policies.

[Unreleased]: https://github.com/inferops/debark/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/inferops/debark/releases/tag/v0.1.0
