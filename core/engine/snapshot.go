package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
)

func (b *build) loadSnapshot(ctx context.Context) error {
	snap, err := openSnapshot(ctx, b.req.SnapshotRef)
	if err != nil {
		return classify(err, dferr.Usage, "engine: open snapshot")
	}
	b.snap = snap
	b.emit(evidence.TypeSnapshotLoaded, "snapshot loaded", map[string]any{
		"snapshot_digest": snap.Digest,
		"distro_id":       snap.Snapshot.Target.DistroID,
		"version_id":      snap.Snapshot.Target.VersionID,
		"codename":        snap.Snapshot.Target.Codename,
		"arch":            snap.Snapshot.Target.Arch,
	})
	return nil
}

// installRecommends computes the effective APT::Install-Recommends value for
// this snapshot. snapshot.InstallRecommends wants an in-memory *FileSet, but
// snapshot.Open (also frozen) hands back an *Archive backed by an extracted
// directory (FilesDir()), not a FileSet — the two exported snapshot shapes
// don't line up for this one call. Bridging is small enough to do here: read
// the same files snap.Snapshot.Files() names back into a FileSet. A file that
// is missing (redacted, or simply absent in a test fixture) is skipped rather
// than treated as an error, matching InstallRecommends' own fallback-to-true
// default for a target with no explicit setting.
func installRecommends(snap *snapshot.Archive) bool {
	fs, err := loadFileSet(snap)
	if err != nil {
		return true
	}
	value, _ := snapshotInstallRecommends(snap.Snapshot, fs)
	return value
}

// dpkgStatusBytes reads the target's captured /var/lib/dpkg/status out of
// the open snapshot archive, so it can be handed to doctor.Input.DpkgStatus
// (finalize.go's runDoctor). It reads the same way installRecommends above
// does: join FilesDir() with the file's recorded ArchivePath. This is
// necessary, not redundant with Input.Snapshot, because snapshot.Snapshot
// only records DpkgStatus's path, size and digest — never its content (see
// doctor.Input.DpkgStatus's own doc comment) — and doctor's held-package and
// dkms-headers checks need the actual installed-package list to do their
// job at all.
//
// A missing or unreadable file (redacted, or simply absent from a minimal
// test fixture) yields nil rather than an error, exactly like loadFileSet's
// own per-file tolerance below: doctor already degrades to a softer finding
// or no finding at all when DpkgStatus is nil (again, see its doc comment),
// so there is nothing here worth failing a build over.
func dpkgStatusBytes(snap *snapshot.Archive) []byte {
	if snap == nil || snap.Snapshot == nil || snap.Snapshot.DpkgStatus.ArchivePath == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(snap.FilesDir(), filepath.FromSlash(snap.Snapshot.DpkgStatus.ArchivePath)))
	if err != nil {
		return nil
	}
	return data
}

func loadFileSet(snap *snapshot.Archive) (*snapshot.FileSet, error) {
	fs := &snapshot.FileSet{Bytes: map[string][]byte{}}
	dir := snap.FilesDir()
	for _, f := range snap.Snapshot.Files() {
		if f.ArchivePath == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		fs.Bytes[f.ArchivePath] = data
	}
	return fs, nil
}
