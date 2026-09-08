package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/verify"
)

// fakeSHA256 returns a deterministic, well-formed (64 lowercase hex chars)
// digest seeded from s, so fixture lock.Package entries satisfy
// lock.Validate's digest.Valid check without needing a real file to hash.
func fakeSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fakeVerifier is the fake verify.Verifier every test in this package uses:
// install.Runner is specified and tested against the verify.Verifier
// interface, never against a real implementation (core/verify, developed
// concurrently) — exactly as the brief asks.
type fakeVerifier struct {
	report *verify.Report
	err    error

	calls          int
	lastBundlePath string
	lastOpts       verify.Options
}

func (f *fakeVerifier) Verify(ctx context.Context, bundlePath string, opts verify.Options) (*verify.Report, error) {
	f.calls++
	f.lastBundlePath = bundlePath
	f.lastOpts = opts
	return f.report, f.err
}

// okVerifyReport returns a passing verify.Report for the given lock target,
// as verify.Verify would return after successfully checking a real bundle.
func okVerifyReport(target lock.Target) *verify.Report {
	return &verify.Report{
		SchemaVersion: verify.SchemaVersion,
		OK:            true,
		Signed:        true,
		BundleID:      "test-bundle-id",
		Target: verify.ReportTarget{
			DistroID:  target.DistroID,
			VersionID: target.VersionID,
			Codename:  target.Codename,
			Arch:      target.Arch,
		},
	}
}

// fixtureOptions configures newFixtureBundle.
type fixtureOptions struct {
	// Lock is written as lock.json verbatim (SchemaVersion filled if empty).
	Lock lock.Lock
	// OmitPoolFiles leaves repo/pool empty. A real bundle always holds the
	// files its lock names, and install now refuses one that does not
	// (missingPoolFiles), so the placeholder .deb per lk.Packages entry is
	// written by default and this flag exists only for the tests that are
	// about that refusal.
	OmitPoolFiles bool
}

// newFixtureBundle builds a minimal, on-disk bundle directory: lock.json and
// an (empty, unless requested) repo/ directory — exactly what install reads
// once Verify has already said the bundle is trustworthy. It never writes a
// manifest or signature: those are verify's concern, and this package's
// tests replace verify entirely with fakeVerifier.
func newFixtureBundle(t *testing.T, opts fixtureOptions) string {
	t.Helper()
	dir := t.TempDir()

	lk := opts.Lock
	if lk.SchemaVersion == "" {
		lk.SchemaVersion = lock.SchemaVersion
	}
	if _, err := lock.Save(dir, &lk); err != nil {
		t.Fatalf("write lock.json: %v", err)
	}

	repoDir := filepath.Join(dir, repoDirName)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	if !opts.OmitPoolFiles {
		for _, p := range lk.Packages {
			path := filepath.Join(repoDir, filepath.FromSlash(p.Filename))
			if path == repoDir {
				path = filepath.Join(repoDir, "pool", p.Name+"_"+p.Version+"_"+p.Arch+".deb")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir pool dir: %v", err)
			}
			if err := os.WriteFile(path, []byte("not a real deb, fixture only"), 0o644); err != nil {
				t.Fatalf("write pool file: %v", err)
			}
		}
	}

	return dir
}

// basicLockTarget is the target identity most tests use.
func basicLockTarget() lock.Target {
	return lock.Target{
		DistroID:  "debian",
		VersionID: "12",
		Codename:  "bookworm",
		Arch:      "amd64",
	}
}

// newRootFixture builds a fake filesystem root with just enough of
// /etc/os-release for readOSRelease, for use as Deps.Root. Keeping this
// explicit (rather than relying on whatever /etc/os-release the test
// machine happens to have, or lacks entirely on Windows) is what makes the
// codename-mismatch and target-identity tests deterministic on every
// platform this suite runs on.
func newRootFixture(t *testing.T, id, versionID, codename string) string {
	t.Helper()
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	content := "ID=" + id + "\nVERSION_ID=\"" + versionID + "\"\nVERSION_CODENAME=" + codename + "\n"
	if err := os.WriteFile(filepath.Join(etcDir, "os-release"), []byte(content), 0o644); err != nil {
		t.Fatalf("write os-release: %v", err)
	}
	return root
}

// snapshotTree records every file under root with its size and content hash,
// for the "system config untouched" assertion (contract item 3): install
// must never create or modify anything outside its own temp directory.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}
