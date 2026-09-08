package sign

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// AlgorithmEd25519 is the algorithm string recorded in signature blocks and
// PublicKey values produced by this file.
const AlgorithmEd25519 = "ed25519"

// ed25519FileSigner implements Signer over a private key file in the format
// documented in keyformat.go.
type ed25519FileSigner struct {
	keyID string
	priv  ed25519.PrivateKey
}

func newEd25519FileSigner(path string) (*ed25519FileSigner, error) {
	// gosec's G703 traces `path` back to the command line and calls it a
	// traversal sink. Reading the file the operator named is the entire point
	// of --key: there is no confinement root this path is supposed to stay
	// inside, so there is nothing for it to traverse out of. debark runs as
	// the operator and reads what the operator already has read access to; it
	// is not a privilege boundary and does not pretend to be one.
	//
	// Contrast core/snapshot/archive.go, where a path DOES come out of an
	// untrusted archive and IS confined (safeArchivePath). That distinction is
	// why G703 is waived here per-site rather than switched off repo-wide.
	// #nosec G703 -- operator-supplied key path, no confinement root
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapErr(dferr.Usage, err, "sign: read private key %s", path).
			WithHint("generate one with `debark keygen --out %s`, or pass an existing .key file", path)
	}
	if err := checkPrivateKeyPermissions(path); err != nil {
		return nil, err
	}
	commentLine, blob, err := decodeKeyFile(raw)
	if err != nil {
		return nil, wrapErr(dferr.Usage, err, "sign: parse private key %s", path)
	}
	blobID, priv, err := parsePrivateKeyBlob(blob)
	if err != nil {
		return nil, wrapErr(dferr.Usage, err, "sign: parse private key %s", path)
	}
	commentKeyID, _, cerr := parseCommentLine(commentLine, labelPrivate)
	if cerr != nil {
		return nil, wrapErr(dferr.Usage, cerr, "sign: private key %s: unusable comment line", path).
			WithHint("the first line must read `untrusted comment: " + labelPrivate + " <keyid>`; a file whose comment line does not parse has been edited or truncated")
	}
	if !strings.EqualFold(commentKeyID, keyIDHex(blobID)) {
		return nil, dferr.New(dferr.Usage, "sign: private key %s: key id in comment line (%s) does not match key id in key material (%s)", path, commentKeyID, keyIDHex(blobID))
	}
	return &ed25519FileSigner{keyID: keyIDHex(blobID), priv: priv}, nil
}

// checkPrivateKeyPermissions refuses a key file any account but its owner can
// read. GenerateKey writes 0600 with O_EXCL, so this only ever fires on a file
// that arrived some other way - copied out of a repository, restored from a
// backup tarball, unpacked by an installer with a loose umask - which is
// exactly the file whose exposure nobody noticed. ssh has enforced the same
// rule on private keys for decades for the same reason; failing at the moment
// the key is loaded is the last point where the operator can still rotate it
// cheaply, instead of discovering the exposure from the signature it produced.
//
// It is a hard refusal rather than a warning because core packages have no
// channel to warn on (they never print; ADR: core is a library), so the
// alternatives were "refuse" and "say nothing at all".
func checkPrivateKeyPermissions(path string) error {
	if runtime.GOOS == "windows" {
		// Windows has no POSIX mode bits: os.Stat synthesises 0666 (or 0444
		// for a read-only file) from the FAT-era attribute word, so the value
		// says nothing whatever about who can read the file - the real answer
		// is in an ACL this package does not read. Checking the synthesised
		// mode there would refuse every key file on the platform.
		return nil
	}
	// Same path, same operator, same reasoning as newEd25519FileSigner above:
	// this stats the key file --key named, in order to refuse it if it is
	// group- or world-readable. Refusing to look would defeat the check.
	// #nosec G703 -- operator-supplied key path, no confinement root
	fi, err := os.Stat(path)
	if err != nil {
		return wrapErr(dferr.Usage, err, "sign: stat private key %s", path)
	}
	return privateKeyPermissionError(path, fi.Mode().Perm())
}

// privateKeyPermissionError is the mode rule itself, split out from the stat
// so it can be tested directly on every platform (including the ones where
// the caller above skips it).
func privateKeyPermissionError(path string, perm os.FileMode) error {
	if perm&0o077 == 0 {
		return nil
	}
	return dferr.New(dferr.Usage, "sign: private key %s has mode %04o: it is readable by accounts other than its owner", path, uint32(perm)).
		WithHint("restrict it with `chmod 600 %s`; if anyone else could already read it, treat the key as compromised and rotate it", path)
}

func (s *ed25519FileSigner) Kind() string  { return manifest.SignerEd25519File }
func (s *ed25519FileSigner) KeyID() string { return s.keyID }

func (s *ed25519FileSigner) Sign(ctx context.Context, purpose string, canon []byte) (manifest.Signature, error) {
	if err := ctx.Err(); err != nil {
		return manifest.Signature{}, err
	}
	msg := SigningInput(purpose, canon)
	raw := ed25519.Sign(s.priv, msg)
	return manifest.Signature{
		SignerKind: manifest.SignerEd25519File,
		KeyID:      s.keyID,
		Algorithm:  AlgorithmEd25519,
		CreatedAt:  nowStamp(),
		Signature:  base64.StdEncoding.EncodeToString(raw),
	}, nil
}

func (s *ed25519FileSigner) Close() error { return nil }

// verifyEd25519Signature checks one ed25519 signature block against a trust
// set keyed by lowercase hex key id. It returns ErrUntrustedKey when sig.KeyID
// matches nothing in trusted (well-formedness aside, there is no key to check
// crypto against), and a dferr.Verification error when the key is trusted but
// the signature bytes do not validate - the clearest possible tamper signal,
// since it means either the signed payload or the signature itself changed
// after signing.
func verifyEd25519Signature(trusted map[string]ed25519.PublicKey, purpose string, canon []byte, sig manifest.Signature) error {
	pub, ok := trusted[strings.ToLower(sig.KeyID)]
	if !ok {
		return ErrUntrustedKey
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return dferr.New(dferr.Verification, "sign: signature %s: malformed base64: %v", sig.KeyID, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return dferr.New(dferr.Verification, "sign: signature %s: wrong length %d, want %d", sig.KeyID, len(raw), ed25519.SignatureSize)
	}
	msg := SigningInput(purpose, canon)
	if !ed25519.Verify(pub, msg, raw) {
		return dferr.New(dferr.Verification, "sign: signature %s: does not verify against the trusted key", sig.KeyID)
	}
	return nil
}

// publicKeyPathFor derives the .pub path GenerateKey writes alongside privPath.
func publicKeyPathFor(privPath string) string {
	if strings.HasSuffix(privPath, PrivateKeyFileSuffix) {
		return strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix
	}
	return privPath + PublicKeyFileSuffix
}

// generateEd25519KeyFiles creates a new key pair and writes both files. It
// refuses to overwrite an existing file at either path.
func generateEd25519KeyFiles(privPath, comment string) (keyID string, err error) {
	if strings.ContainsAny(comment, "\r\n") {
		return "", dferr.New(dferr.Usage, "sign: key comment must fit on one line")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", wrapErr(dferr.Environment, err, "sign: generate ed25519 key")
	}
	id := keyIDFromPublic(pub)
	pubPath := publicKeyPathFor(privPath)

	if dir := filepath.Dir(privPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", wrapErr(dferr.Usage, err, "sign: create directory for %s", privPath)
		}
	}
	if err := writeNewFile(privPath, encodePrivateKeyFile(priv, comment), 0o600); err != nil {
		return "", err
	}
	if err := writeNewFile(pubPath, encodePublicKeyFile(pub, comment), 0o644); err != nil {
		// Keep an existing public key intact and remove our unmatched private
		// key so the operator can retry after fixing the public path.
		if removeErr := os.Remove(privPath); removeErr != nil {
			return "", wrapErr(dferr.Environment, err, "sign: could not remove incomplete private key %s: %v", privPath, removeErr)
		}
		return "", err
	}
	return keyIDHex(id), nil
}

func writeNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return wrapErr(dferr.Usage, err, "sign: create %s", path).
			WithHint("remove the existing file first if you intend to replace it")
	}
	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return wrapErr(dferr.Usage, err, "sign: write %s", path)
	}
	if err := f.Sync(); err != nil {
		return wrapErr(dferr.Environment, err, "sign: flush %s", path)
	}
	if err := f.Close(); err != nil {
		return wrapErr(dferr.Environment, err, "sign: close %s", path)
	}
	complete = true
	return nil
}

// loadPublicKeyFile reads and parses one .pub file.
func loadPublicKeyFile(path string) (PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PublicKey{}, wrapErr(dferr.Usage, err, "sign: read public key %s", path)
	}
	commentLine, blob, err := decodeKeyFile(raw)
	if err != nil {
		return PublicKey{}, wrapErr(dferr.Usage, err, "sign: parse public key %s", path)
	}
	id, pub, err := parsePublicKeyBlob(blob)
	if err != nil {
		return PublicKey{}, wrapErr(dferr.Usage, err, "sign: parse public key %s", path)
	}
	// A comment line that does not parse is a refusal, not a shrug. The line
	// is untrusted in the minisign sense - nothing is BELIEVED because it says
	// so, and the id below is the one derived from the key material - but it
	// is still a line debark itself wrote to a documented shape, so a file
	// whose first line no longer matches that shape has been edited or
	// truncated. Skipping the cross-check whenever the line failed to parse
	// (which is what this used to do) handed an attacker the cheapest possible
	// way to disable it: mangle the line you do not want checked.
	commentKeyID, comment, cerr := parseCommentLine(commentLine, labelPublic)
	if cerr != nil {
		return PublicKey{}, wrapErr(dferr.Usage, cerr, "sign: public key %s: unusable comment line", path).
			WithHint("the first line must read `untrusted comment: " + labelPublic + " <keyid>`; a file whose comment line does not parse has been edited or truncated")
	}
	if !strings.EqualFold(commentKeyID, keyIDHex(id)) {
		return PublicKey{}, dferr.New(dferr.Usage, "sign: public key %s: key id in comment line (%s) does not match key id in key material (%s)", path, commentKeyID, keyIDHex(id))
	}
	return PublicKey{
		KeyID:     keyIDHex(id),
		Algorithm: AlgorithmEd25519,
		Comment:   comment,
		Raw:       []byte(pub),
		Source:    path,
	}, nil
}

// loadPublicKeysFrom implements LoadPublicKeys's file/directory scan.
func loadPublicKeysFrom(ks KeySource) ([]PublicKey, error) {
	var out []PublicKey
	for _, f := range ks.Files {
		pk, err := loadPublicKeyFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	for _, dir := range ks.Dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, wrapErr(dferr.Usage, err, "sign: read keyring directory %s", dir)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), PublicKeyFileSuffix) {
				continue
			}
			pk, err := loadPublicKeyFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			out = append(out, pk)
		}
	}
	return out, nil
}
