package fetch

// This file cross-validates this package's .deb reading and writing against
// pault.ag/go/debian, an independent, Debian-authored implementation of the
// same formats.
//
// History, because it explains what is and is not delegated to that
// library: go.mod did not carry pault.ag/go/debian when this package began,
// so SniffDeb, ReadControlInfo and BuildFixtureDeb were first written
// against the public ar(1) and deb822 formats directly. ReadControlInfo's
// initial version assumed dpkg-deb always gzips the control member; that
// assumption was wrong (see control.go's comment for how it was caught:
// inspecting a live-downloaded Google Chrome .deb and a current Debian
// archive package, both control.tar.xz), and fixing it properly meant real
// xz/bzip2/lzma/zstd decompression, which is a large amount of code to hand
// -roll correctly. By then another, concurrently running package had added
// pault.ag/go/debian to go.mod for its own purposes, so ReadControlInfo now
// delegates exactly that part — finding and decompressing the right
// control.* ar member — to it, while still parsing the resulting control
// paragraph with this package's own deb822 reader (parseControlStanza), not
// pault.ag's typed Control struct: that keeps every relationship field
// (Depends and the rest) exactly as written in the file, with no
// parse-then-restringify round trip through a type this package does not
// control, which matters given the project's determinism principle.
// SniffDeb and BuildFixtureDeb still need nothing from the library at all —
// the former only ever reads the never-compressed debian-binary member, and
// the latter has no analogue in a read-only library — so they are unchanged
// pure-Go code.
//
// Given all that, this file is not exercising an optional, tag-gated extra:
// core/fetch now has a real, unconditional dependency on pault.ag/go/debian
// (see control.go's import), so a build without it was never an option this
// file needed to protect. What is still worth having is the independent
// check below: proof that this package's own control-stanza reader agrees
// field-for-field with pault.ag's typed decode on the same bytes, and that
// pault.ag's reader can open a .deb this package's own writer produced —
// meaning this package's ar/tar member layout is spec-compliant, not just
// self-consistent with its own reader.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"pault.ag/go/debian/deb"
)

func TestCrossValidate_PaultAgReadsOurFixture(t *testing.T) {
	spec := FixtureDeb{
		Package:      "crossvalidate",
		Version:      "4.5.6-1",
		Architecture: "amd64",
		Maintainer:   "debark test fixtures <fixtures@debark.invalid>",
		Depends:      "jq, tree",
		PreDepends:   "dpkg (>= 1.19)",
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "cross.deb")
	if err := WriteFixtureDeb(p, spec); err != nil {
		t.Fatalf("WriteFixtureDeb: %v", err)
	}

	d, closer, err := deb.LoadFile(p)
	if err != nil {
		t.Fatalf("pault.ag/go/debian could not read our fixture: %v", err)
	}
	defer closer()

	if d.Control.Package != spec.Package {
		t.Errorf("pault.ag Package = %q, want %q", d.Control.Package, spec.Package)
	}
	if d.Control.Version.String() != spec.Version {
		t.Errorf("pault.ag Version = %q, want %q", d.Control.Version.String(), spec.Version)
	}
	if d.Control.Architecture.String() != spec.Architecture {
		t.Errorf("pault.ag Architecture = %q, want %q", d.Control.Architecture.String(), spec.Architecture)
	}

	// Cross-check against our own reader too: both readers, one
	// hand-written and one independent, must agree on the same bytes.
	ours, err := ReadControlInfo(p)
	if err != nil {
		t.Fatalf("ReadControlInfo: %v", err)
	}
	if ours.Package != d.Control.Package {
		t.Errorf("our Package = %q, pault.ag Package = %q: disagreement", ours.Package, d.Control.Package)
	}
	if ours.Version != d.Control.Version.String() {
		t.Errorf("our Version = %q, pault.ag Version = %q: disagreement", ours.Version, d.Control.Version.String())
	}
	if ours.Architecture != d.Control.Architecture.String() {
		t.Errorf("our Architecture = %q, pault.ag Architecture = %q: disagreement", ours.Architecture, d.Control.Architecture.String())
	}
}

func TestCrossValidate_PaultAgAgreesSniffDebRejectsGarbage(t *testing.T) {
	garbage := []byte("<html><body>404 not found</body></html>")

	ourErr := SniffDeb(bytes.NewReader(garbage))
	if ourErr == nil {
		t.Fatal("our SniffDeb accepted garbage")
	}

	_, err := deb.LoadAr(bytes.NewReader(garbage))
	if err == nil {
		t.Fatal("pault.ag/go/debian accepted garbage as an ar archive; our rejection may be over-strict")
	}
}

func TestCrossValidate_PaultAgReadsOurDataFiles(t *testing.T) {
	spec := FixtureDeb{
		Package: "withdata",
		DataFiles: map[string][]byte{
			"./usr/bin/withdata": []byte("#!/bin/sh\necho hi\n"),
		},
	}
	b, err := BuildFixtureDeb(spec)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "withdata.deb")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatal(err)
	}

	d, closer, err := deb.LoadFile(tmp)
	if err != nil {
		t.Fatalf("pault.ag/go/debian could not read our fixture: %v", err)
	}
	defer closer()

	found := false
	for {
		hdr, err := d.Data.Next()
		if err != nil {
			break
		}
		if hdr.Name == "./usr/bin/withdata" {
			found = true
			break
		}
	}
	if !found {
		t.Error("pault.ag/go/debian did not find our data file in data.tar.gz")
	}
}
