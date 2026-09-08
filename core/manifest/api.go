// Frozen public API of the manifest package.

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// ErrUnknownSchema is wrapped into the error Load and LoadSignature return
// when a document's schema_version field is not one this build knows, so
// callers (verify, in particular) can tell "unreadable" apart from "readable
// but a version I don't recognise" with errors.Is instead of string matching.
var ErrUnknownSchema = errors.New("manifest: unsupported schema version")

// BuildInput is what the manifest is built from.
type BuildInput struct {
	// Dir is the bundle root; every file under it except SigFileName and the
	// manifest itself is hashed into Files.
	Dir string
	// SnapshotDigest and LockDigest come from the objects already written.
	SnapshotDigest string
	LockDigest     string
	// Repository carries the index digests from the repository writer.
	Repository Repository
	// Target is the bundle's target identity.
	Target Target
	// Tool is the build identity, from core/version.
	Tool Tool
	// CreatedAt is the canonical timestamp.
	CreatedAt string
	// SBOMRef and EvidenceRef are relative paths, when present.
	SBOMRef     string
	EvidenceRef string
}

// Build walks the bundle tree and produces the manifest. It is deterministic:
// Files is sorted by path and paths are always forward-slashed and relative to
// Dir.
func Build(ctx context.Context, in BuildInput) (*Manifest, error) {
	if in.Dir == "" {
		return nil, dferr.New(dferr.Usage, "manifest: Build: Dir is required")
	}
	root, err := filepath.Abs(in.Dir)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: Build: resolve %s", in.Dir)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: Build: stat %s", root)
	}
	if !info.IsDir() {
		return nil, dferr.New(dferr.Usage, "manifest: Build: %s is not a directory", root)
	}

	// files is never left nil: an empty bundle tree must still marshal Files
	// as "[]", not "null" - a stable, always-an-array field is easier for
	// every downstream consumer (verify included) to handle uniformly.
	files := make([]File, 0)
	// The parameter is p, not path: this file needs the "path" package for
	// path.Clean, whose forward-slash semantics are the same on every OS.
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == FileName || rel == SigFileName {
			return nil
		}
		// The manifest is what gets signed, and its canonical bytes go
		// through encoding/json before RFC 8785 - and Go's encoder silently
		// replaces an invalid UTF-8 byte with U+FFFD, so two bundle paths
		// that differ only in invalid UTF-8 would canonicalise, and sign, to
		// identical bytes. Refusing such a name here means the signed
		// serialisation stays unambiguous rather than merely failing closed
		// later. checkFilePath applies exactly the rule Load enforces on the
		// way back in, so a manifest this package builds is always one this
		// package will load.
		if err := checkFilePath(rel); err != nil {
			return dferr.New(dferr.Usage, "manifest: Build: %s: %v", filepath.ToSlash(p), err)
		}
		sum, size, herr := digest.SHA256File(p)
		if herr != nil {
			return herr
		}
		files = append(files, File{Path: rel, Size: size, SHA256: sum})
		return nil
	})
	if walkErr != nil {
		return nil, dferr.Wrap(dferr.Usage, walkErr, "manifest: Build: walk %s", root)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	m := &Manifest{
		SchemaVersion:  SchemaVersion,
		BundleID:       NewBundleID(in.LockDigest, in.CreatedAt),
		CreatedAt:      in.CreatedAt,
		FormatVersion:  CurrentFormatVersion,
		Tool:           in.Tool,
		SnapshotDigest: in.SnapshotDigest,
		LockDigest:     in.LockDigest,
		Repository:     in.Repository,
		Files:          files,
		Target:         in.Target,
		SBOMRef:        in.SBOMRef,
		EvidenceRef:    in.EvidenceRef,
	}
	return m, nil
}

// Canonical returns the canonical bytes of m: what gets signed and what
// manifest_sha256 is computed over.
func Canonical(m *Manifest) ([]byte, error) {
	b, err := canonical.Marshal(m)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: canonical")
	}
	return b, nil
}

// Save writes the manifest to dir as FileName, in indented form. The bytes on
// disk are always re-canonicalisable to Canonical(m).
func Save(dir string, m *Manifest) error {
	out, err := canonical.MarshalIndent(m)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "manifest: marshal")
	}
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return dferr.Wrap(dferr.Usage, err, "manifest: write %s", path)
	}
	return nil
}

// Load reads and parses a manifest, returning it with its canonical bytes.
//
// The canonical bytes are produced by re-canonicalising the exact bytes read
// from disk (the RFC 8785 JCS transform), never by re-marshalling the parsed
// Go struct. That distinction matters: re-marshalling the struct would
// silently drop any field Go does not recognise, so a byte inserted into the
// file on disk would vanish before it ever reached a digest comparison. By
// transforming what was actually read, any change to the file - a reordered
// key, an added or removed field, even whitespace that hid a change - changes
// the canonical bytes returned here, and therefore the digest verify compares
// against debark.manifest.sig's manifest_sha256. Do not "simplify" this to
// canonical.Marshal(&m); that would reopen exactly the hole this comment
// describes.
func Load(dir string) (*Manifest, []byte, error) {
	file := filepath.Join(dir, FileName)
	raw, err := readBounded(file)
	if err != nil {
		return nil, nil, err
	}

	// The top-level object is decoded a second time as raw keys, before the
	// typed decode, for two things the typed decode cannot see: whether a key
	// is spelled exactly as the schema says, and whether an optional-looking
	// Go field was actually present in the document at all.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, dferr.Wrap(dferr.Usage, err, "manifest: parse %s", file)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, dferr.Wrap(dferr.Usage, err, "manifest: parse %s", file)
	}

	// encoding/json falls back to case-insensitive field matching, so a
	// document whose only version key is "SCHEMA_VERSION" used to load as
	// schema v1 - and its canonical bytes then carried a key no other
	// implementation of this format would recognise, while debark's own
	// signature vouched for them. The exact key has to be there.
	if _, ok := top["schema_version"]; !ok {
		return nil, nil, dferr.Wrap(dferr.Usage, ErrUnknownSchema,
			"manifest: %s declares no %q key (JSON field names are case-sensitive; %q is not %q)",
			file, "schema_version", "SCHEMA_VERSION", "schema_version")
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, nil, dferr.Wrap(dferr.Usage, ErrUnknownSchema, "manifest: %s has schema %q, want %q", file, m.SchemaVersion, SchemaVersion)
	}
	if err := checkDocument(file, &m, top); err != nil {
		return nil, nil, err
	}

	canon, err := canonical.Transform(raw)
	if err != nil {
		return nil, nil, dferr.Wrap(dferr.Usage, err, "manifest: canonicalise %s", file)
	}
	return &m, canon, nil
}

// maxManifestSize bounds how many bytes Load will read for one
// debark.manifest.json.
//
// SECURITY. The manifest arrives on the same removable medium as everything
// else in the bundle and is read by verify BEFORE any signature has been
// checked - it has to be, because the signature is over its bytes. So its
// size is entirely attacker-chosen, and nothing upstream constrains it:
// core/bundle/tar.go admits a single imported entry of up to 8 GiB. Loading
// is not cheap in proportion either. A measured 120.9 MiB manifest cost
// 2,682 MiB of total allocation and 1,088 MiB of peak heap inside Load, a
// factor of roughly nine, because the raw bytes, the typed decode, the raw
// key map and the JCS transform are all live at once. That is an
// out-of-memory kill of "debark verify" on the target, from a file the
// target has not yet decided to trust.
//
// 16 MiB is about three orders of magnitude above any real manifest. Each
// entry is roughly 130 bytes of JSON, so this admits some 120,000 files in
// one bundle - more than the whole of Debian main has binary packages -
// while capping Load's peak at a few hundred MiB even in the worst case.
// This mirrors core/fetch's maxControlSize, and for the same reason: when a
// limit this far above reality is actually reached, that is evidence of an
// attack rather than of an unusually large legitimate file.
const maxManifestSize = 16 << 20 // 16 MiB

// readBounded reads path, refusing rather than truncating once
// maxManifestSize is exceeded. A silently truncated manifest would be far
// worse than a refusal: it would parse as invalid JSON at best, and at worst
// drop the very entries that made it look wrong.
func readBounded(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		// Wrapped, not replaced: verify tells a missing manifest apart from
		// an unreadable one with errors.Is(err, fs.ErrNotExist).
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: read %s", file)
	}
	defer f.Close()

	limited := &io.LimitedReader{R: f, N: maxManifestSize + 1}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: read %s", file)
	}
	// N == 0 is what distinguishes "read exactly the limit, then stopped"
	// from "the file really was that short".
	if limited.N == 0 {
		return nil, dferr.New(dferr.Verification,
			"manifest: %s exceeds the %d-byte limit; refusing to load it rather than truncating a document that has not been verified yet",
			file, maxManifestSize)
	}
	return raw, nil
}

// checkDocument enforces the structure api/schema/manifest.v1.schema.json
// publishes, on the way in.
//
// SECURITY. The schema forbids every one of these already - "pattern":
// "^[0-9a-f]{64}$" on sha256, "minimum": 0 on size, "required" on files - but
// it was compiled only by api/schema/schema_test.go, never by this loader, so
// the published contract and the enforced contract were different documents.
// Load accepted a path of "../x", "/etc/passwd" or "C:/x", two entries for
// the same path, an empty or non-hex sha256, a negative size, and a manifest
// with no files key at all. Downstream (core/verify) is written to fail
// closed on each of those, and does; this check is the layer that means an
// operator is told the manifest is malformed instead of being handed a
// confusing list of derived problems, and that no future consumer has to
// rediscover the same defensiveness.
//
// The class is Verification, not Usage: a manifest that does not match its
// own published schema is a bundle whose signed object cannot be trusted,
// which is what class 4 means, not an operator who typed a flag wrong.
func checkDocument(file string, m *Manifest, top map[string]json.RawMessage) error {
	bad := func(format string, args ...any) error {
		return dferr.New(dferr.Verification, "manifest: %s: %s", file, fmt.Sprintf(format, args...))
	}

	// "files" is required by the schema. Absent is not the same as empty: an
	// empty list is a claim that the bundle holds nothing, which verify can
	// check, while an absent key is a document that never made the claim.
	if _, ok := top["files"]; !ok {
		return bad("has no %q key, which the schema requires", "files")
	}

	seen := make(map[string]struct{}, len(m.Files))
	for i, f := range m.Files {
		if err := checkFilePath(f.Path); err != nil {
			return bad("files[%d]: %v", i, err)
		}
		if _, dup := seen[f.Path]; dup {
			// Two entries for one path have no single meaning, and which one
			// wins depends on which consumer is looking: core/verify's
			// coverage map takes the last, its findFile takes the first. A
			// manifest that says two different things about one file is not a
			// manifest.
			return bad("files[%d]: path %q is listed more than once", i, f.Path)
		}
		seen[f.Path] = struct{}{}
		if f.Size < 0 {
			return bad("files[%d] (%s): size is negative (%d)", i, f.Path, f.Size)
		}
		if !digest.Valid(f.SHA256) {
			return bad("files[%d] (%s): sha256 %q is not 64 lowercase hex characters", i, f.Path, f.SHA256)
		}
	}
	return nil
}

// checkFilePath enforces what Manifest.File.Path documents itself to be:
// relative to the bundle root, forward slashes, no leading "./".
//
// It is deliberately the same rule Build applies on the way out, so the
// package's own output always survives its own input check. The properties
// that matter are that a path names something inside the bundle and names it
// exactly once: a "..", an absolute path or a drive letter points outside the
// tree the manifest is supposed to cover, and an uncleaned spelling of a real
// path ("repo/./Packages") is a second name for a file that already has one.
func checkFilePath(p string) error {
	switch {
	case p == "":
		return errors.New("path is empty")
	case len(p) > 4096:
		return errors.New("path is longer than 4096 bytes")
	case !utf8.ValidString(p):
		// Go's JSON encoder replaces an invalid UTF-8 byte with U+FFFD, so
		// two such paths can canonicalise - and therefore sign - identically.
		return errors.New("path is not valid UTF-8")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '\\':
			return errors.New("path contains a backslash (bundle paths are always forward-slashed)")
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("path contains the control byte %#02x", c)
		}
	}
	switch {
	case p[0] == '/':
		return errors.New("path is absolute")
	case len(p) >= 2 && p[1] == ':':
		return errors.New("path names a drive letter")
	case p == "." || p == ".." || p == "./" || len(p) >= 3 && p[:3] == "../":
		return errors.New("path escapes the bundle root")
	}
	// path.Clean, not filepath.Clean: the contract is forward slashes on
	// every OS, so the comparison must not vary with the builder's separator.
	if cleaned := path.Clean(p); cleaned != p {
		return fmt.Errorf("path is not in cleaned form (%q would be a second name for %q)", p, cleaned)
	}
	return nil
}

// SaveSignature writes the detached signature file.
func SaveSignature(dir string, sig *SignatureFile) error {
	out, err := canonical.MarshalIndent(sig)
	if err != nil {
		return dferr.Wrap(dferr.Usage, err, "manifest: marshal signature")
	}
	path := filepath.Join(dir, SigFileName)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return dferr.Wrap(dferr.Usage, err, "manifest: write %s", path)
	}
	return nil
}

// LoadSignature reads the detached signature file. A missing file is not an
// error: it returns nil, nil, so an unsigned bundle is a state, not a failure.
func LoadSignature(dir string) (*SignatureFile, error) {
	path := filepath.Join(dir, SigFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: read %s", path)
	}
	var sig SignatureFile
	if err := json.Unmarshal(raw, &sig); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "manifest: parse %s", path)
	}
	if sig.Schema != SignatureSchemaVersion {
		return nil, dferr.Wrap(dferr.Usage, ErrUnknownSchema, "manifest: %s has schema %q, want %q", path, sig.Schema, SignatureSchemaVersion)
	}
	return &sig, nil
}

// bundleIDInput is the tiny object NewBundleID hashes. Its JSON keys are the
// authoritative field names for the derivation; field order in the struct is
// irrelevant because canonical.Digest sorts keys (RFC 8785).
type bundleIDInput struct {
	LockDigest string `json:"lock_digest"`
	CreatedAt  string `json:"created_at"`
}

// NewBundleID derives a stable bundle id from the lock digest and the creation
// timestamp: the first 16 hex characters of the canonical digest of both.
func NewBundleID(lockDigest, createdAt string) string {
	d, err := canonical.Digest(bundleIDInput{LockDigest: lockDigest, CreatedAt: createdAt})
	if err != nil {
		return ""
	}
	if len(d) < 16 {
		return d
	}
	return d[:16]
}
