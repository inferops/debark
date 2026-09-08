package snapshot

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/inferops/debark/core/digest"
)

// captureFile reads targetPath (root-relative, target-absolute) once, adds
// its bytes to fs under its ArchivePath, and returns its File record.
func captureFile(root, targetPath string, fs *FileSet) (File, error) {
	hp := hostPath(root, targetPath)
	f, err := os.Open(hp)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if info.IsDir() {
		return File{}, fmt.Errorf("%s is a directory, not a file", targetPath)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return File{}, err
	}
	rec := File{
		Path:        targetPath,
		ArchivePath: toArchivePath(targetPath),
		Size:        int64(len(data)),
		SHA256:      digest.Bytes(data),
		Mode:        fmt.Sprintf("0%o", info.Mode().Perm()),
	}
	fs.Bytes[rec.ArchivePath] = data
	return rec, nil
}

// captureOptionalFile captures targetPath, treating "does not exist" as
// nothing to report (most of these files are legitimately absent on a given
// target) and any other read failure (permission denied, a symlink loop, a
// path component that is not a directory, ...) as a warning: per the
// package's contract, an unreadable file never fails the capture.
func captureOptionalFile(root, targetPath string, fs *FileSet) (file *File, warning string) {
	rec, err := captureFile(root, targetPath, fs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, fmt.Sprintf("snapshot: %s: %v", targetPath, err)
	}
	return &rec, ""
}

// captureDir lists dirTargetPath (non-recursive: none of apt's *.d
// directories nest further) and captures every regular file whose name
// satisfies keep (nil means capture everything). A missing directory is
// silent -- most of these are optional -- but an existing, unreadable one is
// a warning, since that means real configuration may be hidden from us.
func captureDir(root, dirTargetPath string, keep func(name string) bool, fs *FileSet) (files []File, warnings []string) {
	hp := hostPath(root, dirTargetPath)
	entries, err := os.ReadDir(hp)
	if err != nil {
		if !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("snapshot: %s: %v", dirTargetPath, err))
		}
		return nil, warnings
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if keep != nil && !keep(name) {
			continue
		}
		tp := path.Join(dirTargetPath, name)
		rec, err := captureFile(root, tp, fs)
		if err != nil {
			if info, statErr := os.Lstat(hostPath(root, tp)); statErr == nil && info.IsDir() {
				continue // a subdirectory where none is expected; not a file to warn about
			}
			warnings = append(warnings, fmt.Sprintf("snapshot: %s: %v", tp, err))
			continue
		}
		files = append(files, rec)
	}
	return files, warnings
}

// isSourcesListName is apt's own sources.list.d filter (sources.list(5)):
// only ".list" (one-line) and ".sources" (deb822) entries are read.
func isSourcesListName(name string) bool {
	return strings.HasSuffix(name, ".list") || strings.HasSuffix(name, ".sources")
}

// isPlainRunPartsName is apt's default filter for apt.conf.d and
// preferences.d-style directories read via its RunParts/GetListOfFilesInDir
// (apt.conf(5)): letters, digits, '_' and '-' only, so editor backups and
// dpkg's ".dpkg-new"/".dpkg-old"/".orig" siblings are skipped. debark
// captures apt.conf.d wholesale for completeness -- nothing here filters
// what gets *captured* -- this only decides what InstallRecommends treats as
// live configuration, matching what apt itself would actually read.
func isPlainRunPartsName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
