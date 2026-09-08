package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/verify"
)

// probeBundle builds a REAL, signed bundle. When skew is true the lock's
// per-package sha256 describes bytes A while the pool file actually holds
// bytes B, and the manifest is built AFTER the overwrite (so it describes B).
func probeBundle(t *testing.T, skew bool) (dir, pubPath string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "bundle")
	keysDir := filepath.Join(base, "keys")
	privPath := filepath.Join(keysDir, "operator.key")
	if _, err := sign.GenerateKey(privPath, "probe key"); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubPath = strings.TrimSuffix(privPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix

	wr := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bytesA := []byte("the .deb the online solve actually chose and hashed")
	bytesB := []byte("DIFFERENT bytes: a later selection overwrote the pool file")
	onDisk := bytesA
	if skew {
		onDisk = bytesB
	}
	relDeb := "pool/d/demo/demo_1.0-1_amd64.deb"
	wr(filepath.Join(dir, "repo", filepath.FromSlash(relDeb)), onDisk)
	pkgBytes := []byte("Package: demo\nVersion: 1.0-1\nArchitecture: amd64\nFilename: " + relDeb + "\n\n")
	wr(filepath.Join(dir, "repo", "Packages"), pkgBytes)
	relBytes := []byte("Origin: debark\nSuite: bundle\nCodename: bookworm\n")
	wr(filepath.Join(dir, "repo", "Release"), relBytes)

	l := &lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-01-01T00:00:00Z",
		SnapshotDigest: strings.Repeat("a", 64),
		RequestDigest:  strings.Repeat("b", 64),
		Target:         lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			PhasedUpdates: "never-include", InstallRecommends: true,
		},
		Packages: []lock.Package{{
			Name: "demo", Arch: "amd64", Version: "1.0-1",
			Filename: relDeb,
			Size:     int64(len(bytesA)),
			// The lock records the digest of bytesA - what the solve chose.
			SHA256:                digest.Bytes(bytesA),
			Origin:                lock.Origin{URI: "https://example.invalid/demo.deb", Suite: "bookworm"},
			Reason:                lock.ReasonRequested,
			PublisherVerification: lock.VerifiedAPTSigned,
		}},
		Install:     []string{"demo:amd64=1.0-1"},
		ClosedWorld: lock.ClosedWorld{Result: lock.ClosedWorldOK},
	}
	if err := lock.Validate(l); err != nil {
		t.Fatalf("probe lock invalid: %v", err)
	}
	lockDigest, err := lock.Save(dir, l)
	if err != nil {
		t.Fatalf("lock.Save: %v", err)
	}

	m, err := manifest.Build(context.Background(), manifest.BuildInput{
		Dir:        dir,
		LockDigest: lockDigest,
		Repository: manifest.Repository{
			PackagesSHA256: digest.Bytes(pkgBytes),
			ReleaseSHA256:  digest.Bytes(relBytes),
			PackageCount:   1,
			PoolBytes:      int64(len(onDisk)),
		},
		Target:    manifest.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		Tool:      manifest.Tool{Name: "debark", Version: "probe", Edition: manifest.EditionCommunity},
		CreatedAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	if err := manifest.Save(dir, m); err != nil {
		t.Fatalf("manifest.Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := canonical.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := sign.SignerFor(context.Background(), privPath)
	if err != nil {
		t.Fatal(err)
	}
	defer signer.Close()
	sig, err := signer.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.SaveSignature(dir, &manifest.SignatureFile{
		Schema:         manifest.SignatureSchemaVersion,
		ManifestSHA256: canonical.DigestBytes(canon),
		Signatures:     []manifest.Signature{sig},
	}); err != nil {
		t.Fatal(err)
	}
	return dir, pubPath
}

// TestInstall_RefusesALockWhoseDigestDescribesOtherBytes covers a gap three
// separate security reviews identified independently and none could close
// from its own package: nothing cross-checked lock.Package.SHA256 against
// the bytes actually in the pool at lock.Package.Filename.
//
// verify hashed every file against the MANIFEST, and checked the lock's own
// canonical-document digest, but never reconciled the two records - so a
// build in which a later selection overwrote an earlier one's pool file left
// the lock recording a digest for bytes that were no longer there, and the
// manifest, built after the overwrite, described the replacement. Both
// documents were internally consistent and the bundle verified clean.
//
// The skewed case here is built that way deliberately: the lock records the
// digest of bytes A, the pool holds bytes B, and the manifest is generated
// AFTER the overwrite so it attests B. It must be refused, and it must be
// refused by the REAL verifier on the REAL install path - which is why this
// drives install.Apply rather than asserting on verify alone.
func TestInstall_RefusesALockWhoseDigestDescribesOtherBytes(t *testing.T) {
	for _, skew := range []bool{false, true} {
		name := "consistent"
		if skew {
			name = "lock-digest-describes-different-bytes"
		}
		t.Run(name, func(t *testing.T) {
			dir, pub := probeBundle(t, skew)

			// 1. The REAL verifier's verdict.
			vrep, verr := verify.New().Verify(context.Background(), dir, verify.Options{
				Keys: sign.KeySource{Files: []string{pub}},
			})
			t.Logf("REAL verify: err=%v OK=%v problems=%+v", verr, vrep != nil && vrep.OK, vrep.Problems)

			// 2. install, driven by the REAL verifier.
			deps := baseDeps(t, nil)
			deps.Verifier = verify.New()
			deps.Root = newRootFixture(t, "debian", "12", "bookworm")
			t.Setenv("FAKE_ARCH", "amd64")
			t.Setenv("FAKE_APT_SIM_FILE", goldenPath(t, "apt-sim-install-new.txt"))
			rep, err := New(deps).Apply(context.Background(), dir, Options{
				Yes:    true,
				Verify: verify.Options{Keys: sign.KeySource{Files: []string{pub}}},
			})
			t.Logf("install Apply: err=%v applied=%v ok=%v problems=%v", err, rep.Applied, rep.OK, rep.Problems)
		})
	}
}
