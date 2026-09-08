package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validLock() *Lock {
	return &Lock{
		SchemaVersion:  SchemaVersion,
		CreatedAt:      "2026-01-01T00:00:00Z",
		SnapshotDigest: strings.Repeat("a", 64),
		RequestDigest:  strings.Repeat("b", 64),
		Target:         Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: Resolver{
			Backend: BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: "never-include", InstallRecommends: true,
		},
		Packages: []Package{
			{
				Name: "demo", Arch: "amd64", Version: "1.0-1",
				Filename: "pool/d/demo/demo_1.0-1_amd64.deb",
				Size:     123, SHA256: strings.Repeat("c", 64),
				Origin: Origin{URI: "file:///dev/null", Suite: "bookworm"},
				Reason: ReasonRequested, PublisherVerification: VerifiedAPTSigned,
			},
			{
				Name: "demo", Arch: "i386", Version: "1.0-1",
				Filename: "pool/d/demo/demo_1.0-1_i386.deb",
				Size:     120, SHA256: strings.Repeat("d", 64),
				Origin: Origin{URI: "file:///dev/null", Suite: "bookworm"},
				Reason: ReasonRequested, PublisherVerification: VerifiedAPTSigned,
			},
		},
		Install:     []string{"demo:amd64=1.0-1", "demo:i386=1.0-1"},
		ClosedWorld: ClosedWorld{Result: ClosedWorldOK},
	}
}

func TestValidate_OK(t *testing.T) {
	if err := Validate(validLock()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_DuplicatePackage(t *testing.T) {
	l := validLock()
	l.Packages = append(l.Packages, l.Packages[0]) // exact duplicate (name, arch, version)
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a duplicate (name, arch, version) entry")
	}
}

func TestValidate_MalformedDigest(t *testing.T) {
	l := validLock()
	l.Packages[0].SHA256 = "not-hex"
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a malformed sha256")
	}
}

func TestValidate_InstallEntryUnknownPackage(t *testing.T) {
	l := validLock()
	l.Install = append(l.Install, "ghost:amd64=9.9-9")
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted an install entry naming a package not present in packages")
	}
}

func TestValidate_InstallEntryWrongVersion(t *testing.T) {
	l := validLock()
	l.Install = append(l.Install, "demo:amd64=2.0-1") // demo:amd64 exists, but not at this version
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted an install entry at a version not present in packages")
	}
}

func TestValidate_UnqualifiedInstallEntryRejected(t *testing.T) {
	l := validLock()
	// "demo=1.0-1" names a real package at a real version, but without an
	// architecture qualifier it is ambiguous by construction (Multi-Arch:
	// same packages can share name and version across architectures), so it
	// must be rejected even though an unqualified reading would resolve.
	l.Install = append(l.Install, "demo=1.0-1")
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted an unqualified install entry (name=version); every entry must be name:arch=version")
	}
}

func TestValidate_MalformedInstallEntry(t *testing.T) {
	l := validLock()
	l.Install = append(l.Install, "no-equals-sign")
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a malformed install entry")
	}
}

func TestValidate_ResolverBackendAuto(t *testing.T) {
	l := validLock()
	l.Resolver.Backend = BackendAuto
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted resolver.backend = auto, which must never be recorded in a saved lock")
	}
}

func TestValidate_WrongSchema(t *testing.T) {
	l := validLock()
	l.SchemaVersion = "debark.lock/v99"
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted an unknown schema version")
	}
}

func TestInstallSet_And_Find(t *testing.T) {
	l := validLock()
	set := l.InstallSet()
	if len(set) != 2 {
		t.Fatalf("InstallSet returned %d packages, want 2: %+v", len(set), set)
	}
	names := map[string]bool{}
	for _, p := range set {
		names[p.Name+"/"+p.Arch] = true
	}
	if !names["demo/amd64"] || !names["demo/i386"] {
		t.Fatalf("InstallSet missing expected entries: %+v", set)
	}

	p, ok := l.Find("demo", "amd64")
	if !ok || p.Version != "1.0-1" {
		t.Fatalf("Find(demo, amd64) = %+v, %v", p, ok)
	}
	if _, ok := l.Find("nope", "amd64"); ok {
		t.Fatal("Find found a package that does not exist")
	}
}

func TestSaveLoadDigest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := validLock()

	digest1, err := Save(dir, l)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if digest1 == "" {
		t.Fatal("Save returned an empty digest")
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	digest2, err := Digest(loaded)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if digest1 != digest2 {
		t.Fatalf("digest changed across a save/load round trip: %s != %s", digest1, digest2)
	}
	if err := Validate(loaded); err != nil {
		t.Fatalf("loaded lock failed Validate: %v", err)
	}
}

func TestDigest_Deterministic(t *testing.T) {
	l1 := validLock()
	l2 := validLock()
	d1, err := Digest(l1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Digest(l2)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("two structurally identical locks produced different digests: %s != %s", d1, d2)
	}
}

func TestLoad_UnknownSchema(t *testing.T) {
	dir := t.TempDir()
	l := validLock()
	l.SchemaVersion = "debark.lock/v99"
	// Written directly rather than through Save: Save now refuses to write a
	// schema version Load will not read back (see
	// TestSave_RefusesASchemaVersionLoadWillNotRead), so producing this file
	// is the test's own job. What is under test here is Load's refusal, and
	// a document that only ever reaches Load from removable media is more
	// faithfully built from bytes anyway.
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted an unknown schema version")
	}
}
