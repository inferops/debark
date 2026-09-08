package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/fetch"
)

// TestDpkgDebPackageName_RealDeb proves the package name comes back
// correctly from a structurally real .deb, decoded in pure Go. PATH is
// emptied for the call (t.Setenv, restored automatically): a regression
// back to shelling out to dpkg-deb would fail this test even on a machine
// that happens to have dpkg-deb installed, which is the whole point of the
// rewrite (see the portability rationale in debcontrol.go) — the source-code
// grep in the implementation notes is one proof, this is the behavioural one.
func TestDpkgDebPackageName_RealDeb(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mytool_1.0_amd64.deb")
	if err := fetch.WriteFixtureDeb(p, fetch.FixtureDeb{
		Package: "mytool", Version: "1.0", Architecture: "amd64", Depends: "libc6 (>= 2.34)",
	}); err != nil {
		t.Fatalf("WriteFixtureDeb: %v", err)
	}

	t.Setenv("PATH", "")

	name, err := dpkgDebPackageName(context.Background(), p)
	if err != nil {
		t.Fatalf("dpkgDebPackageName: %v", err)
	}
	if name != "mytool" {
		t.Errorf("name = %q, want %q", name, "mytool")
	}
}

// TestDpkgDebPackageName_NotADeb exercises the error path for a file that is
// not a .deb at all (an ar archive is expected; this is neither).
func TestDpkgDebPackageName_NotADeb(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notadeb.deb")
	if err := os.WriteFile(p, []byte("<html><body>404 not found</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", "")

	_, err := dpkgDebPackageName(context.Background(), p)
	if err == nil {
		t.Fatal("want an error for a non-.deb file")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
}

// TestDpkgDebPackageName_MissingFile exercises the error path for a path
// that does not exist.
func TestDpkgDebPackageName_MissingFile(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := dpkgDebPackageName(context.Background(), filepath.Join(t.TempDir(), "nope.deb"))
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

// TestDpkgDebPackageName_DependencyFieldsAvailable documents, at the
// fetch.ReadControlInfo level, that version/architecture/dependency fields
// this function does not surface are not lost — they were read in the same
// single pass this function's one call to ReadControlInfo performs, so a
// future caller wanting them calls fetch.ReadControlInfo(path) directly
// rather than adding a second reader here. This guards that claim: if
// ReadControlInfo ever stopped populating these fields, this would catch it.
func TestDpkgDebPackageName_DependencyFieldsAvailable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mytool_1.0_amd64.deb")
	if err := fetch.WriteFixtureDeb(p, fetch.FixtureDeb{
		Package: "mytool", Version: "1.0", Architecture: "amd64", Depends: "libc6 (>= 2.34)",
	}); err != nil {
		t.Fatalf("WriteFixtureDeb: %v", err)
	}

	info, err := fetch.ReadControlInfo(p)
	if err != nil {
		t.Fatalf("ReadControlInfo: %v", err)
	}
	if info.Package != "mytool" || info.Version != "1.0" || info.Architecture != "amd64" || info.Depends != "libc6 (>= 2.34)" {
		t.Errorf("ControlInfo = %+v, want Package=mytool Version=1.0 Architecture=amd64 Depends=%q", info, "libc6 (>= 2.34)")
	}
}
