package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
)

// fixtureSeq makes each buildFixture call produce genuinely different
// content (via RequestDigest below), so two fixtures are never accidentally
// byte-identical - a swapped-in signature block from a distinct bundle must
// actually be over different canonical bytes for that tamper case to mean
// anything.
var fixtureSeq int64

func nextFixtureDigest() string {
	n := atomic.AddInt64(&fixtureSeq, 1)
	return fmt.Sprintf("%064x", n)
}

// This file builds its bundle fixtures directly against core/manifest,
// core/lock and core/sign - all owned by this same area - rather than
// waiting on core/bundle's Assemble (a different, concurrently developed
// package). That also means every fixture here is a real, working product of
// this package's own Build/Save/Sign pipeline, not a hand-typed JSON blob.

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type fixture struct {
	dir          string // bundle directory
	privPath     string // operator private key, deliberately outside dir
	pubPath      string // operator public key, deliberately outside dir
	attackerPriv string // a second, never-trusted key
}

// buildFixture assembles a small, fully self-consistent, signed bundle: a
// lock, a two-file repo/ tree, a snapshot.json, a manifest built from all of
// it, and a detached signature - using nothing but this package's own code.
func buildFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "bundle")
	keysDir := filepath.Join(base, "keys")

	privPath := filepath.Join(keysDir, "operator.key")
	if _, err := sign.GenerateKey(privPath, "test key, do not use"); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	attackerPriv := filepath.Join(keysDir, "attacker.key")
	if _, err := sign.GenerateKey(attackerPriv, "attacker key, do not use"); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubPath := strings.TrimSuffix(privPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix

	debBytes := []byte("fake deb contents for testing, not a real archive")
	writeFile(t, filepath.Join(dir, "repo", "pool", "d", "demo", "demo_1.0-1_amd64.deb"), debBytes)
	pkgBytes := []byte("Package: demo\nVersion: 1.0-1\nArchitecture: amd64\nFilename: pool/d/demo/demo_1.0-1_amd64.deb\n\n")
	writeFile(t, filepath.Join(dir, "repo", "Packages"), pkgBytes)
	relBytes := []byte("Origin: debark\nLabel: debark bundle\nSuite: bundle\nCodename: bookworm\n")
	writeFile(t, filepath.Join(dir, "repo", "Release"), relBytes)
	snapBytes := []byte("{\"schema_version\":\"debark.snapshot/v1\",\"note\":\"test fixture, not a real snapshot\"}\n")
	writeFile(t, filepath.Join(dir, "snapshot.json"), snapBytes)

	l := &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-01-01T00:00:00Z",
		SnapshotDigest: strings.Repeat("a", 64),
		RequestDigest:  nextFixtureDigest(),
		Target:         lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: "never-include", InstallRecommends: true,
		},
		Packages: []lock.Package{{
			Name: "demo", Arch: "amd64", Version: "1.0-1",
			Filename: "pool/d/demo/demo_1.0-1_amd64.deb",
			Size:     int64(len(debBytes)), SHA256: digest.Bytes(debBytes),
			Origin:                lock.Origin{URI: "file:///dev/null", Suite: "bookworm"},
			Reason:                lock.ReasonRequested,
			PublisherVerification: lock.VerifiedAPTSigned,
		}},
		Install:     []string{"demo:amd64=1.0-1"},
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldOK},
	}
	if err := lock.Validate(l); err != nil {
		t.Fatalf("fixture lock invalid: %v", err)
	}
	lockDigest, err := lock.Save(dir, l)
	if err != nil {
		t.Fatalf("lock.Save: %v", err)
	}

	snapDigest, err := canonicalDigestOfFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		t.Fatalf("digest snapshot.json: %v", err)
	}

	in := manifest.BuildInput{
		Dir:            dir,
		SnapshotDigest: snapDigest,
		LockDigest:     lockDigest,
		Repository: manifest.Repository{
			PackagesSHA256: digest.Bytes(pkgBytes),
			ReleaseSHA256:  digest.Bytes(relBytes),
			PackageCount:   1,
			PoolBytes:      int64(len(debBytes)),
		},
		Target:    manifest.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Tool:      manifest.Tool{Name: "debark", Version: "test", Edition: manifest.EditionCommunity},
		CreatedAt: "2026-01-01T00:00:00Z",
	}
	m, err := manifest.Build(context.Background(), in)
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	if err := manifest.Save(dir, m); err != nil {
		t.Fatalf("manifest.Save: %v", err)
	}

	f := &fixture{dir: dir, privPath: privPath, pubPath: pubPath, attackerPriv: attackerPriv}
	resign(t, f, f.privPath)
	return f
}

// resign (re)signs whatever manifest.json currently holds in f.dir with the
// named private key. Tamper cases that target steps 4-7 (the manifest
// document's internal consistency, or its agreement with lock.json/
// snapshot.json) use this to keep steps 1-3 passing, so the specific deeper
// check under test is the only thing that can fail.
//
// It signs the RAW bytes on disk, canonicalised, rather than going through
// manifest.Load. Two reasons, and both matter for a tamper suite:
//
//   - A signature is over bytes. Reading the document into a struct and
//     signing that would sign whatever survived the round trip, which is the
//     one thing these tests exist to catch.
//   - core/manifest.Load validates the document (paths cleaned and unique,
//     digests lowercase hex) and refuses what it does not like. An attacker
//     is under no obligation to use manifest.Save, so the suite must be able
//     to produce a signed document the loader will reject - that is exactly
//     the input verify has to survive - and it could not if signing required
//     the loader to accept it first.
func resign(t *testing.T, f *fixture, privPath string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, manifest.FileName))
	if err != nil {
		t.Fatalf("read %s: %v", manifest.FileName, err)
	}
	canon, err := canonical.Transform(raw)
	if err != nil {
		t.Fatalf("canonical.Transform: %v", err)
	}
	signer, err := sign.SignerFor(context.Background(), privPath)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer signer.Close()
	sig, err := signer.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sf := &manifest.SignatureFile{
		Schema:         manifest.SignatureSchemaVersion,
		ManifestSHA256: canonical.DigestBytes(canon),
		Signatures:     []manifest.Signature{sig},
	}
	if err := manifest.SaveSignature(f.dir, sf); err != nil {
		t.Fatalf("SaveSignature: %v", err)
	}
}

func verifyFixture(t *testing.T, f *fixture, opts Options) (*Report, error) {
	t.Helper()
	if opts.Keys.Files == nil && opts.Keys.Dirs == nil && !opts.AllowUnsigned {
		opts.Keys = sign.KeySource{Files: []string{f.pubPath}}
	}
	return New().Verify(context.Background(), f.dir, opts)
}

func wantProblem(t *testing.T, report *Report, err error, kind string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Verify returned an error instead of a report: %v", err)
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false (a tampered bundle must fail)")
	}
	for _, p := range report.Problems {
		if p.Kind == kind {
			return
		}
	}
	t.Fatalf("problems = %+v, want one with kind %q", report.Problems, kind)
}

func TestVerify_HappyPath(t *testing.T) {
	f := buildFixture(t)
	report, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("report not OK: problems=%+v", report.Problems)
	}
	if !report.Signed {
		t.Fatalf("report.Signed = false, want true")
	}
	if len(report.Signatures) != 1 || !report.Signatures[0].Valid || !report.Signatures[0].Trusted {
		t.Fatalf("unexpected signatures: %+v", report.Signatures)
	}
	if report.FilesChecked == 0 {
		t.Fatalf("FilesChecked = 0, want > 0")
	}
	if report.BundleID == "" {
		t.Fatalf("BundleID is empty")
	}
}

func TestVerify_AllowUnsigned(t *testing.T) {
	f := buildFixture(t)
	if err := os.Remove(filepath.Join(f.dir, manifest.SigFileName)); err != nil {
		t.Fatal(err)
	}

	report, err := verifyFixture(t, f, Options{AllowUnsigned: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("report not OK: problems=%+v", report.Problems)
	}
	if report.Signed {
		t.Fatalf("report.Signed = true, want false")
	}
	if len(report.Warnings) == 0 {
		t.Fatalf("expected a warning when AllowUnsigned accepted an unsigned bundle")
	}

	// The same bundle, without AllowUnsigned, must fail closed: AllowUnsigned
	// is never the default.
	report2, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantProblem(t, report2, err, ProblemSignatureMissing)
}

// TestVerify_SkipFileDigests checks the precise, narrow scope of the flag: it
// skips hashing the *contents* of large pool/*.deb files only. It must never
// weaken the check for anything else - file presence and size are always
// checked, and small files (lock.json, snapshot.json, the repo/ index files)
// are always fully digested, because that is the only thing standing between
// a tampered lock.json and a passing report in this mode.
func TestVerify_SkipFileDigests(t *testing.T) {
	f := buildFixture(t)
	p := filepath.Join(f.dir, "repo", "pool", "d", "demo", "demo_1.0-1_amd64.deb")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte(nil), b...)
	b[0] ^= 0xFF // same-size content change: only content hashing may hide this
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := verifyFixture(t, f, Options{SkipFileDigests: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("SkipFileDigests should not hash pool file contents, got problems=%+v", report.Problems)
	}
	if report.FilesChecked == 0 {
		t.Fatalf("FilesChecked = 0, want > 0 (presence and size are still checked)")
	}

	// A same-size change to the .deb's SIZE must still be caught (cheap: a
	// stat, not a hash).
	fSize := buildFixture(t)
	pSize := filepath.Join(fSize.dir, "repo", "pool", "d", "demo", "demo_1.0-1_amd64.deb")
	orig, err := os.ReadFile(pSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pSize, orig[:len(orig)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	reportSize, err := verifyFixture(t, fSize, Options{SkipFileDigests: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantProblem(t, reportSize, err, ProblemFileSize)

	// A same-size change to lock.json - a small file - must still be caught:
	// SkipFileDigests must never extend to anything but repo/pool/.
	fLock := buildFixture(t)
	lockPath := filepath.Join(fLock.dir, "lock.json")
	lb, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lb = append([]byte(nil), lb...)
	lb[len(lb)/2] ^= 0xFF
	if err := os.WriteFile(lockPath, lb, 0o644); err != nil {
		t.Fatal(err)
	}
	reportLock, err := verifyFixture(t, fLock, Options{SkipFileDigests: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if reportLock.OK {
		t.Fatal("SkipFileDigests must still catch a tampered lock.json; it is not a pool file")
	}
}

func TestVerify_SameMediaKey(t *testing.T) {
	f := buildFixture(t)
	insideCopy := filepath.Join(f.dir, "operator.pub")
	src, err := os.ReadFile(f.pubPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, insideCopy, src)

	report, err := New().Verify(context.Background(), f.dir, Options{Keys: sign.KeySource{Files: []string{insideCopy}}})
	wantProblem(t, report, err, ProblemSameMediaKey)

	// AllowSameMedia is the documented, explicit override: the same-media
	// refusal specifically must be gone, and the signature (genuinely made
	// with this key) must verify as trusted. The copied key file is itself
	// now an untracked extra file in the bundle - a separate, correct finding
	// (file-unexpected) - which is why this checks for the absence of
	// ProblemSameMediaKey rather than report2.OK as a whole.
	report2, err := New().Verify(context.Background(), f.dir, Options{
		Keys: sign.KeySource{Files: []string{insideCopy}, AllowSameMedia: true},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, p := range report2.Problems {
		if p.Kind == ProblemSameMediaKey {
			t.Fatalf("AllowSameMedia did not lift the same-media refusal: %+v", report2.Problems)
		}
	}
	if !report2.Signed {
		t.Fatalf("report2.Signed = false, want true (signature is valid once the same-media key is trusted)")
	}
}

func TestReport_JSONSchemaStable(t *testing.T) {
	f := buildFixture(t)
	report, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var round Report
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if round.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version did not round-trip: got %q", round.SchemaVersion)
	}
	if !strings.Contains(string(b), `"schema_version"`) {
		t.Fatalf("schema_version missing from JSON output: %s", b)
	}
}

// TestVerify_TamperMatrix is this package's headline test: each case mutates one
// thing about an otherwise-valid, signed bundle and asserts the exact
// Problem.Kind verify reports, and that verification fails - every one of
// them before anything could hand the repository to apt (this package never
// calls apt at all).
func TestVerify_TamperMatrix(t *testing.T) {
	debPath := func(f *fixture) string {
		return filepath.Join(f.dir, "repo", "pool", "d", "demo", "demo_1.0-1_amd64.deb")
	}

	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		want   string
		// deep says which half of the check list this row is about, and it
		// is asserted rather than described. A row that claims a steps-4-to-7
		// finding proves nothing if the run in fact aborted at step 3 - the
		// report would carry a problem, OK would be false, and the assertion
		// on the kind would still pass, all without the check under test
		// having run at all. FilesChecked is the evidence either way: nonzero
		// means the walk happened, zero means the refusal came before the
		// bundle's contents were touched.
		deep bool
	}{
		{
			name: "modified .deb",
			mutate: func(t *testing.T, f *fixture) {
				b, err := os.ReadFile(debPath(f))
				if err != nil {
					t.Fatal(err)
				}
				b = append([]byte(nil), b...)
				b[0] ^= 0xFF // flip a byte; length is unchanged
				if err := os.WriteFile(debPath(f), b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemFileDigest,
			deep: true,
		},
		{
			name: "added file",
			mutate: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, "repo", "pool", "d", "demo", "extra.deb"), []byte("not in the manifest"))
			},
			want: ProblemFileUnexpected,
			deep: true,
		},
		{
			name: "removed file",
			mutate: func(t *testing.T, f *fixture) {
				if err := os.Remove(debPath(f)); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemFileMissing,
			deep: true,
		},
		{
			// The name says what this row can and cannot prove. Editing the
			// manifest without refreshing the signature file stops at step
			// 2's plaintext manifest_sha256 comparison - which is a
			// comparison between two documents the attacker controls
			// completely, so it demonstrates nothing about the signature
			// maths. The row that does is
			// TestVerify_SignatureAbuse's "manifest edited with
			// manifest_sha256 refreshed to match", which refreshes the field
			// an attacker would obviously refresh and forces the ed25519
			// check to be the thing that refuses. Both are needed: this one
			// pins that the cheap pre-check happens FIRST, evidenced by
			// FilesChecked staying zero.
			name: "edited manifest field, signature not refreshed",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.Target.Arch = "riscv64" // changed post-signing; signature is NOT refreshed
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemManifestDigest,
		},
		{
			name: "truncated file",
			mutate: func(t *testing.T, f *fixture) {
				b, err := os.ReadFile(debPath(f))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(debPath(f), b[:len(b)/2], 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemFileSize,
			deep: true,
		},
		{
			// Splice a whole signature block from a second, different bundle
			// (re-signed with THIS fixture's operator key, so the block names
			// a key our verifier trusts) into this bundle's sig file, keeping
			// this bundle's own correct manifest_sha256. The key is trusted;
			// the signature bytes are simply over the wrong manifest.
			name: "swapped signature block from another bundle",
			mutate: func(t *testing.T, f *fixture) {
				other := buildFixture(t)
				resign(t, other, f.privPath)
				otherSig, err := manifest.LoadSignature(other.dir)
				if err != nil {
					t.Fatal(err)
				}
				sigFile, err := manifest.LoadSignature(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				sigFile.Signatures = otherSig.Signatures
				if err := manifest.SaveSignature(f.dir, sigFile); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemSignatureInvalid,
		},
		{
			name: "signature by an untrusted key",
			mutate: func(t *testing.T, f *fixture) {
				resign(t, f, f.attackerPriv) // validly signed, but by a key the verifier is never told to trust
			},
			want: ProblemSignatureUntrusted,
		},
		{
			name: "corrupted Packages (manifest internally inconsistent)",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.Repository.PackagesSHA256 = strings.Repeat("0", 64)
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
				resign(t, f, f.privPath)
			},
			want: ProblemRepoDigest,
			deep: true,
		},
		{
			name: "corrupted Release (manifest internally inconsistent)",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.Repository.ReleaseSHA256 = strings.Repeat("0", 64)
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
				resign(t, f, f.privPath)
			},
			want: ProblemRepoDigest,
			deep: true,
		},
		{
			name: "lock digest mismatch",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.LockDigest = strings.Repeat("1", 64)
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
				resign(t, f, f.privPath)
			},
			want: ProblemLockDigest,
			deep: true,
		},
		{
			name: "snapshot digest mismatch",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.SnapshotDigest = strings.Repeat("2", 64)
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
				resign(t, f, f.privPath)
			},
			want: ProblemSnapshotDigest,
			deep: true,
		},
		{
			// Handled by TestVerify_SameMediaKey in full (including the
			// AllowSameMedia override); included here too so the matrix
			// itself is a complete checklist against the task's list.
			name: "key supplied from inside the bundle",
			mutate: func(t *testing.T, f *fixture) {
				// no-op: verifyFixtureSameMedia below points opts.Keys at a
				// copy of the key placed inside f.dir.
			},
			want: ProblemSameMediaKey,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			tc.mutate(t, f)

			opts := Options{}
			if tc.want == ProblemSameMediaKey {
				insideCopy := filepath.Join(f.dir, "operator.pub")
				src, err := os.ReadFile(f.pubPath)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, insideCopy, src)
				opts.Keys = sign.KeySource{Files: []string{insideCopy}}
			}

			report, err := verifyFixture(t, f, opts)
			wantProblem(t, report, err, tc.want)

			// Two assertions this matrix spent its first revisions without,
			// and the omission was structural: every row asserted only that a
			// problem of the wanted kind was PRESENT, which a row satisfies
			// just as well by failing for a reason it was not written to test.
			// Several rows re-sign the manifest precisely so the shallow
			// checks pass and the deep one under test is the only thing that
			// can fail - an arrangement that would silently stop being true
			// the moment anything started refusing earlier.
			wantOnlyKinds(t, report, tc.want)
			if tc.deep {
				wantReachedContentChecks(t, report)
			} else if report.FilesChecked != 0 || report.BytesChecked != 0 {
				t.Fatalf("this row is refused at steps 1-3, but %d file(s)/%d byte(s) of the bundle were read first",
					report.FilesChecked, report.BytesChecked)
			}
		})
	}
}
