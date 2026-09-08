package export

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExportRefusesDestinationAliasIntoSource(t *testing.T) {
	for _, child := range []string{"", "new-bundle"} {
		t.Run(child, func(t *testing.T) {
			src := t.TempDir()
			flatBundle(t, src, 1, 128)
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(src, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			_, err := NewExporter(Options{}).Export(context.Background(), Request{
				SourceDir: src, DestDir: filepath.Join(alias, child), DestFreeBytes: 1 << 30,
			})
			if KindOf(err) != ErrKindOverlap {
				t.Fatalf("got %v, want overlap refusal", err)
			}
			got, err := os.ReadFile(filepath.Join(src, "a.bin"))
			if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{'a'}, 128)) {
				t.Fatalf("export changed its source: %q, %v", got, err)
			}
		})
	}
}

func TestExportDoesNotTruncateDestinationHardlink(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 1, 128)
	outside := filepath.Join(t.TempDir(), "keep")
	want := []byte("unrelated file that must survive")
	if err := os.WriteFile(outside, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dst, "a.bin")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	res, err := NewExporter(Options{}).Export(context.Background(), Request{
		SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30,
	})
	if err != nil || !res.Complete {
		t.Fatalf("export failed: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("export truncated another hardlink: %q, %v", got, err)
	}
}

func TestExportRefusesUnexpectedDestinationEntries(t *testing.T) {
	for _, name := range []string{"old.deb", "old-directory", "file-link", "directory-link", "marker-link"} {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			flatBundle(t, src, 1, 128)
			switch name {
			case "old.deb":
				if err := os.WriteFile(filepath.Join(dst, name), []byte("old bundle"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "old-directory":
				if err := os.Mkdir(filepath.Join(dst, name), 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				target, link := filepath.Join(src, "a.bin"), "a.bin"
				if name == "directory-link" {
					target, link = src, "pool"
					if err := os.Mkdir(filepath.Join(src, "pool"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if name == "marker-link" {
					link = MarkerName
				}
				if err := os.Symlink(target, filepath.Join(dst, link)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			_, err := NewExporter(Options{}).Export(context.Background(), Request{
				SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30,
			})
			if KindOf(err) != ErrKindUnsupported {
				t.Fatalf("got %v, want unsupported destination refusal", err)
			}
		})
	}
}

func TestRunRechecksDestinationAfterPlanning(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	flatBundle(t, src, 1, 128)
	e := NewExporter(Options{})
	p := mustPlan(t, e, Request{SourceDir: src, DestDir: dst, DestFreeBytes: 1 << 30})
	if err := os.WriteFile(filepath.Join(dst, "new-unrelated-file"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), p); KindOf(err) != ErrKindUnsupported {
		t.Fatalf("Run did not recheck the destination: %v", err)
	}
}
