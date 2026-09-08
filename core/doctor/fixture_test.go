package doctor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildAr is the encoder side of ar.go's parseAr, used only by tests to
// construct fixture .deb files without touching disk or a real dpkg-deb.
func buildAr(members []arMember) []byte {
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	for _, m := range members {
		if len(m.Name) > 16 {
			panic(fmt.Sprintf("fixture ar member name %q longer than 16 bytes", m.Name))
		}
		name := m.Name + "                "
		name = name[:16]
		buf.WriteString(name)
		fmt.Fprintf(&buf, "%-12d", 0)       // mtime
		fmt.Fprintf(&buf, "%-6d", 0)        // uid
		fmt.Fprintf(&buf, "%-6d", 0)        // gid
		fmt.Fprintf(&buf, "%-8s", "100644") // mode
		fmt.Fprintf(&buf, "%-10d", len(m.Data))
		buf.WriteString("`\n")
		buf.Write(m.Data)
		if len(m.Data)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// buildTarGzRaw builds an uncompressed tar containing files (name ->
// content), each written as a regular, executable file.
func buildTarGzRaw(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     "./" + name,
			Mode:     0755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write for %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return raw.Bytes()
}

// buildTarGz builds a gzip-compressed tar containing files (name -> content)
// — good enough to round-trip through parseDebReader's control.tar/data.tar
// handling.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	rawBytes := buildTarGzRaw(t, files)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(rawBytes); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gz.Bytes()
}

// readAllString reads r fully and fails the test on error.
func readAllString(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// fixtureDeb is what buildFixtureDeb needs to build one synthetic .deb.
type fixtureDeb struct {
	Control   string            // control file content; a sane default is used when empty
	Scripts   map[string]string // maintainer script name -> content
	DataFiles map[string]string // data.tar member content, e.g. for the DKMS usr/src/*/dkms.conf secondary signal
}

const defaultControl = "Package: fixture-pkg\n" +
	"Version: 1.0-1\n" +
	"Architecture: amd64\n" +
	"Maintainer: Test <test@example.invalid>\n" +
	"Description: a fixture package for doctor tests\n" +
	" Long description line.\n"

// buildFixtureDeb constructs a minimal, real ar(1)+tar+gzip .deb in memory:
// debian-binary, control.tar.gz (control + any maintainer scripts), and
// data.tar.gz (any data files given, e.g. for the DKMS secondary signal).
func buildFixtureDeb(t *testing.T, f fixtureDeb) []byte {
	t.Helper()
	control := f.Control
	if control == "" {
		control = defaultControl
	}
	controlFiles := map[string]string{"control": control}
	for name, content := range f.Scripts {
		controlFiles[name] = content
	}
	dataFiles := f.DataFiles
	if dataFiles == nil {
		dataFiles = map[string]string{}
	}
	return buildAr([]arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.gz", Data: buildTarGz(t, controlFiles)},
		{Name: "data.tar.gz", Data: buildTarGz(t, dataFiles)},
	})
}

// buildFixtureDebZstd is buildFixtureDeb with a zstd control member instead
// of a gzip one. It exists because zstd is the compression doctor's bounds
// have to be checked against most carefully — E7 measured 812 of 1,749 real
// packages using it for control.tar, and it is the only decoder here that
// allocates from a size the archive itself declares (see maxZstdWindow).
func buildFixtureDebZstd(t *testing.T, f fixtureDeb) []byte {
	t.Helper()
	control := f.Control
	if control == "" {
		control = defaultControl
	}
	controlFiles := map[string]string{"control": control}
	for name, content := range f.Scripts {
		controlFiles[name] = content
	}
	raw := buildTarGzRaw(t, controlFiles)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	return buildAr([]arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.zst", Data: enc.EncodeAll(raw, nil)},
		{Name: "data.tar.gz", Data: buildTarGz(t, map[string]string{})},
	})
}

// writeFixtureDeb writes a fixture .deb to dir/name and returns its full path.
func writeFixtureDeb(t *testing.T, dir, name string, f fixtureDeb) string {
	t.Helper()
	data := buildFixtureDeb(t, f)
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return full
}
