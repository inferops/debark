# Measured performance

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

## Desktop simplification: measurement status, 2026-09-07

The historical measurements below belong to the recorded pre-simplification
binaries and corpora. They are retained as baselines. They are **not fresh
performance passes** for the three-stage desktop workflow. Current fixture
and benchmark results are recorded in the final measurement appendix.

The changed package path still searches and pages in Go and renders fixed
32px rows with a bounded recycled DOM. Empty-query entry now shows discovery
controls without requesting the whole catalogue. Full description is requested
only through `GetPackage` for Details; it is not added to search rows. A ready
catalogue is reused, and the controller deduplicates preparation for a target
generation. These implementation choices preserve the intended bounds, but
do not establish their elapsed time or memory cost.

The Node preparation-controller checks passed cache reuse, duplicate-start
prevention, cancellation/retry, failure and stale-target completion. Existing
fixture state checks and the scoped Go audit also passed. S2's
target Continue journal
records exactly one target commit and one preparation call on the captured
WebKitGTK snapshot. Its small fixture catalogue is not a performance corpus.

| Changed-path budget | Current simplification measurement |
|---|---|
| Cold start to interactive <1.5s | Accepted binary: ten new-process launches reached native accessible Continue at p95 258.566ms, below 1.5s. OS/library/profile caches were warm; first paint and truly cold startup are unmeasured. See [final-source startup](#final-source-native-startup). |
| Cache load <500ms | Current real-corpus Go benchmark: 5.152–5.992ms per warm disk load of a 75,176-entry cache v2. Meets this backend budget; not a Wails screen-ready measurement. |
| Search-to-render p95 <100ms | S3 demo: 24ms with a 5–15ms fake backend; 110ms with a 40–100ms fake backend. Real-catalogue Wails measurement remains pending. |
| Parse phase <10s after download | Current staged-corpus Go phase test: 328ms charged to parse, 859ms whole build; archive bytes were already resident. Meets this backend budget. |
| Bounded 60,000-row rendering | S3 demo kept 32 slots and at most 24 cached pages; no stale rows in 1,280 inspected rows. Physical-display smoothness remains unmeasured. |
| Idle resident memory <400MB | Current native loaded-catalogue session: 448.381MB summed RSS (427.609MiB), above budget. A matched baseline/cache-only comparison is still required before attributing a regression. |

Run the existing picker demo's `window.__BENCH__.run('checks')` for selection,
Details and preparation behavior, and its existing scrolling scenarios for
the virtualizer. `window.__SEARCH_UX_CHECK__()` in the search demo checks silent
category shortcut updates and the full query callback snapshot. Record those
as demo checks; they cannot stand in for live Wails end-to-end latency. The
final measurement record needs exact revisions/dirty hashes, binary/cache
identity, corpus size, OS/engine, viewport/zoom, machine load, repeated sample
counts and percentile method. Xvfb frame timing cannot certify physical GPU
smoothness. See [desktop visual review](dev/ui-review.md) for evidence levels.

## Historical measurements

The contract brief's budgets are requirements, and the definition of done says
each number must be **measured and recorded, not asserted**. This is the
record. Every figure here was measured; none is an estimate unless it says so.

Two kinds of measurement live here, and the difference matters. Most of it comes
from a benchmark in this repository. The four app-level figures — cold start,
the bridge, the real process's memory and scrolling — come from **the built
desktop binary driven inside a container**, because there is no way to take them
from a benchmark, and *How the app-level figures were taken* at the foot of this
file says exactly how, including where a temporary hook was on the path of the
thing it measured.

Re-run everything with the commands below — each section carries the one that
produced its numbers — and update this file rather than trusting it. There is
no `make bench`: the real-corpus benchmarks need a staged archive that is
deliberately not in the repository, so they are run by hand.

## Machines

Two, and which one a figure came from changes how it should be read. Every
section below says which.

**Machine A — the host.** Every Go benchmark in this file.

| | |
|---|---|
| CPU | Intel Core Ultra 9 285K (24 logical) |
| OS | Windows 11, `windows/amd64` |
| Go | 1.26.0 |

**Machine B — a Linux container on machine A.** Every app-level figure: cold
start, the bridge, the real process's memory, and scrolling in the shipping
engine. Added in the third round, because those four cannot be taken anywhere else.

| | |
|---|---|
| Image | `debark-shots:go126`, Ubuntu 24.04.4 LTS |
| Engine | WebKit2GTK 4.1 (`libwebkit2gtk-4.1`, 2.52.6), GTK 3 |
| Display | Xvfb, 1400×900×24, openbox, **no GL** |
| Rendering | software; `WEBKIT_DISABLE_COMPOSITING_MODE=1`, and `linux.WebviewGpuPolicyNever` in `main.go` |
| Go | 1.26.0, `linux/amd64` |
| Visible CPUs | 24 logical, shared with the host and every other container |

**Machine B is not a desktop, and this matters most for the frame figures.**
It has no GPU, its WebKit composites in software, and its CPU share is whatever
Docker's VM is getting from a Windows host that spends this project's working
day running other packages' test suites. Compositing cannot be turned on to check:
enabling it crashes the container's X stack, which is a known item on the third
round's backlog.

So the two halves of a frame measurement have to be read differently, and
*Scrolling* reports them separately for that reason. **The update pass is the
app's own work and it transfers** — a real machine's CPU will do the same or
better. **The frame delta is the renderer's and it transfers to nothing**: Xvfb
has no display to miss a vsync against, so a frame delta from machine B is
neither a promise nor a warning about real hardware.

Linux is the shipping target. The Go benchmarks above the app-level sections
were taken on machine A and should still be re-taken on Linux before release;
Windows is the harsher case for process spawning and file I/O and the easier
case for nothing in particular, so they are not obviously optimistic.

**Most figures here were taken on a machine that was concurrently running other
packages' test suites** — at times a dozen `go test` processes holding all 24
logical cores at 100%. Contention can only inflate a latency, never deflate one,
so where a measurement was repeated this file reports both ends: the best
observation, which is the closest estimate of the true cost, and the worst,
which is a real figure for a saturated machine.

For every Go benchmark, that choice changes nothing: the closest call is the
catalogue build's parse phase, and even the worst observation of that is 2.1×
inside its budget. **For cold start it decides the verdict** — 213 ms at p50 on
a quiet machine against 1,557 ms on a saturated one, either side of a 1.5 s
budget — so both are recorded in full and neither is called the answer.

Byte counts are exact. Where this file writes "MB" it means 10⁶ bytes, which is
what Go's benchmark `MB/s` figures use; the memory figures read KiB out of
`/proc` and are converted at 1,024 bytes per KiB before being written as MB.

## The real corpus

Everything in the sections marked **real** was measured against the actual
Ubuntu archive, staged outside the repository by `hack/fetch-indexes.go`:

```
go run ./hack/fetch-indexes.go -out <dir outside the repo> -only ubuntu
```

Ubuntu 24.04 LTS (noble), `amd64`, components `main` and `universe`, pockets
`noble` and `noble-updates` — which is what a real target enables, not just the
release pocket. Fetched from `http://archive.ubuntu.com/ubuntu/` at
`2026-09-07T01:08:55Z`; `MANIFEST.txt` in the staged directory carries the
SHA-256 of every file.

| Index | Compressed | Uncompressed | Stanzas |
|---|---:|---:|---:|
| `noble/main` `Packages` | 1,808,488 | 7,165,069 | 6,099 |
| `noble/universe` `Packages` | 19,315,644 | 73,379,142 | 64,755 |
| `noble-updates/main` `Packages` | 1,561,629 | 6,793,357 | 6,223 |
| `noble-updates/universe` `Packages` | 2,150,721 | 10,552,243 | 8,148 |
| **`Packages` total** | **24,836,482** | **97,889,811** | **85,225** |
| `noble/main` DEP-11 | 664,565 | 1,923,546 | |
| `noble/universe` DEP-11 | 5,942,598 | 18,564,799 | |
| `noble-updates/main` DEP-11 | 242,044 | 734,635 | |
| `noble-updates/universe` DEP-11 | 536,280 | 1,569,020 | |
| **DEP-11 total** | **7,385,487** | **22,792,000** | |
| **Whole first-run download** | **32,221,969** | | |

Parsed and merged by this repository's own loaders, that is:

| | |
|---|---:|
| Distinct binary packages | **75,176** |
| Stanzas read | 85,225 |
| Duplicate names (the `-updates` overlay) | 10,049 |
| Entries carrying a DEP-11 application name | 2,007 |
| Entries DEP-11 calls a desktop application | 1,882 (2.50%) |
| Distinct `Section:` values after stripping the component prefix | 58 |
| Unreadable lines, nameless stanzas, missing indexes | 0 |

The release-pocket stanza counts reproduce `docs/dev/index-formats.md` §2.1
exactly — 6,099 and 64,755, 70,854 together — so the archive has not moved
under the index research, and §2.1's correction is confirmed: a real target is
**75,176** packages, not 70,854. The synthetic corpora below were sized at
70,000, which is 7% short.

`internal/catalog/realcorpus_bench_test.go` holds every benchmark that reads
this corpus. All of them skip when `DEBARK_PERF_CORPUS` is unset, so CI never
needs 100 MB of archive:

```
DEBARK_PERF_CORPUS=<dir> go test ./internal/catalog/ -run '^$' -bench Perf
DEBARK_PERF_CORPUS=<dir> go test ./internal/catalog/ -run TestPerf -v
```

`TestPerfWriteCorpusJSON` turns the corpus into the `[]catalog.Entry` file the
search benchmarks re-point at (23,390,801 bytes of JSON for the 75,176 rows).

## Search — budget: < 100 ms p95, keystroke to rendered results

### Against a real `universe` index — the definition-of-done number

**Met. The worst query's p95 is 6.19 ms on a quiet machine and 13.92 ms at the
worst observation on a saturated one — 16× and 7× inside a 100 ms budget.**

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ -run TestPerfWriteCorpusJSON -v
DEBARK_BENCH_CORPUS=<corpus>/entries-noble-amd64.json \
  go test ./internal/catalog/ -run '^$' -bench SearchIndex -benchtime 1000x -count 5
```

75,176 real entries from Ubuntu noble `main` + `universe`, release and
`-updates`. Five runs of 1,000 queries each, on a machine saturated by other
suites; the last column is the single quieter run taken before they started.

| Query | Matches | p50 best | **p95 best** | p95 worst | p95, quiet machine |
|---|---:|---:|---:|---:|---:|
| Single character (`l`) | 70,473 | 2.51 ms | **5.43 ms** | 5.76 ms | 2.25 ms |
| Two characters (`li`) | 49,743 | 2.51 ms | **6.31 ms** | 6.93 ms | 3.36 ms |
| `lib` — the brief's worst case | 35,263 | 2.32 ms | **5.14 ms** | 7.08 ms | 3.42 ms |
| `lib`, deep page (offset 30,000) | 35,263 | 3.01 ms | **5.76 ms** | 7.84 ms | 4.24 ms |
| `lib`, maximum page size | 35,263 | 2.45 ms | **5.65 ms** | 7.80 ms | 3.52 ms |
| `browser` — the real worst case | 264 | 5.02 ms | **9.25 ms** | 13.92 ms | 6.19 ms |
| No match at all | 0 | 1.04 ms | **1.95 ms** | 3.17 ms | 1.51 ms |
| Empty browse | 75,176 | 0.001 ms | **0.001 ms** | 0.04 ms | 0.002 ms |
| Browse, 60,000 rows down | 75,176 | 0.001 ms | **0.001 ms** | 0.12 ms | 0.002 ms |
| Category filter | 7,140 | 0.002 ms | **0.002 ms** | 0.03 ms | 0.002 ms |
| Category + text | 6,470 | 0.19 ms | **0.25 ms** | 0.34 ms | 0.21 ms |
| Apps only | 1,845 | 0.10 ms | **0.15 ms** | 0.39 ms | 0.11 ms |

**The worst real query is `browser`, not `lib`** — and that inverts the
assumption the synthetic corpus was built on. `lib` matches 35,263 rows but
matches them *early*: it is three bytes and it usually hits in the package name,
so the scan stops almost immediately. `browser` matches 264 rows, which means
`strings.Index` runs to the end of nearly every one of the 75,176 folded
haystacks — name, application name and summary — before giving up. The cost of
a text query is dominated by the bytes it has to reject, not by the rows it
returns. The synthetic corpus hid this because its `browser` matched 2 rows out
of a much shorter summary text; the real archive's summaries are what make this
the expensive case, and it is exactly the query an operator looking for an
application types.

The whole distribution is roughly 2× the synthetic figures below, from two
causes and neither is a defect: 7% more entries, and real summaries that are
longer and more varied than generated ones, so there is simply more haystack.

Two properties survive the move to real data, and they are the ones that matter
for a per-keystroke budget:

- **One allocation per query**, and it is the returned page — 12,288 bytes for
  50 rows, or 114,573 for a 500-row maximum page. Nothing scales with the match
  count, so sustained typing does not degrade.
- **It scales across cores.** `BenchmarkSearchIndexParallel` runs the `lib`
  query from 24 goroutines and reports 0.20–0.27 ms per query of wall clock —
  the single-threaded latency divided by the parallelism. The index takes no
  lock, so a fast typist and a background rebuild do not serialise.

Index construction over the real corpus is **26.6 ms** for 75,176 entries
(50.0 ms on the saturated machine), against 8.0 ms for 70,000 synthetic ones.
It is paid once per catalogue build, inside the parse phase.

The remaining ~90 ms of the budget belongs to the Wails bridge and the DOM.
Both are now measured: see *Keystroke to rendered results* below. The bridge
wants 3 ms of the 90, not 90.

### Against the synthetic corpus, for comparison

`go test -run XXX -bench Search ./internal/catalog/`, against a synthetic
70,000-entry corpus from `internal/catalog/fake` sized to the release-pocket
archive (70,854 stanzas — `docs/dev/index-formats.md` §2.1).

| Query | Matches | p95 | Allocs/op |
|---|---|---|---|
| Single character | 59,414 | **1.10 ms** | 0 |
| Two characters | 37,617 | **1.19 ms** | 0 |
| `lib` — the worst realistic case | 31,921 | **1.38 ms** | 0 |
| `lib`, deep page (offset far in) | 31,921 | **1.62 ms** | 0 |
| `lib`, maximum page size | 31,921 | **1.37 ms** | 0 |
| A word matching almost nothing | 2 | **1.19 ms** | 0 |
| No match at all | 0 | **0.31 ms** | 0 |
| Empty browse | 70,000 | **~0 ms** | 1 |
| Category filter | 7,238 | **~0 ms** | 1 |
| Category + text | 7,238 | **0.08 ms** | 1 |
| Apps only | 1,054 | **0.08 ms** | 1 |

Re-run on the same machine on the same day as the real-corpus pass, this
reproduces: 1.25 ms for a single character, 1.45 ms for `lib`. The synthetic
corpus is a good model of everything except the length of a real summary.

## The app's own target — a bigger corpus than the benchmarks use

Everything above this line is measured against Ubuntu noble `main` + `universe`,
release and `-updates`: **75,176** packages. That is the corpus
`hack/fetch-indexes.go` stages, and it is the right thing for a Go benchmark to
hold still.

**It is not what the application asks for.** The app's default target is the
stock base `ubuntu:24.04/desktop`, and `core/base`'s `ubuntuSources` gives that
base four suites — `noble`, `noble-updates`, `noble-backports` from
`archive.ubuntu.com` and `noble-security` from `security.ubuntu.com` — across
`main restricted universe multiverse`. Sixteen `Packages` indexes and sixteen
DEP-11 indexes, not four and four.

Built by the running app against the live archive (machine B, catalogue built
through the UI's own `StartCatalogBuild`, not a test):

| | Measured |
|---|---:|
| Distinct binary packages | **85,565** |
| Indexes fetched | 16 `Packages` + 16 DEP-11 |
| `catalog.bin` on disk | **18,965,143 bytes** |
| Cache key | `ubuntu-24.04-amd64-f11ae74c8bb9` |
| **Whole first build**, `SelectTarget` → `Ready`, over the network | **20.05 s** |
| Warm start, same call sequence against the written cache | **158 ms**, 162 and 206 ms on repeats; 2.16 s once on a saturated machine |

85,565 is **13.8% more rows than the 75,176 the benchmarks use**, so every
app-level figure below is measured against a catalogue larger than the one the
Go numbers describe. That is the right direction for a budget check and the
wrong direction for comparing the two sets of numbers directly; nothing here
should be read as a regression against a section above.

Three figures are now measured on *this* corpus in a Go process as well as in
the app, so the two scales can be compared directly rather than extrapolated
between: the whole warm-start load at **66.1 ms** (*Cache load*), the loaded
catalogue's live footprint at **38.7 MB** (*Memory*), and what that load costs
the process's resident set, which is the subject of *What the load actually
costs the process*. Everything else on this page that says 75,176 is the
staged corpus and says so.

**The warm-start row is charged for the harness's filesystem.** The app under
machine B runs with `XDG_CACHE_HOME=/appcache`, which is a bind mount onto the
Windows host, so those 158–206 ms include reading 18,965,143 bytes across it.
A paired measurement under *Cache load* puts that penalty at **66 ms** on this
file — 121.9 ms median from the mount against 56.1 ms from the container's own
disk. An installed app reads its cache from the machine's own disk and would
not pay it. The row is left as measured rather than adjusted, because
subtracting one measurement from another taken a different way is how a
number stops being a measurement.

The 20.05 s first build is not comparable to the 6.09 s in *Catalogue build*
below either — four times the indexes, a container's CPU share, and a second
archive host. The budget that section is measured against is the **parse phase**,
which this run does not separate.

## `Packages` parse — budget: < 10 s parse phase

### Against the real indexes

**Met, using 0.5% of the budget at best and 1.1% at worst.**

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ \
  -run '^$' -bench 'PerfPackagesParse|PerfPackagesLoadAll' -benchtime 10x
```

`ParsePackagesIndex` over the decompressed bytes. Best of nine reported runs of
ten iterations each, across three passes. The worst observation was 1.9× the
best for the large `universe` index and up to 3.5× for the small ones, where a
few milliseconds of scheduling delay is most of the measurement.

| Index | Uncompressed | Stanzas | Parse | Throughput | Stanzas/s |
|---|---:|---:|---:|---:|---:|
| `noble/main` | 7,165,069 | 6,099 | **3.85 ms** | 1,859 MB/s | 1,582,347 |
| `noble/universe` | 73,379,142 | 64,755 | **40.3 ms** | 1,823 MB/s | 1,608,353 |
| `noble-updates/main` | 6,793,357 | 6,223 | **3.54 ms** | 1,918 MB/s | 1,756,967 |
| `noble-updates/universe` | 10,552,243 | 8,148 | **5.07 ms** | 2,080 MB/s | 1,606,372 |
| **All four** | **97,889,811** | **85,225** | **52.8 ms** | | |

Worst observed, all four: 112.3 ms — 1.1% of the budget.

This **replaces the 60–90 ms extrapolation** this file previously carried for
the 80.5 MB release-pocket corpus. The measured figure for those two indexes is
44.1 ms, so the extrapolation was conservative by about a third — the real
archive parses faster than the fixtures predicted, because the fixtures are
small enough to be dominated by scanner setup.

`PackagesLoader.Load` — the whole `Packages` half of a build, with the transfer
served from memory so the number is the machine's and not the mirror's — is
**352.7 ms** (worst observed 513 ms) for all four indexes, 75,176 merged
entries. The 300 ms difference between that and the 52.8 ms of parsing is
almost entirely **gzip**: 24.8 MB in, 97.9 MB out, through `compress/gzip`.
That is worth knowing before optimising a parser that is already running at
1.9 GB/s.

### Against the committed fixtures

`go test -run XXX -bench Packages ./internal/catalog/`.

| Corpus | Throughput | Stanzas/s |
|---|---|---|
| Ubuntu `main` fixture | 1,938 MB/s | 1,096,805 |
| Ubuntu `universe` fixture | 2,449 MB/s | 768,644 |
| Debian `main` fixture | 1,331 MB/s | 1,237,186 |
| Synthetic 20k, real field text | 2,079 MB/s | 1,171,867 |
| Synthetic 20k, merged and deduped | 2,018 MB/s | 1,137,911 |

### Fuzzing `ParsePackagesIndex` — a campaign that never finished a clean run

Recorded by the backend work, whose words these numbers are. It is written the
way the `FuzzLoad` passage under *Cache load* is written, and for the same
reason: the execution count is not the evidence, and saying so is the point.

| Run | Workers | Wall clock | Outcome |
|---|---:|---:|---|
| 1 | 6 | 5m07s | died during minimisation |
| 2 | 4 | 4m28s | died during minimisation |
| 3 | 2 | 2m42s | died during minimisation |
| 4 | 4 | 1m22s | died during minimisation |
| **Total** | | | **≈20.5 M executions** |

**All four runs died during minimisation, and all four persisted "failing"
inputs were non-reproducing under `-count=10`.** All four were deleted rather
than committed; there are no crasher files in `testdata/fuzz/`. The machine was
running nine other packages and the reported execution rate spent long stretches
at 0/sec, 30/sec at worst. The corpus grew to ~291 entries.

**This target has not yet had one uninterrupted run on a quiet machine. Until it
has, nobody should call this parser fuzz-clean.** That is the headline, not the
20.5 M.

What the campaign did find, it found in the first second, and from the
hand-written seeds rather than from mutation:

- `pkgAppendFold` folded `Package:` along with every other kept field, so
  `"Package: hello\n evil"` produced an entry named `"hello\nevil"`.
- A control character passed straight through, so `"Package: hel\x00lo"`
  produced a name with a NUL in it.

Both are fixed, and both are pinned by named table tests rather than by a corpus
entry: `TestPackagesUnusableNamesAreDropped` (9 cases) and
`TestPackagesNameSurvivesArgv`.

The parse-phase cost of the name guard those fixes added, measured because a
per-stanza validity check on the hot path is exactly the kind of fix that pays
for itself in correctness and charges the budget for it:

```
go test ./internal/catalog/ -run '^$' \
  -bench 'BenchmarkPackagesParse/ubuntu-universe' -benchtime 2s -cpu 1
```

Seven runs each, machine A, with nine other packages running: **70,107 ns/op with
the guard, 75,048 ns/op without it** (medians). The guard is below this machine's
noise floor — two runs of identical intent differed by ±7% — so the honest
statement is that its cost could not be measured here, not that it is free.

## DEP-11 parse — budget: < 10 s parse phase

### Against the real indexes

**Met, using 3.7% of the budget at best and 10% at worst.**

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ \
  -run '^$' -bench 'PerfDep11' -benchtime 10x
```

| Index | Uncompressed | Components indexed | Decode | Throughput |
|---|---:|---:|---:|---:|
| `noble/main` | 1,923,546 | 82 | **36.6 ms** | 52.6 MB/s |
| `noble/universe` | 18,564,799 | 1,972 | **289.7 ms** | 64.1 MB/s |
| `noble-updates/main` | 734,635 | 28 | **14.8 ms** | 49.5 MB/s |
| `noble-updates/universe` | 1,569,020 | 79 | **26.5 ms** | 59.2 MB/s |
| **All four** | **22,792,000** | **2,061 merged** | **367.6 ms** | |

Worst observed, all four: 1.00 s — 10% of the budget. `dep11Load` — fetch seam,
gzip sniff, streaming decode, merge — is **458.9 ms** for all four, worst
observed 1.14 s.

The `noble` `main`+`universe` pair decodes in **326 ms**, which lands almost
exactly on the 284 ms that `docs/dev/index-formats.md` §7.4 measured with a
throwaway decoder during index research, and **replaces the ≈0.41 s
extrapolation** this file previously carried from a 20 MB synthetic corpus.
The extrapolation was 26% pessimistic.

"Components indexed" is what the index keeps — one entry per package it can
attach metadata to — not the number of YAML documents in the stream; §6.1
counts documents.

## Cache load — budget: < 500 ms

### Against the staged corpus's 16.5 MB catalogue — 75,176 entries

**Met, using 1.1% of the budget.**

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ \
  -run '^$' -bench PerfCache -benchtime 20x
```

75,176 real entries. Best observation, with the saturated-machine figure in
brackets.

| | Measured |
|---|---|
| `catalog.bin` on disk | **16,536,822 bytes** |
| **Load** — `ReadFile` + CRC + full structural validation | **5.71 ms** (17.6 ms worst) |
| Validation alone, buffer already resident | 0.92 ms (4.54 ms worst), 17.9 GB/s |
| Save — build, write, fsync, rename | 63.1 ms (234 ms worst) |

This **replaces the 15–30 ms warm / 40–80 ms cold estimate** and the ~10 ms
extrapolation from the synthetic file. The real catalogue is 16.5 MB rather
than the 19 MB predicted, and it loads in **5.71 ms — 1.1% of the budget, and
3.5% at the worst of nine repetitions on a saturated machine.**

Against the synthetic corpus the same benchmark measures 6.3 ms for an 11.8 MB
file over 70,000 entries; the real file is 40% larger and loads in the same
time, which is what "no entry is decoded at load" means in practice. Under
`-race` the synthetic load is 15.6 ms.

Robustness is measured too, because a cache that is fast and wrong is worse
than no cache: a **43-case** corruption matrix (truncation at every 7th offset,
single-bit flips at every 13th byte, CRC-repaired mutations so the bounds
checks are actually exercised rather than short-circuiting at the checksum),
plus fuzzing. Every damaged input returns a sentinel and rebuilds; none panics.

| Fuzz run | Executions | Wall clock | Findings | New corpus entries |
|---|---:|---:|---:|---:|
| `FuzzLoad`, first round | 148,000,000 | — | 0 | — |
| `FuzzLoad`, later run | **1,399,474,369** | 45m00s | **0** | **0** |
| `FuzzLoad`, re-run after the cache commit changed | 24,647,347 | 1m01s | 0 | 0 |

The long run averages ~518k exec/s over its whole wall clock; Go's own progress
lines report ~780k/s once the corpus is loaded and the process is warm. The
last row is a one-minute confirmation on a machine with four other test suites
running, which is why its rate is lower — it is there because the run is
cheap and the rule is that the parser is re-fuzzed after any change to it.

The second row is worth reading for what it does *not* say. 1.4 billion
executions found nothing, which is the answer everybody wants, and it also
expanded coverage by nothing at all: not one input in forty-five minutes
reached a branch the committed seed corpus does not already reach. That is a
genuine property of this parser — every rejection is a bounds or a magic-number
check within the first few hundred bytes, so the reachable state space is
small and the seeds saturate it — and it is also the honest reason not to
report the execution count as though it were the evidence. The seed corpus and
the 43-case matrix are the evidence; the fuzz run is what says they are not
missing a shape.

Supporting figures from the synthetic pass: materialising one 500-row page
costs 32.8 µs, and `Get` by name (binary search, no index built at load) 1.8 µs.

### Against the application's own 19.0 MB catalogue — 85,565 entries

**Met, using 13% of the budget for the whole warm-start load on machine B and
7.6% on machine A.**

The figures above are `LoadCache` alone: read the file, check the CRC,
validate the structure. That is the right unit for the format, and it is not
what an operator waits for. What the picker's first query actually pays is
`catalogImpl.load` — that read, plus materialising every row, plus building the
search index, plus handing the transient memory back — against the catalogue
the *application* builds for its own default target, which is 13.8% more rows
and 2.4 MB more file than the staged corpus.

```
DEBARK_PERF_APPCACHE=<the directory the app's own catalogue was built into> \
  go test ./internal/catalog/ -run TestPerfLoadMemoryProfile -v
```

`LoadTarget` over 85,565 entries and 18,965,143 bytes, min / median / max,
with `catalog.bin` on the machine's own filesystem:

| Machine | Runs | Measured |
|---|---:|---|
| B, `linux/amd64`, the shipping target | 11 | 53.5 / **66.1** / 72.0 ms |
| A, `windows/amd64` | 9 | 35.6 / **38.1** / 46.0 ms |

**Where the file lives matters more than which machine reads it.** Every other
machine-B figure in this file reaches the repository through a bind mount onto
the Windows host, and reading a 19 MB cache through that mount instead costs
**66 ms**: a paired run the same hour, same commit, same container, measured
**121.9 ms** median from the mount against **56.1 ms** from the container's own
disk, seven runs each and no overlap between the two distributions. That is the
harness and not the product — an installed app reads its cache from the
machine's own disk — but it is worth writing down, because it is larger than
everything else on this page's load path put together and it would otherwise
have been attributed to the CPU.

The before-and-after of the memory release these numbers include is in *What
the load actually costs the process*.

## Catalogue build — budget: parse phase < 10 s after download completes

**Met. The real parse phase is 2.2 s as the progress UI counts it and roughly
0.7 s of actual CPU; the whole first run over the network is 6.1 s.**

The one measurement in this file that talks to `archive.ubuntu.com`:

```
DEBARK_PERF_NETWORK=1 go test ./internal/catalog/ -run TestPerfCatalogBuildNetwork -v
```

Two real builds of the Ubuntu noble `main`+`universe` target, release and
`-updates`, over the network on a domestic link. Both are charged the same way:
a phase ends where the next phase first reports.

| | First run (quiet) | Second run (24 cores saturated) |
|---|---:|---:|
| Download of the four `Packages` indexes, 24,836,482 bytes | 3.74 s | 4.31 s |
| Parse — and the DEP-11 fetch and decode, which are fused | 2.22 s | 4.79 s |
| Save (encode, write, fsync, rename) | 0.13 s | 0.29 s |
| **Total** | **6.09 s** | **9.38 s** |
| Cache written | 16,536,822 bytes | 16,536,822 bytes |
| Entries | 75,176 | 75,176 |

**The parse phase has 4.5× headroom** against its 10 s budget on the quiet run,
and 2.1× on the saturated one — and both of those numbers are pessimistic,
because that row contains the 7,385,487 bytes of DEP-11 transfer. Measured
offline against the staged corpus, with no network in the path at all, the
entire build costs **0.87 s**, so the true CPU cost of the parse phase is
roughly **0.7 s: fourteen times inside its budget.**

Two caveats on the table, both worth knowing before reading a progress bar:

- **The phases interleave.** `PackagesLoader.Load` reports download then parse;
  `dep11Load` then reports download and parse again, once per index, and its
  decode is *streaming* — it pulls YAML out of the same reader that is counting
  downloaded bytes. "Download" and "parse" are therefore genuinely fused for
  DEP-11 and cannot be separated by wall clock, which is why the parse row here
  is the sum of the `Packages` parse, the whole DEP-11 pass, the reconcile, the
  encode-side work and the search index build. It also means an operator
  watching a DEP-11 download sees a bar that is really measuring a decode.
- The cache file was **byte-for-byte the same size** as the one the staged
  corpus produces, which is how the offline reproduction below is known to be
  faithful.

With the transfer served from the staged corpus instead of the mirror, the same
end-to-end path — decompress, parse, merge, DEP-11, reconcile, encode, fsync,
load back through the reader a warm start uses — is:

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ \
  -run '^$' -bench PerfCatalogBuild -benchtime 5x -count 3
```

**0.87–0.94 s per build** on a quiet machine; 1.24–2.54 s across nine further
runs while other packages were saturating all 24 cores. That is the whole build
minus the network.

## Cold start to interactive — budget: < 1.5 s

**Met on a quiet machine, with 7× headroom: 213 ms p50, 245 ms p95 over 25
launches. Missed on a saturated one, by 1.6 s at p95.** Both are recorded, the
way this file records both ends of every repeated measurement.

Machine B, `debark-gui` at `71d4d86` built with
`-tags "desktop,production,webkit2_41"`, a real `debark` on `PATH`, no target
selected — so the app lands on the readiness screen, which is what a first run
does. Twenty-five launches, five seconds apart, each a fresh process on a fresh
window.

```
DISP=:91 MODE=coldstart N=25 GAP=5 bash drive.sh      # the third-round harness
```

### What "interactive" means here

**The first screen painted and able to accept input.** Precisely: the timestamp
taken in the second `requestAnimationFrame` callback after `shell.go(screen)`
resolves. `shell.go` resolves when the screen's DOM is mounted and its handlers
are attached; the first rAF after that runs *before* the frame carrying it is
painted and the second runs after, so the mark is the first moment a person
could have seen the screen and typed into it. It is stamped with `Date.now()`
and differenced against the wall clock the harness read immediately before
`exec`, so **process spawn is inside the number**.

Inside it: `fork`/`exec` and the dynamic linker, the Go runtime, the object
graph, `wails.Run`, GTK window creation, WebKit process spawn, the embedded
asset server, page load, CSS, the ES-module graph, the shell's frame, the
binding resolution, `Readiness()` and `CurrentTarget()`, and the first screen's
own build.

Not inside it: the readiness *pass*. `main.js` starts it fire-and-forget and
never waits, which is correct — a full pass spawns subprocesses. In this
harness's own capture the readiness screen self-reported **282 ms**, and all of
it is spent after the screen is already up and usable; the third round's
first-look screenshot recorded 188 ms on the same image. Also not inside it: the catalogue,
which no boot path touches, because no target is selected at boot.

### The phases

Medians over the 25 launches. The Go-side marks came from a temporary
`fmt.Fprintf` in a throwaway copy of `main.go` — see *How the app-level figures
were taken* below.

| Phase | p50 | p95 | Cumulative p50 |
|---|---:|---:|---:|
| `exec` → Go `main()` — linker, runtime | 19 ms | 22 ms | 19 ms |
| `main()` → Wails calls `OnStartup` — GTK window, WebKit spawn | 32 ms | 39 ms | 51 ms |
| The app's own `OnStartup` | **0.0 ms** | 0.0 ms | 51 ms |
| `OnStartup` → `OnDomReady` — asset server, HTML, CSS | 80 ms | 90 ms | 131 ms |
| `OnDomReady` → `perf.js` evaluated — the ES-module graph | 12 ms | 15 ms | 143 ms |
| Module eval → app frame painted — `createShell` | 29 ms | 33 ms | 172 ms |
| Frame → **first screen painted** — bindings, `Readiness()`, `CurrentTarget()`, screen build | 40 ms | 46 ms | **213 ms** |

| | min | p50 | p95 | max |
|---|---:|---:|---:|---:|
| Window mapped | 121 ms | 127 ms | 134 ms | 136 ms |
| Page load started (`performance.timeOrigin`) | 105 ms | 116 ms | 134 ms | 162 ms |
| App frame painted | 154 ms | 172 ms | 196 ms | 225 ms |
| **First screen painted — interactive** | **190 ms** | **213 ms** | **245 ms** | **269 ms** |
| Of which is in-page work | 76 ms | 93 ms | 106 ms | 111 ms |

**25 of 25 launches were inside 1.5 s**, and the slowest was 269 ms — 18% of the
budget.

The application's own code is a small part of it. Its `OnStartup` is
unmeasurable at 0.0 ms — `app.New` touches no filesystem and starts no process,
deliberately, and it shows. 131 ms of the 213 ms is spent before a line of the
page's own JavaScript runs: the linker, the Go runtime, GTK, the WebKit process
pair, and the asset server delivering `index.html` and 124,799 bytes of CSS. The
remaining 82 ms is the module graph, `createShell`, the binding resolution and
the first screen's own DOM.

### The same measurement on a saturated machine

Twenty launches taken earlier the same day, at `5a260a8`, while the host was
running a dozen other packages' suites:

| | min | p50 | p95 | max |
|---|---:|---:|---:|---:|
| First screen painted | 629 ms | 1,557 ms | 3,068 ms | 3,852 ms |

**8 of 20 inside the budget; p95 misses it by 1,568 ms.** The extra time is not
in the app. `exec` → `main()` alone went from 19 ms to 63 ms at p50 and 599 ms at
p95, and `OnStartup` → `OnDomReady` from 80 ms to 607 ms — the linker and WebKit
waiting for CPU. The two sets bracket the answer: **on a machine that is not
fighting for cores the budget has 7× headroom; on one that is, this app starts
as slowly as everything else on it.**

### Two things this number does not cover

- **The page cache is warm.** These are the 2nd–25th launches in a session, so
  `libwebkit2gtk`, `libgtk-3` and the binary itself are resident. A genuine
  first-ever launch pays disk for all of them. Dropping the cache needs
  `/proc/sys/vm/drop_caches` and a privileged container, which would flush the
  whole Docker VM, so it was not done.
- **It was measured with the instrumented binary**, because the mark has to be
  taken from inside the page. The hook is a 15 KB module, one extra asset-server
  request and two `Date.now()` calls; it is on the critical path it measures.
  Its cost shows up in the `OnDomReady` → module-eval leg, 12 ms p50, of which
  the hook is at most a few.

  **The un-instrumented binary was cross-checked from outside the process** and
  agrees. Four launches of the clean `/out/debark-gui`, each photographed as
  fast as `xwd` will run — 15 ms between captures — with each frame scored by
  `convert … -format "%[fx:standard_deviation]"`, so that a flat fill scores
  0.000 and a painted screen 0.09:

  | Run | Window mapped | First non-flat frame | Image settled at 0.09 |
  |---|---:|---:|---:|
  | 1 | 138 ms | 225 ms | 295 ms |
  | 2 | 135 ms | 203 ms | 251 ms |
  | 3 | 205 ms | 225 ms | 311 ms |
  | 4 | 145 ms | 200 ms | 274 ms |

  First pixels at **200–225 ms** brackets the instrumented 190–269 ms, and the
  settled figure is later by about the capture cadence plus the readiness rows
  arriving. `xwd`'s 15 ms cadence is the resolution of this method; it is a
  confirmation, not a second measurement.

## Memory — budget: < 400 MB resident, idle

### The real process — the number the budget is about

**Still missed by 20.8 MB, 5.2% over, when the catalogue is loaded — if the
budget means the sum of RSS across the process tree. Met at 165.6 MB if it
means PSS. The miss was 48.6 MB before the change described in *What the load
actually costs the process* below; that change recovered 27.8 MB of it and
could not recover the rest, because the rest is not the application's.**

WebKit2GTK is three processes, not one. A figure that counts only the Go
process is wrong by more than half. Machine B, 90 seconds idle after the screen
settled, read straight out of `/proc/<pid>/status` and
`/proc/<pid>/smaps_rollup` for every process in the app's process group:

```
DISP=:123 MODE=rss WHERE=picker    SETTLE=90 APPBIN=/out/gui-before-instr bash drive.sh
DISP=:124 MODE=rss WHERE=picker    SETTLE=90 APPBIN=/out/gui-after-instr  bash drive.sh
DISP=:125 MODE=rss WHERE=readiness SETTLE=90 APPBIN=/out/gui-after-instr  bash drive.sh
```

Both binaries carry the measurement hook, because reaching the picker needs it
to drive `SelectTarget`; they differ only by the commit they were built from.
`gui-before-instr` is `36ab130` — the tree immediately before the catalogue
change — and `gui-after-instr` is `67279ac`, the change itself. The catalogue
is the app's own `ubuntu:24.04/desktop` amd64 target, 85,565 entries,
18,965,143 bytes of `catalog.bin`, loaded from the cache a previous run wrote.

**Idle on the picker, with the catalogue loaded** — the state an operator
actually works in, and the demanding one:

| Process | RSS before | **RSS after** | PSS before | PSS after |
|---|---:|---:|---:|---:|
| `debark-gui` (Go + GTK + the WebKit UI process) | 227.5 MB | **199.5 MB** | 125.6 MB | 100.3 MB |
| `WebKitWebProcess` | 171.2 MB | **171.0 MB** | 50.1 MB | 53.5 MB |
| `WebKitNetworkProcess` | 47.6 MB | **48.2 MB** | 10.5 MB | 11.2 MB |
| `dbus-launch` | 2.4 MB | **2.2 MB** | 0.5 MB | 0.5 MB |
| **Total** | **448.6 MB** | **420.8 MB** | **186.7 MB** | **165.6 MB** |

Reproduced on a second run of the after binary the same hour: `debark-gui`
199.6 MB, **422.7 MB summed RSS**, 155.4 MB PSS. The Go process reproduces to
0.16 MB and the total to 1.9 MB.

The 448.6 MB "before" column reproduces the figure this section carried at
`71d4d86` — 450.2 MB summed RSS, 206.1 MB PSS — to within 1.6 MB of RSS, on a
tree twenty-one commits later. **The difference between the columns is the Go
process and nothing else**: it moves 28.0 MB, while the two WebKit processes
and dbus move +0.2 MB between them, which is inside their run-to-run spread.
The Go process is where the catalogue lives and where the change was made.

**Idle on the readiness screen** — no target, no catalogue, the state a
launched app sits in. The change cannot touch this row, because no catalogue is
loaded on this path, and it does not:

| Process | RSS | PSS |
|---|---:|---:|
| `debark-gui` | 162.6 MB | 58.8 MB |
| `WebKitWebProcess` | 161.4 MB | 43.1 MB |
| `WebKitNetworkProcess` | 48.2 MB | 10.5 MB |
| `dbus-launch` | 2.2 MB | 0.5 MB |
| **Total** | **374.3 MB** | **112.8 MB** |

Against 376.8 MB / 155.9 MB for the same screen at `71d4d86`: RSS agrees to
2.5 MB, PSS does not agree at all, and the next paragraph is about why.

**What was counted, and why the two columns disagree so much.** RSS is the
honest per-process metric and it is the wrong thing to add up: every one of
these processes maps the same `libwebkit2gtk`, `libgtk-3`,
`libjavascriptcoregtk` and fontconfig text, and summing RSS counts those pages
once per process. `Shared_Clean + Shared_Dirty` is 117.8–138.7 MB in each of
the two large processes; PSS divides each shared page by the number of
processes mapping it and is the only one of the two that adds up to something a
machine has to find. **Sum-of-RSS misses the budget; PSS does not; per-process
RSS never approaches it.**

**But PSS is not stable here, and that is a measurement defect rather than a
property of the app.** The divisor PSS uses is every process on the *kernel*
mapping the page, and this host runs a dozen containers off the same
`debark-shots:go126` image, several of them running this same binary against
the same libraries at the same time. Two picker runs of the *identical* binary
read 165.6 and 155.4 MB, and two readiness runs of the same screen read 155.9
and 112.8 MB — a 43 MB swing on a screen whose summed RSS moved 2.5 MB.
Summed RSS reproduced to 1.9 MB across every pair. **So PSS here
measures how busy the developer's machine was, not how much memory Debark
needs.** It is still the right metric for the question "how much of this
machine does Debark occupy"; it is not a metric this harness can hold
still, and a PSS figure from it should be read as an order of magnitude
rather than as a pass.

**The verdict, stated the unflattering way first.** Summed across the process
group the app idles at **420.8 MB with the catalogue loaded, which misses the
400 MB budget by 20.8 MB**. No single process approaches it — the largest is
199.5 MB. On PSS it is 165.6 MB, 41% of the budget.

**The remaining 20.8 MB is not the catalogue's.** The whole Go process is
199.5 MB and the catalogue is 38.7 MB of it (measured below); the readiness
screen, which has no catalogue at all, already sums to 374.3 MB, so 94% of a
420.8 MB total is present before a single package row exists. The two WebKit
processes add 219.2 MB of it, and 117.8–138.7 MB of each large process is
shared library text that the summing counts once per process. Nothing in
`internal/catalog` can move a shared `libwebkit2gtk` mapping, and this file
would rather say so than keep looking for 20 MB in the wrong package.

**Three things that would move it, none of them measured here, and one of them
would not be enough on its own.**

- **Reading the budget as PSS.** Defensible, and this file will not do it on
  its own authority — particularly not with a PSS figure that moves 43 MB
  between runs.
- **A WebKit process-model option** — `WEBKIT_USE_SINGLE_WEB_PROCESS`, and the
  network process's own caches. This is where the memory actually is: the two
  WebKit processes are 219.2 MB of the 420.8. It is a product decision for
  whoever owns `main.go` and wants its own before-and-after.
- **Serving search off the cache buffer instead of materialising every row.**
  `cache.go` already exposes `Haystack`, `IsApp` and `SectionID` for exactly
  this, and `catalog.go` declines it deliberately and says why: the alternative
  is two implementations of relevance — a cache-backed one and an in-memory one
  — that must agree forever, where a cold start exercises a different path from
  a rebuild. The arithmetic on the measured parts: the resident catalogue is
  38.7 MB, of which 30.5 MB is materialised `Entry` rows; a cache-backed
  catalogue would instead hold the 19.0 MB file plus its record and intern
  tables. **That is roughly 19 MB, and 420.8 − 19 is 402 MB.** It would cost
  the project the one thing `catalog.go` spends a page arguing against and it
  would still not reach 400 MB, which is the strongest argument available for
  not doing it.

### What the load actually costs the process

**Confirmed: 28.5 MB of the Go process's growth across a catalogue load was
the load's own transient memory, not anything the catalogue retains. Fixed;
worth 27.1 MB of a bare Go process's RSS and 28.0 MB of the real app's, for
3–7 ms added to a load that is already 38–66 ms and happens once.**

The previous version of this section observed the Go process growing 65.6 MB
across a load whose retained footprint scales to about 36.4 MB, and left the
~29 MB difference as a hypothesis it could not test — because `/proc` cannot
tell "the catalogue is holding this" from "the Go runtime has not handed the
pages back". `runtime.MemStats` can. It was the second.

```
DEBARK_PERF_APPCACHE=<the directory the app's own catalogue was built into> \
  go test ./internal/catalog/ -run TestPerfLoadMemoryProfile -v
```

`TestPerfLoadMemoryProfile` runs the application's own warm-start path — `New`,
then `LoadTarget`, which is `catalogImpl.load` — against the cache the
application itself wrote, and reads `MemStats` and `/proc/self/status` at four
checkpoints without forcing a collection first, because the running app does
not force one either. Machine B, nine runs of each column, medians. The
`MemStats` figures reproduced to 0.01 MB across all nine runs of both columns;
the `VmRSS` figures to 0.7 MB before and 0.9 MB after.

| At the moment `load` returns | Before | **After** |
|---|---:|---:|
| `HeapAlloc` — everything the load allocated and no collection has swept | 68.3 MB | **39.8 MB** |
| Of which is live: the catalogue itself | 38.7 MB | **38.7 MB** |
| Of which is garbage the load left behind | **28.5 MB** | **0.0 MB** |
| `HeapIdle - HeapReleased` after one collection — heap held and not used | 28.2 MB | **0.1 MB** |
| Process `VmRSS` before the load | 13.4 MB | 13.6 MB |
| Process `VmRSS` after it | 80.4 MB | **53.4 MB** |
| **RSS the load costs** | **67.0 MB** | **39.9 MB** |

The last row is the whole finding. **The catalogue needs 38.7 MB and the load
used to cost the process 67.0 MB**; the 28.4 MB difference is the
18,965,143-byte `catalog.bin` buffer plus about 9.5 MB of search-index scratch,
still uncollected at the instant `load` returns.

The before column confirms it from the other side. Call `debug.FreeOSMemory`
by hand at that point and the same process drops to 53.6–54.7 MB — the after
column's 53.4, reached from the other direction, in every one of the nine
runs.

**Why a collection alone would not have fixed it, and why idling did not.**
Sweep the garbage and the pages stay: `HeapIdle - HeapReleased` is 28.2 MB
after two forced collections with the catalogue still reachable. Go's
background scavenger only returns heap above the heap goal, and at the default
`GOGC=100` a 38.7 MB live heap sets a goal of roughly twice that — above
everything the process is holding, so there is nothing for it to return.
Nothing about sitting idle changes a heap goal, which is why ninety seconds of
it did not, and why this was visible in `/proc` as memory that looked
retained.

**What changed, in `catalogImpl.load`.** Two things, and the second is the one
that matters:

- `cf.meta.BuiltAt` is now read *before* `newSearchIndex` rather than after.
  Go's liveness is precise, so reading it afterwards pinned the whole 19 MB
  file buffer across the peak of the load. At the default `GOGC=100` no
  collection lands in that window and it changes nothing measurable; at
  `GOGC=50` it drops the garbage a load leaves from 28.5 MB to 9.5 MB and peak
  `Sys` by 9 MB. It is recorded here because it is a real effect that the
  default hides, not because it moved the number below.
- `load` now ends with `debug.FreeOSMemory`.

**The argument for `debug.FreeOSMemory`, since it is a blunt instrument.** It
forces a full stop-the-world collection and it is spent exactly once per load,
which is at most once per target per process — `ensureLoaded` holds `loadMu`
and re-checks the loaded key, so the picker's first frame firing `Search` and
`Categories` together produces one load and one release. It is the cheapest
moment in the process's life to force a collection: the caller is already
inside a load it is waiting on, no frame is animating, and the next thing that
happens is a picker sitting idle. It is **not** on a timer, which
would pay the same collection repeatedly for nothing and would fight the pacer
while someone is typing — the budget that matters most. It is **not** per
query: `Search` allocates one page per call and nothing that scales with the
corpus, so there is no drift to sweep up.

**What it costs.** `LoadTarget` against the 85,565-entry cache, min / median /
max, with the file on each machine's own disk:

| | Runs | Before | After |
|---|---:|---|---|
| Machine B, `linux/amd64` | 11 | 46.8 / **60.9** / 120.9 ms | 53.5 / **66.1** / 72.0 ms |
| Machine A, `windows/amd64` | 9 | 31.7 / **35.4** / 42.6 ms | 35.6 / **38.1** / 46.0 ms |

**About 5 ms on machine B and 3 ms on machine A** — +5.2 and +2.7 at the
median, +6.7 and +3.9 at the minimum, which is 8% and 7% of a load that is
paid once per target per process. It is less than a full collection of a 68 MB
heap would suggest, because most of what it forces is collection the runtime
was going to do anyway; a `debug.FreeOSMemory` call measured in isolation on a
heap that has *already* been collected is 1.4–2.6 ms on machine B and
4.7–26 ms on machine A, and that is the scavenge alone.

An earlier pass of this same A/B, taken with the cache on the bind mount,
reported the release as free — 105.9 ms median before against 103.5 ms after.
It was not free; the mount's 66 ms was drowning a 5 ms signal. The numbers
above replace it, and the lesson is written into the section above rather than
left here.

`BenchmarkPerfCatalogBuild` — the whole offline build, which ends in a load —
is 0.879 s at its best observation after against 0.878 s before, over three
runs of five builds each on machine A. Search, cache load, the parse phases and
index construction are on none of these paths and are unchanged; the cache
benchmark re-run on the same day reads 6.19 ms, inside the 5.71–17.6 ms band
already recorded.

**What pins it.** `TestWarmStartLoadDoesNotLeaveItsOwnGarbageBehind` asserts
the consequence rather than the call, over two synthetic corpus sizes: the
uncollected garbage a load leaves must be a small fraction of the file it just
read. With the release the 30,000-row case leaves 0.00 MB; without it, 8.37 MB
against a 4 MB bound, and the test fails.

### The catalogue's own contribution, inside a Go test process

**A loaded real catalogue is 32.0 MB for the benchmark corpus's 75,176 entries
and 38.7 MB for the application's own 85,565 — 8% and 9.7% of the budget.**

The 85,565-entry figure is measured, by `TestPerfLoadMemoryProfile` above,
against the cache the running application wrote:
`HeapAlloc` with the catalogue reachable and two collections forced, minus the
same reading before the load, is **38.68 MB** — 452 B per entry, against the
447 B the staged corpus measures below — and it reproduced within 0.05 MB over
nine runs on machine B and seven on machine A. It **replaces the 36.4 MB this
file previously carried**, which was the 75,176-entry figure scaled by row
count and was 6% low.

The per-phase breakdown is still taken against the staged 75,176-entry corpus,
because that is the one a benchmark can hold still:

```
DEBARK_PERF_CORPUS=<corpus> go test ./internal/catalog/ -run TestPerfResidentMemory -v
```

Heap growth measured across a load of the real 75,176-entry catalogue, in the
same order `catalogImpl.load` does it:

| | Retained | Per entry |
|---|---:|---:|
| Cache file resident (`ReadFile` buffer) | 15.80 MB | 220 B |
| Entries materialised from it | 25.50 MB | 356 B |
| Search index over those entries | 6.52 MB | 91 B |
| **Steady state (entries + index)** | **32.02 MB** | **447 B** |

The cache buffer is not retained: `CacheFile.Entry` copies every string out of
the text arena rather than aliasing it, so nothing in an `Entry` or in the
index points into the file, and 32.0 MB is what an idle picker holds. That was
true before this round's change as well — **the buffer was never retained, it
was merely never handed back**, which is exactly the distinction the section
above exists to draw. The search index's self-reported footprint (6.48 MB)
agrees with the measured 6.52 MB. Re-run this round against a freshly staged
corpus it reproduces to the last digit: 32.01 MB, 446 B per entry, over the
same 75,176 entries and the same 16,536,822-byte file.

The synthetic figures this replaces were 30.3 MB over 70,000 entries;
`TestPackagesResidentMemory` streams a 124 MB synthetic index and asserts a
64 MB ceiling, which remains the regression tripwire:

| | Measured |
|---|---|
| Retained, 70,000 synthetic entries | 30.3 MB (432 B/entry) |
| Churn during the parse | 102 MB `TotalAlloc` |
| Per-stanza string bytes | 137 B |

Index research measured the alternatives for the same 70,000 entries
(`docs/dev/index-formats.md` §5):

| Representation | Total | Per row |
|---|---|---|
| Lean 7-field rows | 15.3 MB | 226 B |
| Arena-interned | 7.6 MB | 110 B |
| **Every field as `map[string]string`** | **235.0 MB** | **3,477 B** |

That last row — more than half the entire application's budget consumed by the
catalogue alone, before any UI exists — is why the cache format is hand-rolled,
why the deb822 parser is hand-rolled rather than reusing
`control.Paragraph` (which is exactly a `map[string]string`), and why the
parser never retains a map per stanza.

This is the catalogue's footprint inside a Go test process. The resident set of
the built desktop app is a different number and is measured above.

## Scrolling — budget: 60,000 rows with no dropped frames

**Met in the real app, on the real engine, against 85,565 real rows: zero
dropped frames in every scenario, twice over, and the app's own update pass is
1 ms at p95 out of a 16.7 ms frame.**

### First: is it virtualising at all?

This is the half of the item that needed checking, because **the figure this
section used to carry described a virtualiser the shipped frame had switched
off**. `frontend/index.html` mounted the shell into a bare `<div id="app">`,
`.df-app`'s `height: 100%` had nothing to resolve against, and the scroll port
grew to the whole result set — measured by the second round's accessibility
pass at **4,000 of 4,000 rows in the DOM**. At 60,000 rows that is a
1.92-million-pixel port with every row realised. The Chromium harness could not see it, because a
harness that mounts into an unbounded block cannot see a height bug. It is
fixed: `index.html` now carries `class="df-app-host"`.

Counted in the running app, on WebKit2GTK, with the picker open on the whole
catalogue:

| | Measured |
|---|---:|
| Match count (`SearchResult.Total`) | **85,565** |
| `.df-vlist__window` children — **rows actually in the DOM** | **34** |
| `.df-vlist__sizer` height | 2,738,080 px |
| `.df-vlist` scroll port height | 670 px |
| `scrollHeight` | 2,738,080 px |

85,565 × 32 px = 2,738,080 px exactly, so the sizer is sized for the full result
set while **34 rows exist**. It is virtualising.

34 is the pool the code's own formula predicts for this viewport:
ceil(670/32) + 1 + 2×6 = 22 + 12 = 34, with `OVERSCAN` 6. **The accessibility
follow-up counted 39 rows at 60,000 rows by a different method on the same
engine, and that is the same answer rather than a different one** — 39 is the
same formula at an 812 px port: ceil(812/32) + 1 + 2×6 = 27 + 12. Two independent counts, two viewport
heights, one pool-size rule, no disagreement. The count falls to 27 at the very
bottom of the list, where fewer rows remain than the pool holds.

### Then: the frames

Machine B, `71d4d86`, the real desktop binary on WebKit2GTK's **software**
renderer, 1400×900, driven from inside the page by setting `scrollTop` in a
`requestAnimationFrame` loop. Frame deltas are the rAF timestamps; the update
pass is `performance.now()` across `update()`, read through the virtualiser's
own `onMetrics` seam.

```
DISP=:93 MODE=scroll bash drive.sh          # the third-round harness
```

| Scenario | Frames | Rows crossed | **Dropped** | Frame Δ p50/p95/max | **Update pass p50/p95/max** | Rows written/frame |
|---|---:|---:|---:|---|---|---:|
| **Idle baseline** — same loop, nothing scrolled | 460 | 0 | **0** | 16 / 17 / 18 ms | — (no updates) | — |
| Hard fling, 1,000 px/frame | 460 | 14,344 | **0** | 16 / 17 / 17 ms | 0 / 1 / 1 ms | 31.26 |
| Sustained drag, 60 px/frame | 460 | 861 | **0** | 16 / 17 / 17 ms | 0 / 1 / 1 ms | **1.86** |
| Full traversal, all 85,544 rows in 280 frames | 280 | 85,544 | **0** | 16 / 17 / 17 ms | 0 / 1 / 1 ms | 33.97 |

Reproduced on a second run the same hour: 0 dropped in all four, identical
medians, fling update-pass max 4 ms rather than 1 ms.

**The idle baseline is the control, and it is the reason these numbers can be
believed.** It runs the same rAF loop on the same page with the scroll position
untouched, so anything it reports as a dropped frame is the container's software
renderer and its CPU share rather than the app. It reports zero, and the three
real scenarios report zero against a frame-delta distribution indistinguishable
from it — 16/17/17 against 16/17/18. **The virtualiser is not costing a frame.**

"Dropped" means a frame delta over 1.5× the observed vsync, taken from the data
rather than assumed; the observed vsync is 16 ms. The harness also counts
"frames over 16.7 ms", which is 36–62 per run in every scenario *including the
idle baseline* — which is what that metric is worth at 60 Hz, and why it is not
the one quoted.

**JS clock granularity.** `performance.now()` is clamped to 1 ms in this WebKit
build, so every duration in this section is quantised to 1 ms. "Update pass p50
0 ms" means under 1 ms rather than no work, and the p95 of 1 ms is a bound
rather than a reading. The old Chromium figures below carry sub-millisecond
resolution and read 0.1–0.9 ms; the two are consistent, and the WebKit numbers
cannot be more precise than the clock they were taken with.

### The same scenarios on a saturated machine

Taken earlier the same day at `138153a`, before the idle baseline existed, while
the host was running a dozen other packages' suites:

| Scenario | **Dropped** | Frame Δ p50/p95/max | Update pass p50/p95/max |
|---|---:|---|---|
| Hard fling | **130** of 460 | 17 / 37 / 53 ms | 1 / 2 / 10 ms |
| Sustained drag | **15** of 460 | 16 / 21 / 50 ms | 0 / 1 / 9 ms |
| Full traversal | **14** of 280 | 16 / 24 / 45 ms | 0 / 1 / 9 ms |

**A contended machine drops frames, and it is not the virtualiser doing it.**
The update pass barely moved — p95 went from 1 ms to 2 ms, and its worst single
pass in three runs was 10 ms, still inside a frame. The frame delta is what blew
out: p95 37 ms against a 17 ms vsync in the fling. Those two facts together are
the whole argument for reporting the update pass separately, and they are why
the quiet-machine run above was repeated with a do-nothing baseline before it was
believed. This row has no baseline of its own, so it cannot say how much of the
130 a rAF loop scrolling nothing would also have dropped; on the quiet machine
that answer was zero, and on this one it was not measured.

### What of this transfers to real hardware

**A software-rendered container is not a laptop GPU**, and the quiet run's frame
deltas are good *because* of that rather than despite it: Xvfb's rAF is a fixed
16 ms timer with no display to miss, so the renderer never had a real vsync to
drop. The number that transfers to real hardware is the **update pass — the app's own
work, at most 1 ms out of a 16.7 ms budget, about 6%**. The frame delta
transfers to nothing. A separate third-round backlog item — a black rectangle
appearing below a clicked row under this same software renderer — is still
unresolved, because enabling compositing crashes the container's X stack, and it
still needs one check on real hardware.

Two design choices produced the update-pass figure, and one of them reproduces
exactly across engines:

- **Recycling by moving nodes, not reassigning them.** Rows that fall off the
  top are rotated to the bottom, so only rows that genuinely entered the
  viewport are rewritten. The sustained drag writes **1.86 rows per frame** on
  WebKit2GTK — the same 1.86 the Chromium harness measured, to three significant
  figures, on a different engine against a different row count.
- **Suppressing fetches during a fling** and reissuing 90 ms after the scroll
  settles.

### The superseded figure, and why it was wrong

Kept, because saying what a number used to claim is worth more than the number.
This section previously read:

> **Met. Zero dropped frames in every scenario.** Headless Chromium 152,
> 1600×1000 at 60 Hz, 60,000 rows, 32 px rows, an 812 px viewport and therefore
> a 39-slot recycled pool.

| Scenario | Frames | **Dropped** | Frame Δ p50/p95/max | Update pass p50/p95/max | Rows written/frame |
|---|---|---|---|---|---|
| Hard fling, 12,317 rows crossed | 460 | **0** | 16.7 / 17.6 / 19.5 ms | 0.1 / 0.3 / 0.4 ms | 19.6 mean |
| Sustained drag, 60 px/frame | 460 | **0** | 16.7 / 17.6 / 18.3 ms | 0.1 / 0.2 / 0.3 ms | 1.86 mean |
| Full traversal, all 59,975 rows | 280 | **0** | 16.7 / 18.0 / 22.4 ms | 0.1 / 0.2 / 0.6 ms | 38.6 mean |

"Dropped" means a frame delta over 1.5× the **observed** vsync, taken from the
data rather than assumed. Counting "frames over 16.7 ms" literally gives about
half of every run, which is just 16.7 ms sitting on the median — that metric is
noise at 60 Hz, so both are reported.

It was honest about the component and wrong about the product, in two ways.
**It measured Chromium, which this application does not ship** — and the one bug
that ever reached the real engine was invisible to every Chromium harness in
this repository. And **it mounted the list into a harness page that had a
height, while the shipped `index.html` mounted it into one that did not** — so
the component under test virtualised and the product did not. Nothing in it was
ever reproduced in something an operator could run, until now.

Its supporting measurements were taken against a stub and are not re-taken here,
because they are properties of the virtualiser rather than of the app: page
latency p50 47–70 ms against a stub configured at 40–100 ms; a 200-jump sweep
fetching 261 pages retaining 4 of a bound of 24, with the JS heap moving
22.6 → 22.9 MB; 1,560 rows compared against the backend across 40 random jumps
with 0 mismatches; and 25 rapid query changes with 0 stale rows. The
page-latency half of that is now measured for real in the next section: against
the live bridge and an 85,565-row catalogue a page costs 6 ms at p95, not
47–70 ms.

## Keystroke to rendered results — budget: < 100 ms p95, end to end

**Missed by 3 ms. 103 ms p95 across the real Wails bridge, against a budget of
100 ms.** p50 96 ms, max 117 ms, over 210 real X keystrokes. The p95 reproduced
at 103 ms on two builds taken hours apart, `5a260a8` and `71d4d86`, either side
of the third round's accessibility work landing in `frontend/src`.

**Sixty of those 103 ms are a deliberate 60 ms debounce, and about 33 ms of the
rest is this measurement's own rounding up to whole frames.** The bridge — the
thing that was never measured before and the reason this item existed — costs
**10 ms at p95**. Neither of those makes the miss go away, and the file does not
pretend otherwise: as measured, the budget is exceeded.

### How it was measured

Machine B, `71d4d86`, the real desktop binary, the real 85,565-row catalogue
loaded from cache, the picker open.

```
DISP=:92 MODE=keys REPS=30 bash drive.sh    # the third-round harness
```

`xdotool type --delay 300 browser` — real X key events into the real search
box, 300 ms apart, which is fast typing and slow enough that each keystroke
produces its own query. Thirty repetitions, the field re-selected between them
so each run starts from `b`. **210 keystrokes, 210 samples: one per keystroke.**

**`browser`, not `lib`, and this is why.** `lib` matches 35,263 rows but matches
them early in the package name, so the scan stops; `browser` matches 264 and
runs to the end of nearly every one of the folded haystacks. The Go benchmark
above measures the same inversion. Typing `browser` also exercises the whole
prefix — `b`, `br`, `bro` … — so the sample is a word being typed, not one query
being repeated.

A sample is the interval from the `keydown` event on `.df-search__input` to the
second `requestAnimationFrame` callback after the query's **first page**
(`offset === 0`) has been written into the list. Only the first page counts: a
query change also triggers the virtualiser's follow-on prefetches — 1,421
responses for 210 keystrokes, so 1,211 of them — and those arrive after the
results are already on screen. Counting them too gives 119 ms at p95, and that
number is reported below as well, because it is what the bridge is actually
carrying while someone types.

### The breakdown

| Leg | p50 | **p95** | max |
|---|---:|---:|---:|
| `keydown` → `SearchPackages` called — **the 60 ms debounce** | 62 ms | **63 ms** | 63 ms |
| `SearchPackages` called → promise resolved — **the Wails bridge** | 8 ms | **10 ms** | 22 ms |
| Resolved → rows painted — DOM write plus up to two frames of rAF | 26 ms | **32 ms** | 38 ms |
| **`keydown` → rows painted, end to end** | **96 ms** | **103 ms** | **117 ms** |
| Same, counting every prefetch response as well | 103 ms | 119 ms | 133 ms |

And the seventh keystroke on its own — the one that completes `browser`, the
worst query in the archive:

| | p50 | p95 | max |
|---|---:|---:|---:|
| `keydown` → rows painted, `browser` complete, n=30 | 95 ms | **97 ms** | 103 ms |

**The worst query is not the worst keystroke.** The full `browser` scan costs
2–3 ms more in Go than a two-letter prefix, and that is invisible next to a
60 ms debounce, so the hardest query lands *inside* the budget while the
distribution over all seven does not.

### The bridge on its own

Two hundred `SearchPackages` calls per query, issued back to back from the page
with no debounce, no DOM and no rAF in the path. This is the IPC round trip that
the 41.4 ms browser-harness figure was a stand-in for.

| Query | Matches | Round trip p50 | **p95** | max | Go's own `took_ms` p50/p95 |
|---|---:|---:|---:|---:|---:|
| `browser` — the real worst case | 264 | 6 ms | **7 ms** | 10 ms | 4 / 5 ms |
| `lib` | ~35k | 3 ms | **4 ms** | 4 ms | 2 / 2 ms |
| `l` | ~70k | 3 ms | **4 ms** | 5 ms | 2 / 2 ms |
| `zzzzzz` — no match | 0 | 3 ms | **3 ms** | 4 ms | 2 / 2 ms |
| Empty browse | 85,565 | 0 ms | **1 ms** | 1 ms | 0 / 0 ms |

**The whole Wails bridge costs 2–3 ms on top of the Go search.** That is the
answer to the question this item was opened for, and it is far better than the
budget was drawn to allow: the brief reserved "the remaining ~90 ms of the
budget" for the bridge and the DOM, and the bridge wants three of them.

Under typing load the same round trip is 8 ms p50 and 10 ms p95 rather than 6
and 7, because a query change puts several page requests on a bridge that
serialises them.

### What to do about the 3 ms, and what not to

The honest arithmetic is 63 + 10 + 32 = 105, and the measured 103 ms p95 is
inside that. Three quarters of it is not work:

- **The 60 ms debounce is a deliberate choice**, documented in
  `picker-search.js` as spent knowingly out of this budget. It measures 62 ms
  p50 and 63 ms p95, so it costs exactly what it says. Dropping it to 30 ms
  would put the whole distribution inside the budget and would also treble the
  number of queries a typist issues. That is a product decision for whoever owns
  the search field, not a fix to make here — and the bridge measurement above is
  what it should be decided against, because at 10 ms p95 the argument for
  debouncing at all is weaker than it was when the bridge cost was unknown.
- **Up to 33 ms of the "resolved → painted" leg is measurement, not latency.**
  The mark is taken in the *second* rAF after the DOM write, deliberately, so
  that the frame carrying the rows is guaranteed to have been painted. That
  rounds every sample up by between one and two 16.7 ms frames. A single-rAF
  mark would report roughly 16 ms less and would sometimes be marking a frame
  that had not reached the screen. **The pessimistic mark is the one reported.**
  So 103 ms p95 is an upper bound; the true keystroke-to-pixels p95 is somewhere
  between about 86 and 103 ms, and this file does not know where.
- **The Go side is not the problem.** 4–5 ms of `took_ms` for the worst query in
  the archive, against 6.19 ms measured on machine A for a smaller corpus.

### What this replaces

The figure removed here read **41.4 ms p95 with a 5–15 ms stubbed promise**, and
132 ms against a deliberately slow stub. It was honest about what it measured
and it was not an IPC round trip: a resolved promise in the same JS context
skips process boundaries, JSON serialisation and the WebKit message pump
entirely. It also measured a frontend that has since been rebuilt. Its stated
frontend contribution of 25–35 ms is the one part that survives — the real
"resolved → painted" leg here is 26 ms p50, 32 ms p95, which is the same
quantity to the millisecond.

## How the app-level figures were taken

Every figure attributed to machine B came from **the real desktop binary**,
built inside the container from a committed tree with
`-tags "desktop,production,webkit2_41"` — without `desktop,production` the
binary compiles and then refuses to run. Both binaries were built from
`git archive` of a named commit rather than from the working tree, so that no
other packages's in-flight edits are inside a number here.

| | Cold start, bridge, scrolling | Idle memory, re-taken |
|---|---|---|
| `debark-gui` | `71d4d86` | `36ab130` and `67279ac` |
| `debark` (the CLI on `PATH`, and the sibling module) | `92ebdc2` | `a529c32` |
| Catalogue | the app's own `ubuntu:24.04/desktop` amd64 target, built against the live archive | the same catalogue, loaded from the cache that run wrote |

The second column is *Memory* only. That section is a before-and-after across
one commit to `internal/catalog`, so it needs two builds rather than one:
`/out/gui-before-instr` from `36ab130` and `/out/gui-after-instr` from
`67279ac`, both carrying the hook, because reaching the picker needs it. The
figures either side of the change are therefore taken with the same
instrumentation, the same catalogue and the same harness, and the only
difference between them is the commit.

Two binaries were built from `71d4d86` and differ only by the measurement
hook: `/out/debark-gui`, which is exactly the commit, and
`/out/debark-gui-instr`, which is the commit plus the hook. **The hook was
never written into this repository.** It was applied to a throwaway copy of the
tree inside the container, and it is three things:

1. `frontend/src/perf.js`, a 15 KB module that `index.html` loads after
   `main.js`. It marks the boot path, wraps `window.go.app.App.SearchPackages` to time the bridge,
   times a scroll loop, counts `.df-vlist__window` children, and reports through
   `window.runtime.LogPrint` — which Wails writes to the process's own stdout.
   `document.title` is not a usable channel: under Wails v2 it never reaches the
   GTK window name, verified over an eleven-minute run.
2. Two lines in `picker-list.js` that forward the virtualiser's existing
   `onMetrics` callback, which is where `updateMs` comes from. The seam was
   already there; nothing in the list was changed.
3. Six marks in `main.go`, written to stderr with `fmt.Fprintf`, around
   `main()`, the object graph, `wails.Run`, `OnStartup` and `OnDomReady` —
   which is where the cold-start phase table comes from.

**Where instrumentation could have moved a number, it is stated in that
section**: cold start was taken with the hook on the critical path and
cross-checked externally against the clean binary; idle memory was measured on
both binaries and they agree to 1.2 MiB, in the clean binary's *disfavour*.
The scroll and bridge figures are read out of the app's own timers and the hook
only reports them.

The trigger channel is `Ctrl+Alt+<digit>`, **not** `Ctrl+Alt+F<n>`: X binds
`Ctrl+Alt+F<n>` to `XF86_Switch_VT_<n>` in the default XKB map and the client
never sees the key. That cost an hour and is written down so it costs nobody
else one.

## Still to measure

Recorded honestly rather than left implied.

- **Frame figures on real hardware.** Everything in *Scrolling* is a software
  renderer with no GPU and no display to miss a vsync against. The update pass —
  the app's own work — transfers; the frame delta does not. Compositing cannot be
  enabled in this container to check, because it crashes its X stack. The
  black-rectangle artefact on the third round's backlog needs the same hardware
  run.
- **A genuinely cold page cache.** The cold-start figures are the 2nd–25th
  launch in a session, so `libwebkit2gtk`, `libgtk-3` and the binary are already
  resident. A first-ever launch pays disk for all three. Dropping the cache needs
  a privileged container and would flush the whole Docker VM, so it was not done.
- **Anything on Windows/WebView2.** No app-level figure in this file describes
  the Windows build. The engine, the process model and the memory shape are all
  different there.
- **The 20.8 MB the idle-memory budget is still over, if the budget means
  summed RSS.** None of it is in `internal/catalog`: the readiness screen, with
  no catalogue loaded at all, already sums to 374.3 MB. Moving it means either
  reading the budget as PSS or changing WebKit's process model, and both are
  decisions rather than measurements. See *The real process*.
- **A PSS figure this harness can hold still.** PSS moved 43 MB across two runs
  of the same screen with the same binary, because its divisor counts every
  process on the kernel mapping the page and this host runs many containers off
  one image. A trustworthy PSS number needs a quiet host or a different
  method.
- **One uninterrupted fuzz run of `ParsePackagesIndex` on a quiet machine.** See
  *Fuzzing `ParsePackagesIndex`*: four runs, all four died during minimisation.
  Until one completes, that parser is not fuzz-clean.
- **The keystroke budget re-taken if the 60 ms debounce changes.** The 3 ms miss
  is three quarters debounce and rAF rounding; a debounce decision would move the
  number more than any optimisation would.
## Desktop simplification: final measurement appendix

### S3/01: actual virtualizer with a synthetic backend

The picker behavior/performance journal
ran the existing `picker-list.demo.html` and actual list module from
18:56:17.915 to 18:56:59.270 UTC on 2026-09-07. It used WebKitGTK 2.52.6 on
Ubuntu 24.04.4 LTS, a 1040×720 GTK client, light theme, zoom 1 and GDK scale 1.
The software-rendered Xvfb environment has no physical display timing. Host
load was not sampled. This is a demo run with simulated page responses, not
the Wails application, bridge, downloaded catalogue or native process memory.

Source identity is GUI `69b974f615f5d43c7cc488c19eda02e946ebdad7` plus the
content manifest,
SHA-256 `4877584901fe5d00b91af14ee3a4e9446f62a3ada21d68cacc730ab2475474e8`.
The journal's `gui_diff_sha256: "snapshot"` is a label, not a patch hash;
the content manifest records the served files. Its top-level `scenario:
"target"` is the capture tool's default label; the recorded command correctly
selects `--demo picker-list`. Engine provenance is
`1eedbade8b5f4caa3c5c26e1bc33f08e50cfb88e` with patch SHA-256
`89648938ca88ee8ffac663988afd6b2f61af4a66107b637f66323c1ab0185e41`, although
this fixture run does not call that engine.

The corpus has 60,000 generated rows, 32px row height, a 582px list viewport,
50-row pages and a 24-page cache limit. Percentiles use the sorted sample at
`ceil(p × n) − 1`, clamped to the sample bounds. Reported timer granularity is
approximately 1ms; a zero-millisecond update sample means below that resolution.

| Measurement | Samples and result | Interpretation |
|---|---|---|
| Search to rendered result, 5–15ms backend | 20 inputs; p50 21ms, p95 24ms, max 26ms; all rendered | Meets <100ms in this fixture condition. |
| Search to rendered result, 40–100ms backend | 20 inputs; p50 78ms, p95 110ms, max 113ms; all rendered | Exceeds <100ms in this fixture condition. The aggregate green verdict checks only the fast-backend case. |
| Hard fling / sustained drag / full traversal | 460 / 460 / 280 frames; frame p95 17ms in each; update-pass p95 1ms in each | Zero frames exceeded the demo's drop threshold of 1.5× its measured 16ms cadence. 64 / 65 / 37 frames exceeded 16.7ms, so this is not an every-frame 60fps pass. |
| Recycled-row integrity | 40 jumps, 1,280 rows checked, zero mismatches | Passed the sampled stale-row check. |
| Overlapping query responses | 25 queries; 23 obsolete generations discarded; final total 2,063 matched expected, zero row mismatches | Passed the simulated ordering check. |
| Retained objects after traversal | 200 sweeps, 261 pages touched; final cache 12 of 24, pool 32 before and after | The configured cache and DOM bounds held. Heap measurements were unavailable (`null`); this does not measure bytes or RSS. |

Keyboard, preparation and ARIA checks also passed; the
[accessibility appendix](accessibility.md#desktop-simplification-final-evidence-appendix)
records their scope. No JavaScript error was recorded in this demo. The
separate S3 screen journals include driver failures and must not inherit the
demo's verdict.

### Current full-description cache benchmark

The focused benchmark at GUI commit
`23961921b39be8783683643880e3b34ae14c094b` was run with Go 1.26.0 on
Windows/amd64, Intel Core Ultra 9 285K (24 logical processors):

```powershell
go test ./internal/catalog -run 'TestUXDescription' -bench 'BenchmarkUXDescriptionCache70000' -benchtime=3x -count=1
```

It creates 70,000 synthetic entries, each with a 1,939-byte UTF-8 description
containing a repeated paragraph. Cache v2 occupies 15.09 MiB. These descriptions
are highly compressible and are not a representative distribution of archive
text. Setup, cache construction and compression occur outside the timer. Each
reported value is a Go benchmark mean over three operations in one run, not
a p95, cold-start or resident-memory measurement; host load was not recorded.

| Benchmark | Time/op | Allocated bytes/op | Allocations/op |
|---|---:|---:|---:|
| `CacheAnd50Rows` | 1.382ms | 21,952 | 155 |
| `FullSearchIndex` | 10.972ms | 38,328,517 | 210,016 |

Both cases validate an already resident cache image and read entries without
inflating their descriptions. The second also reads all 70,000 entries, builds
the search index and returns a 50-row query. Neither times a disk read,
Packages parse, network download, cache construction or Wails bridge. Allocated
bytes are cumulative allocations per operation, not retained memory. Thus the
results support bounded cache-page handling with the added field; they do not
establish the native cache-load, parse-time or <400MB idle budget.

The focused description tests and `go test ./internal/catalog ./internal/app
-count=1` passed. Coverage includes complete and truncated descriptions,
summary-only search rows, warm-cache Details projection, old cache rejection,
invalid/oversized compressed payloads and aggregate description limits. Only
description text already in the supplied Packages stream is available; this
change does not fetch Translation indexes.

### Current real-corpus cache and parser measurements

The coordinator ran the existing Go harness against source
`cdb693765ee5defb044e0b4a9ce6f6aa57335558`, with no Go changes in the temporary
frontend candidate used elsewhere in that session. The engine identity and
recorded dirty patch are the same as above. These tests ran on Linux/amd64,
Go 1.26, Intel Core Ultra 9 285K, using the staged Ubuntu corpus fetched by
`hack/fetch-indexes.go`; the
fetch journal,
inventory and phase output,
and benchmark output
are retained. Host load was not sampled by the Go harness. No test skipped;
both commands exited successfully.

```sh
DEBARK_PERF_CORPUS=/ux/perf-corpus go test ./internal/catalog \
  -run 'TestPerfCorpusInventory|TestPerfCatalogBuildPhases' -count=1 -v
DEBARK_PERF_CORPUS=/ux/perf-corpus go test ./internal/catalog \
  -run '^$' -bench 'BenchmarkPerfPackagesParse|BenchmarkPerfCacheLoad' \
  -benchtime=5x -count=3
```

The parser corpus is Ubuntu noble/noble-updates, main/universe: four Packages
indexes, 97,898,224 decompressed bytes and 85,225 stanzas before merging.
The full staged build holds 75,176 entries and writes a 17,138,230-byte cache
(16.344MiB). The fetcher also stages Translation files, but the catalogue
loader does not consume those descriptions; their presence on disk does not
establish full descriptions for this archive.

| Operation | Measured result | Budget and scope |
|---|---|---|
| Warm cache v2 disk load | Three runs of five loads: means 5.992, 5.152 and 5.350ms | Meets <500ms for `ReadFile`, CRC and structural validation. Cache creation, search-index setup and UI rendering are outside this timer. |
| Parse noble/main | Three five-operation means: 4.143–4.182ms, 6,099 stanzas | Decompressed bytes already resident. |
| Parse noble/universe | 42.723–43.660ms, 64,755 stanzas | Same parser-only method. |
| Parse noble-updates/main | 3.631–4.340ms, 6,223 stanzas | Same parser-only method. |
| Parse noble-updates/universe | 5.458–5.969ms, 8,148 stanzas | Same parser-only method. |
| Complete staged catalogue build | One run: 859ms; progress accounting charged 328ms to parse, 394ms to download and 136ms to save | The parse phase is below 10s. The transport served resident archive bytes; “download” is phase labeling, not measured network transfer. Rounding accounts for the 1ms difference in totals. |

The ranges are ranges of three benchmark means, not percentiles. The phase
test is a single run, and phase stretches interleave. These current backend
results do not supply live Wails search latency or first-install network time.

### Native idle memory

The native RSS journal
observed PID 23762 and its two WebKit descendants on display `:80`, from
19:39:41 to 19:39:59 UTC on 2026-09-07. The coordinator identified this as the
real Ubuntu 24.04 minimal catalogue session, with over 70,000 packages loaded,
no active job, and prior native build/copy activity. The memory journal itself
does not record the precise catalogue cache hash or active screen, so this is
not yet a controlled cache-only regression comparison.

The measured GUI is `f3fca6679ccf57920d4ceb7adc0cce4fcbf18990`, binary SHA-256
`c0fca9cce947d8b91d1a17a0930867b534ea2003679285a6ffc2ea7fdc70c6b0`.
The engine binary is
`b0128035ee641556203c2c52ad1f781ff46e5e4e58123479baa1577045003655`.
This identifies the observed binary rather than automatically assigning these
numbers to subsequent frontend candidates or the final repository HEAD.

Ten samples, approximately two seconds apart, read `/proc/*/smaps_rollup`
without forcing GC. Every sample was complete and summed to 437,872KiB:

| Process | RSS |
|---|---:|
| Wails / Go process | 207,936KiB |
| WebKit network process | 47,616KiB |
| WebKit web process | 182,320KiB |
| **Sum** | **437,872KiB = 448.381MB = 427.609MiB** |

This **exceeds the 400MB budget by 48.381MB (12.1%)** when MB means decimal
bytes; it also exceeds 400MiB. Nearest-rank p50, p95 and maximum are identical
in this ten-sample run. Summed PSS p95 was 176,208KiB (172.078MiB), which is a
different metric and does not turn the RSS miss into a pass. The one-minute
load average ranged from 0.128 to 0.165. Orca, Xvfb and unrelated processes are
excluded; RSS counts shared pages in each app process that maps them.

The running helper version was not hashed before later scratch edits, so its
exact source identity is not recoverable from this journal. The later change
to exclude incomplete memory samples does not alter these results: all ten
saved samples have `complete: true`. Retain the raw per-process rows and this
provenance limit.

### Remaining current native measurements

The baseline RSS journal
sampled baseline PID 8960 ten times. Its raw 533,404KiB sum **includes Xvfb
(83,320KiB) and openbox (19,912KiB)** because the original launcher made them
descendants of the application. They are fixture infrastructure, not app
memory. Removing only those two explicitly named rows yields an app subtotal
of 430,172KiB (440.496MB), constant across all ten samples. The raw total must
not be compared directly with the three-process current samples. This baseline
is GUI `cfccce71f40bd74ffcbe6e91a3d51d540c4f1eeb`; its binary and engine
provenance remain in the capture manifest.

The fresh Packages candidate journal
has ten complete samples: p50 416,068KiB and p95/max 416,072KiB (426.058MB).
It uses diagnostic binary SHA-256
`65a2efb18fb8df3de2d8a3f59350a19c7682369280a562ad72d8881bb5761b7a`,
based on `cdb6937` with a temporary, unsuccessful frontend accessibility
candidate. This is not the final source tree. The post-activity native session
was 7,700KiB above the normalized baseline; the fresh candidate's p95 was
14,100KiB below it. Different job histories, unspecified cache identity and the
candidate change prevent attributing either difference to the simplification.
All three app subtotals still exceed 400 decimal MB. The probe now explicitly
excludes named desktop infrastructure even when launcher ancestry includes it;
the historical raw journal is retained unchanged.

The same candidate's startup journal
records ten new processes on display `:82`, with a separate test profile and
warm OS/library caches. All reached a showing, enabled native AT-SPI Continue
button. Nearest-rank p50 was 208.203ms, p95/max 265.858ms. One transient null
accessibility child caused a retry within the observed interval. The observer
polls at least every 20ms and includes its query overhead; this is an upper
bound on accessible target readiness, not first paint. All owned process groups
completed cleanup without forced termination. It is below 1.5s for this
candidate and method, but does not certify the final binary or a truly cold
machine start.

### Final-source native startup

The accepted-source startup journal
is the current result. Its rebuilt GUI binary SHA-256 is
`479ff464ac2e9e395a7ad3df4394b4f8acc8ceef468dd956263cae6c0908a41e`, matching
the accepted native `S4-label` binary with the list and explicit search-label
changes. The engine binary remains the one recorded above. The run used the
Ubuntu 24.04.4 / WebKitGTK 2.52.6 native session on display `:90`, with default
GDK scale 1. All ten new processes reached a showing, enabled native Continue
button. Nearest-rank **p50 was 200.932ms and p95/max 258.566ms**, below 1.5s
for this measurement method.

All ten owned process groups completed cleanup without forced termination;
no observer errors were recorded. Recomputing the nearest-rank values from
the individual samples matches the saved aggregate. The one-minute load
average ranged from 0.327 to 0.420. The timer includes polling at intervals of
at least 20ms and AT-SPI query overhead: it is an upper bound on accessible
Target availability, not first paint. The OS page cache, shared libraries and
reused test profile were warm. This startup measurement does not time catalogue
loading or parsing, establish a truly cold machine start, or measure physical
display smoothness.

The earlier final5 startup journal
is retained as a historical checkpoint. It
uses production GUI `cdb693765ee5defb044e0b4a9ce6f6aa57335558`, binary SHA-256
`e9f60d2fc4b5a8f5f555c9d959f6698fdbc19e34435baa0cd9dc1e861ba6e0d3`, with the
same engine binary recorded above. It ran on display `:86` in the Ubuntu 24.04.4
/ WebKitGTK 2.52.6 native session with `GDK_SCALE=2`. All ten new processes
reached a showing, enabled Continue button through AT-SPI. Nearest-rank p50 was
212.823ms and p95/max 337.636ms, below the 1.5s budget for this method.

The observer includes its polling and accessibility-query overhead, so this
is an upper bound on accessible Target readiness, not first paint. The OS,
shared-library and reused test-profile caches were warm. One transient null
accessibility child caused a retry in the first sample; all samples reached
ready, and every owned process group completed cleanup without forced
termination. The one-minute load average ranged from 0.314 to 0.371.

A matched final-source cache-only memory comparison and live Wails
search-to-render latency remain unmeasured. Neither backend nor fixture timers
fill these app-level gaps. Windows/WebView2, a truly cold OS page cache and
physical-display smoothness remain unmeasured.
