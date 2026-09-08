# hack/matrix — the nightly integration-matrix runner

Runs the fixtures (`../../test/e2e/fixtures/`) across the release/arch
support matrix, with bounded parallelism, and writes the JSON result the
nightly CI job publishes plus a short human summary table. It shares its scenario engine and result schema with
`../../test/e2e/harness/` — see that package's `scenario.go` and `result.go`
— so a row behaves identically whether it ran through `go test` or here.

```bash
go run ./hack/matrix                              # every row
go run ./hack/matrix --fixture alt-deps            # one fixture, its own release
go run ./hack/matrix --fixture tamper              # every fixture whose name contains "tamper"
go run ./hack/matrix --release debian-12           # every fixture that targets/matrixes onto Debian 12
go run ./hack/matrix --release noble                # matches by codename too
go run ./hack/matrix --workers 2 --row-timeout 15m  # a slower/shared host
go run ./hack/matrix --out results/nightly.json --quiet
go run ./hack/matrix --skip-env-preflight            # start rows even if the host's apt looks broken
```

No `DEBARK_E2E` needed — running this binary at all is already an explicit
choice to start containers (the guard exists to keep a bystander `go test
./...` container-free, which this is not).

## What it does

1. Loads every fixture, expands `target.matrix: true` fixtures across
   `core/distro.Supported()` x `target.matrix_arches` (default: just the
   fixture's own arch); every other fixture runs once, against its own
   stated release.
2. Runs the **environment preflight** before any row starts: one short-lived
   container (a release image the host already has cached) running
   `apt-get update -qq`, bounded by `--env-preflight-timeout` (default 60s).
   If a stock container cannot reach its apt archive, the run refuses to
   start and says so, instead of letting every row burn its full
   `--row-timeout` and report a failure that says nothing about debark —
   the run recorded in `results/README.md` cost four hours exactly that way.
   `--skip-env-preflight` bypasses the check for someone who knows the host
   is fine (or is deliberately testing an offline one). On a cold host with
   nothing cached the probe has nothing cheap to run and says so; the first
   image pull proves the network instead.
3. For each distinct (release, arch) pair actually needed, checks the image
   is available (pulling it if not, within `--image-timeout`) and — only for
   an architecture that isn't this host's native one — probes container
   emulation once (`harness.PlatformSupported`). A pair that fails either
   check marks every row that needed it `skipped` with the specific reason,
   and the run continues — a missing image or a host with no QEMU
   `binfmt_misc` registered (the exact condition found on the development
   host: `docker run --platform linux/arm64 ...` fails with
   `exec format error` unless something has already run
   `docker run --privileged --rm tonistiigi/binfmt --install all`) must
   never fail the whole run, only the rows that actually needed it.
4. Runs the remaining rows through a bounded worker pool (`--workers`,
   default 4 — container-minutes-bound work, not CPU-bound, so this does not
   scale with host core count by default). A row still running when
   `--row-timeout` expires is killed and reported `timeout` (below), never
   as whatever exit status the killed process happened to leave behind.
5. Writes the JSON result (`--out`, default `results/result.json`) and,
   unless `--quiet`, the summary table to stdout.
6. Sweeps every container this run's id touched, even if the process is
   about to exit non-zero — and on `SIGINT`/`SIGTERM` too: the signal
   cancels the run, in-flight rows tear down their own containers, and
   anything left is force-removed by label before the process exits.
   Nothing is ever pruned: the sweep filter is
   `label=debark.e2e.run=<this run's id>` and matches nothing else on a
   Docker host shared with other projects. Signal a second time to skip the
   30s wait and sweep immediately.

Exit codes — a nightly job's signal in one integer, without parsing the JSON:

- `0` — every attempted row passed or was cleanly skipped.
- `1` — at least one row is `fail` or `blocked`: a product signal.
- `2` — bad flags, no fixtures, or the result file could not be written.
- `3` — **no verdict**: the environment preflight refused to start, or rows
  ended as `timeout`/`aborted`/`errored`. Check the host, not the code.
- `130` — interrupted by `SIGINT`/`SIGTERM`; containers were swept first.

## The result schema (`debark.e2e.matrixresult/v1`)

```jsonc
{
  "schema_version": "debark.e2e.matrixresult/v1",
  "generated_at": "2026-09-03T21:43:09Z",
  "host": { "goos": "windows", "goarch": "amd64", "go_version": "go1.26.0" },
  "config": { "workers": 6, "fixture_filter": "", "release_filter": "" },
  "rows": [ {
    "fixture": "tamper-modified-deb", "protects": "tamper matrix: modified deb",
    "distro": "debian", "version": "12", "arch": "amd64",
    "status": "pass",                          // pass | fail | blocked | skipped | timeout | aborted | errored
    "stage": "done",                           // furthest stage reached
    "blocker": "",                             // set on anything but pass
    "exit_classes": { "build": "success", "verify": "verification" },
    "started_at": "...", "finished_at": "...", "total_ms": 29866,
    "stage_ms": { "build-binary": 1119, "target-setup": 3092, "snapshot-create": 455,
                  "target-commit": 4180,
                  "build": 9242, "bundle-transfer": 380, "tamper": 110, "verify": 217 },
    "bundle_size_bytes": 420427, "package_count": 3,   // the performance baseline
    "determinism_ok": null,                     // set only for fixtures with determinism:true
    "log_path": "C:\\...\\transcript.log",
    "timed_out": false,                         // omitted entirely unless true; see below
    "runtime_error": false                      // likewise; an exit code debark cannot produce
  } ],
  "summary": { "total": 30, "pass": 0, "fail": 0, "blocked": 30, "skipped": 0,
               "timeout": 0, "aborted": 0, "errored": 0 }
}
```

`timeout`/`aborted`/`errored` (the three `status` values and their three
`summary` counters) and the row-level `timed_out`/`runtime_error` were added **additively** to
`debark.e2e.matrixresult/v1`: no field changed name, type or meaning, so a
reader written against the original v1 still parses the document and still
sees the same top-level keys. `timed_out` is `omitempty`, so it appears only
on a row the harness stopped, `runtime_error` only on a row something other
than debark ended, and every normally-finished row's object is unchanged.
The invariant a reader can keep relying on: `total` equals the sum of all
seven buckets, now including the three added ones. Nothing else in this repository reads this
schema — `api/schema/` does not carry it, `docs/formats.md` does not document
it (whose "a published schema version is never mutated" rule governs the
product's own bundle formats, not the harness's), and
`.github/workflows/matrix.yml` builds and runs this binary and publishes the
JSON it writes, without reading the schema itself — so `hack/matrix` and this
page are the whole contract.

`stage_ms` and `bundle_size_bytes`/`package_count` are the performance
baseline the testing strategy asks for — recorded
for every row that gets far enough to produce them, not gated behind a
separate benchmark mode. The prototype's own recorded figure
(`docs/dev/prototype-baseline.md`: ~6.5s installing 74 packages/40MB) is
context to compare against once the product does enough to measure the same
shape of thing; this schema does not hard-code that comparison, since the
task is explicit that a fabricated one is worse than none: "Do not invent
comparisons; just record what you measure."

### `pass` / `fail` / `blocked` / `skipped` / `timeout` / `aborted` / `errored`

- **skipped** — never attempted: an image or emulator was unavailable, the
  run was cancelled before the row started, or `--fixture`/`--release`
  filtered the row out.
- **blocked** — a stage errored in a way that specifically looks like an
  unimplemented product capability (the debark binary would not even
  build, or a stage's output contains the literal string `"not
  implemented"`, the pattern every unimplemented stub in this tree uses). Expected
  for most rows until the command bodies land; not itself a bug in the harness.
- **fail** — every stage ran without an unexpected error, but an expectation
  did not hold (wrong exit class, a binary exited non-zero, two builds of
  the same request were not byte-identical). This is a real product bug once
  it happens.
- **pass** — every stage matched, every assertion held.
- **timeout** — the row was still running when `--row-timeout` expired and
  the harness killed it. **This is a statement about the host and the budget,
  never about debark**: nothing in the row reached a verdict. The `blocker`
  says how long it ran and how far it got, and quotes whatever the row had
  last recorded, demoted to the end of the line. Before this status existed,
  such a row was filed with the killed process's own exit status — nine rows
  of the run in `results/README.md` claimed `exit 1 (usage), want 0
  (success)`, which reads as a command-line bug that did not exist. Several
  rows timing out at the same limit is the signature of a sick host: re-run
  the environment preflight before reading any of it as product signal.
- **aborted** — the run was cancelled (`SIGINT`/`SIGTERM`) while the row was
  in flight. Same rule: no verdict, so it is not filed as one.
- **errored** — a stage exited with a code debark cannot produce: anything
  outside the frozen 0–7 exit table, so the container runtime or a signal
  chose it, not the product. `docker exec` reports 125/126/127 for its own
  failures, a killed process reports 128+signo (137 for SIGKILL, the shape
  an out-of-memory container leaves), and a platform this host cannot
  execute reports 255 with `exec format error`. Same rule again: no verdict.
  The `blocker` is left exactly as the harness wrote it, because unlike a
  timeout it already names the cause precisely.

  Until this status existed such a row was filed `blocked` and made the
  process exit 1 — "a product signal" — and the 2026-09-07 run spent a row
  and its exit code that way on QEMU binfmt registration evaporating between
  two consecutive `docker exec`s into the same container, with the Docker
  daemon up throughout. A row carrying both this and `timed_out` is a
  **timeout**: a killed process very often leaves 137 behind, and the
  timeout is both what happened and the status whose repetition across rows
  tells you the host is sick.

The line between `blocked` and `fail` is a heuristic (`harness.Blocked`),
not a certainty — read the `blocker` text either way. `timeout`, `aborted`
and `errored`, by contrast, are facts, reported by the harness from inside
the row: `RunFixture` reads its own deadline the instant the row's work returns
and **before** container teardown, and records the answer as the row's
`timed_out`; `reportStageFailure` reads the stage's exit code at the moment it
has it and records `runtime_error`. Both are decided where the evidence still
exists — by the time the runner sees the row, a runtime-chosen exit is
indistinguishable from a product-chosen one.

The runner does not re-derive that from its own context, and the reason is
worth knowing before changing it. Teardown deliberately runs on a fresh 90s
context so containers are removed even after a deadline, so by the time
`RunFixture` returns, the per-row context says the same thing for a row killed
mid-build and for a row that reached a real verdict seconds before the limit
and then spent longer than the remaining budget removing containers. Reading
it out here relabelled the second as `timeout` — exit 1 (a product signal)
became exit 3 (check the host), and the row's own blocker was buried under a
timeout message. `timed_out` is what separates them.

Every row the harness stopped also gets its own line in the console report,
under the table, with its stage and how long it ran — the place a human
actually looks.

### `last-run-unreferenced.txt` in a row's `blocker`

A `blocker` beginning `bundle-file:last-run-unreferenced.txt` is a fixture's
`expect.bundle_files` assertion against one of the three reports the build
writes at the bundle root. `last-run-added.txt` and `last-run-removed.txt`
record what the run **did**; `last-run-unreferenced.txt` records what the
bundle now **is** — every pool file `repo/Packages` does not index, one
repo-relative path per line, sorted.

That third report is the only place such a file is visible. It is on the
media, hashed by the manifest and covered by the signature, and the target's
apt cannot see it, so a file no single run touched appears in neither of the
other two reports; the same state is raised into `lock.json` as the
`pool.unreferenced` warning. Its usual cause is an incremental build into a
directory that already held an earlier run's packages. A finding here is a
product signal like any other `fail`: the bundle's own account of itself does
not match what a fixture built. See `harness.BundleFileCheck` for the exact
spelling and for the paired `lock.json` check.

## Checking the harness itself

`go test ./hack/matrix` covers the status classification, the additive JSON
shape, the signal handler and the preflight verdicts without starting a
single container. Two opt-in checks do use one container each, against a real
daemon:

```bash
DEBARK_MATRIX_DOCKER=1 go test ./hack/matrix -run Docker -v
```

They prove that the sweep removes this run's container and leaves every other
container on the host alone, and that the environment probe passes on a
healthy host (a few seconds) and cleans up after itself.

## Adding a release or architecture

Nothing to add here: the release table is `core/distro.Supported()` (frozen,
owned by another package), and `distro.Architectures()`/`distro.Platform()`
supply the arch-to-container-platform mapping. A fixture opts into the full
sweep with `target.matrix: true` / `target.matrix_arches` — see
`../../test/e2e/README.md`.
