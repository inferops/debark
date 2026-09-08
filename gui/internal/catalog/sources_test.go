package catalog

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/inferops/debark/core/base"
)

// The whole value of this file is that it runs against sources that came off
// real machines. testdata/sources/README.md records where each one came from;
// the ones written by hand are marked there and are the adversarial cases that
// cannot be captured by definition.

func srcTestRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sources", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func srcTestKinds(problems []SourceProblem) []SourceProblemKind {
	out := make([]SourceProblemKind, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.Kind)
	}
	return out
}

func srcTestSources(entries []SourceEntry) []Source {
	out := make([]Source, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Source)
	}
	return out
}

// srcTestCase is one fixture and everything the parser must say about it.
type srcTestCase struct {
	file string
	// stanzas is how many entries the document contains, counting the ones
	// that produced nothing. It backs the "nothing is dropped silently"
	// invariant: every index below it must appear in an entry or in a problem.
	stanzas int
	sources []Source
	// kinds is every problem kind, in order. Order matters: a problem is read
	// next to the file it came from, and a parser that reordered them would
	// make a line number harder to find rather than easier.
	kinds []SourceProblemKind
	// arches, when set, is the per-entry architecture restriction.
	arches [][]string
}

func srcTestCases() []srcTestCase {
	return []srcTestCase{
		{
			// Captured from a real Ubuntu 24.04 machine. Note the trailing
			// slash on both URIs: it is transcribed, not tidied, because
			// IndexRef.URL trims it and nothing else in this package cares.
			file:    "ubuntu-noble.sources",
			stanzas: 2,
			sources: []Source{
				{
					Types:      []string{"deb"},
					URIs:       []string{"http://archive.ubuntu.com/ubuntu/"},
					Suites:     []string{"noble", "noble-updates", "noble-backports"},
					Components: []string{"main", "universe", "restricted", "multiverse"},
				},
				{
					Types:      []string{"deb"},
					URIs:       []string{"http://security.ubuntu.com/ubuntu/"},
					Suites:     []string{"noble-security"},
					Components: []string{"main", "universe", "restricted", "multiverse"},
				},
			},
		},
		{
			// Captured from a real debian:bookworm-slim container. The "#"
			// comment sits *inside* each stanza, between Types and URIs, which
			// is the detail that breaks a parser treating any comment as a
			// paragraph break.
			file:    "debian-bookworm.sources",
			stanzas: 2,
			sources: []Source{
				{
					Types:      []string{"deb"},
					URIs:       []string{"http://deb.debian.org/debian"},
					Suites:     []string{"bookworm", "bookworm-updates"},
					Components: []string{"main"},
				},
				{
					Types:      []string{"deb"},
					URIs:       []string{"http://deb.debian.org/debian-security"},
					Suites:     []string{"bookworm-security"},
					Components: []string{"main"},
				},
			},
		},
		{
			// A real file with nothing in it: Ubuntu 24.04 moved its sources
			// to deb822 and left four comment lines behind. No entries and no
			// problems — an empty answer that is correct is not a skip.
			file:    "ubuntu-noble-sources.list",
			stanzas: 0,
		},
		{
			file:    "tailscale.list",
			stanzas: 1,
			sources: []Source{{
				Types:      []string{"deb"},
				URIs:       []string{"https://pkgs.tailscale.com/stable/ubuntu"},
				Suites:     []string{"noble"},
				Components: []string{"main"},
			}},
			arches: [][]string{nil},
		},
		{
			file:    "debian12-classic.list",
			stanzas: 4,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"http://deb.debian.org/debian"}, Suites: []string{"bookworm"}, Components: []string{"main"}},
				{Types: []string{"deb"}, URIs: []string{"http://deb.debian.org/debian"}, Suites: []string{"bookworm-updates"}, Components: []string{"main"}},
				{Types: []string{"deb"}, URIs: []string{"http://deb.debian.org/debian-security"}, Suites: []string{"bookworm-security"}, Components: []string{"main"}},
			},
			kinds: []SourceProblemKind{SourceProblemSourceOnly},
		},
		{
			// Two flat repositories from a real experiment run. Neither has a
			// dists/ hierarchy, so neither produces a row — and both are said
			// out loud.
			file:    "flat-repo.list",
			stanzas: 2,
			kinds:   []SourceProblemKind{SourceProblemFlatRepo, SourceProblemFlatRepo},
		},
		{
			file:    "jammy-no-signed-by.sources",
			stanzas: 2,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"http://archive.ubuntu.com/ubuntu"}, Suites: []string{"jammy", "jammy-updates"}, Components: []string{"main", "restricted", "universe", "multiverse"}},
				{Types: []string{"deb"}, URIs: []string{"http://security.ubuntu.com/ubuntu"}, Suites: []string{"jammy-security"}, Components: []string{"main", "restricted", "universe", "multiverse"}},
			},
		},
		{
			// An inline armoured key is a folded field whose continuation
			// lines contain colons, dots and base64. None of it may be
			// mistaken for a field, and the stanza around it must survive.
			file:    "inline-armored.sources",
			stanzas: 1,
			sources: []Source{{
				Types:      []string{"deb"},
				URIs:       []string{"https://example.invalid/repo"},
				Suites:     []string{"stable"},
				Components: []string{"main"},
			}},
		},
		{
			file:    "vendor-arch-signed-by.list",
			stanzas: 5,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"https://download.docker.com/linux/ubuntu"}, Suites: []string{"noble"}, Components: []string{"stable"}},
				{Types: []string{"deb"}, URIs: []string{"https://packages.microsoft.com/repos/code"}, Suites: []string{"stable"}, Components: []string{"main"}},
				{Types: []string{"deb"}, URIs: []string{"https://archive.raspberrypi.org/debian"}, Suites: []string{"bookworm"}, Components: []string{"main"}},
				{Types: []string{"deb"}, URIs: []string{"http://archive.ubuntu.com/ubuntu"}, Suites: []string{"noble-proposed"}, Components: []string{"main", "universe"}},
				{Types: []string{"deb"}, URIs: []string{"https://vendor.example/apt"}, Suites: []string{"stable"}, Components: []string{"main"}},
			},
			arches: [][]string{
				{"amd64"},
				{"amd64", "arm64"},
				{"arm64"},
				nil,
				{"any"},
			},
		},
		{
			file:    "mirrors.sources",
			stanzas: 3,
			sources: []Source{
				{
					Types: []string{"deb", "deb-src"},
					URIs: []string{
						"http://ftp.uk.debian.org/debian",
						"http://ftp.de.debian.org/debian",
						"https://deb.debian.org/debian",
					},
					Suites:     []string{"trixie", "trixie-updates"},
					Components: []string{"main", "contrib"},
				},
				{
					Types:      []string{"deb"},
					URIs:       []string{"https://vendor.example/ports"},
					Suites:     []string{"trixie"},
					Components: []string{"main"},
				},
			},
			kinds:  []SourceProblemKind{SourceProblemDisabled},
			arches: [][]string{nil, {"arm64", "armhf"}},
		},
		{
			// Nothing here may produce a Source, and nothing here may pass
			// unremarked. The order mirrors the file.
			file:    "malformed.sources",
			stanzas: 8,
			kinds: []SourceProblemKind{
				SourceProblemMalformed,   // 0: a line that is not a field
				SourceProblemMalformed,   // 1: URIs given twice
				SourceProblemMalformed,   // 2: a Packages stanza
				SourceProblemPlaceholder, // 3: ${codename} in the URI...
				SourceProblemNoURI,       // 3: ...which leaves no URI at all
				SourceProblemNoComponent, // 4
				SourceProblemSourceOnly,  // 5
				SourceProblemUnknownType, // 6
				SourceProblemFlatRepo,    // 7
			},
		},
		{
			// Real bytes from a Packages index. Three stanzas, none of which
			// describes a source: three complaints, no sources, no silence.
			file:    "not-sources.sources",
			stanzas: 3,
			kinds: []SourceProblemKind{
				SourceProblemMalformed,
				SourceProblemMalformed,
				SourceProblemMalformed,
			},
		},
		{file: "empty.sources", stanzas: 0},
		{
			file:    "builtin-ubuntu-2604-amd64.sources",
			stanzas: 2,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"http://archive.ubuntu.com/ubuntu"}, Suites: []string{"resolute", "resolute-updates", "resolute-backports"}, Components: []string{"main", "restricted", "universe", "multiverse"}},
				{Types: []string{"deb"}, URIs: []string{"http://security.ubuntu.com/ubuntu"}, Suites: []string{"resolute-security"}, Components: []string{"main", "restricted", "universe", "multiverse"}},
			},
		},
		{
			file:    "builtin-ubuntu-2604-arm64.sources",
			stanzas: 1,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"http://ports.ubuntu.com/ubuntu-ports"}, Suites: []string{"resolute", "resolute-updates", "resolute-backports", "resolute-security"}, Components: []string{"main", "restricted", "universe", "multiverse"}},
			},
		},
		{
			file:    "builtin-debian-13-amd64.sources",
			stanzas: 2,
			sources: []Source{
				{Types: []string{"deb"}, URIs: []string{"http://deb.debian.org/debian"}, Suites: []string{"trixie", "trixie-updates"}, Components: []string{"main", "contrib", "non-free", "non-free-firmware"}},
				{Types: []string{"deb"}, URIs: []string{"http://security.debian.org/debian-security"}, Suites: []string{"trixie-security"}, Components: []string{"main", "contrib", "non-free", "non-free-firmware"}},
			},
		},
	}
}

func TestSourcesParseFixtures(t *testing.T) {
	for _, tc := range srcTestCases() {
		t.Run(tc.file, func(t *testing.T) {
			entries, problems := ParseSourcesFile(tc.file, srcTestRead(t, tc.file))

			got := srcTestSources(entries)
			want := tc.sources
			if len(got) == 0 && len(want) == 0 {
				// reflect.DeepEqual distinguishes nil from empty; the contract
				// does not.
			} else if !reflect.DeepEqual(got, want) {
				t.Errorf("sources mismatch\n got: %#v\nwant: %#v", got, want)
			}

			if gotKinds := srcTestKinds(problems); !reflect.DeepEqual(gotKinds, tc.kinds) && (len(gotKinds) != 0 || len(tc.kinds) != 0) {
				t.Errorf("problem kinds mismatch\n got: %v\nwant: %v\ndetail:\n%s",
					gotKinds, tc.kinds, srcTestDump(problems))
			}

			if tc.arches != nil {
				for i, e := range entries {
					if i >= len(tc.arches) {
						break
					}
					if !reflect.DeepEqual(e.Arches, tc.arches[i]) {
						t.Errorf("entry %d arches = %v, want %v", i, e.Arches, tc.arches[i])
					}
				}
			}
		})
	}
}

// TestSourcesParseNothingIsDroppedSilently is the invariant the whole package
// exists for: every entry in every fixture either produced a Source or
// produced at least one problem naming it.
//
// A source that is neither is a source the operator will never learn about,
// and the symptom is a package that is simply not in the picker.
func TestSourcesParseNothingIsDroppedSilently(t *testing.T) {
	for _, tc := range srcTestCases() {
		t.Run(tc.file, func(t *testing.T) {
			entries, problems := ParseSourcesFile(tc.file, srcTestRead(t, tc.file))

			accounted := make(map[int]bool, tc.stanzas)
			for _, e := range entries {
				accounted[e.Stanza] = true
			}
			for _, p := range problems {
				accounted[p.Stanza] = true
			}
			for i := 0; i < tc.stanzas; i++ {
				if !accounted[i] {
					t.Errorf("entry %d of %s produced neither a source nor a problem: it vanished", i, tc.file)
				}
			}
			if n := len(entries) + len(problems); n < tc.stanzas {
				t.Errorf("%s has %d entries but only %d results", tc.file, tc.stanzas, n)
			}
		})
	}
}

// TestSourcesParseProblemsAreActionable holds every problem to the project's
// actionable-error rule: a kind to branch on and a sentence a person can read.
func TestSourcesParseProblemsAreActionable(t *testing.T) {
	for _, tc := range srcTestCases() {
		_, problems := ParseSourcesFile(tc.file, srcTestRead(t, tc.file))
		for i, p := range problems {
			if p.Kind == "" {
				t.Errorf("%s problem %d has no kind", tc.file, i)
			}
			if strings.TrimSpace(p.Reason) == "" {
				t.Errorf("%s problem %d (%s) has no reason", tc.file, i, p.Kind)
			}
			if p.File != tc.file {
				t.Errorf("%s problem %d records file %q", tc.file, i, p.File)
			}
			if p.Line <= 0 {
				t.Errorf("%s problem %d (%s) has no line number", tc.file, i, p.Kind)
			}
			if !strings.Contains(p.String(), p.Reason) {
				t.Errorf("%s problem %d does not render its reason: %q", tc.file, i, p.String())
			}
		}
	}
}

func srcTestDump(problems []SourceProblem) string {
	var b strings.Builder
	for _, p := range problems {
		b.WriteString("  ")
		b.WriteString(p.String())
		b.WriteString("\n")
	}
	return b.String()
}

// TestSourcesParseMalformedNeverProducesASource is the other half of the rule:
// wrong is worse than missing. A stanza this parser could not read confidently
// yields nothing at all, never a partial guess at an archive URI.
func TestSourcesParseMalformedNeverProducesASource(t *testing.T) {
	entries, problems := ParseSourcesDeb822("malformed.sources", srcTestRead(t, "malformed.sources"))
	if len(entries) != 0 {
		t.Fatalf("malformed.sources produced %d sources; it must produce none:\n%#v", len(entries), srcTestSources(entries))
	}
	if len(problems) == 0 {
		t.Fatal("malformed.sources produced no problems either, so eight bad stanzas vanished")
	}
}

// TestSourcesParseCRLFAndBOM applies the two encodings a captured file may
// arrive in. A snapshot may have been taken anywhere and copied through
// anything; the parse must not depend on it.
func TestSourcesParseCRLFAndBOM(t *testing.T) {
	const name = "ubuntu-noble.sources"
	base := srcTestRead(t, name)
	wantEntries, wantProblems := ParseSourcesDeb822(name, base)

	variants := map[string][]byte{
		"crlf":      []byte(strings.ReplaceAll(string(base), "\n", "\r\n")),
		"bom":       append([]byte(srcBOM), base...),
		"bom+crlf":  append([]byte(srcBOM), []byte(strings.ReplaceAll(string(base), "\n", "\r\n"))...),
		"no-eol":    []byte(strings.TrimRight(string(base), "\n")),
		"lone-cr":   []byte(strings.ReplaceAll(string(base), "\n", "\r")),
		"blank-eof": append(append([]byte{}, base...), []byte("\n\n\n")...),
	}
	for label, data := range variants {
		t.Run(label, func(t *testing.T) {
			entries, problems := ParseSourcesDeb822(name, data)
			if !reflect.DeepEqual(srcTestSources(entries), srcTestSources(wantEntries)) {
				t.Errorf("%s changed the parse:\n got: %#v\nwant: %#v", label, srcTestSources(entries), srcTestSources(wantEntries))
			}
			if len(problems) != len(wantProblems) {
				t.Errorf("%s produced %d problems, want %d:\n%s", label, len(problems), len(wantProblems), srcTestDump(problems))
			}
		})
	}
}

// TestSourcesParseTabsAndSpacing covers the whitespace a hand-edited file
// carries: tabs after the colon, several spaces between values, trailing
// blanks, and values folded across continuation lines.
func TestSourcesParseTabsAndSpacing(t *testing.T) {
	doc := "Types:\tdeb\n" +
		"URIs:  \thttp://archive.ubuntu.com/ubuntu   \n" +
		"Suites:\tnoble\tnoble-updates  noble-backports\n" +
		"Components: main\n" +
		"  universe\n" +
		"\trestricted\n"

	entries, problems := ParseSourcesDeb822("hand-edited.sources", []byte(doc))
	if len(problems) != 0 {
		t.Fatalf("unexpected problems:\n%s", srcTestDump(problems))
	}
	want := []Source{{
		Types:      []string{"deb"},
		URIs:       []string{"http://archive.ubuntu.com/ubuntu"},
		Suites:     []string{"noble", "noble-updates", "noble-backports"},
		Components: []string{"main", "universe", "restricted"},
	}}
	if got := srcTestSources(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestSourcesParseNonASCIIComments: a comment is a comment whatever alphabet
// it is in, and a UTF-8 comment must not derail the stanza that follows it.
func TestSourcesParseNonASCIIComments(t *testing.T) {
	doc := "# Dépôt Ubuntu — mis à jour le 3 septembre\n" +
		"# Зеркало Ubuntu\n" +
		"Types: deb\n" +
		"# 日本語のコメント\n" +
		"URIs: http://jp.archive.ubuntu.com/ubuntu\n" +
		"Suites: noble\n" +
		"Components: main\n"

	entries, problems := ParseSourcesDeb822("utf8.sources", []byte(doc))
	if len(problems) != 0 {
		t.Fatalf("unexpected problems:\n%s", srcTestDump(problems))
	}
	if len(entries) != 1 || entries[0].Source.URIs[0] != "http://jp.archive.ubuntu.com/ubuntu" {
		t.Fatalf("got %#v", srcTestSources(entries))
	}
}

// TestSourcesParseFileChoosesGrammar pins apt's own rule. Guessing differently
// from apt would build a catalogue from sources the target does not have.
func TestSourcesParseFileChoosesGrammar(t *testing.T) {
	deb822 := []byte("Types: deb\nURIs: http://deb.debian.org/debian\nSuites: trixie\nComponents: main\n")
	oneLine := []byte("deb http://deb.debian.org/debian trixie main\n")

	cases := []struct {
		name string
		data []byte
		want SourceFormat
	}{
		{"/etc/apt/sources.list.d/ubuntu.sources", deb822, SourceFormatDeb822},
		{"UBUNTU.SOURCES", deb822, SourceFormatDeb822},
		{"/etc/apt/sources.list", oneLine, SourceFormatOneLine},
		{"/etc/apt/sources.list.d/vendor.list", oneLine, SourceFormatOneLine},
		{"", oneLine, SourceFormatOneLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, problems := ParseSourcesFile(tc.name, tc.data)
			if len(problems) != 0 {
				t.Fatalf("unexpected problems:\n%s", srcTestDump(problems))
			}
			if len(entries) != 1 {
				t.Fatalf("got %d entries, want 1", len(entries))
			}
			if entries[0].Format != tc.want {
				t.Errorf("format = %q, want %q", entries[0].Format, tc.want)
			}
		})
	}
}

// TestSourcesParseWrongGrammarComplains: a deb822 document read with the
// one-line grammar, and the reverse. Both are real mistakes an operator can
// make by naming a file wrongly, and both must be loud.
func TestSourcesParseWrongGrammarComplains(t *testing.T) {
	deb822 := srcTestRead(t, "ubuntu-noble.sources")
	entries, problems := ParseSourcesOneLine("misnamed.list", deb822)
	if len(entries) != 0 {
		t.Errorf("a deb822 document read as one-line produced %d sources", len(entries))
	}
	if len(problems) == 0 {
		t.Error("a deb822 document read as one-line produced no complaint")
	}

	oneLine := srcTestRead(t, "debian12-classic.list")
	entries, problems = ParseSourcesDeb822("misnamed.sources", oneLine)
	if len(entries) != 0 {
		t.Errorf("a one-line document read as deb822 produced %d sources", len(entries))
	}
	if len(problems) == 0 {
		t.Error("a one-line document read as deb822 produced no complaint")
	}
}

// TestSourcesParseTrailingComment covers the "#" rules the one-line grammar
// carries: a whole-line comment, a trailing one, and a "#" inside a value that
// must NOT be treated as one — truncating a URI is the wrong-answer failure.
func TestSourcesParseTrailingComment(t *testing.T) {
	doc := "# a whole line\n" +
		"   # an indented whole line\n" +
		"deb http://archive.ubuntu.com/ubuntu noble main # trailing\n" +
		"deb [signed-by=/etc/apt/keyrings/a#b.gpg] https://vendor.example/apt stable main\n"

	entries, problems := ParseSourcesOneLine("comments.list", []byte(doc))
	if len(problems) != 0 {
		t.Fatalf("unexpected problems:\n%s", srcTestDump(problems))
	}
	want := []Source{
		{Types: []string{"deb"}, URIs: []string{"http://archive.ubuntu.com/ubuntu"}, Suites: []string{"noble"}, Components: []string{"main"}},
		{Types: []string{"deb"}, URIs: []string{"https://vendor.example/apt"}, Suites: []string{"stable"}, Components: []string{"main"}},
	}
	if got := srcTestSources(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestSourcesParseUnterminatedOptions: "[" with no "]" is refused whole rather
// than guessed at.
func TestSourcesParseUnterminatedOptions(t *testing.T) {
	entries, problems := ParseSourcesOneLine("bad.list", []byte("deb [arch=amd64 https://vendor.example/apt stable main\n"))
	if len(entries) != 0 {
		t.Fatalf("an unterminated [options] produced %d sources", len(entries))
	}
	if len(problems) != 1 || problems[0].Kind != SourceProblemMalformed {
		t.Fatalf("got %v, want one malformed problem", srcTestKinds(problems))
	}
}

// TestSourcesParseIncompleteOneLineEntries: a line that stops early names an
// archive nobody can fetch from. Each one has its own complaint, because each
// one has a different thing wrong with it.
func TestSourcesParseIncompleteOneLineEntries(t *testing.T) {
	cases := []struct {
		line string
		want SourceProblemKind
	}{
		{"deb", SourceProblemNoURI},
		{"deb [arch=amd64]", SourceProblemNoURI},
		{"deb [arch=amd64 signed-by=/k.gpg]   ", SourceProblemNoURI},
		{"deb http://archive.ubuntu.com/ubuntu", SourceProblemNoSuite},
		{"deb http://archive.ubuntu.com/ubuntu noble", SourceProblemNoComponent},
		{"deb-src http://archive.ubuntu.com/ubuntu noble main", SourceProblemSourceOnly},
		{"rpm http://vendor.example/rpm stable main", SourceProblemUnknownType},
		{"deb http://archive.ubuntu.com/ubuntu ./ main", SourceProblemFlatRepo},
		{"deb http://archive.ubuntu.com/ubuntu ${codename} main", SourceProblemPlaceholder},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			entries, problems := ParseSourcesOneLine("partial.list", []byte(tc.line+"\n"))
			if len(entries) != 0 {
				t.Fatalf("produced %d sources from %q", len(entries), tc.line)
			}
			if len(problems) == 0 {
				t.Fatalf("%q vanished without a word", tc.line)
			}
			if problems[0].Kind != tc.want {
				t.Errorf("first problem is %s, want %s:\n%s", problems[0].Kind, tc.want, srcTestDump(problems))
			}
		})
	}
}

// TestSourcesClipCutsOnARuneBoundary: the quoted fragment is shown to a
// person, and half a rune renders as a replacement character in the one place
// they are trying to read what their file actually said.
func TestSourcesClipCutsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 200)
	got := srcClip(s, srcMaxProblemText)
	if !utf8.ValidString(got) {
		t.Errorf("clipping produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped fragment should say it was clipped: %q", got)
	}
	if short := srcClip("main universe", srcMaxProblemText); short != "main universe" {
		t.Errorf("a short fragment was altered: %q", short)
	}
}

// TestSourcesForArch: one catalogue is one architecture, and the entries it
// leaves out are reported rather than dropped.
func TestSourcesForArch(t *testing.T) {
	entries, problems := ParseSourcesFile("vendor-arch-signed-by.list", srcTestRead(t, "vendor-arch-signed-by.list"))
	if len(problems) != 0 {
		t.Fatalf("unexpected parse problems:\n%s", srcTestDump(problems))
	}

	amd64, dropped := SourcesForArch("amd64", entries)
	if len(amd64) != 4 {
		t.Errorf("amd64 kept %d sources, want 4", len(amd64))
	}
	if len(dropped) != 1 || dropped[0].Kind != SourceProblemOtherArch {
		t.Fatalf("amd64 dropped %v, want one other-architecture problem", srcTestKinds(dropped))
	}
	if !strings.Contains(dropped[0].Reason, "amd64") || !strings.Contains(dropped[0].Reason, "arm64") {
		t.Errorf("the reason names neither architecture: %q", dropped[0].Reason)
	}
	if !dropped[0].Deliberate() {
		t.Error("an architecture restriction is a documented scope limit, not a defect")
	}

	arm64, dropped := SourcesForArch("arm64", entries)
	if len(arm64) != 4 {
		t.Errorf("arm64 kept %d sources, want 4", len(arm64))
	}
	if len(dropped) != 1 {
		t.Errorf("arm64 dropped %d sources, want 1", len(dropped))
	}

	// riscv64 keeps only the unrestricted entry and the arch=any one.
	riscv, dropped := SourcesForArch("riscv64", entries)
	if len(riscv) != 2 {
		t.Errorf("riscv64 kept %d sources, want 2", len(riscv))
	}
	if len(dropped) != 3 {
		t.Errorf("riscv64 dropped %d sources, want 3", len(dropped))
	}
}

// TestSourcesProblemDeliberate: the split that decides whether a UI shows a
// line quietly or loudly.
func TestSourcesProblemDeliberate(t *testing.T) {
	deliberate := []SourceProblemKind{
		SourceProblemSourceOnly, SourceProblemFlatRepo,
		SourceProblemDisabled, SourceProblemOtherArch,
	}
	accidental := []SourceProblemKind{
		SourceProblemNoURI, SourceProblemNoSuite, SourceProblemNoComponent,
		SourceProblemUnknownType, SourceProblemPlaceholder,
		SourceProblemMalformed, SourceProblemUnreadable, SourceProblemTruncated,
	}
	for _, k := range deliberate {
		if !(SourceProblem{Kind: k}).Deliberate() {
			t.Errorf("%s should be deliberate", k)
		}
	}
	for _, k := range accidental {
		if (SourceProblem{Kind: k}).Deliberate() {
			t.Errorf("%s should not be deliberate", k)
		}
	}
}

func TestSourcesProblemsSummary(t *testing.T) {
	if got := SourceProblemsSummary(nil); got != "" {
		t.Errorf("no problems should summarise to nothing, got %q", got)
	}

	_, problems := ParseSourcesFile("malformed.sources", srcTestRead(t, "malformed.sources"))
	got := SourceProblemsSummary(problems)
	if !strings.HasPrefix(got, fmt.Sprintf("%d apt sources were not used", len(problems))) {
		t.Errorf("summary does not lead with the count: %q", got)
	}
	for _, want := range []string{"deb-src", "flat", "placeholder"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary omits %q: %q", want, got)
		}
	}

	one := SourceProblemsSummary(problems[:1])
	if !strings.HasPrefix(one, "1 apt source was not used") {
		t.Errorf("a single problem should be singular: %q", one)
	}
}

// TestSourcesParseProblemCap: a document that is not sources at all must
// produce a bounded complaint, not one per stanza forever.
func TestSourcesParseProblemCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < srcMaxProblems*3; i++ {
		fmt.Fprintf(&b, "Package: pkg%d\nVersion: 1.0\n\n", i)
	}
	entries, problems := ParseSourcesDeb822("huge.sources", []byte(b.String()))
	if len(entries) != 0 {
		t.Fatalf("got %d sources from a Packages index", len(entries))
	}
	if len(problems) != srcMaxProblems+1 {
		t.Fatalf("got %d problems, want %d plus one truncation marker", len(problems), srcMaxProblems)
	}
	if last := problems[len(problems)-1]; last.Kind != SourceProblemTruncated {
		t.Errorf("the last problem is %s, want %s", last.Kind, SourceProblemTruncated)
	}
}

// TestSourcesParseOversizeDocument: past the size limit the file is not read,
// and that is said rather than assumed.
func TestSourcesParseOversizeDocument(t *testing.T) {
	big := make([]byte, SourceMaxDocumentBytes+1)
	for i := range big {
		big[i] = '\n'
	}
	entries, problems := ParseSourcesDeb822("big.sources", big)
	if len(entries) != 0 {
		t.Fatalf("got %d sources from an oversize document", len(entries))
	}
	if len(problems) != 1 || problems[0].Kind != SourceProblemUnreadable {
		t.Fatalf("got %v, want one unreadable problem", srcTestKinds(problems))
	}
}

// TestSourcesParseClipsControlCharacters: a problem's quoted text reaches a
// terminal and a details drawer. An escape sequence in a sources file must not
// survive the trip — the same rule core/snapshot applies to its own display
// strings, for the same reason.
func TestSourcesParseClipsControlCharacters(t *testing.T) {
	doc := "Types: deb\nURIs: https://vendor.example/\x1b[2Jrepo\nSuites: ./\nComponents: main\n"
	_, problems := ParseSourcesDeb822("nasty.sources", []byte(doc))
	if len(problems) == 0 {
		t.Fatal("no problem reported")
	}
	for _, p := range problems {
		if strings.ContainsRune(p.Text, 0x1b) || strings.ContainsRune(p.Reason, 0x1b) {
			t.Errorf("an escape character survived into %q", p.String())
		}
	}

	long := "Types: deb\nURIs: https://vendor.example/" + strings.Repeat("x", 4000) + "\nSuites: ./\nComponents: main\n"
	_, problems = ParseSourcesDeb822("long.sources", []byte(long))
	for _, p := range problems {
		if len(p.Text) > srcMaxProblemText+8 {
			t.Errorf("problem text is %d bytes, past the %d cap", len(p.Text), srcMaxProblemText)
		}
	}
}

// TestSourcesBuiltinFixturesMatchTheBinary is the drift canary.
//
// The fixtures under testdata/sources whose names start with "builtin-" are
// copies of what base.Builtin actually returns, and they are the highest-value
// input this file has: they are exactly the text production parses. If the
// core repository changes a builtin base's archive, components or suites, this
// test fails and the fixture is refreshed deliberately — rather than the
// fixtures quietly describing a base that no longer exists.
func TestSourcesBuiltinFixturesMatchTheBinary(t *testing.T) {
	cases := []struct {
		id, arch, fixture string
	}{
		{"ubuntu:26.04/desktop", "amd64", "builtin-ubuntu-2604-amd64.sources"},
		{"ubuntu:26.04/desktop", "arm64", "builtin-ubuntu-2604-arm64.sources"},
		{"debian:13/server", "amd64", "builtin-debian-13-amd64.sources"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			res, err := base.Resolve(tc.id, tc.arch)
			if err != nil {
				t.Fatalf("base.Resolve(%s, %s): %v", tc.id, tc.arch, err)
			}
			want := string(srcTestRead(t, tc.fixture))
			if got := res.Definition.Sources; got != want {
				t.Errorf("testdata/sources/%s no longer matches base.Builtin(%q) for %s.\n"+
					"Refresh the fixture deliberately — the archive a stock base points at has changed.\n"+
					"--- binary ---\n%s\n--- fixture ---\n%s", tc.fixture, tc.arch, tc.id, got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// URI scheme allow-list
// ---------------------------------------------------------------------------
//
// docs/security-review.md §3.1 confirmed by execution that every scheme apt
// supports became an http.NewRequestWithContext URL. These tests are the
// standing version of that experiment: the same seven URIs, plus the shapes
// that are not schemes at all.

func TestIGURIScheme(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"http://archive.ubuntu.com/ubuntu", "http"},
		{"https://archive.ubuntu.com/ubuntu", "https"},
		{"HTTPS://Archive.Example/Ubuntu", "https"},
		{"file:/var/lib/mirror", "file"},
		{"file:///srv/mirror", "file"},
		{"cdrom:[Ubuntu 24.04]/", "cdrom"},
		{"copy:/srv/x", "copy"},
		{"mirror+file:/etc/apt/mirrors.txt", "mirror+file"},
		{"ftp://archive.example/ubuntu", "ftp"},
		{"http://10.0.0.1:8080/ubuntu", "http"},
		// Not schemes. A leading digit, a leading colon and a bare path all
		// have to answer "" rather than something that could be allow-listed
		// by accident.
		{"/srv/mirror", ""},
		{"archive.ubuntu.com/ubuntu", ""},
		{":8080/ubuntu", ""},
		{"1http://x", ""},
		{"", ""},
		// A Windows path parses as scheme "c" under a naive split, which is
		// correct here only because "c" is not on the allow-list; the test
		// records the answer so a later "helpful" change cannot make it http.
		{`C:\srv\mirror`, "c"},
	}
	for _, tc := range cases {
		if got := igURIScheme(tc.uri); got != tc.want {
			t.Errorf("igURIScheme(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

// TestIndexRefsAllowsOnlyHTTPSchemes is the finding itself: none of these may
// become a request, and each must be reported rather than dropped in silence.
func TestIndexRefsAllowsOnlyHTTPSchemes(t *testing.T) {
	refused := []string{
		"file:/var/lib/mirror",
		"file:///srv/mirror",
		"cdrom:[Ubuntu 24.04]/",
		"copy:/srv/x",
		"mirror+file:/etc/apt/mirrors.txt",
		"ftp://archive.example/ubuntu",
		"/srv/mirror",
	}
	for _, uri := range refused {
		t.Run(uri, func(t *testing.T) {
			tgt := Target{
				Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
				DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
				Sources: []Source{{
					Types: []string{"deb"}, URIs: []string{uri},
					Suites: []string{"noble"}, Components: []string{"main"},
				}},
			}
			refs, problems := tgt.IndexRefsWithProblems()
			if len(refs) != 0 {
				t.Fatalf("built %d refs for %q; the first is %s", len(refs), uri, refs[0].URL())
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			p := problems[0]
			if p.Kind != SourceProblemUnsupportedScheme {
				t.Errorf("kind = %q, want %q", p.Kind, SourceProblemUnsupportedScheme)
			}
			if p.Reason == "" || p.Text == "" {
				t.Errorf("problem carries no reason or no text: %+v", p)
			}
			if !p.Deliberate() {
				t.Error("an unfetchable scheme is a documented scope limit, not a fault in the operator's file")
			}
		})
	}
}

// TestIndexRefsKeepsHTTPAndHTTPS pins the other half. Ubuntu still publishes
// plain-http archive URIs; refusing them would empty the catalogue for a large
// and entirely legitimate population.
func TestIndexRefsKeepsHTTPAndHTTPS(t *testing.T) {
	tgt := Target{
		Kind: TargetBase, BaseID: "ubuntu:24.04/minimal",
		DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
		Sources: []Source{
			{Types: []string{"deb"}, URIs: []string{"http://archive.ubuntu.com/ubuntu"}, Suites: []string{"noble"}, Components: []string{"main"}},
			{Types: []string{"deb"}, URIs: []string{"https://security.ubuntu.com/ubuntu"}, Suites: []string{"noble-security"}, Components: []string{"main"}},
		},
	}
	refs, problems := tgt.IndexRefsWithProblems()
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(refs) == 0 {
		t.Fatal("no refs")
	}
	for _, r := range refs {
		if s := igURIScheme(r.URL()); !igIndexSchemes[s] {
			t.Errorf("ref %s has scheme %q", r.URL(), s)
		}
	}
}

// TestIndexRefsMixedStanzaKeepsTheFetchableHalf is the case an offline mirror
// actually produces: one stanza listing a local mirror and a real archive.
// Dropping the whole stanza would lose the packages that are reachable.
func TestIndexRefsMixedStanzaKeepsTheFetchableHalf(t *testing.T) {
	tgt := Target{
		Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
		DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
		Sources: []Source{{
			Types:  []string{"deb"},
			URIs:   []string{"cdrom:[Ubuntu 24.04]/", "https://archive.ubuntu.com/ubuntu"},
			Suites: []string{"noble"}, Components: []string{"main", "universe"},
		}},
	}
	refs, problems := tgt.IndexRefsWithProblems()
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one — one URI, one sentence, however many components it has", problems)
	}
	if len(refs) != 4 { // two components x Packages + DEP-11
		t.Fatalf("refs = %d, want 4 from the https URI", len(refs))
	}
	for _, r := range refs {
		if !strings.HasPrefix(r.URL(), "https://") {
			t.Errorf("ref %s is not https", r.URL())
		}
	}
}

// TestUnsupportedSchemeReachesTheResolveProblems is the plumbing: the sentence
// has to arrive where the existing UI already renders source problems, or the
// catalogue is short by an archive with nothing on screen to say so.
func TestUnsupportedSchemeReachesTheResolveProblems(t *testing.T) {
	// A deb822 URIs: field is space-separated, so a real one carries the
	// install-media label without spaces in it; "cdrom:[Debian GNU/Linux 12]/"
	// is the one-line spelling and parses here as three separate tokens, none
	// of which is a scheme. Either way nothing is fetched.
	const doc = `Types: deb
URIs: cdrom:[Debian-12]/
Suites: bookworm
Components: main

Types: deb
URIs: https://deb.debian.org/debian
Suites: bookworm
Components: main
`
	entries, problems := ParseSourcesDeb822("/etc/apt/sources.list.d/debian.sources", []byte(doc))
	sources, archProblems := SourcesForArch("amd64", entries)
	problems = append(problems, archProblems...)

	tgt := Target{
		Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
		DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
		Sources: sources,
	}
	problems = append(problems, igSchemeProblems(tgt)...)

	var found *SourceProblem
	for i := range problems {
		if problems[i].Kind == SourceProblemUnsupportedScheme {
			found = &problems[i]
		}
	}
	if found == nil {
		t.Fatalf("no unsupported-scheme problem in %v", problems)
	}
	if !strings.Contains(found.Reason, "install media") {
		t.Errorf("reason %q does not explain what a cdrom: source is", found.Reason)
	}
	if summary := SourceProblemsSummary(problems); !strings.Contains(summary, "cannot fetch over") {
		t.Errorf("summary %q does not mention the unfetchable source", summary)
	}
	// The target is still usable: one source of two survived.
	if err := tgt.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestIndexRefsRefusesAnHTTPURIWithNoHost is what FuzzSecSourcesDeb822 found
// fifteen seconds into its first real run, and what FuzzSecSourcesOneLine and
// FuzzSecSourcesFile then found independently: the scheme test alone let
// "https:" through, and IndexRef.URL concatenated it into
// "https:/dists/0/0/binary-amd64/Packages.gz" — a URL with no host in it.
//
// Not a security hole: http.NewRequestWithContext refuses a URL with no host,
// so nothing was fetched. It is the other failure this file's header names,
// and the worse one — reporting a source as usable and then producing a
// nonsense request instead of a problem the operator can act on.
func TestIndexRefsRefusesAnHTTPURIWithNoHost(t *testing.T) {
	cases := []struct {
		uri  string
		kind SourceProblemKind
	}{
		// The three minimised reproducers, verbatim.
		{"https:", SourceProblemMalformed},
		{"http:", SourceProblemMalformed},
		{"http:0", SourceProblemMalformed},
		// The same hole in its other shapes.
		{"https:/", SourceProblemMalformed},
		{"https://", SourceProblemMalformed},
		{"https:///path", SourceProblemMalformed},
		{"https://?q", SourceProblemMalformed},
		{"https://#f", SourceProblemMalformed},
		{"https://arch ive.example/ubuntu", SourceProblemMalformed},
		{"https://arch\tive.example/ubuntu", SourceProblemMalformed},
		{"https://arch\x00ive.example/ubuntu", SourceProblemMalformed},
		// Still an unsupported scheme, not a malformed URI: the operator does
		// a different thing about each, so they must not collapse.
		{"cdrom:[Debian-12]/", SourceProblemUnsupportedScheme},
		{"/srv/mirror", SourceProblemUnsupportedScheme},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			tgt := Target{
				Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
				DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
				Sources: []Source{{
					Types: []string{"deb"}, URIs: []string{tc.uri},
					Suites: []string{"bookworm"}, Components: []string{"main"},
				}},
			}
			refs, problems := tgt.IndexRefsWithProblems()
			if len(refs) != 0 {
				t.Fatalf("built %s from %q", refs[0].URL(), tc.uri)
			}
			if len(problems) != 1 || problems[0].Kind != tc.kind {
				t.Fatalf("problems = %+v, want one %q", problems, tc.kind)
			}
			// A malformed URI is the operator's to fix and must not be filed
			// away as a deliberate scope limit.
			if got := problems[0].Deliberate(); got != (tc.kind == SourceProblemUnsupportedScheme) {
				t.Errorf("Deliberate() = %v for kind %q", got, tc.kind)
			}
		})
	}
}

// TestFetchableURIAcceptsRealArchiveURIs is the other half: the check must not
// have become so strict that a real target stops working. Every URI here is
// one a stock base or a captured snapshot actually carries.
func TestIGFetchableURIAcceptsRealArchiveURIs(t *testing.T) {
	for _, uri := range []string{
		"https://deb.debian.org/debian",
		"http://archive.ubuntu.com/ubuntu",
		"https://security.ubuntu.com/ubuntu/",
		"http://10.0.0.1:8080/ubuntu",
		"https://user:pass@mirror.example/ubuntu",
		"https://[2001:db8::1]/ubuntu",
		"https://ppa.launchpadcontent.net/team/ppa/ubuntu",
		"HTTPS://Archive.Example/Ubuntu",
	} {
		if _, malformed, ok := igFetchableURI(uri); !ok || malformed {
			t.Errorf("igFetchableURI(%q) refused a real archive URI (malformed=%v)", uri, malformed)
		}
	}
}

// TestSourceProblemStringsAreRenderable is what FuzzSecSourcesDeb822,
// FuzzSecSourcesOneLine and FuzzSecSourcesFile found on their second run, once
// the no-host URI defect was out of the way: SourceProblem carries two strings
// a UI renders, and only Text was sanitised. An "Architectures:" field holding
// invalid UTF-8 reached Reason verbatim through fmt.Sprintf.
//
// Not a security hole — encoding/json substitutes U+FFFD rather than failing,
// so the operator saw a replacement character in a sentence — but it is the
// same property cacheClip already enforces for the catalogue's own strings,
// and two halves of one package disagreeing about it is the shape this
// codebase treats as a defect.
func TestSourceProblemStringsAreRenderable(t *testing.T) {
	// 0xff is never valid UTF-8 in any position, and 0x07 is a control
	// character that would otherwise reach a terminal.
	docs := map[string]string{
		"deb822 Architectures": "Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\nArchitectures: \xffbogus\x07\n",
		"deb822 URI":           "Types: deb\nURIs: \xffhttps://x.example/d\nSuites: s\nComponents: main\n",
		"deb822 Types":         "Types: \xffdeb\nURIs: https://x.example/d\nSuites: s\nComponents: main\n",
		"one-line arch option": "deb [arch=\xffbogus] https://x.example/d s main\n",
		"one-line whole line":  "deb \xff\xfe\xfd\n",
		"deb822 unknown field": "Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\n\xffWeird: \xfe\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				grammar string
				parse   func(string, []byte) ([]SourceEntry, []SourceProblem)
			}{
				{"deb822", ParseSourcesDeb822},
				{"one-line", ParseSourcesOneLine},
			} {
				// The file name is untrusted too: it comes from a snapshot's
				// own document, and it is interpolated into every problem.
				entries, problems := tc.parse("/etc/apt/\xffsources.list", []byte(doc))
				_, archProblems := SourcesForArch("amd64", entries)
				problems = append(problems, archProblems...)

				for i, p := range problems {
					for what, v := range map[string]string{"Reason": p.Reason, "Text": p.Text, "File": p.File} {
						if !utf8.ValidString(v) {
							t.Errorf("%s: problem %d has invalid UTF-8 in %s: %q", tc.grammar, i, what, v)
						}
					}
					if !utf8.ValidString(p.String()) {
						t.Errorf("%s: problem %d renders to invalid UTF-8", tc.grammar, i)
					}
					if strings.ContainsRune(p.Reason, 0x07) || strings.ContainsRune(p.File, 0x07) {
						t.Errorf("%s: problem %d carries a control character into a rendered string", tc.grammar, i)
					}
				}
				if s := SourceProblemsSummary(problems); !utf8.ValidString(s) {
					t.Errorf("%s: summary is not valid UTF-8: %q", tc.grammar, s)
				}
			}
		})
	}
}

// TestSourceProblemReasonIsBounded: an Architectures field is as long as the
// file allows, and it used to be interpolated into a sentence whole. The
// sentence is shown to a person.
func TestSourceProblemReasonIsBounded(t *testing.T) {
	long := strings.Repeat("riscv64 ", 4096)
	doc := "Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\nArchitectures: " + long + "\n"
	entries, _ := ParseSourcesDeb822("x.sources", []byte(doc))
	_, problems := SourcesForArch("amd64", entries)
	if len(problems) != 1 {
		t.Fatalf("problems = %d, want 1", len(problems))
	}
	// The sentence's own words plus a clipped fragment; nowhere near 32 KB.
	if n := len(problems[0].Reason); n > 400 {
		t.Errorf("reason is %d bytes; an untrusted field is being interpolated whole", n)
	}
}

// TestIndexRefsAcceptsAMixedCaseScheme records a case the fuzzer found and
// that turned out NOT to be a defect, so that the next person to read
// igURIScheme does not "fix" it.
//
// FuzzSecSources* produced "httP://0/dists/..." and an early version of the
// property rejected it with strings.HasPrefix. URI schemes are
// case-insensitive (RFC 3986 §3.1), net/url lower-cases the scheme, and
// http.NewRequestWithContext turns that string into a correct request to host
// "0" — measured. igURIScheme lower-cases before consulting the allow-list for
// exactly this reason, and the URI is then kept verbatim because it is what
// the operator's own file said.
func TestIndexRefsAcceptsAMixedCaseScheme(t *testing.T) {
	for _, uri := range []string{
		"httP://archive.example/ubuntu",
		"HTTP://archive.example/ubuntu",
		"HtTpS://archive.example/ubuntu",
	} {
		t.Run(uri, func(t *testing.T) {
			tgt := Target{
				Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
				DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
				Sources: []Source{{
					Types: []string{"deb"}, URIs: []string{uri},
					Suites: []string{"noble"}, Components: []string{"main"},
				}},
			}
			refs, problems := tgt.IndexRefsWithProblems()
			if len(problems) != 0 {
				t.Fatalf("refused a case-insensitive scheme: %+v", problems)
			}
			if len(refs) == 0 {
				t.Fatal("no refs")
			}
			// The URI is kept as the file wrote it: this is what the operator
			// will search their sources for, and apt does not rewrite it
			// either.
			if !strings.HasPrefix(refs[0].URL(), uri) {
				t.Errorf("URL %q does not begin with the URI %q as written", refs[0].URL(), uri)
			}
		})
	}
}

// TestIndexRefsRefusesAComponentThatCannotFormAURL is the fourth thing
// FuzzSecSources* found, and the last of the URL-shape family: a scheme and an
// authority are not enough, because the suite and the component become path
// segments and a segment can still make the finished URL unparseable.
//
// A component named "%" produces ".../dists/0/%/binary-amd64/Packages.gz",
// which url.Parse refuses with `invalid URL escape "%/b"` — so
// http.NewRequestWithContext would have returned that from inside the fetch.
// A network-shaped error for a malformed source is the exact complaint
// docs/security-review.md §3.1 made about the local schemes, arriving by a
// different route.
func TestIndexRefsRefusesAComponentThatCannotFormAURL(t *testing.T) {
	cases := []struct {
		name            string
		suite, comp     string
		wantRefs        int
		wantProblemKind SourceProblemKind
	}{
		{name: "a bare percent in a component", suite: "noble", comp: "%", wantProblemKind: SourceProblemMalformed},
		{name: "a bare percent in a suite", suite: "no%ble", comp: "main", wantProblemKind: SourceProblemMalformed},
		{name: "a truncated escape", suite: "noble", comp: "ma%2", wantProblemKind: SourceProblemMalformed},
		// A complete escape is legal and must survive: nothing here is
		// entitled to reject a component an archive really publishes.
		{name: "a complete escape is legal", suite: "noble", comp: "ma%20in", wantRefs: 2},
		{name: "an ordinary component", suite: "noble", comp: "universe", wantRefs: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tgt := Target{
				Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
				DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
				Sources: []Source{{
					Types: []string{"deb"}, URIs: []string{"https://archive.example/ubuntu"},
					Suites: []string{tc.suite}, Components: []string{tc.comp},
				}},
			}
			refs, problems := tgt.IndexRefsWithProblems()
			if len(refs) != tc.wantRefs {
				t.Fatalf("refs = %d, want %d (%v)", len(refs), tc.wantRefs, refs)
			}
			if tc.wantProblemKind == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %+v, want none", problems)
				}
				return
			}
			// One problem, not two: the operator has one line to fix and does
			// not need to be told about it once per index kind.
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want exactly one", problems)
			}
			if problems[0].Kind != tc.wantProblemKind {
				t.Errorf("kind = %q, want %q", problems[0].Kind, tc.wantProblemKind)
			}
			if problems[0].Deliberate() {
				t.Error("a malformed suite or component is the operator's to fix, not a documented scope limit")
			}
			// Every ref that did survive must be one an HTTP client accepts.
			for _, r := range refs {
				if _, err := url.Parse(r.URL()); err != nil {
					t.Errorf("surviving ref %q does not parse: %v", r.URL(), err)
				}
			}
		})
	}
}

// TestIndexRefsRefusesAnAuthorityThatIsNotAHost is the fifth thing
// FuzzSecSources* found, and it beat the hand-written authority check that the
// first no-host finding produced: "http://@" has a non-empty authority made
// entirely of a userinfo separator, and url.Parse gives it an empty Host.
//
// The lesson is the one this file has now learned three times: validate
// through the parser the request will use, not through a copy of its rules.
func TestIndexRefsRefusesAnAuthorityThatIsNotAHost(t *testing.T) {
	for _, uri := range []string{
		"http://@",
		"http://@/ubuntu",
		"https://@/ubuntu",
		"http://a b/ubuntu",   // a space in the host
		"http://a\tb/ubuntu",  // a control character anywhere
		"https://x%zz/ubuntu", // a truncated escape
	} {
		t.Run(uri, func(t *testing.T) {
			tgt := Target{
				Kind: TargetSnapshot, SnapshotPath: "x.tar.zst",
				DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
				Sources: []Source{{
					Types: []string{"deb"}, URIs: []string{uri},
					Suites: []string{"bookworm"}, Components: []string{"main"},
				}},
			}
			refs, problems := tgt.IndexRefsWithProblems()
			if len(refs) != 0 {
				t.Fatalf("built %s from %q", refs[0].URL(), uri)
			}
			if len(problems) != 1 || problems[0].Kind != SourceProblemMalformed {
				t.Fatalf("problems = %+v, want one malformed", problems)
			}
		})
	}
}

// TestOneLineParserRefusesAWhitespaceURI: srcCutField splits on space and tab,
// which is what apt's own one-line parser does, and Go's notion of whitespace
// is wider. "deb \v suite comp" therefore left a vertical tab standing as the
// archive URI, and it reached IndexRefs and was dropped there in silence —
// the one failure this package's header calls unacceptable. Found by
// FuzzSecSourcesOneLine.
func TestOneLineParserRefusesAWhitespaceURI(t *testing.T) {
	cases := []struct {
		doc  string
		want SourceProblemKind
	}{
		{"dEB \v 0 0\n", SourceProblemNoURI},
		{"deb \f suite main\n", SourceProblemNoURI},
		{"deb   suite main\n", SourceProblemNoURI},
		{"deb https://x.example/d \v main\n", SourceProblemNoSuite},
	}
	for _, tc := range cases {
		t.Run(strconv.Quote(tc.doc), func(t *testing.T) {
			entries, problems := ParseSourcesOneLine("/etc/apt/sources.list", []byte(tc.doc))
			if len(entries) != 0 {
				t.Fatalf("produced %d entries: %+v", len(entries), entries)
			}
			if len(problems) != 1 || problems[0].Kind != tc.want {
				t.Fatalf("problems = %+v, want one %q", problems, tc.want)
			}
		})
	}

	// And the ordinary line still parses: the fix must not have made the
	// splitter reject a real sources.list.
	entries, problems := ParseSourcesOneLine("/etc/apt/sources.list",
		[]byte("deb\thttps://deb.debian.org/debian\tbookworm\tmain contrib\n"))
	if len(entries) != 1 || len(problems) != 0 {
		t.Fatalf("a tab-separated line no longer parses: entries=%+v problems=%+v", entries, problems)
	}
}
