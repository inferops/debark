// Frozen public API of the sign package.

package sign

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
)

// nowStamp is the canonical timestamp every Signer records as CreatedAt.
func nowStamp() string { return canonical.Time(time.Now()) }

// wrapErr is dferr.Wrap but returns *dferr.Error instead of the error
// interface, so callers in this package can chain .WithHint(...).
func wrapErr(c dferr.Class, err error, format string, args ...any) *dferr.Error {
	return &dferr.Error{Class: c, Msg: fmt.Sprintf(format, args...), Err: err}
}

// SignerFor builds a Signer from an operator's key reference:
//
//	/path/to/key.key   an ed25519 private key file in debark's native format
//	gpg:<keyid>        signing delegated to the gpg binary
//	plugin:<path>      an executable speaking debark.plugin/v1
//
// The caller always calls Close.
//
// On error the returned Signer is a genuinely nil interface value. That has to
// be spelled out rather than returned straight from the constructors: each of
// them returns a concrete *T, and returning a nil *T through this Signer
// return type produces an interface that is NOT nil - so `signer != nil`
// answers true for a signer that does not exist. Nothing breaks today because
// every caller checks err first, but this is a frozen public API and
// core/engine keeps exactly that shape (`b.signerOwned && b.signer != nil`),
// so the trap is closed here rather than left for the first caller who trips
// it.
func SignerFor(ctx context.Context, ref string) (Signer, error) {
	switch {
	case ref == "":
		return nil, dferr.New(dferr.Usage, "sign: SignerFor: empty key reference")
	case strings.HasPrefix(ref, "gpg:"):
		s, err := newGPGSigner(ctx, strings.TrimPrefix(ref, "gpg:"))
		if err != nil {
			return nil, err
		}
		return s, nil
	case strings.HasPrefix(ref, "plugin:"):
		s, err := newPluginSigner(ctx, strings.TrimPrefix(ref, "plugin:"))
		if err != nil {
			return nil, err
		}
		return s, nil
	default:
		s, err := newEd25519FileSigner(ref)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// VerifierFor builds a Verifier from operator-supplied key sources. It refuses
// a source that resolves inside bundlePath unless KeySource.AllowSameMedia is
// set: a key that travelled with the media proves nothing (ADR-008).
func VerifierFor(ctx context.Context, ks KeySource, bundlePath string) (Verifier, error) {
	if bundlePath == "" {
		return nil, dferr.New(dferr.Usage, "sign: VerifierFor: bundlePath is required")
	}

	candidates := make([]string, 0, len(ks.Files)+len(ks.Dirs)+1)
	candidates = append(candidates, ks.Files...)
	candidates = append(candidates, ks.Dirs...)
	if ks.GPGKeyring != "" {
		candidates = append(candidates, ks.GPGKeyring)
	}
	if !ks.AllowSameMedia {
		for _, c := range candidates {
			if sameMedia(c, bundlePath) {
				e := &dferr.Error{
					Class: dferr.Verification,
					Msg:   fmt.Sprintf("sign: key source %q resolves inside the bundle being verified", c),
					Err:   ErrSameMediaKey,
				}
				return nil, e.WithHint("provision the operator key out of band (a pre-provisioned file, config, or a fingerprint compared by hand); pass AllowSameMedia only to deliberately override this, e.g. for local testing")
			}
		}
	}

	keys, err := loadPublicKeysFrom(ks)
	if err != nil {
		return nil, err
	}
	ed, err := buildEd25519TrustSet(keys)
	if err != nil {
		return nil, err
	}

	gv, err := newGPGVerifier(ks.GPGKeyring)
	if err != nil {
		return nil, err
	}

	return &compositeVerifier{ed25519Keys: ed, gpg: gv}, nil
}

// buildEd25519TrustSet turns the loaded public keys into the map the verifier
// checks ed25519 signatures against, keyed by lowercase hex key id.
//
// It refuses two DIFFERENT keys claiming one id. A map cannot hold both, so
// without this one silently overwrites the other, and which one wins is
// decided by the order loadPublicKeysFrom happened to walk the sources - for a
// keyring directory, alphabetical order, which an attacker who can drop a file
// into it chooses by naming the file "zz-vendor.pub".
//
// That matters because the id is only the first 8 bytes of SHA-256(pub): a
// second key colliding with a given id is about 2^32 work to grind, not a
// break of SHA-256, so this is a thing to arrange rather than a theoretical
// concern. The truncation is the published key format and cannot change, so
// the ambiguity is refused here instead - the operator is the only one who can
// say which of the two keys they meant, and silently trusting either is not an
// answer.
//
// The same key present twice (a --key file that is also in a --keyring
// directory) is ordinary and is deduplicated in silence: identical material is
// not an ambiguity.
func buildEd25519TrustSet(keys []PublicKey) (map[string]ed25519.PublicKey, error) {
	ed := make(map[string]ed25519.PublicKey, len(keys))
	from := make(map[string]string, len(keys))
	for _, k := range keys {
		if k.Algorithm != AlgorithmEd25519 || len(k.Raw) != ed25519.PublicKeySize {
			continue
		}
		id := strings.ToLower(k.KeyID)
		if prev, seen := ed[id]; seen {
			if bytes.Equal(prev, k.Raw) {
				continue
			}
			e := &dferr.Error{
				Class: dferr.Usage,
				Msg: fmt.Sprintf("sign: two different keys in the trust set both claim key id %s: %s and %s",
					id, from[id], k.Source),
			}
			return nil, e.WithHint("remove one of the two files: a key id names exactly one key, so a second key claiming it is either a stale copy or a collision planted to be trusted under the first key's name")
		}
		ed[id] = ed25519.PublicKey(k.Raw)
		from[id] = k.Source
	}
	return ed, nil
}

// GenerateKey writes a new ed25519 key pair: privPath and privPath with the
// .key suffix replaced by .pub. The private file is written with 0600 and a
// passphrase is never required, because an operator who wants one uses gpg.
func GenerateKey(privPath, comment string) (keyID string, err error) {
	return generateEd25519KeyFiles(privPath, comment)
}

// PublicKey is a parsed operator public key.
type PublicKey struct {
	KeyID     string
	Algorithm string
	Comment   string
	// Raw is the key material: 32 bytes for ed25519.
	Raw []byte
	// Source is where it was read from, for reporting.
	Source string
}

// LoadPublicKeys reads every public key named by a KeySource.
func LoadPublicKeys(ks KeySource) ([]PublicKey, error) {
	return loadPublicKeysFrom(ks)
}
