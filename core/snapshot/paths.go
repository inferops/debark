package snapshot

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// hostPath maps a target-absolute path (always forward-slash, e.g.
// "/etc/apt/sources.list") onto the real filesystem location it should be
// read from: root joined with the path, using the host's own separator
// conventions. root is always a real filesystem path (may be "/" for a real
// capture, or an OS-native fixture directory such as "C:\testdata\debian12"
// for a fixture capture).
func hostPath(root, targetPath string) string {
	return filepath.Join(root, filepath.FromSlash(targetPath))
}

// toArchivePath turns a target-absolute path into the archive member name
// used under FilesDir: the path with its leading separator removed, always
// forward-slash. "/etc/apt/sources.list" -> "etc/apt/sources.list".
func toArchivePath(targetPath string) string {
	return strings.TrimPrefix(path.Clean("/"+targetPath), "/")
}

// sortFilesByPath sorts a []File slice by Path (the schema's "RecordedPath"),
// in place, so digests over the snapshot document are stable regardless of
// directory listing order.
func sortFilesByPath(files []File) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// safeArchivePath validates an archive member path (either a File.ArchivePath
// recorded in a snapshot document, or a tar entry name read back from an
// archive) before it is ever joined onto a real directory. A snapshot is
// untrusted input: this is the boundary that stops a crafted document from
// writing outside its extraction directory ("zip slip").
//
// A valid path is non-empty, forward-slash, relative (no leading "/"), has no
// ".." segment at any position, contains neither a backslash nor a colon
// -- both are directory-separator or drive-letter metacharacters on Windows
// that a legitimate captured path (always derived from a Unix absolute path)
// will never contain -- and carries no control characters: this value is
// both joined onto a real directory and printed back to an operator, so a
// NUL truncates the name for anything that later hands it to a C API, and an
// ESC or C1 sequence turns a "refusing %q" diagnostic into terminal commands
// (see hasControlChars).
func safeArchivePath(p string) bool {
	if p == "" || p == "." || p != path.Clean(p) {
		return false
	}
	if path.IsAbs(p) {
		return false
	}
	if strings.ContainsAny(p, `\:`) {
		return false
	}
	if hasControlChars(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}
