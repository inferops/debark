package sign

// The gpg signer and verifier.
//
// All but ONE test in this file drives a stub, never the real gpg. gpg.go
// declares `var gpgBinary = "gpg"` precisely so tests can point it somewhere
// else, and that is what happens here: core/sign is aimed at this test binary
// re-entered as a gpg stub (helperprocess_test.go). That buys three things the
// real binary cannot give: the tests run identically on a machine with no
// gnupg installed, the failure paths (no secret key, a BADSIG, a REVKEYSIG, a
// fingerprint that disagrees with the signature block) are reachable
// deterministically instead of by contriving a revoked or broken keyring, and
// no test can ever touch, create or prompt against an operator's real keys.
//
// The stub is not a yes-man. For the round trips it performs genuine ed25519
// signing and verification over exactly the bytes gpg.go hands it, so a
// gpgSigner that fed gpg the wrong thing produces a signature that fails - see
// TestDomainSeparation_AllKindsSignIdenticalBytes in domain_test.go.
//
// The exception is TestGPG_SignVerify_SigningSubkey_RealGPG at the bottom,
// which needs a real gpg because what it checks IS gpg's own output: that a
// signature made by a [S] subkey still names the primary key on its VALIDSIG
// line, in the field position this package parses. A stub asserting the shape
// debark expects could not tell anyone whether gpg still produces it. It is
// fenced - throwaway GNUPGHOME, loopback pinentry, agent killed - so it can
// neither reach an operator's keys nor open a dialog on their desktop.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// testFingerprint is the 40-hex primary-key fingerprint the stub reports.
const testFingerprint = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"

// testSubkeyFingerprint belongs to a signing SUBKEY of the same key. gpg
// prints it in the same listing, immediately after an "ssb" record, and
// mistaking it for the primary is the specific bug parseColonFingerprint
// exists to avoid: KeyID() would then record a fingerprint that verification
// never sees, and every bundle signed by that key would fail the
// "claims key X but gpg validated it against Y" check on the target.
const testSubkeyFingerprint = "99998888777766665555444433332222111100AA"

// colonListing renders a realistic `gpg --with-colons --fingerprint
// --list-secret-keys` reply: a primary secret key with its fingerprint,
// followed by a signing subkey with its own.
func colonListing(primary string) string {
	return "sec:u:255:22:" + primary[24:] + ":1767225600:::u:::scESC:::+:::23::0:\n" +
		"fpr:::::::::" + primary + ":\n" +
		"grp:::::::::0000000000000000000000000000000000000000:\n" +
		"uid:u::::1767225600::AAAA::debark release engineering <rel@example.invalid>::::::::::0:\n" +
		"ssb:u:255:22:" + testSubkeyFingerprint[24:] + ":1767225600::::::s:::+:::23:\n" +
		"fpr:::::::::" + testSubkeyFingerprint + ":\n"
}

// TestParseColonFingerprint is a pure-function test over gpg's machine-readable
// listing. Getting the primary/subkey distinction wrong here is silent: the
// signer would come up, sign happily, and record a key id no verifier can
// match.
func TestParseColonFingerprint(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "primary secret key with a signing subkey",
			out:  colonListing(testFingerprint),
			want: testFingerprint,
		},
		{
			name: "public listing uses pub instead of sec",
			out:  "pub:u:255:22:AAAA:1767225600:::u:::scESC::::::23::0:\nfpr:::::::::" + testFingerprint + ":\n",
			want: testFingerprint,
		},
		{
			// gpg prints hex in upper case, but the field is normalised on the
			// way in so the comparison against a signature block's key_id can
			// be a plain equality of upper-cased strings.
			name: "lower-case fingerprint is normalised",
			out:  "sec:u:255:22:AAAA:1767225600:::u:::scESC:\nfpr:::::::::" + strings.ToLower(testFingerprint) + ":\n",
			want: testFingerprint,
		},
		{
			// A subkey fingerprint with no primary ahead of it must not be
			// adopted: "sub" turns primary tracking off.
			name: "only a subkey fingerprint",
			out:  "sub:u:255:22:AAAA:1767225600::::::s:\nfpr:::::::::" + testSubkeyFingerprint + ":\n",
			want: "",
		},
		{
			name: "no fingerprint record at all",
			out:  "sec:u:255:22:AAAA:1767225600:::u:::scESC:\nuid:u::::::::someone:\n",
			want: "",
		},
		{
			// gpg found nothing. An empty answer must stay empty rather than
			// becoming some default key.
			name: "empty output",
			out:  "",
			want: "",
		},
		{
			name: "truncated fpr record",
			out:  "sec:u:255:22:AAAA:\nfpr:::\n",
			want: "",
		},
		{
			name: "fpr record with an empty fingerprint field",
			out:  "sec:u:255:22:AAAA:\nfpr:::::::::" + ":\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseColonFingerprint([]byte(tc.out)); got != tc.want {
				t.Fatalf("parseColonFingerprint = %q, want %q", got, tc.want)
			}
		})
	}
}

// validsigLine renders a VALIDSIG the way a real gpg 2.x prints it: ten
// arguments, the SIGNING key's fingerprint first and the PRIMARY key's last.
// The two differ whenever the operator signs with a subkey, which is the
// normal shape of a hardened gpg setup.
func validsigLine(signing, primary string) string {
	return "[GNUPG:] VALIDSIG " + signing + " 2026-01-01 1767225600 0 4 0 22 8 00 " + primary + "\n"
}

// TestParseVerifyStatus reads gpg's status-fd protocol, which is the only
// output of gpg that debark treats as authoritative. Everything else the
// tool prints - including its human-readable "Good signature from" chatter on
// stderr - is advisory.
//
// The trap this test exists to keep shut: gpg emits REVKEYSIG, EXPKEYSIG and
// EXPSIG *alongside* VALIDSIG, not instead of it. A parser that answered
// "good" on the strength of a VALIDSIG line therefore accepted a signature by
// a revoked key as fully valid - which made revocation, the only remedy an
// organisation has after a release key is stolen, completely inert.
func TestParseVerifyStatus(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want gpgVerifyStatus
	}{
		{
			name: "valid signature, primary key signed directly",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG ABCD signer\n" + validsigLine(testFingerprint, testFingerprint),
			want: gpgVerifyStatus{signingKey: testFingerprint, primaryKey: testFingerprint, valid: true, sawStatus: true},
		},
		{
			// The layout the fingerprint cross-check used to reject: a
			// signature made by the [S] subkey of the key whose PRIMARY
			// fingerprint the signature block records.
			name: "valid signature made by a signing subkey",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG ABCD signer\n" + validsigLine(testSubkeyFingerprint, testFingerprint),
			want: gpgVerifyStatus{signingKey: testSubkeyFingerprint, primaryKey: testFingerprint, valid: true, sawStatus: true},
		},
		{
			// One argument short of gpg 2.x's ten. The primary fingerprint is
			// still the last field, and must still be found - otherwise a gpg
			// that prints it differently silently reintroduces the
			// subkey-mismatch failure.
			name: "nine-argument VALIDSIG",
			out:  "[GNUPG:] VALIDSIG " + testSubkeyFingerprint + " 2026-01-01 0 4 0 22 8 00 " + testFingerprint + "\n",
			want: gpgVerifyStatus{signingKey: testSubkeyFingerprint, primaryKey: testFingerprint, valid: true, sawStatus: true},
		},
		{
			// Nothing fingerprint-shaped at the end: no primary is adopted,
			// which only makes the cross-check stricter.
			name: "VALIDSIG whose trailing field is not a fingerprint",
			out:  "[GNUPG:] VALIDSIG " + testFingerprint + " 2026-01-01 1767225600 0 4 0 22 8 00 -\n",
			want: gpgVerifyStatus{signingKey: testFingerprint, valid: true, sawStatus: true},
		},
		{
			// THE case. gpg says the maths is good AND that the key is
			// revoked, in the same stream.
			name: "revoked key: REVKEYSIG alongside VALIDSIG",
			out: "[GNUPG:] NEWSIG\n[GNUPG:] KEYREVOKED\n[GNUPG:] REVKEYSIG ABCD signer\n" +
				validsigLine(testFingerprint, testFingerprint),
			want: gpgVerifyStatus{signingKey: testFingerprint, primaryKey: testFingerprint, valid: true, rejected: "REVKEYSIG", sawStatus: true},
		},
		{
			name: "expired key: EXPKEYSIG alongside VALIDSIG",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] EXPKEYSIG ABCD signer\n" + validsigLine(testFingerprint, testFingerprint),
			want: gpgVerifyStatus{signingKey: testFingerprint, primaryKey: testFingerprint, valid: true, rejected: "EXPKEYSIG", sawStatus: true},
		},
		{
			name: "expired signature: EXPSIG alongside VALIDSIG",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] EXPSIG ABCD signer\n" + validsigLine(testFingerprint, testFingerprint),
			want: gpgVerifyStatus{signingKey: testFingerprint, primaryKey: testFingerprint, valid: true, rejected: "EXPSIG", sawStatus: true},
		},
		{
			name: "bad signature",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] BADSIG ABCD signer\n",
			want: gpgVerifyStatus{rejected: "BADSIG", sawStatus: true},
		},
		{
			// A stream mixing outcomes cannot be laundered into an accept:
			// the rejection is recorded independently of the VALIDSIG, and
			// the caller checks it first.
			name: "valid then bad in one stream",
			out:  validsigLine(testFingerprint, testFingerprint) + "[GNUPG:] BADSIG ABCD signer\n",
			want: gpgVerifyStatus{signingKey: testFingerprint, primaryKey: testFingerprint, valid: true, rejected: "BADSIG", sawStatus: true},
		},
		{
			// No key: gpg cannot even attempt the check. rc 9 is
			// GPG_ERR_NO_PUBKEY, and this is the ONE outcome that is about
			// the verifier's trust set rather than about the bundle.
			name: "no public key",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG ABCD 22 8 00 2026-01-01 9 " + testFingerprint + "\n[GNUPG:] NO_PUBKEY ABCD\n",
			want: gpgVerifyStatus{noPublicKey: true, sawStatus: true},
		},
		{
			// ERRSIG for any other reason (here rc 4, an algorithm this gpg
			// cannot handle) is a failure, not a missing key.
			name: "errsig that is not a missing key",
			out:  "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG ABCD 22 8 00 2026-01-01 4 " + testFingerprint + "\n",
			want: gpgVerifyStatus{rejected: "ERRSIG", sawStatus: true},
		},
		{
			// Human-readable gpg output on its own proves nothing; only the
			// status stream counts. sawStatus stays false, which is how the
			// caller tells "gpg answered and we did not like it" from "gpg
			// never really ran".
			name: "chatty output with no status lines",
			out:  "gpg: Signature made Thu Jan  1 00:00:00 2026\ngpg: Good signature from \"someone\"\n",
			want: gpgVerifyStatus{},
		},
		{
			name: "VALIDSIG with no fingerprint field",
			out:  "[GNUPG:] VALIDSIG\n",
			want: gpgVerifyStatus{sawStatus: true},
		},
		{
			name: "empty output",
			out:  "",
			want: gpgVerifyStatus{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVerifyStatus([]byte(tc.out)); got != tc.want {
				t.Fatalf("parseVerifyStatus =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// TestClassifyGPGError maps gpg's (version- and locale-dependent) stderr onto
// the class and the next action an operator takes. dferr.Class is the process
// exit code, so "the operator cancelled the pinentry" being usage (1) rather
// than environment (2) is something a build script can branch on.
func TestClassifyGPGError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		class  dferr.Class
		hint   string
		// dropsRawText marks the one branch that replaces gpg's own wording
		// with debark's rather than appending to it. See the note below.
		dropsRawText bool
	}{
		{
			name: "no secret key", stderr: "gpg: skipped \"ABCD\": No secret key\n",
			class: dferr.Environment, hint: "gpg --list-secret-keys",
		},
		{
			name: "secret key not available", stderr: "gpg: signing failed: Secret key not available\n",
			class: dferr.Environment, hint: "gpg --list-secret-keys",
		},
		{
			name: "agent socket missing", stderr: "gpg: can't connect to the agent: No such file or directory\n",
			class: dferr.Environment, hint: "gpgconf --launch gpg-agent",
		},
		{
			name: "agent generally unhappy", stderr: "gpg: agent_genkey failed: gpg-agent is not reachable\n",
			class: dferr.Environment, hint: "gpg-agent is running",
		},
		{
			// The operator dismissed the passphrase prompt. That is a choice,
			// not a broken machine.
			//
			// This is also the one branch that does not honour the "the raw
			// first stderr line always rides along" rule stated in
			// classifyGPGError's own doc comment: it builds a fresh message
			// and drops gpg's text. Harmless while the match is unambiguous,
			// but it means a false match on the word "cancel" in some other
			// locale would be reported with no evidence of what gpg said.
			name: "pinentry cancelled", stderr: "gpg: signing failed: Operation cancelled\n",
			class: dferr.Usage, dropsRawText: true,
		},
		{
			name: "no public key", stderr: "gpg: Can't check signature: No public key\n",
			class: dferr.Environment, hint: "import the signer's public key",
		},
		{
			name: "something unrecognised", stderr: "gpg: the flux capacitor is misaligned\n",
			class: dferr.Environment,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyGPGError(errors.New("exit status 2"), []byte(tc.stderr), "sign")
			if got := dferr.ClassOf(err); got != tc.class {
				t.Errorf("class = %v, want %v (err: %v)", got, tc.class, err)
			}
			// The raw gpg text rides along, so a mis-classification is still
			// debuggable from the message alone.
			if !tc.dropsRawText {
				first := strings.TrimSpace(strings.SplitN(tc.stderr, "\n", 2)[0])
				if !strings.Contains(err.Error(), first) {
					t.Errorf("error %q drops gpg's own text %q", err.Error(), first)
				}
			}
			if tc.hint != "" && !strings.Contains(dferr.HintOf(err), tc.hint) {
				t.Errorf("hint %q does not mention %q", dferr.HintOf(err), tc.hint)
			}
		})
	}
}

func TestFirstMeaningfulLine(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		err    error
		want   string
	}{
		{name: "first non-blank line", stderr: "\n\n  gpg: something went wrong  \nand more\n", want: "gpg: something went wrong"},
		{name: "falls back to the error", stderr: "", err: errors.New("exit status 2"), want: "exit status 2"},
		{name: "nothing at all", stderr: "   \n\n", want: "unknown error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstMeaningfulLine([]byte(tc.stderr), tc.err); got != tc.want {
				t.Fatalf("firstMeaningfulLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGPGSigner_RejectsEmptyKeyRef: "gpg:" with nothing after it must not turn
// into "sign with whatever default key gpg picks". Which key signed a release
// is not something to leave to a default.
func TestGPGSigner_RejectsEmptyKeyRef(t *testing.T) {
	s, err := SignerFor(context.Background(), "gpg:")
	if err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted \"gpg:\" with no key reference")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Fatalf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

// TestGPG_BinaryMissing: on a machine with no gnupg, both halves must say so
// as an environment problem with a way forward - never as a verification
// failure, which would read as "this bundle is bad".
func TestGPG_BinaryMissing(t *testing.T) {
	prev := gpgBinary
	gpgBinary = "debark-no-such-gpg-binary"
	t.Cleanup(func() { gpgBinary = prev })

	t.Run("signer", func(t *testing.T) {
		s, err := SignerFor(context.Background(), "gpg:"+testFingerprint)
		if err == nil {
			_ = s.Close()
			t.Fatal("SignerFor succeeded with no gpg on PATH")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("class = %v, want Environment", dferr.ClassOf(err))
		}
		if !strings.Contains(dferr.HintOf(err), "install gnupg") {
			t.Errorf("hint does not tell the operator to install gnupg: %q", dferr.HintOf(err))
		}
	})

	t.Run("verifier", func(t *testing.T) {
		v := &gpgVerifier{}
		err := v.verify(context.Background(), manifest.SignPurpose, []byte("x"),
			manifest.Signature{SignerKind: manifest.SignerGPG, KeyID: testFingerprint, Signature: "AAAA"})
		if err == nil {
			t.Fatal("gpg verification succeeded with no gpg on PATH")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("class = %v, want Environment (a missing tool is not evidence about the bundle)", dferr.ClassOf(err))
		}
		if errors.Is(err, ErrUntrustedKey) {
			t.Error("a missing gpg binary was reported as an untrusted key")
		}
	})
}

// TestGPGSigner_KeyLookupFailures: the fingerprint lookup happens at signer
// construction, before anything is signed, so its failures are the ones an
// operator meets first.
func TestGPGSigner_KeyLookupFailures(t *testing.T) {
	t.Run("gpg succeeds but knows no such key", func(t *testing.T) {
		// Exit 0, empty listing. gpg does this for a key it simply does not
		// have. An empty fingerprint must not become the signer's KeyID.
		useGPGStub(t, gpgStubScript{List: gpgStubOp{Stdout: ""}})
		s, err := SignerFor(context.Background(), "gpg:nobody@example.invalid")
		if err == nil {
			_ = s.Close()
			t.Fatal("SignerFor built a gpg signer with no fingerprint")
		}
		if !strings.Contains(err.Error(), "no secret key found") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(dferr.HintOf(err), "gpg --list-secret-keys") {
			t.Errorf("hint does not tell the operator how to check: %q", dferr.HintOf(err))
		}
	})

	t.Run("gpg fails outright", func(t *testing.T) {
		useGPGStub(t, gpgStubScript{List: gpgStubOp{
			Stderr: "gpg: error reading key: No secret key\n", Exit: 2,
		}})
		s, err := SignerFor(context.Background(), "gpg:nobody@example.invalid")
		if err == nil {
			_ = s.Close()
			t.Fatal("SignerFor ignored a non-zero gpg exit")
		}
		if dferr.ClassOf(err) != dferr.Environment {
			t.Errorf("class = %v, want Environment", dferr.ClassOf(err))
		}
		if !strings.Contains(err.Error(), "look up secret key") {
			t.Errorf("error does not say what was being attempted: %v", err)
		}
	})

	t.Run("lookup succeeds", func(t *testing.T) {
		argsFile := useGPGStub(t, gpgStubScript{List: gpgStubOp{Stdout: colonListing(testFingerprint)}})
		s, err := SignerFor(context.Background(), "gpg:rel@example.invalid")
		if err != nil {
			t.Fatalf("SignerFor: %v", err)
		}
		defer s.Close()
		if s.Kind() != manifest.SignerGPG {
			t.Fatalf("Kind() = %q, want %q", s.Kind(), manifest.SignerGPG)
		}
		// The PRIMARY fingerprint, not the signing subkey's - see
		// testSubkeyFingerprint.
		if s.KeyID() != testFingerprint {
			t.Fatalf("KeyID() = %q, want the primary fingerprint %q", s.KeyID(), testFingerprint)
		}
		inv := gpgStubInvocations(t, argsFile)
		if len(inv) != 1 {
			t.Fatalf("expected exactly one gpg invocation for the lookup, got %d", len(inv))
		}
		for _, want := range []string{"--batch", "--with-colons", "--list-secret-keys", "rel@example.invalid"} {
			if !argvHas(inv[0], want) {
				t.Errorf("key lookup did not pass %q: %v", want, inv[0])
			}
		}
	})
}

// TestGPGSigner_SignFailure: gpg refusing to sign must be an error, never an
// empty signature recorded as success.
func TestGPGSigner_SignFailure(t *testing.T) {
	useGPGStub(t, gpgStubScript{
		List: gpgStubOp{Stdout: colonListing(testFingerprint)},
		Sign: gpgStubOp{Stderr: "gpg: signing failed: Operation cancelled\n", Exit: 2},
	})
	s, err := SignerFor(context.Background(), "gpg:rel@example.invalid")
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer s.Close()

	sig, err := s.Sign(context.Background(), manifest.SignPurpose, []byte("canonical"))
	if err == nil {
		t.Fatalf("Sign reported success after gpg failed: %+v", sig)
	}
	if sig.Signature != "" || sig.KeyID != "" {
		t.Fatalf("Sign returned an error AND a populated block: %+v", sig)
	}
	// A cancelled pinentry is the operator's decision, so it is a usage error.
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage for a cancelled prompt (err: %v)", dferr.ClassOf(err), err)
	}
}

// TestGPGVerifier_Outcomes walks the verdicts gpg can return and what each one
// must mean to debark. The fingerprint cross-check is the interesting one:
// gpg answers "this signature is valid, made by KEY" and debark has to
// notice when KEY is not the key the signature block claimed.
func TestGPGVerifier_Outcomes(t *testing.T) {
	canon := []byte(`{"manifest":"bytes"}`)

	// A real signature over the real signed input, so the "good" case is not
	// simply the stub being told to say yes.
	makeSigner := func(t *testing.T) (Signer, string) {
		t.Helper()
		argsFile := useGPGStub(t, gpgStubScript{
			List:              gpgStubOp{Stdout: colonListing(testFingerprint)},
			RealSign:          true,
			RealVerify:        true,
			VerifyFingerprint: testFingerprint,
		})
		s, err := SignerFor(context.Background(), "gpg:rel@example.invalid")
		if err != nil {
			t.Fatalf("SignerFor: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s, argsFile
	}

	t.Run("good signature", func(t *testing.T) {
		s, argsFile := makeSigner(t)
		sig, err := s.Sign(context.Background(), manifest.SignPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		v := &gpgVerifier{}
		if err := v.verify(context.Background(), manifest.SignPurpose, canon, sig); err != nil {
			t.Fatalf("verify rejected a good signature: %v", err)
		}
		inv := gpgStubInvocations(t, argsFile)
		last := inv[len(inv)-1]
		for _, want := range []string{"--batch", "--status-fd", "--verify"} {
			if !argvHas(last, want) {
				t.Errorf("verification did not pass %q: %v", want, last)
			}
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		s, _ := makeSigner(t)
		sig, err := s.Sign(context.Background(), manifest.SignPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		v := &gpgVerifier{}
		// One byte of the manifest changed after signing. The stub does the
		// real cryptography, so this is a genuine BADSIG.
		//
		// The classification is pinned, not just the failure. A BADSIG is the
		// clearest tamper signal debark can get, and it used to be reported
		// as ErrUntrustedKey - "signed by a key you do not trust" - because
		// gpg prints no fingerprint alongside a BADSIG and the verifier read
		// "no fingerprint" as "trust could not be established". That sent an
		// operator whose media WAS tampered with off to debug their keyring,
		// and it is the difference between core/verify reporting
		// ProblemSignatureInvalid and reporting an untrusted key.
		err = v.verify(context.Background(), manifest.SignPurpose, []byte(`{"manifest":"BYTES"}`), sig)
		if err == nil {
			t.Fatal("verify accepted a gpg signature over a payload it was not made for")
		}
		if errors.Is(err, ErrUntrustedKey) {
			t.Errorf("a tampered payload was reported as an untrusted key: %v", err)
		}
		if dferr.ClassOf(err) != dferr.Verification {
			t.Errorf("class = %v, want Verification (err: %v)", dferr.ClassOf(err), err)
		}
		if !strings.Contains(err.Error(), "does not verify") {
			t.Errorf("error does not say the signature does not verify: %v", err)
		}
	})

	t.Run("gpg validated a different key than the block claims", func(t *testing.T) {
		// The signature really is valid - gpg says so - but against a key
		// other than the one the signature block names. Accepting it would let
		// a bundle claim it was signed by the release key while actually
		// carrying a valid signature from some other key the operator's
		// keyring happens to hold.
		s, _ := makeSigner(t)
		sig, err := s.Sign(context.Background(), manifest.SignPurpose, canon)
		if err != nil {
			t.Fatal(err)
		}
		sig.KeyID = "1111222233334444555566667777888899990000"

		v := &gpgVerifier{}
		err = v.verify(context.Background(), manifest.SignPurpose, canon, sig)
		if err == nil {
			t.Fatal("verify accepted a signature whose key_id disagrees with the key gpg validated it against")
		}
		if dferr.ClassOf(err) != dferr.Verification {
			t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
		}
		if !strings.Contains(err.Error(), "claims key") {
			t.Errorf("error does not explain the disagreement: %v", err)
		}
	})

	t.Run("gpg has no key for this signature", func(t *testing.T) {
		// No VALIDSIG line at all: gpg could not check it. That is
		// specifically the untrusted-key answer, and it must be distinguishable
		// from "the bytes do not match", because the operator's remedy is
		// different (obtain the key, versus stop trusting the bundle).
		useGPGStub(t, gpgStubScript{
			Verify: gpgStubOp{
				Stdout: "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG ABCD 22 8 00 2026-01-01 9\n[GNUPG:] NO_PUBKEY ABCD\n",
				Stderr: "gpg: Can't check signature: No public key\n",
				Exit:   2,
			},
		})
		v := &gpgVerifier{}
		err := v.verify(context.Background(), manifest.SignPurpose, canon, manifest.Signature{
			SignerKind: manifest.SignerGPG,
			KeyID:      testFingerprint,
			Signature:  base64.StdEncoding.EncodeToString([]byte("some detached signature bytes")),
		})
		if !errors.Is(err, ErrUntrustedKey) {
			t.Fatalf("err = %v, want ErrUntrustedKey", err)
		}
	})

	t.Run("signature field is not base64", func(t *testing.T) {
		useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: ""}})
		v := &gpgVerifier{}
		err := v.verify(context.Background(), manifest.SignPurpose, canon, manifest.Signature{
			SignerKind: manifest.SignerGPG, KeyID: testFingerprint, Signature: "@@@not base64@@@",
		})
		if err == nil {
			t.Fatal("verify accepted a signature block whose signature is not base64")
		}
		if dferr.ClassOf(err) != dferr.Verification {
			t.Errorf("class = %v, want Verification", dferr.ClassOf(err))
		}
	})
}

// TestGPGVerifier_KeyringSelection: when the operator designates a keyring,
// gpg must be told to use ONLY that one. Falling back to the default keyring
// would silently widen the trust set to whatever is imported on the machine -
// which is exactly the ambient trust the design refuses (ADR-008).
func TestGPGVerifier_KeyringSelection(t *testing.T) {
	keyring := filepath.Join(t.TempDir(), "release.gpg")
	if err := os.WriteFile(keyring, []byte("not a real keyring"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFile := useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: ""}})

	v, err := VerifierFor(context.Background(), KeySource{GPGKeyring: keyring}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	_ = v.Verify(context.Background(), manifest.SignPurpose, []byte("x"), manifest.Signature{
		SignerKind: manifest.SignerGPG, KeyID: testFingerprint,
		Signature: base64.StdEncoding.EncodeToString([]byte("sig")),
	})

	inv := gpgStubInvocations(t, argsFile)
	if len(inv) == 0 {
		t.Fatal("gpg was never invoked")
	}
	last := inv[len(inv)-1]
	if !argvHas(last, "--no-default-keyring") {
		t.Errorf("the designated keyring did not suppress the default one: %v", last)
	}
	if !argvHas(last, "--keyring") || !argvHas(last, keyring) {
		t.Errorf("the designated keyring was not passed to gpg: %v", last)
	}

	// And with no keyring designated, neither flag appears: the operator's
	// default keyring is used, which verify separately warns about.
	argsFile2 := useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: ""}})
	v2, err := VerifierFor(context.Background(), KeySource{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = v2.Verify(context.Background(), manifest.SignPurpose, []byte("x"), manifest.Signature{
		SignerKind: manifest.SignerGPG, KeyID: testFingerprint,
		Signature: base64.StdEncoding.EncodeToString([]byte("sig")),
	})
	inv2 := gpgStubInvocations(t, argsFile2)
	if len(inv2) == 0 {
		t.Fatal("gpg was never invoked")
	}
	if argvHas(inv2[len(inv2)-1], "--keyring") {
		t.Errorf("a keyring was passed when none was designated: %v", inv2[len(inv2)-1])
	}
}

// TestParseVerifyStatus_MalformedStatusLines: gpg's status stream is parsed
// field-by-field, and a truncated or empty status line must be stepped over
// rather than indexed into. A panic here would crash `debark verify` on a
// bundle instead of failing it, which on an air-gapped target is a much worse
// outcome than a clean rejection.
func TestParseVerifyStatus_MalformedStatusLines(t *testing.T) {
	for _, out := range []string{
		"[GNUPG:] \n",
		"[GNUPG:]\n",
		"[GNUPG:]    \n[GNUPG:] BADSIG\n",
		"[GNUPG:] VALIDSIG\n[GNUPG:] \n",
		"[GNUPG:] ERRSIG\n",
		"[GNUPG:] ERRSIG ABCD 22\n",
		"[GNUPG:] VALIDSIG " + testFingerprint + "\n",
	} {
		st := parseVerifyStatus([]byte(out))
		if st.valid && st.signingKey == "" {
			t.Errorf("parseVerifyStatus(%q) reported a valid signature with no fingerprint", out)
		}
		if st.primaryKey != "" {
			t.Errorf("parseVerifyStatus(%q) invented a primary fingerprint %q", out, st.primaryKey)
		}
	}
}

// TestGPGVerifier_RefusesDisqualifiedSignatures is the regression test for
// the finding that mattered most: a signature made by a REVOKED key verified
// as fully valid, with no warning.
//
// gpg's answer has two independent halves and the old code read neither
// properly. REVKEYSIG/EXPKEYSIG/EXPSIG arrive ALONGSIDE VALIDSIG rather than
// instead of it, so looking for VALIDSIG alone accepted them; and gpg's exit
// status was captured only to be spliced into a diagnostic string, never
// consulted for the verdict. Both halves are asserted here, including the
// case where gpg exits ZERO while saying the key is revoked - whether a given
// gpg version does that is a version detail debark must not depend on.
//
// The consequence of getting this wrong is not academic: after a release key
// is stolen, publishing a revocation is the ONLY remedy an organisation has,
// and a verifier that ignores REVKEYSIG makes that remedy inert while the
// thief keeps signing bundles that every target accepts.
func TestGPGVerifier_RefusesDisqualifiedSignatures(t *testing.T) {
	canon := []byte(`{"manifest":"bytes"}`)
	block := manifest.Signature{
		SignerKind: manifest.SignerGPG,
		KeyID:      testFingerprint,
		Algorithm:  AlgorithmGPG,
		Signature:  base64.StdEncoding.EncodeToString([]byte("a detached signature")),
	}
	good := validsigLine(testFingerprint, testFingerprint)

	cases := []struct {
		name   string
		status string
		stderr string
		exit   int
		want   string // substring the verdict must carry
	}{
		{
			name:   "revoked key, gpg exits non-zero",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] KEYREVOKED\n[GNUPG:] REVKEYSIG ABCD signer\n" + good,
			stderr: "gpg: Note: This key has been revoked!\n",
			exit:   2,
			want:   "REVOKED",
		},
		{
			// The same status stream, but gpg exits 0. Which of the two a
			// given gpg build does is not something the verdict may depend
			// on: the status line is the statement, the exit code is a
			// second, independent chance to notice.
			name:   "revoked key, gpg exits zero",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] KEYREVOKED\n[GNUPG:] REVKEYSIG ABCD signer\n" + good,
			stderr: "gpg: Note: This key has been revoked!\n",
			exit:   0,
			want:   "REVOKED",
		},
		{
			name:   "expired key",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] EXPKEYSIG ABCD signer\n" + good,
			stderr: "gpg: Note: This key has expired!\n",
			exit:   0,
			want:   "EXPIRED",
		},
		{
			name:   "expired signature",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] EXPSIG ABCD signer\n" + good,
			stderr: "gpg: Note: This signature has expired.\n",
			exit:   0,
			want:   "expired",
		},
		{
			// A genuine BADSIG must read as "the bytes do not match", not as
			// "you do not have the key": an operator whose media WAS tampered
			// with must not be sent off to debug their keyring.
			name:   "bad signature",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] BADSIG ABCD signer\n",
			stderr: "gpg: BAD signature from \"signer\"\n",
			exit:   1,
			want:   "does not verify",
		},
		{
			// gpg could not complete the check for a reason that is not a
			// missing key (rc 4 here, not rc 9).
			name:   "errsig that is not a missing key",
			status: "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG ABCD 22 8 00 2026-01-01 4 " + testFingerprint + "\n",
			stderr: "gpg: Can't check signature: Unknown digest algorithm\n",
			exit:   2,
			want:   "rejected by gpg",
		},
		{
			// VALIDSIG and nothing debark recognises as a refusal, but gpg
			// still exited non-zero. The exit status is the one part of gpg's
			// answer that is not version-specific, so it wins.
			name:   "valid signature but gpg exited non-zero anyway",
			status: good,
			stderr: "gpg: some future refusal debark has no keyword for\n",
			exit:   2,
			want:   "exited non-zero",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: tc.status, Stderr: tc.stderr, Exit: tc.exit}})
			v := &gpgVerifier{}
			err := v.verify(context.Background(), manifest.SignPurpose, canon, block)
			if err == nil {
				t.Fatalf("verify ACCEPTED a signature gpg disqualified (%s)", tc.name)
			}
			if errors.Is(err, ErrUntrustedKey) {
				t.Errorf("reported as an untrusted key rather than a bad signature: %v", err)
			}
			if dferr.ClassOf(err) != dferr.Verification {
				t.Errorf("class = %v, want Verification (err: %v)", dferr.ClassOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err.Error(), tc.want)
			}
		})
	}
}

// TestGPGVerifier_EnvironmentalFailureIsNotABundleVerdict: gpg failing
// without ever speaking the status protocol says nothing about the bundle. It
// used to come back as ErrUntrustedKey, which reads as a statement about the
// operator's trust set - and fails closed, but sends them to the wrong place.
func TestGPGVerifier_EnvironmentalFailureIsNotABundleVerdict(t *testing.T) {
	useGPGStub(t, gpgStubScript{Verify: gpgStubOp{
		Stderr: "gpg: fatal: out of core\n", Exit: 2,
	}})
	v := &gpgVerifier{}
	err := v.verify(context.Background(), manifest.SignPurpose, []byte("canon"), manifest.Signature{
		SignerKind: manifest.SignerGPG, KeyID: testFingerprint,
		Signature: base64.StdEncoding.EncodeToString([]byte("sig")),
	})
	if err == nil {
		t.Fatal("verify accepted a signature gpg never checked")
	}
	if dferr.ClassOf(err) != dferr.Environment {
		t.Errorf("class = %v, want Environment (err: %v)", dferr.ClassOf(err), err)
	}
	if errors.Is(err, ErrUntrustedKey) {
		t.Error("a broken gpg was reported as an untrusted key")
	}
}

// TestGPGVerifier_SubkeyFingerprint is the deterministic half of the
// subkey-layout fix. gpg reports TWO fingerprints on a VALIDSIG line: the key
// that made the signature (a [S] subkey on any hardened setup) and the
// primary key it belongs to. The signature block records the PRIMARY, because
// that is the fingerprint an operator publishes and compares by hand and the
// one that survives a subkey rotation - so comparing only against VALIDSIG's
// first field turned an untampered bundle into ProblemSignatureInvalid,
// debark's loudest tamper verdict, on exactly the layouts a gpg-mandating
// shop runs.
func TestGPGVerifier_SubkeyFingerprint(t *testing.T) {
	canon := []byte(`{"manifest":"bytes"}`)
	sigBytes := base64.StdEncoding.EncodeToString([]byte("a detached signature"))
	// gpg validated the signature against the SUBKEY, which belongs to the
	// primary key testFingerprint.
	status := "[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG ABCD signer\n" + validsigLine(testSubkeyFingerprint, testFingerprint)

	cases := []struct {
		name    string
		claimed string
		wantErr bool
	}{
		{
			// The honest round trip that used to fail.
			name:    "block claims the primary fingerprint",
			claimed: testFingerprint,
		},
		{
			// A block that names the subkey directly is equally honest: gpg
			// validated the signature against that very key.
			name:    "block claims the signing subkey fingerprint",
			claimed: testSubkeyFingerprint,
		},
		{
			// The cross-check must still bite. Widening it to accept the
			// primary must not widen it to accept anything else.
			name:    "block claims an unrelated key",
			claimed: "1111222233334444555566667777888899990000",
			wantErr: true,
		},
		{
			// An empty key_id must never be laundered into a match by an
			// empty primary field.
			name:    "block claims no key at all",
			claimed: "",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: status}})
			v := &gpgVerifier{}
			err := v.verify(context.Background(), manifest.SignPurpose, canon, manifest.Signature{
				SignerKind: manifest.SignerGPG, KeyID: tc.claimed, Signature: sigBytes,
			})
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("verify accepted a block claiming %q against a signature by %q", tc.claimed, testSubkeyFingerprint)
			case tc.wantErr:
				if dferr.ClassOf(err) != dferr.Verification {
					t.Errorf("class = %v, want Verification (err: %v)", dferr.ClassOf(err), err)
				}
			case err != nil:
				t.Fatalf("verify rejected an honest subkey signature: %v", err)
			}
		})
	}

	// And with no primary fingerprint printed at all (a gpg too old to emit
	// one), the check falls back to the signing key alone - stricter, never
	// looser.
	t.Run("no primary fingerprint printed", func(t *testing.T) {
		useGPGStub(t, gpgStubScript{Verify: gpgStubOp{
			Stdout: "[GNUPG:] VALIDSIG " + testSubkeyFingerprint + "\n",
		}})
		v := &gpgVerifier{}
		err := v.verify(context.Background(), manifest.SignPurpose, canon, manifest.Signature{
			SignerKind: manifest.SignerGPG, KeyID: testFingerprint, Signature: sigBytes,
		})
		if err == nil {
			t.Fatal("verify accepted a primary fingerprint gpg never reported")
		}
	})
}

// TestGPGVerifier_NoKeyserverLookup: zero telemetry is a project
// non-negotiable, and the air-gap claim must not rest on how the target's
// gpg.conf happens to be written. A target with auto-key-retrieve or a
// keyserver configured would otherwise let `debark verify` reach the
// network - from an air-gapped machine, on attacker-chosen input, since the
// key id gpg would look up comes out of the bundle.
func TestGPGVerifier_NoKeyserverLookup(t *testing.T) {
	argsFile := useGPGStub(t, gpgStubScript{Verify: gpgStubOp{Stdout: ""}})
	v := &gpgVerifier{}
	_ = v.verify(context.Background(), manifest.SignPurpose, []byte("x"), manifest.Signature{
		SignerKind: manifest.SignerGPG, KeyID: testFingerprint,
		Signature: base64.StdEncoding.EncodeToString([]byte("sig")),
	})
	inv := gpgStubInvocations(t, argsFile)
	if len(inv) == 0 {
		t.Fatal("gpg was never invoked")
	}
	last := inv[len(inv)-1]
	for _, want := range []string{"--no-auto-key-retrieve", "--keyserver-options", "no-auto-key-retrieve"} {
		if !argvHas(last, want) {
			t.Errorf("verification did not pass %q: %v", want, last)
		}
	}
}

// TestNewGPGVerifier_RejectsUnusableKeyring: a --gpg-keyring path that is not
// there is a typo in a flag. Left to gpg it becomes an ERRSIG/NO_PUBKEY
// stream, which debark reports as "signed by an untrusted key" - sending
// the operator to debug their trust set when the trust set was never opened.
func TestNewGPGVerifier_RejectsUnusableKeyring(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		_, err := newGPGVerifier(filepath.Join(dir, "no-such-keyring.gpg"))
		if err == nil {
			t.Fatal("newGPGVerifier accepted a keyring that does not exist")
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
		}
		if !strings.Contains(dferr.HintOf(err), "--gpg-keyring") {
			t.Errorf("hint does not name the flag: %q", dferr.HintOf(err))
		}
	})

	t.Run("a directory, which is what --keyring takes", func(t *testing.T) {
		_, err := newGPGVerifier(dir)
		if err == nil {
			t.Fatal("newGPGVerifier accepted a directory as a gpg keyring file")
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("class = %v, want Usage", dferr.ClassOf(err))
		}
	})

	t.Run("an existing file is accepted", func(t *testing.T) {
		p := filepath.Join(dir, "release.gpg")
		if err := os.WriteFile(p, []byte("keyring bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		v, err := newGPGVerifier(p)
		if err != nil {
			t.Fatalf("newGPGVerifier rejected a real file: %v", err)
		}
		if !argvHas(v.keyringArgs, "--no-default-keyring") || !argvHas(v.keyringArgs, p) {
			t.Errorf("keyring args = %v", v.keyringArgs)
		}
	})
}

// TestSignerFor_NilSignerOnError: SignerFor returns a Signer INTERFACE, and
// every constructor behind it returns a concrete *T. Returning a nil *T
// through that interface produces a value for which `signer != nil` is true -
// a nil-pointer dereference waiting for the first caller who checks the
// signer instead of the error. core/engine already has exactly that shape
// (`b.signerOwned && b.signer != nil`), and this is a frozen public API, so
// the guarantee is pinned here rather than left to every caller.
func TestSignerFor_NilSignerOnError(t *testing.T) {
	// Each of the four branches, all failing.
	refs := []string{
		"",                       // empty
		"gpg:",                   // gpg, rejected before gpg is ever run
		"plugin:",                // plugin, no path
		"no-such-key-file.key",   // ed25519 file that does not exist
		"gpg:nobody@example.inv", // gpg, failing in the key lookup
	}
	useGPGStub(t, gpgStubScript{List: gpgStubOp{Stdout: ""}})
	for _, ref := range refs {
		s, err := SignerFor(context.Background(), ref)
		if err == nil {
			_ = s.Close()
			t.Errorf("SignerFor(%q) unexpectedly succeeded", ref)
			continue
		}
		if s != nil {
			t.Errorf("SignerFor(%q) returned err %v AND a non-nil Signer interface (%T); callers checking `signer != nil` would dereference it", ref, err, s)
		}
	}
}

// --- one real-gpg test, fenced ---------------------------------------------

// TestGPG_SignVerify_SigningSubkey_RealGPG is the only test in this file that
// runs the real gpg, and it earns the exception: the whole point is the
// primary/subkey split in gpg's OWN output, and a stub asserting the shape
// debark expects cannot show that a real gpg still prints it. Everything
// else about the gpg signer and verifier stays deterministic above.
//
// The existing real-gpg round trip (sign_test.go) generates a plain RSA key
// with NO signing subkey, so signing key and primary fingerprint are the same
// string and the mismatch this covers is invisible to it.
//
// Fenced so it can never reach an operator's keys or open a prompt:
//
//   - GNUPGHOME is a throwaway directory, set before the first gpg call, so
//     the default keyring is never opened, read or written;
//   - gpg.conf in that home carries no-tty and pinentry-mode loopback, and
//     gpg-agent.conf carries allow-loopback-pinentry, so NO invocation in
//     this home can open a pinentry dialog - including the ones core/sign
//     makes on this test's behalf, which take no flags from here. --batch
//     alone does NOT prevent one;
//   - the key is generated %no-protection, so there is no passphrase to
//     prompt for in the first place;
//   - anything gpg started in that home is killed on the way out.
func TestGPG_SignVerify_SigningSubkey_RealGPG(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found on PATH; skipping the real-gpg subkey round trip")
	}
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	if err := os.WriteFile(filepath.Join(home, "gpg.conf"), []byte("no-tty\npinentry-mode loopback\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gpg-agent.conf"), []byte("allow-loopback-pinentry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Registered after t.Setenv so it runs BEFORE GNUPGHOME is restored.
		if _, err := exec.LookPath("gpgconf"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "gpgconf", "--homedir", home, "--kill", "all").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const email = "debark-subkey-test@example.invalid"
	// A primary key plus a SEPARATE signing subkey - the hardened layout an
	// offline primary or a smartcard produces, and the one gpg picks the
	// subkey for when asked to sign. Generated in one batch run: no
	// --quick-add-key, which cannot be made reliably non-interactive.
	batch := "%no-protection\n" +
		"Key-Type: RSA\nKey-Length: 2048\n" +
		"Subkey-Type: RSA\nSubkey-Length: 2048\nSubkey-Usage: sign\n" +
		"Name-Real: debark subkey test\nName-Email: " + email + "\n" +
		"Expire-Date: 0\n%commit\n"
	gen := exec.CommandContext(ctx, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--gen-key")
	gen.Stdin = strings.NewReader(batch)
	var genErr bytes.Buffer
	gen.Stderr = &genErr
	if err := gen.Run(); err != nil {
		t.Skipf("gpg --batch --gen-key did not succeed in this environment (skipping): %v: %s", err, genErr.String())
	}

	signer, err := SignerFor(context.Background(), "gpg:"+email)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer signer.Close()

	canon := []byte(`{"hello":"gpg subkey"}`)
	sig, err := signer.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Ask gpg directly what it thinks, so the scenario is OBSERVED rather
	// than assumed: this is the only way to know a real gpg really did sign
	// with the subkey and really does print the primary fingerprint last.
	work := t.TempDir()
	sigPath := filepath.Join(work, "sig.bin")
	dataPath := filepath.Join(work, "data.bin")
	raw, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	if err := os.WriteFile(sigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataPath, SigningInput(manifest.SignPurpose, canon), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(ctx, "gpg", "--batch", "--status-fd", "1", "--verify", sigPath, dataPath)
	var probeOut, probeErr bytes.Buffer
	probe.Stdout, probe.Stderr = &probeOut, &probeErr
	probeRunErr := probe.Run()
	t.Logf("gpg --verify status:\n%s", probeOut.String())
	if probeRunErr != nil {
		t.Logf("gpg --verify exit: %v (stderr: %s)", probeRunErr, probeErr.String())
	}
	st := parseVerifyStatus(probeOut.Bytes())
	if !st.valid {
		t.Fatalf("real gpg did not report VALIDSIG for a signature it just made: %s", probeOut.String())
	}
	if st.primaryKey == "" {
		t.Fatalf("real gpg printed no primary-key fingerprint on VALIDSIG; the parser's field position is wrong: %s", probeOut.String())
	}
	if st.signingKey == st.primaryKey {
		t.Skipf("this gpg signed with the primary key, not the subkey, so the subkey layout is not exercised (signing=%s primary=%s)", st.signingKey, st.primaryKey)
	}
	if !strings.EqualFold(st.primaryKey, signer.KeyID()) {
		t.Fatalf("KeyID() = %s but gpg's primary-key fingerprint is %s", signer.KeyID(), st.primaryKey)
	}

	// The bug, exactly: the block records the primary, gpg validated against
	// the subkey. Before the fix this came back as
	//   "gpg signature claims key <primary> but gpg validated it against <subkey>"
	// which core/verify classifies as ProblemSignatureInvalid on an
	// untampered bundle.
	verifier, err := VerifierFor(context.Background(), KeySource{}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	if err := verifier.Verify(context.Background(), manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("an untampered bundle signed with a signing subkey did not verify: %v", err)
	}

	// Controls: the domain separation and the tamper check still bite on this
	// key layout, so the widened fingerprint comparison did not turn the
	// verifier into a yes-man.
	if err := verifier.Verify(context.Background(), "some-other-purpose", canon, sig); err == nil {
		t.Error("a gpg subkey signature verified under a different purpose")
	}
	if err := verifier.Verify(context.Background(), manifest.SignPurpose, []byte(`{"hello":"TAMPERED"}`), sig); err == nil {
		t.Error("a gpg subkey signature verified over a payload it was not made for")
	}
	wrong := sig
	wrong.KeyID = "1111222233334444555566667777888899990000"
	if err := verifier.Verify(context.Background(), manifest.SignPurpose, canon, wrong); err == nil {
		t.Error("a block claiming an unrelated key was accepted")
	}
}

// TEMPORARY end-to-end probe against a real gpg. Removed before completion.
func TestZZProbeRealGPGEndToEnd(t *testing.T) {
	email := os.Getenv("DFPROBE_EMAIL")
	if email == "" {
		t.Skip("no DFPROBE_EMAIL")
	}
	signer, err := SignerFor(context.Background(), "gpg:"+email)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer signer.Close()
	t.Logf("KeyID (primary) = %s", signer.KeyID())

	canon := []byte(`{"hello":"real gpg"}`)
	sig, err := signer.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if vh := os.Getenv("DFPROBE_VERIFY_HOME"); vh != "" {
		os.Setenv("GNUPGHOME", vh)
		t.Logf("verifying in GNUPGHOME=%s", vh)
	}
	v, err := VerifierFor(context.Background(), KeySource{}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	err = v.Verify(context.Background(), manifest.SignPurpose, canon, sig)
	t.Logf("RESULT verify(honest) = %v", err)
	err = v.Verify(context.Background(), manifest.SignPurpose, []byte(`{"hello":"TAMPERED"}`), sig)
	t.Logf("RESULT verify(tampered) = %v (untrusted=%v class=%v)", err, errors.Is(err, ErrUntrustedKey), dferr.ClassOf(err))
	err = v.Verify(context.Background(), "other-purpose", canon, sig)
	t.Logf("RESULT verify(wrong purpose) = %v", err)
}
