# debark threat model

This document is written for the person whose job is to decide whether
debark is allowed to touch a network they are responsible for. It is
adversarial by design: its purpose is to give that person every reason they
would otherwise have to find for themselves to reject the tool, stated plainly
and in one place, alongside the specific code and (where they exist) tests
that back each claim.

**Status of this document: 2026-09-03, mid-implementation.** debark is built
contract-first (`docs/dev/contract-brief.md`): the schemas
and interfaces in this repository are frozen, but several implementations
behind them were still landing, concurrently, while this document was written.
Every claim below that depends on code is cited to a specific file and line
**as it existed at the time stated next to that claim**, and marked with its
implementation status. Do not read a citation as "true forever" — read it as
"true as of the timestamp given, and here is exactly what to re-check before
you rely on it." This document should be re-verified against `core/verify`,
`core/sign`, `core/snapshot`, `core/bundle` and `core/install` before every
release, and the integration matrix is what turns the
claims here into standing proof rather than a snapshot-in-time reading.

> **Commit revisions below refer to the pre-publication history.** That
> history was squashed into a single commit when the project was opened, so
> the short SHAs quoted here do not resolve in this repository. They are kept
> as written because they are the evidence anchors this record was assembled
> against — read them as ordering ("before this change", "after it"), not as
> something to `git show`.

This document is the adversarial analysis. `SECURITY.md` (disclosure process)
and `docs/security-model.md` (user-facing explanation) both point here for the
"why" behind what they say.

---

## 1. Assets

What debark actually has to protect, in order of how much damage its
compromise does:

1. **The bundle's contents** — the `.deb` files, the repository indices
   (`Packages`, `Release`), the lock plan and the manifest. If an attacker can
   change what apt installs on the target without detection, they have code
   execution as root on a machine that was air-gapped specifically because it
   is too sensitive to expose to arbitrary code.

2. **The snapshot.** Say this plainly, because it is easy to treat a snapshot
   as inert metadata: **a snapshot is a sensitive inventory of an air-gapped
   machine.** It contains the exact installed package set and versions
   (`dpkg_status`) — which is a precise vulnerability fingerprint of a machine
   whose entire purpose may be that its vulnerability profile is not supposed
   to be knowable from outside; the apt sources and pins in use (which can
   reveal internal mirror hostnames, vendor relationships, or the existence of
   an OT/ICS-specific repository); every OpenPGP fingerprint and user ID the
   target trusts (`keyring_fingerprints`, `apt.trusted`); the target's
   architecture and foreign architectures (which can hint at emulation or
   legacy hardware); the machine id (unique, and — unless redacted — a stable
   correlation handle, `core/snapshot/types.go:80-83`); and, if the operator
   opted in, a hostname or ticket label. The snapshot is not just an input the
   builder consumes and discards: a copy of it (with any redactions the
   operator applied still absent — `core/snapshot/redact.go`) travels back
   inside the finished bundle as `snapshot.json`, so
   whoever eventually holds the bundle holds this too.

3. **The operator signing key.** Whoever holds it can make the target install
   anything. There is no revocation mechanism, no expiry enforced by the
   target, and (in the community edition) no HSM/KMS custody — see §6.

4. **The target's package database** (`/var/lib/dpkg/status`) — captured into
   every snapshot verbatim. Beyond the confidentiality concern in item 2, its
   *integrity* matters on the way in: apt in the private root resolves against
   this file, so a corrupted or crafted `dpkg_status` changes what the builder
   believes still needs to be added, which is exactly the class of "silently
   wrong bundle" failure the whole product exists to prevent.

5. **The operator's trust decision itself.** A manifest signature is a
   durable, portable encoding of "a specific key holder vouched for these
   exact bytes." Protecting the *integrity of that attestation* — so it can
   never be forged, replayed onto different bytes, silently stripped, or
   quietly downgraded to "unsigned" without the record showing it — is
   arguably the product's actual deliverable, separate from protecting the
   bytes it attests to. `core/verify` and the evidence/install-log trail are
   what make that attestation checkable years later.

---

## 2. Trust boundaries

| Boundary | debark's posture | Mechanism |
|---|---|---|
| **The online builder** | Trusted. This is where a human decides what a snapshot's request should resolve to, where apt runs, and where the signing key is used. debark has no technical control against a compromised or malicious builder — see §5 and §3 ("compromised builder host"). | N/A — trust, not verification |
| **Physical media crossing the gap** | Untrusted by default. This is the headline case the product exists for. | Manifest signature + per-file SHA-256 (`core/verify/verify.go`) |
| **The offline target** | Trusted to run the small, auditable `install` binary correctly, and trusted with root-equivalent apt/dpkg access, which is how installation works everywhere. Not trusted to be immune to a compromise that happened *before* the snapshot was taken — see adversary 2. | `core/install` refuses to act until `core/verify` says OK (`core/install/runner.go:83-96`) |
| **The container runtime** (build-side resolution when host apt doesn't match the target) | Trusted for the resolution *decision* it produces, pinned by image digest. A compromised image could bias what apt resolves to (still archive-signed, since apt inside the container still checks `InRelease`) or corrupt the local repository-generation step. debark cannot detect a compromised image; the manifest signature is what still catches any bundle byte that doesn't match what was actually signed, but it does not tell you *why* the wrong bytes were chosen. | Image digest pinning; otherwise no defense specific to debark |
| **The distribution archives** (Debian/Ubuntu mirrors) | debark never touches archive-sourced package bytes directly — apt does, and apt performs its own standard OpenPGP verification of `InRelease`/`Release` against the keys captured in the snapshot's `apt.trusted`/keyrings (`core/snapshot/keyrings.go`). Those keys are **captured policy, not an independent root of trust** — see adversary 2. | apt's own `apt-secure` mechanism; debark only records what it saw (`KeyringFingerprints`) |
| **Vendor download servers** (raw `.deb` URLs) | Untrusted, explicitly labelled. HTTPS from a vendor server is transport security only, never provenance. | `lock.VerifiedURLUnverified` unless the operator supplies `--digest` (`core/fetch/fetcher.go:236-239`, `core/lock/types.go:97-112`) |

---

## 3. Adversaries

For each: what they can do, and what debark does about it today.

### 3.1 Someone who can modify the media in transit (the headline case)
**Can do:** swap, corrupt, or partially replace any file on the USB
drive/CD/whatever crosses the gap, including adding files, removing files, or
editing `debark.manifest.json` itself.
**Mitigated by:** `core/verify` — see §4 for the full tamper matrix. As of this
writing, `core/verify/verify.go` implements every check in the interface
contract (`core/verify/iface.go:17-34`) and `core/install/runner.go` calls it
unconditionally before touching apt or dpkg.

### 3.2 Someone who can modify the snapshot before it reaches the builder
**Can do:** something qualitatively worse than tampering with a finished
bundle, and the design is explicit about why: **a tampered snapshot can carry
both a malicious source and the key that authenticates it**. Add a rogue `apt.sources` entry pointing at an attacker-controlled
repository, and a matching fabricated keyring so it "verifies." Everything
downstream of that point — apt's own signature check, the lock's provenance
fields, the final manifest signature — will be internally consistent, because
apt genuinely did resolve against a source it was told to trust, and the
operator's own key genuinely did sign the result. This attack does not
produce a detectable tamper; it produces a bundle that debark considers
completely legitimate.
**Mitigated by (partially):** the snapshot records every keyring fingerprint
and every `Signed-By` relationship it found (`core/snapshot/types.go:124-137`,
`core/snapshot/keyrings.go`), specifically so this is *auditable*, not so it
is *prevented*. `--approved-keys` is the control:
`core/policy.LoadApprovedKeys` reads the operator's fingerprint list and
`core/apt/root.go`'s `checkApprovedKeys` enforces it while the private root
is being built, before any `apt-get update` runs, failing the build with
`dferr.Policy` (exit 6). The load-bearing detail, verified 2026-09-04: the
fingerprints it compares against are read out of the keyring bytes the
private root actually installs into `trusted.gpg.d`
(`snapshot.FingerprintsIn`, called from `copySnapshotConfig`), **never** out
of the snapshot document's own `keyring_fingerprints` — checking the
document's claim would let this exact adversary list the genuine archive
key's fingerprint alongside their own key material and be approved. It is
enforced at source-*file* granularity (a multi-suite deb822 stanza passes or
fails as a whole) and fails closed on a stripped, inline, or unparseable
`Signed-By`. **This is still why the snapshot needs its own integrity
protection between capture and build** (see §6) — approved keys constrain
which archive keys the builder will resolve against; nothing in the manifest
signature chain, which only starts once the builder has the snapshot, can
detect the rest of this class of tampering after the fact.

**Escalation found and closed, 2026-09-04.** This adversary was worse than
"a bad but internally consistent bundle": it reached code execution on the
builder, which belongs, not here. A snapshot's captured
`apt.conf.d` fragments are copied into the private root and `APT_CONFIG` is
pointed at them (the mechanism [E4](experiments/E4-aptconfd-leakage.md)
established as the only one that works), and `filterAptConf` dropped only
`Dir::*` and `Acquire::*::Proxy`. Everything else was carried through
verbatim, including the keys apt does not read but *executes* —
`DPkg::Pre-Invoke`, `Post-Invoke`, `Pre-Install-Pkgs`, `Proxy-Auto-Detect`,
which apt runs through `/bin/sh`. Confirmed against apt 2.6.1 in a
container: the hook ran as root during the `apt-get update` every resolve
performs, and `apt-get update` still exited 0, so nothing downstream saw
anything wrong. That is arbitrary code execution on the one host in the
pipeline that holds a signing key, from the product's ordinary untrusted
input. Fixed in commit `98b9127` by adding the hook families and the
apt-secure-disabling keys (`AllowUnauthenticated`, `Check-Valid-Until`,
`Verify-Peer`, and siblings) to the deny-list, each recorded as a
`DroppedConfEntry` rather than silently withheld
(`core/apt/aptconf.go`, `hookKeys` and `verificationKeys`).

**Residual, and the decision taken on it — now implemented.** That fix left
`filterAptConf` a deny-list: it named every executing key apt is known to
have today, but a key added by a future apt, or one simply missed, would be
carried through exactly the way these were. Deny-lists fail that way by
construction — the tell here is that `Dir::Bin::gpgv` had already been
anticipated and dropped while the hook keys, a far more direct route, had
not. It is now an **allow-list** (`core/apt/aptconf.go`, `filterAptConf`):
only the keys that affect apt's *resolution* survive into the private root,
and everything else is dropped and recorded through the same
`DroppedConfEntry` machinery, surfacing to the operator as the
`private-root.apt-conf-dropped` warning naming the key, the file it came
from, and why it went.

What survives is `aptConfAllowList` and nothing else:
`APT::Install-Recommends`, `APT::Install-Suggests`,
`APT::Default-Release`, `APT::Architecture`, `APT::Architectures`,
`APT::Solver`, `APT::Machine-ID`,
`APT::Get::Never-Include-Phased-Updates`,
`APT::Get::Always-Include-Phased-Updates` and `Acquire::Languages`, plus the
proxy family when the operator has opted into it. Each entry earned its place
by changing what apt selects, with this repo's own experiments as the
evidence (E1 for phasing, E2 for the solver, E4 for recommends and
`Default-Release`). Three properties are what make this an allow-list rather
than a longer deny-list wearing the name:

- **Matching is exact**, not by prefix. A prefix match silently makes every
  entry a subtree root — `APT::Install-Recommends::<anything>` would be
  allowed — which is the same fail-open shape reintroduced one level down.
  Nine of the ten entries are scalars or value lists with nothing legal
  beneath them; `APT::Solver` is the only subtree, and it earns that because
  solver3's own tuning keys really do live under it.
- **`Binary::<program>::` scoping is stripped before matching**, so a scoped
  re-entry such as `Binary::apt-get::DPkg::Pre-Invoke` is judged as the key
  it is.
- **The hook and verification-weakening families are still named, and are
  checked *before* the allow-list.** The allow-list would drop them anyway;
  naming them means an operator is told a hook was removed rather than that
  some unrecognised key was, and it means nothing can be re-admitted
  underneath a permitted subtree. `APT::Solver`'s *value* is guarded too: it
  names a program apt runs from `Dir::Bin::solvers`, so a value containing a
  path separator is dropped.

The trade-off is real and is why this is a design decision rather than a
security patch — the whole product promise is that the builder's apt behaves
as the target's would, and every key withheld is a small step away from
that. It is the right trade anyway, because the failure modes are not
symmetric: a withheld resolution key produces a visible warning and, at
worst, a bundle that resolves slightly differently from the target, while a
carried-through executing key produces root on the signing host and exits 0.
An operator who sees `private-root.apt-conf-dropped` for a key that
genuinely changes their resolution should ask for that specific key to be
added to the allow-list. **A general `--apt-conf-passthrough` escape hatch
was considered and rejected, and does not exist**: a flag like that gets set
once, to silence a warning, by someone who has not read this section — and
setting it hands back the arbitrary-code-execution path the allow-list exists
to close.

**One deliberate silence in that report, because the report is the whole
mitigation.** A `#` line that is neither `#include` nor `#clear` is a
comment; it is dropped without a `DroppedConfEntry`. A real lock from a stock
Docker target had 34 of about 48 `private-root.apt-conf-dropped` warnings on
comment lines, all keyed `"#"`, burying the five that carried meaning — two
of them command hooks, including Docker's own `DPkg::Post-Invoke`. A
mitigation that drowns its own signal is not one, and a comment carries no
setting, so staying quiet about it withholds nothing an operator could have
acted on. An unrecognised `#` line is still dropped either way, so the
judgement can cost a warning, never the security property.

**When there is no captured snapshot at all: `--base`, added 2026-09-06.**
`build --base` and `snapshot from-base` produce a *synthesized* snapshot from
a base definition (ADR-014, `docs/formats.md` §3.13) instead of capturing a
real machine. This adversary's target changes shape, and mostly shrinks.

- **Nothing was measured, so there is nothing on the target side to
  intercept.** The document is authored on the builder, from either a table
  compiled into the binary or a YAML file the operator supplied. Modifying
  the compiled-in table means modifying the binary, which is adversary 3.4,
  not this one. Modifying an operator's definition file means write access to
  the builder's inputs — the same position as being able to edit
  `packages.txt` or a `--policy` file, and inside the same trust boundary.
- **A base definition cannot carry key material, and that is a property of
  the format rather than a check.** The escalation this section opens with —
  a snapshot carrying both a malicious source *and* the key that
  authenticates it — cannot be expressed here. `keyrings[]` is a list of
  absolute paths *on the machine that runs the resolution*
  (`core/base/base.go`, `Definition.Keyrings`; `Validate` refuses anything
  that is not absolute), never embedded bytes. An attacker who controls a
  definition file can point at key material the builder already has; they
  cannot supply their own.
- **A base definition also carries no `apt.conf`, no `preferences` and no
  `trusted.gpg.d`.** `core/base`'s `finalSnapshot` writes exactly one sources
  file and the keyrings it names, and nothing else. The arbitrary-code-
  execution route described above — captured `apt.conf.d` fragments carrying
  `DPkg::Pre-Invoke` into the private root — has no field to arrive in on
  this path at all.
- **What remains is the `sources:` document itself.** A hostile definition
  can name an attacker-controlled archive URI, and that text is written into
  the synthesized snapshot verbatim, so it is what the seeds resolve against
  *and* what a later `build` against that snapshot resolves against. That is
  the same power a tampered captured snapshot's `apt.sources[]` has, with the
  same mitigation: the synthesized snapshot enters the identical private-root
  code path, so `--approved-keys` is enforced against the keyring bytes
  actually installed into `trusted.gpg.d`, before any `apt-get update` runs.
  The seed resolution itself downloads nothing (`core/apt/closure.go` — a
  base closure resolves a package list and fetches none of it), so it is not
  a route for getting bytes onto the builder.
- **Auditable, not prevented — as everywhere else in this section.**
  `origin.base_id`, `origin.source` and `origin.source_digest` record which
  definition produced the document and the exact SHA-256 of its canonical
  form, so a definition that changed underneath a fleet is visible rather
  than silent, in the same spirit as `keyring_fingerprints`.

Read 2026-09-06 against `core/base` and `core/apt/closure.go`. What this
entry does **not** cover is a base definition that is merely *wrong* rather
than hostile — an honest description of a release that does not match the
machine it is eventually installed on. No adversary is involved and no check
here fires; see §5.

### 3.3 A malicious or compromised vendor `.deb` URL
**Can do:** serve a `.deb`-shaped file with attacker-chosen contents, an
attacker-chosen `Content-Disposition` filename, an attacker-chosen `Package:`
control field, and redirect the request anywhere it likes.
**Mitigated by:**
- HTTPS→HTTP downgrade on redirect is refused (`core/fetch/fetcher.go:51-63`,
  `checkRedirect`); a redirect to a different host is honoured but recorded as
  a warning (`fetcher.go:148-151`).
- A `Content-Disposition`-supplied filename is sanitised against path
  separators, `..`, control characters and Windows drive-letter forms before
  it is ever used as a pool filename (`fetcher.go:389-418`,
  `sanitizeFilename`).
- The downloaded bytes are sniffed as a real `.deb` (ar archive with a
  `debian-binary` first member) before being trusted at all
  (`core/fetch/debsniff.go`).
- Provenance is labelled honestly: `url-unverified` unless the operator
  supplied a matching `--digest` (`fetcher.go:236-239`).
- The `Package:`/`Architecture:` control fields extracted from the `.deb`
  itself reach a filesystem path, so they are validated against Debian
  Policy §5.6.1's grammar before they can: `repository.PoolPath` returns the
  empty string rather than an escaping path for a name or filename that
  fails `ValidPackageName`/`ValidFilename`, and every caller treats that as
  a hard error (`core/repository/iface.go`,
  `core/bundle/assemble.go`'s `materialiseSelections`, which additionally
  re-checks that the resolved destination is lexically inside `repo/pool`
  before any `store.Materialise` call). This was Finding F1 in
  `docs/security/review-findings.md` — a `Package: ../../../../tmp/evil`
  giving an arbitrary file write on the *builder* — and it is fixed;
  re-verify both the grammar check and the containment check if either file
  is refactored, since the safety property comes from the character set, not
  from any single call site.

### 3.4 A compromised builder host
**Can do:** anything. The builder is where the signing key is used, so a
compromised builder can produce a bundle containing anything at all and sign
it with full validity.
**Mitigated by:** nothing in the target-side trust chain, and the design does
not claim otherwise. The commercial edition's HSM/KMS-backed signing and
multi-person approval exist precisely because the
community edition has no technical answer to this; the community-edition
answer is entirely procedural (key custody, who has shell access to the
builder, evidence review before signing).

**Because there is no answer here, every path from untrusted input to
execution on the builder is a promotion into this adversary and must be
treated as critical, not as a §3.2-class "bad bundle".** One such path
existed and was closed on 2026-09-04 (a captured `apt.conf.d` fragment's
hook keys running under `apt-get update`; see §3.2). The snapshot and any
external `.deb` are the two untrusted inputs the builder consumes by design,
so this is the class to hunt hardest for whenever either parser or either
copy path changes.

### 3.5 A compromised container image
**Can do:** bias which packages a resolution run selects (still constrained
by apt's own archive-signature check running *inside* the container — a
compromised image cannot make apt accept an unsigned or wrongly-signed
archive package, but it can run a different apt, different sources, or tamper
with the private-root construction before apt ever runs), or corrupt files
during the local repository-generation step if it can influence the host
process.
**Mitigated by:** image references are pinned by digest (the distro
table pins every image by digest). debark has no mechanism
to attest to what actually happened *inside* a container run beyond recording
`Resolver.ImageDigest` in the lock (`core/lock/types.go:69-72`) for later
audit — that is a record, not a prevention.

### 3.6 A malicious archive mirror
**Can do:** serve tampered `Packages`/`Release`/`.deb` files for the Debian or
Ubuntu archive itself.
**Mitigated by:** apt's own `apt-secure` OpenPGP verification, which debark
deliberately never re-implements (ADR-001; `core/apt/iface.go:3`). This is
apt's job, not debark's, and debark's only exposure here is the same as
adversary 3.2's: it trusts whatever key set the snapshot says the target
trusts.

### 3.7 An insider with legitimate builder access
**Can do:** sign anything, using their legitimate access, exactly as if they
were doing their job. This is the hardest adversary in this list, because
nothing here distinguishes "the release engineer building a bundle" from "the
release engineer building a malicious bundle."
**Mitigated by:** nothing in the free/community edition. Multi-person
approval / dual control for media crossing the gap is explicitly a paid,
builder-side-only feature ("Multi-person approval / dual
control for media crossing the gap — Paid"). The community edition's only
answer is the local evidence trail (`core/evidence`, `evidence.json`) —
useful for after-the-fact audit, not for prevention.

### 3.8 Someone who obtains the bundle and wants to learn about the target
**Can do:** read everything in it, without needing to defeat a single check,
because **nothing in a debark bundle is encrypted.** Signing provides
integrity and authenticity, never confidentiality. A correctly, legitimately
signed bundle — one that passes `verify` cleanly — still hands anyone who
possesses it: the exact package/version inventory (`lock.json`, and
`snapshot.json` if present — an exact vulnerability fingerprint of the
target, cross-referenceable against any CVE database), the target's
distro/version/codename/architecture, every apt source and keyring fingerprint
the target trusted at capture time, and (in `evidence.json`) build-host and
timing detail. This is true even of a bundle an attacker never tampers with —
merely *intercepting a copy* of otherwise-legitimate media is enough. See §5
and §6.

**One new field, and the honest analysis of it, 2026-09-06.** A bundle built
from a synthesized snapshot (ADR-014) carries
`origin.assumed_installed` inside `snapshot.json`: the full list, as
`name:arch`, of every package the base assumed a stock install already has.
It is there because the bundle carries `snapshot.json` but not the snapshot's
files, so this is the only copy of the assumption that reaches the target,
and without it `install` could report that a base was assumed but not which
packages it assumed.

What it discloses to whoever holds the bundle is **the base**, and the base
was already disclosed: `snapshot.json`'s `target` block names the
distribution, version, codename and architecture, and `origin.base_id` names
the base by id. Learning that a stock `ubuntu:26.04/desktop` contains
`libc6:amd64` tells an attacker nothing they could not get by installing that
release themselves. It is emphatically **not** the real machine's inventory:
no machine was measured, and the list is the output of resolving public
metapackages against a public archive.

That is precisely why the field is **refused** on a captured snapshot rather
than merely left empty (`core/snapshot/validate.go`, the `OriginCaptured`
branch, with its own error message separate from the other origin-field
check). A captured machine's installed set is asset 2 — an exact
vulnerability fingerprint of an air-gapped host — and it is kept out of
bundles deliberately: `snapshot.json` travels but the snapshot's
`dpkg_status` file does not. A field that carried an installed-package list
into a bundle would have quietly undone that, so the format refuses to let it
be populated by the one kind of snapshot where the list would be a
measurement. The whole rest of this adversary's paragraph is unchanged: the
`lock.json` package inventory, which is the sharpest disclosure in a bundle,
is exactly as revealing as before.

---

## 4. Attacks that must fail, and where exactly they fail

The tamper matrix. Each row cites the exact check and, where verified by
direct code reading, its current implementation status.

**The "untested" status in the rightmost column is out of date and the
column is the part of this table to re-derive first.** It was written on
2026-09-03, when neither `core/verify` nor `core/sign` had a single
`*_test.go` file (Finding F2, `docs/security/review-findings.md`). Both now
do: `core/verify` has `tamper_test.go`, `verify_test.go`, `sigblocks_test.go`,
`links_test.go` and `gpgwarning_test.go`, and `core/sign` has
`verifier_test.go`, `sign_test.go`, `gpg_test.go`, `domain_test.go`,
`keyfile_test.go` and `plugin_test.go` — several thousand lines between
them, written specifically against the attacks in the rows below. Do not
read the old status as current, and do not read the new files as a clean
bill either: re-run them and read what each one actually asserts before
relying on a row.

| # | Attack | Must fail at | Code path | Status (2026-09-03) |
|---|---|---|---|---|
| 1 | Flip a bit in a `.deb` inside `repo/pool/` | File digest mismatch | `checkFiles` walks the bundle, recomputes SHA-256 per file, compares to `manifest.Files[]` → `ProblemFileDigest` (`core/verify/verify.go:225-297`) | Implemented, untested |
| 2 | Delete a file the manifest lists | File missing | Same walk; anything in `manifest.Files[]` not seen on disk → `ProblemFileMissing` (`verify.go:283-296`) | Implemented, untested |
| 3 | Add an extra `.deb` or any other file under the bundle root that the manifest never listed (hoping apt or a human reads it) | Unexpected file | Same walk; any on-disk file not in `manifest.Files[]` → `ProblemFileUnexpected` (`verify.go:248-254`) | Implemented, untested |
| 4 | Edit `debark.manifest.json` after signing (change a digest, a target arch, a file list entry) | Recomputed manifest digest ≠ the digest the signature actually covers | `manifest.Load` re-canonicalises the **raw bytes read from disk** (never the re-marshalled Go struct — a deliberate defence against a field Go doesn't recognise silently vanishing before hashing, `core/manifest/api.go:137-149,163`); `runVerify` compares that digest to `debark.manifest.sig`'s `manifest_sha256` before any signature math runs → `ProblemManifestDigest` (`verify.go:90-102`) | Implemented, untested |
| 5 | Strip the signature file, or replace it with one made by an attacker's own key | No trusted signature found | `checkSignatures` builds the trust set only from operator-supplied `KeySource` (`sign.VerifierFor`), never from anything inside the bundle; an untrusted-key signature or a missing signature (without `AllowUnsigned`) → `ProblemSignatureUntrusted` / `ProblemSignatureMissing` (`verify.go:145-216`) | Implemented, untested |
| 6 | Re-sign a *tampered* manifest with a key the operator does trust (tests whether the crypto is real, not just present) | Cryptographic signature check fails | `verifyEd25519Signature` uses `crypto/ed25519.Verify` on `SigningInput(purpose, canonical)` (`core/sign/ed25519.go:76-93`); `gpgVerifier.verify` shells to `gpg --status-fd 1 --verify` and requires a `VALIDSIG` line, never trusting gpg's exit code alone (`core/sign/gpg.go:184-226`) → `ProblemSignatureInvalid` | Implemented, untested |
| 7 | Drop an attacker-controlled public key file *inside the bundle itself* and point `--key` at it, hoping "a key was found" is treated as "a key was provisioned" | Same-media key refused | `sign.VerifierFor` resolves every key-source candidate (files, dirs, GPG keyring path) and the bundle root through symlinks to absolute, cleaned form, and refuses any candidate that is the bundle root or resolves *under* it (correctly using a path-separator-suffixed prefix check, so a sibling directory like `bundle-keys` next to `bundle` is not mistaken for "inside") unless `AllowSameMedia` is explicitly set (`core/sign/verifier.go:50-90`) → `ProblemSameMediaKey`. Confirmed wired with the real bundle path, not bypassable via a different `sign` entry point (`verify.go:159`, `checkSignatures` calls `sign.VerifierFor(ctx, opts.Keys, root)`) | Implemented, untested — **the one check in this table read most carefully; no bypass found** |
| 8 | Edit `repo/Packages` or `repo/Release` to add a malicious stanza or change a hash | Manifest's own repository digests, cross-checked two independent ways | `checkFiles` already proves on-disk `repo/Packages`/`repo/Release` match their `manifest.Files[]` entries; `checkRepository` independently proves those entries match `manifest.Repository.PackagesSHA256`/`ReleaseSHA256` — deliberately two comparisons against two different reference points, not one compared against itself (`verify.go:299-340`) → `ProblemRepoDigest`. This is what makes `Trusted: yes` safe on the target's private apt source (`core/install/privateroot.go:94-105`) — apt itself performs **no** independent check of repo metadata at install time; the manifest-level check is the entire reason that's safe | Implemented, untested |
| 9 | Edit `lock.json` to request a different (still-present-in-the-pool) version — a downgrade attack that doesn't need any file in the pool to change | Lock digest mismatch | `checkLockAndSnapshot` re-canonicalises the raw `lock.json` bytes and compares to `manifest.LockDigest`, same technique as #4 (`verify.go:342-358`) → `ProblemLockDigest`. Independently, `install`'s package selection **only ever consults `lock.Install`**, never asks apt for "the newest available" (ADR-007, `core/install/select.go:11-19`) — so even if this check were absent, install has no code path that would honour a version apt merely finds newer in the pool | Implemented, untested |
| 10 | Edit `snapshot.json` (the copy shipped for reference inside the bundle) | Snapshot digest mismatch, when present | Same re-canonicalisation technique against `manifest.SnapshotDigest` (`verify.go:360-372`) | Implemented, untested |
| 11 | Rely on `--allow-unsigned` being silently assumed, or on the report not saying it was used | Must be an explicit, visible operator choice | `checkSignatures` only takes the `AllowUnsigned` branch when the caller set it, and always appends a `Warnings` entry saying so (`verify.go:147-150, 205-208`); `Report.Signed` is `false` in this case even though `Report.OK` can be `true` — a caller must render that distinction, and the CLI does: `printVerifyHuman` prints `signed:  no (--allow-unsigned)` for an OK-but-unsigned report, distinct from both `signed:  valid by <keyid>` and the plain `signed:  no` of a failure, and prints every `Warnings` entry beneath (`internal/cli/cmd_verify.go`) | Implemented at both layers |
| 12 | Get `install` to run apt/dpkg before verification, via some option combination | Structurally impossible | `Plan` and `Apply` both funnel through one `execute()` that calls `r.deps.Verifier.Verify` as its first action, before loading the lock, checking preconditions, or building the private apt root (`core/install/runner.go:42-96`) | Implemented and, by inspection, has no bypass — this is the one link in the whole chain that cannot regress silently, because there is no second code path that skips it |
| 13 | Put a **symlink** at a manifest-listed path, pointing at bytes that match the manifest but live outside the verified tree, then rewrite those bytes after `verify` passes and before apt reads them | Entry is not a regular file carrying its own bytes | Refused at both ends. On the way in, `core/bundle/tar.go`'s importer rejects `tar.TypeSymlink` and `tar.TypeLink` outright ("bundles never contain links", `tar.go:425-426`) and exports nothing that is not a regular file, so a tree containing a link was not built by debark. As defence in depth for a bundle handed over as a directory rather than an archive, `checkFiles` refuses any walk entry whose type is not zero — symlink, device, socket, FIFO — (`core/verify/verify.go`, `checkFiles`) → `ProblemFileNotRegular`. **A hard link is deliberately accepted**, and an earlier link-count > 1 refusal was removed: a hard link must live on the same filesystem as its inode, so the "another, rewritable name outside the verified medium" this row describes is the symlink form and cannot be the hard-link form — read-only media gives every name the same read-only inode, and on writable storage whoever can rewrite through the second name can rewrite through the first. The count test also broke the product, because `store.Materialise` hard-links pool objects out of the content store into the bundle, so every freshly built directory bundle carried link count 2 and failed verification in place. A hard link at an *unlisted* path is still refused, as `file-unexpected` | Implemented and tested (`core/verify/links_test.go` pins both the acceptance and the unlisted-path refusal; `core/bundle/tar_test.go`'s `TestImportRefusesSymlinkEntries`) — this row records a confirmed HIGH defect, now closed: before it, `verify` opened the listed path, followed the link, hashed whatever was at the other end, and reported OK |

**What this table does not cover:** a tampered *snapshot* reaching the
builder in the first place — nothing in this table detects that,
because the manifest signature chain only begins once the builder has
already accepted the snapshot as ground truth.

---

## 5. What debark explicitly does NOT protect against

State this plainly, because a vague version of this section is worse than
none.

- **It does not audit package contents.** debark never opens a `.deb`'s
  payload (`data.tar.*`); it indexes, copies and reports metadata only.
- **It is not a CVE scanner or a licence adjudicator** (
  permanent do-not-build list). `doctor` heuristics warn about specific,
  narrow, named risk patterns (network-touching maintainer scripts, snap
  shims, DKMS without headers, non-free/restricted redistribution terms); they
  are not a vulnerability or licence-compliance product, and were outside this
  review's scope to verify in detail.
- **A valid signature proves authenticity, not safety.** `verify` answers "is
  this the artefact a specific key holder vouched for," never "is this
  artefact safe to run." A legitimately signed bundle can still contain a
  legitimately-published package with a real vulnerability, or a maintainer
  script that does something the operator would not have wanted, or an
  external `.deb` the operator trusted in error.
- **It cannot defend a compromised builder against itself.** See adversary
  3.4 and 3.7. Every guarantee assumes the signing key and the process
  that used it were not themselves compromised.
- **`--allow-unsigned` and `AllowSameMedia` are operator overrides that void
  specific guarantees, on purpose, with the override recorded.** They exist
  so the refusal they lift is a deliberate, visible choice — not a hole. A
  bundle accepted via `--allow-unsigned` has made zero authenticity claim, and
  a key accepted via `AllowSameMedia` has proven nothing about who provisioned
  it (`core/sign/api.go:22-24`).
- **HTTPS on a vendor URL is transport security, not provenance.** It proves
  the bytes weren't altered in flight from that specific server; it proves
  nothing about who controls that server or what they put there. See
  adversary 3.3 and `lock.VerifiedURLUnverified` (`core/lock/types.go:105-107`).
- **Nothing in a debark bundle is confidential.** No file in a bundle or a
  snapshot archive is ever encrypted; signing is integrity and authenticity
  only. See asset 2, adversary 3.8, and the residual-risk item on physical
  media handling below.
- **A default (unredacted) snapshot is not private.** `snapshot create`
  without `--redact` includes the machine id and, unless the operator also
  configured otherwise, `apt.conf` content verbatim — which can include proxy
  credentials (see `docs/security/review-findings.md` F5). Redaction is an
  operator action, not a default.
- **debark cannot tell you whether a machine it has never measured matches
  the base you assumed — only that it was an assumption.** A bundle built
  with `--base` (or from a `snapshot from-base` archive) is resolved against
  a description of a stock release, not against a target. Nothing on the
  build side can check that description: the builder never sees the machine,
  and `verify` proves that the bundle is the one that was signed, never that
  the machine is the one it was assumed to be. What debark does guarantee
  is that the assumption is *recorded and legible* — `origin.kind` says
  `synthesized`, `origin.base_id` and `origin.source_digest` say exactly
  which description was used, the snapshot carries a permanent warning
  saying so in its own `warnings[]`, and `install` compares
  `origin.assumed_installed` against the target's real dpkg status and
  reports what is missing. That comparison **warns and continues; it never
  refuses** (`core/install/basedivergence.go`), which is the existing rule
  that doctor-style findings warn while policy fails, and it is a finding
  rather than a proof: a missing assumed package is evidence the bundle
  *might* be short, and apt in the same run answers the real question a few
  seconds later. An operator who needs the stronger guarantee should capture
  a snapshot of the real machine — that is what makes a bundle *proven*
  complete, and it is why every document leads with `snapshot create` and
  presents `--base` as the fallback for when there is no machine yet.
- **debark cannot detect a tampered snapshot that internally agrees with
  itself.** See adversary 3.2. This is the sharpest limit in the whole trust
  model and the one most worth an approver's attention: the manifest
  signature protects the media *after* the builder trusted the snapshot, not
  the snapshot itself.
- **A `gpg:`-signed bundle verified *without* `--gpg-keyring` is verified
  against the whole of the verifying machine's default GPG keyring.** An
  empty `KeySource.GPGKeyring` makes `newGPGVerifier` omit the
  `--no-default-keyring --keyring PATH` pair entirely, so gpg falls back to
  the invoking user's own keyring (`core/sign/gpg.go`). That is
  standard gpg behaviour, not a debark bug, and it is now an operator
  choice rather than a missing capability. `--gpg-keyring FILE` sets
  `KeySource.GPGKeyring`, on both `verify` and `install`; `--keyring` is a
  different flag entirely (a directory of debark ed25519 `.pub` files) and
  has no effect on a gpg check. Pass `--gpg-keyring` and the keyring named
  is the whole trust set; omit it and the trust set is as wide as whatever
  the invoking user happens to have imported — which `verify` states in
  `warnings[]` rather than leaving to be inferred. This was Finding F4
  (`docs/security/review-findings.md`) and the flag is the fix. The residual
  exposure is procedural: an operator who omits the flag on a
  security-relevant verification gets ambient trust, and no tool can make
  that decision for them. See §6.

- **`install --keep-source` installs a permanent, unverified, `Trusted: yes`
  apt source on the target.** Without it, install's private apt root is a
  temporary directory that is deleted when the command ends, and its
  `Trusted: yes` stanza is safe because `verify` has already proven the
  repository's `Packages`/`Release` digests against the signed manifest
  moments earlier (row 8). With it, `PersistSource` copies that same stanza
  to `/etc/apt/sources.list.d/debark-bundle.sources` on the real system,
  pointing at the bundle's `repo/` directory by absolute path
  (`core/install/privateroot.go`). Nothing re-verifies that directory
  afterwards — debark is not running afterwards. From then on, every
  `apt install` and `apt upgrade` on that machine will install whatever is
  in that directory, as root, with `apt-secure` disabled for it. Anyone who
  can write into the path holds a root-level package-installation channel on
  an air-gapped host, with no signature to forge. It is opt-in and the
  prototype's `install-offline.sh` behaves the same way, but the flag's
  `--help` text names only the mechanism, not this consequence, so an
  operator can enable it without knowing what they enabled. Treat the
  directory as part of the target's trusted computing base, or leave the
  flag off.

---

## 6. Residual risks and operator responsibilities

- **Out-of-band key provisioning.** The operator public key must reach the
  target independently of the media — a pre-provisioned file, a config
  entry, or a fingerprint compared by hand — never a key found on the same
  media as the bundle (row 7). Write this into the deployment procedure,
  not just into the tool's refusal message.
- **Approved-key lists.** `--approved-keys` is the
  defence against adversary 3.2 (a tampered snapshot carrying its own
  fabricated archive key), and as of 2026-09-04 it is implemented and
  consulted: `core/policy.LoadApprovedKeys` parses the list,
  `core/apt/root.go`'s `checkApprovedKeys` enforces it during private-root
  construction on both backends (the container backend forwards it as
  repeated `--approved-key` arguments to the in-container resolve, which
  runs the same local path), and a failure is `dferr.Policy`, exit 6. Two
  operational consequences to plan for rather than discover: it is enforced
  per source *file*, so a multi-suite deb822 stanza is approved or rejected
  as a unit; and it fails closed on any source it cannot trace to a captured
  keyring, including a plain `sources.list` line with no `Signed-By` at all.
  A fleet still on pre-`Signed-By` sources will not pass an approved-keys
  build until those sources are pinned.
- **Verify the embedded binary's digest out of band.** When a bundle carries
  `bin/debark-linux-<arch>` (`--embed-binary`), its SHA-256 is recorded in
  the manifest and also printed at build time specifically so it can be
  compared by a second channel — do that comparison; the
  manifest listing it is not itself an independent check.
- **Physical media handling is not this product's job.** Malware-scanning
  kiosks, media sanitisation, and chain-of-custody for the drive itself are
  explicitly out of scope (rung 5: "Removable-media
  security — No, partner. debark builds the verifiable payload; kiosks scan
  the media"). debark's signature check happens *after* whatever a kiosk
  does, and proves something different: not "this drive is free of malware,"
  but "these specific bytes are what the signer vouched for."
  A verified bundle can still be handed to someone on media that was never
  scanned for anything else riding along in unused space.
- **A dedicated, minimal GPG keyring for verification: pass
  `--gpg-keyring`, never `--keyring`.** If signing with `gpg:`, every
  security-relevant `verify` and `install` should name a keyring FILE
  holding only the expected release key(s) — e.g. one made with
  `gpg --export --output release.gpg <keyid>`. Do not substitute
  `--keyring`: that fills a different field (`KeySource.Dirs`, directories
  of ed25519 `.pub` files) and has no effect on a gpg check. Earlier
  revisions of this document gave exactly that substitution as advice; it
  was wrong, and the flag that actually works is `--gpg-keyring`. Write the
  flag into the deployment procedure, not just into this document — an
  omitted `--gpg-keyring` fails open to the operator's personal keyring, and
  the only signal is a warning in the report.

- **`--keep-source` is a standing decision, not a per-install one.** If a
  procedure enables it, the procedure owns the bundle directory's
  permissions and location from then on — see §5. Prefer leaving it off and
  re-running `install` when more of the bundle is needed.
- **`snapshot create --redact` is the operator's decision to make, every
  time.** It is not the default. If the target's `apt.conf` carries proxy
  credentials, or the operator does not want the machine id or a hostname
  label leaving the machine, `--redact` (or `--no-keyrings`, for an operator
  who considers the trusted-keyring list itself sensitive) has to be chosen
  explicitly, on every capture.
- **Multi-person control is a paid, builder-side feature, not a community
  one.** An organisation that needs dual control over what crosses the gap
  cannot get it from the free edition's technical controls alone (adversary
  3.7); it needs a process, or the commercial edition.
- **This document, and the tamper matrix specifically, needs re-verification
  against a real, executed test suite before 1.0.** Every "implemented" row
 reflects careful manual reading of the code as it stood on
  2026-09-03, not a passing automated test. Treat that as a punch-list, not
  as reassurance.

---

## 7. Cryptographic inventory

Every algorithm and format debark uses, where, and why — and what is
deliberately not used.

| Purpose | Algorithm / format | Where | Notes |
|---|---|---|---|
| Manifest / signature-payload digest | SHA-256 | `core/canonical/canonical.go:69-83`, used throughout `core/manifest`, `core/verify`, `core/digest`, `core/store` | The one digest algorithm debark ever makes a trust decision on. Always lowercase hex. |
| Per-`.deb` legacy hashes for apt's index format | MD5, SHA-1 | `core/digest/digest.go:55-65`, written into `Packages` stanzas by `core/repository/api.go` | Required by the apt index format for compatibility only; explicitly documented as never used for a trust decision (`digest.go:56-57`: "debark never makes a trust decision on them"). |
| Canonical serialisation for anything hashed or signed | RFC 8785 JSON Canonicalization Scheme (JCS), via `github.com/gowebpki/jcs` | `core/canonical/canonical.go` | Object keys sorted by UTF-16 code unit, no insignificant whitespace, ES6 number formatting. `manifest.Load` and `verify`'s lock/snapshot checks canonicalise the **raw bytes read from disk**, never a re-marshalled struct — the specific, deliberate defence against a field being silently dropped before it reaches a digest (`core/manifest/api.go:130-149`). |
| Domain separation for every signature | `purpose‖0x00‖canonical_payload` | `core/sign/iface.go:43-52`, `SigningInput` | Every signer/verifier in the codebase goes through this one function, so a signature made for one purpose (`debark.manifest/v1`, `core/manifest/types.go:147`) can never be replayed as a signature over a different kind of object. |
| Operator/organisation signature, native format | Ed25519 (`crypto/ed25519`, stdlib) | `core/sign/ed25519.go` | Key files use a minisign-inspired but **not** minisign-compatible on-disk format (`core/sign/keyformat.go:14-80`): a labelled "untrusted comment" line plus one base64 line encoding a small tagged blob (`"Ed"` tag, 8-byte key id = first 8 bytes of SHA-256(pub), then the raw key material). No passphrase/KDF on the private key file by design — an operator who wants secret-key encryption uses `gpg:` instead; protection is the 0600 file mode. |
| Operator/organisation signature, GPG | Whatever algorithm the operator's GPG key uses (RSA, Ed25519, etc.) | `core/sign/gpg.go` | debark never inspects or second-guesses the key algorithm; `Algorithm` in the signature block records `"gpg"` (the mechanism), not the underlying key type. Verification requires an explicit `VALIDSIG` status line from `gpg --status-fd 1 --verify`, never trusts the process exit code alone (`gpg.go:184-226`). `--trust-model always` is used deliberately: it makes gpg use any key **present in the keyring being consulted** regardless of that key's manually-assigned ownertrust, but a key not in that keyring still cannot produce `VALIDSIG` — the keyring is the actual trust boundary, not gpg's web of trust. Which keyring that is, is `--gpg-keyring`'s answer (`KeySource.GPGKeyring`, excluded from same-media resolution like every other key source); without it, the boundary is the invoking user's default keyring and `verify` warns. Ownertrust is deliberately excluded because it is ambient state in the *verifying* machine's `GNUPGHOME` — honouring it would make the same bundle and the same keyring verify differently on two targets. It does not weaken revocation or expiry, which are key state rather than ownertrust: gpg emits `REVKEYSIG`/`EXPKEYSIG`/`EXPSIG` **alongside** `VALIDSIG`, and `core/sign/gpg.go` checks those keywords *before* the valid flag and refuses, each with its own operator remedy. gpg's exit status is read too, so a `VALIDSIG` from a gpg that exited non-zero is refused rather than believed. The fingerprint compared against the signature block is the **primary** key's (`parseColonFingerprint`), not the signing subkey's, matching the offline-primary arrangement a gpg-mandating shop is most likely to run. |
| Signing, pluggable (HSM/KMS/Sigstore/other) | `debark.plugin/v1`: exec + single-line JSON over stdio | `core/sign/plugin.go`, `api/plugin/v1` | A plugin's signature is checked with the same primitive as a native key once the operator has separately imported its exported public key into a trusted keyring — the verification math does not care which process held the private key. |
| OpenPGP keyring parsing (fingerprint/user-ID enumeration only) | `github.com/ProtonMail/go-crypto/openpgp` | `core/snapshot/pgp.go` | Explicitly **not** a trust decision: this only enumerates what a captured keyring contains, matching `openpgp.ReadKeyRing`'s own stated contract ("unsupported keys are ignored as long as at least a single valid key is found"). No signature verification, no revocation checking is performed by debark itself anywhere in this codebase. |
| Archive-package authenticity (the actual "is this `.deb` from the real Debian/Ubuntu archive" check) | Standard apt/dpkg OpenPGP verification (`apt-secure`), performed by the real `apt-get` binary, never by debark | `core/apt` (external process, ADR-001) | debark deliberately does not reimplement this. It is apt's job; debark only shells out and records what apt reported. |
| Deliberately **not** used | Encryption of any kind (no bundle or snapshot content confidentiality); any custom/non-standard cryptographic primitive; any constant-time-comparison requirement on public digests (SHA-256 digest equality checks throughout, e.g. `core/digest/digest.go:99-100`'s `Equal`, use ordinary `strings.EqualFold` — correct, since a content digest is public data and its comparison is not a secret-bearing operation; this is distinct from signature verification, which correctly uses `ed25519.Verify`, a constant-time primitive, for exactly that reason) | — | See §5: confidentiality was never a design goal. |
