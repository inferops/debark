# Trademark policy

"debark" and the debark logo (once one exists) are trademarks associated
with this project. This policy exists so the community can build on, discuss,
package and fork debark freely, while the name keeps meaning something
specific: that a given binary or service actually is what it claims to be.

The trademark policy is separate from the [LICENSE](LICENSE). Apache-2.0
gives you broad rights to the code, including the right to fork and modify
it. It does not give you the right to call your fork "debark" — Apache-2.0
§6 says so explicitly, and this document says what that means in practice.

## Permitted without asking

- **Factual reference.** Saying your product, script, blog post, talk or
  package "uses debark", "wraps debark", or "was built with debark" is
  fine, including in comparisons, as long as it is accurate and does not
  imply endorsement you do not have.
- **Unmodified redistribution of official releases.** Mirroring, packaging or
  reshipping an unmodified official release — including distro packaging
  (Debian, Ubuntu, Homebrew) — under the name "debark" is fine.
- **"Works with debark" statements.** Plugins, wrappers, CI actions and
  integrations may describe themselves as working with debark.
- **Community discussion and coverage.** Forums, articles, talks and
  packaging discussions may use the name freely.

## Requires a distinct name and logo

- **Forks with modifications.** If you change the code and redistribute it,
  give your fork its own name and logo. Say clearly that it is a fork of
  debark and link back to the upstream project — that is encouraged — but
  do not ship it, or its packages, as "debark".
- **Commercial editions or services not operated by this project.** A
  hosted service, support offering, or commercial build that is not produced
  by the debark project itself needs its own name. You may truthfully say
  it is "compatible with" or "built on" debark.
- **Any use that could suggest official status or endorsement** the project
  has not actually given (badges, "official", "certified", "verified"
  claims), even without modifying the code.

## Reserved to the project

- The name **"debark"** as a product/project identifier.
- **The debark logo**, once adopted.
- **"official debark"** / **"official debark binaries"** — reserved for
  binaries produced by this project's own release pipeline (see
  [`.github/workflows/release.yml`](.github/workflows/release.yml) and
  `core/version` `Edition=official`; see ADR-010).
- **"debark Enterprise"** — reserved for the commercial edition described in
  [`docs/free-paid-policy.md`](docs/free-paid-policy.md), if and when one
  ships. No such product exists today.

## Naming layers

| Layer | What it means |
|---|---|
| *debark* | The open-source project: this repository, its community, its Apache-2.0 code. |
| *official debark binaries* | Artefacts produced by this project's own release process, reproducibly built, checksummed and cosign-signed. Anyone can build the same source themselves; only this project's pipeline gets to call its output "official". |
| *debark Enterprise* | The commercial edition (not yet built), a separate codebase built on the same open interfaces, sold as described in `docs/free-paid-policy.md`. |
| Forks | Free to exist under Apache-2.0. Must rename. |

## Status of registration

No trademark application has been filed yet, and the name has not yet been
formally cleared: the project's naming-availability research was carried out
against an earlier working name and does not carry over to `debark`. That
clearance has to be re-run — GitHub org and repo, Debian source and binary
package names, Homebrew formula, language registries, trademark registers,
domains and social handles — before the name is relied on.

This document describes the policy the project intends to operate under
regardless of registration status; a granted registration would strengthen
enforcement, not change the rules above.

## Questions or a request to use the name in a way not covered here

Open a GitHub issue against this repository, or see
[GOVERNANCE.md](GOVERNANCE.md) for how to reach the maintainers. This policy
is deliberately permissive by design, because forks and factual reference are how a community-owned name stays
trusted; when in doubt, the answer is usually "yes, with attribution."
