package catalog_test

// FuzzLoad, required by docs/dev/cache-format.md §5.1: arbitrary bytes into
// the loader, which must return an error and never panic.
//
// The one thing a naive version of this would not test is anything past the
// CRC. A mutated file fails its checksum with overwhelming probability, so a
// fuzzer pointed straight at the loader would spend all its time proving that
// crc32 works and would never reach a single bounds check. Every input is
// therefore tried twice: once as the fuzzer produced it, and once with the
// trailer's length and checksum recomputed, which is what lets a mutated
// offset actually reach the validation it is meant to exercise.
//
// On success the fuzzer does not stop at the load either: it materialises
// every row and calls every accessor, because "never index out of bounds" is a
// claim about Entry and Lookup as much as about CacheFromBytes. A file can
// pass every structural check and still name a string range outside the text
// arena — the case §5.1 requires to yield an empty string and a counter rather
// than a panic.

import (
	"testing"

	"github.com/inferops/debark/gui/internal/catalog"
)

func FuzzLoad(f *testing.F) {
	_, good := cacheTestSave(f, cacheTestEntries())
	f.Add(good)
	f.Add([]byte(nil))
	f.Add(make([]byte, 88))

	// Truncations, including the boundaries the loader reasons about.
	for _, n := range []int{1, 8, 63, 64, 87, 88, 100, len(good) / 4, len(good) / 2, len(good) - 25, len(good) - 24, len(good) - 1} {
		if n > 0 && n < len(good) {
			f.Add(cacheTestClone(good[:n]))
		}
	}

	// Single-byte flips across the header, the record table, the interned
	// table, the arena and the trailer.
	for off := 0; off < len(good); off += 37 {
		b := cacheTestClone(good)
		b[off] ^= 0xff
		f.Add(b)
	}

	// A file whose trailer was repaired after a flip: the shape a fuzzer would
	// otherwise almost never produce, and the only shape that reaches the
	// structural checks.
	for off := 0; off < len(good); off += 101 {
		b := cacheTestClone(good)
		b[off] ^= 0x55
		f.Add(cacheTestRepair(b))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cacheFuzzLoad(t, data)
		cacheFuzzLoad(t, cacheTestRepair(cacheTestClone(data)))
	})
}

// cacheFuzzLoad loads one candidate and, if it is accepted, exercises every
// accessor over it. Any panic fails the fuzz target by definition; the
// assertions here catch the quieter failure, where a damaged file is accepted
// and then hands back something that is not a valid Entry.
func cacheFuzzLoad(t *testing.T, data []byte) {
	cf, err := catalog.CacheFromBytes(data)
	if err != nil {
		if cf != nil {
			t.Fatalf("a rejected cache returned a non-nil *CacheFile: %v", err)
		}
		return
	}
	if cf == nil {
		t.Fatal("CacheFromBytes returned (nil, nil)")
	}

	n := cf.Len()
	if n < 0 {
		t.Fatalf("Len = %d", n)
	}
	// A record table cannot be longer than the file, so this is bounded by the
	// input; the cap only keeps a pathological seed from slowing the fuzzer.
	if n > 4096 {
		n = 4096
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
		// Strings must be copies of arena bytes, never a slice reaching past
		// it: an out-of-range pair yields "" and counts, and nothing else.
		if len(e.Name) > 65535 || len(e.Summary) > 65535 || len(e.Version) > 65535 {
			t.Fatalf("Entry(%d) returned a string longer than a uint16 length can address", i)
		}
		if e.InstalledSizeKiB < 0 || e.DownloadSizeBytes < 0 {
			t.Fatalf("Entry(%d) returned a negative size: %#v", i, e)
		}
		_ = cf.NameBytes(i)
		_ = cf.Haystack(i)
		_ = cf.IsApp(i)
		_ = cf.HasCategoryID(i, cf.SectionID(i))
		if name := cf.Name(i); name != "" {
			// Lookup is a binary search, which only answers correctly over a
			// sorted table. A fuzzed file need not be sorted, so this asserts
			// that it terminates and stays in range, not that it finds the row.
			if j, found := cf.Lookup(name); found && (j < 0 || j >= cf.Len()) {
				t.Fatalf("Lookup(%q) returned out-of-range index %d", name, j)
			}
		}
	}

	for _, c := range cf.Categories() {
		if c.Tier != catalog.TierApplication && c.Tier != catalog.TierSection {
			t.Fatalf("category %q has tier %q", c.ID, c.Tier)
		}
		if _, _, ok := catalog.ParseCategoryID(c.ID); !ok && c.Name != "" {
			t.Fatalf("category id %q does not parse", c.ID)
		}
	}
	if j, ok := cf.Lookup("a-name-no-cache-holds"); ok && (j < 0 || j >= cf.Len()) {
		t.Fatalf("Lookup of an absent name returned out-of-range index %d", j)
	}
	_ = cf.InternedString(65535)
	_, _ = cf.InternedID("")
}
