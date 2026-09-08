# debark format reference

This is the reference for every document debark reads or writes: what it
is, when it is produced, how long it lives, and its exact field shape. It is
written for readers outside the debark codebase — auditors, integrators,
and anyone writing independent tooling against a bundle — and it is the
document a third party should use to verify a debark signature without
running debark itself.

Machine-readable JSON Schemas (draft 2020-12) for every format below are
published in [`api/schema/`](../api/schema/), one file per format, named
`<name>.v1.schema.json`, each with a stable `$id` under
`https://debark.dev/schema/<name>/v1`. This document and those schemas are
kept in lock-step with the Go types in `core/` and `api/` by a drift-checking
test (`api/schema/schema_test.go`); if you find a difference between this
page and a shipped bundle, the schema files and the test are the tie-break.

One format below has no published JSON Schema, deliberately: the base
definition is a YAML file an operator authors and hands to debark,
not a document debark emits, hashes or signs. It is specified
alone, and the parser refuses unknown fields exactly as
`additionalProperties: false` does for the JSON formats, so a typo is an
error rather than a silent default. §3.13 gets the same treatment by another
route: `core/base/docs_test.go` fails if a field of the Go type is missing
from its table, or if the worked example there stops matching what the
encoder actually produces.

## 0. Compatibility promise

Every format here carries a `schema_version` (or, for the two files that use
a different key name, `schema`) as its first logical field, a literal string
such as `debark.snapshot/v1`. **A published schema version is never
mutated.** If a format needs to change shape, it becomes a new version
(`debark.snapshot/v2`) with a documented migration from the old one; the
old version stays readable indefinitely. Tooling should treat an unrecognised
`schema_version` as "I cannot read this," never as "ignore the parts I don't
understand" — these are closed documents (see §1.3) precisely so that
silently-ignored fields never hide a change from a verifier.

## 1. Canonicalisation and digests

This is the section a third-party verifier needs first. Get this wrong and
every digest and signature check below will look like a mismatch even on an
untampered bundle.

### 1.1 RFC 8785 (JCS) — the canonical form

Any debark document that is hashed or signed is first reduced to its
**canonical JSON encoding**, per [RFC 8785, the JSON Canonicalization
Scheme (JCS)](https://www.rfc-editor.org/rfc/rfc8785):

- Object members are sorted by their key, compared as UTF-16 code units (in
  practice, for every key debark ever writes — plain ASCII snake_case — this
  is the same order as an ordinary byte-wise/codepoint sort).
- No insignificant whitespace: no spaces, no newlines, no indentation.
- Numbers are formatted per the ECMAScript `ToString` algorithm. Every number
  debark emits is a non-negative integer (a size, a count, a byte total),
  so in practice this just means "no leading zeros, no unnecessary decimal
  point, no `+` sign."
- Strings use ordinary JSON string escaping; the output is UTF-8 bytes, no
  byte-order mark.

debark's implementation is `core/canonical` (`Marshal`, `Transform`,
`Digest`), backed by `github.com/gowebpki/jcs`. `canonical.Transform(raw
[]byte)` runs JCS over already-encoded JSON bytes; `canonical.Marshal(v any)`
first runs the ordinary Go `encoding/json` marshaller and then JCS on the
result. **A third party can reproduce this with any conformant JCS library in
any language** — JCS is a public standard with implementations for
JavaScript, Python, Rust, Java and more; you do not need Go or debark's
source to do this step.

A digest, everywhere in debark, is:

```
digest(document) = lower_hex( SHA-256( JCS(document) ) )
```

### 1.2 Timestamps

Every timestamp is RFC 3339, UTC, second precision, with a literal `Z` — the
Go format string `2006-01-02T15:04:05Z` — for example `2026-09-03T12:00:00Z`.
Never a numeric offset, never sub-second precision, never a local zone. This
is deliberate: it keeps timestamps a fixed 20-byte string, which matters for
byte-identical, reproducible builds (two builds of the same request produce
byte-identical manifests only if nothing time-related varies in an
unexpected format).

### 1.3 Two different digests that look alike — read this carefully

debark documents carry digests in two distinct roles that are easy to
conflate:

**(a) Whole-file digests** — "the SHA-256 of the bytes actually sitting on
the medium." This is what `manifest.json`'s `files[].sha256` is, what every
`lock.json` package entry's `sha256` is, what a store object's digest is, and
what apt's own `Packages` file hashes are. It is computed by hashing the
literal bytes of a file with no transformation at all — most of these files
are not even JSON (`.deb` archives, apt's `Packages` index). Reproduce it
with `sha256sum <file>`.

**(b) Canonical-content digests** — "the SHA-256 of this JSON document's
*meaning*, independent of how it happens to be formatted on disk." This is
what `manifest.json`'s own `snapshot_digest` and `lock_digest` fields are,
what `lock.json`'s `snapshot_digest` and `request_digest` fields are, and
what `debark.manifest.sig`'s `manifest_sha256` is — the value that is
actually signed. It is computed: JCS-canonicalise, then SHA-256.
**Do not `sha256sum` the file for these fields** — the file on disk is
written indented for humans (`core/canonical.MarshalIndent`, two-space
indent, trailing newline), which is a *different* byte sequence from its
canonical form, and will not match.

Concretely: `lock.json` as it sits in a bundle is **not** the same bytes
that `manifest.json`'s `lock_digest` was computed over. To verify
`lock_digest` yourself: read `lock.json`, parse it as JSON, re-serialise it
through a JCS implementation, SHA-256 the result, and compare. To verify a
`files[]` entry for `lock.json` (the *other* check the manifest carries for
the same file): just `sha256sum lock.json` directly. Both checks exist and
both matter — the whole-file digest is what actually catches a single
tampered byte on the medium (including a byte debark's own parser would
silently ignore, such as an added, unrecognised JSON field); the
canonical-content digest is what lets the *logical content* of a document be
matched even if it were re-serialised by different tooling.

The one exception with no `files[]` backstop is the manifest itself: nothing
else independently checks `manifest.json`'s bytes, because the manifest is
the root of trust. That is why `manifest_sha256` in `debark.manifest.sig`
is computed by canonicalising **the exact bytes read from disk** (not by
re-serialising a parsed struct) — the only correct way to make sure an
attacker cannot inject a byte your canonicaliser doesn't understand and have
it silently vanish before the digest is taken. Any independent
implementation of manifest verification must do the same: canonicalise the
raw bytes you read, never a round-tripped/re-typed copy of them.

### 1.4 Verifying a debark signature with your own tooling

This is the worked recipe. Given a bundle directory (see §2):

1. **Read `debark.manifest.json`** as raw bytes. Do not parse-and-re-marshal
   it through your own types yet — keep the original bytes.
2. **Canonicalise those exact bytes** with a JCS implementation. Call
   the result `canonicalManifest`.
3. **Read `debark.manifest.sig`** and parse it as JSON
   (`debark.signature/v1`, see §3.4). Compute
   `lower_hex(SHA-256(canonicalManifest))` and compare it to the file's
   `manifest_sha256`. **If these do not match, stop — the manifest was
   altered after signing, or you canonicalised something other than the
   exact bytes on disk.** Do not proceed to signature checks; a mismatch here
   means the signature checks below are checking a document that is no
   longer the one that was signed.
4. For each entry in `signatures[]`, reconstruct the exact bytes that were
   signed:

   ```
   signingInput = UTF8("debark.manifest/v1") || 0x00 || canonicalManifest
   ```

   That is: the literal ASCII bytes of the purpose string
   `debark.manifest/v1` (this exact constant, regardless of the document's
   own `schema_version`, which happens to have the same text at the time of
   writing but is a different field serving a different role), one `0x00`
   byte, then the canonical manifest bytes from step 2 — no length prefix,
   no separator character, no newline. This three-part construction is
   `core/sign.SigningInput` and exists to stop a signature made for one kind
   of debark document from ever being replayed as a signature over another.
5. Base64-decode `signature` (standard base64, `+/` alphabet, `=` padding)
   to get the raw signature bytes.
6. Verify, algorithm-appropriately:
   - `signer_kind: "ed25519-file"` — `algorithm` is `ed25519`. `key_id` is a
     lowercase-hex identifier derived from the Ed25519 public key; obtain the
     matching public key **out of band** (see §1.5 — never trust a key found
     on the same medium as the bundle) and check
     `Ed25519.Verify(publicKey, signingInput, signatureBytes)`. Ed25519 signs
     the message directly; there is no separate pre-hash step to reproduce.
   - `signer_kind: "gpg"` — the signature bytes are a detached OpenPGP binary
     signature, but it is a signature **over `signingInput`, not over the raw
     manifest file**. A plain `gpg --verify sigfile debark.manifest.json`
     will therefore report a bad signature even on an untampered bundle;
     instead write `signingInput` to a temporary file yourself and run
     `gpg --verify sigfile signinginput.bin`.
   - `signer_kind: "sigstore"` or `plugin:<name>` — same `signingInput`
     construction; the verification mechanics are specific to that signer
     (a Sigstore bundle, or whatever the named plugin's key type implies) and
     are outside this document's scope, but the bytes being verified are
     always exactly the `signingInput` above.
7. A bundle is trustworthy only if **at least one** signature block verifies
   against a key you trust **for a reason that has nothing to do with the
   bundle itself** (pre-provisioned file, configuration, or a fingerprint you
   compared by hand against the key's owner). debark's own `verify` refuses
   to source a trusted key from inside the bundle it is checking, and an
   independent verifier should apply the same rule.
8. Once the manifest is trusted, its `files[]` array (field table) is
   itself now a trusted list of every other file's path, size and whole-file
   SHA-256 (§1.3(a)) — check every one of them by hashing the actual bytes on
   the medium (no canonicalisation), and treat any file present on the medium
   but absent from `files[]` as a failure too. Then check
   `snapshot_digest` and `lock_digest` per §1.3(b) against `snapshot.json`
   and `lock.json` respectively, and `repository.packages_sha256` /
   `repository.release_sha256` (whole-file digests again) against
   `repo/Packages` and `repo/Release`.

This order matters and mirrors `core/verify`'s own documented sequence: a
malformed or unsigned manifest is rejected before any file on the medium is
even opened, so a hostile bundle cannot use a crafted `files[]` list to make
a verifier read something it shouldn't.

### 1.5 Where trust actually comes from

Three layers, never conflated:

1. **Archive authenticity.** apt's own trust (`InRelease`/`Release.gpg`
   against the archive keys recorded — as *captured policy, not an
   independent root of trust* — in the snapshot) is what vouched for every
   apt-sourced `.deb` at *fetch* time, before it ever entered the bundle.
2. **Transfer integrity.** The manifest signature is what vouches for
   the bytes that actually crossed the air gap, end to end, regardless of
   where they originally came from.
3. **Local repository trust.** Once installed, the target's own apt
   configuration decides how much it trusts the bundle's flat repository —
   in practice `Trusted: yes` on a private source, granted only *after*
   manifest verification (layer 2) has already passed. apt performs no
   independent check of repository metadata at install time, so layer 2 is
   what makes that safe. A repository-level GPG signature over the bundle's
   own `Release` would sit here as a second, apt-native layer, and the
   manifest reserves `inrelease_sha256`/`release_gpg_sha256` for it,
   but **debark does not produce one today** — no bundle it writes
   contains an `InRelease` or `Release.gpg`. Do not plan a policy around
   that layer yet.

The operator's public key for layer 2 must reach the target independently of
the bundle's media — that is a document-external fact this format reference
cannot verify for you, but it is the one precondition the whole scheme rests
on.

## 2. Bundle layout

A build produces this tree (directory form; `bundle export --tar` produces
the identical tree inside a deterministic `<name>.debark.tar.zst`: sorted
entries, fixed mtimes, no host paths, no uid/gid beyond 0):

```
<bundle>/
  debark.manifest.json        canonical content, indented on disk; the signed document
  debark.manifest.sig         detached signature block(s) over the manifest
  lock.json                     what was chosen and why
  snapshot.json                 the input snapshot, redacted fields absent
  repo/
    Release                     apt release file (unsigned; see §1.5 layer 3)
    Packages, Packages.gz       apt package indices
    pool/<p>/<pkg>/<pkg>_<ver>_<arch>.deb
  sbom.cdx.json                 optional CycloneDX SBOM
  evidence.json                 structured build events
  last-run-added.txt            pool files this run added, repo-relative, one per line
  last-run-removed.txt          pool files this run pruned (empty unless --update)
  last-run-unreferenced.txt     pool files repo/Packages does not index (item 5)
  README.txt                    human summary and verify/install instructions
  bin/debark-linux-<arch>       optional (--embed-binary); its sha256 is in the manifest
```

### What an auditor should look at first

In order:

1. **`debark.manifest.sig` and `debark.manifest.json`** — run the
   recipe. If this does not check out, stop; nothing else in the
   bundle is meaningful until the manifest is trusted.
2. **`README.txt`** — the human-readable summary of what this bundle is,
   whether it is signed, and any redistribution warnings the build recorded.
   It is generated, not authoritative, but it is the fastest orientation.
3. **`lock.json`'s `warnings[]` and `packages[].flags`** — redistribution
   terms (`multiverse`, `restricted`, `non-free`), unverified vendor URLs
   (`publisher_verification: url-unverified`), and doctor-derived flags
   (`network-postinst`, `snap-shim`, `dkms`) live here. This is where "is
   there anything about this bundle a reasonable person would want to know
   before installing it" is answered.
4. **`lock.json`'s `closed_world_check`** — did the build prove, offline,
   that the finished bundle installs from itself with no further network
   access? `result: "ok"` is the strong claim; `"skipped"` is a build that
   opted out of the proof and should be treated with more suspicion than
   `"ok"`. `"failed"` covers a proof that ran and came out wrong — the
   simulated install did not resolve, the run reached for a network URI, or
   the simulated upgrade set diverged from the versions the lock recorded
   (E3) — *and* a proof the build could not complete: apt simulate output
   carrying action lines this build could not parse is reported as `failed`
   naming those lines, because a line the parser drops is a package the
   check never compared against the lock at all, and reporting `ok` for it
   would be a stronger claim than the check earned. `detail` says which.
5. **`last-run-unreferenced.txt` and the `pool.unreferenced` warning** —
   every file in `repo/pool` that this bundle's own `repo/Packages` does not
   index. Such a file is on the media and covered by the manifest and the
   signature, so `verify` accepts it, but the target's apt cannot see or
   install it. It is ordinary: a run is additive by default, and even under
   `--update` prune never removes the version the lock names or the highest
   version of a `(name, arch)` group, so a package an earlier build asked
   for and this one did not simply stays. The file is the complete sorted
   list; the `pool.unreferenced` warning in `lock.json` names the first few
   and, where the same package *is* indexed at another version, both
   versions. Empty file, no warning, means the pool and the index agree
   exactly.
6. **`snapshot.json`'s `target` and `origin`** — confirm this bundle was
   actually built for the machine you intend to install it on (distro,
   version, codename, architecture) before doing anything else; `debark
   install` checks this too (and refuses on an architecture mismatch), but
   an auditor reviewing a bundle before it reaches the target should check
   it first. `origin.kind` then says whether the bundle was resolved against
   a *measurement* of a real machine (`captured`) or an *assumption* about a
   stock release (`synthesized`) — see §3.1, because the two carry different
   completeness guarantees and only one of them was checked against a
   machine.
7. **`evidence.json`** — the full structured timeline, for anyone who needs
   more than the above (which build host, which container image digest, what
   solver-divergence warning fired, what every fetch resolved to).

## 3. Per-format reference

### 3.1 Snapshot — `debark.snapshot/v1`

**What it is.** Everything the target's own apt would need to resolve a
request as the target would, and nothing more by default. **Produced by**
either of two commands, and `origin.kind` says which: `debark snapshot
create`, run on the offline target without root and with no network access
required, which *measures* a real machine (`captured`); or `debark snapshot
from-base`, run on the builder, which *synthesizes* a document describing a
stock release from a base definition, for a target that does not
exist yet (`synthesized`). `build --base` performs the same synthesis inline,
so a build always has a snapshot whether the operator produced one by hand or
not. **Carried as** `snapshot.tar.zst`, containing `snapshot.json` (the
document below) and every captured file verbatim under `files/<archive
path>`. A copy of `snapshot.json` alone (redacted fields already stripped) is
also placed at the bundle root by `build`, for reference — the full
`snapshot.tar.zst` with file contents is not distributed inside a bundle.
**Lifetime.** One snapshot is one point-in-time answer — a capture, or one
synthesis of one base definition — and it is never edited in place. A stale
snapshot is superseded by taking a new one, not by patching the old one.
Field reference in
[`snapshot.v1.schema.json`](../api/schema/snapshot.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.snapshot/v1` |
| `created_at` | string | RFC 3339 UTC |
| `tool` | object | `{name, version, edition?}` — what captured it |
| `target` | object | distro/version/codename/arch/apt version/dpkg version/machine-id (redactable) |
| `apt` | object | `sources[]`, `preferences[]`, `conf[]`, `trusted[]`, `keyrings[]` — each a list of captured `File` |
| `dpkg_status` | object (`File`) | pointer to the captured `/var/lib/dpkg/status`; for a synthesized snapshot, to the dpkg status the seed closure produced |
| `origin` | object | **required** — `{kind: captured\|synthesized, base_id?, source?, source_digest?, assumed_installed[]?}`. How this document came to exist, and with it the guarantee any bundle built from it can carry. See below |
| `installed_count` | integer | derived, human output only. For a synthesized snapshot it counts the **assumed** installed set, which is a claim about a stock release rather than a measurement of a machine |
| `keyring_fingerprints[]` | array | every OpenPGP fingerprint found, as captured policy (§1.5) |
| `labels` | object | operator metadata, off by default, stripped by `--redact` |
| `redactions[]` | array of string | which field classes were stripped: `machine-id`, `proxies`, `labels` |
| `warnings[]` | array of string | what capture could not do without failing |

`snapshot create --redact` removes `machine_id`, proxy settings inside
`apt.conf[]`, and `labels`. A redacted snapshot forces the conservative
`never-include` phased-update policy at resolve time,
because a builder cannot reproduce the target's own phasing decision without
the target's machine id.

**The two kinds of snapshot, and what each does and does not guarantee.**
`origin.kind` is how you tell them apart, and the difference is a difference
in what the finished bundle proves:

- **`captured`** — a *measurement* of one real machine. apt was given that
  machine's own dpkg status, sources, pins and apt configuration, and
  answered with exactly what is missing there, so the bundle is **proven
  complete for that machine**. It guarantees nothing about any other
  machine, and nothing about that machine after it has changed: a snapshot
  is a point in time, and installing something else on the target
  afterwards can make a bundle built from it short.
- **`synthesized`** — an *assumption* about a machine that may not exist
  yet. `snapshot from-base` resolves a base definition's seed packages
 with the release's own apt in a private root where nothing is
  installed, and records what apt says a stock install of that release
  contains as this snapshot's dpkg status. The bundle is complete **only if
  the target really is a stock install of that release.** If the real
  machine has *more* packages than the base assumed, the bundle is merely
  larger than it needed to be — harmless. If it has *fewer*, the bundle is
  short and fails at the gap, which is the failure debark exists to
  prevent. Nothing here is a measurement of the eventual target: no machine
  was consulted. The distance between a seed closure and a real installed
  system is the subject of experiment E8
  (`docs/experiments/E8-base-fidelity.md`), which is where that question is
  measured rather than reasoned about.

A synthesized snapshot also deliberately carries no `machine_id`, no
`labels` and no captured `os-release`: a base is a claim about a release,
and a plausible-looking machine id invented by a builder would reproduce a
phasing decision belonging to no machine at all. Resolution against one
therefore always uses the conservative `never-include` phased-update policy,
for the same reason a redacted snapshot does.

`origin` is **required**, and an absent one is an error rather than a
default. Absent-means-captured is the tempting shortcut and it is the wrong
direction: it would make the dangerous case the silent default, so that a
synthesized snapshot which lost the field would read as a measurement of a
real machine — the strongest claim this format can make, asserted by the
document that says least (ADR-014). A captured snapshot carrying
base-definition fields is rejected too; neither kind may quietly wear the
other's clothes.

**Do not confuse this `origin` with a lock package's `origin`.** The
two fields share a name, live in different documents, and mean unrelated
things. A *snapshot's* `origin` is the provenance of the snapshot document
itself — captured from a machine, or synthesized from a base. A *lock
package's* `origin` is the provenance of one `.deb` file — the archive URI,
suite, component, release digest and key fingerprint it was fetched from, or
the vendor URL it was downloaded from. Neither refers to the other.

`origin.assumed_installed` restates what a synthesized snapshot's
`dpkg_status` file already says, and the duplication is the point rather
than an oversight. A bundle carries `snapshot.json` but **not** the
snapshot's files, so on the target — the one place where the
assumption can finally be compared against a real dpkg status — this list is
the only copy of it that exists. It is what lets `install` report how far
the machine in front of it diverges from the base rather than only that a
base was assumed. Carrying it is safe for a reason that does not generalise:
it is a claim about a public stock release. The installed-package list of a
*real* machine is a sensitive inventory of an air-gapped system
(`docs/threat-model.md` asset 2), which is exactly why a captured snapshot's
dpkg status never travels inside a bundle and why this field must be absent
for a captured snapshot — one that carries it is refused.

**Example** (abridged — a real snapshot carries the full captured file list):

```json
{
  "schema_version": "debark.snapshot/v1",
  "created_at": "2026-09-03T12:00:00Z",
  "tool": { "name": "debark", "version": "1.0.0", "edition": "community" },
  "target": {
    "distro_id": "debian", "version_id": "12", "codename": "bookworm",
    "pretty_name": "Debian GNU/Linux 12 (bookworm)", "arch": "amd64",
    "foreign_archs": ["i386"], "apt_version": "2.6.1", "dpkg_version": "1.21.22",
    "machine_id": "0123456789abcdef0123456789abcdef"
  },
  "apt": {
    "sources": [
      { "path": "/etc/apt/sources.list", "archive_path": "etc/apt/sources.list",
        "size": 1200, "sha256": "93b01c58ab034ef261f31762c5246f5ddebb1c7c3db60cde6e6956487b6ff066" }
    ],
    "trusted": [
      { "path": "/etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg",
        "archive_path": "etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg",
        "size": 2400, "sha256": "b6fd458a7205924b78c0b1bcc6730fc69ee72e093dd49f56e106479057f9441d" }
    ]
  },
  "dpkg_status": { "path": "/var/lib/dpkg/status", "archive_path": "var/lib/dpkg/status",
    "size": 892000, "sha256": "48c6dc7efba94dc886451717a6d7077271646ac6be4b90e561d6ce86daf87645" },
  "origin": { "kind": "captured" },
  "installed_count": 742,
  "keyring_fingerprints": [
    { "fingerprint": "CAE265BE4F5710A1C6714384E04F6BB6DC1AE916", "key_id": "E04F6BB6DC1AE916",
      "keyring": "etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg" }
  ],
  "redactions": ["proxies"],
  "warnings": []
}
```

A synthesized snapshot is the same document with a fuller `origin` block and
a `warnings[]` entry it carries permanently, so that whoever reads the file
months later — having not run the command that made it — is told what it is:

```json
  "origin": {
    "kind": "synthesized",
    "base_id": "ubuntu:26.04/desktop",
    "source": "builtin",
    "source_digest": "5ca54ae935ebe6a895ac8f31606e4db256628778d35f562ff44ee0aba658c349",
    "assumed_installed": ["base-files:amd64", "bash:amd64", "dpkg:amd64", "libc6:amd64"]
  },
  "warnings": [
    "this snapshot was synthesized from a base definition, not captured from a real machine: the bundle built from it is complete only if the target really is a stock install of this release"
  ]
```

`source` is `builtin` for a definition compiled into the binary, or the
**base name** of the operator's definition file — never an absolute path, so
the directory layout of the machine that ran `from-base` does not travel
into a bundle. `source_digest` is the SHA-256 of the canonicalised
definition, which is what tells two snapshots claiming the same
`base_id` apart when the definition itself changed underneath them: a
maintenance release altering a seed, or an operator editing their golden-image
file. A name can mean different things at different times; a digest cannot.

### 3.2 Lock plan — `debark.lock/v1`

**What it is.** Exactly what the online solve chose and why — the install
plan (exact versions, never "whatever is newest") and the provenance record
for every file that crosses the gap. **Produced by** `debark build`, once
per build (including `--update` runs). **Carried as** `lock.json` at the
bundle root. **Lifetime.** Immutable once a bundle is exported; a re-run
produces a new lock, not an edited one. Field reference in
[`lock.v1.schema.json`](../api/schema/lock.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.lock/v1` |
| `snapshot_digest` | string | canonical-content digest (§1.3(b)) of the snapshot this plan was resolved against |
| `request_digest` | string | canonical-content digest (§1.3(b)) of a deliberately narrowed projection of the `BuildRequest` — see below, it is not the digest of the whole request |
| `target` | object | restated target identity; `install` refuses an architecture mismatch |
| `resolver` | object | `backend` (`local`/`container`, never `auto` — that value is resolved away before the lock is written), apt/dpkg versions that actually ran, `phased_updates` policy applied, `install_recommends` effective value, `solver_divergence` when the resolving apt differed from the target's |
| `packages[]` | array | every file in the bundle pool: name/arch/version/filename/size/sha256, `origin` (archive URI, suite, component, release digest, key fingerprint — or a vendor URL/local path; this is the provenance of one `.deb`, and is unrelated to the snapshot's own `origin`), `reason` (`requested`, `upgrade`, `external`, or `dependency-of:<pkg>`), `publisher_verification` (`apt-signed`, `url-unverified`, `user-digest`, `user-signature`), `flags[]` |
| `install[]` | array of string | the exact `name=version` set `install` requests — the subset that was requested or is a dependency of something requested, never the whole pool |
| `closed_world_check` | object | pre-export proof the finished bundle resolves against itself offline (item 4) |
| `warnings[]`, `unresolved[]` | array | structured, not log lines — see §2, item 3 |
| `stats` | object | `added`/`removed`/`unchanged`/`bytes` for this run, plus `downloaded_bytes`, which is always `0` here — see below |

`request_digest` answers one question — "were these two builds asked for the
same thing?" — and is computed over a narrowed projection of the
`BuildRequest`, not over the whole of it. Package names and vendor URLs
(with the operator's `--digest` assertion) are hashed verbatim, because they
are names rather than locations, and every list is sorted first so a
reordering does not move the digest. Every host location is kept out:
`store_dir` is dropped outright, `policy_ref`, `approved_keys_ref`,
`embed_binary` and the file, directory and list inputs are reduced to their
basenames, and `output` and `snapshot_ref` are excluded entirely. The same
logical build therefore digests identically from a CI checkout and from a
laptop, which matters because `request_digest` is a `lock.json` field and so
cascades into `LockDigest`, the manifest's `bundle_id`, and the bundle id
`README.txt` prints. The content those paths pointed at is not lost: every
local `.deb` that actually resolved is recorded by name, version and SHA-256
in this same lock's `packages[]`, and the snapshot's own identity is in
`snapshot_digest` right beside it — an auditor asking "the same request
against the same snapshot" reads both fields, not one.

`stats.downloaded_bytes` is present and always `0`. It counted the bytes of
this run's selections that the local content store did not already hold,
which is a fact about the machine rather than about the request: the same
`build` run twice reported N and then 0, giving two different `lock.json`
bytes, two different `LockDigest` values and two different bundle ids for
one request. Recording it broke the byte-identical-rebuild property for no
reason other than having built the bundle once before, so the lock no longer
carries it. The number itself is still exact where it is a fact about the
run rather than about the bundle — `BuildResult.stats.downloaded_bytes`
, which is what `build` prints as "fetched this run". `added`,
`removed` and `unchanged` are *not* cleared and are not the same problem:
they are measured against the previous contents of the output directory,
which an auditor rebuilding from scratch has empty by definition.

**Example** (two packages, one apt-sourced, one vendor):

```json
{
  "schema_version": "debark.lock/v1",
  "created_at": "2026-09-03T12:05:00Z",
  "snapshot_digest": "94a1a6bd87442a401b30634b46577a677f31a4ad066a943fb646ff6899054df0",
  "request_digest": "80d51bb829a6e379a6f43309aa6b28d206ff39953816b748267b16bed58be497",
  "target": { "distro_id": "debian", "version_id": "12", "codename": "bookworm", "arch": "amd64" },
  "resolver": {
    "backend": "local", "apt_version": "2.6.1", "dpkg_version": "1.21.22",
    "phased_updates": "target-machine-id", "install_recommends": true
  },
  "packages": [
    { "name": "vlc", "arch": "amd64", "version": "3.0.21-1build1",
      "filename": "pool/v/vlc/vlc_3.0.21-1build1_amd64.deb", "size": 1957888,
      "sha256": "b6fd458a7205924b78c0b1bcc6730fc69ee72e093dd49f56e106479057f9441d",
      "origin": { "uri": "http://deb.debian.org/debian", "suite": "bookworm", "component": "main" },
      "reason": "requested", "publisher_verification": "apt-signed" },
    { "name": "acme-agent", "arch": "amd64", "version": "2.1.0",
      "filename": "pool/a/acme-agent/acme-agent_2.1.0_amd64.deb", "size": 88000,
      "sha256": "5ca54ae935ebe6a895ac8f31606e4db256628778d35f562ff44ee0aba658c349",
      "origin": { "uri": "https://vendor.example.com/acme-agent_2.1.0_amd64.deb" },
      "reason": "external", "publisher_verification": "user-digest", "flags": ["network-postinst"] }
  ],
  "install": ["acme-agent=2.1.0", "vlc=3.0.21-1build1"],
  "closed_world_check": { "result": "ok",
    "command_digest": "19c29e0fbf72ec4bf6d83c5bf7f54efb97921f94480e70af8d1c94b49b7ce4b4",
    "output_digest": "ea08eeadaaf778f1c36d5b880e677b8277fddaff3704cde3ea029b85fbd9a34f" },
  "warnings": [],
  "stats": { "added": 2, "removed": 0, "unchanged": 0, "bytes": 2045888, "downloaded_bytes": 0 }
}
```

### 3.3 Manifest — `debark.manifest/v1`

**What it is.** The one object that is signed, and the only thing the target
has to trust: it binds the snapshot, the lock, the generated repository
metadata and every other file in the bundle together. **Produced by**
`debark build`, last, after everything else in the bundle is final.
**Carried as** `debark.manifest.json` at the bundle root, written indented
for humans. **Lifetime.** Immutable; see §1.4 for how it is verified.
Field reference in
[`manifest.v1.schema.json`](../api/schema/manifest.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.manifest/v1` |
| `bundle_id` | string | stable id derived from `lock_digest` and `created_at` (first 16 hex characters of the canonical digest of `{created_at, lock_digest}`) |
| `format_version` | integer | bundle *layout* version (the tree shape); currently `1` |
| `tool` | object | `{name, version, build_digest?, edition}` — `edition` is `community`/`official`/`enterprise`, branding metadata only, never a feature gate |
| `snapshot_digest`, `lock_digest` | string | canonical-content digests (§1.3(b)) |
| `repository` | object | whole-file digests (§1.3(a)) of `repo/Packages`, `repo/Release`, and optionally `Packages.gz`/`InRelease`/`Release.gpg`, plus `package_count`/`pool_bytes`. `inrelease_sha256`/`release_gpg_sha256` are reserved: `verify` checks them when present, but nothing writes them today (layer 3) |
| `files[]` | array | every bundle file **except `debark.manifest.json` and `debark.manifest.sig` themselves** — path (forward-slashed, relative to the bundle root), size, whole-file SHA-256 (§1.3(a)) |
| `target` | object | distro/version/codename/arch, restated for `install`'s precondition check |
| `sbom_ref`, `evidence_ref` | string | relative paths to the optional SBOM and evidence files |

**Example:**

```json
{
  "schema_version": "debark.manifest/v1",
  "bundle_id": "bd-80be0631f286fe7e",
  "created_at": "2026-09-03T12:10:00Z",
  "format_version": 1,
  "tool": { "name": "debark", "version": "1.0.0",
    "build_digest": "353797a12e4fbdb499f56f11b2b24ab86179b605b65b259358c00aed8b53541d", "edition": "official" },
  "snapshot_digest": "94a1a6bd87442a401b30634b46577a677f31a4ad066a943fb646ff6899054df0",
  "lock_digest": "80be0631f286fe7e12719ab399568f969c31d98f4bf6321666f58cc15e21ba4f",
  "repository": {
    "packages_sha256": "4a7c25efce77741075c75312ff07c5a6033ad5d5f6b5705ba4aad8284091456a",
    "release_sha256": "b6fd458a7205924b78c0b1bcc6730fc69ee72e093dd49f56e106479057f9441d",
    "package_count": 2, "pool_bytes": 2045888
  },
  "files": [
    { "path": "lock.json", "size": 4096, "sha256": "80be0631f286fe7e12719ab399568f969c31d98f4bf6321666f58cc15e21ba4f" },
    { "path": "repo/Packages", "size": 12000, "sha256": "4a7c25efce77741075c75312ff07c5a6033ad5d5f6b5705ba4aad8284091456a" },
    { "path": "repo/pool/v/vlc/vlc_3.0.21-1build1_amd64.deb", "size": 1957888,
      "sha256": "353797a12e4fbdb499f56f11b2b24ab86179b605b65b259358c00aed8b53541d" }
  ],
  "target": { "distro_id": "debian", "version_id": "12", "codename": "bookworm", "arch": "amd64" },
  "evidence_ref": "evidence.json"
}
```

### 3.4 Signature — `debark.signature/v1`

**What it is.** The detached signature file: one or more independent
signature blocks over the manifest's canonical bytes, so more than one party
can co-sign a bundle. **Produced by** `debark build`'s signing step, or
`debark verify`'s companion for a later, additional signature. **Carried
as** `debark.manifest.sig` beside the manifest. **Lifetime.** A bundle may
gain additional signature blocks over time (a second approver co-signing);
existing blocks are never edited or removed by debark tooling. Field
reference in [`signature.v1.schema.json`](../api/schema/signature.v1.schema.json).
See §1.4 for the verification recipe.

| Field | Type | Notes |
|---|---|---|
| `schema` | string, const | `debark.signature/v1` (note the key is `schema`, not `schema_version` — see §4) |
| `manifest_sha256` | string | canonical-content digest (§1.3(b)) of the manifest bytes that were signed |
| `signatures[]` | array, ≥1 | each: `signer_kind` (`ed25519-file`, `gpg`, `sigstore`, or `plugin:<name>`), `key_id`, `algorithm`, `created_at`, `signature` (base64), optional `comment` |

**Example:**

```json
{
  "schema": "debark.signature/v1",
  "manifest_sha256": "b6fd458a7205924b78c0b1bcc6730fc69ee72e093dd49f56e106479057f9441d",
  "signatures": [
    { "signer_kind": "ed25519-file", "key_id": "dGVzdC1rZXktaWQ", "algorithm": "ed25519",
      "created_at": "2026-09-03T12:11:00Z",
      "signature": "ZGViZmVycnktdGVzdC1zaWduYXR1cmUtYnl0ZXMtMDE=",
      "comment": "release engineering key, 2026" }
  ]
}
```

### 3.5 Evidence — `debark.events/v1`

**What it is.** Structured build/install events, written as if an auditor
will read them in five years — objects, not log strings. **Produced**
incrementally throughout `build`, `verify` and `install`. **Carried as**
`evidence.json` (a `Document`: a header plus every recorded `Event`) inside
the bundle; the same `Event` shape also streams as one-JSON-object-per-line
NDJSON on stdout under `--json-events`, which is why `Event` repeats its own
`"schema"` field on every line — a single streamed line is self-describing
with no wrapping `Document` around it. **Lifetime.** `evidence.json` is
immutable once written into a bundle; installation on the target appends to
a *separate*, local-only evidence log that never leaves the machine and is
not part of the bundle format. Field reference in
[`events.v1.schema.json`](../api/schema/events.v1.schema.json).

| Field (`Document`) | Type | Notes |
|---|---|---|
| `schema` | string, const | `debark.events/v1` |
| `created_at` | string | when the file was finalised |
| `context` | object | run identity: tool version, edition, backend, build host kind, an operator identity only if the operator supplied one — never a machine fingerprint, never collected unasked |
| `events[]` | array of `Event` | see below |

| Field (`Event`) | Type | Notes |
|---|---|---|
| `schema` | string, const | `debark.events/v1` |
| `ts` | string | RFC 3339 UTC |
| `type` | string, enum | one of 22 stable, additive-only event types: `snapshot.created`, `snapshot.loaded`, `backend.selected`, `apt.update`, `apt.resolve`, `fetch.file`, `input.external`, `store.hit`, `policy.finding`, `doctor.finding`, `repo.indexed`, `closed_world.checked`, `manifest.signed`, `bundle.assembled`, `bundle.pruned`, `verify.result`, `install.plan`, `install.result`, `warning`, `build.started`, `build.finished`, `progress` |
| `level` | string | `info` (default), `warn`, `error` |
| `msg` | string | short human sentence; machine consumers read `attrs`, not this |
| `attrs` | object | type-specific fields, always JSON-encodable, never a secret |

**Example:**

```json
{
  "schema": "debark.events/v1",
  "created_at": "2026-09-03T12:12:00Z",
  "context": { "tool_version": "1.0.0", "edition": "community", "backend": "local" },
  "events": [
    { "schema": "debark.events/v1", "ts": "2026-09-03T12:00:01Z", "type": "snapshot.loaded",
      "level": "info", "msg": "snapshot loaded",
      "attrs": { "snapshot_digest": "94a1a6bd87442a401b30634b46577a677f31a4ad066a943fb646ff6899054df0" } },
    { "schema": "debark.events/v1", "ts": "2026-09-03T12:00:09Z", "type": "warning",
      "level": "warn", "msg": "external package acme-agent is url-unverified" }
  ]
}
```

### 3.6 Build job — `debark.buildjob/v1`

**What it is.** The job model that keeps the resolution/build engine
callable by something other than the CLI: `BuildRequest` in, `BuildResult`
out. **Produced/consumed by** whatever calls the engine — today only the CLI,
constructing a `BuildRequest` from flags and receiving a `BuildResult`; a
future controller could store requests and call the same entry point.
**Never written into a bundle** — this is a wire/API object, not a bundle
artefact. **Lifetime.** Both shapes may gain new optional fields across minor
versions of the tool without a schema bump, in the same additive spirit as
the events list; a field removal or meaning change would be a new schema
version, same as everywhere else. Field reference in
[`buildjob.v1.schema.json`](../api/schema/buildjob.v1.schema.json), which
validates either shape via `oneOf`.

| Field (`BuildRequest`) | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.buildjob/v1` |
| `snapshot_ref` | string | path to a snapshot archive or extracted directory |
| `inputs` | object | `packages[]`, `urls[]` (each with an optional operator-supplied `sha256`), `files[]`, `local_dirs[]`, `list_files[]` |
| `options` | object | `recommends`, `upgrades`, `update_mode` (`additive`/`refresh`), `prune`, `arch_override`, `mirror_overrides`, `backend` (`auto`/`local`/`container`), `image`, `policy_ref`, `approved_keys_ref`, `acknowledge_redistribution`, `embed_binary`, `sbom`, `store_dir`, `closed_world_check` |
| `output` | object | `path`, `format` (`dir`/`tar`), `sign` (signer selection — see note below) |

> **Note on `output.sign`.** `Output.Sign` is a non-pointer struct field
> tagged `json:"sign,omitempty"` in the Go type. Go's `encoding/json`
> `omitempty` has no effect on struct-typed fields (only on pointers, maps,
> slices, strings and the empty/zero basic types) — see §5 of this document
> and the schema comment on this property. In practice `sign` is **always**
> present on the wire, at minimum as `{}`.

| Field (`BuildResult`) | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.buildjob/v1` |
| `lock_ref`, `manifest_ref`, `bundle_path`, `bundle_id` | string | where the outputs landed |
| `signed` | boolean | at least one signature block was written |
| `stats` | object | added/removed/unchanged/bytes/downloaded_bytes/package_count/duration_seconds. `downloaded_bytes` is exact here — it is a fact about the run, which is why it is reported and not recorded in `lock.json` |
| `warnings[]`, `unresolved[]`, `fetch_failed[]` | array of string | human-facing summaries. `fetch_failed` is every external input that did not make it into the bundle — a URL that did not download **or** a local `.deb` that could not be read — as the operator's literal input string. It says *which*, never *why* |
| `fetch_failures[]` | array of object | `fetch_failed` with the reason attached: `{input, reason, detail?}`, one entry per `fetch_failed` entry, in the same order, with `input` equal to it. `reason` is one of `unreachable`, `tls-untrusted`, `http-status`, `digest-mismatch`, `not-a-deb`, `too-large`, `refused-redirect`, `unreadable`, `cancelled`, `bad-input`, `local-storage`, `other`. `detail` is the human sentence, already redacted. See the note below |
| `exit_class` | string, enum | mirrors `core/dferr.Class.String()` exactly: `success`, `usage`, `environment`, `incomplete`, `verification`, `resolution`, `policy`, `target-mismatch` |

> **Note on `fetch_failures` and why `fetch_failed` was not simply widened.**
> An unreachable host, a SHA-256 that did not match and a certificate this
> machine does not trust are three different problems with three different
> next steps, and until this field existed they were one line in the result
> document; the reason lived only in the text of a warn-level
> `input.external` event, so a caller reading the result had to say "a
> download failed" and stop. Turning `fetch_failed` from an array of string
> into an array of object would have been a *meaning change*, which §0 and
> the Lifetime note above make a `debark.buildjob/v2` matter — every
> existing consumer broken for one added attribute. Adding an optional field
> is the additive case those same rules allow, so both fields ship and
> `fetch_failed` keeps its exact shape. They are derived from one list inside
> the engine, which is what makes their correspondence a property rather than
> a promise. The `reason` set is itself additive: a reader must have a
> default arm and should treat an unrecognised value as `other`.

### 3.7 Plugin protocol — `debark.plugin/v1`

**What it is.** An out-of-process, language-agnostic plugin protocol carried
as single-line JSON over stdio — no `.so` files, no Go ABI coupling, no gRPC
dependency. Only the `sign` capability is defined in v1. **Produced/consumed
by** the debark host process and a plugin executable, in-memory over a
pipe; never written into a bundle. **Lifetime.** The protocol version
(`debark.plugin/v1`) is what changes if the wire shape ever needs to;
adding a new *method* or *capability* string is additive and does not bump
it. Field reference in
[`plugin.v1.schema.json`](../api/schema/plugin.v1.schema.json).

Wire discipline (one JSON object per line, in order):

```
plugin -> host   {"protocol":"debark.plugin/v1","name":"acme-hsm","version":"1.4.0","capabilities":["sign"]}
host   -> plugin {"id":"1","method":"sign","params":{"purpose":"debark.manifest/v1","payload":"<base64>"}}
plugin -> host   {"id":"1","result":{"signature":"<base64>","algorithm":"ed25519","key_id":"…"}}
```

| Message | Required fields | Notes |
|---|---|---|
| `Handshake` | `protocol` (const), `name`, `version`, `capabilities[]` | the plugin's first line on start |
| `Request` | `id`, `method` (`sign`, `key_info`, `shutdown`) | `params` shape depends on `method` |
| `Response` | `id`, exactly one of `result`/`error` | `result` shape depends on the request's `method` |
| `SignParams` | `purpose`, `payload` (base64) | carried in a `sign` request's `params` |
| `SignResult` | `signature` (base64), `algorithm`, `key_id` | carried in a `sign` response's `result` |
| `KeyInfoResult` | `key_id`, `algorithm` | carried in a `key_info` response's `result` |
| `Error` | `code` (`unsupported-method`/`key-unavailable`/`user-declined`/`internal`), `message` | carried in any error response |

### 3.8 Verify report — `debark.verifyreport/v1`

**What it is.** The machine-readable answer to "can this artifact be
trusted." **Produced by** `debark verify --json` (and internally by
`install`, which never proceeds without one). **Never written into a
bundle.** **Lifetime.** A fresh report every time verification runs; nothing
about it is persisted by debark itself, though a caller is free to keep
one. Field reference in
[`verifyreport.v1.schema.json`](../api/schema/verifyreport.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.verifyreport/v1` |
| `ok` | boolean | true only if every check passed. **A bundle verified with `--allow-unsigned` has `ok: true` and `signed: false` — a caller must check both, never `ok` alone, to learn whether a bundle was actually authenticated.** |
| `signed` | boolean | whether at least one signature block was present |
| `signatures[]` | array | per-block `valid` (cryptographically checks out) and `trusted` (against an operator-supplied key) — these can differ: a structurally valid signature by an untrusted key has `valid: true, trusted: false` |
| `files_checked`, `bytes_checked` | integer | how much was actually verified |
| `problems[]` | array | every failure found; a stable `kind` enum of 16 values such as `signature-invalid`, `file-digest-mismatch`, `file-not-regular`, `same-media-key-refused` |
| `warnings[]` | array of string | non-fatal observations about a bundle that was still accepted: an unsigned bundle taken on `--allow-unsigned`, a gpg check that used the machine's default keyring. Nothing about the bundle's contents is a warning — an unlisted file and a non-regular entry are both `problems[]` |
| `target` | object | restated from the manifest |

### 3.9 Install report — `debark.installreport/v1`

**What it is.** The machine-readable result of `debark install --json`
(also used for `--status` and `--dry-run`, where `applied` is `false`).
**Never written into a bundle.** **Lifetime.** One per invocation; the
target-side local evidence log is where a durable trail of these
lives, outside the bundle format. Field reference in
[`installreport.v1.schema.json`](../api/schema/installreport.v1.schema.json),
which carries its own local copy of the verify-report shape so the
document validates standalone without fetching a second schema file.

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.installreport/v1` |
| `verify` | object | the embedded verify report; install never proceeds without one |
| `applied` | boolean | false for `Plan`, `--status`, `--dry-run` |
| `ok` | boolean | true once the requested state was reached |
| `to_install[]`, `to_upgrade[]`, `to_remove[]` | array of string | `name=version`, parsed from apt's simulation |
| `already_current` | integer | bundle packages already at the locked version |
| `target_expected`, `target_actual` | object | what the lock was built for, and what was actually found |
| `base_divergence` | object, optional | present only for a bundle built from a *synthesized* snapshot (ADR-014): `base_id`, `assumed` (how many packages the base claimed the target already had) and `missing[]` (those assumed `name:arch` entries dpkg does not report installed here, sorted, complete — the human warning names only a sample). A short bundle is the failure debark exists to prevent, so this is reported as a finding and a warning, never a refusal. Absent for a bundle built from a captured snapshot, which measured this machine and so has no assumption to diverge from |
| `warnings[]` | array of string | non-fatal notes about an install that still went ahead: a tolerated codename mismatch, a dpkg fallback used, holds bypassed, and the base divergence above rendered for a human |
| `problems[]` | array of string | the reasons `ok` is false; empty on success |
| `apt_output_digest` | string | whole-file digest of the captured apt output, kept in the local evidence log |

### 3.10 Doctor report — `debark.doctorreport/v1`

**What it is.** What is likely to go wrong on the far side of the gap:
maintainer scripts that want the network, snap-shim packages, DKMS packages
without matching headers, redistribution terms. Every finding is a
heuristic, worded as one: `"no obvious network action found"`, never
`"proven safe"`. **Produced by** `debark doctor --json`. `doctor` never
fails a build by itself; policy does. **Never written into a bundle** as its
own file, though its findings feed `packages[].flags` in the lock. **Lifetime.**
One report per invocation. Field reference in
[`doctorreport.v1.schema.json`](../api/schema/doctorreport.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.doctorreport/v1` |
| `findings[]` | array | each: `check` (one of 7 stable identifiers), `severity` (`note`/`warn`), `package`, `version`, `message`, `evidence`, `flag` (the lock flag this corresponds to, if any) |
| `scanned` | integer | packages actually examined |
| `summary` | object | finding counts keyed by severity |

### 3.11 Policy — `debark.policy/v1`

**What it is.** A local, operator-owned policy file, evaluated against a
resolved plan; every field is optional and an empty policy finds nothing.
**Authored by** the operator (conventionally as YAML — its keys are
identical to the JSON field names below), **read by** `debark build
--policy FILE`. **Lifetime.** Operator-maintained; debark never writes one.
A `SeverityDeny` finding fails the build with exit code 6. Field reference in
[`policy.v1.schema.json`](../api/schema/policy.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.policy/v1` (the only required field) |
| `allow_components[]`, `deny_components[]` | array of string | archive components, e.g. `main`, `non-free` |
| `allow_packages[]`, `deny_packages[]` | array of string | name globs |
| `require_signed_publisher` | boolean | denies any file whose `publisher_verification` is not one of `apt-signed`, `user-digest`, `user-signature`. It is an allow-list, not a test for the one bad value, so an empty, misspelled or unrecognised claim is treated as unsigned and denied rather than passed |
| `allow_url_inputs` | boolean | default true |
| `approved_keys[]` | array of string | archive key fingerprints, uppercase hex |
| `deny_flags[]` | array of string | denies any package carrying one of these lock flags |
| `default_severity` | string | `info`/`warn`/`deny`, applied to rules that don't state their own |

### 3.12 Store index — `debark.storeindex/v1`

**What it is.** The content-addressed object store's own metadata index —
package identity for every object the store holds, so `store ls` and prune
decisions work without opening every `.deb`. **Produced/consumed by** the
store implementation itself. **Internal**: it never crosses the gap and is
not part of a bundle, but is versioned like everything else. **Carried as**
`index.json` at the store root (conventionally
`~/.local/share/debark/store/`). **Lifetime.** Updated in place as objects
are added or garbage-collected; not a append-only log. Field reference in
[`storeindex.v1.schema.json`](../api/schema/storeindex.v1.schema.json).

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.storeindex/v1` |
| `entries[]` | array | each: `digest` (the object's own address, whole-file SHA-256), `name`, `version`, `arch`, `size`, `filename`, `added_at`, `user_supplied` (never pruned or GC'd implicitly) |

### 3.13 Base definition — `debark.base/v1`

**What it is.** The description of a *baseline*: a stock release, its
architecture, the apt sources it resolves against, and the seed packages
whose closure stands for "a default install of it". It is the input to
`snapshot from-base` and `build --base`, for a target that has not been
installed yet and so cannot be snapshotted (ADR-014). **Authored by** the
operator as YAML, or compiled into the binary — the two are the same type
with no privileged shape, which is what keeps "which bases do you support?"
from becoming unbounded maintenance across every derivative. **Read by**
`debark snapshot from-base BASE` and `debark build --base BASE`, where
`BASE` is a builtin id or a path to a file. **Never carried in a bundle:**
what travels is the *snapshot* the definition produced, which records the
definition's id, source and digest in `origin`. **Lifetime.**
Operator-maintained; debark never writes one.

This is the one format on this page with no published JSON Schema, because
it is an input an operator writes rather than a document debark emits and
hashes. The parser refuses unknown fields rather than ignoring them, which
is the same closed-document rule `additionalProperties: false` enforces
elsewhere: a misspelled `recomends: true` that quietly resolved with
recommends *off* would produce a base whose claim differs from what the file
says, with nothing to show the operator that it had.

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.base/v1` |
| `id` | string | `<distro>:<version>/<variant>`, e.g. `ubuntu:26.04/desktop`. A bare `<distro>:<version>` means the **`minimal`** variant — the smallest claim, because a bare id is the least informed request the parser can receive. A builtin id always wins over a file of the same name, so a file lying about in the working directory can never silently redefine a documented base |
| `description` | string, optional | one line, shown by `snapshot list-bases` |
| `distro_id`, `version_id`, `codename` | string | the target identity, written straight into the synthesized snapshot's `target` and used to select the container image. `codename` is required here — unlike in a captured snapshot, where a stripped-down system may genuinely lack `VERSION_CODENAME` — because every suite name in `sources` is built from it and a definition is written rather than measured |
| `variant` | string, optional | `desktop`, `server`, `minimal`, or an operator's own word. Documentation, not behaviour: `seeds` is what actually differs |
| `arch` | string | the dpkg architecture this definition is materialised for. A file may pin one, or leave it empty to accept whatever is requested; a pinned architecture that disagrees with the request is an error, never silently overridden, because a file that pins one is making a claim |
| `seeds[]` | array of string | the metapackages whose closure stands for a stock install. At least one required. Resolved by the release's own apt in a private root where nothing is installed; **never expanded by debark** (ADR-001) |
| `recommends` | boolean | `APT::Install-Recommends` for the seed resolution. Defaults to **false**, and that default is load-bearing rather than conservative by habit: a recommended package apt would pull in but the real installer did not take is a package the base would claim as installed when it is not, which is the direction that produces a short bundle. Set it only having measured that a real image matches |
| `sources` | string | the target's apt sources as a deb822 document (`Types:`/`URIs:`/`Suites:`/`Components:`/`Signed-By:`). Written verbatim into the synthesized snapshot as `/etc/apt/sources.list.d/<distro_id>.sources`, so it is both what the seeds resolve against now and what a later `build` against that snapshot resolves against — one text, one meaning |
| `excludes[]` | array of string, optional | packages the seed closure names that this base must **not** claim the target already has. It is the one place a package name is written by hand, and the asymmetry is what makes that acceptable: an exclusion only ever makes the base's claim smaller, moving a package from "assumed present" to "must be carried", so it can enlarge a bundle but never shorten one. Each entry in a builtin definition names the measurement that put it there (`docs/experiments/E8-base-fidelity.md`); an unmeasured exclusion is a guess, and this format is for recording what was measured |
| `keyrings[]` | array of string, optional | absolute paths, **on the machine that runs the resolution**, to the archive keyrings the `Signed-By` lines name. Read from the resolving system rather than shipped inside debark: the tool must never become a distributor of archive keys (layer 1). The two machines a base is ever resolved on both already have them — a host of the same distribution, or the pinned container image of the target release itself |

**Substitution.** Exactly three placeholders are recognised, and they are
substituted in `sources`, `keyrings[]`, `seeds[]` and `excludes[]` before
anything runs:

| Placeholder | Expands to |
|---|---|
| `${codename}` | `codename`, e.g. `resolute` |
| `${version_id}` | `version_id`, e.g. `26.04` |
| `${arch}` | the materialised `arch`, e.g. `amd64` |

There is deliberately nothing richer — no conditionals, no arithmetic, no
environment lookup, no other names. A base definition is a file an operator
commits to git and a builder later runs apt against; the set of things it
may legitimately vary is small and knowable, and anything richer would be a
template engine deciding which archive a build talks to. A `${...}` left
unsubstituted in `sources` is refused rather than passed through as a
literal.

**Example** — the builtin `ubuntu:26.04/desktop`, materialised for `amd64`
and rendered by the same encoder the parser reads back, so this is exactly
what the format accepts and a reasonable starting point for an operator's
own file:

<!-- debark:generated base.Marshal(base.Lookup("ubuntu:26.04/desktop", "amd64")); checked by core/base/docs_test.go -->

```yaml
schema_version: debark.base/v1
id: ubuntu:26.04/desktop
description: 'Ubuntu 26.04 (resolute) — a desktop install: the minimal system plus the default desktop environment'
distro_id: ubuntu
version_id: "26.04"
codename: resolute
variant: desktop
arch: amd64
seeds:
  - ubuntu-desktop-minimal
recommends: false
sources: |
  Types: deb
  URIs: http://archive.ubuntu.com/ubuntu
  Suites: resolute resolute-updates resolute-backports
  Components: main restricted universe multiverse
  Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

  Types: deb
  URIs: http://security.ubuntu.com/ubuntu
  Suites: resolute-security
  Components: main restricted universe multiverse
  Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
excludes:
  - lsb-base
keyrings:
  - /usr/share/keyrings/ubuntu-archive-keyring.gpg
```

An operator's own file is the same shape, and is usually where the
placeholders earn their place — one definition that renders for whichever
architecture is asked for:

```yaml
schema_version: debark.base/v1
id: acme:soe-2026
distro_id: ubuntu
version_id: "24.04"
codename: noble
# arch is omitted, so this one file renders for whichever --arch is asked
# for; pin it instead when the file describes one architecture's image.
seeds: [acme-soe-base, openssh-server]
recommends: false
sources: |
  Types: deb
  URIs: http://mirror.acme.internal/ubuntu
  Suites: ${codename} ${codename}-updates ${codename}-security
  Components: main restricted universe
  Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
keyrings: [/usr/share/keyrings/ubuntu-archive-keyring.gpg]
```

The builtin table covers exactly the releases this project commits to
CI-testing — Debian 12 and 13, Ubuntu 22.04, 24.04 and 26.04 — each in
`minimal`, `server` and `desktop` variants. `debark snapshot list-bases`
prints them as a table, or under `--json` as a `debark.baselist/v1` object
. There is no hosted catalogue and no network call to anything the
debark project runs, here or anywhere else: a catalogue would make every
`from-base` an outbound request revealing which release a site is
provisioning, which is the same sensitive inventory the no-telemetry rule
exists to protect. `from-base` needs exactly the
network access `build` already needs — the distribution's own archive, from
the machine that already reaches it — and no more.

### 3.14 Base listing — `debark.baselist/v1`

**What it is.** Which baseline definitions a debark binary has
compiled in, projected for one architecture. **Produced by** `debark
snapshot list-bases --json`; the same command without `--json` prints the
same set as a table. **Never written into a bundle**, never hashed, never
signed — it is a listing, not an artefact, and it is versioned all the same
because every `--json` output is a versioned, schema-published object
(ADR-012). **Lifetime.** One per invocation; it describes the binary that
printed it and nothing else. Field reference in
[`baselist.v1.schema.json`](../api/schema/baselist.v1.schema.json).

The listing is answered entirely from inside the binary. There is no
catalogue to fetch and nothing to reach for: see §3.13 for why an outbound
request naming the release a site is provisioning would be the wrong shape
for this tool.

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string, const | `debark.baselist/v1` |
| `arch` | string | the dpkg architecture the listing was rendered for, from `--arch` or this machine's own. Part of the answer rather than context around it: a base's `sources`, and therefore its `digest`, differ per architecture, so "which bases exist" has no architecture-free answer |
| `bases[]` | array | one entry per compiled-in definition, in the same order the human table prints them — by release, then variant — so the two renderings of one listing agree row for row |

Each entry is a projection of the definition, not the definition
itself: `sources` and `keyrings[]` are omitted, because they are long and are
the part an operator overrides, and `digest` is added.

| Field | Type | Notes |
|---|---|---|
| `id` | string | the base id, as passed to `snapshot from-base` or `build --base`, and as recorded in a synthesized snapshot's `origin.base_id` |
| `description` | string, optional | the definition's one-line summary |
| `distro_id`, `version_id`, `codename` | string | the identity a target running this baseline would report |
| `variant` | string, optional | `desktop`, `server`, `minimal`, or an operator's own word |
| `arch` | string | the architecture this row was materialised for; equals the listing's `arch`, restated so one entry stays meaningful when a consumer pulls rows out of the array |
| `seeds[]` | array of string | the metapackages whose closure stands for a stock install. What the closure actually contains is decided by the release's own apt at synthesis time and is deliberately not listed here |
| `excludes[]` | array of string, optional | packages that closure names which this baseline does not claim the target already has. Shown, unlike `sources` and `keyrings[]`, because it changes what the base *claims*: two bases with the same seeds and different excludes assume different machines, and only `digest` would otherwise say so |
| `recommends` | boolean | whether the seed resolution runs with `APT::Install-Recommends` on; `false` for every builtin, for the reason §3.13 gives |
| `digest` | string | SHA-256 of the canonicalised definition, lowercase hex — the same value a snapshot synthesized from it records in `origin.source_digest`. An id is a name and a name can mean different things at different times; this is what tells an operator which listed base a snapshot in hand was actually built from |

## 4. Field-name irregularity: `schema` vs `schema_version`

Every format above uses `schema_version` as its first field's key, with one
deliberate exception: `debark.signature/v1` and `debark.events/v1`
(`Document` and `Event` alike) use the key `schema` instead. This is not a
typo — it matches the design's own Appendix A wire examples for both formats
verbatim, and for `Event` specifically it matters mechanically: an `Event`
is also streamed standalone, one per NDJSON line, with no `Document` wrapper
around it, so it needs to be self-describing on its own. `schema` and
`schema_version` are never used interchangeably within one format; check the
field table above for the one that applies.

## 5. A Go idiom note that affects the wire format

`encoding/json`'s `omitempty` tag option has no effect on a struct-typed
field that is not a pointer — only on pointers, maps, slices, strings and
the zero value of basic types. `api/buildjob/v1.Output.Sign` is declared as
a plain (non-pointer) `SignOptions` with `json:"sign,omitempty"`; the tag is
present but inert, so `sign` is emitted on every `BuildRequest`, never
omitted, even when it is an empty object. `buildjob.v1.schema.json` and §3.6
above document `sign` as **required** to match this actual wire behaviour,
not the field tag's apparent intent. No other field across the thirteen
schema-backed formats above has this issue — see the drift-checking test in
`api/schema/schema_test.go` for the systematic check. The base definition
 is outside that check by construction: it is YAML an operator
authors, is never serialised by `encoding/json`, and has no schema file for
the test to compare against. It is not therefore unguarded —
`core/base/docs_test.go` holds the field table and its worked example to
`core/base.Definition` and to the encoder the parser reads back, which is
the same drift question asked of the one format that cannot be asked it in
JSON Schema.
