package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// archiveEpoch is the fixed mtime every tar entry gets. Real capture times
// live in Snapshot.CreatedAt, which is what the document digest is a
// function of; the tar layer around it carries no wall-clock information at
// all, so two writes of the same document are byte-identical.
var archiveEpoch = time.Unix(0, 0).UTC()

// WriteArchive writes snapshot.tar.zst at path: snapshot.json (the
// human-readable canonical form -- canonical.MarshalIndent, matching the
// convention "digest is always computed over the canonical form, never over
// this one") plus files/<ArchivePath> for every entry in fs, in a fixed
// order (snapshot.json, then files sorted by ArchivePath) with fixed
// per-entry metadata (mtime, mode, no owner). The archive is a pure function
// of (s, fs): writing it twice yields byte-identical output.
func doWriteArchive(path string, s *Snapshot, fs *FileSet) error {
	if s == nil {
		return dferr.New(dferr.Usage, "snapshot: WriteArchive: nil document")
	}
	if fs == nil || fs.Bytes == nil {
		return dferr.New(dferr.Usage, "snapshot: WriteArchive: nil file set")
	}
	if err := checkFileSetComplete(s, fs); err != nil {
		return err
	}

	doc, err := canonical.MarshalIndent(s)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "snapshot: encode document")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarEntry(tw, DocumentName, doc); err != nil {
		return dferr.Wrap(dferr.Usage, err, "snapshot: write %s", DocumentName)
	}
	names := make([]string, 0, len(fs.Bytes))
	for name := range fs.Bytes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !safeArchivePath(name) {
			return dferr.New(dferr.Usage, "snapshot: WriteArchive: unsafe archive path %q in file set", name)
		}
		if err := writeTarEntry(tw, FilesDir+"/"+name, fs.Bytes[name]); err != nil {
			return dferr.Wrap(dferr.Usage, err, "snapshot: write %s/%s", FilesDir, name)
		}
	}
	if err := tw.Close(); err != nil {
		return dferr.Wrap(dferr.Usage, err, "snapshot: close tar")
	}

	compressed, err := zstdCompress(buf.Bytes())
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "snapshot: compress archive")
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return dferr.Wrap(dferr.Usage, err, "snapshot: create %s", dir)
		}
	}
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		return dferr.Wrap(dferr.Usage, err, "snapshot: write %s", path)
	}
	return nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Size:     int64(len(data)),
		Mode:     0o644,
		ModTime:  archiveEpoch,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// checkFileSetComplete verifies that every File the document references has
// matching bytes in fs. This is a caller-side sanity check -- a hand-built
// Snapshot missing a byte slice is a programmer error -- not the full
// Validate: WriteArchive persists whatever document it is given, including
// one Capture produced with warnings, so a caller can archive and later
// inspect a partial capture rather than losing it.
func checkFileSetComplete(s *Snapshot, fs *FileSet) error {
	for _, f := range s.Files() {
		if f.Path == "" && f.ArchivePath == "" {
			continue
		}
		if _, ok := fs.Bytes[f.ArchivePath]; !ok {
			return dferr.New(dferr.Usage, "snapshot: WriteArchive: no bytes recorded for %s (archive_path %q)", f.Path, f.ArchivePath)
		}
	}
	return nil
}

// notASnapshotHint is the operator-facing advice attached to every
// "you did not give me a snapshot" refusal.
const notASnapshotHint = "`debark snapshot create` on the target machine writes one; `debark snapshot from-base` synthesizes one from a stock release"

// notASnapshotError is the refusal for input that is not a snapshot at all,
// as opposed to a snapshot that fails a check.
//
// The class is dferr.Usage (exit 1, "fix your command line"), never
// dferr.Verification (exit 4). Exit 4 means, and is documented in ADR-012 to
// mean, that a signature, digest or metadata check failed — the code a
// script is meant to escalate on and an operator is meant to read as "the
// media may have been tampered with". Pointing this function at a .deb, a
// text file or a directory that is not a snapshot is an ordinary mistake,
// made likelier by every file picker, and saying "verification failed" to it
// is both wrong and alarming.
//
// why is a sentence fragment completing "... is not a debark snapshot:".
func notASnapshotError(path, why string, args ...any) error {
	return dferr.New(dferr.Usage, "snapshot: %s is not a debark snapshot: %s",
		path, fmt.Sprintf(why, args...)).WithHint(notASnapshotHint)
}

// Open reads a snapshot from a .tar.zst archive or from an already-extracted
// directory, validates it against the schema and returns it ready to use.
//
// Redaction recorded in the document is re-applied on load, so a consumer
// can never see a field the operator asked to remove.
func doOpen(ctx context.Context, path string) (*Archive, error) {
	if err := ctx.Err(); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "snapshot: open cancelled")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "snapshot: open %s", path)
	}

	tempDir, err := os.MkdirTemp("", "debark-snapshot-")
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "snapshot: create temp directory")
	}
	fail := func(err error) (*Archive, error) {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}

	var docBytes []byte
	if fi.IsDir() {
		docPath := filepath.Join(path, DocumentName)
		docBytes, err = os.ReadFile(docPath)
		switch {
		case os.IsNotExist(err):
			// Not a snapshot at all, so not a verification failure: see
			// looksLikeZstd's doc comment for why that distinction matters.
			return fail(notASnapshotError(path,
				"it is a directory with no %s in it", DocumentName))
		case err != nil:
			return fail(dferr.Wrap(dferr.Verification, err, "snapshot: read %s", DocumentName))
		}
		srcFiles := filepath.Join(path, FilesDir)
		if _, statErr := os.Stat(srcFiles); os.IsNotExist(statErr) {
			// A directory that has the document but no files/ tree at all,
			// not merely one missing a particular entry, is exactly the
			// shape of a bundle's snapshot.json copy: core/bundle writes
			// only the document into a bundle (assemble.go's
			// writeSnapshotJSON), never the files/ tree its File entries
			// reference (core/bundle.Open reads snapshot.json directly for
			// this exact reason -- see bundle/open.go's loadSnapshotJSON
			// doc comment -- rather than calling this function). Name that
			// now, precisely, instead of letting the loop below fail on
			// the first File with a generic "missing from archive" that
			// gives no hint the files/ tree was never there to begin with.
			return fail(dferr.New(dferr.Verification,
				"snapshot: %s has a %s but no %s/ directory beside it; Open reads a full snapshot -- document plus captured files -- not the document alone",
				path, DocumentName, FilesDir,
			).WithHint("read just the document with snapshot.OpenDocument, or open the bundle itself with bundle.Open"))
		}
		if err := copyFilesDir(srcFiles, filepath.Join(tempDir, FilesDir)); err != nil {
			return fail(dferr.Wrap(dferr.Verification, err, "snapshot: copy %s", FilesDir))
		}
	} else {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return fail(dferr.Wrap(dferr.Usage, rerr, "snapshot: read %s", path))
		}
		if !looksLikeZstd(raw) {
			return fail(notASnapshotError(path, "it does not begin with a zstd frame header"))
		}
		tarBytes, derr := zstdDecompress(raw)
		if derr != nil {
			return fail(dferr.Wrap(dferr.Verification, derr, "snapshot: decompress %s", path))
		}
		docBytes, err = extractTar(tarBytes, tempDir)
		if err != nil {
			return fail(err)
		}
	}

	var s Snapshot
	if err := json.Unmarshal(docBytes, &s); err != nil {
		return fail(dferr.Wrap(dferr.Verification, err, "snapshot: decode %s", DocumentName))
	}
	if err := doValidate(&s); err != nil {
		return fail(err)
	}
	filesDir := filepath.Join(tempDir, FilesDir)
	if err := verifyExtractedFiles(&s, filesDir); err != nil {
		return fail(err)
	}
	if err := verifyKeyringFingerprints(&s, filesDir); err != nil {
		return fail(err)
	}

	// Digest the document as it was DELIVERED, before any normalisation this
	// function performs on it. Archive.Digest is what the lock and the
	// manifest record as snapshot_digest, and the whole point of recording it
	// is to answer "which snapshot file was this bundle built from" -- an
	// answer a reviewer is meant to be able to check by comparing digests
	// over a second channel (docs/threat-model.md §6). Digesting the
	// post-normalisation document broke exactly that: two byte-different
	// archives, one of them carrying unredacted proxy credentials, both
	// normalised to the same document and so reported the same digest. On an
	// honest snapshot the normalisation below is a no-op, so this moves no
	// digest that anyone has ever seen; it only stops two different inputs
	// from ever again sharing one identity.
	d, err := doDigest(&s)
	if err != nil {
		return fail(err)
	}

	restripped, err := reapplyRedactions(&s, filesDir)
	if err != nil {
		return fail(err)
	}
	if len(restripped) > 0 {
		// Say this loudly rather than silently repairing it: in an honest
		// flow WriteArchive persisted an already-redacted document, so
		// reapplyRedactions never changes anything. Something it *did* have
		// to strip means the document's own redactions list disagreed with
		// its contents -- a hand-edited or tampered document -- and the
		// operator is the one who has to decide what that means.
		//
		// Make room first. checkDisplayStrings bounds a document at
		// maxWarnings, and this append happens *after* doValidate has already
		// accepted a document that may be sitting exactly on that bound -- so
		// appending unconditionally hands the caller a document Open itself
		// would refuse on the next load, which is only reachable on a
		// document already found to disagree with itself but is a real
		// contradiction all the same (found by FuzzOpenSnapshotDocument's
		// "what Open accepts, doValidate still accepts" invariant). Dropping
		// the last capture-side warning is the right trade: this one is the
		// tamper signal, and it is the one the operator must not lose.
		if len(s.Warnings) >= maxWarnings {
			s.Warnings = s.Warnings[: maxWarnings-1 : maxWarnings-1]
		}
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"snapshot: this document claimed redactions it had not applied; %s re-stripped on load. An honest snapshot never needs this: treat the document as hand-edited or tampered with.",
			strings.Join(restripped, ", ")))
	}

	return &Archive{
		Snapshot: &s,
		Digest:   d,
		Path:     path,
		filesDir: filesDir,
		cleanup:  func() error { return os.RemoveAll(tempDir) },
	}, nil
}

// doOpenDocument is the real implementation behind OpenDocument. Unlike
// doOpen, it never touches a files/ tree and never creates a temp directory:
// path is either a snapshot.json file directly, or a directory containing
// one at its root -- a bundle directory is exactly that shape, since
// core/bundle writes the document there but never the captured files/ tree
// (see Open's doc comment, and the precise error Open itself now gives on a
// directory shaped like this).
func doOpenDocument(ctx context.Context, path string) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "snapshot: open document cancelled")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "snapshot: open %s", path)
	}
	docPath := path
	if fi.IsDir() {
		docPath = filepath.Join(path, DocumentName)
	}

	f, err := os.Open(docPath)
	switch {
	case os.IsNotExist(err):
		// Same distinction doOpen draws: an absent document means this is
		// not a snapshot, not that a snapshot failed a check.
		return nil, notASnapshotError(path, "there is no %s to read", DocumentName)
	case err != nil:
		return nil, dferr.Wrap(dferr.Verification, err, "snapshot: read %s", docPath)
	}
	docBytes, readErr := readCappedDocument(f, docPath)
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, dferr.Wrap(dferr.Environment, closeErr, "snapshot: close %s", docPath)
	}

	var s Snapshot
	if err := json.Unmarshal(docBytes, &s); err != nil {
		return nil, dferr.Wrap(dferr.Verification, err, "snapshot: decode %s", docPath)
	}
	if err := doValidate(&s); err != nil {
		return nil, err
	}

	// Re-strip whatever s.Redactions already claims was removed, the same
	// integrity guarantee doOpen gives via reapplyRedactions: a hand-edited
	// document cannot smuggle back a machine id or a label just by having
	// the field present while its name stays in Redactions. Passing a nil
	// FileSet makes the RedactProxies case a no-op here, which is correct,
	// not merely unavoidable: that redaction lives entirely in the captured
	// apt.conf *file bytes*, and OpenDocument never reads or returns any
	// file content for a document to smuggle a secret back inside.
	doRedact(&s, nil, s.Redactions...)

	return &s, nil
}

// maxSnapshotDocumentSize bounds how many bytes a snapshot.json document may
// be, whether read from inside a tar (extractTar) or directly off disk
// (doOpenDocument) -- independent of (and much tighter than) the
// whole-archive cap in zstdDecompress: a real snapshot.json is well under a
// megabyte even for a machine with a large dpkg_status, so this is set two
// orders of magnitude above that, giving a clearer, more specific refusal
// ("this one member is oversized") than waiting for the outer archive-wide
// limit (F6, docs/security/review-findings.md).
const maxSnapshotDocumentSize = 64 << 20 // 64 MiB

// readCappedDocument reads r up to maxSnapshotDocumentSize+1 bytes and
// refuses anything longer, rather than either silently truncating it or
// first trusting some declared size -- a tar header's, or a stat's -- that
// untrusted input could lie about. label is the name used in the error
// message: the tar member name (extractTar) or the filesystem path
// (doOpenDocument) the bytes came from.
func readCappedDocument(r io.Reader, label string) ([]byte, error) {
	limited := &io.LimitedReader{R: r, N: maxSnapshotDocumentSize + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, dferr.Wrap(dferr.Verification, err, "snapshot: read %s", label)
	}
	if limited.N == 0 {
		return nil, dferr.New(dferr.Verification,
			"snapshot: %s exceeds the %d-byte limit (refusing rather than silently truncating it)",
			label, maxSnapshotDocumentSize)
	}
	return data, nil
}

// extractTar extracts a decompressed snapshot tar into destDir/files, and
// returns the bytes of snapshot.json. Every files/* entry name is checked
// with safeArchivePath before being joined onto destDir: a snapshot is
// untrusted input, and this is the boundary that stops a crafted archive
// from writing outside its own extraction directory ("zip slip").
func extractTar(tarBytes []byte, destDir string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var doc []byte
	sawDoc := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, dferr.Wrap(dferr.Verification, err, "snapshot: read tar")
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case hdr.Name == DocumentName:
			if sawDoc {
				// A tar may name the same member twice, and "last one wins"
				// would let a crafted archive show one document to whatever
				// read the archive first and a different one to debark --
				// or simply hide the real document behind a second copy.
				// There is exactly one snapshot.json in a snapshot.
				return nil, dferr.New(dferr.Verification,
					"snapshot: archive contains %s more than once", DocumentName)
			}
			// readCappedDocument, not a bare io.ReadAll: see maxControlSize's
			// twin in core/fetch/control.go for why a limit-plus-truncation-
			// check is used instead of trusting the tar header's own
			// declared size, or silently truncating.
			data, rerr := readCappedDocument(tr, DocumentName)
			if rerr != nil {
				return nil, rerr
			}
			doc, sawDoc = data, true
		case strings.HasPrefix(hdr.Name, FilesDir+"/"):
			member := strings.TrimPrefix(hdr.Name, FilesDir+"/")
			if !safeArchivePath(member) {
				return nil, dferr.New(dferr.Verification, "snapshot: archive entry %q escapes the extraction directory", hdr.Name)
			}
			dest := filepath.Join(destDir, FilesDir, filepath.FromSlash(member))
			if err := extractRegularFile(dest, tr); err != nil {
				return nil, dferr.Wrap(dferr.Verification, err, "snapshot: extract %s", hdr.Name)
			}
		default:
			// Unknown member: ignore. WriteArchive never produces one; a
			// crafted archive gains nothing by adding one either, since
			// nothing downstream reads anything but snapshot.json and files/*.
		}
	}
	if !sawDoc {
		// Usage, not Verification: a zstd-compressed tar with no
		// snapshot.json in it is some other archive, not a tampered
		// snapshot. See notASnapshotError. (The path is not in scope here;
		// the sentence still says which thing was missing.)
		return nil, dferr.New(dferr.Usage,
			"snapshot: this is not a debark snapshot: the archive contains no %s", DocumentName).
			WithHint(notASnapshotHint)
	}
	return doc, nil
}

// extractRegularFile writes one tar member to dest.
//
// SECURITY: dest is derived from a tar entry name in an untrusted archive, so
// this function is a zip-slip sink and gosec's G703 is right to flag it. It
// does NOT validate dest itself, by design -- the check belongs where the
// name is still a name rather than a path, and that is extractTar above:
//
//   - safeArchivePath(member) rejects absolute paths, any ".." or "." or empty
//     segment, backslashes and colons (Windows separators / drive letters),
//     control characters, and anything that is not already path.Clean'd. See
//     core/snapshot/paths.go, and paths_test.go / open_fuzz_test.go, which
//     fuzz the "safeArchivePath said yes but the host disagreed" gap directly.
//   - hdr.Typeflag != tar.TypeReg is skipped before we ever get here, so a
//     symlink or hardlink member cannot be created and then written through
//     on a later iteration -- the other half of the classic tar escape, and
//     the reason this file never needs an O_NOFOLLOW argument.
//
// Keep it that way: if this function ever gains a second caller, that caller
// owes dest the same validation, and this comment is the contract for it.
func extractRegularFile(dest string, r io.Reader) error {
	// #nosec G703 -- dest is validated by safeArchivePath in extractTar; see the doc comment
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// #nosec G703 -- same: confined by safeArchivePath, and only TypeReg members reach here
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// copyFilesDir copies an already-extracted files/ tree onto a fresh temp
// directory, so Open() never hands out (or mutates, via redaction
// reapplication) a caller-owned directory directly -- an existing directory
// input is treated as read-only, just like an archive is.
func copyFilesDir(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return os.MkdirAll(dst, 0o755)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// G122 flags a filesystem call inside a WalkDir callback: the entry
		// was Lstat'd during the walk and is opened again here, so in
		// principle something could swap it for a symlink in between, and
		// os.ReadFile would follow it out of src. Left as-is deliberately,
		// for two reasons.
		//
		// First, the exposure is not what it looks like. This branch runs
		// only for the DIRECTORY form of a snapshot (Open given an
		// already-extracted tree). The untrusted form -- an archive off
		// removable media -- goes through extractTar instead, which never
		// creates a symlink at all (TypeReg only) and confines every name
		// with safeArchivePath. Following a symlink here would copy a file
		// the operator can already read into a temp directory that this
		// process owns and deletes on Close, and every copied file is then
		// digest-checked against the document by verifyExtractedFiles.
		//
		// Second, the suggested fix is not a lint fix. Switching to os.Root
		// would make debark start REFUSING snapshot directories that
		// contain a symlink, which it accepts today -- a behaviour change to
		// a supported input, not a tightening of an existing rule. If that
		// is wanted it should be its own change, with its own test, and a
		// note in the changelog; it should not arrive as a drive-by while
		// clearing linter findings.
		// #nosec G122,G703 -- see above: local directory input, digest-verified, temp destination
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// target is filepath.Join(dst, rel) where rel came from
		// filepath.Rel(src, p) for a p the walk itself produced under src, so
		// it cannot climb above dst.
		// #nosec G703,G122 -- target is Join(dst, rel) with rel produced by the walk under src
		return os.WriteFile(target, data, 0o644)
	})
}

// verifyExtractedFiles checks every captured file's digest against what was
// actually extracted. A mismatch, or a File the document names but the
// archive never delivered, is a dferr.Verification error.
func verifyExtractedFiles(s *Snapshot, filesDir string) error {
	for _, f := range s.Files() {
		if f.Path == "" && f.ArchivePath == "" {
			continue
		}
		p := filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath))
		data, err := os.ReadFile(p)
		if err != nil {
			return dferr.Wrap(dferr.Verification, err, "snapshot: %s: missing from archive", f.Path)
		}
		if got := digest.Bytes(data); !digest.Equal(got, f.SHA256) {
			return dferr.New(dferr.Verification, "snapshot: %s: digest mismatch: document says %s, archive has %s", f.Path, f.SHA256, got)
		}
	}
	return nil
}

// reapplyRedactions re-applies whatever s.Redactions already lists, so a
// hand-edited or corrupted document can never smuggle back a field the
// operator asked to remove: a consumer of Open()'s result gets the same
// guarantee Capture's caller did. It reuses Redact itself, operating on a
// scratch FileSet built from the extracted apt.conf files (the only files
// any current RedactionKind touches), then writes back only what changed --
// a no-op in the normal case, since WriteArchive persisted an
// already-redacted document.
//
// It returns the names of whatever it actually had to strip, so Open can say
// so out loud: "no-op in the normal case" means a non-empty return is
// evidence the document disagreed with itself, and silently repairing that
// would hide the one signal a reader has that the document was edited after
// capture.
func reapplyRedactions(s *Snapshot, filesDir string) ([]string, error) {
	if len(s.Redactions) == 0 {
		return nil, nil
	}
	scratch := &FileSet{Bytes: map[string][]byte{}}
	for _, f := range s.APT.Conf {
		p := filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath))
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, dferr.Wrap(dferr.Verification, err, "snapshot: %s: re-reading for redaction", f.Path)
		}
		scratch.Bytes[f.ArchivePath] = data
	}

	hadMachineID := s.Target.MachineID != ""
	hadLabels := len(s.Labels) > 0

	doRedact(s, scratch, s.Redactions...)

	var restripped []string
	if hadMachineID && s.Target.MachineID == "" {
		restripped = append(restripped, "target.machine_id")
	}
	if hadLabels && len(s.Labels) == 0 {
		restripped = append(restripped, "labels")
	}
	for _, f := range s.APT.Conf {
		data := scratch.Bytes[f.ArchivePath]
		p := filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath))
		if existing, err := os.ReadFile(p); err == nil && bytes.Equal(existing, data) {
			continue
		}
		restripped = append(restripped, f.Path)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Verification, err, "snapshot: %s: rewriting after redaction", f.Path)
		}
	}
	return restripped, nil
}
