package sign

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// AlgorithmGPG is the algorithm string recorded when gpg made the signature.
// gpg picks its own key algorithm (RSA, ed25519, ...); debark does not
// second-guess it, so the field records the signer mechanism, not the key
// type.
const AlgorithmGPG = "gpg"

// gpgBinary is the executable name; a variable so tests can point it at a
// stub, and so a future --gpg-binary flag has somewhere to land.
var gpgBinary = "gpg"

// gpgSigner shells out to gpg --detach-sign. It signs
// sign.SigningInput(purpose, canonical) - the same domain-separated payload
// every other Signer kind signs - and records the raw (non-armoured) detached
// signature bytes, base64-encoded, in the JSON signature block. --armor is
// deliberately not used: the JSON envelope already carries the bytes as
// base64, so ASCII-armouring them first would just be wasted encoding.
type gpgSigner struct {
	keyRef      string // whatever the operator passed after "gpg:"
	fingerprint string // resolved uppercase 40-hex fingerprint; this is KeyID()
}

func newGPGSigner(ctx context.Context, keyRef string) (*gpgSigner, error) {
	if keyRef == "" {
		return nil, dferr.New(dferr.Usage, "sign: gpg: empty key reference; use gpg:<keyid>")
	}
	if _, err := exec.LookPath(gpgBinary); err != nil {
		return nil, wrapErr(dferr.Environment, err, "sign: gpg binary not found on PATH").
			WithHint("install gnupg, or sign with an ed25519 key file or a plugin instead")
	}
	fp, err := gpgSecretKeyFingerprint(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	return &gpgSigner{keyRef: keyRef, fingerprint: fp}, nil
}

func (s *gpgSigner) Kind() string  { return manifest.SignerGPG }
func (s *gpgSigner) KeyID() string { return s.fingerprint }

func (s *gpgSigner) Sign(ctx context.Context, purpose string, canon []byte) (manifest.Signature, error) {
	msg := SigningInput(purpose, canon)
	args := []string{"--batch", "--yes", "--local-user", s.keyRef, "--detach-sign", "--output", "-", "-"}
	out, errOut, err := runGPG(ctx, msg, args...)
	if err != nil {
		return manifest.Signature{}, classifyGPGError(err, errOut, "sign")
	}
	return manifest.Signature{
		SignerKind: manifest.SignerGPG,
		KeyID:      s.fingerprint,
		Algorithm:  AlgorithmGPG,
		CreatedAt:  nowStamp(),
		Signature:  base64.StdEncoding.EncodeToString(out),
	}, nil
}

func (s *gpgSigner) Close() error { return nil }

// runGPG runs gpg with args, feeding it stdin, and captures stdout/stderr
// separately. It never touches the terminal (ADR-... core packages never read
// a terminal or print): --batch plus an explicit --local-user/--keyring keep
// gpg from trying to prompt on anything but the pinentry it needs for a
// secret-key operation, which is between the operator and gpg-agent.
func runGPG(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	// gosec's G702 traces args back to caller-influenced data (a key ref, a
	// keyring path) and calls it command injection. It is not, and the reason
	// is structural rather than a promise about the callers: this builds an
	// argument VECTOR -- gpgBinary is a compile-time constant program name and
	// args is a []string handed to exec, one element per argv slot. No shell
	// parses any of it, so there is no metacharacter, quoting or word-splitting
	// step for a hostile value to escape through. The worst a bad element can
	// do is be a bad gpg argument, which gpg rejects.
	//
	// This is the same reasoning .golangci.yml records for excluding G204
	// repo-wide (ADR-001: debark orchestrates gpg/apt/dpkg as processes and
	// never re-implements them); G702 is the taint-analysis restatement of it.
	// G702 is NOT excluded globally, because unlike G204 it fires selectively
	// and a future `sh -c` really would deserve to be caught -- which is why
	// this is a per-site waiver and not another entry in the excludes list.
	// #nosec G702 -- argv vector, constant program, no shell; see above
	cmd := exec.CommandContext(ctx, gpgBinary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// gpgSecretKeyFingerprint resolves a key reference to the PRIMARY key's
// fingerprint, which is what the signature block records as key_id.
//
// The primary is the right identity even though gpg may well make the
// signature with a [S] subkey: it is the fingerprint an operator publishes
// and a target compares by hand (ADR-008 layer 2), and it survives a subkey
// rotation, which is a routine operation that must not change who the release
// key is. Recording whichever subkey gpg happened to pick would also be
// guesswork here - gpg chooses at signing time, not at lookup time. The
// verifier closes the gap from the other end by accepting either fingerprint
// gpg reports on its VALIDSIG line.
func gpgSecretKeyFingerprint(ctx context.Context, keyRef string) (string, error) {
	args := []string{"--batch", "--with-colons", "--fingerprint", "--list-secret-keys", keyRef}
	out, errOut, err := runGPG(ctx, nil, args...)
	if err != nil {
		return "", classifyGPGError(err, errOut, "look up secret key "+keyRef)
	}
	fp := parseColonFingerprint(out)
	if fp == "" {
		return "", dferr.New(dferr.Environment, "sign: gpg: no secret key found for %q", keyRef).
			WithHint("import or generate the key; check with `gpg --list-secret-keys`")
	}
	return fp, nil
}

// parseColonFingerprint extracts the primary key's fingerprint from
// `gpg --with-colons --fingerprint` output: the "fpr" record that immediately
// follows a "sec" or "pub" record (as opposed to one following "ssb"/"sub",
// which belongs to a subkey).
func parseColonFingerprint(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	primary := false
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "sec", "pub":
			primary = true
		case "ssb", "sub":
			primary = false
		case "fpr":
			if primary && len(fields) > 9 && fields[9] != "" {
				return strings.ToUpper(fields[9])
			}
		}
	}
	return ""
}

// classifyGPGError turns a failed gpg invocation into a dferr with a hint,
// based on best-effort matching over gpg's (version- and locale-dependent)
// stderr text. The raw first stderr line always rides along in the message so
// a mismatch is still debuggable.
func classifyGPGError(err error, stderr []byte, action string) error {
	text := strings.ToLower(string(stderr))
	detail := firstMeaningfulLine(stderr, err)
	base := dferr.New(dferr.Environment, "sign: gpg: %s failed: %s", action, detail)
	switch {
	case strings.Contains(text, "no secret key") || strings.Contains(text, "secret key not available") || strings.Contains(text, "no default secret key"):
		return base.WithHint("import or generate the signing key; check with `gpg --list-secret-keys`")
	case strings.Contains(text, "no such file or directory") && strings.Contains(text, "agent"):
		return base.WithHint("gpg-agent is not running; start it with `gpgconf --launch gpg-agent`")
	case strings.Contains(text, "gpg-agent"):
		return base.WithHint("check gpg-agent is running and reachable: `gpgconf --launch gpg-agent`")
	case strings.Contains(text, "cancel"):
		return dferr.New(dferr.Usage, "sign: gpg: %s: operation cancelled at the pinentry prompt", action)
	case strings.Contains(text, "no public key"):
		return base.WithHint("import the signer's public key into this keyring")
	default:
		return base
	}
}

func firstMeaningfulLine(stderr []byte, err error) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

// gpgVerifier checks gpg signature blocks against an operator-designated
// keyring (KeySource.GPGKeyring, from --gpg-keyring) or, when that is empty,
// the operator's default keyring - "out of band" either way, since
// VerifierFor has already refused a GPGKeyring path that resolves inside the
// bundle being verified.
//
// The empty case is deliberately a warning (core/verify reports it) rather
// than a refusal. An operator who imported the release key into their own
// keyring out of band has satisfied ADR-008 layer 2 exactly as well as one
// who exported it to a file, and refusing them would break verification on an
// air-gapped target where re-exporting a keyring may not be easy. What the
// ambient keyring can do is widen WHICH keys are acceptable; it cannot make
// debark accept a signature by a key other than the one the block names,
// because the fingerprint cross-check below pins that.
type gpgVerifier struct {
	keyringArgs []string
}

func newGPGVerifier(gpgKeyring string) (*gpgVerifier, error) {
	if gpgKeyring == "" {
		return &gpgVerifier{}, nil
	}
	// A keyring path that is not there is a typo in a flag, not evidence
	// about the bundle. Caught here it is one Usage error naming the path;
	// left to gpg it becomes an ERRSIG/NO_PUBKEY stream that reads as
	// "signed by an untrusted key" and sends the operator off to debug their
	// trust set instead of their command line.
	st, err := os.Stat(gpgKeyring)
	if err != nil {
		return nil, wrapErr(dferr.Usage, err, "sign: gpg keyring %q cannot be read", gpgKeyring).
			WithHint("--gpg-keyring takes a keyring FILE holding the expected release key(s), e.g. one made with `gpg --export --output release.gpg <keyid>`")
	}
	if st.IsDir() {
		return nil, dferr.New(dferr.Usage, "sign: gpg keyring %q is a directory, not a keyring file", gpgKeyring).
			WithHint("--gpg-keyring takes a gpg keyring file; --keyring takes a directory of debark .pub files")
	}
	return &gpgVerifier{keyringArgs: []string{"--no-default-keyring", "--keyring", gpgKeyring}}, nil
}

func (v *gpgVerifier) verify(ctx context.Context, purpose string, canon []byte, sig manifest.Signature) error {
	if _, err := exec.LookPath(gpgBinary); err != nil {
		return wrapErr(dferr.Environment, err, "sign: gpg binary not found on PATH").
			WithHint("install gnupg to verify a gpg-signed bundle")
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return dferr.New(dferr.Verification, "sign: gpg signature %s: malformed base64: %v", sig.KeyID, err)
	}

	dir, err := os.MkdirTemp("", "debark-gpgverify-*")
	if err != nil {
		return wrapErr(dferr.Environment, err, "sign: gpg: create temp dir")
	}
	// Best-effort. The directory holds only the detached signature and the
	// signing input -- both derived from data the caller already has, neither
	// secret -- so a failed removal leaks nothing and cannot change the verify
	// verdict this function returns. Discarded explicitly so the reader knows
	// it was considered.
	defer func() { _ = os.RemoveAll(dir) }()

	sigPath := filepath.Join(dir, "sig.bin")
	dataPath := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(sigPath, rawSig, 0o600); err != nil {
		return wrapErr(dferr.Environment, err, "sign: gpg: write temp signature")
	}
	// dataPath is filepath.Join(dir, "data.bin") with dir from os.MkdirTemp
	// eight lines up and a constant base name: neither half is caller-supplied,
	// so there is no traversal to perform. G703 flags it because the *content*
	// (canon, the untrusted bundle bytes) is tainted, and it does not
	// distinguish tainting the bytes from tainting the destination.
	// #nosec G703 -- destination is MkdirTemp + a constant name; only the payload is untrusted
	if err := os.WriteFile(dataPath, SigningInput(purpose, canon), 0o600); err != nil {
		return wrapErr(dferr.Environment, err, "sign: gpg: write temp payload")
	}

	// --no-auto-key-retrieve, in both spellings, is belt and braces against a
	// target whose gpg.conf sets auto-key-retrieve or a keyserver: zero
	// telemetry is a project non-negotiable, and the air-gap claim must not
	// depend on how the target's gpg happens to be configured. Which spelling
	// wins depends on where the target set the option, so both are passed.
	//
	// --trust-model always stays, deliberately. debark's trust decision is
	// not gpg's web of trust: it is "the key is in the keyring the operator
	// designated, and its fingerprint is the one the signature block names".
	// The ownertrust database the default model consults lives in the
	// VERIFYING machine's GNUPGHOME - exactly the ambient state ADR-008 keeps
	// out of the decision - so honouring it would make the same bundle and
	// the same keyring verify differently on two targets while establishing
	// nothing debark does not establish itself. Nor does it weaken the
	// checks below: revocation and expiry are key state, not ownertrust, and
	// gpg reports them (REVKEYSIG/EXPKEYSIG) whatever the trust model says.
	args := append([]string{
		"--batch", "--status-fd", "1", "--trust-model", "always",
		"--no-auto-key-retrieve", "--keyserver-options", "no-auto-key-retrieve",
	}, v.keyringArgs...)
	args = append(args, "--verify", sigPath, dataPath)
	out, errOut, runErr := runGPG(ctx, nil, args...)

	st := parseVerifyStatus(out)
	detail := firstMeaningfulLine(errOut, runErr)
	switch {
	case st.rejected != "":
		// Checked BEFORE st.valid, because gpg emits REVKEYSIG, EXPKEYSIG and
		// EXPSIG *alongside* VALIDSIG rather than instead of it. Looking only
		// for VALIDSIG made a signature by a revoked key verify as fully
		// valid, which left revocation - the only remedy an organisation has
		// once a release key is stolen - entirely inert.
		return gpgRejection(st.rejected, sig.KeyID, detail)
	case st.noPublicKey && !st.valid:
		// gpg has no key for this signature, so it could not even attempt the
		// check. That is specifically the untrusted-key answer, and it must
		// stay distinguishable from "the bytes do not match": the operator's
		// remedy is to obtain the key, not to distrust the bundle.
		return ErrUntrustedKey
	case !st.valid && !st.sawStatus && runErr != nil:
		// gpg failed without saying anything on the status protocol at all:
		// it crashed, or refused the invocation outright. Nothing was learned
		// about the bundle, so this must not be reported as a fact about one.
		return wrapErr(dferr.Environment, runErr, "sign: gpg: could not check signature %s: %s", sig.KeyID, detail).
			WithHint("run the same `gpg --verify` by hand to see what gpg is unhappy about")
	case !st.valid:
		// Status lines, but no verdict debark recognises. Fail closed.
		return ErrUntrustedKey
	case runErr != nil:
		// VALIDSIG, but gpg still exited non-zero: it refused for a reason
		// this parser has no keyword for - a status line added by a newer
		// gpg, a policy this build enforces - and the exit status is the one
		// part of its answer that is not version-specific. Believing the line
		// we recognise over the verdict we do not is exactly the mistake
		// REVKEYSIG was.
		return dferr.New(dferr.Verification,
			"sign: gpg signature %s: gpg reported it valid but exited non-zero: %s", sig.KeyID, detail).
			WithHint("gpg refused this signature for a reason debark does not recognise; run the same `gpg --verify` by hand")
	}

	// Fingerprint cross-check. gpg answers "this signature is valid, made by
	// KEY"; the block claims a key of its own, and the two must agree, or a
	// bundle could claim the release key while carrying a valid signature
	// from some other key the keyring happens to hold.
	//
	// Both fingerprints gpg reports are accepted. On the hardened layouts a
	// shop mandating gpg is most likely to run - an offline primary with an
	// [S] subkey, or a smartcard - the signature is made by the SUBKEY, so
	// VALIDSIG's first field is the subkey's fingerprint while KeyID()
	// records the primary's. gpg derives that primary field from its own
	// keyring rather than from the signature, so the two name one key by
	// gpg's own reckoning; and the primary is the fingerprint the operator
	// publishes, compares by hand, and keeps across a subkey rotation.
	// Demanding the first field alone turned an honest round trip on those
	// layouts into debark's loudest tamper verdict.
	if !fingerprintMatches(sig.KeyID, st.signingKey, st.primaryKey) {
		return dferr.New(dferr.Verification, "sign: gpg signature claims key %s but gpg validated it against %s", sig.KeyID, st.signingKey)
	}
	return nil
}

// fingerprintMatches reports whether the fingerprint a signature block claims
// is one of the two gpg reported for the key it validated against: the
// signing key itself, or the primary key that signing key belongs to. An
// empty claim never matches - a block with no key_id must not be laundered
// into an accept by a gpg too old to have printed the primary fingerprint.
func fingerprintMatches(claimed, signingKey, primaryKey string) bool {
	if claimed == "" {
		return false
	}
	if signingKey != "" && strings.EqualFold(claimed, signingKey) {
		return true
	}
	return primaryKey != "" && strings.EqualFold(claimed, primaryKey)
}

// gpgRejection turns a disqualifying status keyword into the verdict and the
// operator's next action. All of these are dferr.Verification: gpg checked
// the signature and said it must not be relied on, which is a fact about the
// bundle rather than about this machine.
func gpgRejection(keyword, keyID, detail string) error {
	switch keyword {
	case "REVKEYSIG":
		return dferr.New(dferr.Verification, "sign: gpg signature %s: made by a REVOKED key: %s", keyID, detail).
			WithHint("the signing key has been revoked; do not install this bundle - obtain one signed by the current release key")
	case "EXPKEYSIG":
		return dferr.New(dferr.Verification, "sign: gpg signature %s: made by an EXPIRED key: %s", keyID, detail).
			WithHint("re-sign the bundle with a current key, or extend the key's expiry and re-import the updated public key on this target")
	case "EXPSIG":
		return dferr.New(dferr.Verification, "sign: gpg signature %s: the signature itself has expired: %s", keyID, detail).
			WithHint("re-sign the bundle")
	case "BADSIG":
		// A genuine BADSIG: gpg has the key, and the bytes do not match it.
		// This is the tamper verdict, and it must never be reported as an
		// untrusted key - that would send an operator whose media WAS
		// tampered with off to debug their keyring.
		return dferr.New(dferr.Verification, "sign: gpg signature %s: does not verify against this manifest: %s", keyID, detail).
			WithHint("the bundle does not match the signature; treat the media as untrusted")
	default:
		return dferr.New(dferr.Verification, "sign: gpg signature %s: rejected by gpg (%s): %s", keyID, keyword, detail)
	}
}

// gpgVerifyStatus is the distilled outcome of one `gpg --status-fd 1
// --verify`. Neither half of gpg's answer is sufficient alone: several status
// lines that disqualify a signature are printed ALONGSIDE VALIDSIG rather
// than instead of it, and gpg's exit status carries refusals that have no
// status keyword debark knows. So both are read, and both have to agree.
type gpgVerifyStatus struct {
	// signingKey is VALIDSIG's first field: the fingerprint of the key that
	// actually made the signature, which is a SUBKEY whenever the operator
	// signs with one.
	signingKey string
	// primaryKey is VALIDSIG's last documented field: the fingerprint of the
	// primary key signingKey belongs to (the same value when the primary
	// signed directly). Empty when gpg did not print it.
	primaryKey string
	// valid records that gpg emitted VALIDSIG - the cryptographic check over
	// exactly these bytes passed.
	valid bool
	// rejected is the status keyword that disqualifies the signature, empty
	// when none was seen.
	rejected string
	// noPublicKey records that gpg said it has no key for this signature
	// (NO_PUBKEY, or an ERRSIG it blamed on a missing key). That is the one
	// outcome that is about the verifier's trust set rather than the bundle.
	noPublicKey bool
	// sawStatus records that gpg spoke the status protocol at all, which
	// separates "gpg answered, and the answer was not one we accept" from
	// "gpg never really ran".
	sawStatus bool
}

// parseVerifyStatus scans `gpg --status-fd 1 --verify` output for every line
// that bears on the verdict. VALIDSIG is emitted only when the cryptographic
// check passed, and carries the fingerprints; BADSIG means it did not; and
// REVKEYSIG, EXPKEYSIG and EXPSIG mean gpg checked the maths, found it good,
// and is telling the caller the key or the signature is nonetheless not to be
// relied on. gpg's human-readable chatter on stderr is advisory; only this
// stream is authoritative.
func parseVerifyStatus(out []byte) gpgVerifyStatus {
	var st gpgVerifyStatus
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "[GNUPG:] ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "[GNUPG:] "))
		if len(fields) == 0 {
			continue
		}
		st.sawStatus = true
		switch fields[0] {
		case "VALIDSIG":
			// VALIDSIG <fpr> <date> <ts> <expire-ts> <version> <reserved>
			//          <pubkey-algo> <hash-algo> <sig-class> <primary-fpr>
			if len(fields) > 1 {
				st.signingKey = strings.ToUpper(fields[1])
				st.valid = true
			}
			st.primaryKey = validsigPrimaryKey(fields)
		case "BADSIG", "REVKEYSIG", "EXPKEYSIG", "EXPSIG":
			// Any one of these is fatal, so which one is recorded last does
			// not matter; a stream mixing outcomes can never be laundered
			// into an accept, because rejected is checked before valid.
			st.rejected = fields[0]
		case "ERRSIG":
			// ERRSIG <keyid> <pkalgo> <hashalgo> <sig-class> <time> <rc> [<fpr>]
			// rc 9 is GPG_ERR_NO_PUBKEY: gpg has no key, which is a statement
			// about the trust set. Any other rc means gpg tried and could not
			// complete the check, which is a failure of the signature.
			if len(fields) > 6 && fields[6] == "9" {
				st.noPublicKey = true
			} else {
				st.rejected = "ERRSIG"
			}
		case "NO_PUBKEY":
			st.noPublicKey = true
		}
	}
	return st
}

// validsigPrimaryKey extracts the primary-key fingerprint from a VALIDSIG
// line's fields, or "" when there is none to be had.
//
// GnuPG documents it as VALIDSIG's tenth and final argument, so that is where
// it is looked for first; the last field is taken as a fallback, so a gpg
// printing one argument fewer (or more) still yields it instead of silently
// reverting to the signing-key-only comparison this exists to fix. The index
// floor keeps a truncated VALIDSIG from having its own leading fingerprint
// re-read as a primary, and anything that is not a fingerprint is ignored -
// finding no primary only ever makes the cross-check stricter.
func validsigPrimaryKey(fields []string) string {
	if len(fields) > 10 && isHexFingerprint(fields[10]) {
		return strings.ToUpper(fields[10])
	}
	if len(fields) > 9 {
		if last := fields[len(fields)-1]; isHexFingerprint(last) {
			return strings.ToUpper(last)
		}
	}
	return ""
}

// isHexFingerprint guards the optional trailing VALIDSIG field: a gpg that
// prints something other than a fingerprint there, or nothing at all, must
// not have it adopted as a second fingerprint the cross-check would accept.
func isHexFingerprint(s string) bool {
	if len(s) < 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
