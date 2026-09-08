package sign

// The verifier: what it will check, what it refuses to check, and where it
// gets its keys from.
//
// This is the code that runs on the air-gapped target, before apt has seen
// anything. Its failure mode of record is not "rejects a good bundle" - that
// is annoying but visible - it is "accepts a bad one", which is invisible
// until much later. So every case here is written as: what would have to be
// true for a bad bundle to pass, and does it.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// TestCompositeVerifier_Kinds documents what a verifier built by VerifierFor
// will attempt at all. A kind missing from this list is a kind whose
// signatures cannot establish trust - which is the safe direction, but it has
// to be a deliberate list rather than an accident.
func TestCompositeVerifier_Kinds(t *testing.T) {
	v, err := VerifierFor(context.Background(), KeySource{}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	kinds := v.Kinds()
	want := map[string]bool{manifest.SignerEd25519File: true, manifest.SignerGPG: true}
	if len(kinds) != len(want) {
		t.Fatalf("Kinds() = %v, want exactly %v", kinds, want)
	}
	for _, k := range kinds {
		if !want[k] {
			t.Errorf("Kinds() reports %q, which VerifierFor cannot actually check", k)
		}
	}
	// Sigstore is named in the manifest schema but has no verifier yet. A
	// sigstore-signed bundle must therefore fail, not pass by default.
	for _, k := range kinds {
		if k == manifest.SignerSigstore {
			t.Error("Kinds() claims sigstore support that compositeVerifier does not implement")
		}
	}
}

// TestVerifyEd25519Signature is the tamper matrix for the native signature
// path. Each case is one thing an attacker with write access to the bundle
// could change; the distinction being asserted is between ErrUntrustedKey ("I
// have no key to check this with") and a dferr.Verification error ("I checked
// it with a key you trust and it does not match"), because those two tell the
// operator to do completely different things.
func TestVerifyEd25519Signature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := keyIDHex(keyIDFromPublic(pub))
	trusted := map[string]ed25519.PublicKey{keyID: pub}

	attackerPub, attackerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = attackerPub

	const purpose = manifest.SignPurpose
	canon := []byte(`{"target":{"arch":"amd64"},"files":[]}`)
	good := manifest.Signature{
		SignerKind: manifest.SignerEd25519File,
		KeyID:      keyID,
		Algorithm:  AlgorithmEd25519,
		Signature:  base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningInput(purpose, canon))),
	}

	// Control: the untouched signature verifies. Without this, every case
	// below could be passing for the wrong reason.
	if err := verifyEd25519Signature(trusted, purpose, canon, good); err != nil {
		t.Fatalf("control case failed: a good signature must verify: %v", err)
	}

	cases := []struct {
		name         string
		purpose      string
		canon        []byte
		mutate       func(s *manifest.Signature)
		wantUntruste bool // ErrUntrustedKey, as opposed to a Verification error
		wantMsg      string
	}{
		{
			// Signed by a key the operator never trusted. There is nothing to
			// check against, so this is untrusted-key and not tamper.
			name: "signature from an unknown key",
			mutate: func(s *manifest.Signature) {
				s.KeyID = keyIDHex(keyIDFromPublic(attackerPub))
				s.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(attackerPriv, SigningInput(purpose, canon)))
			},
			wantUntruste: true,
		},
		{
			// An attacker signs with their own key but writes the operator's
			// key id into the block, hoping the id is what gets checked. The
			// id only selects which key to check WITH; the cryptography then
			// says no.
			name: "attacker signature wearing a trusted key id",
			mutate: func(s *manifest.Signature) {
				s.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(attackerPriv, SigningInput(purpose, canon)))
			},
			wantMsg: "does not verify against the trusted key",
		},
		{
			name:    "payload changed after signing",
			canon:   []byte(`{"target":{"arch":"arm64"},"files":[]}`),
			wantMsg: "does not verify against the trusted key",
		},
		{
			name:    "purpose changed after signing",
			purpose: "debark.something-else/v1",
			wantMsg: "does not verify against the trusted key",
		},
		{
			name: "one bit of the signature flipped",
			mutate: func(s *manifest.Signature) {
				raw, _ := base64.StdEncoding.DecodeString(s.Signature)
				raw[0] ^= 0x01
				s.Signature = base64.StdEncoding.EncodeToString(raw)
			},
			wantMsg: "does not verify against the trusted key",
		},
		{
			name: "signature is not base64",
			mutate: func(s *manifest.Signature) {
				s.Signature = "@@@ not base64 @@@"
			},
			wantMsg: "malformed base64",
		},
		{
			// Valid base64, wrong number of bytes. ed25519.Verify would panic
			// on a wrong-length signature, so the explicit length check is
			// what keeps a malformed bundle from crashing the target instead
			// of failing it.
			name: "signature is the wrong length",
			mutate: func(s *manifest.Signature) {
				s.Signature = base64.StdEncoding.EncodeToString([]byte("too short"))
			},
			wantMsg: "wrong length 9, want 64",
		},
		{
			name: "empty signature",
			mutate: func(s *manifest.Signature) {
				s.Signature = ""
			},
			wantMsg: "wrong length 0, want 64",
		},
		{
			name: "no key id at all",
			mutate: func(s *manifest.Signature) {
				s.KeyID = ""
			},
			wantUntruste: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := good
			if tc.mutate != nil {
				tc.mutate(&sig)
			}
			p, c := purpose, canon
			if tc.purpose != "" {
				p = tc.purpose
			}
			if tc.canon != nil {
				c = tc.canon
			}
			err := verifyEd25519Signature(trusted, p, c, sig)
			if err == nil {
				t.Fatalf("verification accepted: %s", tc.name)
			}
			if tc.wantUntruste {
				if !errors.Is(err, ErrUntrustedKey) {
					t.Fatalf("err = %v, want ErrUntrustedKey", err)
				}
				return
			}
			if errors.Is(err, ErrUntrustedKey) {
				t.Fatalf("a signature checked against a TRUSTED key was reported as untrusted, which sends the operator looking for the wrong problem: %v", err)
			}
			if dferr.ClassOf(err) != dferr.Verification {
				t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestVerifyEd25519Signature_KeyIDCaseInsensitive: key ids are hex, and hex
// comes back upper-cased from plenty of tools. A block whose key_id differs
// only in case names the same key, and refusing it would be a false tamper
// report.
func TestVerifyEd25519Signature_KeyIDCaseInsensitive(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := keyIDHex(keyIDFromPublic(pub))
	trusted := map[string]ed25519.PublicKey{keyID: pub}

	canon := []byte("payload")
	sig := manifest.Signature{
		KeyID:     strings.ToUpper(keyID),
		Algorithm: AlgorithmEd25519,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningInput(manifest.SignPurpose, canon))),
	}
	if err := verifyEd25519Signature(trusted, manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("an upper-case key id did not match its lower-case trust entry: %v", err)
	}
}

// TestCompositeVerifier_UnknownKind: a signature block naming a kind this
// verifier cannot check must be untrusted, never skipped-and-therefore-fine.
func TestCompositeVerifier_UnknownKind(t *testing.T) {
	v := &compositeVerifier{ed25519Keys: map[string]ed25519.PublicKey{}}
	for _, kind := range []string{manifest.SignerSigstore, "plugin:acme-hsm", "", "future-kind"} {
		err := v.Verify(context.Background(), manifest.SignPurpose, []byte("x"), manifest.Signature{
			SignerKind: kind,
			Algorithm:  "some-algorithm-we-do-not-implement",
			Signature:  base64.StdEncoding.EncodeToString([]byte("whatever")),
		})
		if !errors.Is(err, ErrUntrustedKey) {
			t.Errorf("signer_kind %q: err = %v, want ErrUntrustedKey", kind, err)
		}
	}
}

// TestErrUntrustedKey_Message: the sentinel's text ends up in operator-facing
// output, so it has to say the thing it means.
func TestErrUntrustedKey_Message(t *testing.T) {
	if got := ErrUntrustedKey.Error(); !strings.Contains(got, "untrusted key") {
		t.Fatalf("ErrUntrustedKey.Error() = %q", got)
	}
}

// TestVerifierFor_RequiresBundlePath: without a bundle path there is nothing
// to compare key sources against, so the same-media refusal could not run.
// Defaulting to "no bundle, no check" would turn a missing argument into a
// silently weaker trust decision.
func TestVerifierFor_RequiresBundlePath(t *testing.T) {
	_, err := VerifierFor(context.Background(), KeySource{}, "")
	if err == nil {
		t.Fatal("VerifierFor accepted an empty bundlePath")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Fatalf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

// TestVerifierFor_SameMediaRefusalCoversEverySource: ADR-008's refusal has to
// apply to every way a key can be named, not just --key. A keyring directory
// or a gpg keyring inside the bundle is exactly the same attack: whoever
// tampered with the media supplies the key that "verifies" the tampering.
func TestVerifierFor_SameMediaRefusalCoversEverySource(t *testing.T) {
	bundle := t.TempDir()

	// Real key material, so the refusal cannot be mistaken for a parse error.
	outside := t.TempDir()
	priv := filepath.Join(outside, "operator.key")
	if _, err := GenerateKey(priv, ""); err != nil {
		t.Fatal(err)
	}
	pubBytes, err := os.ReadFile(publicKeyPathFor(priv))
	if err != nil {
		t.Fatal(err)
	}

	insideKeyDir := filepath.Join(bundle, "keys")
	if err := os.MkdirAll(insideKeyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	insideKey := filepath.Join(insideKeyDir, "operator.pub")
	if err := os.WriteFile(insideKey, pubBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	insideKeyring := filepath.Join(bundle, "release.gpg")
	if err := os.WriteFile(insideKeyring, []byte("keyring"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ks   KeySource
	}{
		{"a key file inside the bundle", KeySource{Files: []string{insideKey}}},
		{"a keyring directory inside the bundle", KeySource{Dirs: []string{insideKeyDir}}},
		{"a gpg keyring inside the bundle", KeySource{GPGKeyring: insideKeyring}},
		{"the bundle directory itself", KeySource{Dirs: []string{bundle}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifierFor(context.Background(), tc.ks, bundle)
			if err == nil {
				t.Fatal("VerifierFor accepted a key source that travelled with the media")
			}
			if !errors.Is(err, ErrSameMediaKey) {
				t.Fatalf("error does not wrap ErrSameMediaKey (verify matches on it to report the right problem): %v", err)
			}
			if dferr.ClassOf(err) != dferr.Verification {
				t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
			}
			if !strings.Contains(dferr.HintOf(err), "out of band") {
				t.Errorf("hint does not tell the operator what to do instead: %q", dferr.HintOf(err))
			}
		})
	}
}

// TestSameMedia_PrefixTrap is the classic string-prefix bug: "/media/bundle"
// and "/media/bundle-keys" share a prefix but one is not inside the other.
// Getting this wrong in the permissive direction refuses a legitimate key
// directory; getting it wrong the other way would let a path that merely looks
// similar slip past the check. Both are worth a test.
func TestSameMedia_PrefixTrap(t *testing.T) {
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	sibling := filepath.Join(base, "bundle-keys")
	inside := filepath.Join(bundle, "keys")
	for _, d := range []string{bundle, sibling, inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if sameMedia(sibling, bundle) {
		t.Errorf("%q was treated as inside %q merely because the names share a prefix", sibling, bundle)
	}
	if !sameMedia(inside, bundle) {
		t.Errorf("%q was not recognised as inside %q", inside, bundle)
	}
	if !sameMedia(bundle, bundle) {
		t.Errorf("the bundle directory itself was not recognised as same-media")
	}
	// The parent IS refused, and deliberately so (ADR-008): --keyring
	// /media/usb with the bundle at /media/usb/bundle designates a directory
	// the tamperable medium is part of, so whoever rewrote the bundle can
	// drop a key into the trusted directory above it. The refusal is about
	// the medium, not about the string, so it is symmetric - "one of these
	// contains the other" - rather than one-directional.
	if !sameMedia(base, bundle) {
		t.Errorf("the PARENT of the bundle was accepted as an out-of-band key source")
	}
	// Relative and messy spellings of the same place still compare equal:
	// resolveReal cleans and absolutises both sides.
	if !sameMedia(filepath.Join(bundle, ".", "keys", "..", "keys"), bundle) {
		t.Errorf("an uncleaned path inside the bundle escaped the check")
	}
}

// TestSameMedia_FollowsSymlinks: a symlink outside the bundle pointing into it
// is the obvious way to dress up a same-media key as an out-of-band one. The
// check resolves symlinks before comparing, so it must not be fooled.
func TestSameMedia_FollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bundle, "operator.pub")
	if err := os.WriteFile(target, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "looks-external.pub")
	if err := os.Symlink(target, link); err != nil {
		// Creating a symlink on Windows needs Developer Mode or elevation.
		t.Skipf("cannot create a symlink in this environment (%v); skipping", err)
	}
	if !sameMedia(link, bundle) {
		t.Fatalf("a symlink at %q pointing inside %q was accepted as an out-of-band key source", link, bundle)
	}
}

// TestResolveReal_NonExistentPath: resolveReal has to answer for paths that do
// not exist, because a key source the operator mistyped still has to be
// compared before the "file not found" error is produced.
func TestResolveReal_NonExistentPath(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-created", "key.pub")
	got := resolveReal(missing)
	if !filepath.IsAbs(got) {
		t.Fatalf("resolveReal(%q) = %q, want an absolute path", missing, got)
	}
	if runtime.GOOS != "windows" && got != filepath.Clean(missing) {
		t.Fatalf("resolveReal(%q) = %q", missing, got)
	}
}

// TestVerifierFor_AllowSameMediaIsAnExplicitOverride: the escape hatch exists
// so the refusal is documented rather than absent. It must work, and it must
// be the only thing that lifts the refusal.
func TestVerifierFor_AllowSameMediaIsAnExplicitOverride(t *testing.T) {
	bundle := t.TempDir()
	priv := filepath.Join(t.TempDir(), "operator.key")
	if _, err := GenerateKey(priv, ""); err != nil {
		t.Fatal(err)
	}
	pubBytes, err := os.ReadFile(publicKeyPathFor(priv))
	if err != nil {
		t.Fatal(err)
	}
	insideKey := filepath.Join(bundle, "operator.pub")
	if err := os.WriteFile(insideKey, pubBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := VerifierFor(context.Background(), KeySource{Files: []string{insideKey}, AllowSameMedia: true}, bundle); err != nil {
		t.Fatalf("AllowSameMedia did not lift the refusal: %v", err)
	}
}

// TestVerifierFor_PropagatesKeyLoadErrors: a key source that names a file
// debark cannot parse must fail the whole construction. A verifier that came
// up with a silently empty trust set would report every signature as untrusted
// - indistinguishable, to the operator, from a bundle signed by a stranger.
func TestVerifierFor_PropagatesKeyLoadErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.pub")
	if err := os.WriteFile(bad, []byte("untrusted comment: debark ed25519 public key 0011223344556677\nnot base64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifierFor(context.Background(), KeySource{Files: []string{bad}}, t.TempDir()); err == nil {
		t.Fatal("VerifierFor built a verifier from an unparseable key file")
	}
}

// TestEd25519Signer_RespectsCancelledContext: a build the operator cancelled
// (or that hit its deadline) must not go on to produce a signature. Signing is
// the last step before a bundle is declared trustworthy, so a signer that
// ignores cancellation can hand back a signed artefact for a build nobody
// waited for.
func TestEd25519Signer_RespectsCancelledContext(t *testing.T) {
	priv := filepath.Join(t.TempDir(), "operator.key")
	if _, err := GenerateKey(priv, ""); err != nil {
		t.Fatal(err)
	}
	s, err := SignerFor(context.Background(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("payload"))
	if err == nil {
		t.Fatalf("Sign produced a signature under a cancelled context: %+v", sig)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if sig.Signature != "" {
		t.Fatalf("Sign returned an error AND a signature: %+v", sig)
	}
}

// TestGenerateKey_UnusableDestination: `debark keygen` pointed at a
// path whose parent is a regular file must fail with a usage error naming the
// path, not a bare syscall error.
func TestGenerateKey_UnusableDestination(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(notADir, "nested", "operator.key")
	if _, err := GenerateKey(target, ""); err == nil {
		t.Fatal("GenerateKey succeeded with a regular file in place of a parent directory")
	} else if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage (err: %v)", dferr.ClassOf(err), err)
	}
}

// TestVerifierFor_RefusesKeySourceAboveTheBundle is the S8 fix. ADR-008
// refuses "a key travelling on the same tamperable medium", and the refusal
// used to be a prefix test in one direction only - is the key INSIDE the
// bundle - so moving it one level up, to the mount point the bundle sits on,
// walked straight past it. That is not an exotic layout: it is where anyone
// would put a key directory next to a bundle directory on a USB stick.
func TestVerifierFor_RefusesKeySourceAboveTheBundle(t *testing.T) {
	medium := t.TempDir() // stands in for /media/usb
	bundle := filepath.Join(medium, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	priv := filepath.Join(outside, "operator.key")
	if _, err := GenerateKey(priv, ""); err != nil {
		t.Fatal(err)
	}
	pubBytes, err := os.ReadFile(publicKeyPathFor(priv))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(medium, "operator.pub"), pubBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// The medium's root as a keyring directory: an attacker who can rewrite
	// the bundle can drop a .pub into the directory above it just as easily.
	_, err = VerifierFor(context.Background(), KeySource{Dirs: []string{medium}}, bundle)
	if !errors.Is(err, ErrSameMediaKey) {
		t.Fatalf("a keyring directory CONTAINING the bundle was accepted: %v", err)
	}

	// What this does NOT catch, deliberately: a key SIBLING of the bundle,
	// /media/usb/keys/op.pub beside /media/usb/bundle. Neither path contains
	// the other, so the only thing that separates it from a legitimately
	// out-of-band /etc/debark/keys is which VOLUME each lives on - and a
	// volume-identity rule cannot be written portably without refusing every
	// ordinary same-disk layout too (a bundle built into a temp directory with
	// the key beside it, which is every local run and every test in this
	// repository; /tmp is a separate mount on most Linux systems, so even
	// "different mount" does not mean "different medium"). The remedy stays
	// the one ADR-008 states: provision the key out of band.

	// The control: a genuinely unrelated directory is still fine, or the
	// refusal above would just be "everything is refused".
	if _, err := VerifierFor(context.Background(), KeySource{Dirs: []string{outside}}, bundle); err != nil {
		t.Fatalf("an out-of-band key directory was refused: %v", err)
	}
}

// TestSameMedia_ComparesIdentityNotSpelling: the containment test is
// os.SameFile up the parent chain, not a string prefix, so a path that reaches
// the bundle by another spelling is still inside it. The case that motivated
// it: sameMedia used to lowercase both paths on Windows ONLY, while the
// filesystems this refusal exists for - the vfat/exFAT sticks an air-gap
// workflow actually uses - are case-insensitive on Linux too.
func TestSameMedia_ComparesIdentityNotSpelling(t *testing.T) {
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Reached through "..", so the cleaned string differs from the bundle's
	// own spelling at every step; identity does not.
	viaParent := filepath.Join(bundle, "keys", "..", "..", "bundle", "keys", "op.pub")
	if !sameMedia(viaParent, bundle) {
		t.Errorf("a key inside the bundle reached via .. was accepted as out of band")
	}

	// A path that does not exist at all still resolves to an ancestor that
	// does: the walk keeps going up until it finds one.
	if !sameMedia(filepath.Join(bundle, "not", "created", "yet", "op.pub"), bundle) {
		t.Errorf("a not-yet-created path inside the bundle escaped the check")
	}
}

// TestBuildEd25519TrustSet_RefusesCollidingKeyIDs: the trust set is a map
// keyed by key id, so two different keys claiming one id cannot both be in it
// - one silently wins, chosen by the order the sources were walked, which for
// a keyring directory means alphabetical order, which an attacker who can drop
// a file in picks. The key id is 8 bytes of SHA-256, so grinding a collision
// is work, not magic. An ambiguous trust set is refused rather than resolved.
func TestBuildEd25519TrustSet_RefusesCollidingKeyIDs(t *testing.T) {
	releasePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := keyIDHex(keyIDFromPublic(releasePub))

	release := PublicKey{KeyID: id, Algorithm: AlgorithmEd25519, Raw: releasePub, Source: "aa-release.pub"}
	// The collision is constructed directly here: producing a real one is
	// ~2^32 work, which is a fact about the format, not something a test does.
	collision := PublicKey{KeyID: id, Algorithm: AlgorithmEd25519, Raw: attackerPub, Source: "zz-vendor.pub"}

	if _, err := buildEd25519TrustSet([]PublicKey{release, collision}); err == nil {
		t.Fatal("two different keys claiming one key id were both accepted into the trust set")
	} else {
		for _, want := range []string{id, "aa-release.pub", "zz-vendor.pub"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q, so the operator cannot tell which files collide: %v", want, err)
			}
		}
	}

	// The same key listed twice - a --key file that is also in a --keyring
	// directory - is ordinary and must not be an error.
	dup := release
	dup.Source = "keyring.d/release.pub"
	set, err := buildEd25519TrustSet([]PublicKey{release, dup})
	if err != nil {
		t.Fatalf("the same key from two sources was refused: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("trust set holds %d entries, want 1", len(set))
	}
}

// TestCompositeVerifier_RefusesRelabelledProvenance is the S6 fix.
//
// signer_kind, algorithm, created_at and comment live in
// debark.manifest.sig, which is NOT covered by the signature, so anyone who
// can write the medium can rewrite them. core/verify copies signer_kind,
// key_id and algorithm straight into the report an auditor reads. The
// verifier's answer to that is: a block only verifies if its own account of
// itself matches the check that actually ran.
func TestCompositeVerifier_RefusesRelabelledProvenance(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := keyIDHex(keyIDFromPublic(pub))
	v := &compositeVerifier{ed25519Keys: map[string]ed25519.PublicKey{keyID: pub}, gpg: &gpgVerifier{}}

	canon := []byte(`{"files":[]}`)
	good := manifest.Signature{
		SignerKind: manifest.SignerEd25519File,
		KeyID:      keyID,
		Algorithm:  AlgorithmEd25519,
		CreatedAt:  "2026-01-01T00:00:00Z",
		Signature:  base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningInput(manifest.SignPurpose, canon))),
	}
	if err := v.Verify(context.Background(), manifest.SignPurpose, canon, good); err != nil {
		t.Fatalf("control case failed: an honest block must verify: %v", err)
	}

	t.Run("algorithm relabelled on a native block", func(t *testing.T) {
		// ed25519 arithmetic is what would check this, and the report would
		// print "rsa-4096" beside it.
		relabelled := good
		relabelled.Algorithm = "rsa-4096"
		err := v.Verify(context.Background(), manifest.SignPurpose, canon, relabelled)
		if err == nil {
			t.Fatal("a block whose algorithm contradicts the check that ran verified")
		}
		if dferr.ClassOf(err) != dferr.Verification {
			t.Errorf("class = %v, want Verification (err: %v)", dferr.ClassOf(err), err)
		}
	})

	t.Run("kind relabelled to gpg", func(t *testing.T) {
		// Routing alone closes this one: a block that says gpg is handed to
		// gpg, which has no OpenPGP signature packet to find in 64 bytes of
		// ed25519. The trusted ed25519 key that WOULD have validated it is
		// never consulted. (The stub stands in for gpg here so the test
		// cannot reach the operator's real keyring.)
		useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: "", Exit: 1}})
		relabelled := good
		relabelled.SignerKind = manifest.SignerGPG
		if err := v.Verify(context.Background(), manifest.SignPurpose, canon, relabelled); err == nil {
			t.Fatal("an ed25519 signature relabelled as a gpg one verified")
		}
	})

	t.Run("plugin block with an algorithm this verifier cannot check", func(t *testing.T) {
		// NOT tamper: a real ecdsa-p256 HSM plugin produces exactly this, and
		// reporting it as a bad signature would turn an honest bundle from an
		// unsupported signer into debark's loudest verdict. It is
		// ErrUntrustedKey - "nothing here can answer for this".
		unsupported := good
		unsupported.SignerKind = manifest.SignerPluginPrefix + "acme-hsm"
		unsupported.Algorithm = "ecdsa-p256-sha256"
		if err := v.Verify(context.Background(), manifest.SignPurpose, canon, unsupported); !errors.Is(err, ErrUntrustedKey) {
			t.Fatalf("err = %v, want ErrUntrustedKey", err)
		}
	})

	t.Run("plugin block that did use ed25519 still verifies", func(t *testing.T) {
		// The operator trusted this key by adding its .pub to a keyring; which
		// process held the private half changes nothing about the arithmetic.
		plugin := good
		plugin.SignerKind = manifest.SignerPluginPrefix + "acme-hsm"
		if err := v.Verify(context.Background(), manifest.SignPurpose, canon, plugin); err != nil {
			t.Fatalf("a plugin's ed25519 signature over a trusted key was refused: %v", err)
		}
	})
}
