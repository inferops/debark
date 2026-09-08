// Package manifest models debark.manifest/v1 — the one object that is
// signed, and the only thing the target has to trust.
//
// The manifest binds the snapshot, the lock, the generated repository metadata
// and every byte in the bundle together. apt's own Release/InRelease remains a
// second, standard layer underneath it (ADR-003, ADR-008).
package manifest

// SchemaVersion is the only manifest schema debark writes.
const SchemaVersion = "debark.manifest/v1"

// SignatureSchemaVersion is the schema of the detached signature file.
const SignatureSchemaVersion = "debark.signature/v1"

// File names inside a bundle.
const (
	FileName    = "debark.manifest.json"
	SigFileName = "debark.manifest.sig"
)

// Edition values recorded in Tool.Edition. The community binary always
// reports community unless it was produced by the project release process.
const (
	EditionCommunity  = "community"
	EditionOfficial   = "official"
	EditionEnterprise = "enterprise"
)

// Manifest is debark.manifest/v1. Its canonical JSON encoding is what gets
// signed; the file on disk is the same document, indented for humans, and
// verify re-canonicalises before checking the signature.
type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	// BundleID is a stable identifier for this bundle: the canonical digest of
	// the manifest with BundleID and Files elided is not usable (chicken and
	// egg), so BundleID is derived from lock digest + created_at.
	BundleID  string `json:"bundle_id"`
	CreatedAt string `json:"created_at"`
	// FormatVersion is the bundle layout version, bumped only when the bundle
	// tree changes shape.
	FormatVersion int `json:"format_version"`
	// Tool records what built the bundle.
	Tool Tool `json:"tool"`
	// SnapshotDigest is the canonical digest of the input snapshot.
	SnapshotDigest string `json:"snapshot_digest"`
	// LockDigest is the canonical digest of lock.json.
	LockDigest string `json:"lock_digest"`
	// Repository carries the digests of the generated index files, so a
	// tampered Packages file fails before apt reads it.
	Repository Repository `json:"repository"`
	// Files is every file in the bundle except the signature file itself,
	// sorted by path, with paths relative to the bundle root and always using
	// forward slashes.
	Files []File `json:"files"`
	// Target restates the target identity for install's precondition checks.
	Target Target `json:"target"`
	// SBOMRef and EvidenceRef name optional companion files, relative to the
	// bundle root.
	SBOMRef     string `json:"sbom_ref,omitempty"`
	EvidenceRef string `json:"evidence_ref,omitempty"`
}

// CurrentFormatVersion is the bundle layout version this build writes.
const CurrentFormatVersion = 1

// Tool identifies the binary that produced the bundle.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// BuildDigest is the digest of the debark binary itself when it could be
	// determined, so an auditor can tie a bundle to a release.
	BuildDigest string `json:"build_digest,omitempty"`
	// Edition is community, official or enterprise.
	Edition string `json:"edition"`
}

// Target is the identity install checks against.
type Target struct {
	DistroID  string `json:"distro_id"`
	VersionID string `json:"version_id"`
	Codename  string `json:"codename"`
	Arch      string `json:"arch"`
	// ForeignArchs mirrors lock.Target.ForeignArchs: the target's dpkg
	// --print-foreign-architectures, sorted. install's architecture
	// precondition check depends on foreign-arch support matching, not just
	// the primary Arch, so it lives in the signed manifest's Target too -
	// install should not need to fall back to the (differently-trusted)
	// lock.Target just for this one field.
	ForeignArchs []string `json:"foreign_archs,omitempty"`
}

// Repository carries digests of the generated apt index files.
type Repository struct {
	PackagesSHA256 string `json:"packages_sha256"`
	// PackagesGzSHA256 is recorded when Packages.gz is written.
	PackagesGzSHA256 string `json:"packages_gz_sha256,omitempty"`
	ReleaseSHA256    string `json:"release_sha256"`
	// InReleaseSHA256 and ReleaseGPGSHA256 are recorded when the repository is
	// signed with a GPG key the target trusts.
	InReleaseSHA256  string `json:"inrelease_sha256,omitempty"`
	ReleaseGPGSHA256 string `json:"release_gpg_sha256,omitempty"`
	// PackageCount and PoolBytes are summary numbers for human output.
	PackageCount int   `json:"package_count"`
	PoolBytes    int64 `json:"pool_bytes"`
}

// File is one file in the bundle.
type File struct {
	// Path is relative to the bundle root, forward slashes, no leading "./".
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SignatureFile is debark.manifest.sig: one or more detached signature
// blocks over the manifest's canonical bytes.
type SignatureFile struct {
	Schema string `json:"schema"`
	// ManifestSHA256 is the digest of the canonical manifest bytes that were
	// signed. verify recomputes it and refuses a mismatch before it even looks
	// at a signature.
	ManifestSHA256 string      `json:"manifest_sha256"`
	Signatures     []Signature `json:"signatures"`
}

// SignerKind values.
const (
	SignerEd25519File = "ed25519-file"
	SignerGPG         = "gpg"
	SignerSigstore    = "sigstore"
	// SignerPluginPrefix is used as plugin:<name>.
	SignerPluginPrefix = "plugin:"
)

// Signature is one signature block.
type Signature struct {
	// SignerKind is ed25519-file, gpg, sigstore or plugin:<name>.
	SignerKind string `json:"signer_kind"`
	// KeyID identifies the key: 16 lowercase hex characters (the first 8
	// bytes of SHA-256(public key)) for ed25519-file, the uppercase 40-hex
	// fingerprint for gpg. Both are hex, deliberately: it keeps the two
	// native Signer kinds visually consistent and matches minisign's own
	// convention for a short key id (see core/sign/keyformat.go).
	KeyID string `json:"key_id"`
	// Algorithm is the signature algorithm, e.g. ed25519.
	Algorithm string `json:"algorithm"`
	CreatedAt string `json:"created_at"`
	// Signature is standard base64 of the raw signature bytes. For gpg it is
	// the base64 of the detached binary signature.
	Signature string `json:"signature"`
	// Comment is optional operator text ("release engineering key, 2026").
	Comment string `json:"comment,omitempty"`
}

// SignPurpose is the domain-separation string mixed into every signature so a
// signature over one kind of object can never be replayed as another.
const SignPurpose = "debark.manifest/v1"
