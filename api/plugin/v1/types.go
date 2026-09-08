// Package v1 is debark.plugin/v1: an out-of-process, language-agnostic
// plugin protocol carried as single-line JSON over stdio (ADR-009).
//
// No Go plugin ABI, no .so files, no gRPC dependency. A plugin is any
// executable that prints a Handshake on start and then answers requests, one
// JSON object per line, until its stdin closes. Only the sign capability is
// used in v1; the protocol carries a version so a later capability is additive.
//
// Wire discipline:
//
//	plugin -> host   {"protocol":"debark.plugin/v1","name":"acme-hsm",...}
//	host   -> plugin {"id":"1","method":"sign","params":{...}}
//	plugin -> host   {"id":"1","result":{...}}      or  {"id":"1","error":{...}}
//
// Binary values (bytes to sign, signature bytes) are standard base64 strings.
// Anything a plugin writes to stderr is surfaced as plugin diagnostics; it is
// never parsed.
package v1

// Protocol is the version string a plugin must announce.
const Protocol = "debark.plugin/v1"

// Capability names.
const (
	// CapabilitySign means the plugin can sign canonical bytes. It is the only
	// capability defined in v1.
	CapabilitySign = "sign"
)

// Method names.
const (
	// MethodSign asks the plugin to sign SignParams.Payload.
	MethodSign = "sign"
	// MethodKeyInfo asks for the key identity without signing anything.
	MethodKeyInfo = "key_info"
	// MethodShutdown asks the plugin to exit cleanly. The host also closes
	// stdin, which is the authoritative signal.
	MethodShutdown = "shutdown"
)

// Handshake is the first line a plugin prints on start.
type Handshake struct {
	Protocol string `json:"protocol"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	// Capabilities lists what this plugin can do; the host refuses to call a
	// method whose capability was not announced.
	Capabilities []string `json:"capabilities"`
}

// Request is one host-to-plugin call.
type Request struct {
	// ID correlates a response with its request. The host generates it.
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// Response is one plugin-to-host reply. Exactly one of Result and Error is set.
type Response struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

// Error is a plugin-reported failure.
type Error struct {
	// Code is a short stable identifier: unsupported-method, key-unavailable,
	// user-declined, internal.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes.
const (
	ErrUnsupportedMethod = "unsupported-method"
	ErrKeyUnavailable    = "key-unavailable"
	ErrUserDeclined      = "user-declined"
	ErrInternal          = "internal"
)

// SignParams are the parameters of a sign call.
type SignParams struct {
	// Purpose is the domain-separation string, e.g. debark.manifest/v1. A
	// plugin must incorporate it or refuse.
	Purpose string `json:"purpose"`
	// Payload is standard base64 of the canonical bytes to sign.
	Payload string `json:"payload"`
	// KeyRef is the operator's key selector, passed through verbatim.
	KeyRef string `json:"key_ref,omitempty"`
}

// SignResult is the reply to a sign call.
type SignResult struct {
	// Signature is standard base64 of the raw signature bytes.
	Signature string `json:"signature"`
	// Algorithm is the signature algorithm, e.g. ed25519 or ecdsa-p256-sha256.
	Algorithm string `json:"algorithm"`
	// KeyID identifies the key that signed.
	KeyID string `json:"key_id"`
	// SignerKind is recorded in the manifest signature block; when empty the
	// host records plugin:<name>.
	SignerKind string `json:"signer_kind,omitempty"`
}

// KeyInfoResult is the reply to a key_info call.
type KeyInfoResult struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	// PublicKey is standard base64 of the raw public key, when the plugin can
	// export one. Optional: an HSM may not.
	PublicKey string `json:"public_key,omitempty"`
	Comment   string `json:"comment,omitempty"`
}
