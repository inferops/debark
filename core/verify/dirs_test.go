package verify

import (
	"os"
	"path/filepath"
	"testing"
)

// This file pins a deliberate decision: verify ACCEPTS a directory the
// manifest does not list, as long as it is empty.
//
// It is written down because the decision looks like an oversight. Both halves
// of the file machinery return early on a directory - manifest.Build does not
// record one, and checkFiles skips one - so an attacker can add
// repo/pool/e/evil/ to a signed bundle and it verifies OK. Somebody will
// eventually notice that and "fix" it. These tests are the argument against,
// and the boundary that makes the argument hold.
//
// Why it is acceptable:
//
//  1. A directory carries no bytes. There is nothing for the manifest's File
//     shape (path, size, sha256) to say about one, and nothing in a directory
//     for apt to fetch, dpkg to unpack, or anything to execute. The property
//     the manifest exists to guarantee is "every BYTE on this medium is
//     covered by the signed document", and an empty directory contains none.
//
//  2. The acceptance is of an empty container, and emptiness is not taken on
//     trust - it is enforced, by the same walk, on every run. Put one file in
//     that directory and the walk reports file-unexpected for it, exactly as
//     it would anywhere else in the tree. There is no state in which the
//     directory is accepted AND its contents are unattested, which is what
//     would make this a hole.
//
//  3. Refusing one would need the manifest to list directories, which is an
//     api/schema/manifest.v1.schema.json change and therefore not verify's to
//     make. It would also make verification depend on how the medium was
//     produced: tar creates ancestor directories on extraction, and different
//     filesystems and copy tools disagree about which empty directories
//     survive a round trip. A bundle that verifies on one target and not
//     another, over a directory holding nothing, would cost far more trust
//     than it bought.
//
// What is NOT accepted, and is covered elsewhere: a directory standing at a
// path the manifest lists as a file (tamper_test.go's "listed file replaced by
// a directory" - file-missing for the path plus file-unexpected for anything
// smuggled underneath), and a directory where lock.json or snapshot.json
// should be (tamper_test.go's malformed-documents cases).

// TestVerify_AddedEmptyDirectoryIsAccepted is the decision itself. Three
// placements, including the most alarming-looking one - a directory named
// exactly like a .deb, sitting in the pool next to the real packages.
//
// Even that is inert: apt fetches what repo/Packages names, repo/Packages is
// digest-checked against the signed manifest, so nothing can add a stanza
// pointing at it, and there are no bytes behind the name in any case.
func TestVerify_AddedEmptyDirectoryIsAccepted(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"at the bundle root", "unlisted-dir"},
		{"deep inside the pool", "repo/pool/e/evil"},
		{"named exactly like a package file", "repo/pool/d/demo/backdoor_1.0-1_amd64.deb"},
		{"beside the index apt reads", "repo/dists"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			clean, cerr := verifyFixture(t, f, Options{})
			if cerr != nil || !clean.OK {
				t.Fatalf("fixture did not verify before the change: %v %v", cerr, problemKinds(clean))
			}

			if err := os.MkdirAll(filepath.Join(f.dir, filepath.FromSlash(tc.rel)), 0o755); err != nil {
				t.Fatal(err)
			}

			r, err := verifyFixture(t, f, Options{})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !r.OK {
				t.Fatalf("an empty unlisted directory is a deliberate accept, not a refusal; problems=%v", problemKinds(r))
			}
			// Nothing about the coverage numbers moved: the directory
			// contributed no file and no byte, which is the whole reason it is
			// acceptable. If FilesChecked ever changes here, a directory is
			// being counted as a file somewhere and this decision needs
			// revisiting.
			if r.FilesChecked != clean.FilesChecked || r.BytesChecked != clean.BytesChecked {
				t.Fatalf("adding an empty directory changed the coverage counts: files %d->%d, bytes %d->%d",
					clean.FilesChecked, r.FilesChecked, clean.BytesChecked, r.BytesChecked)
			}
		})
	}
}

// TestVerify_AnythingInsideAnAddedDirectoryIsRefused is the boundary that
// makes the accept above safe, and it is the half that must never regress.
//
// The directory is tolerated only because it is empty. The moment it holds
// anything at all - a payload .deb, a dotfile, an empty file, a nested tree -
// that content is a file the signed manifest does not cover, and the walk says
// so under its own path. There is no depth at which the walk stops looking.
func TestVerify_AnythingInsideAnAddedDirectoryIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		rel     string
		content []byte
	}{
		{"a payload package", "repo/pool/e/evil/backdoor_1.0-1_amd64.deb", []byte("payload")},
		{"a zero-byte file", "repo/pool/e/evil/.keep", nil},
		{"a dotfile", "unlisted-dir/.hidden", []byte("x")},
		{"buried several levels down", "unlisted-dir/a/b/c/d/payload", []byte("deep")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			writeFile(t, filepath.Join(f.dir, filepath.FromSlash(tc.rel)), tc.content)

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, ProblemFileUnexpected)
			wantOnlyKinds(t, r, ProblemFileUnexpected)
			wantOnlyKnownKinds(t, r)
			if !hasProblem(r, ProblemFileUnexpected, tc.rel) {
				t.Fatalf("expected %s naming %s verbatim; problems=%v", ProblemFileUnexpected, tc.rel, problemKinds(r))
			}
			// The rest of the bundle was still checked: an unexpected file
			// must not short-circuit the walk, or one stray file would hide
			// every real tamper behind it.
			wantReachedContentChecks(t, r)
		})
	}
}

// TestVerify_RemovedEmptyDirectoryIsAccepted is the mirror image, and it is
// accepted for the same reason plus one more: an empty directory that
// disappears in transit takes nothing with it, and every directory that holds
// a manifest-listed file cannot disappear without that file coming up missing.
// So the only removals this tolerates are removals of nothing.
func TestVerify_RemovedEmptyDirectoryIsAccepted(t *testing.T) {
	f := buildFixture(t)
	dir := filepath.Join(f.dir, "repo", "pool", "e", "empty-on-the-builder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Sign the bundle in that state, so the directory really was there when
	// the manifest was made.
	resealBundle(t, f)
	if r, err := verifyFixture(t, f, Options{}); err != nil || !r.OK {
		t.Fatalf("the bundle must verify with the empty directory present: %v %v", err, problemKinds(r))
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	r, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK {
		t.Fatalf("removing an empty directory removes no attested byte; problems=%v", problemKinds(r))
	}
}

// TestVerify_DirectoryHoldingAListedFileCannotDisappearQuietly is the
// assertion that limits the two above: the tolerance is for EMPTY directories
// only, and it is limited by the manifest's file list rather than by any rule
// about directories.
//
// Removing a directory that holds attested files takes those files with it,
// and each one must be reported missing under its own path - not one summary
// line about the directory, which an operator cannot act on.
func TestVerify_DirectoryHoldingAListedFileCannotDisappearQuietly(t *testing.T) {
	const rel = "repo/pool/d/demo/demo_1.0-1_amd64.deb"

	f := buildFixture(t)
	if err := os.RemoveAll(filepath.Join(f.dir, "repo", "pool")); err != nil {
		t.Fatal(err)
	}

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemFileMissing)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemFileMissing, rel) {
		t.Fatalf("expected %s naming the file, not its directory; problems=%v", ProblemFileMissing, rel)
	}
	// The remaining files were still checked, so the report describes the
	// whole medium rather than stopping at the first gap.
	wantReachedContentChecks(t, r)
}
