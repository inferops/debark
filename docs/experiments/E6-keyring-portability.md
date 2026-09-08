# E6 — Keyring portability

**Question:** "Can a modern host resolve for an older target's
keyrings?" → settles "whether container-by-default is required for older
targets."

**Finding, up front: no rejection observed anywhere. This is a clean negative
result, not a contradiction of the design.** Every combination tested —
Ubuntu 22.04 (jammy, the release named in the brief) resolved from both
Ubuntu 24.04 and Ubuntu 26.04 hosts, and, as a bonus stress test, Ubuntu 18.04
(bionic, EOL-adjacent) resolved from Ubuntu 26.04 — produced a real,
successful, cryptographically-verified `apt-get update` with **zero**
`NO_PUBKEY` / `EXPKEYSIG` / `WEAK` / `SHA1` / `deprecated` / `insecure` /
`BADSIG` warnings, confirmed independently with `gpgv --status-fd 1` and raw
`gpg --list-packets` inspection of the live signatures. Ubuntu's real archive
keys are old-ish (2007-2018) but have been consistently RSA-4096 for the
entire period this experiment could reach; the newest available GnuPG/apt
(2.4.8 / apt 3.2.0 on Ubuntu 26.04) accepted them without complaint, including
one key whose own 2012 self-signature uses SHA-1 while the signatures it
issues *today* on live `InRelease` files use SHA-512 (see "Why this isn't a
contradiction" below).

## Method

Full detail and exact commands: `hack/experiments/e6-keyring-portability.sh`
(rerunnable; writes evidence to `hack/experiments/out/e6/`). Summary:

1. **Capture**, inside a container of the TARGET release itself (never a
   newer one), everything `snapshot-target.sh` captures: `/etc/apt/trusted.gpg.d/*`,
   `/usr/share/keyrings/*`, `/etc/apt/keyrings/*`.
2. **Ground truth**, using the TARGET's *own* `gpg`/`gpgv` (never a newer
   one): `gpg --with-colons --list-keys` and `gpg --list-packets` on every
   captured file — algorithm, key length, creation date, and every
   signature packet's digest algorithm.
3. **Verify**, inside a container of a NEWER host release: build a private
   apt root exactly the way `download-packages.sh`'s `--state` path builds
   one from a real snapshot — flatten every captured keyring file into
   `Dir::Etc::trustedparts` and carry no `Signed-By` pinning (see
   `download-packages.sh`'s `strip_signed_by()` and the
   `$SNAP/keyrings → $ROOT/etc/apt/trusted.gpg.d` copy loop). Nothing from
   the newer host's own trust store is used. Point sources at the target's
   real release pockets and run `apt-get update` **for real** — genuine
   network fetch and cryptographic verification of the target's live
   `InRelease`, which cannot be answered by `-s`/simulation.
4. **Cross-check**, independent of apt: fetch each `InRelease` with `curl`
   and verify it directly with the newer host's own `gpgv --status-fd 1`
   (reports `GOODSIG`/`VALIDSIG` plus the actual hash algorithm used for
   *that* signature) and cross-confirm via `gpg --list-packets` on the
   extracted signature block.

Primary case: target `ubuntu:22.04` (jammy, apt 2.4.14) resolved from hosts
`ubuntu:26.04` (apt 3.2.0 — the newest available, sharpest test) and
`ubuntu:24.04` (apt 2.8.3). Bonus case (run because the primary case turned
out clean, per the brief's instruction to add a real stress test in that
situation): target `ubuntu:18.04` (bionic, apt 1.6.17, EOL for standard
support but still served live by `archive.ubuntu.com` at test time) resolved
from `ubuntu:26.04`. Image digests pulled and used:

| image | digest | apt | codename |
|---|---|---|---|
| `ubuntu:22.04` | `sha256:2edbbc5dc405e9612ba3584ce95480277e3eb374407b5505fe26f17df77c7dbc` | 2.4.14 | jammy |
| `ubuntu:24.04` | `sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517` | 2.8.3 | noble |
| `ubuntu:26.04` | `sha256:2260313b31c8c011cd2eebe728008efac1b3982be73eb71348ea2648d2c0e09b` | 3.2.0 | resolute |
| `ubuntu:18.04` (bonus) | `sha256:152dc042452c496007f07ca9127571cb9c29697f42acbfad72324b2bb2e43c98` | 1.6.17 | bionic |

Test date: 2026-09-03. All fetches were real network fetches against
`archive.ubuntu.com` / `security.ubuntu.com`.

## Evidence

### A. Ground truth — jammy's captured keyring set, read with jammy's own gpg 2.2.27

Digest algo legend: 2 = SHA-1, 8 = SHA-256, 10 = SHA-512. Pubkey algo: 1 =
RSA, 17 = DSA. "Self-sig digest" = the digest algorithm of the signature
packet whose keyid matches the key's own id (the identity-binding
signature made when the key/UID was created).

| file | key (uid) | pubkey algo / bits | created | self-sig digest |
|---|---|---|---|---|
| `trusted.gpg.d/ubuntu-keyring-2012-cdimage.gpg` | CD Image Signing (2012) | RSA / 4096 | 2012-05-11 | **10 (SHA-512)** |
| `trusted.gpg.d/ubuntu-keyring-2018-archive.gpg` | Archive Signing (2018) | RSA / 4096 | 2018-09-17 | **10 (SHA-512)** |
| `usr-share-keyrings/ubuntu-archive-keyring.gpg` | Archive Signing (2012) | RSA / 4096 | 2012-05-11 | **2 (SHA-1)** |
| " | CD Image Signing (2012) | RSA / 4096 | 2012-05-11 | 10 (SHA-512) |
| " | Archive Signing (2018) | RSA / 4096 | 2018-09-17 | 10 (SHA-512) |
| `usr-share-keyrings/ubuntu-archive-removed-keys.gpg` | Archive Signing (orig., *revoked/superseded*) | **DSA / 1024** | 2004-09-12 | 2 (SHA-1) |
| " | CD Image Signing (orig., *revoked/superseded*) | **DSA / 1024** | 2004-12-30 | 2 (SHA-1) |
| `usr-share-keyrings/ubuntu-cloudimage-keyring.gpg` | UEC Image Signing | RSA / 4096 | 2009-09-15 | 2 (SHA-1) |
| " | Cloud Image Builder | RSA / 4096 | 2012-10-27 | 2 (SHA-1) |
| `usr-share-keyrings/ubuntu-master-keyring.gpg` | Archive **Master** Signing | RSA / 4096 | 2007-11-09 | 2 (SHA-1) |
| `usr-share-keyrings/ubuntu-cloudimage-removed-keys.gpg` | *(empty file — no keys)* | — | — | — |

10 public keys total across 6 non-empty files (`jammy`'s own `ubuntu-keyring`
2021.03.26 package; full listing:
`hack/experiments/out/e6/keyrings-jammy/ground-truth.txt`).

**This directly answers step 2 of the brief's method: no, jammy's archive
keys are not uniformly modern.** The two keys jammy's *default* stock trust
set actually uses (`trusted.gpg.d`, no `Signed-By` needed) are both
SHA-512-self-signed. But the *full* captured set — `snapshot-target.sh` also
captures `/usr/share/keyrings`, which is exactly what a real snapshot does —
includes real 2004-2007-vintage material: two 1024-bit DSA keys (retired,
kept only for verifying old archived `Release` files) and three RSA-4096 keys
whose own self-signatures use SHA-1, including the "Archive Signing (2012)"
key which (see part C below) is *still the live signer* for the bonus
bionic target.

### B. Bonus ground truth — bionic (18.04), read with bionic's own gpg 2.2.4

Same key material as jammy plus one extra file: `trusted.gpg.d` on bionic
directly includes `ubuntu-keyring-2012-archive.gpg` (RSA-4096, 2012-05-11,
SHA-1 self-sig — the same key that's tucked inside `usr-share-keyrings` on
jammy). 11 public keys total across the equivalent 6 non-empty files
(`hack/experiments/out/e6/keyrings-bionic/ground-truth.txt`). No DSA/RSA
material weaker than what jammy already has — Ubuntu's actual signing keys
have been RSA-4096 since at least the 2007 master key.

### C. `apt-get update` from the newer host, keys ONLY from the target's capture

Private root built per the Method above (`Dir::Etc::trustedparts` = a
directory containing *only* the flattened jammy/bionic capture — nothing
from the resolving host's own trust store).

| target → host | host apt / gpgv | exit | `apt-get update` warning-grep hits* | result |
|---|---|---|---|---|
| jammy → ubuntu:26.04 | apt 3.2.0 / gpgv 2.4.8 | 0 | 0 | **PASS** |
| jammy → ubuntu:24.04 | apt 2.8.3 / gpgv 2.4.4 | 0 | 0 | **PASS** |
| bionic → ubuntu:26.04 (bonus) | apt 3.2.0 / gpgv 2.4.8 | 0 | 0 | **PASS** |

\* grep -inE `NO_PUBKEY|EXPKEYSIG|WEAK|SHA1|deprecated|insecure|BADSIG|error|warn`
over the full `apt-get update` transcript. All three transcripts fetched and
verified `InRelease`/`Release`/`Packages` for every configured pocket
(`<release>`, `<release>-updates`, `<release>-security`) with zero matches.
Raw logs: `hack/experiments/out/e6/{primary-ubuntu-26.04,primary-ubuntu-24.04,bonus-ubuntu-26.04}/apt-update.log`.

### D. Independent cross-check — `gpgv --status-fd 1` and `gpg --list-packets` on the live signature

This is the part that actually resolves the "does it matter" question from
step 4 of the brief, by showing *which* key really signs each live
`InRelease` today and what hash algorithm secures that specific signature
(as opposed to the hash used when the key's identity was first bound, years
earlier):

| suite | signed by (today) | key's own self-sig digest | **this signature's digest** (gpgv `VALIDSIG` field 8 / `list-packets`) |
|---|---|---|---|
| jammy | Archive Signing (2018) | SHA-512 | **SHA-512** |
| jammy-updates | Archive Signing (2018) | SHA-512 | **SHA-512** |
| jammy-security | Archive Signing (2018) | SHA-512 | **SHA-512** |
| bionic | Archive Signing (**2012**) | **SHA-1** | **SHA-512** |
| bionic-updates | Archive Signing (**2012**) | **SHA-1** | **SHA-512** |
| bionic-security | Archive Signing (**2012**) | **SHA-1** | **SHA-512** |

Every row: `gpgv` printed `GOODSIG`/`VALIDSIG`, exit 0, on the newest
available stack (gpg/gpgv 2.4.8). Sample (bionic, the sharper case — signed
by the SHA-1-self-signed 2012 key):

```
gpgv: Signature made Thu Sep  3 17:37:37 2026 UTC
gpgv:                using RSA key 790BC7277767219C42C86F933B4FE6ACC0B21F32
[GNUPG:] GOODSIG 3B4FE6ACC0B21F32 Ubuntu Archive Automatic Signing Key (2012) <ftpmaster@ubuntu.com>
gpgv: Good signature from "Ubuntu Archive Automatic Signing Key (2012) <ftpmaster@ubuntu.com>"
[GNUPG:] VALIDSIG 790BC...93C 2026-09-03 1788457057 0 4 0 1 10 01 790BC...93C
gpgv exit=0
```//
(`VALIDSIG` field 8 = `10` = SHA-512.) Confirmed independently via
`gpg --list-packets` on the extracted signature block:
```
:signature packet: algo 1, keyid 3B4FE6ACC0B21F32
	digest algo 10, begin of digest d3 50
```
Full transcripts: `hack/experiments/out/e6/{primary-ubuntu-26.04,primary-ubuntu-24.04,bonus-ubuntu-26.04}/{gpgv-crosscheck.txt,listpackets-signatures.txt}`.

Separately, as a direct test of whether newer gpg refuses to even *load* the
weakest material (1024-bit DSA, 2004-vintage, unrelated to whether it's
actually used for verification): `gpg --list-keys` against
`ubuntu-archive-removed-keys.gpg` and `ubuntu-master-keyring.gpg` on the
26.04 host (gpg 2.4.8) — `dsa1024`/`elg2048`/`rsa4096`, exit 0, no warning of
any kind.

## Why this isn't a contradiction: self-signature age ≠ live-signature strength

The brief specifically asked to check whether an old self-signature hash
"actually matters for these specific keys' signature algorithms" — it
doesn't, and part D shows exactly why. A PGP key's *self-signature* (made
once, when the identity/UID was bound, e.g. 2012) is a different signature
from the ones that *same key* makes on *new documents* later. GnuPG lets a
signer choose a fresh, modern hash algorithm every time it signs something,
independent of what hash secured its own identity years before. Canonical's
signing infrastructure evidently defaults to SHA-512 for every live
`InRelease` signature regardless of which vintage key is doing the signing —
so even bionic, whose live signer is the one real key in this whole capture
set with a SHA-1 self-signature, is secured today by a SHA-512 document
signature that both apt and independent `gpgv`/`gpg --list-packets` on the
newest available GnuPG (2.4.8) accept without any special handling. The
genuinely weak material this experiment found (1024-bit DSA in
`ubuntu-archive-removed-keys.gpg`) is retired key material Ubuntu keeps
around only to verify old already-downloaded `Release` files from years ago
— it is never `KEY_CONSIDERED` by `gpgv` in any of these runs because
nothing currently live is signed with it.

## What this means for the design

- **The keyring-portability open question, answered for the cases tested:**
  yes — a modern host (apt 3.2.0 / gpgv 2.4.8 on Ubuntu 26.04, and apt 2.8.3 / gpgv 2.4.4 on
  24.04) resolves cleanly for an older target (22.04) using *only* that
  target's captured keyrings, with zero cryptographic warnings or
  rejections. The bonus case (18.04, an EOL-adjacent target actually signed
  today by 2012-vintage key material with a SHA-1 self-signature) also
  passed cleanly. **On the keyring-portability axis specifically, this
  experiment found no evidence that container-by-default is required for
  older targets.**
- **Scope of that answer:** this is one axis of the `auto` backend
  rule, not the whole rule. The backend rule selects the container backend
  whenever host apt major.minor differs from the target's, for solver-fidelity
  reasons independent of trust (E2) — that rule stands regardless
  of E6's result. What E6 specifically removes is any *additional*
  trust/keyring-based argument for forcing a container on older targets:
  there isn't one, at least for Ubuntu jammy/bionic against the newest
  available GnuPG/apt.
- **The captured-policy rule (ADR-008: "Snapshot keyrings are captured
  policy, not a root of trust... Record every fingerprint and Signed-By
  relationship... never auto-import replacement archive keys") is unaffected
  and, if anything, reinforced.**
  This experiment deliberately trusted *only* the exact keyring bytes
  captured from the target (flattened into `Dir::Etc::trustedparts`, no
  `Signed-By` narrowing — mirroring `download-packages.sh`'s `--state`
  path) and verified real, live signatures against them with no
  replacement/auto-imported key of any kind. That the newer host accepted
  this without complaint is a statement about GnuPG/apt's algorithm
  policy, not about whether the design should relax its "never auto-import"
  rule — it should not, and this experiment didn't need it to.
- **Caveat worth recording for future maintainers:** this result is
  specific to Ubuntu's own archive-signing key management, which has used
  RSA-4096 exclusively since at least the 2007 master key (part A/B). A
  target whose captured keyrings carry a genuinely small/legacy live-signing
  key (some third-party PPAs and older non-Ubuntu repositories do still sign
  with 1024/2048-bit RSA or DSA) was not exercised here and could behave
  differently — GnuPG's weak-key refusal thresholds are about key size, not
  about the age of a self-signature, and this experiment only found
  self-signature age (SHA-1, harmless) in play, never small live-signing
  key size. If a future experiment wants to actually trip a rejection, it
  would need a target whose *live* signing key itself is sub-2048-bit RSA
  or 1024-bit DSA — not just old.

## Methodology notes for whoever reruns this

- **`Dir::Etc::*` overrides must be absolute paths.** A first draft of the
  private-root construction passed a path relative to the repo root; apt
  silently treated it as relative to `Dir::Etc` (default `/etc/apt`) and
  produced `E: List directory .../partial is missing` — a real
  `apt-get update` failure with **zero** matches against the
  warning grep (it's not a trust/signature message at all). The script
  re-derives an absolute `REPO_ROOT` before building any `Dir::Etc::*`
  option; watch for this if you extend it.
- **A red herring for the "deprecated" grep term:** an earlier draft also
  set `-o Dir::Etc::Trusted=<path>` (the legacy single-file trusted
  keyring option) defensively. apt printed
  `W: .../InRelease: Loading <path> from deprecated option Dir::Etc::Trusted`
  — which matches this experiment's own `deprecated` grep, but is apt
  complaining about a **deprecated config *key name***, unrelated to any
  cryptographic policy. The final script simply never sets that option
  (only `Dir::Etc::trustedparts`), so this doesn't appear in the evidence
  under `hack/experiments/out/e6/`; noted here so a future maintainer
  who reintroduces it doesn't mistake it for a real finding.
- `gpg --list-packets` on a whole clearsigned file (as `InRelease` is)
  mis-parses in this environment; the script extracts just the
  `-----BEGIN/END PGP SIGNATURE-----` block first and runs
  `--list-packets` on that, which parses cleanly and is what part D's
  numbers come from.

## What was not completed / out of scope

Everything in the brief's required method (steps 1-5, primary jammy case
against both 24.04 and 26.04, real network `apt-get update`, ground truth
from the target's own gpg, independent `gpgv`/`gpg --list-packets`
cross-check, and the bonus older-release stress test) was completed with
real, on-disk evidence. Not attempted, as genuinely out of scope for this
brief: a non-Ubuntu target (e.g. Debian), and a target whose *live* signing
key is itself small/weak rather than merely old — see the caveat above.
