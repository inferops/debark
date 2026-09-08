package catalog

// FuzzSecPackagesIndex is the last unfuzzed parser of attacker-adjacent bytes
// in this repository. The deb822 sources parsers and the DEP-11 decoder were
// fuzzed earlier and the sources targets found five real defects, the first
// in fifteen seconds; this one closes the set.
//
// # Why these bytes are attacker-adjacent
//
// A `Packages` index is tens of megabytes fetched over HTTPS from whichever
// archive the target's own sources name, and this parser is what turns it into
// every row the operator sees and every name they can select. Three things
// make it worth fuzzing rather than merely testing:
//
//   - The archive is not always the distro's. A snapshot is untrusted input by
//     construction — a file produced on another machine and carried across the
//     air gap — and its captured sources decide which hosts are contacted. A
//     vendor repository, a corporate mirror or a compromised one all reach
//     this function through exactly the same code path as archive.ubuntu.com.
//   - The signature is not checked here and deliberately never will be.
//     catalogue-sourcing.md §1 says the catalogue is not a trust boundary and
//     this package ships no keyrings; `debark` re-resolves everything through
//     apt, which does check. So the bytes arriving here have been authenticated
//     by nothing.
//   - What comes out is not only displayed. Entry.Name is the cache's binary
//     search key, the tray's identity, and the string that ends up in the
//     package list a build is asked to resolve. A parser that can be made to
//     emit a name the rest of the application cannot use as a key is a
//     different kind of problem from one that merely renders oddly.
//
// The cost of a defect here is bounded — catalogue-sourcing.md is explicit
// that a wrong catalogue costs "a missing row in a picker" and never a wrong
// bundle, because apt is the oracle. That is a sound argument about what the
// catalogue *produces*. It is not an argument about what the parser *does* on
// the way there, and this target is about the second.
//
// # The properties, and why they are these
//
// Structural, not example-based: a fuzzer earns its keep on the properties
// that hold for every input, and the examples are already pinned in
// packages_test.go against fixtures that came off real archives.
//
//  1. It returns, without panicking and without unbounded allocation. The
//     scanner's ceiling is packagesScanBufferMax and a line above it must come
//     back as an error rather than as a short catalogue.
//  2. The statistics describe what happened. Emitted equals the number of emit
//     calls; Emitted and SkippedStanzas together never exceed Stanzas; no
//     counter is negative. These numbers are what tells the difference between
//     "this component is empty" and "a mirror served an error page as 200",
//     which packagesParseVerdict then turns into a hard failure — so a
//     miscount is a wrong verdict about a whole archive.
//  3. Every emitted Entry has a name, and it is a name the rest of the
//     application can use. Non-empty is what flush already promises. Single
//     line and free of NUL is the property this target was written to check,
//     because Entry.Name is a key and a line in a package list, not a label.
//  4. Sizes are never negative. Entry documents zero as "unknown" and the UI
//     renders these as bytes; a negative would be neither.
//  5. Parsing is deterministic. The same bytes twice must give the same rows
//     in the same order, because the cache is written from one parse and the
//     picker's ordering contract is a property of the other.
//
// Deliberately NOT asserted:
//
//   - That any particular input round-trips. The parser is allowed to skip
//     stanzas and unknown fields, and packages_test.go is where the accepting
//     behaviour is pinned.
//   - That every string is valid UTF-8. Real Debian archives carry Latin-1
//     bytes in old maintainer names and descriptions, apt reads them, and the
//     consequence downstream is that encoding/json substitutes U+FFFD on the
//     way across the bridge. That is cosmetic, it is what apt itself does, and
//     asserting otherwise would make this target reject data the archive
//     genuinely publishes.
//
// # The campaign
//
// Recorded here rather than in docs/performance.md because that file belongs
// to another package this wave; see the report accompanying this commit for the
// routing.
//
//	go test ./internal/catalog/ -run '^$' -fuzz '^FuzzSecPackagesIndex$' \
//	  -parallel 6 -fuzztime 10m
//
// Six workers on a 24-logical-core machine, which is the quarter-of-the-cores
// rule this project adopted after full-width runs produced workers that died
// under load and wrote non-reproducing inputs that looked like findings.
//
// Findings are reproduced as named table tests in packages_test.go before
// being reported or fixed, never as a bare corpus entry: a crasher file says
// what broke and a table test says what the rule is.
//
// What the fuzzing campaign actually did, stated as a record rather than as a
// score. Two defects were found, both by the hand-written seeds above and both
// within the first second: a folded Package: field extending a name across
// lines, and a control character passing through into one. Both are fixed and
// pinned by TestPackagesUnusableNamesAreDropped.
//
// Beyond the seeds it found nothing, and the campaign did not run to
// completion. Four runs — 6, 4, 2 and 4 workers, about 20.5 million executions
// in total — each ended the same way: a worker died during minimisation and Go
// persisted whatever input it happened to be holding as a "failing" one. None
// of the four reproduced under -count=10, and all four were deleted rather
// than committed, because committing a non-reproducing input records a
// machine's bad afternoon as a finding. The machine was running nine other packages concurrently and the reported execution rate spent long stretches at
// zero — 30/sec at its worst — which is precisely the condition the
// quarter-of-the-cores rule exists for, at a load that rule did not anticipate.
// The determinism check below was made size-bounded in response, on the theory
// that it was the expensive half of each execution and therefore the likely
// reason minimisation looked like a hang; the fourth run died anyway, on a
// 66 KB input that the new bound excludes from that check entirely, which
// argues the cause is the machine rather than this target.
//
// So, plainly: the corpus grew to roughly 291 entries and branch coverage of
// this parser is genuinely wider than it was. An execution count is a measure
// of effort, not of coverage, and this target has not yet had one
// uninterrupted run on a quiet machine. Give it one before calling this parser
// fuzz-clean.

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

// pkgFuzzSeeds are the handwritten shapes: the grammar's corners, the cases
// index-formats.md names as real, and the ones a mutation reaches usefully
// only from a valid start. The committed fixtures are added beside them, whole
// and stanza by stanza, in FuzzSecPackagesIndex itself.
var pkgFuzzSeeds = []string{
	// The minimum stanza, and a full one.
	"Package: hello\n",
	"Package: hello\nVersion: 2.10-3\nArchitecture: amd64\nSection: devel\n" +
		"Priority: optional\nInstalled-Size: 280\nSize: 53024\n" +
		"Homepage: https://www.gnu.org/software/hello/\n" +
		"Description: example package based on GNU hello\n",
	// Two stanzas, and a file that does not end in a blank line — §2 says the
	// last stanza need not be terminated.
	"Package: a\nVersion: 1\n\nPackage: b\nVersion: 2\n",
	"Package: a\nVersion: 1\n\n\n\nPackage: b\nVersion: 2\n\n\n",
	// Case-insensitive field names, which §2 says not to rely on the archive
	// for once a snapshot's operator can supply the file.
	"package: hello\nVERSION: 2.10\nInStAlLeD-sIzE: 12\n",
	// The component prefix flush strips, and one that must not be stripped.
	"Package: hello\nSection: universe/devel\n",
	"Package: hello\nSection: devel\n",
	// A continuation of a field the parser keeps, and of one it skips. The
	// second is the case §4.1 exists for: Debian folds Tag: across lines and
	// its values contain "::", so a parser splitting every line on the first
	// colon would invent a field called "uitoolkit".
	"Package: hello\nDescription: short\n long body line\n another\n",
	"Package: hello\nTag: implemented-in::c\n uitoolkit::gtk\nVersion: 1\n",
	// A continuation of Package: itself. Nothing in a real archive folds it;
	// the parser's general fold path does not know that.
	"Package: hello\n evil\nVersion: 1\n",
	// A stanza with fields but no name, which is counted and dropped.
	"Version: 1\nArchitecture: amd64\n\nPackage: real\n",
	// Numeric fields that are not numbers, negative, or enormous. Entry
	// documents zero as unknown and a malformed size as a missing one.
	"Package: hello\nInstalled-Size: not-a-number\nSize: -5\n",
	"Package: hello\nInstalled-Size: 99999999999999999999999\nSize: 9223372036854775808\n",
	"Package: hello\nInstalled-Size:\nSize:\n",
	// Structural nastiness: no colon, a leading colon, CRLF, a BOM, a NUL, an
	// empty value, and whitespace where a value should be.
	"Package hello\nVersion: 1\n",
	":\nPackage: hello\n",
	"Package: hello\r\nVersion: 1\r\n\r\nPackage: two\r\n",
	"\xef\xbb\xbfPackage: hello\nVersion: 1\n",
	"Package: hel\x00lo\nVersion: 1\n",
	"Package:\nVersion: 1\n",
	"Package:    \t \nVersion: 1\n",
	"Package: \thello\t\nVersion: 1\n",
	// Not a Packages file at all: an error page a mirror served as 200, and a
	// sources file. Both are what MalformedLines exists to count.
	"<!DOCTYPE html>\n<html><head><title>404</title></head></html>\n",
	"Types: deb\nURIs: https://deb.debian.org/debian\nSuites: bookworm\n",
	"",
	"\n\n\n\n",
	// Scale, in the two directions that matter: many stanzas, and one line
	// long enough to test the scanner's ceiling rather than the archive's
	// manners.
	strings.Repeat("Package: p\nVersion: 1\n\n", 512),
	"Package: hello\nDescription: " + strings.Repeat("x", 70000) + "\n",
	strings.Repeat("Package: ", 4096),
}

func FuzzSecPackagesIndex(f *testing.F) {
	for _, s := range pkgFuzzSeeds {
		f.Add([]byte(s))
	}
	// The committed excerpts, whole and split into single stanzas. Whole files
	// give the mutator real field orders and real value shapes to work from;
	// single stanzas keep most of the corpus small enough that a mutation
	// lands somewhere interesting rather than in the middle of a description
	// forty kilobytes in.
	for _, name := range []string{pkgFixtureUbuntuMain, pkgFixtureUbuntuUniverse, pkgFixtureDebianMain} {
		body := pkgFixture(f, name)
		f.Add(body)
		for _, stanza := range bytes.Split(body, []byte("\n\n")) {
			if len(bytes.TrimSpace(stanza)) == 0 {
				continue
			}
			f.Add(append(bytes.TrimRight(stanza, "\n"), '\n'))
		}
	}

	ref := pkgRef("noble", "universe")

	f.Fuzz(func(t *testing.T, data []byte) {
		// The production path caps the decompressed stream long before this;
		// a fuzzer that spends its budget on ten-megabyte inputs is testing
		// the machine rather than the parser.
		if len(data) > 1<<20 {
			return
		}

		entries, stats := pkgFuzzParse(t, data, ref)
		pkgFuzzCheck(t, entries, stats)

		// Property 5. Two parses of the same bytes, compared as values.
		//
		// Only for inputs small enough that the second parse and the
		// entry-by-entry comparison are cheap. This is not fastidiousness: a
		// megabyte of minimal stanzas is roughly 40,000 entries, so the check
		// costs two full parses and 40,000 deep comparisons, and on a loaded
		// machine that is slow enough for the fuzzing coordinator to decide a
		// worker has hung — which it reports as "hung or terminated
		// unexpectedly while minimizing", writes out whatever input it was
		// holding, and ends the run. Three campaigns ended that way and
		// none of the three inputs reproduced. Determinism is a property of the
		// scanner's state machine and a 64 KiB stanza stream exercises every
		// branch of it that a megabyte does; the larger inputs still get
		// properties 1 to 4 on a single parse, which is what they are actually
		// good at reaching.
		if len(data) > 64<<10 {
			return
		}
		again, statsAgain := pkgFuzzParse(t, data, ref)
		if stats != statsAgain {
			t.Fatalf("two parses of the same bytes produced different statistics:\n%+v\n%+v", stats, statsAgain)
		}
		if len(again) != len(entries) {
			t.Fatalf("two parses of the same bytes produced %d and %d entries", len(entries), len(again))
		}
		for i := range entries {
			// reflect.DeepEqual rather than ==: Entry carries Categories,
			// which ParsePackagesIndex never sets but which makes the struct
			// incomparable.
			if !reflect.DeepEqual(entries[i], again[i]) {
				t.Fatalf("entry %d differs between two parses of the same bytes:\n%+v\n%+v", i, entries[i], again[i])
			}
		}
	})
}

// pkgFuzzParse runs one parse and returns what came out. An error is a
// legitimate answer — a line above the scanner's ceiling, or a cancelled
// context — and only the entries emitted before it are examined.
func pkgFuzzParse(t *testing.T, data []byte, ref IndexRef) ([]Entry, PackagesParseStats) {
	t.Helper()
	var got []Entry
	stats, err := ParsePackagesIndex(context.Background(), bytes.NewReader(data), ref, func(e Entry) {
		got = append(got, e)
	})
	if err != nil {
		// The one error this path can produce is a line the scanner cannot
		// hold, which packagesScanBufferMax bounds. It must say so rather
		// than come back as a short catalogue, and the entries emitted before
		// it must still be well formed, so the checks below still run.
		if !strings.Contains(err.Error(), "reading index") {
			t.Fatalf("ParsePackagesIndex returned an unexpected error shape: %v", err)
		}
	}
	return got, stats
}

// pkgFuzzCheck is properties 2, 3 and 4 applied to one parse.
func pkgFuzzCheck(t *testing.T, entries []Entry, stats PackagesParseStats) {
	t.Helper()

	// Property 2.
	if stats.Stanzas < 0 || stats.Emitted < 0 || stats.SkippedStanzas < 0 || stats.MalformedLines < 0 {
		t.Fatalf("a negative counter: %+v", stats)
	}
	if stats.Emitted != int64(len(entries)) {
		t.Fatalf("stats say %d entries were emitted and %d arrived", stats.Emitted, len(entries))
	}
	if stats.Emitted+stats.SkippedStanzas > stats.Stanzas {
		t.Fatalf("emitted (%d) plus skipped (%d) exceeds stanzas seen (%d): packagesParseVerdict decides whether a whole archive failed from these", stats.Emitted, stats.SkippedStanzas, stats.Stanzas)
	}

	for i, e := range entries {
		// Property 3.
		if e.Name == "" {
			t.Fatalf("entry %d was emitted with no name: %+v", i, e)
		}
		if strings.ContainsAny(e.Name, "\n\r") {
			t.Fatalf("entry %d has a name spanning more than one line: %q — Name is the cache's binary-search key, the tray's identity and a line in the package list a build is asked to resolve, none of which can hold a newline", i, e.Name)
		}
		if strings.ContainsRune(e.Name, 0) {
			t.Fatalf("entry %d has a NUL in its name: %q", i, e.Name)
		}
		// Property 4.
		if e.InstalledSizeKiB < 0 {
			t.Fatalf("entry %d has a negative installed size: %+v", i, e)
		}
		if e.DownloadSizeBytes < 0 {
			t.Fatalf("entry %d has a negative download size: %+v", i, e)
		}
	}
}
