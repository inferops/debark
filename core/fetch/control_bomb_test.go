package fetch

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// buildBombDeb builds a structurally real .deb (same ar/tar/gzip layers
// BuildFixtureDeb produces) whose control.tar.gz's "control" member
// decompresses to controlSize bytes of a single repeated byte — small on
// the wire (gzip collapses a run of one repeated byte to almost nothing),
// enormous once decompressed. That gap is exactly what a decompression bomb
// is; a legitimate control file has no reason to look like this.
func buildBombDeb(t *testing.T, controlSize int) []byte {
	t.Helper()
	bomb := bytes.Repeat([]byte{'A'}, controlSize)
	controlTarGz, err := buildTarGz(map[string][]byte{"./control": bomb})
	if err != nil {
		t.Fatalf("buildTarGz: %v", err)
	}

	var buf bytes.Buffer
	buf.WriteString(arMagic)
	writeArMember(&buf, "debian-binary", []byte("2.0\n"))
	writeArMember(&buf, "control.tar.gz", controlTarGz)
	return buf.Bytes()
}

// TestReadControlInfo_RejectsDecompressionBomb is the regression test for
// F6 (docs/security/review-findings.md): a control member that decompresses
// to far more than a real control file ever legitimately does must be
// refused with a clear, classified error, not read fully into memory.
func TestReadControlInfo_RejectsDecompressionBomb(t *testing.T) {
	// One byte over the limit: the narrowest case that must still trip it.
	deb := buildBombDeb(t, maxControlSize+1)

	info, err := readControlInfo(bytes.NewReader(deb))
	if err == nil {
		t.Fatalf("readControlInfo accepted a %d-byte control file (limit is %d): %+v", maxControlSize+1, maxControlSize, info)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not clearly say the size limit was exceeded: %v", err)
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("dferr.ClassOf(err) = %v, want %v", dferr.ClassOf(err), dferr.Incomplete)
	}

	// The error must name the offending member, so an operator can tell
	// which .deb (once ReadControlInfo's own path-naming wrap is applied at
	// the top level) and which member inside it misbehaved.
	if !strings.Contains(err.Error(), "control.tar.gz") {
		t.Errorf("error does not name the offending ar member: %v", err)
	}
}

// TestReadControlInfo_AcceptsLargeButUnderLimitControl proves the limit is
// on the right side of real usage: a control file comfortably larger than
// any real one, but still under maxControlSize, must parse normally rather
// than being caught by an over-eager check.
func TestReadControlInfo_AcceptsLargeButUnderLimitControl(t *testing.T) {
	// A real control file under the limit, padded with a long but legitimate
	// multi-line Description (deb822 continuation lines), well short of
	// maxControlSize.
	pad := strings.Repeat("this line is part of a long description.\n ", 2000)
	control := "Package: big-description\nVersion: 1.0\nArchitecture: amd64\nDescription: padded\n " + pad + "\n"
	if len(control) >= maxControlSize {
		t.Fatalf("test setup bug: control (%d bytes) is not under maxControlSize (%d)", len(control), maxControlSize)
	}
	controlTarGz, err := buildTarGz(map[string][]byte{"./control": []byte(control)})
	if err != nil {
		t.Fatalf("buildTarGz: %v", err)
	}
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	writeArMember(&buf, "debian-binary", []byte("2.0\n"))
	writeArMember(&buf, "control.tar.gz", controlTarGz)

	info, err := readControlInfo(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	if info.Package != "big-description" {
		t.Errorf("Package = %q, want %q", info.Package, "big-description")
	}
}
