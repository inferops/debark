# Security policy

debark prepares software for offline installation and verifies bundles before
passing their repositories to apt. This policy explains how to report a
vulnerability and which parts of the project are in scope.

For technical details, see the [security model](docs/security-model.md),
[threat model](docs/threat-model.md), and
[independent verification guide](docs/security/verification-guide.md).
[Known limitations](docs/status.md) records validation coverage.

## Supported versions

During pre-1.0 development, security fixes land on the default branch and ship
in the next release. Superseded pre-1.0 releases do not receive separate
backports. See [CHANGELOG.md](CHANGELOG.md) for release history.

| Line | Security maintenance |
| --- | --- |
| `main` (pre-1.0) | Fixes land here and ship in the next tag |
| Superseded pre-1.0 releases | No separate backport line |

## Report a vulnerability privately

Use [GitHub private vulnerability reporting](https://github.com/inferops/debark/security/advisories/new)
or **Security → Report a vulnerability** in this repository.
Please do not disclose vulnerability details in a public issue.

If private reporting is unavailable, open a public issue asking for a private
contact channel **without including the vulnerability details**. The project
does not currently publish a dedicated security email address.

Include:

- Output of `debark version --json`, or the tested source revision.
- The affected component: CLI, engine, desktop app, or release workflow.
- A minimal reproduction, expected behavior, and actual result.
- Your assessment of impact, if available.
- Whether you would like public credit when the fix is disclosed.

Use a synthetic fixture where possible. Even in a private report, omit unrelated
secrets, private signing keys, and sensitive machine inventory.

## Response and disclosure

Maintainers acknowledge and investigate reports on a best-effort basis.
There is no round-the-clock response commitment. For confirmed reports, the
project coordinates a fix and disclosure date with the reporter and includes
credit in release notes when requested.

## In scope

- **Build correctness:** unintended package omission or substitution, or a
  repository, lock, manifest, or signature that misrepresents its contents.
- **Verification and installation:** accepting a tampered bundle, bypassing
  required trust checks, or exposing an unverified repository to apt/dpkg.
- **Trust handling:** signatures, key selection, digests, canonicalization,
  and archive provenance.
- **Desktop security:** unsafe handling of package metadata, file paths,
  frontend bindings, or CLI invocation.
- **Release integrity:** build, packaging, signing, and reproducibility
  failures in the project's release pipeline.
- **Misleading provenance or redistribution findings:** reporting a stronger
  claim than the evidence supports.

## Outside the project's scope

Vulnerabilities in apt, dpkg, GPG, container runtimes, or upstream repositories
should also be reported to their maintainers. Report a debark integration flaw
here if its use of those components creates a vulnerability.

A vulnerable `.deb` faithfully transferred without modification is not by
itself a debark vulnerability. A bundle signature authenticates the covered
content; it does not certify that the packaged software is safe. Incorrect
claims made by debark about that content remain in scope.

The existing threat model excludes denial of service against an operator's own
machine from explicitly trusted attacker-controlled input unless it demonstrates
a verification bypass. See the [threat model](docs/threat-model.md) for the
full trust boundaries, exclusions, and residual risks.

## Community security and privacy

Security fixes and capabilities needed to authenticate a bundle remain part of
the Apache-2.0 community project. See the [free/paid policy](docs/free-paid-policy.md).

debark has no telemetry, analytics, crash-reporting uploads, or update checks.
Online operations contact the archives, vendor URLs, and container registries
needed for the requested work. Snapshots and metadata can still contain sensitive
information; review them before sharing.
