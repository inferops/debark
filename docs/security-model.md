# Security model

This is a plain-language walk-through of how debark establishes trust. Where
this page and the code disagree, the code is the specification and this page
has a bug — say so rather than trusting this page over it.

## The trust chain, end to end

```
approved archive key(s)  ──►  signed InRelease/Release  ──►  Packages hashes  ──►  .deb files
  (captured policy;             (apt verifies during           (apt verifies)        (sha256 in lock)
   fingerprints recorded)        update/fetch)
                                                                                        │
                          deterministic bundle + canonical manifest  ◄──────────────────┘
                                          │
                        operator/organisation signature (Signer)
                                          │
                 target: verify manifest signature + every file digest + repo metadata digests
                                          │
                                   only then: apt sees the repository
```

Three different questions get three different, never-conflated answers
(principle 4):

1. **Is this archive package authentic?** apt's job, on the builder, using
   the keys the snapshot captured. debark never re-implements or
   second-guesses it.
2. **Did the bundle arrive intact and from who I think built it?** The
   manifest's signature and every file's digest — checked by `verify`, on the
   target, before apt is ever pointed at the bundle's repository.
3. **Should apt trust the bundle's repository as a source at all?** A
   decision the target's own apt configuration makes, taken *after* step 2
   has already passed — never before.

## What a snapshot's keyrings are, and are not

A snapshot carries the target's `trusted.gpg.d/*` and every keyring a
`sources.list` entry names with `Signed-By`. That is **captured policy, not
an independent root of trust.** If the machine the snapshot was taken on was
already compromised, the snapshot can carry a malicious source *and* the key
that would make it look authenticated — nothing about capturing that state
onto a builder changes what it proves.

Two things follow:

- Every fingerprint and `Signed-By` relationship the snapshot recorded is
  written into the lock, so a reviewer sees exactly which key authenticated
  which archive years later — `snapshot inspect` and `debark doctor` surface
  it too.
- An organisation that wants a stronger guarantee than "whatever this
  particular machine happened to trust" can supply `--approved-keys FILE`: a
  fingerprint allow-list every apt source the snapshot carries must satisfy
  before resolution runs. A source that fails it fails the build as a policy
  violation (exit 6), not a warning.

  What the check compares matters more than the fact that it exists. The
  fingerprints come from the keyring bytes the private root actually writes
  into `trusted.gpg.d` — the bytes apt will really verify against — never
  from the snapshot document's own `keyring_fingerprints` list
  (`core/apt/root.go`, `copySnapshotConfig` and `checkApprovedKeys`). That
  distinction is the whole control: a tampered snapshot can record the
  genuine archive key's fingerprint next to an attacker's key material, so a
  check against the document's own claim would approve the attacker's key
  and report success.

  Two limits to know before relying on it. It is enforced per source *file*,
  not per suite: a deb822 stanza naming several suites is approved or
  rejected as a whole. And it fails closed on anything it cannot trace to a
  captured keyring — a source with no path-form `Signed-By`, a `Signed-By`
  whose keyring the snapshot did not capture, an inline armoured key, or a
  keyring whose bytes no OpenPGP parser can read. Failing closed is the
  right answer for all four (nothing is in a position to say which key apt
  would verify those sources against), but it does mean a target still using
  a pre-`Signed-By` `sources.list` fails an approved-keys build outright
  rather than passing it quietly.

debark never auto-imports a replacement archive key on either side of the
gap.

## A snapshot from `--base` is an assumption about the target, not a measurement of it

There are two kinds of snapshot, and `origin.kind` in `snapshot.json` says
which one a bundle was built from (`docs/formats.md` §3.1). The distinction
is not bookkeeping. It changes what the bundle proves.

A **captured** snapshot is a measurement. `snapshot create` read that
machine's dpkg status, sources, pins and apt configuration, and the builder's
apt answered with exactly what is missing *there*. The bundle is proven
complete for that machine.

A **synthesized** snapshot is a claim about a release. `snapshot from-base`
(and `build --base`, which does the same thing inline) resolves a base
definition's seed packages with the release's own apt in a private root where
nothing is installed, and writes what apt says a stock install contains as
the snapshot's dpkg status. It exists because you cannot snapshot a machine
that has not been installed yet — fifty boxes arriving at a site with no
network — and because a first evaluation should not require physical access
to the air-gapped side. Nothing in it was measured on the eventual target.
**No machine was consulted at any point.**

What it therefore does and does not claim:

- **It claims** that a default install of that release, as the base
  definition describes it, already contains the listed packages, and that the
  bundle contains everything else the requested packages need. Every part of
  that is still resolved by the release's own apt; debark does not curate
  package lists.
- **It does not claim** that the machine you install it on is such a system.
  If the real machine has *more* packages than the base assumed, the bundle
  is merely larger than it needed to be — harmless, and the direction the
  base definitions deliberately err toward: every variant seeds the smaller
  of two plausible metapackages, and recommends default to off. If the real
  machine has *fewer*, the bundle is short and fails at the gap, which is the
  failure this tool exists to prevent.
- **It does not claim** anything about the base definition itself if you
  wrote it. A definition file is operator input, exactly like a `packages.txt`
  or a policy file, and it is trusted the way the builder's own inputs are
  trusted. `origin.source` and `origin.source_digest` record which definition
  was used and its exact content, so a changed definition is visible rather
  than silent.

`install` **warns and continues** rather than refusing, and that follows the
existing rule that doctor-style findings warn while policy fails. It runs on
the target with the real dpkg status in front of it, so it can compare the
base's assumed set against reality almost for free and say how far apart they
are. Refusing would be wrong twice over: a machine that diverges upward from
the base installs perfectly well, and a machine that diverges downward will
fail anyway, at apt, with an unmet dependency naming the actual gap. The
warning turns a silent failure on the far side of an air gap into a legible
finding on the near side of one; it is not a gate, and no operator should
read it as having proven anything about the machine.

None of this touches who vouched for the bundle. The manifest signature, the
requirement that the operator's public key reach the target independently of
the bundle's media, and the rule that `verify` completes before apt is ever
pointed at the repository are all exactly as described above and below, and
are unaffected by which kind of snapshot the build started from. A
synthesized base changes what a bundle *contains*; it never changes who
signed it, what the signature covers, or the order in which the target checks
it.

One field is new in the bundle because of this: `origin.assumed_installed`,
the base's assumed installed set, which travels inside `snapshot.json`. It is
there because a bundle carries `snapshot.json` but not the snapshot's files,
so on the target it is the only copy of the assumption there is — without it
`install` could say only "a base was assumed", not which packages it assumed.
It is safe to carry for a reason that does not generalise: it describes a
public stock release, not a real machine. The installed-package list of a
*real* target is a sensitive inventory and never travels in a bundle, which
is why the field is refused outright on a captured snapshot rather than
merely left empty.

## The manifest signature is the thing that actually matters here

`verify` treats an unsigned bundle as untrusted **by default**. The only way
past that is `--allow-unsigned`, and it has to be typed out by the person
running the command — there is no config setting that silently makes it the
default, and a verify report produced with it set says so explicitly
(`"signed": false`) rather than just reporting "OK".

The signing key itself comes from a pluggable `Signer`:

- an ed25519 private key file (`debark keygen`), the default — no
  passphrase, because an operator who wants one already has `gpg`;
- `gpg:<keyid>`, for shops that mandate GnuPG;
- `plugin:<name>`, an out-of-process executable speaking
  `debark.plugin/v1` over stdio — an HSM or KMS integration drops in here
  without debark ever linking against a vendor SDK.

**The operator public key must reach the target independently of the
media it's verifying** — pre-provisioned in config, baked into a
distribution package, or a fingerprint compared by hand over a channel the
media itself didn't carry. Point `--key` or `--keyring` at a path that
resolves inside the bundle and `verify` refuses outright
(`same-media-key-refused`); there is no flag that lifts it. The
`AllowSameMedia` override in `core/sign`'s library API is deliberately not
wired to any CLI flag — `internal/cli/cmd_verify.go` builds the key source
from `--key` and `--keyring` and nothing else. A key that travelled with the
bundle proves nothing about who signed the bundle, because whoever tampered
with the bundle could have replaced the key alongside it.

**If you sign with `gpg:`, pass `--gpg-keyring`, not `--keyring`.** The two
are different things and are deliberately not overloaded onto one name:
`--keyring` takes a *directory of debark ed25519 `.pub` files*, while
`--gpg-keyring` takes a *gpg keyring FILE* holding the expected release
key(s), and it is the only way to narrow the trust set for a gpg-signed
bundle. Given one, `core/sign` runs gpg with `--no-default-keyring
--keyring PATH`, so the keyring you named is the whole trust set. Omit it
and gpg falls back to whatever is imported for the invoking user — which
`verify` reports as a warning rather than leaving you to infer it, and that
warning now names `--gpg-keyring` as the remedy (`core/verify/verify.go`).
Both `verify` and `install` accept the flag.

Three properties of the gpg path are worth knowing because getting any of
them wrong would be quiet rather than loud, and all three are handled:

- **A revoked or expired key is refused.** gpg prints `REVKEYSIG`,
  `EXPKEYSIG` and `EXPSIG` *alongside* `VALIDSIG`, not instead of it, so a
  verifier that looks only for `VALIDSIG` accepts a signature by a stolen,
  revoked key — which would make revocation, an organisation's only remedy
  after a key compromise, completely inert. `core/sign/gpg.go` checks the
  disqualifying keywords first and reports each with its own remedy.
- **Signing with a subkey works, and the fingerprint you compare is the
  primary's.** `VALIDSIG`'s first field is the subkey that actually signed;
  debark records the primary key's fingerprint, because that is the
  identity an operator compares by hand and the one an offline-primary
  setup — the arrangement a shop mandating gpg is most likely to run —
  publishes.
- **gpg's exit status is read as well as its status lines.** A `VALIDSIG`
  from a gpg that still exited non-zero is refused, not accepted: it
  refused for a reason this parser has no keyword for, and believing the
  line we recognise over the verdict we do not is exactly the mistake
  `REVKEYSIG` was.

`--trust-model always` is used deliberately and does not weaken any of this.
It removes gpg's *ownertrust* database — ambient state in the verifying
machine's `GNUPGHOME`, which would make the same bundle and the same keyring
verify differently on two targets — from a decision debark makes for
itself. Revocation and expiry are key state, not ownertrust, and gpg reports
them either way.

A second, apt-level GPG signature over the bundle's *own* repository
metadata (`InRelease`/`Release.gpg`) is designed for but **not implemented**,
as of 2026-09-05. The consuming half exists — the manifest has
`inrelease_sha256` and `release_gpg_sha256` fields and `verify` checks both
when they are present (`core/manifest/types.go`, `core/verify/verify.go`) —
but nothing produces them: `repository.Input` has no signer, `build` has no
flag for one, and no bundle debark writes today contains an `InRelease` or
a `Release.gpg`. If your policy requires a standard apt-level signature on
the repository in addition to the manifest signature, debark does not
satisfy it yet; do not read the schema fields as evidence that it does.

This does not weaken the chain, because that signature was never what the
chain rests on. The manifest is what binds the repository, the lock and the
snapshot digest together as one signed object, and it is checked before apt
sees the repository at all. It is also precisely why `install` can use
`Trusted: yes` on its private source: apt performs no independent check of
repository metadata at install time, so if the manifest-level check were not
already done, nothing would be checking. A repository signature would be a
second, standard, apt-native layer *underneath* that one — useful for shops
whose policy names it, never a substitute for it.

## What `verify` actually checks, and in what order

In order. Steps 1–3 gate whether the manifest can be trusted at all, so each
stops the run. Steps 4–7 all keep going and accumulate every problem they
find, so a tampered medium's full extent is visible in one run rather than
one failure at a time (`core/verify/verify.go`, the step comments in
`runVerify`):

1. The manifest parses and its schema version is one this build knows.
2. The signature file's recorded digest matches the manifest's own canonical
   bytes — catches a manifest edited after signing.
3. At least one signature verifies against a key you supplied out of band
   (unless `--allow-unsigned`).
4. Every file the manifest lists exists, **as a regular file that carries its
   own bytes**, at the recorded size and digest.
5. No unexpected file, and no entry that is not a regular file, is present in
   the bundle.
6. The repository index digests (`Packages`, `Release`) match what the
   manifest recorded.
7. The lock's digest matches the manifest's `lock_digest`, and the
   snapshot's digest matches `snapshot_digest` when a snapshot is present.

The words "carries its own bytes" in steps 4 and 5 are doing real work, and
they are there because the absence of them was a live hole. A **symlink**
sitting at a manifest-listed path used to verify cleanly: `verify` opens the
path, follows the link, hashes the bytes at the other end, and they match —
because the attacker put the right bytes there. But those bytes never lived
under the tree that was verified, so they can be rewritten between the
moment `verify` hashes them and the moment apt reads the same path, without
touching the medium that was just approved. That turns a narrow race into
"pre-position a file and wait." The rule now holds at both ends:
`core/bundle`'s tar reader refuses `TypeSymlink` and `TypeLink` outright on
the way in ("bundles never contain links"), and `core/verify`'s walk refuses
any entry whose type is not a plain regular file — symlink, device, socket,
FIFO — reporting `file-not-regular`. That is deliberately a separate problem
kind from `file-unexpected`: a script may reasonably tolerate a stray editor
backup, and must not thereby tolerate a listed path whose contents live
somewhere verification never covered.

A **hard link is accepted**, and that is deliberate rather than an omission;
an earlier version of the check refused any regular file with a link count
above 1, and it was removed. The threat the count was guarding against
cannot take this form: a hard link must live on the same filesystem as its
inode, so "another name on a filesystem that may still be writable when the
medium is not" describes a symlink and not this. Read-only media gives every
name the same read-only inode, and where the bundle sits on writable
storage, whoever can rewrite it through a second name can rewrite it through
the first — permissions are the control there, not link counts. Refusing it
also broke the product: `store.Materialise` hard-links pool objects out of
the content-addressed store into the bundle, which is what makes assembly
cheap and is sanctioned, so every freshly built bundle
carried link count 2 on every `.deb` and failed verification in place. Only
a bundle round-tripped through tar export and import passed, because the tar
reader refuses link entries and the extracted copy has a single name. A hard
link at a path the manifest does not list is still refused — as
`file-unexpected`, like any other unlisted file. The reasoning is written
into `core/verify/verify.go` at the check itself, because the check reads as
obviously correct and would otherwise be re-added.

Every failure is a specific, actionable `Problem` — `file-digest-mismatch`
(the real `Problem.Kind` constant, `core/verify/iface.go`) at
`repo/pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb`, not "verification failed";
that is the literal, human-readable message a tampered bundle produced in a
real run — see `docs/quickstart.md`'s security demo.
`debark verify --json` emits the whole `Report`, machine-readable, for a
pipeline that wants to act on it without scraping text.

## What `verify` does not cover: the target's own pins

Everything above is about the bundle arriving intact and from who signed it.
It says nothing about a target whose `apt_preferences` pins a package away
from the version the bundle carries — because the install path never gives
apt the chance to make that decision. It plans from the lock and installs an
exact, architecture-qualified version (`apt-get install name:arch=version`),
which either matches what the target's own pins would have chosen online or
it did not get built that way in the first place
(`core/install/select.go`, `selectInstallSet`).

`install --upgrade` used to be the exception, and was a real gap rather than
a hypothetical: it ran a plain `apt-get full-upgrade` against the bundle's
repository, with no comparison against the lock. Experiment
[E3](experiments/E3-pin-fidelity.md) reproduced the failure directly — a
bundle carrying more than one version of a package (the normal outcome of
the additive default) has `Release` fields that no longer carry the
origins the target's pin was written against, so a free re-resolution
silently reselects the highest-numbered version, the one the pin exists to
reject, and reports ordinary zero-network success.

**That is fixed.** `--upgrade` now installs exactly the versions the online
solve's own full-upgrade pass recorded in the lock
(`lock.Package.Reason == "upgrade"`), folded into the same exact-version
`apt-get install name:arch=version` call the default path already uses;
there is no `full-upgrade` invocation left on the target side, for the plan
or for the apply (`core/install/select.go`, `selectUpgradeSet`;
`core/install/runner.go`). This is ADR-007's rule applied one stage later:
never let a free re-resolution override what the lock already decided. Where
a package appears in the lock both as part of the requested closure and
separately with reason `upgrade`, the closure's version wins — it came from
a harder constraint, and apt cannot be asked for two versions of one
`name:arch` in a single call.

What remains outside the guarantee is narrower and worth stating plainly:
debark compares the installed result against the lock, not against the
target's preferences file. If the target's pins disagree with what the
*builder's* apt chose — because the pins were edited after the snapshot was
taken, say — the install still proceeds to the locked version, and neither
`verify` nor the closed-world check will object. Exact-version installs make
that visible rather than silent (apt names the version it is installing),
but the reconciliation is still the operator's.

## `--keep-source` leaves a permanently trusted apt source behind

By default `install` touches nothing outside the packages it installs. It
builds a private, temporary apt view in a scratch directory, points apt at
it with `-o Dir::Etc::sourceparts=...`, installs, and deletes the directory
(`core/install/privateroot.go`). The source file it writes there says
`Trusted: yes`, which is safe for exactly one reason: `verify` has already
proven the repository's own `Packages` and `Release` digests against the
signed manifest before apt was allowed to look at them, and the source stops
existing when the command ends.

`--keep-source` changes both halves of that. It copies the same
`Trusted: yes` stanza into `/etc/apt/sources.list.d/debark-bundle.sources`
on the real system, permanently, pointing at the bundle's `repo/` directory
by absolute path (`PersistSource`, `core/install/privateroot.go`). From that
moment on:

- **Every future `apt install` and `apt upgrade` on that machine reads that
  directory, as root, with signature checking disabled for it.** `Trusted:
  yes` is apt's instruction to skip `apt-secure` entirely for a source.
- **Nothing re-verifies the directory, ever again.** debark's manifest
  check runs once, inside the `install` command that wrote the file. Anyone
  who can write a `.deb` and a `Packages` entry into that directory
  afterwards — the media it lives on, a later `install` run, any local
  process with write access to the path — has a root-level package
  installation channel on an air-gapped machine, with no signature to forge
  and no digest to match.

That is a deliberate, opt-in exception (the Bash prototype's
`install-offline.sh` has the same flag), and it is genuinely useful when the
bundle lives on managed local storage and you want `apt install` of a
package the bundle already carries to keep working afterwards. It is not a
convenience flag. If you use it: put the bundle somewhere only root can
write, keep it out of removable media's mount path, and treat the directory
as part of the machine's trusted computing base from then on. If you only
want to install the bundle's contents once, leave the flag off — the default
already does that and leaves nothing behind.

The flag's own `--help` text describes only the mechanism ("write the
bundle's .sources file permanently instead of a temporary one"), not this
consequence.

## Redistribution is a warning, never a block

Packages from `multiverse`, `restricted`, `non-free`, and raw vendor `.deb`
files you supplied yourself carry their own licensing terms that debark
has no authority to adjudicate. It records the provenance of every file
(source package identity, where it came from, whether a digest or signature
backs it) and warns loudly — `doctor` calls it out by name, the bundle's
README states it in plain language — but it never refuses to build the
bundle over it, and `--acknowledge-redistribution` exists only to suppress
an interactive prompt for scripted builds, not to change what gets recorded.
**The operator is responsible for whether they are authorised to move a
given package outside their organisation** — the same as it would be if they
copied the `.deb` by hand. Packages are never modified in transit: debark
is a pass-through fetcher plus locally generated indices, nothing more.

## What crosses the gap, and what never does

The bundle contains: a real flat apt repository (`Packages`, `Packages.gz`,
`Release`, and the pool of `.deb` files themselves), the lock plan recording
exactly what was chosen and why, the signed manifest, the input snapshot
(with any redacted fields already absent), an optional SBOM, and a
structured evidence log of the build itself.

The SBOM is real: `build --sbom` renders CycloneDX from the finished lock
and writes `sbom.cdx.json` into the bundle, where the manifest covers it
like every other file (`core/engine/sbom.go`, called from
`core/engine/finalize.go`). It is rendered from the lock rather than from
the pool so that what the SBOM describes and what the lock names cannot
drift apart. It is off unless you ask for it; it is free and uncapped when
you do, per `docs/free-paid-policy.md`.

What never leaves the builder: signing keys, of any kind. What never leaves
the target in a snapshot unless explicitly asked for: the machine id and
proxy credentials are captured only because Ubuntu's phased-update selection
needs the machine id to reproduce faithfully (`snapshot create --redact`
strips both, and resolution falls back to the conservative
never-include-phased-updates policy instead) — **for the automatic
full-upgrade pass specifically.** Experiment
[E1](experiments/E1-phased-updates.md) measured that apt bypasses phasing
entirely for a package named explicitly on the command line, machine id or
redaction notwithstanding, so a redacted snapshot's "conservative fallback"
does not extend to a plain, named-package install — the main workflow this
tool exists for. That is not a hole in this tool's behaviour (apt itself
does the same thing on the target), but it does mean `--redact` should not
be read as "this bundle will only ever carry fully-phased versions of what
I asked for"; it only makes that guarantee for `--upgrades`. Labels are
opt-in metadata, off unless you pass `--label`. There is no telemetry, no
update check and no phone-home anywhere in this tool, on either side of the
gap — see `docs/faq.md`.
