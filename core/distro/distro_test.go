package distro

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// hostileTargets are the strings a malicious or merely broken snapshot could
// put in target.distro_id / version_id / codename. Every one of them must
// fail closed: no release row, no image reference, and no fragment of the
// input surviving into anything the container backend would run.
var hostileTargets = [][3]string{
	{"debian", "12; rm -rf /", "bookworm-evil"},
	{"debian", "12 --privileged", "bookworm evil"},
	{"attacker.io/evil:latest", "1", "x"},
	{"ubuntu", "24.04 --privileged", ""},
	{"", "", ""},
	{"debian", "", ""},
	{"", "12", ""},
	{"", "", "not-a-codename"},
	{"debian\n", "12", "bookworm-x"},
	{" debian", "12", "bookwormx"},
	{"debian", " 12", "bookwormx"},
	{"debian", "12 ", " bookworm"},
	{"debian", "13.0", "trixie-slim"},
	{"docker.io/library/alpine", "latest", "alpine"},
	{"ubuntu", "24.04\x00", ""},
	{"debian", "$(id)", "x`id`"},
	{"debian", "../../etc/passwd", ""},
	{"DEBIAN", "12.0", "BOOKWORM-X"},
}

// TestResolveIsAClosedTable is the central security property of this package:
// (distro_id, version_id, codename) come straight off an untrusted snapshot,
// and this is the only path from them to a container image. Nothing that is
// not already a row in the table may ever come out.
func TestResolveIsAClosedTable(t *testing.T) {
	for _, in := range hostileTargets {
		r, err := Resolve(in[0], in[1], in[2])
		if err == nil {
			t.Errorf("Resolve(%q, %q, %q) succeeded with image %q; it must fail closed",
				in[0], in[1], in[2], r.Ref())
			continue
		}
		if r.Image != "" || r.Ref() != "" {
			t.Errorf("Resolve(%q, %q, %q) failed but still returned an image %q",
				in[0], in[1], in[2], r.Ref())
		}
		// dferr.ClassOf defaults an unclassified error to Usage, so checking
		// the class alone would pass for a bare fmt.Errorf too. The rule is
		// that the error CARRIES a class (ADR-012), so look for the *dferr.Error
		// itself.
		var de *dferr.Error
		if !errors.As(err, &de) {
			t.Errorf("Resolve(%q, %q, %q) returned an unclassified error %v; every error must carry a dferr class",
				in[0], in[1], in[2], err)
		} else if de.Class != dferr.Usage {
			t.Errorf("Resolve(%q, %q, %q) error class = %v, want %v", in[0], in[1], in[2], de.Class, dferr.Usage)
		}
	}
}

// TestResolveOnlyEverReturnsATableRow proves the returned Release is one of
// the table's own values rather than anything assembled from the arguments:
// even the successful path never concatenates caller input into an image.
func TestResolveOnlyEverReturnsATableRow(t *testing.T) {
	inTable := func(r Release) bool {
		for _, want := range releases {
			if r == want {
				return true
			}
		}
		return false
	}
	for _, want := range releases {
		for _, args := range [][3]string{
			{want.DistroID, want.VersionID, ""},
			{strings.ToUpper(want.DistroID), want.VersionID, ""},
			{"", "", want.Codename},
			{"", "", strings.ToUpper(want.Codename)},
			{want.DistroID, "no-such-version", want.Codename},
		} {
			got, err := Resolve(args[0], args[1], args[2])
			if err != nil {
				t.Errorf("Resolve(%q, %q, %q): %v", args[0], args[1], args[2], err)
				continue
			}
			if !inTable(got) {
				t.Errorf("Resolve(%q, %q, %q) returned a Release that is not a table row: %+v",
					args[0], args[1], args[2], got)
			}
		}
	}
}

var (
	imageRe  = regexp.MustCompile(`^docker\.io/library/(debian|ubuntu):[a-z0-9.-]+$`)
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// TestTableInvariants pins the shape of every row. The image allowlist is the
// point: a row pointing anywhere but docker.io/library would silently move
// where every build of that release resolves.
func TestTableInvariants(t *testing.T) {
	seenRelease := map[string]bool{}
	seenCodename := map[string]bool{}
	for _, r := range releases {
		if !imageRe.MatchString(r.Image) {
			t.Errorf("%s %s: image %q is not an official docker.io/library image", r.DistroID, r.VersionID, r.Image)
		}
		if r.ImageDigest != "" && !digestRe.MatchString(r.ImageDigest) {
			t.Errorf("%s %s: image_digest %q is malformed", r.DistroID, r.VersionID, r.ImageDigest)
		}
		if r.DistroID != Debian && r.DistroID != Ubuntu {
			t.Errorf("unknown distro id %q", r.DistroID)
		}
		if r.DistroID != strings.ToLower(r.DistroID) || r.Codename != strings.ToLower(r.Codename) {
			t.Errorf("%s %s: distro id and codename must be lowercase so Lookup/ByCodename can match them", r.DistroID, r.VersionID)
		}
		key := r.DistroID + " " + r.VersionID
		if seenRelease[key] {
			t.Errorf("duplicate release row %s", key)
		}
		seenRelease[key] = true
		// ByCodename assumes codenames are unique across the whole table, so a
		// collision would silently resolve to whichever row came first.
		if seenCodename[r.Codename] {
			t.Errorf("duplicate codename %q; ByCodename would be ambiguous", r.Codename)
		}
		seenCodename[r.Codename] = true
		if r.Supported && r.ImageDigest == "" {
			t.Errorf("%s: a CI-supported release must be pinned by digest", key)
		}
	}
}

// TestRefPinsByDigest covers the reference rewriting, including the registry
// port case: cutting at the first ':' turns a ported registry reference into a
// completely different image (a bare name resolves against Docker Hub), which
// is a worse failure than not pinning at all.
func TestRefPinsByDigest(t *testing.T) {
	const dig = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		name string
		rel  Release
		want string
	}{
		{"tagged", Release{Image: "docker.io/library/debian:trixie-slim", ImageDigest: dig},
			"docker.io/library/debian@" + dig},
		{"untagged", Release{Image: "docker.io/library/debian", ImageDigest: dig},
			"docker.io/library/debian@" + dig},
		{"ported registry", Release{Image: "registry.internal:5000/library/debian:trixie-slim", ImageDigest: dig},
			"registry.internal:5000/library/debian@" + dig},
		{"ported registry, untagged", Release{Image: "registry.internal:5000/library/debian", ImageDigest: dig},
			"registry.internal:5000/library/debian@" + dig},
		{"already digest-pinned", Release{Image: "docker.io/library/debian@sha256:" + strings.Repeat("2", 64), ImageDigest: dig},
			"docker.io/library/debian@" + dig},
		{"no digest known", Release{Image: "docker.io/library/debian:trixie-slim"},
			"docker.io/library/debian:trixie-slim"},
		{"no image at all", Release{}, ""},
	}
	for _, c := range cases {
		if got := c.rel.Ref(); got != c.want {
			t.Errorf("%s: Ref() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRefNeverEmitsTwoDigests guards the property the whole pin depends on:
// exactly one "@sha256:" in the reference a runtime is handed.
func TestRefNeverEmitsTwoDigests(t *testing.T) {
	for _, r := range All() {
		ref := r.Ref()
		if n := strings.Count(ref, "@"); n > 1 {
			t.Errorf("%s: Ref() = %q has %d '@' separators", r.Codename, ref, n)
		}
		if r.ImageDigest != "" && !strings.HasSuffix(ref, "@"+r.ImageDigest) {
			t.Errorf("%s: Ref() = %q is not pinned to %s", r.Codename, ref, r.ImageDigest)
		}
	}
}

func TestLookupAndByCodenameAreCaseInsensitiveOnTheirOwnKey(t *testing.T) {
	if _, ok := Lookup("Debian", "12"); !ok {
		t.Error(`Lookup("Debian", "12") should match the lowercase table row`)
	}
	if _, ok := ByCodename("BOOKWORM"); !ok {
		t.Error(`ByCodename("BOOKWORM") should match the lowercase table row`)
	}
	// VersionID is compared verbatim on purpose: "12.0" is not "12".
	if _, ok := Lookup("debian", "12.0"); ok {
		t.Error(`Lookup("debian", "12.0") should not match the "12" row`)
	}
}

func TestAllAndSupportedReturnCopies(t *testing.T) {
	got := All()
	if len(got) != len(releases) {
		t.Fatalf("All() returned %d rows, want %d", len(got), len(releases))
	}
	got[0].Image = "attacker.io/evil:latest"
	if releases[0].Image == "attacker.io/evil:latest" {
		t.Fatal("All() aliases the package's own table; a caller could rewrite the image every build uses")
	}
	for _, r := range Supported() {
		if !r.Supported {
			t.Errorf("Supported() returned a best-effort row: %s %s", r.DistroID, r.VersionID)
		}
	}
}

func TestPlatformIsAClosedMapping(t *testing.T) {
	if p, ok := Platform("amd64"); !ok || p != "linux/amd64" {
		t.Errorf(`Platform("amd64") = %q, %v`, p, ok)
	}
	for _, bad := range []string{"", "amd64 --privileged", "linux/amd64", "AMD64", "x86_64", "amd64\n"} {
		if p, ok := Platform(bad); ok {
			t.Errorf("Platform(%q) = %q, want no match", bad, p)
		}
	}
}

// TestArchitecturesIsSorted matters because this list is rendered into
// operator-facing error text; map iteration order would make that text vary
// from run to run.
func TestArchitecturesIsSorted(t *testing.T) {
	first := Architectures()
	for i := 0; i < 20; i++ {
		got := Architectures()
		if len(got) != len(first) {
			t.Fatalf("Architectures() length varies: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("Architectures() order varies at %d: %q vs %q", j, got[j], first[j])
			}
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("Architectures() is not sorted: %q then %q", first[i-1], first[i])
		}
	}
}

func TestComponentFlag(t *testing.T) {
	cases := []struct {
		in   string
		flag string
		ok   bool
	}{
		{"multiverse", "multiverse", true},
		{"restricted/net", "restricted", true},
		{"non-free", "non-free", true},
		{"non-free-firmware/kernel", "non-free-firmware", true},
		{"  Non-Free  ", "non-free", true},
		{"contrib", "", false}, // free-ish; doctor mentions it, no lock flag
		{"main", "", false},
		{"universe/net", "", false},
		{"", "", false},
		{"non-freeX", "", false},
	}
	for _, c := range cases {
		flag, ok := ComponentFlag(c.in)
		if flag != c.flag || ok != c.ok {
			t.Errorf("ComponentFlag(%q) = (%q, %v), want (%q, %v)", c.in, flag, ok, c.flag, c.ok)
		}
	}
}

func TestPrimaryArchitecturesAreInTheTable(t *testing.T) {
	for _, a := range PrimaryArchitectures() {
		if _, ok := Platform(a); !ok {
			t.Errorf("PrimaryArchitectures() names %q, which Platform() does not know", a)
		}
	}
}
