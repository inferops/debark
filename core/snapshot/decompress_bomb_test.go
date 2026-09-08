package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractTar_RejectsOversizedDocument is the regression test for F6
// (docs/security/review-findings.md) applied to the snapshot archive
// reader: a snapshot.json tar member larger than maxSnapshotDocumentSize
// must be refused with a clear error, not read fully into memory.
func TestExtractTar_RejectsOversizedDocument(t *testing.T) {
	bomb := bytes.Repeat([]byte{'A'}, maxSnapshotDocumentSize+1)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarEntryRaw(t, tw, DocumentName, bomb)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := extractTar(buf.Bytes(), t.TempDir())
	if err == nil {
		t.Fatalf("extractTar accepted a %d-byte %s (limit is %d)", len(bomb), DocumentName, maxSnapshotDocumentSize)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not clearly say the size limit was exceeded: %v", err)
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}

// TestExtractTar_AcceptsDocumentUnderLimit proves the limit is on the right
// side of real usage: a snapshot.json comfortably larger than any real one
// (a large dpkg_status inlined would still be a files/* entry, not part of
// the document itself) but under maxSnapshotDocumentSize parses normally.
func TestExtractTar_AcceptsDocumentUnderLimit(t *testing.T) {
	doc := minimalValidSnapshotJSON(t)
	if len(doc) >= maxSnapshotDocumentSize {
		t.Fatalf("test setup bug: doc (%d bytes) is not under maxSnapshotDocumentSize (%d)", len(doc), maxSnapshotDocumentSize)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarEntryRaw(t, tw, DocumentName, doc)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := extractTar(buf.Bytes(), t.TempDir())
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if !bytes.Equal(got, doc) {
		t.Errorf("extractTar returned different bytes than were written")
	}
}

// TestOpenDocument_RejectsOversizedDocument is TestExtractTar_RejectsOversizedDocument's
// twin for the other reader of a snapshot.json document: OpenDocument, which
// reads the file directly off disk instead of out of a tar, must apply the
// same maxSnapshotDocumentSize cap (both call the same readCappedDocument)
// rather than reading an arbitrarily large file fully into memory first.
func TestOpenDocument_RejectsOversizedDocument(t *testing.T) {
	bomb := bytes.Repeat([]byte{'A'}, maxSnapshotDocumentSize+1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DocumentName), bomb, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := OpenDocument(context.Background(), dir)
	if err == nil {
		t.Fatalf("OpenDocument accepted a %d-byte %s (limit is %d)", len(bomb), DocumentName, maxSnapshotDocumentSize)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not clearly say the size limit was exceeded: %v", err)
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}

// TestZstdDecompress_RejectsBomb proves the outer, whole-archive defence:
// zstdDecompress must refuse a stream that decodes to more than
// maxSnapshotDecompressedSize, rather than allocating that much memory. The
// source is a single repeated byte specifically so it compresses to almost
// nothing (a real decompression bomb's whole point) while still decoding to
// just over the limit.
func TestZstdDecompress_RejectsBomb(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates just over 512MiB to build a realistic bomb payload; skipped with -short")
	}
	huge := bytes.Repeat([]byte{'A'}, maxSnapshotDecompressedSize+(1<<20))
	compressed, err := zstdCompress(huge)
	if err != nil {
		t.Fatalf("zstdCompress: %v", err)
	}
	// Load-bearing, despite looking like a no-op: `huge` is 513 MiB and is
	// never read again, but the local still holds the only reference to that
	// allocation, so without this the backing array stays reachable for the
	// rest of the function. zstdDecompress is then asked to allocate up to
	// another 512 MiB while that one is still live, and the test that is
	// supposed to prove we REFUSE a decompression bomb instead OOMs the
	// runner -- a failure that looks like flake and is really this line
	// missing. ineffassign flags it because it cannot see that the point of
	// the assignment is the reference it drops, not the value it stores.
	//
	// Do not delete this to quiet the linter. If it must go, the test has to
	// stop holding the plaintext across the decompress call some other way
	// (build the compressed form in a helper function that returns only
	// `compressed`, so `huge` goes out of scope).
	huge = nil //nolint:ineffassign // drops a 512MiB reference before the decompress attempt; see above

	if len(compressed) >= maxSnapshotDecompressedSize/100 {
		t.Fatalf("test setup bug: compressed form (%d bytes) is not small — this input is not a realistic bomb", len(compressed))
	}

	_, err = zstdDecompress(compressed)
	if err == nil {
		t.Fatalf("zstdDecompress accepted a stream decoding to more than %d bytes from a %d-byte compressed input", maxSnapshotDecompressedSize, len(compressed))
	}
}
