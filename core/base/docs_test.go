package base

// The drift guard for docs/formats.md §3.13.
//
// Every other format on that page is held to its Go type by
// api/schema/schema_test.go, which compares the published JSON Schema against
// the struct in both directions. debark.base/v1 is the one format with no
// JSON Schema — deliberately: it is YAML an operator authors, not a document
// debark emits, hashes or signs — so that machinery has nothing to grip,
// and §3.13 could drift from core/base.Definition with nothing to say so.
//
// Two things can rot, and this file checks both:
//
//  1. The worked example. It was rendered by Marshal from the real builtin
//     table, and it is published as "exactly what the format accepts". A
//     changed seed, codename, description or archive URI silently turns that
//     claim false, and the reader who copies it gets a file describing a base
//     debark no longer has.
//
//  2. The field table. A field added to Definition and not to §3.13 is the
//     direction that actually happens — the struct is what a change touches,
//     the prose is what it forgets — and it leaves an operator with a
//     documented format narrower than the parser's.
//
// The example block is located by an explicit anchor comment in the Markdown
// rather than by line number or by "the first fence in the section": §3.13
// carries a second YAML example (an operator's own file, with placeholders
// left unsubstituted) which must NOT be compared against the encoder, and
// line numbers move every time a paragraph above is edited.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// formatsDoc is the page under test, relative to this package's directory.
const formatsDoc = "../../docs/formats.md"

// baseSectionHeading starts §3.13. Matched as a prefix, so the rest of the
// heading text can be reworded without breaking the scan.
const baseSectionHeading = "### 3.13 "

// generatedAnchor marks the fenced block that was produced by Marshal. It is
// a comment in the Markdown, so it is invisible when rendered and impossible
// to move by accident: the block it introduces is the one directly below it.
const generatedAnchor = "<!-- debark:generated"

// readFormatsSection returns the lines of the section that starts with
// heading, up to but not including the next heading at the same level or
// above.
//
// Fenced code blocks are tracked, because a heading-looking line inside a
// fence is content, not a heading — a YAML example may legitimately contain
// a "###" comment, and a scan that ended the section there would silently
// truncate what the checks below see.
func readFormatsSection(t *testing.T, heading string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(formatsDoc))
	if err != nil {
		t.Fatalf("read %s: %v", formatsDoc, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	start := -1
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(l, heading) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no section starting %q; the heading was renumbered or reworded, and this guard "+
			"and every cross-reference to it need updating together", formatsDoc, heading)
	}

	end := len(lines)
	inFence = false
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "### ") {
			end = i
			break
		}
	}
	return lines[start:end]
}

// anchoredFencedBlock returns the body of the first fenced block that follows
// the anchor line, without the fence markers.
func anchoredFencedBlock(t *testing.T, lines []string, anchor string) string {
	t.Helper()
	at := -1
	for i, l := range lines {
		if strings.Contains(l, anchor) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("%s §3.13: no %q anchor comment; the generated example must be marked so this guard "+
			"cannot compare the wrong block", formatsDoc, anchor)
	}
	open := -1
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			open = i
			break
		}
	}
	if open < 0 {
		t.Fatalf("%s §3.13: the %q anchor is followed by no fenced block", formatsDoc, anchor)
	}
	for i := open + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			return strings.Join(lines[open+1:i], "\n")
		}
	}
	t.Fatalf("%s §3.13: the generated example's fence is never closed", formatsDoc)
	return ""
}

// TestDocsBaseExampleMatchesEncoder holds the worked example to what
// Marshal actually produces for the base it claims to show.
func TestDocsBaseExampleMatchesEncoder(t *testing.T) {
	const id, arch = "ubuntu:26.04/desktop", "amd64"

	d, err := Lookup(id, arch)
	if err != nil {
		t.Fatalf("Lookup(%q, %q): %v", id, arch, err)
	}
	rendered, err := Marshal(d)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", id, err)
	}
	want := strings.TrimRight(string(rendered), "\n")

	got := anchoredFencedBlock(t, readFormatsSection(t, baseSectionHeading), generatedAnchor)
	if got == want {
		return
	}
	t.Errorf("%s §3.13: the worked example no longer matches base.Marshal(base.Lookup(%q, %q)).\n"+
		"The page publishes it as exactly what the format accepts, so it must be regenerated.\n"+
		"Replace the fenced block below the %q anchor with, verbatim:\n\n"+
		"```yaml\n%s\n```\n\nIt currently reads:\n\n```yaml\n%s\n```",
		formatsDoc, id, arch, generatedAnchor, want, got)
}

// backticked pulls `name` tokens out of one Markdown table cell.
var backticked = regexp.MustCompile("`([^`]+)`")

// documentedBaseFields reads the first Markdown table — the field
// table — and returns the field names its first column names, with any
// trailing "[]" (the page's mark for an array) removed.
func documentedBaseFields(t *testing.T, lines []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	started := false
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, "|") {
			if started {
				break // the field table has ended; a later table is a different subject
			}
			continue
		}
		started = true
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if len(cells) == 0 {
			continue
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[0], -1) {
			out[strings.TrimSuffix(m[1], "[]")] = true
		}
	}
	if !started {
		t.Fatalf("%s §3.13: no field table found", formatsDoc)
	}
	return out
}

// TestDocsBaseFieldTableMatchesDefinition holds the field table to the Go
// type, in both directions.
//
// The struct-to-docs direction is the one that catches the change that
// actually happens: a field added to Definition and not written up. The
// docs-to-struct direction catches its opposite — a field removed or renamed
// and left documented — which is worse for a reader, because a documented
// field the parser has never heard of is refused as an unknown key.
func TestDocsBaseFieldTableMatchesDefinition(t *testing.T) {
	documented := documentedBaseFields(t, readFormatsSection(t, baseSectionHeading))

	rt := reflect.TypeOf(Definition{})
	declared := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never serialised, so never documented
		}
		tag, ok := f.Tag.Lookup("yaml")
		if !ok {
			t.Errorf("Definition.%s has no yaml tag, so it cannot round-trip through an operator's file", f.Name)
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		declared[name] = true
		if !documented[name] {
			t.Errorf("%s §3.13: Definition.%s serialises as %q but has no row in the field table. "+
				"A field the parser accepts and the reference does not mention is a format only the code knows.",
				formatsDoc, f.Name, name)
		}
	}
	for name := range documented {
		if !declared[name] {
			t.Errorf("%s §3.13: the field table documents %q, which no field of base.Definition serialises as. "+
				"The parser refuses unknown keys, so an operator following the page would get an error.",
				formatsDoc, name)
		}
	}
}
