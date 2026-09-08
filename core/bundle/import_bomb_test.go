package bundle

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/inferops/debark/core/dferr"
)

// withImportEntryLimit temporarily lowers maxImportEntrySize for the
// duration of one test, restored on cleanup. The real 8 GiB limit cannot be
// exercised directly without a test that writes multiple gigabytes of
// actual data (archive/tar refuses to let a header claim more bytes than
// are actually written) — see maxImportEntrySize's own doc comment.
func withImportEntryLimit(t *testing.T, n int64) {
	t.Helper()
	orig := maxImportEntrySize
	maxImportEntrySize = n
	t.Cleanup(func() { maxImportEntrySize = orig })
}

// withImportEntryCountLimit and withImportTotalLimit do the same for the two
// whole-archive ceilings, for the same reason: a test that genuinely built
// 50,000 entries takes 13 seconds and one that wrote 64 GiB is not a test at
// all. Lowering the ceiling exercises the same code path the real number
// guards, and the real numbers themselves are justified in tar.go.
func withImportEntryCountLimit(t *testing.T, n int) {
	t.Helper()
	orig := maxImportEntries
	maxImportEntries = n
	t.Cleanup(func() { maxImportEntries = orig })
}

func withImportTotalLimit(t *testing.T, n int64) {
	t.Helper()
	orig := maxImportTotalSize
	maxImportTotalSize = n
	t.Cleanup(func() { maxImportTotalSize = orig })
}

// buildZeroArchive writes an archive of n entries, each sizeEach bytes of
// zeros, streaming the payload instead of holding it in memory — this is the
// shape of the real expansion bomb (zeros compress to almost nothing, so the
// archive stays tiny however much it expands), scaled down to run fast.
func buildZeroArchive(t *testing.T, n int, sizeEach int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bomb.tar.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	tw := tar.NewWriter(zw)
	buf := make([]byte, 64<<10)
	for i := 0; i < n; i++ {
		hdr := &tar.Header{
			Name:     fmt.Sprintf("repo/pool/z/zeros/zeros_%06d_amd64.deb", i),
			Typeflag: tar.TypeReg,
			Size:     sizeEach,
			Mode:     0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %d: %v", i, err)
		}
		for left := sizeEach; left > 0; {
			c := int64(len(buf))
			if c > left {
				c = left
			}
			if _, err := tw.Write(buf[:c]); err != nil {
				t.Fatalf("write payload %d: %v", i, err)
			}
			left -= c
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return path
}

// treeStats reports how much a refused import actually managed to write, so
// a test can assert the bound stopped the writing rather than merely
// reporting on it afterwards.
func treeStats(t *testing.T, dir string) (files int, bytes int64) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		bytes += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files, bytes
}

// TestImportRefusesEntryOverSizeLimit is the regression test for F6
// (docs/security/review-findings.md) applied to bundle import: a tar entry
// whose actual decompressed content exceeds the configured limit must be
// refused — classified as a verification problem (hostile input), not an
// environment one — rather than written to disk in full or silently
// truncated.
func TestImportRefusesEntryOverSizeLimit(t *testing.T) {
	withImportEntryLimit(t, 1024)

	archive := buildRawArchive(t, []rawEntry{
		{name: "repo/pool/d/demo/demo_1.0_amd64.deb", typeflag: tar.TypeReg, data: make([]byte, 2048)},
	})
	dst := filepath.Join(t.TempDir(), "out")

	err := ImportTar(context.Background(), archive, dst)
	if err == nil {
		t.Fatal("ImportTar accepted an entry twice the configured limit")
	}
	if !errors.Is(err, errEntryTooLarge) {
		t.Errorf("error does not wrap errEntryTooLarge, so some other check refused this archive: %v", err)
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf(err) = %v, want %v (a hostile-input finding, not an environment failure): %v", got, dferr.Verification, err)
	}
	assertNothingEscaped(t, dst)
}

// TestImportAcceptsEntryUnderSizeLimit proves the limit is on the right
// side of real usage: an entry under the configured limit is written
// through in full, byte for byte.
func TestImportAcceptsEntryUnderSizeLimit(t *testing.T) {
	withImportEntryLimit(t, 4096)

	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	archive := buildRawArchive(t, []rawEntry{
		{name: "repo/pool/d/demo/demo_1.0_amd64.deb", typeflag: tar.TypeReg, data: payload},
	})
	dst := filepath.Join(t.TempDir(), "out")

	if err := ImportTar(context.Background(), archive, dst); err != nil {
		t.Fatalf("ImportTar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "repo", "pool", "d", "demo", "demo_1.0_amd64.deb"))
	if err != nil {
		t.Fatalf("reading imported file: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("imported file is %d bytes, want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("imported file content differs at byte %d", i)
		}
	}
}

// TestImportRefusesArchiveOverTotalSizeLimit is the regression test for the
// hole the per-entry limit left open: every entry here is far below
// maxImportEntrySize, which stays at its production value for this test, so
// nothing but the whole-archive ceiling can refuse this archive. Before that
// ceiling existed, this exact shape — 32 entries of 64 MiB of zeros, a
// 232 KB archive — expanded to 2 GiB on disk in 1.8 seconds and returned
// nil.
func TestImportRefusesArchiveOverTotalSizeLimit(t *testing.T) {
	const total = 1 << 20 // 1 MiB
	withImportTotalLimit(t, total)

	archive := buildZeroArchive(t, 8, 1<<20) // 8 MiB of zeros in 8 entries
	if fi, err := os.Stat(archive); err == nil {
		t.Logf("archive is %d bytes and declares 8 MiB of content", fi.Size())
	}
	dst := filepath.Join(t.TempDir(), "out")

	err := ImportTar(context.Background(), archive, dst)
	if err == nil {
		t.Fatal("ImportTar accepted an archive expanding to eight times the whole-archive limit")
	}
	if !errors.Is(err, errArchiveTooLarge) {
		t.Errorf("error does not wrap errArchiveTooLarge, so some other check refused this archive: %v", err)
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf(err) = %v, want %v: %v", got, dferr.Verification, err)
	}

	// The bound has to stop the writing, not report on it afterwards. One
	// byte of slack: extractRegularFile reads limit+1 bytes to tell "hit the
	// limit" apart from "was exactly that long".
	_, written := treeStats(t, dst)
	if written > total+1 {
		t.Errorf("refused import still wrote %d bytes, over the %d-byte ceiling: the limit is being checked after the fact, not during", written, total)
	}
}

// TestImportAcceptsArchiveExactlyAtTotalSizeLimit pins the boundary: an
// archive whose contents sum to exactly the ceiling is a legitimate bundle,
// not a bomb, and must import in full.
func TestImportAcceptsArchiveExactlyAtTotalSizeLimit(t *testing.T) {
	const total = 4 << 10
	withImportTotalLimit(t, total)

	archive := buildZeroArchive(t, 4, 1<<10) // 4 x 1 KiB = exactly 4 KiB
	dst := filepath.Join(t.TempDir(), "out")

	if err := ImportTar(context.Background(), archive, dst); err != nil {
		t.Fatalf("ImportTar refused an archive exactly at the total limit: %v", err)
	}
	files, written := treeStats(t, dst)
	if files != 4 || written != total {
		t.Errorf("imported %d files / %d bytes, want 4 / %d", files, written, total)
	}
}

// TestImportRefusesTooManyEntries covers the other half of the hole: an
// attacker who cannot make one entry big enough simply writes more of them.
// Every entry here is empty, so neither the per-entry nor the total-byte
// ceiling can fire; only the entry count can. Before it existed, a 137 KB
// archive created 50,000 files in 13 seconds and returned nil.
func TestImportRefusesTooManyEntries(t *testing.T) {
	const limit = 8
	withImportEntryCountLimit(t, limit)

	archive := buildZeroArchive(t, limit+1, 0)
	dst := filepath.Join(t.TempDir(), "out")

	err := ImportTar(context.Background(), archive, dst)
	if err == nil {
		t.Fatal("ImportTar accepted an archive with more entries than the limit allows")
	}
	if !errors.Is(err, errTooManyEntries) {
		t.Errorf("error does not wrap errTooManyEntries, so some other check refused this archive: %v", err)
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf(err) = %v, want %v: %v", got, dferr.Verification, err)
	}
	if files, _ := treeStats(t, dst); files > limit {
		t.Errorf("refused import created %d files, over the %d-entry ceiling", files, limit)
	}
}

// TestImportAcceptsExactlyTheEntryCountLimit pins that boundary too: the
// ceiling is a maximum, not a strict upper bound, so a bundle with exactly
// that many files still imports.
func TestImportAcceptsExactlyTheEntryCountLimit(t *testing.T) {
	const limit = 8
	withImportEntryCountLimit(t, limit)

	archive := buildZeroArchive(t, limit, 16)
	dst := filepath.Join(t.TempDir(), "out")

	if err := ImportTar(context.Background(), archive, dst); err != nil {
		t.Fatalf("ImportTar refused an archive with exactly %d entries: %v", limit, err)
	}
	if files, _ := treeStats(t, dst); files != limit {
		t.Errorf("imported %d files, want %d", files, limit)
	}
}

// TestImportCountsDirectoryEntriesTowardsTheEntryLimit closes the obvious
// way around the counter: directory members cost a syscall each and are
// just as cheap to generate as empty files.
func TestImportCountsDirectoryEntriesTowardsTheEntryLimit(t *testing.T) {
	withImportEntryCountLimit(t, 3)

	entries := make([]rawEntry, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, rawEntry{name: fmt.Sprintf("repo/pool/d%02d", i), typeflag: tar.TypeDir})
	}
	archive := buildRawArchive(t, entries)
	dst := filepath.Join(t.TempDir(), "out")

	err := ImportTar(context.Background(), archive, dst)
	if err == nil {
		t.Fatal("ImportTar accepted five directory entries under a three-entry limit")
	}
	if !errors.Is(err, errTooManyEntries) {
		t.Errorf("error does not wrap errTooManyEntries: %v", err)
	}
}

// TestOpenRemovesTheTempDirWhenImportIsRefused is the point of the whole
// exercise. Open extracts an untrusted archive into os.MkdirTemp before
// anything has verified it — `debark inspect` and `debark doctor` on
// freshly arrived media are exactly this call — and it removes that
// directory only when importTar returns an error. Until importTar had these
// ceilings there was no error to trigger the cleanup, so the expansion sat
// in the operator's temp filesystem.
func TestOpenRemovesTheTempDirWhenImportIsRefused(t *testing.T) {
	withImportEntryCountLimit(t, 4)
	archive := buildZeroArchive(t, 40, 512)

	// Establish the precondition rather than assuming it: importTar really
	// does leave files behind on the refusal path, so "the temp directory is
	// empty afterwards" below cannot pass vacuously.
	partial := filepath.Join(t.TempDir(), "partial")
	if err := ImportTar(context.Background(), archive, partial); err == nil {
		t.Fatal("ImportTar accepted the bomb archive")
	}
	if files, _ := treeStats(t, partial); files == 0 {
		t.Fatal("test is vacuous: ImportTar wrote nothing before refusing, so there is no partial extraction for Open to clean up")
	}

	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot) // unix
	t.Setenv("TMP", tmpRoot)    // windows
	t.Setenv("TEMP", tmpRoot)
	if got := filepath.Clean(os.TempDir()); !strings.HasPrefix(got, filepath.Clean(tmpRoot)) {
		t.Fatalf("test setup: os.TempDir() = %s, not under %s; this test cannot observe Open's temp directory", got, tmpRoot)
	}

	b, err := Open(context.Background(), archive)
	if err == nil {
		b.Close()
		t.Fatal("Open accepted the bomb archive")
	}
	if !errors.Is(err, errTooManyEntries) {
		t.Errorf("Open failed for some other reason than the entry-count ceiling: %v", err)
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf(err) = %v, want %v: %v", got, dferr.Verification, err)
	}
	left, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read %s: %v", tmpRoot, err)
	}
	for _, e := range left {
		files, bytes := treeStats(t, filepath.Join(tmpRoot, e.Name()))
		t.Errorf("Open left %s behind after refusing the archive (%d files, %d bytes)", e.Name(), files, bytes)
	}
}

// TestImportRefusesDuplicateEntries: two members for one path is never
// something exportTar produces (it walks a real directory tree and sorts
// unique paths), and accepting it last-writer-wins means the tree on disk is
// not the tree the archive appears to describe. The Windows cases are the
// same defect wearing a different hat — that platform folds case and strips
// trailing dots and spaces, so all four of these names are one file there.
func TestImportRefusesDuplicateEntries(t *testing.T) {
	cases := []struct {
		desc   string
		first  string
		second string
	}{
		{"exact duplicate", "lock.json", "lock.json"},
		{"differing only in case", "lock.json", "LOCK.JSON"},
		{"trailing dot", "lock.json", "lock.json."},
		{"trailing space", "lock.json", "lock.json "},
		{"duplicate pool file", "repo/pool/j/jq/jq_1.0_amd64.deb", "repo/pool/j/jq/jq_1.0_amd64.deb"},
		{"file colliding with an earlier directory", "repo/pool", "repo/pool"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			archive := buildRawArchive(t, []rawEntry{
				{name: c.first, typeflag: tar.TypeReg, data: []byte(`{"real":true}`)},
				{name: c.second, typeflag: tar.TypeReg, data: []byte(`{"swapped":true}`)},
			})
			dst := filepath.Join(t.TempDir(), "out")

			err := ImportTar(context.Background(), archive, dst)
			if err == nil {
				t.Fatalf("ImportTar accepted %q followed by %q", c.first, c.second)
			}
			if !errors.Is(err, errDuplicateEntry) {
				t.Errorf("error does not wrap errDuplicateEntry: %v", err)
			}
			if got := dferr.ClassOf(err); got != dferr.Verification {
				t.Errorf("dferr.ClassOf(err) = %v, want %v: %v", got, dferr.Verification, err)
			}
		})
	}
}

// TestImportAcceptsDistinctNamesThatOnlyLookAlike guards the duplicate check
// against being too eager: a real pool holds many similar names, and none of
// them may be mistaken for one another.
func TestImportAcceptsDistinctNamesThatOnlyLookAlike(t *testing.T) {
	archive := buildRawArchive(t, []rawEntry{
		{name: "lock.json", typeflag: tar.TypeReg, data: []byte(`{}`)},
		{name: "lock.jsonx", typeflag: tar.TypeReg, data: []byte(`{}`)},
		{name: "sub/lock.json", typeflag: tar.TypeReg, data: []byte(`{}`)},
		{name: "repo/pool/j/jq/jq_1.6-2.1_amd64.deb", typeflag: tar.TypeReg, data: []byte("a")},
		{name: "repo/pool/j/jq/jq_1.6-2.2_amd64.deb", typeflag: tar.TypeReg, data: []byte("b")},
		{name: "repo/pool/libj/libjq1/libjq1_1.6_amd64.deb", typeflag: tar.TypeReg, data: []byte("c")},
	})
	dst := filepath.Join(t.TempDir(), "out")

	if err := ImportTar(context.Background(), archive, dst); err != nil {
		t.Fatalf("ImportTar refused a bundle-shaped archive of distinct names: %v", err)
	}
	if files, _ := treeStats(t, dst); files != 6 {
		t.Errorf("imported %d files, want 6", files)
	}
}

// TestImportRefusesOverlongEntryNameAsVerification: an entry name is
// attacker-chosen, so an absurd one is a hostile-input finding. Without a
// length bound in safeRelPath the name reaches os.MkdirAll/os.OpenFile and
// comes back as an operating-system failure, which importTar classifies as
// dferr.Environment (exit 2, "the machine cannot do the job") — blaming the
// operator's disk for a string that arrived on the media.
func TestImportRefusesOverlongEntryNameAsVerification(t *testing.T) {
	cases := []struct {
		desc string
		name string
	}{
		{"whole name over the limit", strings.Repeat("d/", maxImportEntryNameLen/2+1) + "f.deb"},
		{"one component over the limit", "repo/" + strings.Repeat("a", maxImportNameComponentLen+1) + ".deb"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			archive := buildRawArchive(t, []rawEntry{
				{name: c.name, typeflag: tar.TypeReg, data: []byte("evil")},
			})
			dst := filepath.Join(t.TempDir(), "out")

			err := ImportTar(context.Background(), archive, dst)
			if err == nil {
				t.Fatalf("ImportTar accepted a %d-byte entry name", len(c.name))
			}
			if got := dferr.ClassOf(err); got != dferr.Verification {
				t.Errorf("dferr.ClassOf(err) = %v, want %v (attacker-chosen input, not an environment failure): %v", got, dferr.Verification, err)
			}
		})
	}
}

// TestImportErrorDoesNotEchoAnUnboundedName: a PAX entry name can be
// megabytes long, and an error message that quotes one in full turns a
// refusal into a log flood on the operator's console.
func TestImportErrorDoesNotEchoAnUnboundedName(t *testing.T) {
	huge := strings.Repeat("x", 200000) + ".deb"
	archive := buildRawArchive(t, []rawEntry{{name: huge, typeflag: tar.TypeReg, data: []byte("evil")}})
	dst := filepath.Join(t.TempDir(), "out")

	err := ImportTar(context.Background(), archive, dst)
	if err == nil {
		t.Fatal("ImportTar accepted a 200 KB entry name")
	}
	if len(err.Error()) > 1024 {
		t.Errorf("error message is %d bytes: an attacker-chosen name is being echoed unbounded", len(err.Error()))
	}
}
