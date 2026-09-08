// Command signer-plugin is a minimal, working example of a
// debark.plugin/v1 "sign" plugin (ADR-009). It exists to prove the
// protocol end to end and to give plugin authors a small, complete program to
// copy from - it is not meant to be a production key-custody tool.
//
// The plugin holds one ed25519 key, generated on first run and persisted to a
// local file so its identity (and key id) is stable across invocations - the
// same "purpose, NUL, payload" domain separation as every other debark
// Signer, using core/sign.SigningInput directly so the two can never drift
// apart.
//
// Run it with no arguments; debark (or the "manual" mode below) spawns and
// talks to it over stdin/stdout. See README.md in this directory for the key
// file location, the manual smoke-test steps, and how to point `debark
// verify` at a bundle this plugin signed.
package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/inferops/debark/api/plugin/v1"
	"github.com/inferops/debark/core/sign"
)

const pluginName = "debark-sample-signer-plugin"
const pluginVersion = "0.1.0"

func main() {
	if err := run(os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "signer-plugin:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer, errOut io.Writer) error {
	priv, err := loadOrCreateKey(errOut)
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	keyID := deriveKeyID(pub)

	w := bufio.NewWriter(out)
	if err := writeLine(w, v1.Handshake{
		Protocol:     v1.Protocol,
		Name:         pluginName,
		Version:      pluginVersion,
		Capabilities: []string{v1.CapabilitySign},
	}); err != nil {
		return fmt.Errorf("write handshake: %w", err)
	}

	fmt.Fprintf(errOut, "%s: ready, key id %s\n", pluginName, keyID)

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20) // signed payloads can be a full manifest
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req v1.Request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(errOut, "%s: malformed request, ignored: %v\n", pluginName, err)
			continue
		}

		resp := handle(req, priv, pub, keyID)
		if err := writeLine(w, resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		if req.Method == v1.MethodShutdown {
			break
		}
	}
	return scanner.Err()
}

func handle(req v1.Request, priv ed25519.PrivateKey, pub ed25519.PublicKey, keyID string) v1.Response {
	switch req.Method {
	case v1.MethodSign:
		return handleSign(req, priv, keyID)
	case v1.MethodKeyInfo:
		return v1.Response{ID: req.ID, Result: v1.KeyInfoResult{
			KeyID:     keyID,
			Algorithm: "ed25519",
			PublicKey: base64.StdEncoding.EncodeToString(pub),
			Comment:   pluginName + " sample key - do not use for anything real",
		}}
	case v1.MethodShutdown:
		return v1.Response{ID: req.ID, Result: map[string]any{}}
	default:
		return v1.Response{ID: req.ID, Error: &v1.Error{
			Code:    v1.ErrUnsupportedMethod,
			Message: "this sample plugin only implements sign, key_info and shutdown",
		}}
	}
}

func handleSign(req v1.Request, priv ed25519.PrivateKey, keyID string) v1.Response {
	var params v1.SignParams
	if err := remarshal(req.Params, &params); err != nil {
		return v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "malformed sign params: " + err.Error()}}
	}
	if params.Purpose == "" {
		return v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "sign request is missing purpose; refusing to sign without domain separation"}}
	}
	payload, err := base64.StdEncoding.DecodeString(params.Payload)
	if err != nil {
		return v1.Response{ID: req.ID, Error: &v1.Error{Code: v1.ErrInternal, Message: "payload is not valid base64: " + err.Error()}}
	}

	// The same "purpose, NUL, payload" construction every debark Signer
	// uses - imported directly from core/sign so this plugin can never drift
	// from the host's own domain separation.
	msg := sign.SigningInput(params.Purpose, payload)
	raw := ed25519.Sign(priv, msg)

	return v1.Response{ID: req.ID, Result: v1.SignResult{
		Signature: base64.StdEncoding.EncodeToString(raw),
		Algorithm: "ed25519",
		KeyID:     keyID,
	}}
}

func remarshal(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func writeLine(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

// deriveKeyID matches core/sign's own key file format (keyformat.go): the
// first 8 bytes of SHA-256(public key), lowercase hex. A key id computed the
// same way makes this plugin's output directly cross-checkable against a
// debark-native .pub file holding the same key, exported via key_info.
func deriveKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// loadOrCreateKey persists the plugin's ed25519 key as a raw 64-byte seed||
// public file (crypto/ed25519's own private-key encoding) at a fixed path, so
// the plugin's identity survives across invocations. This is deliberately not
// core/sign's own key file format: a plugin has no obligation to look like a
// native key file - it should manage key material however suits its use
// case (an HSM, a cloud KMS, a keychain) - this sample just needs *a* key.
//
// The path is $DEBARK_SAMPLE_SIGNER_KEYFILE when set (this is how the
// tests in core/sign point it at a throwaway temp directory instead of a
// real user profile), otherwise <user config dir>/debark/sample-signer-
// plugin.key.
func loadOrCreateKey(errOut io.Writer) (ed25519.PrivateKey, error) {
	path, err := keyFilePath()
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("key file %s is %d bytes, want %d", path, len(raw), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(raw), nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(errOut, "%s: generated a new key at %s\n", pluginName, path)
	return priv, nil
}

func keyFilePath() (string, error) {
	if p := os.Getenv("DEBARK_SAMPLE_SIGNER_KEYFILE"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine user config dir (set DEBARK_SAMPLE_SIGNER_KEYFILE to override): %w", err)
	}
	return filepath.Join(dir, "debark", "sample-signer-plugin.key"), nil
}
