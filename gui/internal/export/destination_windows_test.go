//go:build windows

package export

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExportWindowsDestinationCaseAlias(t *testing.T) {
	for _, child := range []string{"", "new-bundle"} {
		t.Run(child, func(t *testing.T) {
			src, parent := t.TempDir(), t.TempDir()
			flatBundle(t, src, 1, 128)
			dir := filepath.Join(parent, "MixedCaseDestination")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(parent, "mixedcasedestination")
			if _, err := os.Stat(alias); os.IsNotExist(err) {
				t.Skip("destination filesystem distinguishes case")
			} else if err != nil {
				t.Fatal(err)
			}
			res, err := NewExporter(Options{}).Export(context.Background(), Request{
				SourceDir: src, DestDir: filepath.Join(alias, child), DestFreeBytes: 1 << 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Complete {
				t.Fatal("export through a case alias did not complete")
			}
			assertMarkerInDirectory(t, res.Plan.MarkerPath, filepath.Join(dir, child))
			assertDestMatches(t, filepath.Join(dir, child), map[string][]byte{
				"a.bin": bytes.Repeat([]byte{'a'}, 128),
			})
		})
	}
}
