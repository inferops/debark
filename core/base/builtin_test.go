package base

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"pault.ag/go/debian/control"

	"github.com/inferops/debark/core/distro"
)

// definitionStrings yields every string a Definition carries, labelled by
// field name. It walks the struct with reflection rather than listing the
// fields, so a field added later is scanned for leftover placeholders without
// anyone remembering to update this file — which is the only way a "no ${ }
// anywhere" test stays true.
func definitionStrings(d Definition) map[string][]string {
	out := map[string][]string{}
	v := reflect.ValueOf(d)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		switch fv := v.Field(i); fv.Kind() {
		case reflect.String:
			out[f.Name] = []string{fv.String()}
		case reflect.Slice:
			if fv.Type().Elem().Kind() != reflect.String {
				continue
			}
			vals := make([]string, 0, fv.Len())
			for j := 0; j < fv.Len(); j++ {
				vals = append(vals, fv.Index(j).String())
			}
			out[f.Name] = vals
		}
	}
	return out
}

// TestEveryBuiltinIsUsableOnEveryPrimaryArchitecture is the table's own
// integrity check. buildBuiltin already runs Validate, so a row that cannot
// be materialised makes Builtin return an error rather than a bad definition;
// this asserts that never happens for the architectures the project promises, and
// that what comes back is a full table rather than a silently short one.
//
// The placeholder scan is the part worth having. A base still carrying ${arch}
// has no single digest, so two builds of "the same base" would not be
// comparable — and Validate only looks at Sources, so a placeholder left in a
// seed or a keyring path would sail straight through.
func TestEveryBuiltinIsUsableOnEveryPrimaryArchitecture(t *testing.T) {
	wantRows := len(builtinReleases) * len(variants)
	for _, arch := range distro.PrimaryArchitectures() {
		defs, err := Builtin(arch)
		if err != nil {
			t.Errorf("Builtin(%q): %v", arch, err)
			continue
		}
		if len(defs) == 0 {
			t.Errorf("Builtin(%q) returned no definitions", arch)
			continue
		}
		if len(defs) != wantRows {
			t.Errorf("Builtin(%q) returned %d definitions, want %d (%d releases x %d variants)",
				arch, len(defs), wantRows, len(builtinReleases), len(variants))
		}
		seen := map[string]bool{}
		for _, d := range defs {
			if err := Validate(d); err != nil {
				t.Errorf("Builtin(%q): %s does not validate: %v", arch, d.ID, err)
			}
			if d.Arch != arch {
				t.Errorf("Builtin(%q): %s was materialised for arch %q", arch, d.ID, d.Arch)
			}
			if seen[d.ID] {
				t.Errorf("Builtin(%q) returned %s twice", arch, d.ID)
			}
			seen[d.ID] = true
			for field, values := range definitionStrings(d) {
				for _, s := range values {
					if strings.Contains(s, "${") {
						t.Errorf("Builtin(%q): %s: field %s still carries a placeholder: %q",
							arch, d.ID, field, s)
					}
				}
			}
		}
	}
}

// TestBuiltinRefusesAnArchitectureItCannotResolve keeps the arch argument a
// closed lookup. A base that cannot be resolved is not a base, and the failure
// belongs here rather than after a container has started.
func TestBuiltinRefusesAnArchitectureItCannotResolve(t *testing.T) {
	for _, arch := range []string{"", "AMD64", "x86_64", "amd64 --privileged", "linux/amd64", "amd64\n", "sparc64"} {
		defs, err := Builtin(arch)
		requireUsage(t, err, "Builtin("+arch+")")
		if len(defs) != 0 {
			t.Errorf("Builtin(%q) failed but still returned %d definitions", arch, len(defs))
		}
		d, err := Lookup("ubuntu:26.04/minimal", arch)
		requireUsage(t, err, "Lookup with arch "+arch)
		if !reflect.DeepEqual(d, Definition{}) {
			t.Errorf("Lookup with arch %q failed but still returned %+v", arch, d)
		}
	}
}

// TestBuiltinReleasesAndDistroAgree proves the "unreachable" branch in
// buildBuiltin really is unreachable, in both directions.
//
// Forward: every row here must exist in core/distro, or Builtin returns an
// error instead of a definition. Backward: every CI-supported release in
// core/distro must have a base, because that is the promise this table
// exists to track — a supported release with no builtin base is a gap an
// operator only discovers when `--base` refuses their release.
func TestBuiltinReleasesAndDistroAgree(t *testing.T) {
	for _, r := range builtinReleases {
		rel, ok := distro.Lookup(r.distroID, r.versionID)
		if !ok {
			t.Errorf("builtinReleases names %s %s, which core/distro does not know", r.distroID, r.versionID)
			continue
		}
		if !rel.Supported {
			t.Errorf("builtinReleases names %s %s, which core/distro marks best-effort rather than Supported; "+
				"the builtin table is supposed to track the CI-tested set", r.distroID, r.versionID)
		}
		if strings.TrimSpace(rel.Codename) == "" {
			// Every suite name in Sources is built from the codename, so a
			// row without one would produce a definition Validate refuses.
			t.Errorf("%s %s has no codename in core/distro", r.distroID, r.versionID)
		}
	}

	inBuiltins := func(distroID, versionID string) bool {
		for _, r := range builtinReleases {
			if r.distroID == distroID && r.versionID == versionID {
				return true
			}
		}
		return false
	}
	for _, rel := range distro.Supported() {
		if !inBuiltins(rel.DistroID, rel.VersionID) {
			t.Errorf("core/distro marks %s %s CI-supported but the builtin base table has no row for it",
				rel.DistroID, rel.VersionID)
		}
	}
}

// TestBuiltinIDsAreSortedUniqueAndRoundTrip covers the list `snapshot
// list-bases` and shell completion both print. Map iteration order anywhere in
// the chain would make that output — and the docs generated from it — vary
// between runs.
func TestBuiltinIDsAreSortedUniqueAndRoundTrip(t *testing.T) {
	ids := BuiltinIDs()
	if want := len(builtinReleases) * len(variants); len(ids) != want {
		t.Errorf("BuiltinIDs() returned %d ids, want %d", len(ids), want)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if i > 0 && ids[i-1] >= id {
			t.Errorf("BuiltinIDs() is not sorted: %q then %q", ids[i-1], id)
		}
		if seen[id] {
			t.Errorf("BuiltinIDs() contains %q twice", id)
		}
		seen[id] = true

		if !IsBuiltinID(id) {
			t.Errorf("IsBuiltinID(%q) is false for an id BuiltinIDs() produced", id)
		}
		d, err := Lookup(id, "amd64")
		if err != nil {
			t.Errorf("Lookup(%q): %v", id, err)
			continue
		}
		if d.ID != id {
			t.Errorf("Lookup(%q) returned a definition whose id is %q", id, d.ID)
		}
	}

	// The order is stable across calls, not merely sorted once.
	for i := 0; i < 8; i++ {
		if !reflect.DeepEqual(BuiltinIDs(), ids) {
			t.Fatalf("BuiltinIDs() varies between calls")
		}
	}
}

// hostileBaseIDs are the strings an operator's --base argument, a shell
// completion, or a crafted script could put in front of ParseID. Every one
// must fail closed: either ParseID refuses it with a class, or it parses to
// something Lookup then refuses. Nothing may reach a definition, and
// IsBuiltinID — which is what decides whether the string is treated as an id
// or as a path to open — must say false for all of them.
//
// Modelled on core/distro's hostileTargets table, and for the same reason:
// this is the string that chooses which archive a build talks to.
var hostileBaseIDs = []string{
	"",
	"   ",
	"ubuntu",
	"ubuntu:",
	":26.04",
	"::",
	"ubuntu:26.04/",
	"/desktop",
	"/",
	"ubuntu:26.04/desktop/extra",
	"ubuntu:26.04/../../etc/passwd",
	"ubuntu:26.04/desktop\n",
	"ubuntu:26.04\n/desktop",
	"ubuntu:26.04/desktop\x00",
	"ubuntu:26.04\x00/desktop",
	"ubuntu:26.04 --privileged",
	"ubuntu:26.04/desktop --privileged",
	"ubuntu:$(id)/desktop",
	"ubuntu:26.04/desk top",
	" ubuntu:26.04/desktop",
	"ubuntu:26.04/ desktop",
	"ubuntu: 26.04/desktop",
	"attacker.io/evil:latest",
	"docker.io/library/alpine:latest",
	"ubuntu:20.04/desktop",  // a real release, but not one the builtin table covers
	"ubuntu:26.04/standard", // a real release, but not a variant that exists
	"debian:12/gnome",
	"mint:22/desktop",
	"../../etc/passwd",
	"C:/bases/ubuntu.yaml",
}

func TestParseIDAndLookupFailClosedOnHostileInput(t *testing.T) {
	for _, id := range hostileBaseIDs {
		if IsBuiltinID(id) {
			t.Errorf("IsBuiltinID(%q) is true; Resolve would treat this string as a documented base", id)
		}

		distroID, versionID, variantName, err := ParseID(id)
		if err != nil {
			requireUsage(t, err, fmt.Sprintf("ParseID(%q)", id))
			if distroID != "" || versionID != "" || variantName != "" {
				t.Errorf("ParseID(%q) failed but returned (%q, %q, %q); a refused id must yield no parts",
					id, distroID, versionID, variantName)
			}
			continue
		}
		// It parsed. That is allowed — ParseID deliberately does not consult
		// the table, so an operator-supplied id that is not builtin at all
		// still parses — but Lookup is then the closed gate and must refuse.
		d, lerr := Lookup(id, "amd64")
		requireUsage(t, lerr, fmt.Sprintf("Lookup(%q)", id))
		if !reflect.DeepEqual(d, Definition{}) {
			t.Errorf("Lookup(%q) was refused but still returned %+v", id, d)
		}
	}
}

// TestParseIDLowercasesTheDistroAndVariantButNotTheVersion pins the folding
// ParseID actually does, because it decides which strings IsBuiltinID accepts
// and therefore which strings Resolve will never open as a file. The distro id
// and variant fold (core/distro.Lookup folds the distro id too); the version
// id does not, because "12.0" is not "12".
func TestParseIDLowercasesTheDistroAndVariantButNotTheVersion(t *testing.T) {
	distroID, versionID, variantName, err := ParseID("UBUNTU:26.04/DESKTOP")
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if distroID != "ubuntu" || versionID != "26.04" || variantName != "desktop" {
		t.Errorf("ParseID(UBUNTU:26.04/DESKTOP) = (%q, %q, %q)", distroID, versionID, variantName)
	}
	if !IsBuiltinID("UBUNTU:26.04/DESKTOP") {
		t.Error("IsBuiltinID should fold the distro id and variant, as core/distro.Lookup does")
	}
	if IsBuiltinID("ubuntu:26.04.0/desktop") {
		t.Error(`"26.04.0" is not "26.04" and must not resolve to it`)
	}
}

// TestABareIDResolvesToMinimal is the err-toward-not-installed default in its
// most load-bearing spot. A bare "ubuntu:26.04" is the least informed request
// this package can receive, and the smallest claim is the safe answer: the
// bundle comes out larger than it needed to be rather than short. If this ever
// defaulted to desktop or server, every bare id would start claiming packages
// a minimally-installed target does not have.
func TestABareIDResolvesToMinimal(t *testing.T) {
	if DefaultVariant != "minimal" {
		t.Fatalf("DefaultVariant = %q; the safe default is the least inclusive variant", DefaultVariant)
	}
	minimal, ok := variantByName("minimal")
	if !ok {
		t.Fatal("the table has no minimal variant")
	}

	for _, c := range []struct {
		bare      string
		explicit  string
		wantSeeds []string
	}{
		{"ubuntu:26.04", "ubuntu:26.04/minimal", minimal.ubuntuSeeds},
		{"debian:12", "debian:12/minimal", minimal.debianSeeds},
	} {
		got, err := Lookup(c.bare, "amd64")
		if err != nil {
			t.Errorf("Lookup(%q): %v", c.bare, err)
			continue
		}
		want, err := Lookup(c.explicit, "amd64")
		if err != nil {
			t.Fatalf("Lookup(%q): %v", c.explicit, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) and Lookup(%q) differ:\n%+v\n%+v", c.bare, c.explicit, got, want)
		}
		if got.Variant != "minimal" {
			t.Errorf("Lookup(%q).Variant = %q, want minimal", c.bare, got.Variant)
		}
		assertStrings(t, "Lookup("+c.bare+").Seeds", got.Seeds, c.wantSeeds)
	}
}

func variantByName(name string) (variant, bool) {
	for _, v := range variants {
		if v.name == name {
			return v, true
		}
	}
	return variant{}, false
}

// TestUbuntuUsesPortsOnlyWhereUbuntuDoes is the one mapping in this package
// where a wrong answer is invisible until a container has already started and
// apt reports finding nothing. The expectation is written out here rather than
// read from ubuntuPortsArches: a test that asks the rule what the rule says has
// no oracle at all.
func TestUbuntuUsesPortsOnlyWhereUbuntuDoes(t *testing.T) {
	// Where Ubuntu actually publishes each architecture. amd64 and i386 are on
	// archive.ubuntu.com with a separate security host; everything else Ubuntu
	// builds is on ports.ubuntu.com, security included.
	wantPorts := map[string]bool{
		"amd64":   false,
		"i386":    false,
		"arm64":   true,
		"armhf":   true,
		"ppc64el": true,
		"s390x":   true,
		"riscv64": true,
	}
	for arch, ports := range wantPorts {
		doc := ubuntuSources(arch)
		hasPorts := strings.Contains(doc, ubuntuPortsArchive)
		hasArchive := strings.Contains(doc, ubuntuArchive)
		hasSecurity := strings.Contains(doc, ubuntuSecurity)
		switch {
		case ports && (!hasPorts || hasArchive || hasSecurity):
			t.Errorf("ubuntuSources(%q) should serve everything from %s, got:\n%s", arch, ubuntuPortsArchive, doc)
		case !ports && (hasPorts || !hasArchive || !hasSecurity):
			t.Errorf("ubuntuSources(%q) should serve from %s and %s, got:\n%s", arch, ubuntuArchive, ubuntuSecurity, doc)
		}
		if ports && !strings.Contains(doc, "${codename}-security") {
			t.Errorf("ubuntuSources(%q) drops the security suite", arch)
		}
	}

	// And the same seen from the outside, through a real definition, so the
	// wiring from Lookup's arch argument through to the document is covered
	// too and not just the renderer.
	amd64, err := Lookup("ubuntu:26.04/minimal", "amd64")
	if err != nil {
		t.Fatalf("Lookup amd64: %v", err)
	}
	arm64, err := Lookup("ubuntu:26.04/minimal", "arm64")
	if err != nil {
		t.Fatalf("Lookup arm64: %v", err)
	}
	if !strings.Contains(arm64.Sources, ubuntuPortsArchive) || strings.Contains(arm64.Sources, ubuntuArchive) {
		t.Errorf("ubuntu arm64 must resolve against the ports archive, got:\n%s", arm64.Sources)
	}
	if strings.Contains(amd64.Sources, ubuntuPortsArchive) {
		t.Errorf("ubuntu amd64 must not resolve against the ports archive, got:\n%s", amd64.Sources)
	}
}

// TestDebianSourcesAreTheSameOnEveryArchitecture: Debian serves every
// released architecture from deb.debian.org, so an arch-dependent Debian
// document would mean somebody had copied Ubuntu's rule across.
func TestDebianSourcesAreTheSameOnEveryArchitecture(t *testing.T) {
	for _, id := range []string{"debian:12/minimal", "debian:13/server"} {
		amd64, err := Lookup(id, "amd64")
		if err != nil {
			t.Fatalf("Lookup(%q, amd64): %v", id, err)
		}
		arm64, err := Lookup(id, "arm64")
		if err != nil {
			t.Fatalf("Lookup(%q, arm64): %v", id, err)
		}
		if amd64.Sources != arm64.Sources {
			t.Errorf("%s: sources differ by architecture:\n--- amd64\n%s\n--- arm64\n%s", id, amd64.Sources, arm64.Sources)
		}
		if !strings.Contains(amd64.Sources, debianArchive) || !strings.Contains(amd64.Sources, debianSecurity) {
			t.Errorf("%s: sources name neither %s nor %s:\n%s", id, debianArchive, debianSecurity, amd64.Sources)
		}
		// The only difference between the two definitions should be Arch.
		amd64.Arch, arm64.Arch = "", ""
		if !reflect.DeepEqual(amd64, arm64) {
			t.Errorf("%s: definitions differ by more than arch:\n%+v\n%+v", id, amd64, arm64)
		}
	}
}

// TestEveryBuiltinSourcesDocumentPinsAKeyringItCarries is a real invariant,
// not a formatting check.
//
// core/apt's private-root builder rewrites each source's Signed-By to point at
// its in-root copy of the named keyring, matched by path. A Signed-By naming a
// keyring the definition does not carry has nothing to match, so the pin is
// STRIPPED — apt then verifies that source against the private root's whole
// combined keyring set instead of the one key it was supposed to be pinned to,
// which is a policy downgrade nobody asked for and nothing here would print.
//
// The document is parsed with a real deb822 reader rather than grepped, so a
// document that is malformed in a way apt would reject fails here too.
func TestEveryBuiltinSourcesDocumentPinsAKeyringItCarries(t *testing.T) {
	for _, arch := range distro.PrimaryArchitectures() {
		defs, err := Builtin(arch)
		if err != nil {
			t.Fatalf("Builtin(%q): %v", arch, err)
		}
		for _, d := range defs {
			what := d.ID + " on " + arch

			reader, err := control.NewParagraphReader(strings.NewReader(d.Sources), nil)
			if err != nil {
				t.Errorf("%s: sources is not readable as deb822: %v", what, err)
				continue
			}
			stanzas, err := reader.All()
			if err != nil {
				t.Errorf("%s: sources is not valid deb822: %v", what, err)
				continue
			}
			if len(stanzas) == 0 {
				t.Errorf("%s: sources parsed to no stanzas at all", what)
				continue
			}

			carried := map[string]bool{}
			for _, k := range d.Keyrings {
				carried[k] = true
			}
			referenced := map[string]bool{}

			for i, st := range stanzas {
				for _, field := range []string{"Types", "URIs", "Suites", "Components", "Signed-By"} {
					if v, ok := st.Values[field]; !ok || strings.TrimSpace(v) == "" {
						t.Errorf("%s: stanza %d has no %s", what, i, field)
					}
				}
				signedBy := strings.TrimSpace(st.Values["Signed-By"])
				if signedBy == "" {
					continue
				}
				// A multi-value Signed-By is legal apt syntax; every path in
				// it has to be carried, not just the first.
				for _, k := range strings.Fields(signedBy) {
					referenced[k] = true
					if !carried[k] {
						t.Errorf("%s: stanza %d is Signed-By %q, which the definition's keyrings do not carry (%q); "+
							"core/apt would strip the pin and verify this source against the whole keyring set",
							what, i, k, d.Keyrings)
					}
				}
			}

			for k := range carried {
				if !referenced[k] {
					t.Errorf("%s: keyring %q is carried but no source names it in Signed-By; "+
						"it would be copied into the private root and pin nothing", what, k)
				}
			}
		}
	}
}

// TestRecommendsIsOffForEveryBuiltin guards a deliberate default with a stated
// safety argument, which is exactly the kind of thing a well-meaning edit
// flips. A recommended package apt would pull in but the real installer did
// not take is a package the base claims as installed when it is not — the
// short-bundle direction, and the one failure debark exists to prevent.
func TestRecommendsIsOffForEveryBuiltin(t *testing.T) {
	for _, arch := range distro.PrimaryArchitectures() {
		defs, err := Builtin(arch)
		if err != nil {
			t.Fatalf("Builtin(%q): %v", arch, err)
		}
		for _, d := range defs {
			if d.Recommends {
				t.Errorf("%s on %s has recommends on; turning it on makes the base's claim strictly larger "+
					"and must only ever be done by someone who has measured that their real image matches",
					d.ID, arch)
			}
		}
	}
}

// TestEveryBuiltinSeedsTheSmallerMetapackage pins the other half of the
// err-toward-not-installed rule. Ubuntu ships both ubuntu-desktop and
// ubuntu-desktop-minimal; claiming the larger one and meeting a minimally
// installed machine produces a SHORT bundle, which fails at the far side of
// the air gap.
func TestEveryBuiltinSeedsTheSmallerMetapackage(t *testing.T) {
	for _, v := range variants {
		for _, seed := range v.ubuntuSeeds {
			if seed == "ubuntu-desktop" || seed == "ubuntu-server" {
				t.Errorf("variant %q seeds %q; the -minimal metapackage is the safe claim", v.name, seed)
			}
		}
		if len(v.ubuntuSeeds) == 0 || len(v.debianSeeds) == 0 {
			t.Errorf("variant %q has no seeds for one of the two distributions", v.name)
		}
	}
}

// TestHostArchIsAnArchitectureThisPackageWillAccept: HostArch is what --arch
// defaults to, so a value checkArch refuses would make the no-flag invocation
// fail with "unknown architecture" on a machine that is perfectly ordinary.
func TestHostArchIsAnArchitectureThisPackageWillAccept(t *testing.T) {
	arch := HostArch()
	if err := checkArch(arch); err != nil {
		t.Errorf("HostArch() = %q, which this package refuses: %v", arch, err)
	}
	if _, err := Lookup("ubuntu:26.04/minimal", arch); err != nil {
		t.Errorf("HostArch() = %q, which Lookup refuses: %v", arch, err)
	}
	// Every mapped dpkg name must be one core/distro knows, or the mapping
	// silently produces an architecture no container platform exists for.
	for goarch, dpkgArch := range goArchToDpkg {
		if _, ok := distro.Platform(dpkgArch); !ok {
			t.Errorf("goArchToDpkg maps GOARCH %q to %q, which core/distro.Platform does not know", goarch, dpkgArch)
		}
	}
}

// TestVariantNamesAreLeastInclusiveFirst pins an ordering that carries a
// safety meaning, after that meaning was lost once.
//
// The guided flow picks its default install profile by taking the FIRST entry
// of this list, on the stated grounds that it is the smallest claim a base can
// make — and a smaller claim can only ever make a bundle larger, never short.
// The picker originally derived its list from BuiltinIDs, which is sorted, so
// it offered "desktop, minimal, server" and defaulted to desktop: the largest
// claim of the three, and the exact inversion of the rule. Every option was
// present and only the order was wrong, so nothing failed; it was found by
// running the flow through a pty and reading it.
//
// Two assertions, because either alone is passable by accident.
func TestVariantNamesAreLeastInclusiveFirst(t *testing.T) {
	names := VariantNames()
	if len(names) < 2 {
		t.Fatalf("VariantNames() = %v, want at least two profiles", names)
	}

	// The first entry must be the same profile a bare "distro:version" id
	// resolves to. Those are two expressions of one decision — "when nobody
	// said, assume the least" — and they must not be able to disagree.
	if names[0] != DefaultVariant {
		t.Errorf("VariantNames()[0] = %q, want %q: the guided flow defaults to the first entry, "+
			"and it must be the same smallest claim a bare distro:version id resolves to",
			names[0], DefaultVariant)
	}

	// And the list must not be in sorted order, because sorted order is
	// exactly the regression: it puts "desktop" first.
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if slicesEqual(names, sorted) {
		t.Errorf("VariantNames() = %v is in lexical order; that is how the default became the "+
			"largest claim last time. The order must come from the table, which lists profiles "+
			"least inclusive first", names)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
