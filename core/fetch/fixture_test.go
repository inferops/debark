package fetch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildFixtureDeb_RoundTrips(t *testing.T) {
	b, err := BuildFixtureDeb(FixtureDeb{
		Package:      "mytool",
		Version:      "1.2.3-1",
		Architecture: "amd64",
		Depends:      "jq, tree",
		PreDepends:   "dpkg (>= 1.19)",
		Recommends:   "less",
		ExtraFields:  map[string]string{"Section": "utils", "Priority": "optional"},
	})
	if err != nil {
		t.Fatalf("BuildFixtureDeb: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("BuildFixtureDeb returned no bytes")
	}
	if !bytes.HasPrefix(b, []byte(arMagic)) {
		t.Fatalf("output does not start with the ar magic")
	}

	if err := SniffDeb(bytes.NewReader(b)); err != nil {
		t.Fatalf("SniffDeb rejected our own fixture: %v", err)
	}

	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	want := ControlInfo{
		Package: "mytool", Version: "1.2.3-1", Architecture: "amd64",
		Depends: "jq, tree", PreDepends: "dpkg (>= 1.19)", Recommends: "less",
	}
	if info.Package != want.Package || info.Version != want.Version || info.Architecture != want.Architecture ||
		info.Depends != want.Depends || info.PreDepends != want.PreDepends || info.Recommends != want.Recommends {
		t.Errorf("ControlInfo = %+v, want (at least) %+v", info, want)
	}
	if info.SourceName() != "mytool" {
		t.Errorf("SourceName() = %q, want mytool (falls back to Package)", info.SourceName())
	}
}

func TestBuildFixtureDeb_Defaults(t *testing.T) {
	b, err := BuildFixtureDeb(FixtureDeb{})
	if err != nil {
		t.Fatalf("BuildFixtureDeb: %v", err)
	}
	info, err := readControlInfo(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("readControlInfo: %v", err)
	}
	if info.Package == "" || info.Version == "" || info.Architecture == "" {
		t.Errorf("zero-value FixtureDeb produced an incomplete control stanza: %+v", info)
	}
}

func TestBuildFixtureDeb_Deterministic(t *testing.T) {
	spec := FixtureDeb{Package: "detpkg", Version: "1.0", Depends: "libc6"}
	a, err := BuildFixtureDeb(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildFixtureDeb(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("BuildFixtureDeb is not deterministic for the same spec")
	}
}

func TestBuildFixtureDeb_CustomDataFiles(t *testing.T) {
	b, err := BuildFixtureDeb(FixtureDeb{
		Package: "withdata",
		DataFiles: map[string][]byte{
			"./usr/bin/withdata": []byte("#!/bin/sh\necho hi\n"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := SniffDeb(bytes.NewReader(b)); err != nil {
		t.Fatalf("SniffDeb: %v", err)
	}
}

func TestWriteFixtureDeb(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "pkg.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "onfile", Version: "9.9"}); err != nil {
		t.Fatalf("WriteFixtureDeb: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture file not written: %v", err)
	}
	info, err := ReadControlInfo(p)
	if err != nil {
		t.Fatalf("ReadControlInfo: %v", err)
	}
	if info.Package != "onfile" || info.Version != "9.9" {
		t.Errorf("ControlInfo = %+v", info)
	}
}
