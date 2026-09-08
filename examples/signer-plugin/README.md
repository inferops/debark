# signer-plugin — a sample `debark.plugin/v1` signer

A minimal, working `sign`-capability plugin, in one file (`main.go`), meant to
be read end to end and copied from. It is **not** a production key-custody
tool: it holds one ed25519 key in a plain file. A real plugin would talk to an
HSM, a cloud KMS, or a hardware token instead — the protocol on stdio does not
change either way.

## What it does

On start it loads (or generates, on first run) one ed25519 key, prints the
`debark.plugin/v1` handshake line, and then answers requests one line of
JSON at a time until its stdin closes:

- `sign` — signs `purpose || 0x00 || payload` with the plugin's key. This is
  exactly `core/sign.SigningInput(purpose, payload)`, imported directly from
  the host module, so the plugin can never drift from the host's own domain
  separation. It refuses to sign a request with an empty `purpose`.
- `key_info` — returns the key id, algorithm, and the raw public key
  (base64), without signing anything.
- `shutdown` — replies, then exits. The host closing stdin does the same
  thing even if `shutdown` is never sent.

Anything else comes back as an `unsupported-method` error. Every line this
plugin does *not* understand how to log goes to stderr, never stdout — stdout
carries only protocol traffic.

## Build and run it standalone

```sh
go build -o signer-plugin ./examples/signer-plugin
```

The plugin is designed to be spawned by a host speaking the protocol, not
typed at by a human, but you can drive it by hand to see the wire format:

```sh
$ ./signer-plugin
{"protocol":"debark.plugin/v1","name":"debark-sample-signer-plugin","version":"0.1.0","capabilities":["sign"]}
```

(the process is now waiting on stdin). In another line, paste a request and
press enter:

```json
{"id":"1","method":"key_info"}
```

```json
{"id":"1","result":{"key_id":"...","algorithm":"ed25519","public_key":"...","comment":"debark-sample-signer-plugin sample key - do not use for anything real"}}
```

```json
{"id":"2","method":"sign","params":{"purpose":"debark.manifest/v1","payload":"aGVsbG8="}}
```

```json
{"id":"2","result":{"signature":"...","algorithm":"ed25519","key_id":"..."}}
```

Closing stdin (Ctrl-D / Ctrl-Z) ends the process cleanly.

## Using it with debark

```sh
debark build ... --sign plugin:/path/to/signer-plugin
```

`core/sign.SignerFor` spawns the executable named after `plugin:`, performs
the handshake, and calls `sign` once per manifest. `Signer.Close` sends
`shutdown`, closes stdin, and kills the process if it has not exited shortly
after.

### Key file location

The key is persisted at `<user config dir>/debark/sample-signer-plugin.key`
(`os.UserConfigDir()` — `%AppData%` on Windows, `~/Library/Application Support`
on macOS, `$XDG_CONFIG_HOME` or `~/.config` on Linux), mode `0600`, as the raw
64-byte `crypto/ed25519` private key encoding. Set
`DEBARK_SAMPLE_SIGNER_KEYFILE=/some/path` to use a different file instead —
this is also how `core/sign`'s own tests point the plugin at a throwaway
directory rather than touching a real profile.

### Verifying a bundle this plugin signed

`debark verify` checks a `plugin:<name>`-kind signature the same way it
checks a native `ed25519-file` one, *provided* the operator has separately
told it to trust the key: export the public key with `key_info` above, then
hand-write it into `core/sign`'s native `.pub` format (documented in
`core/sign/keyformat.go`) so it can be listed in a `--keyring` directory:

```
untrusted comment: debark ed25519 public key <key id from key_info>
<base64 of: "Ed" + 8-byte key id + the 32-byte public_key from key_info, all concatenated>
```

The key id and the algorithm (`ed25519`) already match `core/sign`'s own
derivation (first 8 bytes of `SHA-256(public key)`), so no translation is
needed beyond building that one blob.

## What a real plugin should change

- Key custody: talk to the real signing device instead of a local file.
- `key_info.public_key` may legitimately be empty when the device cannot
  export a public key at all (some HSMs refuse to).
- Consider a `--verbose`-style flag surfaced via an environment variable for
  your own diagnostics; anything printed to stderr shows up in the host's
  error messages and hints when a call fails.
- The 5-second grace period the host gives `Close` before it kills the
  process is fixed; make sure `shutdown` returns quickly.
