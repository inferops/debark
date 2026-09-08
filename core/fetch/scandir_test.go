package fetch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

func TestScanDir(t *testing.T) {
	dir := t.TempDir()
	names := []string{"c.deb", "a.deb", "b.deb"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a .deb: must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nested .deb: must be ignored (non-recursive).
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.deb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory that happens to be named *.deb: must be ignored too.
	if err := os.MkdirAll(filepath.Join(dir, "dirlookslikea.deb"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ScanDir(dir)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	want := []string{
		filepath.Join(dir, "a.deb"),
		filepath.Join(dir, "b.deb"),
		filepath.Join(dir, "c.deb"),
	}
	if !equalStrings(got, want) {
		t.Errorf("ScanDir = %v, want %v (sorted, non-recursive)", got, want)
	}
}

func TestScanDir_Empty(t *testing.T) {
	dir := t.TempDir()
	got, err := ScanDir(dir)
	if err != nil {
		t.Fatalf("ScanDir on an empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ScanDir on an empty dir = %v, want none", got)
	}
}

func TestScanDir_NotFound(t *testing.T) {
	_, err := ScanDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}
