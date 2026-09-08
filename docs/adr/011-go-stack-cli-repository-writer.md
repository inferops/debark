# ADR-011: Go stack — Cobra, koanf, mpb, slog, and a pure-Go repository writer

**Status:** Accepted — 2026-09-03
**Design reference:** the do-not-build list, principle 7, Appendix G

## Context

Several independent tooling choices needed to be made for the CLI layer and
the repository writer, each with a documented alternative the research
rounds disagreed on (Appendix G): CLI framework (Cobra vs. Kong), progress
rendering (mpb vs. a full Bubble Tea/bubbles TUI), and how to generate apt's
`Packages`/`Release` index files (a pure-Go writer vs. shelling out to
`apt-ftparchive`, the prototype's own approach).

## Decision

| Concern | Choice | Why |
|---|---|---|
| CLI framework | `spf13/cobra` | Completions, `did-you-mean`, man-page generation for Debian packaging, broad contributor familiarity |
| Config | `knadh/koanf` | Small, explicit flag > env > file precedence, no hidden magic |
| Progress | `vbauerster/mpb` | Multi-transfer bars that degrade to plain lines on a non-TTY, CI log, or transcripted SSH session |
| Styling | `charmbracelet/lipgloss` (light use) | Colour and table formatting only, respects `NO_COLOR` |
| Logging | `log/slog` | Zero-dependency structured logs; `core/evidence` is the audit trail, not the log stream |
| Repository indexing | Pure-Go writer over `pault.ag/go/debian` primitives | Deterministic, dependency-free, works inside a slim container image with no `apt-ftparchive` binary present |

## Consequences

- **Accepted happily.** No `.deb`/apt tooling needs to exist on the machine
  that indexes a repository — a real requirement once the design commits to
  slim container images and Windows/macOS-hosted `build` runs whose
  fetch/index/sign/assemble steps are "pure Go... all platforms". A
  hand-rolled indexer is also fully under debark's own determinism control
  (principle 2), where `apt-ftparchive`'s own output ordering and formatting
  choices are not.
- **Accepted unhappily.** A pure-Go index writer must independently track
  apt's `Packages`/`Release` field requirements (MD5/SHA1/SHA256,
  `Filename`, sizes, section/priority fields) rather than delegating that
  surface to apt's own already-correct implementation — real, ongoing
  maintenance surface that `apt-ftparchive` would have avoided, accepted
  because determinism and slim-container compatibility outweigh it.
- Cobra and koanf are both larger dependencies than a hand-rolled flag
  parser would be; accepted for the completions/man-page/precedence
  functionality they provide essentially for free, and both are
  already-vetted, widely-used, permissively-licensed choices.

## Alternatives considered

- **`alecthomas/kong` for the CLI** (Appendix G). Rejected in favour of
  Cobra specifically for shell completion generation, man-page generation
  (needed for Debian packaging), and the larger existing pool of
  contributors already familiar with Cobra's command-tree conventions.
- **`charmbracelet/bubbles`/Bubble Tea as the primary progress/output
  surface** (Appendix G). Rejected: a full-screen TUI is explicitly on the
  permanent do-not-build list — "must work over SSH, serial
  consoles, CI logs, transcripted sessions... progress bars that degrade to
  lines are enough." mpb's line-degradation behaviour is the actual
  requirement; a TUI framework would need to be worked around to get it,
  not embraced for it.
- **Continue shelling out to `apt-ftparchive`** (the prototype's own
  approach) for repository indexing. Rejected: not guaranteed present in
  slim images, its determinism is not under this project's control, and a
  pure-Go writer using `pault.ag/go/debian`'s already-correct control-file
  parsing primitives closes the gap at an acceptable, bounded cost ("Works in
  slim containers; deterministic; fast").
