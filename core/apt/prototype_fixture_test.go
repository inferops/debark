package apt

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/snapshot"
)

// This file builds a *snapshot.Snapshot directly from the real, messy
// captured state checked in at testdata/real-targets/ (the Bash prototype's
// snapshot-target.sh format: target-state/ containing dpkg-status,
// apt/sources.list(.d), apt/preferences.d, apt/trusted.gpg.d,
// keyrings/<original absolute path>, arch, foreign-arches, codename,
// version_id). core/snapshot may not be ready, and the contract
// brief says not to wait for it or implement it: this reads the same real
// fixture snapshot.Capture would eventually read, without depending on it at
// all.

// extractPrototypeState extracts a snapshot-target.sh-format tar.gz into a
// fresh subdirectory of dir and returns the target-state/ path inside it.
// Standard-library only, so this runs identically on every platform the unit
// tests run on. The fixture's one dangling symlink (os-release, pointing at
// a path this extraction has no reason to also carry) is skipped rather than
// followed or failed on.
func extractPrototypeState(t *testing.T, tarGzPath, dir string) string {
	t.Helper()
	f, err := os.Open(tarGzPath)
	if err != nil {
		t.Fatalf("open %s: %v", tarGzPath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", tarGzPath, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar %s: %v", tarGzPath, err)
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
			}
			wf, err := os.Create(target)
			if err != nil {
				t.Fatalf("create %s: %v", target, err)
			}
			if _, err := io.Copy(wf, tr); err != nil {
				wf.Close()
				t.Fatalf("write %s: %v", target, err)
			}
			wf.Close()
		default:
			// symlinks and anything else: not needed here, skip.
		}
	}
	return filepath.Join(dir, "target-state")
}

// snapshotFromPrototypeState builds a *snapshot.Snapshot from an extracted
// snapshot-target.sh directory, writing every referenced file's bytes into
// filesDir at its ArchivePath (the same layout a real debark.snapshot/v1
// archive's files/ directory has), so the result is usable anywhere a
// *snapshot.Snapshot + files directory pair is expected.
func snapshotFromPrototypeState(t *testing.T, stateDir, filesDir string) *snapshot.Snapshot {
	t.Helper()

	readTrim := func(name string) string {
		b, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}

	addFile := func(targetPath, srcPath string) snapshot.File {
		data, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatalf("read fixture file %s: %v", srcPath, err)
		}
		archivePath := strings.TrimPrefix(filepath.ToSlash(targetPath), "/")
		dst := filepath.Join(filesDir, filepath.FromSlash(archivePath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", dst, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
		sum := sha256.Sum256(data)
		return snapshot.File{
			Path:        targetPath,
			ArchivePath: archivePath,
			Size:        int64(len(data)),
			SHA256:      hex.EncodeToString(sum[:]),
			Mode:        "0644",
		}
	}

	// addTree walks a captured directory (e.g. keyrings/usr/share/keyrings)
	// whose relative layout under it already mirrors the target's own
	// absolute paths (keyrings/<abs path with leading / removed>), or a
	// flat *.d directory (sources.list.d, preferences.d, trusted.gpg.d)
	// whose entries map to "<targetDirPrefix>/<basename>".
	addFlatDir := func(stateSub, targetDirPrefix string) []snapshot.File {
		dir := filepath.Join(stateDir, filepath.FromSlash(stateSub))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []snapshot.File
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, addFile(targetDirPrefix+"/"+e.Name(), filepath.Join(dir, e.Name())))
		}
		return out
	}

	var keyrings []snapshot.File
	keyringsRoot := filepath.Join(stateDir, "keyrings")
	filepathWalk(t, keyringsRoot, func(p string) {
		rel, err := filepath.Rel(keyringsRoot, p)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		targetPath := "/" + filepath.ToSlash(rel)
		keyrings = append(keyrings, addFile(targetPath, p))
	})

	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Tool:          snapshot.Tool{Name: "debark-test-fixture", Version: "0"},
		Target: snapshot.Target{
			DistroID:    "debian",
			VersionID:   readTrim("version_id"),
			Codename:    readTrim("codename"),
			Arch:        readTrim("arch"),
			APTVersion:  "2.6.1",
			DpkgVersion: "1.21.23",
		},
	}
	if fa := readTrim("foreign-arches"); fa != "" {
		snap.Target.ForeignArchs = strings.Fields(fa)
	}
	if p := filepath.Join(stateDir, "dpkg-status"); fileExists(p) {
		snap.DpkgStatus = addFile("/var/lib/dpkg/status", p)
	}
	if p := filepath.Join(stateDir, "apt", "sources.list"); fileExists(p) {
		snap.APT.Sources = append(snap.APT.Sources, addFile("/etc/apt/sources.list", p))
	}
	snap.APT.Sources = append(snap.APT.Sources, addFlatDir("apt/sources.list.d", "/etc/apt/sources.list.d")...)
	if p := filepath.Join(stateDir, "apt", "preferences"); fileExists(p) {
		snap.APT.Preferences = append(snap.APT.Preferences, addFile("/etc/apt/preferences", p))
	}
	snap.APT.Preferences = append(snap.APT.Preferences, addFlatDir("apt/preferences.d", "/etc/apt/preferences.d")...)
	snap.APT.Trusted = addFlatDir("apt/trusted.gpg.d", "/etc/apt/trusted.gpg.d")
	snap.APT.Keyrings = keyrings

	return snap
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func filepathWalk(t *testing.T, root string, fn func(path string)) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			filepathWalk(t, p, fn)
			continue
		}
		fn(p)
	}
}
