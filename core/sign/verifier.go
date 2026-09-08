package sign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// ErrSameMediaKey is returned (wrapped in a dferr.Verification error) by
// VerifierFor when a key source resolves inside the bundle being verified and
// KeySource.AllowSameMedia was not set (ADR-008). A key that travelled with
// the media it is meant to authenticate proves nothing: whoever can tamper
// with the bundle can just as easily drop in a key that "verifies" their
// tampering. verify uses errors.Is against this value to report
// verify.ProblemSameMediaKey precisely, rather than a generic failure.
var ErrSameMediaKey = errors.New("sign: key source resolves inside the bundle being verified")

// compositeVerifier is the Verifier VerifierFor builds: it knows ed25519-file
// signatures against a loaded trust set, and gpg signatures against an
// operator-designated keyring. A signature made by a plugin is checked the
// same way as ed25519-file whenever it used the ed25519 algorithm and the
// operator has separately trusted that key (by exporting the plugin's public
// key with key_info and adding it to a keyring dir) - the verification math
// is identical regardless of which process held the private key.
type compositeVerifier struct {
	ed25519Keys map[string]ed25519.PublicKey // lowercase hex key id -> public key
	gpg         *gpgVerifier
}

func (v *compositeVerifier) Kinds() []string {
	return []string{manifest.SignerEd25519File, manifest.SignerGPG}
}

// Verify routes a signature block to the mechanism that can check it, and
// then refuses any block whose own description of itself contradicts the check
// that ran.
//
// The routing fields are attacker-editable - signer_kind and algorithm live in
// debark.manifest.sig, which is NOT covered by the signature (the signed
// bytes are purpose||NUL||canonical-manifest, and the .sig file is excluded
// from the manifest's own file list). So they are treated as a request for a
// verdict, never as a fact: whatever they ask for, the block is only accepted
// if the check that actually ran agrees with what they claim.
//
// What that closes: a block claiming algorithm "rsa-4096" while ed25519
// arithmetic is what verified it. That used to verify, and core/verify copies
// signer_kind, key_id and algorithm straight into the report an auditor reads,
// so it was a free relabelling of provenance. (Claiming "gpg" over an ed25519
// key was already closed by routing: such a block goes to gpg, which has no
// ed25519 signature packet to find.)
//
// What it does NOT close, because no verifier can: the mechanism half of
// signer_kind. "ed25519-file" and "plugin:acme-hsm" are the same arithmetic
// over the same key - the difference between them is where the private half
// was kept, which leaves no trace in a signature. The identity that IS
// established is the key id, and it is established by the trust set the
// operator assembled out of band. Binding the label itself would take a
// changed signed pre-image, which cannot be done from this package alone: see
// the note on SigningInput in iface.go.
//
// The two shapes of refusal below are deliberately different classes, and the
// difference is the whole point of separating them:
//
//   - The NATIVE ed25519-file kind paired with any algorithm but ed25519 is a
//     contradiction no honest producer can emit - that signer has exactly one
//     algorithm - so it is a dferr.Verification fact about the medium,
//     reported with both halves named.
//   - A PLUGIN block using an algorithm this verifier cannot check (a real
//     ecdsa-p256 HSM, say) is not tamper and must never be reported as such -
//     it is ErrUntrustedKey, "no key here can answer for this", exactly as it
//     was before. Calling that one tampering would turn an honest bundle from
//     an unsupported signer into debark's loudest verdict.
//
// An empty algorithm is tolerated on the native kinds: it claims nothing, so
// it cannot misdescribe anything. It is NOT tolerated for a plugin, because
// then ed25519 arithmetic would be attempted on a block that never said it
// was ed25519 - and a failure of that arithmetic reads as tamper.
func (v *compositeVerifier) Verify(ctx context.Context, purpose string, canon []byte, sig manifest.Signature) error {
	switch {
	case sig.SignerKind == manifest.SignerGPG:
		// No algorithm check on this branch, deliberately. The field records
		// the signer MECHANISM, not the key type (see AlgorithmGPG), and gpg
		// keys really are RSA or ed25519 or something else - so a block
		// pairing signer_kind "gpg" with algorithm "ed25519" is not
		// self-contradictory the way the native branch below would be. What
		// pins this branch's provenance is not the label anyway: gpg reports
		// which key it validated against, and the fingerprint cross-check
		// below refuses any answer that is not the key the block names.
		return v.gpg.verify(ctx, purpose, canon, sig)
	case sig.SignerKind == manifest.SignerEd25519File:
		if sig.Algorithm != "" && !strings.EqualFold(sig.Algorithm, AlgorithmEd25519) {
			return dferr.New(dferr.Verification,
				"sign: signature %s claims signer_kind %q with algorithm %q; an %s signature block records algorithm %q",
				sig.KeyID, sig.SignerKind, sig.Algorithm, manifest.SignerEd25519File, AlgorithmEd25519)
		}
		return verifyEd25519Signature(v.ed25519Keys, purpose, canon, sig)
	case isPluginSignerKind(sig.SignerKind) && strings.EqualFold(sig.Algorithm, AlgorithmEd25519):
		// A plugin's ed25519 signature is checked with the same arithmetic and
		// the same trust set as a native one: which process held the private
		// half changes nothing about the check (see the type comment).
		return verifyEd25519Signature(v.ed25519Keys, purpose, canon, sig)
	default:
		return ErrUntrustedKey
	}
}

// isPluginSignerKind reports whether a signer kind names a plugin, i.e. is
// "plugin:" followed by a non-empty name. A bare "plugin:" names nothing and
// is not one.
func isPluginSignerKind(kind string) bool {
	return strings.HasPrefix(kind, manifest.SignerPluginPrefix) && len(kind) > len(manifest.SignerPluginPrefix)
}

// sameMedia reports whether a key source and the bundle are so plainly the
// same courier-able object that trusting one to authenticate the other proves
// nothing (ADR-008). That is true in BOTH directions:
//
//   - the key resolves inside the bundle (…/bundle/keys/op.pub): whoever
//     rewrote the manifest just replaced the key beside it too;
//   - the key IS, or contains, the bundle root (--keyring /media/usb with the
//     bundle at /media/usb/bundle): the operator has designated a directory
//     that the tamperable medium is a part of, so an attacker who can write
//     the bundle can write a key into the trusted directory above it.
//
// The second direction used not to be checked at all, which made the refusal
// bypassable by moving the key one level up - the single most natural place
// for it to be.
//
// Containment is decided by FILE IDENTITY (os.SameFile up the parent chain),
// not by string prefix. os.SameFile compares the inode/volume+index the
// kernel reports, so it is immune to the two ways a prefix comparison is
// wrong: it cannot be fooled by case (the refusal is meant for removable
// media, and the removable media in an air-gap workflow are vfat/exFAT, which
// are case-insensitive on Linux too - where the old code, lowercasing only on
// Windows, let "…/BUNDLE/op.pub" through), and it cannot be fooled by a
// hard link, a bind mount or a path that reaches the same directory by
// another route.
func sameMedia(candidate, bundleRoot string) bool {
	c := resolveReal(candidate)
	b := resolveReal(bundleRoot)
	return pathContains(b, c) || pathContains(c, b)
}

// pathContains reports whether child is at, or below, parent. Both paths are
// already absolute and symlink-resolved.
//
// It walks child's ancestors comparing identities, which also answers for a
// child that does not exist yet: the walk simply keeps going until it reaches
// an ancestor that does (a key file named on the command line but not present
// is still refused for being inside the bundle, and is reported as missing
// later, by the loader, where that is the actual complaint).
//
// When parent itself cannot be stat'ed there is no identity to compare
// against, and only then does this fall back to the old textual comparison -
// neither path exists, so no medium is involved and there is nothing for a
// case-folding filesystem to fold.
func pathContains(parent, child string) bool {
	pfi, err := os.Stat(parent)
	if err != nil {
		return textualPathContains(parent, child)
	}
	for p := child; ; {
		if fi, serr := os.Stat(p); serr == nil && os.SameFile(pfi, fi) {
			return true
		}
		up := filepath.Dir(p)
		if up == p {
			// Reached the volume root: filepath.Dir is idempotent there, so
			// this is the loop's only termination condition.
			return false
		}
		p = up
	}
}

func textualPathContains(parent, child string) bool {
	if runtime.GOOS == "windows" {
		parent, child = strings.ToLower(parent), strings.ToLower(child)
	}
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// resolveReal makes a best effort to turn path into a canonical absolute
// form: symlinks resolved when the path exists, cleaned and absolute either
// way. It never fails - a path that does not exist yet still compares
// meaningfully by prefix, and the caller's own existence checks are the place
// a missing file becomes an error.
func resolveReal(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}
