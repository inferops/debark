package verify

import (
	"strings"
	"testing"

	"github.com/inferops/debark/core/manifest"
)

// This file covers the fields of a signature BLOCK that describe the
// signature rather than being it: signer_kind, algorithm, created_at and
// comment.
//
// None of them is covered by any signature. The signed pre-image is
// purpose||NUL||canonical-manifest, and debark.manifest.sig is excluded from
// the manifest's own file list, so every one of these strings is
// attacker-editable on a bundle that is otherwise perfectly valid. verify
// copies three of them - signer_kind, key_id, algorithm - straight into the
// SignatureResult an auditor reads, which is what makes them worth a test at
// this layer rather than only in core/sign: they are the provenance an
// operator is shown.
//
// The existing signature cases in tamper_test.go reach signer_kind by
// changing it to something with no verifier at all (sigstore, plugin:attacker),
// which lands on ErrUntrustedKey. The cases below are the harder shapes: a
// label that contradicts the check that actually ran, and labels that contradict
// nothing and therefore cannot be caught by anyone.

// TestVerify_ForgedAlgorithmOnANativeBlock covers the contradiction core/sign
// closes and verify reports: a block claiming signer_kind ed25519-file - a
// signer with exactly one algorithm - while naming some other algorithm.
//
// The signature bytes here are genuine and would verify. Only the label is
// false, and it must not be possible to relabel provenance for free: verify
// puts Algorithm into the report, so "rsa-4096, valid, trusted" was a
// free-form claim an auditor had no way to question. It is now a refusal, and
// the block is recorded valid=false trusted=true - the key really is one the
// operator trusts, which is precisely why the contradiction is evidence about
// the medium rather than an unknown signer.
func TestVerify_ForgedAlgorithmOnANativeBlock(t *testing.T) {
	for _, alg := range []string{"rsa-4096", "ecdsa-p256", "none"} {
		t.Run(alg, func(t *testing.T) {
			f := buildFixture(t)
			sf := loadSig(t, f)
			sf.Signatures[0].Algorithm = alg
			saveSig(t, f, sf)

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, ProblemSignatureInvalid)
			wantOnlyKinds(t, r, ProblemSignatureInvalid)
			wantOnlyKnownKinds(t, r)
			if len(r.Signatures) != 1 {
				t.Fatalf("len(report.Signatures) = %d, want 1", len(r.Signatures))
			}
			res := r.Signatures[0]
			if res.Valid {
				t.Fatal("a block whose own algorithm contradicts the check that ran is recorded valid")
			}
			if !res.Trusted {
				t.Fatal("the key is one the operator trusts; the report must say so, or this reads as an unknown signer")
			}
			// The report must show the operator the string that was actually
			// in the file, not a sanitised one: the whole finding is that the
			// document claims something it is not.
			if res.Algorithm != alg {
				t.Fatalf("report.Signatures[0].Algorithm = %q, want the verbatim claim %q", res.Algorithm, alg)
			}
			if !strings.Contains(res.Detail, alg) {
				t.Fatalf("the detail must name the claimed algorithm; got %q", res.Detail)
			}
		})
	}
}

// TestVerify_AlgorithmSpellingsThatClaimNothing is the other side of the rule
// above, and it exists so that tightening the check cannot quietly start
// refusing honest bundles.
//
//   - An empty algorithm claims nothing, so it cannot misdescribe anything.
//     Older signers left it empty and their bundles must keep verifying.
//   - A case-different spelling is the same claim. core/digest folds case for
//     digests and core/sign folds it here for the same reason: the alternative
//     is refusing a bundle over the shape of a letter.
//
// Both must PASS. If either ever starts failing, that is an API-visible
// tightening someone has to decide on deliberately, not discover in the field.
func TestVerify_AlgorithmSpellingsThatClaimNothing(t *testing.T) {
	for _, alg := range []string{"", "ED25519", "Ed25519"} {
		t.Run("algorithm="+alg, func(t *testing.T) {
			f := buildFixture(t)
			sf := loadSig(t, f)
			sf.Signatures[0].Algorithm = alg
			saveSig(t, f, sf)

			r, err := verifyFixture(t, f, Options{})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !r.OK {
				t.Fatalf("algorithm %q must not be a refusal; problems=%v", alg, problemKinds(r))
			}
			if !r.Signed || len(r.Signatures) != 1 || !r.Signatures[0].Valid {
				t.Fatalf("signature should still be valid and trusted: signed=%v sigs=%+v", r.Signed, r.Signatures)
			}
		})
	}
}

// TestVerify_UnauthenticatedSignatureMetadataIsNotSurfaced pins a deliberate
// limitation rather than a check, and it is written down because the shape of
// it is easy to get wrong later.
//
// created_at and comment are not covered by any signature and cannot be:
// binding them would take a changed signed pre-image (see the note on
// SigningInput in core/sign/iface.go), which is a format change, not a verify
// change. So an attacker can set them to anything on an otherwise-valid
// bundle, and verify has no way to know. That is accepted.
//
// What makes it acceptable is the second half, which this test is really
// here to hold: verify does not put either field in the report. Nothing an
// operator reads presents them as attested. The failure this guards against
// is somebody adding CreatedAt or Comment to SignatureResult "for
// completeness" - at which point attacker-chosen text would appear in an
// auditor's report of a bundle marked valid and trusted, with nothing saying
// it was never verified.
//
// The bundle therefore verifies OK here, and that is correct: the medium is
// genuinely untampered. Only the .sig's decorative fields are lies, and they
// reach nobody.
func TestVerify_UnauthenticatedSignatureMetadataIsNotSurfaced(t *testing.T) {
	const forgedComment = "release engineering key, audited 2026"
	const forgedTime = "1999-01-01T00:00:00Z"

	f := buildFixture(t)
	sf := loadSig(t, f)
	sf.Signatures[0].CreatedAt = forgedTime
	sf.Signatures[0].Comment = forgedComment
	saveSig(t, f, sf)

	r, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK {
		t.Fatalf("editing unsigned .sig metadata is not a tamper of the bundle; problems=%v", problemKinds(r))
	}

	// Neither string may appear anywhere in what an operator is shown.
	for _, res := range r.Signatures {
		if strings.Contains(res.Detail, forgedComment) || strings.Contains(res.Detail, forgedTime) {
			t.Fatalf("unauthenticated .sig metadata reached the report: %+v", res)
		}
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, forgedComment) || strings.Contains(w, forgedTime) {
			t.Fatalf("unauthenticated .sig metadata reached a warning: %q", w)
		}
	}
	// CreatedAt in the report is the MANIFEST's, which is signed. If these
	// two were ever confused, a bundle's age - the thing an operator checks
	// to spot a replayed medium - would become attacker-controlled.
	if r.CreatedAt == forgedTime {
		t.Fatal("report.CreatedAt came from the unsigned signature block, not the signed manifest")
	}
}

// TestVerify_SignerKindRelabelledAsAPlugin pins the one relabelling that
// genuinely cannot be caught, so that its cost is visible in a test rather
// than only in a comment three packages away.
//
// "ed25519-file" and "plugin:acme-hsm" are the same arithmetic over the same
// key. The difference between them is where the private half was kept, and
// that leaves no trace in a signature - so a bundle signed with a key file on
// a laptop can be relabelled as signed by an HSM, and the report will say
// signer_kind plugin:acme-hsm, valid, trusted. core/sign documents this and
// cannot close it; neither can verify.
//
// The property that survives, and that this asserts, is the one that actually
// carries trust: the KEY ID is unforgeable, because it selects which public
// key the arithmetic runs against, and the operator assembled that trust set
// out of band. An auditor who checks the key id learns the truth; an auditor
// who reads signer_kind as provenance does not.
//
// If the signed pre-image is ever widened to cover the block's own labels,
// this test is the one that has to be revisited - deliberately, with the
// format change - rather than a silent behaviour drift nobody notices.
func TestVerify_SignerKindRelabelledAsAPlugin(t *testing.T) {
	const relabel = manifest.SignerPluginPrefix + "acme-hsm"

	f := buildFixture(t)
	genuine := loadSig(t, f)
	trueKeyID := genuine.Signatures[0].KeyID

	genuine.Signatures[0].SignerKind = relabel
	saveSig(t, f, genuine)

	r, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK {
		t.Fatalf("relabelling the mechanism does not break the arithmetic; problems=%v", problemKinds(r))
	}
	if len(r.Signatures) != 1 {
		t.Fatalf("len(report.Signatures) = %d, want 1", len(r.Signatures))
	}
	if got := r.Signatures[0].SignerKind; got != relabel {
		t.Fatalf("report.Signatures[0].SignerKind = %q, want the verbatim (false) claim %q", got, relabel)
	}
	// The half that is not forgeable, and the reason this is a limitation
	// rather than a hole: the report still names the key that really signed.
	if got := r.Signatures[0].KeyID; got != trueKeyID {
		t.Fatalf("report.Signatures[0].KeyID = %q, want the key that actually signed, %q", got, trueKeyID)
	}
}

// TestVerify_SignerKindRelabelledAsAPluginWithNoAlgorithm is the boundary of
// the case above, and it is a refusal rather than a pass.
//
// A plugin block gets ed25519 arithmetic only when it SAYS ed25519. Strip the
// algorithm and there is nothing left claiming the block is checkable at all,
// so it is an untrusted key - not tamper. The distinction matters: calling an
// honest bundle from an unsupported signer "tampered" would be debark's
// loudest verdict fired at the wrong target.
func TestVerify_SignerKindRelabelledAsAPluginWithNoAlgorithm(t *testing.T) {
	f := buildFixture(t)
	sf := loadSig(t, f)
	sf.Signatures[0].SignerKind = manifest.SignerPluginPrefix + "acme-hsm"
	sf.Signatures[0].Algorithm = ""
	saveSig(t, f, sf)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemSignatureUntrusted)
	wantOnlyKinds(t, r, ProblemSignatureUntrusted)
	wantOnlyKnownKinds(t, r)
	if len(r.Signatures) != 1 || r.Signatures[0].Valid || r.Signatures[0].Trusted {
		t.Fatalf("want one block recorded valid=false trusted=false; got %+v", r.Signatures)
	}
}

// TestVerify_BareSignerKindPluginPrefix covers "plugin:" with no name after
// it. It names no plugin, so it is not a plugin signer kind and cannot be
// routed anywhere - the default branch, untrusted. It is here because a
// prefix test written as HasPrefix alone would accept it and then attempt
// ed25519 arithmetic on a block that never claimed to be one, turning an
// unsupported signer into a tamper verdict.
func TestVerify_BareSignerKindPluginPrefix(t *testing.T) {
	f := buildFixture(t)
	sf := loadSig(t, f)
	sf.Signatures[0].SignerKind = manifest.SignerPluginPrefix
	saveSig(t, f, sf)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemSignatureUntrusted)
	wantOnlyKinds(t, r, ProblemSignatureUntrusted)
	wantOnlyKnownKinds(t, r)
}

// TestVerify_SignatureFileSchemaIsNotAttackerChoosable checks the one field of
// the signature FILE, as opposed to a block, that has not already been worked:
// its schema string. It is read before anything is verified, so an
// unrecognised value must end the run at step 1 with the kind that tells an
// operator to check their debark version rather than their medium - and it
// must never be treated as "a schema I do not know, so I will assume the
// defaults".
func TestVerify_SignatureFileSchemaIsNotAttackerChoosable(t *testing.T) {
	f := buildFixture(t)
	sf := loadSig(t, f)
	sf.Schema = manifest.SignatureSchemaVersion + "-plus"
	saveSig(t, f, sf)

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemSchemaUnknown)
	wantOnlyKinds(t, r, ProblemSchemaUnknown)
	wantOnlyKnownKinds(t, r)
	if !hasProblem(r, ProblemSchemaUnknown, manifest.SigFileName) {
		t.Fatalf("expected %s naming %s; problems=%v", ProblemSchemaUnknown, manifest.SigFileName, problemKinds(r))
	}
	if r.FilesChecked != 0 || r.BytesChecked != 0 {
		t.Fatalf("a step-1 refusal read %d file(s)/%d byte(s) of the bundle", r.FilesChecked, r.BytesChecked)
	}
}
