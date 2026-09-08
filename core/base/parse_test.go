package base

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/core/snapshot"
)

// knownDefect marks a test that asserts the behaviour core/base SHOULD have
// and currently does not.
//
// The brief for this package's tests is "report a defect, do not fix it", and
// the tree is expected to be green, so these skip by default rather than
// leaving a red suite behind for someone else to interpret. Run them with
// DEBARK_BASE_KNOWN_DEFECTS=1 and read the failures; each one names the
// non-test change that would fix it, which is not this file's to make.
func knownDefect(t *testing.T, summary string) {
	t.Helper()
	if os.Getenv("DEBARK_BASE_KNOWN_DEFECTS") == "" {
		t.Skipf("KNOWN DEFECT, skipped by default — set DEBARK_BASE_KNOWN_DEFECTS=1 to run: %s", summary)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	return p
}

// --- Marshal / LoadFile round trip ------------------------------------

// TestMarshalAndLoadFileRoundTrip covers the promise Marshal's doc makes: the
// YAML an operator would commit is read back by the same parser, so
// `list-bases --yaml` output and the docs' examples cannot drift from what
// LoadFile accepts. The digest is compared as well as the struct, because the
// digest is what a synthesized snapshot records — two definitions that compare
// equal but digest differently would make origin.source_digest a lie.
func TestMarshalAndLoadFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, arch := range distro.PrimaryArchitectures() {
		for i, id := range BuiltinIDs() {
			want, err := Lookup(id, arch)
			if err != nil {
				t.Fatalf("Lookup(%q, %q): %v", id, arch, err)
			}
			data, err := Marshal(want)
			if err != nil {
				t.Fatalf("Marshal(%s): %v", id, err)
			}
			path := writeFile(t, dir, arch+"-"+strconv.Itoa(i)+".yaml", string(data))

			got, err := LoadFile(path, arch)
			if err != nil {
				t.Errorf("LoadFile after Marshal(%s, %s): %v\n%s", id, arch, err, data)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s on %s did not round-trip:\n want %+v\n  got %+v", id, arch, want, got)
			}
			wantDigest, err := Digest(want)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			gotDigest, err := Digest(got)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if gotDigest != wantDigest {
				t.Errorf("%s on %s round-tripped to a different digest: %s vs %s", id, arch, gotDigest, wantDigest)
			}
		}
	}
}

// TestLoadFileReadsAHandWrittenFixture is the round-trip test's oracle.
//
// Marshalling a definition and loading it back proves the encoder and decoder
// agree with each other, and nothing more: a field both spelled wrongly in the
// same way would round-trip perfectly. testdata/acme-soe.yaml was typed out by
// hand, in a shape Marshal does not produce, so the values asserted here are
// values a reader can check against the file itself.
func TestLoadFileReadsAHandWrittenFixture(t *testing.T) {
	d, err := LoadFile(filepath.Join("testdata", "acme-soe.yaml"), "amd64")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if d.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q", d.SchemaVersion)
	}
	if d.ID != "acme:soe-2026/workstation" {
		t.Errorf("ID = %q", d.ID)
	}
	if d.Description != "ACME standard operating environment, 2026 refresh" {
		t.Errorf("Description = %q", d.Description)
	}
	if d.DistroID != "ubuntu" {
		t.Errorf("DistroID = %q", d.DistroID)
	}
	// The file quotes it. An unquoted 24.04 would be a YAML float, and
	// "24.04" is a string everywhere else in debark.
	if d.VersionID != "24.04" {
		t.Errorf("VersionID = %q", d.VersionID)
	}
	if d.Codename != "noble" {
		t.Errorf("Codename = %q", d.Codename)
	}
	if d.Variant != "workstation" {
		t.Errorf("Variant = %q, and an operator's own word must survive", d.Variant)
	}
	// The file pins no arch, so it accepts the one that was asked for.
	if d.Arch != "amd64" {
		t.Errorf("Arch = %q", d.Arch)
	}
	assertStrings(t, "Seeds", d.Seeds, []string{"acme-soe-workstation", "ubuntu-minimal"})
	if d.Recommends {
		t.Error("Recommends = true; the file says false")
	}
	// ${codename} in the file, substituted on load.
	wantSources := "Types: deb\n" +
		"URIs: http://apt.acme.example/ubuntu\n" +
		"Suites: noble noble-updates\n" +
		"Components: main acme\n" +
		"Signed-By: /usr/share/keyrings/acme-archive-keyring.gpg\n"
	if d.Sources != wantSources {
		t.Errorf("Sources =\n%q\nwant\n%q", d.Sources, wantSources)
	}
	assertStrings(t, "Keyrings", d.Keyrings, []string{"/usr/share/keyrings/acme-archive-keyring.gpg"})

	// The same file loaded for a second architecture differs only in Arch,
	// which is what "a file may leave arch empty to accept whatever is asked
	// for" has to mean.
	other, err := LoadFile(filepath.Join("testdata", "acme-soe.yaml"), "arm64")
	if err != nil {
		t.Fatalf("LoadFile arm64: %v", err)
	}
	if other.Arch != "arm64" {
		t.Errorf("arm64 load produced arch %q", other.Arch)
	}
	other.Arch = d.Arch
	if !reflect.DeepEqual(other, d) {
		t.Errorf("loading the same file for two architectures changed more than arch:\n%+v\n%+v", d, other)
	}
}

// --- LoadFile refusals -------------------------------------------------

const goodDefinitionYAML = `schema_version: debark.base/v1
id: acme:soe-2026/workstation
distro_id: ubuntu
version_id: "24.04"
codename: noble
seeds:
  - acme-soe-workstation
sources: |
  Types: deb
  URIs: http://apt.acme.example/ubuntu
  Suites: ${codename}
  Components: main
  Signed-By: /usr/share/keyrings/acme-archive-keyring.gpg
keyrings:
  - /usr/share/keyrings/acme-archive-keyring.gpg
`

// TestLoadFileRefusesAnUnknownField: a typo in a field name must not silently
// mean "the default". A misspelled `recommend: true` that quietly resolved
// with recommends off would produce a base whose claim differs from what the
// file says, and the operator would have no way to see it — so the diagnostic
// has to name the offending field, not just say the file is bad.
func TestLoadFileRefusesAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, field, yaml string }{
		{"a misspelled recommends", "recommend", goodDefinitionYAML + "recommend: true\n"},
		{"a field from another schema", "packages", goodDefinitionYAML + "packages:\n  - vim\n"},
		{"a field with the JSON name only", "distroId", goodDefinitionYAML + "distroId: debian\n"},
	} {
		p := writeFile(t, dir, strings.ReplaceAll(c.name, " ", "-")+".yaml", c.yaml)
		_, err := LoadFile(p, "amd64")
		requireUsage(t, err, c.name)
		if err != nil && !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: diagnostic %q does not name the field %q", c.name, err.Error(), c.field)
		}
	}
}

// TestLoadFileRefusesASecondDocument: a single Decode would silently ignore
// everything after the `---`, and "the part of my file after the --- did
// nothing" is a bad way to find out that half a definition was dropped.
func TestLoadFileRefusesASecondDocument(t *testing.T) {
	dir := t.TempDir()
	second := goodDefinitionYAML + "---\n" + strings.Replace(goodDefinitionYAML,
		"acme:soe-2026/workstation", "acme:soe-2026/server", 1)
	p := writeFile(t, dir, "two-documents.yaml", second)
	_, err := LoadFile(p, "amd64")
	requireUsage(t, err, "a file holding two definitions")
	if err != nil && !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("diagnostic %q does not explain that there is a second document", err.Error())
	}
}

// TestLoadFileRefusesASecondDocumentThatIsNotADefinition is the same rule
// applied to the case the current implementation misses. See the report.
func TestLoadFileRefusesASecondDocumentThatIsNotADefinition(t *testing.T) {
	knownDefect(t, "LoadFile detects a second YAML document only when that document happens to decode "+
		"into a Definition. The second Decode's error is discarded, so a second document that is a "+
		"sequence, a scalar, or a Definition with a typo'd field is silently ignored — which is exactly "+
		"the class of second document an operator is most likely to have written by mistake.")

	dir := t.TempDir()
	for _, c := range []struct{ name, yaml string }{
		{"a sequence", goodDefinitionYAML + "---\n- one\n- two\n"},
		{"a scalar", goodDefinitionYAML + "---\njust a string\n"},
		{"a definition with a typo", goodDefinitionYAML + "---\nrecommend: true\n"},
	} {
		p := writeFile(t, dir, strings.ReplaceAll(c.name, " ", "-")+".yaml", c.yaml)
		if _, err := LoadFile(p, "amd64"); err == nil {
			t.Errorf("%s: LoadFile accepted a file with a second YAML document in it", c.name)
		}
	}
}

// TestLoadFileRefusesAnArchMismatch: a file that pins an architecture is
// making a claim ("this is our arm64 image"). Quietly overriding it with the
// requested arch would produce a base that is neither what the file says nor
// what the operator asked for — and the seeds of an arm64 golden image are not
// the seeds of the amd64 one.
func TestLoadFileRefusesAnArchMismatch(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "pinned.yaml", goodDefinitionYAML+"arch: arm64\n")

	_, err := LoadFile(p, "amd64")
	requireUsage(t, err, "a file pinning arm64 loaded for amd64")
	if err != nil {
		for _, want := range []string{"arm64", "amd64"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("diagnostic %q does not name %q", err.Error(), want)
			}
		}
	}

	// The matching request, and the "no arch asked for" request, both stand.
	d, err := LoadFile(p, "arm64")
	if err != nil {
		t.Errorf("LoadFile(pinned arm64, arm64): %v", err)
	} else if d.Arch != "arm64" {
		t.Errorf("arch = %q", d.Arch)
	}
	d, err = LoadFile(p, "")
	if err != nil {
		t.Errorf(`LoadFile(pinned arm64, ""): %v`, err)
	} else if d.Arch != "arm64" {
		t.Errorf("arch = %q", d.Arch)
	}
}

// TestLoadFileRefusesAnArchItCannotResolve: a definition file is operator
// input, and an architecture with no container platform behind it is a base
// that cannot be resolved at all. Better here than four frames down.
func TestLoadFileRefusesAnArchItCannotResolve(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "sparc.yaml", goodDefinitionYAML+"arch: sparc64\n")
	_, err := LoadFile(p, "")
	requireUsage(t, err, "a file pinning an unsupported architecture")

	// A file with no arch and no arch asked for cannot be materialised either.
	q := writeFile(t, dir, "no-arch.yaml", goodDefinitionYAML)
	_, err = LoadFile(q, "")
	requireUsage(t, err, "a file with no arch, loaded with no arch")
}

// TestLoadFileRefusesUnreadableInputs covers the three ways the path itself is
// wrong. All three are dferr.Usage: a base definition is operator input, and
// the operator is the one who can fix any of them.
func TestLoadFileRefusesUnreadableInputs(t *testing.T) {
	dir := t.TempDir()

	sub := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := LoadFile(sub, "amd64")
	requireUsage(t, err, "a directory")
	if err != nil && !strings.Contains(err.Error(), "directory") {
		t.Errorf("diagnostic %q does not say it is a directory", err.Error())
	}

	// One byte over the limit, so the test pins the boundary rather than
	// merely "something huge is refused". The padding is a YAML comment, so
	// the file would parse perfectly if the size gate were not there — the
	// refusal has to come from the size and nothing else.
	big := goodDefinitionYAML + "# " + strings.Repeat("p", maxDefinitionSize) + "\n"
	bigPath := writeFile(t, dir, "oversize.yaml", big)
	if info, serr := os.Stat(bigPath); serr != nil || info.Size() <= maxDefinitionSize {
		t.Fatalf("the oversize fixture is not oversize: %v", serr)
	}
	_, err = LoadFile(bigPath, "amd64")
	requireUsage(t, err, "an oversize file")

	// A file just under the limit still loads, which is what proves the
	// refusal above was the size gate and not the padding.
	underPath := writeFile(t, dir, "just-under.yaml",
		goodDefinitionYAML+"# "+strings.Repeat("p", maxDefinitionSize-len(goodDefinitionYAML)-16)+"\n")
	if info, serr := os.Stat(underPath); serr != nil || info.Size() > maxDefinitionSize {
		t.Fatalf("the just-under fixture is not under the limit: %v", serr)
	}
	if _, err := LoadFile(underPath, "amd64"); err != nil {
		t.Errorf("a large but legal file was refused: %v", err)
	}

	_, err = LoadFile(filepath.Join(dir, "does-not-exist.yaml"), "amd64")
	requireUsage(t, err, "a nonexistent path")
}

// --- Resolve -----------------------------------------------------------

// TestResolvePrefersABuiltinIDOverAFileOfTheSameName is the ordering rule in
// Resolve's doc, tested from the direction that can actually go wrong.
//
// If the two steps were swapped, a file lying about in the working directory
// would silently redefine a documented base — the same class of surprise as a
// `./git` on PATH, except the thing being redefined is which archive a build
// resolves against. The shadow file here is a VALID definition with different
// seeds, so a reversed order would not merely error: it would succeed with the
// wrong answer, which is the failure worth detecting.
//
// "debian:12" is used because it is a builtin id (the bare form defaults to
// the minimal variant) that is also a creatable filename on both Linux and
// Windows.
func TestResolvePrefersABuiltinIDOverAFileOfTheSameName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	const shadow = `schema_version: debark.base/v1
id: shadow:1/minimal
distro_id: debian
version_id: "12"
codename: bookworm
seeds:
  - shadow-package
sources: |
  Types: deb
  URIs: http://shadow.example/debian
  Suites: ${codename}
  Components: main
  Signed-By: /usr/share/keyrings/shadow.gpg
keyrings:
  - /usr/share/keyrings/shadow.gpg
`
	if err := os.WriteFile("debian:12", []byte(shadow), 0o644); err != nil {
		t.Skipf("this filesystem cannot hold a file named after a base id (%v); "+
			"the ordering rule is unverifiable here", err)
	}
	if _, err := os.Stat("debian:12"); err != nil {
		t.Skipf("a file named after a base id is not statable here (%v)", err)
	}

	got, err := Resolve("debian:12", "amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Source != snapshot.OriginSourceBuiltin {
		t.Errorf("Resolve(debian:12) took the file in the working directory: source = %q", got.Source)
	}
	if got.Definition.ID != "debian:12/minimal" {
		t.Errorf("Resolve(debian:12).ID = %q", got.Definition.ID)
	}
	for _, seed := range got.Definition.Seeds {
		if seed == "shadow-package" {
			t.Fatalf("Resolve(debian:12) resolved the shadowing file's seeds: %q", got.Definition.Seeds)
		}
	}
	want, err := Lookup("debian:12/minimal", "amd64")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !reflect.DeepEqual(got.Definition, want) {
		t.Errorf("Resolve did not return the builtin definition:\n%+v\n%+v", got.Definition, want)
	}
	// The explicit path form still reaches the file, so the rule shadows the
	// bare name and not the operator's ability to say what they meant. An
	// absolute path is used rather than "./debian:12" because filepath.Join
	// would clean the "./" away on Unix and hand Resolve the bare id again.
	viaPath, err := Resolve(filepath.Join(dir, "debian:12"), "amd64")
	if err != nil {
		t.Fatalf("Resolve(<abs>/debian:12): %v", err)
	}
	if viaPath.Definition.ID != "shadow:1/minimal" {
		t.Errorf("an explicit path should still read the file, got id %q", viaPath.Definition.ID)
	}
}

// TestResolveOnAnUnknownStringNamesTheClosedSet: one message for both
// failures, because from the operator's point of view there is one mistake —
// this string names neither a base debark ships nor a file on disk. The
// message has to carry the closed set, the same shape core/distro.Resolve
// uses, or the operator has to go and find it.
func TestResolveOnAnUnknownStringNamesTheClosedSet(t *testing.T) {
	dir := t.TempDir()
	for _, arg := range []string{
		filepath.Join(dir, "nope.yaml"),
		"not-a-base",
		"no-such-base-definition.yaml",
		"",
	} {
		assertResolveNamesTheClosedSet(t, arg)
	}
}

// TestResolveOnAMistypedBaseIDNamesTheClosedSet is the same rule applied to
// the input an operator is most likely to get wrong: a string that is shaped
// exactly like a base id and names a release or a variant debark does not
// ship. See the report — this is the one that fails on Windows.
func TestResolveOnAMistypedBaseIDNamesTheClosedSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		knownDefect(t, `Resolve treats a stat failure as "this names no file" only for fs.ErrNotExist. `+
			`On Windows os.Stat of an id-shaped string ("ubuntu:99.04/desktop") fails with `+
			`ERROR_INVALID_NAME rather than ENOENT, so the operator gets a raw CreateFile message `+
			`instead of the list of builtin base ids. Every mistyped base id on Windows lands here.`)
	}
	for _, arg := range []string{
		"ubuntu:99.04/desktop",
		"ubuntu:26.04/standard",
		"mint:22/cinnamon",
	} {
		assertResolveNamesTheClosedSet(t, arg)
	}
}

func assertResolveNamesTheClosedSet(t *testing.T, arg string) {
	t.Helper()
	got, err := Resolve(arg, "amd64")
	requireUsage(t, err, fmt.Sprintf("Resolve(%q)", arg))
	if !reflect.DeepEqual(got, Resolved{}) {
		t.Errorf("Resolve(%q) failed but still returned %+v", arg, got)
	}
	if err == nil {
		return
	}
	text := err.Error() + "\n" + dferr.HintOf(err)
	for _, id := range BuiltinIDs() {
		if !strings.Contains(text, id) {
			t.Errorf("Resolve(%q): the diagnostic does not list the builtin id %q:\n%s", arg, id, text)
			return
		}
	}
}

// TestResolveRecordsOnlyTheBaseNameOfAFile. Resolved.Source becomes
// origin.source in the synthesized snapshot, and that snapshot travels to a
// builder and then into a bundle that crosses an air gap. The directory layout
// of the machine that happened to run from-base is not the target's business
// , so a separator in this field is a leak, not a cosmetic problem.
func TestResolveRecordsOnlyTheBaseNameOfAFile(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sites", "melbourne", "images")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := writeFile(t, nested, "our-soe.yaml", goodDefinitionYAML)

	for _, arg := range []string{p, filepath.ToSlash(p)} {
		got, err := Resolve(arg, "amd64")
		if err != nil {
			t.Errorf("Resolve(%q): %v", arg, err)
			continue
		}
		if got.Source != "our-soe.yaml" {
			t.Errorf("Resolve(%q).Source = %q, want the base name only", arg, got.Source)
		}
		if strings.ContainsAny(got.Source, `/\`) {
			t.Errorf("Resolve(%q).Source = %q carries a path separator", arg, got.Source)
		}
		if strings.Contains(got.Source, dir) {
			t.Errorf("Resolve(%q).Source = %q leaks the host directory", arg, got.Source)
		}
		if got.Digest == "" {
			t.Errorf("Resolve(%q) recorded no digest", arg)
		}
		if want, _ := Digest(got.Definition); got.Digest != want {
			t.Errorf("Resolve(%q).Digest = %q, want %q", arg, got.Digest, want)
		}
	}

	// A relative argument records the same thing: the field is about the
	// definition's identity, not about how the operator spelled the path.
	t.Chdir(nested)
	got, err := Resolve("our-soe.yaml", "amd64")
	if err != nil {
		t.Fatalf("Resolve relative: %v", err)
	}
	if got.Source != "our-soe.yaml" {
		t.Errorf("Resolve(relative).Source = %q", got.Source)
	}
}

// TestResolveOnABuiltinIDRecordsBuiltinProvenance is the other half of the
// Source contract, and the thing the next test says is not safe.
func TestResolveOnABuiltinIDRecordsBuiltinProvenance(t *testing.T) {
	for _, id := range BuiltinIDs() {
		got, err := Resolve(id, "amd64")
		if err != nil {
			t.Errorf("Resolve(%q): %v", id, err)
			continue
		}
		if got.Source != snapshot.OriginSourceBuiltin {
			t.Errorf("Resolve(%q).Source = %q, want %q", id, got.Source, snapshot.OriginSourceBuiltin)
		}
		if got.Definition.ID != id {
			t.Errorf("Resolve(%q).Definition.ID = %q", id, got.Definition.ID)
		}
	}
}

// TestResolveOnAFileNamedBuiltinMustNotClaimBuiltinProvenance. See the report.
//
// snapshot.OriginSourceBuiltin is the string "builtin", and Resolve fills
// Source from filepath.Base for a file, so a definition file whose base name
// is literally "builtin" lands on the sentinel. Two things follow, and the
// second is the one that bites:
//
//  1. the synthesized snapshot's origin.source says the definition was
//     compiled into this binary when it was in fact read off the operator's
//     disk — a provenance claim that is simply false, in the one block of the
//     format that exists to record provenance;
//  2. synthesizeInContainer derives `isFile := Source != OriginSourceBuiltin`,
//     so the container path would not mount the file and would hand the inner
//     binary the host path as if it were a builtin id.
//
// The fix is not this file's to make; it belongs in Resolve, which knows which
// branch it took and does not need to infer it from a string.
func TestResolveOnAFileNamedBuiltinMustNotClaimBuiltinProvenance(t *testing.T) {
	knownDefect(t, `Resolved.Source is filepath.Base(path) for a file, so a definition file named `+
		`"builtin" collides with snapshot.OriginSourceBuiltin. origin.source then claims the base was `+
		`compiled in, and synthesizeInContainer's isFile check reads false for a real file.`)

	dir := t.TempDir()
	p := writeFile(t, dir, snapshot.OriginSourceBuiltin, goodDefinitionYAML)

	got, err := Resolve(p, "amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Source == snapshot.OriginSourceBuiltin {
		t.Errorf("Resolve(%q).Source = %q, the reserved value for a compiled-in definition; "+
			"origin.source would claim provenance this definition does not have, and "+
			"synthesizeInContainer would treat a real file as a builtin id", p, got.Source)
	}
}
