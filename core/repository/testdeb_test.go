package repository

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

// This file builds minimal, valid .deb (ar archive) fixtures entirely in Go,
// so the writer's control-stanza extraction can be tested on Windows without
// dpkg-deb, apt-ftparchive or any other Debian tooling. The real-apt E2E test
// in e2e_linux_test.go covers the case a hand-built fixture cannot: whether a
// real apt accepts the generated repository.

// arMember writes one ar(1) member: a 60-byte header (name, mtime, uid, gid,
// mode, size, then the 0x60 0x0A magic) followed by the data and, if size is
// odd, a single pad byte, matching the layout pault.ag/go/debian/deb.ArEntry
// parses.
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
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	// Deterministic order is not required for these fixtures, but a stable
	// iteration keeps failures reproducible.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(data)),
		}); err != nil {
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

// buildDebBytes assembles a minimal, valid Debian binary package (format
// 2.0): debian-binary, a control.tar.gz containing exactly the given control
// text as ./control, and an empty data.tar.gz.
func buildDebBytes(t *testing.T, controlText string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar.gz", gzipTarOf(t, map[string][]byte{"./control": []byte(controlText)}))
	arMember(&buf, "data.tar.gz", gzipTarOf(t, map[string][]byte{}))
	return buf.Bytes()
}

// writeTestDeb builds a .deb from controlText and writes it under dir at the
// given pool-relative path (forward slashes), returning that relative path
// ready to use as a PoolFile.Path.
func writeTestDeb(t *testing.T, dir, poolRelPath, controlText string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(poolRelPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", poolRelPath, err)
	}
	if err := os.WriteFile(full, buildDebBytes(t, controlText), 0o644); err != nil {
		t.Fatalf("write %s: %v", poolRelPath, err)
	}
	return poolRelPath
}

// simpleControl renders a minimal, syntactically valid control stanza for a
// package with a given name/version/arch and no extra fields, terminated
// like a real one (trailing newline, no blank line).
func simpleControl(pkg, version, arch string) string {
	return fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.com>\nDescription: a test package\n",
		pkg, version, arch,
	)
}
