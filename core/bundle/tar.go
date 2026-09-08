package bundle

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/inferops/debark/core/dferr"
)

// fixedModTime is FixedModTime rendered as a time.Time, computed once.
var fixedModTime = time.Unix(FixedModTime, 0).UTC()

// exportEntry is one regular file destined for the archive.
type exportEntry struct {
	relPath string // forward-slash, relative to the bundle root
	absPath string
	size    int64
	exec    bool
}

// collectExportEntries walks dir and returns every regular file under it,
// sorted by relPath. Directories are not recorded as entries (a file's own
// path is enough to recreate its parent directories on import); anything
// that is not a directory or a regular file - a symlink most of all - is
// refused, because a debark-built bundle never legitimately contains one.
func collectExportEntries(dir string) ([]exportEntry, error) {
	var entries []exportEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)

		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		switch {
		case d.IsDir():
			return nil
		case info.Mode().IsRegular():
			entries = append(entries, exportEntry{
				relPath: relSlash,
				absPath: p,
				size:    info.Size(),
				exec:    info.Mode().Perm()&0o111 != 0,
			})
			return nil
		default:
			return dferr.New(dferr.Environment, "bundle: export: %s is not a regular file or directory (mode %v)", relSlash, info.Mode())
		}
	})
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "bundle: export: scan %s", dir)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })
	return entries, nil
}

// ExportTar writes dir as <tarPath> deterministically: tar entries sorted by
// path, a fixed ModTime, uid/gid 0, forward-slash names, no host paths and
// no extended attributes, compressed with zstd. Two exports of the same tree
// produce byte-identical output (proven in tar_test.go).
func exportTar(ctx context.Context, dir, tarPath string) error {
	entries, err := collectExportEntries(dir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: export: create %s", filepath.Dir(tarPath))
	}
	tmp, err := os.CreateTemp(filepath.Dir(tarPath), ".tmp-*")
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: export: create temp file")
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			// Best-effort: this runs only when the export already failed, so the
			// error being returned is the one that matters. A leftover .tmp-* beside
			// the target is cosmetic, and there is nowhere better to report it to.
			_ = os.Remove(tmpName)
		}
	}()

	zw, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "bundle: export: create zstd writer")
	}
	tw := tar.NewWriter(zw)

	// Two shapes of Close appear below, and the difference is deliberate.
	//
	// On an ERROR path the writers are closed with the result discarded
	// (`_ = tw.Close()`): we are abandoning the temp file, the deferred
	// os.Remove above will delete it, and an error from flushing a stream
	// nobody will ever read would only mask the real failure being returned.
	//
	// On the SUCCESS path (after the loop) every Close IS checked, because
	// tar.Writer.Close writes the trailer and zstd.Encoder.Close flushes the
	// last frame: a dropped error there means a silently truncated bundle
	// that still gets renamed into place. That is the determinism guarantee,
	// and .golangci.yml names this file when it explains why the errcheck
	// exclusion list is not broadened to all Close calls. Do not "simplify"
	// the two groups into one.
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			tmp.Close()
			return err
		}
		mode := int64(0o644)
		if e.exec {
			mode = 0o755
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.relPath,
			Size:     e.size,
			Mode:     mode,
			ModTime:  fixedModTime,
			Uid:      0,
			Gid:      0,
			Uname:    "",
			Gname:    "",
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			tmp.Close()
			return dferr.Wrap(dferr.Environment, err, "bundle: export: write header for %s", e.relPath)
		}
		if err := copyFileInto(tw, e.absPath); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			tmp.Close()
			return dferr.Wrap(dferr.Environment, err, "bundle: export: write %s", e.relPath)
		}
	}

	if err := tw.Close(); err != nil {
		_ = zw.Close()
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "bundle: export: close tar writer")
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "bundle: export: close zstd writer")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "bundle: export: sync %s", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: export: close %s", tmpName)
	}
	if err := os.Rename(tmpName, tarPath); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: export: publish %s", tarPath)
	}
	removeTmp = false
	return nil
}

func copyFileInto(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// maxImportEntrySize bounds how many decompressed bytes importTar will
// write to disk for a single tar entry. A real bundle's largest entries are
// its .deb pool files, which in the wild run from a few KB to at most a few
// GB (an unusually large driver or data package); this is set comfortably
// above that so no real bundle is ever rejected, while still giving
// importTar a hard backstop against an entry whose declared (or, via a
// highly compressible zstd stream, actually delivered) size is effectively
// unbounded — the same "impose a sane limit, do not silently truncate"
// reasoning F6 (docs/security/review-findings.md) asks to be applied
// everywhere debark decompresses data it has not yet verified. This tar
// is untrusted at the point importTar runs: it arrived on removable media,
// and verify.Verify has not yet checked it (that is the whole reason every
// entry name is already validated below, before anything is written).
//
// A var, not a const: archive/tar will not let a test construct an entry
// whose header claims more bytes than are actually written through the tar
// writer, so the only way to exercise this limit tripping without a test
// that genuinely writes multiple gigabytes of data is to lower the limit
// for the duration of one test (saved and restored via t.Cleanup) — the
// same injection pattern core/engine/vars.go uses for its own
// package-level collaborators. Production code never assigns this.
var maxImportEntrySize int64 = 8 << 30 // 8 GiB

// maxImportEntries and maxImportTotalSize bound the two dimensions
// maxImportEntrySize leaves wide open: how MANY entries a hostile archive
// may contain, and how many decompressed bytes it may produce in TOTAL.
// Neither follows from the per-entry limit — an attacker who cannot exceed
// 8 GiB in one entry simply writes more entries, which is exactly what makes
// an expansion bomb cheap. Measured here before these two limits existed: a
// 232 KB archive (32 entries of 64 MiB of zeros) expanded to 2 GiB of disk
// in 1.8s, and a 554 KB archive created 200,000 files. Both returned nil,
// and both run before verify.Verify has looked at anything — `debark
// inspect` and `debark doctor` on freshly arrived media are exactly this
// path.
//
// The ceilings come from what a real bundle actually holds. The bundle
// hack/demo-airgap.sh builds (jq plus its dependency closure) is 14 files
// and 420 KB. The largest real .deb corpus this repo has measured is E7's
// (docs/experiments/E7-network-postinst.md): 1,750 packages, ~1.3 GB. A
// bundle is such a pool plus a dozen fixed documents — lock.json,
// snapshot.json, the manifest and its signature, README.txt, the repository
// indices — and optionally one embedded ~16 MB linux binary. So:
//
//   - maxImportEntries at 50,000 is ~28x E7's file count, comfortably past
//     any dependency closure a single air-gap bundle plausibly carries (a
//     full desktop plus texlive is a few thousand packages). The 200,000-file
//     archive above now stops at 50,000 files in 85s on this host with a
//     Verification error, instead of running to completion and reporting
//     success.
//   - maxImportTotalSize at 64 GiB is ~50x E7's byte count and 8x
//     maxImportEntrySize, so a bundle may still hold several of the largest
//     entries the per-entry rule permits — and it is above the capacity of
//     the removable media a bundle realistically crosses the gap on.
//
// Neither ceiling makes a bomb free: reaching one still costs the operator
// the work done up to it. What they buy is that the work terminates, that it
// terminates with a refusal an operator can act on rather than a bundle that
// looks fine, and that Open then removes the partial extraction.
//
// Vars, not consts, for the same reason maxImportEntrySize is one: a test
// that genuinely wrote 50,000 entries or 64 GiB of data would be unusable,
// so tests lower these for their own duration and restore them via
// t.Cleanup. Production code never assigns them.
var (
	maxImportEntries         = 50_000
	maxImportTotalSize int64 = 64 << 30 // 64 GiB
)

// maxImportEntryNameLen and maxImportNameComponentLen bound how long an
// entry name may be. Without them an absurd name is not refused as hostile
// input at all: it travels down to os.MkdirAll/os.OpenFile and returns an
// operating-system "name too long" failure, which importTar classifies as
// dferr.Environment — telling the operator their machine is at fault for a
// string the attacker chose. The longest path a real bundle holds is around
// 90 bytes (repo/ + the pool path for linux-headers-6.1.0-13-common, the
// longest example in core/repository's own tests), so 1024 leaves an order
// of magnitude of headroom; 255 is the per-component maximum ext4, NTFS and
// APFS all impose, so no entry that ever existed as a file on a real
// filesystem — the only kind exportTar can produce — can exceed it.
const (
	maxImportEntryNameLen     = 1024
	maxImportNameComponentLen = 255
)

// errEntryTooLarge is wrapped into the error extractRegularFile returns
// when a single entry exceeds maxImportEntrySize, so importTar's caller can
// tell "this entry was refused as oversized" (a verification-worthy,
// hostile-input finding) apart from an ordinary disk I/O failure (an
// environment problem) without string-matching an error message.
var errEntryTooLarge = errors.New("bundle: import: entry exceeds the per-entry size limit")

// The remaining refusals importTar can reach are sentinels too, for the same
// reason: a caller (and a test) must be able to tell which bound tripped
// without matching on message text, and every one of them is a
// hostile-input finding rather than an environment failure.
var (
	errTooManyEntries  = errors.New("bundle: import: archive contains too many entries")
	errArchiveTooLarge = errors.New("bundle: import: archive expands past the whole-archive size limit")
	errDuplicateEntry  = errors.New("bundle: import: archive contains two entries for the same path")
)

// shortName trims an attacker-chosen entry name before it is quoted into an
// error message. Nothing else bounds it: a PAX name can be megabytes long,
// and an error that reproduces one in full turns a refusal into a log flood.
func shortName(name string) string {
	const maxLen = 120
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen] + "..."
}

// collisionKey maps an entry name to the file it will actually land on.
// Windows folds case and strips trailing dots and spaces from every path
// component, so "lock.json", "LOCK.JSON", "lock.json." and "lock.json " are
// four distinct tar entries that resolve to one file there — the last one
// written silently wins, and the extracted tree then differs from the
// archive that was handed over. Keying on the folded form refuses that
// collapse on every platform, not only where it happens, for the same
// reason safeRelPath refuses backslashes and drive letters everywhere: a
// hostile archive is not obliged to know what will extract it. The cost is
// that a bundle holding two pool files whose names differ only in letter
// case would be refused; Debian policy §5.6.1 forbids an uppercase letter in
// a binary package name, so a real pool does not contain such a pair.
func collisionKey(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.TrimRight(p, ". "))
	}
	return strings.Join(parts, "/")
}

// importTar extracts tarPath into dir. Every entry name is validated before
// anything is written: this is a security boundary, because a bundle
// arrives on removable media that is not trusted until verify.Verify has
// checked its manifest. No symlink or hard-link entry is ever honoured, no
// name may escape dir via "..", an absolute path, or a Windows drive letter,
// and no entry type other than a regular file or a directory is accepted.
// Nor is an archive allowed to be unboundedly expensive: the three ceilings
// above cap one entry, the entry count and the total decompressed bytes, and
// two entries that would land on the same file are refused rather than
// silently collapsed. Every one of those refusals is dferr.Verification —
// hostile input, not a sick machine.
func importTar(ctx context.Context, tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "bundle: import: open %s", tarPath)
	}
	defer f.Close()

	// WithDecoderMaxMemory bounds the decoder's own internal window/memory
	// use during streaming decompression, guarding against a frame header
	// that declares an absurd window size purely to force a large
	// allocation. It does not, by itself, bound the *total* bytes a long
	// stream can produce (streaming decompression is unbounded output by
	// design) — maxImportEntrySize and maxImportTotalSize below, enforced
	// against bytes actually written as the stream is consumed, are what
	// bound that.
	zr, err := zstd.NewReader(f, zstd.WithDecoderMaxMemory(1<<30))
	if err != nil {
		return dferr.Wrap(dferr.Verification, err, "bundle: import: %s is not a valid zstd stream", tarPath)
	}
	defer zr.Close()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: import: resolve %s", dir)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "bundle: import: create %s", absDir)
	}

	tr := tar.NewReader(zr)
	// Three running counters, because three different things about an
	// archive can be hostile independently: one entry too big
	// (maxImportEntrySize), too many entries (maxImportEntries), and too
	// many bytes across all of them (maxImportTotalSize). Each has to be
	// checked as the stream is consumed — a tar carries no trustworthy
	// up-front total, and a header that did carry one would be
	// attacker-written anyway. seen, which holds one key per accepted entry
	// so a second entry for the same file can be refused, is bounded by the
	// entry ceiling for the same reason.
	entries := 0
	remaining := maxImportTotalSize
	seen := make(map[string]string)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return dferr.Wrap(dferr.Verification, err, "bundle: import: read %s", tarPath)
		}

		entries++
		if entries > maxImportEntries {
			return dferr.Wrap(dferr.Verification, errTooManyEntries, "bundle: import: %s declares more than %d entries", tarPath, maxImportEntries)
		}

		rel, err := safeRelPath(hdr.Name)
		if err != nil {
			return dferr.New(dferr.Verification, "bundle: import: refusing entry %q: %v", shortName(hdr.Name), err)
		}
		key := collisionKey(rel)
		if prev, dup := seen[key]; dup {
			return dferr.Wrap(dferr.Verification, errDuplicateEntry, "bundle: import: refusing entry %q: it lands on the same file as earlier entry %q", shortName(hdr.Name), shortName(prev))
		}
		seen[key] = hdr.Name
		dest := filepath.Join(absDir, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return dferr.Wrap(dferr.Environment, err, "bundle: import: create directory %s", rel)
			}
		// TypeRegA is '\x00', the pre-POSIX spelling of "regular file". It is
		// deprecated because archive/tar's Reader normalises it away before we
		// ever see it (reader.go: a TypeRegA header becomes TypeDir if its name
		// ends in '/', else TypeReg), so this arm is unreachable through the
		// Reader we use. It stays because this switch is the import path's
		// allow-list for an untrusted archive: the failure mode of listing one
		// extra constant here is nothing, and the failure mode of a future
		// refactor that reads headers from somewhere WITHOUT that
		// normalisation is that every legacy regular file silently falls
		// through to the default arm and gets refused. Belt and braces on the
		// cheap side of an asymmetric bet.
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // SA1019: TypeRegA is deprecated and unreachable here; kept as a safety arm, see above
			// The allowance for this entry is whichever ceiling binds first:
			// the per-entry one, or whatever is left of the whole-archive
			// budget. Taking the smaller of the two is what stops a bomb
			// overshooting the total by a whole 8 GiB entry before anything
			// notices, and it is why the budget is spent against bytes
			// actually written rather than against hdr.Size, which is
			// attacker-chosen and need not match what the stream delivers.
			allow, overTotal := maxImportEntrySize, false
			if remaining < allow {
				allow, overTotal = remaining, true
			}
			n, err := extractRegularFile(dest, tr, hdr, allow)
			remaining -= n
			if err != nil {
				if errors.Is(err, errEntryTooLarge) {
					// A hostile-input finding, not a disk/permission
					// problem: classify as Verification like every other
					// refusal in this loop (safeRelPath, link entries,
					// unsupported types), not Environment.
					if overTotal {
						return dferr.Wrap(dferr.Verification, errArchiveTooLarge, "bundle: import: %s expands past the %d-byte limit for one archive (reached at entry %s)", tarPath, maxImportTotalSize, shortName(rel))
					}
					return dferr.Wrap(dferr.Verification, err, "bundle: import: %s", shortName(rel))
				}
				return dferr.Wrap(dferr.Environment, err, "bundle: import: write %s", shortName(rel))
			}
		case tar.TypeSymlink, tar.TypeLink:
			return dferr.New(dferr.Verification, "bundle: import: refusing link entry %q (bundles never contain links)", shortName(hdr.Name))
		default:
			return dferr.New(dferr.Verification, "bundle: import: refusing entry %q of unsupported type %v", shortName(hdr.Name), hdr.Typeflag)
		}
	}
	return nil
}

// extractRegularFile writes one entry and reports how many bytes it wrote,
// so importTar can charge them against the whole-archive budget. limit is
// the smaller of the per-entry and remaining-total allowances; exceeding it
// yields errEntryTooLarge, and importTar decides which of the two ceilings
// to name.
func extractRegularFile(dest string, r io.Reader, hdr *tar.Header, limit int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	mode := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	// io.LimitedReader, not a bare io.Copy: archive/tar already stops a
	// TypeReg entry's Read at hdr.Size, so this is not defending against
	// reading past a declared size — it is defending against a declared (or,
	// via a highly compressible zstd stream, actually delivered) size that
	// is itself absurd. Checking limited.N == 0 afterwards tells "hit the
	// limit and kept going" apart from "the entry really was exactly that
	// long"; a truncated pool file that then sits in the bundle looking
	// complete would be worse than refusing outright.
	limited := &io.LimitedReader{R: r, N: limit + 1}
	n, copyErr := io.Copy(out, limited)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	if limited.N == 0 {
		return n, fmt.Errorf("%s: %w (limit %d bytes)", shortName(hdr.Name), errEntryTooLarge, limit)
	}
	return n, nil
}

// safeRelPath validates a tar entry name arriving from untrusted media and
// returns it cleaned, still forward-slash, relative, and guaranteed not to
// escape the directory it will be joined under. Backslashes and a
// Windows-style drive-letter prefix are refused unconditionally, regardless
// of the host platform ImportTar happens to run on: a hostile archive is not
// obliged to know or care what will extract it.
func safeRelPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty entry name")
	}
	if len(name) > maxImportEntryNameLen {
		return "", fmt.Errorf("name is %d bytes, over the %d-byte limit", len(name), maxImportEntryNameLen)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("contains a NUL byte")
	}
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("contains a backslash")
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path")
	}
	if len(name) >= 2 && name[1] == ':' {
		return "", fmt.Errorf("looks like a Windows drive-letter path")
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", fmt.Errorf("empty after cleaning")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path traversal")
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("absolute path after cleaning")
	}
	for _, part := range strings.Split(cleaned, "/") {
		if len(part) > maxImportNameComponentLen {
			return "", fmt.Errorf("a path component is %d bytes, over the %d-byte limit", len(part), maxImportNameComponentLen)
		}
	}
	return cleaned, nil
}
