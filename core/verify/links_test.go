package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
)

// This file covers one property: every entry the bundle walk meets must be a
// real, regular file that carries its own bytes under the bundle root.
//
// It is the property that decides whether verification means anything at all
// once apt takes over. Every digest check below the walk opens the path and
// follows it wherever it goes - digest.SHA256File uses os.Open, and
// checkLockAndSnapshot uses os.ReadFile - so a name that merely *points* at
// the right bytes verifies exactly like the bytes themselves. The difference
// only shows up afterwards: the data behind such a name never lived under the
// verified root, so it can be swapped between the moment verify hashes it and
// the moment apt reads it, without touching the (possibly read-only) medium
// verify just approved. That turns a narrow race into "pre-position a file and
// wait".
//
// Both shapes are built here against real bundles, with content that MATCHES
// the manifest exactly - a link to the wrong bytes would be caught by the
// digest check and would prove nothing. The refusal under test has to happen
// because of what the entry *is*, not what it contains.
//
// The same rule already holds on the way in: core/bundle/tar.go refuses tar
// TypeSymlink and TypeLink outright ("bundles never contain links") and
// refuses to export anything that is not a regular file. This file holds the
// matching rule at the point it actually protects an operator.

// hardLinksAvailable probes rather than assumes: hard links need the source
// and destination on one volume, and some filesystems have none at all.
func hardLinksAvailable(t *testing.T) bool {
	t.Helper()
	d := t.TempDir()
	target := filepath.Join(d, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return os.Link(target, filepath.Join(d, "link")) == nil
}

// relinkOutside replaces the bundle file at rel with a hard link to a copy of
// its own bytes parked OUTSIDE the bundle, in the fixture's parent temp dir.
// The bundle's directory entry is then a second name for data that lives -
// and can be rewritten - somewhere the walk never looks, while every byte
// verify reads through it still matches the signed manifest.
func relinkOutside(t *testing.T, f *fixture, rel string) {
	t.Helper()
	inside := filepath.Join(f.dir, filepath.FromSlash(rel))
	data, err := os.ReadFile(inside)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	outside := filepath.Join(filepath.Dir(f.dir), "attacker-blob-"+filepath.Base(rel))
	if err := os.WriteFile(outside, data, 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if err := os.Remove(inside); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Fatalf("hard link %s: %v", rel, err)
	}
}

// TestVerify_HardLinkedBundleEntry is the executable form of the attack: a
// manifest-listed path whose contents are byte-identical to what the signed
// manifest says, reached through a second name for a file outside the bundle.
// Before the entry-type gate every one of these verified *** OK ***, with the
// full file count and no warning.
// TestVerify_HardLinkedBundleEntry pins that a hard link is ACCEPTED, which
// is the opposite of what an earlier version of this file asserted.
//
// This is not a relaxation for convenience: it is the product's normal state.
// store.Materialise hard links pool objects out of the content-addressed
// store into the bundle, so EVERY freshly built bundle has link count 2 on
// every .deb, and refusing that made `verify` fail on bundles debark had
// just written. Only a bundle round-tripped through tar export/import passed,
// because core/bundle/tar.go refuses link entries and the extracted copy has
// one name - which is why the export test kept passing and hid the break.
//
// The security argument is in verify.go: a hard link cannot cross a
// filesystem, so it cannot reach the writable-target case that makes a
// symlink dangerous, and where the bundle is writable at all the file's own
// mode already grants what a second name would. The symlink form is still
// refused outright, by the entry-type gate.
func TestVerify_HardLinkedBundleEntry(t *testing.T) {
	if !hardLinksAvailable(t) {
		t.Skip("this filesystem does not support hard links; skipping")
	}

	cases := []struct {
		name string
		rel  string
	}{
		// The one that matters most: this is exactly what store.Materialise
		// produces for every package in every bundle.
		{"a pool .deb", "repo/pool/d/demo/demo_1.0-1_amd64.deb"},
		{"the repository index", "repo/Packages"},
		{"lock.json", "lock.json"},
		{"snapshot.json", "snapshot.json"},
		{"the manifest itself", manifest.FileName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			relinkOutside(t, f, tc.rel)

			r, err := verifyFixture(t, f, Options{})
			if err != nil {
				t.Fatalf("verify returned a Go error for a hard-linked entry: %v", err)
			}
			if hasProblem(r, ProblemFileNotRegular, tc.rel) {
				t.Fatalf("hard link at %s was refused as not-a-regular-file; "+
					"that rejects every bundle assembled from the store", tc.rel)
			}
			if !r.OK {
				t.Fatalf("bundle with a hard-linked %s did not verify; problems=%v", tc.rel, problemKinds(r))
			}
		})
	}
}

// TestVerify_HardLinkAtAnUnlistedPath pins that a hard link the manifest does
// not list is refused for the reason that actually applies to it - it is a
// file the signed manifest does not cover - rather than for being a link.
// Since hard links are no longer refused as such, file-unexpected is the
// whole of the finding here.
func TestVerify_HardLinkAtAnUnlistedPath(t *testing.T) {
	if !hardLinksAvailable(t) {
		t.Skip("this filesystem does not support hard links; skipping")
	}

	f := buildFixture(t)
	outside := filepath.Join(filepath.Dir(f.dir), "attacker-blob")
	if err := os.WriteFile(outside, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(f.dir, "repo", "extra")); err != nil {
		t.Fatal(err)
	}

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemFileUnexpected)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemFileUnexpected, "repo/extra") {
		t.Fatalf("expected %s naming repo/extra; problems=%v", ProblemFileUnexpected, problemKinds(r))
	}
	// The refusal must not depend on it being a link: an attacker who simply
	// COPIES the payload in gets the identical verdict, which is the point -
	// coverage by the signed manifest is what is being enforced here, not the
	// number of names the bytes happen to have.
	if hasProblem(r, ProblemFileNotRegular, "repo/extra") {
		t.Fatalf("a hard link was reported as not-a-regular-file; hard links are accepted: problems=%v", problemKinds(r))
	}
}

// TestVerify_SymlinkToIdenticalContentOutsideTheBundle is the same attack in
// its realistic delivery form, and the reason the gate exists at all. tar
// restores a symlink target verbatim, including an absolute one, so an
// attacker's archive can ship repo/pool/j/jq/jq_*.deb -> /var/tmp/.cache/blob
// and the operator's own tar will faithfully create it.
//
// The existing dangling/directory symlink cases in tamper_test.go fail today
// for incidental reasons (the open fails, or the digest cannot be taken). This
// one cannot: the target is a readable regular file holding exactly the listed
// bytes, so nothing below the walk can tell the difference.
//
// Creating a symlink needs a privilege Windows does not grant by default, so
// this skips rather than fails there. It is deliberately not the only cover
// for the entry-type gate - the hard-link tests above run everywhere.
func TestVerify_SymlinkToIdenticalContentOutsideTheBundle(t *testing.T) {
	if !symlinksAvailable(t) {
		t.Skip("this environment does not permit creating symlinks; skipping")
	}

	const rel = "repo/pool/d/demo/demo_1.0-1_amd64.deb"
	f := buildFixture(t)
	inside := filepath.Join(f.dir, filepath.FromSlash(rel))
	data, err := os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	// The decoy sits in the fixture's own temp tree, so an escape is
	// contained and observable rather than aimed at the real filesystem.
	outside := filepath.Join(filepath.Dir(f.dir), "swappable-blob")
	if err := os.WriteFile(outside, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemFileNotRegular)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemFileNotRegular, rel) {
		t.Fatalf("expected %s naming %s; problems=%v", ProblemFileNotRegular, rel, problemKinds(r))
	}
}

// TestVerify_SymlinkedBundleRootIsResolved covers the disagreement between the
// two ways runVerify looked at its own root: os.Stat follows a symlink, and
// filepath.WalkDir does not. Handed a symlink to a good bundle, the old code
// passed the "is a directory" test and then walked the link itself, finding a
// single non-directory entry and no bundle at all. Resolving the root once,
// up front, makes every later step agree about which tree is being checked.
//
// An operator reaching a bundle through a symlinked mount point is ordinary;
// the refusal above is about links INSIDE the bundle, not about the name the
// operator used to get to it.
func TestVerify_SymlinkedBundleRootIsResolved(t *testing.T) {
	if !symlinksAvailable(t) {
		t.Skip("this environment does not permit creating symlinks; skipping")
	}

	f := buildFixture(t)
	alias := filepath.Join(filepath.Dir(f.dir), "bundle-alias")
	if err := os.Symlink(f.dir, alias); err != nil {
		t.Fatal(err)
	}

	r, err := New().Verify(context.Background(), alias, Options{Keys: sign.KeySource{Files: []string{f.pubPath}}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK {
		t.Fatalf("a good bundle reached through a symlinked root must verify: problems=%v", problemKinds(r))
	}
	if r.FilesChecked == 0 {
		t.Fatal("FilesChecked = 0: the walk never entered the bundle")
	}
}
