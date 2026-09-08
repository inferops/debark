package export

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Resolve an existing directory, or the existing parent of a new one. Plan
// has already required that parent to exist. This catches aliases through
// symlinks and Windows junctions before comparing source and destination.
func resolveDestination(dst string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dst)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dst))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(dst)), nil
}

func unsafeDestination(path string) error {
	return &ExportError{
		Kind: ErrKindUnsupported, Op: "check", Path: path,
		Summary: fmt.Sprintf("The destination contains an unexpected file, folder, or link: %s.", path),
		Hint:    "Choose an empty destination folder, or remove the previous export before trying again. Existing unrelated files have been left in place.",
	}
}

// An existing partial copy may be resumed, but merging different bundles
// leaves extra files that bundle verification will reject on the target.
// Refuse links as well: opening them for writing can damage another tree.
// Recheck at Run time because planning and copying are separate UI actions.
func checkDestinationEntries(ctx context.Context, p *Plan) error {
	src, err := filepath.EvalSymlinks(p.SourceDir)
	if err != nil {
		return classify(sideSource, "resolve", p.SourceDir, err)
	}
	dst, err := resolveDestination(p.DestDir)
	if err != nil {
		return classify(sideDest, "resolve", p.DestDir, err)
	}
	if err := checkOverlap(src, dst); err != nil {
		return err
	}
	allowed := make(map[string]bool, len(p.Dirs)+len(p.Files)+1)
	allowed[MarkerName] = false
	for _, dir := range p.Dirs {
		allowed[filepath.FromSlash(dir)] = true
	}
	for _, file := range p.Files {
		allowed[filepath.FromSlash(file.Rel)] = false
	}
	return filepath.WalkDir(p.DestDir, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return classify(sideDest, "check", path, ctx.Err())
		}
		if path == p.DestDir && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return classify(sideDest, "check", path, err)
		}
		if path == p.DestDir && entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(p.DestDir, path)
		if err != nil {
			return classify(sideDest, "check", path, err)
		}
		wantDir, ok := allowed[rel]
		if !ok || entry.IsDir() != wantDir || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return unsafeDestination(path)
		}
		return nil
	})
}
