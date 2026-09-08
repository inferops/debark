package sign

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// debark's native ed25519 key file format.
//
// It is inspired by minisign's key files - readable, one algorithm, no
// external dependency - but is deliberately NOT byte-for-byte compatible with
// the minisign CLI: no scrypt-encrypted secret key, no separate trusted-
// comment-plus-global-signature line. "minisign-compatible-ish" describes the
// shape (a labelled comment line, then one line of base64 wrapping a small
// tagged binary blob), not interoperability with minisign's tooling.
//
// This format is published and MUST remain stable: every byte offset below is
// part of the wire contract old key files must keep working against. Add a new
// algorithm tag rather than changing the meaning of an existing one.
//
// Public key file (conventionally named "<name>.pub"):
//
//	untrusted comment: debark ed25519 public key <keyid>[ <comment>]
//	<base64>
//
// Private key file (conventionally named "<name>.key", written with mode
// 0600):
//
//	untrusted comment: debark ed25519 secret key <keyid>[ <comment>]
//	<base64>
//
// <keyid> is 16 lowercase hex characters, printed on the comment line purely
// so a human, `file`, or a script can identify which key a file holds without
// decoding it. Neither copy of it is ever trusted: on load the id is
// RECOMPUTED from the key material itself (keyIDFromPublic), and both the
// blob's copy and the comment line's copy must equal what came out. The id
// KeyID() and verify use is therefore always the one the key material
// dictates - a file cannot name itself after a key it does not hold, in
// either field, which is what makes the trust set VerifierFor keys by that id
// mean anything.
//
// The base64 on the second line decodes to a small binary blob:
//
//	public key blob  (42 bytes total):
//	  offset  0   2 bytes   algorithm tag, ASCII "Ed" (Ed25519; the only value
//	                        debark writes or accepts today)
//	  offset  2   8 bytes   key id: the first 8 bytes of SHA-256(public key)
//	  offset 10  32 bytes   the raw Ed25519 public key
//
//	private key blob (74 bytes total):
//	  offset  0   2 bytes   algorithm tag, ASCII "Ed"
//	  offset  2   8 bytes   key id, same derivation as the public blob
//	  offset 10  64 bytes   the raw Ed25519 private key, in crypto/ed25519's
//	                        own seed||publicKey encoding
//
// The key id is always derived from the public key, never generated at
// random: two files that hold the same key material always agree on it, and
// KeyID() never needs a lookup or network round trip to produce it. There is
// deliberately no passphrase or KDF on the private key file - an operator who
// wants secret-key encryption already has one available (gpg:<keyid>, this
// package's other Signer). What protects a plain key file meant for
// unattended signing is the file mode (0600) and normal filesystem
// permissions; the "untrusted comment" label, exactly as in minisign, is never
// trust-relevant on its own.
const (
	keyAlgTag  = "Ed" // 2 ASCII bytes
	keyIDLen   = 8
	pubKeyLen  = ed25519.PublicKeySize  // 32
	privKeyLen = ed25519.PrivateKeySize // 64

	pubBlobLen  = 2 + keyIDLen + pubKeyLen  // 42
	privBlobLen = 2 + keyIDLen + privKeyLen // 74

	untrustedCommentPrefix = "untrusted comment: "
	labelPublic            = "debark ed25519 public key"
	labelPrivate           = "debark ed25519 secret key"
)

// keyID is the first keyIDLen bytes of SHA-256(pub), the deterministic key id
// embedded in both key file blobs.
func keyIDFromPublic(pub ed25519.PublicKey) [keyIDLen]byte {
	sum := sha256.Sum256(pub)
	var id [keyIDLen]byte
	copy(id[:], sum[:keyIDLen])
	return id
}

func keyIDHex(id [keyIDLen]byte) string { return hex.EncodeToString(id[:]) }

// encodeKeyFile renders the two-line key file text for one blob.
func encodeKeyFile(label, keyIDHexStr, comment string, blob []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(untrustedCommentPrefix)
	buf.WriteString(label)
	buf.WriteByte(' ')
	buf.WriteString(keyIDHexStr)
	if comment != "" {
		buf.WriteByte(' ')
		buf.WriteString(comment)
	}
	buf.WriteByte('\n')
	buf.WriteString(base64.StdEncoding.EncodeToString(blob))
	buf.WriteByte('\n')
	return buf.Bytes()
}

func encodePublicKeyFile(pub ed25519.PublicKey, comment string) []byte {
	id := keyIDFromPublic(pub)
	blob := make([]byte, 0, pubBlobLen)
	blob = append(blob, keyAlgTag...)
	blob = append(blob, id[:]...)
	blob = append(blob, pub...)
	return encodeKeyFile(labelPublic, keyIDHex(id), comment, blob)
}

func encodePrivateKeyFile(priv ed25519.PrivateKey, comment string) []byte {
	id := keyIDFromPublic(priv.Public().(ed25519.PublicKey))
	blob := make([]byte, 0, privBlobLen)
	blob = append(blob, keyAlgTag...)
	blob = append(blob, id[:]...)
	blob = append(blob, priv...)
	return encodeKeyFile(labelPrivate, keyIDHex(id), comment, blob)
}

// decodeKeyFile splits a key file into its comment line and decoded blob. It
// tolerates blank lines and trailing whitespace but not much else: this is a
// small, fixed, security-relevant format, not a lenient one.
func decodeKeyFile(data []byte) (commentLine string, blob []byte, err error) {
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) == 2 {
			break
		}
	}
	if len(lines) < 2 {
		return "", nil, fmt.Errorf("expected an untrusted-comment line followed by a base64 line, found %d line(s)", len(lines))
	}
	blob, err = base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return "", nil, fmt.Errorf("decode base64 line: %w", err)
	}
	return lines[0], blob, nil
}

// parseCommentLine extracts the key id and optional trailing comment from an
// "untrusted comment: <wantLabel> <keyid>[ <comment>]" line.
func parseCommentLine(line, wantLabel string) (keyIDHexStr, comment string, err error) {
	if !strings.HasPrefix(line, untrustedCommentPrefix) {
		return "", "", fmt.Errorf("missing %q prefix", strings.TrimSpace(untrustedCommentPrefix))
	}
	rest := strings.TrimPrefix(line, untrustedCommentPrefix)
	prefix := wantLabel + " "
	if !strings.HasPrefix(rest, prefix) {
		return "", "", fmt.Errorf("unexpected key label (want %q)", wantLabel)
	}
	rest = strings.TrimPrefix(rest, prefix)
	fields := strings.SplitN(rest, " ", 2)
	keyIDHexStr = fields[0]
	if len(fields) == 2 {
		comment = fields[1]
	}
	return keyIDHexStr, comment, nil
}

// keyIDMismatchError is the error both blob parsers return when the id a file
// carries is not the id of the key material sitting next to it.
//
// The stored id is never returned to the caller in that case, and never
// returned unchecked in any case: the id is DERIVED from the public key on
// every load, which is what makes the format documentation above true. It used
// to be copied out verbatim, so a hand-built file could carry the release
// key's id over an attacker's key material - and since the trust set
// VerifierFor builds is a map keyed by exactly this id, that file would sit in
// the operator's keyring under the release key's name and answer for it.
func keyIDMismatchError(what string, stored, derived [keyIDLen]byte) error {
	return fmt.Errorf("%s blob declares key id %s but the key material it carries has id %s",
		what, keyIDHex(stored), keyIDHex(derived))
}

func parsePublicKeyBlob(blob []byte) (id [keyIDLen]byte, pub ed25519.PublicKey, err error) {
	if len(blob) != pubBlobLen {
		return id, nil, fmt.Errorf("public key blob is %d bytes, want %d", len(blob), pubBlobLen)
	}
	if string(blob[:2]) != keyAlgTag {
		return id, nil, fmt.Errorf("unsupported key algorithm tag %q", blob[:2])
	}
	var stored [keyIDLen]byte
	copy(stored[:], blob[2:2+keyIDLen])
	pub = make(ed25519.PublicKey, pubKeyLen)
	copy(pub, blob[2+keyIDLen:])
	id = keyIDFromPublic(pub)
	if id != stored {
		return [keyIDLen]byte{}, nil, keyIDMismatchError("public key", stored, id)
	}
	return id, pub, nil
}

func parsePrivateKeyBlob(blob []byte) (id [keyIDLen]byte, priv ed25519.PrivateKey, err error) {
	if len(blob) != privBlobLen {
		return id, nil, fmt.Errorf("private key blob is %d bytes, want %d", len(blob), privBlobLen)
	}
	if string(blob[:2]) != keyAlgTag {
		return id, nil, fmt.Errorf("unsupported key algorithm tag %q", blob[:2])
	}
	var stored [keyIDLen]byte
	copy(stored[:], blob[2:2+keyIDLen])
	priv = make(ed25519.PrivateKey, privKeyLen)
	copy(priv, blob[2+keyIDLen:])

	// crypto/ed25519's private key is seed||publicKey, and nothing in the
	// library checks that the two halves belong together: ed25519.Sign uses
	// the seed to derive the scalar and the STORED public half to build the
	// signature, so a file whose halves disagree signs happily and produces a
	// signature no verifier on earth can check. The only place that would
	// surface is the air-gapped target, after the medium has been couriered -
	// the most expensive possible moment to learn a key file was corrupt. The
	// seed is authoritative, so re-derive from it and compare.
	if !bytes.Equal(ed25519.NewKeyFromSeed(priv.Seed()), priv) {
		return [keyIDLen]byte{}, nil, fmt.Errorf("private key blob is internally inconsistent: the public half does not belong to the seed")
	}
	id = keyIDFromPublic(priv.Public().(ed25519.PublicKey))
	if id != stored {
		return [keyIDLen]byte{}, nil, keyIDMismatchError("private key", stored, id)
	}
	return id, priv, nil
}
