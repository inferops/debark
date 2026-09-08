# Verifying a debark bundle without trusting debark

This is for someone who wants to check a bundle's manifest and signature by
hand, using ordinary tools, without running the `debark` binary and without
taking its "verified" claim on faith. If you cannot reproduce a bundle's
verdict with `sha256sum`, `jq` and a signature tool, this guide has failed at
its one job — please treat that as a bug in the guide, not in your
understanding.

**Testing status.** As of this writing `core/verify` and `core/bundle` have no
CLI commands wired up yet in this repository, so no real, `debark`-produced
bundle existed to test this guide against end to end. Every step below is
derived directly from the schema and the current implementation
(`core/manifest`, `core/canonical`, `core/sign`, cited by file and line
throughout), and the two steps with the most room for a documentation writer
to get subtly wrong — reproducing the RFC 8785 canonical form with ordinary
tools, and verifying a debark ed25519 signature with `openssl` rather than
debark itself — were built and checked end-to-end against a synthetic
manifest and a real generated Ed25519 keypair before this guide was written,
including two negative controls (a flipped byte, a wrong signature) that both
correctly failed. That test is not the same as testing against a real
`debark build` output; re-run this guide against the first real bundle that
exists and correct anything that doesn't match.

## 0. What this does and does not prove

Doing every step below and getting a clean result proves: *the bytes in this
bundle are exactly the bytes a holder of the private key corresponding to the
public key you supplied vouched for, at the time they signed.* It does not
prove the key holder is who you think they are (that binding is entirely
external — see `docs/threat-model.md` §6), does not prove the *contents* of
any package are safe,
and does not prove the bundle wasn't built from a tampered snapshot (see
`docs/threat-model.md` §3.2 and §5). Read the threat model before relying on
a clean result for anything consequential.

## 1. Prerequisites

- `sha256sum` (or any SHA-256 tool — `openssl dgst -sha256` works too, and is
  used below to cross-check)
- `jq`
- `openssl` **3.0 or later** (for native Ed25519 support in `pkeyutl`), or
  `gpg` if the bundle is GPG-signed
- The bundle, already on disk as a directory (extract `<name>.debark.tar.zst`
  first with any zstd-aware tar: `tar --zstd -xf <name>.debark.tar.zst`)
- The operator's public key, obtained **out of band** — never from the
  bundle itself (`docs/threat-model.md` §4 row 7). For an `ed25519-file`
  signature this is a `.pub` file; for a `gpg` signature this is the signer's
  public key already imported into a keyring you control.

Every command below is written to run from inside the bundle's root
directory (the one directly containing `debark.manifest.json`).

## 2. Recompute the manifest digest

The manifest on disk (`debark.manifest.json`) is **not** what was hashed
and signed directly — it is indented, human-formatted JSON. What was
actually signed is its **RFC 8785 (JSON Canonicalization Scheme) form**:
object keys sorted, no insignificant whitespace, UTF-8 output
(`core/canonical/canonical.go:1-13`, `core/manifest/api.go:137-149`). `jq -S`
(sort keys) piped through `jq -c` (compact) reproduces this exactly for every
value type debark's schemas actually use — plain ASCII string and integer
fields, no floats, no non-ASCII content — which is what was checked in the
test described above.

```sh
jq -S -c . debark.manifest.json > manifest.canonical.bytes
sha256sum manifest.canonical.bytes
```

**If jq is not available**, `python3 -c "import json,sys; d=json.load(open('debark.manifest.json')); sys.stdout.buffer.write(json.dumps(d, sort_keys=True, separators=(',',':'), ensure_ascii=False).encode())" > manifest.canonical.bytes` is an equivalent that was cross-checked against the real algorithm (`github.com/gowebpki/jcs`) byte-for-byte on a representative manifest.

**If you need certainty rather than a well-tested approximation** — for
example, the two digests below don't match and you want to rule out a
canonicalisation difference before concluding the bundle is tampered — build
debark's own canonicalizer as a five-line standalone program and run it
instead; it is the exact library debark uses, not a lookalike:

```sh
cat > canon.go <<'EOF'
package main
import ("os"; "github.com/gowebpki/jcs")
func main() {
	raw, _ := os.ReadFile(os.Args[1])
	out, err := jcs.Transform(raw)
	if err != nil { panic(err) }
	os.Stdout.Write(out)
}
EOF
go mod init canon && go get github.com/gowebpki/jcs@v1.0.1
go run canon.go debark.manifest.json > manifest.canonical.bytes
sha256sum manifest.canonical.bytes
```

## 3. Compare against the recorded digest

```sh
jq -r .manifest_sha256 debark.manifest.sig
```

This must equal the digest from step 2, character for character. If it does
not, **stop here** — the manifest on disk is not the one that was signed,
full stop, regardless of what any signature check below would say. This is
`core/verify`'s step 2 (`core/verify/verify.go:90-102`,
`ProblemManifestDigest`).

## 4. Verify the signature

First check what kind of signature you have:

```sh
jq -r '.signatures[] | "\(.signer_kind) \(.key_id) \(.algorithm)"' debark.manifest.sig
```

### 4a. `ed25519-file` signatures

debark's own key file format is **inspired by minisign but is not
minisign-compatible** — do not try to verify it with the `minisign` or
`signify` CLI; it will not read the key or signature file
(`core/sign/keyformat.go:14-21`). Use `openssl` instead, which needs the raw
32-byte public key wrapped in a standard DER `SubjectPublicKeyInfo` structure.

**a. Extract the raw public key from the `.pub` file.** The file is two
lines: an "untrusted comment" line, then one base64 line decoding to a
42-byte blob (`core/sign/keyformat.go:45-57`): 2 bytes `"Ed"`, 8 bytes key id,
32 bytes raw Ed25519 public key. Skip the first 10 decoded bytes:

```sh
tail -n1 operator.pub | base64 -d | tail -c 32 > pubkey.raw
```

**b. Wrap it as a PEM public key**, using the fixed 12-byte DER prefix for an
Ed25519 `SubjectPublicKeyInfo` (RFC 8410) — this prefix is constant for every
Ed25519 key, never computed from the key material itself:

```sh
{ printf '302a300506032b6570032100' | xxd -r -p; cat pubkey.raw; } > pubkey.der
{ echo "-----BEGIN PUBLIC KEY-----"; openssl base64 -in pubkey.der -A | fold -w64; echo; echo "-----END PUBLIC KEY-----"; } > pubkey.pem
openssl pkey -pubin -in pubkey.pem -text -noout   # sanity check: should print "ED25519 Public-Key:" and 32 key bytes
```

**c. Reconstruct exactly what was signed.** debark signs
`purpose ‖ 0x00 ‖ canonical_manifest_bytes`, never the manifest bytes alone —
this domain separation is what stops a manifest signature from being replayed
as a signature over some other kind of object
(`core/sign/iface.go:43-52`). The purpose for a manifest is the fixed string
`debark.manifest/v1` (`core/manifest/types.go:147`):

```sh
{ printf 'debark.manifest/v1'; printf '\0'; cat manifest.canonical.bytes; } > signing_input.bin
```

**d. Extract the raw signature and verify:**

```sh
jq -r '.signatures[0].signature' debark.manifest.sig | base64 -d > signature.raw
openssl pkeyutl -verify -pubin -inkey pubkey.pem -rawin \
  -in signing_input.bin -sigfile signature.raw
```

`Signature Verified Successfully` (exit code 0) is the only acceptable
result. Anything else — including any error — means the bundle does not have
a valid signature from the key in `pubkey.pem`; do not install it.

### 4b. `gpg` signatures

```sh
jq -r '.signatures[0].signature' debark.manifest.sig | base64 -d > signature.raw
{ printf 'debark.manifest/v1'; printf '\0'; cat manifest.canonical.bytes; } > signing_input.bin
gpg --no-default-keyring --keyring /path/to/your/own/keyring.gpg \
    --verify signature.raw signing_input.bin
```

Use a keyring you control and populated yourself with the expected signer's
key (`gpg --import`) — never the bundle's own media, and be deliberate about
which keyring you point at: an operator's personal default GPG keyring can
contain keys unrelated to this bundle (`docs/threat-model.md` §5, last
bullet). Require `gpg`'s own "Good signature from ..." line **and** confirm
the fingerprint it names matches `key_id` in `debark.manifest.sig` — do not
accept the exit code alone.

## 5. Verify every file digest

```sh
jq -r '.files[] | "\(.sha256)  \(.path)"' debark.manifest.json > MANIFEST.sha256
sha256sum -c MANIFEST.sha256
```

Every line must say `OK`. This is `core/verify`'s steps 4–5
(`core/verify/verify.go:225-297`): every file the manifest lists must be
present with the right digest, **and** — which `sha256sum -c` alone will not
catch — no *extra* file may exist that the manifest never listed:

```sh
{ find . -type f ! -name debark.manifest.json ! -name debark.manifest.sig -printf '%P\n'; } | sort > on-disk-files.txt
jq -r '.files[].path' debark.manifest.json | sort > manifest-files.txt
diff manifest-files.txt on-disk-files.txt
```

An empty `diff` output is what you want. Any line starting with `>` is a file
present on disk but never listed in the manifest — apt will not read it, but
you should still ask why it's there.

## 6. Cross-check the repository metadata

```sh
jq -r '.repository.packages_sha256, .repository.release_sha256' debark.manifest.json
sha256sum repo/Packages repo/Release
```

The values must match, in order. (This is redundant with step 5 if
`repo/Packages` and `repo/Release` are correctly listed in `files[]` — the
point of doing it separately is exactly what `core/verify` does
(`checkRepository`, `core/verify/verify.go:299-331`): it catches the manifest
document itself being internally inconsistent, rather than trusting one
summary field against another summary field with nothing independent in
between.

## 7. Cross-check the lock and snapshot digests

```sh
jq -S -c . lock.json | sha256sum
jq -r .lock_digest debark.manifest.json
```

These two digests must match. If `snapshot.json` is present in the bundle,
repeat with it against `.snapshot_digest`. (Same canonicalisation caveat as
step 2 applies to both files.)

## 8. What a clean result means, and what it doesn't

If every step above passed: the manifest is exactly what was signed, the
signature is genuinely valid for the key you independently obtained, every
file matches what the manifest claims (with nothing extra), and the
repository/lock/snapshot are all internally consistent with the manifest.
That is everything `debark verify` itself checks
(`core/verify/iface.go:17-34`) — you have reproduced its verdict without
running it.

What you have **not** checked: whether the key you used was the *right* key
(that binding has to come from outside this bundle — a fingerprint compared
by hand, a pre-provisioned config entry, never something shipped alongside
the bundle you're checking); whether the packages inside are safe, current,
or licensed for your use; or whether the snapshot this bundle was built from
was itself genuine. See `docs/threat-model.md` §5 for the complete list.
