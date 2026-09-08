package catalog

// packages_test.go — tests for the Packages fetch and parse (packages.go).
//
// Every fixture used here is a byte-exact slice of a real archive index
// (testdata/README.md). Nothing is hand-tidied, because the traps these tests
// exist to catch — a 70,830-byte field value, a folded Tag: whose values
// contain "::", a package name that appears twice — are exactly what a tidied
// fixture would have removed.
//
// No test here touches the network: PackagesLoader.HTTPClient is injected and
// every server is an httptest server serving those fixtures.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	pkgFixtureUbuntuMain     = "ubuntu-noble-main-Packages.excerpt"
	pkgFixtureUbuntuUniverse = "ubuntu-noble-universe-Packages.excerpt"
	pkgFixtureDebianMain     = "debian-bookworm-main-Packages.excerpt"
)

// pkgFixture reads one committed excerpt. The fixtures live at the repository
// root rather than under internal/catalog, because four packages share them.
func pkgFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

func pkgRef(suite, component string) IndexRef {
	return IndexRef{
		URI:       "http://archive.ubuntu.com/ubuntu",
		Suite:     suite,
		Component: component,
		Arch:      "amd64",
		Kind:      IndexPackages,
	}
}

// pkgParseAll parses a fixture and returns the entries in emission order.
func pkgParseAll(t testing.TB, data []byte, ref IndexRef) ([]Entry, PackagesParseStats) {
	t.Helper()
	var got []Entry
	stats, err := ParsePackagesIndex(context.Background(), bytes.NewReader(data), ref, func(e Entry) {
		got = append(got, e)
	})
	if err != nil {
		t.Fatalf("ParsePackagesIndex: %v", err)
	}
	return got, stats
}

func pkgFind(entries []Entry, name string) (Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// ---------------------------------------------------------------------------
// The real fixtures
// ---------------------------------------------------------------------------

func TestPackagesParseRealFixtures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		file string
		ref  IndexRef
		// stanzas is the count committed in testdata/README.md.
		stanzas int64
		// emitted differs from stanzas only if a stanza has no Package:.
		emitted int64
	}{
		{"ubuntu main", pkgFixtureUbuntuMain, pkgRef("noble", "main"), 31, 31},
		{"ubuntu universe", pkgFixtureUbuntuUniverse, pkgRef("noble", "universe"), 52, 52},
		{"debian main", pkgFixtureDebianMain, IndexRef{URI: "http://deb.debian.org/debian", Suite: "bookworm", Component: "main", Arch: "amd64", Kind: IndexPackages}, 33, 33},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, stats := pkgParseAll(t, pkgFixture(t, tc.file), tc.ref)

			if stats.Stanzas != tc.stanzas {
				t.Errorf("stanzas = %d, want %d", stats.Stanzas, tc.stanzas)
			}
			if stats.Emitted != tc.emitted {
				t.Errorf("emitted = %d, want %d", stats.Emitted, tc.emitted)
			}
			if int64(len(entries)) != tc.emitted {
				t.Errorf("entries = %d, want %d", len(entries), tc.emitted)
			}
			if stats.MalformedLines != 0 {
				t.Errorf("malformed lines = %d, want 0 on a real index", stats.MalformedLines)
			}

			for _, e := range entries {
				if e.Name == "" {
					t.Fatalf("emitted an entry with no name")
				}
				if e.Version == "" {
					t.Errorf("%s: no version; Version: is on 100%% of real stanzas", e.Name)
				}
				if e.Summary == "" {
					// index-formats.md §3: Description: is on 100.00% of
					// stanzas in both archives and IS the one-line summary.
					t.Errorf("%s: no summary", e.Name)
				}
				if e.Suite != tc.ref.Suite || e.Component != tc.ref.Component {
					t.Errorf("%s: suite/component = %q/%q, want %q/%q", e.Name, e.Suite, e.Component, tc.ref.Suite, tc.ref.Component)
				}
				if e.Arch != "amd64" && e.Arch != "all" {
					t.Errorf("%s: arch = %q, want amd64 or all", e.Name, e.Arch)
				}
				if strings.Contains(e.Section, "/") {
					t.Errorf("%s: section %q still carries a component prefix", e.Name, e.Section)
				}
			}
		})
	}
}

func TestPackagesParseFieldValues(t *testing.T) {
	t.Parallel()

	entries, _ := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuUniverse), pkgRef("noble", "universe"))
	e, ok := pkgFind(entries, "0ad-data")
	if !ok {
		t.Fatal("0ad-data not found")
	}

	want := Entry{
		Name:              "0ad-data",
		Version:           "0.0.26-1",
		Suite:             "noble",
		Component:         "universe",
		Arch:              "all",
		Section:           "games",
		Priority:          "optional",
		Summary:           "Real-time strategy game of ancient warfare (data files)",
		Homepage:          "https://play0ad.com/",
		InstalledSizeKiB:  3218736,
		DownloadSizeBytes: 1377404936,
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("0ad-data =\n %+v\nwant\n %+v", e, want)
	}
}

// ---------------------------------------------------------------------------
// §4.2 — the 70,830-byte field value
// ---------------------------------------------------------------------------

// TestPackagesOverlongFieldTrapIsReal proves the fixture actually contains the
// trap, by failing a default bufio.Scanner on it. If this test ever stops
// failing the default scanner, the fixture was re-cut and
// TestPackagesOverlongFieldParses below has stopped testing anything.
func TestPackagesOverlongFieldTrapIsReal(t *testing.T) {
	t.Parallel()

	data := pkgFixture(t, pkgFixtureUbuntuUniverse)
	sc := bufio.NewScanner(bytes.NewReader(data)) // default 64 KiB token limit
	for sc.Scan() {                               //nolint:revive // draining is the point
	}
	if err := sc.Err(); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("default bufio.Scanner over the universe fixture: err = %v, want bufio.ErrTooLong.\n"+
			"The fixture is supposed to carry librust-winapi-dev's 70,830-byte Provides: (index-formats.md §4.2).", err)
	}
}

// TestPackagesOverlongFieldParses is the same data through this parser, which
// sizes its buffer for it.
func TestPackagesOverlongFieldParses(t *testing.T) {
	t.Parallel()

	entries, stats := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuUniverse), pkgRef("noble", "universe"))
	if stats.Emitted != 52 {
		t.Fatalf("emitted = %d, want 52 — a token-length failure truncates the index silently", stats.Emitted)
	}
	e, ok := pkgFind(entries, "librust-winapi-dev")
	if !ok {
		t.Fatal("librust-winapi-dev not found: the stanza carrying the 70,830-byte value was not parsed")
	}
	if e.Version == "" || e.Summary == "" {
		t.Errorf("librust-winapi-dev: version %q summary %q — the stanza after the long value was not read", e.Version, e.Summary)
	}
	// The stanza that follows it in the file must be intact too: a scanner
	// that recovered badly would lose the next row, not this one.
	if _, ok := pkgFind(entries, "librust-dirs-next-dev"); !ok {
		t.Error("librust-dirs-next-dev not found: parsing did not recover after the long value")
	}
}

// TestPackagesOverlongSyntheticValue pins the actual limit rather than relying
// on the fixture's particular size.
func TestPackagesOverlongSyntheticValue(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("libfoo-dev (= 1.0-1), ", 40000) // ~880 KB on one line
	src := "Package: huge\nVersion: 1.0\nArchitecture: amd64\nSection: libs\nProvides: " + long + "\nDescription: a package with an enormous Provides\n\n"

	entries, stats := pkgParseAll(t, []byte(src), pkgRef("noble", "universe"))
	if stats.Emitted != 1 || len(entries) != 1 {
		t.Fatalf("emitted = %d, want 1", stats.Emitted)
	}
	if entries[0].Summary != "a package with an enormous Provides" {
		t.Errorf("summary = %q — the field after the long value was lost", entries[0].Summary)
	}
}

// ---------------------------------------------------------------------------
// §4.1 — folded fields
// ---------------------------------------------------------------------------

// TestPackagesFoldedSkippedField is the Debian Tag: case. Its continuation
// lines contain "::", so a parser that split every line on the first colon
// without checking for a leading space would invent fields named "uitoolkit"
// and "use", and — worse — would keep reading them as part of the stanza.
func TestPackagesFoldedSkippedField(t *testing.T) {
	t.Parallel()

	ref := IndexRef{URI: "http://deb.debian.org/debian", Suite: "bookworm", Component: "main", Arch: "amd64", Kind: IndexPackages}
	entries, stats := pkgParseAll(t, pkgFixture(t, pkgFixtureDebianMain), ref)

	if stats.MalformedLines != 0 {
		t.Errorf("malformed lines = %d, want 0", stats.MalformedLines)
	}
	e, ok := pkgFind(entries, "0ad")
	if !ok {
		t.Fatal("0ad not found")
	}
	// 0ad's Tag: is folded across three lines, immediately before Section:.
	if e.Section != "games" {
		t.Errorf("0ad section = %q, want \"games\" — a continuation line was read as a field", e.Section)
	}
	if strings.Contains(e.Summary, "uitoolkit") || strings.Contains(e.Priority, "::") {
		t.Errorf("0ad: folded Tag: leaked into %+v", e)
	}
}

// TestPackagesFoldedEmptyFirstLine is the Ubuntu X-Cargo-Built-Using shape:
// the field's own line has no value at all and everything is on the
// continuation.
func TestPackagesFoldedEmptyFirstLine(t *testing.T) {
	t.Parallel()

	entries, _ := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuUniverse), pkgRef("noble", "universe"))
	e, ok := pkgFind(entries, "btm")
	if !ok {
		t.Fatal("btm not found")
	}
	if e.Summary == "" {
		t.Error("btm: the field after the folded X-Cargo-Built-Using was not read")
	}
	if e.Section != "utils" {
		t.Errorf("btm section = %q, want \"utils\" (its stanza says universe/utils)", e.Section)
	}
}

// TestPackagesFoldedKeptField covers the general case the corpus does not
// exercise: a field this parser keeps, folded. Nothing in Ubuntu or Debian
// folds one today; a derivative might.
func TestPackagesFoldedKeptField(t *testing.T) {
	t.Parallel()

	src := "Package: folded\nVersion: 1.0\nArchitecture: amd64\nSection: main/net\n" +
		"Homepage: https://example.invalid/one\n https://example.invalid/two\n" +
		"Description: a package whose homepage is folded\n\n"

	entries, _ := pkgParseAll(t, []byte(src), pkgRef("noble", "main"))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if got, want := entries[0].Homepage, "https://example.invalid/one\nhttps://example.invalid/two"; got != want {
		t.Errorf("homepage = %q, want %q", got, want)
	}
	if entries[0].Summary != "a package whose homepage is folded" {
		t.Errorf("summary = %q — the field after the fold was lost", entries[0].Summary)
	}
}

// A long description is detail metadata; it must not turn the list summary
// into a paragraph or discard the body supplied by an index.
func TestPackagesFoldedDescriptionKeepsOnlySummary(t *testing.T) {
	t.Parallel()

	src := "Package: verbose\nVersion: 1.0\nArchitecture: amd64\nSection: doc\n" +
		"Description: the one-line summary\n a long body line\n .\n another paragraph\n\n"

	entries, _ := pkgParseAll(t, []byte(src), pkgRef("noble", "main"))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if got := entries[0].Summary; got != "the one-line summary" {
		t.Errorf("summary = %q, want just the first line", got)
	}
	if got, want := entries[0].Description, "the one-line summary\na long body line\n\nanother paragraph"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// §4.3 — UTF-8
// ---------------------------------------------------------------------------

func TestPackagesUTF8(t *testing.T) {
	t.Parallel()

	entries, _ := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuUniverse), pkgRef("noble", "universe"))
	e, ok := pkgFind(entries, "adwaita-qt")
	if !ok {
		t.Fatal("adwaita-qt not found")
	}
	if got, want := e.Summary, "Qt 5 port of GNOME’s Adwaita theme"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, e := range entries {
		if !utf8.ValidString(e.Summary) || !utf8.ValidString(e.Name) {
			t.Errorf("%s: invalid UTF-8 survived the parse", e.Name)
		}
	}

	// The em dash in agda-stdlib-doc's summary is the multi-byte rune that a
	// byte-at-a-time truncation would split.
	if e, ok := pkgFind(entries, "agda-stdlib-doc"); ok {
		if !strings.Contains(e.Summary, "—") {
			t.Errorf("agda-stdlib-doc summary = %q, want an em dash", e.Summary)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.5 — component-prefixed Section:
// ---------------------------------------------------------------------------

func TestPackagesSectionPrefixStripped(t *testing.T) {
	t.Parallel()

	universe, _ := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuUniverse), pkgRef("noble", "universe"))
	main, _ := pkgParseAll(t, pkgFixture(t, pkgFixtureUbuntuMain), pkgRef("noble", "main"))

	// Every universe stanza in the fixture is "universe/<section>"; every
	// main stanza is bare. After stripping, the two must agree on the name of
	// a section they share, or the sidebar shows it twice.
	uniSections := map[string]bool{}
	for _, e := range universe {
		if strings.Contains(e.Section, "/") {
			t.Fatalf("%s: section %q not stripped", e.Name, e.Section)
		}
		uniSections[e.Section] = true
	}
	if !uniSections["games"] {
		t.Error("universe fixture produced no \"games\" section; 0ad-data is Section: universe/games")
	}
	for _, e := range main {
		if strings.Contains(e.Section, "/") {
			t.Fatalf("%s: main section %q should have been bare already", e.Name, e.Section)
		}
	}
	if e, ok := pkgFind(main, "accountsservice"); ok && e.Section != "gnome" {
		t.Errorf("accountsservice section = %q, want \"gnome\"", e.Section)
	}
}

func TestPkgStripComponent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		section, component, want string
	}{
		{"universe/games", "universe", "games"},
		{"admin", "main", "admin"},
		{"main/debian-installer", "main", "debian-installer"},
		// A prefix that does not match the ref's component is still a
		// component prefix as far as grouping is concerned.
		{"non-free/libs", "contrib", "libs"},
		{"", "universe", ""},
		{"libs", "", "libs"},
	}
	for _, c := range cases {
		if got := pkgStripComponent(c.section, c.component); got != c.want {
			t.Errorf("pkgStripComponent(%q, %q) = %q, want %q", c.section, c.component, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.4 — duplicate Package: names, without comparing versions
// ---------------------------------------------------------------------------

// TestPackagesDuplicateNamesLastWins uses the two real duplicates in the
// fixtures. In both, the later stanza happens to carry the higher version —
// which is a coincidence of how these files are written and is why the next
// test exists.
func TestPackagesDuplicateNamesLastWins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, file, pkg, wantVersion string
		ref                          IndexRef
	}{
		{
			name: "ubuntu universe", file: pkgFixtureUbuntuUniverse, ref: pkgRef("noble", "universe"),
			pkg: "android-platform-frameworks-native-headers", wantVersion: "1:34.0.4-1build3",
		},
		{
			name: "debian main", file: pkgFixtureDebianMain,
			ref: IndexRef{URI: "http://deb.debian.org/debian", Suite: "bookworm", Component: "main", Arch: "amd64", Kind: IndexPackages},
			pkg: "linux-doc", wantVersion: "6.1.176-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newPackagesMerger(0)
			stats, err := ParsePackagesIndex(context.Background(), bytes.NewReader(pkgFixture(t, tc.file)), tc.ref, m.add)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if m.duplicates == 0 {
				t.Fatalf("no duplicate names seen in %s; the fixture is supposed to carry one", tc.file)
			}
			if int64(len(m.entries)) != stats.Emitted-m.duplicates {
				t.Errorf("entries %d, emitted %d, duplicates %d — merge lost a row", len(m.entries), stats.Emitted, m.duplicates)
			}
			e, ok := pkgFind(m.entries, tc.pkg)
			if !ok {
				t.Fatalf("%s not found", tc.pkg)
			}
			if e.Version != tc.wantVersion {
				t.Errorf("%s version = %q, want %q (the LAST stanza in the file)", tc.pkg, e.Version, tc.wantVersion)
			}
		})
	}
}

// TestPackagesDuplicateRuleIsPositionalNotNewest is the rule-1 test.
//
// The two stanzas here are the real Debian linux-doc pair with their order
// reversed, so the LAST stanza carries the LOWER version. A parser that picked
// "the newest" — that is, one that compared two version strings, which
// contract-brief rule 1 forbids — would return 6.1.176-1. The documented rule
// is positional, so the answer must be 6.1.170-3.
func TestPackagesDuplicateRuleIsPositionalNotNewest(t *testing.T) {
	t.Parallel()

	src := "Package: linux-doc\nVersion: 6.1.176-1\nArchitecture: all\nSection: doc\nDescription: Linux kernel specific documentation\n\n" +
		"Package: linux-doc\nVersion: 6.1.170-3\nArchitecture: all\nSection: doc\nDescription: Linux kernel specific documentation\n\n"

	m := newPackagesMerger(0)
	if _, err := ParsePackagesIndex(context.Background(), strings.NewReader(src), pkgRef("bookworm", "main"), m.add); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.entries))
	}
	if got := m.entries[0].Version; got != "6.1.170-3" {
		t.Errorf("version = %q, want %q — the LAST stanza wins by position. "+
			"Getting %q here would mean something compared two version strings, which contract-brief rule 1 forbids.",
			got, "6.1.170-3", "6.1.176-1")
	}
	if m.duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", m.duplicates)
	}
}

// TestPackagesDuplicateKeepsFirstSeenPosition: a duplicate replaces a row's
// contents, never its place, so the catalogue does not reshuffle.
func TestPackagesDuplicateKeepsFirstSeenPosition(t *testing.T) {
	t.Parallel()

	src := "Package: aaa\nVersion: 1\nDescription: a\n\n" +
		"Package: bbb\nVersion: 1\nDescription: b\n\n" +
		"Package: aaa\nVersion: 2\nDescription: a again\n\n"

	m := newPackagesMerger(0)
	if _, err := ParsePackagesIndex(context.Background(), strings.NewReader(src), pkgRef("noble", "main"), m.add); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.entries) != 2 || m.entries[0].Name != "aaa" || m.entries[1].Name != "bbb" {
		t.Fatalf("entries = %+v, want [aaa bbb]", m.entries)
	}
	if m.entries[0].Version != "2" {
		t.Errorf("aaa version = %q, want %q", m.entries[0].Version, "2")
	}
}

// TestPackagesSourceHasNoVersionComparison is a source-level guard. The
// behavioural test above proves the rule; this one catches a future edit that
// reaches for a version comparator to "improve" it.
func TestPackagesSourceHasNoVersionComparison(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("packages.go")
	if err != nil {
		t.Fatalf("reading packages.go: %v", err)
	}
	banned := []string{
		"debian/version",  // pault.ag's version comparator
		"version.Compare", // ...and its call
		"CompareVersion",
		"e.Version <", "e.Version >",
		".Version <", ".Version >",
	}
	for _, b := range banned {
		if bytes.Contains(src, []byte(b)) {
			t.Errorf("packages.go contains %q — comparing versions is engine work, forbidden by contract-brief rule 1", b)
		}
	}
}

// ---------------------------------------------------------------------------
// Missing and malformed input
// ---------------------------------------------------------------------------

func TestPackagesOptionalFieldsAbsent(t *testing.T) {
	t.Parallel()

	// index-formats.md §4.6: Installed-Size is absent on 0.20% of real
	// stanzas and must read as unknown, not as 0 B. Homepage is absent on
	// 8.3%. Neither absence may drop the row.
	src := "Package: minimal\nVersion: 1.0-1\nArchitecture: all\nDescription: a stanza with almost nothing in it\n\n"

	entries, stats := pkgParseAll(t, []byte(src), pkgRef("noble", "main"))
	if stats.Emitted != 1 {
		t.Fatalf("emitted = %d, want 1", stats.Emitted)
	}
	e := entries[0]
	if e.InstalledSizeKiB != 0 || e.DownloadSizeBytes != 0 {
		t.Errorf("sizes = %d/%d, want 0/0 for absent fields", e.InstalledSizeKiB, e.DownloadSizeBytes)
	}
	if e.Homepage != "" || e.Section != "" || e.Priority != "" {
		t.Errorf("absent optional fields materialised: %+v", e)
	}
	if e.Name != "minimal" || e.Version != "1.0-1" || e.Arch != "all" {
		t.Errorf("entry = %+v", e)
	}
	if e.Suite != "noble" || e.Component != "main" {
		t.Errorf("suite/component not taken from the ref: %+v", e)
	}
}

func TestPackagesStanzaWithoutName(t *testing.T) {
	t.Parallel()

	src := "Version: 1.0\nDescription: a stanza with no Package field\n\n" +
		"Package: real\nVersion: 2.0\nDescription: a real one\n\n"

	entries, stats := pkgParseAll(t, []byte(src), pkgRef("noble", "main"))
	if stats.Stanzas != 2 || stats.Emitted != 1 || stats.SkippedStanzas != 1 {
		t.Errorf("stats = %+v, want 2 stanzas / 1 emitted / 1 skipped", stats)
	}
	if len(entries) != 1 || entries[0].Name != "real" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestPackagesMalformedAndTruncated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		src            string
		wantEmitted    int64
		wantMalformed  int64
		wantLastFields func(t *testing.T, e Entry)
	}{
		{
			// A file cut mid-stanza: the last stanza has no terminating
			// blank line and no Description. It must still be emitted (§2:
			// "the last stanza may or may not be followed by one").
			name: "truncated mid-file",
			src:  "Package: one\nVersion: 1\nDescription: first\n\nPackage: two\nVersion: 2\nArchitectu",
			// The cut line has no colon, so a truncated file announces
			// itself in the stats rather than passing for a whole one.
			wantEmitted:   2,
			wantMalformed: 1,
			wantLastFields: func(t *testing.T, e Entry) {
				if e.Name != "two" || e.Version != "2" {
					t.Errorf("truncated stanza = %+v", e)
				}
			},
		},
		{
			name:          "garbage lines between stanzas",
			src:           "Package: one\nVersion: 1\nDescription: first\n\n<<<not deb822 at all>>>\nanother garbage line\n\nPackage: two\nVersion: 2\nDescription: second\n\n",
			wantEmitted:   2,
			wantMalformed: 2,
		},
		{
			name:        "CRLF line endings",
			src:         "Package: crlf\r\nVersion: 1.0\r\nSection: main/net\r\nDescription: a snapshot written on Windows\r\n\r\n",
			wantEmitted: 1,
			wantLastFields: func(t *testing.T, e Entry) {
				if e.Summary != "a snapshot written on Windows" {
					t.Errorf("summary = %q — a carriage return survived", e.Summary)
				}
				if e.Section != "net" {
					t.Errorf("section = %q", e.Section)
				}
			},
		},
		{
			name:        "repeated blank lines",
			src:         "\n\nPackage: one\nVersion: 1\nDescription: first\n\n\n\nPackage: two\nVersion: 2\nDescription: second\n\n\n",
			wantEmitted: 2,
		},
		{
			name:          "a field name with no colon",
			src:           "Package: one\nVersion 1\nDescription: first\n\n",
			wantEmitted:   1,
			wantMalformed: 1,
			wantLastFields: func(t *testing.T, e Entry) {
				if e.Version != "" {
					t.Errorf("version = %q, want empty: the line carried no colon", e.Version)
				}
				if e.Summary != "first" {
					t.Errorf("summary = %q — a malformed line swallowed the next field", e.Summary)
				}
			},
		},
		{
			name:        "a non-numeric size",
			src:         "Package: one\nVersion: 1\nInstalled-Size: not-a-number\nSize: -5\nDescription: first\n\n",
			wantEmitted: 1,
			wantLastFields: func(t *testing.T, e Entry) {
				if e.InstalledSizeKiB != 0 || e.DownloadSizeBytes != 0 {
					t.Errorf("sizes = %d/%d, want 0/0", e.InstalledSizeKiB, e.DownloadSizeBytes)
				}
			},
		},
		{
			name:        "lowercase field names",
			src:         "package: one\nversion: 1\ndescription: first\n\n",
			wantEmitted: 1,
			wantLastFields: func(t *testing.T, e Entry) {
				if e.Name != "one" || e.Version != "1" || e.Summary != "first" {
					t.Errorf("entry = %+v; deb822 field names are case-insensitive", e)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, stats := pkgParseAll(t, []byte(tc.src), pkgRef("noble", "main"))
			if stats.Emitted != tc.wantEmitted {
				t.Errorf("emitted = %d, want %d", stats.Emitted, tc.wantEmitted)
			}
			if stats.MalformedLines != tc.wantMalformed {
				t.Errorf("malformed = %d, want %d", stats.MalformedLines, tc.wantMalformed)
			}
			if tc.wantLastFields != nil && len(entries) > 0 {
				tc.wantLastFields(t, entries[len(entries)-1])
			}
		})
	}
}

func TestPackagesEmptyInput(t *testing.T) {
	t.Parallel()

	entries, stats := pkgParseAll(t, nil, pkgRef("noble", "main"))
	if len(entries) != 0 || stats.Stanzas != 0 {
		t.Errorf("entries %d stats %+v, want nothing", len(entries), stats)
	}
}

// TestPackagesBodyWithNoStanzasIsFatal: a file that arrived and decompressed
// but yielded no rows is not an empty component, it is an unreadable index.
// catalogue-sourcing.md's one unacceptable failure mode is the silently
// incomplete catalogue.
func TestPackagesBodyWithNoStanzasIsFatal(t *testing.T) {
	t.Parallel()

	body := pkgGzip(t, []byte("<html><body>404 Not Found</body></html>\n"))
	_, err := packagesParseBody(context.Background(), body, pkgRef("noble", "main"), packagesDefaultLimits(), func(Entry) {})
	if err == nil {
		t.Fatal("parsing an HTML error page as an index returned no error")
	}
	var fe *PackagesFetchError
	if !errors.As(err, &fe) {
		t.Errorf("err = %v (%T), want *PackagesFetchError", err, err)
	}
}

// TestPackagesEmptyIndexIsNotFatal is the other side of that rule: an archive
// may legitimately publish an empty Packages file for a component that has
// nothing in it for this architecture. That is a shorter catalogue, not a
// failure.
func TestPackagesEmptyIndexIsNotFatal(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"", "\n", "\n\n\n"} {
		stats, err := packagesParseBody(context.Background(), pkgGzip(t, []byte(content)), pkgRef("noble", "restricted"), packagesDefaultLimits(), func(Entry) {})
		if err != nil {
			t.Errorf("an empty index (%q) was fatal: %v", content, err)
		}
		if stats.Emitted != 0 {
			t.Errorf("emitted = %d from an empty index", stats.Emitted)
		}
	}
}

func TestPackagesCorruptGzipIsFatal(t *testing.T) {
	t.Parallel()

	good := pkgGzip(t, pkgFixture(t, pkgFixtureUbuntuMain))
	// Truncating a gzip stream is what a dropped connection produces.
	_, err := packagesParseBody(context.Background(), good[:len(good)/2], pkgRef("noble", "main"), packagesDefaultLimits(), func(Entry) {})
	if err == nil {
		t.Fatal("a truncated gzip stream parsed without error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Logf("truncated gzip error: %v", err) // any error is acceptable; silence is not
	}
}

// TestPackagesUncompressedBodyStillParses: some proxies decompress on the
// operator's behalf. The .gz magic sniff means that produces a catalogue
// rather than "invalid header".
func TestPackagesUncompressedBodyStillParses(t *testing.T) {
	t.Parallel()

	stats, err := packagesParseBody(context.Background(), pkgFixture(t, pkgFixtureUbuntuMain), pkgRef("noble", "main"), packagesDefaultLimits(), func(Entry) {})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if stats.Emitted != 31 {
		t.Errorf("emitted = %d, want 31", stats.Emitted)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestPackagesParseCancellation(t *testing.T) {
	t.Parallel()

	// A stream long enough that the parser's periodic context check is
	// reached well before the end.
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "Package: pkg%d\nVersion: 1.0\nArchitecture: amd64\nSection: universe/libs\nDescription: package number %d\n\n", i, i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen int
	_, err := ParsePackagesIndex(ctx, strings.NewReader(b.String()), pkgRef("noble", "universe"), func(Entry) {
		seen++
		if seen == 100 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if seen >= 20000 {
		t.Errorf("parsed %d stanzas after cancellation — it ran to completion", seen)
	}
	t.Logf("cancelled after %d stanzas (check interval is %d)", seen, packagesCancelCheckEvery)
}

func TestPackagesParseAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ParsePackagesIndex(ctx, bytes.NewReader(pkgFixture(t, pkgFixtureUbuntuMain)), pkgRef("noble", "main"), func(Entry) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Fetching — httptest only, never the network
// ---------------------------------------------------------------------------

func pkgGzip(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// pkgArchive is a fake archive: a map of repo-relative path to response.
type pkgArchive struct {
	mu       sync.Mutex
	files    map[string][]byte
	statuses map[string]int
	requests []string
	agents   []string
	// hold, when non-nil, blocks the handler after the first write of every
	// body until it is closed — for cancellation tests. started is closed
	// once, when the first held response has been written and flushed.
	hold    chan struct{}
	started chan struct{}
	once    sync.Once
	// plain serves bodies uncompressed.
	plain bool
}

func pkgNewArchive() *pkgArchive {
	return &pkgArchive{files: map[string][]byte{}, statuses: map[string]int{}}
}

func (a *pkgArchive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ubuntu/")

	a.mu.Lock()
	a.requests = append(a.requests, path)
	a.agents = append(a.agents, r.Header.Get("User-Agent"))
	body, ok := a.files[path]
	status := a.statuses[path]
	hold := a.hold
	a.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "error\n")
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found\n")
		return
	}
	if hold != nil {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:1])
		if f, okf := w.(http.Flusher); okf {
			f.Flush()
		}
		if a.started != nil {
			a.once.Do(func() { close(a.started) })
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

func (a *pkgArchive) add(t testing.TB, suite, component string, fixture string) {
	t.Helper()
	data := pkgFixture(t, fixture)
	if !a.plain {
		data = pkgGzip(t, data)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[fmt.Sprintf("dists/%s/%s/binary-amd64/Packages.gz", suite, component)] = data
}

func (a *pkgArchive) fail(suite, component string, status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statuses[fmt.Sprintf("dists/%s/%s/binary-amd64/Packages.gz", suite, component)] = status
}

func (a *pkgArchive) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func pkgTarget(uris []string, suites, components []string) Target {
	return Target{
		Kind:      TargetBase,
		BaseID:    "ubuntu:24.04/desktop",
		DistroID:  "ubuntu",
		VersionID: "24.04",
		Codename:  "noble",
		Arch:      "amd64",
		Sources: []Source{{
			Types:      []string{"deb"},
			URIs:       uris,
			Suites:     suites,
			Components: components,
		}},
	}
}

type pkgProgressLog struct {
	mu sync.Mutex
	ps []Progress
}

func (l *pkgProgressLog) record(p Progress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ps = append(l.ps, p)
}

func (l *pkgProgressLog) all() []Progress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Progress(nil), l.ps...)
}

func TestPackagesLoadOverHTTP(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	arc.add(t, "noble", "universe", pkgFixtureUbuntuUniverse)
	srv := httptest.NewServer(arc)
	defer srv.Close()

	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main", "universe"})
	log := &pkgProgressLog{}

	loader := PackagesLoader{HTTPClient: srv.Client()}
	res, err := loader.Load(context.Background(), tgt, log.record)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.Indexes != 2 {
		t.Errorf("indexes = %d, want 2", res.Indexes)
	}
	if len(res.Missing) != 0 {
		t.Errorf("missing = %+v, want none", res.Missing)
	}
	// 31 + 52 stanzas, one duplicate name inside universe.
	if res.Stats.Emitted != 83 {
		t.Errorf("emitted = %d, want 83", res.Stats.Emitted)
	}
	if res.DuplicateNames != 1 {
		t.Errorf("duplicates = %d, want 1", res.DuplicateNames)
	}
	if len(res.Entries) != 82 {
		t.Errorf("entries = %d, want 82", len(res.Entries))
	}
	if res.BytesDownloaded == 0 {
		t.Error("no bytes recorded as downloaded")
	}

	// Rows from both components, with the component recorded.
	if e, ok := pkgFind(res.Entries, "accountsservice"); !ok || e.Component != "main" {
		t.Errorf("accountsservice = %+v, want component main", e)
	}
	if e, ok := pkgFind(res.Entries, "0ad-data"); !ok || e.Component != "universe" {
		t.Errorf("0ad-data = %+v, want component universe", e)
	}

	// The DEP-11 refs belong to another package and must not be fetched here.
	for _, p := range arc.seen() {
		if strings.Contains(p, "dep11") {
			t.Errorf("requested %s — packages.go must fetch only IndexPackages refs", p)
		}
	}

	// Progress: both phases, in order, and never an empty label.
	ps := log.all()
	if len(ps) == 0 {
		t.Fatal("no progress reported")
	}
	var sawDownload, sawParse bool
	for _, p := range ps {
		if p.Label == "" {
			t.Errorf("progress with an empty label: %+v", p)
		}
		if p.PhaseCount != len(Phases) {
			t.Errorf("phase count = %d, want %d", p.PhaseCount, len(Phases))
		}
		if p.PhaseIndex != p.Phase.Index() {
			t.Errorf("phase index = %d, want %d for %s", p.PhaseIndex, p.Phase.Index(), p.Phase)
		}
		switch p.Phase {
		case PhaseDownload:
			if sawParse {
				t.Error("a download report arrived after a parse report")
			}
			sawDownload = true
		case PhaseParse:
			sawParse = true
		default:
			t.Errorf("unexpected phase %q; this package reports only download and parse", p.Phase)
		}
	}
	if !sawDownload || !sawParse {
		t.Errorf("phases seen: download=%v parse=%v", sawDownload, sawParse)
	}
	if last := ps[len(ps)-1]; last.Phase != PhaseParse || last.Current != last.Total {
		t.Errorf("last progress = %+v, want a completed parse phase", last)
	}

	// The user agent identifies this application honestly.
	arc.mu.Lock()
	agents := append([]string(nil), arc.agents...)
	arc.mu.Unlock()
	for _, ua := range agents {
		if ua != PackagesUserAgent {
			t.Errorf("User-Agent = %q, want %q", ua, PackagesUserAgent)
		}
	}
}

func TestPackagesLoad404IsNotFatal(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	// "restricted" publishes nothing: the archive answers 404.
	srv := httptest.NewServer(arc)
	defer srv.Close()

	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main", "restricted"})

	loader := PackagesLoader{HTTPClient: srv.Client()}
	res, err := loader.Load(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("a 404 on one component was fatal: %v", err)
	}
	if res.Indexes != 1 {
		t.Errorf("indexes = %d, want 1", res.Indexes)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %+v, want exactly one", res.Missing)
	}
	m := res.Missing[0]
	if m.Ref.Component != "restricted" || m.Status != http.StatusNotFound {
		t.Errorf("miss = %+v, want restricted / 404", m)
	}
	if m.Reason == "" || !strings.Contains(m.Reason, "restricted") {
		t.Errorf("miss reason = %q, want something an operator can read", m.Reason)
	}
	if len(res.Entries) != 31 {
		t.Errorf("entries = %d, want the 31 from main", len(res.Entries))
	}
}

func TestPackagesLoadServerErrorIsFatal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
	}{
		{"500", http.StatusInternalServerError},
		{"403", http.StatusForbidden},
		{"503", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arc := pkgNewArchive()
			arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
			arc.fail("noble", "universe", tc.status)
			srv := httptest.NewServer(arc)
			defer srv.Close()

			tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main", "universe"})
			loader := PackagesLoader{HTTPClient: srv.Client()}
			_, err := loader.Load(context.Background(), tgt, nil)
			if err == nil {
				t.Fatalf("HTTP %d was not fatal; a catalogue missing universe must not look complete", tc.status)
			}
			var fe *PackagesFetchError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v (%T), want *PackagesFetchError", err, err)
			}
			if fe.Status != tc.status {
				t.Errorf("status = %d, want %d", fe.Status, tc.status)
			}
			if errors.Is(err, ErrPackagesIndexMissing) {
				t.Error("a server error was reported as a missing index; those are different facts")
			}
			if !strings.Contains(fe.Error(), fe.URL) {
				t.Errorf("error %q does not name the URL that failed", fe.Error())
			}
		})
	}
}

func TestPackagesLoadUnreachableHostIsFatal(t *testing.T) {
	t.Parallel()

	// A server that is closed before use: connecting fails at the transport.
	srv := httptest.NewServer(pkgNewArchive())
	client := srv.Client()
	url := srv.URL
	srv.Close()

	tgt := pkgTarget([]string{url + "/ubuntu"}, []string{"noble"}, []string{"main"})
	loader := PackagesLoader{HTTPClient: client}
	_, err := loader.Load(context.Background(), tgt, nil)
	if err == nil {
		t.Fatal("an unreachable archive was not fatal")
	}
	if errors.Is(err, ErrPackagesIndexMissing) {
		t.Error("an unreachable archive was reported as a missing index")
	}
	var fe *PackagesFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v (%T), want *PackagesFetchError", err, err)
	}
}

// TestPackagesLoadMirrorFallback: a source may list several URIs for the same
// index. IndexRefs expands them all and documents that "the fetcher may
// choose"; this is that choice.
func TestPackagesLoadMirrorFallback(t *testing.T) {
	t.Parallel()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	arc := pkgNewArchive()
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	good := httptest.NewServer(arc)
	defer good.Close()

	tgt := pkgTarget([]string{broken.URL + "/ubuntu", good.URL + "/ubuntu"}, []string{"noble"}, []string{"main"})
	loader := PackagesLoader{HTTPClient: good.Client()}
	res, err := loader.Load(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("Load did not fall back to the second mirror: %v", err)
	}
	if len(res.Entries) != 31 {
		t.Errorf("entries = %d, want 31", len(res.Entries))
	}
}

// TestPackagesLoadFetchesEachIndexOnce: two mirrors of one archive must not
// mean two downloads of the same 19 MB file.
func TestPackagesLoadFetchesEachIndexOnce(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	srv := httptest.NewServer(arc)
	defer srv.Close()

	// The same server listed twice, as two "mirrors".
	tgt := pkgTarget([]string{srv.URL + "/ubuntu", srv.URL + "/ubuntu-mirror"}, []string{"noble"}, []string{"main"})
	loader := PackagesLoader{HTTPClient: srv.Client()}
	res, err := loader.Load(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Entries) != 31 {
		t.Errorf("entries = %d, want 31 — the index was merged with itself", len(res.Entries))
	}
	if n := len(arc.seen()); n != 1 {
		t.Errorf("%d requests for one index, want 1: %v", n, arc.seen())
	}
}

func TestPackagesLoadAllMirrors404(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	srv := httptest.NewServer(arc)
	defer srv.Close()

	tgt := pkgTarget([]string{srv.URL + "/ubuntu", srv.URL + "/ubuntu-mirror"}, []string{"noble"}, []string{"main"})
	loader := PackagesLoader{HTTPClient: srv.Client()}
	res, err := loader.Load(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("404 from every mirror should be a miss, not an error: %v", err)
	}
	if len(res.Missing) != 1 || len(res.Entries) != 0 {
		t.Errorf("missing = %+v, entries = %d", res.Missing, len(res.Entries))
	}
}

func TestPackagesLoadCancelDuringDownload(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.hold = make(chan struct{})
	arc.started = make(chan struct{})
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	srv := httptest.NewServer(arc)
	defer srv.Close()
	defer close(arc.hold)

	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The archive answers, sends one byte, and then stops — an operator
	// staring at a stalled mirror, which is exactly when Cancel gets pressed.
	go func() {
		<-arc.started
		cancel()
	}()

	var sawDownload atomic.Bool
	loader := PackagesLoader{HTTPClient: srv.Client()}
	_, err := loader.Load(ctx, tgt, func(p Progress) {
		if p.Phase == PhaseDownload {
			sawDownload.Store(true)
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !sawDownload.Load() {
		t.Error("no download progress was reported before the cancellation")
	}
}

func TestPackagesLoadNoIndexes(t *testing.T) {
	t.Parallel()

	// A target whose only source is a flat repository: IndexRefs skips it.
	tgt := pkgTarget([]string{"http://example.invalid/repo"}, []string{"./"}, []string{"main"})
	_, err := PackagesLoader{}.Load(context.Background(), tgt, nil)
	if err == nil {
		t.Fatal("a target with no Packages indexes returned no error")
	}
}

func TestPackagesLoadPlainBodies(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.plain = true
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	srv := httptest.NewServer(arc)
	defer srv.Close()

	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main"})
	res, err := PackagesLoader{HTTPClient: srv.Client()}.Load(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Entries) != 31 {
		t.Errorf("entries = %d, want 31", len(res.Entries))
	}
}

func TestPackagesLoadOversizeIndexIsRejected(t *testing.T) {
	t.Parallel()

	arc := pkgNewArchive()
	arc.add(t, "noble", "main", pkgFixtureUbuntuMain)
	srv := httptest.NewServer(arc)
	defer srv.Close()

	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, []string{"main"})
	loader := PackagesLoader{HTTPClient: srv.Client(), MaxIndexBytes: 128}
	_, err := loader.Load(context.Background(), tgt, nil)
	if err == nil {
		t.Fatal("an index over the size limit was accepted")
	}
	var fe *PackagesFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v (%T), want *PackagesFetchError", err, err)
	}
}

// TestPackagesGroupsPreserveSourceOrder pins the merge order that the
// duplicate-name rule depends on.
// TestPackagesLoadConcurrencyIsBounded checks both halves of "concurrent,
// with a sane bound": that several indexes really are in flight at once, and
// that never more than Concurrency of them are.
//
// The gate makes it deterministic rather than timing-dependent: the handler
// blocks until the expected number of requests have arrived, so a serial
// implementation fails the assertion instead of racing to a lucky pass.
func TestPackagesLoadConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	const bound = 3
	var (
		inFlight atomic.Int64
		peak     atomic.Int64
		opened   = make(chan struct{}, 64)
	)
	release := make(chan struct{})

	body := pkgGzip(t, pkgFixture(t, pkgFixtureUbuntuMain))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "Packages.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		opened <- struct{}{}
		<-release
		inFlight.Add(-1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Eight components, so the bound has something to bind.
	comps := []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8"}
	tgt := pkgTarget([]string{srv.URL + "/ubuntu"}, []string{"noble"}, comps)

	done := make(chan error, 1)
	go func() {
		_, err := PackagesLoader{HTTPClient: srv.Client(), Concurrency: bound}.Load(context.Background(), tgt, nil)
		done <- err
	}()

	// Wait for the bound to be saturated, then let everything through.
	timeout := time.After(30 * time.Second)
	for i := 0; i < bound; i++ {
		select {
		case <-opened:
		case <-timeout:
			close(release)
			t.Fatalf("only %d requests were in flight after 30s; Load is not fetching concurrently", inFlight.Load())
		}
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := peak.Load(); got > bound {
		t.Errorf("peak concurrency = %d, want at most %d — an archive is one host", got, bound)
	} else if got < 2 {
		t.Errorf("peak concurrency = %d; the refs were fetched serially", got)
	}
}

func TestPackagesGroupsPreserveSourceOrder(t *testing.T) {
	t.Parallel()

	tgt := Target{
		Kind: TargetBase, BaseID: "ubuntu:24.04/desktop",
		DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
		Sources: []Source{
			{Types: []string{"deb"}, URIs: []string{"http://a.invalid/u", "http://b.invalid/u"}, Suites: []string{"noble", "noble-updates"}, Components: []string{"main", "universe"}},
		},
	}
	groups := packagesGroupsFor(tgt)
	want := []string{"noble/main", "noble/universe", "noble-updates/main", "noble-updates/universe"}
	if len(groups) != len(want) {
		t.Fatalf("%d groups, want %d: %+v", len(groups), len(want), groups)
	}
	for i, g := range groups {
		if got := g.ref.Suite + "/" + g.ref.Component; got != want[i] {
			t.Errorf("group %d = %s, want %s", i, got, want[i])
		}
		if len(g.urls) != 2 {
			t.Errorf("group %d has %d urls, want 2 mirrors", i, len(g.urls))
		}
		if g.ref.Kind != IndexPackages {
			t.Errorf("group %d kind = %s, want %s", i, g.ref.Kind, IndexPackages)
		}
	}
}

// ---------------------------------------------------------------------------
// Budgets: throughput and resident memory
// ---------------------------------------------------------------------------

// pkgSynthCorpus streams an index of n stanzas built from a real fixture by
// renaming each Package: uniquely.
//
// It is a reader rather than a []byte so that the memory measurement below is
// not measuring its own input: 70,000 stanzas of real field text is 124 MB,
// which would swamp the ~17 MB the entries themselves cost. Every byte except
// the package name is real archive text, so the shape of the work — field
// lengths, UTF-8, the long fields — is real even though the corpus is not one
// the archive publishes.
type pkgSynthCorpus struct {
	lines [][]byte
	want  int
	made  int
	buf   bytes.Buffer
}

func pkgSynthReader(t testing.TB, fixture string, n int) *pkgSynthCorpus {
	t.Helper()
	return &pkgSynthCorpus{lines: bytes.Split(pkgFixture(t, fixture), []byte("\n")), want: n}
}

func (c *pkgSynthCorpus) Read(p []byte) (int, error) {
	for c.buf.Len() == 0 {
		if c.made >= c.want {
			return 0, io.EOF
		}
		for _, ln := range c.lines {
			if rest, ok := bytes.CutPrefix(ln, []byte("Package: ")); ok {
				if c.made >= c.want {
					break
				}
				fmt.Fprintf(&c.buf, "Package: p%d-%s\n", c.made, rest)
				c.made++
				continue
			}
			c.buf.Write(ln)
			c.buf.WriteByte('\n')
		}
	}
	return c.buf.Read(p)
}

// pkgSynthIndex is the same corpus materialised, for benchmarks that need a
// repeatable input whose generation is not on the clock.
func pkgSynthIndex(t testing.TB, fixture string, n int) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := io.Copy(&out, pkgSynthReader(t, fixture, n)); err != nil {
		t.Fatalf("building synthetic index: %v", err)
	}
	return out.Bytes()
}

// TestPackagesResidentMemory measures what 70,000 parsed entries actually
// cost, against the < 400 MB idle-resident budget in the contract brief.
//
// docs/dev/index-formats.md §5 measured 15.3 MB for the lean strategy over the
// real corpus and 235 MB for the map-per-stanza one. This is the same
// measurement over real field text with synthesised names, and it exists to
// fail if this parser ever starts retaining whole stanzas.
func TestPackagesResidentMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("parses a 124 MB synthetic index")
	}

	const n = 70000
	m := newPackagesMerger(0)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	stats, err := ParsePackagesIndex(context.Background(), pkgSynthReader(t, pkgFixtureUbuntuMain, n), pkgRef("noble", "main"), m.add)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	if len(m.entries) != n || stats.Emitted != n {
		t.Fatalf("entries = %d, emitted = %d, want %d", len(m.entries), stats.Emitted, n)
	}
	held := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("retained %d bytes for %d entries = %d B/entry (entries, their strings, and the transient name->slot map)",
		held, n, held/int64(n))
	t.Logf("churn during the pass: %d bytes TotalAlloc", after.TotalAlloc-before.TotalAlloc)

	// Generous, because a heap measurement has noise in it. The point of the
	// ceiling is the 235 MB failure mode, not a byte count.
	const ceiling = 64 << 20
	if held > ceiling {
		t.Errorf("retained %d bytes for %d entries, over the %d byte ceiling — something is holding whole stanzas", held, n, ceiling)
	}
	runtime.KeepAlive(m)
}

// BenchmarkPackagesParse measures the parse phase against the < 10 s budget in
// the contract brief.
//
//	go test ./internal/catalog -run '^$' -bench PackagesParse -benchmem
//
// MEASURED, 2026-09-06, Go 1.26.0 windows/amd64, Intel Core Ultra 9 285K,
// -benchtime 3s (record the machine with the number; a throughput figure with
// no machine attached is a rumour):
//
//	ubuntu-main            28.3 µs/op   1938 MB/s   1,096,805 stanzas/s    209 allocs/op
//	ubuntu-universe        67.7 µs/op   2449 MB/s     768,644 stanzas/s    361 allocs/op
//	debian-main            26.7 µs/op   1331 MB/s   1,237,186 stanzas/s    232 allocs/op
//	synthetic-20k          17.1 ms/op   2079 MB/s   1,171,867 stanzas/s  6.7 allocs/stanza
//	synthetic-20k-merged   17.6 ms/op   2018 MB/s   1,137,911 stanzas/s  6.7 allocs/stanza
//
// The real corpus is 80.5 MB uncompressed and 70,854 stanzas for Ubuntu noble
// main+universe (docs/dev/index-formats.md §1, §2.1). At the merged figure
// above that is a parse phase of roughly 60–90 ms — about 1% of the 10 s
// budget, and in the same range as §7.4's independently measured 86–96 ms.
// The download is the cost of a catalogue build; parsing is not.
//
// Resident memory for the same 70,854 rows is measured by
// TestPackagesResidentMemory: 30.3 MB for 70,000 entries, or 432 B/entry
// including the transient name->slot map, against a 400 MB idle budget.
func BenchmarkPackagesParse(b *testing.B) {
	fixtures := []struct {
		name string
		file string
	}{
		{"ubuntu-main", pkgFixtureUbuntuMain},
		{"ubuntu-universe", pkgFixtureUbuntuUniverse},
		{"debian-main", pkgFixtureDebianMain},
	}
	for _, f := range fixtures {
		b.Run(f.name, func(b *testing.B) {
			data := pkgFixture(b, f.file)
			ref := pkgRef("noble", "main")
			var stanzas int64
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				st, err := ParsePackagesIndex(context.Background(), bytes.NewReader(data), ref, func(Entry) {})
				if err != nil {
					b.Fatal(err)
				}
				stanzas = st.Emitted
			}
			b.StopTimer()
			b.ReportMetric(float64(stanzas)*float64(b.N)/b.Elapsed().Seconds(), "stanzas/s")
		})
	}

	// A corpus the size of a real component, of real field text.
	const synth = 20000
	b.Run("synthetic-20k", func(b *testing.B) {
		data := pkgSynthIndex(b, pkgFixtureUbuntuMain, synth)
		ref := pkgRef("noble", "main")
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := ParsePackagesIndex(context.Background(), bytes.NewReader(data), ref, func(Entry) {}); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(synth*float64(b.N)/b.Elapsed().Seconds(), "stanzas/s")
	})

	// The same, merged and deduplicated: the shape of a real build.
	b.Run("synthetic-20k-merged", func(b *testing.B) {
		data := pkgSynthIndex(b, pkgFixtureUbuntuMain, synth)
		ref := pkgRef("noble", "main")
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m := newPackagesMerger(int64(len(data)) / 4)
			if _, err := ParsePackagesIndex(context.Background(), bytes.NewReader(data), ref, m.add); err != nil {
				b.Fatal(err)
			}
			if len(m.entries) != synth {
				b.Fatalf("entries = %d", len(m.entries))
			}
		}
		b.StopTimer()
		b.ReportMetric(synth*float64(b.N)/b.Elapsed().Seconds(), "stanzas/s")
	})
}

// ---------------------------------------------------------------------------
// decompression and entry caps
// ---------------------------------------------------------------------------
//
// docs/security-review.md §3.2 marked this one "confirmed by reading, not
// demonstrated end to end". These tests demonstrate it: pkgBomb builds a real
// gzip bomb out of the same stanzas a real index carries, and the ratio it
// achieves is asserted rather than assumed, so the argument for the cap stays
// true rather than becoming folklore.

// pkgBomb returns a gzip stream that inflates to at least want bytes of valid
// Packages stanzas. Highly repetitive by construction, which is the point.
func pkgBomb(t testing.TB, want int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	stanza := []byte("Package: p\nVersion: 1\nArchitecture: amd64\nSection: net\nInstalled-Size: 1\nDescription: x\n\n")
	block := bytes.Repeat(stanza, 4096)
	for written := int64(0); written < want; written += int64(len(block)) {
		if _, err := zw.Write(block); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestPackagesGzipBombIsRefused is the finding. Before the cap, the only
// bound on this path was on the compressed body, so the expansion ratio below
// was the whole attack.
func TestPackagesGzipBombIsRefused(t *testing.T) {
	const expand = 64 << 20 // 64 MiB inflated; enough to measure, quick to build
	body := pkgBomb(t, expand)

	ratio := float64(expand) / float64(len(body))
	t.Logf("gzip bomb: %d compressed bytes inflate to %d (%.0f:1)", len(body), expand, ratio)
	if ratio < 100 {
		t.Fatalf("ratio %.0f:1 is too low to be the case the cap exists for; the fixture is not repetitive enough", ratio)
	}

	// The cap is lowered rather than the bomb enlarged: a 512 MiB fixture
	// would make this test cost more than the bug ever did.
	lim := packagesLimits{Decompressed: 1 << 20, Entries: 0}
	var got int
	_, err := packagesParseBody(context.Background(), body, pkgRef("noble", "main"), lim, func(Entry) { got++ })
	if err == nil {
		t.Fatalf("no error; parsed %d entries from a bomb", got)
	}
	var fe *PackagesFetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a *PackagesFetchError", err)
	}
	if !strings.Contains(err.Error(), "expands to more than") {
		t.Errorf("error %q does not say the index expanded past the cap", err)
	}
}

// TestPackagesEntryCapIsRefused covers the half a byte cap cannot: 512 MiB of
// minimal stanzas is tens of millions of retained Entries, well inside a limit
// that was only ever about bytes.
func TestPackagesEntryCapIsRefused(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "Package: p%d\nVersion: 1\nArchitecture: amd64\n\n", i)
	}
	body := pkgGzip(t, []byte(sb.String()))

	lim := packagesLimits{Decompressed: packagesDefaultMaxDecompressedBytes, Entries: 100}
	var got int
	_, err := packagesParseBody(context.Background(), body, pkgRef("noble", "main"), lim, func(Entry) { got++ })
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "more than 100 package stanzas") {
		t.Errorf("error %q does not name the entry cap", err)
	}
	// The cap stops handing entries over the moment it is reached, so the
	// caller never accumulates more than it allowed.
	if int64(got) > lim.Entries {
		t.Errorf("emitted %d entries past a cap of %d", got, lim.Entries)
	}
}

// TestPackagesCapsAgreeWithDep11 is the reason this is a defect rather than a
// judgement call: two loaders in one package, reading the same kind of file
// from the same kind of host, disagreeing about whether a bomb is bounded.
func TestPackagesCapsAgreeWithDep11(t *testing.T) {
	if packagesDefaultMaxDecompressedBytes != dep11MaxDecompressedBytes {
		t.Fatalf("Packages caps decompression at %d and DEP-11 at %d; one of them is wrong and it is not the one with the older cap",
			packagesDefaultMaxDecompressedBytes, dep11MaxDecompressedBytes)
	}
}

// TestPackagesRealIndexIsNowhereNearTheCaps keeps the numbers honest: a cap
// that a real archive brushes against is a cap that will break a legitimate
// build one day, and the fixture here is a slice of the real universe index.
func TestPackagesRealIndexIsNowhereNearTheCaps(t *testing.T) {
	body := pkgFixture(t, pkgFixtureUbuntuMain)
	var n int64
	stats, err := packagesParseBody(context.Background(), body, pkgRef("noble", "main"), packagesDefaultLimits(), func(Entry) { n++ })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n == 0 {
		t.Fatal("no entries")
	}
	t.Logf("real fixture: %d compressed bytes, %d stanzas, %d entries", len(body), stats.Stanzas, n)
	if n*10 > packagesDefaultMaxEntries {
		t.Errorf("a real index emits %d entries against a cap of %d — less than an order of magnitude of headroom",
			n, packagesDefaultMaxEntries)
	}
}

// TestPackagesLoaderLimitsDefaultAndOverride pins the option plumbing, which
// is the part a caller sees.
func TestPackagesLoaderLimitsDefaultAndOverride(t *testing.T) {
	if got := (PackagesLoader{}).limits(); got != packagesDefaultLimits() {
		t.Errorf("zero loader limits = %+v, want %+v", got, packagesDefaultLimits())
	}
	l := PackagesLoader{MaxDecompressedBytes: 7, MaxEntries: 9}
	if got := l.limits(); got.Decompressed != 7 || got.Entries != 9 {
		t.Errorf("limits = %+v, want 7/9", got)
	}
	// Negative disables, matching dep11Options.MaxDecompressedBytes.
	if got := (PackagesLoader{MaxDecompressedBytes: -1}).limits(); got.Decompressed != -1 {
		t.Errorf("negative did not survive: %+v", got)
	}
}

// TestPackagesUncompressedBodyIsNotDoubleCapped: an uncompressed body was
// already bounded on the way in by packagesGet's io.LimitReader, so wrapping
// it again would report a limit that is not the one that applied.
func TestPackagesUncompressedBodyIsNotDoubleCapped(t *testing.T) {
	plain := []byte("Package: hello\nVersion: 1\nArchitecture: amd64\n\n")
	lim := packagesLimits{Decompressed: 4, Entries: 0} // absurdly small
	var got int
	if _, err := packagesParseBody(context.Background(), plain, pkgRef("noble", "main"), lim, func(Entry) { got++ }); err != nil {
		t.Fatalf("uncompressed body hit the decompression cap: %v", err)
	}
	if got != 1 {
		t.Errorf("emitted %d entries, want 1", got)
	}
}

// TestCheckIndexRedirectPolicy is docs/security-review.md §3.3. Neither index
// client set CheckRedirect, so both took Go's default: up to ten redirects, to
// any host, including https: -> http:. Two of those three are worth refusing
// and one is not, and all three are pinned here — the allowance especially,
// because it is the part someone will otherwise "fix" later.
func TestCheckIndexRedirectPolicy(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		return &http.Request{URL: u, Host: u.Host}
	}
	cases := []struct {
		name    string
		to      string
		via     []*http.Request
		wantErr string
	}{
		{"https to https on the same host", "https://archive.example/a", []*http.Request{req("https://archive.example/b")}, ""},
		{
			// mirrors.ubuntu.com and the httpredir generation of Debian
			// services exist to do exactly this. Refusing it would break
			// legitimate targets and close nothing: the host set is already
			// whatever the snapshot names, and a snapshot that wanted to name
			// another host could simply name it.
			name: "https to another host is ordinary mirror infrastructure",
			to:   "https://mirror.example/a", via: []*http.Request{req("https://archive.example/b")},
		},
		{"http to http", "http://mirror.example/a", []*http.Request{req("http://archive.example/b")}, ""},
		{"http upgraded to https", "https://mirror.example/a", []*http.Request{req("http://archive.example/b")}, ""},
		{"https downgraded to http", "http://mirror.example/a", []*http.Request{req("https://archive.example/b")}, "https to http"},
		{"redirect to file:", "file:///etc/passwd", []*http.Request{req("https://archive.example/b")}, "http and https only"},
		{"redirect to ftp:", "ftp://archive.example/a", []*http.Request{req("https://archive.example/b")}, "http and https only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := igCheckIndexRedirect(req(tc.to), tc.via)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused a legitimate redirect: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatal("allowed it")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("a chain is bounded", func(t *testing.T) {
		var via []*http.Request
		for i := 0; i < igMaxIndexRedirects; i++ {
			via = append(via, req("https://archive.example/"))
		}
		if err := igCheckIndexRedirect(req("https://archive.example/x"), via); err == nil {
			t.Fatalf("followed more than %d redirects", igMaxIndexRedirects)
		}
	})
}

// TestBothIndexClientsCheckRedirects: the policy is worth nothing if only one
// of the two clients that fetch an index installs it.
func TestBothIndexClientsCheckRedirects(t *testing.T) {
	if packagesDefaultClient.CheckRedirect == nil {
		t.Error("the Packages client follows redirects with Go's default policy")
	}
	if dep11DefaultClient().CheckRedirect == nil {
		t.Error("the DEP-11 client follows redirects with Go's default policy")
	}
}

// TestRedirectPolicyAppliesToARealFetch drives the whole thing through an
// httptest server, because a CheckRedirect that is installed but never
// consulted is the failure mode a field test would not catch.
func TestRedirectPolicyAppliesToARealFetch(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: igCheckIndexRedirect}

	target = "ftp://archive.example/ubuntu/x"
	if _, err := client.Get(srv.URL + "/dists/noble/main/binary-amd64/Packages.gz"); err == nil {
		t.Error("followed a redirect to ftp:")
	}

	target = "file:///etc/passwd"
	if _, err := client.Get(srv.URL + "/dists/noble/main/binary-amd64/Packages.gz"); err == nil {
		t.Error("followed a redirect to file:")
	}
}

// ---------------------------------------------------------------------------
// What the fuzz target found, reproduced as rules
// ---------------------------------------------------------------------------

// TestPackagesUnusableNamesAreDropped is FuzzSecPackagesIndex's two findings,
// written down as the rule rather than left as corpus entries.
//
// Entry.Name is not a label. It is the key a sorted cache is binary-searched
// by, the identity the selection tray holds a row under, one element of the
// argv a build runs ("apt:"+name), and a line in the packages.txt-shaped list
// the CLI accepts instead. A name carrying a control character is unusable as
// several of those: a newline becomes two lines in that list and in the
// command rule 8 requires the UI to show, and os/exec refuses an argument
// containing NUL outright.
//
// Stated accurately rather than dramatically: internal/app's validPackageName
// refuses all of these when a row is added to the tray, so none reaches an
// argv today. The defect is one layer earlier. The picker offers a row, the
// operator clicks it, and the application tells them that what they clicked is
// not a package name -- a row it produced itself.
//
// Neither shape occurs in the Ubuntu or Debian archives. Both are reachable
// from an operator-supplied snapshot's captured indexes and from a mirror this
// application has authenticated nothing about, which is what
// catalogue-sourcing.md means by the catalogue not being a trust boundary.
//
// The stanza is dropped and counted in SkippedStanzas, exactly as a stanza
// with no name at all already was.
func TestPackagesUnusableNamesAreDropped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		// want is the names that must be emitted, in order.
		want    []string
		skipped int64
		stanzas int64
	}{{
		// Finding 1. pkgAppendFold folded Package: along with every other
		// kept field, so a continuation line became part of the name.
		name:    "a folded Package field does not extend the name",
		body:    "Package: hello\n evil\nVersion: 1\n",
		want:    []string{"hello"},
		stanzas: 1,
	}, {
		name:    "a folded Package field with a colon in it is still not a field",
		body:    "Package: hello\n uitoolkit::gtk\nVersion: 1\n",
		want:    []string{"hello"},
		stanzas: 1,
	}, {
		// Finding 2. A NUL byte inside the value passed straight through.
		name:    "a NUL in the name drops the stanza",
		body:    "Package: hel\x00lo\nVersion: 1\n",
		want:    nil,
		skipped: 1,
		stanzas: 1,
	}, {
		name:    "an escape or a DEL in the name drops the stanza",
		body:    "Package: hel\x1blo\n\nPackage: he\x7fllo\n",
		want:    nil,
		skipped: 2,
		stanzas: 2,
	}, {
		name:    "a tab in the name drops the stanza",
		body:    "Package: hel\tlo\nVersion: 1\n",
		want:    nil,
		skipped: 1,
		stanzas: 1,
	}, {
		// The one that must keep working: a bad stanza costs its own row and
		// nothing else. A catalogue that dropped an index over one damaged
		// name would be the worse failure, and it is the failure an operator
		// cannot see.
		name:    "a bad stanza costs one row, not the index",
		body:    "Package: before\nVersion: 1\n\nPackage: bad\x00name\n\nPackage: after\nVersion: 2\n",
		want:    []string{"before", "after"},
		skipped: 1,
		stanzas: 3,
	}, {
		// And the rule must not reach past control characters. These are all
		// odd, none is a control character, and apt reads every one of them.
		name:    "unusual but printable names are kept",
		body:    "Package: lib++\n\nPackage: a.b-c+d\n\nPackage: -starts-with-a-dash\n\nPackage: ünïcøde\n\nPackage: has space\n",
		want:    []string{"lib++", "a.b-c+d", "-starts-with-a-dash", "ünïcøde", "has space"},
		stanzas: 5,
	}, {
		// Leading and trailing whitespace is stripped by the field reader
		// before the name is ever seen, so this is a name, not a blank.
		name:    "surrounding whitespace is not part of the name",
		body:    "Package:   hello   \nVersion: 1\n",
		want:    []string{"hello"},
		stanzas: 1,
	}, {
		name:    "a name that is only whitespace is not a name",
		body:    "Package:  \nVersion: 1\n",
		want:    nil,
		skipped: 1,
		stanzas: 1,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, stats := pkgParseAll(t, []byte(tc.body), pkgRef("noble", "universe"))

			got := make([]string, 0, len(entries))
			for _, e := range entries {
				got = append(got, e.Name)
			}
			if !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("names = %q, want %q", got, tc.want)
			}
			if stats.SkippedStanzas != tc.skipped {
				t.Errorf("SkippedStanzas = %d, want %d", stats.SkippedStanzas, tc.skipped)
			}
			if stats.Stanzas != tc.stanzas {
				t.Errorf("Stanzas = %d, want %d", stats.Stanzas, tc.stanzas)
			}
			if stats.Emitted != int64(len(tc.want)) {
				t.Errorf("Emitted = %d, want %d", stats.Emitted, len(tc.want))
			}
		})
	}
}

// TestPackagesNameSurvivesArgv drives the outer half rather than asserting it.
// A name this parser emits must be usable as the argument a build actually
// runs, and os/exec is the authority on that, so os/exec is what gets asked
// rather than a restatement of its rules. validPackageName is the layer that
// would catch these first; this is the property that has to hold whether or
// not it stays that strict.
func TestPackagesNameSurvivesArgv(t *testing.T) {
	t.Parallel()

	body := "Package: hello\n evil\nVersion: 1\n\nPackage: nul\x00name\n\nPackage: fine\nVersion: 2\n"
	entries, _ := pkgParseAll(t, []byte(body), pkgRef("noble", "universe"))
	if len(entries) == 0 {
		t.Fatal("nothing was emitted, so this test proves nothing")
	}
	for _, e := range entries {
		cmd := exec.Command(os.Args[0], "apt:"+e.Name)
		// Not run: Start is where os/exec validates the arguments, and
		// running the test binary again is not the point.
		if err := cmd.Start(); err != nil {
			t.Fatalf("a name this parser emitted cannot be passed to a build: %q: %v", e.Name, err)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
