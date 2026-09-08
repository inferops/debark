package sign

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/inferops/debark/api/plugin/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
)

// defaultPluginTimeout bounds one request when the caller's context carries
// no deadline of its own.
const defaultPluginTimeout = 60 * time.Second

// maxStderrLines bounds how much of a plugin's stderr is retained for
// diagnostics; it is never parsed, only surfaced to the operator on failure.
const maxStderrLines = 50

// maxStderrLineLen truncates one retained stderr line. A plugin's stderr is
// text debark does not control that ends up verbatim in an operator-facing
// error - and from there in a CI log or a bug report - so it is bounded in
// both directions (how many lines, how long each) before it is shown.
const maxStderrLineLen = 512

// maxPluginFieldLen bounds the key_id and algorithm strings a plugin can put
// into a signature block. debark's own ids are 16 and 40 hex characters; 128
// leaves room for a longer identifier from some future signer without leaving
// room for a payload.
const maxPluginFieldLen = 128

// pluginSigner is the debark.plugin/v1 client: it spawns the plugin
// executable, reads its handshake, and speaks single-line-JSON request/
// response over stdio for the sign and key_info methods.
type pluginSigner struct {
	path string
	cmd  *exec.Cmd
	name string

	stdin      io.WriteCloser
	readerDone chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan v1.Response

	stderrMu    sync.Mutex
	stderrLines []string

	keyMu   sync.Mutex
	keyInfo *v1.KeyInfoResult // nil until key_info succeeds or a Sign call fills it in
}

func newPluginSigner(ctx context.Context, path string) (*pluginSigner, error) {
	if path == "" {
		return nil, dferr.New(dferr.Usage, "sign: plugin: empty plugin path")
	}
	// Running `path` IS the feature: --signer plugin:<path> is the operator
	// naming an executable to sign with (api/plugin/v1), so the path arriving
	// from the command line is the input, not an injection. gosec's G702
	// traces it back to argv and flags it anyway.
	//
	// What matters for safety is the shape, and the shape is right: a bare
	// exec.Command with a program name and no arguments at all, never a shell
	// string -- so there is nothing for a crafted path to break out of. A
	// path that is not an executable fails at cmd.Start below with a usable
	// error. An operator who can pass --signer can already run anything as
	// themselves; debark is not a privilege boundary here.
	// #nosec G702 -- operator-chosen signer binary, no shell, no argv
	cmd := exec.Command(path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, wrapErr(dferr.Environment, err, "sign: plugin: stdin pipe")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, wrapErr(dferr.Environment, err, "sign: plugin: stdout pipe")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, wrapErr(dferr.Environment, err, "sign: plugin: stderr pipe")
	}
	if err := cmd.Start(); err != nil {
		return nil, wrapErr(dferr.Environment, err, "sign: plugin: start %s", path).
			WithHint("check the plugin path is executable")
	}

	p := &pluginSigner{
		path:       path,
		cmd:        cmd,
		stdin:      stdin,
		pending:    make(map[string]chan v1.Response),
		readerDone: make(chan struct{}),
	}
	go p.collectStderr(stderr)

	reader := bufio.NewReaderSize(stdout, 64*1024)
	hsLine, err := reader.ReadString('\n')
	if err != nil {
		p.killAfterFailedStart()
		return nil, wrapErr(dferr.Environment, err, "sign: plugin: read handshake from %s", path).
			WithHint("%s", p.diagnosticsHint())
	}
	var hs v1.Handshake
	if err := json.Unmarshal([]byte(strings.TrimSpace(hsLine)), &hs); err != nil {
		p.killAfterFailedStart()
		return nil, wrapErr(dferr.Usage, err, "sign: plugin: malformed handshake from %s", path).
			WithHint("%s", p.diagnosticsHint())
	}
	if hs.Protocol != v1.Protocol {
		p.killAfterFailedStart()
		return nil, dferr.New(dferr.Usage, "sign: plugin: %s speaks protocol %q, want %q", path, hs.Protocol, v1.Protocol)
	}
	caps := make(map[string]bool, len(hs.Capabilities))
	for _, c := range hs.Capabilities {
		caps[c] = true
	}
	if !caps[v1.CapabilitySign] {
		p.killAfterFailedStart()
		return nil, dferr.New(dferr.Usage, "sign: plugin: %s did not announce the %q capability", path, v1.CapabilitySign)
	}
	if hs.Name == "" {
		p.killAfterFailedStart()
		return nil, dferr.New(dferr.Usage, "sign: plugin: %s: handshake is missing a name", path)
	}
	// The announced name is now the whole of the signer_kind recorded in every
	// block this plugin signs ("plugin:<name>"), so it has to be a name and
	// not a sentence, a newline, or a JSON fragment - it is written into
	// debark.manifest.sig and read back by verify.
	if err := validPluginField("handshake name", hs.Name); err != nil {
		p.killAfterFailedStart()
		return nil, dferr.New(dferr.Usage, "sign: plugin: %s: %v", path, err)
	}
	p.name = hs.Name

	go p.readLoop(reader)

	if ki, err := p.keyInfoRequest(ctx); err == nil {
		p.keyMu.Lock()
		p.keyInfo = ki
		p.keyMu.Unlock()
	}
	// A plugin that does not implement key_info is not fatal: KeyID() reports
	// "" until the first successful Sign call fills the cache in.

	return p, nil
}

func (p *pluginSigner) killAfterFailedStart() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}

func (p *pluginSigner) collectStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := redactStderrLine(sc.Text())
		p.stderrMu.Lock()
		p.stderrLines = append(p.stderrLines, line)
		if len(p.stderrLines) > maxStderrLines {
			p.stderrLines = p.stderrLines[len(p.stderrLines)-maxStderrLines:]
		}
		p.stderrMu.Unlock()
	}
}

func (p *pluginSigner) diagnosticsHint() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	if len(p.stderrLines) == 0 {
		return "the plugin wrote nothing to stderr"
	}
	return "plugin stderr:\n" + strings.Join(p.stderrLines, "\n")
}

// redactStderrLine bounds and scrubs one line of plugin stderr on the way in,
// before it is retained at all.
//
// The line is about to become part of an error an operator sees, and errors
// travel: into CI output, into a pasted bug report, into whatever the caller
// logs. A signing plugin is exactly the kind of program that logs a bearer
// token, a smartcard PIN prompt echo or an API key while failing to reach its
// HSM, and debark then reprinted all of it verbatim - up to fifty lines of
// it. The diagnostics are worth keeping (they are the operator's only clue
// about a plugin that will not start), so they are kept, minus the shapes a
// secret takes:
//
//   - a long unbroken run of token characters, which is what a key, a JWT or
//     a base64 blob looks like and which almost nothing else in a log message
//     does. Paths and URLs are exempted from that rule explicitly (they are
//     the most useful thing in a diagnostic and they are long), rather than by
//     excluding '/' from the run - a base64 secret is about as likely to
//     contain a '/' as not, so excluding it would let most of them through.
//   - anything after a secret-shaped assignment ("token=", "password: "),
//     however short.
//
// This is best effort and is documented as such: it cannot recognise a short
// secret printed on its own ("hunter2"), and it never will. That is the other
// reason the retention bound exists.
func redactStderrLine(line string) string {
	if len(line) > maxStderrLineLen {
		line = line[:maxStderrLineLen] + " [truncated]"
	}
	fields := strings.Fields(line)
	out := make([]string, 0, len(fields))
	for i, f := range fields {
		key, val, isAssignment := splitAssignment(f)
		switch {
		case isAssignment && isSecretKeyword(key):
			if val != "" {
				// "token=abc": the name is diagnostics, the value is not.
				out = append(out, f[:len(key)+1]+"REDACTED")
				continue
			}
			// "Authorization:" with the value in the fields that follow. Where
			// such a value ENDS cannot be known ("Bearer abc.def.ghi" is three
			// fields to a human and one credential), so everything after the
			// name goes.
			out = append(out, f)
			if i+1 < len(fields) {
				out = append(out, "REDACTED")
				return strings.Join(out, " ")
			}
		case looksLikeOpaqueToken(f):
			out = append(out, "REDACTED")
		default:
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// secretKeywords are the field names whose VALUE is redacted wherever one
// appears as "<keyword><sep>". Matched case-insensitively against the part
// before the separator, so "AUTH_TOKEN=" and "api-key:" both hit.
var secretKeywords = []string{"token", "secret", "password", "passwd", "passphrase", "pin", "key", "credential", "auth", "authorization", "bearer", "session", "cookie"}

func splitAssignment(f string) (key, val string, ok bool) {
	i := strings.IndexAny(f, "=:")
	if i < 0 {
		return "", "", false
	}
	return f[:i], f[i+1:], true
}

// isSecretKeyword matches only the NAME half of an assignment, never a bare
// word in prose: "the session key is unavailable" is a diagnostic, and a rule
// that fired on the word "key" alone would eat most of what a plugin says
// while it fails.
func isSecretKeyword(key string) bool {
	lower := strings.ToLower(key)
	for _, kw := range secretKeywords {
		// Suffix rather than equality so "api_key", "x-auth-token" and
		// "--passphrase" all match the keyword they end with.
		if strings.HasSuffix(lower, kw) {
			return true
		}
	}
	return false
}

// looksLikePathOrURL exempts the two long strings a plugin's diagnostics are
// actually made of: an absolute path (POSIX or Windows) and a URL. A relative
// path is not exempted - there is nothing to recognise it by that a token
// does not also have - so a long one may be redacted, which is the direction
// this errs in on purpose.
func looksLikePathOrURL(f string) bool {
	if strings.HasPrefix(f, "/") || strings.HasPrefix(f, `\`) || strings.Contains(f, "://") {
		return true
	}
	// "C:\keys\op.key" and "C:/keys/op.key".
	return len(f) > 2 && f[1] == ':' && (f[2] == '\\' || f[2] == '/')
}

// minOpaqueTokenLen is where "a long meaningless string" starts. Short enough
// to catch a 32-hex key id or a 24-character API key, long enough that
// ordinary English words, version numbers and short flags are never touched.
const minOpaqueTokenLen = 24

func looksLikeOpaqueToken(f string) bool {
	if len(f) < minOpaqueTokenLen || looksLikePathOrURL(f) {
		return false
	}
	digits, letters := false, false
	for _, r := range f {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letters = true
		case strings.ContainsRune("+/=_-.", r):
			// Padding and separators a token may hold. Note the deliberate
			// absence of the path separators: a run containing one is a path,
			// and a path is diagnostics, not a secret.
		default:
			return false
		}
	}
	// Both classes present: a long run of only letters is a sentence with the
	// spaces eaten or a hostname, and a long run of only digits is a
	// timestamp or a size.
	return digits && letters
}

// readLoop dispatches one response line at a time to the pending call that
// requested it, by id, until stdout closes (the plugin exited or crashed).
func (p *pluginSigner) readLoop(r *bufio.Reader) {
	defer close(p.readerDone)
	for {
		line, err := r.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var resp v1.Response
			if jerr := json.Unmarshal([]byte(trimmed), &resp); jerr == nil && resp.ID != "" {
				p.dispatch(resp)
			}
		}
		if err != nil {
			p.failAllPending()
			return
		}
	}
}

func (p *pluginSigner) dispatch(resp v1.Response) {
	p.mu.Lock()
	ch, ok := p.pending[resp.ID]
	if ok {
		delete(p.pending, resp.ID)
	}
	p.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// failAllPending closes every outstanding call's channel so it returns
// immediately (with ok=false) instead of hanging until its timeout.
func (p *pluginSigner) failAllPending() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		delete(p.pending, id)
		close(ch)
	}
}

func remarshal(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func pluginError(name, method string, e *v1.Error) error {
	if e == nil {
		return dferr.New(dferr.Environment, "sign: plugin %s: %s failed with no error detail", name, method)
	}
	switch e.Code {
	case v1.ErrKeyUnavailable:
		return dferr.New(dferr.Environment, "sign: plugin %s: %s", name, e.Message).
			WithHint("check the plugin's key is configured and reachable")
	case v1.ErrUserDeclined:
		return dferr.New(dferr.Usage, "sign: plugin %s: %s", name, e.Message)
	case v1.ErrUnsupportedMethod:
		return dferr.New(dferr.Usage, "sign: plugin %s does not support %q: %s", name, method, e.Message)
	default:
		return dferr.New(dferr.Environment, "sign: plugin %s: %s", name, e.Message)
	}
}

// call sends one request and waits for its matching response, id-correlated,
// bounded by ctx or defaultPluginTimeout when ctx carries no deadline.
func (p *pluginSigner) call(ctx context.Context, method string, params any) (v1.Response, error) {
	p.mu.Lock()
	p.nextID++
	id := strconv.FormatInt(p.nextID, 10)
	ch := make(chan v1.Response, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	req := v1.Request{ID: id, Method: method, Params: params}
	line, err := json.Marshal(req)
	if err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return v1.Response{}, wrapErr(dferr.Usage, err, "sign: plugin: marshal %s request", method)
	}
	line = append(line, '\n')

	callCtx, cancel := ctx, func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		callCtx, cancel = context.WithTimeout(ctx, defaultPluginTimeout)
	}
	defer cancel()

	if _, err := p.stdin.Write(line); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return v1.Response{}, wrapErr(dferr.Environment, err, "sign: plugin: write %s request", method).
			WithHint("%s", p.diagnosticsHint())
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return v1.Response{}, dferr.New(dferr.Environment, "sign: plugin: %s exited before responding to %q", p.name, method).
				WithHint("%s", p.diagnosticsHint())
		}
		return resp, nil
	case <-callCtx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return v1.Response{}, dferr.New(dferr.Environment, "sign: plugin: %s timed out on %q", p.name, method).
			WithHint("%s", p.diagnosticsHint())
	}
}

func (p *pluginSigner) keyInfoRequest(ctx context.Context) (*v1.KeyInfoResult, error) {
	resp, err := p.call(ctx, v1.MethodKeyInfo, nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, pluginError(p.name, v1.MethodKeyInfo, resp.Error)
	}
	var ki v1.KeyInfoResult
	if err := remarshal(resp.Result, &ki); err != nil {
		return nil, wrapErr(dferr.Usage, err, "sign: plugin: malformed key_info result from %s", p.name)
	}
	return &ki, nil
}

func (p *pluginSigner) Kind() string { return manifest.SignerPluginPrefix + p.name }

func (p *pluginSigner) KeyID() string {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if p.keyInfo != nil {
		return p.keyInfo.KeyID
	}
	return ""
}

func (p *pluginSigner) Sign(ctx context.Context, purpose string, canon []byte) (manifest.Signature, error) {
	params := v1.SignParams{
		Purpose: purpose,
		Payload: base64.StdEncoding.EncodeToString(canon),
	}
	resp, err := p.call(ctx, v1.MethodSign, params)
	if err != nil {
		return manifest.Signature{}, err
	}
	if resp.Error != nil {
		return manifest.Signature{}, pluginError(p.name, v1.MethodSign, resp.Error)
	}
	var sr v1.SignResult
	if err := remarshal(resp.Result, &sr); err != nil {
		return manifest.Signature{}, wrapErr(dferr.Usage, err, "sign: plugin: malformed sign result from %s", p.name)
	}
	if sr.Signature == "" {
		return manifest.Signature{}, dferr.New(dferr.Usage, "sign: plugin: %s returned an empty signature", p.name)
	}
	rawSig, err := base64.StdEncoding.DecodeString(sr.Signature)
	if err != nil {
		return manifest.Signature{}, dferr.New(dferr.Usage, "sign: plugin: %s returned a non-base64 signature", p.name)
	}
	if err := validPluginField("key_id", sr.KeyID); err != nil {
		return manifest.Signature{}, dferr.New(dferr.Usage, "sign: plugin: %s returned an unusable key_id: %v", p.name, err)
	}
	if err := validPluginField("algorithm", sr.Algorithm); err != nil {
		return manifest.Signature{}, dferr.New(dferr.Usage, "sign: plugin: %s returned an unusable algorithm: %v", p.name, err)
	}
	if err := p.checkReturnedSignature(purpose, canon, sr, rawSig); err != nil {
		return manifest.Signature{}, err
	}

	p.keyMu.Lock()
	if p.keyInfo == nil {
		p.keyInfo = &v1.KeyInfoResult{KeyID: sr.KeyID, Algorithm: sr.Algorithm}
	}
	p.keyMu.Unlock()

	return manifest.Signature{
		// The kind is the HOST's to state, not the plugin's. sr.SignerKind
		// used to be copied through whenever it was non-empty, so a plugin
		// could label its own output "gpg" or "ed25519-file" and the bundle
		// would claim provenance from a mechanism that never ran - the one
		// field in the block that debark actually knows the truth about,
		// surrendered to the least trusted participant. "plugin:" plus the
		// name announced at the handshake is what happened, and it is what
		// gets recorded. (v1's SignResult.SignerKind is now advisory only;
		// nothing reads it.)
		SignerKind: manifest.SignerPluginPrefix + p.name,
		KeyID:      sr.KeyID,
		Algorithm:  sr.Algorithm,
		CreatedAt:  nowStamp(),
		Signature:  sr.Signature,
	}, nil
}

// checkReturnedSignature verifies the plugin's answer locally, whenever the
// plugin gave the host enough to do so: an ed25519 public key from key_info,
// and an ed25519 signature to check with it.
//
// Validation used to be "non-empty, and decodes as base64", so a plugin
// returning base64 of "not-a-signature" produced Sign err=nil and a bundle
// written and reported as SIGNED. Nothing then checks it until an operator
// runs verify - realistically, on the far side of the air gap, on a medium
// that has already been couriered. The public key was sitting in p.keyInfo
// the whole time.
//
// When the plugin exports no public key (an HSM that will not, which the
// protocol explicitly allows) or signs with an algorithm this package cannot
// check, there is nothing to check against and the signature is taken as
// given - unchanged from before. The check is silent when it cannot run, and
// fatal when it can and fails.
func (p *pluginSigner) checkReturnedSignature(purpose string, canon []byte, sr v1.SignResult, rawSig []byte) error {
	p.keyMu.Lock()
	ki := p.keyInfo
	p.keyMu.Unlock()
	if ki == nil || ki.PublicKey == "" {
		return nil
	}
	if !strings.EqualFold(sr.Algorithm, AlgorithmEd25519) || !strings.EqualFold(ki.Algorithm, AlgorithmEd25519) {
		return nil
	}
	pub, err := base64.StdEncoding.DecodeString(ki.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		// key_info's public key is unusable, so it proves nothing either way.
		// This is not the place to fail the build over it: KeyID() already
		// reports what the plugin said, and a signature that cannot be
		// checked here is caught at verify.
		return nil
	}

	// The key id must be the one this public key derives to, because that is
	// the id the trust set is keyed by (keyformat.go): a block naming any
	// other id can never be matched to the key that actually signed it, so a
	// bundle carrying one is dead on arrival at the air gap.
	if want := keyIDHex(keyIDFromPublic(ed25519.PublicKey(pub))); !strings.EqualFold(sr.KeyID, want) {
		return dferr.New(dferr.Usage, "sign: plugin: %s returned key_id %q for a public key whose id is %s", p.name, sr.KeyID, want).
			WithHint("the plugin's key_info and sign replies disagree about which key signed; a bundle recording that key id could never be verified against the exported key")
	}
	if len(rawSig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(pub), SigningInput(purpose, canon), rawSig) {
		return dferr.New(dferr.Usage, "sign: plugin: %s returned a signature that does not verify against the public key it exported", p.name).
			WithHint("the plugin is not signing purpose||NUL||payload with the key it announced; a bundle written from this would fail verification on the target")
	}
	return nil
}

// validPluginField bounds a string a plugin puts into the signature block.
// Both fields end up in debark.manifest.sig, in evidence.json and in
// verify's report, and both are attacker-chosen when the plugin is hostile or
// merely broken - one observed a plugin returning a key_id with spaces in it,
// which is not a key id in any format debark defines. This is a shape check
// only: it says the value could be a key id or an algorithm name, never that
// it is the right one.
func validPluginField(what, s string) error {
	if s == "" {
		// Refused, not tolerated. Both fields are load-bearing at verify time:
		// the trust set is a map keyed by key id, and a plugin block is only
		// routed to the ed25519 check when it says so in algorithm. A block
		// missing either can never verify, so accepting one here writes a
		// bundle that is certain to fail - and it fails at the air gap,
		// weeks later, reported as a signature problem on a medium that is by
		// then genuinely suspect.
		return fmt.Errorf("%s is empty", what)
	}
	if len(s) > maxPluginFieldLen {
		return fmt.Errorf("%s is %d bytes, want at most %d", what, len(s), maxPluginFieldLen)
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if strings.ContainsRune("-_.:+", r) {
			continue
		}
		return fmt.Errorf("%s contains %q, which no key id or algorithm name may hold", what, r)
	}
	return nil
}

// Close asks the plugin to shut down, closes stdin (the authoritative
// shutdown signal per the protocol doc), and always kills the process if it
// has not exited on its own within a short grace period.
func (p *pluginSigner) Close() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = p.call(shutdownCtx, v1.MethodShutdown, nil)
	cancel()

	_ = p.stdin.Close()

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-done
	}

	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}
