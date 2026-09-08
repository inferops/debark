package doctor

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestParseDebReader_ControlAndScripts(t *testing.T) {
	data := buildFixtureDeb(t, fixtureDeb{
		Scripts: map[string]string{
			"postinst": "#!/bin/sh\necho hello\n",
			"prerm":    "#!/bin/sh\ntrue\n",
		},
	})
	df, err := parseDebReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parseDebReader: %v", err)
	}
	if df.Unscannable != "" {
		t.Fatalf("unexpected Unscannable: %q", df.Unscannable)
	}
	if got := df.Control["Package"]; got != "fixture-pkg" {
		t.Errorf("Control[Package] = %q, want fixture-pkg", got)
	}
	if got := df.Scripts["postinst"]; got != "#!/bin/sh\necho hello\n" {
		t.Errorf("Scripts[postinst] = %q", got)
	}
	if _, ok := df.Scripts["preinst"]; ok {
		t.Error("Scripts[preinst] should be absent (not provided)")
	}
}

func TestParseDebReader_NoControlMember(t *testing.T) {
	raw := buildAr([]arMember{{Name: "debian-binary", Data: []byte("2.0\n")}})
	if _, err := parseDebReader(bytes.NewReader(raw)); err == nil {
		t.Fatal("expected an error for a .deb with no control.tar member")
	}
}

// TestParseDebReader_RealXZFixture opens a real .deb pulled from a live
// Debian 12 archive during experiment E7 (docs/experiments/
// E7-network-postinst.md) — task-norwegian, chosen only for being the
// smallest package the E7 sample happened to contain. It has an
// xz-compressed control.tar, like 917 of the 1,749 real packages (52%; a
// further 46% were zstd) in the E7 sample, so this is the fixture that
// actually proves xz support works end to end rather than just against
// synthetic gzip data — zstd support has its own round-trip test right below
// (TestDecompressMember_Zstd), since klauspost/compress can encode zstd too
// and no equivalent xz encoder was available to build a synthetic fixture.
func TestParseDebReader_RealXZFixture(t *testing.T) {
	df, err := openDebFile(filepath.Join("testdata", "fixture-real-xz.deb"))
	if err != nil {
		t.Fatalf("openDebFile: %v", err)
	}
	if df.Unscannable != "" {
		t.Fatalf("real xz fixture reported Unscannable = %q, want scanned", df.Unscannable)
	}
	if got := df.Control["Package"]; got != "task-norwegian" {
		t.Errorf("Control[Package] = %q, want task-norwegian", got)
	}
}

func TestDecompressMember_Zstd(t *testing.T) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	payload := buildTarGzRaw(t, map[string]string{"control": defaultControl})
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	rc, unsupported, err := decompressMember(arMember{Name: "control.tar.zst", Data: buf.Bytes()})
	if err != nil {
		t.Fatalf("decompressMember: %v", err)
	}
	if unsupported != "" {
		t.Fatalf("zstd reported unsupported = %q", unsupported)
	}
	defer rc.Close()
	got := readAllString(t, rc)
	want := string(payload)
	if got != want {
		t.Errorf("round-tripped zstd content does not match (got %d bytes, want %d)", len(got), len(want))
	}
}

func TestDecompressMember_Lzma_Unsupported(t *testing.T) {
	_, unsupported, err := decompressMember(arMember{Name: "control.tar.lzma", Data: []byte("whatever")})
	if err != nil {
		t.Fatalf("decompressMember: %v", err)
	}
	if unsupported != "lzma" {
		t.Errorf("unsupported = %q, want lzma", unsupported)
	}
}

func TestOpenDebFile_UnsupportedCompressionDegradesGracefully(t *testing.T) {
	raw := buildAr([]arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.lzma", Data: []byte("not real lzma data")},
	})
	df, err := parseDebReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parseDebReader should degrade gracefully, not error: %v", err)
	}
	if df.Unscannable != "lzma" {
		t.Errorf("Unscannable = %q, want lzma", df.Unscannable)
	}
}
