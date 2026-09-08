package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildTree creates a small representative bundle-shaped tree under dir:
// nested pool files, a top-level document, and (on a platform where it is
// meaningful) one executable file, so export/import has more than a single
// flat directory to prove itself against.
func buildTree(t *testing.T, dir string) {
	t.Helper()
	files := map[string][]byte{
		"debark.manifest.json": []byte(`{"schema_version":"debark.manifest/v1"}`),
		"lock.json":            []byte(`{"schema_version":"debark.lock/v1"}`),
		"README.txt":           []byte("hello\n"),
		"repo/Packages":        []byte("Package: jq\n"),
		"repo/Release":         []byte("Origin: debark\n"),
		"repo/pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb": []byte("fake deb bytes for jq"),
		"repo/pool/libj/libjq1/libjq1_1.6_amd64.deb":  []byte("fake deb bytes for libjq1, a bit longer than the others"),
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, files[name], 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	exe := filepath.Join(binDir, "debark-linux-amd64")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	src := t.TempDir()
	buildTree(t, src)

	tarPath := filepath.Join(t.TempDir(), "bundle.debark.tar.zst")
	if err := ExportTar(context.Background(), src, tarPath); err != nil {
		t.Fatalf("ExportTar: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "extracted")
	if err := ImportTar(context.Background(), tarPath, dst); err != nil {
		t.Fatalf("ImportTar: %v", err)
	}

	srcFiles := collectFileBytes(t, src)
	dstFiles := collectFileBytes(t, dst)
	if len(srcFiles) != len(dstFiles) {
		t.Fatalf("extracted %d files, want %d\nsrc=%v\ndst=%v", len(dstFiles), len(srcFiles), keysOf(srcFiles), keysOf(dstFiles))
	}
	for name, want := range srcFiles {
		got, ok := dstFiles[name]
		if !ok {
			t.Errorf("missing %s after round trip", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s content changed by round trip: got %q, want %q", name, got, want)
		}
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dst, "bin", "debark-linux-amd64"))
		if err != nil {
			t.Fatalf("stat extracted binary: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("extracted embedded binary lost its executable bit: mode=%v", info.Mode())
		}
		info2, err := os.Stat(filepath.Join(dst, "README.txt"))
		if err != nil {
			t.Fatalf("stat extracted README: %v", err)
		}
		if info2.Mode().Perm()&0o111 != 0 {
			t.Errorf("extracted non-executable file gained an executable bit: mode=%v", info2.Mode())
		}
	}
}

func collectFileBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestExportDeterministic(t *testing.T) {
	src := t.TempDir()
	buildTree(t, src)

	out1 := filepath.Join(t.TempDir(), "a.debark.tar.zst")
	out2 := filepath.Join(t.TempDir(), "b.debark.tar.zst")
	if err := ExportTar(context.Background(), src, out1); err != nil {
		t.Fatalf("first ExportTar: %v", err)
	}
	if err := ExportTar(context.Background(), src, out2); err != nil {
		t.Fatalf("second ExportTar: %v", err)
	}

	b1, err := os.ReadFile(out1)
	if err != nil {
		t.Fatalf("read out1: %v", err)
	}
	b2, err := os.ReadFile(out2)
	if err != nil {
		t.Fatalf("read out2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("two exports of the same tree are not byte-identical (%d vs %d bytes)", len(b1), len(b2))
	}
}

func TestExportEntriesAreSortedFixedModTimeAndNormalised(t *testing.T) {
	src := t.TempDir()
	buildTree(t, src)

	tarPath := filepath.Join(t.TempDir(), "bundle.debark.tar.zst")
	if err := ExportTar(context.Background(), src, tarPath); err != nil {
		t.Fatalf("ExportTar: %v", err)
	}

	names, headers := readTarEntries(t, tarPath)
	if len(names) == 0 {
		t.Fatalf("no entries in exported tar")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Fatalf("entries not sorted: %v", names)
		}
	}
	for _, h := range headers {
		if h.Typeflag == tar.TypeDir {
			t.Errorf("exported an explicit directory entry %q; ExportTar should write files only", h.Name)
		}
		if !h.ModTime.Equal(fixedModTime) {
			t.Errorf("entry %s ModTime = %v, want %v", h.Name, h.ModTime, fixedModTime)
		}
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("entry %s uid/gid = %d/%d, want 0/0", h.Name, h.Uid, h.Gid)
		}
		if h.Uname != "" || h.Gname != "" {
			t.Errorf("entry %s uname/gname = %q/%q, want empty", h.Name, h.Uname, h.Gname)
		}
		if filepath.Separator != '/' {
			if bytesContainsByte(h.Name, '\\') {
				t.Errorf("entry name %q contains a host path separator", h.Name)
			}
		}
		if len(h.PAXRecords) > 0 {
			for k := range h.PAXRecords {
				if k == "path" {
					continue // added automatically for long names, not an xattr
				}
				t.Errorf("entry %s carries unexpected PAX record %q", h.Name, k)
			}
		}
	}
}

func bytesContainsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func readTarEntries(t *testing.T, tarPath string) ([]string, []*tar.Header) {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open %s: %v", tarPath, err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	var names []string
	var headers []*tar.Header
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
		h := *hdr
		headers = append(headers, &h)
	}
	return names, headers
}

// --- Hostile-archive security tests -----------------------------------

type rawEntry struct {
	name     string
	typeflag byte
	linkname string
	data     []byte
}

func buildRawArchive(t *testing.T, entries []rawEntry) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hostile.tar.zst")
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
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Size:     int64(len(e.data)),
			Mode:     0o644,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header for %q: %v", e.name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("write data for %q: %v", e.name, err)
			}
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

func TestImportRefusesPathTraversal(t *testing.T) {
	cases := []string{
		"../outside.txt",
		"../../outside.txt",
		"a/../../outside.txt",
		"a/b/../../../outside.txt",
		"..",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			archive := buildRawArchive(t, []rawEntry{{name: name, typeflag: tar.TypeReg, data: []byte("evil")}})
			dst := filepath.Join(t.TempDir(), "out")
			err := ImportTar(context.Background(), archive, dst)
			if err == nil {
				t.Fatalf("ImportTar accepted a traversal entry %q", name)
			}
			assertNothingEscaped(t, dst)
		})
	}
}

func TestImportRefusesAbsolutePaths(t *testing.T) {
	cases := []string{"/etc/passwd", "/tmp/evil"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			archive := buildRawArchive(t, []rawEntry{{name: name, typeflag: tar.TypeReg, data: []byte("evil")}})
			dst := filepath.Join(t.TempDir(), "out")
			if err := ImportTar(context.Background(), archive, dst); err == nil {
				t.Fatalf("ImportTar accepted an absolute path entry %q", name)
			}
		})
	}
}

func TestImportRefusesWindowsDriveLetterPath(t *testing.T) {
	archive := buildRawArchive(t, []rawEntry{{name: `C:\Windows\System32\evil.exe`, typeflag: tar.TypeReg, data: []byte("evil")}})
	dst := filepath.Join(t.TempDir(), "out")
	if err := ImportTar(context.Background(), archive, dst); err == nil {
		t.Fatalf("ImportTar accepted a Windows drive-letter path entry")
	}
}

func TestImportRefusesBackslashPaths(t *testing.T) {
	archive := buildRawArchive(t, []rawEntry{{name: `pool\evil.deb`, typeflag: tar.TypeReg, data: []byte("evil")}})
	dst := filepath.Join(t.TempDir(), "out")
	if err := ImportTar(context.Background(), archive, dst); err == nil {
		t.Fatalf("ImportTar accepted a backslash-containing entry name")
	}
}

func TestImportRefusesSymlinkEntries(t *testing.T) {
	cases := []struct {
		name     string
		typeflag byte
		link     string
	}{
		{"innocuous.deb", tar.TypeSymlink, "../../../etc/passwd"},
		{"innocuous2.deb", tar.TypeSymlink, "/etc/passwd"},
		{"innocuous3.deb", tar.TypeSymlink, "harmless-looking-target"},
		{"hardlink.deb", tar.TypeLink, "some/other/file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			archive := buildRawArchive(t, []rawEntry{{name: c.name, typeflag: c.typeflag, linkname: c.link}})
			dst := filepath.Join(t.TempDir(), "out")
			if err := ImportTar(context.Background(), archive, dst); err == nil {
				t.Fatalf("ImportTar accepted a link entry (type %v -> %s)", c.typeflag, c.link)
			}
			assertNothingEscaped(t, dst)
		})
	}
}

// TestSafeRelPath exercises the entry-name validator directly, including
// cases like an embedded NUL byte that archive/tar's own Writer refuses to
// encode at all (WriteHeader rejects it as an invalid PAX record before a
// hostile archive using it could ever be constructed through the standard
// library), so ImportTar's own read path can never observe one from a
// legitimately-encoded stream. Testing the validator directly instead of
// only through a round-tripped archive keeps this defence covered anyway.
func TestSafeRelPath(t *testing.T) {
	valid := []string{
		"README.txt",
		"repo/pool/j/jq/jq_1.0_amd64.deb",
		"a/b/c",
	}
	for _, name := range valid {
		if _, err := safeRelPath(name); err != nil {
			t.Errorf("safeRelPath(%q): unexpected error: %v", name, err)
		}
	}

	invalid := []string{
		"",
		"..",
		"../outside",
		"../../outside",
		"a/../../outside",
		"/etc/passwd",
		"/tmp/evil",
		`C:\Windows\System32\evil.exe`,
		`pool\evil.deb`,
		"evil\x00.deb",
		".",
		// Length: an attacker-chosen name must be refused here, as hostile
		// input, rather than travelling down to the filesystem and coming
		// back as an environment failure.
		strings.Repeat("d/", maxImportEntryNameLen/2+1) + "f.deb",
		"repo/" + strings.Repeat("a", maxImportNameComponentLen+1) + ".deb",
	}
	for _, name := range invalid {
		if _, err := safeRelPath(name); err == nil {
			t.Errorf("safeRelPath(%q): want error, got nil", name)
		}
	}
}

func TestImportAcceptsBenignArchiveMixedWithNothingHostile(t *testing.T) {
	archive := buildRawArchive(t, []rawEntry{
		{name: "repo/pool/j/jq/jq_1.0_amd64.deb", typeflag: tar.TypeReg, data: []byte("fake")},
		{name: "lock.json", typeflag: tar.TypeReg, data: []byte(`{}`)},
	})
	dst := filepath.Join(t.TempDir(), "out")
	if err := ImportTar(context.Background(), archive, dst); err != nil {
		t.Fatalf("ImportTar rejected a well-formed archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "lock.json")); err != nil {
		t.Errorf("lock.json missing after import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "repo", "pool", "j", "jq", "jq_1.0_amd64.deb")); err != nil {
		t.Errorf("pool file missing after import: %v", err)
	}
}

// TestImportOneHostileEntryAmongManyBenignRefusesTheWholeArchive proves a
// single bad entry cannot be smuggled in among otherwise-normal ones.
func TestImportOneHostileEntryAmongManyBenignRefusesTheWholeArchive(t *testing.T) {
	archive := buildRawArchive(t, []rawEntry{
		{name: "lock.json", typeflag: tar.TypeReg, data: []byte(`{}`)},
		{name: "repo/Packages", typeflag: tar.TypeReg, data: []byte("Package: jq\n")},
		{name: "../../outside.txt", typeflag: tar.TypeReg, data: []byte("evil")},
		{name: "README.txt", typeflag: tar.TypeReg, data: []byte("hi")},
	})
	dst := filepath.Join(t.TempDir(), "out")
	if err := ImportTar(context.Background(), archive, dst); err == nil {
		t.Fatalf("ImportTar accepted an archive with one hostile entry among benign ones")
	}
}

// assertNothingEscaped checks that no file was written anywhere outside dst
// (specifically, not in dst's parent), guarding against a traversal entry
// that ImportTar rejected in name only but still wrote somewhere.
func assertNothingEscaped(t *testing.T, dst string) {
	t.Helper()
	parent := filepath.Dir(dst)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(dst) {
			continue
		}
		if e.Name() == "outside.txt" {
			t.Errorf("a traversal entry escaped to %s", filepath.Join(parent, e.Name()))
		}
	}
}
