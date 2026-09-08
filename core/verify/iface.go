// Package verify checks a bundle before apt is allowed anywhere near it
// (ADR-008). Its output is a Report: an auditor's answer to "can this artifact
// be trusted", not a log.
package verify

import (
	"context"

	"github.com/inferops/debark/core/sign"
)

// SchemaVersion is the verify report schema, emitted by verify --json.
const SchemaVersion = "debark.verifyreport/v1"

// Verifier checks one bundle.
type Verifier interface {
	// Verify checks, in this order and stopping at the first hard failure:
	//
	//  1. the manifest parses and its schema version is known;
	//  2. the signature file's manifest_sha256 matches the manifest's
	//     canonical bytes;
	//  3. at least one signature verifies against a trusted key, unless
	//     AllowUnsigned;
	//  4. every file listed in the manifest exists, as a regular file that
	//     carries its own bytes, with the recorded size and digest;
	//  5. no unexpected file, and no entry that is not a regular file, is
	//     present in the bundle;
	//  6. the repository metadata digests match the manifest;
	//  7. the lock's digest matches the manifest's lock_digest, the
	//     snapshot's digest matches snapshot_digest when the snapshot is
	//     present, and every package the lock names carries the size and
	//     digest the signed manifest records for that same pool file.
	//
	// It never executes apt, never writes to the bundle, and never trusts a
	// key found inside it.
	Verify(ctx context.Context, bundlePath string, opts Options) (*Report, error)
}

// Options controls one verification.
type Options struct {
	// Keys says where operator public keys come from.
	Keys sign.KeySource
	// AllowUnsigned permits a bundle with no signature to pass. It must be an
	// explicit operator choice; the report always records that it was used.
	AllowUnsigned bool
	// SkipFileDigests skips hashing the contents of the large pool/*.deb
	// files only - every file's presence, absence and size is still checked,
	// and every small file (lock.json, snapshot.json, the repo/ index files)
	// is still digested regardless. Reserved for inspect on very large
	// bundles; verify itself never sets it.
	SkipFileDigests bool
}

// Report is the machine-readable verification result.
type Report struct {
	SchemaVersion string `json:"schema_version"`
	CheckedAt     string `json:"checked_at"`
	BundlePath    string `json:"bundle_path"`
	BundleID      string `json:"bundle_id,omitempty"`

	// OK is true only when every check passed. A bundle that passes with
	// AllowUnsigned has OK true and Signed false, and the caller must say so.
	OK bool `json:"ok"`

	// Signed and Signatures record what was checked.
	Signed     bool              `json:"signed"`
	Signatures []SignatureResult `json:"signatures,omitempty"`

	// Counts summarise the file check.
	FilesChecked int   `json:"files_checked"`
	BytesChecked int64 `json:"bytes_checked"`

	// Problems is every failure found. Verification stops at the first hard
	// failure, so this is usually one entry; file-digest checking continues to
	// the end so an operator sees the full extent of a tampered medium.
	Problems []Problem `json:"problems,omitempty"`

	// Warnings are non-fatal observations about a bundle that was still
	// accepted: an unsigned bundle taken on AllowUnsigned, a signature that
	// verified only because no trusted key was demanded, a gpg check that ran
	// against this machine's ambient default keyring.
	//
	// Nothing about the bundle's CONTENTS is a warning. A file the manifest
	// does not list, and an entry that is not a regular file, are both
	// Problems (file-unexpected, file-not-regular) and both make OK false -
	// including under repo/, which apt would not walk. This checker does not
	// get to assume anything on the medium is inert.
	Warnings []string `json:"warnings,omitempty"`

	// Manifest summary fields, so a caller need not parse the manifest again.
	Target      ReportTarget `json:"target"`
	CreatedAt   string       `json:"created_at,omitempty"`
	ToolVersion string       `json:"tool_version,omitempty"`
	Edition     string       `json:"edition,omitempty"`
}

// ReportTarget restates who the bundle is for.
type ReportTarget struct {
	DistroID  string `json:"distro_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Codename  string `json:"codename,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// ForeignArchs mirrors manifest.Target.ForeignArchs (which mirrors
	// lock.Target.ForeignArchs in turn): install's architecture precondition
	// check needs foreign-arch support, not just the primary Arch, and this
	// is the signed, verified source for it - install should not need a
	// second, differently-trusted target model just for this one field.
	ForeignArchs []string `json:"foreign_archs,omitempty"`
}

// SignatureResult is one signature block's outcome.
type SignatureResult struct {
	SignerKind string `json:"signer_kind"`
	KeyID      string `json:"key_id"`
	Algorithm  string `json:"algorithm,omitempty"`
	// Valid is true when the signature verified against a trusted key.
	Valid bool `json:"valid"`
	// Trusted is false when the signature is cryptographically fine but the
	// key is not one the operator supplied out of band.
	Trusted bool   `json:"trusted"`
	Detail  string `json:"detail,omitempty"`
}

// Problem kinds. These strings are stable: scripts branch on them.
const (
	ProblemManifestMissing    = "manifest-missing"
	ProblemManifestMalformed  = "manifest-malformed"
	ProblemSchemaUnknown      = "schema-unknown"
	ProblemSignatureMissing   = "signature-missing"
	ProblemSignatureInvalid   = "signature-invalid"
	ProblemSignatureUntrusted = "signature-untrusted"
	ProblemManifestDigest     = "manifest-digest-mismatch"
	ProblemFileMissing        = "file-missing"
	ProblemFileDigest         = "file-digest-mismatch"
	ProblemFileSize           = "file-size-mismatch"
	ProblemFileUnexpected     = "file-unexpected"
	// ProblemFileNotRegular is an entry in the bundle tree that is not a
	// regular file carrying its own bytes: a symlink, a hard link to data
	// outside the bundle, a device, a socket, a FIFO.
	//
	// It is deliberately NOT folded into ProblemFileUnexpected, even though
	// both mean "this must not be here". They are different incidents with
	// different next actions: file-unexpected says a file was added that the
	// manifest does not cover, and an operator's script may reasonably
	// tolerate a stray editor backup; file-not-regular says a listed path
	// does not hold the bytes the manifest describes, because the data lives
	// somewhere this verification never covered and can be swapped after it.
	// A script that whitelists the first must not thereby whitelist the
	// second.
	//
	// Adding this string widens the frozen set, which is an API-visible
	// change: api/schema/verifyreport.v1.schema.json and
	// installreport.v1.schema.json carry the same enum, and both were
	// widened with it. The change is additive - no existing kind changed
	// meaning - and a consumer that does not recognise it still sees ok
	// false, which is the signal that decides whether to install.
	ProblemFileNotRegular = "file-not-regular"
	ProblemRepoDigest     = "repo-digest-mismatch"
	ProblemLockDigest     = "lock-digest-mismatch"
	ProblemSnapshotDigest = "snapshot-digest-mismatch"
	ProblemSameMediaKey   = "same-media-key-refused"
)

// Problem is one specific, actionable failure. Path is set whenever the
// failure is about a file, so an operator can go and look at it.
type Problem struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	// Expected and Got are digests or sizes, rendered as strings.
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got,omitempty"`
}
