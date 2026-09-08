// Package sign is the signing seam. Everything that signs anything in
// debark goes through Signer, so an HSM, a KMS or Sigstore drops in later
// without touching a line of repository, bundle or verify code (ADR-009).
package sign

import (
	"context"

	"github.com/inferops/debark/core/manifest"
)

// Signer produces one signature block over canonical bytes.
//
// Implementations must incorporate purpose into what they sign — debark
// signs purpose, a NUL byte, then the payload — so a signature over a manifest
// can never be replayed as a signature over something else.
type Signer interface {
	// Kind is the signer_kind recorded in the signature block:
	// ed25519-file, gpg, sigstore or plugin:<name>.
	Kind() string
	// KeyID identifies the signing key.
	KeyID() string
	// Sign signs canonical bytes for the given purpose.
	Sign(ctx context.Context, purpose string, canonical []byte) (manifest.Signature, error)
	// Close releases whatever the signer holds (a plugin process, an agent
	// connection). It is always called, even on error paths.
	Close() error
}

// Verifier checks one signature block. A Verifier is built from keys the
// operator supplied out of band; it never learns anything from the media it is
// checking (ADR-008).
type Verifier interface {
	// Kinds lists the signer kinds this verifier can check.
	Kinds() []string
	// Verify returns nil when sig is a valid signature by a trusted key over
	// canonical for purpose. It returns ErrUntrustedKey when the signature is
	// well-formed but made by a key this verifier does not trust, and a
	// dferr.Verification error otherwise.
	Verify(ctx context.Context, purpose string, canonical []byte, sig manifest.Signature) error
}

// SigningInput is what gets hashed and signed: purpose, a NUL separator, then
// the canonical payload. Exported so signer and verifier implementations
// cannot drift apart.
//
// What is deliberately NOT in here: the signature block's own metadata -
// signer_kind, algorithm, created_at, comment. They live in
// debark.manifest.sig, which is not covered by any signature and is not
// listed in the manifest's Files, so anyone who can write the medium can
// rewrite them and every signature still verifies. That is a real limit on
// what a verified bundle attests, and it is documented rather than fixed
// because binding them here cannot be done from this package alone:
//
//   - signer_kind: the three Signer kinds must sign IDENTICAL bytes for the
//     same manifest (pinned by TestDomainSeparation_AllKindsSignIdenticalBytes):
//     that is how a plugin's signature is checkable by the same code as a
//     native one. Mixing the kind into the pre-image makes each kind sign
//     different bytes, by construction.
//   - algorithm: the host does not know it when it builds the pre-image. It
//     hands a plugin the bytes to sign and only learns the algorithm from the
//     answer, so binding it is circular across the plugin protocol
//     (api/plugin/v1) this package does not own.
//   - created_at: core/engine deliberately OVERWRITES Signature.CreatedAt
//     after Sign returns, so the whole bundle has one timestamp under
//     SOURCE_DATE_EPOCH (see core/engine/finalize.go). A signature binding the
//     signer's own clock reading would fail on every engine-built bundle.
//
// And any change here invalidates every signature already issued, with no
// version marker in the block (core/manifest) to negotiate the transition.
//
// What is done instead, in this package: compositeVerifier.Verify refuses a
// block whose claimed algorithm and kind contradict the check that actually
// ran, so the metadata can no longer describe a verification that did not
// happen. The residue - an ed25519 block relabelled from "ed25519-file" to
// "plugin:acme-hsm", which is the same key and the same arithmetic - is
// unattestable by any verifier and is called out where it is reported.
func SigningInput(purpose string, canonical []byte) []byte {
	out := make([]byte, 0, len(purpose)+1+len(canonical))
	out = append(out, purpose...)
	out = append(out, 0)
	out = append(out, canonical...)
	return out
}

// KeySource describes where verify should look for operator public keys. At
// least one must be set for a signed bundle to verify; none of them may point
// inside the bundle being verified.
type KeySource struct {
	// Files are individual public key files.
	Files []string
	// Dirs are keyring directories; every readable key in them is trusted.
	Dirs []string
	// GPGKeyring is a path passed to gpg --keyring, or empty for the
	// operator's default keyring.
	GPGKeyring string
	// AllowSameMedia lifts the refusal to trust a key that lives inside the
	// bundle. It exists only so the refusal has a documented, explicit
	// override; it is never set by default and verify says loudly when it is.
	AllowSameMedia bool
}

// PublicKeyFileSuffix and PrivateKeyFileSuffix are the conventional extensions
// for debark's native ed25519 key files.
const (
	PublicKeyFileSuffix  = ".pub"
	PrivateKeyFileSuffix = ".key"
)

// Untrusted-key sentinel. Callers distinguish "not signed by anyone I trust"
// from "the bytes do not match", because the operator remedies are different.
type untrustedKeyError struct{ msg string }

func (e *untrustedKeyError) Error() string { return e.msg }

// ErrUntrustedKey is returned by Verify when a signature is structurally valid
// but was made by a key the verifier does not trust.
var ErrUntrustedKey error = &untrustedKeyError{"signature made by an untrusted key"}
