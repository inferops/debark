package bundle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/snapshot"
)

// openBundle is the real implementation behind the package-level Open.
func openBundle(ctx context.Context, path string) (*Bundle, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "bundle: open %s", path)
	}

	dir := path
	extracted := false
	if !fi.IsDir() {
		tmp, err := os.MkdirTemp("", "debark-bundle-*")
		if err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "bundle: open: create temp dir")
		}
		// ImportTar is the same untrusted-media boundary a bundle arriving on
		// removable media must cross: Open never skips it, tar file or not.
		if err := importTar(ctx, path, tmp); err != nil {
			// Best-effort: the extraction already failed and that error is what
			// the caller needs. A temp dir we could not remove is a leak in
			// os.TempDir, not a reason to replace a real diagnosis with an
			// os.RemoveAll errno.
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		dir = tmp
		extracted = true
	}

	cleanup := func() {
		if extracted {
			// Same reasoning as above: cleanup runs on the failure paths below,
			// where an error is already being returned. Discarding this one is
			// explicit so nobody has to wonder whether it was forgotten.
			_ = os.RemoveAll(dir)
		}
	}

	l, err := lock.Load(dir)
	if err != nil {
		cleanup()
		return nil, err
	}

	snap, err := loadSnapshotJSON(dir)
	if err != nil {
		cleanup()
		return nil, err
	}

	m, canon, err := manifest.Load(dir)
	if err != nil {
		cleanup()
		return nil, err
	}

	sig, err := manifest.LoadSignature(dir)
	if err != nil {
		cleanup()
		return nil, err
	}

	return &Bundle{
		Dir:               dir,
		Manifest:          m,
		Signature:         sig,
		Lock:              l,
		Snapshot:          snap,
		ManifestCanonical: canon,
		Extracted:         extracted,
	}, nil
}

// loadSnapshotJSON reads the plain snapshot.json copy Assemble wrote into
// the bundle. This is deliberately not snapshot.Open, which parses a
// standalone snapshot.tar.zst archive (document plus captured files); a
// bundle carries only the document, already validated when it was captured,
// so a plain JSON decode of the frozen Snapshot type is what "parses and
// nothing more" (Open's own contract) means here.
func loadSnapshotJSON(dir string) (*snapshot.Snapshot, error) {
	path := filepath.Join(dir, SnapshotFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, dferr.Wrap(dferr.Usage, err, "bundle: read %s", path)
	}
	var s snapshot.Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "bundle: parse %s", path)
	}
	return &s, nil
}

func closeBundle(b *Bundle) error {
	if b == nil || !b.Extracted {
		return nil
	}
	if err := os.RemoveAll(b.Dir); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: close: remove %s", b.Dir)
	}
	return nil
}
