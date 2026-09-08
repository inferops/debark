// Frozen public API of the snapshot package. The signatures here
// are the contract other packages compile against; the implementation replaces
// the bodies.

package snapshot

import (
	"context"
)

// CaptureOptions controls a capture run.
type CaptureOptions struct {
	// Root is the filesystem root to read from. Empty means "/". Tests point
	// it at a fixture tree, which is also how the container backend captures a
	// foreign root.
	Root string
	// Redact strips the machine id, proxy settings and labels.
	Redact bool
	// Labels are operator-supplied key=value pairs. Off by default.
	Labels map[string]string
	// IncludeKeyrings copies the keyrings referenced by Signed-By and the
	// contents of trusted.gpg.d. Default true; --no-keyrings turns it off for
	// operators who consider their keyring list sensitive.
	//
	// The cost is paid on the builder, and nothing there can make it back:
	// with no captured key material, core/apt builds a private root whose
	// trusted.gpg.d is empty, strips every path-form Signed-By (warning
	// private-root.signed-by-stripped as it goes) and refuses every source
	// file under --approved-keys, which can then never pass. The container
	// backend cannot supply the difference either -- it never mounts a key, a
	// key path or a keyring into a container. Note this is deliberately not
	// the claim "apt cannot verify the archives": a captured source that
	// already carries trusted=yes is an exception to that, and only what is
	// unconditionally true belongs here.
	IncludeKeyrings bool
}

// Capture reads the target's state and returns the snapshot document together
// with the files it references. The caller writes it with WriteArchive.
//
// It requires no root privileges and never writes outside its output path.
func Capture(ctx context.Context, opts CaptureOptions) (*Snapshot, *FileSet, error) {
	return doCapture(ctx, opts)
}

// FileSet holds the bytes of every captured file, keyed by ArchivePath.
type FileSet struct {
	// Bytes maps ArchivePath to file content.
	Bytes map[string][]byte
}

// WriteArchive writes snapshot.tar.zst at path.
func WriteArchive(path string, s *Snapshot, fs *FileSet) error {
	return doWriteArchive(path, s, fs)
}

// Archive is an opened snapshot: the document, the digest that identifies it,
// and the extracted files on disk.
type Archive struct {
	// Snapshot is the parsed, validated document.
	Snapshot *Snapshot
	// Digest is the canonical digest of Snapshot, which is what the lock and
	// manifest record.
	Digest string
	// Path is where the archive was read from.
	Path string

	filesDir string
	cleanup  func() error
}

// FilesDir returns the directory holding the extracted captured files. Paths
// inside it are the ArchivePath values from the document.
func (a *Archive) FilesDir() string { return a.filesDir }

// Close releases any temporary directory the archive was extracted into.
func (a *Archive) Close() error {
	if a == nil || a.cleanup == nil {
		return nil
	}
	return a.cleanup()
}

// Open reads a snapshot from a .tar.zst archive or from an already-extracted
// directory, validates it against the schema and returns it ready to use.
//
// Open reads a full snapshot: every File the document lists must be present,
// and digest-verified, under the archive's or directory's files/ tree. It is
// not a way to read just the document -- core/bundle, for one, copies only
// the document into a bundle as snapshot.json and never the files/ tree the
// document's File entries reference (a signed bundle carries the packages
// themselves, not the target's original config files), so pointing Open at a
// bundle directory, or at any other directory that has the document but not
// the files it names, fails for exactly that reason. Call OpenDocument
// instead when only the document is available.
//
// Redaction recorded in the document is re-applied on load, so a consumer can
// never see a field the operator asked to remove.
func Open(ctx context.Context, path string) (*Archive, error) {
	return doOpen(ctx, path)
}

// OpenDocument reads and schema-validates a snapshot document without
// requiring the files/ tree Open demands. path is either a snapshot.json
// file directly, or a directory containing one at its root -- a bundle
// directory is exactly that shape, since core/bundle writes the document
// there but never the files/ tree (see Open's doc comment for why Open
// itself cannot be pointed at one).
//
// The returned Snapshot's File entries carry only metadata (path, archive
// path, digest, size): OpenDocument neither extracts nor digest-verifies any
// file content, because in the case it exists to serve -- a bundle's
// snapshot.json copy -- there is none to verify. Use Open on a real
// snapshot.tar.zst archive or extracted snapshot directory when the captured
// files themselves are needed too.
//
// Redaction recorded in the document is re-applied on load, the same
// guarantee Open gives.
func OpenDocument(ctx context.Context, path string) (*Snapshot, error) {
	return doOpenDocument(ctx, path)
}

// Validate checks a document against the schema rules: known schema version,
// required fields present, digests well formed, architectures plausible.
func Validate(s *Snapshot) error {
	return doValidate(s)
}

// Digest returns the canonical digest of a snapshot document.
func Digest(s *Snapshot) (string, error) {
	return doDigest(s)
}

// FingerprintsIn returns the uppercase-hex OpenPGP fingerprint of every key
// -- primary and subkey alike -- in one keyring file's raw bytes, sorted.
// Binary and ASCII-armored keyrings are both accepted, detected by content.
//
// An error means the bytes are not an OpenPGP keyring at all; a caller
// enforcing an approved-key list must treat that as zero fingerprints and
// fail closed. See the implementation's doc for why this is exported.
func FingerprintsIn(data []byte) ([]string, error) {
	return doFingerprintsIn(data)
}

// KeyMaterial derives the complete KeyringFingerprints list for a set of
// captured keyring files, in the order Capture would have recorded them.
//
// It exists because Capture is no longer the only producer of snapshots.
// `snapshot from-base` (core/base, ADR-014) assembles a document from a base
// definition rather than from a machine, and it has to get this exactly
// right: Open's verifyKeyringFingerprints holds a snapshot to the strictest
// rule in the format — the recorded fingerprints must be neither more nor
// less than what the accompanying bytes actually contain, key ids and user
// ids included. A second implementation of that derivation would be a second
// chance to disagree with the checker, so there is one, here, and Capture
// uses it too.
//
// keyrings are the File records; fs must hold each one's bytes at its
// ArchivePath. A file whose bytes are absent is skipped (the caller has
// already decided it was not worth keeping); a file whose bytes are present
// but are not a parseable keyring is an error, because recording it in
// APT.Keyrings while deriving nothing from it is exactly the state Open
// refuses.
func KeyMaterial(keyrings []File, fs *FileSet) ([]KeyFingerprint, error) {
	return doKeyMaterial(keyrings, fs)
}

// Redact removes the fields named by kinds from a loaded snapshot, in place,
// and records them in Redactions.
func Redact(s *Snapshot, fs *FileSet, kinds ...string) {
	doRedact(s, fs, kinds...)
}

// InstallRecommends reports the effective APT::Install-Recommends value from
// the captured apt.conf.d, and whether the target set it explicitly. Ubuntu
// and Debian default to true.
func InstallRecommends(s *Snapshot, fs *FileSet) (value bool, explicit bool) {
	return doInstallRecommends(s, fs)
}

// EffectiveSolver reports the target's effective APT::Solver value from the
// captured apt.conf.d, and whether the target set it explicitly (experiment
// E2.3: divergence tracks this value, not apt
// major.minor). When not explicit, value is that apt version's own shipped
// default (s.Target.APTVersion), not a fixed constant -- see
// defaultSolverForAPTVersion's doc for exactly which releases that default
// table is measured for versus inferred. value is "" only when neither an
// explicit override nor a recognisable apt version was available; a caller
// must treat that as "unknown", never as a third solver choice.
func EffectiveSolver(s *Snapshot, fs *FileSet) (value string, explicit bool) {
	return doEffectiveSolver(s, fs)
}
