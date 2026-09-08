package catalog_test

// Tests for the on-disk cache (docs/dev/cache-format.md, the implementing package).
//
// This is an external test package because the benchmarks build their corpus
// with internal/catalog/fake, which imports internal/catalog: an in-package
// test could not reach it. The happy side effect is that every test below goes
// through the exported API, so the surface catalog.go and search.go build on
// is exercised exactly as they will use it.
//
// The centre of gravity here is the corruption matrix. A cache is derived data
// that a power cut, a full disk or a killed process can damage at any moment,
// and the one outcome the format may never produce is a crash on start-up over
// a directory the operator cannot see. So every mutation below asserts two
// things: the right sentinel, and that nothing panicked.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/catalog/fake"
)

var cacheTestCRCTable = crc32.MakeTable(crc32.Castagnoli)

// cacheTestTarget is a realistic Ubuntu target: two suites, two components,
// which is what makes IndexRefs and therefore the identity non-trivial.
func cacheTestTarget() catalog.Target {
	return catalog.Target{
		Kind:       "base",
		BaseID:     "ubuntu:24.04/desktop",
		DistroID:   "ubuntu",
		VersionID:  "24.04",
		Codename:   "noble",
		PrettyName: "Ubuntu 24.04 LTS",
		Arch:       "amd64",
		Sources: []catalog.Source{{
			Types:      []string{"deb"},
			URIs:       []string{"http://archive.ubuntu.com/ubuntu"},
			Suites:     []string{"noble", "noble-updates"},
			Components: []string{"main", "universe"},
		}},
	}
}

// cacheTestEntries covers the shapes a real index produces and the ones that
// break naive formats: empty optional fields, non-ASCII text that must survive
// byte-for-byte, several categories on one row, a row with none, and values at
// the length the record's uint16 fields can just address.
func cacheTestEntries() []catalog.Entry {
	long := strings.Repeat("a very long upstream summary that keeps going, ", 6)
	return []catalog.Entry{
		{
			// Deliberately not first alphabetically: the writer must sort.
			Name: "zutty", Version: "0.14-1", Suite: "noble", Component: "universe",
			Arch: "amd64", Section: "x11", Priority: "optional",
			Summary: "Efficient full-featured X11 terminal emulator", Homepage: "https://git.hq.sig7.se/zutty.git",
			InstalledSizeKiB: 512, DownloadSizeBytes: 204800,
		},
		{
			// Every optional field empty. The (0,0) "absent" encoding has to
			// survive this without becoming an empty string at offset 0.
			Name: "aardvark-dns", Arch: "amd64",
		},
		{
			// UTF-8 in every text field, including a summary that is mostly
			// non-ASCII, plus lower-casing that is not ASCII folding.
			Name: "gnome-text-editor", Version: "46.3-1", Suite: "noble-updates", Component: "main",
			Arch: "amd64", Section: "gnome", Priority: "optional",
			AppName: "Éditeur de texte", Summary: "Éditeur de texte — 日本語 — Ταχύ — тест ΣΊΣΥΦΟΣ",
			Homepage: "https://apps.gnome.org/TextEditor/",
			IsApp:    true, Categories: []string{"Utility", "TextEditor"},
			IconRef:          "dep11/gnome-text-editor_64x64.png",
			InstalledSizeKiB: 4096, DownloadSizeBytes: 1048576,
		},
		{
			Name: "gimp", Version: "2.10.36-3", Suite: "noble", Component: "main",
			Arch: "all", Section: "graphics", Priority: "optional",
			AppName: "GNU Image Manipulation Program", Summary: long,
			IsApp: true, Categories: []string{"Graphics", "RasterGraphics", "Photography"},
			InstalledSizeKiB: 1 << 20, DownloadSizeBytes: 1 << 30,
		},
		{
			// A size that is not a whole number of KiB: the record stores KiB
			// rounded up, and the documented consequence is that this value
			// does not survive a round trip unchanged.
			Name: "libc6", Version: "2.39-0ubuntu8.3", Suite: "noble-updates", Component: "main",
			Arch: "amd64", Section: "libs", Priority: "required",
			Summary:          "GNU C Library: Shared libraries",
			InstalledSizeKiB: 13000, DownloadSizeBytes: 3221225,
		},
	}
}

// cacheTestWant applies the format's one documented lossy transform, so a
// round-trip comparison asserts what the format promises rather than what it
// happens to do. Size: is stored as KiB rounded up, which costs at most 1023
// bytes on a number displayed as "about 340 MB" (§4.5).
func cacheTestWant(e catalog.Entry) catalog.Entry {
	if e.DownloadSizeBytes > 0 {
		e.DownloadSizeBytes = ((e.DownloadSizeBytes + 1023) / 1024) * 1024
	} else {
		e.DownloadSizeBytes = 0
	}
	if e.InstalledSizeKiB < 0 {
		e.InstalledSizeKiB = 0
	}
	if len(e.Categories) == 0 {
		e.Categories = nil
	}
	return e
}

// cacheTestSave writes a cache into a fresh directory and returns the
// directory and the bytes of catalog.bin.
func cacheTestSave(tb testing.TB, entries []catalog.Entry) (string, []byte) {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "root", cacheTestTarget().CacheKey())
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:      cacheTestTarget(),
		Entries:     entries,
		ToolVersion: "debark-gui test",
	}); err != nil {
		tb.Fatalf("SaveCache: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, catalog.CacheBinName))
	if err != nil {
		tb.Fatalf("reading back %s: %v", catalog.CacheBinName, err)
	}
	return dir, raw
}

// cacheTestRepair recomputes the trailer so a structural mutation reaches the
// bounds checks instead of stopping at the CRC. Without it the CRC would be
// the only check any of these tests ever exercised.
func cacheTestRepair(buf []byte) []byte {
	if len(buf) < 24 {
		return buf
	}
	payload := len(buf) - 24
	binary.LittleEndian.PutUint64(buf[payload:payload+8], uint64(payload))
	binary.LittleEndian.PutUint32(buf[payload+8:payload+12], crc32.Checksum(buf[:payload], cacheTestCRCTable))
	return buf
}

func cacheTestClone(buf []byte) []byte { return append([]byte(nil), buf...) }

// cchInternedTable rebuilds a cache image around an interned string table that
// declares count strings, every one of them the reserved empty string, with an
// empty category summary and a one-byte arena behind it.
//
// Everything the loader checks structurally still holds: the body lies inside
// the payload, the summary is well formed, the arena opens with its NUL, the
// records are the ones the header describes and the trailer is recomputed. The
// only thing wrong with the result is the count. That is the whole point — an
// inflated count that does not fit is caught by the extent check and says
// nothing about whether the format's uint16 ceiling is enforced.
func cchInternedTable(good []byte, count uint32) []byte {
	internedOff := int(binary.LittleEndian.Uint64(good[56:64]))
	body := int(count) * 6
	out := make([]byte, 0, internedOff+4+body+4+1+24)
	out = append(out, good[:internedOff+4]...)
	out = append(out, make([]byte, body+4+1)...) // the body, an empty summary, the arena's NUL
	out = append(out, good[len(good)-24:]...)
	binary.LittleEndian.PutUint32(out[internedOff:internedOff+4], count)
	return cacheTestRepair(out)
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestCacheRoundTrip(t *testing.T) {
	in := cacheTestEntries()
	dir, _ := cacheTestSave(t, in)

	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatalf("LoadCacheDir: %v", err)
	}
	if cf.Len() != len(in) {
		t.Fatalf("loaded %d entries, wrote %d", cf.Len(), len(in))
	}

	// Records must be in package-name ascending byte order: Get is a binary
	// search over that invariant and Search's contract order depends on it.
	for i := 1; i < cf.Len(); i++ {
		if bytes.Compare(cf.NameBytes(i-1), cf.NameBytes(i)) >= 0 {
			t.Fatalf("records out of order at %d: %q then %q", i, cf.Name(i-1), cf.Name(i))
		}
	}

	for _, want := range in {
		want = cacheTestWant(want)
		got, ok := cf.Get(want.Name)
		if !ok {
			t.Fatalf("Get(%q): not found", want.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Get(%q):\n got %#v\nwant %#v", want.Name, got, want)
		}
	}
	if _, ok := cf.Get("no-such-package"); ok {
		t.Error("Get of an absent name reported found")
	}
	if _, ok := cf.Get(""); ok {
		t.Error("Get(\"\") reported found")
	}
	if n := cf.BadRefs(); n != 0 {
		t.Errorf("BadRefs = %d on an undamaged cache, want 0", n)
	}

	// The absent-versus-empty distinction the reserved NUL at arena byte 0
	// exists for.
	bare, _ := cf.Get("aardvark-dns")
	if bare.Version != "" || bare.Summary != "" || bare.Homepage != "" || bare.AppName != "" || bare.IconRef != "" {
		t.Errorf("empty optional fields came back populated: %#v", bare)
	}
	if bare.Categories != nil {
		t.Errorf("a row with no categories came back with %v", bare.Categories)
	}

	meta := cf.Meta()
	if meta.FormatVersion != catalog.CacheFormatVersion {
		t.Errorf("meta format version = %d", meta.FormatVersion)
	}
	if meta.EntryCount != len(in) || meta.BinSize != int64(cf.Size()) {
		t.Errorf("meta describes %d entries / %d bytes, file has %d / %d", meta.EntryCount, meta.BinSize, cf.Len(), cf.Size())
	}
	if meta.Identity != cacheTestTarget().Identity() {
		t.Error("meta.identity is not Target.Identity() verbatim")
	}
}

func TestCacheHaystack(t *testing.T) {
	dir, _ := cacheTestSave(t, cacheTestEntries())
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	i, ok := cf.Lookup("gnome-text-editor")
	if !ok {
		t.Fatal("Lookup failed")
	}
	hay := string(cf.Haystack(i))
	// name NUL app name NUL summary, lower-cased with full Unicode rules.
	// Note the trailing sigma: strings.ToLower maps every Σ to σ and does not
	// apply the final-sigma rule, so "ΣΊΣΥΦΟΣ" becomes "σίσυφοσ" rather
	// than "σίσυφος". That is fine and is why the format insists the same
	// function is applied to the query: both sides are wrong identically, so
	// the substring match still works.
	for _, want := range []string{"gnome-text-editor", "\x00éditeur de texte\x00", "日本語", "σίσυφοσ"} {
		if !strings.Contains(hay, want) {
			t.Errorf("haystack %q does not contain %q", hay, want)
		}
	}
	if strings.ContainsAny(hay, "ÉΣ") {
		t.Errorf("haystack was not lower-cased: %q", hay)
	}
	// The NUL separators are what stop a query matching across two fields.
	if strings.Count(hay, "\x00") != 2 {
		t.Errorf("haystack %q does not have exactly two NUL separators", hay)
	}
}

func TestCacheCategories(t *testing.T) {
	dir, _ := cacheTestSave(t, cacheTestEntries())
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cats := cf.Categories()
	if len(cats) == 0 {
		t.Fatal("no categories")
	}
	// Applications first, then sections, each ascending by name — the order
	// Categories() must return, stored rather than sorted at read time.
	seenSection := false
	var lastName string
	var lastTier catalog.CategoryTier
	for _, c := range cats {
		if c.Tier == catalog.TierSection {
			seenSection = true
		} else if seenSection {
			t.Fatalf("application category %q after a section category", c.Name)
		}
		if c.Tier == lastTier && c.Name <= lastName {
			t.Fatalf("categories not ascending within a tier: %q then %q", lastName, c.Name)
		}
		lastName, lastTier = c.Name, c.Tier
		if c.Count <= 0 {
			t.Errorf("category %q has count %d", c.ID, c.Count)
		}
		tier, name, ok := catalog.ParseCategoryID(c.ID)
		if !ok || tier != c.Tier || name != c.Name {
			t.Errorf("category id %q does not parse back to (%s, %s)", c.ID, c.Tier, c.Name)
		}
	}

	byID := map[string]int{}
	for _, c := range cats {
		byID[c.ID] = c.Count
	}
	if got := byID[catalog.AppCategoryID("Graphics")]; got != 1 {
		t.Errorf("app:Graphics count = %d, want 1", got)
	}
	if got := byID[catalog.SectionCategoryID("libs")]; got != 1 {
		t.Errorf("section:libs count = %d, want 1", got)
	}
	if _, ok := byID[catalog.SectionCategoryID("")]; ok {
		t.Error("an empty section produced a category")
	}

	// The uint16 path the search side filters with, not a per-row string compare.
	id, ok := cf.InternedID("RasterGraphics")
	if !ok {
		t.Fatal("InternedID(RasterGraphics) not found")
	}
	i, _ := cf.Lookup("gimp")
	if !cf.HasCategoryID(i, id) {
		t.Error("gimp is not in RasterGraphics")
	}
	j, _ := cf.Lookup("libc6")
	if cf.HasCategoryID(j, id) {
		t.Error("libc6 is in RasterGraphics")
	}
	if _, ok := cf.InternedID("NotACategory"); ok {
		t.Error("InternedID invented an id for a string the cache never saw")
	}
	if s := cf.InternedString(id); s != "RasterGraphics" {
		t.Errorf("InternedString(%d) = %q", id, s)
	}
	if s := cf.InternedString(65535); s != "" {
		t.Errorf("InternedString of an out-of-range id = %q, want \"\"", s)
	}
}

func TestCacheDuplicateAndUnsortedNames(t *testing.T) {
	// Two sources offering the same name: Entry.Version's rule is that the
	// later one wins, and the format needs unique names for the binary search
	// to have one answer.
	in := []catalog.Entry{
		{Name: "curl", Version: "8.5.0-2", Suite: "noble", Arch: "amd64"},
		{Name: "aa", Arch: "amd64"},
		{Name: "curl", Version: "8.5.0-2ubuntu10.6", Suite: "noble-updates", Arch: "amd64"},
		{Name: "", Arch: "amd64"}, // no name at all: dropped
	}
	dir, _ := cacheTestSave(t, in)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Len() != 2 {
		t.Fatalf("loaded %d entries, want 2 (one duplicate collapsed, one nameless dropped)", cf.Len())
	}
	got, _ := cf.Get("curl")
	if got.Version != "8.5.0-2ubuntu10.6" || got.Suite != "noble-updates" {
		t.Errorf("duplicate name kept the wrong occurrence: %#v", got)
	}
}

func TestCacheEmptyCatalogue(t *testing.T) {
	// A build that produced nothing must still write a loadable file: the
	// alternative is that an empty archive looks like a corrupt cache.
	dir, _ := cacheTestSave(t, nil)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatalf("LoadCacheDir on an empty catalogue: %v", err)
	}
	if cf.Len() != 0 || len(cf.Categories()) != 0 {
		t.Errorf("empty catalogue has %d entries and %d categories", cf.Len(), len(cf.Categories()))
	}
	if _, ok := cf.Entry(0); ok {
		t.Error("Entry(0) succeeded on an empty catalogue")
	}
	if _, ok := cf.Get("anything"); ok {
		t.Error("Get succeeded on an empty catalogue")
	}
}

func TestCacheOutOfRangeIndexIsSafe(t *testing.T) {
	dir, _ := cacheTestSave(t, cacheTestEntries())
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{-1, cf.Len(), cf.Len() + 1, 1 << 30} {
		if _, ok := cf.Entry(i); ok {
			t.Errorf("Entry(%d) reported ok", i)
		}
		if cf.NameBytes(i) != nil || cf.Haystack(i) != nil || cf.IsApp(i) || cf.SectionID(i) != 0 || cf.HasCategoryID(i, 1) {
			t.Errorf("an accessor answered for out-of-range index %d", i)
		}
	}
}

func TestCacheLongValuesAreClippedOnARuneBoundary(t *testing.T) {
	// A uint16 length cannot address more than 65535 bytes. Real summaries are
	// two orders of magnitude below that; a malformed index is not, and the
	// clip must not cut a multi-byte rune in half.
	head := strings.Repeat("x", 65534)
	e := catalog.Entry{
		Name:    "over-long",
		Arch:    "amd64",
		Summary: head + "€€€", // the first euro sign straddles the limit
	}
	dir, _ := cacheTestSave(t, []catalog.Entry{e})
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cf.Get("over-long")
	if !ok {
		t.Fatal("not found")
	}
	if len(got.Summary) > 65535 {
		t.Errorf("summary is %d bytes, longer than a uint16 can address", len(got.Summary))
	}
	if !utf8.ValidString(got.Summary) {
		t.Error("clipping cut a rune in half")
	}
	if got.Summary != head {
		t.Errorf("summary clipped to %d bytes, want %d", len(got.Summary), len(head))
	}
}

// TestCacheOverLongInvalidValueDoesNotStall pins the cost of the worst input
// the clip can be handed, not just its answer.
//
// The shape that costs the most is an over-long field whose first invalid byte
// sits early in the 65,535-byte clip window: the clip has to walk back to it,
// and the obvious implementation — shrink by a byte, revalidate the whole
// candidate — does that in about two billion byte comparisons. Reported at
// 174 ms for one such field; ~30 ms here, where utf8.ValidString is very fast.
// One per entry across a real archive is half an hour to several hours, which
// is why it counts. Reachable only from a malformed index, which is precisely
// the input a writer that clips at all exists to survive.
//
// Following the house rule, the time is measured and logged rather than
// asserted: a wall-clock assertion on a shared machine is a flake generator,
// and this one would have to distinguish 30 ms from 1 ms. What is asserted is
// the answer, which the linear scan must not change — run this against the old
// clip and it still passes, which is the honest thing to say about it.
func TestCacheOverLongInvalidValueDoesNotStall(t *testing.T) {
	const (
		size    = 4 << 20
		invalid = 32767 // before the 65,535-byte limit, which is the bad case
	)
	b := []byte(strings.Repeat("x", size))
	b[invalid] = 0xFF // never valid UTF-8, in any position

	started := time.Now()
	dir, _ := cacheTestSave(t, []catalog.Entry{{
		Name: "over-long-invalid", Arch: "amd64", Summary: string(b),
	}})
	elapsed := time.Since(started)

	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cf.Get("over-long-invalid")
	if !ok {
		t.Fatal("not found")
	}
	if got.Summary != strings.Repeat("x", invalid) {
		t.Errorf("summary is %d bytes, want the %d valid ones before the bad byte", len(got.Summary), invalid)
	}
	if !utf8.ValidString(got.Summary) {
		t.Error("the clipped value is not valid UTF-8, which is the one thing cacheClip promises")
	}
	t.Logf("saving one %d-byte field whose first invalid byte is at %d took %v", size, invalid, elapsed.Round(time.Millisecond))
}

// TestCacheOverLongNamesKeepTheSortInvariant covers the other half of the same
// defect: the writer sorted on the name that arrived and clipped it afterwards,
// so the array Lookup binary-searches was sorted by one string and stored
// another.
//
// Two failures come out of that, and both are silent. Names over the limit that
// share a prefix collapse into one stored name, so the array is no longer
// unique. And a name cut short by an invalid byte can sort before the name it
// followed, so the array is no longer ascending — after which a binary search
// can walk past a row that is genuinely there.
func TestCacheOverLongNamesKeepTheSortInvariant(t *testing.T) {
	const limit = 65535
	long := func(prefix, tail string) string {
		return prefix + strings.Repeat("a", limit-len(prefix)) + tail
	}
	// "cut" is longer than "plain" and identical up to an invalid byte well
	// before the limit, so unclipped it sorts after and clipped it sorts
	// before. That inversion is what breaks the ascending invariant.
	cut := []byte(long("m", "zzz"))
	cut[1000] = 0xFF

	entries := []catalog.Entry{
		{Name: "aardvark", Arch: "amd64"},
		{Name: long("m", "one"), Arch: "amd64", Summary: "first"},
		{Name: long("m", "two"), Arch: "amd64", Summary: "second"},
		{Name: string(cut), Arch: "amd64"},
		{Name: "zzz-last", Arch: "amd64"},
	}
	dir, _ := cacheTestSave(t, entries)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The format's central invariant, checked over the stored bytes rather
	// than over what went in.
	var prev string
	for i := 0; i < cf.Len(); i++ {
		name := string(cf.NameBytes(i))
		if i > 0 && name <= prev {
			t.Fatalf("row %d is %q after %q: the stored names are not ascending and unique", i, name, prev)
		}
		prev = name
	}
	// Every stored row must be findable by the name it is stored under, which
	// is what a broken invariant takes away.
	for i := 0; i < cf.Len(); i++ {
		name := string(cf.NameBytes(i))
		if j, ok := cf.Lookup(name); !ok || j != i {
			t.Errorf("Lookup(%d bytes) = (%d, %v), want (%d, true)", len(name), j, ok, i)
		}
	}
	// Two names that clip to the same stored string are one row, keeping the
	// last, which is the same rule a duplicate name has always followed.
	e, ok := cf.Get(long("m", ""))
	if !ok {
		t.Fatal("the collapsed over-long name is not there at all")
	}
	if e.Summary != "second" {
		t.Errorf("summary = %q, want the last of the two that collapsed", e.Summary)
	}
	if cf.Len() != 4 {
		t.Errorf("cache holds %d rows, want 4: aardvark, the collapsed pair, the cut name and zzz-last", cf.Len())
	}
}

// cchOverLong builds a value longer than a uint16 length can address, with a
// multi-byte rune straddling the limit so that clipping (which stops at the
// rune before it) is distinguishable from clamping the length (which cuts the
// rune in half) and from wrapping it (which describes a different string
// entirely). marker keeps each field's value distinct, so a mixed-up field is
// visible in the failure rather than hidden behind a shared interned id.
func cchOverLong(marker string) (raw, clipped string) {
	head := marker + strings.Repeat("x", 65534-len(marker))
	return head + "€€€", head
}

func TestCacheLongInternedValuesAreClippedOnARuneBoundary(t *testing.T) {
	// The interned fields — Section, Priority, Suite, Component, Architecture
	// and DEP-11 category names — reach the arena from a malformed index at
	// whatever length the index carried, and the interned table addresses them
	// with the same uint16 length a record field uses. Clipping them at the
	// call site is what keeps the stored string the one the length describes.
	sectionRaw, sectionWant := cchOverLong("section-")
	priorityRaw, priorityWant := cchOverLong("priority-")
	suiteRaw, suiteWant := cchOverLong("suite-")
	componentRaw, componentWant := cchOverLong("component-")
	archRaw, archWant := cchOverLong("arch-")
	categoryRaw, categoryWant := cchOverLong("category-")

	// Exactly 65536 bytes is the length the original wrap turned into the
	// empty string: uint16(65536) is 0, and (0,0) is the format's encoding for
	// "absent". A CRC-valid cache silently losing a field is the failure the
	// whole clipping rule exists to prevent, so it gets its own row.
	wrapRaw := "wraps-" + strings.Repeat("y", 65536-len("wraps-"))
	wrapWant := wrapRaw[:65535]

	in := []catalog.Entry{
		{
			Name: "over-long-interned", Version: "1", Section: sectionRaw,
			Priority: priorityRaw, Suite: suiteRaw, Component: componentRaw,
			Arch: archRaw, IsApp: true, Categories: []string{categoryRaw},
		},
		{Name: "wraps-to-empty", Arch: "amd64", Section: wrapRaw},
	}
	dir, _ := cacheTestSave(t, in)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := cf.Get("over-long-interned")
	if !ok {
		t.Fatal("over-long-interned: not found")
	}
	for _, f := range []struct {
		field string
		got   string
		want  string
	}{
		{"Section", got.Section, sectionWant},
		{"Priority", got.Priority, priorityWant},
		{"Suite", got.Suite, suiteWant},
		{"Component", got.Component, componentWant},
		{"Arch", got.Arch, archWant},
	} {
		if len(f.got) > 65535 {
			t.Errorf("%s is %d bytes, longer than a uint16 length can address", f.field, len(f.got))
		}
		if !utf8.ValidString(f.got) {
			t.Errorf("%s was cut mid-rune: it is not valid UTF-8", f.field)
		}
		if f.got != f.want {
			t.Errorf("%s round-tripped as %d bytes, want the %d-byte value clipped on a rune boundary", f.field, len(f.got), len(f.want))
		}
	}
	if len(got.Categories) != 1 || got.Categories[0] != categoryWant {
		t.Errorf("DEP-11 category round-tripped as %d values, want the one clipped name", len(got.Categories))
	}

	wrapped, ok := cf.Get("wraps-to-empty")
	if !ok {
		t.Fatal("wraps-to-empty: not found")
	}
	if wrapped.Section == "" {
		t.Error("a 65536-byte Section came back empty: the length wrapped to zero, which the format reads as absent")
	}
	if wrapped.Section != wrapWant {
		t.Errorf("a 65536-byte Section round-tripped as %d bytes, want %d", len(wrapped.Section), len(wrapWant))
	}
	if n := cf.BadRefs(); n != 0 {
		t.Errorf("BadRefs = %d", n)
	}
}

func TestCacheClippedInternedValuesKeyTheCountsAndTheSummary(t *testing.T) {
	// The subtle half of the same bug. An interned value is used three times
	// per row: interned into the table a record's id points at, counted into
	// appCounts/sectionCounts, and looked up again to key the category
	// summary. Clip it in one of those places and not the others and the
	// counts and the summary describe a string no record points at — inside a
	// file whose CRC says it is intact, so nothing downstream can notice.
	sectionRaw, sectionWant := cchOverLong("section-")
	categoryRaw, categoryWant := cchOverLong("category-")

	in := []catalog.Entry{
		{
			Name: "aaa", Arch: "amd64", Section: sectionRaw,
			IsApp: true, Categories: []string{categoryRaw},
		},
		{Name: "bbb", Arch: "amd64", Section: sectionRaw},
	}
	dir, _ := cacheTestSave(t, in)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	sectionID, ok := cf.InternedID(sectionWant)
	if !ok || sectionID == 0 {
		t.Fatalf("the clipped Section is not in the interned table (id %d, ok %v)", sectionID, ok)
	}
	if _, ok := cf.InternedID(sectionRaw); ok {
		t.Error("the unclipped Section was interned as well as the clipped one")
	}
	categoryID, ok := cf.InternedID(categoryWant)
	if !ok || categoryID == 0 {
		t.Fatalf("the clipped category is not in the interned table (id %d, ok %v)", categoryID, ok)
	}

	// Every row points at the clipped string...
	for i := 0; i < cf.Len(); i++ {
		if id := cf.SectionID(i); id != sectionID {
			t.Errorf("record %d has section id %d, want %d", i, id, sectionID)
		}
	}
	if !cf.HasCategoryID(0, categoryID) {
		t.Error("record 0 does not carry the clipped category id")
	}

	// ...and the summary must name that same string, with the counts the rows
	// actually carry. A summary naming id 0 — the reserved empty string — is
	// what an unclipped counting key produces.
	var sawSection, sawApp bool
	for _, c := range cf.Categories() {
		switch c.Tier {
		case catalog.TierSection:
			sawSection = true
			if c.Name != sectionWant {
				t.Errorf("the section summary names a %d-byte string, want the %d-byte clipped Section", len(c.Name), len(sectionWant))
			}
			if c.ID != catalog.SectionCategoryID(sectionWant) {
				t.Error("the section summary's id does not derive from the clipped name")
			}
			if c.Count != 2 {
				t.Errorf("the section summary counts %d rows, want 2", c.Count)
			}
		case catalog.TierApplication:
			sawApp = true
			if c.Name != categoryWant {
				t.Errorf("the application summary names a %d-byte string, want the %d-byte clipped category", len(c.Name), len(categoryWant))
			}
			if c.Count != 1 {
				t.Errorf("the application summary counts %d rows, want 1", c.Count)
			}
		}
	}
	if !sawSection || !sawApp {
		t.Errorf("summary is missing a tier: section %v, application %v", sawSection, sawApp)
	}

	// And the row the summary claims to describe agrees with it.
	got, ok := cf.Get("aaa")
	if !ok {
		t.Fatal("aaa: not found")
	}
	if got.Section != sectionWant {
		t.Errorf("Entry.Section is %d bytes, want the %d-byte clipped value the summary names", len(got.Section), len(sectionWant))
	}
	if !reflect.DeepEqual(got.Categories, []string{categoryWant}) {
		t.Error("Entry.Categories does not hold the clipped name the summary names")
	}
}

func TestCacheFakeCorpusRoundTrips(t *testing.T) {
	in := cacheTestCorpus(t, 4000)
	dir, _ := cacheTestSave(t, in)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Len() != len(in) {
		t.Fatalf("loaded %d, wrote %d", cf.Len(), len(in))
	}
	for _, want := range in {
		got, ok := cf.Get(want.Name)
		if !ok {
			t.Fatalf("Get(%q): not found", want.Name)
		}
		if w := cacheTestWant(want); !reflect.DeepEqual(got, w) {
			t.Fatalf("Get(%q):\n got %#v\nwant %#v", want.Name, got, w)
		}
	}
	if n := cf.BadRefs(); n != 0 {
		t.Errorf("BadRefs = %d", n)
	}
}

// ---------------------------------------------------------------------------
// The corruption matrix
// ---------------------------------------------------------------------------

func TestCacheCorruptionMatrix(t *testing.T) {
	_, good := cacheTestSave(t, cacheTestEntries())
	// Locate the sections the way the loader does, so the mutations below land
	// where they are meant to rather than at a guessed offset.
	recordsOff := int(binary.LittleEndian.Uint64(good[24:32]))
	recordsLen := int(binary.LittleEndian.Uint64(good[32:40]))
	internedOff := int(binary.LittleEndian.Uint64(good[56:64]))
	internCount := int(binary.LittleEndian.Uint32(good[internedOff : internedOff+4]))
	catSummaryOff := internedOff + 4 + internCount*6
	catSummaryCount := int(binary.LittleEndian.Uint32(good[catSummaryOff : catSummaryOff+4]))
	arenaOff := catSummaryOff + 4 + catSummaryCount*8
	payload := len(good) - 24

	cases := []struct {
		name   string
		mutate func(b []byte) []byte
		repair bool // recompute the trailer, so the mutation reaches the structural checks
		want   error
	}{
		{"empty file", func(b []byte) []byte { return nil }, false, catalog.ErrCacheCorrupt},
		{"one byte", func(b []byte) []byte { return b[:1] }, false, catalog.ErrCacheCorrupt},
		{"truncated inside the header", func(b []byte) []byte { return b[:40] }, false, catalog.ErrCacheCorrupt},
		{"truncated one byte under the minimum", func(b []byte) []byte { return b[:87] }, false, catalog.ErrCacheCorrupt},
		{"truncated inside the records", func(b []byte) []byte { return b[:recordsOff+8] }, false, catalog.ErrCacheCorrupt},
		{"truncated halfway", func(b []byte) []byte { return b[:len(b)/2] }, false, catalog.ErrCacheCorrupt},
		{"truncated inside the arena", func(b []byte) []byte { return b[:arenaOff+4] }, false, catalog.ErrCacheCorrupt},
		{"trailer removed", func(b []byte) []byte { return b[:payload] }, false, catalog.ErrCacheCorrupt},
		{"last byte lost", func(b []byte) []byte { return b[:len(b)-1] }, false, catalog.ErrCacheCorrupt},
		{"all zeroes", func(b []byte) []byte { return make([]byte, len(b)) }, false, catalog.ErrCacheCorrupt},

		{"magic flipped", func(b []byte) []byte { b[3] ^= 0x20; return b }, true, catalog.ErrCacheCorrupt},
		{"format version too new", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[8:12], catalog.CacheFormatVersion+1)
			return b
		}, true, catalog.ErrCacheVersion},
		{"format version too old", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[8:12], 0)
			return b
		}, true, catalog.ErrCacheVersion},
		{"format version flipped without repairing the crc", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[8:12], 99)
			return b
		}, false, catalog.ErrCacheVersion},
		{"header size wrong", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[12:16], 128)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"reserved header flags set", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[20:24], 1)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"entry count inflated", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[16:20], 1000)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"records overlap the header", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[24:32], 0)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"records run past the payload", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[32:40], uint64(len(b)*4))
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"records length not a whole number of records", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[32:40], uint64(recordsLen+1))
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category ids overlap the records", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[40:48], uint64(recordsOff))
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category id array has an odd length", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[48:56], 3)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category id array runs past the payload", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[48:56], 1<<40)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"interned table starts past the payload", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[56:64], uint64(len(b)+4096))
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"interned table offset overflows", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[56:64], ^uint64(0)-8)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"interned count inflated", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[internedOff:internedOff+4], 1<<20)
			return b
		}, true, catalog.ErrCacheCorrupt},
		// The class the matrix did not cover: a count that is too large for
		// the format's uint16 ids and yet small enough that its body fits the
		// payload, so no extent check fires. Every other "count inflated" case
		// here is caught by arithmetic; this one has to be caught by policy.
		{"interned count above the ceiling with a body that fits", func(b []byte) []byte {
			return cchInternedTable(b, 65536)
		}, false, catalog.ErrCacheCorrupt},
		{"interned id 0 is not the empty string", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[internedOff+8:internedOff+10], 3)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"interned string points outside the arena", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[internedOff+10:internedOff+14], uint32(len(b)))
			binary.LittleEndian.PutUint16(b[internedOff+14:internedOff+16], 8)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category summary count inflated", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[catSummaryOff:catSummaryOff+4], 1<<20)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category summary tier out of range", func(b []byte) []byte {
			b[catSummaryOff+4] = 7
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category summary reserved byte set", func(b []byte) []byte {
			b[catSummaryOff+5] = 1
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"category summary names an unknown interned id", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[catSummaryOff+6:catSummaryOff+8], 60000)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"arena byte 0 is not the reserved NUL", func(b []byte) []byte {
			b[arenaOff] = 'x'
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"record reserved flag bit set", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[recordsOff+6:recordsOff+8], 4)
			return b
		}, true, catalog.ErrCacheCorrupt},
		{"record reserved word set", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[recordsOff+62:recordsOff+64], 1)
			return b
		}, true, catalog.ErrCacheCorrupt},

		{"byte flipped in a record", func(b []byte) []byte { b[recordsOff+3] ^= 0xff; return b }, false, catalog.ErrCacheCorrupt},
		{"byte flipped in the arena", func(b []byte) []byte { b[arenaOff+9] ^= 0x01; return b }, false, catalog.ErrCacheCorrupt},
		{"byte flipped in the interned table", func(b []byte) []byte { b[internedOff+7] ^= 0x40; return b }, false, catalog.ErrCacheCorrupt},
		{"crc corrupted", func(b []byte) []byte { b[payload+8] ^= 0x01; return b }, false, catalog.ErrCacheCorrupt},
		{"payload length in the trailer is wrong", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[payload:payload+8], uint64(payload-1))
			return b
		}, false, catalog.ErrCacheCorrupt},
		{"trailer reserved word set", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[payload+12:payload+16], 1)
			return b
		}, false, catalog.ErrCacheCorrupt},
		{"trailer magic damaged", func(b []byte) []byte { b[payload+20] = 'X'; return b }, false, catalog.ErrCacheCorrupt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.mutate(cacheTestClone(good))
			if tc.repair {
				b = cacheTestRepair(b)
			}
			// The panic guard is the point of the whole matrix: an app that
			// crashes at start-up because of a file the operator cannot see is
			// the worst outcome this format can produce.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			cf, err := catalog.CacheFromBytes(b)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if cf != nil {
				t.Fatalf("a rejected cache returned a non-nil *CacheFile")
			}
		})
	}
}

func TestCacheInternedTableCeiling(t *testing.T) {
	_, good := cacheTestSave(t, cacheTestEntries())

	// 65,535 is the largest table the writer can emit, and it has to keep
	// loading: the guard exists to reject tables the format cannot address,
	// not to narrow the format.
	cf, err := catalog.CacheFromBytes(cchInternedTable(good, 65535))
	if err != nil {
		t.Fatalf("a table at the ceiling must load: %v", err)
	}
	if cf == nil {
		t.Fatal("CacheFromBytes returned (nil, nil)")
	}

	// One past it is the silent failure. Ids are uint16, so string 65,536 is
	// filed under id 0 — which the format reserves for "absent" — and every
	// row naming it loses the field, inside a file whose CRC says it is
	// intact. Nothing here is memory-unsafe, which is exactly why it needs a
	// sentinel: without one it is invisible.
	for _, count := range []uint32{65536, 65537, 1 << 17} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on a table of %d strings: %v", count, r)
				}
			}()
			cf, err := catalog.CacheFromBytes(cchInternedTable(good, count))
			if !errors.Is(err, catalog.ErrCacheCorrupt) {
				t.Errorf("a table of %d strings gave %v, want ErrCacheCorrupt", count, err)
			}
			if cf != nil {
				t.Errorf("a table of %d strings returned a non-nil *CacheFile", count)
			}
		}()
	}
}

func TestCacheTruncationAtEveryOffset(t *testing.T) {
	// Every prefix of a valid file, at a stride that still covers the header,
	// each section boundary and the trailer. A power cut can leave any of
	// them, and none may be accepted or panic.
	_, good := cacheTestSave(t, cacheTestEntries())
	for n := 0; n < len(good); n += 7 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic truncating at %d: %v", n, r)
				}
			}()
			cf, err := catalog.CacheFromBytes(good[:n:n])
			if err == nil {
				t.Fatalf("a %d-byte prefix of a %d-byte file loaded", n, len(good))
			}
			if !errors.Is(err, catalog.ErrCacheCorrupt) && !errors.Is(err, catalog.ErrCacheVersion) {
				t.Fatalf("truncation at %d gave %v, want a cache sentinel", n, err)
			}
			if cf != nil {
				t.Fatalf("truncation at %d returned a non-nil *CacheFile", n)
			}
		}()
	}
}

func TestCacheSingleByteFlipsAreCaught(t *testing.T) {
	// A CRC-32C is what makes bitrot and a half-flushed write detectable.
	// Sample the file rather than flipping all ~9 KB of it: the check is
	// arithmetic, not positional.
	_, good := cacheTestSave(t, cacheTestEntries())
	for off := 0; off < len(good); off += 13 {
		for _, bit := range []byte{0x01, 0x80} {
			b := cacheTestClone(good)
			b[off] ^= bit
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic flipping %#x at %d: %v", bit, off, r)
					}
				}()
				if _, err := catalog.CacheFromBytes(b); err == nil {
					t.Fatalf("flipping %#x at offset %d was not detected", bit, off)
				}
			}()
		}
	}
}

func TestCacheDamagedRecordServesAPageAndCountsIt(t *testing.T) {
	// A record naming a plausible range outside the arena is the case §5.1
	// singles out: the page is still served, the row loses a field, and the
	// counter says the cache should be rebuilt. Failing a keystroke over one
	// bad row would be worse than the row being wrong.
	_, good := cacheTestSave(t, cacheTestEntries())
	recordsOff := int(binary.LittleEndian.Uint64(good[24:32]))
	// Record 1 is gimp: it has a summary and three categories, so damaging its
	// pointers actually reaches the point-of-use checks.
	recordSize := int(binary.LittleEndian.Uint64(good[32:40])) / int(binary.LittleEndian.Uint32(good[16:20]))
	rec := recordsOff + recordSize
	b := cacheTestClone(good)
	binary.LittleEndian.PutUint32(b[rec+20:rec+24], 1<<30) // summary_off
	binary.LittleEndian.PutUint32(b[rec+52:rec+56], 1<<30) // category_off
	b = cacheTestRepair(b)

	cf, err := catalog.CacheFromBytes(b)
	if err != nil {
		t.Fatalf("a damaged record must not fail the load: %v", err)
	}
	e, ok := cf.Entry(1)
	if !ok {
		t.Fatal("Entry(1) failed")
	}
	if e.Name != "gimp" {
		t.Fatalf("record 1 is %q, want gimp", e.Name)
	}
	if e.Categories != nil {
		t.Errorf("out-of-range category range yielded %v, want none", e.Categories)
	}
	if e.Summary != "" {
		t.Errorf("out-of-range summary yielded %q, want the empty string", e.Summary)
	}
	if e.Name == "" {
		t.Error("the undamaged fields of a damaged row were lost too")
	}
	if cf.BadRefs() == 0 {
		t.Error("BadRefs = 0; a damaged row must schedule a rebuild")
	}
}

// ---------------------------------------------------------------------------
// The directory protocol
// ---------------------------------------------------------------------------

func TestCacheDirectoryProtocol(t *testing.T) {
	t.Run("no directory at all", func(t *testing.T) {
		_, err := catalog.LoadCacheDir(filepath.Join(t.TempDir(), "nothing"))
		if !errors.Is(err, catalog.ErrNotBuilt) {
			t.Fatalf("err = %v, want ErrNotBuilt", err)
		}
	})

	t.Run("commit marker missing", func(t *testing.T) {
		// The half-written case §3.1 exists for: catalog.bin is complete and
		// meta.json never arrived. Nothing may read it, and nothing may delete
		// it either — it can be a build running right now.
		dir, _ := cacheTestSave(t, cacheTestEntries())
		if err := os.Remove(filepath.Join(dir, catalog.CacheMetaName)); err != nil {
			t.Fatal(err)
		}
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrNotBuilt) {
			t.Fatalf("err = %v, want ErrNotBuilt", err)
		}
		if _, err := os.Stat(filepath.Join(dir, catalog.CacheBinName)); err != nil {
			t.Errorf("a directory with no commit marker was deleted under a possible concurrent build: %v", err)
		}
		if ready, err := catalog.CacheReadyDir(dir, cacheTestTarget()); ready || err != nil {
			t.Errorf("Ready = (%v, %v), want (false, nil)", ready, err)
		}
	})

	t.Run("commit marker unparseable", func(t *testing.T) {
		dir, _ := cacheTestSave(t, cacheTestEntries())
		if err := os.WriteFile(filepath.Join(dir, catalog.CacheMetaName), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheCorrupt) {
			t.Fatalf("err = %v, want ErrCacheCorrupt", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("wrong format version in the marker", func(t *testing.T) {
		dir, _ := cacheTestSave(t, cacheTestEntries())
		cacheTestRewriteMeta(t, dir, func(m map[string]any) { m["format_version"] = catalog.CacheFormatVersion + 1 })
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheVersion) {
			t.Fatalf("err = %v, want ErrCacheVersion", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("index missing", func(t *testing.T) {
		dir, _ := cacheTestSave(t, cacheTestEntries())
		if err := os.Remove(filepath.Join(dir, catalog.CacheBinName)); err != nil {
			t.Fatal(err)
		}
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrNotBuilt) {
			t.Fatalf("err = %v, want ErrNotBuilt", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("marker disagrees about the entry count", func(t *testing.T) {
		dir, _ := cacheTestSave(t, cacheTestEntries())
		cacheTestRewriteMeta(t, dir, func(m map[string]any) { m["entry_count"] = 999 })
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheCorrupt) {
			t.Fatalf("err = %v, want ErrCacheCorrupt", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("marker disagrees about the size", func(t *testing.T) {
		dir, _ := cacheTestSave(t, cacheTestEntries())
		cacheTestRewriteMeta(t, dir, func(m map[string]any) { m["bin_size"] = 123 })
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheCorrupt) {
			t.Fatalf("err = %v, want ErrCacheCorrupt", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("truncated index on disk", func(t *testing.T) {
		dir, raw := cacheTestSave(t, cacheTestEntries())
		if err := os.WriteFile(filepath.Join(dir, catalog.CacheBinName), raw[:len(raw)/3], 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheCorrupt) {
			t.Fatalf("err = %v, want ErrCacheCorrupt", err)
		}
		cacheTestAssertGone(t, dir)
	})

	t.Run("a commit in flight is never deleted over a disagreement", func(t *testing.T) {
		// Between a build's two renames the marker and the index legitimately
		// describe different files. §5 calls that corruption and answers it by
		// deleting the directory, which is exactly wrong here: the cache being
		// disagreed about is the one the build is halfway through replacing,
		// and either the build finishes or its own rollback puts the previous
		// index back. A temporary beside the marker is what that window looks
		// like from outside, so nothing is removed.
		//
		// This subtest used to go on to assert ErrCacheCorrupt "-- the caller
		// rebuilds", which does not follow from its own reasoning: if the
		// build is about to finish, the answer is to wait, not to start a
		// second forty-second download. The verdict is now ErrCacheBusy. See
		// TestLoadDuringACommitIsBusyNotCorrupt, which pins the whole
		// distinction, and TestLoadOfAQuietMismatchIsStillCorrupt, which keeps
		// §6.4 intact for a directory nothing is writing.
		dir, _ := cacheTestSave(t, cacheTestEntries())
		tmp := filepath.Join(dir, catalog.CacheMetaName+".tmp-deadbeef")
		if err := os.WriteFile(tmp, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		cacheTestRewriteMeta(t, dir, func(m map[string]any) { m["entry_count"] = 999 })

		_, err := catalog.LoadCacheDir(dir)
		if !errors.Is(err, catalog.ErrCacheBusy) {
			t.Fatalf("err = %v, want ErrCacheBusy", err)
		}
		if _, err := os.Stat(filepath.Join(dir, catalog.CacheBinName)); err != nil {
			t.Errorf("a directory a build was still writing into was deleted: %v", err)
		}
	})

	t.Run("a temporary file left behind is ignored", func(t *testing.T) {
		// A killed build leaves catalog.bin.tmp-xxxxxxxx. It must not be
		// mistaken for anything, and the cache beside it must still load.
		dir, raw := cacheTestSave(t, cacheTestEntries())
		tmp := filepath.Join(dir, catalog.CacheBinName+".tmp-deadbeef")
		if err := os.WriteFile(tmp, raw[:len(raw)/2], 0o644); err != nil {
			t.Fatal(err)
		}
		cf, err := catalog.LoadCacheDir(dir)
		if err != nil {
			t.Fatalf("a leftover temporary broke the load: %v", err)
		}
		if cf.Len() != len(cacheTestEntries()) {
			t.Errorf("loaded %d entries", cf.Len())
		}
		if ready, err := catalog.CacheReadyDir(dir, cacheTestTarget()); !ready || err != nil {
			t.Errorf("Ready = (%v, %v), want (true, nil)", ready, err)
		}
	})
}

func cacheTestAssertGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cache that failed its checks was not deleted: %v", err)
	}
}

func cacheTestRewriteMeta(t *testing.T, dir string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, catalog.CacheMetaName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Identity, freshness and Ready
// ---------------------------------------------------------------------------

func TestCacheReadyAndStaleness(t *testing.T) {
	root := t.TempDir()
	// os.UserCacheDir reads XDG_CACHE_HOME on Linux and LocalAppData on
	// Windows; setting both keeps this test hermetic on either.
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target := cacheTestTarget()
	if ready, err := catalog.CacheReady(target); ready || err != nil {
		t.Fatalf("Ready before any build = (%v, %v), want (false, nil)", ready, err)
	}
	if _, err := catalog.LoadCache(target); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Fatalf("Load before any build = %v, want ErrNotBuilt", err)
	}

	dir, err := catalog.CacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "debark-gui", "catalog", target.CacheKey()); dir != want {
		t.Fatalf("CacheDir = %q, want %q", dir, want)
	}

	built := target
	built.IndexDigest = "sha256:aaaa"
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target:            built,
		Entries:           cacheTestEntries(),
		IndexDigest:       built.IndexDigest,
		IndexDigestSource: catalog.CacheDigestRelease,
	}); err != nil {
		t.Fatal(err)
	}

	if ready, err := catalog.CacheReady(target); !ready || err != nil {
		t.Fatalf("Ready after a build = (%v, %v), want (true, nil)", ready, err)
	}

	// Digest unknown on the target: freshness is unknown, and unknown is not
	// stale. The picker opens before anything has talked to the archive.
	if cf, err := catalog.LoadCache(target); err != nil || cf == nil {
		t.Fatalf("Load with an unknown digest = (%v, %v), want a cache and no error", cf, err)
	}

	// Digests equal: fresh.
	same := target
	same.IndexDigest = "sha256:aaaa"
	if cf, err := catalog.LoadCache(same); err != nil || cf == nil {
		t.Fatalf("Load with a matching digest = (%v, %v)", cf, err)
	}

	// Digests differ: still loads, and says so. This is the case the UI turns
	// into "may be out of date" rather than an empty screen.
	moved := target
	moved.IndexDigest = "sha256:bbbb"
	cf, err := catalog.LoadCache(moved)
	if !errors.Is(err, catalog.ErrStale) {
		t.Fatalf("Load with a changed digest = %v, want ErrStale", err)
	}
	if cf == nil {
		t.Fatal("ErrStale came back without a usable catalogue; a stale cache must still load")
	}
	if cf.Len() != len(cacheTestEntries()) {
		t.Errorf("stale cache holds %d entries", cf.Len())
	}
	if e, ok := cf.Get("gimp"); !ok || e.AppName == "" {
		t.Error("a stale cache must serve rows exactly like a fresh one")
	}
	// Staleness must not touch the directory: the operator was offered a
	// refresh, not charged for one.
	if ready, err := catalog.CacheReady(moved); !ready || err != nil {
		t.Errorf("Ready after a stale load = (%v, %v), want (true, nil)", ready, err)
	}

	// A directory holding a cache for a different target is never served, even
	// though the key derives from the same text and this cannot normally
	// happen.
	cacheTestRewriteMeta(t, dir, func(m map[string]any) { m["identity_sha256"] = strings.Repeat("0", 64) })
	if _, err := catalog.LoadCache(target); !errors.Is(err, catalog.ErrNotBuilt) {
		t.Errorf("Load of a foreign identity = %v, want ErrNotBuilt", err)
	}
	if ready, err := catalog.CacheReady(target); ready || err != nil {
		t.Errorf("Ready of a foreign identity = (%v, %v), want (false, nil)", ready, err)
	}
}

func TestCacheReadyIsCheap(t *testing.T) {
	// Ready must answer from meta.json alone: it is on the path to opening the
	// picker, and reading 19 MB to answer "is there a cache?" would spend the
	// load budget twice.
	dir, _ := cacheTestSave(t, cacheTestEntries())
	if err := os.Remove(filepath.Join(dir, catalog.CacheBinName)); err != nil {
		t.Fatal(err)
	}
	// With the index gone the size check fails, which is the point: the answer
	// comes from a stat, not from a read.
	if ready, err := catalog.CacheReadyDir(dir, cacheTestTarget()); ready || err != nil {
		t.Fatalf("Ready = (%v, %v), want (false, nil)", ready, err)
	}
}

// ---------------------------------------------------------------------------
// Housekeeping
// ---------------------------------------------------------------------------

func TestCacheSweep(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-3 * time.Hour)

	mk := func(name string, builtAt time.Time, valid bool) string {
		dir := filepath.Join(root, name)
		target := cacheTestTarget()
		target.Codename = name // a different identity per directory
		if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
			Target: target, Entries: cacheTestEntries(), BuiltAt: builtAt,
		}); err != nil {
			t.Fatal(err)
		}
		if !valid {
			if err := os.Remove(filepath.Join(dir, catalog.CacheMetaName)); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	newest := mk("keep-1", time.Now(), true)
	mk("keep-2", time.Now().Add(-time.Hour), true)
	mk("keep-3", time.Now().Add(-2*time.Hour), true)
	oldest := mk("evict-me", time.Now().Add(-100*time.Hour), true)
	abandoned := mk("no-marker", time.Now(), false)

	// A stale temporary anywhere under the root, and a fresh one that a build
	// running right now could still own.
	staleTmp := filepath.Join(newest, catalog.CacheBinName+".tmp-00000000")
	if err := os.WriteFile(staleTmp, []byte("half a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleTmp, old, old); err != nil {
		t.Fatal(err)
	}
	freshTmp := filepath.Join(root, "catalog.bin.tmp-ffffffff")
	if err := os.WriteFile(freshTmp, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := catalog.SweepCache(root, 3); err != nil {
		t.Fatalf("SweepCache: %v", err)
	}

	for _, path := range []string{staleTmp, oldest, abandoned} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep", filepath.Base(path))
		}
	}
	for _, path := range []string{freshTmp, newest} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was swept and should not have been: %v", filepath.Base(path), err)
		}
	}
	if err := catalog.SweepCache(filepath.Join(root, "does-not-exist"), 4); err != nil {
		t.Errorf("sweeping a root that does not exist = %v, want nil", err)
	}
}

func TestCacheSweepSpareTheDirectoryJustWritten(t *testing.T) {
	// SaveCache sweeps after committing. A five-directory root must lose its
	// oldest, never the one that just finished building.
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		target := cacheTestTarget()
		target.Codename = fmt.Sprintf("release-%d", i)
		dir := filepath.Join(root, target.CacheKey())
		if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
			Target:  target,
			Entries: cacheTestEntries(),
			BuiltAt: time.Now().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.LoadCacheDir(dir); err != nil {
			t.Fatalf("the cache just written is not loadable: %v", err)
		}
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 4 {
		t.Errorf("root holds %d directories after six builds, want 4", len(ents))
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestCacheConcurrentReaders(t *testing.T) {
	// A *CacheFile is immutable after load, so any number of goroutines may
	// read it without a lock. Run under -race.
	dir, _ := cacheTestSave(t, cacheTestCorpus(t, 500))
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < cf.Len(); i++ {
				e, ok := cf.Entry(i)
				if !ok || e.Name == "" {
					t.Errorf("goroutine %d: Entry(%d) = (%v, %v)", g, i, e, ok)
					return
				}
				if j, ok := cf.Lookup(e.Name); !ok || j != i {
					t.Errorf("goroutine %d: Lookup(%q) = (%d, %v), want (%d, true)", g, e.Name, j, ok, i)
					return
				}
				_ = cf.Haystack(i)
				_ = cf.IsApp(i)
				_ = cf.Categories()
			}
		}(g)
	}
	wg.Wait()
	if cf.BadRefs() != 0 {
		t.Errorf("BadRefs = %d", cf.BadRefs())
	}
}

// TestCacheSaveKeepsTheCacheItCannotReplace is the deterministic half of the
// race TestCacheReaderRacingWriter below only samples.
//
// The product bug it pins: a rebuild replaces catalog.bin, then loses the
// sharing-violation retry on meta.json, and every window with the cache open
// reads ErrNotBuilt over a catalogue that was perfectly good — until some
// later build happens to win. A failed rebuild has to leave the operator with
// what they already had.
//
// It is Windows-only because the failure is: POSIX rename(2) replaces an open
// destination without complaint, so there is no way to make the second rename
// fail on Linux without an injection seam that would then be the only thing
// under test. Windows refuses a rename over a handle opened without
// FILE_SHARE_DELETE, which os.Open does not pass — so holding meta.json open,
// exactly as a second window reading the cache does, makes the failure certain
// rather than occasional.
func TestCacheSaveKeepsTheCacheItCannotReplace(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows can refuse a rename over an open destination; POSIX rename(2) always replaces")
	}
	dir := filepath.Join(t.TempDir(), "root", cacheTestTarget().CacheKey())
	first := cacheTestCorpus(t, 40)
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target: cacheTestTarget(), Entries: first,
	}); err != nil {
		t.Fatalf("the first save: %v", err)
	}

	held, err := os.Open(filepath.Join(dir, catalog.CacheMetaName))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			held.Close()
		}
	}()

	second := cacheTestCorpus(t, 200)
	if len(second) == len(first) {
		t.Fatal("the two corpora are the same size, so this test cannot tell them apart")
	}
	_, err = catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target: cacheTestTarget(), Entries: second,
	})
	if !errors.Is(err, catalog.ErrCacheNotReplaced) {
		t.Fatalf("err = %v, want ErrCacheNotReplaced", err)
	}

	held.Close()
	closed = true

	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatalf("the cache a failed rebuild left behind does not load: %v", err)
	}
	if cf.Len() != len(first) {
		t.Errorf("loaded %d entries, want the %d the previous build committed", cf.Len(), len(first))
	}
	if _, ok := cf.Entry(cf.Len() - 1); !ok {
		t.Error("the last row of the restored cache is missing")
	}
	if ready, err := catalog.CacheReadyDir(dir, cacheTestTarget()); !ready || err != nil {
		t.Errorf("Ready = (%v, %v), want (true, nil): a failed rebuild left the cache unusable", ready, err)
	}
}

func TestCacheReaderRacingWriter(t *testing.T) {
	// Two windows: one rebuilding, one browsing. The writer never modifies a
	// file in place, so a reader sees the whole old file or the whole new one
	// and never a partial write. Run under -race.
	//
	// The two rebuilds alternate between corpora of different sizes, which is
	// the part that makes this a test rather than a warm-up: writing the same
	// bytes every time means every marker agrees with every index, so a reader
	// that interleaved between the two renames could not tell. Every load here
	// must land on one whole generation or the other.
	dir := filepath.Join(t.TempDir(), "root", cacheTestTarget().CacheKey())
	generations := [2][]catalog.Entry{cacheTestCorpus(t, 300), cacheTestCorpus(t, 700)}
	sizes := [2]int{len(generations[0]), len(generations[1])}
	if sizes[0] == sizes[1] {
		t.Fatal("the two generations are the same size, so a torn read would look like a clean one")
	}
	save := func(gen int) error {
		_, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
			Target: cacheTestTarget(), Entries: generations[gen%2],
		})
		// A rename that cannot replace an open destination is the documented
		// Windows outcome, not a failure: the build stands, the previous cache
		// is left intact, and the cache is written next time.
		if errors.Is(err, catalog.ErrCacheNotReplaced) {
			return nil
		}
		return err
	}
	if err := save(0); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < 30; i++ {
			if err := save(i); err != nil {
				t.Errorf("save %d: %v", i, err)
				return
			}
		}
	}()

	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				cf, err := catalog.LoadCacheDir(dir)
				if errors.Is(err, catalog.ErrCacheBusy) {
					// The writer here never pauses -- thirty rebuilds
					// back to back against four readers -- so a reader can
					// spend its whole patience watching commits land and
					// give up still able to see one in flight. That is the
					// honest answer and it satisfies both halves of what
					// this test is for: no partial data was served, and
					// nothing was removed. It is NOT a pass for
					// ErrCacheCorrupt, which is checked below and is what
					// this path used to return.
					//
					// The distinction is the whole point. This test failed
					// intermittently in a loaded full-suite run and could not
					// be reproduced on demand afterwards -- about thirty
					// attempts, including under twelve spinning cores. The
					// mechanism is pinned deterministically instead, by
					// TestLoadDuringACommitIsBusyNotCorrupt.
					continue
				}
				if err != nil {
					// Nothing else is acceptable: a reader must never see a
					// partial write, and a rebuild that cannot commit must
					// leave the previous cache readable rather than removing
					// its commit marker.
					t.Errorf("load during a rebuild: %v", err)
					return
				}
				if cf.Len() != sizes[0] && cf.Len() != sizes[1] {
					t.Errorf("load during a rebuild saw %d entries, want %d or %d", cf.Len(), sizes[0], sizes[1])
					return
				}
				if cf.Meta().EntryCount != cf.Len() {
					t.Errorf("a marker saying %d entries was served with an index of %d", cf.Meta().EntryCount, cf.Len())
					return
				}
				if _, ok := cf.Entry(cf.Len() - 1); !ok {
					t.Error("the last row of a cache read during a rebuild is missing")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The index digest
// ---------------------------------------------------------------------------

func TestCacheIndexDigest(t *testing.T) {
	target := cacheTestTarget()
	release := func(hashes ...string) []byte {
		var b strings.Builder
		b.WriteString("Origin: Ubuntu\nSuite: noble\nMD5Sum:\n dead 1 main/binary-amd64/Packages.gz\nSHA256:\n")
		for i := 0; i < len(hashes); i += 2 {
			fmt.Fprintf(&b, " %s 12345 %s\n", hashes[i+1], hashes[i])
		}
		b.WriteString("Acquire-By-Hash: yes\n")
		return []byte(b.String())
	}
	key := func(suite string) string {
		return "http://archive.ubuntu.com/ubuntu dists/" + suite + "/Release"
	}
	releases := map[string][]byte{
		key("noble"): release(
			"main/binary-amd64/Packages.gz", "aa11",
			"main/dep11/Components-amd64.yml.gz", "bb22",
			"universe/binary-amd64/Packages.gz", "cc33",
		),
		key("noble-updates"): release("main/binary-amd64/Packages.gz", "dd44"),
	}

	first, ok := catalog.CacheIndexDigest(target, releases)
	if !ok {
		t.Fatal("CacheIndexDigest reported no usable Release hashes")
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("digest %q is not sha256:<64 hex>", first)
	}
	if again, _ := catalog.CacheIndexDigest(target, releases); again != first {
		t.Error("the digest is not deterministic")
	}

	// A component that gains DEP-11 must change the digest, which is why a
	// missing ref contributes "-" rather than being skipped.
	grew := map[string][]byte{
		key("noble"): release(
			"main/binary-amd64/Packages.gz", "aa11",
			"main/dep11/Components-amd64.yml.gz", "bb22",
			"universe/binary-amd64/Packages.gz", "cc33",
			"universe/dep11/Components-amd64.yml.gz", "ee55",
		),
		key("noble-updates"): release("main/binary-amd64/Packages.gz", "dd44"),
	}
	if d, _ := catalog.CacheIndexDigest(target, grew); d == first {
		t.Error("a component gaining DEP-11 did not change the digest")
	}

	// One index republished must change it too.
	republished := map[string][]byte{
		key("noble"):         releases[key("noble")],
		key("noble-updates"): release("main/binary-amd64/Packages.gz", "ffff"),
	}
	if d, _ := catalog.CacheIndexDigest(target, republished); d == first {
		t.Error("a republished index did not change the digest")
	}

	// An archive with no SHA256: section at all is the "content" fallback's
	// trigger, and must be reported rather than silently hashing nothing.
	if _, ok := catalog.CacheIndexDigest(target, map[string][]byte{
		key("noble"): []byte("Origin: Ubuntu\nMD5Sum:\n dead 1 main/binary-amd64/Packages.gz\n"),
	}); ok {
		t.Error("a Release with no SHA256 section reported a usable digest")
	}

	if h := catalog.CacheReleaseHashes(releases[key("noble")]); h["main/binary-amd64/Packages.gz"] != "aa11" {
		t.Errorf("CacheReleaseHashes: %v", h)
	}
	if h := catalog.CacheReleaseHashes(releases[key("noble")]); h["main/binary-amd64/Packages.gz"] == "dead" {
		t.Error("CacheReleaseHashes returned an MD5Sum entry")
	}
	if h := catalog.CacheReleaseHashes(nil); len(h) != 0 {
		t.Errorf("CacheReleaseHashes(nil) = %v", h)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestCacheSaveCancelled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root", "key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := catalog.SaveCache(ctx, dir, catalog.CacheInput{Target: cacheTestTarget(), Entries: cacheTestEntries()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("a cancelled save created a directory")
	}
}

func TestCacheSavePreservesThePreviousCacheOnFailure(t *testing.T) {
	// "Build returning an error must never leave a directory in a worse state
	// than it found it."
	dir, _ := cacheTestSave(t, cacheTestEntries())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.SaveCache(ctx, dir, catalog.CacheInput{Target: cacheTestTarget()}); err == nil {
		t.Fatal("a cancelled save reported success")
	}
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		t.Fatalf("the previous cache did not survive a failed save: %v", err)
	}
	if cf.Len() != len(cacheTestEntries()) {
		t.Errorf("the previous cache holds %d entries", cf.Len())
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a failed save left %s behind", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Budgets: measured, not asserted
// ---------------------------------------------------------------------------

// cacheTestCorpus builds n entries with internal/catalog/fake, which is the
// only corpus generator in the tree — a hand-written one would model whatever
// the format happens to be good at.
func cacheTestCorpus(tb testing.TB, n int) []catalog.Entry {
	tb.Helper()
	f := fake.New(n)
	total := f.Len()
	out := make([]catalog.Entry, 0, total)
	for off := 0; off < total; off += catalog.MaxPageSize {
		p, err := f.Search(context.Background(), catalog.Query{Offset: off, Limit: catalog.MaxPageSize})
		if err != nil {
			tb.Fatalf("fake.Search: %v", err)
		}
		if len(p.Entries) == 0 {
			break
		}
		out = append(out, p.Entries...)
	}
	if len(out) != total {
		tb.Fatalf("collected %d of %d fake entries", len(out), total)
	}
	return out
}

// TestCacheLoadBudget measures the load against the brief's < 500 ms budget
// and records the number. It deliberately does not assert a time — the project
// requires budgets measured and recorded, not asserted, and a wall-clock
// assertion on shared CI hardware is a flake generator. What it does assert is
// that the file is intact and complete at full size.
//
// Measured, 70,000 entries from internal/catalog/fake, on an Intel Core Ultra 9
// 285K running Windows 11 with Go 1.26 (go test -bench, -benchtime 5x):
//
//	catalog.bin                             11.8 MB
//	load, ReadFile + CRC + validation        6.3 ms   1.3% of the 500 ms budget
//	validation alone, buffer resident        1.5 ms   8.3 GB/s
//	save, build + write + fsync + rename    64.1 ms
//	materialise one 500-row page            32.8 us
//	Get by name, binary search               1.8 us
//
// The file is smaller than the ~19 MB §4.2 predicts because the synthetic
// corpus writes shorter summaries than the archive does; the record table is
// exactly the predicted 5.04 MB and the text arena is what differs, so a real
// main+universe catalogue should land near the document's estimate. The load
// figure is dominated by reading 11.8 MB and would grow with it: at the
// predicted 19 MB the same measurement extrapolates to about 10 ms, still a
// fiftyfold margin.
func TestCacheLoadBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("70,000 entries; -short")
	}
	entries := cacheTestCorpus(t, 70000)
	dir, _ := cacheTestSave(t, entries)

	start := time.Now()
	cf, err := catalog.LoadCacheDir(dir)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("LoadCacheDir: %v", err)
	}
	if cf.Len() != len(entries) {
		t.Fatalf("loaded %d of %d entries", cf.Len(), len(entries))
	}
	t.Logf("%d entries: %s on disk, loaded in %s (budget 500 ms, %.1f%% used) on %s/%s",
		cf.Len(), cacheTestBytes(cf.Size()), elapsed.Round(100*time.Microsecond),
		float64(elapsed)/float64(500*time.Millisecond)*100, runtime.GOOS, runtime.GOARCH)

	// Spot-check both ends and the middle: a load that is fast because it
	// skipped something is not a load.
	for _, i := range []int{0, cf.Len() / 2, cf.Len() - 1} {
		e, ok := cf.Entry(i)
		if !ok || e.Name == "" {
			t.Fatalf("Entry(%d) = (%#v, %v)", i, e, ok)
		}
		if j, ok := cf.Lookup(e.Name); !ok || j != i {
			t.Fatalf("Lookup(%q) = (%d, %v), want (%d, true)", e.Name, j, ok, i)
		}
	}
	if cf.BadRefs() != 0 {
		t.Errorf("BadRefs = %d", cf.BadRefs())
	}
}

func cacheTestBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func BenchmarkCacheLoad(b *testing.B) {
	entries := cacheTestCorpus(b, 70000)
	dir, raw := cacheTestSave(b, entries)
	b.Logf("%d entries, %s on disk", len(entries), cacheTestBytes(len(raw)))
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cf, err := catalog.LoadCacheDir(dir)
		if err != nil {
			b.Fatal(err)
		}
		if cf.Len() != len(entries) {
			b.Fatalf("loaded %d entries", cf.Len())
		}
	}
}

// BenchmarkCacheParse isolates the part of the load that is not I/O: the CRC
// and the bounds checks over an already-resident buffer. It is the number that
// says whether the format, rather than the disk, is the cost.
func BenchmarkCacheParse(b *testing.B) {
	entries := cacheTestCorpus(b, 70000)
	_, raw := cacheTestSave(b, entries)
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := catalog.CacheFromBytes(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheSave(b *testing.B) {
	entries := cacheTestCorpus(b, 70000)
	dir := filepath.Join(b.TempDir(), "root", cacheTestTarget().CacheKey())
	in := catalog.CacheInput{Target: cacheTestTarget(), Entries: entries}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := catalog.SaveCache(context.Background(), dir, in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCacheEntryPage is what a keystroke actually pays: materialising one
// page of rows out of a loaded cache.
func BenchmarkCacheEntryPage(b *testing.B) {
	entries := cacheTestCorpus(b, 70000)
	dir, _ := cacheTestSave(b, entries)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := (i * catalog.MaxPageSize) % (cf.Len() - catalog.MaxPageSize)
		for j := 0; j < catalog.MaxPageSize; j++ {
			if _, ok := cf.Entry(start + j); !ok {
				b.Fatalf("Entry(%d) failed", start+j)
			}
		}
	}
}

func BenchmarkCacheLookup(b *testing.B) {
	entries := cacheTestCorpus(b, 70000)
	dir, _ := cacheTestSave(b, entries)
	cf, err := catalog.LoadCacheDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	names := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		names = append(names, entries[i*(len(entries)/64)].Name)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := cf.Lookup(names[i%len(names)]); !ok {
			b.Fatal("Lookup failed")
		}
	}
}

// TestReadCacheMetaRefusesNegativeCounts is docs/security-review.md §4.4,
// found by FuzzSecCacheMeta's sixth seed. Nothing downstream was harmed —
// cacheLoadIndex compares EntryCount with the file's real length and
// CacheReadyDir compares BinSize with its real size, and a negative matches
// neither — but a CacheMeta that contradicts its own field comments is a
// value the next caller has to remember to distrust, and the next caller is
// the one that writes make([]X, meta.EntryCount).
func TestReadCacheMetaRefusesNegativeCounts(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"both negative", `{"format_version":%d,"entry_count":-1,"bin_size":-1}`},
		{"entry_count alone", `{"format_version":%d,"entry_count":-1,"bin_size":0}`},
		{"bin_size alone", `{"format_version":%d,"entry_count":0,"bin_size":-4096}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := fmt.Sprintf(tc.doc, catalog.CacheFormatVersion)
			if err := os.WriteFile(filepath.Join(dir, catalog.CacheMetaName), []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			meta, err := catalog.ReadCacheMeta(dir)
			if err == nil {
				t.Fatalf("accepted %s, returning %+v", doc, meta)
			}
			if !errors.Is(err, catalog.ErrCacheCorrupt) {
				t.Fatalf("error %v is not ErrCacheCorrupt, so a caller cannot tell it from an unreadable file", err)
			}
			// The refusal must not leak a half-filled struct: the value is
			// what a careless caller would use.
			if meta.EntryCount != 0 || meta.BinSize != 0 {
				t.Errorf("returned %+v alongside the error", meta)
			}
		})
	}
}

// TestReadCacheMetaAcceptsZero: zero is a legal count. An architecture with no
// packages in a component is a shorter catalogue, not a corrupt one.
func TestReadCacheMetaAcceptsZero(t *testing.T) {
	dir := t.TempDir()
	doc := fmt.Sprintf(`{"format_version":%d,"entry_count":0,"bin_size":0}`, catalog.CacheFormatVersion)
	if err := os.WriteFile(filepath.Join(dir, catalog.CacheMetaName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ReadCacheMeta(dir); err != nil {
		t.Fatalf("rejected a zero-count meta.json: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A commit in flight is not damage
// ---------------------------------------------------------------------------

// cacheMidCommit builds the exact state a reader sees between the commit's two
// renames: catalog.bin from one generation, meta.json from another, and — when
// inFlight — the writer's temporary still sitting beside them.
//
// Built by hand rather than by racing a writer, because the race is what made
// TestCacheReaderRacingWriter an intermittent failure nobody could reproduce on
// demand. The state it lands in is small and completely describable, so it is
// described.
func cacheMidCommit(t *testing.T, inFlight bool) (dir string, target catalog.Target) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("LocalAppData", root)

	target = cacheTestTarget()
	dir = filepath.Join(root, "staged", target.CacheKey())

	// Generation A, committed whole.
	if _, err := catalog.SaveCache(context.Background(), dir, catalog.CacheInput{
		Target: target, Entries: cacheTestCorpus(t, 300),
	}); err != nil {
		t.Fatalf("SaveCache(A): %v", err)
	}
	metaA, err := os.ReadFile(filepath.Join(dir, catalog.CacheMetaName))
	if err != nil {
		t.Fatal(err)
	}

	// Generation B, committed whole, in a directory of its own.
	other := filepath.Join(root, "genB")
	if _, err := catalog.SaveCache(context.Background(), other, catalog.CacheInput{
		Target: target, Entries: cacheTestCorpus(t, 700),
	}); err != nil {
		t.Fatalf("SaveCache(B): %v", err)
	}
	binB, err := os.ReadFile(filepath.Join(other, catalog.CacheBinName))
	if err != nil {
		t.Fatal(err)
	}

	// B's index beside A's marker: phase one of the commit has landed and
	// phase two has not. This is the only disagreement the protocol can
	// produce, and it is the one ErrCacheCorrupt describes.
	if err := os.WriteFile(filepath.Join(dir, catalog.CacheBinName), binB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, catalog.CacheMetaName), metaA, 0o644); err != nil {
		t.Fatal(err)
	}
	if inFlight {
		// What a writer between its two renames has left on disk: meta.json's
		// temporary, not yet renamed into place.
		tmp := filepath.Join(dir, catalog.CacheMetaName+".tmp-0123456789abcdef")
		if err := os.WriteFile(tmp, metaA, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, target
}

// TestLoadDuringACommitIsBusyNotCorrupt is the misclassification, pinned.
//
// A cache is committed by two renames — catalog.bin, then meta.json — so a
// reader that lands between them sees a marker and an index that disagree.
// That is precisely what ErrCacheCorrupt describes, and it was what the reader
// returned: a second window browsing while the first rebuilt could be told its
// cache was damaged and offered a rebuild it did not need, over a file that was
// about to be perfectly good.
//
// LoadCacheDir already knew enough not to delete such a directory. It just
// reported the wrong thing about it.
func TestLoadDuringACommitIsBusyNotCorrupt(t *testing.T) {
	dir, _ := cacheMidCommit(t, true)

	start := time.Now()
	cf, err := catalog.LoadCacheDir(dir)
	elapsed := time.Since(start)

	if cf != nil {
		t.Error("a half-committed directory must not be served as a catalogue")
	}
	if !errors.Is(err, catalog.ErrCacheBusy) {
		t.Fatalf("LoadCacheDir = %v, want ErrCacheBusy", err)
	}
	if errors.Is(err, catalog.ErrCacheCorrupt) {
		t.Error("a commit in flight is still being reported as corruption")
	}
	// It waited before giving up rather than failing on the first look: the
	// ordinary case is that the second rename lands and the retry succeeds.
	if elapsed < 500*time.Millisecond {
		t.Errorf("gave up after %s without waiting for the writer", elapsed)
	}

	// And it left everything alone. A reader must never remove a cache another
	// process is in the middle of committing.
	for _, name := range []string{catalog.CacheBinName, catalog.CacheMetaName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed from a directory being written: %v", name, err)
		}
	}
}

// TestLoadOfAQuietMismatchIsStillCorrupt is the other half, and it is what
// keeps the change above from weakening the invalidation rule.
//
// §6.4 is about a directory nothing is writing. The same disagreement with no
// commit in flight is real damage — a crash between the two renames, or a
// hand-edited directory — and it must still read as ErrCacheCorrupt and still
// be removed so the next build starts clean.
func TestLoadOfAQuietMismatchIsStillCorrupt(t *testing.T) {
	dir, _ := cacheMidCommit(t, false)

	cf, err := catalog.LoadCacheDir(dir)
	if cf != nil {
		t.Error("a mismatched directory must not be served as a catalogue")
	}
	if !errors.Is(err, catalog.ErrCacheCorrupt) {
		t.Fatalf("LoadCacheDir = %v, want ErrCacheCorrupt", err)
	}
	if errors.Is(err, catalog.ErrCacheBusy) {
		t.Error("a quiet mismatch is damage, not a commit in flight")
	}
	if _, err := os.Stat(filepath.Join(dir, catalog.CacheMetaName)); !os.IsNotExist(err) {
		t.Errorf("the damaged directory's commit marker survived: %v", err)
	}
}
