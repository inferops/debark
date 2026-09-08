package fetch

// BuildFixtureDeb builds a tiny, structurally real .deb entirely in memory,
// for tests that need to exercise real .deb parsing without checking in a
// binary fixture. It is a plain (non-test) file on purpose, so any other packages can import and reuse it instead of writing its own.
//
// Sharing this across packages: core/fetch imports core/store, core/lock
// and core/evidence, so a same-package ("package store") test *inside* any
// of those three packages cannot import core/fetch — that would close an
// import cycle. An external test package (conventionally "package
// store_test") can import "github.com/inferops/debark/core/fetch" freely,
// and so can any test, internal or external, in a package core/fetch does
// not depend on — which is everything else (apt, sign, manifest,
// repository, bundle, install, doctor, policy, snapshot, resolve, engine,
// distro, canonical, digest, version, dferr, buildjob's own tests aside).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FixtureDeb describes the control metadata for a synthetic .deb built by
// BuildFixtureDeb. Every field is optional; the zero value produces a
// small, valid, installable-looking package.
type FixtureDeb struct {
	Package      string // default "debark-fixture"
	Version      string // default "1.0"
	Architecture string // default "amd64"
	Maintainer   string
	Description  string
	Depends      string
	PreDepends   string
	Recommends   string
	// ExtraFields adds arbitrary deb822 fields (Source, Multi-Arch,
	// Provides, Conflicts, ...) without widening this struct. Each value
	// must be a single line; written in sorted key order for determinism.
	ExtraFields map[string]string
	// DataFiles adds files to data.tar.gz, keyed by their in-archive path
	// (e.g. "./usr/bin/thing"). Nil or empty still produces a valid data
	// member with one placeholder file.
	DataFiles map[string][]byte
}

// BuildFixtureDeb returns the bytes of a small, structurally real .deb: an
// ar archive containing debian-binary, control.tar.gz and data.tar.gz, in
// that order, laid out the same way a real dpkg-deb output is. It round
// trips through this package's own SniffDeb and ReadControlInfo (see
// fixture_test.go), and through any correct .deb reader.
func BuildFixtureDeb(spec FixtureDeb) ([]byte, error) {
	pkg := spec.Package
	if pkg == "" {
		pkg = "debark-fixture"
	}
	version := spec.Version
	if version == "" {
		version = "1.0"
	}
	arch := spec.Architecture
	if arch == "" {
		arch = "amd64"
	}
	maintainer := spec.Maintainer
	if maintainer == "" {
		maintainer = "debark test fixtures <fixtures@debark.invalid>"
	}
	description := spec.Description
	if description == "" {
		description = "synthetic package built by core/fetch's BuildFixtureDeb test helper"
	}

	var control strings.Builder
	fmt.Fprintf(&control, "Package: %s\n", pkg)
	fmt.Fprintf(&control, "Version: %s\n", version)
	fmt.Fprintf(&control, "Architecture: %s\n", arch)
	fmt.Fprintf(&control, "Maintainer: %s\n", maintainer)
	fmt.Fprintf(&control, "Installed-Size: 1\n")
	if spec.PreDepends != "" {
		fmt.Fprintf(&control, "Pre-Depends: %s\n", spec.PreDepends)
	}
	if spec.Depends != "" {
		fmt.Fprintf(&control, "Depends: %s\n", spec.Depends)
	}
	if spec.Recommends != "" {
		fmt.Fprintf(&control, "Recommends: %s\n", spec.Recommends)
	}
	extraKeys := make([]string, 0, len(spec.ExtraFields))
	for k := range spec.ExtraFields {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		fmt.Fprintf(&control, "%s: %s\n", k, spec.ExtraFields[k])
	}
	fmt.Fprintf(&control, "Description: %s\n", description)

	controlTarGz, err := buildTarGz(map[string][]byte{"./control": []byte(control.String())})
	if err != nil {
		return nil, fmt.Errorf("fetch: fixture: building control.tar.gz: %w", err)
	}

	dataFiles := spec.DataFiles
	if len(dataFiles) == 0 {
		dataFiles = map[string][]byte{
			"./usr/share/doc/" + pkg + "/fixture.txt": []byte("built by core/fetch.BuildFixtureDeb for tests\n"),
		}
	}
	dataTarGz, err := buildTarGz(dataFiles)
	if err != nil {
		return nil, fmt.Errorf("fetch: fixture: building data.tar.gz: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(arMagic)
	writeArMember(&buf, "debian-binary", []byte("2.0\n"))
	writeArMember(&buf, "control.tar.gz", controlTarGz)
	writeArMember(&buf, "data.tar.gz", dataTarGz)
	return buf.Bytes(), nil
}

// WriteFixtureDeb builds a fixture .deb per spec and writes it to path,
// creating parent directories as needed. A convenience for tests that want
// a real file on disk rather than an in-memory []byte.
func WriteFixtureDeb(path string, spec FixtureDeb) error {
	b, err := BuildFixtureDeb(spec)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("fetch: fixture: %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("fetch: fixture: %s: %w", path, err)
	}
	return nil
}

// buildTarGz writes files (in sorted-key order, for byte-identical output
// across runs — this project never lets non-determinism into a fixture any
// more than into a real artefact) as a gzip-compressed tar archive.
func buildTarGz(files map[string][]byte) ([]byte, error) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return gz.Bytes(), nil
}

// writeArMember appends one ar(1) member (header plus content plus the
// pad byte an odd-length member needs) to buf. Timestamp, uid and gid are
// fixed at zero: deterministic output, and dpkg has never cared about them.
//
// name must fit the format's fixed 16-byte name field: %-16s does not
// truncate a longer string, it just prints all of it, which would silently
// shift every field after it out of place and produce a corrupt member that
// only fails much later (and confusingly) when something tries to read it.
// Every caller in this file uses a fixed, short, known-safe name, so this is
// a programmer error, not a runtime condition — hence panic, not an error
// return that every call site would otherwise have to check.
func writeArMember(buf *bytes.Buffer, name string, content []byte) {
	if len(name) > 16 {
		panic(fmt.Sprintf("fetch: fixture: ar member name %q is longer than the 16-byte name field", name))
	}
	fmt.Fprintf(buf, "%-16s%-12d%-6d%-6d%-8s%-10d", name, 0, 0, 0, "100644", len(content))
	buf.Write([]byte{0x60, 0x0A})
	buf.Write(content)
	if len(content)%2 == 1 {
		buf.WriteByte('\n')
	}
}
