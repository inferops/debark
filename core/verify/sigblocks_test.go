package verify

import (
	"encoding/base64"
	"testing"

	"github.com/inferops/debark/core/manifest"
)

// This file covers the cost of checking a signature file, as distinct from the
// answer it produces.
//
// debark.manifest.sig is attacker-supplied and entirely unauthenticated at
// the moment verify starts reading it: nothing has been checked yet, and the
// file itself says which verifier each of its blocks needs. A gpg block costs
// a subprocess. So the attacker, not the operator, chose how much work the
// verification of their own bundle would take - a 29 MB .sig holding 200,000
// gpg blocks kept verify running past a minute, while the byte-identical file
// labelled ed25519-file finished in one second. The amplification is entirely
// in the subprocess path, and the label that selects it is attacker-chosen.
//
// The tests below are structural rather than timed: they assert that no
// verifier is built at all past the bound, and that block checking stops the
// moment the answer cannot change. A timing assertion would be flaky and would
// prove less.

// repeatSignatureBlocks rewrites the fixture's signature file to carry n
// copies of its one genuine block. Every copy is a real, valid signature over
// this manifest by the trusted operator key, which is the point: the refusal
// under test must be about the COUNT and nothing else, so it cannot be
// mistaken for the ordinary invalid-signature refusal.
func repeatSignatureBlocks(t *testing.T, f *fixture, n int) {
	t.Helper()
	sf := loadSig(t, f)
	one := sf.Signatures[0]
	sf.Signatures = make([]manifest.Signature, 0, n)
	for i := 0; i < n; i++ {
		sf.Signatures = append(sf.Signatures, one)
	}
	saveSig(t, f, sf)
}

// TestVerify_SignatureBlockFloodIsRefusedBeforeAnyVerifierRuns asserts the
// bound and, just as importantly, where it sits: before sign.VerifierFor. The
// evidence is report.Signatures being empty - not one block was examined, so
// not one subprocess could have been spawned - and FilesChecked being zero,
// because this is a step-3 refusal and the bundle's contents are never
// touched after one.
func TestVerify_SignatureBlockFloodIsRefusedBeforeAnyVerifierRuns(t *testing.T) {
	f := buildFixture(t)
	repeatSignatureBlocks(t, f, maxSignatureBlocks+1)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemManifestMalformed)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemManifestMalformed, manifest.SigFileName) {
		t.Fatalf("expected manifest-malformed naming %s; problems=%v", manifest.SigFileName, problemKinds(r))
	}
	if len(r.Signatures) != 0 {
		t.Fatalf("len(report.Signatures) = %d, want 0: %d blocks were checked before the count was refused",
			len(r.Signatures), len(r.Signatures))
	}
	if r.FilesChecked != 0 || r.BytesChecked != 0 {
		t.Fatalf("a step-3 refusal read %d file(s)/%d byte(s) of the bundle", r.FilesChecked, r.BytesChecked)
	}
}

// TestVerify_SignatureBlockCountAtTheBoundIsStillChecked is the other side of
// the bound: a file exactly at the limit is not refused for its size, and
// every one of its blocks is checked. Without this a later "tighten the
// bound" edit could pass the test above while quietly refusing bundles that
// are perfectly fine.
func TestVerify_SignatureBlockCountAtTheBoundIsStillChecked(t *testing.T) {
	f := buildFixture(t)
	repeatSignatureBlocks(t, f, maxSignatureBlocks)

	r, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK {
		t.Fatalf("a signature file at exactly the bound must still verify: problems=%v", problemKinds(r))
	}
	if len(r.Signatures) != maxSignatureBlocks {
		t.Fatalf("len(report.Signatures) = %d, want %d", len(r.Signatures), maxSignatureBlocks)
	}
}

// TestVerify_SignatureCheckingStopsOnceTheVerdictIsInvalid covers the early
// exit. One signature from a trusted key that does not verify settles the
// outcome for the whole file - checkSignatures turns it into
// ProblemSignatureInvalid and stops verification regardless of what follows -
// so every block after it is work spent on an attacker-chosen list after the
// answer is already known.
//
// The assertion is that exactly one block was examined out of many. Without
// the break every block is examined, which is what made a .sig full of gpg
// blocks worth writing.
func TestVerify_SignatureCheckingStopsOnceTheVerdictIsInvalid(t *testing.T) {
	f := buildFixture(t)
	sf := loadSig(t, f)
	genuine := sf.Signatures[0]

	// A trusted key, a well-formed signature of the right length, wrong bytes:
	// this is dferr.Verification (a tamper signal), not an untrusted key and
	// not an environment failure, which is the branch that settles the verdict.
	broken := genuine
	broken.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))

	sf.Signatures = []manifest.Signature{broken}
	for i := 0; i < 8; i++ {
		sf.Signatures = append(sf.Signatures, genuine)
	}
	saveSig(t, f, sf)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemSignatureInvalid)
	wantOnlyKnownKinds(t, r)
	if len(r.Signatures) != 1 {
		t.Fatalf("len(report.Signatures) = %d, want 1: checking continued after the verdict was settled", len(r.Signatures))
	}
	if r.Signatures[0].Valid {
		t.Fatal("the block that settled the verdict is recorded as valid")
	}
	if !r.Signatures[0].Trusted {
		t.Fatal("the block that settled the verdict must be recorded as from a trusted key")
	}
}
