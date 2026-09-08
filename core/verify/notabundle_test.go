package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

// TestVerify_NotABundleIsUsage covers the refusal and, in the same file, the
// half of the boundary that must NOT move. Both are here on purpose: the two
// are one decision, and splitting them across files is how a later change
// softens one without noticing it has broken the other. This mirrors
// TestOpenTruncatedZstdStaysVerification, which does the same job for
// snapshot.Open after 2d6d5c7.
//
// The defect: `debark verify some-folder` on a directory that had never
// been near debark reported "1 problem: no debark.manifest.json in this
// bundle" and exited 4. Exit 4 is dferr.Verification — a signature, digest
// or metadata check did not match, the code a script escalates on and an
// operator reads as "the media may have been tampered with". Nothing had
// been verified. They had picked the wrong folder.
func TestVerify_NotABundleIsUsage(t *testing.T) {
	notBundles := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "an empty directory",
			setup: func(t *testing.T, dir string) {},
		},
		{
			name: "somebody's documents",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "notes.txt"), []byte("shopping list"))
				writeFile(t, filepath.Join(dir, "photo.jpg"), []byte("not really a jpeg"))
			},
		},
		{
			name: "a directory with a README and nothing else",
			setup: func(t *testing.T, dir string) {
				// README.txt is a real bundle file, and deliberately not a
				// marker: every third directory on a USB stick has one, and
				// a marker that fires on unrelated folders would put the
				// alarming class back on exactly the mistake this refusal
				// exists to catch.
				writeFile(t, filepath.Join(dir, "README.txt"), []byte("hello"))
			},
		},
		{
			name: "the media root, one level above the bundle",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "plant4-bundle", "repo"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range notBundles {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)

			report, err := New().Verify(context.Background(), dir, Options{})
			if err == nil {
				t.Fatalf("Verify returned a report instead of refusing: %+v", report)
			}
			if report != nil {
				t.Errorf("Verify returned both a report and an error; a document whose subject is \"can this artifact be trusted\" has nothing to say about a directory that is not an artifact")
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("dferr.ClassOf = %v (exit %d), want %v (exit %d): exit 4 means a check did not match, and nothing was checked here.\nerror: %v",
					got, int(got), dferr.Usage, int(dferr.Usage), err)
			}
			if !strings.Contains(err.Error(), "is not a debark bundle") {
				t.Errorf("the message does not say what is wrong: %v", err)
			}
			if dferr.HintOf(err) == "" {
				t.Errorf("no hint: an operator who pointed verify at the wrong folder needs to be told where the right one is")
			}
		})
	}
}

// TestVerify_BundleWithoutManifestStaysVerification is the other half, and
// the reason the refusal above tests for a bundle's PARTS rather than just
// for the manifest. A directory that really is a bundle and has had its
// manifest deleted is not an operator mistake — it is the single file that
// says what the bundle should contain, removed. Exit 4 is exactly right for
// that, and the boundary drawn here is structural, not a general softening.
func TestVerify_BundleWithoutManifestStaysVerification(t *testing.T) {
	// The fixture path, driven directly: build a real signed bundle, remove
	// only the manifest, and check the class did not move.
	f := buildFixture(t)
	if err := os.Remove(filepath.Join(f.dir, manifest.FileName)); err != nil {
		t.Fatal(err)
	}
	report, err := New().Verify(context.Background(), f.dir, Options{})
	if err != nil {
		t.Fatalf("Verify refused a real bundle: %v", err)
	}
	wantProblem(t, report, err, ProblemManifestMissing)

	// And the minimal case: each structural marker on its own is enough to
	// keep a directory inside "this is a bundle", so a partially wiped
	// bundle is never reclassified as somebody's holiday photos.
	for _, marker := range []string{manifest.SigFileName, lock.FileName, snapshotDocumentName} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, marker), []byte("{}"))
			report, err := New().Verify(context.Background(), dir, Options{})
			if err != nil {
				t.Fatalf("Verify refused a directory holding %s: %v", marker, err)
			}
			wantProblem(t, report, err, ProblemManifestMissing)
		})
	}
	t.Run(repoDirName, func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, repoDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		report, err := New().Verify(context.Background(), dir, Options{})
		if err != nil {
			t.Fatalf("Verify refused a directory holding %s/: %v", repoDirName, err)
		}
		wantProblem(t, report, err, ProblemManifestMissing)
	})
}
