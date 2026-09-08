package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
)

func TestWriteArchiveOpenRoundTrip(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/ubuntu2404-deb822", IncludeKeyrings: true})
	origDigest, err := Digest(s)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	if a.Digest != origDigest {
		t.Errorf("Archive.Digest = %s, want %s (round trip must be digest-stable)", a.Digest, origDigest)
	}
	if a.Snapshot.Target.DistroID != s.Target.DistroID || a.Snapshot.InstalledCount != s.InstalledCount {
		t.Errorf("round-tripped document differs: %+v vs %+v", a.Snapshot.Target, s.Target)
	}
	if len(a.Snapshot.KeyringFingerprints) != len(s.KeyringFingerprints) {
		t.Errorf("keyring_fingerprints count changed across round trip: %d vs %d",
			len(a.Snapshot.KeyringFingerprints), len(s.KeyringFingerprints))
	}

	// Every captured file must be present, at its ArchivePath, under FilesDir.
	for _, f := range s.Files() {
		if f.Path == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.FilesDir(), filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			t.Errorf("%s: not extracted: %v", f.Path, err)
			continue
		}
		if sha256Hex(data) != f.SHA256 {
			t.Errorf("%s: extracted content does not match its own recorded digest", f.Path)
		}
	}

	dir := a.FilesDir()
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("Close did not remove the extraction directory")
	}
}

func TestWriteArchiveDeterministic(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})

	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.tar.zst")
	p2 := filepath.Join(dir, "b.tar.zst")
	if err := WriteArchive(p1, s, fs); err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(p2, s, fs); err != nil {
		t.Fatal(err)
	}
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("two writes of the same snapshot produced different archives (%d vs %d bytes)", len(b1), len(b2))
	}
}

func TestOpenDirectoryInput(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})

	dir := t.TempDir()
	doc, err := canonicalIndent(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DocumentName), doc, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.Files() {
		if f.Path == "" {
			continue
		}
		p := filepath.Join(dir, FilesDir, filepath.FromSlash(f.ArchivePath))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, fs.Bytes[f.ArchivePath], 0o644); err != nil {
			t.Fatal(err)
		}
	}

	a, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open(directory): %v", err)
	}
	defer a.Close()
	if a.Snapshot.Target.DistroID != "debian" {
		t.Errorf("distro_id = %q", a.Snapshot.Target.DistroID)
	}
	// Open must never hand back (or mutate) the caller's own directory.
	if a.FilesDir() == filepath.Join(dir, FilesDir) {
		t.Error("Open returned the caller's own directory instead of a private copy")
	}
}

func TestOpenRedactionReappliedOnLoad(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})

	// Simulate a hand-edited or corrupted document: Redactions still claims
	// machine-id was removed, but the field was put back.
	s.Redactions = []string{RedactMachineID}
	s.Target.MachineID = "put-back-by-tampering"

	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatal(err)
	}

	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	if a.Snapshot.Target.MachineID != "" {
		t.Errorf("machine_id = %q, Open must re-strip a field its own Redactions list claims was removed", a.Snapshot.Target.MachineID)
	}
}

// TestOpenRejectsBundleShapedDirectory is the regression test for the
// integration finding recorded in test/integration/pipeline_test.go's
// assertSnapshotDigestAgreement: a directory holding just the document --
// exactly what a bundle's snapshot.json copy looks like, since core/bundle's
// Assemble (assemble.go's writeSnapshotJSON) writes only the document, never
// a files/ tree beside it -- must not be silently half-accepted by Open, and
// the eventual failure must name the situation and say what to call instead,
// rather than dying on the first File with a generic "missing from archive".
func TestOpenRejectsBundleShapedDirectory(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})
	dir := t.TempDir()
	writeBundleShapedSnapshotJSON(t, dir, s)
	// Deliberately no files/ directory at all: this, and only this, is what
	// core/bundle ever writes at a bundle's root.

	_, err := Open(context.Background(), dir)
	if err == nil {
		t.Fatal("Open accepted a directory holding only the document, with no files/ tree beside it")
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
	if !strings.Contains(err.Error(), "no "+FilesDir+"/ directory") {
		t.Errorf("error does not precisely name the missing %s/ directory: %v", FilesDir, err)
	}
	hint := dferr.HintOf(err)
	if !strings.Contains(hint, "OpenDocument") || !strings.Contains(hint, "bundle.Open") {
		t.Errorf("hint does not say what to call instead: %q", hint)
	}
}

// TestOpenDocumentReadsBundleShapedDirectory proves the fix for the trap
// TestOpenRejectsBundleShapedDirectory documents: the same bundle-shaped
// input -- document only, no files/ tree -- opens cleanly through
// OpenDocument, in both forms a caller might reasonably pass it: the
// directory, or the snapshot.json file inside it.
func TestOpenDocumentReadsBundleShapedDirectory(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})
	wantDigest, err := Digest(s)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeBundleShapedSnapshotJSON(t, dir, s)

	t.Run("directory", func(t *testing.T) {
		got, err := OpenDocument(context.Background(), dir)
		if err != nil {
			t.Fatalf("OpenDocument(dir): %v", err)
		}
		gotDigest, err := Digest(got)
		if err != nil {
			t.Fatal(err)
		}
		if gotDigest != wantDigest {
			t.Errorf("digest = %s, want %s", gotDigest, wantDigest)
		}
		if len(got.Files()) == 0 {
			t.Error("File metadata was lost even though OpenDocument never touches file bytes")
		}
	})

	t.Run("file", func(t *testing.T) {
		got, err := OpenDocument(context.Background(), filepath.Join(dir, DocumentName))
		if err != nil {
			t.Fatalf("OpenDocument(file): %v", err)
		}
		gotDigest, err := Digest(got)
		if err != nil {
			t.Fatal(err)
		}
		if gotDigest != wantDigest {
			t.Errorf("digest = %s, want %s", gotDigest, wantDigest)
		}
	})
}

// TestOpenDocumentValidatesSchema proves OpenDocument still runs the same
// schema validation Open does -- it is a narrower read, not a raw JSON
// decode -- by rejecting a document with an unknown schema_version.
func TestOpenDocumentValidatesSchema(t *testing.T) {
	dir := t.TempDir()
	bad := []byte(`{"schema_version": "not-a-real-version"}`)
	if err := os.WriteFile(filepath.Join(dir, DocumentName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := OpenDocument(context.Background(), dir)
	if err == nil {
		t.Fatal("OpenDocument accepted a document with an unknown schema_version")
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}

// TestOpenDocumentReappliesRedactions mirrors TestOpenRedactionReappliedOnLoad:
// OpenDocument must give a caller the same guarantee Open does, that a
// hand-edited document cannot smuggle back a field its own Redactions list
// claims was removed.
func TestOpenDocumentReappliesRedactions(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", IncludeKeyrings: true})
	s.Redactions = []string{RedactMachineID}
	s.Target.MachineID = "put-back-by-tampering"

	dir := t.TempDir()
	writeBundleShapedSnapshotJSON(t, dir, s)

	got, err := OpenDocument(context.Background(), dir)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if got.Target.MachineID != "" {
		t.Errorf("machine_id = %q, OpenDocument must re-strip a field its own Redactions list claims was removed", got.Target.MachineID)
	}
}

func TestOpenDocumentNonexistentPath(t *testing.T) {
	if _, err := OpenDocument(context.Background(), filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOpenPathEscapeRejectedTarEntry(t *testing.T) {
	doc := minimalValidSnapshotJSON(t)

	cases := []string{
		"../../../evil",
		"../evil",
		"a/../../evil",
	}
	for _, malicious := range cases {
		t.Run(malicious, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			mustWriteTarEntryRaw(t, tw, DocumentName, doc)
			mustWriteTarEntryRaw(t, tw, FilesDir+"/"+malicious, []byte("evil payload"))
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			compressed, err := zstdCompress(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "evil.tar.zst")
			if err := os.WriteFile(path, compressed, 0o644); err != nil {
				t.Fatal(err)
			}

			a, err := Open(context.Background(), path)
			if err == nil {
				a.Close()
				t.Fatal("Open accepted an archive with a path-escaping entry")
			}
			if got := errClass(err); got != "verification" {
				t.Errorf("class = %s, want verification", got)
			}
		})
	}
}

func TestOpenPathEscapeRejectedByDocumentValidation(t *testing.T) {
	// Entry names in the tar are safe; the *document* itself claims an
	// unsafe ArchivePath for dpkg_status. This must be caught by Validate
	// before any join against the extraction directory is attempted.
	bad := &Snapshot{
		SchemaVersion: SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Target:        Target{DistroID: "debian", VersionID: "12", Arch: "amd64"},
		DpkgStatus:    File{Path: "/var/lib/dpkg/status", ArchivePath: "../../../etc/evil", SHA256: sha256Hex(nil)},
	}
	doc, err := canonicalIndent(bad)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarEntryRaw(t, tw, DocumentName, doc)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed, err := zstdCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evil2.tar.zst")
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Open(context.Background(), path)
	if err == nil {
		t.Fatal("Open accepted a document with an unsafe archive_path")
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}

func TestOpenDigestMismatchRejected(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list"})
	path := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(path, s, fs); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, err := zstdDecompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite just the sources.list *file entry's own content* -- not a
	// blind byte-replace over the whole tar, which would just as easily
	// land inside snapshot.json's own JSON text (e.g. "codename":
	// "bookworm") and prove nothing about file-digest verification at all.
	tampered := tamperTarFileEntry(t, tarBytes, "etc/apt/sources.list")
	compressed, err := zstdCompress(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Open(context.Background(), path)
	if err == nil {
		t.Fatal("Open accepted an archive whose file content does not match its recorded digest")
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}

func TestOpenNonexistentPath(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "nope.tar.zst")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWriteArchiveRejectsIncompleteFileSet(t *testing.T) {
	s := &Snapshot{
		SchemaVersion: SchemaVersion,
		DpkgStatus:    File{Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status", SHA256: sha256Hex([]byte("x"))},
	}
	fs := &FileSet{Bytes: map[string][]byte{}} // missing the referenced bytes
	err := WriteArchive(filepath.Join(t.TempDir(), "x.tar.zst"), s, fs)
	if err == nil {
		t.Fatal("expected an error for a FileSet missing a referenced file's bytes")
	}
	if got := errClass(err); got != "usage" {
		t.Errorf("class = %s, want usage", got)
	}
}

func TestWriteArchiveNilInputs(t *testing.T) {
	if err := WriteArchive("x", nil, &FileSet{Bytes: map[string][]byte{}}); err == nil {
		t.Error("expected an error for a nil document")
	}
	if err := WriteArchive("x", &Snapshot{}, nil); err == nil {
		t.Error("expected an error for a nil file set")
	}
}

func TestDigestStableAndSensitiveToContent(t *testing.T) {
	s1 := &Snapshot{SchemaVersion: SchemaVersion, Target: Target{DistroID: "debian"}}
	s2 := &Snapshot{SchemaVersion: SchemaVersion, Target: Target{DistroID: "debian"}}
	s3 := &Snapshot{SchemaVersion: SchemaVersion, Target: Target{DistroID: "ubuntu"}}

	d1, err := Digest(s1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Digest(s2)
	if err != nil {
		t.Fatal(err)
	}
	d3, err := Digest(s3)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Error("identical documents produced different digests")
	}
	if d1 == d3 {
		t.Error("different documents produced the same digest")
	}
	if _, err := Digest(nil); err == nil {
		t.Error("expected an error digesting a nil document")
	}
}

// --- helpers ---

func canonicalIndent(s *Snapshot) ([]byte, error) {
	return canonical.MarshalIndent(s)
}

// writeBundleShapedSnapshotJSON writes just s's canonical document to
// dir/snapshot.json, with no files/ tree beside it -- the same shape
// core/bundle's Assemble (assemble.go's writeSnapshotJSON) writes at a
// bundle's root.
func writeBundleShapedSnapshotJSON(t *testing.T, dir string, s *Snapshot) {
	t.Helper()
	doc, err := canonicalIndent(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DocumentName), doc, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWriteTarEntryRaw(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
}

// tamperTarFileEntry rebuilds a tar byte stream with one byte flipped in the
// content of the files/<archivePath> entry, leaving every other entry
// (including snapshot.json) byte-for-byte identical.
func tamperTarFileEntry(t *testing.T, tarBytes []byte, archivePath string) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	found := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == FilesDir+"/"+archivePath && len(data) > 0 {
			data[0] ^= 0xFF
			found = true
		}
		newHdr := *hdr
		newHdr.Size = int64(len(data))
		if err := tw.WriteHeader(&newHdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("archive entry files/%s not found in tar to tamper with", archivePath)
	}
	return out.Bytes()
}

func minimalValidSnapshotJSON(t *testing.T) []byte {
	t.Helper()
	s := &Snapshot{
		SchemaVersion: SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Target:        Target{DistroID: "debian", VersionID: "12", Arch: "amd64"},
		DpkgStatus:    File{Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status", SHA256: sha256Hex(nil)},
	}
	doc, err := canonical.MarshalIndent(s)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// --- "that is not a snapshot" is a usage error, not a verification failure ---
//
// dferr.Verification (exit 4) means a signature, digest or metadata check
// failed (ADR-012). Input that was never a snapshot in the first place — the
// wrong file from a file picker, a directory that is not one — is an
// ordinary command-line mistake and must classify as dferr.Usage (exit 1),
// or an operator who mis-clicked is told their media may have been tampered
// with.

func TestOpenNonArchiveIsUsageNotVerification(t *testing.T) {
	cases := map[string][]byte{
		"plain text":      []byte("this is not a snapshot at all\n"),
		"empty file":      {},
		"three bytes":     {0x28, 0xB5, 0x2F},
		"gzip magic":      {0x1F, 0x8B, 0x08, 0x00, 0x00},
		"deb ar archive":  []byte("!<arch>\ndebian-binary   1234567890  0     0     100644  4         `\n2.0\n"),
		"a json document": []byte(`{"schema_version":"debark.snapshot/v1"}`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "picked-by-mistake")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Open(context.Background(), path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %s, want usage (exit 1); message: %v", got, err)
			}
			if !strings.Contains(err.Error(), "not a debark snapshot") {
				t.Errorf("message does not say what is wrong: %v", err)
			}
			if dferr.HintOf(err) == "" {
				t.Error("want a hint saying how to get a real snapshot")
			}
		})
	}
}

func TestOpenDirectoryWithoutDocumentIsUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), dir)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class = %s, want usage; message: %v", got, err)
	}
}

func TestOpenZstdArchiveWithoutDocumentIsUsage(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarEntryRaw(t, tw, "some-other-archive.txt", []byte("not a snapshot"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed, err := zstdCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "other.tar.zst")
	if err := os.WriteFile(path, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class = %s, want usage; message: %v", got, err)
	}
}

func TestOpenDocumentMissingDocumentIsUsage(t *testing.T) {
	for name, path := range map[string]string{
		"missing file":            filepath.Join(t.TempDir(), "nope.json"),
		"directory without a doc": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := OpenDocument(context.Background(), path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class = %s, want usage; message: %v", got, err)
			}
		})
	}
}

// A real, well-formed archive whose CONTENT is wrong must still be
// verification: this change must not widen into "everything is a usage
// error". TestOpenDigestMismatchRejected covers the tampered case directly;
// this pins the pairing so the two cannot drift apart.
func TestOpenTruncatedZstdStaysVerification(t *testing.T) {
	s, fs := mustCapture(t, CaptureOptions{Root: "testdata/ubuntu2404-deb822"})
	full := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := WriteArchive(full, s, fs); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.tar.zst")
	if err := os.WriteFile(truncated, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), truncated)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("class = %s, want verification: the zstd header IS a snapshot's, so this is a damaged snapshot, not a wrong file", got)
	}
}
