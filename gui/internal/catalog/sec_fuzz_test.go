package catalog_test

// sec_fuzz_test.go — the security review's additions to the cache parser's
// fuzzing. It does not replace FuzzLoad in fuzz_test.go; it
// covers the shapes that target cannot reach often enough to matter.
//
// # Why a second set of targets
//
// FuzzLoad feeds arbitrary bytes to CacheFromBytes and, cleverly, tries every
// input twice — once raw and once with the trailer's length and CRC recomputed
// — because a mutated file fails its checksum with overwhelming probability
// and a fuzzer that stops at the CRC never reaches a single bounds check. That
// is the right design and it found real bugs.
//
// What it still leaves thin is everything *between* the CRC and the record
// bounds. A repaired random buffer almost never has the eight-byte magic, the
// right format version, header_size == 64, zero flags, the trailer magic, and
// `records_len == entry_count × 80` all at once — and every structural check in
// the loader sits behind that conjunction. Measured against the corpus this
// pass produced, the overwhelming majority of executions are refused by the
// magic or by records_len long before the section-extent arithmetic runs.
//
// So these targets are structured rather than raw:
//
//   - FuzzSecCacheImage builds a file that is *always* well-formed down to the
//     trailer and lets the fuzzer choose only the five section words. Every
//     execution reaches the section-extent validation, the interned-table
//     ceiling, the counted-section arithmetic and the arena bound.
//   - FuzzSecCacheRecords takes a real cache and lets the fuzzer rewrite one
//     80-byte record. Every execution reaches the per-record reserved-bit
//     check and, on acceptance, the point-of-use validation that Entry,
//     Haystack and the category ids are supposed to perform.
//   - FuzzSecCacheMeta covers the other parser in the load path. meta.json is
//     the commit marker: a directory whose meta is unreadable has no usable
//     cache, and that decision is made by encoding/json over bytes on disk.
//
// The property under test is the one docs/dev/cache-format.md §5.1 states: no
// code path may panic on cache content, an out-of-range reference yields the
// empty string and a counter, and every structural violation is
// ErrCacheCorrupt or ErrCacheVersion — never a silent wrong answer.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/gui/internal/catalog"
)

const (
	secHeaderSize  = 64
	secRecordSize  = 80
	secTrailerSize = 24
	secMagic       = "DFGCAT01"
	secEndMagic    = "DFGCEND\n"
	// secMaxBody caps the fuzzer-supplied section bytes. The loader's cost is
	// linear in the file, and a fuzzer left to grow its input spends its whole
	// budget CRC-ing megabytes instead of trying new shapes.
	secMaxBody = 1 << 14
)

var secCRCTable = crc32.MakeTable(crc32.Castagnoli)

// secBuildImage assembles a cache file that is valid as far as the trailer and
// carries exactly the section words it is given.
//
// Everything the loader checks before it looks at an offset — magic, version,
// header size, flags, trailer magic, trailer reserved word, payload length,
// CRC — is correct by construction. That is the whole point: those eight
// checks are what a raw fuzzer spends its budget failing, and they are not
// where the interesting bugs are.
func secBuildImage(entryCount uint32, recordsOff, recordsLen, catidsOff, catidsLen, internedOff uint64, body []byte) []byte {
	if len(body) > secMaxBody {
		body = body[:secMaxBody]
	}
	buf := make([]byte, secHeaderSize+len(body)+secTrailerSize)
	copy(buf[0:8], secMagic)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(catalog.CacheFormatVersion))
	binary.LittleEndian.PutUint32(buf[12:16], secHeaderSize)
	binary.LittleEndian.PutUint32(buf[16:20], entryCount)
	binary.LittleEndian.PutUint32(buf[20:24], 0) // flags: must be 0 in version 2
	binary.LittleEndian.PutUint64(buf[24:32], recordsOff)
	binary.LittleEndian.PutUint64(buf[32:40], recordsLen)
	binary.LittleEndian.PutUint64(buf[40:48], catidsOff)
	binary.LittleEndian.PutUint64(buf[48:56], catidsLen)
	binary.LittleEndian.PutUint64(buf[56:64], internedOff)
	copy(buf[secHeaderSize:], body)
	return secSealImage(buf)
}

// secSealImage writes the trailer for the payload already in buf.
func secSealImage(buf []byte) []byte {
	payload := len(buf) - secTrailerSize
	tr := buf[payload:]
	binary.LittleEndian.PutUint64(tr[0:8], uint64(payload))
	binary.LittleEndian.PutUint32(tr[8:12], crc32.Checksum(buf[:payload], secCRCTable))
	binary.LittleEndian.PutUint32(tr[12:16], 0)
	copy(tr[16:24], secEndMagic)
	return buf
}

// secExerciseCache calls everything a *CacheFile exposes over a file the loader
// accepted.
//
// Written out rather than reusing fuzz_test.go's cacheFuzzLoad on purpose: the
// two targets assert different things about the same object, and a shared
// helper would mean a change made for one silently weakens the other. Any panic
// fails the target by definition; the assertions here catch the quieter
// failure, where a damaged file is accepted and then hands back something that
// is not a valid Entry.
func secExerciseCache(t *testing.T, cf *catalog.CacheFile) {
	t.Helper()

	n := cf.Len()
	if n < 0 {
		t.Fatalf("Len = %d", n)
	}
	// A record table cannot be longer than the file, so n is already bounded
	// by the input; the cap only keeps a pathological seed from slowing the
	// fuzzer down.
	if n > 2048 {
		n = 2048
	}
	for i := -1; i <= n; i++ {
		e, ok := cf.Entry(i)
		if ok != (i >= 0 && i < cf.Len()) {
			t.Fatalf("Entry(%d) ok = %v with Len = %d", i, ok, cf.Len())
		}
		if !ok {
			if e.Name != "" || e.Categories != nil {
				t.Fatalf("Entry(%d) failed but returned %#v", i, e)
			}
			continue
		}
		// §4.5: every short string is addressed by a uint16 length, so a
		// longer one means a length was read from the wrong place.
		if len(e.Name) > 65535 || len(e.Summary) > 65535 || len(e.Version) > 65535 ||
			len(e.AppName) > 65535 || len(e.Homepage) > 65535 || len(e.IconRef) > 65535 {
			t.Fatalf("Entry(%d) returned a string longer than a uint16 length can address: %#v", i, e)
		}
		if e.InstalledSizeKiB < 0 || e.DownloadSizeBytes < 0 {
			t.Fatalf("Entry(%d) returned a negative size: %#v", i, e)
		}
		// A category that is the empty string is what an out-of-range interned
		// id yields, and Entry must drop it rather than surface it as a real
		// category — a blank chip in the picker is the visible form of the
		// silent-mislabelling failure this format guards against.
		for _, c := range e.Categories {
			if c == "" {
				t.Fatalf("Entry(%d) returned an empty category name: %#v", i, e)
			}
		}
		// The name Entry materialises and the name the byte-level accessor
		// returns are the same bytes read two different ways. They must agree,
		// or one of them is reading past its bound without saying so.
		if got := string(cf.NameBytes(i)); got != e.Name {
			t.Fatalf("Entry(%d).Name = %q but NameBytes = %q", i, e.Name, got)
		}
		_ = cf.Haystack(i)
		_ = cf.IsApp(i)
		_ = cf.HasCategoryID(i, cf.SectionID(i))

		if e.Name != "" {
			// Lookup is a binary search, which only answers correctly over a
			// sorted table; a hostile file need not be sorted. This asserts
			// that it terminates and stays in range, not that it finds the row.
			if j, found := cf.Lookup(e.Name); found && (j < 0 || j >= cf.Len()) {
				t.Fatalf("Lookup(%q) returned out-of-range index %d", e.Name, j)
			}
			if _, found := cf.Get(e.Name); found {
				// Get is Lookup plus Entry; reaching here without a panic is
				// the assertion.
				_ = found
			}
		}
	}

	for _, c := range cf.Categories() {
		if c.Tier != catalog.TierApplication && c.Tier != catalog.TierSection {
			t.Fatalf("category %q has tier %q", c.ID, c.Tier)
		}
		if c.Count < 0 {
			t.Fatalf("category %q has count %d", c.ID, c.Count)
		}
	}

	// The interned table's edges, in both directions.
	for _, id := range []uint16{0, 1, 65534, 65535} {
		_ = cf.InternedString(id)
	}
	if id, ok := cf.InternedID(""); ok && id != 0 {
		t.Fatalf("the empty string is interned as id %d; the format reserves id 0 for it", id)
	}
	_, _ = cf.InternedID("a-string-no-cache-holds")
	if j, ok := cf.Lookup("a-name-no-cache-holds"); ok && (j < 0 || j >= cf.Len()) {
		t.Fatalf("Lookup of an absent name returned out-of-range index %d", j)
	}
	_ = cf.BadRefs()
	_ = cf.Size()
	_ = cf.Meta()
}

// secCheckLoad loads one candidate and either exercises it or asserts the
// refusal was the documented one.
func secCheckLoad(t *testing.T, data []byte) {
	t.Helper()
	cf, err := catalog.CacheFromBytes(data)
	if err != nil {
		if cf != nil {
			t.Fatalf("a rejected cache returned a non-nil *CacheFile: %v", err)
		}
		// §5 is explicit that every failure means "rebuild", and the caller
		// distinguishes the two sentinels to decide what to tell the operator.
		// An error that is neither is an error no caller handles.
		if !errors.Is(err, catalog.ErrCacheCorrupt) && !errors.Is(err, catalog.ErrCacheVersion) {
			t.Fatalf("refusal is neither ErrCacheCorrupt nor ErrCacheVersion: %v", err)
		}
		return
	}
	if cf == nil {
		t.Fatal("CacheFromBytes returned (nil, nil)")
	}
	secExerciseCache(t, cf)
}

// FuzzSecCacheImage fuzzes the five section words over a file that is
// well-formed everywhere else.
//
// This is the target that actually exercises docs/dev/cache-format.md §5's
// extent arithmetic: `records_len == entry_count × 80`, the forward,
// non-overlapping section order, the even catids length, the two counted
// sections' two-step bound, the interned-table ceiling, and the requirement
// that byte 0 of the arena is the reserved NUL.
func FuzzSecCacheImage(f *testing.F) {
	_, good := cacheTestSave(f, cacheTestEntries())

	// A real file's own words, so the fuzzer starts from a shape that loads.
	if len(good) >= secHeaderSize+secTrailerSize {
		f.Add(
			binary.LittleEndian.Uint32(good[16:20]),
			binary.LittleEndian.Uint64(good[24:32]),
			binary.LittleEndian.Uint64(good[32:40]),
			binary.LittleEndian.Uint64(good[40:48]),
			binary.LittleEndian.Uint64(good[48:56]),
			binary.LittleEndian.Uint64(good[56:64]),
			append([]byte(nil), good[secHeaderSize:len(good)-secTrailerSize]...),
		)
	}

	// An empty catalogue: zero records, an empty catids array, one interned
	// string (the reserved empty one), no categories, a one-byte NUL arena.
	// This is the smallest file the loader must accept, and every seed below
	// is a deliberate perturbation of it.
	empty := secEmptyBody()
	f.Add(uint32(0), uint64(64), uint64(0), uint64(64), uint64(0), uint64(64), empty)
	// The same body with each word pushed to an edge.
	f.Add(uint32(0), uint64(64), uint64(0), uint64(64), uint64(0), uint64(64+uint64(len(empty))), empty)
	f.Add(uint32(1), uint64(64), uint64(secRecordSize), uint64(64+secRecordSize), uint64(0), uint64(64+secRecordSize), empty)
	f.Add(uint32(0), uint64(63), uint64(0), uint64(64), uint64(0), uint64(64), empty)   // records inside the header
	f.Add(uint32(0), uint64(64), uint64(0), uint64(64), uint64(1), uint64(65), empty)   // odd catids length
	f.Add(uint32(0), uint64(64), uint64(0), uint64(63), uint64(0), uint64(64), empty)   // catids before the record table
	f.Add(^uint32(0), uint64(64), ^uint64(0), uint64(64), uint64(0), uint64(64), empty) // wrap attempts
	f.Add(uint32(0), ^uint64(0), uint64(0), ^uint64(0), uint64(0), ^uint64(0), empty)
	f.Add(uint32(0), uint64(64), uint64(0), uint64(64), uint64(0), uint64(64), []byte{})
	f.Add(uint32(0), uint64(64), uint64(0), uint64(64), uint64(0), uint64(64), []byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, entryCount uint32, recordsOff, recordsLen, catidsOff, catidsLen, internedOff uint64, body []byte) {
		secCheckLoad(t, secBuildImage(entryCount, recordsOff, recordsLen, catidsOff, catidsLen, internedOff, body))

		// And again with records_len made consistent with entry_count, which
		// is the single check that refuses most fuzzer-chosen headers before
		// any offset is looked at. Doing both halves in one execution is what
		// keeps the coverage from collapsing onto that one comparison.
		secCheckLoad(t, secBuildImage(entryCount, recordsOff, uint64(entryCount)*secRecordSize,
			catidsOff, catidsLen, internedOff, body))
	})
}

// secEmptyBody is the body of the smallest cache the loader accepts: an empty
// catids array, an interned table of one (the reserved empty string), an empty
// category summary, and a one-byte arena holding the reserved NUL.
func secEmptyBody() []byte {
	var b bytes.Buffer
	// interned table: count = 1, then one (off, len) pair naming (0, 0).
	_ = binary.Write(&b, binary.LittleEndian, uint32(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	// category summary: count = 0.
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	// arena: the reserved NUL.
	b.WriteByte(0)
	return b.Bytes()
}

// FuzzSecCacheRecords rewrites one record of a real cache and reloads it.
//
// The record table is where "validate every (off, len) pair at the point of
// use, not only at load" is tested, and it is the part of the file a raw
// fuzzer reaches least often: it sits behind the CRC, the header, the trailer
// and five section-extent checks, all of which a valid file passes and a
// mutated one usually does not.
func FuzzSecCacheRecords(f *testing.F) {
	_, good := cacheTestSave(f, cacheTestEntries())

	f.Add(uint32(0), make([]byte, secRecordSize))
	f.Add(uint32(0), bytes.Repeat([]byte{0xff}, secRecordSize))
	f.Add(uint32(1), bytes.Repeat([]byte{0xff}, secRecordSize))
	f.Add(^uint32(0), bytes.Repeat([]byte{0x7f}, secRecordSize))
	f.Add(uint32(0), []byte{})
	// A record whose string pairs each name a range one byte past the arena.
	over := make([]byte, secRecordSize)
	for _, off := range []int{0, 8, 20, 28, 32, 56} {
		binary.LittleEndian.PutUint32(over[off:off+4], 0xffff_fff0)
	}
	f.Add(uint32(0), over)

	f.Fuzz(func(t *testing.T, index uint32, rec []byte) {
		recordsOff := binary.LittleEndian.Uint64(good[24:32])
		recordsLen := binary.LittleEndian.Uint64(good[32:40])
		count := recordsLen / secRecordSize
		if count == 0 || len(rec) == 0 {
			return
		}
		at := recordsOff + uint64(index%uint32(count))*secRecordSize
		if at+secRecordSize > uint64(len(good)) {
			return
		}

		buf := append([]byte(nil), good...)
		n := copy(buf[at:at+secRecordSize], rec)
		_ = n
		secCheckLoad(t, secSealImage(buf))
	})
}

// FuzzSecCacheMeta fuzzes the other parser in the load path.
//
// meta.json is the commit marker (§3.1): a directory without a parseable one
// has no usable cache whatever else it holds. It is small, it is JSON, and it
// is read before the 19 MB beside it — which makes it the first attacker-
// adjacent bytes the loader touches, and the one place where a stack overflow
// in a decoder would happen before any of the format's own checks ran.
func FuzzSecCacheMeta(f *testing.F) {
	f.Add([]byte(`{"format_version":1,"cache_key":"ubuntu-24.04-amd64-1f2a3b4c5d6e","entry_count":3,"bin_size":512}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"format_version":99}`))
	f.Add([]byte(`{"format_version":1,"entry_count":-1,"bin_size":-1}`))
	f.Add([]byte(`{"format_version":1,"cache_key":"../../escape"}`))
	f.Add(bytes.Repeat([]byte(`{"a":`), 4096))

	dir := f.TempDir()
	path := filepath.Join(dir, catalog.CacheMetaName)

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		meta, err := catalog.ReadCacheMeta(dir)
		if err != nil {
			if !errors.Is(err, catalog.ErrCacheCorrupt) && !errors.Is(err, catalog.ErrCacheVersion) &&
				!errors.Is(err, catalog.ErrNotBuilt) {
				t.Fatalf("refusal is none of the three documented sentinels: %v", err)
			}
			return
		}
		// An accepted meta.json must at least be the version this build reads;
		// that is the one thing ReadCacheMeta promises about its result.
		if meta.FormatVersion != catalog.CacheFormatVersion {
			t.Fatalf("accepted a meta.json with format_version %d", meta.FormatVersion)
		}
		// Now asserted. This target found, through the seed above, that
		// `{"format_version":1,"entry_count":-1,"bin_size":-1}` was accepted
		// and returned a CacheMeta contradicting its own field comments
		// (docs/security-review.md §4.4). It was recorded rather than
		// asserted at the time, because asserting a property the code does
		// not have would have left a red test in a package this one did not
		// own. cacheReadMeta now refuses both, so the property is real and
		// belongs here: the next caller of ReadCacheMeta may be the one that
		// writes make([]X, meta.EntryCount).
		if meta.EntryCount < 0 || meta.BinSize < 0 {
			t.Fatalf("accepted a meta.json with entry_count %d and bin_size %d", meta.EntryCount, meta.BinSize)
		}
	})
}

// TestSecCacheHostileImages is the corner cases named rather than searched
// for: shapes a fuzzer reaches only by luck, kept as a table so a regression
// in any one of them is a named failure instead of a corpus entry.
func TestSecCacheHostileImages(t *testing.T) {
	empty := secEmptyBody()
	internedOff := uint64(secHeaderSize)

	cases := []struct {
		name   string
		image  []byte
		accept bool
	}{
		{
			// The smallest legal file. If this is rejected, every negative
			// case below proves nothing: they would all be failing on the
			// baseline rather than on the thing they name.
			name:   "the smallest legal cache",
			image:  secBuildImage(0, secHeaderSize, 0, secHeaderSize, 0, internedOff, empty),
			accept: true,
		},
		{
			name:  "arena is empty, so byte 0 cannot be the reserved NUL",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 0, internedOff, empty[:len(empty)-1]),
		},
		{
			name:  "arena byte 0 is not NUL",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 0, internedOff, append(append([]byte(nil), empty[:len(empty)-1]...), 'x')),
		},
		{
			// The ceiling the correctness backlog asked to be enforced: ids
			// are uint16, so a table one longer wraps and files string 65,536
			// under id 0 — the id the format reserves for "absent". Every row
			// naming it would silently lose the field inside a CRC-valid file.
			name:  "interned table declares more strings than a uint16 can address",
			image: secInternedCountImage(t, cacheMaxInternedForTest+1),
		},
		{
			name:   "interned table declares exactly the ceiling",
			image:  secInternedCountImage(t, cacheMaxInternedForTest),
			accept: true,
		},
		{
			name:  "interned count claims more strings than the file holds",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 0, internedOff, secInternedHeaderOnly(0xffff)),
		},
		{
			name:  "category summary count runs past the payload",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 0, internedOff, secCatSummaryCount(0xffff_ffff)),
		},
		{
			name:  "records_len disagrees with entry_count",
			image: secBuildImage(2, secHeaderSize, secRecordSize, secHeaderSize+secRecordSize, 0, secHeaderSize+secRecordSize, empty),
		},
		{
			name:  "records start inside the header",
			image: secBuildImage(0, 63, 0, secHeaderSize, 0, internedOff, empty),
		},
		{
			name:  "category ids start inside the record table",
			image: secBuildImage(1, secHeaderSize, secRecordSize, secHeaderSize+8, 0, secHeaderSize+secRecordSize, empty),
		},
		{
			name:  "catids_len is odd, so it cannot be an array of uint16",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 1, secHeaderSize+1, empty),
		},
		{
			name:  "interned table starts inside the category id array",
			image: secBuildImage(0, secHeaderSize, 0, secHeaderSize, 4, secHeaderSize+2, empty),
		},
		{
			// uint64 arithmetic must not wrap into a small, in-range end.
			name:  "records_off + records_len wraps",
			image: secBuildImage(0, ^uint64(0)-8, 16, secHeaderSize, 0, internedOff, empty),
		},
		{
			name:  "every section offset is the maximum uint64",
			image: secBuildImage(0, ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), empty),
		},
		{
			name:  "entry_count is the maximum uint32",
			image: secBuildImage(^uint32(0), secHeaderSize, uint64(^uint32(0))*secRecordSize, secHeaderSize, 0, internedOff, empty),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cf, err := catalog.CacheFromBytes(tc.image)
			switch {
			case tc.accept && err != nil:
				t.Fatalf("a legal cache was refused: %v", err)
			case tc.accept:
				secExerciseCache(t, cf)
			case err == nil:
				t.Fatalf("accepted a cache that %s", tc.name)
			case !errors.Is(err, catalog.ErrCacheCorrupt) && !errors.Is(err, catalog.ErrCacheVersion):
				t.Fatalf("refused with an error no caller handles: %v", err)
			case cf != nil:
				t.Fatalf("a rejected cache returned a non-nil *CacheFile")
			}
		})
	}
}

// cacheMaxInternedForTest mirrors the unexported cacheMaxInterned. Duplicated
// deliberately: this is a test of the *format*, and reading the constant from
// the code under test would make the check "the loader agrees with itself".
const cacheMaxInternedForTest = 65535

// secInternedCountImage builds a cache whose interned table declares count
// strings, every one of them the reserved empty string.
func secInternedCountImage(tb testing.TB, count uint32) []byte {
	tb.Helper()
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, count)
	for i := uint32(0); i < count; i++ {
		_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // off
		_ = binary.Write(&b, binary.LittleEndian, uint16(0)) // len
	}
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // no categories
	b.WriteByte(0)                                       // the reserved NUL
	body := b.Bytes()
	// secMaxBody exists for the fuzzer's benefit; the ceiling case is 384 KB
	// and is built here directly.
	buf := make([]byte, secHeaderSize+len(body)+secTrailerSize)
	copy(buf[0:8], secMagic)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(catalog.CacheFormatVersion))
	binary.LittleEndian.PutUint32(buf[12:16], secHeaderSize)
	binary.LittleEndian.PutUint64(buf[24:32], secHeaderSize)
	binary.LittleEndian.PutUint64(buf[40:48], secHeaderSize)
	binary.LittleEndian.PutUint64(buf[56:64], secHeaderSize)
	copy(buf[secHeaderSize:], body)
	return secSealImage(buf)
}

// secInternedHeaderOnly is an interned table that claims count strings and
// carries none of them, followed by nothing.
func secInternedHeaderOnly(count uint32) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, count)
	b.WriteByte(0)
	return b.Bytes()
}

// secCatSummaryCount is a one-string interned table followed by a category
// summary claiming count entries it does not carry.
func secCatSummaryCount(count uint32) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, count)
	b.WriteByte(0)
	return b.Bytes()
}
