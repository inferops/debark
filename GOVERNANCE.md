# Governance

## Where the project is today

debark is pre-1.0 and maintained through the
[inferops/debark repository](https://github.com/inferops/debark).
The founding maintainer group handles reviews, releases, and project decisions.
There is no separate public maintainer roster yet. To reach the maintainers,
follow [SUPPORT.md](SUPPORT.md); use [SECURITY.md](SECURITY.md) for private
vulnerability reports.

This document describes the model the project runs under now and the
path by which it grows, so that expectations are set honestly rather than
implying a foundation-scale process that does not exist yet.

## Maintainer model

- **Maintainers** have merge rights and are collectively responsible for the
  project's direction, releases, and enforcement of this document, the
  [Code of Conduct](CODE_OF_CONDUCT.md), and the [trademark policy](TRADEMARK.md).
- New maintainers are added by existing maintainer consensus, based on a
  sustained record of good contributions (code, review, docs, triage, or
  design) and demonstrated judgment about the project's scope — see
  "the do-not-build list" below.
- Maintainers can step back at any time; the project does not ask for a
  commitment beyond what someone chooses to give.
- There is no separate "core team" versus "committers" tier yet. If the
  project grows enough that package-level ownership needs to be formalised
  beyond the areas in `docs/dev/contract-brief.md`, that will be written
  down here and likely encoded as `CODEOWNERS`, not invented ad hoc.

## Decision process

- **Routine changes** (bug fixes, docs, tests, non-breaking additions within
  an existing area's ownership) merge on one maintainer's review under
  normal lazy consensus: opened, reviewed, no sustained objection, merged.
- **Changes to a frozen contract** — anything under `api/`, the frozen files
  listed in `docs/dev/contract-brief.md`, or a published JSON Schema — require
  an ADR (see `docs/adr/`) explaining why the freeze is being broken, and
  maintainer consensus, not a single approval. Schemas are versioned;
  breaking a schema means a new version, not a silent change to `v1`.
- **Scope decisions** (does debark do X) are resolved by checking against
  the do-not-build list below first, before any other discussion.
- **Disagreements among maintainers** are resolved by discussion aimed at
  consensus. If consensus genuinely cannot be reached on a maintainer-level
  decision, the founding maintainer(s) make the call, explain the reasoning
  in the relevant issue or ADR, and the project moves on. This project is
  small enough that formal voting procedures would be theatre; that changes
  if and when the contributor base does.

## The do-not-build list is product definition, not backlog

The project's permanent do-not-build list (a pure-Go apt solver; Snap/
Flatpak/pip/npm/cargo/OCI/Helm orchestration; a mirror manager; a daemon or
server in the community binary; a GUI or full-screen TUI as the primary
interface; a self-updater; telemetry or crash-reporting uploads; licence
checks or metering in the community binary; a general CVE scanner or licence
adjudicator) is treated as **settled product definition**, on the same
footing as a frozen schema — not as a backlog of features someone might get
to. A pull request implementing an item on that list will be closed with a
link to this section, regardless of how well it is written.

The reasoning matters more than the list itself: each entry exists because
building it would either (a) create existential correctness or maintenance
risk — a Go dependency solver producing bundles that resolve online and fail
offline is the named catastrophic failure mode — or (b) expand scope into a
domain with its own trust semantics and its own better-suited tools, which
would dilute debark's actual differentiator (apt-based air-gap transfer,
done correctly) into something worse at everything. A maintainer who wants to
change this list is proposing to change what the project *is*; that requires
an ADR and explicit, recorded maintainer consensus, not a routine merge. The
scope ladder shows what happens above debark's line (organisational fleet
features, live outside this repository, in a separate commercial codebase)
and beside it (mirror management, multi-ecosystem transfer, removable-media
security kiosks — other tools' jobs, permanently).

The optional [desktop app](gui/README.md) is an online-builder companion. It
does not replace the CLI as the complete interface or add GUI dependencies
to the engine module.

## The free/paid boundary is protected as project policy, not a business lever

[`docs/free-paid-policy.md`](docs/free-paid-policy.md) publishes the rule
verbatim: everything needed to securely prepare, understand, transfer, verify
and install a bundle is free forever, and two things are permanently refused
— quantity caps and gating anything security-relevant. This is not a
marketing position that a future maintainer or a future employer of a
maintainer can quietly narrow. Concretely:

- A pull request, from anyone, including a maintainer, that adds a licence
  check, usage cap, feature flag gated on payment, or phone-home
  entitlement check to code under `core/`, `internal/`, `cmd/`, or `api/` is
  out of scope by definition (see `docs/dev/contract-brief.md`'s
  non-negotiable rules) and will not be merged, independent of code quality.
- Any commercial edition is a **separate codebase** consuming the same
  public interfaces (`Signer`, `Policy`, `EventSink`, the plugin protocol). It is never a build flag, a crippled binary, or a
  fork-and-relicense of this repository. The licence on this repository
  (Apache-2.0) does not change; see
  [LICENSE](LICENSE) and [TRADEMARK.md](TRADEMARK.md) for how the name is
  protected instead of the code.
- Changing the free/paid line itself is a governance-level decision, not an
  engineering one: it requires updating `docs/free-paid-policy.md` explicitly,
  with the reasoning recorded, following the same consensus bar as a scope
  change above — never a side effect of an unrelated change.

## Code of Conduct and trademark

Enforcement of the [Code of Conduct](CODE_OF_CONDUCT.md) and the
[trademark policy](TRADEMARK.md) is a maintainer responsibility; see those
documents for process and [SECURITY.md](SECURITY.md) for how to reach
maintainers privately.

## Changing this document

Governance changes follow the same consensus bar as a scope decision: open a
pull request against this file, explain the reasoning, and get maintainer
consensus. As the maintainer group grows beyond its founding size, expect
this document to grow with it — including, eventually, a documented process
for removing an inactive or unresponsive maintainer — rather than being
rewritten from a blank page.
