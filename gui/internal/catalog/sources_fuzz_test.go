package catalog_test

// FuzzSecSourcesDeb822, FuzzSecSourcesOneLine and FuzzSecSourcesFile close the
// first half of the largest gap docs/security-review.md named against itself:
// "neither the deb822 sources parser nor the DEP-11 YAML parser has a fuzz
// target, though both read attacker-adjacent bytes".
//
// # Why these bytes are attacker-adjacent
//
// A snapshot is untrusted input by construction — a file produced on another
// machine and carried across the air gap — and its captured sources files are
// the first thing ResolveSnapshot parses. Everything the catalogue then does
// follows from what this parser returned: which archives are contacted, which
// suites and components appear in the cache key, and what the operator is told
// about the sources that were skipped. A parser that could be made to panic
// here would take the application down on "open a file"; one that could be
// made to invent an archive URI would have it fetching somewhere the target
// never named, which sources.go's own header calls the worse of the two
// failures.
//
// # The properties, and why they are these
//
// Three, and all three are structural rather than about any particular input,
// because a fuzzer's value is entirely in the properties that hold for every
// input rather than in the ones a human thought to write down.
//
//  1. It returns. No panic, no unbounded allocation, no hang. This is the one
//     docs/dev/cache-format.md §5.1 states for the cache loader and it is just
//     as load-bearing here.
//  2. Every Source it returns is complete. sources.go's second rule is that a
//     wrong answer is worse than a missing one, and a Source with no URI, no
//     suite or no component is a wrong answer: IndexRefs would build a
//     nonsense path out of it, or silently drop it, and either way the
//     operator is told a source was used when it was not.
//  3. Every URI it returns is fetchable, or is not returned at all. This is
//     the standing guard on the allow-list added for §3.1 — the property is
//     asserted against IndexRefs' own filter rather than against a copy of the
//     rule, so the two cannot drift.
//
// Deliberately NOT asserted: that a well-formed document round-trips, or that
// the problem count matches anything. The parser is allowed to reject
// aggressively; the tests in sources_test.go are where the accepting behaviour
// is pinned, against fixtures that came off real machines.

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/inferops/debark/gui/internal/catalog"
)

// srcFuzzSeeds are the shapes worth starting from: the two real grammars, the
// adversarial cases sources_test.go names, and the ones that only a fuzzer's
// mutations reach usefully from a valid start.
var srcFuzzSeeds = []string{
	// A real deb822 stanza, and two of them.
	"Types: deb\nURIs: https://deb.debian.org/debian\nSuites: bookworm\nComponents: main\n",
	"Types: deb deb-src\nURIs: http://archive.ubuntu.com/ubuntu\nSuites: noble noble-updates\nComponents: main universe\nArchitectures: amd64\n\nTypes: deb\nURIs: https://security.ubuntu.com/ubuntu\nSuites: noble-security\nComponents: main\n",
	// One-line format, with and without options.
	"deb https://deb.debian.org/debian bookworm main\n",
	"deb [arch=amd64 signed-by=/usr/share/keyrings/x.gpg] http://archive.ubuntu.com/ubuntu noble main universe\n",
	"deb-src https://deb.debian.org/debian bookworm main\n",
	// A flat repository: no dists/ hierarchy, so no components.
	"deb https://vendor.example/repo/ /\n",
	// The schemes §3.1 drove by hand.
	"Types: deb\nURIs: file:/var/lib/mirror\nSuites: noble\nComponents: main\n",
	"Types: deb\nURIs: cdrom:[Debian-12]/\nSuites: bookworm\nComponents: main\n",
	"Types: deb\nURIs: mirror+file:/etc/apt/mirrors.txt\nSuites: noble\nComponents: main\n",
	"deb ftp://archive.example/ubuntu noble main\n",
	// An unexpanded template placeholder, which ResolveBase turns into a hard
	// error and which must therefore be reachable from here.
	"Types: deb\nURIs: ${MIRROR}\nSuites: ${SUITE}\nComponents: main\n",
	// Switched off, and restricted to another architecture.
	"Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\nEnabled: no\n",
	"Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\nArchitectures: riscv64\n",
	// Structural nastiness: a continuation line, a field given twice, a
	// paragraph with none of the four fields, CRLF, a BOM, and a NUL.
	"Types: deb\nURIs: https://x.example/d\n  continued\nSuites: s\nComponents: main\n",
	"Types: deb\nTypes: deb-src\nURIs: https://x.example/d\nSuites: s\nComponents: main\n",
	"Origin: Somebody\nLabel: Something\n",
	"Types: deb\r\nURIs: https://x.example/d\r\nSuites: s\r\nComponents: main\r\n",
	"\xef\xbb\xbfTypes: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\n",
	"Types: deb\x00\nURIs: https://x.example/d\nSuites: s\nComponents: main\n",
	// Not a sources file at all: the case the size limit and the malformed
	// counter exist for.
	"Package: hello\nVersion: 2.10\nArchitecture: amd64\n\n",
	"",
	":",
	"\n\n\n\n",
	strings.Repeat("URIs: https://x.example/d\n", 2048),
	strings.Repeat("a", 4096),
}

// srcFuzzCheck is the three properties, applied to whatever a parser returned.
func srcFuzzCheck(t *testing.T, name string, sources []catalog.Source, problems []catalog.SourceProblem) {
	t.Helper()

	for i, s := range sources {
		// Property 2: a returned Source is complete. A flat repository is the
		// one legitimate shape with no components, and IndexRefs skips it, so
		// it is excluded from the component half rather than from all of it.
		if len(s.Types) == 0 {
			t.Fatalf("%s: source %d has no types: %+v", name, i, s)
		}
		if len(s.URIs) == 0 {
			t.Fatalf("%s: source %d has no URI: %+v", name, i, s)
		}
		for _, u := range s.URIs {
			if strings.TrimSpace(u) == "" {
				t.Fatalf("%s: source %d has a blank URI: %+v", name, i, s)
			}
		}
		if len(s.Suites) == 0 {
			t.Fatalf("%s: source %d has no suite: %+v", name, i, s)
		}
		flat := false
		for _, su := range s.Suites {
			if strings.HasSuffix(su, "/") || strings.Contains(su, "/") {
				flat = true
			}
		}
		if !flat && len(s.Components) == 0 {
			t.Fatalf("%s: source %d names a dists/ suite but no component: %+v", name, i, s)
		}
	}

	if summary := catalog.SourceProblemsSummary(problems); !utf8.ValidString(summary) {
		t.Fatalf("%s: the problem summary is not valid UTF-8: %q", name, summary)
	}

	// Property 3: whatever survived, no ref may name a scheme the fetcher is
	// not allowed to use. Asserted through the same IndexRefs the fetcher
	// calls, so this cannot drift from the rule it is guarding.
	tgt := catalog.Target{
		Kind: catalog.TargetSnapshot, SnapshotPath: "fuzz.tar.zst",
		DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
		Sources: sources,
	}
	refs, schemeProblems := tgt.IndexRefsWithProblems()
	for _, r := range refs {
		raw := r.URL()
		// Parsed with net/url rather than tested with strings.HasPrefix,
		// because net/url is what http.NewRequestWithContext uses and the
		// property that matters is "an HTTP client will send this to a real
		// host", not "this string starts with http://".
		//
		// The difference is not academic: this target's third run produced
		// "httP://0/dists/...", which a prefix test rejects and which Go
		// accepts, lower-cases and sends correctly — schemes are
		// case-insensitive per RFC 3986. That was a wrong assertion, not a
		// defect, and it is written down here so it is not "fixed" back.
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: IndexRefs built %q, which does not parse as a URL: %v", name, raw, err)
		}
		if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
			t.Fatalf("%s: IndexRefs built %q, whose scheme is %q", name, raw, scheme)
		}
		if u.Host == "" {
			t.Fatalf("%s: IndexRefs built %q, which names no host", name, raw)
		}
	}
	for _, p := range schemeProblems {
		// Two kinds only, and the split matters: an unsupported scheme is a
		// documented scope limit a UI shows quietly, and a malformed URI is a
		// broken line the operator can fix. Anything else here would mean a
		// URI was dropped for a reason nobody wrote a sentence for.
		if p.Kind != catalog.SourceProblemUnsupportedScheme && p.Kind != catalog.SourceProblemMalformed {
			t.Fatalf("%s: IndexRefsWithProblems returned kind %q", name, p.Kind)
		}
		if p.Reason == "" {
			t.Fatalf("%s: a dropped URI came back with no reason: %+v", name, p)
		}
		if p.Kind == catalog.SourceProblemUnsupportedScheme && !p.Deliberate() {
			t.Fatalf("%s: an unsupported scheme is a scope limit and must read as one: %+v", name, p)
		}
		if p.Kind == catalog.SourceProblemMalformed && p.Deliberate() {
			t.Fatalf("%s: a malformed URI is the operator's to fix and must not be hidden as a deliberate skip: %+v", name, p)
		}
	}

	// Every problem must carry the two fields a UI renders. A problem with no
	// reason is the wall.
	for i, p := range problems {
		if p.Kind == "" {
			t.Fatalf("%s: problem %d has no kind: %+v", name, i, p)
		}
		if p.Reason == "" {
			t.Fatalf("%s: problem %d has no reason: %+v", name, i, p)
		}
		// Every string a UI renders must survive the JSON bridge. This is the
		// property that found the Reason/Text asymmetry: encoding/json
		// substitutes U+FFFD for invalid UTF-8 silently rather than failing,
		// so nothing downstream would ever have complained.
		for what, v := range map[string]string{"Reason": p.Reason, "Text": p.Text, "File": p.File, "Kind": string(p.Kind), "Format": string(p.Format)} {
			if !utf8.ValidString(v) {
				t.Fatalf("%s: problem %d has invalid UTF-8 in %s: %+v", name, i, what, p)
			}
		}
		if p.String() == "" {
			t.Fatalf("%s: problem %d renders as nothing: %+v", name, i, p)
		}
		if !utf8.ValidString(p.String()) {
			t.Fatalf("%s: problem %d renders to invalid UTF-8: %+v", name, i, p)
		}
	}
}

func FuzzSecSourcesDeb822(f *testing.F) {
	for _, s := range srcFuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		entries, problems := catalog.ParseSourcesDeb822("/etc/apt/sources.list.d/fuzz.sources", data)
		sources, archProblems := catalog.SourcesForArch("amd64", entries)
		srcFuzzCheck(t, "deb822", sources, append(problems, archProblems...))
	})
}

func FuzzSecSourcesOneLine(f *testing.F) {
	for _, s := range srcFuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		entries, problems := catalog.ParseSourcesOneLine("/etc/apt/sources.list", data)
		sources, archProblems := catalog.SourcesForArch("amd64", entries)
		srcFuzzCheck(t, "one-line", sources, append(problems, archProblems...))
	})
}

// FuzzSecSourcesFile fuzzes the dispatcher as well as the two grammars,
// because which grammar a document is read with is decided by its *name*, and
// a snapshot supplies both the name and the bytes.
func FuzzSecSourcesFile(f *testing.F) {
	for _, s := range srcFuzzSeeds {
		f.Add("/etc/apt/sources.list.d/x.sources", []byte(s))
		f.Add("/etc/apt/sources.list", []byte(s))
	}
	f.Add("", []byte("deb https://x.example/d s main\n"))
	f.Add(strings.Repeat("../", 64)+"x.sources", []byte("Types: deb\n"))
	f.Add("x.SOURCES", []byte("Types: deb\nURIs: https://x.example/d\nSuites: s\nComponents: main\n"))

	f.Fuzz(func(t *testing.T, name string, data []byte) {
		if len(data) > 1<<20 || len(name) > 4096 {
			return
		}
		entries, problems := catalog.ParseSourcesFile(name, data)
		sources, archProblems := catalog.SourcesForArch("amd64", entries)
		srcFuzzCheck(t, "file", sources, append(problems, archProblems...))
	})
}
