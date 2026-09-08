# FAQ

## Why does it need the target's snapshot at all? Why not just point it at a package list?

Because `apt-get install --download-only` on an internet-connected machine
only downloads what's missing *on that machine* — every dependency the
online box already happens to have gets silently skipped. The bundle looks
complete, ships, and then fails on the offline machine with unmet
dependencies. This is not a hypothetical: practitioners rediscover it
constantly on Ask Ubuntu, Unix & Linux SE, r/sysadmin and vendor air-gap
guides, which is exactly why the CLI answers "what will be installed" as one
of its four standard questions on every build — getting that wrong is the
whole failure this tool exists to fix.

A snapshot is what lets the online builder resolve *as the target would*:
its actual installed set (`dpkg_status`), its actual configured sources and
pins, its actual `Install-Recommends` setting, even the machine id needed to
reproduce Ubuntu's phased-update selection faithfully for the
*automatic* upgrade pass (`--upgrades`) — all fed to the target release's own
apt, running in a private root, so the dependency decision is the one the
target's apt would have made itself. That machine-id qualifier matters for
an ordinary named-package request like the one above, too, just not the way
it first sounds: measured directly (experiment
[E1](experiments/E1-phased-updates.md)), apt bypasses phasing
entirely for an explicitly-named package — `apt-get install vim`, with or
without a version — regardless of machine id or a redacted snapshot's
conservative fallback, so the builder and a hands-on admin on the target
agree here for a *different* reason than machine-id reproduction: both sides
ignore phasing the same way. The machine-id mechanism is real and does what
ADR-006 describes, but only for the automatic full-upgrade pass; treat a redacted
snapshot as *not* guaranteeing a fully-phased version of a package you asked
for by name. The alternative — asking the operator to describe the target's state by hand, or
trusting a generic "Debian 12" baseline — reintroduces exactly the class of
bug this tool exists to eliminate. `apt-offline`, the closest prior art,
avoids this by resolving *on the target instead* — which produces an empty
request on a fresh, never-networked machine and cannot handle a vendor
`.deb` at all (workflow 1 and workflow 2 in `docs/quickstart.md`).

Capturing a snapshot needs no root privileges, and `--redact` exists
precisely because the tool recognises this is a somewhat sensitive
inventory: it strips the machine id, proxy settings and any operator labels
before the file ever leaves the target.

**One qualification, added 2026-09-06.** debark can now be told to assume a
baseline instead of measuring one: `build --base ubuntu:26.04/desktop`, and
`snapshot from-base`, describe the target by a stock release, for the case
where the machine does not exist yet and so cannot be snapshotted. That is a
"generic Debian 12 baseline", which is the thing this answer argues against,
and the argument is unchanged — which is exactly why the assumed path is the
fallback and never the default. What changed is that debark now says which
of the two it did, out loud and in the artefact: the snapshot records
`origin.kind: synthesized` and the digest of the definition that produced it,
the bundle carries the assumed package list, and `install` reports on the
target how far the real machine diverges from what was assumed. A measurement
is still strictly better than an assumption; the assumption exists for when
there is nothing to measure. The next question sets out what it costs.

## Do I need access to the offline machine first?

Not strictly, not since 2026-09-06 — but the answer you get without it is
weaker, and it is worth being precise about how much weaker.

The accurate path is unchanged, and it is still the one to use whenever you
can: run `debark snapshot create` on the target (no root, no network, writes
one file), carry it to the builder, and build against it. That snapshot is a
measurement of one real machine, so the bundle is **proven** complete for it —
apt was given that machine's own installed set, sources, pins and apt
configuration, and answered with exactly what is missing there.

When there is no machine to measure — fifty boxes arriving at a site with no
network, or an evaluation before anything has been carried across the gap —
`build --base ubuntu:26.04/desktop` (or `snapshot from-base`, when you want
the snapshot as a file to inspect, diff or commit) describes the target by its
stock release instead. debark resolves that base's seed packages with the
release's own apt in a private root where nothing is installed, and treats
what apt returns as what the target already has. It is still apt deciding
what depends on what; where a measurement has caught a closure claiming a
package a real image does not have, debark narrows the claim by excluding
that package by name, and narrowing can only ever make a bundle larger.

What that costs you, precisely:

- **The bundle is complete only if the target really is a stock install of
  that release.** Nothing was measured on the eventual machine, because there
  was no machine to measure. It is an assumption, and the artefact says so
  permanently: `origin.kind` is `synthesized`, alongside the base's id and the
  digest of the definition it came from.
- **The two ways of being wrong are not symmetric.** If the real machine has
  *more* installed than the base assumed, the bundle is larger than it needed
  to be — a cost in media space and nothing else. If it has *fewer*, the
  bundle is short and fails at the gap with an unmet dependency, which is the
  precise failure this tool exists to prevent. Every builtin base therefore
  errs downward: each variant seeds the smaller of two plausible metapackages
  (`ubuntu-desktop-minimal`, not `ubuntu-desktop`), a bare `ubuntu:26.04` means
  the `minimal` variant, and `Recommends` defaults to off.
- **How good the assumption is has been measured once, for one release.**
  `docs/experiments/E8-base-fidelity.md` compared each Ubuntu 24.04 base
  against Canonical's published image manifests: nothing assumed present that
  a real install lacks, once a real defect found by that very measurement was
  fixed — `ubuntu-desktop-minimal` resolved from an empty root pulls in
  `lsb-base`, which the desktop image does not install, and a base claiming it
  would have made every desktop bundle short by one dependency. It also showed
  how small the claim is: 147 assumed packages against a real 588 for
  `minimal`, 802 against 1,809 for `desktop`. Debian 12 and 13, Ubuntu 22.04
  and 26.04 and arm64 are not merely unmeasured but untried: nothing on this
  path has been run outside Ubuntu 24.04 amd64, down to whether Debian's seed
  metapackages resolve at all. Nor has the container backend, which is the
  path every macOS, Windows and cross-release build takes — it has never run
  `from-base`, and its tests drive a fake. A bundle has now been built from a
  synthesized snapshot and its install plan checked on a real machine with
  `install --status`; none has been installed. The README's status section is
  the list to read before relying on any of it.
- **You find out on the target, not on the builder.** `install` compares the
  assumed package list carried in the bundle against the real dpkg status in
  front of it, and reports how many of the assumed packages are missing and
  which ones. It warns and continues rather than refusing, per the existing
  rule that doctor-style findings warn while policy fails. That turns a silent
  unmet-dependency failure on the far side of an air gap into a legible
  finding on the near side of one — it does not make the bundle correct, it
  tells you it may not be.

That last point is the one you can actually see, so here it is for real: a
bundle built against `ubuntu:24.04/desktop`, then checked with `--status` on a
machine that is not a desktop. Run on 2026-09-06 on WSL Ubuntu 24.04.3 LTS
(noble) amd64 with apt 2.8.3, a real installed system, against the live
`archive.ubuntu.com` and `security.ubuntu.com`, with debark at commit
`38b5357`. It is not from `hack/demo-airgap.sh`, the Debian 12 container the
project's other demo transcripts come from. (That SHA, like every commit
revision quoted in this repository's documentation, is from the
pre-publication history, which was squashed into a single commit when the
project was opened; it will not resolve here.) The run:

```console
$ debark install ./bundle --status
OK  install plan for ./bundle
  verify:    OK
  target:    ubuntu 24.04 (noble) amd64
  to install: 0
  to upgrade: 0
  unchanged:  3
  base:       ubuntu:24.04/desktop (assumed 802 installed, 433 not present here)
  warning: this bundle was built against an assumed base (ubuntu:24.04/desktop), not a snapshot of this machine
  warning: 433 of the 802 assumed base packages are not present on this machine: accountsservice:amd64, acl:amd64, alsa-base:all, alsa-utils:amd64, anacron:amd64, apg:amd64, aptdaemon-data:all, aptdaemon:all, bubblewrap:amd64, colord-data:all and 423 more - the bundle was resolved as if this machine already had them, so it may be short
```

`--status` changes nothing; it answers a question. And the answer is the
failure this tool exists to prevent, named on the near side of the gap instead
of arriving as an apt error on the far side with no network to fix it from.
On a machine that does match its base the same lines say so — `assumed 147
installed, 0 not present here`, and `all 147 packages the base assumes are
present on this machine` — while the first warning, that this was an
assumption at all, stays either way.

`install --status` is as far as this has been proven. `install` proper on a
bundle built from a base has not been run, on that machine or any other.

One practical knock-on: with no first trip to the target, there is also no
debark binary on it. Build with `--embed-binary ./debark-linux-amd64` and
the target-architecture binary travels inside the bundle at
`bin/debark-linux-amd64`, so the first thing to cross the gap carries both
the software and the tool that verifies it.

So: snapshot if you can, base if you cannot. If you get access to the machine
even once, take the snapshot on that visit — it costs one command, no root and
no network, and it upgrades every bundle you build afterwards from an
assumption into a proof.

## Why not just support snaps / Flatpak / pip / npm / cargo / OCI / Helm too, while you're at it?

Because each of those has its own closure semantics and its own trust model,
and getting one ecosystem's air-gap story right is already the entire scope
of this tool. A snap's dependency closure isn't apt's dependency closure; a
Python wheel's provenance story isn't a signed `Release` file's provenance
story. Trying to be the air-gap tool for every packaging ecosystem at once is
the single failure mode every report that informed this design agreed was
fatal to the project — scope-destroying, and it would mean being mediocre at
all of them instead of correct at one. If your workflow needs several
ecosystems across the gap, each already has a tool that understands its own
trust model better than a generalist wrapper ever could; debark's job ends
at "correct and verifiable for Debian/Ubuntu's own apt," on purpose.

Snap packages that show up as a *dependency* of something you asked for are
a different, narrower case debark does handle: `doctor` flags snap-shim
packages so you know before you ship the media, because a shim that expects
snapd and a network-connected snap store at install time is exactly the kind
of surprise this tool is supposed to prevent, not cause.

## Why not solve dependencies natively on Windows/macOS instead of requiring a container?

Because **apt is the oracle** — the first, non-negotiable rule this whole
project is built on. Every dependency decision has to be made by the
actual apt binary from the actual target release, because that is the only
thing that reliably makes the same choice the target machine's own apt would
make: the same solver version, the same handling of `Phased-Update-Percentage`,
the same pinning behaviour. A from-scratch or ported solver would inevitably
diverge from real apt in edge cases — and a diverging solver doesn't fail
loudly, it produces a bundle that looks fine and fails on the far side of the
gap, which is the one outcome this entire tool exists to prevent. The
permanent do-not-build list is explicit about this: *"A pure-Go apt
dependency solver — wrong bundles that fail on the far side of the gap;
unbounded maintenance. Existential risk."*

Windows and macOS simply don't have apt. So on those platforms — and on a
Linux host whose own apt version or distribution doesn't match the target's
closely enough — debark resolves inside a container
running a pinned image of the *actual target release*, and nothing about the
resolution logic itself changes: the same private-root construction, the
same apt invocation, the same lock output. It costs a container runtime; the
alternative costs correctness, which isn't a trade this project makes.

The divergence this guards against is real and can be significant, not a
theoretical worry: experiment
[E2](experiments/E2-solver-divergence.md) reproduced apt's classic
resolver and its newer `solver3` picking genuinely different closures for
the same real request (`postfix` alone versus a 6-package `courier-mta`
pull-in, for `mail-transport-agent`). E2's other finding was that the
original "close enough to resolve locally" test — comparing apt's own
major.minor version between host and target — was checking the wrong
variable in principle: divergence tracks the effective solver algorithm
(`APT::Solver`, an ordinary `apt.conf.d` setting), not the apt version as
such. The same apt binary diverges from itself under
`-o APT::Solver=internal`, while two different apt versions running the same
solver never diverged in that experiment.

That is fixed. `auto` now compares effective solvers
(`core/apt/select.go`, `localMismatchReason`): the host's is probed live
with `apt-config dump`, the target's is the solver its captured apt version
would use by default, and anything other than a confirmed match falls to the
container — including "could not confirm", which prefers the container
rather than risking an unverified match. The apt major.minor difference is
still recorded, as a non-blocking note in the lock's warnings, because it is
worth seeing even on a build where local was correctly chosen.

One asymmetry is deliberate and worth knowing: the *target's* solver is
inferred from its apt version, not read from its captured `apt.conf.d`,
because the frozen `Selection` contract carries only `snapshot.Target` and
not the snapshot's conf files. A target that hand-edits `APT::Solver` away
from its release default is therefore not caught by the backend gate — it is
caught one stage later, at resolve time, where the full snapshot is
available and `lock.Resolver.SolverDivergence` records it.

## What will debark never do?

From the design's permanent do-not-build list, because each of these
either recreates the "wrong bundle that fails offline" risk above, or is a
different, better-served problem entirely:

- **A pure-Go apt dependency solver.** Covered above — apt is always the
  oracle, full stop.
- **A mirror manager or artifact repository.** aptly, Pulp, Katello,
  Artifactory and Nexus already own that problem; debark produces one
  bundle for one request, not a mirror.
- **A daemon, scheduler, or server in the community binary.** cron and CI
  already do scheduling. A job *protocol* (`BuildRequest`/`BuildResult`)
  exists so something else can call the engine later — a server calling it
  is not something this project builds.
- **A GUI, or a full-screen TUI as the primary interface.** This has to work
  over SSH, a serial console, a CI log, and a transcript pasted into a
  ticket. Progress bars that degrade to plain lines are enough; see
  "Progress" in `debark --help` output
- **A self-updater or any update check.** Conflicts with distro packaging
  and with environments where nothing is allowed to reach the network
  unannounced — which describes a meaningful share of this tool's own
  audience.
- **Telemetry, crash reporting, or any analytics dependency — architecturally,
  not just by policy.** A debark snapshot is itself a sensitive inventory
  of a locked-down machine; a tool that collects telemetry about *itself*
  while making exactly that promise to its users would be absurd. There is
  no code path in this tool, on either side of the gap, that sends anything
  anywhere without the operator explicitly invoking a command that writes a
  file. Nothing here calls home. Ever.
- **A general CVE scanner or software licence adjudicator.** `doctor`
  records what it can observe — redistribution flags, network-looking
  maintainer scripts, DKMS without matching headers — and stops there. It
  does not become Trivy, and it does not tell you whether you're allowed to
  ship a package; see `docs/security-model.md`'s redistribution section for
  why that's a warning, never a block.
- **Licence checks, caps, or metering in the community binary, ever.** The
  free edition contains no code path that knows or cares whether anyone
  paid. Every signature, verification, provenance and SBOM capability this
  tool has is in the free edition, uncapped, permanently — see
  `docs/free-paid-policy.md`.

## Does the target machine need to install debark?

No, and it's a design rule, not a nicety: the target side has to work with
nothing but the open binary, or nothing at all beyond apt itself if you
cannot get even that far. `verify` and `install` are the only two commands
that ever need to run there; a bundle built with `--embed-binary` carries a
copy of the target-architecture binary alongside itself so there is nothing
separate to source, vet, or install ahead of time. Everything with any
licensing weight — fleet workflows, central policy, HSM/KMS signing,
retention — lives only on the connected builder side, in a separate
commercial component that never touches the target.
