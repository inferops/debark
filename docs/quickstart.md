# Quick start

`debark` moves Debian/Ubuntu software across an air gap. A snapshot is
taken on the offline target; an online builder resolves the exact missing
closure with the target release's own apt, running in a private apt root; the
output is a bundle containing a real flat apt repository, a lock plan and a
signed manifest; the target verifies the manifest — **before apt ever sees
the repository** — and then installs exact versions from the lock.

```
   offline target                    online builder                       offline target
 ┌────────────────┐   snapshot    ┌──────────────────────┐   bundle    ┌────────────────┐
 │ debark         │ ────────────► │ debark build         │ ──────────► │ debark         │
 │ snapshot create│  (small file) │  apt in private root │  (USB/media)│ verify         │
 │                │               │  fetch → repo → lock │             │ install        │
 └────────────────┘               │  → signed manifest   │             └────────────────┘
                                   └──────────────────────┘
```

> **Implementation status.** Every transcript on this page is real output
> from an actual run — most of it from [hack/demo-airgap.sh](../hack/demo-airgap.sh),
> which snapshots a real Debian 12 container, builds and signs a bundle, and
> verifies and installs it in a second container started with
> `--network none`, then runs the software it just installed. Workflow 4 is
> the exception: its transcripts come from a second run, on a real Ubuntu
> 24.04 machine rather than a container, and it states that run's provenance
> where they appear. The engines behind each command are still separate work
> under active, concurrent development in this checkout, so where something
> does not yet work the way the surrounding prose describes, it is called out
> inline at the point it matters rather than collected in one disclaimer.
> [The README's Status section](../README.md#status-and-known-gaps) is the
> single list of what is still missing; this page defers to it.

## Before you start

Two machines (or the same machine playing both roles for a test run):

- **The offline target** — the Debian/Ubuntu machine you are installing
  software on. It never needs to install debark at all if it already has
  apt: `verify` and `install` are the only commands that ever run there, and
  either can run from a copy on the media itself if you built one with
  `--embed-binary`.
- **The online builder** — any machine with network access. Linux with a
  matching apt resolves directly; anything else (Windows, macOS, a
  mismatched Linux release) resolves inside a container automatically.

## The guided flow — if you would rather be asked

Everything on this page is an explicit command, because a command is what goes
into a runbook. The first time through, `--interactive` will ask instead:

```sh
debark build --interactive
```

It asks, in order: whether you have a snapshot of the target machine — and if
you do not, walks a base picker (distribution, then release, then install
profile, then architecture) of the kind Workflow 4 describes; what should go
in the bundle, one entry per line with a blank line to finish, echoing back
what each line *is* as you type it (`vlc — apt package`; a URL comes back as
`vendor .deb, dependencies will be resolved`); where to write the bundle; a
signing key, pointing you at `debark keygen` if you have none; whether to
add `--upgrades` and an SBOM. Then it shows a review — nothing has touched the
network up to that point — and asks whether to go ahead. `debark snapshot
from-base --interactive` asks the base questions alone, for when you want the
snapshot rather than a bundle.

Two things it does at the end are why it is worth using rather than avoiding.
It writes what you typed into a real `packages.txt`, which you can diff, edit
and re-run with `--list`; and it prints the equivalent non-interactive command,
so the flags are learned by using them, not looked up. Flags always win —
anything already given on the command line is never asked about again — and it
never prompts without a terminal, never under `--json` or `--json-events`, and
never starts by itself: a bare `debark build` is a usage error naming the
flags it needs — one of `--snapshot`, `--base` or `--interactive` — not a
command that quietly waits on stdin.

> **Still no transcript, because no complete guided run has been captured.**
> The refusals above (no terminal, `--json`) are covered by tests in
> `internal/cli/cli_surface_test.go`, and the prompt mechanics have unit tests
> of their own. `snapshot from-base --interactive` has since been driven
> through a real pty, and that run earned its keep: it caught the base picker
> offering `desktop` as the default — the largest of the three claims, and the
> exact inversion of the err-toward-not-installed rule this feature rests on.
> That is fixed, and no unit test could have found it, because every option
> was present and only the order was wrong. Still missing is a recorded run to
> quote, and the fuller `build --interactive` flow — package entry, output
> path, signing key, review — has not been driven end to end at all. The house
> rule on this page is that a transcript is real output or it is not printed;
> there will be one here when there is a run to take it from.

The rest of this page stays in explicit commands.

## Workflow 1 — a never-networked machine

The case `apt-get install --download-only` cannot handle: a machine that has
never touched the internet, so there is nothing to compare "missing" against
except the machine's own installed state.

**On the target**, capture what it has. This, and every other transcript in
this section, is real output from an actual Debian 12 container run on
2026-09-03 (`hack/demo-airgap.sh`), not a constructed example:

```console
$ debark snapshot create --out site-42.snapshot.tar.zst
snapshot created: site-42.snapshot.tar.zst
  target:    debian 12 (bookworm) amd64
  installed: 88 packages
  digest:    sha256:fb02144688b0193f07ef946efc45942f940d78500a4aa21940dcfedf2dd56d3f
```

No root required. Move `site-42.snapshot.tar.zst` across the gap however you
already do (email it to yourself, USB, a jump host) — it is small, and by
default it carries nothing identifying beyond hostname-free package and apt
configuration state. Pass `--redact` to also drop the machine id and any
proxy settings, or `--label site=42` to attach your own tracking metadata
instead.

**On the builder**, resolve and fetch the packages you actually need:

```console
$ debark build --snapshot site-42.snapshot.tar.zst \
    --out ./site-42-bundle \
    --sign ~/.debark/release.key \
    jq
OK  ./site-42-bundle
  target:     debian 12 (bookworm) amd64
  packages:   3 packages in the bundle (3 added, 0 unchanged, 0 removed this run)
  size:       387.5 kB total, 387.5 kB fetched this run
  trust:      signed
```

`jq` pulls in whatever *this target's installed set* doesn't already satisfy
(here, `libjq1` and `libonig5`) — debark never re-derives that; apt does,
running against the snapshot's own `dpkg_status` in a private root. See
`docs/security-model.md` for what "signed" means and why it is worth doing
before the bundle ever leaves the builder.

Copy `./site-42-bundle` to removable media.

**Back on the target, with the network off:**

```console
$ debark verify ./site-42-bundle --key operator.pub
OK  ./site-42-bundle
  target:  debian 12 (bookworm) amd64
  signed:  valid by 47110757540aeb75 (ed25519-file) [ed25519]
  checked: 12 files (417.2 kB)

$ debark install ./site-42-bundle --key operator.pub --yes
OK  installed ./site-42-bundle
  verify:    OK
  target:    debian 12 (bookworm) amd64
  to install: 3
  to upgrade: 0
  unchanged:  0
```

`install` calls `verify` itself and refuses to let apt near the repository if
it fails — you do not need to remember to run them in order. This is not a
claim taken on faith: the same demo run went on to actually execute the
`jq` it had just installed, with `--network none` on the container the whole
time.

## Workflow 2 — a vendor `.deb` with missing dependencies

The other case the incumbent (`apt-offline`) cannot handle at all: a vendor
ships a `.deb` that isn't in any archive, and it depends on things that
*are*. This is the design the CLI is built toward — the command below is
the intended shape:

```console
$ debark build --snapshot site-42.snapshot.tar.zst \
    --out ./vendor-bundle \
    --sign ~/.debark/release.key \
    ./vendor/acme-agent_4.2.0_amd64.deb \
    --digest https://vendor.example.com/acme-agent_4.2.0_amd64.deb=<sha256>
```

> **Fixed on 2026-09-03; this path works.** It was genuinely broken until
> then: every external input form — a direct path, `--local-dir`, or a URL —
> failed with `engine: store <file>: ...: no such file or directory` (exit
> 2). The cause is worth knowing, because it is a real apt behaviour rather
> than a typo: a package apt acquires over a `file:` URI is used **in
> place** and is never copied into `Dir::Cache::Archives`, but the local
> backend computed every selection's staged path as though it had been. The
> container backend already handled this correctly; the local one now does
> too. Verified after the fix against a real Debian 12 target: a vendor `jq`
> package resolved its own `Depends` and produced a three-package bundle
> (`jq`, `libjq1`, `libonig5`).

A local path or an `https://` URL is meant to be fetched or copied, verified
to actually be a `.deb`, and indexed as a private staging repository so apt
resolves *its* `Depends`/`Recommends` against the real archive alongside
your other requests. `--digest URL=SHA256` upgrades that URL's provenance
from "unverified" to "matches an operator-supplied digest" — HTTPS on its
own proves nothing about who signed the file, only that the bytes were not
altered in transit. A vendor `.deb` you cannot get a digest for is meant to
still build; `verify` and the bundle's own README are meant to simply record
the weaker claim honestly rather than pretending otherwise.

If the vendor package needs something the target's configured archives don't
carry, `build` is meant to exit **3** (bundle built but incomplete) and name
exactly what's missing in `unresolved` — check `debark doctor` too, which
flags whether the vendor package looks like it wants the network at install
time, uses DKMS without matching headers, or carries redistribution terms
(`multiverse`/`restricted`/`non-free`) worth knowing about before you ship
the media. `doctor`'s own checks are implemented and exercised independently
of the vendor-.deb build path above (`core/doctor`), so this part is not
affected by the defect described there.

## Workflow 3 — refreshing last month's bundle

Bundles are incremental by default: run the same `build` again and only what
changed is fetched.

```console
$ debark build --snapshot site-42.snapshot.tar.zst \
    --out ./site-42-bundle --sign ~/.debark/release.key \
    --update \
    jq tree
OK  ./site-42-bundle
  target:     debian 12 (bookworm) amd64
  packages:   4 packages in the bundle (1 added, 3 unchanged, 0 removed this run)
  size:       439.9 kB total, 52.5 kB fetched this run
  trust:      signed
```

(Real output, same target as Workflow 1: the bundle already held `jq` and
its two library dependencies; adding `tree` to the request fetched just the
one new package. Nothing was superseded here, so nothing was pruned — see
the prose above for what `--update` prunes when a newer version *does*
replace an old one.)

`--update` re-resolves against the current state of the target's configured
archives, fetches anything newer, and prunes superseded files afterward —
per `(name, arch)`, the highest version by dpkg's own comparison is kept, and
two things are never pruned regardless of version: anything you supplied
yourself (a local `.deb`, a `--digest`-verified URL), and whatever the
build's own `lock.json` names — so if your target's pins select an older
version than one already sitting in the bundle, the pinned version you
actually asked for survives the prune. Pass `--no-prune` to keep every
version around instead. Without `--update`, a rerun is purely additive: it
adds what's missing and touches nothing else, which is the right default
when you're topping up a bundle with one more package rather than
refreshing it.

One consequence of that exemption is worth knowing, and the build tells you
about it. When the lock names the *lower* of two versions in the pool, the
prune protects the lock's version — and the higher one is not removed
either, because it is already the highest of its `(name, arch)` group and
nothing supersedes it. So it stays on the media.

The commoner case needs no pin at all. If last month's build asked for `jq
tree` and this month's asks for only `jq`, `tree`'s `.deb` is still sitting
in the pool, nothing supersedes it, and `--update` keeps it: a package you
stopped asking for does not leave by itself.

Either way the leftover is a real file in a real bundle — the manifest lists
it, the signature covers it, and `verify` accepts it — but it is not in
`Packages`, so the target's apt cannot see or install it, and it appears in
neither `last-run-added.txt` nor `last-run-removed.txt`, because this run
neither added nor removed it. That is why it gets a report of its own. Every
pool file `repo/Packages` does not index is written to
`last-run-unreferenced.txt`, one repo-relative path per line, sorted, beside
its two siblings in the bundle root and covered by the manifest and the
signature like everything else. It is also raised as a `pool.unreferenced`
warning in `lock.json`, which carries it into the `build` output,
`README.txt`'s Warnings section and `debark inspect` — so you are told
without having to know this failure mode exists first. The warning names the
file, and where the same package *is* indexed at another version, it names
both versions. The bundle is slightly larger than it looks, and now it says
so. Building into a clean output directory avoids the situation entirely,
since there is then no leftover to survive.

`store gc` reclaims space in the local content-addressed store once you no
longer need old bundles' objects:

```console
$ debark store gc ./site-42-bundle
removed 0 objects, freeing 0 B (kept 4 objects)
```

(Real output from the same run: nothing was superseded, so `gc` correctly
kept all 4 objects — the 3 from Workflow 1 plus `tree`'s own file from the
refresh above. A bundle whose `--update` runs actually replace a package
with a newer version, instead of only adding one, is what makes `gc` have
something to remove; see `debark store gc --help` for `--dry-run`.)

Objects referenced by any bundle you name, plus anything you supplied
yourself, are always kept; debark does not maintain a registry of every
bundle you've ever built, so name the ones you still care about.

## Workflow 4 — a target that does not exist yet

The case where there is nothing to snapshot: fifty machines being installed at
a site with no network and needing software on first boot, or a first
evaluation of debark before anyone has been to the air-gapped side at all.
Workflow 1 remains the right answer whenever the machine exists — it
*measures* the target, and this one only *assumes* it — so this is what you
reach for when you cannot do that, never instead of it.

> **Where this workflow's transcripts come from.** Not
> [hack/demo-airgap.sh](../hack/demo-airgap.sh), which is a Debian 12
> container, and which is the source of every other transcript on this page.
> These are from a run on 2026-09-06 on WSL Ubuntu 24.04.3 LTS (noble) amd64
> with apt 2.8.3 — a real installed system — resolving against the live
> `archive.ubuntu.com` and `security.ubuntu.com`, with debark at commit
> `38b5357` — a pre-publication revision, from a history that was squashed
> into a single commit when the project was opened, so it will not resolve in
> this repository. Every command below used `--backend local` on a host matching the
> target, and none of them was signed, which is why the bundle paths and the
> `trust:` line differ from Workflow 1's. What that run did *not* cover is set
> out at the end of this workflow.

Describe the target by its stock release. `snapshot list-bases` prints the
releases compiled into the binary, with the seed packages each one stands for
(abridged — four of the fifteen rows the real table prints, kept so that both
distributions' seeds are visible; the full listing is `minimal`, `server` and
`desktop` for each of `debian:12`, `debian:13`, `ubuntu:22.04`,
`ubuntu:24.04` and `ubuntu:26.04`, in that order, and the two closing lines
are verbatim):

```console
$ debark snapshot list-bases
BASE                  SEEDS                        DESCRIPTION
debian:12/minimal     apt init                     Debian 12 (bookworm) — a minimal install: the base system and apt, nothing chosen by an installer profile
debian:12/server      apt init openssh-server      Debian 12 (bookworm) — a server install: the minimal system plus the standard server set
debian:12/desktop     apt init task-gnome-desktop  Debian 12 (bookworm) — a desktop install: the minimal system plus the default desktop environment
ubuntu:24.04/desktop  ubuntu-desktop-minimal       Ubuntu 24.04 (noble) — a desktop install: the minimal system plus the default desktop environment

architecture: amd64 (use --arch to see another)
note: a base is an assumption about a stock release, not a measurement of a machine: capture a snapshot of the real target when you can, and use a base when the target does not exist yet
```

That closing note is the command telling you the same thing this workflow
does, at the moment you are choosing a base: an assumption is not a
measurement.

Then build in one step, naming a base where Workflow 1 named a snapshot:

```console
$ debark build --base ubuntu:24.04/minimal --backend local --out ./bundle --no-sign jq
assumed base ubuntu:24.04/minimal: 147 packages assumed installed on the target
OK  ./bundle
  target:     ubuntu 24.04 (noble) amd64
  packages:   3 packages in the bundle (3 added, 0 unchanged, 0 removed this run)
  size:       379.9 kB total, 379.9 kB fetched this run
  trust:      unsigned
```

The first line is the entire difference from Workflow 1's build: it says out
loud that this bundle was resolved against an assumption, and how big the
assumption was. Everything below it is the same four answers Workflow 1 gives.
(The standard unsigned-bundle warning that follows any `--no-sign` build is
trimmed from that transcript. No base build has been signed yet, so there is
no signed variant of it here to show.)

`--snapshot` and `--base` are mutually exclusive and exactly one is required:
they are two ways of naming the same thing, which machine this bundle is for.
`--base` synthesizes a snapshot from that base's seed packages — resolved by
the release's own apt in a private root where nothing is installed, never by
debark itself — and then enters Workflow 1's pipeline unchanged. There is
one resolution path and one input artefact; nothing after the synthesis knows
or cares that the snapshot was not captured. `--arch` names the architecture
the base describes, defaulting to the builder's own.

A base id is `<distro>:<version>/<variant>` — `debian:12/server`,
`ubuntu:24.04/desktop` — and a bare `ubuntu:26.04` means the `minimal`
variant, the smallest of the three claims. Any baseline the builtin table does
not carry is written as a definition file and passed by path, which is usually
the better answer anyway, because an organisation's real baseline is its own
golden image rather than stock Ubuntu:

```sh
debark build --base ./our-soe.yaml \
    --out ./site-42-bundle --sign ~/.debark/release.key jq
```

That file format is `debark.base/v1`, with a worked example in
`docs/formats.md` §3.13.

When you want the snapshot itself rather than a bundle — to inspect or diff
it, to derive a base once and reuse it across many builds, or to commit it so
a fleet provably builds against the same assumed baseline — produce it
explicitly:

```console
$ debark snapshot from-base ubuntu:24.04/minimal --backend local --out ubuntu-2404.snapshot.tar.zst
snapshot synthesized: ubuntu-2404.snapshot.tar.zst
  base:      ubuntu:24.04/minimal (builtin)
  target:    ubuntu 24.04 (noble) amd64
  assumed:   147 packages installed on the target
  resolver:  local
  digest:    sha256:4b2a796a8f4e5b8e50709ebde9b1f324293d44c6365028684ea4f39e81b00555
  assumption: a base is an assumption about a stock release, not a measurement of a machine: capture a snapshot of the real target when you can, and use a base when the target does not exist yet
```

About seven seconds, and not one `.deb`: synthesis reads index metadata to
work out what a stock install would already have, and fetching the packages
themselves is `build`'s job. What it writes is an ordinary snapshot, which
`snapshot inspect` reads like any other — except that it says where it came
from, and warns about it:

```console
$ debark snapshot inspect ubuntu-2404.snapshot.tar.zst
ubuntu-2404.snapshot.tar.zst
  origin:      synthesized base ubuntu:24.04/minimal (builtin), assuming 147 packages installed
  target:      ubuntu 24.04 (noble) amd64
  apt/dpkg:    2.8.3 / 1.22.6
  created:     2025-09-04T15:33:20Z
  digest:      sha256:4b2a796a8f4e5b8e50709ebde9b1f324293d44c6365028684ea4f39e81b00555
  installed:   147 packages
  sources:     1
  preferences: 0
  keyrings:    1 (3 fingerprints)
  warnings:    1
    - this snapshot was synthesized from a base definition, not captured from a real machine: the bundle built from it is complete only if the target really is a stock install of this release
```

Two lines in that output need a word of explanation. `created` reads 2025
rather than the run date because the run pinned `SOURCE_DATE_EPOCH` for a
reproducible artefact, and the pinned value is what lands in the snapshot; it
is quoted here unaltered rather than tidied. And `digest` matches the one
`from-base` printed a moment earlier, because it is the same file being read
back.

That snapshot then feeds `build --snapshot` exactly as a captured one does:

```sh
debark build --snapshot ubuntu-2404.snapshot.tar.zst \
    --out ./site-42-bundle --sign ~/.debark/release.key jq
```

Both entry points call the same synthesizer, so those two routes are meant to
produce the same bundle — "meant to" because they have not been run against
each other and byte-compared, only run separately.

**What you give up, precisely.** A captured snapshot proves the bundle is
complete for that machine, because apt was handed that machine's own installed
set and answered with what is missing *there*. A synthesized one is only as
good as "the target really is a stock install of this release", and the two
ways of being wrong are not symmetric. A machine with *more* installed than
the base assumed gets a bundle larger than it needed to be, which costs space
on the media and nothing else. A machine with *fewer* gets a bundle that is
short, and it fails at the gap with an unmet dependency — the failure this
tool exists to prevent. The builtin bases therefore err downward on purpose:
each variant seeds the smaller of two plausible metapackages
(`ubuntu-desktop-minimal`, not `ubuntu-desktop`), `Recommends` is off, and a
package a measurement has caught the closure claiming wrongly is excluded by
name. E8 puts numbers on how much of a machine a base actually accounts for:
on Ubuntu 24.04, 147 assumed packages against a real 588 for `minimal`, and
802 against 1,809 for `desktop`. Read `--base` as "assume a quarter to a half
of a real install and carry the rest", not as "assume a real install".

The artefact says which kind it is, permanently and inside what the signature
covers: `origin.kind` is `synthesized`, `origin.base_id` and
`origin.source_digest` record which definition produced it and its exact
content, and the snapshot carries a warning saying so — the `origin:` and
`warnings:` lines in the `snapshot inspect` transcript above — so that
somebody reading the file months later is told without having to ask.

On the target, the same honesty is enforced where it matters. `install`
compares the base's assumed package list against the real dpkg status in front
of it and reports how far apart they are. This is a bundle built against
`ubuntu:24.04/desktop`, checked on a machine that is not a desktop:

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

Read what that did. 433 packages the build treated as already present are not
on this machine, so the bundle may be short — and that is said here, on the
near side of the gap, by a command that changes nothing, instead of arriving
as an apt unmet-dependency error on the far side where there is no network to
fix it from. Converting the one failure this tool exists to prevent into a
legible warning before anyone carries the media anywhere is the entire reason
the assumed path is allowed to exist. It warns and continues rather than
refusing, following the rule that doctor-style findings warn while policy
fails.

Nothing is wrong with the base itself, incidentally:
[E8](experiments/E8-base-fidelity.md) measured this same desktop base against
Canonical's published desktop image and found nothing it assumes that a real
desktop install lacks. This machine is not a desktop, which is precisely the
divergence the check exists to find.

The matching case reads the same way and says the opposite — the bundle built
from `ubuntu:24.04/minimal`, on a machine that really does have all of it:

```console
  base:       ubuntu:24.04/minimal (assumed 147 installed, 0 not present here)
  warning: this bundle was built against an assumed base (ubuntu:24.04/minimal), not a snapshot of this machine
  warning: all 147 packages the base assumes are present on this machine - nothing it assumed is missing
```

The first warning survives the good case on purpose. An assumption that turns
out to have held is still an assumption, and the bundle should not start
claiming otherwise because it got lucky. `docs/formats.md` §3.1 and
`docs/security-model.md` set out exactly what each kind of snapshot does and
does not guarantee.

> **What is still not real here.** `install --status` has been run against
> bundles built from a base; `install` proper has not, so no bundle built from
> a synthesized snapshot has ever been installed on a machine. The container
> backend — the path every macOS, Windows and cross-release build takes — has
> now run `from-base` and `build` once each, from Windows, against
> ubuntu:24.04/minimal (2026-09-06, Docker 29.6.2, both exit 0); macOS has
> still never been tried and the backend's unit tests still drive a fake. And
> only Ubuntu 24.04 amd64 has
> been run: Debian 12 and 13, Ubuntu 22.04 and 26.04, and arm64 are untried,
> including whether Debian's `apt`, `init` and `task-gnome-desktop` seeds
> resolve at all. [E8](experiments/E8-base-fidelity.md) measures fidelity for
> Ubuntu 24.04 amd64 only and has its own limitations section; [the README's
> Status section](../README.md#status-and-known-gaps) is the single list of
> what is still missing.

## Security demo — a tampered USB stick

The property that makes the media untrusted-by-default worth trusting: apt
never reads a byte of the bundle's repository until the manifest's signature
and every file's digest have already checked out.

Build and copy a small bundle to a USB stick, then tamper with it as an
attacker with physical access might — swap one `.deb` for a different build,
or hand-edit a byte of `lock.json`. The transcript below is real: same
Debian 12 bundle as Workflow 3, with 8 bytes flipped inside the pooled `jq`
`.deb`:

```console
$ debark build --snapshot site-42.snapshot.tar.zst --out /media/usb/bundle \
    --sign ~/.debark/release.key jq tree

$ dd if=/dev/urandom of=/media/usb/bundle/repo/pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb \
    bs=1 count=8 seek=100 conv=notrunc
```

Now verify it, from the target, before installing anything:

```console
$ debark verify /media/usb/bundle --key operator.pub
FAILED  /media/usb/bundle
  target:  debian 12 (bookworm) amd64
  signed:  valid by 6bf9bac9b6555053 (ed25519-file) [ed25519]
  checked: 13 files (472.1 kB)
  problem: repo/pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb: file digest does not match the manifest
      expected f2303584378ac85f6d3a9ae8e46412196061681e81610d3b020abe4b5d389eb0, got 32c81c8f70f8910bf60821899c80d2ba794cf699e28712c59c58393df6724df8
```

Exit code **4**. The signature over the manifest is still cryptographically
valid — the manifest itself wasn't touched — but the manifest's own record of
that file's digest no longer matches the bytes on the stick, and verification
stops right there. `install /media/usb/bundle --key operator.pub --yes`
performs the identical check first and refuses for the same reason
(`install: bundle failed verification`, the same exit code 4) — confirmed
against this same tampered bundle; apt never sees the tampered repository at
all. (Leaving `--key` off entirely, rather than tampering with the bundle,
produces a different, equally real result: `signed: no` and `no signature
verifies against a trusted key` — verify never assumes trust it wasn't
explicitly given.) Try tampering with `debark.manifest.json` itself instead
(edit one character and re-save) and the signature check fails first, before
digests are even compared — see `docs/security-model.md` for the full order
of checks and why it stops at the first hard failure.

## Where to go next

- `docs/security-model.md` — what "signed" and "verified" actually mean here,
  and where the trust boundary really is.
- `docs/faq.md` — why this exists instead of `apt-offline`, snaps, or solving
  dependencies on Windows.
- `debark <command> --help` — every command documents its own flags; the
  full tree is in `debark --help`.
- `debark config init` — write a config file once so `--backend`,
  `--sign` and your keyring locations don't need repeating on every build.
