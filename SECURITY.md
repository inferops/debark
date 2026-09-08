# Security policy

debark moves software across an air gap and verifies it before installing.
Its own security posture matters as much as the guarantees it makes about the
bundles it produces. This document is the disclosure process, plus a
high-level in/out of scope summary. The full adversarial analysis lives in
[`docs/threat-model.md`](docs/threat-model.md) — assets, trust boundaries,
each adversary considered, the attacks that must fail and exactly where they
fail, what debark explicitly does not protect against, residual risks, and
a cryptographic inventory. Read that document for the real analysis; this
file is deliberately shorter. [`docs/security-model.md`](docs/security-model.md)
is the plain-language version of the trust chain, and
[`docs/security/verification-guide.md`](docs/security/verification-guide.md)
walks through verifying a bundle by hand. **All three, like the rest of this
pre-1.0 codebase, are marked with their own implementation status and should
be re-read against current code before you rely on a specific claim** — see
each document's own status note.

## Supported versions

debark has not made a tagged release yet (see
[CHANGELOG.md](CHANGELOG.md)). Until a 1.0 release ships, security fixes land
on the default branch only — there is no older version to backport to. Once
releases exist, this table will list which lines receive security fixes,
matching the platform support the README records
([README.md](README.md#platform-support)).

| Version | Supported |
|---|---|
| `main` (pre-1.0, unreleased) | Yes — this is the only line that exists |

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Report privately using GitHub's built-in mechanism for this repository:
**Security tab -> Report a vulnerability** (GitHub Security Advisories). This
creates a private discussion visible only to you and the maintainers, and
lets us coordinate a fix and a disclosure date before anything is public.

If you cannot use GitHub's private reporting for some reason, open a regular
issue asking a maintainer to contact you through another channel, without
describing the vulnerability itself.

We do not yet have a dedicated security contact address; if the project
adopts a domain and PGP key, they will be published here (and in a
`security.txt`) rather than assumed in advance.

### What to include

- The version or commit you tested (`debark version --json` once that
  command is implemented; otherwise the commit hash).
- What you expected versus what happened, and the smallest reproduction you
  have — a snapshot/bundle/lock/manifest fixture is ideal, since debark's
  correctness claims are about exactly those artefacts.
- Your assessment of impact, if you have one. We would rather triage a
  false alarm than miss a real one.

### What to expect

- Acknowledgement as soon as a maintainer sees the report; this is currently
  a small, pre-1.0 project, so please allow for that rather than assuming a
  round-the-clock SLA.
- A best-effort fix and coordinated disclosure once the report is confirmed.
  Credit in the release notes and `CHANGELOG.md`, if you want it.

## What is in scope

At a high level — see [`docs/threat-model.md`](docs/threat-model.md) §§1-4
for the real, cited version:

- The **build path**: anything that could make a bundle (repository, lock
  plan, manifest, signature) misrepresent what it actually contains, or make
  a build silently drop or substitute a package.
- The **verify/install path**: anything that lets `verify` accept a tampered
  bundle, or lets `install` reach apt/dpkg before verification has succeeded.
  The rule is "verify before apt ever sees the repository".
  This is the property the tool exists to guarantee; a break here is
  critical by definition.
- The **trust model**: signature handling (`core/sign`, `core/verify`),
  digest computation (`core/digest`), canonicalisation (`core/canonical`),
  and anything that could cause a key or fingerprint to be trusted that
  should not be.
- **Supply-chain integrity of the project's own releases**: the
  `goreleaser`/cosign/SBOM pipeline in `.github/workflows/release.yml` and
  reproducibility of the build (`hack/reproducible-check.sh`).
- **Redistribution and provenance correctness** — `doctor` heuristics and
  redistribution warnings being silently wrong in the unsafe direction
  (saying something is fine when it is not).

## What is explicitly out of scope for this project

(See also [`docs/threat-model.md`](docs/threat-model.md) §5, "What debark
explicitly does NOT protect against," and §6, "Residual risks and operator
responsibilities," for the fuller, cited list.)

- Vulnerabilities in `apt`, `dpkg`, `gpg`, the Debian/Ubuntu archive, or a
  container runtime debark shells out to. Report those to the relevant
  upstream. debark treats apt as the oracle for dependency resolution by
  design (ADR-001) and does not vendor or patch it.
  Note: apt is a separate process debark executes, never a linked library
  — apt's GPL licensing does not attach to the debark binary; see
  `.github/workflows/licence-scan.yml`.
- Vulnerabilities in a specific `.deb` package that debark faithfully
  bundled or installed unmodified. debark never modifies packages
  (ADR-001, ADR-013); a vulnerable package correctly transferred is not a
  debark bug, though `doctor` heuristics that fail to warn about it may be.
- Denial of service against your own machine from running a tool you invoked
  yourself with attacker-controlled input you chose to trust (the standard
  caveat for a CLI tool that reads local files and network URLs you give
  it) — unless it demonstrates a verification bypass.

## The promise this project makes

**No security capability is ever paywalled.** Signature verification, digest
checking, provenance and SBOM generation for a single bundle, and every other
capability needed to know whether an artifact is authentic are permanently
part of the free, Apache-2.0 community edition — see
[`docs/free-paid-policy.md`](docs/free-paid-policy.md)
Commercial editions may add central *enforcement*, *aggregation* and
*retention* of security data across a fleet; they will never add security
functionality withheld from a single-bundle user. A vulnerability report is
never an opportunity to justify a paid capability — a security fix is always
free.

## No telemetry

debark has no telemetry, crash reporting, or update check, in any edition,
ever. Nothing about your usage, snapshots or
bundles leaves your machine except the network calls you explicitly make
(archive mirrors, vendor URLs, a container registry) — see
[CONTRIBUTING.md](CONTRIBUTING.md) for how this is enforced in review.
