package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// realisticBundle writes a tree shaped like a real debark bundle: a flat apt
// pool with nested directories, one package big enough to need several copy
// buffers, an empty file, an empty directory, and a non-ASCII name — because
// every one of those has broken a naive copier at some point.
func realisticBundle(t *testing.T, root string) map[string][]byte {
	t.Helper()

	big := make([]byte, 3<<20) // larger than the 1 MiB buffer, so multi-chunk
	for i := range big {
		big[i] = byte(i%251) ^ byte(i>>13)
	}

	want := map[string][]byte{
		"manifest.json": []byte(`{"schema":"debark.bundle/v1"}` + "\n"),
		"lock.plan":     []byte("apt 2.4.10\nzzz 1.0\n"),
		"empty.marker":  {},
		"dists/stable/main/binary-amd64/Packages": []byte("Package: apt\nVersion: 2.4.10\n\n"),
		"pool/main/a/apt_2.4.10_amd64.deb":        big,
		"pool/main/z/zzz_1.0_all.deb":             []byte("a small package"),
		"pool/universe/p/pügins_ähh_中文_1.0.deb":   []byte("non-ascii names are ordinary here"),
	}

	for rel, body := range want {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An empty directory: structure the copy should still reproduce.
	if err := os.MkdirAll(filepath.Join(root, "pool", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return want
}

// flatBundle writes n equally sized files named a.bin, b.bin, ... so the copy
// order is predictable and a cancel can be aimed at a known file.
func flatBundle(t *testing.T, root string, n, size int) []string {
	t.Helper()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := string(rune('a'+i)) + ".bin"
		body := bytes.Repeat([]byte{byte('a' + i)}, size)
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func assertDestMatches(t *testing.T, dst string, want map[string][]byte) {
	t.Helper()
	got := map[string][]byte{}
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dst, path)
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		got[filepath.ToSlash(rel)] = body
		return nil
	})
	if err != nil {
		t.Fatalf("walking the destination: %v", err)
	}

	for rel, wantBody := range want {
		gotBody, ok := got[rel]
		if !ok {
			t.Errorf("%s is missing from the destination", rel)
			continue
		}
		if !bytes.Equal(gotBody, wantBody) {
			t.Errorf("%s differs: %d bytes on the drive, %d in the bundle", rel, len(gotBody), len(wantBody))
		}
		delete(got, rel)
	}
	for rel := range got {
		t.Errorf("%s appeared on the destination but is not in the bundle", rel)
	}
}

func mustPlan(t *testing.T, e *Exporter, req Request) *Plan {
	t.Helper()
	p, err := e.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

func assertMarkerInDirectory(t *testing.T, marker, dir string) {
	t.Helper()
	if filepath.Base(marker) != MarkerName {
		t.Fatalf("marker filename = %q, want %q", filepath.Base(marker), MarkerName)
	}
	want, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(filepath.Dir(marker))
	if err != nil {
		t.Fatal(err)
	}
	// Plan resolves aliases. Windows can change case or expand an 8.3 name,
	// so compare directory identity rather than the caller's path spelling.
	if !os.SameFile(got, want) {
		t.Fatalf("marker %q is not in destination %q", marker, dir)
	}
}

// ---------------------------------------------------------------------------
// the happy path, byte for byte
// ---------------------------------------------------------------------------

func TestExportCopiesARealisticBundleByteForByte(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "bundle")
	want := realisticBundle(t, src)

	e := NewExporter(Options{})
	res, err := e.Export(context.Background(), Request{
		SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	assertDestMatches(t, dst, want)

	if !res.Complete {
		t.Error("Result.Complete is false after a clean, verified export")
	}
	if res.Copy.FilesCopied != len(want) {
		t.Errorf("copied %d files, want %d", res.Copy.FilesCopied, len(want))
	}
	var totalBytes int64
	for _, body := range want {
		totalBytes += int64(len(body))
	}
	if res.Copy.BytesCopied != totalBytes {
		t.Errorf("copied %d bytes, want %d", res.Copy.BytesCopied, totalBytes)
	}
	if !res.Verification.OK || !res.Verification.MarkerRemoved {
		t.Errorf("verification did not pass cleanly: %+v", res.Verification)
	}
	if res.Verification.FilesChecked != len(want) || res.Verification.BytesChecked != totalBytes {
		t.Errorf("verification checked %d files / %d bytes, want %d / %d",
			res.Verification.FilesChecked, res.Verification.BytesChecked, len(want), totalBytes)
	}
	if !strings.Contains(res.Verification.Method, "SHA-256") {
		t.Errorf("the report does not say what was verified: %q", res.Verification.Method)
	}
	assertSentence(t, "verification method", res.Verification.Method)
	assertSentence(t, "verification caveat", res.Verification.Caveat)
	assertSentence(t, "result summary", res.Summary)

	// The empty directory is structure, and structure is copied.
	if fi, err := os.Stat(filepath.Join(dst, "pool", "empty")); err != nil || !fi.IsDir() {
		t.Errorf("the empty directory was not reproduced: %v", err)
	}
	// A finished export leaves no marker.
	if incomplete, err := IsIncomplete(dst); err != nil || incomplete {
		t.Errorf("the incomplete marker survived a clean export (%v, %v)", incomplete, err)
	}
	if res.Copy.Duration < 0 || res.Duration < 0 {
		t.Error("negative durations")
	}
}

func TestExportCreatesTheDestinationDirectory(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "new-folder")
	realisticBundle(t, src)

	e := NewExporter(Options{})
	if _, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		t.Fatalf("the destination was not created: %v", err)
	}
}

// ---------------------------------------------------------------------------
// stage 1: refuse before starting
// ---------------------------------------------------------------------------

func TestPlanRefusesWhenTheBundleWillNotFit(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 3, 100*1024)

	e := NewExporter(Options{})
	// Injected free space: no real full disk needed, and none ever will be.
	_, err := e.Plan(context.Background(), Request{
		SourceDir: src, DestDir: dst, DestFreeBytes: 200 * 1024,
	})
	ee := exportErr(t, err)
	if ee.Kind != ErrKindNotEnoughRoom {
		t.Fatalf("kind = %q, want %q", ee.Kind, ErrKindNotEnoughRoom)
	}
	if !IsOutOfSpace(err) {
		t.Error("IsOutOfSpace did not recognise the refusal")
	}
	assertSentence(t, "summary", ee.Summary)
	assertSentence(t, "hint", ee.Hint)
	for _, want := range []string{"needs", "free", "safety margin"} {
		if !strings.Contains(ee.Summary, want) {
			t.Errorf("summary %q does not mention %q", ee.Summary, want)
		}
	}
	// Nothing may have been written by a refusal.
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("a refused plan wrote %d entries to the destination", len(entries))
	}
}

func TestPlanRefusesAFullDriveReportingZeroFree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 1, 10)

	e := NewExporter(Options{})
	_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 0})
	if !IsOutOfSpace(err) {
		t.Fatalf("zero free bytes was not treated as a full drive: %v", err)
	}
}

func TestPlanAcceptsWhatFitsAndAccountsForTheMargin(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 4, 100*1024)

	e := NewExporter(Options{MarginBytes: 1 << 20})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 100 << 20})

	if p.TotalFiles != 4 || p.TotalBytes != 4*100*1024 {
		t.Errorf("plan counted %d files / %d bytes", p.TotalFiles, p.TotalBytes)
	}
	if p.MarginBytes != 1<<20 {
		t.Errorf("margin = %d, want the override", p.MarginBytes)
	}
	// Each 100 KiB file is charged a whole number of 32 KiB allocation units.
	wantOnDisk := int64(4 * roundUp(100*1024, allocationUnit))
	if p.RequiredBytes != wantOnDisk+p.MarginBytes {
		t.Errorf("RequiredBytes = %d, want %d", p.RequiredBytes, wantOnDisk+p.MarginBytes)
	}
	if p.RequiredBytes <= p.TotalBytes {
		t.Error("RequiredBytes must exceed TotalBytes: it carries the margin and the allocation rounding")
	}
	if !p.VerifyPlanned {
		t.Error("verification is not planned by default")
	}
	assertMarkerInDirectory(t, p.MarkerPath, dst)
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", p.Warnings)
	}
}

func TestPlanWarnsWhenFreeSpaceIsUnknown(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 2, 1024)

	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: FreeSpaceUnknown})
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "could not be measured") {
		t.Fatalf("warnings = %v, want one about unmeasurable free space", p.Warnings)
	}
	assertSentence(t, "warning", p.Warnings[0])
}

func TestPlanRefusesAMissingOrEmptyOrFileSource(t *testing.T) {
	dst := t.TempDir()
	e := NewExporter(Options{})

	t.Run("missing", func(t *testing.T) {
		_, err := e.Plan(context.Background(), Request{
			SourceDir: filepath.Join(t.TempDir(), "nope"), DestDir: dst, DestFreeBytes: 1 << 30,
		})
		ee := exportErr(t, err)
		if ee.Kind != ErrKindSourceMissing {
			t.Errorf("kind = %q", ee.Kind)
		}
		assertSentence(t, "summary", ee.Summary)
	})

	t.Run("empty", func(t *testing.T) {
		_, err := e.Plan(context.Background(), Request{
			SourceDir: t.TempDir(), DestDir: dst, DestFreeBytes: 1 << 30,
		})
		ee := exportErr(t, err)
		if ee.Kind != ErrKindEmptySource {
			t.Errorf("kind = %q", ee.Kind)
		}
		assertSentence(t, "summary", ee.Summary)
	})

	t.Run("a file, not a folder", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "bundle.tar")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := e.Plan(context.Background(), Request{SourceDir: f, DestDir: dst, DestFreeBytes: 1 << 30})
		ee := exportErr(t, err)
		if ee.Kind != ErrKindNotDirectory {
			t.Errorf("kind = %q", ee.Kind)
		}
		assertSentence(t, "summary", ee.Summary)
	})
}

func TestPlanRefusesOverlappingSourceAndDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "bundle")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	flatBundle(t, src, 1, 16)

	e := NewExporter(Options{})
	cases := map[string]string{
		"the same folder":        src,
		"inside the bundle":      filepath.Join(src, "usb"),
		"a parent of the bundle": root,
	}
	for name, dst := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
			ee := exportErr(t, err)
			if ee.Kind != ErrKindOverlap {
				t.Errorf("kind = %q, want %q", ee.Kind, ErrKindOverlap)
			}
			assertSentence(t, "summary", ee.Summary)
			assertSentence(t, "hint", ee.Hint)
		})
	}
}

func TestPlanRefusesAMissingDestinationParent(t *testing.T) {
	src := t.TempDir()
	flatBundle(t, src, 1, 16)
	dst := filepath.Join(t.TempDir(), "gone", "deeper")

	e := NewExporter(Options{})
	_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	ee := exportErr(t, err)
	assertSentence(t, "summary", ee.Summary)
	if !strings.Contains(ee.Summary, "does not exist") {
		t.Errorf("summary = %q", ee.Summary)
	}
}

func TestPlanWarnsAboutNamesFATCannotStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot create a file with a colon in its name, which is the point of the test")
	}
	src, dst := t.TempDir(), t.TempDir()
	// A colon is legal in a Debian pool path (an epoch) and illegal on FAT, in
	// both a file name and a directory name.
	if err := os.MkdirAll(filepath.Join(src, "pool", "epoch:1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pool", "epoch:1", "libfoo_1:2.3-4_amd64.deb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if len(p.Warnings) != 2 {
		t.Fatalf("warnings = %v, want one for the directory and one for the file", p.Warnings)
	}
	for _, w := range p.Warnings {
		if !strings.Contains(w, "FAT") {
			t.Errorf("warning does not mention FAT: %q", w)
		}
		assertSentence(t, "warning", w)
	}
}

func TestPlanWarnsAboutFilesFAT32CannotHold(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a 4 GB sparse file is not free on NTFS")
	}
	src, dst := t.TempDir(), t.TempDir()
	f, err := os.Create(filepath.Join(src, "huge.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fat32MaxFileSize + 1); err != nil {
		f.Close()
		t.Skipf("this filesystem will not make a sparse file: %v", err)
	}
	f.Close()

	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 40})
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "FAT32") {
			found = true
			assertSentence(t, "warning", w)
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want one about the FAT32 file size limit", p.Warnings)
	}
}

func TestEquivalentCommandIsShowable(t *testing.T) {
	p := &Plan{SourceDir: filepath.FromSlash("/home/op/bundle"), DestDir: filepath.FromSlash("/media/usb/bundle")}
	got := p.EquivalentCommand()
	if len(got) == 0 || got[0] != "cp" {
		t.Fatalf("EquivalentCommand = %v", got)
	}
	if got[len(got)-1] != p.DestDir {
		t.Errorf("the destination is not the last argument: %v", got)
	}
}

// ---------------------------------------------------------------------------
// stage 3: verification actually catches things
// ---------------------------------------------------------------------------

// runToVerify does Plan and Run and hands back what Verify will need, so a test
// can damage the destination in between.
func runToVerify(t *testing.T, e *Exporter, src, dst string) (*Plan, *RunReport) {
	t.Helper()
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	run, err := e.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return p, run
}

func TestVerifyCatchesAFlippedByte(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	realisticBundle(t, src)
	e := NewExporter(Options{})
	p, run := runToVerify(t, e, src, dst)

	// Corrupt one byte, keeping the length identical — the exact failure a
	// size comparison cannot see.
	victim := filepath.Join(dst, filepath.FromSlash("pool/main/a/apt_2.4.10_amd64.deb"))
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0x01
	if err := os.WriteFile(victim, body, 0o644); err != nil {
		t.Fatal(err)
	}

	vr, err := e.Verify(context.Background(), p, run)
	if err == nil {
		t.Fatal("verification passed on a corrupted destination")
	}
	ee := exportErr(t, err)
	if ee.Kind != ErrKindVerifyFailed {
		t.Fatalf("kind = %q, want %q", ee.Kind, ErrKindVerifyFailed)
	}
	assertSentence(t, "summary", ee.Summary)
	assertSentence(t, "hint", ee.Hint)

	if vr.OK {
		t.Error("VerifyReport.OK is true after a mismatch")
	}
	if len(vr.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", vr.Mismatches)
	}
	m := vr.Mismatches[0]
	if m.Reason != MismatchContent {
		t.Errorf("reason = %q, want %q", m.Reason, MismatchContent)
	}
	if m.WantSHA256 == m.GotSHA256 || m.WantSHA256 == "" || m.GotSHA256 == "" {
		t.Errorf("both digests should be recorded and differ: %+v", m)
	}
	if m.WantSize != m.GotSize {
		t.Errorf("the sizes should be identical — that is why size alone is not verification: %+v", m)
	}

	// A failed verification must leave the destination marked unusable.
	if vr.MarkerRemoved {
		t.Error("the marker was removed despite a failed verification")
	}
	if incomplete, _ := IsIncomplete(dst); !incomplete {
		t.Error("the destination is not marked incomplete after a failed verification")
	}
}

func TestVerifyCatchesATruncatedFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	realisticBundle(t, src)
	e := NewExporter(Options{})
	p, run := runToVerify(t, e, src, dst)

	victim := filepath.Join(dst, filepath.FromSlash("pool/main/z/zzz_1.0_all.deb"))
	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(victim, fi.Size()-1); err != nil {
		t.Fatal(err)
	}

	vr, err := e.Verify(context.Background(), p, run)
	if err == nil {
		t.Fatal("verification passed on a truncated destination")
	}
	if len(vr.Mismatches) != 1 || vr.Mismatches[0].Reason != MismatchSize {
		t.Fatalf("mismatches = %+v, want one size mismatch", vr.Mismatches)
	}
	if incomplete, _ := IsIncomplete(dst); !incomplete {
		t.Error("the destination is not marked incomplete")
	}
}

func TestVerifyCatchesAMissingFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	realisticBundle(t, src)
	e := NewExporter(Options{})
	p, run := runToVerify(t, e, src, dst)

	if err := os.Remove(filepath.Join(dst, "manifest.json")); err != nil {
		t.Fatal(err)
	}

	vr, err := e.Verify(context.Background(), p, run)
	if err == nil {
		t.Fatal("verification passed with a file missing")
	}
	if len(vr.Mismatches) != 1 || vr.Mismatches[0].Reason != MismatchMissing {
		t.Fatalf("mismatches = %+v, want one missing file", vr.Mismatches)
	}
}

func TestVerifyReportsEveryBadFileNotJustTheFirst(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 3, 4096)
	e := NewExporter(Options{})
	p, run := runToVerify(t, e, src, dst)

	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(dst, name), bytes.Repeat([]byte{0}, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	vr, err := e.Verify(context.Background(), p, run)
	if err == nil {
		t.Fatal("verification passed on two corrupted files")
	}
	if len(vr.Mismatches) != 2 {
		t.Fatalf("mismatches = %+v, want two", vr.Mismatches)
	}
	if !strings.Contains(err.Error(), "2 files") {
		t.Errorf("the summary does not say how many files failed: %q", err.Error())
	}
}

func TestVerifyNeedsAPlanAndARun(t *testing.T) {
	e := NewExporter(Options{})
	if _, err := e.Verify(context.Background(), nil, nil); err == nil {
		t.Fatal("Verify accepted a nil plan")
	} else {
		assertSentence(t, "summary", exportErr(t, err).Summary)
	}
	if _, err := e.Run(context.Background(), nil); err == nil {
		t.Fatal("Run accepted a nil plan")
	} else {
		assertSentence(t, "summary", exportErr(t, err).Summary)
	}
}

// ---------------------------------------------------------------------------
// the incomplete marker
// ---------------------------------------------------------------------------

func TestTheMarkerIsPresentUntilVerificationPasses(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	realisticBundle(t, src)
	e := NewExporter(Options{})

	p, run := runToVerify(t, e, src, dst)

	if !run.MarkerPresent {
		t.Error("RunReport says the marker is absent right after the copy")
	}
	if !run.VerifyRequired {
		t.Error("RunReport does not record that a Verify is still owed")
	}
	if incomplete, err := IsIncomplete(dst); err != nil || !incomplete {
		t.Fatalf("a copied-but-unverified destination is not marked incomplete (%v, %v)", incomplete, err)
	}

	// The marker is readable, and leads with a sentence, because the person
	// most likely to open it is an operator holding the drive.
	body, err := os.ReadFile(p.MarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc markerDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the marker is not readable JSON: %v", err)
	}
	if !strings.Contains(doc.Warning, "INCOMPLETE") {
		t.Errorf("the marker does not warn: %q", doc.Warning)
	}
	if doc.SourceDir != p.SourceDir || doc.TotalFiles != p.TotalFiles {
		t.Errorf("the marker does not describe the export: %+v", doc)
	}

	if _, err := e.Verify(context.Background(), p, run); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if incomplete, err := IsIncomplete(dst); err != nil || incomplete {
		t.Errorf("the marker survived a passing verification (%v, %v)", incomplete, err)
	}
}

func TestSkipVerifyIsDeliberateAndSaysSo(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	want := realisticBundle(t, src)

	e := NewExporter(Options{SkipVerify: true})
	res, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	assertDestMatches(t, dst, want)

	if res.Plan.VerifyPlanned {
		t.Error("the plan claims verification is planned")
	}
	if res.Copy.VerifyRequired {
		t.Error("RunReport still asks for a Verify that will never come")
	}
	if incomplete, _ := IsIncomplete(dst); incomplete {
		t.Error("SkipVerify left the marker behind, which would make the drive permanently unusable")
	}
	if res.Verification.OK {
		t.Error("a skipped verification must not report OK")
	}
	if res.Complete {
		t.Error("an unchecked drive must never be reported as Complete")
	}
	if !strings.Contains(res.Verification.Method, "skipped") {
		t.Errorf("the report does not say verification was skipped: %q", res.Verification.Method)
	}
	if !strings.Contains(res.Summary, "not been checked") {
		t.Errorf("the summary hides the missing verification: %q", res.Summary)
	}
}

func TestIsIncompleteOnAnUntouchedFolder(t *testing.T) {
	incomplete, err := IsIncomplete(t.TempDir())
	if err != nil || incomplete {
		t.Fatalf("IsIncomplete = %v, %v; want false, nil", incomplete, err)
	}
}

// ---------------------------------------------------------------------------
// cancellation
// ---------------------------------------------------------------------------

func TestCancelMidCopyLeavesTheDocumentedState(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	const size = 8 * 1024
	flatBundle(t, src, 3, size) // a.bin, b.bin, c.bin, copied in that order

	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	opts := Options{
		BufferSize:       1024,
		ProgressInterval: time.Millisecond,
		now:              func() time.Time { clk.add(time.Millisecond); return clk.now() },
	}
	// Cancel part way through b.bin: a.bin is finished, c.bin never starts.
	opts.Progress = func(p Progress) {
		if p.BytesDone >= size+2048 {
			cancel()
		}
	}

	e := NewExporter(opts)
	res, err := e.Export(ctx, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if !IsCancelled(err) {
		t.Fatalf("error is not a cancellation: %v", err)
	}
	assertSentence(t, "Error()", err.Error())
	if !strings.Contains(err.Error(), MarkerName) {
		t.Errorf("the cancellation message does not name the marker: %q", err.Error())
	}
	if res.Complete {
		t.Error("Result.Complete is true after a cancellation")
	}

	// The documented post-cancel state, asserted item by item.
	if incomplete, _ := IsIncomplete(dst); !incomplete {
		t.Error("the destination is not marked incomplete after a cancel")
	}
	if body, err := os.ReadFile(filepath.Join(dst, "a.bin")); err != nil {
		t.Errorf("a file finished before the cancel is missing: %v", err)
	} else if len(body) != size {
		t.Errorf("a.bin is %d bytes, want %d", len(body), size)
	}
	if _, err := os.Stat(filepath.Join(dst, "b.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the half-written file was left on the drive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "c.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file after the cancel point exists: %v", err)
	}
	if res.Copy.FailedFile != "b.bin" {
		t.Errorf("FailedFile = %q, want b.bin", res.Copy.FailedFile)
	}
	if res.Copy.FilesCopied != 1 {
		t.Errorf("FilesCopied = %d, want 1", res.Copy.FilesCopied)
	}
	if !strings.Contains(res.Summary, "stopped after 1 of 3 files") {
		t.Errorf("the summary does not say how far it got: %q", res.Summary)
	}
}

func TestCancelBeforeTheCopyStarts(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 2, 1024)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e := NewExporter(Options{})
	_, err := e.Export(ctx, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if !IsCancelled(err) {
		t.Fatalf("error is not a cancellation: %v", err)
	}
	assertSentence(t, "Error()", err.Error())
}

// ---------------------------------------------------------------------------
// the drive being pulled, and other mid-copy failures
// ---------------------------------------------------------------------------

func TestDriveRemovedMidCopyRendersAsARemovedDrive(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 3, 8*1024)

	opts := Options{BufferSize: 1024}
	opts.openDest = failingDestAfter(10*1024, syscall.EIO)

	e := NewExporter(opts)
	res, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if !IsDriveRemoved(err) {
		t.Fatalf("error is not a drive removal: %v", err)
	}
	ee := exportErr(t, err)
	assertSentence(t, "summary", ee.Summary)
	assertSentence(t, "hint", ee.Hint)
	if !strings.Contains(ee.Summary, "unplugged") {
		t.Errorf("the message does not read as a removed drive: %q", ee.Summary)
	}
	// The raw error is still available for a details drawer, and only there.
	if !errors.Is(err, syscall.EIO) {
		t.Error("the underlying error was lost")
	}
	if res.Complete {
		t.Error("Result.Complete is true after the drive went away")
	}
	if incomplete, _ := IsIncomplete(dst); !incomplete {
		t.Error("the destination is not marked incomplete after a drive removal")
	}
	if res.Copy.FailedFile == "" {
		t.Error("the report does not name the file it stopped on")
	}
}

func TestDriveFullMidCopyRendersAsAFullDrive(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 3, 8*1024)

	opts := Options{BufferSize: 1024}
	opts.openDest = failingDestAfter(10*1024, syscall.ENOSPC)

	e := NewExporter(opts)
	_, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if !IsOutOfSpace(err) {
		t.Fatalf("error is not a full drive: %v", err)
	}
	ee := exportErr(t, err)
	assertSentence(t, "summary", ee.Summary)
	if !strings.Contains(ee.Summary, "ran out of space") {
		t.Errorf("the message does not read as a full drive: %q", ee.Summary)
	}
	if incomplete, _ := IsIncomplete(dst); !incomplete {
		t.Error("the destination is not marked incomplete after the drive filled up")
	}
}

func TestAReadOnlyDestinationIsRefusedBeforeCopying(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 2, 4096)

	// Writing the marker is the writability probe, so a destination that
	// rejects the marker must stop the export before any file is copied.
	opts := Options{}
	e := NewExporter(opts)
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})

	// Replace the destination directory with a file: the marker cannot be
	// created inside it, which is the same shape of failure as a read-only or
	// unwritable drive.
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(dst) })

	_, err := e.Run(context.Background(), p)
	if err == nil {
		t.Fatal("Run wrote into a destination it could not create")
	}
	assertSentence(t, "summary", exportErr(t, err).Summary)
}

// ---------------------------------------------------------------------------
// symbolic links and permissions
// ---------------------------------------------------------------------------

func TestSymlinksAreDereferencedIntoRealFiles(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	target := filepath.Join(src, "real.deb")
	if err := os.WriteFile(target, []byte("the real package bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(src, "link.deb")); err != nil {
		t.Skipf("this platform will not create a symbolic link: %v", err)
	}

	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})

	var linked *PlanFile
	for i := range p.Files {
		if p.Files[i].Rel == "link.deb" {
			linked = &p.Files[i]
		}
	}
	if linked == nil {
		t.Fatal("the symbolic link was silently dropped from the plan")
	}
	if !linked.Symlink {
		t.Error("the plan does not record that the entry was a link")
	}
	if linked.Size != int64(len("the real package bytes")) {
		t.Errorf("the plan sized the link, not its target: %d", linked.Size)
	}

	if _, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// On the drive it is an ordinary file. FAT and exFAT have no symbolic
	// links, so this is the only thing that can work there.
	fi, err := os.Lstat(filepath.Join(dst, "link.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t.Error("the copy recreated a symbolic link, which a FAT drive cannot hold")
	}
	body, err := os.ReadFile(filepath.Join(dst, "link.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the real package bytes" {
		t.Errorf("the dereferenced file has the wrong content: %q", body)
	}
}

func TestSymlinkFailPolicyRefusesLinks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	target := filepath.Join(src, "real.deb")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(src, "link.deb")); err != nil {
		t.Skipf("this platform will not create a symbolic link: %v", err)
	}

	e := NewExporter(Options{Symlinks: SymlinkFail})
	_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	ee := exportErr(t, err)
	if ee.Kind != ErrKindUnsupported {
		t.Errorf("kind = %q, want %q", ee.Kind, ErrKindUnsupported)
	}
	assertSentence(t, "summary", ee.Summary)
	assertSentence(t, "hint", ee.Hint)
}

func TestABrokenOrDirectoryLinkIsRefusedNotSkipped(t *testing.T) {
	e := NewExporter(Options{})

	t.Run("dangling", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "real.deb"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(src, "gone.deb"), filepath.Join(src, "link.deb")); err != nil {
			t.Skipf("this platform will not create a symbolic link: %v", err)
		}
		_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
		ee := exportErr(t, err)
		if ee.Kind != ErrKindUnsupported {
			t.Errorf("kind = %q", ee.Kind)
		}
		assertSentence(t, "summary", ee.Summary)
	})

	t.Run("to a directory", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(src, "pool"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "pool", "a.deb"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(src, "pool"), filepath.Join(src, "alias")); err != nil {
			t.Skipf("this platform will not create a symbolic link: %v", err)
		}
		_, err := e.Plan(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
		ee := exportErr(t, err)
		if ee.Kind != ErrKindUnsupported {
			t.Errorf("kind = %q", ee.Kind)
		}
		assertSentence(t, "summary", ee.Summary)
	})
}

func TestPermissionBitsAreNotReplicated(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	readOnly := filepath.Join(src, "read-only.deb")
	if err := os.WriteFile(readOnly, []byte("payload"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readOnly, 0o600) })

	e := NewExporter(Options{})
	if _, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dst, "read-only.deb"))
	if err != nil {
		t.Fatal(err)
	}
	// A bundle is data, not an installed tree, and FAT/exFAT have no
	// permission bits at all. Copying 0400 across would make the next export
	// to the same drive fail, so the copy must not do it.
	if fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("the copy replicated a read-only mode (%v); re-exporting to this drive would now fail", fi.Mode().Perm())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() == 0o400 {
		t.Errorf("the copy replicated the source mode exactly (%v)", fi.Mode().Perm())
	}
}

// ---------------------------------------------------------------------------
// progress across a whole export
// ---------------------------------------------------------------------------

func TestProgressIsEnoughForABarAndAnETA(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 4, 300*1024)

	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var samples []Progress
	e := NewExporter(Options{
		BufferSize:       64 * 1024,
		ProgressInterval: time.Millisecond,
		now:              func() time.Time { clk.add(time.Millisecond); return clk.now() },
		Progress:         func(p Progress) { samples = append(samples, p) },
	})

	res, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(samples) < 4 {
		t.Fatalf("only %d progress samples for a multi-file, multi-buffer copy", len(samples))
	}

	total := res.Plan.TotalBytes
	files := res.Plan.TotalFiles
	seenPhases := map[Phase]bool{}
	sawFileName := false
	perPhaseLast := map[Phase]Progress{}

	for _, s := range samples {
		seenPhases[s.Phase] = true
		if s.File != "" {
			sawFileName = true
		}
		if s.BytesTotal != total || s.FilesTotal != files {
			t.Fatalf("a sample has the wrong totals: %+v (want %d bytes / %d files)", s, total, files)
		}
		if s.BytesDone > s.BytesTotal || s.FilesDone > s.FilesTotal {
			t.Fatalf("a sample overshot its totals: %+v", s)
		}
		if s.Elapsed < 0 {
			t.Fatalf("negative elapsed time: %+v", s)
		}
		perPhaseLast[s.Phase] = s
	}

	for _, want := range []Phase{PhaseCopy, PhaseFlush, PhaseVerify} {
		if !seenPhases[want] {
			t.Errorf("no sample for phase %q; the UI could not label the work", want)
		}
	}
	if !sawFileName {
		t.Error("no sample named the file being copied")
	}
	if samples[0].BytesDone != 0 {
		t.Errorf("the first sample is not at zero: %+v", samples[0])
	}
	for _, ph := range []Phase{PhaseCopy, PhaseVerify} {
		last := perPhaseLast[ph]
		if last.BytesDone != total || last.FilesDone != files {
			t.Errorf("phase %q never reported completion: %+v", ph, last)
		}
	}
}

func TestProgressIsOptional(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 2, 4096)
	e := NewExporter(Options{}) // no Progress callback at all
	if _, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30}); err != nil {
		t.Fatalf("Export: %v", err)
	}
}

// ---------------------------------------------------------------------------
// re-export over a previous attempt
// ---------------------------------------------------------------------------

func TestExportingOverAnInterruptedAttemptSucceeds(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	want := realisticBundle(t, src)

	// Leave the debris of an interrupted export: a marker and a truncated file.
	if err := os.WriteFile(filepath.Join(dst, MarkerName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "pool", "main", "z"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "pool", "main", "z", "zzz_1.0_all.deb"), []byte("half a pack"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExporter(Options{})
	res, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if err != nil {
		t.Fatalf("Export over a previous attempt: %v", err)
	}
	if !res.Complete {
		t.Error("Result.Complete is false after a clean re-export")
	}
	assertDestMatches(t, dst, want)
}

func TestAMarkerInTheSourceIsNotCopiedButIsWarnedAbout(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.deb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, MarkerName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if p.TotalFiles != 1 {
		t.Errorf("the marker was planned for copying: %d files", p.TotalFiles)
	}
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, MarkerName) {
			found = true
			assertSentence(t, "warning", w)
		}
	}
	if !found {
		t.Errorf("no warning that the source itself looks incomplete: %v", p.Warnings)
	}

	if _, err := e.Export(context.Background(), Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if incomplete, _ := IsIncomplete(dst); incomplete {
		t.Error("the copied marker was mistaken for this export's own marker")
	}
}
