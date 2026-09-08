package base

import (
	"errors"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// sampleDefinition is a hand-written, already-materialised definition used by
// the Expand, Materialise, Validate and Digest tests.
//
// It is deliberately NOT a builtin one. A builtin definition has already been
// through Materialise and Validate inside buildBuiltin, so using one here
// would let this file's tests pass against a Validate that checked nothing —
// the same "the implementation is its own oracle" shape the project has paid
// for before. Every field is written out so a rejection test can knock exactly
// one of them out and know the rest were fine.
func sampleDefinition() Definition {
	return Definition{
		SchemaVersion: SchemaVersion,
		ID:            "acme:soe-2026/workstation",
		Description:   "ACME standard operating environment, 2026 refresh",
		DistroID:      "ubuntu",
		VersionID:     "24.04",
		Codename:      "noble",
		Variant:       "workstation",
		Arch:          "amd64",
		Seeds:         []string{"acme-soe-workstation", "ubuntu-minimal"},
		Recommends:    false,
		Sources: "Types: deb\n" +
			"URIs: http://apt.acme.example/ubuntu\n" +
			"Suites: noble noble-updates\n" +
			"Components: main acme\n" +
			"Signed-By: /usr/share/keyrings/acme-archive-keyring.gpg\n",
		Keyrings: []string{"/usr/share/keyrings/acme-archive-keyring.gpg"},
	}
}

// requireUsage asserts an error both carries a dferr class (ADR-012) and that
// the class is Usage. Checking dferr.ClassOf alone would pass for a bare
// fmt.Errorf, because ClassOf defaults an unclassified error to Usage — the
// same trap core/distro's own tests call out.
func requireUsage(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected an error, got nil", what)
		return
	}
	var de *dferr.Error
	if !errors.As(err, &de) {
		t.Errorf("%s: error %v carries no dferr class", what, err)
		return
	}
	if de.Class != dferr.Usage {
		t.Errorf("%s: error class = %v, want %v (%v)", what, de.Class, dferr.Usage, err)
	}
}

// --- Expand ------------------------------------------------------------

// TestExpandSubstitutesExactlyThreeNames pins the whole substitution
// vocabulary. The doc comment on Expand promises three fixed names and no
// expression language, and the reason is not tidiness: a base definition is a
// file an operator commits to git and a builder then runs apt against, so
// anything richer would be a template deciding which archive a build talks to.
// An unrecognised ${...} must therefore survive verbatim, where Validate can
// see it and refuse.
func TestExpandSubstitutesExactlyThreeNames(t *testing.T) {
	d := sampleDefinition() // noble / 24.04 / amd64
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"codename", "Suites: ${codename}", "Suites: noble"},
		{"version id", "URIs: http://x/${version_id}", "URIs: http://x/24.04"},
		{"arch", "Architectures: ${arch}", "Architectures: amd64"},
		{"all three, more than once", "${codename}/${arch}/${codename}/${version_id}", "noble/amd64/noble/24.04"},
		{"an unknown name is left alone", "${nope}", "${nope}"},
		{"an unknown name beside a known one", "${nope}-${codename}", "${nope}-noble"},
		{"case is not folded", "${CODENAME}", "${CODENAME}"},
		{"inner spaces are not trimmed", "${ codename }", "${ codename }"},
		{"no braces is not a placeholder", "$codename", "$codename"},
		{"no dollar is not a placeholder", "{codename}", "{codename}"},
		{"no placeholders at all", "Types: deb", "Types: deb"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := d.Expand(c.in); got != c.want {
			t.Errorf("%s: Expand(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// --- Materialise -------------------------------------------------------

// TestMaterialiseDoesNotAliasTheReceiversSlices is the one that would have
// caught a plain `out := d` with no re-slicing. A Definition is copied by
// value everywhere in this package — Builtin hands out a fresh one per call,
// Lookup materialises the table's row — so a Materialise that shared the
// Seeds or Keyrings backing array would let one caller's edit reach into
// another caller's "own" definition, and the compiled-in table behind it.
// The mutation is done in both directions because aliasing is symmetric and
// only checking one of them would miss half the cases.
func TestMaterialiseDoesNotAliasTheReceiversSlices(t *testing.T) {
	orig := sampleDefinition()
	wantSeeds := append([]string(nil), orig.Seeds...)
	wantKeyrings := append([]string(nil), orig.Keyrings...)

	out := orig.Materialise("arm64")

	if &out.Seeds[0] == &orig.Seeds[0] {
		t.Error("Materialise returned a definition sharing the receiver's Seeds backing array")
	}
	if &out.Keyrings[0] == &orig.Keyrings[0] {
		t.Error("Materialise returned a definition sharing the receiver's Keyrings backing array")
	}

	// Forward: writing through the result must not reach the receiver.
	out.Seeds[0] = "clobbered"
	out.Keyrings[0] = "/clobbered"
	out.Seeds = append(out.Seeds, "appended")
	assertStrings(t, "orig.Seeds after mutating the result", orig.Seeds, wantSeeds)
	assertStrings(t, "orig.Keyrings after mutating the result", orig.Keyrings, wantKeyrings)

	// Backward: writing through the receiver must not reach an earlier result.
	out2 := orig.Materialise("arm64")
	orig.Seeds[0] = "clobbered-the-other-way"
	orig.Keyrings[0] = "/clobbered-the-other-way"
	assertStrings(t, "an earlier result's Seeds after mutating the receiver", out2.Seeds, wantSeeds)
	assertStrings(t, "an earlier result's Keyrings after mutating the receiver", out2.Keyrings, wantKeyrings)
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %q, want %q", what, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %q, want %q", what, got, want)
			return
		}
	}
}

// TestMaterialiseAppliesArchAndSubstitutes covers the contract Synthesize
// depends on: a definition that still carries ${arch} has no single digest, so
// two builds of "the same base" would not be comparable.
func TestMaterialiseAppliesArchAndSubstitutes(t *testing.T) {
	d := sampleDefinition()
	d.Arch = ""
	d.Sources = "Types: deb\nSuites: ${codename}\nArchitectures: ${arch}\nSigned-By: /k/${version_id}.gpg\n"
	d.Keyrings = []string{"/usr/share/keyrings/${codename}.gpg"}
	d.Seeds = []string{"seed-${arch}"}

	out := d.Materialise("arm64")
	if out.Arch != "arm64" {
		t.Errorf("Arch = %q, want arm64", out.Arch)
	}
	if strings.Contains(out.Sources, "${") {
		t.Errorf("Sources still carries a placeholder: %q", out.Sources)
	}
	if want := "Types: deb\nSuites: noble\nArchitectures: arm64\nSigned-By: /k/24.04.gpg\n"; out.Sources != want {
		t.Errorf("Sources = %q, want %q", out.Sources, want)
	}
	assertStrings(t, "Keyrings", out.Keyrings, []string{"/usr/share/keyrings/noble.gpg"})
	assertStrings(t, "Seeds", out.Seeds, []string{"seed-arm64"})

	// An empty arch means "keep whatever the definition pinned", which is how
	// LoadFile lets a file claim its own architecture.
	pinned := sampleDefinition()
	pinned.Arch = "ppc64el"
	if got := pinned.Materialise("").Arch; got != "ppc64el" {
		t.Errorf(`Materialise("") changed a pinned arch to %q`, got)
	}
}

// --- Validate ----------------------------------------------------------

func TestValidateAcceptsAWellFormedDefinition(t *testing.T) {
	if err := Validate(sampleDefinition()); err != nil {
		t.Fatalf("a well-formed definition must validate: %v", err)
	}
	// Keyrings is optional: a definition whose sources need no Signed-By pin
	// (an operator's internal mirror served over a trusted transport) is a
	// legitimate shape, and Validate is not the place to relitigate it.
	d := sampleDefinition()
	d.Keyrings = nil
	if err := Validate(d); err != nil {
		t.Errorf("keyrings is optional: %v", err)
	}
}

// TestValidateRejects is the operator-input gate. Every failure must be
// dferr.Usage, whether the definition came from a file the operator wrote or
// from this binary's own table — in which case a failure here is a bug
// builtin_test.go is supposed to catch first.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Definition)
		want   string // substring the diagnostic must name
	}{
		{"wrong schema_version", func(d *Definition) { d.SchemaVersion = "debark.base/v2" }, "schema_version"},
		{"absent schema_version", func(d *Definition) { d.SchemaVersion = "" }, "schema_version"},
		{"empty id", func(d *Definition) { d.ID = "" }, "id is required"},
		{"blank id", func(d *Definition) { d.ID = "   " }, "id is required"},
		{"empty distro_id", func(d *Definition) { d.DistroID = "" }, "distro_id is required"},
		{"blank distro_id", func(d *Definition) { d.DistroID = "\t" }, "distro_id is required"},
		{"empty version_id", func(d *Definition) { d.VersionID = "" }, "version_id is required"},
		{"empty codename", func(d *Definition) { d.Codename = "" }, "codename is required"},
		{"empty arch", func(d *Definition) { d.Arch = "" }, "arch is required"},
		{"no seeds at all", func(d *Definition) { d.Seeds = nil }, "at least one seed"},
		{"an empty seed list", func(d *Definition) { d.Seeds = []string{} }, "at least one seed"},
		{"an empty-string seed", func(d *Definition) { d.Seeds = []string{"apt", ""} }, "empty seed"},
		{"a blank seed", func(d *Definition) { d.Seeds = []string{" \t "} }, "empty seed"},
		{"empty sources", func(d *Definition) { d.Sources = "" }, "sources is required"},
		{"blank sources", func(d *Definition) { d.Sources = "\n  \n" }, "sources is required"},
		{"sources still holding a placeholder", func(d *Definition) {
			d.Sources = strings.Replace(d.Sources, "noble", "${codename}", 1)
		}, "unsubstituted"},
		{"sources holding an unrecognised placeholder", func(d *Definition) {
			d.Sources += "X-Repolib-Name: ${nope}\n"
		}, "unsubstituted"},
		{"a relative keyring path", func(d *Definition) {
			d.Keyrings = []string{"keys/acme-archive-keyring.gpg"}
		}, "absolute path"},
		{"a bare keyring filename", func(d *Definition) {
			d.Keyrings = []string{"acme-archive-keyring.gpg"}
		}, "absolute path"},
		{"a Windows keyring path", func(d *Definition) {
			// The path is absolute on the machine an operator might be
			// sitting at, and meaningless on the Linux machine that resolves
			// the seeds. That is the whole reason the rule is "starts with /"
			// rather than filepath.IsAbs.
			d.Keyrings = []string{`C:\keys\acme-archive-keyring.gpg`}
		}, "absolute path"},
		{"a relative keyring after a good one", func(d *Definition) {
			d.Keyrings = []string{"/usr/share/keyrings/a.gpg", "../b.gpg"}
		}, "absolute path"},
	}
	for _, c := range cases {
		d := sampleDefinition()
		c.mutate(&d)
		err := Validate(d)
		requireUsage(t, err, c.name)
		if err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: diagnostic %q does not name %q", c.name, err.Error(), c.want)
		}
	}
}

// --- Digest ------------------------------------------------------------

// TestDigestIsStable is what makes origin.source_digest worth recording at
// all: two builds of the same base must agree, or the field says nothing.
func TestDigestIsStable(t *testing.T) {
	first, err := Digest(sampleDefinition())
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if len(first) != 64 {
		t.Errorf("Digest = %q, want 64 lowercase hex characters", first)
	}
	if first != strings.ToLower(first) {
		t.Errorf("Digest = %q, want lowercase", first)
	}
	for i := 0; i < 32; i++ {
		got, err := Digest(sampleDefinition())
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		if got != first {
			t.Fatalf("Digest varies between runs: %q then %q", first, got)
		}
	}
}

// TestDigestDistinguishesEveryField is the other half. The digest exists
// because origin.base_id is a name, and a name can mean different things at
// different times — a maintenance release changing a seed, or an operator
// editing their golden-image file. Any field that can change the apt run and
// does not change the digest is a silent divergence between two snapshots
// claiming the same base.
//
// Seeds order and Recommends have their own rows because they are the two
// most tempting to normalise away. Seed order is apt's install order and
// therefore part of the request; Recommends is the err-toward-not-installed
// default, and flipping it makes the base's claim strictly larger.
func TestDigestDistinguishesEveryField(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*Definition)
	}{
		{"unmutated base", func(*Definition) {}},
		{"schema_version", func(d *Definition) { d.SchemaVersion = "debark.base/v2" }},
		{"id", func(d *Definition) { d.ID = "acme:soe-2026/server" }},
		{"description", func(d *Definition) { d.Description = "something else entirely" }},
		{"distro_id", func(d *Definition) { d.DistroID = "debian" }},
		{"version_id", func(d *Definition) { d.VersionID = "22.04" }},
		{"codename", func(d *Definition) { d.Codename = "jammy" }},
		{"variant", func(d *Definition) { d.Variant = "server" }},
		{"arch", func(d *Definition) { d.Arch = "arm64" }},
		{"one more seed", func(d *Definition) { d.Seeds = append(d.Seeds, "openssh-server") }},
		{"one fewer seed", func(d *Definition) { d.Seeds = d.Seeds[:1] }},
		{"a different seed", func(d *Definition) { d.Seeds[1] = "ubuntu-server-minimal" }},
		{"seeds order only", func(d *Definition) { d.Seeds[0], d.Seeds[1] = d.Seeds[1], d.Seeds[0] }},
		{"recommends flipped", func(d *Definition) { d.Recommends = !d.Recommends }},
		{"sources", func(d *Definition) { d.Sources += "\nTypes: deb-src\n" }},
		{"sources whitespace only", func(d *Definition) { d.Sources = strings.Replace(d.Sources, "Types: deb", "Types:  deb", 1) }},
		{"one more keyring", func(d *Definition) { d.Keyrings = append(d.Keyrings, "/usr/share/keyrings/other.gpg") }},
		{"a different keyring", func(d *Definition) { d.Keyrings[0] = "/usr/share/keyrings/other.gpg" }},
		{"no keyrings", func(d *Definition) { d.Keyrings = nil }},
	}

	seen := map[string]string{} // digest -> the mutation that produced it
	for _, m := range mutations {
		d := sampleDefinition()
		m.mutate(&d)
		dg, err := Digest(d)
		if err != nil {
			t.Fatalf("%s: Digest: %v", m.name, err)
		}
		if prior, ok := seen[dg]; ok {
			t.Errorf("%q and %q digest identically (%s); the two definitions differ and the digest cannot tell them apart",
				prior, m.name, dg)
			continue
		}
		seen[dg] = m.name
	}
	if len(seen) != len(mutations) {
		t.Errorf("%d distinct digests for %d distinct definitions", len(seen), len(mutations))
	}
}

// TestSortIDs pins the display order `snapshot list-bases` and the docs
// generated from it both depend on.
func TestSortIDs(t *testing.T) {
	ids := []string{"ubuntu:26.04/minimal", "debian:12/server", "ubuntu:22.04/desktop", "debian:12/desktop"}
	SortIDs(ids)
	want := []string{"debian:12/desktop", "debian:12/server", "ubuntu:22.04/desktop", "ubuntu:26.04/minimal"}
	assertStrings(t, "SortIDs", ids, want)
}
