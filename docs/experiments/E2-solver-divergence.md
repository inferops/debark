# E2 — Solver divergence

**Status: complete.** Result **qualifies** backend auto-selection and solver-version recording. The divergence
the design worries about is real, confirmed, and can be significant — but the
trigger the design proposes (**apt major.minor mismatch**) is the wrong
granularity. In every case tested, divergence tracked the **effective solver
algorithm** (`APT::Solver`) specifically, not apt version: two different apt
versions running the *same* solver never diverged; the *same* apt binary
running two different solvers did, repeatedly and predictably.

## Question

Resolve the same request with Ubuntu 24.04's apt and with a newer apt
(25.10/26.04, which ship `solver3`), for fixtures involving alternatives
(`Depends: a | b`), virtual packages/`Provides`, and a package with several
satisfiable versions. Do the two apts choose the same closure? Where they
differ, how? Does the "host apt major.minor == target apt
major.minor" threshold actually track where divergence appears, or does it
appear/disappear at a different granularity — specifically the solver
algorithm?

Settles backend auto-selection and solver-version divergence.

## Method

Script: [`hack/experiments/e2-solver-divergence.sh`](../../hack/experiments/e2-solver-divergence.sh)
(rerunnable — re-pulls and re-inspects the images, rebuilds the fixture repo,
and reruns the whole comparison matrix from scratch every time; nothing about
apt's current behaviour is hard-coded).

1. **Images** (re-pulled and re-inspected the run this document reports,
   2026-09-03T21:41Z):

   | image | digest | apt | codename |
   |---|---|---|---|
   | `ubuntu:24.04` | `sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517` | 2.8.3 | noble |
   | `ubuntu:25.10` | `sha256:7cc5e35f6567ee8c66d2abb4aab0fd866669e6207c237c3a8f0947a5c7f17092` | 3.1.6ubuntu2 | questing |
   | `ubuntu:26.04` | `sha256:2260313b31c8c011cd2eebe728008efac1b3982be73eb71348ea2648d2c0e09b` | 3.2.0 | resolute |

   `apt-config dump | grep -i solv`: 24.04 has **no `APT::Solver` key at
   all** (predates it). 25.10 has `APT::Solver ""` globally but
   `binary::apt::APT::Solver "3.0"` / `binary::apt-get::APT::Solver "3.0"` —
   solver3 is the effective default for both front-ends. 26.04 has a blanket
   `APT::Solver "3.0"`. `/usr/lib/apt/solvers/` on all three contains only a
   `dump` helper — solver3 is compiled into `libapt-pkg` itself, there is no
   external solver binary to swap.

2. **Solver-activation probe** (`apt-get -o APT::Solver=<value> install -s -y
   bash`): on 24.04, `APT::Solver=3.0` is a **hard error** —
   `E: Can't call external solver '3.0' as it is not in a configured
   directory!` — not a silently-ignored unknown key; apt 2.8.3 parses
   `APT::Solver` fine, it just tries (and fails) to exec an external solver
   binary named "3.0". `APT::Solver=internal` works identically to no
   override at all, on all three releases. This matters methodologically:
   **`internal` is a safe, portable "force the classic resolver" sentinel;
   the string `"3.0"` is not a safe universal probe for solver3 support** —
   a version-detection scheme that just tries the flag and checks for an
   "unknown key" error would misdiagnose this case.

3. **Fixtures**, built fresh each run as a local flat apt repository (same
   `apt-ftparchive packages`/`release` pattern as `download-packages.sh`
   section 6, 21 `.deb` files total). Most are `equivs-build` placeholder
   `.deb`s (control metadata only, no payload) so the dependency shape is
   fully controlled; one is a real Debian/Ubuntu package relationship:

   | id | shape | mechanics |
   |---|---|---|
   | A (`alt-fwd`/`alt-rev`) | `Depends: liba \| libb` and the reverse | both sides trivially installable, no other constraints |
   | B (`mta`) | `Depends: mail-transport-agent` | **real** virtual package, real archive: 9 real providers on noble (postfix, exim4-daemon-{light,heavy}, ssmtp, sendmail-bin, opensmtpd, nullmailer, msmtp-mta, esmtp-run, dma), 10 on questing/resolute (+courier-mta) — confirmed via `apt-cache showpkg mail-transport-agent`'s Reverse-Provides section on all 3 releases |
   | B2 (`virt`) | `Depends: zz-e2-virt` | 3 synthetic, otherwise-identical providers (`zz-e2-pv-{alpha,mike,zulu}`); apt-ftparchive's directory-scan order forced (via a numeric filename prefix — apt-ftparchive sorts by **filename**, not the control `Package:` field, confirmed by inspecting the generated `Packages` file) to `zulu, mike, alpha` — the **reverse** of alphabetical package-name order |
   | B3 (`virt-nat`) | `Depends: zz-e2-virt-nat` | the same idea, 3 more synthetic providers (`zz-e2-pvn-{alpha,mike,zulu}`), but left under their natural equivs filenames, so scan order and alphabetical-name order **agree** (both `alpha, mike, zulu`) instead of being opposed as in B2 — added specifically to turn B2's single confounded data point into a real two-point comparison (see Finding) |
   | C1 (`multiclean`) | `Depends: multi-clean (>= 1.0)` | 3 fully-installable versions (1.0/1.1/2.0), trivial "pick highest" baseline |
   | C2 (`multibroken`) | `Depends: multi-broken (>= 1.0)` | same shape, but version 2.0 (the one both resolvers try first) `Depends` on a package that exists nowhere in the scenario — forces a choice between failing outright and backing off to the still-satisfying 1.1 |

4. **Matrix**: every fixture resolved with `apt-get install -s` (simulate
   only — no real download ever happens) inside a private apt root built
   exactly (explicit `Dir::*` options), under 6 rows: apt
   2.8.3 (24.04, the only solver it has); apt 3.1.6ubuntu2 (25.10) default
   and `-o APT::Solver=internal`; apt 3.2.0 (26.04) default and `-o
   APT::Solver=internal`. The base dpkg status for every private root is
   that **container's own real** `/var/lib/dpkg/status` — a stock
   `ubuntu:*.*` image's own status file is already realistic and
   dependency-consistent, so (unlike E1, which needed one specific
   package/version state and had to do a real install first) nothing needs
   installing before copying it.

   This gives four pairwise comparisons per fixture, each isolating a
   different variable:
   - **version+solver together**: 24.04-default vs 26.04-default (both vary)
   - **version-only** (same solver3): 25.10-default vs 26.04-default
   - **solver-only** (same apt binary): 26.04-default vs 26.04-internal, and
     independently 25.10-default vs 25.10-internal

## Raw evidence

Full transcript: `hack/experiments/out/e2/e2-run.log` (also
`out/e2/summary.txt`); per-row logs under `out/e2/runs/{2404,2510,2604}/`.
The script's own automated diff reports **11 of 28** pairwise comparisons as
textually different — but a plain string-diff over-counts (below); the real
count of comparisons where a **different provider/package was actually
chosen** is 6, all of them in exactly the two fixtures that ever diverge, and
all 6 fall on a "solver differs" comparison — never on "version differs,
solver same." That pattern, not the raw 11, is the finding.

**A (alt-fwd/alt-rev)** — identical on all 6 rows: the **first-listed**
alternative wins every time (`liba` for fwd, `libb` for rev), on every apt
version and every solver setting. No divergence anywhere.

**B (`mta`, real `mail-transport-agent`)** — the clearest real-world
divergence:

```text
apt 2.8.3        (24.04, only option)            -> postfix
apt 3.1.6ubuntu2  (25.10) default (solver3)        -> courier-mta + 5 extras (courier-authdaemon,
                                                       courier-authlib, courier-authlib-userdb,
                                                       courier-base, libcourier-unicode8)
apt 3.1.6ubuntu2  (25.10) -o APT::Solver=internal  -> postfix                [matches 24.04]
apt 3.2.0        (26.04) default (solver3)        -> courier-mta + 5 extras (unicode10 variant)
apt 3.2.0        (26.04) -o APT::Solver=internal  -> postfix                [matches 24.04]
```

The **version-only** comparison (25.10-default vs 26.04-default) picks the
identical *package set* (courier-mta + the same 5 auxiliary packages) both
times — the only difference is each release's own current version numbers
(e.g. `courier-mta 1.4.1-3` vs `1.5.1-2`, `libcourier-unicode8` vs
`-unicode10`), which is ordinary cross-release version drift, not a
selection divergence. The script's string-diff still flags this comparison
as "DIVERGED" because the version strings differ textually — that is the
first source of over-counting in the raw 11/28.

`apt-cache policy postfix courier-mta exim4-daemon-light` (checked directly
on 26.04): all three pinned at the same priority (500, standard archive) —
rules out pinning as the explanation. `Priority:`/`Section:` also don't
explain legacy's specific pick (`postfix`: optional/mail; `exim4-daemon-light`:
**extra**/mail — i.e. also a `main` package — yet legacy still preferred
postfix over it).

**B2/B3 (`virt`/`virt-nat`, synthetic virtual package)** — designed together
to pin down *why*:

```text
B2 (virt):     scan order zulu,mike,alpha   name-alphabetical alpha,mike,zulu (OPPOSED)
   -> every one of 6 rows picks alpha (the alphabetically-first name)

B3 (virt-nat): scan order alpha,mike,zulu   name-alphabetical alpha,mike,zulu (AGREE)
   -> apt 2.8.3 (24.04)                           -> zulu
      apt 3.1.6ubuntu2 (25.10) default (solver3)  -> alpha
      apt 3.1.6ubuntu2 (25.10) internal           -> zulu   [matches 24.04]
      apt 3.2.0 (26.04) default (solver3)         -> alpha
      apt 3.2.0 (26.04) internal                  -> zulu   [matches 24.04]
```

This is a clean, fully consistent mechanism across **both** synthetic
fixtures: **solver3 always picks the alphabetically-first provider name,
regardless of scan order** (alpha in B2, alpha in B3 — same answer even
though scan order flipped). **Legacy always picks whichever provider is
scanned *last*** (alpha in B2, since alpha was scanned last there; zulu in
B3, since zulu is scanned last there) — equivalently, whichever provider
`apt-cache showpkg <virtual-pkg>` lists *first* under "Reverse Provides"
(consistent with an internal list built by prepending each newly-scanned
entry, so the list head ends up being the last thing scanned). This same
rule *also* correctly predicts fixture B's real result: `courier-mta` is, in
fact, the alphabetically-first name among all 10 real providers of
`mail-transport-agent` on questing/resolute — the release pair where solver3
exists and is the one that actually picks it (`courier-mta` < `dma` <
`esmtp-run` < ... < `postfix` < ... < `ssmtp`) — and `postfix` is what
`apt-cache showpkg`'s real Reverse-Provides listing shows first on every
release tested, noble's 9-provider set included. Both
mechanisms — "solver3: alphabetically-first name" and "legacy: last-scanned
== first-in-Reverse-Provides" — now have three-for-three agreement: the real
fixture (B) and both controlled fixtures (B2, B3).

**C1 (`multiclean`)** — identical on all 6 rows: everyone picks
`zz-e2-multi-clean` **2.0** (highest). No divergence.

**C2 (`multibroken`)** — identical *substantive outcome* on all 6 rows
(**every row fails the whole request — zero packages selected**), but with
different diagnostic text, which is the second source of over-counting in
the raw 11/28:

```text
legacy (24.04, and both *-internal rows):
  E: Unable to correct problems, you have held broken packages.

solver3, apt 3.1.6ubuntu2 (25.10):
  E: Unable to satisfy dependencies. Reached two conflicting decisions: ...
     [a structured trace showing 1.1 and 1.0 were both considered and rejected]

solver3, apt 3.2.0 (26.04):
  E: Unable to satisfy dependencies. Reached two conflicting assignments: ...
     [same structured trace, "assignments" instead of "decisions" —
      even solver3's own message wording changed between 3.1.6ubuntu2 and 3.2.0]
```

**Neither** solver generation automatically backs off to the still-satisfying
1.1 — both fail the whole request outright, identically. Not a selection
divergence (both select nothing); a genuine, useful diagnostic-quality
difference (solver3 explicitly shows 1.1/1.0 were considered and rejected;
legacy just says "held broken packages"), and a minor extra finding that the
diagnostic *format itself* is not stable release-to-release even within
"solver3."

**Corrected divergence tally**: of 7 fixtures × 4 comparisons = 28 total,
the script's plain string-diff flags 11 as different. Manually re-checked
against actual package **selections** (not version-number text, not
diagnostic text): only **2 of 7 fixtures ever show a genuinely different
provider/package chosen** — `mta` and `virt-nat` — contributing **6** real
selection-divergence comparisons. All other apparent "divergences" (1 more
from `mta`'s version-only cell, 4 from `multibroken`) are text-level
artifacts of comparing exact strings, not resolution-outcome differences. Of
those 6 genuine divergences: **0 occur on a "version differs, solver
matches" comparison, and 6 of 6 occur on a "solver differs" comparison**
(3 solver-only + 3 version-and-solver-together, and the version-and-solver
cell in both cases matches what the solver-only cells already show — apt
version contributes nothing beyond what the solver setting alone explains).

## Finding

**Divergence between apt's classic resolver and `solver3` is real and can be
significant** (fixture B: a single-package answer vs. a 6-package answer with
a materially different, heavier provider) — the underlying worry
is confirmed, not a false alarm.

**But the divergence tracks the *solver algorithm* (`APT::Solver`), not apt
`major.minor` as such — with zero counterexamples in this experiment.** This
was demonstrated directly, not inferred, and triangulated across a real
package relationship and two independently-constructed synthetic ones:

- Holding the solver fixed and varying major.minor (25.10-default vs
  26.04-default — two different apt versions, both defaulting to solver3)
  produced **zero** genuine selection divergence across all 7 fixtures,
  `mta` included (same provider set, only ordinary version-number drift).
- Holding major.minor fixed (a single 26.04 binary, or independently a
  single 25.10 binary) and varying only `-o APT::Solver=internal` reproduced
  the exact same divergence **twice**, on the same apt install, for both
  fixtures that diverge at all (`mta`, `virt-nat`).

The proposed trigger — `host apt major.minor == target apt
major.minor → local`, else container — happens to give the right answer for
these three **stock, untouched** Ubuntu images, because major.minor
currently predicts the default solver perfectly (24.04: no solver3 exists;
25.10/26.04: solver3 is the shipped default). But major.minor is a **proxy**
for the thing that actually matters, not the thing itself: `APT::Solver` is
an ordinary `apt.conf.d`-scoped configuration key, confirmed settable
independently of the installed apt version (that is exactly what `-o
APT::Solver=internal` does, and it is a real, supported apt flag, not a test
harness artifact). A host or target whose `apt.conf.d` has been hand-edited
— or a future stable-release update that changes the packaged default while
major.minor stays put, which is precisely what would need to happen for a
point release to backport or revert a solver default — breaks the
correlation the design's check silently assumes, with no signal in
`major.minor` that anything changed.

**Verdict on the specific backend-auto-selection question**: "same distro id and same
apt major.minor" is **too loose** as the sole test. It is not that it gives
wrong answers today — in every row tested here it happens to agree with the
stricter, correct signal — but it is not actually testing the thing the rule
cares about, and this experiment found the two can come apart (same
major.minor, different solver, via `-o APT::Solver=internal`) as easily as a
single `-o` flag. A rule that is right for the reason it thinks it's right is
not the same as a rule that is right for the right reason, and only one of
those survives an apt.conf.d edit or a future point release.

## What this means for the design

- the caution ("resolution must be made by the same apt the target
  runs") is **validated** — keep it, and keep container-by-default as the
  safe fallback when the local backend is uncertain.
- the specific trigger should be **sharpened, not replaced**: compare
  the **effective `APT::Solver`** value between host and target — obtainable
  today via `apt-config dump | grep -i solv` (specifically the
  `binary::apt-get::APT::Solver` scoped key, since that's the front-end
  debark actually shells out to) — in addition to, or instead of, apt
  major.minor. This is cheap (one more `apt-config dump` call) and is
  `apt.conf.d`-adjacent data design already captures, so it is a
  small addition, not a new snapshot subsystem.
- An even more direct option than comparing config strings: a **capability
  probe**. Attempt `-o APT::Solver=<target's recorded value>` locally and
  treat `E: Can't call external solver …` as an unambiguous "must use
  container" signal (confirmed reliable: that is exactly the error apt 2.8.3
  gives for `APT::Solver=3.0`). This tests the actual capability instead of
  inferring it from version strings, and sidesteps having to keep a
  version-to-solver-default table in sync with upstream Ubuntu.
- Fixture C2 is a useful side note, not a backend-selection finding: **do not
  assume either solver generation will silently back off to a lower,
  still-satisfying version** of a dependency when the highest candidate has
  an unrelated broken sub-dependency — both fail the whole request outright.
  A builder that wants "fall back to an older version automatically" needs
  its own explicit version pin, not a solver rescue.

## Limitations

- Only one apt build per major.minor was available (Ubuntu's own `docker.io`
  images); did not test whether two different **patch** releases of the
  exact same major.minor (e.g. two 3.2.x point releases) can themselves
  diverge. The default-vs-`internal` toggle is the closest available proxy
  for "same apt version, different solver," and is a real, not synthetic,
  configuration axis — but it is not literally two different apt binaries.
- The positive mechanism identified for provider tie-breaking ("solver3:
  alphabetically-first name; legacy: last-scanned / first-in-Reverse-Provides")
  fits all three fixtures tested (B, B2, B3) with no exceptions, but the
  *reason* real-archive scanning puts `postfix` first in Reverse-Provides —
  i.e. why the real archive's internal merge order comes out that way — was
  not independently traced through libapt-pkg; it is an observed regularity
  in what `apt-cache showpkg` reports, not a verified account of the archive
  merge mechanics.
- The WSL Ubuntu 24.04 fallback mentioned in the task recon was not needed —
  Docker supplied all three releases directly and consistently, and the
  experiment inherently needs three distinct apt versions, which WSL alone
  cannot provide.
- **Methodology note, recorded for the record**: this experiment's shared
  output paths were briefly, accidentally overwritten mid-run by a duplicate
  orchestration attempt that used an *empty* dpkg status file — exactly the
  false-superset pitfall this document's method section (and E1's) warns
  against. That run's output was discarded entirely and does not appear
  anywhere above; every number in this document comes from this script, with
  a real per-container dpkg status, rerun cleanly afterward.
