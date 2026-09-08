# The catalogue cache on disk

Normative. `internal/catalog/cache.go`  implements this document;
where the two disagree, this document is wrong and should be fixed in the same
commit as the code.

`docs/dev/index-formats.md` is the companion measurement: real field coverage,
real index sizes, and the resident-memory experiment whose "strategy B" —
fixed-width records over one string arena, with `uint16` ids for the repeating
fields — is exactly what §4 below specifies. Where that document reports what
the archive actually ships, it wins over this one.

The interface this serves is frozen in `internal/catalog/iface.go`. Two symbols
there are normative for this document and must not be redefined in `cache.go`:

- `catalog.CacheRoot()` — the directory every cache lives under.
- `catalog.CacheFormatVersion` — the version this build reads and writes.
  Currently **2**, with 80-byte records and individually compressed extended
  descriptions. Version 1 used 72-byte records and is incompatible (§9).

## 1. What this has to achieve

| Requirement | Where it comes from |
|---|---|
| Load ~70,000 entries in **< 500 ms** | contract brief, performance budgets |
| Never crash on a damaged or half-written cache | definition of done: every error path is actionable |
| Invalidate on release, arch, components and index digest | contract brief, rule 3 of the budget section |
| No new Go dependency | contract brief, non-negotiable rule 6 |

And one thing that is *not* a requirement, which shapes everything else:

> **The cache is not a trust boundary.** Nothing installed on any machine is
> decided from it. It is a list of names an operator scrolls through; the
> packages that actually go into a bundle are resolved by apt inside
> `debark`, from the snapshot, against the live archive, long after this file
> has been forgotten. A cache that is stale, or even wrong, costs a misleading
> row in a picker — it cannot produce a short bundle.

That is why the integrity check below is a CRC and not a signature, why the
archive `Release` files are fetched but not verified here, and why the answer
to every problem in this document is "rebuild" rather than "refuse to start".

## 2. Location

```go
catalog.CacheRoot()  // <os.UserCacheDir()>/debark-gui/catalog
```

`os.UserCacheDir` already implements the platform conventions this project
needs, so there is no per-OS branch in the code and none should be added:

| Platform | Resolves to |
|---|---|
| Linux, `XDG_CACHE_HOME` set | `$XDG_CACHE_HOME/debark-gui/catalog/` |
| Linux, unset | `~/.cache/debark-gui/catalog/` |
| Windows | `%LOCALAPPDATA%\debark-gui\catalog\` |
| macOS (not a shipping target; dev machines only) | `~/Library/Caches/debark-gui/catalog/` |

Deleting `CacheRoot()`, at any time, with the app running or not, is always
safe and is always a valid recovery step. The settings screen shows this path
for exactly that reason.

## 3. Layout

One directory per target, named by `Target.CacheKey()`:

```
<CacheRoot()>/
  ubuntu-24.04-amd64-1f2a3b4c5d6e/
    meta.json                        ~2 KB   header, and the commit marker
    catalog.bin                      packed index; size depends on retained text
    catalog.bin.tmp-8f31a0           (only while a build is writing)
  ubuntu-24.04-arm64-77c1d9e40ab2/
    ...
```

### 3.1 The commit marker

`catalog.bin` is written first, `meta.json` second. **`meta.json` is the commit
marker**: a directory with no `meta.json`, or with an unparseable one, has no
usable cache, whatever else is in it. That gives partial-write detection at the
directory level for free — a build killed between the two renames leaves a
complete-looking `catalog.bin` that nothing will ever read — on top of the
in-file detection in §5.

### 3.2 `meta.json`

Small on purpose: `Ready()` reads this marker and stats the binary; it does not
read the binary's contents or decompress description metadata.

```json
{
  "format_version": 2,
  "cache_key": "ubuntu-24.04-amd64-1f2a3b4c5d6e",
  "identity_sha256": "9b1c…",
  "identity": "catalog-identity/v2\ndistro_id=ubuntu\n…",
  "target": { "kind": "base", "base_id": "ubuntu:24.04/desktop", "…": "…" },
  "index_digest": "sha256:4e0f…",
  "index_digest_source": "release",
  "built_at": "2026-09-06T10:22:31Z",
  "tool_version": "debark-gui 0.1.0",
  "entry_count": 71234,
  "bin_size": 19004416,
  "bin_crc32c": 3221225472
}
```

- `identity` is `Target.Identity()` verbatim. It is stored, not just hashed,
  because "why did my cache not hit?" is otherwise unanswerable in a bug
  report. It is ~30 short lines.
- `identity_sha256` is the SHA-256 of that text, lowercase hex.
- `target` is the `catalog.Target` that produced the cache, marshalled as-is.
  Informational; nothing is decided from it.
- `index_digest_source` is `"release"` or `"content"` — see §6.

### 3.3 `Ready(t)`

Cheap by construction: one `stat`, one small read, no network, no `catalog.bin`.

1. `dir := filepath.Join(CacheRoot(), t.CacheKey())`.
2. Read `meta.json`. Not found → `(false, nil)`. This is the ordinary
   first-run answer and is not an error.
3. `format_version != CacheFormatVersion` → `(false, nil)`; the directory is
   swept on the next successful build.
4. `identity_sha256 != sha256(t.Identity())` → `(false, nil)`. Cannot normally
   happen (the key is derived from the same text) but a hash collision or a
   hand-edited file must not be served.
5. `stat(catalog.bin).Size() != bin_size` → `(false, nil)`.
6. Otherwise `(true, nil)`.

`Ready` returns a non-nil error only when the cache directory itself could not
be consulted — a permission error, an I/O error. "There is nothing here" is
never an error.

## 4. `catalog.bin`

### 4.1 Why a hand-rolled binary layout

The 500 ms budget is not the hard part. The hard part is that **the format
decides how much work loading is**, and the only format that makes loading
almost no work is one where the file *is* the in-memory representation.

So: one `os.ReadFile` into one `[]byte`, a CRC over it, bounds and record-flag
checks, and the `CacheFile` is open. Opening that file does not materialise
entries or inflate descriptions. The production `Catalog` then materialises
rows once to build its search index (§11), retaining descriptions as compressed
strings until an exact-name `Get` requests one.

The alternatives, and why they lose:

**`encoding/gob`.** Decoding 70,000 structs means ~500,000 allocations (five
strings and a slice each) driven by reflection, then a live heap of 25–35 MB
that exists so the UI can display fifty rows. Realistic decode is in the
low hundreds of milliseconds *before* the GC work it causes — most of the
budget, spent on materialising rows nobody asked for. It also carries no
integrity check and no partial-write detection, and its self-describing type
stream means renaming a struct field silently changes the wire format without
changing any version number. Rejected on all three counts.

**`encoding/json`.** ~19 MB of JSON is 400–800 ms in `encoding/json` on its
own. It fails the budget before anything else is considered.

**Whole-file compression.** The original 19 MB estimate predicted 150–250 ms
to inflate a roughly 5 MB gzip file, losing the read-and-use property. Version
2 keeps the header, records and searchable text uncompressed, but stores each
extended package description as a separate zlib stream in the arena. Only
`Get` inflates that stream; loading and paging keep it compressed. This is part
of the version-2 layout, not an optional header flag: header `flags` stays zero.

**`encoding/binary` for the scalars, hand-rolled for the layout.** This is what
is specified below. `binary.LittleEndian.Uint32` and friends compile to a
single load on both target architectures; they are used for every scalar, and
nothing here uses `unsafe`.

### 4.2 Budget accounting

The following are planning estimates, not version-2 measurements. The measured row count
for noble is 70,854 (`index-formats.md` §2.1), which recommends sizing for
~75,000; the table below uses 70,000 so it can be compared against the brief's
round number. The record estimate below uses the current 80-byte size; the
ordinary-text estimate predates description retention. Extended-description
storage varies with the actual metadata and its compression ratio.

| Section | Size | How it is reached |
|---|---|---|
| Header | 64 B | fixed |
| Records | 5.6 MB | 70,000 × 80 B (version 2) |
| Category id array | ~12 KB | ~3,000 applications × ~2 categories × 2 B |
| Interned string table | ~4 KB | a few hundred sections, priorities, suites, components, category names |
| Category summary | ~2 KB | ~90 categories |
| Text blob | ~13.8 MB | names 1.3 MB, versions 1.1 MB, summaries 4.3 MB, homepages 1.3 MB, app names 0.1 MB, lower-cased search haystack 5.7 MB |
| Compressed descriptions in the same blob | variable, at most 64 MiB | sum of retained per-record zlib stream lengths |
| Trailer | 24 B | fixed |
| **Total** | **~19.4 MB plus compressed descriptions** | decimal MB estimates above; the metadata limit uses binary MiB |

Historical load-cost predictions for the original approximately 19 MB file:

| Step | Warm page cache | Cold NVMe |
|---|---|---|
| `os.ReadFile` 19 MB | 5–8 ms | 30–60 ms |
| `crc32.Checksum` (Castagnoli, hardware path) | 6–19 ms | same |
| Header and bounds validation | < 0.1 ms | < 0.1 ms |
| **Total** | **~15–30 ms** | **~40–80 ms** |

These estimates do not establish the version-2 load or resident-memory cost.
`BenchmarkLoad` and `BenchmarkUXDescriptionCache70000` exercise ordinary and
description-bearing caches; [performance.md](../performance.md) records the
measured runs and their source snapshots. The compressed-description limit is
a bound, not a claim about typical cache size or latency.

### 4.3 Conventions

- All integers are **little-endian**, unsigned, written with
  `encoding/binary.LittleEndian`.
- All offsets are byte offsets from the start of the file unless stated
  otherwise.
- String and compressed-description `(offset, length)` pairs are relative to
  the start of the text arena, not the file.
- All display strings are UTF-8, not NUL-terminated; every string is a
  (offset, length) pair into the text blob.
- Extended-description references point to bounded zlib streams in the same
  arena, not UTF-8 bytes until decompressed.
- Offset/length pairs of `(0, 0)` mean "absent". Byte 0 of the text blob is
  therefore reserved and must be a NUL, so that an absent string and an empty
  string at offset 0 cannot be confused.

### 4.4 Header — 64 bytes at offset 0

| Offset | Size | Field | Value |
|---|---|---|---|
| 0 | 8 | `magic` | ASCII `DFGCAT01` |
| 8 | 4 | `format_version` | `catalog.CacheFormatVersion` |
| 12 | 4 | `header_size` | 64 |
| 16 | 4 | `entry_count` | number of records |
| 20 | 4 | `flags` | reserved; **must be 0** in version 2 |
| 24 | 8 | `records_off` | offset of the record table |
| 32 | 8 | `records_len` | `entry_count × 80` |
| 40 | 8 | `catids_off` | offset of the category id array |
| 48 | 8 | `catids_len` | length in bytes (always even) |
| 56 | 8 | `interned_off` | offset of the interned string table |

Version 2 fixes the section order, so the remaining section offsets are
implied and are not stored: the interned table is self-delimiting (§4.7), the
category summary follows it (§4.8), and the text blob runs from the end of the
category summary to `trailer_off = filesize − 24`.

The two digits in the magic are part of the magic and are **not** the format
version — `format_version` is the only thing that decides compatibility.

### 4.5 Record table — `entry_count` × 80 bytes

Records are in **package-name ascending order**, byte-wise. That ordering is
load-bearing in three places, so it is an invariant of the format rather than a
convenience:

- `Get(name)` is a binary search over records, comparing name bytes in the text
  blob. ~17 comparisons, no index structure, no map to build at load.
- `Search`'s contract order ("name matches first, then summary matches, each by
  name ascending") falls out of a single forward scan with two output buckets,
  with no per-query sort. That is what keeps a keystroke inside 100 ms.
- Paging is stable across calls without the implementation storing anything.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `name_off` |
| 4 | 2 | `name_len` |
| 6 | 2 | `flags` — bit 0 = `IsApp`; bit 1 = `DescriptionTruncated`; bits 2–15 reserved, must be 0 |
| 8 | 4 | `version_off` |
| 12 | 2 | `version_len` |
| 14 | 2 | `app_name_len` |
| 16 | 4 | `app_name_off` |
| 20 | 4 | `summary_off` |
| 24 | 2 | `summary_len` |
| 26 | 2 | `homepage_len` |
| 28 | 4 | `homepage_off` |
| 32 | 4 | `haystack_off` |
| 36 | 4 | `haystack_len` |
| 40 | 2 | `section_id` |
| 42 | 2 | `priority_id` |
| 44 | 2 | `suite_id` |
| 46 | 2 | `component_id` |
| 48 | 2 | `arch_id` |
| 50 | 2 | `category_count` |
| 52 | 4 | `category_off` — index into the category id array, in **u16 units** |
| 56 | 4 | `icon_ref_off` |
| 60 | 2 | `icon_ref_len` |
| 62 | 2 | reserved, must be 0 |
| 64 | 4 | `installed_size_kib` |
| 68 | 4 | `download_size_kib` |
| 72 | 4 | `description_off` — zlib stream offset in the arena |
| 76 | 4 | `description_len` — compressed byte length, zero when absent |

Version 2 preserves text already supplied in a Packages `Description` field.
The first line remains `Summary`. When continuation lines exist, `Description`
contains that first line followed by the body: one continuation marker is
removed, a dot-only continuation becomes a paragraph break, and additional
indentation is preserved. The catalogue does not fetch `Translation-en`, so an
index that supplies only a summary has an empty `Description`, normally with
`DescriptionTruncated == false`. DEP-11 summary decoration does not overwrite
the retained Packages text.

The description fields reference one complete zlib stream per retained body.
The writer uses the standard library's `zlib.BestSpeed`, reuses its compressor,
and can copy an already packed `Entry.descriptionData` without inflating it.
`description_len` counts compressed bytes, not characters or decoded bytes.

| Bound | Enforcement and outcome |
|---|---|
| 64 KiB decoded bytes per package | The parser and writer clip retained text at a UTF-8 boundary and set `DescriptionTruncated`. |
| 64 MiB plain text per Packages parse and per merged catalogue | A body that would exceed the remaining budget is omitted and flagged; its summary and package row remain. Replacing a duplicate row releases the previous body's contribution to the merged budget. |
| 64 KiB + 1024 bytes per compressed stream | The writer omits and flags a stream that exceeds this limit. `CacheFile.Entry` also checks this limit before reading its reference. |
| 64 MiB compressed bytes per cache | The writer omits and flags bodies that would exceed the cumulative budget. The loader independently sums every record's declared `description_len` in `uint64` and rejects an excessive total as `ErrCacheCorrupt`. |
| 64 KiB + 1 byte per decode attempt | `Get` uses `io.LimitReader` to detect excess expansion; at most 64 KiB of valid decoded UTF-8 is accepted. |

An out-of-range or oversized per-record reference leaves `Description` empty,
sets the limitation flag and increments the cache's bad-reference counter.
A corrupt zlib stream, excess expansion or invalid decoded UTF-8 likewise
leaves `Description` empty and sets the flag; `CacheFile.Get` counts a failed
decode. `Summary` is preserved for the caller's visible fallback. A valid
clipped prefix may remain nonempty while the flag still warns that text is
missing. Details must disclose `DescriptionTruncated` in either case.

Search neither decodes nor matches extended text. `CacheFile.Entry` copies the
compressed arena slice into the private `descriptionData` string. The
production search index retains that packed string and `Get` decodes only its
returned entry copy; the retained index stays compressed (§11).

Two notes on the size fields:

- `installed_size_kib` is apt's `Installed-Size:` unchanged — it is already in
  KiB. `Entry.InstalledSizeKiB` carries it through unchanged.
- `download_size_kib` is apt's `Size:` (bytes) **divided by 1024, rounded up**,
  so that both fit a `uint32` and a 4 TiB package is not representable. The
  loader multiplies by 1024 to fill `Entry.DownloadSizeBytes`. The rounding
  loses at most 1023 bytes per package on a number that is displayed as
  "about 340 MB"; storing bytes would need 8 bytes per record for no visible
  benefit. **The lost precision must be documented in the UI's own rounding,
  not compensated for.**

`haystack_off`/`haystack_len` point at the pre-lower-cased search text for this
entry: `strings.ToLower(name + "\x00" + app_name + "\x00" + summary)`. It is
stored rather than computed because 70,000 `ToLower` calls per keystroke is the
difference between meeting the search budget and missing it, and because a
`bytes.Contains` over a contiguous region allocates nothing.

Lower-casing is `strings.ToLower` — full Unicode, not ASCII folding — applied
identically at write time and to the query at read time. Anything cleverer
(diacritic folding, normalisation) is out of scope and must not be added on one
side only.

### 4.6 Category id array

A packed `uint16` array. A record's categories are
`catids[category_off : category_off+category_count]`, each element an index
into the interned string table. Applications typically have 1–3; non-
applications have `category_count == 0` and `category_off == 0`.

### 4.7 Interned string table

Sections, priorities, suites, components, architectures and freedesktop
category names repeat across tens of thousands of entries, so they are stored
once and referenced by a 2-byte id.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `count` |
| 4 | `count` × 6 | entries: `u32 off`, `u16 len` into the text blob |

Id 0 is reserved and must be the empty string, so a `*_id` of 0 means "absent".

`count` must not exceed 65,535. A writer that would exceed it **fails the
build** with an error naming the field that overflowed, rather than truncating.
A real archive uses a few hundred; hitting this limit means the input is not
what this format assumed, which is a thing to find out about, not to paper
over.

### 4.8 Category summary

The precomputed answer to `Categories()`, so that call is a copy and not a scan
of 70,000 entries.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `count` |
| 4 | `count` × 8 | entries |

Each entry: `u8 tier` (0 = `TierApplication`, 1 = `TierSection`), `u8` reserved
(0), `u16 name_id` into the interned table, `u32 count`.

Entries are stored in the order `Categories()` must return them: applications
first, then sections, each ascending by name. The reader emits them in file
order and sorts nothing.

### 4.9 Text blob

Everything else: names, versions, summaries, homepages, application names, icon
refs, haystacks, the bytes the interned table points at, and version-2 zlib
description streams. Byte 0 is NUL (§4.3). The arena therefore contains both
UTF-8 strings and compressed binary slices; it is not one UTF-8 document.

A writer **may** deduplicate identical strings; a reader must not depend on
either behaviour. Deduplication is worthwhile for versions and for the
"Transitional package" class of summary and is cheap (one `map[string]uint32`
during the write), but it changes nothing about how the file is read.

### 4.10 Trailer — 24 bytes at the end of the file

Written last, and the in-file evidence that the write completed.

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | `payload_len` — must equal `filesize − 24` |
| 8 | 4 | `crc32c` — Castagnoli CRC-32 over bytes `[0, payload_len)` |
| 12 | 4 | reserved, must be 0 |
| 16 | 8 | `magic2` — ASCII `DFGCEND\n` |

CRC-32C rather than SHA-256 because this is an integrity check against
truncation and bitrot, not a signature: `hash/crc32` with
`crc32.MakeTable(crc32.Castagnoli)` takes the hardware path on amd64 and arm64
at 1–3 GB/s, so 19 MB costs single-digit to twenty milliseconds. SHA-256 over
the same bytes would cost 50–150 ms — a fifth of the whole load budget bought
with a property nothing here needs, because §1 already established that this
file is not a trust boundary. If it ever becomes one, the answer is a signature
over the archive's own `Release`, not a stronger hash over a derived file.

## 5. Loading, and being unable to load

```
load(dir):
  meta ← read meta.json                    → missing:      ErrNotBuilt
                                           → unparseable:  ErrCacheCorrupt
  if meta.format_version ≠ CacheFormatVersion              → ErrCacheVersion
  buf ← os.ReadFile(dir/catalog.bin)       → missing:      ErrNotBuilt
                                           → I/O error:    wrapped, returned
  if len(buf) < 88                                         → ErrCacheCorrupt
  if buf[0:8] ≠ "DFGCAT01"                                 → ErrCacheCorrupt
  if header.format_version ≠ CacheFormatVersion            → ErrCacheVersion
  if header.header_size ≠ 64 or header.flags ≠ 0           → ErrCacheCorrupt
  trailer ← buf[len(buf)-24:]
  if trailer.magic2 ≠ "DFGCEND\n"                          → ErrCacheCorrupt
  if trailer.payload_len ≠ len(buf)-24                     → ErrCacheCorrupt
  if crc32c(buf[:payload_len]) ≠ trailer.crc32c            → ErrCacheCorrupt
  validate every section extent lies within [64, payload_len)
  and that records_len = entry_count × 80                  → ErrCacheCorrupt
  if interned.count > 65,535                               → ErrCacheCorrupt
  if meta.entry_count ≠ header.entry_count                 → ErrCacheCorrupt
  if any record.flags has bits outside 0–1 set             → ErrCacheCorrupt
  if any record.reserved word ≠ 0                          → ErrCacheCorrupt
  if sum(record.description_len) > 64 MiB                  → ErrCacheCorrupt
  ok
```

The interned-count line is a *policy* check, not an arithmetic one, and that is
why it has to be written down separately. §4.7 states the ceiling as a rule for
the writer; the reader needs it too, because a count of 65,536 is small enough
that its body still fits inside the payload, so every extent check passes and
nothing else fires. Without it, ids past 65,535 truncate into the `uint16`
fields silently and rows point at the wrong strings inside a file whose CRC is
valid. Every other inflated count in this format is caught by the arithmetic;
this one had to be caught by the ceiling.

**Every one of those failures means "rebuild".** `cache.go` deletes the
directory and returns the sentinel; the caller shows the same "build the
catalogue" call to action it shows for `ErrNotBuilt`, plus a line in the log
saying which check failed. Nothing surfaces a raw error to the operator with no
action attached — that is a definition-of-done item for the whole product.

**With one exception: a directory something is writing into is never deleted.**
The last two checks above compare `meta.json` against `catalog.bin`, and those
two files are read at two different instants, so a disagreement is as likely to
mean "a rebuild committed in between" as "damage". A load that fails re-reads
the marker, re-stats the index, and looks for a `*.tmp-*` file beside them; while
any of those says the directory is moving, it waits and looks again, and if it
runs out of patience it reports the failure and leaves the bytes alone. See §10.

### 5.1 No code path may panic on cache content

A truncated file after a power cut is routine, and an application that crashes
on start-up because of it — when the fix is deleting a directory the operator
cannot see — is the worst outcome this format can produce.

Concretely:

- Validate every section extent **before** slicing it. After validation, all
  reads within a section are provably in range, so `binary.LittleEndian.Uint32`
  on a slice cannot panic.
- Validate every `(off, len)` pair against the text blob's bounds **at the
  point of use**, not only at load: a record can name a valid-looking range
  that lies outside the blob. An out-of-range pair yields the empty string and
  increments a counter; if the counter is non-zero at the end of a page, the
  page is served and the cache is scheduled for rebuild. Do not fail a
  keystroke over a bad row.
- `category_off + category_count` must be checked against the category id
  array's length before slicing.
- `cache.go` needs a `FuzzLoad` in `cache_test.go`: arbitrary bytes into the
  loader, which must return an error and never panic. Seed it with a valid
  file, that file truncated at several points, and that file with single-byte
  flips.

## 6. The index digest, and what invalidates a cache

Two different mechanisms, deliberately, because two different things change.

### 6.1 Identity — changes the directory

`Target.Identity()` is the canonical text; `Target.CacheKey()` is
`<distro>-<version>-<arch>-<first 12 hex of its SHA-256>`. The text is:

```
catalog-identity/v2
distro_id=ubuntu
version_id=24.04
codename=noble
arch=amd64
index=http://archive.ubuntu.com/ubuntu dists/noble/main/binary-amd64/Packages.gz
index=http://archive.ubuntu.com/ubuntu dists/noble/main/dep11/Components-amd64.yml.gz
index=http://archive.ubuntu.com/ubuntu dists/noble/universe/binary-amd64/Packages.gz
… one sorted line per index file …
```

Release, architecture, suites and components are all in it, because each of
them changes *which packages exist*. Enabling `universe` more than doubles the
catalogue, and a cache built without it must never be served for a target that
has it.

What is deliberately **not** in it: `PrettyName` and `SnapshotPath` (cosmetic —
a nicer label must not orphan a 19 MB file, and the same snapshot copied to a
second path must hit the same cache), `Kind` and `BaseID` (a base and a
snapshot describing the same release with the same sources genuinely are the
same catalogue), and `IndexDigest`, for the reason in §6.3.

A change to any of those facts produces a different key and therefore a
different directory. The old directory is simply not used again, and is swept
by §8.

### 6.2 Index digest — changes freshness within the directory

`Target.IndexDigest` is what changes when the archive publishes. It is derived
from the archive's own recorded hashes, so it costs a few kilobytes to compute
rather than the tens of megabytes it describes:

1. For each suite in `t.Suites()`, and each archive URI serving it, fetch
   `<uri>/dists/<suite>/Release`. A few kilobytes each; four suites is four
   requests.
2. Parse its `SHA256:` section. Each line is
   `<hex> <size> <path relative to dists/<suite>/>`, e.g.
   `main/binary-amd64/Packages.gz`.
3. For every `IndexRef` this target consumes (`t.IndexRefs()`), build one line:

   ```
   <ref.ID()> <hex>\n
   ```

   where `ref.ID()` is `<uri> <repo-relative path>` and `<hex>` is the recorded
   SHA-256 for that path. A ref the `Release` does not list — a component that
   publishes no DEP-11, which is normal — contributes `<ref.ID()> -\n`, so that
   a component gaining or losing DEP-11 still changes the digest.
4. Sort those lines byte-wise, concatenate, and take the SHA-256. Record it as
   `"sha256:" + lowercase hex`.

Set `index_digest_source` to `"release"`. If a `Release` carries no `SHA256:`
section at all — ancient or unusual archives — fall back to hashing the fetched
index bytes themselves in the same line format, and record
`index_digest_source: "content"`, so a support log shows that the cheap path
was not available.

The `Release` files are fetched over HTTPS and **not** signature-verified here:
this package ships no keyrings and is not a trust boundary (§1). `InRelease` is
not used, because the detached `Release` is the smaller fetch and the inline
signature would go unchecked either way.

### 6.3 Why the digest is not part of the key

Because a Debian or Ubuntu archive republishes its indexes daily. Keying the
directory on the digest would leave a fresh 19 MB directory behind on every
publish, and an operator who opens the app once a week for three months would
accumulate a quarter of a gigabyte of catalogues for one target. The digest
therefore lives *inside* the cache, and a rebuild overwrites the same
directory.

### 6.4 The invalidation rule

> A cache is **valid** when its `identity_sha256` matches the target's. It is
> **fresh** when, in addition, its `index_digest` matches the target's current
> one. An invalid cache is not used. A stale-but-valid cache **is** used, and
> the operator is told.

In full:

| Condition | Behaviour |
|---|---|
| No `meta.json` | `ErrNotBuilt`. Offer to build. |
| `format_version` mismatch | `ErrCacheVersion`. Delete the directory, offer to build. |
| `identity_sha256` mismatch | Treat as not built (cannot normally happen; the key derives from the same text). |
| Valid, marker and index disagree, **a commit is in flight** | `ErrCacheBusy`. Retry in a moment; **never delete**. A reader that lands between §7's two renames is not looking at damage. |
| Integrity failure in `catalog.bin`, with no commit in flight | `ErrCacheCorrupt`. Delete the directory, offer to build. |
| Valid, `t.IndexDigest` empty | Load and serve. Freshness is unknown, and unknown is not stale. |
| Valid, digests equal | Load and serve. |
| Valid, digests differ | Load and serve, **and** report `ErrStale` alongside so the UI can offer a refresh. |

Two things this rule refuses to do, both on purpose:

- **It never rebuilds silently on staleness.** An operator who opened the
  picker to type one package name must not be made to wait forty seconds for a
  download they did not ask for. The catalogue they have is browsable and the
  build re-resolves against the live archive regardless of what was on screen.
- **It never refuses to load a stale cache.** Working offline, or against a
  mirror that has moved on, must still get a picker.

The staleness check needs a live `IndexDigest`, which needs a network fetch,
which `Ready()` is forbidden to do. So the sequence is: `Ready()` answers from
`meta.json` and the picker opens; the app then computes the digest in the
background (§6.2, four small requests) and, if it differs, surfaces the refresh
affordance. The picker is never blocked on it.

## 7. Writing

```
tmp  := dir/catalog.bin.tmp-<8 random hex>
1. MkdirAll(dir, 0o755)
2. create tmp with mode 0o644, write header, sections, trailer
3. f.Sync(); f.Close()
4. os.Rename(tmp, dir/catalog.bin)
5. write meta.json the same way (tmp, Sync, Close, Rename)
6. on Linux, fsync the directory
7. remove any sibling *.tmp-* older than one hour
```

- **Never modify a cache file in place.** Always temp-then-rename, so a reader
  either sees the whole old file or the whole new one. A reader that has
  already `ReadFile`d the bytes is unaffected by a replacement.
- **Cancellation and failure remove the temp file** and leave the previous
  cache — if there was one — untouched and usable. `Build` returning an error
  must never leave a directory in a worse state than it found it.
- **Two processes writing the same key is fine.** `os.Rename` is atomic, last
  writer wins, and both wrote equivalent content. No lock file, no lock
  protocol, nothing to leave stale.
- **Windows caveat.** `os.Rename` replaces an existing file on Windows, but
  fails with a sharing violation if another process has the destination open.
  Retry. If it still fails, keep the in-memory catalogue (the build succeeded —
  only saving it did not), return a warning rather than an error, and let the
  next run try again. Losing a cache is a slow start-up; failing the build over
  it is a lost forty seconds of work.
- **The two renames are one commit, and step 5 losing that caveat is the one
  failure that costs the operator a cache.** Steps 1–4 can fail freely: nothing
  a reader sees has changed, and the previous cache still loads. Once step 4
  lands, the marker beside the index is stale, and the directory stays that way
  until one of two renames succeeds — the marker forward, or the previous index
  back. So the previous `catalog.bin` is hard-linked aside before step 4 (free,
  and unlike a rename it never contends with a reader), and steps 5 and that
  restore are attempted in turn until one lands. Only if neither does inside
  the budget is the stale marker removed. See §10.

## 8. Housekeeping

Each target adds a cache whose size depends on its rows and retained compressed
descriptions (§4.2). After a successful build, sweep
`CacheRoot()`:

- Remove `*.tmp-*` files older than one hour anywhere under the root.
- Remove directories with no `meta.json`, or an unparseable one, or a
  `format_version` other than the current one.
- Keep at most **four** target directories, evicting by oldest `built_at`. Four
  covers the realistic case — one operator, a couple of releases, two
  architectures — while bounding the number of retained catalogues.

Sweeping is best-effort: a failure to remove anything is logged and ignored. It
must never fail a build.

## 9. Changing this format

`catalog.CacheFormatVersion` is bumped in the same commit as any change to the
bytes, including adding a field to `catalog.Entry` that the record table has to
carry. There is **no migration, in either direction** — the cache is derived
data, and rebuilding it costs one download the operator has already survived
once. A version this build does not recognise, older or newer, is deleted and
rebuilt.

For the version-1 to version-2 transition, `Target.Identity()` starts with
`catalog-identity/v2` instead of `catalog-identity/v1`. Its SHA-256 and
`CacheKey()` therefore change even when all target sources are identical. A
normal version-2 lookup finds no cache at the new key and prepares one; it does
not reinterpret the old 72-byte records. Old incompatible directories are
eligible for the existing sweep after a successful build. If incompatible
metadata or a binary is encountered directly at the current location, the
version checks reject it with `ErrCacheVersion` through the normal load path.
A downgrade likewise needs a compatible cache or a rebuild. There is no
in-place conversion and no change to engine package resolution.

Header `flags`, record flag bits 2–15, and the record's reserved word must be
zero in version 2. Record flag bits 0 and 1 are defined as `IsApp` and
`DescriptionTruncated`; they are not reserved. A reader seeing a
non-zero reserved field with a matching `format_version` treats the file as
corrupt: it was written by something that thought it was compatible and was
not.

---

## 10. Corrections from the implementation

This specification was written before the format existed. Building it found
places where it was wrong, contradictory or silent. **Where this document and
`internal/catalog/cache.go` disagree, the code is authoritative** — it is the
thing that is tested against a 43-case corruption matrix and 1.5 billion fuzz
executions with zero findings (`docs/performance.md` carries the runs and the
one thing they do not prove).

### Two real bugs the spec would have caused

**A concurrent rebuild could delete a valid cache.** §7 says two writers need
no lock; §5 says a `meta.json`/`catalog.bin` disagreement is corruption and the
answer is deletion. Together those are wrong: a reader that reads the marker
*before* a rebuild's two renames and the index *after* them sees a count/size
mismatch and deletes a perfectly good directory. The fix is to re-read the
marker before concluding damage — if it moved, retry rather than delete.

**On Windows, a reader racing a rename gets a sharing violation, not a corrupt
file.** `os.ReadFile` during a replace fails with "the process cannot access
the file because it is being used by another process". §7 anticipated the
writer's side of this and not the reader's. Such an error must never be treated
as corruption; it is retried, like the writer's.

### Where the spec was wrong or silent

- **§4.3 contradicts itself on offsets.** "Byte offsets from the start of the
  file" cannot coexist with reserving arena byte 0 as a NUL, because file
  offset 0 holds the magic. String `(off,len)` pairs are **arena-relative**.
- **§5 omits checks** it depends on elsewhere: the trailer's reserved word, the
  arena's leading NUL, interned id 0 being empty, and the record reserved bits
  (implied only by §9).
- **§5 omits §4.7's interned-count ceiling**, which is the one omission that is
  not merely a tightening. §4.7 gives it as a writer rule and the reader
  enforces it too (`internCount > cacheMaxInterned` → `ErrCacheCorrupt`),
  because a declared count just over the ceiling has a body that still fits the
  payload: no extent check fires, and the ids truncate into `uint16` silently
  inside a CRC-valid file. It is now listed in §5 with the rest.
- **§4.4 fixes a section order that §5 never enforces**, so a file could point
  the records and the arena at the same bytes and pass validation. Sections are
  now required to be forward and non-overlapping.
- **§8's sweep races a concurrent build**: removing directories with no
  `meta.json` deletes exactly the state a build legitimately occupies between
  its two renames. The same age guard as temp files applies.
- **§8 is ambiguous about whether the just-written directory counts** toward
  the keep-four. It does, and it is exempt from removal — otherwise every build
  leaves five.
- **§6.4 covers an empty `t.IndexDigest` but not an empty `meta.index_digest`.**
  Either side empty means unknown, therefore not stale.
- **§3.3 does not say what an unparseable `meta.json` means.** `Ready` returns
  false, not an error; errors are reserved for "could not consult the
  directory".
- **§7 does not cover `meta.json` failing to write after `catalog.bin` was
  replaced.** Removing the stale marker, which is what the implementation did
  first, makes every window with that cache open read it as "not built" over a
  catalogue that was perfectly good — and it stays that way until some later
  build wins the same race. Measured on Windows, a rebuild racing four readers
  hit it in 3 of 20 runs. The commit now restores the index the marker
  describes instead, and only removes the marker when neither direction can be
  made to land.
- **§5's "delete it and rebuild" is wrong for a `meta.json`/`catalog.bin`
  disagreement.** The two files are read at two different instants and each is
  independently integrity-checked (JSON parse; CRC and bounds), so a
  disagreement means the two reads landed in different generations, not that
  either file is damaged. Re-reading the marker catches most of that, and it is
  not enough: a reader that keeps losing to a busy directory, or that arrives
  while a writer is stuck mid-commit and nothing is moving at all, sees a
  stable disagreement and would delete a good cache. A load therefore also
  compares `catalog.bin`'s size and modification time across the read, treats a
  `*.tmp-*` file in the directory as "a commit is in flight", retries for as
  long as any of those hold, and **never deletes a directory something is
  writing into**.
- **§4.5 gives no writer rule for a string longer than its `uint16` length.**
  It is clipped on a rune boundary.
- **Duplicate names are unstated**, but the binary search requires uniqueness.
  The last occurrence wins, matching `Entry.Version`'s "the source the target
  listed later".
- **§4.5's "full Unicode" and Go's final sigma.** `strings.ToLower` maps every
  `Σ` to `σ`, so `ΣΊΣΥΦΟΣ` lowercases to `σίσυφοσ`, not `σίσυφος`. Harmless
  here — the same function runs on the query — but not what "full Unicode"
  implies.
- **The original version-1 ~19 MB estimate** measured at **11.8 MB** for 70,000 entries. The
  record table matched the prediction exactly; the arena is where it differs,
  because the synthetic corpus writes shorter summaries than the real archive.
  This historical result predates 80-byte records and compressed descriptions.

## 11. How the cache is actually consumed

Worth writing down, because the format's central promise — *no entry is decoded
at load* — reads as though search must run over the raw accessors, and it does
not.

`CacheFile` exposes `Haystack`, `IsApp`, `SectionID` and `HasCategoryID` so a
caller can filter without materialising entries. `catalogImpl.load`
nonetheless calls `CacheFile.Entry` for each row and hands the results to
`newSearchIndex`. In version 2 this copies compressed description slices into
private `Entry.descriptionData` strings, without decoding them. The cache
buffer can then be collected independently of those strings.

Search returns row copies with short summaries. `searchIndex.Get` and
`CacheFile.Get` call `entryWithDescription` to inflate one requested body into
the returned entry. The stored search index remains compressed and does not
accumulate decoded bodies as the operator opens different Details dialogs.

The reason is not performance, it is having **one ranking implementation**. A
cache-backed search path and an in-memory one would be two implementations of
relevance that must agree forever, and the one exercised on a cold start would
differ from the one exercised after a rebuild — a difference no test would
naturally catch and every user would eventually hit.

The historical version-1 measurement was 6.3 ms to open an 11.8 MB cache,
about 5 ms to materialise its rows, and 1.8 µs for a direct exact-name lookup.
Those figures exclude version-2 description retention and decoding. Current
measurements belong in [performance.md](../performance.md); the format
guarantee here is bounded, deferred description decoding, not a fixed latency.
