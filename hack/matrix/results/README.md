# Matrix results

> **Commit revisions below refer to the pre-publication history.** That
> history was squashed into a single commit when the project was opened, so
> the short SHAs quoted here do not resolve in this repository. They are kept
> as written because they are the evidence anchors this record was assembled
> against — read them as ordering ("before this change", "after it"), not as
> something to `git show`.

`run-2026-09-07.json` — **the current run**, and the first since the eight
commits `bf51914..5d26dac`: 32 rows, `29 pass, 0 fail, 1 blocked, 2 skipped,
0 timeout, 0 aborted`, process exit 1. **No row failed.** The three rows
without a verdict are all `basic-install-matrix` on arm64 and all have one
cause outside debark. Read this one, and read its section below before
reading `exit 1` as a product signal.

`run-2026-09-07-arm64.json` — the same fixture across all ten of its
release/arch rows, re-run once emulation had been re-registered, so those
three rows have a verdict too: `9 pass, 1 fail`. Not a second signal — the
completion of the first. Its one failure is ubuntu 22.04/arm64 at install,
and it is this host's QEMU emulation, reproduced with no debark involved.

`run-2026-09-07-determinism.json` — all ten `basic-install-matrix` rows re-run
after `checkDeterminism`'s second output directory gained an underscore, since
that changes what every determinism row tests: `10 pass, 0 fail`, exit 0, all
ten `determinism_ok: true`. It also re-decided the one row the arm64 supplement
failed — see below.

`run-2026-09-06.json` — the previous run, and the first fully green one:
32 rows, `32 pass, 0 fail, 0 blocked, 0 skipped, 0 timeout`, process exit 0.
Still the reference for per-row timings.

`run-2026-09-05.json` — the first VALID run (17 pass / 13 fail of 30). Kept
because the four causes behind its 13 failures are documented below and are
the reason the suite now says anything at all; not a current signal.

`rerun-debian12.json` and `result.json` — **INVALID. Do not read either as a
product signal.** Kept for the post-mortem below.

## run-2026-09-07.json — the first run since the eight commits

32 rows, 2 workers, `--row-timeout 25m`:
`29 pass, 0 fail, 1 blocked, 2 skipped, 0 timeout, 0 aborted`. Process exit 1.
14m26s of wall clock for 28.1 minutes of row time. The environment preflight
passed in 4.7s, all seven determinism rows came back `true`, and the harness
swept everything it created — `docker ps -a --filter
label=debark.e2e.run=red8ad231` and `docker images --filter
reference=dfe2e-red8ad231-*` both return nothing.

**Run against a fixed revision, not a working tree.** A `git archive` of
`5d26dace0dacf4ab1084cfdfdcdab678ff28b4b8` ("Stop a slow network changing the
bundle id") unpacked into a scratch directory. That mattered more than usual
here: other work was writing into `debark/` throughout, nine files were
modified in the working tree while the run was in flight, and a further commit
(`61b3e7b`) landed on master before it finished. A run against a tree that
moves under it measures nothing. The v1 result schema has no field for the
revision under test, so it is recorded here; a `source_sha` on `RunConfig`
would be an additive change worth making, and a run artefact that cannot say
what it measured is one edit away from being unreadable.

### The three rows with no verdict are one host condition

`basic-install-matrix` on ubuntu 22.04/arm64 is `blocked`; ubuntu 24.04/arm64
and ubuntu 26.04/arm64 are `skipped`. All three are QEMU binfmt registration
disappearing from the host *while the run was in flight*. Emulation was
registered and proved before the run started (`docker run --platform
linux/arm64 debian:bookworm-slim uname -m` → `aarch64`), and debian 12/arm64
and debian 13/arm64 both passed using it. The blocked row's transcript dates
the loss to the second:

```text
--- keygen operator: exit 0 (success) in 337.1986ms ---        # emulation fine
--- keygen wrong (decoy): exit 255 (unknown(255)) in 161.4141ms ---
exec /debark: exec format error                              # gone
--- build: exit 255 (unknown(255)) in 154.9636ms ---
exec /debark: exec format error
```

Two consecutive `docker exec`s into the *same* arm64 container, about a second
apart, on opposite sides of the loss. `docker run --privileged --rm
tonistiigi/binfmt` immediately afterwards reported `"emulators": null`. The
Docker daemon had not restarted — containers belonging to unrelated work had
been up 44 hours throughout.

**This host loses binfmt registration spontaneously.** It was already known to
be cleared by a Docker Desktop restart; it is now also known to go while the
daemon stays up. Re-register it and *prove it took* immediately before a run,
and treat an arm64 row that dies on `exec format error` as this, never as
debark.

### `blocked` overstated what a runtime-chosen exit says — since FIXED

That row made the process exit 1, which `../README.md` defines as "a product
signal", and filed the row as `blocked`, defined there as "a stage errored in
a way that specifically looks like an unimplemented product capability".
Neither was true of it. The harness already knew better in its own prose — the
blocker reads "exit 255 is not one of debark's own exit classes (0-7), so
the product did not choose it — the container runtime or a signal did" — but
that knowledge never reached the status. It is the same shape as the
mislabelling already fixed for timeouts, where a killed row was filed with the
killed process's own exit status and nine rows claimed a usage error that had
never happened.

**Fixed after this run.** `harness.RowResult.RuntimeError` records the fact
inside the row, where the evidence still exists, and `hack/matrix` classifies
it as the new `errored` status counting toward exit 3, "no verdict: check the
host" — additive to schema v1 on the same terms `timeout` and `aborted` were.
A row carrying both flags is a `timeout`, because SIGKILL is 137 and a killed
row very often leaves a runtime-chosen exit behind, and the timeout is the
status whose repetition tells a reader the host is sick. **The row in
`run-2026-09-07.json` is left exactly as it was recorded**: a result file
records what the run reported, and re-labelling it afterwards would make the
artefact disagree with the run that produced it. A re-run of that row today
would say `errored`, and the process would exit 3.

### run-2026-09-07-arm64.json — the arm64 rows, with emulation held

Ten rows, 2 workers, `--row-timeout 25m`, binfmt re-registered and re-proved
first: `9 pass, 1 fail, 0 blocked, 0 skipped, 0 timeout`. Every arm64 row ran
this time, so the three rows above have verdicts, and all ten determinism
checks passed. Swept clean.

The one failure is **ubuntu 22.04/arm64 at install**, and it is this host's
emulation, not the product:

```text
install: exit 5 (resolution): apt-get exited 100: dpkg: error processing
package libc-bin (--configure): installed libc-bin package post-installation
script subprocess returned error exit status 139
```

139 is 128+11 — `libc-bin`'s postinst, which runs `ldconfig`, took SIGSEGV.
`ldconfig` is Ubuntu's binary; debark does not write it, call it or ship it.
Reproduced with no debark involved at all, three containers, one command:

| image | platform | `ldconfig` |
| --- | --- | --- |
| `ubuntu:22.04` | linux/amd64 | exits 0 immediately |
| `ubuntu:24.04` | linux/arm64 | exits 0 immediately |
| `ubuntu:22.04` | linux/arm64 | **still running after 120s** |

Only the release/arch pair the row failed on is broken, and it is broken
without debark in the picture. Everything the product did on that row was
right: the build succeeded, the determinism rerun was byte-identical
(`determinism_ok: true`), and `verify` returned `ok: true` over a signed
bundle of 14 files and 422,468 bytes. Only the emulated target's own dpkg
trigger crashed. The same fixture on the same release passed on amd64 in both
runs, and on arm64 for debian 12, debian 13, ubuntu 24.04 and ubuntu 26.04.

It passed on 2026-09-06 (287.9s, the slowest row of that run), so the
difference is the emulation registered on the host that day, not the code.
**Do not read this row as a product signal, and do not "fix" it in debark.**

**Confirmed intermittent, later the same day.** After the QEMU registration
was uninstalled and reinstalled, the same row on the same release and arch
**passed** (162.4s, `run-2026-09-07-determinism.json`), with the same product
code and the same fixture. Two different verdicts from one unchanged tree is
the definition of an environment-dependent row, and it retires the last doubt
about the triage above: a failure that a re-registration of the emulator makes
go away was never debark's. It also means a single red arm64 install row on
this host should be re-run before it is believed — one observation of it is
not evidence.

### What the eight commits changed, and what this run could see

The run is a green statement about the paths the fixtures drive. It is worth
being exact about which of the eight commits that covers, because for three of
them the honest answer is "none of it".

**Covered, and passing.**

- `5837d82` — `verify` on a non-bundle is Usage, not Verification. All six
  tamper rows still exit 4. The boundary was also driven by hand against a
  real bundle this run produced: intact → 0; the same bundle with
  `debark.manifest.json` deleted → **4**, which is the case exit 4 exists
  for; a directory carrying none of a bundle's parts → **1**, with `verify
  --json` writing zero bytes to stdout. No fixture passes `--json` to any
  stage, so the output-shape half of that change is invisible to the matrix.
- `d5f31cb` — `sources_fetched`/`sources_failed` on `apt.update`. Both are
  environment facts and both are stripped from the copy the bundle keeps.
  Confirmed in the artefacts rather than in the source: every `apt.update`
  event in every `evidence.json` this run wrote carries `{"failed": false}`
  and nothing else. Had the strip been missed, the determinism rows would
  have gone red exactly as `entries` made them go red on 2026-09-05.
- `bf51914`, `9c77bbe`, and the `fetch_failed` half of `1c767f7` — no fixture
  asserts stderr text or the buildjob document's shape, so none of these can
  move a row. `external-deb-missing-deps` still reaches `incomplete` with its
  `unresolved_contains` intact.

**Not covered at all: everything that needs a vendor URL.**

No fixture in this suite downloads anything over HTTP. `scenario.go` builds
each `request.vendor_debs` entry on the host and copies it into the builder as
a **local path**; `RequestSpec.URLs` exists and no fixture sets it. Measured,
not inferred: of the 35 `evidence.json` files this run produced, **zero**
contain a `progress` event, and the six vendor-`.deb` rows carry
`input.external` instead.

So `core/fetch`'s HTTP path is untouched by the matrix, and with it:

- **`5d26dac`, the determinism fix itself.** Its own commit message says the
  determinism matrix "resolves apt:jq, which apt downloads in its own root, so
  core/fetch never runs" — still true, and it means the one row carrying the
  determinism claim cannot see the class of defect that commit fixed.
- `034f709`, redaction of a vendor URL on its way into the signed bundle.
- The `reason`/`detail` half of `1c767f7`, which needs a download to fail.

This is a fixture gap, not a harness gap. `RequestSpec.URLs` is already
plumbed into `BuildArgs`, and `harness.RepoServer` already serves files to
containers over `host.docker.internal`. One fixture that fetches a vendor
`.deb` from it, carrying `determinism: true`, would put all three commits
under the suite — and would have made the progress-event defect a red row
instead of something found by reading `core/fetch`.

### The determinism claim, checked two ways the harness does not check it

`checkDeterminism` compares five files between `/work/bundle` and
`/work/bundle-determinism-2`. Both of that pair's known weaknesses — five
files rather than all of them, and two paths containing no character apt
percent-encodes — were closed by hand this run:

- **Every file, not five.** `diff -rq` over the full bundle tree, for four
  rows across three releases and both arches: all 16 files identical each
  time, `evidence.json` included, with the same `bundle_id`
  (`a5aebdd15ef70dca` debian 12/amd64, `44e53cdcf5e8cb7a` debian 13/amd64,
  `beb2bece02a167b8` ubuntu 22.04/amd64, `e8401ea9fa9e6901` debian
  12/arm64). The five-file check was not hiding a sixth difference.
- **A vendor URL at two network speeds, into two `_`-bearing paths.** The
  shape no fixture has, driven end to end through the real fetcher and two
  real `debark build` runs in one container — not through `5d26dac`'s own
  fake, whose event count is a parameter. One synthetic 6 MiB vendor `.deb`
  (sha256 `6ca05df6…`) served twice by one HTTP server, unpaced and then
  paced at 120 ms per 64 KiB, with `SOURCE_DATE_EPOCH` pinned and a separate
  `DEBARK_STORE_DIR` per build.

  The control comes first, because without it the comparison proves nothing:
  the live `--json-events` stream recorded **2** progress events on the fast
  run and **34** on the paced one, so the variable really was varied — and
  the operator's stream is still exact, which is the other half of the claim.

  With that established: `evidence.json` byte-identical at 5160 bytes and
  carrying zero progress events either time; `debark.manifest.json`,
  `lock.json`, `repo/Packages` and `repo/Release` all identical; and
  `bundle_id` **`bbcc3589b2c84cf6`** from both. That is `5d26dac` holding
  under a real download, and `aptURItoFileName`'s masking holding for an
  `--out` path containing `_`, which `bundle` vs `bundle-determinism-2`
  cannot test.

### One defect found while verifying, not a regression

**`build --digest` still splits on the first `=`.** `92ebdc2` fixed this for
`fetch`, which now splits at the LAST `=`, validates the sha256 at parse time,
and says so in its own flag help. `cmd_build.go` was not touched and still
registers the flag with pflag's `StringToStringVar`, then looks the digest up
by the full URL:

```text
operator wrote:  --digest https://example.invalid/x.deb?token=abc=<64 hex>
pflag parsed to: map["https://example.invalid/x.deb?token":"abc=<64 hex>"]
cmd_build.go does digests["https://example.invalid/x.deb?token=abc"] -> ""
```

The digest is silently dropped and the input's provenance is never upgraded to
`user-digest`. `build` is the command an operator actually makes a bundle
with, so `docs/security-review.md`'s LOW finding is half-closed, not closed.
Deliberately held rather than fixed: the flag's registration lives in
`cmd_build.go`, which carries unrelated uncommitted work.

No row covers it yet. `vendor-url-fetch` now gives the suite a URL input, so
covering it is one `build_flags: ["--digest", "<url>=<sha256>"]` plus a
`lock.json` assertion on `publisher_verification: user-digest` — worth adding
once the flag is fixed, and not before, since a fixture that asserts the
broken behaviour would have to be rewritten by whoever fixes it.

### The gap is closed: `vendor-url-fetch`, and what it found on its first run

`test/e2e/fixtures/vendor-url-fetch.json` is the row this run's analysis said
was missing. `RequestSpec.VendorURLs` builds a vendor `.deb` on the host
exactly as `vendor_debs` does, but serves it over this run's own `RepoServer`
and hands `debark build` an `http://` input, so `core/fetch`'s HTTP path is
finally on a row. It carries `determinism: true`, and the server paces every
vendor response after the first — deterministic in the request ordinal, which
is the only way a determinism row can vary the network. Served at one speed
both builds emit the same two progress events and the row is green whether or
not the drop exists; paced, build one emits ~2 and build two ~10.

Checked both ways rather than argued. Against HEAD the row passes in 23s with
`determinism_ok: true`. Against the identical tree with **only**
`liveOnlyTypes`' drop removed it fails at `determinism`:
`debark.manifest.json: differs between two builds of the same request (3241
vs 3241 bytes)` — equal length, differing content, the signature of the
defect. `evidence.json` is not in `checkDeterminism`'s five files, but the
manifest hashes it, so the manifest catches it.

**On its first run it found a credential in `lock.json`.** A vendor URL's
query string is written verbatim into `packages[].origin.uri` — inside the
bundle, hashed by the manifest, covered by the signature, carried across the
air gap. `evidence.json` is clean, so this is the other half of the leak
`034f709` closed, in a place that commit did not know about.

The cause is two writers of `lock.Package` and one of them redacting.
`core/engine/lockbuild.go` writes `Origin: redactOrigin(sel.Origin)`;
`core/bundle/assemble.go`'s own `buildLock` then rebuilds `l.Packages` from
the same selections with a bare `Origin: sel.Origin`, and the assembler's copy
is the one that reaches disk. `redactOrigin`'s doc comment still says "No
backend populates Origin.URI with a live vendor URL as of this writing", which
stopped being true when `attributeExternalOrigins` landed.

Reproduced outside the harness, one container, the real binary, no fixture:
build with one `http://` vendor URL carrying `?token=…`, then grep the bundle.
`lock.json` leaks it; `evidence.json`, `debark.manifest.json` and
`repo/Packages` do not. `fetch.RedactURL` handles the URL correctly when it is
called — it is simply not called on this path.

**The fixture's row is therefore red, and the assertion stays.** A row that is
red because the product writes a credential into a signed artefact is this
suite working. The fix belongs to `core/bundle`, not to this package.

### `liveOnlyTypes` and the retry notice — decided, and why

`liveOnlyTypes` drops the `progress` type, and `core/fetch` has **two**
emitters of it: the byte counter (`{url, bytes, total_bytes}`, empty message)
and the retry notice (`{url, attempt, attempts}`, "retrying URL (attempt
2/3)"). The comment justifying the drop describes only the first, so a reader
would not know the second exists — and a bundle now has no record that a
vendor download had to be retried.

**The behaviour is right and should not change; the comment is wrong.**
Reasoned, not measured, and labelled as such: how many times a download was
retried is a fact about the network at that moment, exactly like how many
250ms ticks it spanned. Admitting it into `evidence.json` would give two
builds of one request different bundle ids whenever one of them crossed a
flaky link — reintroducing the defect `5d26dac` closed, in a narrower form.
The bundle records what was built, not what the weather was; an operator who
wants the retries has `--json-events`, the stream this does not touch.

The real defect is that one event type carries two unrelated things, so the
deny-list cannot be precise and its comment cannot be accurate. Giving the
retry notice its own type would let the byte counter be dropped exactly and
the retry be judged on its own merits. That is a `core/fetch` change.

## run-2026-09-05.json — the first run that measured the product

30 rows, 2 workers, `--row-timeout 15m`:
`17 pass, 13 fail, 0 blocked, 0 skipped, 0 timeout, 0 aborted`.

Process exit was 1 (rows failed), **not** 3 (no verdict). Every row reached a
verdict of its own, the environment preflight passed in 5.9s, and the harness
swept every container it started. That is what makes this run readable as a
statement about debark rather than about the host — the distinction the
2026-09-03 run could not make.

The 13 failures were four separate causes, not one:

**1. Determinism, 8 rows** — every release/arch of `basic-install-matrix` that
got as far as the check. Four files differed: `debark.manifest.json`,
`debark.manifest.sig`, `lock.json`, `repo/Release` — each one *the same
length* in both builds, with `repo/Packages` identical. Equal length plus
differing content, only in the files carrying a stamp, is a timestamp, not a
reordering.

**Three** distinct causes were underneath, and each was only visible once the
one before it was fixed — which is why this needed a real run rather than a
reading of the code. All three are now fixed: re-running
`--fixture basic-install-matrix --release debian-12` after them gives
`2 total: 2 pass` on both amd64 and arm64, process exit 0.

  - *The harness never fixed the build clock.* `SOURCE_DATE_EPOCH` is exactly
    what core/engine/clock.go implements for this, and the harness did not set
    it, so two builds seconds apart stamped different times. Before the
    harness was fixed to produce a real verdict, this check passed by racing
    the clock — both builds landing in the same wall-clock second — which is
    why a defect this systematic had never been reported.
  - *A real product defect, found only after pinning the clock.* With the
    clock fixed, `repo/Release` became identical and three files still
    differed, all driven by one field: `lock.ClosedWorld.OutputDigest`. apt
    spells a source URI as a cache file name by turning `/` into `_` AND
    percent-encoding a character set that includes `_` itself
    (`_work_bundle_repo_._Release`), and `maskClosedWorldPaths` knew only the
    native and forward-slashed spellings. apt names those files in two
    ordinary success-path warnings, so the bundle's own output directory
    reached OutputDigest → LockDigest → BundleID → the signature over it.
    Two builds of the same request into different directories produced
    different bundle ids — the thing `requestDigestInput` already refuses by
    construction when it keeps `Output.Path` out of `RequestDigest`.
    Reproduced directly by mounting one real bundle repo at two paths and
    diffing the masked apt output; pinned by
    `TestMaskClosedWorldPathsSurvivesAnyOutputDirectory`. NOTE: the first fix
    for this modelled the encoding as a bare `/` -> `_` and was WRONG — apt
    percent-encodes `_`, so any `--out` path containing one still leaked. The
    test could not see it because it generated its fixture with the same rule
    it was checking. Fixed by reusing `aptURItoFileName`, which already
    existed in the same package.
  - *A second product defect, visible only after that one.* With lock.json and
    repo/Release both byte-identical, `evidence.json` still differed — same
    length, three events apart. core/engine pins every event it raises itself
    to the build clock, but a COLLABORATOR's event is not its to pin:
    core/apt reaches its sink through `evidence.Emit`, whose `New()` has
    already stamped `time.Now()` into `TS` before the event is handed over.
    So `apt.update`, `apt.resolve` and one `backend.selected`
    carried real wall-clock times twelve seconds apart while every
    engine-raised event around them sat at the pinned `created_at`.
    evidence.json is hashed by the manifest and the manifest is signed, so the
    bundle id moved. Fixed by wrapping the engine's sink (`buildClockSink`)
    rather than passing a clock into core/apt: an event cannot reach the
    build's collector without passing through it, whereas a new backend option
    could be forgotten. This restores the property evidence.go already claimed
    — "nothing that ends up in an artefact ever reads the real clock except
    effectiveCreatedAt, once" — for collaborators and not just for core/engine.

**2. Debian 13 build, 2 rows** — `exit 2 (environment)`: "host apt's effective
solver \"internal\" differs from the target's expected solver \"3.0\"". Both
sides here are a Debian 13 container, so this is the two solver-derivation
paths in `core/apt/solver.go` disagreeing about the *same apt*.
`defaultSolverForAPTVersion` infers `3.0` from apt major ≥ 3; the live probe
runs `apt-config dump`. Measured directly on `debian:trixie-slim`: apt is
3.0.3 and **`apt-config dump` contains no `APT::Solver` key at all**, so the
probe yields `internal` and the inference is simply wrong for trixie. Effect:
the local backend is unusable for every Debian 13 target, even on a Debian 13
host. **FIXED.** The defect was not the table's value but the gate comparing
an inference against a live probe of the SAME apt. The table is deliberately
left as it is — flipping `major>=3` would contradict E2's direct measurements
of apt 3.1.6 and 3.2.0 — and the gate now prefers the measurement when the
probed apt reports no configured solver and both sides share apt major.minor.

**3. Multiarch install, 2 rows** — `foreign-arch-i386` (apt-get exited 100)
and `multiarch-coexist` ("unmet dependencies … held broken packages"), both at
the install stage with exit 5 (resolution). **FIXED, and neither was the
product resolving badly.** Both bundles were correct; the harness was
installing them on a machine that was not the one they were built for (it
started the fresh target from the stock base image and never applied the
fixture's declared state). Proven by mounting each run's real bundle and
installing it both ways: without `dpkg --add-architecture i386` rc=100, with
it rc=0, same bundle. The same harness bug was ALSO making `held-package` and
`stale-installed-version` pass vacuously.

**4. Phased updates, 1 row** — `phased-updates` installed
`acme-phased-demo 2.0` where the fixture required `1.0`. **FIXED: the fixture
was wrong.** `docs/experiments/E1-*.md` had already measured that an
explicitly named `install <pkg>` bypasses phasing entirely — phasing governs
apt's automatic upgrade pass and nothing else — so the fixture asked by name
and then required the phasing-aware answer. Retargeted at the upgrade pass
rather than weakened, and that immediately surfaced a real product bug: apt
emits `Inst` lines with one bracketed group per reason, and the parser allowed
one, so `--upgrades` failed with exit 5 on all of Ubuntu 24.04.

## Before trusting a future run

Check that a stock container can reach its archive in a few seconds:

```bash
docker run --rm debian:bookworm-slim bash -c 'timeout 60 apt-get update -qq; echo rc=$?'
```

The harness now refuses to start when this fails (`--skip-env-preflight`
overrides it), so a repeat of the 2026-09-03 run should no longer be possible
without an explicit opt-out. Run the matrix in the background, never under a
hard `timeout`: killing it skips its cleanup and orphans containers.

Worker count is a real variable on this host, not a speed dial. The invalid
run used 4 workers with 19 running and 59 stopped containers already present;
this one used 2.

**Register QEMU immediately before the run, and prove it took.** Not once at
the start of a session:

```bash
docker run --privileged --rm tonistiigi/binfmt --install all
docker run --rm --platform linux/arm64 debian:bookworm-slim uname -m   # aarch64
```

The 2026-09-07 run did exactly that and still lost the registration fifteen
minutes in, with the daemon up the whole time — three arm64 rows, two
consecutive `docker exec`s apart. A second registration and a re-run gave all
five arm64 rows a verdict.

**Emulation being *available* is not emulation being *correct*.** On
2026-09-07 `ldconfig` in `ubuntu:22.04` under `linux/arm64` never returned,
while the same binary on `ubuntu:24.04`/arm64 and `ubuntu:22.04`/amd64
finished instantly. That surfaces in a row as `libc-bin`'s postinst exiting
139 at the install stage — a `fail`, with a message about dpkg, on a bundle
that built deterministically and verified clean. Before filing an arm64
install failure against debark, run the offending maintainer script's work
in a bare container of the same image and platform.

**Load is not the risk this host's history suggests.** The 2026-09-07 run
shared the machine with nine other work streams, twelve unrelated containers
and a pegged CPU, and was still 12% faster in aggregate than the quiet
2026-09-06 run (1661s against 1892s of row time over the 29 comparable rows).
Individual rows moved between 0.17x and 2.00x — that is contention noise, not
a trend — and the slowest row of all used 12% of a 25m budget. Contention on
this host shows up as variance, not as timeouts.

**Not every core commit is worth a re-run, and which ones are is checkable
rather than guessable.** The harness sets `Backend: "local"` at both of its
two build sites and nowhere else — it is already inside the target release's
container, so re-entering a second one would prove nothing. Every row in this
suite therefore runs the local backend, and nothing about the container
backend can move a row. That disposed of three of the four commits that landed
between `5d26dac` and `e1e1832`: `01ceb55` forwards the inner container's
event stream (no row starts an inner container), `a529c32`/`d13827f` move the
stale-archives-volume refusal from exit 4 to exit 2 for the shared named
volume the container backend mounts (no row mounts one), and `61b3e7b` is a
rename. Grep `Backend:` in `test/e2e/harness/` before spending forty minutes.

What DID warrant a re-run was a change to the harness itself: giving
`checkDeterminism` an output path containing `_` changes what every
determinism row tests, so all ten `basic-install-matrix` rows were re-run
rather than assumed — `10 pass, 0 fail`, exit 0, every `determinism_ok` true
(`run-2026-09-07-determinism.json`). Ten rows for ten minutes, against a
32-row grid for forty, is the trade worth making when you can say which rows
your change could reach.

**A run stopped without its sweep leaves containers behind.** Four containers
and two images labelled `debark.e2e.run=rbfc47cc4`, from a partial run that
was stopped deliberately rather than by a failure, were still up 17 hours
later and are not touched by a later run's sweep — the filter is by run id, by
design. They are safe to remove by that label when their owner is done with
them.

---

## Post-mortem: the 2026-09-03 invalid run (`rerun-debian12.json`)

Recorded after the fixes for vendor `.deb` ingestion, determinism, holds, and
the closed-world upgrade path had landed. It reports
`19 total: 0 pass, 9 fail, 10 blocked`. That number measures the network, not
the code.

Every single row ran for ~720 seconds — exactly the configured
`--row-timeout`. Nineteen independent rows do not all fail at precisely the
timeout unless something outside them is stalling.

Diagnosed directly, with no debark involved at all:

```
$ docker run --rm debian:bookworm-slim bash -c 'timeout 90 apt-get update -qq'
apt-get update rc=124 in 90s          # 124 = timed out
```

A bare `apt-get update` in a stock Debian container hung. Meanwhile, from the
same image: DNS resolved, TCP to port 80 connected, a raw
`GET /debian/dists/bookworm/Release` returned `HTTP/1.1 200 OK` instantly, and
forcing `Acquire::ForceIPv4=true` changed nothing. So the archive was
reachable and apt still stalled — most likely Docker Desktop's networking
degrading under load.

Two things that run did legitimately surface:

**1. The harness mislabelled a timeout as a usage error.** Nine rows reported
`exit 1 (usage), want 0 (success)`. Nothing returned a usage error; the
harness killed the build at its row timeout and recorded the killed process's
exit status as though the product had chosen it. Fixed: a killed row is now
`timeout`, a cancelled one `aborted`, and either makes the process exit 3
("no verdict") rather than 1 ("the product failed").

The first version of that fix decided "was this row killed?" by reading the
per-row context *after* `RunFixture` returned, and shipped with a known false
positive in the opposite direction: teardown runs on its own fresh context so
containers are removed even after a deadline, so a row that reached a genuine
`fail` verdict seconds before `--row-timeout` — and whose container removal
then crossed it — was relabelled `timeout`, turning exit 1 back into exit 3
and burying the row's own blocker. That is now closed too. `RunFixture` reads
its deadline the instant the row's work returns and before teardown, and
records the answer on the row as `timed_out`; the runner classifies from that
and uses the context error only to name *which* ending it was. Both
directions of the mislabelling are pinned by container-free tests in
`go test ./hack/matrix ./test/e2e/harness`.

**2. The product's own error classification is correct.** With the network
genuinely absent, `debark build` exited 5 (resolution failed) with a message
saying what happened — the right class.

`result.json` is older still (2026-09-03 15:17, 30 rows, `6 pass, 24 fail`),
from before the vendor-.deb and determinism fixes landed. It predates the
harness fixes too, so its statuses carry the same mislabelling.
