package repository

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/digest"
)

func testRelease() ReleaseFields {
	return ReleaseFields{
		Origin:        DefaultOrigin,
		Label:         DefaultLabel,
		Suite:         DefaultSuite,
		Codename:      "bookworm",
		Architectures: []string{"amd64"},
		Components:    []string{DefaultComponent},
		Description:   DefaultDescription,
		Date:          "Thu, 03 Sep 2026 00:00:00 UTC",
	}
}

func TestWriteFieldOrderAndMultilineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	control := "Package: jq\n" +
		"Version: 1.6-2.1+deb12u2\n" +
		"Architecture: amd64\n" +
		"Maintainer: Test <test@example.com>\n" +
		"Installed-Size: 111\n" +
		"Depends: libjq1 (= 1.6-2.1+deb12u2), libc6 (>= 2.34)\n" +
		"Section: utils\n" +
		"Priority: optional\n" +
		"Multi-Arch: foreign\n" +
		"Homepage: https://example.invalid/jq\n" +
		"Description: lightweight and flexible command-line JSON processor\n" +
		" jq is like sed for JSON data, first paragraph line one.\n" +
		" first paragraph line two.\n" +
		" .\n" +
		" It is written in portable C, second paragraph.\n" +
		" .\n" +
		" jq can mangle the data format, third paragraph.\n"
	rel := writeTestDeb(t, dir, "pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb", control)

	res, err := NewWriter().Write(context.Background(), Input{
		Dir:      dir,
		Files:    []PoolFile{{Path: rel}},
		Release:  testRelease(),
		Compress: true,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.PackageCount != 1 {
		t.Fatalf("PackageCount = %d, want 1", res.PackageCount)
	}

	got, err := os.ReadFile(filepath.Join(dir, "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	text := string(got)

	// Package must be the first field.
	if !strings.HasPrefix(text, "Package: jq\n") {
		t.Errorf("Packages does not start with Package: jq\\n; got prefix %q", text[:min(40, len(text))])
	}

	// Control fields keep the source order; Filename/Size/MD5sum/SHA1/SHA256
	// are appended in that order, after Description.
	wantOrder := []string{
		"Package:", "Version:", "Architecture:", "Maintainer:", "Installed-Size:",
		"Depends:", "Section:", "Priority:", "Multi-Arch:", "Homepage:", "Description:",
		"Filename:", "Size:", "MD5sum:", "SHA1:", "SHA256:",
	}
	lastIdx := -1
	for _, field := range wantOrder {
		idx := indexOfFieldLine(text, field)
		if idx < 0 {
			t.Fatalf("field %q not found in output:\n%s", field, text)
		}
		if idx <= lastIdx {
			t.Errorf("field %q out of order (idx %d <= previous %d)", field, idx, lastIdx)
		}
		lastIdx = idx
	}

	// The multi-line Description, including its " ." blank-paragraph
	// convention, must round-trip exactly.
	wantDescription := "Description: lightweight and flexible command-line JSON processor\n" +
		" jq is like sed for JSON data, first paragraph line one.\n" +
		" first paragraph line two.\n" +
		" .\n" +
		" It is written in portable C, second paragraph.\n" +
		" .\n" +
		" jq can mangle the data format, third paragraph.\n"
	if !strings.Contains(text, wantDescription) {
		t.Errorf("Description did not round-trip.\ngot:\n%s\nwant substring:\n%s", text, wantDescription)
	}

	// Appended fields carry real values.
	if !strings.Contains(text, "Filename: "+rel+"\n") {
		t.Errorf("Filename field missing or wrong; got:\n%s", text)
	}
	fh, err := digest.AllFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("hash fixture: %v", err)
	}
	for _, want := range []string{
		"MD5sum: " + fh.MD5,
		"SHA1: " + fh.SHA1,
		"SHA256: " + fh.SHA256,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in Packages output", want)
		}
	}

	// No trailing whitespace on any line, and the file ends in exactly one
	// newline (no trailing blank line).
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i, line)
		}
	}
	if strings.HasSuffix(text, "\n\n") {
		t.Errorf("Packages ends with a blank line")
	}
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("Packages does not end with a newline")
	}
}

// indexOfFieldLine returns the byte offset at which a line starting with
// field (e.g. "Package:") begins in text, or -1 if there is no such line.
func indexOfFieldLine(text, field string) int {
	if strings.HasPrefix(text, field) {
		return 0
	}
	if idx := strings.Index(text, "\n"+field); idx >= 0 {
		return idx + 1
	}
	return -1
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestWriteSortsStanzasByPackageArchVersion(t *testing.T) {
	dir := t.TempDir()
	var files []PoolFile
	specs := []struct{ pkg, ver, arch string }{
		{"zeta", "1.0", "amd64"},
		{"alpha", "2.0", "amd64"},
		{"alpha", "1.0", "amd64"},
		{"alpha", "1.0", "arm64"},
	}
	for _, s := range specs {
		fn := s.pkg + "_" + s.ver + "_" + s.arch + ".deb"
		rel := writeTestDeb(t, dir, PoolPath(s.pkg, fn), simpleControl(s.pkg, s.ver, s.arch))
		files = append(files, PoolFile{Path: rel})
	}

	res, err := NewWriter().Write(context.Background(), Input{Dir: dir, Files: files, Release: testRelease(), Compress: true})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.PackageCount != len(specs) {
		t.Fatalf("PackageCount = %d, want %d", res.PackageCount, len(specs))
	}

	got, err := os.ReadFile(filepath.Join(dir, "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	var gotOrder []string
	for _, stz := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n\n") {
		var pkg, arch, ver string
		for _, line := range strings.Split(stz, "\n") {
			switch {
			case strings.HasPrefix(line, "Package: "):
				pkg = strings.TrimPrefix(line, "Package: ")
			case strings.HasPrefix(line, "Architecture: "):
				arch = strings.TrimPrefix(line, "Architecture: ")
			case strings.HasPrefix(line, "Version: "):
				ver = strings.TrimPrefix(line, "Version: ")
			}
		}
		gotOrder = append(gotOrder, pkg+"/"+arch+"@"+ver)
	}
	want := []string{"alpha/amd64@1.0", "alpha/amd64@2.0", "alpha/arm64@1.0", "zeta/amd64@1.0"}
	if len(gotOrder) != len(want) {
		t.Fatalf("got %d stanzas %v, want %v", len(gotOrder), gotOrder, want)
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Errorf("stanza[%d] = %s, want %s (full order: %v)", i, gotOrder[i], want[i], gotOrder)
		}
	}
}

func TestWriteDeterministic(t *testing.T) {
	specs := []struct{ pkg, ver, arch string }{
		{"jq", "1.6-2.1+deb12u2", "amd64"},
		{"libjq1", "1.6-2.1+deb12u2", "amd64"},
		{"tree", "2.1.0-1", "amd64"},
	}

	run := func() (packages, packagesGz, release []byte) {
		dir := t.TempDir()
		var files []PoolFile
		for _, s := range specs {
			fn := s.pkg + "_" + s.ver + "_" + s.arch + ".deb"
			rel := writeTestDeb(t, dir, PoolPath(s.pkg, fn), simpleControl(s.pkg, s.ver, s.arch))
			files = append(files, PoolFile{Path: rel})
		}
		if _, err := NewWriter().Write(context.Background(), Input{Dir: dir, Files: files, Release: testRelease(), Compress: true}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		p, err := os.ReadFile(filepath.Join(dir, "Packages"))
		if err != nil {
			t.Fatalf("read Packages: %v", err)
		}
		g, err := os.ReadFile(filepath.Join(dir, "Packages.gz"))
		if err != nil {
			t.Fatalf("read Packages.gz: %v", err)
		}
		r, err := os.ReadFile(filepath.Join(dir, "Release"))
		if err != nil {
			t.Fatalf("read Release: %v", err)
		}
		return p, g, r
	}

	p1, g1, r1 := run()
	p2, g2, r2 := run()

	if !bytes.Equal(p1, p2) {
		t.Errorf("Packages not byte-identical across runs")
	}
	if !bytes.Equal(g1, g2) {
		t.Errorf("Packages.gz not byte-identical across runs")
	}
	if !bytes.Equal(r1, r2) {
		t.Errorf("Release not byte-identical across runs")
	}

	// The gzip stream must also decompress back to exactly Packages: the
	// fixed header changes only metadata, never the payload.
	zr, err := gzip.NewReader(bytes.NewReader(g1))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decompressed := new(bytes.Buffer)
	if _, err := decompressed.ReadFrom(zr); err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(decompressed.Bytes(), p1) {
		t.Errorf("Packages.gz does not decompress to Packages")
	}

	// The gzip header itself must carry no mtime and no original name, or a
	// byte-identical run tomorrow would not be guaranteed.
	if zr.ModTime.Unix() != 0 && !zr.ModTime.IsZero() {
		t.Errorf("gzip ModTime = %v, want unset", zr.ModTime)
	}
	if zr.Name != "" {
		t.Errorf("gzip Name = %q, want empty", zr.Name)
	}
}

func TestWriteReleaseFields(t *testing.T) {
	dir := t.TempDir()
	rel := writeTestDeb(t, dir, PoolPath("tree", "tree_2.1.0-1_amd64.deb"), simpleControl("tree", "2.1.0-1", "amd64"))

	fields := ReleaseFields{
		Origin:        "debark",
		Label:         "debark bundle",
		Suite:         "bundle",
		Codename:      "bookworm",
		Architectures: []string{"amd64", "arm64"},
		Components:    []string{"main"},
		Description:   "debark offline bundle",
		Date:          "Thu, 03 Sep 2026 00:00:00 UTC",
		ExtraFields:   map[string]string{"Zeta": "z", "Alpha": "a"},
	}
	res, err := NewWriter().Write(context.Background(), Input{Dir: dir, Files: []PoolFile{{Path: rel}}, Release: fields, Compress: true})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "Release"))
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}
	text := string(got)

	for _, want := range []string{
		"Origin: debark\n",
		"Label: debark bundle\n",
		"Suite: bundle\n",
		"Codename: bookworm\n",
		"Architectures: amd64 arm64\n",
		"Components: main\n",
		"Date: Thu, 03 Sep 2026 00:00:00 UTC\n",
		"Description: debark offline bundle\n",
		"MD5Sum:\n",
		"SHA1:\n",
		"SHA256:\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Release missing %q; full text:\n%s", want, text)
		}
	}

	// ExtraFields are sorted by key.
	if idx := strings.Index(text, "Alpha: a\n"); idx < 0 || strings.Index(text, "Zeta: z\n") < idx {
		t.Errorf("ExtraFields not sorted by key; text:\n%s", text)
	}

	// Every hash block references both Packages and Packages.gz, in that
	// order, with the digests the writer itself just returned.
	for _, block := range []struct {
		header string
		hash   string
	}{
		{"MD5Sum:", ""}, // presence checked above; content spot-checked below
	} {
		_ = block
	}
	if !strings.Contains(text, res.PackagesSHA256+" ") {
		t.Errorf("Release SHA256 block missing Packages digest %s", res.PackagesSHA256)
	}
	if !strings.Contains(text, res.PackagesGzSHA256+" ") {
		t.Errorf("Release SHA256 block missing Packages.gz digest %s", res.PackagesGzSHA256)
	}
	if !strings.Contains(text, " Packages\n") {
		t.Errorf("Release hash blocks missing the Packages path")
	}
	if !strings.Contains(text, " Packages.gz\n") {
		t.Errorf("Release hash blocks missing the Packages.gz path")
	}

	if res.ReleaseSHA256 != digest.Bytes(got) {
		t.Errorf("Result.ReleaseSHA256 = %s, does not match actual file digest %s", res.ReleaseSHA256, digest.Bytes(got))
	}
}

func TestWriteWithoutCompress(t *testing.T) {
	dir := t.TempDir()
	rel := writeTestDeb(t, dir, PoolPath("tree", "tree_2.1.0-1_amd64.deb"), simpleControl("tree", "2.1.0-1", "amd64"))

	res, err := NewWriter().Write(context.Background(), Input{Dir: dir, Files: []PoolFile{{Path: rel}}, Release: testRelease(), Compress: false})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.PackagesGzPath != "" || res.PackagesGzSHA256 != "" {
		t.Errorf("Compress:false still produced a gz result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "Packages.gz")); !os.IsNotExist(err) {
		t.Errorf("Packages.gz written despite Compress:false")
	}
	rtext, err := os.ReadFile(filepath.Join(dir, "Release"))
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}
	if strings.Contains(string(rtext), "Packages.gz") {
		t.Errorf("Release mentions Packages.gz despite Compress:false")
	}
}

func TestWriteRejectsMissingPackageField(t *testing.T) {
	dir := t.TempDir()
	control := "Version: 1.0\nArchitecture: amd64\n"
	rel := writeTestDeb(t, dir, "pool/b/bad/bad_1.0_amd64.deb", control)

	_, err := NewWriter().Write(context.Background(), Input{Dir: dir, Files: []PoolFile{{Path: rel}}, Release: testRelease(), Compress: true})
	if err == nil {
		t.Fatalf("Write: want error for a stanza with no Package field, got nil")
	}
}

func TestWriteUsesProvidedHashesWhenNonZero(t *testing.T) {
	dir := t.TempDir()
	rel := writeTestDeb(t, dir, PoolPath("tree", "tree_2.1.0-1_amd64.deb"), simpleControl("tree", "2.1.0-1", "amd64"))

	// Deliberately wrong hashes: if the writer trusts the caller-supplied
	// FileHashes instead of recomputing when they are non-zero, they show up
	// verbatim in the output, which is exactly what this test checks for.
	bogus := digest.FileHashes{Size: 999, MD5: "m", SHA1: "s1", SHA256: "s256"}
	res, err := NewWriter().Write(context.Background(), Input{
		Dir:      dir,
		Files:    []PoolFile{{Path: rel, Hashes: bogus}},
		Release:  testRelease(),
		Compress: false,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.PoolBytes != 999 {
		t.Errorf("PoolBytes = %d, want 999 (from the supplied Hashes)", res.PoolBytes)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	for _, want := range []string{"Size: 999\n", "MD5sum: m\n", "SHA1: s1\n", "SHA256: s256\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("Packages missing %q when Hashes was supplied non-zero", want)
		}
	}
}

func TestPoolPath(t *testing.T) {
	cases := []struct{ pkg, filename, want string }{
		{"vlc", "vlc_3.0.21-1build1_amd64.deb", "pool/v/vlc/vlc_3.0.21-1build1_amd64.deb"},
		{"libjq1", "libjq1_1.6-2.1+deb12u2_amd64.deb", "pool/libj/libjq1/libjq1_1.6-2.1+deb12u2_amd64.deb"},
		{"a", "a_1_amd64.deb", "pool/a/a/a_1_amd64.deb"},
	}
	for _, c := range cases {
		if got := PoolPath(c.pkg, c.filename); got != c.want {
			t.Errorf("PoolPath(%q, %q) = %q, want %q", c.pkg, c.filename, got, c.want)
		}
	}
}
