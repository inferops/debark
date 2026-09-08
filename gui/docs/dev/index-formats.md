# apt index formats, measured

Reference for the four catalogue packages (`packages.go`, `dep11.go`,
`cache.go`, `search.go` / `catalog.go`). **Every number here was
measured against real archive files**, not taken from a specification. Where
this document and the DEP-11 / Debian policy specs disagree about what is
*present in practice*, this document is describing what the archive actually
ships and wins.

Corpus fetched **2026-09-06** from `http://archive.ubuntu.com/ubuntu/` and
`http://deb.debian.org/debian/`. Ubuntu 24.04 LTS (noble) is a frozen release
pocket — its `Release` is dated *Thu, 25 Apr 2024 15:10:33 UTC* — so these
figures are reproducible, not a moving target.

---

## 0. TL;DR — the six things that change the design

1. **The one-line summary a list row shows is in `Packages`.** `Description:`
   is present on **100.00%** of stanzas in both Ubuntu and Debian, always
   single-line. You do **not** need `Translation-en` to render the picker.
   You *do* need it for the detail panel's long body. See §3.
2. **`Description-md5` is the md5 of the *full* description, not the short
   one.** It is a join key into `Translation-en`, not a checksum of the field
   next to it. See §3.
3. **A single field value in `universe` is 70,830 bytes long.** A default
   `bufio.Scanner` fails with `bufio.ErrTooLong` on real data. See §4.1.
4. **`Package:` names repeat inside one index, at different versions.** The
   catalogue must dedupe *without comparing versions* — that is engine work and
   forbidden by rule 1. See §4.4.
5. **DEP-11 covers 3.01% of packages** (2,132 of 70,853). The applications tier
   is a small curated layer over a large `Section:`-organised catalogue, not a
   peer of it. See §6.
6. **yaml.v3 returns `*yaml.TypeError` on real DEP-11 data and it must be
   treated as non-fatal.** Treating it as fatal truncates the catalogue at
   component 449 of 2,587 — an 83% silent loss. See §7.3.

---

## 1. Staging the corpus

```
go run ./hack/fetch-indexes.go -out <dir-outside-the-repo> -icons
```

`-out` refuses any directory inside the module (it walks up looking for
`go.mod`), because the corpus is ~90 MB compressed and ~250 MB expanded.
The script fetches, gunzips alongside, and writes `MANIFEST.txt` with the URL,
compressed and uncompressed byte counts, and SHA-256 of every artefact. Flags:
`-only ubuntu|debian`, `-icons`, `-force`, `-timeout`.

The corpus used for this document:

| File | URL path under the mirror | Compressed | Uncompressed |
|---|---|---:|---:|
| Ubuntu noble `main` Packages | `dists/noble/main/binary-amd64/Packages.gz` | 1,808,488 | 7,165,069 |
| Ubuntu noble `universe` Packages | `dists/noble/universe/binary-amd64/Packages.gz` | 19,315,644 | 73,379,142 |
| Ubuntu noble-updates `main` Packages | `dists/noble-updates/main/binary-amd64/Packages.gz` | 1,561,629 | 6,793,357 |
| Ubuntu noble-updates `universe` Packages | `dists/noble-updates/universe/binary-amd64/Packages.gz` | 2,150,721 | 10,552,243 |
| Ubuntu noble `main` Translation-en | `dists/noble/main/i18n/Translation-en.gz` | 721,109 | 3,095,169 |
| Ubuntu noble `universe` Translation-en | `dists/noble/universe/i18n/Translation-en.gz` | 8,424,803 | 32,194,499 |
| Ubuntu noble `main` DEP-11 | `dists/noble/main/dep11/Components-amd64.yml.gz` | 664,565 | 1,923,546 |
| Ubuntu noble `universe` DEP-11 | `dists/noble/universe/dep11/Components-amd64.yml.gz` | 5,942,598 | 18,564,799 |
| Ubuntu noble Release | `dists/noble/Release` | 254,968 | — |
| Ubuntu noble InRelease | `dists/noble/InRelease` | 255,850 | — |
| Debian bookworm `main` Packages | `dists/bookworm/main/binary-amd64/Packages.gz` | 12,084,798 | 50,060,337 |
| Debian bookworm `main` DEP-11 | `dists/bookworm/main/dep11/Components-amd64.yml.gz` | 6,949,579 | 20,255,642 |
| Debian bookworm `main` Translation-en | `dists/bookworm/main/i18n/Translation-en.xz` | 6,107,304 | — (xz) |
| Ubuntu noble `main` icons 64px | `dists/noble/main/dep11/icons-64x64.tar.gz` | 155,618 | — |
| Ubuntu noble `universe` icons 64px | `dists/noble/universe/dep11/icons-64x64.tar.gz` | 7,747,169 | — |

SHA-256 of every file is in the corpus's `MANIFEST.txt`; the ones the fixtures
were cut from are repeated in `testdata/README.md`.

### 1.1 Compression: fetch `.gz`, not `.xz`

Both variants are published for Ubuntu. **Go's standard library decompresses
gzip and has no xz decoder**, so `.gz` is the stdlib-only choice, at a
transfer cost of about 28% (19.3 MB vs 15.0 MB for `universe`).

One exception matters: **Debian publishes `Translation-en` as `.xz` only** —
`dists/bookworm/main/i18n/Translation-en.gz` is a 404, and `Release` lists only
the plain and `.xz` forms. If the catalogue ever needs Debian long
descriptions in-process it needs an xz reader. `github.com/xi2/xz` is already
in the module graph transitively (via `pault.ag/go/debian`, which `debark`
depends on), so promoting it to a direct dependency adds no new code to the
build — but it is still a dependency decision and belongs in
`docs/dependency-review.md` per rule 6, not in a `go get`.

### 1.2 `Release` is the cache key material

`Release` / `InRelease` carry `Origin`, `Label`, `Suite`, `Version`,
`Codename`, `Date`, `Architectures`, `Components`, and then `MD5Sum:`,
`SHA1:` and `SHA256:` sections listing every index file with its size and
digest. The `SHA256:` entry for `main/binary-amd64/Packages` in the real file
is:

```
 8f6f71ae839c8cba390a7643fcbbdacddb0bc7d12c1583a2dd80a1f8443a30e5          7165069 main/binary-amd64/Packages
```

which matches the file `fetch-indexes.go` downloaded, byte for byte.
**`cache.go` should key the cache on these digests** rather than hashing the
downloaded index itself: they are authoritative, signed (in `InRelease`), and
available before the 19 MB download starts. `Date` and, where present, `Valid-Until`
say whether a cached catalogue is stale. A trimmed real `Release` is in
`testdata/ubuntu-noble-Release.excerpt`.

Note the checksum lines are a *folded* section: the section header is
`SHA256:` on its own line and every entry is a continuation line beginning with
a single space. Same folding rule as any other control field (§4.1).

---

## 2. `Packages`: stanza structure

RFC-822-ish "Debian control" format:

- A **stanza** is a run of lines terminated by a blank line. The last stanza
  may or may not be followed by one — handle both.
- A **field** is `Name: value`. Field names are case-insensitive by spec; in
  practice the archive is consistent, but do not rely on that for input the
  operator supplies.
- A line beginning with a **space or tab is a continuation** of the previous
  field. Within a continuation, a line whose only content is `.` represents a
  blank line in the rendered text. See §4.1.
- Fields are **not ordered**. Ubuntu and Debian emit different orders (compare
  the two fixtures: Ubuntu leads with `Package`/`Architecture`/`Version`,
  Debian with `Package`/`Version`/`Installed-Size`). Never index by position.

### 2.1 Stanza counts — the "~70,000" figure is right

| Index | Stanzas | Distinct `Package:` names |
|---|---:|---:|
| Ubuntu noble `main` | 6,099 | 6,099 |
| Ubuntu noble `universe` | 64,755 | 64,754 |
| **Ubuntu noble main + universe** | **70,854** | **70,853** |
| Debian bookworm `main` | 63,440 | 63,436 |

The contract brief's "roughly 70,000 binary packages" is **confirmed: 70,854**.

One correction the budgets should absorb: a *real target* also enables the
`-updates` and `-security` pockets. Adding `noble-updates` (6,223 + 8,148
stanzas) brings the union of distinct names to **75,176**. Sizing anything at
exactly 70,854 will be 6% short in the field; the 60,000-row scroll target and
the search budget should be measured against ~75,000.

### 2.2 Every field that actually occurs

Ubuntu noble `main` + `universe`, 70,854 stanzas, **64 distinct field names**.
"Folded" counts stanzas where the field had continuation lines; "non-ASCII"
counts stanzas where the value had a byte ≥ 0x80.

| Field | Count | % of stanzas | Folded | Non-ASCII | Longest value (bytes) |
|---|---:|---:|---:|---:|---:|
| `Package` | 70,854 | 100.00 | 0 | 0 | 75 |
| `Architecture` | 70,854 | 100.00 | 0 | 0 | 5 |
| `Version` | 70,854 | 100.00 | 0 | 0 | 55 |
| `Priority` | 70,854 | 100.00 | 0 | 0 | 9 |
| `Section` | 70,854 | 100.00 | 0 | 0 | 22 |
| `Origin` | 70,854 | 100.00 | 0 | 0 | 6 |
| `Maintainer` | 70,854 | 100.00 | 0 | 12 | 74 |
| `Bugs` | 70,854 | 100.00 | 0 | 0 | 42 |
| `Filename` | 70,854 | 100.00 | 0 | 0 | 179 |
| `Size` | 70,854 | 100.00 | 0 | 0 | 10 |
| `MD5sum` | 70,854 | 100.00 | 0 | 0 | 32 |
| `SHA1` | 70,854 | 100.00 | 0 | 0 | 40 |
| `SHA256` | 70,854 | 100.00 | 0 | 0 | 64 |
| `SHA512` | 70,854 | 100.00 | 0 | 0 | 128 |
| **`Description`** | **70,854** | **100.00** | **0** | 195 | 348 |
| **`Description-md5`** | **70,854** | **100.00** | 0 | 0 | 32 |
| `Installed-Size` | 70,711 | 99.80 | 0 | 0 | 7 |
| `Original-Maintainer` | 68,736 | 97.01 | 0 | 1,218 | 91 |
| `Homepage` | 64,971 | 91.70 | 0 | 0 | 206 |
| `Depends` | 62,436 | 88.12 | 0 | 0 | 8,637 |
| `Source` | 49,091 | 69.28 | 0 | 0 | 71 |
| `Multi-Arch` | 26,097 | 36.83 | 0 | 0 | 7 |
| `Provides` | 13,287 | 18.75 | 0 | 0 | **70,830** |
| `Suggests` | 12,085 | 17.06 | 0 | 0 | 4,013 |
| `Recommends` | 10,418 | 14.70 | 0 | 0 | 6,841 |
| `Replaces` | 8,788 | 12.40 | 0 | 0 | 9,890 |
| `Breaks` | 7,645 | 10.79 | 0 | 0 | 9,778 |
| `Built-Using` | 6,438 | 9.09 | 0 | 0 | 6,753 |
| `Task` | 5,441 | 7.68 | 0 | 0 | 852 |
| `Conflicts` | 3,916 | 5.53 | 0 | 0 | 6,201 |
| `Enhances` | 1,527 | 2.16 | 0 | 0 | 886 |
| `Pre-Depends` | 1,295 | 1.83 | 0 | 0 | 271 |
| `Ghc-Package` | 1,104 | 1.56 | 0 | 0 | 65 |
| `Ruby-Versions` | 835 | 1.18 | 0 | 0 | 3 |
| `Lua-Versions` | 145 | 0.20 | 0 | 0 | 15 |
| `Build-Ids` | 82 | 0.12 | 0 | 0 | 11,192 |
| `X-Cargo-Built-Using` | 96 | 0.14 | **12** | 0 | 6,323 |
| `Build-Essential` | 73 | 0.10 | 0 | 0 | 3 |
| `Static-Built-Using` | 42 | 0.06 | 0 | 0 | 4,833 |
| `Essential` | 23 | 0.03 | 0 | 0 | 3 |
| `Python-Egg-Name` | 16 | 0.02 | 0 | 0 | 14 |
| `Gstreamer-Elements` | 13 | 0.02 | 0 | 0 | 9,421 |
| `Gstreamer-Version` | 13 | 0.02 | 0 | 0 | 4 |
| `Original-Vcs-Browser` | 15 | 0.02 | 0 | 0 | 41 |
| `Original-Vcs-Git` | 15 | 0.02 | 0 | 0 | 45 |
| `Tag` | 6 | 0.01 | 0 | 0 | 270 |
| `Modaliases` | 6 | 0.01 | 0 | 0 | 1,772 |
| `Protected` | 5 | 0.01 | 0 | 0 | 3 |
| `Important` | 5 | 0.01 | 0 | 0 | 3 |
| `Gstreamer-Decoders` | 5 | 0.01 | 0 | 0 | 4,811 |
| `Gstreamer-Encoders` | 5 | 0.01 | 0 | 0 | 4,029 |
| `Gstreamer-Uri-Sources` | 4 | 0.01 | 0 | 0 | 150 |
| `Cnf-Visible-Pkgname` | 4 | 0.01 | 0 | 0 | 10 |
| `Cnf-Ignore-Commands` | 4 | 0.01 | 0 | 0 | 13 |
| `Gstreamer-Uri-Sinks` | 3 | 0.00 | 0 | 0 | 58 |
| `Cnf-Extra-Commands` | 2 | 0.00 | 0 | 0 | 17 |
| `Auto-Built-Package` | 2 | 0.00 | 0 | 0 | 13 |
| `Efi-Vendor` | 2 | 0.00 | 0 | 0 | 6 |
| `Go-Import-Path` | 2 | 0.00 | 0 | 0 | 28 |
| `Javascript-Built-Using` | 2 | 0.00 | 0 | 0 | 524 |
| `Python-Version` | 2 | 0.00 | 0 | 0 | 7 |
| `Postgresql-Catversion` | 1 | 0.00 | 0 | 0 | 9 |
| `Ubuntu-Oem-Kernel-Flavour` | 1 | 0.00 | 0 | 0 | 7 |
| `Built-Using-Newlib-Source` | 1 | 0.00 | 0 | 0 | 9 |

The long tail is real and vendor-specific (`Ubuntu-Oem-Kernel-Flavour`,
`Postgresql-Catversion`, `Efi-Vendor`). **A parser must skip unknown fields
silently.** Do not enumerate an allow-list and error on the rest; a snapshot
from a derivative will carry names nobody here has seen.

Debian bookworm `main` shows **53 distinct fields** — a mostly-overlapping but
not identical set. Notably Debian ships `Tag:` on 47.77% of stanzas (Ubuntu:
0.01%) and almost never ships `Original-Maintainer` (4 stanzas vs Ubuntu's
97%). Test against both fixtures.

---

## 3. `Description` vs `Description-md5` — **read this before designing the catalogue**

The question was whether Ubuntu's `Packages` carries only the md5 and pushes
real descriptions elsewhere, because that would move the list row's summary
out of the index. **The measured answer is a split, and the split is the whole
point:**

> **`Packages` carries the short summary. It does not carry the long body.**

Precisely, over all 70,854 Ubuntu noble `main`+`universe` stanzas:

| Measurement | Result |
|---|---|
| Stanzas with a `Description:` field | **70,854 — 100.00%** |
| ...of which are multi-line / folded | **0** |
| Stanzas with a `Description-md5:` field | 70,854 — 100.00% |
| Stanzas with both | 70,854 |
| `md5(Description + "\n") == Description-md5` | **5 of 70,854** |

Debian bookworm `main` behaves identically: `Description` on 100.00% of 63,440
stanzas, never folded, and the md5 matches the short line in 1 case out of
63,440.

So `Description:` in `Packages` **is** the one-line summary — exactly what a
list row needs — and `Description-md5` is **not** a checksum of the field
sitting next to it. It is the md5 of the *complete* description (the short
line, a newline, then the folded long body), and it exists as a **join key
into `Translation-en`**.

That join is total and exact. Over `main`+`universe`:

- `Translation-en` holds **65,031 distinct `Description-md5` values** across
  71,333 `Package:` rows (more rows than binary packages — translations are
  emitted per source-ish grouping and shared by identical descriptions).
- **70,854 of 70,854** `Description-md5` values from `Packages` resolve in
  `Translation-en`. Zero misses.
- `md5(Description-en + "\n") == Description-md5` for **65,031 of 65,031**
  entries. The digest is over the folded text with continuation-line leading
  spaces preserved, plus one trailing newline.
- The first line of `Description-en` equals the `Packages` `Description:`
  value in **70,854 of 70,854** cases. Zero disagreements.

### 3.1 What this means for the design

- **The picker does not need `Translation-en`.** Summaries for all ~70,000
  rows come from `Packages` alone, at 3.1 MB of summary text. `packages.go`
  can populate `Entry.Summary` directly and `search.go` can index it for
  search without a second download.
- **The detail panel does need it**, and it is a large fetch: 8.4 MB
  compressed / 32.2 MB uncompressed for `universe` alone — nearly half the
  `Packages` download again. Recommended: treat `Translation-en` as a
  **second-phase, optional** artefact. Build and cache the catalogue from
  `Packages` + DEP-11 first (the picker is then usable), then fetch
  translations in the background, or lazily per detail-panel open, keyed by the
  stanza's `Description-md5`.
- **`Description-md5` is worth keeping in the cache**, at 16 bytes packed
  (decode the hex), because it is the only key into the long text and it is
  stable across pockets and mirrors.
- **`Translation-en` is where the folded-`Description` parsing actually
  lives.** Because `Packages` never folds `Description`, a parser tested only
  against `Packages` will never exercise the continuation-line path for
  descriptions at all. `testdata/ubuntu-noble-main-Translation-en.excerpt`
  exists for exactly this; all 25 of its stanzas contain the ` .` blank-line
  marker.

`Translation-en` stanza shape — three fields, nothing else (verified: the
6,370 stanzas of `main` contain only these three field names):

```
Package: acct
Description-md5: 2411ebcaa9bca02b21c19f927d3e1bda
Description-en: GNU Accounting utilities for process and login accounting
 GNU Accounting Utilities is a set of utilities which reports and summarizes
 data about user connect times and process execution statistics.
 .
 "Login accounting" provides summaries of system resource usage based on connect
 time, and "process accounting" provides summaries based on the commands
 executed on the system.
 .
 The 'last' command is provided by the util-linux package and not included here.
```

---

## 4. Field value quirks that break naive parsers

### 4.1 Folded (continuation) fields

A line starting with space or tab continues the previous field. Two forms
occur:

- **`Description` / `Description-en`** (in `Translation-en`, never in
  `Packages`): first line is the summary; continuation lines are the body; a
  continuation line whose content is exactly `.` is a paragraph break and must
  render as a blank line, not a literal dot. Leading whitespace beyond the
  first space is significant (verbatim/preformatted text).
- **`Tag:`** in Debian, folded on **9,136** of 63,440 stanzas — a comma-
  separated list wrapped across lines. Ubuntu almost never folds it, so
  **this case only appears in the Debian fixture.**
- **`X-Cargo-Built-Using`** in Ubuntu `universe`, folded on 12 stanzas — the
  only folded field in the Ubuntu corpus besides descriptions.

Test the folding path against
`testdata/debian-bookworm-main-Packages.excerpt` (35 continuation lines) and
`testdata/ubuntu-noble-main-Translation-en.excerpt` (194).

### 4.2 The 70 KB field value — `bufio.Scanner` fails by default

The longest field value in the corpus is `Provides:` on
**`librust-winapi-dev`, 70,830 bytes on a single line** (Debian's copy is
75,639). This is a Rust crate's feature list rendered as virtual packages.

`bufio.Scanner`'s default maximum token is `bufio.MaxScanTokenSize` = 64 KiB
(65,536). Reading real `universe` data with a default scanner therefore fails.
Verified against the committed fixture:

```
Trap 1 -- default bufio.Scanner over the universe fixture
  lines read: 754
  scanner err: bufio.Scanner: token too long
  errors.Is(err, bufio.ErrTooLong): true

  with Buffer(_, 1 MiB): lines read: 1187, err: <nil>
```

**`packages.go` must call `sc.Buffer(make([]byte, 64<<10), 1<<20)`** (or use
`bufio.Reader.ReadString('\n')`, which has no token cap). This is not
hypothetical: `testdata/ubuntu-noble-universe-Packages.excerpt` contains that
stanza and a default scanner fails on it.

### 4.3 UTF-8

Values are UTF-8 and **1.99%** of Ubuntu stanzas (1,411 of 70,854) contain a
non-ASCII byte; Debian is 2.04%. Every value in the corpus is valid UTF-8 —
the analyser checked `utf8.ValidString` on every field of every stanza and
found no violation. Non-ASCII appears mostly in `Maintainer` /
`Original-Maintainer` (1,218 stanzas) and in `Description` (195 stanzas, e.g.
`adwaita-qt`, `agda-stdlib-doc`, `apertium-nno-nob`).

Consequence for `search.go`: **search must fold case and, ideally,
diacritics**. A byte-wise `strings.Contains` will not find `adwaita-qt` by
typing an unaccented form of an accented word in its summary.
`strings.ToLower` on UTF-8 is correct but allocates; for a 100 ms p95 budget
over 70,000 rows, pre-fold the searchable text once at build time and store the
folded form.

### 4.4 Duplicate `Package:` names in one index — and why it is a rule-1 hazard

Names are **not unique** within a single `Packages` file:

- Ubuntu noble `universe`: **1** duplicate —
  `android-platform-frameworks-native-headers` appears twice, at
  `1:10.0.0+r36-1` (from source `android-platform-frameworks-native`) and
  `1:34.0.4-1build3` (from source `android-platform-tools`).
- Debian bookworm `main`: **4** duplicates — `linux-doc`, `linux-doc-6.1`,
  `linux-source`, `linux-source-6.1`, each at two versions (e.g. `6.1.170-3`
  and `6.1.176-1`).

apt resolves these by version comparison. **This repository may not.** Rule 1
of the contract brief forbids comparing two version strings to decide
anything, and picking "the newer one" is exactly that.

The catalogue is a *browsing index*; it decides nothing. Two defensible
options, both compliant:

1. **Last-stanza-wins**, documented as an arbitrary index-order rule, not a
   "newest" rule. Cheap, deterministic for a fixed index, and honest.
2. **Keep both**, and let the row show that two candidates exist, deferring to
   `debark` for what actually installs.

Either way the choice must be a *positional* rule, never a version-ordering
one, and the code comment should say so — this is the kind of thing a reviewer
will flag as engine logic if the intent is not written down. Both duplicate
cases are in the fixtures.

### 4.5 Section names are component-prefixed outside `main`

Ubuntu `main` uses bare sections (`admin`, `gnome`, `libs`); `universe` uses
`universe/devel`, `universe/libs`, and so on. Across `main`+`universe` there
are **99 distinct `Section:` values** which collapse to roughly 58 real
sections. Debian `main` uses bare names throughout (53 distinct).

**`search.go` / `catalog.go` must strip the `component/` prefix before
grouping**, or the UI will show `devel` and `universe/devel` as two different
categories. Top sections after stripping, from the real data: `devel` (7,695
in universe alone), `libs`, `libdevel`, `python`, `doc`, `perl`, `rust`,
`utils`, `haskell`, `net`.

### 4.6 Other traps worth knowing

- **`Installed-Size` is absent on 143 stanzas** (0.20%) and is in **kibibytes**,
  not bytes. Absent must render as "unknown", not "0 B".
- **No stanza in either corpus has a repeated field name.** Duplicate *fields*
  within a stanza do not occur; duplicate *stanzas* (§4.4) do.
- **`Architecture: all`** is common and is not `amd64`. Both values appear in
  an `amd64` index (2 distinct values across the corpus).
- **CRLF**: the archive files are LF-only, but a snapshot the operator supplies
  may not be. Normalising `\r\n` costs nothing and avoids a `Description:`
  whose value ends in `\r`.

---

## 5. Memory: what to keep resident, measured

Measured on Go 1.26.0/amd64 with `debug.SetGCPercent(-1)` and forced GCs
around each phase, over the real 70,854-row `main`+`universe` corpus.

| Strategy | Struct size | Heap | Per row |
|---|---:|---:|---:|
| **A. Lean** — `Package`, `Version`, `Architecture`, `Section`, `Description`, `Installed-Size`, `Priority` as plain Go strings | 88 B | **15.3 MB** | 226 B |
| **B. Interned** — same data, one byte arena for name/summary/version + `uint16` ids for section/arch | 36 B | **7.6 MB** (5.2 MB arena + 2.4 MB records) | 110 B |
| **C. Fat** — every field of every stanza, `map[string]string` per stanza | — | **235.0 MB** | 3,477 B |

Raw field bytes for reference: everything = 75.3 MB, the 8 GUI-relevant fields
= 9.3 MB (12.3% of the total). Average stanza is 1,114 bytes of field data, of
which 137 bytes matter to the GUI.

**Conclusions for `cache.go` and `search.go` / `catalog.go`:**

- **Do not keep whole stanzas.** Strategy C alone is 235 MB against a 400 MB
  idle-resident budget, before the search index, the WebView, and Wails. It
  is a 15.4× overhead for data no screen displays.
- Strategy A at 15.3 MB is already comfortable and is the obvious, readable
  implementation. **Recommend starting there**; it leaves ~380 MB of headroom.
- Strategy B halves it again and makes the cache a near-verbatim `mmap`-able
  image (fixed-width records + one arena), which is the natural way to hit the
  **< 500 ms cache load** budget: read two blobs, no per-row allocation, no
  decoding. If `cache.go` wants a format that loads in tens of milliseconds, B
  is it. At 75,000 rows (§2.1) B is ~8.1 MB.
- Add roughly 1.1 MB per 70,000 rows for each additional 16-byte field
  (`Description-md5` packed), and 3.1 MB if summaries are stored unfolded as
  their own strings rather than arena slices.

---

## 6. DEP-11: how much is actually there

### 6.1 Coverage — the number that decides the applications tier

| Measurement | Ubuntu noble main+universe | Debian bookworm main |
|---|---:|---:|
| Binary packages in `Packages` | 70,853 | 63,436 |
| DEP-11 component documents | 2,587 | 2,381 |
| Distinct `Package:` referenced by DEP-11 | 2,189 | 2,023 |
| ...that exist in `Packages` | **2,132** | **2,021** |
| **Fraction of packages with any DEP-11 data** | **3.01%** | **3.19%** |
| Fraction with a `desktop-application` component | **2.65%** (1,878) | — |
| DEP-11 `Package:` refs with **no** matching stanza | **57** | 2 |

**The applications tier is a 3% curated layer, not a peer of the package
list.** It is still worth building — those 1,878 packages are precisely the
ones an operator browsing a picker is looking for, and they are the only ones
with a human name, an icon and a freedesktop category. But the UI must not
present "Applications" and "All packages" as two equal halves; the second is
37× the first, and the two-tier grouping in the `Catalog.Categories` contract
should reflect that (applications as a curated shortcut, `Section:` as the
complete index).

**The 57 dangling references are a real staleness signal.** Ubuntu's noble
DEP-11 header says `Time: 20231112T224135` — generated 12 November 2023,
five months *before* noble released on 25 April 2024. Components survive for
packages that were later removed or renamed (`0ad`, `cura`, `dolphin-emu`,
`anjuta`, `bless`…). **`dep11.go` must tolerate a `Package:` that does not
exist** and drop the component rather than creating a phantom catalogue row.

### 6.2 Component types

Ubuntu noble main+universe, 2,587 components:

| `Type` | Count |
|---|---:|
| `desktop-application` | 2,187 |
| `addon` | 244 |
| `generic` | 60 |
| `font` | 49 |
| `inputmethod` | 21 |
| `codec` | 13 |
| `console-application` | 11 |
| `icon-theme` | 1 |
| `web-application` | 1 |

Only `desktop-application` (and arguably `console-application`) belongs in an
"Applications" tier. `addon` components describe plugins and carry `Extends:`
pointing at another component's `ID` — they are not standalone installables
and should not become rows of their own.

### 6.3 Top-level keys actually present

Across all 2,587 Ubuntu components:

| Key | Count | % |
|---|---:|---:|
| `ID` | 2,587 | 100.0 |
| `Name` | 2,587 | 100.0 |
| `Package` | 2,587 | 100.0 |
| `Summary` | 2,587 | 100.0 |
| `Type` | 2,587 | 100.0 |
| `Icon` | 2,356 | 91.1 |
| `Description` | 2,345 | 90.6 |
| `Categories` | 2,219 | 85.8 |
| `Launchable` | 2,126 | 82.2 |
| `Keywords` | 1,351 | 52.2 |
| `ProjectLicense` | 1,150 | 44.5 |
| `Url` | 1,131 | 43.7 |
| `Provides` | 980 | 37.9 |
| `Screenshots` | 809 | 31.3 |
| `Releases` | 628 | 24.3 |
| `ContentRating` | 612 | 23.7 |
| `DeveloperName` | 494 | 19.1 |
| `ProjectGroup` | 384 | 14.8 |
| `Languages` | 342 | 13.2 |
| `Extends` | 245 | 9.5 |
| `Recommends` | 64 | 2.5 |
| `Requires` | 51 | 2.0 |
| `CompulsoryForDesktops` | 17 | 0.7 |
| `Supports` | 14 | 0.5 |
| `Suggests` | 6 | 0.2 |
| `Branding` | 2 | 0.1 |

`ID`, `Name`, `Package`, `Summary` and `Type` are the only guaranteed keys.
**`Categories` is missing on 14.2%** and `Icon` on 8.9% — both must be
optional in the decoded struct.

### 6.4 The `Categories` vocabulary, as used

**135 distinct category strings** occur. These are freedesktop menu categories
(main categories plus "additional" ones), and components carry a mix: 1,662 of
2,219 have more than one. Counting only the 13 freedesktop **main** categories,
across `desktop-application` components whose package exists:

| Category | Packages |
|---|---:|
| Game | 426 |
| Utility | 390 |
| AudioVideo | 287 |
| System | 211 |
| Education | 205 |
| Audio | 198 |
| Network | 197 |
| Science | 184 |
| Graphics | 160 |
| Development | 140 |
| Office | 133 |
| Settings | 102 |
| Video | 43 |

Only **5** desktop-applications carry no main category at all, so grouping the
applications tier by main category covers essentially everything. The counts
sum to more than 1,878 because a package appears under each main category it
declares — the UI must expect a package in several groups, or pick one.

The other 122 strings are additional/subsidiary categories (`LogicGame` 99,
`ArcadeGame` 74, `Math` 53, `Viewer` 53, `StrategyGame` 50, `Player` 49,
`TextEditor` 44, `BoardGame` 41, … down to `Productivity`, `Profiling`,
`VideoConference` at 1 each). **Do not build the UI's group list from the
observed vocabulary** — it has a 122-item long tail. Group by the 13 main
categories and treat the rest as searchable keywords.

---

## 7. DEP-11: the YAML itself

### 7.1 It is a multi-document stream

One `Components-amd64.yml` is a YAML stream: documents separated by `---` at
column 0. **The first document is a header; every subsequent document is a
component.** Ubuntu `main` has 1 header + 94 components; `universe` has
1 + 2,493. Concatenating components from several files means several headers
in one logical stream — handle a header appearing anywhere, not only at
offset 0. (`fetch-indexes.go` keeps files separate; the analyser detects a
header by the presence of a `File:` key rather than by position.)

**Header document**, verbatim from `dists/noble/main/dep11/Components-amd64.yml`:

```yaml
---
File: DEP-11
Version: '0.14'
Origin: ubuntu-noble-main
MediaBaseUrl: https://appstream.ubuntu.com/media/noble
Time: 20231112T224135
```

Debian bookworm's header, verbatim:

```yaml
---
File: DEP-11
Version: '0.16'
Origin: debian-bookworm-main
MediaBaseUrl: https://appstream.debian.org/media/bookworm
Time: 20230609T082314
```

Five keys, and only these five, in both. Note:

- **`Version` differs between distributions** — `'0.14'` on Ubuntu noble,
  `'0.16'` on Debian bookworm — and is quoted, so it decodes as a *string*,
  not a float. Do not hard-code either value or `float64` it.
- `Priority` and `Architecture`, which the DEP-11 spec permits in the header,
  are **absent from both**. Treat every header key as optional.
- `MediaBaseUrl` is where screenshots and remote icons live. **The GUI must
  not fetch from it** — rule 3 limits network access to the distro archives in
  the target's own sources and operator-typed vendor URLs, and
  `appstream.ubuntu.com` is neither an archive mirror nor operator-supplied.
  Parse the field, ignore the URL.
- `Time` is generation time, **not** release time, and is months stale (§6.1).

### 7.2 A `Type: desktop-application` component, verbatim

From `dists/noble/main/dep11/Components-amd64.yml`, `ID: htop.desktop`,
reproduced exactly (this document is in
`testdata/ubuntu-noble-main-Components-amd64.yml.excerpt`; the `Summary` map
is shown in full because its key order is the point):

```yaml
---
Type: desktop-application
ID: htop.desktop
Package: htop
Name:
  C: Htop
Summary:
  nl: Systeemprocessen tonen
  es: Mostrar procesos del sistema
  sv: Visa systemprocesser
  sr@ijekavianlatin: Prikaz sistemskih procesa
  sk: Zobraziť systémové procesy
  en_GB: Show System Processes
  fi: Katsele järjestelmän prosesseja
  ca: Visualitzeu els processos del sistema
  it: Mostra processi di sistema
  pt: Mostrar os Processos do Sistema
  tr: Sistem Süreçlerini Göster
  de: Systemprozesse anzeigen
  fr: Affiche les processus système
  nb: Vis systemprosesser
  pl: Pokaż procesy systemowe
  sl: Prikaz sistemskih opravil
  zh_TW: 顯示系統行程
  C: Show System Processes
  sr@latin: Prikaz sistemskih procesa
  sr@ijekavian: Приказ системских процеса
  nn: Vis systemprosessar
  sr: Приказ системских процеса
  uk: Перегляд системних процесів
  pt_BR: Mostra os processos do sistema
  zh_CN: 显示系统进程
  ko: 시스템 프로세스 보기
  da: Vis systemprocesser
  gl: Mostrar os procesos do sistema.
  ru: Просмотр списка процессов в системе
Description:
  C: >-
    <p>Htop is an ncursed-based process viewer similar to top, but it allows one to scroll the list vertically and horizontally
    to see all processes and their full command lines.</p>

    <p>Tasks related to processes (killing, renicing) can be done without entering their PIDs.</p>
Categories:
- System
- Monitor
- ConsoleOnly
Keywords:
  C:
  - system
  - process
  - task
Icon:
  cached:
  - name: htop_htop.png
    width: 48
    height: 48
```

Reading that carefully:

- `ID` is the desktop-entry id, **not** the package name — `htop.desktop` vs
  `htop`. Many ids are reverse-DNS (`org.gnome.Klotski`,
  `io.github.feralinteractive.gamemode`). Join to `Packages` on `Package`,
  never on `ID`.
- **`Name` and `Summary` are maps keyed by locale.** `C` is the
  untranslated/English value. Measured: `C` is present under `Name` in
  **2,587 of 2,587** components and under `Summary` in **2,587 of 2,587** —
  so an English label is always available. But **`C` is the *first* key only
  1,644 of 2,587 times (63.5%)**; here it is 19th. Look `C` up by key; never
  take the first entry.
- Locale keys carry modifiers and regions: `en_GB`, `pt_BR`, `zh_TW`,
  `sr@latin`, `sr@ijekavianlatin`. **1,105 distinct locale keys** occur.
- `Description` values are YAML **folded block scalars** (`>-`) containing
  HTML (`<p>`, `<ul>`, `<li>`). If the detail panel renders DEP-11 descriptions
  it is rendering untrusted HTML from an archive — sanitise or strip tags.
  Note `Description` here is markup, whereas `Translation-en`'s is plain text.
- `Keywords` is a map from locale to a **list** of strings — a different shape
  from `Name`/`Summary`. One struct field cannot cover both.
- `Icon` is a map with up to three kinds: `cached` (2,356 components),
  `stock` (2,032), `remote` (1,545). `cached` and `remote` are **lists of maps**
  with `name`/`width`/`height`; `stock` is a **plain string** (an icon-theme
  name). Decoding `Icon` as `map[string]string` fails; `map[string]any` or a
  custom type is required.

A component with **no `Icon`** and only two categories, verbatim
(`io.github.feralinteractive.gamemode`, also in the fixture) — note
`ContentRating` decodes to an empty nested map, another shape trap:

```yaml
---
Type: generic
ID: io.github.feralinteractive.gamemode
Package: gamemode
Name:
  C: gamemode
Summary:
  C: daemon that allows games to request a set of optimizations be temporarily applied
DeveloperName:
  C: Feral Interactive
ProjectLicense: BSD-3-Clause
Categories:
- Utility
- Game
ContentRating:
  oars-1.1: {}
```

### 7.3 yaml.v3 hits a `TypeError` on real data — and it must not be fatal

**`gopkg.in/yaml.v3` is already an (indirect) module dependency**, so using it
adds nothing to the build. It works, but there is one trap that silently
destroys the catalogue.

Ubuntu's `universe` DEP-11 contains a **duplicate mapping key**: component
`org.gnome.Klotski` (package `gnome-klotski`) declares `ca_ES` twice inside its
`Keywords` map, at lines 59,572 and 59,702 of the uncompressed file. yaml.v3
reports:

```
yaml: unmarshal errors:
  line 59702: mapping key "ca_ES" already defined at line 59572
```

The naive loop — `if err != nil { break }` — stops at **component 449 of
2,587**, discarding **83%** of the applications, with no crash and no obvious
symptom. The catalogue simply comes up mostly empty of apps.

`*yaml.TypeError` is a **soft** error: the decoder has already populated what
it could and the stream stays correctly positioned. Verified against
`testdata/ubuntu-noble-universe-Components-amd64.yml.excerpt`:

```
Trap 2 -- yaml.v3 over the universe DEP-11 fixture (duplicate ca_ES key)
  doc 1: ok  ID="" Package=""
  doc 2: *yaml.TypeError (SOFT) -- 1 error(s); fields still populated?
        line 725: mapping key "ca_ES" already defined at line 595
        ID="org.gnome.Klotski" Package="gnome-klotski" Name[C]="GNOME Klotski" locales in Keywords=0
  documents decoded after the TypeError: 2 (stream continued: true)
```

So `dep11.go` must:

```go
var te *yaml.TypeError
if err != nil && !errors.As(err, &te) {
    return err // only a non-TypeError ends the stream
}
// on a TypeError: keep the component, count it, carry on
```

Note the second half of that output: `Name["C"]` survived, but **`Keywords`
came back empty** — the specific map that hit the duplicate is abandoned.
The component is usable; the offending field is not. Decoding into `yaml.Node`
instead avoids the error entirely (all 2,589 documents decode clean), at the
cost of doing the field extraction by hand.

### 7.4 Decode cost — well inside the 10 s budget

Ubuntu noble `main`+`universe`, 19.5 MB uncompressed, 2,587 components,
Go 1.26.0/amd64, streaming `yaml.NewDecoder` over a 1 MiB `bufio.Reader`,
three runs each:

| Strategy | Time | Throughput |
|---|---:|---|
| Decode each document into `yaml.Node` | 284 / 283 / 280 ms | ~65 MB/s, ~8,600 components/s |
| Decode into a typed struct (soft-error handling, `Description` omitted) | 388 / 312 / 313 ms | ~62 MB/s |

Debian bookworm `main` (19.3 MB, 2,381 components): **273 ms**.

Peak allocation during the streaming decode is 198.7 MB `TotalAlloc` (churn,
not resident) while `HeapAlloc` after the pass is **3.5 MB** — streaming keeps
almost nothing live, so this does not threaten the 400 MB budget as long as
components are folded into the catalogue as they arrive rather than collected
into one big slice of fully-decoded documents.

For comparison, on the same machine the `Packages` side is much cheaper:

| Work | Time |
|---|---|
| Scan 70,854 stanzas, extract `Package:` only | 68 / 64 / 63 ms |
| Parse 70,854 stanzas into the 7-field lean row | 96 / 86 / 91 ms |

**Total parse phase ≈ 0.4 s** against a **10 s budget**. Parsing is not the
bottleneck; the 19.3 MB `universe` download is. Cost the progress UI
accordingly — show download progress in bytes, and treat parse as a single
short final step.

**No new dependency is needed.** yaml.v3 is fast enough by an order of
magnitude, and its one defect (§7.3) is a five-line workaround.

### 7.5 Localisation is 83% of the payload

Bytes of localised string values under `Name`, `Summary`, `Description`,
`Keywords` and `DeveloperName`:

| Corpus | `C`/`en`/`en_*` | Other locales | Non-English share |
|---|---:|---:|---:|
| Ubuntu noble main+universe | 2.3 MB | 11.4 MB | **83.0%** |
| Debian bookworm main | 1.8 MB | 12.1 MB | **87.1%** |

1,105 distinct locale keys in the Ubuntu corpus; the top ten by occurrence are
`C` (9,363), `fr` (3,013), `de` (2,911), `it` (2,693), `ru` (2,568), `es`
(2,549), `pl` (2,381), `uk` (2,214), `nl` (2,196), `tr` (2,150).

**Discard non-matching locales during decode, not after.** Keeping only
`C`/`en` drops ~83% of the string bytes the decoder would otherwise allocate
and store. Since the GUI runs on the builder machine and shows one locale, a
decoder that reads `Name`, `Summary` and `Keywords` as `yaml.Node` and pulls
out just the wanted key avoids materialising 11 MB of strings the UI will
never render. This is the single biggest lever on DEP-11 memory.

If localisation is ever offered, it should re-read the cached component blob
rather than keeping every locale resident.

### 7.6 Icons: probably not worth shipping

`icons-64x64.tar.gz` is a **flat** tarball of PNGs named
`<package>_<icon-file>.png` — e.g. `htop_htop.png`,
`gnome-system-monitor_org.gnome.SystemMonitor.png`. That name is exactly the
`Icon: cached: name:` value, so the join is trivial.

| Tarball | Files | Compressed |
|---|---:|---:|
| Ubuntu noble `main` 64×64 | 71 | 155,618 B |
| Ubuntu noble `universe` 64×64 | 2,301 | 7,747,169 B |
| Debian bookworm `main` 64×64 | — | 7,294,983 B |
| Ubuntu noble `main` 128×128 | — | 328,021 B |

**Recommendation: skip icons in the first implementation.** They cost ~7.9 MB
of extra download and an extraction step, they decorate the 3% of rows that
have DEP-11 (§6.1), and the picker is a virtualised list where 2,372 icons
would be decoded to serve ~50 visible rows. A `Section:`-derived glyph or a
plain type badge conveys the same grouping at zero cost. If they are added
later, the tarball is small enough to extract into the cache directory once
and reference by filename — but that is a polish item, not a first-round one,
and it should not gate the catalogue.

---

## 8. Fixtures

`testdata/` holds small, byte-exact slices of these real files, with full
provenance in `testdata/README.md`. 424 KB total. Coverage:

| Fixture | Exercises |
|---|---|
| `ubuntu-noble-universe-Packages.excerpt` | 52 stanzas, 49 distinct fields, the 70,830-byte `Provides` (§4.2), the duplicate `Package:` name (§4.4), UTF-8 in `Description` and `Maintainer`, folded `X-Cargo-Built-Using`, `Multi-Arch` (16), `Provides` (9), `universe/` section prefixes |
| `ubuntu-noble-main-Packages.excerpt` | 31 stanzas, Ubuntu-only fields — `Task`, `Essential`, `Protected`, `Important`, `Efi-Vendor`, `Gstreamer-*`, `Build-Ids` (11,192 B), `Postgresql-Catversion`, `Ubuntu-Oem-Kernel-Flavour`, bare section names |
| `debian-bookworm-main-Packages.excerpt` | 33 stanzas, folded `Tag:` (§4.1), two duplicate `Package:` names, UTF-8 `Maintainer`, Debian field order, no `Original-Maintainer` |
| `ubuntu-noble-main-Translation-en.excerpt` | 25 stanzas, folded `Description-en` with the ` .` paragraph marker in all 25, the `Description-md5` join (§3) |
| `ubuntu-noble-main-Components-amd64.yml.excerpt` | header + 32 components; 7 of the 9 `Type` values (`desktop-application` 9, `font` 11, `inputmethod` 4, `generic` 3, `addon` 2, `codec` 2, `console-application` 1); `libreoffice-draw.desktop` has 5 `Categories`; 12 components have no `Icon`; all three `Icon` kinds (`cached` 20, `remote` 19, `stock` 9); 2 `Extends` addons |
| `ubuntu-noble-universe-Components-amd64.yml.excerpt` | header + `org.gnome.Klotski` — the duplicate-locale-key `yaml.TypeError` (§7.3) |
| `debian-bookworm-main-Components-amd64.yml.excerpt` | header + 12 components, DEP-11 `Version: '0.16'`, Debian `MediaBaseUrl` |
| `ubuntu-noble-Release.excerpt` | header fields + the `SHA256:` entries for every index this catalogue reads (§1.2) |

---

## 9. Open items for the catalogue packages

- **`packages.go`**: enlarge the scanner buffer (§4.2); skip unknown fields
  (§2.2); decide and *document* the duplicate-name rule as positional, not
  version-ordered (§4.4).
- **`dep11.go`**: treat `*yaml.TypeError` as soft (§7.3); filter locales during
  decode (§7.5); tolerate `Package:` refs with no stanza (§6.1); `Icon` is not
  `map[string]string` (§7.2).
- **`cache.go`**: key on the `Release` `SHA256:` digests, not a hash of the
  download (§1.2); strategy B is the format that makes < 500 ms achievable
  (§5).
- **`search.go` / `catalog.go`**: strip the `component/` prefix from `Section:`
  (§4.5); pre-fold search text at build time (§4.3); size for ~75,000 rows,
  not 70,854 (§2.1); the applications tier is 3% of the catalogue and should
  be presented as a shortcut, not a half (§6.1).
- **Unresolved, needs a decision outside the catalogue packages**: whether
  `Translation-en` is fetched at all in the first round. The picker does not
  need it (§3.1); the detail panel's long description does. It is 9.1 MB
  compressed for main+universe.
