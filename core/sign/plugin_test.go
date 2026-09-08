package sign

// Failure paths of the debark.plugin/v1 client (plugin.go).
//
// The plugin signer is the only Signer whose counterparty is a program
// debark did not write, so it is the only one where "the signer said yes"
// and "the bundle is really signed" can come apart. Every test in this file
// exists because there is a way for that gap to open: a plugin that never
// speaks, one that speaks a protocol we do not, one that answers a question we
// did not ask, one that never answers at all, one that says "signed" and hands
// back nothing usable, and one that will not die when told to.
//
// The child process in all of these is this same test binary re-entered as a
// helper; see helperprocess_test.go for how and why.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/inferops/debark/api/plugin/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// startHelperPlugin points SignerFor at this test binary running the named
// helper behaviour. It deliberately returns the raw (Signer, error) pair so a
// caller can assert on a failure rather than being fataled out.
func startHelperPlugin(t *testing.T, ctx context.Context, behaviour string) (Signer, error) {
	t.Helper()
	t.Setenv(envPluginBehaviour, behaviour)
	return SignerFor(ctx, "plugin:"+testExecutable(t))
}

// mustStartHelperPlugin is startHelperPlugin for the cases where construction
// is expected to succeed, with Close registered so a failing assertion cannot
// leave a plugin process behind.
func mustStartHelperPlugin(t *testing.T, ctx context.Context, behaviour string) Signer {
	t.Helper()
	s, err := startHelperPlugin(t, ctx, behaviour)
	if err != nil {
		t.Fatalf("SignerFor(plugin, %q): %v", behaviour, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPlugin_HandshakeRejections covers every way the opening handshake can be
// wrong. All of them must fail closed: SignerFor returns an error and the
// caller never gets a Signer it could sign with. The handshake is the only
// point at which debark establishes that the thing on the other end of the
// pipe is a debark plugin at all, so a lenient handshake is a lenient trust
// decision.
//
// Every case here has the plugin EXIT, because the handshake read itself is
// unbounded: plugin.go:82 blocks in ReadString('\n') with no deadline and
// without consulting the ctx SignerFor was given, so a plugin that starts,
// says nothing and stays alive hangs the caller indefinitely. That is a
// separate defect from anything this table asserts and there is deliberately
// no test for it here - a test that reproduces it would itself hang.
func TestPlugin_HandshakeRejections(t *testing.T) {
	cases := []struct {
		name      string
		behaviour string
		class     dferr.Class
		wantMsg   string // substring of the error message
	}{
		{
			// The child writes nothing and exits. The host's first read is
			// EOF, which must be an error and not an empty-but-acceptable
			// handshake.
			name: "no handshake at all", behaviour: "silent-exit",
			class: dferr.Environment, wantMsg: "read handshake",
		},
		{
			// Same, but with a diagnostic on stderr the operator needs to see.
			name: "dies with a message on stderr", behaviour: "stderr-then-exit",
			class: dferr.Environment, wantMsg: "read handshake",
		},
		{
			name: "handshake is not JSON", behaviour: "not-json",
			class: dferr.Usage, wantMsg: "malformed handshake",
		},
		{
			// Valid JSON of the wrong shape. Unmarshalling an array into a
			// struct fails, so this lands on the malformed path rather than
			// quietly producing a zero-valued Handshake.
			name: "handshake is a JSON array", behaviour: "handshake-not-an-object",
			class: dferr.Usage, wantMsg: "malformed handshake",
		},
		{
			// A byte-complete handshake with no terminating newline. The
			// framing is line-oriented, so a line that never ended is a line
			// that was never sent: ReadString hands back the partial text
			// together with io.EOF and the host must treat that as a failure
			// rather than parse what it happened to receive.
			name: "handshake truncated (no newline)", behaviour: "truncated-handshake",
			class: dferr.Environment, wantMsg: "read handshake",
		},
		{
			name: "wrong protocol version", behaviour: "wrong-protocol",
			class: dferr.Usage, wantMsg: "protocol",
		},
		{
			name: "sign capability not announced", behaviour: "no-sign-capability",
			class: dferr.Usage, wantMsg: v1.CapabilitySign,
		},
		{
			name: "no capabilities at all", behaviour: "no-capabilities",
			class: dferr.Usage, wantMsg: v1.CapabilitySign,
		},
		{
			// Kind() is manifest.SignerPluginPrefix + name, so an empty name
			// would produce the signer_kind "plugin:" - unattributable in a
			// manifest an operator has to audit later.
			name: "handshake carries no name", behaviour: "empty-name",
			class: dferr.Usage, wantMsg: "missing a name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s, err := startHelperPlugin(t, ctx, tc.behaviour)
			if err == nil {
				_ = s.Close()
				t.Fatalf("SignerFor accepted a plugin whose handshake was %s", tc.name)
			}
			if got := dferr.ClassOf(err); got != tc.class {
				t.Errorf("error class = %v, want %v (err: %v)", got, tc.class, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestPlugin_HandshakeFailureSurfacesStderr protects the operator's only clue
// about why a plugin would not start. The plugin's own diagnostics are the
// difference between "sign failed" and "the smartcard is not inserted".
func TestPlugin_HandshakeFailureSurfacesStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := startHelperPlugin(t, ctx, "stderr-then-exit")
	if err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted a plugin that exited without a handshake")
	}
	hint := dferr.HintOf(err)
	if !strings.Contains(hint, "cannot reach the signing device") {
		t.Fatalf("hint does not carry the plugin's stderr; got %q", hint)
	}
}

// TestPlugin_StderrRetentionIsCapped checks the bound on retained stderr and,
// more importantly, that the lines kept are the LAST ones - the ones written
// closest to the failure, which is where the cause of it is.
func TestPlugin_StderrRetentionIsCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := startHelperPlugin(t, ctx, "flood-stderr"); err == nil {
		t.Fatal("SignerFor accepted a plugin that never handshook")
	} else {
		hint := dferr.HintOf(err)
		lines := strings.Split(strings.TrimPrefix(hint, "plugin stderr:\n"), "\n")
		if len(lines) > maxStderrLines {
			t.Fatalf("retained %d stderr lines, want at most %d", len(lines), maxStderrLines)
		}
		// maxStderrLines*3 lines were written, numbered from 0; the last one
		// must have survived and the first one must not have.
		if !strings.Contains(hint, "helper noise line "+strconv.Itoa(maxStderrLines*3-1)) {
			t.Errorf("the last stderr line was dropped; retained hint:\n%s", hint)
		}
		if strings.Contains(hint, "helper noise line 0\n") {
			t.Errorf("the oldest stderr line was retained instead of the newest")
		}
	}
}

// TestPlugin_RejectsMismatchedResponseID is the headline protocol test.
//
// The helper answers the sign request with a cryptographically PERFECT
// signature - the same bytes the honest mode would return - carrying an id
// that does not match the request. Nothing about the payload is wrong; the
// only defect is that it is an answer to a different question. A host that
// paired replies positionally, or that fell back to "the only reply I got",
// would accept it and emit a manifest signed by a call it never made. The
// requirement is that the reply is discarded and the call fails.
func TestPlugin_RejectsMismatchedResponseID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:wrong-id")

	// key_info answered correctly, so the signer is fully healthy up to here -
	// the id mismatch below is the only thing that goes wrong.
	if s.KeyID() != helperKeyID() {
		t.Fatalf("KeyID() = %q, want %q (key_info should have succeeded)", s.KeyID(), helperKeyID())
	}

	signCtx, signCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer signCancel()
	sig, err := s.Sign(signCtx, manifest.SignPurpose, []byte(`{"payload":"whatever"}`))
	if err == nil {
		t.Fatalf("Sign accepted a response carrying the wrong id: %+v", sig)
	}
	if sig.Signature != "" {
		t.Fatalf("Sign returned an error AND a signature: %+v", sig)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected the uncorrelated reply to be ignored and the call to time out, got: %v", err)
	}
}

// TestPlugin_TimesOutWhenPluginNeverAnswers checks the deadline actually
// fires. A signer that blocks forever is not a hang in the plugin, it is a
// hang in `debark build`: an operator with a wedged smartcard reader gets no
// bundle and no error.
func TestPlugin_TimesOutWhenPluginNeverAnswers(t *testing.T) {
	// The construction context bounds the key_info probe. key_info is
	// optional, so newPluginSigner is expected to succeed regardless - but it
	// must succeed after the deadline, not hang on it.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	s := mustStartHelperPlugin(t, ctx, "serve:hang")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("newPluginSigner took %v; the key_info probe did not respect its deadline", elapsed)
	}
	// key_info never answered, so there is no key identity to report. An empty
	// KeyID here is honest; a fabricated one would not be.
	if s.KeyID() != "" {
		t.Fatalf("KeyID() = %q, want \"\" after key_info timed out", s.KeyID())
	}

	signCtx, signCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer signCancel()
	start = time.Now()
	sig, err := s.Sign(signCtx, manifest.SignPurpose, []byte("payload"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Sign returned success from a plugin that never replied: %+v", sig)
	}
	if sig.Signature != "" {
		t.Fatalf("Sign returned an error AND a signature: %+v", sig)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("Sign took %v to give up on a 500ms deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error does not say it timed out: %v", err)
	}
	if dferr.ClassOf(err) != dferr.Environment {
		t.Fatalf("error class = %v, want Environment", dferr.ClassOf(err))
	}
}

// TestPlugin_CloseReapsAChildThatWillNotExit is the leaked-process test.
//
// The helper acknowledges shutdown and then refuses to die, ignoring the
// closed stdin that the protocol calls the authoritative signal. Close must
// escalate to killing it. The proof is a heartbeat file the child appends to
// every 20ms for as long as it lives: it must be growing before Close (so the
// detector itself is known to work) and frozen afterwards.
func TestPlugin_CloseReapsAChildThatWillNotExit(t *testing.T) {
	if testing.Short() {
		t.Skip("Close's kill escalation waits out a fixed 5s grace period")
	}
	beat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv(envPluginHeartbeat, beat)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := startHelperPlugin(t, ctx, "serve:ignore-shutdown")
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}

	// Establish that the heartbeat detector is not vacuous: a live child must
	// visibly grow the file.
	before := heartbeatSize(t, beat)
	time.Sleep(200 * time.Millisecond)
	during := heartbeatSize(t, beat)
	if during <= before {
		t.Fatalf("heartbeat did not grow while the plugin was alive (%d -> %d); the leak detector would prove nothing", before, during)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ps, ok := s.(*pluginSigner)
	if !ok {
		t.Fatalf("SignerFor returned %T, want *pluginSigner", s)
	}
	if ps.cmd.ProcessState == nil {
		t.Fatal("Close returned before the child was waited on: the process was left unreaped")
	}

	// And the child really is gone, not merely detached: the heartbeat must
	// stop for good.
	settled := heartbeatSize(t, beat)
	time.Sleep(300 * time.Millisecond)
	if after := heartbeatSize(t, beat); after != settled {
		t.Fatalf("plugin process is still running after Close: heartbeat grew %d -> %d", settled, after)
	}
}

// TestPlugin_FailedHandshakeReapsChild is the same requirement on the other
// path: a plugin rejected during the handshake never becomes a Signer, so
// nobody will ever call Close on it. If newPluginSigner does not reap it
// itself, `debark build` leaves a stranger's process running on the operator
// 's machine after printing an error.
func TestPlugin_FailedHandshakeReapsChild(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv(envPluginHeartbeat, beat)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// wrong-protocol prints its handshake and then blocks on stdin forever,
	// so the only thing that can end it is the host killing it.
	s, err := startHelperPlugin(t, ctx, "wrong-protocol")
	if err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted a plugin speaking the wrong protocol")
	}

	settled := heartbeatSize(t, beat)
	time.Sleep(300 * time.Millisecond)
	if after := heartbeatSize(t, beat); after != settled {
		t.Fatalf("plugin process survived a rejected handshake: heartbeat grew %d -> %d", settled, after)
	}
}

func heartbeatSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// TestPlugin_SignFailureModes walks the ways a plugin can answer a sign call
// without actually producing a usable signature. Every one of them must be an
// error: "the plugin replied" is not "the manifest is signed".
func TestPlugin_SignFailureModes(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		class   dferr.Class
		wantMsg string
	}{
		{
			// The single worst outcome: a success response carrying nothing.
			name: "empty signature", mode: "empty-signature",
			class: dferr.Usage, wantMsg: "empty signature",
		},
		{
			name: "signature is not base64", mode: "non-base64-signature",
			class: dferr.Usage, wantMsg: "non-base64",
		},
		{
			// A result field that is a bare string rather than an object.
			name: "sign result is the wrong shape", mode: "malformed-sign-result",
			class: dferr.Usage, wantMsg: "malformed sign result",
		},
		{
			name: "plugin reports key unavailable", mode: "error-key-unavailable",
			class: dferr.Environment, wantMsg: "smartcard is not inserted",
		},
		{
			name: "plugin reports the operator declined", mode: "error-user-declined",
			class: dferr.Usage, wantMsg: "declined",
		},
		{
			name: "plugin does not support sign", mode: "error-unsupported",
			class: dferr.Usage, wantMsg: "does not support",
		},
		{
			name: "plugin reports an internal failure", mode: "error-internal",
			class: dferr.Environment, wantMsg: "internal helper failure",
		},
		{
			// The plugin crashes mid-call. The pending call must fail as soon
			// as stdout closes rather than sit out the full timeout.
			name: "plugin exits instead of answering", mode: "exit-on-sign",
			class: dferr.Environment, wantMsg: "exited before responding",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s := mustStartHelperPlugin(t, ctx, "serve:"+tc.mode)

			signCtx, signCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer signCancel()
			start := time.Now()
			sig, err := s.Sign(signCtx, manifest.SignPurpose, []byte("canonical bytes"))
			if err == nil {
				t.Fatalf("Sign reported success for %s: %+v", tc.name, sig)
			}
			if sig.Signature != "" || sig.KeyID != "" || sig.SignerKind != "" {
				t.Fatalf("Sign returned an error AND a populated signature block: %+v", sig)
			}
			if got := dferr.ClassOf(err); got != tc.class {
				t.Errorf("error class = %v, want %v (err: %v)", got, tc.class, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
			if elapsed := time.Since(start); elapsed > 9*time.Second {
				t.Errorf("Sign took %v; it waited out the deadline instead of failing on the response", elapsed)
			}
		})
	}
}

// TestPlugin_HonestSignerRoundTrip is the control for every failure above: the
// same helper, behaving correctly, must produce a signature that verifies
// against the key the parent derived for itself. Without this, a bug that made
// every plugin call fail would leave the whole failure table passing.
func TestPlugin_HonestSignerRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:good")

	if want := manifest.SignerPluginPrefix + pluginHelperName; s.Kind() != want {
		t.Fatalf("Kind() = %q, want %q", s.Kind(), want)
	}
	if s.KeyID() != helperKeyID() {
		t.Fatalf("KeyID() = %q, want %q", s.KeyID(), helperKeyID())
	}

	canon := []byte(`{"schema":"debark.manifest/v1","files":[]}`)
	sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if sig.SignerKind != manifest.SignerPluginPrefix+pluginHelperName {
		t.Fatalf("signer_kind = %q", sig.SignerKind)
	}
	if sig.KeyID != helperKeyID() || sig.Algorithm != AlgorithmEd25519 {
		t.Fatalf("unexpected signature block: %+v", sig)
	}
	if _, err := time.Parse(time.RFC3339, sig.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC3339: %v", sig.CreatedAt, err)
	}

	trusted := helperTrustSet()
	if err := verifyEd25519Signature(trusted, manifest.SignPurpose, canon, sig); err != nil {
		t.Fatalf("an honestly produced plugin signature did not verify: %v", err)
	}
}

// helperTrustSet is the parent's own view of the helper's public key, derived
// from the shared seed rather than from anything the helper said.
func helperTrustSet() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{helperKeyID(): helperPublicKey()}
}

// TestPlugin_GarbageSignatureIsCaughtNoLaterThanVerify is the far end of the
// same defence as TestPlugin_SignatureIsCheckedBeforeItIsWritten: the host now
// refuses a non-signature at signing time, but a block that reaches a verifier
// some other way (a bundle written by an older build, or by another tool) must
// still fail there. Both ends are asserted, because a check at signing time is
// a convenience for the operator and the check at verify time is the security
// property.
func TestPlugin_GarbageSignatureIsCaughtNoLaterThanVerify(t *testing.T) {
	canon := []byte("canonical bytes that will not actually be signed")
	garbage := manifest.Signature{
		SignerKind: manifest.SignerPluginPrefix + "acme-hsm",
		KeyID:      helperKeyID(),
		Algorithm:  AlgorithmEd25519,
		Signature:  base64.StdEncoding.EncodeToString([]byte("not a signature, just some bytes")),
	}
	if err := verifyEd25519Signature(helperTrustSet(), manifest.SignPurpose, canon, garbage); err == nil {
		t.Fatal("a garbage signature verified against the plugin's own trusted key")
	}
	if raw, _ := base64.StdEncoding.DecodeString(garbage.Signature); len(raw) == ed25519.SignatureSize {
		t.Fatal("the fixture is a plausible-length signature; the case is no longer testing garbage")
	}
}

// TestPlugin_KeyInfoIsOptional: the protocol allows a plugin that cannot
// answer key_info (an HSM that will not export identity until it signs). That
// must not be fatal, KeyID() must stay honestly empty until a Sign call fills
// it in, and it must then report what the plugin actually said.
func TestPlugin_KeyInfoIsOptional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:no-key-info")

	if s.KeyID() != "" {
		t.Fatalf("KeyID() = %q before any Sign call, want \"\"", s.KeyID())
	}
	sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("payload"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if sig.KeyID != helperKeyID() {
		t.Fatalf("signature KeyID = %q, want %q", sig.KeyID, helperKeyID())
	}
	if s.KeyID() != helperKeyID() {
		t.Fatalf("KeyID() = %q after signing, want the key the plugin signed with (%q)", s.KeyID(), helperKeyID())
	}
}

// TestPlugin_MalformedKeyInfoIsNotFatalButIsNotBelieved: a key_info reply of
// the wrong shape must be discarded, not half-parsed into a plausible-looking
// key identity.
func TestPlugin_MalformedKeyInfoIsNotFatalButIsNotBelieved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:malformed-key-info")
	if s.KeyID() != "" {
		t.Fatalf("KeyID() = %q after a malformed key_info reply, want \"\"", s.KeyID())
	}
}

// TestPlugin_EmptyKeyIDIsNotSilentlyPapered: a plugin that signs but names no
// key produces a block nothing can ever attribute - the trust set is a map
// keyed by key id, and the empty string is in nobody's keyring.
//
// The host now refuses that at signing time rather than writing it into a
// bundle. Both halves are asserted, because the second is what makes the
// first a hardening and not just a different error message: Sign fails, AND a
// block with no key id is still untrusted if one reaches a verifier some other
// way.
func TestPlugin_EmptyKeyIDIsNotSilentlyPapered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:no-key-id")

	canon := []byte("payload")
	sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
	if err == nil {
		t.Fatalf("Sign wrote a signature naming no key: %+v", sig)
	}
	if sig.Signature != "" {
		t.Fatalf("Sign returned an error AND a signature: %+v", sig)
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage (err: %v)", dferr.ClassOf(err), err)
	}

	unattributed := manifest.Signature{
		SignerKind: manifest.SignerPluginPrefix + "acme-hsm",
		Algorithm:  AlgorithmEd25519,
		Signature:  base64.StdEncoding.EncodeToString(ed25519.Sign(helperPrivateKey(), SigningInput(manifest.SignPurpose, canon))),
	}
	if err := verifyEd25519Signature(helperTrustSet(), manifest.SignPurpose, canon, unattributed); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("a signature naming no key was not treated as untrusted: %v", err)
	}
}

// TestPluginError_Classification maps every protocol error code onto the class
// and hint an operator acts on. The classes are not cosmetic: dferr.Class is
// the process exit code, so "the operator cancelled the prompt" (usage, 1) and
// "the key is unreachable" (environment, 2) are distinguishable by a script.
func TestPluginError_Classification(t *testing.T) {
	cases := []struct {
		name    string
		in      *v1.Error
		class   dferr.Class
		wantMsg string
		hint    string
	}{
		{
			name: "key unavailable", in: &v1.Error{Code: v1.ErrKeyUnavailable, Message: "no smartcard"},
			class: dferr.Environment, wantMsg: "no smartcard", hint: "key is configured",
		},
		{
			name: "user declined", in: &v1.Error{Code: v1.ErrUserDeclined, Message: "declined at the prompt"},
			class: dferr.Usage, wantMsg: "declined at the prompt",
		},
		{
			name: "unsupported method", in: &v1.Error{Code: v1.ErrUnsupportedMethod, Message: "not implemented"},
			class: dferr.Usage, wantMsg: "does not support",
		},
		{
			name: "internal", in: &v1.Error{Code: v1.ErrInternal, Message: "boom"},
			class: dferr.Environment, wantMsg: "boom",
		},
		{
			name: "unknown code falls back to environment", in: &v1.Error{Code: "something-new", Message: "future code"},
			class: dferr.Environment, wantMsg: "future code",
		},
		{
			// A response with error set to null. There is nothing to report
			// but there is still no signature, so it must not be success.
			name: "no error detail at all", in: nil,
			class: dferr.Environment, wantMsg: "no error detail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pluginError("acme-hsm", v1.MethodSign, tc.in)
			if err == nil {
				t.Fatal("pluginError returned nil")
			}
			if got := dferr.ClassOf(err); got != tc.class {
				t.Errorf("class = %v, want %v", got, tc.class)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
			if !strings.Contains(err.Error(), "acme-hsm") {
				t.Errorf("error %q does not name the plugin", err.Error())
			}
			if tc.hint != "" && !strings.Contains(dferr.HintOf(err), tc.hint) {
				t.Errorf("hint %q does not mention %q", dferr.HintOf(err), tc.hint)
			}
		})
	}
}

// TestSignerFor_RejectsUnusablePluginRefs covers the two references that can
// never produce a plugin: no path at all, and a path that is not a program.
func TestSignerFor_RejectsUnusablePluginRefs(t *testing.T) {
	ctx := context.Background()

	if s, err := SignerFor(ctx, "plugin:"); err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted \"plugin:\" with no path")
	} else if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("empty plugin path: class = %v, want Usage", dferr.ClassOf(err))
	}

	missing := filepath.Join(t.TempDir(), "there-is-no-such-plugin")
	if s, err := SignerFor(ctx, "plugin:"+missing); err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted a plugin path that does not exist")
	} else if dferr.ClassOf(err) != dferr.Environment {
		t.Errorf("missing plugin: class = %v, want Environment", dferr.ClassOf(err))
	}
}

// TestSignerFor_RejectsEmptyRef: an empty --sign value must be a usage error,
// never a silently unsigned bundle.
func TestSignerFor_RejectsEmptyRef(t *testing.T) {
	s, err := SignerFor(context.Background(), "")
	if err == nil {
		_ = s.Close()
		t.Fatal("SignerFor accepted an empty key reference")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Fatalf("class = %v, want Usage", dferr.ClassOf(err))
	}
}

// TestPlugin_SignAfterCloseFails: Close is documented as "always called, even
// on error paths", so a caller with a bug (or a retry loop) can reach Sign
// after it. That must be a clean error - the plugin's stdin is gone, so there
// is no signer any more - and never a panic or a silently empty block.
func TestPlugin_SignAfterCloseFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := startHelperPlugin(t, ctx, "serve:good")
	if err != nil {
		t.Fatalf("SignerFor: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	signCtx, signCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer signCancel()
	sig, err := s.Sign(signCtx, manifest.SignPurpose, []byte("payload"))
	if err == nil {
		t.Fatalf("Sign succeeded after Close: %+v", sig)
	}
	if sig.Signature != "" {
		t.Fatalf("Sign returned an error AND a signature: %+v", sig)
	}
}

// TestPlugin_RemarshalRejectsUnencodableValues covers the small conversion
// helper the sign and key_info replies both go through. It is the only place a
// plugin's arbitrary JSON becomes a typed result, so its error path has to
// return an error rather than a half-populated struct.
func TestPlugin_RemarshalRejectsUnencodableValues(t *testing.T) {
	var out v1.SignResult
	if err := remarshal(make(chan int), &out); err == nil {
		t.Fatal("remarshal accepted a value that cannot be JSON-encoded")
	}
	if out.Signature != "" {
		t.Fatalf("remarshal populated the output despite failing: %+v", out)
	}
}

// TestPlugin_SignerKindIsTheHostsToState is the first half of the S7 fix.
//
// SignResult.SignerKind used to be copied into the signature block whenever it
// was non-empty, so a plugin could label its own output "gpg" or
// "ed25519-file" and the bundle would record provenance from a mechanism that
// never ran. That is the one field in the block debark knows the truth
// about - it started the process and read its handshake - and it was handed to
// the least trusted participant in the exchange.
func TestPlugin_SignerKindIsTheHostsToState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:claims-native-kind")

	sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("payload"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Kind() is built from the name announced at the handshake, which is the
	// only account of what signed that debark has any grounds for.
	want := s.Kind()
	if !strings.HasPrefix(want, manifest.SignerPluginPrefix) {
		t.Fatalf("Kind() = %q, want a %q prefix", want, manifest.SignerPluginPrefix)
	}
	if sig.SignerKind != want {
		t.Fatalf("signer_kind = %q, want %q: the plugin's own claim was recorded", sig.SignerKind, want)
	}
	if sig.SignerKind == manifest.SignerEd25519File {
		t.Fatal("a plugin signature is recorded as debark's native file signer")
	}
}

// TestPlugin_HostileFieldsAreRefused is the second half: a plugin's key_id and
// algorithm end up in debark.manifest.sig, in evidence.json and in verify's
// report, so they are shape-checked before they get there. The observed case
// was a plugin returning signer_kind "gpg" with a spaces-and-all key id.
func TestPlugin_HostileFieldsAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := mustStartHelperPlugin(t, ctx, "serve:hostile-fields")

	sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("payload"))
	if err == nil {
		t.Fatalf("Sign accepted a key id that is not a key id: %+v", sig)
	}
	if !strings.Contains(err.Error(), "key_id") {
		t.Errorf("the error does not say which field is wrong: %v", err)
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage (err: %v)", dferr.ClassOf(err), err)
	}
}

// TestPlugin_SignatureIsCheckedBeforeItIsWritten is the S7 self-verification.
//
// Validation used to be "non-empty, and decodes as base64", so a plugin
// returning base64 of "not-a-signature" gave Sign err=nil and the engine wrote
// the bundle as SIGNED. The next thing to look at it is `debark verify` on
// the far side of the air gap, on a medium that has already been couriered -
// and by then a failing signature reads as tampering rather than as the broken
// plugin it is. key_info had already handed the host the public key needed to
// catch it in the same second it happened.
func TestPlugin_SignatureIsCheckedBeforeItIsWritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("bytes that are not a signature", func(t *testing.T) {
		s := mustStartHelperPlugin(t, ctx, "serve:garbage-signature")
		sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("canonical bytes"))
		if err == nil {
			t.Fatalf("Sign reported success for a signature that is not a signature: %+v", sig)
		}
		if !strings.Contains(err.Error(), "does not verify") {
			t.Errorf("the error does not say what was checked: %v", err)
		}
	})

	t.Run("a signature over the wrong bytes", func(t *testing.T) {
		// The plugin signs the payload but drops the purpose, so the signature
		// is real, made by the right key, and over the wrong message. v1's
		// protocol doc says a plugin "must incorporate it or refuse"; this is
		// where the host stops taking its word for it.
		s := mustStartHelperPlugin(t, ctx, "serve:no-domain-separation")
		if sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("canonical bytes")); err == nil {
			t.Fatalf("Sign accepted a signature that omits the domain separation: %+v", sig)
		}
	})

	t.Run("a signature attributed to the wrong key", func(t *testing.T) {
		s := mustStartHelperPlugin(t, ctx, "serve:wrong-key-id")
		sig, err := s.Sign(ctx, manifest.SignPurpose, []byte("canonical bytes"))
		if err == nil {
			t.Fatalf("Sign accepted a block naming a key that did not sign it: %+v", sig)
		}
		if !strings.Contains(err.Error(), helperKeyID()) {
			t.Errorf("the error does not name the id the exported key actually has: %v", err)
		}
	})

	t.Run("an honest plugin is unaffected", func(t *testing.T) {
		// The control. Every refusal above has to leave the working case
		// working, or it is just a broken plugin path.
		s := mustStartHelperPlugin(t, ctx, "serve:good")
		canon := []byte("canonical bytes")
		sig, err := s.Sign(ctx, manifest.SignPurpose, canon)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := verifyEd25519Signature(helperTrustSet(), manifest.SignPurpose, canon, sig); err != nil {
			t.Fatalf("the checked-and-accepted signature does not verify: %v", err)
		}
	})
}

// TestPlugin_StderrIsRedacted: a plugin's stderr is surfaced verbatim in the
// error an operator sees, and errors travel - into CI output, into a pasted
// bug report. A signing plugin is exactly the kind of program that logs a
// bearer token while failing to reach its HSM, so the shapes a secret takes
// are scrubbed on the way in. What must survive is everything that makes the
// diagnostics worth keeping.
func TestPlugin_StderrIsRedacted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := startHelperPlugin(t, ctx, "stderr-with-secrets"); err == nil {
		t.Fatal("SignerFor accepted a plugin that never handshook")
	} else {
		hint := dferr.HintOf(err)
		for _, secret := range []string{"s3cr3t-Wq8xLm2ZpR7bNv4kTjHy", "AKIA7NQ4XZLMPD3RTVWB9CEG"} {
			if strings.Contains(hint, secret) {
				t.Errorf("the plugin's credential was reprinted in a user-facing error:\n%s", hint)
			}
		}
		// The diagnostics themselves have to survive, or the redaction has
		// simply destroyed the operator's only clue.
		for _, keep := range []string{"kms.example", "/etc/debark/plugin.conf", "auth_token="} {
			if !strings.Contains(hint, keep) {
				t.Errorf("redaction removed %q, which is diagnostics and not a secret:\n%s", keep, hint)
			}
		}
	}
}

// TestRedactStderrLine covers the scrub directly, including what it must NOT
// touch: over-redaction is safe for a secret and expensive for a diagnostic,
// so the line is drawn at "a long opaque run of token characters" and "the
// value of a secret-shaped key", and paths, versions and prose stay.
func TestRedactStderrLine(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		gone  string // must not appear in the output
		stays string // must appear in the output
	}{
		{
			name:  "assignment with a short value",
			in:    "connecting with password=hunter2",
			gone:  "hunter2",
			stays: "password=",
		},
		{
			name:  "assignment whose value is the next field",
			in:    "Authorization: Bearer abc123",
			gone:  "abc123",
			stays: "Authorization:",
		},
		{
			name:  "a bare long token",
			in:    "session resumed as Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA",
			gone:  "Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA",
			stays: "session resumed",
		},
		{
			name:  "a long path is not a secret",
			in:    "reading /var/lib/debark7/plugins/acme-hsm/config2.toml",
			gone:  "\x00", // nothing must be removed
			stays: "/var/lib/debark7/plugins/acme-hsm/config2.toml",
		},
		{
			// Digits and all, and long enough to look like a token if the
			// path exemption were not there.
			name:  "a URL is not a secret",
			in:    "POST https://kms7.example.com/v1/keys/sign failed",
			gone:  "\x00",
			stays: "https://kms7.example.com/v1/keys/sign",
		},
		{
			// Base64 with a '/' in it is still a secret: excluding the slash
			// from the token rule would let most real ones through.
			name:  "base64 containing a slash",
			in:    "signing with Zm9vYmFy/YmF6cXV4MTIzNDU2",
			gone:  "Zm9vYmFy/YmF6cXV4MTIzNDU2",
			stays: "signing with",
		},
		{
			name:  "ordinary prose survives",
			in:    "helper: cannot reach the signing device",
			gone:  "\x00",
			stays: "cannot reach the signing device",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactStderrLine(tc.in)
			if tc.gone != "\x00" && strings.Contains(got, tc.gone) {
				t.Errorf("redactStderrLine(%q) = %q, still holds %q", tc.in, got, tc.gone)
			}
			if !strings.Contains(got, tc.stays) {
				t.Errorf("redactStderrLine(%q) = %q, lost %q", tc.in, got, tc.stays)
			}
		})
	}

	// A very long line is truncated as well as scrubbed: an unbounded one is
	// its own denial-of-readability problem in an error message.
	long := redactStderrLine(strings.Repeat("x", maxStderrLineLen*3))
	if len(long) > maxStderrLineLen+32 {
		t.Errorf("a %d-byte stderr line was retained at %d bytes", maxStderrLineLen*3, len(long))
	}
}
