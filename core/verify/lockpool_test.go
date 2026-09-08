package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
)

// This file covers step 7's second half: the lock's own per-package digests,
// cross-checked against the signed manifest.
//
// It exists because of a hole no other test in this package could see. Every
// check here used to be one of two shapes - "the manifest agrees with the
// disk" (checkFiles) or "lock.json is the document the manifest signed"
// (checkLockAndSnapshot) - and neither of them looks at what the lock
// actually SAYS. A lock is not made true by being signed. The digests inside
// it were nobody's business.
//
// The measured consequence, reproduced by
// TestVerify_LockDigestNoLongerMatchesThePoolFile below: a build in which a
// later selection overwrote an earlier one's pool file left the lock's
// recorded sha256 describing bytes that were no longer in the bundle, and
// verify passed - full file count, valid signature, no warning - because the
// manifest was built AFTER the overwrite and so agreed with the disk
// perfectly.

// resealBundle rebuilds the manifest from whatever is on disk right now and
// re-signs it, exactly as the builder does at the end of an assembly: file
// digests, the repository summary and the lock/snapshot binding digests are
// all recomputed from the current bytes.
//
// That is what makes every case in this file mean something. After a reseal
// the bundle is internally consistent in every way verify already knew how to
// check - steps 1 to 6 pass, and so does checkLockAndSnapshot - so the only
// thing left that can refuse it is the new cross-check, and a case that fails
// here cannot be failing for an incidental reason.
func resealBundle(t *testing.T, f *fixture) {
	t.Helper()
	m, _, err := manifest.Load(f.dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	lockDigest, err := canonicalDigestOfFile(filepath.Join(f.dir, lock.FileName))
	if err != nil {
		t.Fatalf("digest %s: %v", lock.FileName, err)
	}
	snapDigest, err := canonicalDigestOfFile(filepath.Join(f.dir, snapshotDocumentName))
	if err != nil {
		t.Fatalf("digest %s: %v", snapshotDocumentName, err)
	}
	repo := m.Repository
	repo.PackagesSHA256 = hashBundleFile(t, f, "repo/Packages")
	repo.ReleaseSHA256 = hashBundleFile(t, f, "repo/Release")

	nm, err := manifest.Build(context.Background(), manifest.BuildInput{
		Dir:            f.dir,
		SnapshotDigest: snapDigest,
		LockDigest:     lockDigest,
		Repository:     repo,
		Target:         m.Target,
		Tool:           m.Tool,
		CreatedAt:      m.CreatedAt,
	})
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	if err := manifest.Save(f.dir, nm); err != nil {
		t.Fatalf("manifest.Save: %v", err)
	}
	resign(t, f, f.privPath)
}

func hashBundleFile(t *testing.T, f *fixture, rel string) string {
	t.Helper()
	sum, _, err := digest.SHA256File(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("hash %s: %v", rel, err)
	}
	return sum
}

// lockPoolFile returns the fixture's one pool entry and its bundle-relative
// path, so a case does not have to restate the fixture's layout.
func lockPoolFile(t *testing.T, f *fixture) (lock.Package, string) {
	t.Helper()
	l, err := lock.Load(f.dir)
	if err != nil {
		t.Fatalf("lock.Load: %v", err)
	}
	if len(l.Packages) != 1 {
		t.Fatalf("fixture lock has %d packages, want 1", len(l.Packages))
	}
	return l.Packages[0], lockRepoPrefix + l.Packages[0].Filename
}

// writeRawLock replaces lock.json with hand-edited bytes. It exists because
// lock.Save refuses to write anything Load will refuse to read (an unknown
// schema version, an unsafe filename), and several cases here need exactly
// such a document: an attacker is under no obligation to use lock.Save, and
// verify has to survive - and refuse - what it is handed.
func writeRawLock(t *testing.T, f *fixture, old, new string) {
	t.Helper()
	p := filepath.Join(f.dir, lock.FileName)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", lock.FileName, err)
	}
	out := strings.Replace(string(raw), old, new, 1)
	if out == string(raw) {
		t.Fatalf("writeRawLock found nothing matching %q; the case would prove nothing", old)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatalf("write %s: %v", lock.FileName, err)
	}
}

// TestVerify_LockDigestNoLongerMatchesThePoolFile is the executable form of
// the gap, built the way the build itself produced it: the pool file is
// overwritten, and then the whole bundle is resealed - manifest rebuilt from
// the new bytes, signature refreshed. Everything the old checker knew how to
// ask is satisfied. Only lock.json still describes the file that used to be
// there.
//
// This must be a refusal. The lock is the plan an auditor reads and the
// document core/install acts on (ADR-007); a plan whose digests describe bytes
// that are not on the medium is not a plan anyone can check, and the evidence
// trail an air-gapped operator signs off - "these are the exact files, by
// digest, that crossed the gap" - is precisely what stops being true here.
func TestVerify_LockDigestNoLongerMatchesThePoolFile(t *testing.T) {
	f := buildFixture(t)
	before, rel := lockPoolFile(t, f)

	// A later selection writes over the same pool path.
	replacement := []byte("a different .deb entirely, written by a later selection")
	if err := os.WriteFile(filepath.Join(f.dir, filepath.FromSlash(rel)), replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if digest.Bytes(replacement) == before.SHA256 {
		t.Fatal("the replacement has the original digest; the case would prove nothing")
	}
	resealBundle(t, f)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemLockDigest)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemLockDigest, rel) {
		t.Fatalf("expected %s naming the pool file %s; problems=%v", ProblemLockDigest, rel, problemKinds(r))
	}
	// The refusal must be the cross-check and nothing else: no file digest
	// mismatch (the manifest was rebuilt from the new bytes), no lock BINDING
	// digest mismatch (lock.json itself was never edited), and the walk must
	// have run to completion rather than the run aborting early.
	wantOnlyKinds(t, r, ProblemLockDigest)
	if hasProblem(r, ProblemLockDigest, lock.FileName) {
		t.Fatalf("lock.json's own binding digest was reported wrong; it was never edited: problems=%v", problemKinds(r))
	}
	wantReachedContentChecks(t, r)

	// The report has to carry both digests, or an operator cannot tell which
	// of the two documents is the one that moved.
	var reported bool
	for _, p := range r.Problems {
		if p.Kind == ProblemLockDigest && p.Path == rel {
			reported = p.Got == before.SHA256 && p.Expected == digest.Bytes(replacement)
		}
	}
	if !reported {
		t.Fatalf("the problem must report the lock's claim as got and the manifest's digest as expected; problems=%+v", r.Problems)
	}
}

// TestVerify_LockPackageCheckDoesNotDependOnHashing runs the same tamper under
// SkipFileDigests, the one mode in which a pool file's bytes are never read.
//
// It pins the design decision behind the check: it compares the lock against
// the MANIFEST, not against the disk, so it costs nothing and cannot be
// switched off by the flag that exists to make very large bundles cheap to
// inspect. A lock-to-disk implementation would have gone quiet here - which is
// exactly the bundle size at which a stale digest is least likely to be
// noticed any other way.
//
// The replacement is the same LENGTH as the original, so the size check
// SkipFileDigests leaves in place cannot be what fires either.
func TestVerify_LockPackageCheckDoesNotDependOnHashing(t *testing.T) {
	f := buildFixture(t)
	_, rel := lockPoolFile(t, f)
	full := filepath.Join(f.dir, filepath.FromSlash(rel))
	orig, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("X", len(orig))), 0o644); err != nil {
		t.Fatal(err)
	}
	resealBundle(t, f)

	r, verr := verifyFixture(t, f, Options{SkipFileDigests: true})
	wantRefusedWith(t, r, verr, ProblemLockDigest)
	wantOnlyKinds(t, r, ProblemLockDigest)
	if !hasProblem(r, ProblemLockDigest, rel) {
		t.Fatalf("expected %s naming %s even with SkipFileDigests; problems=%v", ProblemLockDigest, rel, problemKinds(r))
	}
}

// TestVerify_LockNamesAFileTheManifestDoesNotAttest is the other direction:
// the lock's digest is not wrong, it is unanswerable. The lock plans to
// install a .deb at a pool path the signed manifest says nothing about, so
// there is no attested file to compare it against.
//
// Fail closed. "I could not check this" must never be reported the same way as
// "I checked this and it was fine" - the same distinction
// TestVerify_UnverifiableSignatureIsNeverAPass draws for signatures, applied
// to the plan. The filename used here is one lock.Save itself accepts (clean,
// relative, pool-shaped), so this is a document debark could have written;
// it is simply not true of this bundle.
func TestVerify_LockNamesAFileTheManifestDoesNotAttest(t *testing.T) {
	f := buildFixture(t)
	_, oldRel := lockPoolFile(t, f)

	l, err := lock.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Packages[0].Filename = "pool/d/demo/demo_9.9-9_amd64.deb"
	if _, err := lock.Save(f.dir, l); err != nil {
		t.Fatalf("lock.Save: %v", err)
	}
	resealBundle(t, f)

	wantRel := lockRepoPrefix + l.Packages[0].Filename
	r, verr := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, verr, ProblemLockDigest)
	wantOnlyKinds(t, r, ProblemLockDigest)
	if !hasProblem(r, ProblemLockDigest, wantRel) {
		t.Fatalf("expected %s naming the unattested path %s; problems=%v", ProblemLockDigest, wantRel, problemKinds(r))
	}
	// The real .deb is still there and still attested, so nothing may be
	// reported about it: this finding is about the lock, not about the medium.
	if hasProblem(r, ProblemFileMissing, oldRel) || hasProblem(r, ProblemFileUnexpected, oldRel) {
		t.Fatalf("the untouched pool file %s was reported as a file problem; problems=%v", oldRel, problemKinds(r))
	}
}

// TestVerify_LockThisBuildCannotReadIsNotCertified covers the branch that
// makes the cross-check honest rather than best-effort.
//
// The lock here is byte-for-byte the document the manifest signed - its
// canonical digest matches lock_digest exactly, so checkLockAndSnapshot is
// perfectly happy - but core/lock refuses to decode it, so verify cannot see
// the packages inside. That is the shape a bundle from a newer or forked
// debark takes, and it is the one case where silence would be indefensible:
// the report would say ok, having never looked at the plan at all.
//
// It is reported under lock.json rather than under a pool path, because the
// operator's next action is about the document, not about a file.
func TestVerify_LockThisBuildCannotReadIsNotCertified(t *testing.T) {
	f := buildFixture(t)
	writeRawLock(t, f, `"`+lock.SchemaVersion+`"`, `"debark.lock/v99"`)
	resealBundle(t, f)

	// Precondition: this lock IS the signed one. If the binding digest were
	// wrong the case would prove nothing, because checkLockAndSnapshot would
	// refuse it first for an entirely different reason.
	m, _, err := manifest.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalDigestOfFile(filepath.Join(f.dir, lock.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !digest.Equal(got, m.LockDigest) {
		t.Fatal("precondition failed: the manifest's lock_digest does not describe this lock.json")
	}

	r, verr := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, verr, ProblemLockDigest)
	wantOnlyKinds(t, r, ProblemLockDigest)
	if !hasProblem(r, ProblemLockDigest, lock.FileName) {
		t.Fatalf("expected %s naming %s; problems=%v", ProblemLockDigest, lock.FileName, problemKinds(r))
	}
	var explained bool
	for _, p := range r.Problems {
		if p.Kind == ProblemLockDigest && strings.Contains(p.Message, "could not be checked") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("the refusal must say the lock could not be read, not merely that a digest is wrong; problems=%+v", r.Problems)
	}
}

// TestVerify_HostileLockFilenameIsNeverResolved fires the path abuse at the
// lock that TestVerify_ManifestPathAbuse fires at the manifest, for the same
// reason: a lock filename is attacker-supplied text, and the one thing that
// must never happen is for verify to join it onto the bundle root and open
// whatever comes out.
//
// The structural guarantee is that it cannot. checkLockPackages concatenates
// the prefix and looks the result up in the index checkFiles already built -
// nothing is joined, and nothing is opened - so a traversing filename can only
// ever fail to match. Note that this is a stronger position than the manifest
// checker's, which does hash files: here there is no code path that could read
// the target at all.
//
// wantAt records WHICH layer refuses each spelling, the same way the manifest
// suite does:
//
//   - lock.json: core/lock's checkLoadSafety refuses the document at Load, so
//     verify reports that it could not read the lock.
//   - a derived pool path: a spelling core/lock permits, which reaches
//     verify's own lookup and comes out unattested under the verbatim path the
//     concatenation produced.
//
// A canary file sits in the bundle's parent (the test's own t.TempDir(), so an
// escape would be contained and observable) and no problem may ever name it.
func TestVerify_HostileLockFilenameIsNeverResolved(t *testing.T) {
	cases := []struct {
		name string
		// jsonFilename is the exact JSON string body written into lock.json,
		// already escaped: lock.Save refuses most of these outright.
		jsonFilename string
		wantAt       string
	}{
		{"parent traversal", `../canary.txt`, lock.FileName},
		{"deep traversal", `../../canary.txt`, lock.FileName},
		{"absolute path", `/etc/passwd`, lock.FileName},
		{"backslash separator", `pool\\d\\demo\\canary.txt`, lock.FileName},
		{"embedded parent component", `pool/d/../../canary.txt`, lock.FileName},
		// A NUL written as a JSON \u0000 escape: legal JSON, and a byte that
		// has no business in a pool path.
		{"nul byte", `pool/d/demo/demo_1.0-1_amd64.deb\u0000`, lock.FileName},
		{"empty filename", ``, lock.FileName},
		// Clean, relative and permitted by core/lock, so it reaches verify's
		// own lookup. This is the case that exercises checkLockPackages
		// rather than the loader.
		{"a legal pool path that is not in this bundle", `pool/c/canary/canary.txt`,
			lockRepoPrefix + "pool/c/canary/canary.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			canaryPath := filepath.Join(filepath.Dir(f.dir), "canary.txt")
			if err := os.WriteFile(canaryPath, []byte("canary: verify must never resolve a lock filename"), 0o644); err != nil {
				t.Fatal(err)
			}

			writeRawLock(t, f,
				`"pool/d/demo/demo_1.0-1_amd64.deb"`,
				`"`+tc.jsonFilename+`"`)
			resealBundle(t, f)

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, ProblemLockDigest)
			wantOnlyKnownKinds(t, r)
			if !hasProblem(r, ProblemLockDigest, tc.wantAt) {
				t.Fatalf("expected %s at %q; problems=%v", ProblemLockDigest, tc.wantAt, problemKinds(r))
			}
			// Every path verify named is either lock.json or a bundle-relative
			// path under the repository prefix. Nothing outside the bundle was
			// adopted, resolved or reported.
			for _, p := range r.Problems {
				if p.Path == lock.FileName || strings.HasPrefix(p.Path, lockRepoPrefix) {
					continue
				}
				t.Fatalf("a problem names a path that is neither the lock nor a bundle path: %+v", p)
			}
			if _, serr := os.Stat(canaryPath); serr != nil {
				t.Fatalf("the canary is gone; verify must never write outside the bundle: %v", serr)
			}
		})
	}
}

// TestVerify_LockPackageCheckPassesAGoodBundle is the negative control for
// everything above. A bundle straight out of the build pipeline must verify
// with the cross-check in place - a check that refuses honest bundles is worse
// than no check, and this one runs on every verification of every bundle,
// including one an operator built five minutes ago.
//
// It also states, as an executable assertion rather than only a comment, the
// layout invariant checkLockPackages depends on: lock.Package.Filename is
// POOL-relative, and the bundle path is lockRepoPrefix + that. If core/bundle
// ever moves the pool, this fails here with a clear reason instead of by
// silently refusing every bundle in the field.
func TestVerify_LockPackageCheckPassesAGoodBundle(t *testing.T) {
	f := buildFixture(t)
	pkg, rel := lockPoolFile(t, f)

	if !strings.HasPrefix(rel, poolPathPrefix) {
		t.Fatalf("derived pool path %q does not start with %q; the lock filename convention has moved", rel, poolPathPrefix)
	}
	if _, err := os.Stat(filepath.Join(f.dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("derived pool path %q does not name a real file: %v", rel, err)
	}
	m, _, err := manifest.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := indexFiles(m.Files)
	entry, ok := expected[rel]
	if !ok {
		t.Fatalf("the manifest does not list %s at all", rel)
	}
	if !digest.Equal(entry.SHA256, pkg.SHA256) || entry.Size != pkg.Size {
		t.Fatalf("manifest and lock disagree about %s in a freshly built bundle", rel)
	}

	r, verr := verifyFixture(t, f, Options{})
	if verr != nil {
		t.Fatalf("Verify: %v", verr)
	}
	if !r.OK {
		t.Fatalf("a freshly built bundle must verify with the lock cross-check in place: problems=%v", problemKinds(r))
	}
}
