package sign

// Malformed key files.
//
// keyformat.go calls itself "a small, fixed, security-relevant format, not a
// lenient one", and that is the property under test here. Every case below is
// a file an operator could plausibly end up holding - truncated by a bad copy,
// mangled by an editor, the wrong file entirely, or crafted by somebody who
// wants debark to trust a key it should not - and every one of them must be
// refused with an error that says which file and why, rather than loaded into
// a trust set.

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// writeKeyFile writes raw bytes to a file inside dir and returns the path.
func writeKeyFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// realKeyPair generates a genuine key pair on disk and returns both paths and
// the key id, so a test can corrupt one byte of a file that was otherwise
// perfectly valid.
func realKeyPair(t *testing.T) (privPath, pubPath, keyID string) {
	t.Helper()
	dir := t.TempDir()
	privPath = filepath.Join(dir, "operator.key")
	keyID, err := GenerateKey(privPath, "operator key")
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return privPath, publicKeyPathFor(privPath), keyID
}

// TestPrivateKeyFile_Rejections: newEd25519FileSigner is what `--sign
// /path/key` runs. A file it accepts becomes the thing that signs a bundle, so
// anything it cannot fully understand must be an error rather than a partly
// initialised signer.
func TestPrivateKeyFile_Rejections(t *testing.T) {
	_, goodPub, _ := realKeyPair(t)
	goodPubBytes, err := os.ReadFile(goodPub)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		build   func(t *testing.T, dir string) string // returns the path to hand the signer
		wantMsg string
	}{
		{
			name: "file does not exist",
			build: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "nope.key")
			},
			wantMsg: "read private key",
		},
		{
			// An operator who passes a keyring directory where a key file was
			// wanted. Reading it must fail, not produce an empty key.
			name: "a directory, not a file",
			build: func(t *testing.T, dir string) string {
				sub := filepath.Join(dir, "keys.d")
				if err := os.Mkdir(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				return sub
			},
			wantMsg: "private key",
		},
		{
			name: "empty file",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "empty.key", nil)
			},
			wantMsg: "found 0 line(s)",
		},
		{
			name: "only whitespace",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "blank.key", []byte("\n\n   \n\t\n"))
			},
			wantMsg: "found 0 line(s)",
		},
		{
			// Truncated after the comment line: the base64 that actually
			// carries the key never arrived.
			name: "comment line only",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "half.key",
					[]byte(untrustedCommentPrefix+labelPrivate+" 0011223344556677\n"))
			},
			wantMsg: "found 1 line(s)",
		},
		{
			name: "base64 line is not base64",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "notb64.key",
					[]byte(untrustedCommentPrefix+labelPrivate+" 0011223344556677\nthis is not base64!!\n"))
			},
			wantMsg: "decode base64 line",
		},
		{
			// A file cut short mid-blob. Nothing about the surviving bytes is
			// individually wrong, which is exactly why the length check has to
			// exist: a short private key would otherwise be used to sign.
			name: "blob truncated",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPrivPath(t)))
				return writeKeyFile(t, dir, "short.key",
					encodeKeyFile(labelPrivate, "0011223344556677", "", blob[:privBlobLen-10]))
			},
			wantMsg: "private key blob is 64 bytes, want 74",
		},
		{
			// Right length, wrong algorithm tag. The format's stability rule
			// is "add a new tag rather than change an existing one", so an
			// unknown tag must be refused rather than assumed to be Ed25519.
			name: "unknown algorithm tag",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPrivPath(t)))
				bad := append([]byte(nil), blob...)
				bad[0], bad[1] = 'X', 'z'
				return writeKeyFile(t, dir, "alg.key", encodeKeyFile(labelPrivate, "0011223344556677", "", bad))
			},
			wantMsg: "unsupported key algorithm tag",
		},
		{
			// The classic operator slip: passing the .pub where the .key was
			// wanted. It must fail loudly - and it must never be treated as a
			// signing key.
			name: "public key file passed as the private key",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "wrongtype.key", goodPubBytes)
			},
			wantMsg: "want 74",
		},
		{
			// The comment line and the key material disagree about which key
			// this is. The comment is untrusted, but a disagreement means the
			// file has been edited or spliced, and debark refuses rather
			// than silently preferring one of the two.
			name: "comment line names a different key id",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPrivPath(t)))
				return writeKeyFile(t, dir, "mismatch.key",
					encodeKeyFile(labelPrivate, "deadbeefdeadbeef", "", blob))
			},
			wantMsg: "does not match key id in key material",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t, t.TempDir())
			s, err := newEd25519FileSigner(path)
			if err == nil {
				t.Fatalf("newEd25519FileSigner accepted %s (key id %q)", tc.name, s.KeyID())
			}
			if dferr.ClassOf(err) != dferr.Usage {
				t.Errorf("class = %v, want Usage (err: %v)", dferr.ClassOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestPublicKeyFile_Rejections is the same matrix on the trust-set side.
// loadPublicKeyFile decides what debark will accept a signature FROM, so a
// file it mis-parses is a trust decision made on garbage.
func TestPublicKeyFile_Rejections(t *testing.T) {
	privPath, _, _ := realKeyPair(t)
	privBytes := mustReadFile(t, privPath)

	cases := []struct {
		name    string
		build   func(t *testing.T, dir string) string
		wantMsg string
	}{
		{
			name: "file does not exist",
			build: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "nope.pub")
			},
			wantMsg: "read public key",
		},
		{
			name: "empty file",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "empty.pub", nil)
			},
			wantMsg: "found 0 line(s)",
		},
		{
			// An armoured key from some other tool. debark's format is not
			// PEM and must not half-decode one.
			name: "PEM armour instead of debark format",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "pem.pub", []byte(
					"-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA\n-----END PUBLIC KEY-----\n"))
			},
			wantMsg: "public key",
		},
		{
			// An OpenPGP armoured block, the other thing an operator is
			// likely to have lying around.
			name: "OpenPGP armour instead of debark format",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "gpg.pub", []byte(
					"-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmDMEZ+not+real+armour=\n=abcd\n-----END PGP PUBLIC KEY BLOCK-----\n"))
			},
			wantMsg: "public key",
		},
		{
			name: "blob truncated",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPubPath(t)))
				return writeKeyFile(t, dir, "short.pub",
					encodeKeyFile(labelPublic, "0011223344556677", "", blob[:10]))
			},
			wantMsg: "public key blob is 10 bytes, want 42",
		},
		{
			name: "unknown algorithm tag",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPubPath(t)))
				bad := append([]byte(nil), blob...)
				bad[0], bad[1] = 'R', 'S'
				return writeKeyFile(t, dir, "alg.pub", encodeKeyFile(labelPublic, "0011223344556677", "", bad))
			},
			wantMsg: "unsupported key algorithm tag",
		},
		{
			// The private key handed over as if it were the public one. This
			// one matters twice: it must fail, and failing keeps an operator
			// from publishing their secret key as a "public" key.
			name: "private key file passed as the public key",
			build: func(t *testing.T, dir string) string {
				return writeKeyFile(t, dir, "wrongtype.pub", privBytes)
			},
			wantMsg: "want 42",
		},
		{
			name: "comment line names a different key id",
			build: func(t *testing.T, dir string) string {
				_, blob := mustDecodeKeyFile(t, mustReadFile(t, freshPubPath(t)))
				return writeKeyFile(t, dir, "mismatch.pub",
					encodeKeyFile(labelPublic, "deadbeefdeadbeef", "", blob))
			},
			wantMsg: "does not match key id in key material",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t, t.TempDir())
			pk, err := loadPublicKeyFile(path)
			if err == nil {
				t.Fatalf("loadPublicKeyFile accepted %s (key id %q)", tc.name, pk.KeyID)
			}
			if dferr.ClassOf(err) != dferr.Usage {
				t.Errorf("class = %v, want Usage (err: %v)", dferr.ClassOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
			if len(pk.Raw) != 0 {
				t.Errorf("loadPublicKeyFile returned key material alongside an error: %x", pk.Raw)
			}
		})
	}
}

// TestKeyFile_ToleratedFormatting pins the format's documented leniency, which
// is as much a contract as its strictness: a key file that survived a Windows
// editor, an extra blank line, or trailing whitespace must still load. The
// alternative is an operator whose valid key is refused for a reason that has
// nothing to do with trust.
func TestKeyFile_ToleratedFormatting(t *testing.T) {
	_, pubPath, keyID := realKeyPair(t)
	commentLine, blob := mustDecodeKeyFile(t, mustReadFile(t, pubPath))
	b64 := base64.StdEncoding.EncodeToString(blob)

	cases := []struct {
		name string
		text string
	}{
		{"CRLF line endings", commentLine + "\r\n" + b64 + "\r\n"},
		{"no trailing newline", commentLine + "\n" + b64},
		{"blank lines interleaved", "\n\n" + commentLine + "\n\n\n" + b64 + "\n"},
		{"trailing spaces", commentLine + "   \n" + b64 + "\t \n"},
		{
			// decodeKeyFile stops after two non-blank lines, so anything a
			// tool appended (a second key, a signature block, notes) is
			// ignored rather than smuggled in.
			"extra content after the key", commentLine + "\n" + b64 + "\nignored trailing junk\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeKeyFile(t, t.TempDir(), "operator.pub", []byte(tc.text))
			pk, err := loadPublicKeyFile(path)
			if err != nil {
				t.Fatalf("loadPublicKeyFile: %v", err)
			}
			if pk.KeyID != keyID {
				t.Fatalf("KeyID = %q, want %q", pk.KeyID, keyID)
			}
			if len(pk.Raw) != ed25519.PublicKeySize {
				t.Fatalf("key material is %d bytes, want %d", len(pk.Raw), ed25519.PublicKeySize)
			}
		})
	}
}

// TestKeyFile_CommentIsUntrusted is the "untrusted comment" contract, borrowed
// from minisign and stated in keyformat.go: the comment line is a convenience
// for humans and `file`, never a source of truth. Nothing is believed because
// the comment line says it - the key id the file loads under is the one
// DERIVED from the key material, and the comment line's copy only ever gets to
// agree with it or be refused.
//
// "Untrusted" is not "ignored", which is the other half of the same contract
// and what this test now pins. The line has a shape debark itself wrote; a
// file whose first line no longer matches that shape has been edited or
// truncated, and it is refused rather than loaded with the cross-check
// silently skipped. Skipping it was an attacker's cheapest move: mangle the
// line you do not want checked, and the check that would have caught you is
// the one that stops running.
func TestKeyFile_CommentIsUntrusted(t *testing.T) {
	_, pubPath, keyID := realKeyPair(t)
	commentLine, blob := mustDecodeKeyFile(t, mustReadFile(t, pubPath))
	b64 := base64.StdEncoding.EncodeToString(blob)
	dir := t.TempDir()

	// A comment line with the wrong label entirely: parseCommentLine gives up,
	// and so does the loader.
	path := writeKeyFile(t, dir, "odd.pub",
		[]byte("untrusted comment: something else entirely\n"+b64+"\n"))
	if _, err := loadPublicKeyFile(path); err == nil {
		t.Fatal("loadPublicKeyFile accepted a file whose comment line does not parse")
	} else if !strings.Contains(err.Error(), "comment line") {
		t.Fatalf("error does not say what is wrong with the file: %v", err)
	}

	// The trailing free text on a well-formed line, on the other hand, is
	// exactly what "untrusted comment" means: it is reported as the operator
	// wrote it and nothing is decided by it.
	relabelled := writeKeyFile(t, dir, "relabelled.pub",
		[]byte(commentLine+" not the description this key was generated with\n"+b64+"\n"))
	pk, err := loadPublicKeyFile(relabelled)
	if err != nil {
		t.Fatalf("loadPublicKeyFile refused a well-formed file over its free-text comment: %v", err)
	}
	if pk.KeyID != keyID {
		t.Fatalf("KeyID = %q, want the id derived from the key material (%q)", pk.KeyID, keyID)
	}
}

// TestParseCommentLine covers the comment parser directly, including the two
// shapes that must be rejected outright.
func TestParseCommentLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		label     string
		wantID    string
		wantCmt   string
		wantError bool
	}{
		{
			name: "id only", line: untrustedCommentPrefix + labelPublic + " 0011223344556677",
			label: labelPublic, wantID: "0011223344556677",
		},
		{
			name: "id and comment", line: untrustedCommentPrefix + labelPublic + " 0011223344556677 release engineering, 2026",
			label: labelPublic, wantID: "0011223344556677", wantCmt: "release engineering, 2026",
		},
		{
			name: "missing the untrusted-comment prefix", line: labelPublic + " 0011223344556677",
			label: labelPublic, wantError: true,
		},
		{
			// A secret-key comment on a file being read as a public key. The
			// labels exist to keep the two apart at a glance.
			name: "wrong label", line: untrustedCommentPrefix + labelPrivate + " 0011223344556677",
			label: labelPublic, wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, cmt, err := parseCommentLine(tc.line, tc.label)
			if tc.wantError {
				if err == nil {
					t.Fatalf("parseCommentLine accepted %q", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCommentLine: %v", err)
			}
			if id != tc.wantID || cmt != tc.wantCmt {
				t.Fatalf("got (%q, %q), want (%q, %q)", id, cmt, tc.wantID, tc.wantCmt)
			}
		})
	}
}

// TestLoadPublicKeys_KeyringDirectory covers the exported LoadPublicKeys entry
// point over a keyring directory: what it picks up, what it steps over, and -
// the part that matters - that one unreadable key in the directory fails the
// whole load instead of being skipped. Silently dropping a key the operator
// meant to trust turns a typo into "signature made by an untrusted key" on the
// far side of an air gap, with no clue as to why.
func TestLoadPublicKeys_KeyringDirectory(t *testing.T) {
	ringDir := t.TempDir()

	// Two genuine keys.
	var wantIDs []string
	for _, name := range []string{"alice", "bob"} {
		priv := filepath.Join(ringDir, name+PrivateKeyFileSuffix)
		id, err := GenerateKey(priv, name+" key")
		if err != nil {
			t.Fatal(err)
		}
		wantIDs = append(wantIDs, id)
		// The private halves must not be picked up: only .pub is scanned.
	}
	// Things the scan must step over rather than choke on.
	writeKeyFile(t, ringDir, "README", []byte("this directory holds operator keys\n"))
	writeKeyFile(t, ringDir, "notes.txt", []byte("not a key\n"))
	if err := os.Mkdir(filepath.Join(ringDir, "old.pub"), 0o755); err != nil {
		t.Fatal(err)
	}

	keys, err := LoadPublicKeys(KeySource{Dirs: []string{ringDir}})
	if err != nil {
		t.Fatalf("LoadPublicKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("loaded %d keys, want 2: %+v", len(keys), keys)
	}
	got := map[string]bool{}
	for _, k := range keys {
		got[k.KeyID] = true
		if k.Algorithm != AlgorithmEd25519 {
			t.Errorf("key %s: Algorithm = %q", k.KeyID, k.Algorithm)
		}
		if k.Source == "" {
			t.Errorf("key %s: Source is empty; the operator cannot tell which file it came from", k.KeyID)
		}
	}
	for _, id := range wantIDs {
		if !got[id] {
			t.Errorf("key %s was not loaded", id)
		}
	}

	// One corrupt .pub alongside the good ones must fail the whole load.
	writeKeyFile(t, ringDir, "corrupt.pub", []byte("untrusted comment: debark ed25519 public key 0011223344556677\nnot base64\n"))
	if _, err := LoadPublicKeys(KeySource{Dirs: []string{ringDir}}); err == nil {
		t.Fatal("LoadPublicKeys silently skipped an unparseable key file in a keyring directory")
	} else if !strings.Contains(err.Error(), "corrupt.pub") {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

// TestLoadPublicKeys_MissingDirectory: a keyring directory the operator named
// but that is not there is a configuration error, not an empty trust set. An
// empty trust set would make every signature "untrusted" for a reason that
// looks like tampering.
func TestLoadPublicKeys_MissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-keyring")
	_, err := LoadPublicKeys(KeySource{Dirs: []string{missing}})
	if err == nil {
		t.Fatal("LoadPublicKeys accepted a keyring directory that does not exist")
	}
	if !strings.Contains(err.Error(), "read keyring directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGenerateKey_PathsAndCollisions covers the .pub path derivation for a
// private key whose name does not end in .key, and the refusal to clobber a
// half-existing pair - which is how an operator loses a key they still need.
func TestGenerateKey_PathsAndCollisions(t *testing.T) {
	dir := t.TempDir()

	// A name without the conventional suffix still gets a companion .pub.
	odd := filepath.Join(dir, "operator")
	if _, err := GenerateKey(odd, ""); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if _, err := os.Stat(odd + PublicKeyFileSuffix); err != nil {
		t.Fatalf("expected %s alongside %s: %v", odd+PublicKeyFileSuffix, odd, err)
	}

	// GenerateKey creates intermediate directories.
	nested := filepath.Join(dir, "a", "b", "c", "deep.key")
	if _, err := GenerateKey(nested, ""); err != nil {
		t.Fatalf("GenerateKey into a new directory: %v", err)
	}

	// A stale .pub with no .key must not be quietly overwritten either: the
	// operator is told to remove it deliberately.
	orphan := filepath.Join(dir, "orphan.key")
	writeKeyFile(t, dir, "orphan.pub", []byte("someone else's key\n"))
	if _, err := GenerateKey(orphan, ""); err == nil {
		t.Fatal("GenerateKey overwrote an existing .pub belonging to another key")
	} else if !strings.Contains(dferr.HintOf(err), "remove the existing file") {
		t.Errorf("hint does not tell the operator what to do: %q", dferr.HintOf(err))
	}
}

// --- small helpers ---------------------------------------------------------

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func mustDecodeKeyFile(t *testing.T, data []byte) (commentLine string, blob []byte) {
	t.Helper()
	c, b, err := decodeKeyFile(data)
	if err != nil {
		t.Fatalf("decodeKeyFile: %v", err)
	}
	return c, b
}

// freshPrivPath and freshPubPath hand back a genuinely valid key file that a
// table case can then corrupt one field of, so every rejection below is a
// rejection of exactly one defect rather than of a wholly invented file.
func freshPrivPath(t *testing.T) string {
	t.Helper()
	p, _, _ := realKeyPair(t)
	return p
}

func freshPubPath(t *testing.T) string {
	t.Helper()
	_, p, _ := realKeyPair(t)
	return p
}

// TestKeyFile_KeyIDIsRederivedFromKeyMaterial is the S5 fix, from the
// attacker's side.
//
// Both key file parsers used to copy the 8-byte key id out of the blob
// verbatim, so a hand-built file could carry the RELEASE key's id over the
// ATTACKER's key material - and every layer above believed it, because there
// is no other place the id comes from. VerifierFor keys its trust set by that
// id, so such a file in a keyring directory is trusted under the release key's
// name; on the private side the same hole made Kind()/KeyID(), which
// core/engine writes into evidence.json, whatever the file claimed.
//
// The public-key half is the one that decides who a bundle is attributed to,
// and the whole point is that the file cannot name itself: the id is
// recomputed from the key material and the file's copy has to match.
func TestKeyFile_KeyIDIsRederivedFromKeyMaterial(t *testing.T) {
	// The key the operator trusts, and the key the attacker holds.
	releasePub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	attackerPub, attackerPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseID := keyIDFromPublic(releasePub)
	attackerID := keyIDFromPublic(attackerPub)
	if releaseID == attackerID {
		t.Fatal("two freshly generated keys collided; rerun")
	}

	t.Run("public key wearing another key's id", func(t *testing.T) {
		// Byte-for-byte a valid file except that the id names the release key
		// while the material is the attacker's.
		blob := append([]byte(keyAlgTag), releaseID[:]...)
		blob = append(blob, attackerPub...)
		file := encodeKeyFile(labelPublic, keyIDHex(releaseID), "vendor key", blob)
		path := writeKeyFile(t, t.TempDir(), "zz-vendor.pub", file)

		pk, err := loadPublicKeyFile(path)
		if err == nil {
			t.Fatalf("a public key file claiming an id it does not hold loaded as %s", pk.KeyID)
		}
		if !strings.Contains(err.Error(), keyIDHex(attackerID)) {
			t.Errorf("the error does not name the id the material actually has: %v", err)
		}
	})

	t.Run("private key wearing another key's id", func(t *testing.T) {
		blob := append([]byte(keyAlgTag), releaseID[:]...)
		blob = append(blob, attackerPriv...)
		path := writeKeyFile(t, t.TempDir(), "claims-release.key",
			encodeKeyFile(labelPrivate, keyIDHex(releaseID), "", blob))

		s, err := newEd25519FileSigner(path)
		if err == nil {
			t.Fatalf("a private key file claiming an id it does not hold loaded, and signs as %s", s.KeyID())
		}
	})

	t.Run("id disagrees with material but agrees with the comment line", func(t *testing.T) {
		// The comment-line cross-check cannot catch this on its own: the
		// attacker controls both copies of the id, so they agree with each
		// other and disagree only with the key. Only re-derivation sees it.
		blob := append([]byte(keyAlgTag), releaseID[:]...)
		blob = append(blob, attackerPub...)
		path := writeKeyFile(t, t.TempDir(), "consistent-lie.pub",
			encodeKeyFile(labelPublic, keyIDHex(releaseID), "", blob))
		if _, err := loadPublicKeyFile(path); err == nil {
			t.Fatal("a file whose two copies of the id agree with each other and with nothing else was accepted")
		}
	})
}

// TestPrivateKeyFile_HalvesMustBelongTogether: crypto/ed25519's private key is
// seed||publicKey and the library checks nothing about the pairing - Sign
// derives the scalar from the seed but copies the STORED public half into the
// signature. A file whose halves disagree therefore signs without complaint
// and produces a signature that cannot be verified by anybody, including the
// holder. The only place that would surface is the air-gapped target, after
// the medium has been couriered.
func TestPrivateKeyFile_HalvesMustBelongTogether(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	frankenstein := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(frankenstein, priv[:32])
	copy(frankenstein[32:], otherPub)

	// The id is computed from the (foreign) public half, so every other check
	// in the parser passes: this file is internally consistent everywhere
	// except where it counts.
	id := keyIDFromPublic(otherPub)
	blob := append([]byte(keyAlgTag), id[:]...)
	blob = append(blob, frankenstein...)
	path := writeKeyFile(t, t.TempDir(), "mismatched.key",
		encodeKeyFile(labelPrivate, keyIDHex(id), "", blob))

	if _, err := newEd25519FileSigner(path); err == nil {
		t.Fatal("a private key whose public half does not belong to its seed was accepted for signing")
	} else if !strings.Contains(err.Error(), "seed") {
		t.Errorf("the error does not say what is wrong with the file: %v", err)
	}
}

// TestPrivateKeyFile_PermissionRule covers the mode check as a rule, on every
// platform: the caller skips it on Windows (where os.Stat synthesises a mode
// that says nothing about who can read the file), so the rule itself is what
// gets tested here.
func TestPrivateKeyFile_PermissionRule(t *testing.T) {
	cases := []struct {
		perm    os.FileMode
		wantErr bool
	}{
		{0o600, false},
		{0o400, false},
		{0o700, false},
		{0o640, true}, // group-readable: a shared build account can read it
		{0o644, true},
		{0o666, true},
		{0o604, true},
		{0o777, true},
	}
	for _, tc := range cases {
		err := privateKeyPermissionError("/keys/op.key", tc.perm)
		if (err != nil) != tc.wantErr {
			t.Errorf("mode %04o: err = %v, want error: %v", tc.perm, err, tc.wantErr)
		}
		if err != nil && dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("mode %04o: class = %v, want Usage", tc.perm, dferr.ClassOf(err))
		}
	}
}

// TestPrivateKeyFile_RefusesWorldReadableKey is the same rule end to end, on
// the platforms that have real mode bits.
func TestPrivateKeyFile_RefusesWorldReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows; the rule itself is covered by TestPrivateKeyFile_PermissionRule")
	}
	privPath, _, _ := realKeyPair(t)
	if err := os.Chmod(privPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newEd25519FileSigner(privPath); err == nil {
		t.Fatal("a world-readable private key was loaded and would have signed a bundle")
	}
	if err := os.Chmod(privPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newEd25519FileSigner(privPath); err != nil {
		t.Fatalf("a correctly restricted key was refused: %v", err)
	}
}
