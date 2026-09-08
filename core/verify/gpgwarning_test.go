package verify

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/sign"
)

// requireGPGForVerify mirrors core/sign's own requireGPG (unexported there,
// so not reusable directly): skip cleanly when gpg is not on PATH, rather
// than failing a security-relevant test for an environment reason.
func requireGPGForVerify(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found on PATH; skipping gpg default-keyring warning test")
	}
}

// isolatedGNUPGHOME points GNUPGHOME at a throwaway directory for the rest of
// the test and makes it impossible for anything run inside it to prompt.
//
// Two hazards this closes, both of which have actually bitten:
//
//   - The operator's real ~/.gnupg. A test that generates and signs with keys
//     must never touch it, so GNUPGHOME is redirected before the first gpg
//     call and every gpg invocation - including the ones core/sign makes on
//     this test's behalf, which take no flags from here - inherits it.
//   - An interactive pinentry dialog. --batch alone does NOT prevent one; only
//     loopback pinentry does, and core/sign's gpg signer cannot be passed that
//     flag from a test. Writing it into this home's gpg.conf (plus
//     allow-loopback-pinentry in gpg-agent.conf, without which gpg refuses
//     loopback) applies it to every invocation in this home instead, so a
//     hung passphrase prompt on somebody's desktop is not a possible outcome
//     of running the test suite.
//
// The gpg-agent gpg starts inside this home is killed on cleanup: it would
// otherwise outlive the test and hold the temp directory open.
func isolatedGNUPGHOME(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	conf := "no-tty\npinentry-mode loopback\n"
	if err := os.WriteFile(filepath.Join(home, "gpg.conf"), []byte(conf), 0o600); err != nil {
		t.Fatalf("write gpg.conf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "gpg-agent.conf"), []byte("allow-loopback-pinentry\n"), 0o600); err != nil {
		t.Fatalf("write gpg-agent.conf: %v", err)
	}
	t.Cleanup(func() {
		if _, err := exec.LookPath("gpgconf"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "gpgconf", "--homedir", home, "--kill", "all").Run()
	})
	return home
}

// TestVerify_GPGDefaultKeyringWarning is the regression test for F4
// (docs/security/review-findings.md): gpg's fallback to the verifying
// machine's ambient default keyring when no --keyring is given is standard,
// intentional gpg behaviour and is not changed by this test — what must
// change is that verify.Report says so, so an operator reading the report
// (not the source code) can tell which trust set actually decided the
// bundle was signed.
func TestVerify_GPGDefaultKeyringWarning(t *testing.T) {
	requireGPGForVerify(t)
	isolatedGNUPGHOME(t)

	const email = "debark-verify-test@example.invalid"
	batch := "%no-protection\nKey-Type: RSA\nKey-Length: 2048\n" +
		"Name-Real: debark verify test\nName-Email: " + email + "\nExpire-Date: 0\n%commit\n"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	genCmd := exec.CommandContext(ctx, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--gen-key")
	genCmd.Stdin = strings.NewReader(batch)
	var genErrBuf bytes.Buffer
	genCmd.Stderr = &genErrBuf
	if err := genCmd.Run(); err != nil {
		t.Skipf("gpg --batch --gen-key did not succeed in this environment (skipping): %v: %s", err, genErrBuf.String())
	}

	f := buildFixture(t)
	resign(t, f, "gpg:"+email)

	// --- Case 1: no --keyring (KeySource.GPGKeyring left empty) --------
	// gpg falls back to GNUPGHOME's ambient default keyring, which is
	// exactly where the key just generated above lives — the signature
	// genuinely verifies, on the strength of a keyring nothing here
	// designated explicitly for debark.
	report, err := New().Verify(context.Background(), f.dir, Options{Keys: sign.KeySource{Files: []string{f.pubPath}}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("report not OK: problems=%+v", report.Problems)
	}
	if !report.Signed {
		t.Fatalf("report.Signed = false, want true")
	}
	if !hasWarningContaining(report, "default") || !hasWarningContaining(report, "keyring") {
		t.Fatalf("expected a warning naming the default GPG keyring, got warnings=%v", report.Warnings)
	}

	// --- Case 2: an explicit --keyring, containing only the release key -
	// The same signature, the same bundle — only the trust-set source
	// changes. The warning must disappear once the keyring is explicit.
	keyringPath := filepath.Join(t.TempDir(), "release.gpg")
	exportCmd := exec.CommandContext(ctx, "gpg", "--batch", "--output", keyringPath, "--export", email)
	var exportErrBuf bytes.Buffer
	exportCmd.Stderr = &exportErrBuf
	if err := exportCmd.Run(); err != nil {
		t.Fatalf("gpg --export: %v: %s", err, exportErrBuf.String())
	}

	report2, err := New().Verify(context.Background(), f.dir, Options{Keys: sign.KeySource{GPGKeyring: keyringPath}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report2.OK {
		t.Fatalf("report2 not OK: problems=%+v", report2.Problems)
	}
	if !report2.Signed {
		t.Fatalf("report2.Signed = false, want true")
	}
	if hasWarningContaining(report2, "default") {
		t.Fatalf("unexpected default-keyring warning when an explicit --keyring was given: warnings=%v", report2.Warnings)
	}
}

// TestVerify_NoDefaultKeyringWarningForEd25519 guards against the warning
// becoming unconditional (e.g. firing for every signature, gpg or not):
// buildFixture's own signature is ed25519-file, which has no "default
// keyring" concept at all (an empty KeySource there is a genuinely empty
// trust set, per sign/gpg.go's own comment contrasting the two paths), so
// no such warning should ever appear for it.
func TestVerify_NoDefaultKeyringWarningForEd25519(t *testing.T) {
	f := buildFixture(t)
	report, err := verifyFixture(t, f, Options{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("report not OK: problems=%+v", report.Problems)
	}
	if hasWarningContaining(report, "default") {
		t.Fatalf("unexpected default-keyring warning for an ed25519-file signature: warnings=%v", report.Warnings)
	}
}

func hasWarningContaining(report *Report, substr string) bool {
	substr = strings.ToLower(substr)
	for _, w := range report.Warnings {
		if strings.Contains(strings.ToLower(w), substr) {
			return true
		}
	}
	return false
}
