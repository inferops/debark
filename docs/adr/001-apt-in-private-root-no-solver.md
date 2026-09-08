# ADR-001: apt in a private root is the sole resolver; no Go dependency solver

**Status:** Accepted — 2026-09-03
**Design reference:** the do-not-build list, principle 1, decision D2

## Context

DR1 was unanimous: debark's real differentiators require a *correct*
dependency closure across an air gap, and any independent solver risks
producing exactly the failure this project exists to fix — a bundle that
resolves online but fails offline (the description of the
`apt-get install --download-only` problem, and the kill criterion "zero
bundles that resolve online but fail offline"). Debian/Ubuntu dependency
semantics — alternatives, virtual packages/`Provides`, versioned relations,
architecture qualifiers, essential/`Pre-Depends` ordering, phased updates —
are large, change over time (new solver behaviour ships with new apt
releases), and are already correctly implemented by apt itself.

## Decision

Every dependency decision is made by the target release's own `apt-get`,
invoked as a subprocess against a private, disposable apt root
(`ROOT/etc/apt/...`, `ROOT/var/lib/apt/lists`, the target's own captured
`/var/lib/dpkg/status`) constructed from the snapshot. Go's job is to
orchestrate that root, parse apt's own output (`-s`/`--print-uris`), fetch
what apt selected, index, sign, verify and report. debark never
re-derives, second-guesses, or "fixes up" what apt decided.

## Consequences

- **Accepted happily.** Correctness tracks apt's own correctness.
  Debian/Ubuntu dependency-semantics churn — new relation types, phasing,
  `solver3` — becomes apt's maintenance burden, not debark's. The Bash
  prototype's exact private-root invocation pattern is already proven end to
  end.
- **Accepted unhappily.** The resolution side needs a *working* apt always —
  either the host's own (ADR-002, when it matches the target) or a
  container's. There is no "no apt available, fall back to something
  simpler" path; an environment with neither hard-fails rather than
  degrading to a lesser guarantee.
- The resolving apt's own version can still diverge from the target's apt's
  behaviour — mitigated, not eliminated, by ADR-002's mismatch
  detection.

## Alternatives considered

- **A pure-Go or embedded dependency solver**, reimplementing or vendoring
  an approximation of apt's algorithm. Rejected: this is the project's own
  permanent do-not-build item #1 — "wrong bundles that fail on the
  far side of the gap; unbounded maintenance. Existential risk." Every
  research round converged independently on shelling out to apt as the only
  viable design (D2).
- **A second, independent solver used only to validate apt's answer.**
  Rejected as complexity with no correctness benefit: apt's answer is
  authoritative by construction (principle 1); a second solver could
  only ever disagree, never adjudicate, and a disagreement would leave the
  operator no better informed about which answer to trust.
