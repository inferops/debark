package sign

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/manifest"
)

func TestGenerateKey_LoadAndKeyIDConsistency(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "operator.key")

	keyID, err := GenerateKey(privPath, "test comment")
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(keyID) != 16 {
		t.Fatalf("keyID = %q, want 16 hex characters", keyID)
	}

	pubPath := strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix
	if _, err := os.Stat(pubPath); err != nil {
		t.Fatalf("public key file was not written: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(privPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("private key file mode = %o, want 0600", perm)
		}
	}

	signer, err := newEd25519FileSigner(privPath)
	if err != nil {
		t.Fatalf("newEd25519FileSigner: %v", err)
	}
	defer signer.Close()
	if signer.KeyID() != keyID {
		t.Fatalf("signer.KeyID() = %q, GenerateKey returned %q", signer.KeyID(), keyID)
	}

	pub, err := loadPublicKeyFile(pubPath)
	if err != nil {
		t.Fatalf("loadPublicKeyFile: %v", err)
	}
	if pub.KeyID != keyID {
		t.Fatalf("public key file KeyID = %q, want %q", pub.KeyID, keyID)
	}
	if pub.Comment != "test comment" {
		t.Fatalf("comment = %q, want %q", pub.Comment, "test comment")
	}
	if len(pub.Raw) != ed25519.PublicKeySize {
		t.Fatalf("public key material is %d bytes, want %d", len(pub.Raw), ed25519.PublicKeySize)
	}
}

func TestGenerateKey_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "operator.key")
	if _, err := GenerateKey(privPath, ""); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if _, err := GenerateKey(privPath, ""); err == nil {
		t.Fatal("GenerateKey silently overwrote an existing key file")
	}
}

func TestEd25519_SignVerify_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "operator.key")
	if _, err := GenerateKey(privPath, ""); err != nil {
		t.Fatal(err)
	}
	pubPath := strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix

	signer, err := SignerFor(context.Background(), privPath)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer signer.Close()
	if signer.Kind() != manifest.SignerEd25519File {
		t.Fatalf("Kind() = %q, want %q", signer.Kind(), manifest.SignerEd25519File)
	}

	canon := []byte(`{"hello":"world"}`)
	sig, err := signer.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if sig.SignerKind != manifest.SignerEd25519File || sig.Algorithm != "ed25519" {
		t.Fatalf("unexpected signature block: %+v", sig)
	}
	if _, err := time.Parse(time.RFC3339, sig.CreatedAt); err != nil {
		t.Fatalf("CreatedAt = %q is not RFC3339: %v", sig.CreatedAt, err)
	}

	verifier, err := VerifierFor(context.Background(), KeySource{Files: []string{pubPath}}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	if err := verifier.Verify(context.Background(), manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Tampering with the payload after the fact must be caught.
	if err := verifier.Verify(context.Background(), manifest.SignPurpose, []byte(`{"hello":"tampered"}`), sig); err == nil {
		t.Fatal("Verify accepted a signature over the wrong payload")
	}
}

// TestSigningInput_DomainSeparation is the explicit requirement: a signature
// made for one purpose must never verify under a different one.
func TestSigningInput_DomainSeparation(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "operator.key")
	if _, err := GenerateKey(privPath, ""); err != nil {
		t.Fatal(err)
	}
	pubPath := strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix

	signer, err := SignerFor(context.Background(), privPath)
	if err != nil {
		t.Fatal(err)
	}
	defer signer.Close()

	canon := []byte(`{"x":1}`)
	sig, err := signer.Sign(context.Background(), "purpose-a", canon)
	if err != nil {
		t.Fatal(err)
	}

	verifier, err := VerifierFor(context.Background(), KeySource{Files: []string{pubPath}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), "purpose-a", canon, sig); err != nil {
		t.Fatalf("signature did not verify under its own purpose: %v", err)
	}
	if err := verifier.Verify(context.Background(), "purpose-b", canon, sig); err == nil {
		t.Fatal("signature over purpose-a verified under purpose-b")
	}
}

func TestVerifierFor_RefusesSameMediaKey(t *testing.T) {
	bundleDir := t.TempDir()

	// A real key, so the AllowSameMedia override below has valid key material
	// to load - this test is about the same-media refusal, not key parsing.
	genDir := t.TempDir()
	privPath := filepath.Join(genDir, "operator.key")
	if _, err := GenerateKey(privPath, ""); err != nil {
		t.Fatal(err)
	}
	pubBytes, err := os.ReadFile(strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix)
	if err != nil {
		t.Fatal(err)
	}
	insideKey := filepath.Join(bundleDir, "operator.pub")
	if err := os.WriteFile(insideKey, pubBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = VerifierFor(context.Background(), KeySource{Files: []string{insideKey}}, bundleDir)
	if err == nil {
		t.Fatal("VerifierFor accepted a key source that resolves inside the bundle")
	}
	if !isSameMediaErr(err) {
		t.Fatalf("error does not wrap ErrSameMediaKey: %v", err)
	}

	// AllowSameMedia is the documented override.
	if _, err := VerifierFor(context.Background(), KeySource{Files: []string{insideKey}, AllowSameMedia: true}, bundleDir); err != nil {
		t.Fatalf("VerifierFor with AllowSameMedia: %v", err)
	}
}

func TestVerifierFor_AllowsKeyOutsideBundle(t *testing.T) {
	bundleDir := t.TempDir()
	outsideDir := t.TempDir()
	privPath := filepath.Join(outsideDir, "operator.key")
	if _, err := GenerateKey(privPath, ""); err != nil {
		t.Fatal(err)
	}
	pubPath := strings.TrimSuffix(privPath, PrivateKeyFileSuffix) + PublicKeyFileSuffix

	if _, err := VerifierFor(context.Background(), KeySource{Files: []string{pubPath}}, bundleDir); err != nil {
		t.Fatalf("VerifierFor refused a key outside the bundle: %v", err)
	}
}

func isSameMediaErr(err error) bool {
	for err != nil {
		if err == ErrSameMediaKey {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- gpg: guarded, skips cleanly when gpg is unavailable or unusable. ------

func requireGPG(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not found on PATH; skipping gpg signer/verifier test")
	}
	return path
}

// TestGPG_SignVerify_RoundTrip is the one test in this package that drives the
// real gpg binary. Everything about the gpg signer and verifier is covered
// deterministically by the stub in gpg_test.go; this exists only to catch
// drift between what debark expects of gpg's output and what a real gpg
// actually prints, on a machine that has one.
//
// It is fenced accordingly, because a test must never reach an operator's own
// keys or open a prompt on their desktop:
//
//   - GNUPGHOME is a throwaway directory, so the default keyring is never
//     opened, read or written;
//   - --batch, --pinentry-mode loopback and an explicit empty --passphrase
//     together mean no invocation can open a pinentry dialog, whatever the
//     local gpg-agent configuration says (%no-protection alone is not enough
//     on every gpg version);
//   - anything gpg started in that home is killed on the way out, so no agent
//     outlives the test holding a directory t.TempDir is about to delete.
func TestGPG_SignVerify_RoundTrip(t *testing.T) {
	requireGPG(t)
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() {
		// Registered after t.Setenv, so it runs BEFORE GNUPGHOME is restored.
		kill := exec.Command("gpgconf", "--homedir", home, "--kill", "all")
		_ = kill.Run()
	})

	const email = "debark-test@example.invalid"
	batch := "%no-protection\nKey-Type: RSA\nKey-Length: 2048\n" +
		"Name-Real: debark test\nName-Email: " + email + "\nExpire-Date: 0\n%commit\n"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--gen-key")
	cmd.Stdin = strings.NewReader(batch)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Skipf("gpg --batch --gen-key did not succeed in this environment (skipping): %v: %s", err, errBuf.String())
	}

	signer, err := SignerFor(context.Background(), "gpg:"+email)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer signer.Close()
	if signer.Kind() != manifest.SignerGPG {
		t.Fatalf("Kind() = %q, want %q", signer.Kind(), manifest.SignerGPG)
	}
	if len(signer.KeyID()) != 40 {
		t.Fatalf("KeyID() = %q, want a 40-character fingerprint", signer.KeyID())
	}

	canon := []byte(`{"hello":"gpg"}`)
	sig, err := signer.Sign(context.Background(), "purpose-a", canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if sig.SignerKind != manifest.SignerGPG {
		t.Fatalf("unexpected signer_kind: %q", sig.SignerKind)
	}

	verifier, err := VerifierFor(context.Background(), KeySource{}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifierFor: %v", err)
	}
	if err := verifier.Verify(context.Background(), "purpose-a", canon, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := verifier.Verify(context.Background(), "purpose-b", canon, sig); err == nil {
		t.Fatal("gpg signature over purpose-a verified under purpose-b")
	}
}

// --- plugin: guarded, skips cleanly when the sample plugin cannot be built. -

func buildSamplePlugin(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Skipf("cannot resolve module root: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "signer-plugin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./examples/signer-plugin")
	cmd.Dir = repoRoot
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Skipf("could not build examples/signer-plugin (skipping): %v: %s", err, errBuf.String())
	}
	return bin
}

func TestPlugin_SignVerify_RoundTrip(t *testing.T) {
	bin := buildSamplePlugin(t)
	t.Setenv("DEBARK_SAMPLE_SIGNER_KEYFILE", filepath.Join(t.TempDir(), "plugin.key"))

	s, err := SignerFor(context.Background(), "plugin:"+bin)
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	defer s.Close()

	if !strings.HasPrefix(s.Kind(), manifest.SignerPluginPrefix) {
		t.Fatalf("Kind() = %q, want a %q prefix", s.Kind(), manifest.SignerPluginPrefix)
	}
	if s.KeyID() == "" {
		t.Fatal("KeyID() is empty; the sample plugin implements key_info")
	}

	ps, ok := s.(*pluginSigner)
	if !ok {
		t.Fatalf("SignerFor(\"plugin:...\") returned %T, want *pluginSigner", s)
	}
	ps.keyMu.Lock()
	pubB64 := ""
	if ps.keyInfo != nil {
		pubB64 = ps.keyInfo.PublicKey
	}
	ps.keyMu.Unlock()
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("plugin key_info did not return a usable ed25519 public key (%q): %v", pubB64, err)
	}

	canon := []byte("hello canonical bytes, from the plugin round trip test")
	sig, err := s.Sign(context.Background(), manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if sig.KeyID != s.KeyID() {
		t.Fatalf("signature KeyID %q != Signer.KeyID() %q", sig.KeyID, s.KeyID())
	}

	// verify's ed25519 path matches on Algorithm as well as SignerKind (see
	// verifier.go), so a plugin-produced signature verifies once its public
	// key is trusted the normal way - exactly what examples/signer-plugin's
	// README documents.
	trusted := map[string]ed25519.PublicKey{strings.ToLower(sig.KeyID): ed25519.PublicKey(pub)}
	if err := verifyEd25519Signature(trusted, manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("plugin signature did not verify: %v", err)
	}
	if err := verifyEd25519Signature(trusted, "a-different-purpose", canon, sig); err == nil {
		t.Fatal("plugin signature verified under the wrong purpose")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// buildFakePlugin compiles a tiny standalone program (not part of the
// module's own package graph) that prints handshakeLine and then just waits
// for stdin to close, so tests can exercise SignerFor's handshake validation
// without a real plugin.
func buildFakePlugin(t *testing.T, handshakeLine string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake_plugin.go")
	source := "package main\n\n" +
		"import (\"bufio\"; \"fmt\"; \"os\")\n\n" +
		"func main() {\n" +
		"\tfmt.Println(`" + handshakeLine + "`)\n" +
		"\tbufio.NewReader(os.Stdin).ReadString('\\n')\n" +
		"}\n"
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("write fake plugin source: %v", err)
	}
	bin := filepath.Join(dir, "fake_plugin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Skipf("could not build fake plugin helper (skipping): %v: %s", err, errBuf.String())
	}
	return bin
}

func TestPlugin_RejectsProtocolMismatch(t *testing.T) {
	bin := buildFakePlugin(t, `{"protocol":"debark.plugin/v2","name":"fake","version":"0.0.1","capabilities":["sign"]}`)
	_, err := SignerFor(context.Background(), "plugin:"+bin)
	if err == nil {
		t.Fatal("SignerFor accepted a plugin announcing the wrong protocol version")
	}
}

func TestPlugin_RejectsCapabilityMismatch(t *testing.T) {
	bin := buildFakePlugin(t, `{"protocol":"debark.plugin/v1","name":"fake","version":"0.0.1","capabilities":["key_info"]}`)
	_, err := SignerFor(context.Background(), "plugin:"+bin)
	if err == nil {
		t.Fatal("SignerFor accepted a plugin that never announced the sign capability")
	}
}
