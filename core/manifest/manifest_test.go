package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sampleBundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "repo", "Packages"), []byte("Package: demo\n"))
	writeFile(t, filepath.Join(dir, "repo", "pool", "d", "demo", "demo_1.0_amd64.deb"), []byte("fake deb"))
	writeFile(t, filepath.Join(dir, "lock.json"), []byte(`{"schema_version":"debark.lock/v1"}`))
	return dir
}

func buildInputFor(dir string) BuildInput {
	return BuildInput{
		Dir:            dir,
		SnapshotDigest: "snap-digest",
		LockDigest:     "lock-digest",
		Repository:     Repository{PackagesSHA256: "aa", ReleaseSHA256: "bb", PackageCount: 1, PoolBytes: 8},
		Target:         Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Tool:           Tool{Name: "debark", Version: "test", Edition: EditionCommunity},
		CreatedAt:      "2026-01-01T00:00:00Z",
	}
}

func TestBuild_Deterministic(t *testing.T) {
	dir := sampleBundleDir(t)
	m1, err := Build(context.Background(), buildInputFor(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m2, err := Build(context.Background(), buildInputFor(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c1, err := Canonical(m1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Canonical(m2)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) != string(c2) {
		t.Fatalf("two Build runs over the same tree produced different canonical bytes")
	}

	// Files must be sorted by path and use forward slashes.
	for i := 1; i < len(m1.Files); i++ {
		if m1.Files[i-1].Path >= m1.Files[i].Path {
			t.Fatalf("Files not sorted: %q >= %q", m1.Files[i-1].Path, m1.Files[i].Path)
		}
	}
	for _, f := range m1.Files {
		if filepath.ToSlash(f.Path) != f.Path {
			t.Fatalf("path %q is not forward-slashed", f.Path)
		}
		if f.Path == FileName || f.Path == SigFileName {
			t.Fatalf("Files must never include %q or %q, found %q", FileName, SigFileName, f.Path)
		}
	}
}

func TestBuild_ExcludesManifestAndSig(t *testing.T) {
	dir := sampleBundleDir(t)
	// Simulate a stale manifest/sig already sitting in the directory from a
	// previous build (Build must never fold its own output into Files).
	writeFile(t, filepath.Join(dir, FileName), []byte("stale"))
	writeFile(t, filepath.Join(dir, SigFileName), []byte("stale"))

	m, err := Build(context.Background(), buildInputFor(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range m.Files {
		if f.Path == FileName || f.Path == SigFileName {
			t.Fatalf("Build included its own output file %q in Files", f.Path)
		}
	}
}

func TestBuild_BundleIDDerivedFromLockDigestAndCreatedAt(t *testing.T) {
	dir := sampleBundleDir(t)
	in := buildInputFor(dir)
	m, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := NewBundleID(in.LockDigest, in.CreatedAt)
	if m.BundleID != want {
		t.Fatalf("BundleID = %q, want %q", m.BundleID, want)
	}
	if m.BundleID == "" || len(m.BundleID) != 16 {
		t.Fatalf("BundleID = %q, want 16 hex characters", m.BundleID)
	}
}

func TestNewBundleID_Deterministic(t *testing.T) {
	id1 := NewBundleID("abc", "2026-01-01T00:00:00Z")
	id2 := NewBundleID("abc", "2026-01-01T00:00:00Z")
	if id1 != id2 {
		t.Fatalf("NewBundleID not deterministic: %s != %s", id1, id2)
	}
	if id3 := NewBundleID("xyz", "2026-01-01T00:00:00Z"); id3 == id1 {
		t.Fatalf("NewBundleID did not change when lockDigest changed")
	}
	if id4 := NewBundleID("abc", "2026-02-02T00:00:00Z"); id4 == id1 {
		t.Fatalf("NewBundleID did not change when createdAt changed")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := sampleBundleDir(t)
	m, err := Build(context.Background(), buildInputFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	wantCanon, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, m); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, gotCanon, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(gotCanon) != string(wantCanon) {
		t.Fatalf("Load's canonical bytes do not match Canonical(m) computed before Save")
	}
	if loaded.BundleID != m.BundleID {
		t.Fatalf("BundleID lost across round trip: %q != %q", loaded.BundleID, m.BundleID)
	}
}

// TestLoad_ReCanonicalisesDiskBytes proves Load does not simply trust the
// file's on-disk formatting: it must reproduce the same canonical bytes
// regardless of how the JSON on disk happens to be whitespaced or ordered.
func TestLoad_ReCanonicalisesDiskBytes(t *testing.T) {
	dir := sampleBundleDir(t)
	m, err := Build(context.Background(), buildInputFor(dir))
	if err != nil {
		t.Fatal(err)
	}

	// Hand-write the manifest with different (but semantically identical)
	// whitespace and key order than MarshalIndent would produce.
	handWritten := []byte("{\n  \"format_version\": 1,\n  \"schema_version\": \"" + SchemaVersion + "\",\n" +
		"  \"bundle_id\": \"" + m.BundleID + "\",\n  \"created_at\": \"" + m.CreatedAt + "\",\n" +
		"  \"tool\": {\"name\":\"debark\",\"version\":\"test\",\"edition\":\"community\"},\n" +
		"  \"snapshot_digest\": \"snap-digest\",\n  \"lock_digest\": \"lock-digest\",\n" +
		"  \"repository\": {\"packages_sha256\":\"aa\",\"release_sha256\":\"bb\",\"package_count\":1,\"pool_bytes\":8},\n" +
		"  \"files\": [],\n  \"target\": {\"distro_id\":\"debian\",\"version_id\":\"12\",\"codename\":\"bookworm\",\"arch\":\"amd64\"}\n}\n")
	// Only compare against a manifest whose Files also happen to be empty, so
	// build one with an empty directory instead of reusing m (which has real
	// files). This isolates the test to "does re-formatting change the
	// canonical output", not file contents.
	emptyDir := t.TempDir()
	emptyIn := buildInputFor(emptyDir)
	emptyM, err := Build(context.Background(), emptyIn)
	if err != nil {
		t.Fatal(err)
	}
	emptyM.BundleID = m.BundleID
	emptyM.CreatedAt = m.CreatedAt
	wantCanon, err := Canonical(emptyM)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, FileName), handWritten)
	_, gotCanon, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(gotCanon) != string(wantCanon) {
		t.Fatalf("Load did not re-canonicalise disk bytes:\ngot:  %s\nwant: %s", gotCanon, wantCanon)
	}
}

func TestSaveLoadSignature_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	sf := &SignatureFile{
		Schema:         SignatureSchemaVersion,
		ManifestSHA256: "deadbeef",
		Signatures: []Signature{{
			SignerKind: SignerEd25519File, KeyID: "abcd1234", Algorithm: "ed25519",
			CreatedAt: "2026-01-01T00:00:00Z", Signature: "c2ln",
		}},
	}
	if err := SaveSignature(dir, sf); err != nil {
		t.Fatalf("SaveSignature: %v", err)
	}
	loaded, err := LoadSignature(dir)
	if err != nil {
		t.Fatalf("LoadSignature: %v", err)
	}
	if loaded.ManifestSHA256 != sf.ManifestSHA256 || len(loaded.Signatures) != 1 {
		t.Fatalf("signature file did not round-trip: %+v", loaded)
	}
}

func TestLoadSignature_MissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	sf, err := LoadSignature(dir)
	if err != nil {
		t.Fatalf("LoadSignature on a missing file returned an error: %v", err)
	}
	if sf != nil {
		t.Fatalf("LoadSignature on a missing file returned non-nil: %+v", sf)
	}
}

func TestLoad_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Load(dir); err == nil {
		t.Fatal("Load on a missing manifest did not return an error")
	}
}

func TestLoad_UnknownSchema(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), []byte(`{"schema_version":"debark.manifest/v99"}`))
	if _, _, err := Load(dir); err == nil {
		t.Fatal("Load accepted an unknown schema version")
	}
}
