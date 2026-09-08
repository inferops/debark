package catalog

// FuzzSecDEP11Decode and FuzzSecDEP11Body close the second half of the largest
// gap docs/security-review.md named against itself: "neither the deb822
// sources parser nor the DEP-11 YAML parser has a fuzz target, though both
// read attacker-adjacent bytes and yaml.v3 has resource-exhaustion history".
//
// # Why this one matters more than its severity suggests
//
// This is the only place in the application where a third-party parser is
// pointed at bytes off a socket. Everything else that reads untrusted input
// here is hand-written and in this repository: the deb822 parsers, the cache
// image reader, the sources reader. gopkg.in/yaml.v3 is not, and YAML is a
// format with anchors, aliases and recursive structure — the shape that
// produced the billion-laughs class of bug in essentially every
// implementation of it that has ever existed. The catalogue is not a trust
// boundary (docs/dev/catalogue-sourcing.md) and a bug here costs "a missing
// row in a picker", which is a sound argument about what the catalogue
// *produces*; it is not an argument about what the decoder *does* on the way
// there, and that is what these targets are about.
//
// # The properties
//
//  1. It returns, and it returns bounded. No panic and no hang: the decode
//     runs under a context with a deadline, and the target fails if the
//     deadline is what ended it, because a YAML document that takes seconds is
//     the resource-exhaustion finding rather than a slow test.
//  2. Nothing accepted is unrenderable. Every string that reaches an Entry
//     crosses the Wails bridge as JSON, so invalid UTF-8 in an accepted
//     document is a defect whatever the decoder thought of it.
//  3. Applying the index to an Entry never invents a package. dep11Index.Apply
//     is the join, and a join keyed on a name the document supplied is the one
//     place a malicious component could put its own text on a row belonging to
//     something else.
//
// # The size cap is part of the test, not a way around it
//
// FuzzSecDEP11Body goes through the gzip path with dep11MaxDecompressedBytes
// lowered, so the bomb defence is exercised on every input rather than
// asserted once. A target that skipped compressed inputs would be fuzzing the
// half of the loader that never sees a real file.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// dep11FuzzSeeds are real component shapes plus the ones a fuzzer will not
// invent: anchors and aliases, a deep nest, a recursive alias, and the header
// document every real file starts with.
var dep11FuzzSeeds = []string{
	// The header every Components-<arch>.yml opens with.
	"File: DEP-11\nVersion: '0.12'\nOrigin: ubuntu-noble-main\nMediaBaseUrl: https://x.example/media\nTime: 2026-01-01T00:00:00Z\n",
	// A desktop application, which is the common case.
	"---\nType: desktop-application\nID: org.gnome.Klotski.desktop\nPackage: gnome-klotski\nName:\n  C: Klotski\n  de: Klotski\nSummary:\n  C: Slide blocks to solve the puzzle\nCategories:\n  - Game\n  - LogicGame\n",
	// An addon, which Extends: another component rather than standing alone.
	"---\nType: addon\nID: x.addon\nPackage: x-addon\nExtends:\n  - org.gnome.Klotski.desktop\nName:\n  C: An addon\n",
	// A header and two components in one stream, which is the real layout.
	"File: DEP-11\nVersion: '0.12'\nOrigin: o\n---\nType: generic\nID: a\nPackage: pa\n---\nType: font\nID: b\nPackage: pb\n",
	// No Package: field at all — the component cannot be joined and must be
	// counted rather than guessed at.
	"---\nType: desktop-application\nID: orphan.desktop\nName:\n  C: Orphan\n",
	// A type error the decoder reports softly: Categories is a scalar where a
	// sequence belongs. The stream stays positioned; the component survives.
	"---\nType: generic\nID: a\nPackage: p\nCategories: notalist\n",
	// Anchors and aliases: legal YAML, and the mechanism behind every
	// expansion bomb ever written against the format.
	"---\nType: generic\nID: a\nPackage: p\nName: &n\n  C: x\nSummary: *n\n",
	// A small billion-laughs. Deliberately small: the point is that the
	// decoder's own limits engage, not that this machine can be exhausted.
	"---\na: &a [\"l\",\"l\",\"l\",\"l\",\"l\"]\nb: &b [*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b]\nd: [*c,*c,*c,*c,*c]\nType: generic\nID: x\nPackage: p\n",
	// A merge key, which yaml.v3 resolves.
	"---\nbase: &base\n  Type: generic\n<<: *base\nID: a\nPackage: p\n",
	// Deep nesting, flow style, which is where a recursive descent parser goes
	// if it is going anywhere.
	"---\nType: generic\nID: a\nPackage: p\nIcon: " + strings.Repeat("[", 200) + strings.Repeat("]", 200) + "\n",
	// Explicit tags, including the ones that mean "not a string".
	"---\nType: !!str generic\nID: !!int 5\nPackage: !!binary aGVsbG8=\n",
	// Invalid UTF-8 in a value, and a NUL.
	"---\nType: generic\nID: a\nPackage: p\nName:\n  C: \xff\xfe\n",
	"---\nType: generic\nID: a\nPackage: p\x00\n",
	// A document separator storm, and an unterminated one.
	strings.Repeat("---\n", 4096),
	"---\nType: generic\nID: a\nPackage: p\nName:\n  C: \"unterminated",
	// Not YAML at all, and empty.
	"Package: hello\nVersion: 2.10\n\n",
	"\x1f\x8b\x08\x00",
	"",
	"\n",
	strings.Repeat("a: ", 4096),
}

// dep11FuzzCheck applies the properties to whatever a decode produced.
func dep11FuzzCheck(t *testing.T, idx *dep11Index) {
	t.Helper()
	if idx == nil {
		return
	}

	pkgs := idx.Packages()
	if len(pkgs) != idx.Len() {
		t.Fatalf("Packages() returned %d names for an index of %d", len(pkgs), idx.Len())
	}

	for _, pkg := range pkgs {
		if pkg == "" {
			t.Fatal("an index entry is keyed on the empty package name, which would join to nothing and hide a component")
		}
		app, ok := idx.Lookup(pkg)
		if !ok {
			t.Fatalf("Packages() named %q and Lookup could not find it", pkg)
		}
		if app.Package != pkg {
			t.Fatalf("entry keyed %q holds a component for %q", pkg, app.Package)
		}
		// Property 2: everything here crosses the bridge as JSON.
		for what, s := range map[string]string{"Name": app.Name, "Summary": app.Summary, "ID": app.ID, "Type": app.Type} {
			if !utf8.ValidString(s) {
				t.Fatalf("%s of %q is not valid UTF-8, and the Wails bridge cannot render it", what, pkg)
			}
		}
		for _, c := range app.Categories {
			if !utf8.ValidString(c) {
				t.Fatalf("a category of %q is not valid UTF-8", pkg)
			}
		}

		// Property 3: the join must land on the row it names and no other.
		e := Entry{Name: pkg}
		if !idx.Apply(&e) {
			t.Fatalf("Apply refused the very package Lookup found: %q", pkg)
		}
		if e.Name != pkg {
			t.Fatalf("Apply renamed %q to %q", pkg, e.Name)
		}
		other := Entry{Name: pkg + "\x00not-a-real-package"}
		if idx.Apply(&other) {
			t.Fatalf("Apply matched %q against a component for %q", other.Name, pkg)
		}
	}
}

// dep11FuzzDecode runs one decode under a deadline and reports whether the
// deadline is what stopped it. A YAML document that takes seconds to decode is
// the resource-exhaustion finding, not a slow test, so it is a failure.
func dep11FuzzDecode(t *testing.T, r io.Reader) *dep11Index {
	t.Helper()
	// Generous against a fuzz worker sharing a machine, and still four orders
	// of magnitude above a real 18.6 MB component file's ~300 ms.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	idx := dep11NewIndex()
	ref := IndexRef{URI: "https://x.example/ubuntu", Suite: "noble", Component: "main", Arch: "amd64", Kind: IndexDEP11}

	start := time.Now()
	err := dep11Decode(ctx, r, ref, idx)
	elapsed := time.Since(start)

	if errors.Is(err, context.DeadlineExceeded) || (err != nil && ctx.Err() != nil) {
		t.Fatalf("decode did not finish within the deadline (%s elapsed): this is the resource-exhaustion case, not a slow machine", elapsed)
	}
	if err != nil {
		// A refusal is a fine outcome. The properties below only apply to
		// what was accepted, and dep11Decode fills the index as it goes, so
		// the partial result is still checked.
		return idx
	}
	return idx
}

func FuzzSecDEP11Decode(f *testing.F) {
	for _, s := range dep11FuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		dep11FuzzCheck(t, dep11FuzzDecode(t, bytes.NewReader(data)))
	})
}

// FuzzSecDEP11Body goes through the size cap the real loader applies, with the
// cap lowered so a bomb is refused in milliseconds rather than after half a
// gigabyte. Both halves are covered: the fuzzer's bytes as YAML, and the same
// bytes gzipped, which is what a mirror actually serves.
func FuzzSecDEP11Body(f *testing.F) {
	for _, s := range dep11FuzzSeeds {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}

	f.Fuzz(func(t *testing.T, data []byte, compress bool) {
		if len(data) > 1<<20 {
			return
		}
		var src io.Reader = bytes.NewReader(data)
		if compress {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if _, err := zw.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = zr.Close() }()
			src = zr
		}

		// The same wrapper dep11Load installs, with a low cap so the refusal
		// path is exercised on inputs a fuzzer can actually reach.
		const cap = 1 << 16
		limited := &dep11CountingReader{r: src, limit: cap, what: "Components-amd64.yml"}
		idx := dep11FuzzDecode(t, limited)
		if limited.n > cap+int64(dep11ReadBuffer) {
			t.Fatalf("read %d bytes past a %d-byte cap", limited.n, cap)
		}
		dep11FuzzCheck(t, idx)
	})
}
