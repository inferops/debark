package catalog

// The on-disk catalogue cache. docs/dev/cache-format.md is the normative
// description of the bytes; this file implements it, and where the two
// disagree the document is wrong and must be fixed in the same commit.
//
// # The one design property everything else follows from
//
// Loading decodes nothing. One os.ReadFile, one CRC-32C over the buffer, a
// handful of bounds checks, and the catalogue is open: the file *is* the
// in-memory representation. A catalog.Entry is materialised only for the ≤ 500
// rows a Page actually returns. That is what makes the < 500 ms budget a
// sixfold margin rather than a fight, and it is why gob (half a million
// reflective allocations) and JSON (400–800 ms for 19 MB) were both rejected
// in §4.1 of the format document.
//
// The corollary is that a CacheFile aliases one big []byte and hands out
// sub-slices of it. Nothing in this package writes to that buffer after load,
// which is also what makes a *CacheFile safe to share between goroutines
// without a lock — see the concurrency notes on CacheFile.
//
// # Every failure is "delete and rebuild"
//
// The cache is not a trust boundary (format document §1): nothing installed on
// any machine is decided from it, so a damaged one costs a misleading row in a
// picker and never a short bundle. Consequently no condition in this file is
// fatal and none may panic. A truncated, garbled, half-written, wrong-version
// or foreign-endian file returns ErrCacheCorrupt / ErrCacheVersion /
// ErrNotBuilt, the directory is removed, and the caller shows the same "build
// the catalogue" call to action it shows on first run.
//
// # Endianness and alignment
//
// Every scalar is little-endian, read and written through encoding/binary's
// LittleEndian, which compiles to a single unaligned load on both target
// architectures and does not care about alignment. No section is padded, no
// unsafe is used, and a file written on a big-endian machine fails the magic
// or the CRC and is rebuilt rather than misread.

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// File and directory names inside a target's cache directory.
const (
	// CacheBinName is the packed index: the ~19 MB the loader reads.
	CacheBinName = "catalog.bin"
	// CacheMetaName is the header *and the commit marker*. It is written
	// second, so a directory without it has no usable cache whatever else it
	// contains — which is partial-write detection at the directory level, on
	// top of the CRC inside the file.
	CacheMetaName = "meta.json"
	// cacheTmpInfix marks a temporary file mid-write. Sweeping matches
	// "*.tmp-*", so both catalog.bin's and meta.json's temporaries are found.
	cacheTmpInfix = ".tmp-"
)

// Format constants. See docs/dev/cache-format.md §4.
const (
	cacheMagic       = "DFGCAT01" // header bytes 0..8; not the format version
	cacheEndMagic    = "DFGCEND\n"
	cacheHeaderSize  = 64
	cacheRecordSize  = 80
	cacheTrailerSize = 24
	cacheMinSize     = cacheHeaderSize + cacheTrailerSize

	// cacheMaxInterned is the interned table's hard ceiling: ids are uint16
	// and id 0 is reserved for "absent". A real archive uses a few hundred.
	cacheMaxInterned = 65535
	// cacheMaxShortString is the longest string a uint16 length can address.
	cacheMaxShortString = 65535
	// Descriptions stay compressed and outside the searchable text. Only Get
	// inflates one, bounded independently of the stream's claimed output size.
	cacheMaxDescriptionPacked  = packagesMaxDescriptionBytes + 1024
	cacheMaxDescriptionsPacked = 64 << 20

	// cacheKeepDirs is how many target directories survive a sweep, evicting
	// by oldest built_at. Four covers one operator with a couple of releases
	// and two architectures without accumulating a quarter of a gigabyte.
	cacheKeepDirs = 4
)

// cacheTmpMaxAge is how old a temporary file must be before a sweep removes
// it. Long enough that it cannot belong to a build running right now.
const cacheTmpMaxAge = time.Hour

// The two sides of a commit racing a read, both on Windows, where a rename
// cannot replace a file another process holds open.
//
// cacheCommitAttempts and cacheCommitWait are the writer's: how many times
// cacheCommitPair retries the *pair* of renames, and how long it waits between
// attempts for the reader that is about to finish. Three at 50 ms covers an
// ordinary read of a 19 MB index; after that the caller keeps the catalogue it
// built and writes the cache next run. Five rather than three because phase 2
// spends each attempt on two renames that a reader can refuse independently,
// and the fallback it protects — removing the marker — is the outcome this
// whole function exists to avoid.
//
// cacheRacePatience is the reader's: how long a load that failed its
// cross-checks keeps looking before it believes them, while the directory
// shows any sign of being written. It has to outlast a whole commit — every
// attempt, and the undo after each — because the alternative to waiting is
// deleting a cache that was never damaged. It is paid only by a load that has
// already failed, and only while something is demonstrably writing.
const (
	cacheCommitAttempts = 5
	cacheCommitWait     = 50 * time.Millisecond
	cacheRacePatience   = 2 * time.Second
)

// cacheCastagnoli is the CRC-32C table. Castagnoli rather than IEEE because it
// takes the hardware path on amd64 and arm64 (1–3 GB/s), and a CRC rather than
// a SHA-256 because this is an integrity check against truncation and bitrot,
// not a signature (§4.10).
var cacheCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCacheNotReplaced means the catalogue was built and is usable in memory,
// but the new cache file could not be moved into place — on Windows, because
// another process holds the destination open.
//
// It is deliberately not one of the frozen sentinels in iface.go: those all
// describe a cache the operator has to do something about, and this one does
// not. Losing a cache is a slow start-up next time; failing a build over it is
// forty seconds of the operator's work thrown away. Callers log it and carry
// on with the catalogue they already have in hand.
var ErrCacheNotReplaced = errors.New("catalog: cache file could not be replaced")

// CacheMeta is meta.json: the small file Ready answers from without touching
// the 19 MB beside it, and the commit marker that makes a directory count as
// having a cache at all.
type CacheMeta struct {
	// FormatVersion is the CacheFormatVersion that wrote this directory. A
	// different value is rebuilt, never migrated.
	FormatVersion int `json:"format_version"`
	// CacheKey is the directory's own name, repeated inside it so a copied or
	// renamed directory is recognisable in a bug report.
	CacheKey string `json:"cache_key"`
	// IdentitySHA256 is the lowercase hex SHA-256 of Identity below. It is
	// what Ready compares: the directory name derives from the same text, so a
	// mismatch means a hand-edited or hand-copied directory.
	IdentitySHA256 string `json:"identity_sha256"`
	// Identity is Target.Identity() verbatim, ~30 short lines. Stored rather
	// than only hashed because "why did my cache not hit?" is otherwise
	// unanswerable from a support log.
	Identity string `json:"identity"`
	// Target is the target that produced the cache. Informational; nothing is
	// decided from it.
	Target Target `json:"target"`
	// IndexDigest is the freshness marker: what the archive's own recorded
	// hashes said when this cache was written. Empty means unknown, which is
	// not the same as stale.
	IndexDigest string `json:"index_digest,omitempty"`
	// IndexDigestSource is "release" or "content" — which derivation produced
	// IndexDigest, so a support log shows whether the cheap path was
	// available. See §6.2.
	IndexDigestSource string `json:"index_digest_source,omitempty"`
	// BuiltAt is when the cache was written, UTC. The sweep evicts by it.
	BuiltAt time.Time `json:"built_at"`
	// ToolVersion is the build that wrote it, for support logs.
	ToolVersion string `json:"tool_version,omitempty"`
	// EntryCount, BinSize and BinCRC32C describe catalog.bin. EntryCount and
	// BinSize are cross-checked against the file on load; BinSize alone is
	// what makes Ready cheap.
	EntryCount int    `json:"entry_count"`
	BinSize    int64  `json:"bin_size"`
	BinCRC32C  uint32 `json:"bin_crc32c"`
}

// Index digest sources, recorded in CacheMeta.IndexDigestSource.
const (
	// CacheDigestRelease means the digest came from the suites' Release files
	// — a few kilobytes describing tens of megabytes, which is the whole
	// point of deriving it this way.
	CacheDigestRelease = "release"
	// CacheDigestContent means no Release carried a SHA256: section and the
	// index bytes themselves were hashed instead.
	CacheDigestContent = "content"
)

// CacheInput is everything SaveCache needs to write a target's cache.
type CacheInput struct {
	// Target is the target the catalogue was built for. Its Identity and
	// CacheKey go into meta.json.
	Target Target
	// Entries is the parsed catalogue, in any order: SaveCache sorts by name
	// because package-name ascending order is an invariant of the format, not
	// a convenience (it is what makes Get a binary search and Search's
	// contract order a single forward scan).
	//
	// Duplicate names are collapsed keeping the last occurrence, matching
	// Entry.Version's rule that a name offered by two sources takes the one
	// the target listed later. A duplicate would otherwise make the binary
	// search's answer arbitrary.
	Entries []Entry
	// IndexDigest and IndexDigestSource record what the archive's indexes
	// hashed to when this catalogue was parsed. Leave empty when unknown;
	// unknown is not stale.
	IndexDigest       string
	IndexDigestSource string
	// ToolVersion is the build writing the cache, e.g. "debark-gui 0.1.0".
	ToolVersion string
	// BuiltAt defaults to time.Now().UTC().
	BuiltAt time.Time
}

// CacheDir returns the directory holding this target's cache:
// CacheRoot()/Target.CacheKey(). It is the only place a cache path is built.
func CacheDir(t Target) (string, error) {
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, t.CacheKey()), nil
}

// CacheFile is a loaded catalogue: the file's bytes plus the offsets needed to
// address them. It decodes nothing until asked.
//
// # Concurrency
//
// A *CacheFile is immutable after CacheFromBytes returns. Every method is safe
// to call concurrently from any number of goroutines, which is the model the
// rest of the app needs: the bridge serves Search from one goroutine while
// another is part-way through a rebuild. The only mutable state is the
// bad-reference counter, which is atomic.
//
// The slices returned by NameBytes and Haystack alias the loaded buffer.
// Callers must not modify them; doing so would corrupt every subsequent read
// and, because the CRC was checked once at load, would not be detected.
//
// A rebuild never mutates a loaded CacheFile: writers always write a temporary
// and rename over the destination, so a reader holding these bytes keeps the
// whole old file and a reader arriving later gets the whole new one. There is
// no lock file and no lock protocol, on disk or in memory.
type CacheFile struct {
	buf   []byte
	meta  CacheMeta
	count int

	records []byte // count × 80
	catids  []byte // packed uint16, category ids
	blob    []byte // the text arena; byte 0 is a reserved NUL

	interned  []string
	internIdx map[string]uint16
	cats      []Category

	// bad counts (offset, length) pairs that pointed outside the text arena
	// at the point of use. A non-zero value means the cache is damaged in a
	// way the CRC already ruled out, so in practice it stays zero; it exists
	// so a page can be served and a rebuild scheduled instead of a keystroke
	// failing over one bad row (§5.1).
	bad atomic.Uint64
}

// Len reports how many entries the cache holds.
func (c *CacheFile) Len() int { return c.count }

// Size reports the size of catalog.bin in bytes, for the performance log.
func (c *CacheFile) Size() int { return len(c.buf) }

// Meta returns the cache's meta.json. It is the zero value for a CacheFile
// built by CacheFromBytes rather than read from a directory.
func (c *CacheFile) Meta() CacheMeta { return c.meta }

// BadRefs reports how many string references have pointed outside the text
// arena since load. Non-zero means serve the page and schedule a rebuild.
func (c *CacheFile) BadRefs() uint64 { return c.bad.Load() }

// Categories returns the precomputed two-tier grouping, in the order the
// Catalog contract requires: applications first, then sections, each ascending
// by name. It is a copy of a ~90-element slice, not a scan of 70,000 entries.
func (c *CacheFile) Categories() []Category {
	out := make([]Category, len(c.cats))
	copy(out, c.cats)
	return out
}

// InternedID returns the id of a repeating string — a section, priority,
// suite, component, architecture or freedesktop category — so a query can
// resolve one string once and then compare uint16s per row. ok is false when
// the cache has never seen the string, which means no row can match it.
func (c *CacheFile) InternedID(s string) (uint16, bool) {
	id, ok := c.internIdx[s]
	return id, ok
}

// InternedString returns the string an id names, or "" when the id is out of
// range.
func (c *CacheFile) InternedString(id uint16) string {
	if int(id) >= len(c.interned) {
		return ""
	}
	return c.interned[id]
}

// rec returns record i's 80 bytes. i must already be in range.
func (c *CacheFile) rec(i int) []byte {
	off := i * cacheRecordSize
	return c.records[off : off+cacheRecordSize : off+cacheRecordSize]
}

// NameBytes returns entry i's package name without copying it. The slice
// aliases the loaded buffer and must not be modified. Nil for an out-of-range
// index.
func (c *CacheFile) NameBytes(i int) []byte {
	if i < 0 || i >= c.count {
		return nil
	}
	r := c.rec(i)
	return c.slice(binary.LittleEndian.Uint32(r[0:4]), uint32(binary.LittleEndian.Uint16(r[4:6])))
}

// Name returns entry i's package name, copied. "" for an out-of-range index.
func (c *CacheFile) Name(i int) string { return string(c.NameBytes(i)) }

// Haystack returns entry i's pre-lower-cased search text —
// lower(name) NUL lower(app name) NUL lower(summary) — without copying it. The
// slice aliases the loaded buffer and must not be modified.
//
// It is stored rather than computed because 70,000 strings.ToLower calls per
// keystroke is the difference between meeting the 100 ms search budget and
// missing it, and because bytes.Contains over a contiguous region allocates
// nothing. Lower-casing is full-Unicode strings.ToLower, applied identically
// here and to the query; anything cleverer must not be added on one side only.
func (c *CacheFile) Haystack(i int) []byte {
	if i < 0 || i >= c.count {
		return nil
	}
	r := c.rec(i)
	return c.slice(binary.LittleEndian.Uint32(r[32:36]), binary.LittleEndian.Uint32(r[36:40]))
}

// IsApp reports whether DEP-11 describes entry i as a desktop application.
// It backs Query.AppsOnly and costs one byte-pair load.
func (c *CacheFile) IsApp(i int) bool {
	if i < 0 || i >= c.count {
		return false
	}
	return binary.LittleEndian.Uint16(c.rec(i)[6:8])&1 != 0
}

// SectionID returns entry i's interned apt Section: id, for filtering by the
// section tier without materialising a string per row. 0 means absent.
func (c *CacheFile) SectionID(i int) uint16 {
	if i < 0 || i >= c.count {
		return 0
	}
	return binary.LittleEndian.Uint16(c.rec(i)[40:42])
}

// HasCategoryID reports whether entry i carries the freedesktop category with
// this interned id, for filtering by the application tier. Applications
// typically carry one to three.
func (c *CacheFile) HasCategoryID(i int, id uint16) bool {
	if i < 0 || i >= c.count || id == 0 {
		return false
	}
	for _, got := range c.categoryIDs(i) {
		if got == id {
			return true
		}
	}
	return false
}

// categoryIDs returns entry i's category ids, decoding them from the packed
// array. The (off, count) pair is validated against the array's length before
// slicing (§5.1) — a record can name a plausible range that lies outside it.
func (c *CacheFile) categoryIDs(i int) []uint16 {
	r := c.rec(i)
	n := int(binary.LittleEndian.Uint16(r[50:52]))
	if n == 0 {
		return nil
	}
	off := int(binary.LittleEndian.Uint32(r[52:56]))
	total := len(c.catids) / 2
	if off < 0 || off > total || n > total-off {
		c.bad.Add(1)
		return nil
	}
	out := make([]uint16, n)
	for j := range out {
		out[j] = binary.LittleEndian.Uint16(c.catids[(off+j)*2:])
	}
	return out
}

// slice returns the text-arena bytes an (offset, length) pair names.
//
// A pair of (0, 0) means absent, which is why byte 0 of the arena is a
// reserved NUL: an absent string and an empty string at offset 0 must not be
// confusable. Any pair reaching outside the arena yields nil and increments
// the bad-reference counter rather than panicking — the whole point of
// validating at the point of use and not only at load.
func (c *CacheFile) slice(off, n uint32) []byte {
	if n == 0 {
		return nil
	}
	end := uint64(off) + uint64(n)
	if end > uint64(len(c.blob)) {
		c.bad.Add(1)
		return nil
	}
	return c.blob[off:end:end]
}

func (c *CacheFile) str(off, n uint32) string { return string(c.slice(off, n)) }

// Entry materialises row i. ok is false for an out-of-range index.
//
// This is the only place a catalogue row becomes Go strings, and it is called
// for the ≤ 500 rows a Page returns, never for all 70,000. Every string
// reference is bounds-checked against the text arena as it is read.
func (c *CacheFile) Entry(i int) (Entry, bool) {
	if i < 0 || i >= c.count {
		return Entry{}, false
	}
	r := c.rec(i)
	e := Entry{
		Name:                 c.str(binary.LittleEndian.Uint32(r[0:4]), uint32(binary.LittleEndian.Uint16(r[4:6]))),
		Version:              c.str(binary.LittleEndian.Uint32(r[8:12]), uint32(binary.LittleEndian.Uint16(r[12:14]))),
		AppName:              c.str(binary.LittleEndian.Uint32(r[16:20]), uint32(binary.LittleEndian.Uint16(r[14:16]))),
		Summary:              c.str(binary.LittleEndian.Uint32(r[20:24]), uint32(binary.LittleEndian.Uint16(r[24:26]))),
		DescriptionTruncated: binary.LittleEndian.Uint16(r[6:8])&2 != 0,
		Homepage:             c.str(binary.LittleEndian.Uint32(r[28:32]), uint32(binary.LittleEndian.Uint16(r[26:28]))),
		IconRef:              c.str(binary.LittleEndian.Uint32(r[56:60]), uint32(binary.LittleEndian.Uint16(r[60:62]))),
		Section:              c.internedAt(binary.LittleEndian.Uint16(r[40:42])),
		Priority:             c.internedAt(binary.LittleEndian.Uint16(r[42:44])),
		Suite:                c.internedAt(binary.LittleEndian.Uint16(r[44:46])),
		Component:            c.internedAt(binary.LittleEndian.Uint16(r[46:48])),
		Arch:                 c.internedAt(binary.LittleEndian.Uint16(r[48:50])),
		IsApp:                binary.LittleEndian.Uint16(r[6:8])&1 != 0,
		InstalledSizeKiB:     int64(binary.LittleEndian.Uint32(r[64:68])),
		DownloadSizeBytes:    int64(binary.LittleEndian.Uint32(r[68:72])) * 1024,
	}
	if packedLen := binary.LittleEndian.Uint32(r[76:80]); packedLen > 0 {
		if packedLen > cacheMaxDescriptionPacked {
			c.bad.Add(1)
			e.DescriptionTruncated = true
		} else {
			e.descriptionData = c.str(binary.LittleEndian.Uint32(r[72:76]), packedLen)
			if e.descriptionData == "" {
				e.DescriptionTruncated = true
			}
		}
	}
	if ids := c.categoryIDs(i); len(ids) > 0 {
		cats := make([]string, 0, len(ids))
		for _, id := range ids {
			if s := c.internedAt(id); s != "" {
				cats = append(cats, s)
			}
		}
		if len(cats) > 0 {
			e.Categories = cats
		}
	}
	return e, true
}

// internedAt resolves an interned id, counting an out-of-range id as a bad
// reference and yielding "".
func (c *CacheFile) internedAt(id uint16) string {
	if id == 0 {
		return ""
	}
	if int(id) >= len(c.interned) {
		c.bad.Add(1)
		return ""
	}
	return c.interned[id]
}

// Lookup finds the index of an exact package name, or reports false.
//
// A binary search over the record table — about seventeen comparisons against
// name bytes in the text arena, with no index structure to build at load and
// no map to keep resident. That is the first of the three things package-name
// ascending order buys (§4.5).
func (c *CacheFile) Lookup(name string) (int, bool) {
	if name == "" || c.count == 0 {
		return 0, false
	}
	want := []byte(name)
	i := sort.Search(c.count, func(i int) bool {
		return bytes.Compare(c.NameBytes(i), want) >= 0
	})
	if i < c.count && bytes.Equal(c.NameBytes(i), want) {
		return i, true
	}
	return 0, false
}

// Get returns the entry with this exact name.
func (c *CacheFile) Get(name string) (Entry, bool) {
	i, ok := c.Lookup(name)
	if !ok {
		return Entry{}, false
	}
	e, ok := c.Entry(i)
	e = entryWithDescription(e)
	if e.descriptionData != "" && e.Description == "" {
		c.bad.Add(1)
	}
	return e, ok
}

// entryWithDescription materialises only the one detail requested. Invalid or
// oversized metadata degrades visibly to the summary without breaking search.
func entryWithDescription(e Entry) Entry {
	if e.Description != "" || e.descriptionData == "" {
		return e
	}
	zr, err := zlib.NewReader(strings.NewReader(e.descriptionData))
	if err != nil {
		e.DescriptionTruncated = true
		return e
	}
	body, err := io.ReadAll(io.LimitReader(zr, packagesMaxDescriptionBytes+1))
	closeErr := zr.Close()
	if err != nil || closeErr != nil || len(body) > packagesMaxDescriptionBytes || !utf8.Valid(body) {
		e.DescriptionTruncated = true
		return e
	}
	e.Description = string(body)
	return e
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// CacheFromBytes validates a catalog.bin image and returns a cache over it.
//
// The buffer is retained, not copied: it becomes the catalogue. It must not be
// modified afterwards.
//
// Every failure returns ErrCacheCorrupt or ErrCacheVersion, wrapped with which
// check failed so the log can say. Nothing here panics on any input, which is
// what FuzzLoad exists to keep true: arbitrary bytes, a truncated file and a
// half-written one all have to land in this function safely.
func CacheFromBytes(buf []byte) (*CacheFile, error) {
	if len(buf) < cacheMinSize {
		return nil, fmt.Errorf("%w: file is %d bytes, shorter than a header and trailer", ErrCacheCorrupt, len(buf))
	}
	if string(buf[0:8]) != cacheMagic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrCacheCorrupt, buf[0:8])
	}
	if v := binary.LittleEndian.Uint32(buf[8:12]); v != CacheFormatVersion {
		return nil, fmt.Errorf("%w: file is version %d, this build reads %d", ErrCacheVersion, v, CacheFormatVersion)
	}
	if hs := binary.LittleEndian.Uint32(buf[12:16]); hs != cacheHeaderSize {
		return nil, fmt.Errorf("%w: header_size is %d, want %d", ErrCacheCorrupt, hs, cacheHeaderSize)
	}
	// flags is the format's only header-level extension point and must be
	// zero in version 2: a non-zero value with a matching version means the
	// file was written by something that thought it was compatible (§9).
	if f := binary.LittleEndian.Uint32(buf[20:24]); f != 0 {
		return nil, fmt.Errorf("%w: header flags are %#x, must be 0 in version %d", ErrCacheCorrupt, f, CacheFormatVersion)
	}

	// Trailer first: it is the in-file evidence that the write completed, and
	// checking it before anything else means a half-written file is rejected
	// without reading a single offset out of it.
	tr := buf[len(buf)-cacheTrailerSize:]
	if string(tr[16:24]) != cacheEndMagic {
		return nil, fmt.Errorf("%w: bad trailer magic %q", ErrCacheCorrupt, tr[16:24])
	}
	if r := binary.LittleEndian.Uint32(tr[12:16]); r != 0 {
		return nil, fmt.Errorf("%w: trailer reserved word is %#x, must be 0", ErrCacheCorrupt, r)
	}
	payloadLen := binary.LittleEndian.Uint64(tr[0:8])
	// Widen first, subtract second: len(buf) >= cacheMinSize > cacheTrailerSize
	// was checked above, so the difference cannot go negative, and doing the
	// subtraction in uint64 keeps it out of int in the first place.
	if payloadLen != uint64(len(buf))-cacheTrailerSize {
		return nil, fmt.Errorf("%w: trailer says %d payload bytes, file holds %d", ErrCacheCorrupt, payloadLen, len(buf)-cacheTrailerSize)
	}
	if got, want := crc32.Checksum(buf[:payloadLen], cacheCastagnoli), binary.LittleEndian.Uint32(tr[8:12]); got != want {
		return nil, fmt.Errorf("%w: crc32c is %08x, trailer says %08x", ErrCacheCorrupt, got, want)
	}

	entryCount := binary.LittleEndian.Uint32(buf[16:20])
	recordsOff := binary.LittleEndian.Uint64(buf[24:32])
	recordsLen := binary.LittleEndian.Uint64(buf[32:40])
	catidsOff := binary.LittleEndian.Uint64(buf[40:48])
	catidsLen := binary.LittleEndian.Uint64(buf[48:56])
	internedOff := binary.LittleEndian.Uint64(buf[56:64])

	if recordsLen != uint64(entryCount)*cacheRecordSize {
		return nil, fmt.Errorf("%w: records_len is %d, want %d × %d", ErrCacheCorrupt, recordsLen, entryCount, cacheRecordSize)
	}

	// Version 2 fixes the section order, so the sections must run forward from
	// the header without overlapping. Checking that here means every slice
	// taken below is provably in range and disjoint, and no later read can
	// reach into another section's bytes.
	if recordsOff < cacheHeaderSize {
		return nil, fmt.Errorf("%w: records start at %d, inside the header", ErrCacheCorrupt, recordsOff)
	}
	records, ok := cacheSection(buf, recordsOff, recordsLen, payloadLen)
	if !ok {
		return nil, fmt.Errorf("%w: record table [%d,+%d) is outside the payload", ErrCacheCorrupt, recordsOff, recordsLen)
	}
	if catidsOff < recordsOff+recordsLen {
		return nil, fmt.Errorf("%w: category ids start at %d, inside the record table", ErrCacheCorrupt, catidsOff)
	}
	if catidsLen%2 != 0 {
		return nil, fmt.Errorf("%w: catids_len is %d, must be even", ErrCacheCorrupt, catidsLen)
	}
	catids, ok := cacheSection(buf, catidsOff, catidsLen, payloadLen)
	if !ok {
		return nil, fmt.Errorf("%w: category id array [%d,+%d) is outside the payload", ErrCacheCorrupt, catidsOff, catidsLen)
	}
	if internedOff < catidsOff+catidsLen {
		return nil, fmt.Errorf("%w: interned table starts at %d, inside the category id array", ErrCacheCorrupt, internedOff)
	}

	// The interned table and the category summary are self-delimiting: each
	// starts with a count, so its extent has to be validated in two steps —
	// the count word first, then the body it implies.
	internCount, internBody, ok := cacheCountedSection(buf, internedOff, 6, payloadLen)
	if !ok {
		return nil, fmt.Errorf("%w: interned string table at %d is outside the payload", ErrCacheCorrupt, internedOff)
	}
	// An interned id is a uint16 and the format caps the table at
	// cacheMaxInterned, which is the same ceiling the writer enforces by
	// refusing to intern one value past it (cacheArena.intern). A file
	// declaring more was not written by this format, and one entry further on
	// the reverse index built below wraps outright: string 65,536 is filed
	// under id 0, which the format reserves for "absent", so every row naming
	// it silently loses the field.
	//
	// Nothing about that is memory-unsafe — every id is range-checked where it
	// is used, so an over-long table is read without a panic — and that is
	// exactly why it needs a sentinel. Silent mislabelling inside a file whose
	// CRC says it is intact is the failure mode this validation exists to turn
	// into a rebuild, so it gets the same answer as every other structural
	// violation.
	if internCount > cacheMaxInterned {
		return nil, fmt.Errorf("%w: interned string table declares %d strings, the format addresses at most %d", ErrCacheCorrupt, internCount, cacheMaxInterned)
	}
	catsumOff := internedOff + 4 + uint64(internCount)*6
	catCount, catBody, ok := cacheCountedSection(buf, catsumOff, 8, payloadLen)
	if !ok {
		return nil, fmt.Errorf("%w: category summary at %d is outside the payload", ErrCacheCorrupt, catsumOff)
	}

	blobOff := catsumOff + 4 + uint64(catCount)*8
	if blobOff > payloadLen {
		return nil, fmt.Errorf("%w: text arena starts at %d, past the payload end %d", ErrCacheCorrupt, blobOff, payloadLen)
	}
	blob := buf[blobOff:payloadLen:payloadLen]
	if len(blob) == 0 || blob[0] != 0 {
		return nil, fmt.Errorf("%w: byte 0 of the text arena is not the reserved NUL", ErrCacheCorrupt)
	}

	c := &CacheFile{
		buf:     buf,
		count:   int(entryCount),
		records: records,
		catids:  catids,
		blob:    blob,
	}

	// The interned table and the category summary are the two structures the
	// loader does materialise: a few hundred strings and ~90 categories, which
	// costs microseconds and makes every later category comparison a uint16.
	// Records are emphatically not materialised.
	c.interned = make([]string, internCount)
	c.internIdx = make(map[string]uint16, internCount)
	for i := 0; i < int(internCount); i++ {
		e := internBody[i*6 : i*6+6]
		off := binary.LittleEndian.Uint32(e[0:4])
		n := uint32(binary.LittleEndian.Uint16(e[4:6]))
		end := uint64(off) + uint64(n)
		if end > uint64(len(blob)) {
			return nil, fmt.Errorf("%w: interned string %d names [%d,+%d) outside the text arena", ErrCacheCorrupt, i, off, n)
		}
		s := string(blob[off:end])
		if i == 0 && s != "" {
			return nil, fmt.Errorf("%w: interned id 0 is %q, must be the empty string", ErrCacheCorrupt, s)
		}
		c.interned[i] = s
		if _, dup := c.internIdx[s]; !dup {
			c.internIdx[s] = uint16(i)
		}
	}

	c.cats = make([]Category, 0, catCount)
	for i := 0; i < int(catCount); i++ {
		e := catBody[i*8 : i*8+8]
		if e[1] != 0 {
			return nil, fmt.Errorf("%w: category summary %d has a non-zero reserved byte", ErrCacheCorrupt, i)
		}
		var tier CategoryTier
		switch e[0] {
		case 0:
			tier = TierApplication
		case 1:
			tier = TierSection
		default:
			return nil, fmt.Errorf("%w: category summary %d has tier %d", ErrCacheCorrupt, i, e[0])
		}
		nameID := binary.LittleEndian.Uint16(e[2:4])
		if int(nameID) >= len(c.interned) {
			return nil, fmt.Errorf("%w: category summary %d names interned id %d of %d", ErrCacheCorrupt, i, nameID, len(c.interned))
		}
		name := c.interned[nameID]
		id := SectionCategoryID(name)
		if tier == TierApplication {
			id = AppCategoryID(name)
		}
		c.cats = append(c.cats, Category{
			ID:    id,
			Tier:  tier,
			Name:  name,
			Count: int(binary.LittleEndian.Uint32(e[4:8])),
		})
	}

	// One pass over the records checking only the reserved bits — two uint16
	// loads per row, well under a millisecond for 70,000, and the one thing
	// §9 requires a reader to reject: a reserved field a future version set
	// without bumping format_version.
	var descriptionsPacked uint64
	for i := 0; i < c.count; i++ {
		r := c.rec(i)
		if f := binary.LittleEndian.Uint16(r[6:8]); f&^3 != 0 {
			return nil, fmt.Errorf("%w: record %d has reserved flag bits %#x set", ErrCacheCorrupt, i, f)
		}
		if r2 := binary.LittleEndian.Uint16(r[62:64]); r2 != 0 {
			return nil, fmt.Errorf("%w: record %d has a non-zero reserved word", ErrCacheCorrupt, i)
		}
		descriptionsPacked += uint64(binary.LittleEndian.Uint32(r[76:80]))
		if descriptionsPacked > cacheMaxDescriptionsPacked {
			return nil, fmt.Errorf("%w: packed descriptions exceed the metadata budget", ErrCacheCorrupt)
		}
	}

	return c, nil
}

// cacheSection bounds-checks [off, off+length) against the payload and returns
// the slice. All arithmetic is uint64 so a hostile offset cannot wrap.
func cacheSection(buf []byte, off, length, payloadLen uint64) ([]byte, bool) {
	end := off + length
	if end < off || end > payloadLen || payloadLen > uint64(len(buf)) {
		return nil, false
	}
	return buf[off:end:end], true
}

// cacheCountedSection reads a uint32 count at off and returns the count and
// the count × stride bytes that follow it.
func cacheCountedSection(buf []byte, off, stride, payloadLen uint64) (uint32, []byte, bool) {
	head, ok := cacheSection(buf, off, 4, payloadLen)
	if !ok {
		return 0, nil, false
	}
	n := binary.LittleEndian.Uint32(head)
	body, ok := cacheSection(buf, off+4, uint64(n)*stride, payloadLen)
	if !ok {
		return 0, nil, false
	}
	return n, body, true
}

// ReadCacheMeta reads and validates a directory's meta.json.
//
// A missing file is ErrNotBuilt and is the ordinary first-run answer, not a
// problem. An unparseable one is ErrCacheCorrupt, and a foreign format version
// is ErrCacheVersion.
func ReadCacheMeta(dir string) (CacheMeta, error) {
	_, m, err := cacheReadMeta(dir)
	return m, err
}

// cacheReadMeta is ReadCacheMeta plus the raw bytes, which LoadCacheDir uses
// to tell a damaged cache from one that was replaced while it was reading.
func cacheReadMeta(dir string) ([]byte, CacheMeta, error) {
	var m CacheMeta
	b, err := cacheReadRetry(filepath.Join(dir, CacheMetaName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, m, ErrNotBuilt
		}
		return nil, m, fmt.Errorf("catalog: reading %s: %w", CacheMetaName, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return b, CacheMeta{}, fmt.Errorf("%w: %s is not valid JSON: %v", ErrCacheCorrupt, CacheMetaName, err)
	}
	if m.FormatVersion != CacheFormatVersion {
		return b, m, fmt.Errorf("%w: %s says version %d, this build reads %d", ErrCacheVersion, CacheMetaName, m.FormatVersion, CacheFormatVersion)
	}
	// A negative count or size is corruption, not a value.
	//
	// docs/security-review.md §4.4 found this with a fuzz seed:
	// {"format_version":1,"entry_count":-1,"bin_size":-1} was accepted and
	// returned a CacheMeta that contradicted its own field comments. Nothing
	// downstream was harmed, because cacheLoadIndex compares EntryCount with
	// cf.Len() and CacheReadyDir compares BinSize with the file's real size,
	// and a negative matches neither -- so the answer was "rebuild", which is
	// right. It is refused here anyway because the next caller might not
	// compare: make([]X, meta.EntryCount) panics on a negative, and a
	// progress denominator of -1 renders as nonsense. Rejecting the class at
	// the parse boundary costs two lines and removes it for every reader.
	if m.EntryCount < 0 || m.BinSize < 0 {
		return b, CacheMeta{}, fmt.Errorf("%w: %s says %d entries and %d bytes, and neither can be negative",
			ErrCacheCorrupt, CacheMetaName, m.EntryCount, m.BinSize)
	}
	return b, m, nil
}

// cacheReadRetry reads a file, retrying the transient failure a concurrent
// replacement produces.
//
// On Windows, opening a file while another process is renaming over it fails
// with a sharing violation — "the process cannot access the file because it is
// being used by another process" — which is exactly what a second window
// reading the cache during a rebuild hits. It is not damage and must not be
// reported as one, so three attempts at 50 ms cover the replacement window and
// a genuinely unreadable file still fails.
//
// A file that does not exist is returned immediately: that is an answer, not a
// transient.
func cacheReadRetry(path string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		var b []byte
		b, err = os.ReadFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return b, err
		}
	}
	return nil, err
}

// LoadCacheDir loads the cache in one directory, following §5 exactly.
//
// On ErrCacheCorrupt or ErrCacheVersion the directory is removed before
// returning: every failure in this format means "rebuild", the operator cannot
// see the directory to delete it themselves, and leaving damaged bytes around
// only means failing the same way at the next start-up.
//
// A directory with no commit marker at all is left alone. It is either a first
// run or a build running right now in another process, and deleting it would
// turn that build's last thirty seconds into a failed save.
//
// # Reading while another process rebuilds
//
// A rebuild replaces catalog.bin and then meta.json, so a reader can interleave
// between the two renames and see a new index described by an old marker. That
// looks exactly like a corrupt cache — and the response to a corrupt cache is
// to delete it, which would destroy a cache that is in fact perfectly good.
//
// So before concluding damage, the directory is checked for having moved
// underneath the read. Three independent signs count, and any of them means
// "look again" rather than "delete":
//
//   - the marker changed since it was read;
//   - catalog.bin changed since the load started, which is the same event seen
//     from the other side and is the one that catches a reader who took the
//     *new* index and the *old* marker;
//   - a temporary file sits beside them, which is what a commit looks like
//     from outside for exactly as long as it is between its two renames — and
//     which is the only sign left when a writer is stuck losing a Windows
//     sharing violation on the marker and neither file is moving at all.
//
// A meta.json/catalog.bin disagreement is never by itself evidence of damage:
// the two files are read at two different instants, and each is independently
// integrity-checked on its own (JSON parse, and the format's CRC and bounds).
// A disagreement means the two reads landed in different generations. Deleting
// over it destroys a cache that is perfectly good — the failure this protocol
// exists to prevent, and one a reader that simply ran out of attempts while a
// build committed thirty times could still walk into.
//
// So the directory is only ever removed on a failure that reproduced against a
// directory nothing was touching. While one of the three signs holds, the load
// is retried for cacheRacePatience; after that the failure is reported and the
// bytes are left exactly where they are, because a caller that is told to
// rebuild loses a download and a caller whose cache was deleted loses it too —
// but the second one cannot be taken back. This is the whole concurrency
// protocol: no lock file, no lock protocol, just atomic renames, a re-read,
// and never deleting what somebody else is writing.
func LoadCacheDir(dir string) (*CacheFile, error) {
	patientUntil := time.Now().Add(cacheRacePatience)
	for attempt := 0; ; attempt++ {
		before := cacheStatIndex(dir)
		marker, meta, err := cacheReadMeta(dir)
		var cf *CacheFile
		if err == nil {
			cf, err = cacheLoadIndex(dir, meta)
			if err == nil {
				return cf, nil
			}
		}
		if marker == nil && errors.Is(err, ErrNotBuilt) {
			// No commit marker: nothing here, and nothing to remove.
			return nil, err
		}

		changed := cacheMarkerMoved(dir, marker) || !cacheStatIndex(dir).sameAs(before)
		writing := changed || cacheCommitInFlight(dir)

		// A foreign format version is decided from the marker alone and no
		// amount of waiting changes it, so it is reported at once — but it is
		// still not deleted out from under a build that is mid-commit.
		if writing && !errors.Is(err, ErrCacheVersion) && time.Now().Before(patientUntil) {
			// The first look again is free: the commonest case by far is a
			// marker that has already moved, and the answer is one directory
			// away. After that, wait for the writer rather than spinning on a
			// 19 MB read.
			if attempt > 0 || !changed {
				time.Sleep(cacheCommitWait)
			}
			continue
		}
		if writing {
			// Out of patience over a directory something is demonstrably
			// still writing. Never delete it, and do not call it corrupt: the
			// commit protocol's own two-rename window produces exactly the
			// marker/index disagreement ErrCacheCorrupt describes, and a
			// second window browsing while the first rebuilds would be told
			// its cache was damaged over a file that was about to be fine.
			// The underlying error is wrapped rather than dropped, so a
			// support log still says which disagreement was seen.
			return nil, fmt.Errorf("%w: %s: %v", ErrCacheBusy, dir, err)
		}
		if errors.Is(err, ErrCacheCorrupt) || errors.Is(err, ErrCacheVersion) || errors.Is(err, ErrNotBuilt) {
			cacheRemoveDir(dir)
		}
		return nil, err
	}
}

// cacheCommitInFlight reports whether the directory holds a temporary file,
// which is what a commit looks like from the outside for the whole time it is
// between its two renames — and what a build killed mid-commit leaves behind
// until the sweep's age guard collects it an hour later.
func cacheCommitInFlight(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, ent := range ents {
		if !ent.IsDir() && strings.Contains(ent.Name(), cacheTmpInfix) {
			return true
		}
	}
	return false
}

// cacheStamp is what a load saw of catalog.bin, cheaply enough to take twice:
// its size and modification time, or absent when there was no file there.
//
// It is the index's half of cacheMarkerMoved. Size alone would miss a rebuild
// that produced the same byte count, and a modification time alone would miss
// two writes inside one filesystem timestamp tick; together they catch the
// replacement of a 19 MB file by a rename, which is the only way this file
// ever changes.
type cacheStamp struct {
	size    int64
	mod     time.Time
	present bool
}

func cacheStatIndex(dir string) cacheStamp {
	st, err := os.Stat(filepath.Join(dir, CacheBinName))
	if err != nil {
		return cacheStamp{}
	}
	return cacheStamp{size: st.Size(), mod: st.ModTime(), present: true}
}

func (s cacheStamp) sameAs(o cacheStamp) bool {
	return s.present == o.present && s.size == o.size && s.mod.Equal(o.mod)
}

// cacheLoadIndex reads catalog.bin and cross-checks it against the marker.
func cacheLoadIndex(dir string, meta CacheMeta) (*CacheFile, error) {
	buf, err := cacheReadRetry(filepath.Join(dir, CacheBinName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// meta.json is written second, so a commit marker with no index
			// beside it is damage rather than a partial write.
			return nil, fmt.Errorf("%w: %s has %s but no %s", ErrNotBuilt, dir, CacheMetaName, CacheBinName)
		}
		return nil, fmt.Errorf("catalog: reading %s: %w", CacheBinName, err)
	}
	cf, err := CacheFromBytes(buf)
	if err != nil {
		return nil, err
	}
	if meta.EntryCount != cf.Len() {
		return nil, fmt.Errorf("%w: %s says %d entries, %s holds %d", ErrCacheCorrupt, CacheMetaName, meta.EntryCount, CacheBinName, cf.Len())
	}
	// The "!= 0" exemption treats a bin_size of zero as "unknown" and skips
	// the cross-check. CacheReadyDir has no such exemption (it compares
	// st.Size() with meta.BinSize outright), so the two paths disagreed about
	// what zero means, and only this one is reachable without going through
	// Ready (§4.4, "related, noticed while tracing it"). The disagreement is
	// resolved in CacheReadyDir's favour -- zero means zero -- because a
	// cache this build wrote always records a real size, so "unknown" can
	// only come from a file that was not written by CacheWrite -- which is
	// exactly the case a cross-check exists to catch.
	//
	// Nothing older is broken by tightening it: cacheReadMeta refuses a
	// FormatVersion that is not this build's before reaching here, and a
	// meta.json that predates the field could only carry a different one.
	// An honestly empty catalogue still matches, because len(buf) is then 0
	// as well.
	if meta.BinSize != int64(len(buf)) {
		return nil, fmt.Errorf("%w: %s says %d bytes, %s is %d", ErrCacheCorrupt, CacheMetaName, meta.BinSize, CacheBinName, len(buf))
	}
	cf.meta = meta
	return cf, nil
}

// cacheMarkerMoved reports whether meta.json changed since it was read, which
// means a rebuild committed underneath this load.
func cacheMarkerMoved(dir string, before []byte) bool {
	now, err := cacheReadRetry(filepath.Join(dir, CacheMetaName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return !bytes.Equal(before, now)
}

// LoadCache loads the cache for a target, applying the invalidation rule of
// §6.4.
//
// The distinction that rule turns on is the whole reason there are two
// mechanisms: identity decides *which directory*, and the index digest decides
// *freshness within it*. So a stale cache still loads. ErrStale is returned
// **alongside a usable *CacheFile**, and callers must render the catalogue and
// offer a refresh rather than showing an empty screen — an operator who opened
// the picker to type one package name must not be made to wait for a download
// they did not ask for, and someone working offline must still get a picker.
//
// Freshness is only ever a comparison between two known digests. Either side
// empty means unknown, and unknown is not stale.
func LoadCache(t Target) (*CacheFile, error) {
	dir, err := CacheDir(t)
	if err != nil {
		return nil, err
	}
	cf, err := LoadCacheDir(dir)
	if err != nil {
		return nil, err
	}
	if want := cacheIdentityHash(t); cf.meta.IdentitySHA256 != want {
		// Cannot normally happen: the directory name derives from the same
		// text. A hand-edited or hand-copied directory must not be served.
		return nil, fmt.Errorf("%w: %s holds a cache for a different target", ErrNotBuilt, dir)
	}
	if cacheStale(t, cf.meta) {
		return cf, fmt.Errorf("%w: built against %s, archive now publishes %s", ErrStale, cf.meta.IndexDigest, t.IndexDigest)
	}
	return cf, nil
}

// cacheStale applies §6.4's freshness half to a marker that has already been
// read: the digests disagree, and neither side is unknown.
//
// One function rather than a condition repeated at each caller, because the
// warm path answers this from meta.json without loading anything and the load
// path answers it from the file it just opened. Two spellings of "stale" that
// could drift is exactly how a picker ends up offering a refresh it does not
// need, or not offering the one it does.
//
// Both digests empty is the ordinary state today and means unknown: see
// docs/dev/cache-format.md §6.2 for what has to fetch a Release file before
// either side is ever populated.
func cacheStale(t Target, meta CacheMeta) bool {
	return t.IndexDigest != "" && meta.IndexDigest != "" && t.IndexDigest != meta.IndexDigest
}

// CacheReady reports whether a usable cache exists for this target.
//
// Cheap by construction (§3.3): one small read and one stat, never the 19 MB
// beside them and never the network. "There is nothing here" is the ordinary
// first-run answer and is not an error; a non-nil error means the cache
// directory itself could not be consulted.
func CacheReady(t Target) (bool, error) {
	dir, err := CacheDir(t)
	if err != nil {
		return false, err
	}
	return CacheReadyDir(dir, t)
}

// CacheReadyDir is CacheReady against an explicit directory.
func CacheReadyDir(dir string, t Target) (bool, error) {
	_, ok, err := cacheReadyMeta(dir, t)
	return ok, err
}

// cacheReadyMeta is CacheReadyDir, plus the marker it had to read anyway.
//
// It exists because two facts the UI needs — how many packages the cache holds
// and whether the archive has moved on since it was written — are both in
// meta.json, and the readiness check already opens it. Reading it twice, or
// materialising the 19 MB beside it to count rows, would both be answers to a
// question that has already been answered. The returned meta is only
// meaningful when ok is true.
func cacheReadyMeta(dir string, t Target) (CacheMeta, bool, error) {
	meta, err := ReadCacheMeta(dir)
	switch {
	case errors.Is(err, ErrNotBuilt), errors.Is(err, ErrCacheVersion), errors.Is(err, ErrCacheCorrupt):
		// Not built, a version this build does not read, or an unreadable
		// marker: all of them mean "offer to build", none of them is an error
		// for the operator to act on. The directory is swept by the next
		// successful build.
		return CacheMeta{}, false, nil
	case err != nil:
		return CacheMeta{}, false, err
	}
	if meta.IdentitySHA256 != cacheIdentityHash(t) {
		return CacheMeta{}, false, nil
	}
	st, err := os.Stat(filepath.Join(dir, CacheBinName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CacheMeta{}, false, nil
		}
		return CacheMeta{}, false, fmt.Errorf("catalog: checking %s: %w", CacheBinName, err)
	}
	if st.Size() != meta.BinSize {
		return CacheMeta{}, false, nil
	}
	return meta, true, nil
}

func cacheIdentityHash(t Target) string {
	sum := sha256.Sum256([]byte(t.Identity()))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// SaveCache writes a target's catalogue into dir and commits it.
//
// The protocol is §7's, and its two halves both matter. Each file is written
// to a temporary and renamed, so a reader sees either the whole old file or
// the whole new one and never a partial write. meta.json is written *second*
// and is the commit marker, so a machine that loses power between the two
// renames comes back to "no cache" rather than "half a cache".
//
// Failure and cancellation remove the temporary and leave any previous cache
// untouched and usable: a Build that returns an error must never leave the
// directory worse than it found it. That holds for a failure *between* the two
// renames as well, which takes a rollback rather than an ordering — see
// cacheLinkAside.
//
// Two processes writing the same key is fine and needs no lock: os.Rename is
// atomic, last writer wins, and both wrote equivalent content.
//
// A rename that cannot replace the destination — Windows, another process
// holding it open — returns the meta it would have written together with
// ErrCacheNotReplaced. The catalogue in memory is still good; the caller logs
// it and carries on rather than throwing away a completed build.
func SaveCache(ctx context.Context, dir string, in CacheInput) (CacheMeta, error) {
	var meta CacheMeta
	if err := ctx.Err(); err != nil {
		return meta, err
	}

	entries := cacheSortedUnique(in.Entries)
	img, err := cacheBuildImage(entries)
	if err != nil {
		return meta, err
	}
	if err := ctx.Err(); err != nil {
		return meta, err
	}

	built := in.BuiltAt
	if built.IsZero() {
		built = time.Now()
	}
	meta = CacheMeta{
		FormatVersion:     CacheFormatVersion,
		CacheKey:          in.Target.CacheKey(),
		IdentitySHA256:    cacheIdentityHash(in.Target),
		Identity:          in.Target.Identity(),
		Target:            in.Target,
		IndexDigest:       in.IndexDigest,
		IndexDigestSource: in.IndexDigestSource,
		BuiltAt:           built.UTC(),
		ToolVersion:       in.ToolVersion,
		EntryCount:        len(entries),
		BinSize:           int64(len(img)),
		BinCRC32C:         binary.LittleEndian.Uint32(img[len(img)-cacheTrailerSize+8:]),
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return meta, fmt.Errorf("catalog: encoding %s: %w", CacheMetaName, err)
	}
	metaJSON = append(metaJSON, '\n')

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return meta, fmt.Errorf("catalog: creating the cache directory: %w", err)
	}

	binPath := filepath.Join(dir, CacheBinName)
	metaPath := filepath.Join(dir, CacheMetaName)

	// Both files are written to their temporaries first, and only then
	// committed as a pair. Writing is the slow part and it touches nothing a
	// reader can see; the commit is two renames and is where every interesting
	// failure lives. See cacheCommitPair.
	binTmp, err := cacheWriteTemp(binPath, img)
	if err != nil {
		return meta, err
	}
	defer func() { _ = os.Remove(binTmp) }()

	metaTmp, err := cacheWriteTemp(metaPath, metaJSON)
	if err != nil {
		return meta, err
	}
	defer func() { _ = os.Remove(metaTmp) }()

	if err := ctx.Err(); err != nil {
		return meta, err
	}
	if err := cacheCommitPair(binTmp, binPath, metaTmp, metaPath); err != nil {
		return meta, err
	}
	cacheSyncDir(dir)

	// Housekeeping is best-effort and must never fail a build (§8), so the
	// error is discarded here rather than at the bottom of cacheSweep: the
	// sweep reports what went wrong to whoever calls it directly, and this
	// caller has already written the cache the operator asked for.
	// The directory just written is protected from its own sweep.
	_ = cacheSweep(filepath.Dir(dir), cacheKeepDirs, filepath.Base(dir))
	return meta, nil
}

// cacheWriteTemp writes one file's bytes to its temporary and returns the
// temporary's path, ready to be renamed into place. Nothing a reader can see
// changes here: this is the slow half of a save, and it is deliberately
// separated from the commit so that the commit is two renames and nothing
// else.
//
// The caller removes the temporary. Once it has been renamed into place that
// removal finds nothing, which is the intended outcome and not an error.
func cacheWriteTemp(path string, data []byte) (string, error) {
	suffix, err := cacheRandHex()
	if err != nil {
		return "", err
	}
	tmp := path + cacheTmpInfix + suffix

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("catalog: creating %s: %w", filepath.Base(tmp), err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return tmp, fmt.Errorf("catalog: writing %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return tmp, fmt.Errorf("catalog: flushing %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Close(); err != nil {
		return tmp, fmt.Errorf("catalog: closing %s: %w", filepath.Base(tmp), err)
	}
	return tmp, nil
}

// cacheCommitPair puts the index and then the marker into place, and is the
// only place a save changes anything a reader can see.
//
// Why it is one function rather than two independent atomic writes, which is
// what this was until the benchmark work's racing test caught it: os.Rename
// replaces an existing file on Windows but fails while another process holds
// the destination open, and the destination here is exactly what a second
// window browsing the catalogue has open. meta.json is renamed second because
// it is the commit marker (§7), so the rename that can fail is the one *after*
// the index has already been replaced — and a marker left describing an index
// that is no longer there is a cache the operator has lost. The old answer was
// to delete that marker, which made every open window read a perfectly good
// directory as "not built" until some later build happened to win.
//
// The commit is therefore in two phases, and the second one is the whole point.
//
// Phase 1 gets the index in. Failing here costs nothing: the marker and the
// index it describes are both untouched, so the previous cache still loads.
//
// Phase 2 starts with the directory inconsistent — a new index under a stale
// marker — and ends it, in whichever of the two directions works. Forward is
// renaming the marker in, which completes the save. Backward is renaming the
// previous index back, which abandons it; that index is hard-linked aside
// before phase 1 precisely so there is something to go back to, a link being
// free and, unlike a rename, never in contention with a reader because linking
// reads the source rather than moving it. Both directions are tried, in turn,
// until one lands: either is a directory the operator can use, and *neither*
// rename is more likely to succeed than the other, since a reader holds both
// files. Trying only one of them is how the earlier version of this could
// still leave the directory in the state it was meant to prevent.
//
// Only if neither direction lands inside the budget is the stale marker
// removed, which leaves the directory reading as "not built" — true, and
// overwritten by the next build — rather than as "corrupt", which would be a
// lie about what happened.
//
// Giving up returns ErrCacheNotReplaced, which is not a build failure: the
// catalogue in memory is good and the cache is written next run.
func cacheCommitPair(binTmp, binPath, metaTmp, metaPath string) error {
	var last error
	rollback := ""
	defer func() {
		if rollback != "" {
			_ = os.Remove(rollback)
		}
	}()

	// Phase 1.
	placed := false
	for attempt := 0; attempt < cacheCommitAttempts && !placed; attempt++ {
		if attempt > 0 {
			time.Sleep(cacheCommitWait)
		}
		rollback = cacheLinkAside(binPath, metaPath)
		if err := os.Rename(binTmp, binPath); err != nil {
			last = err
			if rollback != "" {
				_ = os.Remove(rollback)
				rollback = ""
			}
			continue
		}
		placed = true
	}
	if !placed {
		return fmt.Errorf("%w: %s: %v", ErrCacheNotReplaced, filepath.Base(binPath), last)
	}

	// Phase 2.
	for attempt := 0; attempt < cacheCommitAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(cacheCommitWait)
		}
		err := os.Rename(metaTmp, metaPath)
		if err == nil {
			return nil
		}
		last = err
		if rollback == "" {
			// A first build has no previous index to go back to, so forward
			// is the only direction there is.
			continue
		}
		if err := os.Rename(rollback, binPath); err == nil {
			rollback = "" // it is catalog.bin again
			return fmt.Errorf("%w: %s: %v", ErrCacheNotReplaced, filepath.Base(metaPath), last)
		}
	}

	_ = os.Remove(metaPath)
	return fmt.Errorf("%w: %s: %v", ErrCacheNotReplaced, filepath.Base(metaPath), last)
}

// cacheLinkAside hard-links the committed index to a temporary name so that
// cacheCommitPair can put it back.
//
// Only a directory that already carries a commit marker has a cache to lose,
// so one without is skipped: there is nothing there worth restoring, and the
// first build of a target would otherwise link a file that does not exist.
//
// An empty return means "no rollback available", not an error. A filesystem
// with no hard links costs the rollback, not the build. The name matches the
// sweep's "*.tmp-*" pattern, so a link left behind by a process that died
// mid-commit is collected like every other leftover temporary — and, while it
// is there, it is also what tells a concurrent reader that this directory is
// being written and must not be deleted.
func cacheLinkAside(binPath, metaPath string) string {
	if _, err := os.Stat(metaPath); err != nil {
		return ""
	}
	suffix, err := cacheRandHex()
	if err != nil {
		return ""
	}
	link := binPath + cacheTmpInfix + suffix
	if err := os.Link(binPath, link); err != nil {
		return ""
	}
	return link
}

// cacheSyncDir fsyncs a directory so the renames above survive a power cut.
// Best-effort: Windows has no directory handle to sync, and a failure here
// costs a rebuild rather than correctness.
func cacheSyncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func cacheRandHex() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("catalog: naming a temporary file: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// cacheSortedUnique returns the entries in package-name ascending byte order
// with duplicate names collapsed to the last occurrence.
//
// The order is an invariant of the format, not a nicety: Get is a binary
// search over it, Search's contract order falls out of one forward scan over
// it, and paging is stable across calls because of it.
func cacheSortedUnique(in []Entry) []Entry {
	if len(in) == 0 {
		return nil
	}
	// Sort and deduplicate on the name that will be *stored*, not the one that
	// arrived. cacheBuildImage clips every name to the uint16 length a record
	// can address, and clipping after this function ran left the two
	// disagreeing about the very invariant the format is sorted for: two names
	// over 65,535 bytes that share a prefix become one stored name, so the
	// array is no longer unique, and a name cut short by an invalid byte can
	// end up sorting before the one it followed, so the array is no longer
	// ascending. Either makes Lookup's binary search miss a row that is
	// genuinely there.
	//
	// Real package names are tens of bytes and this costs a length check
	// each. An index that says otherwise is exactly the input a writer that
	// clips at all is there to survive.
	names := make([]string, len(in))
	idx := make([]int32, 0, len(in))
	for i := range in {
		names[i] = cacheClip(in[i].Name, cacheMaxShortString)
		idx = append(idx, int32(i))
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return names[idx[a]] < names[idx[b]]
	})
	out := make([]Entry, 0, len(idx))
	for _, i := range idx {
		e := in[i]
		e.Name = names[i]
		if e.Name == "" {
			continue
		}
		// SliceStable keeps input order within a run of equal names, so the
		// last one of the run wins, which is the rule Entry.Version states.
		if n := len(out); n > 0 && out[n-1].Name == e.Name {
			out[n-1] = e
			continue
		}
		out = append(out, e)
	}
	return out
}

// cacheArena accumulates the text blob and the interned string table while the
// records are being written.
type cacheArena struct {
	blob []byte
	// dedup covers the fields that genuinely repeat — versions, the
	// "Transitional package" class of summary, homepages, icon refs. Names and
	// haystacks are near-unique by construction and are appended without
	// consulting it, which keeps the map at a few tens of thousands of entries
	// instead of several hundred thousand during a build.
	dedup map[string]uint32

	interned  []string
	internIdx map[string]uint16
}

func newCacheArena(n int) *cacheArena {
	a := &cacheArena{
		blob:      make([]byte, 1, 1+n*200), // byte 0 is the reserved NUL
		dedup:     make(map[string]uint32, n),
		interned:  []string{""},
		internIdx: map[string]uint16{"": 0},
	}
	return a
}

// add appends a string and returns its (offset, length) in the arena.
// Offsets are relative to the start of the arena, not the file.
func (a *cacheArena) add(s string, dedup bool) (uint32, uint32) {
	if s == "" {
		return 0, 0
	}
	if dedup {
		if off, ok := a.dedup[s]; ok {
			return off, cacheLen32(len(s))
		}
	}
	off := cacheLen32(len(a.blob))
	a.blob = append(a.blob, s...)
	if dedup {
		a.dedup[s] = off
	}
	return off, cacheLen32(len(s))
}

// intern returns the uint16 id of a repeating string, adding it if new.
func (a *cacheArena) intern(s, field string) (uint16, error) {
	if id, ok := a.internIdx[s]; ok {
		return id, nil
	}
	// n is read once and reused as the new id, which is what makes the
	// narrowing below provably safe: the guard rejects anything that would not
	// fit in the uint16 an id is.
	n := len(a.interned)
	if n >= cacheMaxInterned {
		// Truncating would silently mislabel tens of thousands of rows. A real
		// archive uses a few hundred, so hitting this means the input is not
		// what the format assumed — a thing to find out about.
		return 0, fmt.Errorf("catalog: more than %d distinct values for %s; the cache format cannot address them", cacheMaxInterned, field)
	}
	id := uint16(n)
	a.interned = append(a.interned, s)
	a.internIdx[s] = id
	// Interned strings live in the arena like everything else, deduplicated
	// against it: a section name that is also a word in a summary costs one
	// copy.
	a.add(s, true)
	return id, nil
}

// offsetOf returns an interned string's arena (offset, length).
func (a *cacheArena) offsetOf(s string) (uint32, uint32) {
	if s == "" {
		return 0, 0
	}
	if off, ok := a.dedup[s]; ok {
		return off, cacheLen32(len(s))
	}
	return a.add(s, true)
}

// cacheCategoryCount counts one tier's categories while the records are built.
type cacheCategoryCount struct {
	tier CategoryTier
	name string
	n    int
}

// cacheBuildImage renders the whole file into one buffer.
//
// Assembling in memory rather than streaming keeps the offset arithmetic in
// one place and makes the write a single syscall; the ~19 MB it costs is
// transient, at the end of a build that has just parsed 60 MB of indexes.
func cacheBuildImage(entries []Entry) ([]byte, error) {
	n := len(entries)
	a := newCacheArena(n)
	records := make([]byte, n*cacheRecordSize)
	catids := make([]byte, 0, n/8)

	appCounts := make(map[string]int)
	sectionCounts := make(map[string]int)
	var descriptionBuffer bytes.Buffer
	var descriptionWriter *zlib.Writer
	var descriptionsPacked int

	for i, e := range entries {
		r := records[i*cacheRecordSize : (i+1)*cacheRecordSize]

		// Every uint16-addressed field is clipped to what its length can
		// describe, on a rune boundary so a clipped string stays valid UTF-8.
		// Real values are two orders of magnitude below this.
		name := cacheClip(e.Name, cacheMaxShortString)
		version := cacheClip(e.Version, cacheMaxShortString)
		appName := cacheClip(e.AppName, cacheMaxShortString)
		summary := cacheClip(e.Summary, cacheMaxShortString)
		homepage := cacheClip(e.Homepage, cacheMaxShortString)
		iconRef := cacheClip(e.IconRef, cacheMaxShortString)
		packedDescription := e.descriptionData
		descriptionTruncated := e.DescriptionTruncated
		if e.Description != "" {
			description := cacheClip(e.Description, packagesMaxDescriptionBytes)
			descriptionTruncated = descriptionTruncated || len(description) < len(e.Description)
			descriptionBuffer.Reset()
			if descriptionWriter == nil {
				descriptionWriter, _ = zlib.NewWriterLevel(&descriptionBuffer, zlib.BestSpeed)
			} else {
				descriptionWriter.Reset(&descriptionBuffer)
			}
			_, _ = io.WriteString(descriptionWriter, description)
			_ = descriptionWriter.Close()
			packedDescription = descriptionBuffer.String()
		}
		if len(packedDescription) > cacheMaxDescriptionPacked || descriptionsPacked+len(packedDescription) > cacheMaxDescriptionsPacked {
			packedDescription = ""
			descriptionTruncated = true
		}
		descriptionsPacked += len(packedDescription)

		// The interned fields are clipped here as well, and for a second
		// reason on top of the length. Each of them is used three times in
		// this loop — interned into the table the record's id points at,
		// counted into appCounts/sectionCounts, and looked up again when the
		// category summary is keyed — and those three uses must be the same
		// string. Clipping deeper, inside intern, would satisfy the length and
		// leave the counting maps holding the unclipped key, so the summary
		// would name a string no record points at. So the clipped value is
		// bound once here and is the only one the rest of the iteration sees.
		section := cacheClip(e.Section, cacheMaxShortString)
		priority := cacheClip(e.Priority, cacheMaxShortString)
		suite := cacheClip(e.Suite, cacheMaxShortString)
		component := cacheClip(e.Component, cacheMaxShortString)
		arch := cacheClip(e.Arch, cacheMaxShortString)

		nameOff, nameLen := a.add(name, false)
		verOff, verLen := a.add(version, true)
		appOff, appLen := a.add(appName, true)
		sumOff, sumLen := a.add(summary, true)
		homeOff, homeLen := a.add(homepage, true)
		iconOff, iconLen := a.add(iconRef, true)
		descriptionOff, descriptionLen := a.add(packedDescription, false)

		// The haystack is built from the clipped values so that what is
		// searched and what is displayed are the same text.
		hayOff, hayLen := a.add(strings.ToLower(name+"\x00"+appName+"\x00"+summary), false)

		sectionID, err := a.intern(section, "Section")
		if err != nil {
			return nil, err
		}
		priorityID, err := a.intern(priority, "Priority")
		if err != nil {
			return nil, err
		}
		suiteID, err := a.intern(suite, "Suite")
		if err != nil {
			return nil, err
		}
		componentID, err := a.intern(component, "Component")
		if err != nil {
			return nil, err
		}
		archID, err := a.intern(arch, "Architecture")
		if err != nil {
			return nil, err
		}

		var catOff, catCount uint32
		if len(e.Categories) > 0 {
			catOff = cacheLen32(len(catids) / 2)
			for _, raw := range e.Categories {
				// Clipped before it is either interned or counted, so the id
				// in catids and the key in appCounts name the same bytes.
				category := cacheClip(raw, cacheMaxShortString)
				if category == "" {
					continue
				}
				id, err := a.intern(category, "DEP-11 category")
				if err != nil {
					return nil, err
				}
				catids = binary.LittleEndian.AppendUint16(catids, id)
				catCount++
				appCounts[category]++
			}
			if catCount == 0 {
				catOff = 0
			}
		}
		if section != "" {
			sectionCounts[section]++
		}

		var flags uint16
		if e.IsApp {
			flags |= 1
		}
		if descriptionTruncated {
			flags |= 2
		}

		binary.LittleEndian.PutUint32(r[0:4], nameOff)
		binary.LittleEndian.PutUint16(r[4:6], cacheU16(nameLen))
		binary.LittleEndian.PutUint16(r[6:8], flags)
		binary.LittleEndian.PutUint32(r[8:12], verOff)
		binary.LittleEndian.PutUint16(r[12:14], cacheU16(verLen))
		binary.LittleEndian.PutUint16(r[14:16], cacheU16(appLen))
		binary.LittleEndian.PutUint32(r[16:20], appOff)
		binary.LittleEndian.PutUint32(r[20:24], sumOff)
		binary.LittleEndian.PutUint16(r[24:26], cacheU16(sumLen))
		binary.LittleEndian.PutUint16(r[26:28], cacheU16(homeLen))
		binary.LittleEndian.PutUint32(r[28:32], homeOff)
		binary.LittleEndian.PutUint32(r[32:36], hayOff)
		binary.LittleEndian.PutUint32(r[36:40], hayLen)
		binary.LittleEndian.PutUint16(r[40:42], sectionID)
		binary.LittleEndian.PutUint16(r[42:44], priorityID)
		binary.LittleEndian.PutUint16(r[44:46], suiteID)
		binary.LittleEndian.PutUint16(r[46:48], componentID)
		binary.LittleEndian.PutUint16(r[48:50], archID)
		binary.LittleEndian.PutUint16(r[50:52], uint16(catCount))
		binary.LittleEndian.PutUint32(r[52:56], catOff)
		binary.LittleEndian.PutUint32(r[56:60], iconOff)
		binary.LittleEndian.PutUint16(r[60:62], cacheU16(iconLen))
		// r[62:64] stays zero: reserved, and a reader rejects a file that
		// sets it.
		binary.LittleEndian.PutUint32(r[64:68], cacheU32(e.InstalledSizeKiB))
		binary.LittleEndian.PutUint32(r[68:72], cacheU32(cacheKiB(e.DownloadSizeBytes)))
		binary.LittleEndian.PutUint32(r[72:76], descriptionOff)
		binary.LittleEndian.PutUint32(r[76:80], descriptionLen)
	}

	// The category summary is the precomputed answer to Categories(), stored
	// in the order that call must return: applications first, then sections,
	// each ascending by name. The reader sorts nothing.
	summary := make([]cacheCategoryCount, 0, len(appCounts)+len(sectionCounts))
	for name, count := range appCounts {
		summary = append(summary, cacheCategoryCount{TierApplication, name, count})
	}
	for name, count := range sectionCounts {
		summary = append(summary, cacheCategoryCount{TierSection, name, count})
	}
	sort.Slice(summary, func(i, j int) bool {
		if (summary[i].tier == TierApplication) != (summary[j].tier == TierApplication) {
			return summary[i].tier == TierApplication
		}
		return summary[i].name < summary[j].name
	})

	internedTable := make([]byte, 0, 4+len(a.interned)*6)
	internedTable = binary.LittleEndian.AppendUint32(internedTable, cacheLen32(len(a.interned)))
	for _, s := range a.interned {
		off, ln := a.offsetOf(s)
		internedTable = binary.LittleEndian.AppendUint32(internedTable, off)
		internedTable = binary.LittleEndian.AppendUint16(internedTable, cacheU16(ln))
	}

	catSummary := make([]byte, 0, 4+len(summary)*8)
	catSummary = binary.LittleEndian.AppendUint32(catSummary, cacheLen32(len(summary)))
	for _, c := range summary {
		var tier byte = 1
		if c.tier == TierApplication {
			tier = 0
		}
		// Every counted name was interned in the same iteration that counted
		// it, from the same clipped string, so this cannot miss. It is checked
		// anyway because a miss would not fail: the zero value is id 0, the
		// reserved empty string, and the summary would quietly describe a
		// category the records do not point at inside a file whose CRC says it
		// is intact. That is the disagreement clipping at the call site exists
		// to prevent, and this is the assertion that keeps it prevented.
		id, ok := a.internIdx[c.name]
		if !ok {
			return nil, fmt.Errorf("catalog: internal: category %q was counted but never interned", c.name)
		}
		catSummary = append(catSummary, tier, 0)
		catSummary = binary.LittleEndian.AppendUint16(catSummary, id)
		catSummary = binary.LittleEndian.AppendUint32(catSummary, cacheLen32(c.n))
	}

	// The arena stops growing here, so its length is final and every offset
	// already written into a record is valid against it.
	if uint64(len(a.blob)) > math.MaxUint32 {
		return nil, fmt.Errorf("catalog: the catalogue's text is %d bytes; the cache format addresses at most %d", len(a.blob), uint64(math.MaxUint32))
	}

	recordsOff := uint64(cacheHeaderSize)
	recordsLen := uint64(len(records))
	catidsOff := recordsOff + recordsLen
	catidsLen := uint64(len(catids))
	internedOff := catidsOff + catidsLen
	catSummaryOff := internedOff + uint64(len(internedTable))
	blobOff := catSummaryOff + uint64(len(catSummary))
	payloadLen := blobOff + uint64(len(a.blob))

	out := make([]byte, 0, payloadLen+cacheTrailerSize)
	header := make([]byte, cacheHeaderSize)
	copy(header[0:8], cacheMagic)
	binary.LittleEndian.PutUint32(header[8:12], CacheFormatVersion)
	binary.LittleEndian.PutUint32(header[12:16], cacheHeaderSize)
	binary.LittleEndian.PutUint32(header[16:20], cacheLen32(n))
	binary.LittleEndian.PutUint32(header[20:24], 0) // flags: reserved, must be 0
	binary.LittleEndian.PutUint64(header[24:32], recordsOff)
	binary.LittleEndian.PutUint64(header[32:40], recordsLen)
	binary.LittleEndian.PutUint64(header[40:48], catidsOff)
	binary.LittleEndian.PutUint64(header[48:56], catidsLen)
	binary.LittleEndian.PutUint64(header[56:64], internedOff)

	out = append(out, header...)
	out = append(out, records...)
	out = append(out, catids...)
	out = append(out, internedTable...)
	out = append(out, catSummary...)
	out = append(out, a.blob...)
	if uint64(len(out)) != payloadLen {
		// Unreachable unless the arithmetic above and the appends below it
		// disagree, which is exactly the bug worth failing loudly on.
		return nil, fmt.Errorf("catalog: internal: wrote %d payload bytes, computed %d", len(out), payloadLen)
	}

	trailer := make([]byte, cacheTrailerSize)
	binary.LittleEndian.PutUint64(trailer[0:8], payloadLen)
	binary.LittleEndian.PutUint32(trailer[8:12], crc32.Checksum(out, cacheCastagnoli))
	binary.LittleEndian.PutUint32(trailer[12:16], 0) // reserved
	copy(trailer[16:24], cacheEndMagic)
	out = append(out, trailer...)
	return out, nil
}

// cacheClip trims s to at most limit bytes and to the longest prefix of that
// which is valid UTF-8, so the result is still valid UTF-8. It is the writer's
// one defence against a field a malformed index made longer than the uint16
// length that has to address it.
//
// The scan is forward and single-pass. It used to shrink the candidate by one
// byte and revalidate the whole of it, which is the obvious way to write this
// and is quadratic in the limit: an invalid byte early in the window makes it
// revalidate ~65 KiB some 33,000 times, about two billion byte comparisons.
// The cost is bounded per field rather than unbounded — the window is always
// 65,535 bytes — and that is exactly what makes it easy to underrate. It was
// reported at 174 ms for one such field; on the machine this comment was
// written on, where utf8.ValidString runs at tens of GB/s, the same field
// costs about 30 ms. Either way an index carrying one per entry turns a
// two-second parse into somewhere between half an hour and several hours, so
// it is an availability problem and not a curiosity.
//
// The answer is the same string either way: b[:i] is valid because whole runes
// were decoded up to i, and no longer prefix can be, because the sequence
// starting at i is invalid however many bytes follow it.
func cacheClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := s[:limit]
	if utf8.ValidString(b) {
		return b
	}
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRuneInString(b[i:])
		// (RuneError, 1) is the invalid encoding; a genuine U+FFFD decodes as
		// (RuneError, 3) and must not be mistaken for one.
		if r == utf8.RuneError && size <= 1 {
			return b[:i]
		}
		i += size
	}
	return b
}

// cacheU32 clamps a size into the uint32 a record field holds. Negative is
// nonsense from a malformed index and becomes zero.
func cacheU32(v int64) uint32 {
	switch {
	case v <= 0:
		return 0
	case v > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(v)
	}
}

// cacheLen32 narrows a Go length or count into the uint32 the format's offset,
// length and count fields hold.
//
// Every caller is bounded far below the ceiling by something else — the arena
// is rejected above math.MaxUint32 before this image is emitted, the interned
// table cannot exceed cacheMaxInterned, and a record count that overflowed
// would need 300 GB of records — so this is the invariant restated at the one
// place the width actually changes, not a second policy. It clamps rather than
// wraps because a wrapped length or offset points a reader at bytes that are
// not the value, whereas a clamped one is caught by the reader's own bounds
// check.
func cacheLen32(n int) uint32 { return cacheU32(int64(n)) }

// cacheU16 narrows a string length into the uint16 a record field or the
// interned table holds.
//
// Every caller is safe by construction: record fields and interned values
// alike — Section, Priority, Suite, Component, Architecture and DEP-11
// category names, any of which a malformed index can carry at up to
// packagesScanBufferMax bytes — are cacheClip(_, cacheMaxShortString) at the
// call site before they reach the arena, and cacheMaxShortString is
// math.MaxUint16.
//
// The clamp stays as that invariant restated at the one place the width
// actually changes, because the cost of getting it wrong is not a truncation
// but a different string: 65536 bytes wraps to 0, which is the empty one,
// inside a file whose CRC says it is intact. Clipping at the call site is what
// makes that unreachable; clamping here is what keeps it merely a truncation
// if a new call site ever forgets. See cacheClip.
func cacheU16(n uint32) uint16 {
	if n > cacheMaxShortString {
		return cacheMaxShortString
	}
	return uint16(n)
}

// cacheKiB converts apt's Size: in bytes to the KiB a record stores, rounding
// up. The lost precision is at most 1023 bytes on a number displayed as "about
// 340 MB", and the UI documents that rounding rather than compensating for it.
func cacheKiB(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 1023) / 1024
}

// ---------------------------------------------------------------------------
// Housekeeping
// ---------------------------------------------------------------------------

// SweepCache removes what a cache root has accumulated: temporary files a
// killed build left behind, directories with no usable commit marker, and all
// but the newest keep target directories.
//
// Best-effort by contract (§8): a removal that fails is skipped, not reported,
// because housekeeping must never fail a build. The error return covers only
// "the root could not be read at all". A root that does not exist is not an
// error — there is simply nothing to sweep.
func SweepCache(root string, keep int) error { return cacheSweep(root, keep, "") }

func cacheSweep(root string, keep int, protect string) error {
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("catalog: reading the cache root: %w", err)
	}
	now := time.Now()

	type kept struct {
		dir     string
		builtAt time.Time
	}
	var valid []kept

	for _, ent := range ents {
		path := filepath.Join(root, ent.Name())
		if !ent.IsDir() {
			cacheSweepTemp(path, ent, now)
			continue
		}
		// Temporaries live inside target directories, so they are swept there
		// too — "anywhere under the root".
		if sub, err := os.ReadDir(path); err == nil {
			for _, s := range sub {
				if !s.IsDir() {
					cacheSweepTemp(filepath.Join(path, s.Name()), s, now)
				}
			}
		}

		meta, err := ReadCacheMeta(path)
		switch {
		case ent.Name() == protect:
			// The directory this sweep was called from. It still counts
			// towards the keep budget — it is one of the caches on disk — but
			// nothing may remove it, and being the newest it survives anyway.
			valid = append(valid, kept{dir: path, builtAt: meta.BuiltAt})
		case errors.Is(err, ErrCacheVersion):
			// Written by another build of this application. It will never be
			// read and is never migrated, so it goes now.
			cacheRemoveDir(path)
		case err != nil:
			// No commit marker, or an unreadable one. This is also what a
			// build running *right now* in another process looks like between
			// its two renames, so an age guard is applied that §8 does not
			// ask for: sweeping a directory out from under a concurrent build
			// would turn its last thirty seconds into a failed save, and
			// waiting an hour to reclaim 19 MB costs nothing.
			if info, statErr := os.Stat(path); statErr == nil && now.Sub(info.ModTime()) > cacheTmpMaxAge {
				cacheRemoveDir(path)
			}
		default:
			valid = append(valid, kept{dir: path, builtAt: meta.BuiltAt})
		}
	}

	if keep > 0 && len(valid) > keep {
		sort.Slice(valid, func(i, j int) bool { return valid[i].builtAt.After(valid[j].builtAt) })
		for _, v := range valid[keep:] {
			if filepath.Base(v.dir) == protect {
				continue
			}
			cacheRemoveDir(v.dir)
		}
	}
	return nil
}

// cacheSweepTemp removes one leftover temporary if it is old enough to be
// certain no build is still writing it.
func cacheSweepTemp(path string, ent os.DirEntry, now time.Time) {
	if !strings.Contains(ent.Name(), cacheTmpInfix) {
		return
	}
	info, err := ent.Info()
	if err != nil || now.Sub(info.ModTime()) <= cacheTmpMaxAge {
		return
	}
	_ = os.Remove(path)
}

// cacheRemoveDir deletes a cache directory. Failure is ignored on purpose:
// deleting CacheRoot() by hand is always a valid recovery step, so the worst
// case of a failed removal is that the operator does what the settings screen
// already tells them to.
func cacheRemoveDir(dir string) { _ = os.RemoveAll(dir) }

// ---------------------------------------------------------------------------
// The index digest
// ---------------------------------------------------------------------------

// CacheIndexDigest derives Target.IndexDigest from the archives' own recorded
// hashes (§6.2), which is what makes freshness cost a few kilobytes rather
// than the tens of megabytes it describes.
//
// releases maps "<archive root> dists/<suite>/Release" — an IndexRef's URI and
// ReleasePath joined the way ID() joins them — to that file's bytes. The
// network lives in the caller: this package fetches nothing and verifies no
// signature (§1: the cache is not a trust boundary, and this file ships no
// keyrings).
//
// ok is false when no consulted Release carried a SHA256: section at all, in
// which case the caller hashes the fetched index bytes instead and records
// CacheDigestContent as the source.
func CacheIndexDigest(t Target, releases map[string][]byte) (digest string, ok bool) {
	parsed := make(map[string]map[string]string, len(releases))
	anyHashes := false
	for key, body := range releases {
		h := CacheReleaseHashes(body)
		if len(h) > 0 {
			anyHashes = true
		}
		parsed[key] = h
	}
	if !anyHashes {
		return "", false
	}
	hashes := make(map[string]string, 32)
	for _, r := range t.IndexRefs() {
		key := strings.TrimRight(r.URI, "/") + " " + r.ReleasePath()
		if h, found := parsed[key][cacheSuiteRelPath(r)]; found {
			hashes[r.ID()] = h
		}
	}
	return CacheIndexDigestFromHashes(t, hashes), true
}

// CacheIndexDigestFromHashes builds the digest from one hash per index file.
//
// hashes is keyed by IndexRef.ID(). A ref the archive does not list — a
// component that publishes no DEP-11, which is normal — contributes "-", so
// that a component gaining or losing DEP-11 still changes the digest.
func CacheIndexDigestFromHashes(t Target, hashes map[string]string) string {
	refs := t.IndexRefs()
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		h := hashes[r.ID()]
		if h == "" {
			h = "-"
		}
		lines = append(lines, r.ID()+" "+h+"\n")
	}
	sort.Strings(lines)
	sum := sha256.New()
	for _, l := range lines {
		sum.Write([]byte(l))
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

// CacheReleaseHashes parses a Release file's SHA256: section into a map from
// the path it names — relative to dists/<suite>/, e.g.
// "main/binary-amd64/Packages.gz" — to the lowercase hex hash.
//
// Deliberately a lenient scanner rather than a parser: a line it cannot read
// is skipped, because the only consequence of missing one is a "-" in the
// digest and therefore a rebuild the operator did not strictly need.
func CacheReleaseHashes(release []byte) map[string]string {
	out := make(map[string]string)
	inSection := false
	for _, raw := range strings.Split(string(release), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			// A new field. SHA256: opens the section; anything else closes it.
			inSection = strings.EqualFold(strings.TrimSuffix(strings.Fields(line)[0], ":"), "SHA256")
			continue
		}
		if !inSection {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		out[f[2]] = strings.ToLower(f[0])
	}
	return out
}

// cacheSuiteRelPath is a ref's path as a Release file names it: relative to
// dists/<suite>/ rather than to the archive root.
func cacheSuiteRelPath(r IndexRef) string {
	return strings.TrimPrefix(r.Path(), "dists/"+r.Suite+"/")
}
