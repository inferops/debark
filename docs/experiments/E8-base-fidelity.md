# E8 — Base fidelity

**Question (ADR-014):** how close is the resolved closure of a base
definition's seed metapackages to what a real install of that release
actually has? `debark snapshot from-base` produces a snapshot that is an
*assumption* about a machine rather than a *measurement* of one, and nobody
had measured how good the assumption is.

**Finding, up front: the assumption holds in the direction that matters, and
it took a fix to get there.** Across all three Ubuntu 24.04 base variants the
dangerous direction is now empty — no base claims a package a real install of
that release lacks. Before the fix, the desktop base claimed exactly one:
`lsb-base`, a transitional package apt selects when resolving from an empty
root and the published desktop image does not install. That single package is
the whole reason this experiment exists, and it would not have been found by
reading anything.

The harmless direction is large and expected: a base claims roughly a quarter
to a half of what a real image has. A bundle built from one is therefore a
superset — bigger than it needed to be, and correct. That is the same trade
the Bash prototype made deliberately.

## Method

Script: [`hack/experiments/e8-base-fidelity.sh`](../../hack/experiments/e8-base-fidelity.sh)
(rerunnable; writes evidence to `hack/experiments/out/e8/`).

Three stages.

1. **Closure.** Run `debark snapshot from-base <base> --backend local` for
   each variant and read `origin.assumed_installed` back out of the resulting
   snapshot with `snapshot inspect --json`. This is the real product code
   path, not a re-implementation of it: apt resolves the seeds in a private
   root where nothing is installed, exactly as an operator's from-base run
   does. Running it any other way would have measured a second implementation
   nobody ships.

2. **Ground truth,** from two independent sources, because neither covers
   every variant.
   - **server, desktop** — the `.manifest` Canonical publishes beside each
     image on `releases.ubuntu.com`, which lists every package that image
     installs. A fact about a real image, produced by neither this experiment
     nor debark. The point release moves, so the script scrapes the release
     index rather than guessing a URL: a guessed URL that 404s would produce
     an empty ground truth and a flattering result.
   - **minimal** — there is no ISO for a minimal install, so the ground truth
     is the dpkg database of a real installed system of that release, read
     with `dpkg-query -W`. Whatever machine the script runs on, when it
     matches the release under test.

3. **Compare, by package name.** Versions are deliberately ignored. A target
   running a different version of an assumed package still *has* it, so the
   bundle is not short; counting that as divergence would bury the one
   direction that matters under ordinary patching noise.

The two directions are reported separately because they are not equally
dangerous:

| direction | meaning | consequence |
|---|---|---|
| **assumed-but-absent** | the base claims a package the real install does not have | the bundle omits it, the target's apt cannot satisfy the dependency, and the install **fails at the far side of the air gap**. The only direction that can hurt anyone. Must be empty. |
| **absent-but-present** | the real install has a package the base did not claim | the bundle carries it needlessly. Harmless. |

The host must be the same release it is measuring. That is not convenience:
the backend gate refuses the local backend unless it can confirm the
host apt's effective solver matches the target's, and for a base — a machine
that does not exist — the only case where it can is when the host *is* that
release (see `core/base`'s `gateTarget`). Measuring 22.04 from a 24.04 host
would have measured the wrong apt.

**Run date: 2026-09-06.** Host: Ubuntu 24.04.3 LTS (noble) amd64, apt 2.8.3,
a real installed system. debark at commit `7bfa75d` — a pre-publication
revision, from a history that was squashed into a single commit when the
project was opened, so it will not resolve in this repository. All archive fetches
were real network fetches against `archive.ubuntu.com`, `security.ubuntu.com`
and `releases.ubuntu.com`.

## Raw evidence

### A. The three closures

| base | seeds | packages assumed |
|---|---|---|
| `ubuntu:24.04/minimal` | `ubuntu-minimal` | 147 |
| `ubuntu:24.04/server` | `ubuntu-server-minimal` | 256 |
| `ubuntu:24.04/desktop` | `ubuntu-desktop-minimal` | 802 |

Full lists: `hack/experiments/out/e8/closure-{minimal,server,desktop}.txt`.

This also settled a question nobody had checked: **all three seed
metapackages exist.** `ubuntu-minimal`, `ubuntu-server-minimal` and
`ubuntu-desktop-minimal` all resolve in noble. A missing seed fails loudly
(`E: Unable to locate package`), so this was never a silent risk, but it was
an unverified assumption in the builtin table until now.

### B. Ground truth

| variant | source | packages |
|---|---|---|
| minimal | this host's `dpkg-query -W` | 588 |
| server | `ubuntu-24.04.4-live-server-amd64.manifest` | 687 |
| desktop | `ubuntu-24.04.4-desktop-amd64.manifest` | 1809 |

Raw: `truth-{minimal,server,desktop}.txt`, and the two fetched manifests.

### C. The comparison, before the fix

```
minimal: PASS  closure=147  real=588   ASSUMED-BUT-ABSENT=0  ABSENT-BUT-PRESENT=441
server:  PASS  closure=257  real=687   ASSUMED-BUT-ABSENT=0  ABSENT-BUT-PRESENT=430
desktop: FAIL  closure=803  real=1809  ASSUMED-BUT-ABSENT=1  ABSENT-BUT-PRESENT=1007
    first 20 assumed-but-absent:
      lsb-base
overall: FAIL
```

### D. `lsb-base`, the one real finding

Diagnosed on the same host:

```
$ apt-cache show lsb-base
Package: lsb-base
Version: 11.6
Priority: optional
Section: misc
Depends: sysvinit-utils (>= 3.05-4~)
Description-en: transitional package for Linux Standard Base init script functionality

$ dpkg-query -W -f '${Package} ${Status}\n' lsb-base
lsb-base unknown ok not-installed
```

So: a transitional package carrying nothing but a dependency on
`sysvinit-utils`. apt resolving `ubuntu-desktop-minimal` from an empty root
selects it; the published desktop manifest does not list it; a real noble
system reports it not-installed. The server manifest *does* carry it, which
is why only desktop failed.

The mechanism of the failure is worth stating precisely, because it is the
failure the whole product exists to prevent. A base claiming `lsb-base` makes
`build` treat it as already present, so it is omitted from the bundle. Any
requested package depending on `lsb-base` then resolves cleanly on the
builder and fails to install on a target that does not have it — on the far
side of an air gap, with no network to fix it from.

### E. The comparison, after the fix

```
minimal: PASS  closure=147  real=588   ASSUMED-BUT-ABSENT=0  ABSENT-BUT-PRESENT=441
server:  PASS  closure=256  real=687   ASSUMED-BUT-ABSENT=0  ABSENT-BUT-PRESENT=431
desktop: PASS  closure=802  real=1809  ASSUMED-BUT-ABSENT=0  ABSENT-BUT-PRESENT=1007
overall: PASS
```

`assumed-but-absent-{minimal,server,desktop}.txt` are all zero bytes.

### F. What the harmless direction contains

A sample, to show what a base does *not* claim (and therefore what a bundle
built from one carries needlessly):

- minimal: `adwaita-icon-theme apparmor apport appstream base-files base-passwd …`
- server: `amd64-microcode apport-symptoms appstream apt-utils bash bash-completion bc bind9-dnsutils …`
- desktop: `adcli alsa-ucm-conf amd64-microcode apparmor apport-gtk apt-config-icons …`

`base-files` and `bash` appearing here is the clearest illustration of what a
seed closure is: apt was asked what installing `ubuntu-minimal` requires, not
what a bootstrapped system contains, and `Essential: yes` packages that
nothing explicitly depends on are simply not in any dependency chain. Every
one of them is in the safe direction.

## The fix, and why it is allowed

`core/base.Definition` gained an `Excludes` field: packages the seed closure
may name but the base must not claim the target already has. Every Ubuntu
base excludes `lsb-base`.

This is not hand-curating a package list, which ADR-001 forbids. An exclusion
only ever makes the base's claim **smaller**, and a smaller claim can only
ever make a bundle **larger** — the package moves from "assumed present" to
"must be carried". It cannot produce a short bundle. A wrong exclusion costs
bytes; a wrong inclusion costs an install on the far side of an air gap. apt
is still the sole authority on what depends on what; the exclusion only
narrows what debark is willing to *assume* about a machine it has never
seen.

`lsb-base` is excluded from every Ubuntu base, not only desktop, even though
the server image carries it. Excluding it where it is genuinely present costs
one small package of bundle size. Fitting the exclusion per-variant to a
single release's measurement would claim knowledge about 22.04 and 26.04 that
nobody has.

## What this means for the design

- **ADR-014's err-toward-not-installed rule is now measured, not reasoned.**
  The three variants' dangerous direction is empty and the harmless direction
  is 441/431/1007 packages. The asymmetry the rule is built on is real and
  large.
- **The rule needed enforcement, not just intent.** Every seed in the table
  was already the smaller of two plausible metapackages, and `Recommends` was
  already off, and the desktop base was *still* wrong by one package. Careful
  choices at the seed level do not guarantee the property; only measuring the
  closure does.
- **A base is a much smaller claim than an image.** 147 of 588 for minimal,
  802 of 1809 for desktop. An operator should read `from-base` as "assume a
  quarter to a half of a real install, and carry the rest", not as "assume a
  real install". The docs say a base is an assumption; this is how big an
  assumption.
- **`from-base` needs exactly the network access `build` needs.** The closure
  stage contacted the distro archive and nothing else. No debark-operated
  service was involved, which is the property D8 requires and ADR-014 states.

## Limitations

Stated plainly, because they bound what the PASS above means.

- **One release, one architecture.** Ubuntu 24.04 amd64 only. Debian 12 and
  13 and Ubuntu 22.04 and 26.04 are **not measured**, and neither is arm64.
  The script refuses to run against a release the host is not, so measuring
  the rest needs a host or container of each — which is the natural home for
  this in the integration matrix.
- **Debian has no published image manifest** to compare against in the way
  Ubuntu does, so stage 2's authoritative source does not exist there. A real
  installed Debian system is the available ground truth, and the script's
  minimal path already supports exactly that shape.
- **The desktop and server ground truths are ISO manifests, not machines.** A
  machine that has been running for a year has more packages than its image
  did, which moves divergence further into the harmless direction. Using the
  image is the conservative choice: it is the smallest real install of that
  release, and therefore the hardest test of the dangerous direction.
- **The minimal ground truth is a WSL system**, which is a real Ubuntu 24.04
  install but not one produced by the desktop or server installer. It is
  ground truth for "a real machine of this release", not for "the minimal
  ISO", which does not exist.
- **Nothing here measures the container backend.** Every closure was resolved
  by the local backend on a matching host. The container path produces the
  same artefact by construction (it runs this same command inside the image),
  but that is reasoning, not a measurement.

## Reproducing this

```bash
# On a host of the release under test, with a debark binary:
./hack/experiments/e8-base-fidelity.sh --binary /path/to/debark

# Closure only, no network beyond the distro archive:
./hack/experiments/e8-base-fidelity.sh --no-network
```

The script exits after printing `overall: PASS` or `overall: FAIL`. A FAIL
naming any package under "assumed-but-absent" is a base definition that must
shrink before it ships — add the package to that base's `Excludes` with a
comment citing the run that found it, exactly as `lsb-base` was handled.
