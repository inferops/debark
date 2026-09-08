package sign

// Helper-process plumbing for core/sign's two out-of-process clients.
//
// The plugin client (plugin.go) and the gpg client (gpg.go) both drive a real
// child process, and everything interesting about them is what happens when
// that child misbehaves: it prints no handshake, it answers with the wrong id,
// it never exits, gpg reports BADSIG. None of that can be reached by calling
// into the package - it needs a child that actually does the wrong thing.
//
// Shipping a shell script per case is not portable (this repo builds and tests
// on Windows) and compiling a throwaway Go program per case costs seconds each
// - the two existing plugin tests in sign_test.go each pay a full `go build`.
// So the test binary is its own helper: TestMain re-enters as the child when
// one of the env vars below is set and never runs a single test in that mode.
//
// newPluginSigner spawns the plugin with exec.Command(path) - no argv of its
// own, no cmd.Env - so the inherited environment is the only channel the
// parent has to steer the child. That is why behaviour is selected by env var
// rather than by the usual `-test.run=TestHelperProcess -- args` idiom.
//
// The helper signers below hold a REAL ed25519 key derived from a fixed seed,
// and the parent derives the same public key independently. Every assertion
// about a signature the helper produced is checked with crypto against that
// key rather than against the helper's own say-so: a fake that can only ever
// agree with the test proves nothing.

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/inferops/debark/api/plugin/v1"
	"github.com/inferops/debark/core/manifest"
)

const (
	// envPluginBehaviour selects the misbehaving-plugin script; see
	// pluginHelperMain.
	envPluginBehaviour = "DEBARK_TEST_PLUGIN"
	// envPluginHeartbeat names a file the helper appends one byte to every
	// few milliseconds for as long as it is alive. It is how a test proves a
	// child process was actually reaped rather than leaked: a dead process
	// stops growing the file.
	envPluginHeartbeat = "DEBARK_TEST_PLUGIN_HEARTBEAT"
	// envGPGScript carries a JSON gpgStubScript telling the helper how to
	// impersonate gpg.
	envGPGScript = "DEBARK_TEST_GPG"
)

// pluginHelperName is the plugin name the helper announces; Kind() must come
// back as "plugin:" + this.
const pluginHelperName = "debark-test-helper"

// helperSeed is a fixed ed25519 seed. Both the child and the parent derive the
// key pair from it, so the parent can verify the child's signatures with
// crypto instead of trusting the child's report of its own behaviour.
var helperSeed = bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)

func helperPrivateKey() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(helperSeed) }
func helperPublicKey() ed25519.PublicKey {
	return helperPrivateKey().Public().(ed25519.PublicKey)
}

// helperKeyID derives the key id exactly the way keyformat.go does, so a
// signature the helper produced is addressable in a trust map built from a
// debark-native .pub file holding the same key.
func helperKeyID() string {
	sum := sha256.Sum256(helperPublicKey())
	return hex.EncodeToString(sum[:keyIDLen])
}

func TestMain(m *testing.M) {
	if b := os.Getenv(envPluginBehaviour); b != "" {
		pluginHelperMain(b) // never returns
	}
	if s := os.Getenv(envGPGScript); s != "" {
		gpgHelperMain(s) // never returns
	}
	os.Exit(m.Run())
}

// testExecutable is the path to this test binary, which is also the path every
// helper-process test hands to SignerFor as "plugin:<path>".
func testExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// --- the misbehaving plugin -------------------------------------------------

// helperWatchdog guarantees no helper process outlives the test run by more
// than a minute, whatever the host does or fails to do. A child nobody reaps
// is precisely the defect these tests hunt for; it must never become a defect
// of the test suite itself. The window is far longer than any timeout under
// test (the longest is Close's 5s kill escalation), so it cannot mask a result.
func helperWatchdog() {
	go func() {
		time.Sleep(60 * time.Second)
		fmt.Fprintln(os.Stderr, "helper: watchdog expiry, exiting")
		os.Exit(90)
	}()
}

// startHeartbeat, when envPluginHeartbeat is set, appends one byte to that file
// every 20ms forever. It runs in a goroutine so the helper's main behaviour is
// unaffected; the file stops growing the instant the process dies.
func startHeartbeat() {
	path := os.Getenv(envPluginHeartbeat)
	if path == "" {
		return
	}
	go func() {
		for {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.Write([]byte{'.'})
				_ = f.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// pluginHelperMain implements every plugin behaviour the tests need. The
// behaviour string is "<name>" or "serve:<mode>"; the serve modes are handled
// by pluginHelperServe below.
func pluginHelperMain(behaviour string) {
	helperWatchdog()
	startHeartbeat()

	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: marshal:", err)
			os.Exit(71)
		}
		_, _ = out.Write(b)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}

	name, mode := behaviour, ""
	if i := strings.IndexByte(behaviour, ':'); i >= 0 {
		name, mode = behaviour[:i], behaviour[i+1:]
	}

	switch name {
	case "silent-exit":
		// Writes nothing at all and exits: the host's very first read of the
		// handshake sees EOF.
		os.Exit(0)

	case "stderr-then-exit":
		fmt.Fprintln(os.Stderr, "helper: cannot reach the signing device")
		fmt.Fprintln(os.Stderr, "helper: giving up")
		os.Exit(3)

	case "stderr-with-secrets":
		// A plugin that logs its own credentials while failing - which is
		// exactly what a signing plugin talking to a cloud KMS or an HSM
		// gateway does when the call fails. Every one of these lines used to
		// be reprinted verbatim in the error the operator (and their CI log)
		// sees.
		fmt.Fprintln(os.Stderr, "helper: POST https://kms.example/sign failed")
		fmt.Fprintln(os.Stderr, "helper: auth_token=s3cr3t-Wq8xLm2ZpR7bNv4kTjHy")
		fmt.Fprintln(os.Stderr, "helper: retrying with AKIA7NQ4XZLMPD3RTVWB9CEG")
		fmt.Fprintln(os.Stderr, "helper: config at /etc/debark/plugin.conf line 12")
		os.Exit(3)

	case "flood-stderr":
		// More stderr than maxStderrLines, so the retention cap is exercised
		// and the LAST lines - the ones nearest the failure - are the ones
		// kept.
		for i := 0; i < maxStderrLines*3; i++ {
			fmt.Fprintf(os.Stderr, "helper noise line %d\n", i)
		}
		os.Exit(4)

	case "not-json":
		fmt.Println("this line is not JSON at all")

	case "handshake-not-an-object":
		// Valid JSON, wrong shape: an array where an object is required.
		fmt.Println(`["debark.plugin/v1","helper"]`)

	case "truncated-handshake":
		// A complete-looking handshake with NO terminating newline, then exit
		// immediately. bufio.ReadString('\n') hands back the partial line
		// together with io.EOF; accepting it would mean trusting a handshake
		// the plugin never finished writing.
		//
		// The exit is load-bearing, and the reason is a real defect: the host
		// reads the handshake with a blocking ReadString that consults no
		// deadline and not the caller's context (plugin.go:82). A child that
		// wrote a partial line and then stayed alive would wedge `debark
		// build` forever rather than fail, so this helper must not model that
		// case - see the note above TestPlugin_HandshakeRejections.
		fmt.Print(`{"protocol":"debark.plugin/v1","name":"helper","capabilities":["sign"]}`)
		os.Exit(0)

	case "wrong-protocol":
		emit(v1.Handshake{Protocol: "debark.plugin/v2", Name: pluginHelperName, Version: "0.0.1", Capabilities: []string{v1.CapabilitySign}})

	case "no-sign-capability":
		emit(v1.Handshake{Protocol: v1.Protocol, Name: pluginHelperName, Version: "0.0.1", Capabilities: []string{"key_info"}})

	case "no-capabilities":
		emit(v1.Handshake{Protocol: v1.Protocol, Name: pluginHelperName, Version: "0.0.1"})

	case "empty-name":
		emit(v1.Handshake{Protocol: v1.Protocol, Version: "0.0.1", Capabilities: []string{v1.CapabilitySign}})

	case "serve":
		emit(v1.Handshake{Protocol: v1.Protocol, Name: pluginHelperName, Version: "0.0.1", Capabilities: []string{v1.CapabilitySign}})
		pluginHelperServe(mode, emit) // never returns

	default:
		fmt.Fprintf(os.Stderr, "helper: unknown plugin behaviour %q\n", behaviour)
		os.Exit(70)
	}

	// The behaviours above that printed something the host must reject hold
	// still until stdin closes, so what the test observes is the handshake
	// rejection itself and not an incidental broken pipe. A host that forgot
	// to reap us would leave this process here - which is exactly what the
	// heartbeat file lets a test detect.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// pluginHelperServe answers requests after a well-formed handshake. Every mode
// is one specific way a signer can fail after the connection looked healthy.
func pluginHelperServe(mode string, emit func(any)) {
	priv := helperPrivateKey()
	pub := priv.Public().(ed25519.PublicKey)
	keyID := helperKeyID()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req v1.Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Method {
		case v1.MethodKeyInfo:
			switch mode {
			case "hang", "hang-key-info":
				// Deliberately no reply: the host must time out rather than
				// block forever.
			case "no-key-info":
				emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrUnsupportedMethod, Message: "this helper does not implement key_info"}})
			case "malformed-key-info":
				emit(v1.Response{ID: req.ID, Result: "a bare string, which is not a key_info object"})
			default:
				emit(v1.Response{ID: req.ID, Result: v1.KeyInfoResult{
					KeyID:     keyID,
					Algorithm: AlgorithmEd25519,
					PublicKey: base64.StdEncoding.EncodeToString(pub),
					Comment:   "core/sign test helper key",
				}})
			}

		case v1.MethodSign:
			pluginHelperSign(mode, req, priv, keyID, emit)

		case v1.MethodShutdown:
			if mode == "ignore-shutdown" {
				// Acknowledge, then refuse to die. The host must fall back to
				// killing us; if it does not, the heartbeat keeps ticking and
				// the test fails.
				emit(v1.Response{ID: req.ID, Result: map[string]any{}})
				helperSleepForever()
			}
			emit(v1.Response{ID: req.ID, Result: map[string]any{}})
			os.Exit(0)

		default:
			emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrUnsupportedMethod, Message: "unknown method " + req.Method}})
		}
	}
	if mode == "ignore-shutdown" {
		// stdin closed and we still will not exit.
		helperSleepForever()
	}
	os.Exit(0)
}

// helperSleepForever blocks the calling goroutine forever without ever letting
// the Go runtime declare an all-goroutines-asleep deadlock, which a bare
// select{} would risk when the heartbeat goroutine is not running.
func helperSleepForever() {
	for {
		time.Sleep(time.Second)
	}
}

func pluginHelperSign(mode string, req v1.Request, priv ed25519.PrivateKey, keyID string, emit func(any)) {
	var params v1.SignParams
	if err := remarshal(req.Params, &params); err != nil {
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "malformed sign params"}})
		return
	}
	payload, err := base64.StdEncoding.DecodeString(params.Payload)
	if err != nil {
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "payload is not base64"}})
		return
	}
	honest := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningInput(params.Purpose, payload)))

	result := v1.SignResult{Signature: honest, Algorithm: AlgorithmEd25519, KeyID: keyID}

	switch mode {
	case "hang", "hang-sign":
		return // no reply at all

	case "wrong-id":
		// The signature itself is perfectly valid - the ONLY thing wrong is
		// that it answers a different id. A host that matched replies
		// positionally instead of by id would accept this and produce a
		// "signed" manifest from an uncorrelated answer.
		emit(v1.Response{ID: req.ID + "-not-the-id-you-asked-for", Result: result})
		return

	case "no-domain-separation":
		// A plugin that signs the raw payload and drops the purpose. v1's
		// protocol doc says a plugin "must incorporate it or refuse"; the host
		// does not enforce that, so this exists to show what the resulting
		// block does (and does not) do at verify time.
		result.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))

	case "empty-signature":
		result.Signature = ""

	case "non-base64-signature":
		result.Signature = "@@@ this is not base64 @@@"

	case "garbage-signature":
		// Well-formed base64 of bytes that are not a signature at all.
		result.Signature = base64.StdEncoding.EncodeToString([]byte("not a signature, just some bytes"))

	case "short-signature":
		result.Signature = base64.StdEncoding.EncodeToString([]byte("tooshort"))

	case "malformed-sign-result":
		emit(v1.Response{ID: req.ID, Result: "a bare string, which is not a sign result"})
		return

	case "no-key-id":
		result.KeyID = ""

	case "claims-native-kind":
		// A plugin asserting it is the native ed25519-file signer. The host
		// states the kind itself and must ignore this.
		result.SignerKind = manifest.SignerEd25519File

	case "hostile-fields":
		// The whole shape of a hostile plugin's answer: it labels itself gpg,
		// and names its key with a string that is not a key id in any format
		// debark defines. Both used to be copied into the signature block
		// verbatim, so the bundle claimed a gpg release key.
		result.SignerKind = manifest.SignerGPG
		result.KeyID = "gpg key 0xDEADBEEF (release)"

	case "wrong-key-id":
		// A perfectly valid signature, attributed to a key id that is not the
		// one key_info exported. The block could never be matched to the key
		// that made it.
		result.KeyID = "00112233445566ff"

	case "error-unsupported":
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrUnsupportedMethod, Message: "sign is not available here"}})
		return
	case "error-key-unavailable":
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrKeyUnavailable, Message: "the smartcard is not inserted"}})
		return
	case "error-user-declined":
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrUserDeclined, Message: "the operator declined the signing prompt"}})
		return
	case "error-internal":
		emit(v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "internal helper failure"}})
		return

	case "exit-on-sign":
		// Crash instead of answering: the host's pending call must fail fast
		// rather than wait out the whole timeout.
		fmt.Fprintln(os.Stderr, "helper: crashing instead of signing")
		os.Exit(9)
	}

	emit(v1.Response{ID: req.ID, Result: result})
}

// --- the gpg stub -----------------------------------------------------------

// gpgStubOp is one canned gpg invocation result.
type gpgStubOp struct {
	Stdout    string `json:"stdout,omitempty"`
	StdoutB64 string `json:"stdout_b64,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Exit      int    `json:"exit,omitempty"`
}

// gpgStubScript is the whole instruction set handed to the stub over
// envGPGScript. The stub picks List/Sign/Verify by looking at its own argv,
// exactly as real gpg would dispatch on its flags.
type gpgStubScript struct {
	// ArgsFile, when set, is appended with one NUL-joined argv line per
	// invocation. Tests read it to prove the stub was actually reached and
	// that the flags core/sign promises (--batch, --detach-sign, the keyring
	// selection) were really passed. Without it a stub that silently did
	// nothing could satisfy a "no error" assertion.
	ArgsFile string `json:"args_file,omitempty"`

	List   gpgStubOp `json:"list"`
	Sign   gpgStubOp `json:"sign"`
	Verify gpgStubOp `json:"verify"`

	// RealSign makes the stub produce a genuine ed25519 signature over
	// exactly the bytes it was handed on stdin, with the helper key. That is
	// what gives the gpg domain-separation test teeth: the parent verifies the
	// result against SigningInput(purpose, canonical), so a gpgSigner that
	// stopped mixing the purpose in produces a signature the parent cannot
	// verify.
	RealSign bool `json:"real_sign,omitempty"`
	// RealVerify makes the stub check the detached signature file against the
	// data file with the helper key and emit VALIDSIG or BADSIG accordingly -
	// a real cryptographic answer, not a canned one.
	RealVerify bool `json:"real_verify,omitempty"`
	// VerifyFingerprint is the fingerprint reported on the VALIDSIG line.
	VerifyFingerprint string `json:"verify_fingerprint,omitempty"`
}

func gpgHelperMain(script string) {
	var s gpgStubScript
	if err := json.Unmarshal([]byte(script), &s); err != nil {
		fmt.Fprintln(os.Stderr, "gpg stub: bad script:", err)
		os.Exit(70)
	}
	args := os.Args[1:]

	if s.ArgsFile != "" {
		if f, err := os.OpenFile(s.ArgsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString(strings.Join(args, "\x00") + "\n")
			_ = f.Close()
		}
	}

	has := func(flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}

	switch {
	case has("--list-secret-keys"):
		gpgStubEmit(s.List)
	case has("--detach-sign"):
		if s.RealSign {
			stdin, _ := io.ReadAll(os.Stdin)
			_, _ = os.Stdout.Write(ed25519.Sign(helperPrivateKey(), stdin))
			os.Exit(s.Sign.Exit)
		}
		gpgStubEmit(s.Sign)
	case has("--verify"):
		if s.RealVerify {
			gpgStubRealVerify(s, args)
		}
		gpgStubEmit(s.Verify)
	default:
		fmt.Fprintf(os.Stderr, "gpg stub: unrecognised invocation %v\n", args)
		os.Exit(2)
	}
	os.Exit(0)
}

func gpgStubEmit(op gpgStubOp) {
	if op.StdoutB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(op.StdoutB64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gpg stub: bad stdout_b64:", err)
			os.Exit(70)
		}
		_, _ = os.Stdout.Write(raw)
	} else if op.Stdout != "" {
		_, _ = io.WriteString(os.Stdout, op.Stdout)
	}
	if op.Stderr != "" {
		_, _ = io.WriteString(os.Stderr, op.Stderr)
	}
	os.Exit(op.Exit)
}

// gpgStubRealVerify does what gpg does: check the detached signature in the
// second-to-last argument against the data in the last one, and report the
// outcome on the status fd (stdout, because core/sign passes --status-fd 1).
func gpgStubRealVerify(s gpgStubScript, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "gpg stub: --verify needs a signature and a data file")
		os.Exit(2)
	}
	sigPath, dataPath := args[len(args)-2], args[len(args)-1]
	sig, err1 := os.ReadFile(sigPath)
	data, err2 := os.ReadFile(dataPath)
	if err1 != nil || err2 != nil {
		fmt.Fprintln(os.Stderr, "gpg stub: cannot read verify inputs:", err1, err2)
		os.Exit(2)
	}
	fpr := s.VerifyFingerprint
	if len(fpr) < 16 {
		fpr = strings.ToUpper(strings.Repeat("ab", 20))
	}
	if ed25519.Verify(helperPublicKey(), data, sig) {
		fmt.Printf("[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG %s helper key\n[GNUPG:] VALIDSIG %s 2026-01-01 0 4 0 22 8 00 %s\n", fpr[:16], fpr, fpr)
		fmt.Fprintln(os.Stderr, "gpg: Good signature from \"helper key\"")
		os.Exit(0)
	}
	fmt.Printf("[GNUPG:] NEWSIG\n[GNUPG:] BADSIG %s helper key\n", fpr[:16])
	fmt.Fprintln(os.Stderr, "gpg: BAD signature from \"helper key\"")
	os.Exit(1)
}

// useGPGStub points core/sign's gpg client at this test binary and restores
// the real binary name afterwards. It returns the path of the argv log, which
// callers read to confirm the stub was genuinely invoked.
func useGPGStub(t *testing.T, s gpgStubScript) string {
	t.Helper()
	argsFile := s.ArgsFile
	if argsFile == "" {
		argsFile = filepath.Join(t.TempDir(), "gpg-args.log")
		s.ArgsFile = argsFile
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal gpg stub script: %v", err)
	}
	t.Setenv(envGPGScript, string(b))

	prev := gpgBinary
	gpgBinary = testExecutable(t)
	t.Cleanup(func() { gpgBinary = prev })
	return argsFile
}

// gpgStubInvocations returns one []string of argv per stub invocation.
func gpgStubInvocations(t *testing.T, argsFile string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, "\x00"))
	}
	return out
}
