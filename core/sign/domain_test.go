package sign

// Domain separation, across every signer kind.
//
// A debark signature is a statement about a (purpose, payload) pair, not
// about bytes. The whole construction rests on one rule stated in iface.go:
// "Implementations must incorporate purpose into what they sign - debark
// signs purpose, a NUL byte, then the payload - so a signature over a manifest
// can never be replayed as a signature over something else."
//
// Two things therefore have to hold, and neither is checked by any single
// signer's own tests:
//
//  1. Every kind applies the SAME construction. A signer that mixed the
//     purpose in differently would produce signatures that its own verifier
//     accepts and everyone else's rejects - or worse, a signer that omitted it
//     would produce signatures that verify under any purpose at all.
//  2. A signature made for one purpose does not verify under another, for
//     every kind.
//
// The first is the one that is easy to lose silently, so it is tested by
// making all three kinds sign with the SAME key and asserting the signature
// bytes come out identical. Ed25519 is deterministic, so identical bytes means
// identical signed input; a single differing byte anywhere in the construction
// shows up immediately.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inferops/debark/core/manifest"
)

// TestSigningInput_Construction pins the wire format of the signed bytes. It
// is a published contract between this package's signers and its verifiers
// (and between debark and any out-of-tree plugin), so it is spelled out
// literally rather than expressed in terms of the function under test.
func TestSigningInput_Construction(t *testing.T) {
	got := SigningInput("debark.manifest/v1", []byte(`{"a":1}`))
	want := append([]byte("debark.manifest/v1"), 0)
	want = append(want, []byte(`{"a":1}`)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("SigningInput = %q, want %q", got, want)
	}

	// The separator is what makes the encoding unambiguous: without it,
	// purpose "ab" over payload "c" and purpose "a" over payload "bc" would be
	// the same signed bytes, and a signature over one would be a signature
	// over the other.
	if bytes.Equal(SigningInput("ab", []byte("c")), SigningInput("a", []byte("bc"))) {
		t.Fatal("SigningInput is ambiguous across the purpose/payload boundary")
	}

	// An empty payload is still domain-separated, so an empty manifest cannot
	// be replayed as an empty anything-else.
	if !bytes.Equal(SigningInput("p", nil), []byte{'p', 0}) {
		t.Fatalf("SigningInput(p, nil) = %q", SigningInput("p", nil))
	}
}

// writeSeedKeyFile writes a debark-native private key file holding the key
// the helper plugin and the gpg stub both sign with, so all three kinds can be
// compared against each other.
func writeSeedKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.key")
	if err := os.WriteFile(path, encodePrivateKeyFile(helperPrivateKey(), "shared across signer kinds"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// TestDomainSeparation_AllKindsSignIdenticalBytes is the consistency half of
// the requirement. ed25519-file signs in-process, the plugin signs in another
// process over JSON, and gpg signs through an external binary - three entirely
// different paths that must arrive at the same signed input. Holding the key
// fixed across all three turns "do they agree?" into a byte comparison.
//
// If any kind stopped prefixing the purpose, or used a different separator, or
// signed the base64 of the payload rather than the payload, exactly one of
// these three would differ and this test would say which.
func TestDomainSeparation_AllKindsSignIdenticalBytes(t *testing.T) {
	const purpose = manifest.SignPurpose
	canon := []byte(`{"schema_version":"debark.manifest/v1","files":[{"path":"repo/Packages"}]}`)

	// The reference: what the construction says the bytes must be, signed
	// directly. Nothing in the package produced this.
	reference := ed25519.Sign(helperPrivateKey(), SigningInput(purpose, canon))
	referenceB64 := base64.StdEncoding.EncodeToString(reference)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("ed25519-file", func(t *testing.T) {
		s, err := SignerFor(ctx, writeSeedKeyFile(t))
		if err != nil {
			t.Fatalf("SignerFor: %v", err)
		}
		defer s.Close()
		if s.Kind() != manifest.SignerEd25519File {
			t.Fatalf("Kind() = %q", s.Kind())
		}
		sig, err := s.Sign(ctx, purpose, canon)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if sig.Signature != referenceB64 {
			t.Fatalf("ed25519-file signed different bytes than purpose||NUL||payload\n got %s\nwant %s", sig.Signature, referenceB64)
		}
		if sig.KeyID != helperKeyID() {
			t.Fatalf("KeyID = %q, want %q", sig.KeyID, helperKeyID())
		}
	})

	t.Run("plugin", func(t *testing.T) {
		s := mustStartHelperPlugin(t, ctx, "serve:good")
		sig, err := s.Sign(ctx, purpose, canon)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if sig.Signature != referenceB64 {
			t.Fatalf("the plugin kind signed different bytes than purpose||NUL||payload\n got %s\nwant %s", sig.Signature, referenceB64)
		}
	})

	t.Run("gpg", func(t *testing.T) {
		// The stub signs exactly the bytes gpg is handed on stdin, with the
		// same key. So this compares what gpgSigner FEEDS gpg against the
		// construction - which is the only part of the gpg path debark
		// controls, and the only part that can drift.
		argsFile := useGPGStub(t, gpgStubScript{
			List:     gpgStubOp{Stdout: colonListing(testFingerprint)},
			RealSign: true,
		})
		s, err := SignerFor(ctx, "gpg:"+testFingerprint)
		if err != nil {
			t.Fatalf("SignerFor(gpg): %v", err)
		}
		defer s.Close()
		sig, err := s.Sign(ctx, purpose, canon)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if sig.Signature != referenceB64 {
			t.Fatalf("the gpg kind handed gpg different bytes than purpose||NUL||payload\n got %s\nwant %s", sig.Signature, referenceB64)
		}
		// And prove the stub was genuinely driven rather than short-circuited:
		// a stub that was never invoked could not have produced those bytes,
		// but say so explicitly so a future refactor cannot quietly bypass it.
		inv := gpgStubInvocations(t, argsFile)
		if len(inv) < 2 {
			t.Fatalf("expected the gpg stub to be invoked for the key lookup and the signature, got %d invocations", len(inv))
		}
		if !argvHas(inv[len(inv)-1], "--detach-sign") {
			t.Errorf("the signing invocation did not pass --detach-sign: %v", inv[len(inv)-1])
		}
	})
}

// TestDomainSeparation_PurposeIsBindingPerKind is the replay half: for every
// kind, a signature made for one purpose must be worthless under another.
// debark only defines one purpose today (manifest.SignPurpose), which is
// precisely why this has to be tested rather than observed - the second
// purpose will be added by someone who assumes this already works.
func TestDomainSeparation_PurposeIsBindingPerKind(t *testing.T) {
	const signedPurpose = manifest.SignPurpose
	const otherPurpose = "debark.snapshot/v1"
	canon := []byte(`{"payload":"identical under both purposes"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	trusted := map[string]ed25519.PublicKey{helperKeyID(): helperPublicKey()}

	t.Run("ed25519-file", func(t *testing.T) {
		s, err := SignerFor(ctx, writeSeedKeyFile(t))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		sig, err := s.Sign(ctx, signedPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyEd25519Signature(trusted, signedPurpose, canon, sig); err != nil {
			t.Fatalf("signature did not verify under its own purpose: %v", err)
		}
		if err := verifyEd25519Signature(trusted, otherPurpose, canon, sig); err == nil {
			t.Fatalf("an ed25519-file signature over %q verified under %q", signedPurpose, otherPurpose)
		}
	})

	t.Run("plugin", func(t *testing.T) {
		s := mustStartHelperPlugin(t, ctx, "serve:good")
		sig, err := s.Sign(ctx, signedPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyEd25519Signature(trusted, signedPurpose, canon, sig); err != nil {
			t.Fatalf("signature did not verify under its own purpose: %v", err)
		}
		if err := verifyEd25519Signature(trusted, otherPurpose, canon, sig); err == nil {
			t.Fatalf("a plugin signature over %q verified under %q", signedPurpose, otherPurpose)
		}
	})

	t.Run("gpg", func(t *testing.T) {
		// RealVerify makes the stub do the cryptography rather than report a
		// canned verdict, so the purpose swap below is decided by whether the
		// bytes gpgVerifier wrote to the data file actually match what was
		// signed - not by anything the test told the stub to say.
		useGPGStub(t, gpgStubScript{
			List:              gpgStubOp{Stdout: colonListing(testFingerprint)},
			RealSign:          true,
			RealVerify:        true,
			VerifyFingerprint: testFingerprint,
		})
		s, err := SignerFor(ctx, "gpg:"+testFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		sig, err := s.Sign(ctx, signedPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		v, err := VerifierFor(ctx, KeySource{}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(ctx, signedPurpose, canon, sig); err != nil {
			t.Fatalf("gpg signature did not verify under its own purpose: %v", err)
		}
		if err := v.Verify(ctx, otherPurpose, canon, sig); err == nil {
			t.Fatalf("a gpg signature over %q verified under %q", signedPurpose, otherPurpose)
		}
	})
}

// TestDomainSeparation_PayloadIsBindingPerKind is the same replay question
// asked of the payload: same purpose, one byte of the manifest changed.
func TestDomainSeparation_PayloadIsBindingPerKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	trusted := map[string]ed25519.PublicKey{helperKeyID(): helperPublicKey()}

	canon := []byte(`{"target":{"arch":"amd64"}}`)
	tampered := []byte(`{"target":{"arch":"arm64"}}`)

	t.Run("ed25519-file", func(t *testing.T) {
		s, err := SignerFor(ctx, writeSeedKeyFile(t))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyEd25519Signature(trusted, manifest.SignPurpose, tampered, sig); err == nil {
			t.Fatal("an ed25519-file signature verified over a payload it was not made for")
		}
	})

	t.Run("plugin", func(t *testing.T) {
		s := mustStartHelperPlugin(t, ctx, "serve:good")
		sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyEd25519Signature(trusted, manifest.SignPurpose, tampered, sig); err == nil {
			t.Fatal("a plugin signature verified over a payload it was not made for")
		}
	})
}

// TestDomainSeparation_KindCannotBeSwappedAtVerify covers the other axis: a
// signature block is routed to a verifier by its signer_kind field, and that
// field lives in the bundle, where an attacker can edit it. Re-labelling a
// block must never turn a signature nobody can check into one that passes.
func TestDomainSeparation_KindCannotBeSwappedAtVerify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	privPath := writeSeedKeyFile(t)
	s, err := SignerFor(ctx, privPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canon := []byte(`{"real":"manifest"}`)
	sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
	if err != nil {
		t.Fatal(err)
	}

	// A verifier that trusts this key, and nothing else.
	pubPath := filepath.Join(t.TempDir(), "shared.pub")
	if err := os.WriteFile(pubPath, encodePublicKeyFile(helperPublicKey(), ""), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := VerifierFor(ctx, KeySource{Files: []string{pubPath}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(ctx, manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("control case failed: the untouched signature must verify: %v", err)
	}

	for _, kind := range []string{manifest.SignerSigstore, "plugin:something-nobody-configured", "", "ED25519-FILE"} {
		swapped := sig
		swapped.SignerKind = kind
		swapped.Algorithm = "" // an attacker would clear this too, since it is what routes an ed25519 block
		if err := v.Verify(ctx, manifest.SignPurpose, canon, swapped); err == nil {
			t.Errorf("a signature relabelled signer_kind=%q was accepted by a verifier that has no such verifier", kind)
		}
	}

	// The one relabelling that is deliberately still checkable: the algorithm
	// field, not the kind, is what selects the ed25519 path (see the comment
	// on compositeVerifier). A plugin-produced ed25519 signature whose key the
	// operator has separately trusted verifies as ed25519 - that is documented
	// behaviour, and it is safe because it still requires the private key.
	asPlugin := sig
	asPlugin.SignerKind = manifest.SignerPluginPrefix + "acme-hsm"
	if err := v.Verify(ctx, manifest.SignPurpose, canon, asPlugin); err != nil {
		t.Fatalf("an ed25519-algorithm block from a plugin should still verify against a trusted key: %v", err)
	}
	// ... but only over the bytes it was actually made for.
	if err := v.Verify(ctx, "some-other-purpose", canon, asPlugin); err == nil {
		t.Fatal("relabelling the kind let a signature escape its purpose binding")
	}
}

func argvHas(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}
