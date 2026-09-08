package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// This file builds minimal, valid .deb (ar archive) fixtures entirely in Go
// for the same reason core/repository/testdeb_test.go does: Assemble's own
// pipeline calls into the repository writer (and, when pruning, re-reads
// leftover pool files), both of which parse real control data via
// pault.ag/go/debian/deb, and there is no dpkg-deb on the Windows machines
// these tests must also pass on.

func arMember(buf *bytes.Buffer, name string, data []byte) {
	h := make([]byte, 60)
	for i := range h {
		h[i] = ' '
	}
	copy(h[0:16], name)
	copy(h[16:28], "0")
	copy(h[28:34], "0")
	copy(h[34:40], "0")
	copy(h[40:48], "100644")
	copy(h[48:58], strconv.Itoa(len(data)))
	h[58] = 0x60
	h[59] = 0x0A
	buf.Write(h)
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func gzipTarOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gzBuf.Bytes()
}

func buildDebBytes(t *testing.T, controlText string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar.gz", gzipTarOf(t, map[string][]byte{"./control": []byte(controlText)}))
	arMember(&buf, "data.tar.gz", gzipTarOf(t, map[string][]byte{}))
	return buf.Bytes()
}

func simpleControl(pkg, version, arch string) string {
	return fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.com>\nDescription: a test package\n",
		pkg, version, arch,
	)
}

// writeStagedDeb writes a synthetic .deb to a fresh staging file (not under
// the bundle dir) and returns its path, ready to use as a
// resolve.Selection.StagedPath.
func writeStagedDeb(t *testing.T, dir, filename, pkg, version, arch string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, buildDebBytes(t, simpleControl(pkg, version, arch)), 0o644); err != nil {
		t.Fatalf("write staged %s: %v", filename, err)
	}
	return path
}
