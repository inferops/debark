package fetch

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inferops/debark/core/dferr"
)

// rawDeb assembles an ar archive from exactly the members given, in order,
// bypassing BuildFixtureDeb's own control-file construction so tests can
// build deliberately broken inputs.
func rawDeb(members map[string][]byte, order []string) []byte {
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	for _, name := range order {
		writeArMember(&buf, name, members[name])
	}
	return buf.Bytes()
}

func TestReadControlInfo_MissingRequiredField(t *testing.T) {
	// A control file with no Version field at all.
	controlTarGz, err := buildTarGz(map[string][]byte{
		"./control": []byte("Package: broken\nArchitecture: amd64\nMaintainer: x <x@example.com>\nDescription: no version\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	dataTarGz, err := buildTarGz(map[string][]byte{"./usr/share/doc/broken/x": []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	b := rawDeb(map[string][]byte{
		"debian-binary":  []byte("2.0\n"),
		"control.tar.gz": controlTarGz,
		"data.tar.gz":    dataTarGz,
	}, []string{"debian-binary", "control.tar.gz", "data.tar.gz"})

	_, err = readControlInfo(bytes.NewReader(b))
	if err == nil {
		t.Fatal("want an error for a control file missing Version")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("Version")) {
		t.Errorf("error %q does not name the missing field", err.Error())
	}
}

// realXZControlTarBase64 is a genuine, valid xz-compressed tar archive
// containing one file, "control", with the text:
//
//	Package: xztest
//	Version: 1.0
//	Architecture: amd64
//
// built with `tar -cf control.tar control && xz -9 -e control.tar` (real
// dpkg-deb output for a modern control.tar.xz uses the same xz container
// format). Generated once and embedded here — 216 bytes — rather than
// checked in as a binary file, specifically to prove real xz decompression
// works end to end through this package's pault.ag/go/debian/deb-backed
// reader, which a synthetic corrupt-magic-bytes test cannot prove.
const realXZControlTarBase64 = `` +
	`/Td6WFoAAATm1rRGAgAhARwAAAAQz1jM4Cf/AJddADGbyhnbre+8GtxPrcaFT+6Fq/wxSX43Dqa1bF0E` +
	`nDM10gJH98Fv5MlTyAycYPseeN6Q3ytEYOW9eWE7BH/kVZvaF3Ywlv1KSS6gRPghwkXo5RHVuwNCO0HQ` +
	`sXWlYW9IG79IcqU5e1o9GG14PSrJv1dYraPf0gD6XeqpvyCNY2o/EwezBPMevbLbBSVK14tctPJcZQSU` +
	`HwAAAAKFFP/SD080AAGzAYBQAAA9BI7jscRn+wIAAAAABFla`

func TestReadControlInfo_RealXZDecompresses(t *testing.T) {
	controlTarXz, err := base64.StdEncoding.DecodeString(realXZControlTarBase64)
	if err != nil {
		t.Fatal(err)
	}
	b := rawDeb(map[string][]byte{
		"debian-binary":  []byte("2.0\n"),
		"control.tar.xz": controlTarXz,
	}, []string{"debian-binary", "control.tar.xz"})

	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	if info.Package != "xztest" || info.Version != "1.0" || info.Architecture != "amd64" {
		t.Errorf("ControlInfo = %+v, want Package=xztest Version=1.0 Architecture=amd64", info)
	}
}

// TestReadControlInfo_XZExtensionIsAttemptedAndCorruptionReported proves
// control.tar.xz is genuinely attempted (via pault.ag/go/debian/deb, which
// this package delegates decompression to — see the long comment on
// ReadControlInfo for why control.tar.gz turned out to be the wrong
// assumption) rather than rejected outright the way an earlier version of
// this package did. The xz magic bytes here are not followed by a real xz
// stream, so the read still fails, but for the honest reason — corrupt
// compressed data — not a made-up "unsupported format".
func TestReadControlInfo_XZExtensionIsAttemptedAndCorruptionReported(t *testing.T) {
	b := rawDeb(map[string][]byte{
		"debian-binary":  []byte("2.0\n"),
		"control.tar.xz": {0xFD, '7', 'z', 'X', 'Z', 0x00}, // real xz magic, but no valid stream follows
		"data.tar.gz":    {},
	}, []string{"debian-binary", "control.tar.xz", "data.tar.gz"})

	_, err := readControlInfo(bytes.NewReader(b))
	if err == nil {
		t.Fatal("want an error: the bytes after the xz magic are not a valid xz stream")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("unsupported")) {
		t.Errorf("error %q wrongly claims xz is unsupported; it is, this stream is just corrupt", err.Error())
	}
}

// TestReadControlInfo_UnknownExtensionIsTreatedAsUncompressed documents
// pault.ag/go/debian/deb's actual, permissive behaviour for a control
// member extension it does not specifically recognise: it is read as
// though uncompressed, not rejected. A member actually compressed with
// something exotic would then fail at the tar-parsing step instead
// (garbage is not a valid tar header either), which is still a clear
// error, just from a different layer — this test exists so that behaviour
// is asserted, not assumed.
func TestReadControlInfo_UnknownExtensionIsTreatedAsUncompressed(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	content := []byte("Package: oddext\nVersion: 1\nArchitecture: all\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: int64(len(content)),
		ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	b := rawDeb(map[string][]byte{
		"debian-binary": []byte("2.0\n"),
		"control.tar.Z": raw.Bytes(), // old-style "compress" extension: not in pault.ag's known list
	}, []string{"debian-binary", "control.tar.Z"})

	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	if info.Package != "oddext" {
		t.Errorf("ControlInfo = %+v", info)
	}
}

func TestReadControlInfo_UncompressedControlTar(t *testing.T) {
	// buildTarGz always gzips; build a plain tar by hand for this one case.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	content := []byte("Package: plain\nVersion: 1\nArchitecture: all\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: int64(len(content)),
		ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	b := rawDeb(map[string][]byte{
		"debian-binary": []byte("2.0\n"),
		"control.tar":   raw.Bytes(),
		"data.tar":      {},
	}, []string{"debian-binary", "control.tar", "data.tar"})

	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	if info.Package != "plain" || info.Version != "1" || info.Architecture != "all" {
		t.Errorf("ControlInfo = %+v", info)
	}
}

func TestReadControlInfo_FoldedContinuationLine(t *testing.T) {
	// deb822 folds a long Depends across continuation lines; each
	// continuation is joined with a single space.
	control := "Package: folded\nVersion: 1\nArchitecture: amd64\n" +
		"Depends: libc6 (>= 2.34),\n libjpeg62-turbo,\n zlib1g\n"
	controlTarGz, err := buildTarGz(map[string][]byte{"./control": []byte(control)})
	if err != nil {
		t.Fatal(err)
	}
	b := rawDeb(map[string][]byte{
		"debian-binary":  []byte("2.0\n"),
		"control.tar.gz": controlTarGz,
	}, []string{"debian-binary", "control.tar.gz"})

	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	want := "libc6 (>= 2.34), libjpeg62-turbo, zlib1g"
	if info.Depends != want {
		t.Errorf("Depends = %q, want %q", info.Depends, want)
	}
}

func TestReadControlInfo_NotADeb(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notadeb.deb")
	if err := os.WriteFile(p, []byte("<html><body>404 not found</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadControlInfo(p)
	if err == nil {
		t.Fatal("want an error")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
}

func TestReadControlInfo_MissingFile(t *testing.T) {
	_, err := ReadControlInfo(filepath.Join(t.TempDir(), "nope.deb"))
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestReadControlInfoBatch(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.deb")
	if err := WriteFixtureDeb(good, FixtureDeb{Package: "good", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.deb")
	if err := os.WriteFile(bad, []byte("not a deb"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.deb")

	results := ReadControlInfoBatch([]string{good, bad, missing})
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Err != nil || results[0].Info.Package != "good" {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Err == nil {
		t.Errorf("results[1] should have an error (not a .deb)")
	}
	if results[2].Err == nil || dferr.ClassOf(results[2].Err) != dferr.Usage {
		t.Errorf("results[2] should have a Usage error (missing file); got %+v", results[2])
	}
	for i, r := range results {
		if r.Path != []string{good, bad, missing}[i] {
			t.Errorf("results[%d].Path = %q", i, r.Path)
		}
	}
}

func TestControlInfo_SourceName(t *testing.T) {
	cases := []struct {
		source, pkg, want string
	}{
		{"", "foo", "foo"},
		{"foo-src", "foo", "foo-src"},
		{"foo-src (1.2-3)", "foo", "foo-src"},
	}
	for _, c := range cases {
		info := ControlInfo{Package: c.pkg, Source: c.source}
		if got := info.SourceName(); got != c.want {
			t.Errorf("SourceName() with Source=%q Package=%q = %q, want %q", c.source, c.pkg, got, c.want)
		}
	}
}
