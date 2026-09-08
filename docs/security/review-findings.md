# Security review findings

Code as read on **2026-09-03, 20:49–21:12 UTC** (the codebase was
under concurrent edit by other packages throughout this review; every finding
below cites the state at the timestamp given and was re-checked at least once
against a later state before being finalised here). Ranked by severity.
This record was written by a package that owns documentation only, per
`docs/dev/contract-brief.md`, so nothing here was fixed by its author; each
finding names the owning package that had to pick it up.

**Every finding below is the text as written on 2026-09-03, with a `Status:`
line added to it recording where it stands in the code today.** Nothing in
the original text has been softened or removed: where a finding is fixed, it
still describes the bug that was there, and the status line names the code
that closed it. Where one is still open, the status line says so plainly.
Read the status line first — several of these findings describe live
vulnerabilities that no longer exist, and a status line is the only thing
separating "this was true once" from "this is true now".

**Status at a glance:** F1 **fixed**. F2 **fixed**. F3 **fixed**. F4
**addressed in part** — the underlying gpg behaviour is unchanged by design
and is now reported to the operator. F5 **open**, by the same design
decision it names. F6 **fixed**. F7 **open**, accepted as architectural. The
positive findings F8–F10 were spot-checked and the guards they record are
still in place, though F8's helper has since been rewritten (see its own
note).

---

## HIGH

### F1 — Path traversal / arbitrary file write during `debark build`, via an unsanitised package name from an untrusted `.deb`

> **Status: FIXED.** The write described below cannot happen. Read this
> before the finding, not after it: the attack sketch further down is a
> record of what was possible on 2026-09-03, not a live hole.
>
> `repository.PoolPath` no longer interpolates an unvalidated name into a
> path. It returns `""` unless the package name and the filename both pass
> `ValidPackageName` and `ValidFilename` (`core/repository/iface.go`, lines
> 111–168): the name must match Debian Policy's lowercase-alphanumeric set
> plus `+`, `-` and `.`, which contains no path separator and no parent
> reference; the filename must be a single path element, free of `/`, `\`,
> `:` and control characters, not begin with a dot, and end in `.deb`.
>
> ```go
> func PoolPath(pkg, filename string) string {
> 	if !ValidPackageName(pkg) || !ValidFilename(filename) {
> 		return ""
> 	}
> ```
>
> `core/bundle/assemble.go`'s `materialiseSelections` refuses loudly on that
> empty return (`dferr.Usage`, "refusing package with an unsafe name or
> filename") instead of writing anywhere, and then confirms the resolved
> `dest` is still inside `repo/pool/` before any `store.Materialise` call —
> exactly the defence in depth this finding asked for, and both halves of it.
> `core/apt/local.go` runs the same `ValidPackageName` check (line 1434)
> before a name can become an apt operand.
>
> The regression test is `core/repository/poolpath_security_test.go`
> (`TestPoolPathRefusesTraversal`). Its hostile table carries this finding's
> own sketch, `../../../../tmp/evil`, plus absolute names, separators,
> backslashes, a NUL, an uppercase name and a leading-dash name — and it
> additionally asserts that joining the *unrefused* form would in fact have
> escaped, so the test is guarding a real hazard rather than an imagined one.
>
> **What was not done, stated plainly:** only half of the recommendation
> below was taken. `core/engine/debcontrol.go` and `core/fetch/control.go`
> still extract `Package:` with no grammar check at the point of extraction.
> What closes the arbitrary write is the refusal at the write boundary, not
> validation at the parse boundary. A `.deb` with a non-conforming name
> therefore fails later, and with a different message, than rejecting it on
> extraction would have produced — but it fails, and nothing is written
> outside the pool.

**Where:** `core/engine/debcontrol.go` (`dpkgDebPackageName`) and
`core/fetch/control.go` (`parseControlStanza`) extract a `.deb`'s `Package:`
control field with **no validation against Debian's package-name grammar** —
only a non-empty check. That value flows, unsanitised, into
`core/repository/iface.go`'s `PoolPath(pkg, filename string) string`, which
builds `"pool/" + prefix + "/" + pkg + "/" + filename"` directly from it.
`core/bundle/assemble.go`'s `materialiseSelections` then does:

```go
relPath := repository.PoolPath(sel.Name, sel.Filename)
dest := filepath.Join(repoDir, filepath.FromSlash(relPath))
...
st.Materialise(useDigest, dest)   // an actual hardlink-or-copy write to dest
```

`filepath.Join` resolves `..` segments arithmetically. `sel.Name` (a
`resolve.Selection.Name`) is populated from the unsanitised control-field
extraction above for any external/vendor `.deb` input (confirmed via
`core/engine/lockbuild.go:33,37` and `core/engine/resolve.go:126`).

**What an attacker does:** publish (or place in `local-debs/`) a `.deb` whose
control file declares, e.g., `Package: ../../../../../../tmp/evil`. When
`debark build` processes it as an external input, the file is written
outside `repo/pool/` — on the **builder** host, not the target — at a
location chosen by the attacker (bounded only by how many `../` segments they
use versus the depth of the bundle's output directory). This is adversary
"a malicious or compromised vendor `.deb` URL" from `docs/threat-model.md`
§3.3, and it is reachable end-to-end in the current tree, not merely latent:
`core/bundle.Assemble` (previously a stub) is now implemented and calls this
exact path.

**Contrast:** `core/fetch/fetcher.go`'s `sanitizeFilename` (used for a
`Content-Disposition`-supplied filename from the same untrusted download)
gets this right — rejects path separators, `..`, control characters,
absolute/drive-letter forms. No equivalent exists for the package-name path
component used to build the *directory* part of a pool path.

**What to change (in `core/engine` and `core/repository`/`core/bundle`):**
validate `Package`, `Architecture` (and ideally `Version`) against Debian
Policy §5.6.7's grammar (`[a-z0-9][a-z0-9+.-]+`) immediately after extraction,
in both `core/engine/debcontrol.go` and `core/fetch/control.go`, rejecting a
non-conforming external `.deb` the same way an unresolvable one is already
rejected (resolve together; report each unresolved
name). As defence in depth, `repository.PoolPath` or
`bundle.materialiseSelections` should also refuse to write a `dest` that
isn't lexically confined under `repoDir` (Go's `filepath.IsLocal(relPath)`,
or an explicit `filepath.Rel` + reject-a-leading-`..` check) before any
`store.Materialise` call.

**Severity:** High. Arbitrary file write on the builder host, triggered by
processing attacker-controlled input the product is specifically designed to
accept (vendor `.deb` URLs and `local-debs/` are first-class inputs). This
is exactly the bug class the review brief asked to be hunted hardest for.

---

## MEDIUM

### F2 — Zero automated test coverage for `core/verify` and `core/sign`

> **Status: FIXED.** Both packages are now covered, and covered in the shape
> this finding recommended. `core/verify` carries nine test files — 52 test
> functions — of which `verify_test.go` and `tamper_test.go` are the threat
> model's tamper matrix turned into a suite (`tamper_test.go`'s own opening
> comment: "This file extends verify_test.go's tamper matrix"), alongside
> `dirs_test.go`, `links_test.go`, `lockpool_test.go`, `notabundle_test.go`,
> `sigblocks_test.go`, `sigfields_test.go` and `gpgwarning_test.go`.
> `core/sign` carries eight — 89 test functions — including
> `verifier_test.go`, which is where `sign.VerifierFor`'s same-media refusal
> (the control this review examined most closely, F8) gets its positive and
> negative cases.

Both packages are fully implemented as of this review and, on careful manual
reading (documented row-by-row in `docs/threat-model.md` §4), appear
correct — but neither has a single `*_test.go` file yet. These are the two
packages that *are* the product's central trust guarantee. Contract-brief's
own definition of done requires "Unit tests for parsers, pure functions and
error paths; table-driven where the input space is wide." Recommend the
twelve rows of the threat-model tamper matrix become the first test suite
written for `core/verify`, plus positive/negative cases for
`sign.VerifierFor`'s same-media refusal specifically (the one control this
review examined most closely — see F8).

### F3 — Vendor-URL credentials are never redacted before reaching shipped artefacts

> **Status: FIXED**, and closed more widely than the recommendation asked
> for. `core/fetch/redact.go`'s `RedactURL` is the single place the project
> decides how a possibly-credentialed URL may reach a persisted artefact: it
> replaces the userinfo with a visible `REDACTED` marker — visible on
> purpose, so a URL that carried a credential does not read as one that
> never had one — and drops the query string whole rather than guessing
> which parameter is the presigned-URL signature. It also has a textual
> fallback, `redactUnparsed`, for the strings `url.Parse` refuses, which are
> precisely the strings an attacker would use to route a password around a
> parser-based redactor.
>
> It is wired on every path this finding named and several it did not:
> `core/fetch/fetcher.go` redacts at each evidence and message site
> (`safeURL := RedactURL(in.URL)`), `core/engine/lockbuild.go` runs
> `o.URI = fetch.RedactURL(o.URI)` before an origin is written into
> `lock.json`, `core/bundle/assemble.go` redacts `l.Packages[i].Origin.URI`
> again on the way into the bundle, and `core/policy/evaluate.go` and
> `core/apt/local.go` redact URIs in policy detail and in text destined for
> the bundle. The full URL, credentials included, is still what the actual
> HTTP request uses — exactly as recommended.

**Where:** `core/fetch/fetcher.go`, `attempt()` and `progressWriter.emit()`
record `in.URL` — the operator's literal input string, which may be
`https://user:token@host/pkg.deb` — verbatim into evidence events, and
`Fetched.URL = in.URL` is the value that becomes `lock.Origin.URI` once the
engine wires a fetch result into a lock entry. Both `evidence.json` and
`lock.json` ship inside the bundle and cross the air
gap.

**Why it matters:** `core/evidence/types.go`'s own doc comment states "every
value must be JSON-encodable and must never contain a secret." A URL-embedded
token — a common pattern for an internal artifact repository (Artifactory,
Nexus, a CI package feed) — currently violates that invariant on every build
that uses one, shipping the credential to the target machine and into
permanent audit records.

**Recommendation:** strip `url.URL.User` before a URL is written into any
persisted artefact (evidence, lock, README); keep using the full URL,
credentials included, for the actual HTTP request.

**Severity:** Medium. Requires the operator to have used a credentialed URL
in the first place, but when they do, the leak is total, silent, and
permanent (baked into every copy of the bundle).

### F4 — GPG verification silently trusts the verifying machine's default keyring when `--keyring` is omitted

> **Status: ADDRESSED IN PART — the gpg behaviour is unchanged, and is no
> longer silent.** `core/sign/gpg.go` still falls back to the ambient
> default keyring when `KeySource.GPGKeyring` is empty; that is standard gpg
> behaviour, it was documented as intentional then, and it was deliberately
> not changed.
>
> What landed is the second half of the recommendation. `core/verify/verify.go`
> tracks `usedDefaultGPGKeyring` — set when a signature block of kind
> `SignerGPG` verifies while `opts.Keys.GPGKeyring == ""` — and appends a
> warning to the report saying so, "regardless of which branch the switch
> below takes", so the fact is recorded even when the overall verdict is a
> failure for some other reason. `core/verify/iface.go` names it in
> `Report.Warnings`' own doc comment ("a gpg check that ran against this
> machine's ambient default keyring"), and
> `core/verify/gpgwarning_test.go` tests both that it fires for gpg and that
> it does not fire for `ed25519-file`. The CLI half is done too: the flag is
> `--gpg-keyring` on both `verify` and `install`, and its help text states
> the fallback outright.
>
> What this finding asked for that is *not* enforced: nothing makes
> `--gpg-keyring` mandatory. An operator who omits it still gets a
> successful verification, now with a warning attached.

**Where:** `core/sign/gpg.go`'s `newGPGVerifier`: an empty
`KeySource.GPGKeyring` produces a `gpgVerifier` with no `--keyring` override,
so `gpg --verify` falls back to the ambient default keyring on whatever
machine runs `verify`/`install`.

**Why it matters:** this is standard, well-known `gpg` behaviour and is
explicitly documented as intentional (`sign/iface.go`'s `KeySource.GPGKeyring`
doc comment) — it is not a coding bug. But it means a `gpg`-signed bundle can
pass verification on the strength of a key that happens to already be
imported into that keyring for a reason unrelated to debark, with zero
explicit configuration by the operator. The ed25519-file path has no
equivalent gap: an empty `KeySource` there produces a genuinely empty trust
set. Worth naming because it is exactly the kind of thing an approver will
ask about, and because "an operator forgot `--keyring`" is a much easier
mistake to make silently than "an operator forgot to sign."

**Recommendation:** treat `--keyring` as effectively mandatory in CLI
documentation and help text for `gpg:`-based verification; never show an
example that omits it. Consider having `verify --json` note explicitly when
a GPG check ran against the caller's default keyring rather than an
explicit one, so the fact is visible in the report, not just in
documentation. Already recorded as an operator responsibility in
`docs/threat-model.md` §6.

### F5 — Proxy credentials in `apt.conf` are captured unredacted by default

> **Status: OPEN, unchanged.** `--redact` is still opt-in and still defaults
> to false (`internal/cli/cmd_snapshot.go`), and
> `core/snapshot/capture.go` runs `doRedact(s, fs, RedactMachineID,
> RedactProxies, RedactLabels)` only inside `if opts.Redact`. Userinfo
> stripping specifically was not split out to run unconditionally, so the
> residual concern this finding raises stands exactly as written. It was
> always framed as a question for the security-model owners rather than as a
> code defect, and that question has not been answered by a change.

Design-level observation, not a code bug: `snapshot create` without
`--redact` captures `apt.conf`/`apt.conf.d/*` verbatim, which can contain
`Acquire::http::Proxy` credentials. This is a known, already-documented
tradeoff, and the redaction logic itself, once invoked,
is precise and well-scoped (`core/snapshot/redact.go`'s `redactProxyBytes`:
userinfo-only stripping, regex-matched against actual proxy directives and
credentialed URLs, leaving host/port/path intact). The residual concern is
purely that redaction is opt-in, while a snapshot is routinely handled by
someone other than whoever configured the proxy. Recommend the security-model
owners consider whether userinfo-stripping specifically (not the rest of
`--redact`'s broader scope) should happen unconditionally.

---

## LOW

### F6 — Unbounded decompression of a vendor `.deb`'s `control.tar.gz`

> **Status: FIXED.** `core/fetch/control.go` now reads the control member
> through `limited := &io.LimitedReader{R: tr, N: maxControlSize + 1}` with
> `maxControlSize = 8 << 20`, and checks `limited.N == 0` afterwards so that
> "read exactly the limit and stopped" is refused rather than parsed as a
> short file — a truncated control file that silently parsed, perhaps having
> dropped the very field that would have looked wrong, would have been worse
> than refusing. The limit is set about three orders of magnitude above a
> real control file, so no legitimate `.deb` is rejected. The code comment
> cites this finding by name, and `core/fetch/control_bomb_test.go` covers
> both directions: `TestReadControlInfo_RejectsDecompressionBomb` and
> `TestReadControlInfo_AcceptsLargeButUnderLimitControl`.

**Where:** `core/fetch/control.go`'s `readControlMember`:
`body, err := io.ReadAll(tr)` on a gzip-wrapped tar entry, no size cap.

**What an attacker does:** a crafted vendor `.deb` with a small, highly
compressed, enormous `control.tar.gz` could exhaust builder memory while
`debark build` processes external `.deb` inputs — a decompression bomb.

**Severity:** Low. `SECURITY.md` already scopes this class of issue as
"denial of service against your own machine from... input you chose to trust
... unless it demonstrates a verification bypass," which this does not. Cheap
to close whenever this file is next touched: wrap the reader in
`io.LimitReader` — a legitimate control member is a few KB.

### F7 — TOCTOU between `install`'s verification and apt/dpkg's later reads of the same directory

> **Status: OPEN, and accepted rather than fixed** — which is what this
> finding proposed. `core/install/runner.go`'s `execute` still verifies once
> (`r.deps.Verifier.Verify(ctx, bundleDir, opts.Verify)`) and then hands the
> same on-disk path to apt/dpkg several steps later. The single-`execute`
> shape is deliberate and documented in the function's own comment: `Plan`
> and `Apply` share one body so that "Apply runs Plan and then performs the
> installation" is true of the actual control flow, "and so a bundle is never
> re-verified or re-simulated partway through applying it". Nothing re-reads
> or re-hashes those files at the moment apt uses them, and the architectural
> reason given below — apt reads by path, not by a descriptor debark could
> hold — has not changed.

**Where:** `core/install/runner.go`'s `execute()` verifies the bundle
directory once, then — several steps later — hands the same on-disk path to
`apt-get`/`dpkg` as subprocess arguments. Nothing re-reads or re-hashes those
files at the moment apt actually uses them.

**Severity:** Low / architectural. Requires a concurrent, active local
attacker able to modify bundle files in the narrow window between verify
completing and apt reading them — a materially stronger threat than the
"media tampered before it reached the target" case, which signature
verification fully defeats. Not practically fixable inside this architecture
(apt reads by path, not by an already-open file descriptor debark could
hold across the whole operation). Recorded as a residual risk in
`docs/threat-model.md` §6 rather than proposed as a code change.

---

## Informational / positive findings

Worth recording explicitly, because a security review that only lists
problems is as misleading as one that only lists reassurances.

### F8 — Same-media key refusal is implemented correctly

`core/sign/verifier.go`'s `sameMedia`: resolves symlinks, cleans both paths
to absolute form, compares case-insensitively on Windows, and — the detail
that matters — appends the OS path separator before the prefix check
(`strings.HasPrefix(c, b+string(filepath.Separator))`), which is exactly what
stops a sibling directory (`/mnt/bundle-keys` next to `/mnt/bundle`) from
being wrongly treated as "inside" the bundle. Confirmed wired correctly:
`core/verify/verify.go`'s `checkSignatures` calls
`sign.VerifierFor(ctx, opts.Keys, root)` with the real, resolved bundle root,
not a placeholder. No bypass found in this review.

> **Status: still holds, but the implementation quoted above has been
> rewritten.** `sameMedia` now delegates to `pathContains`, which walks the
> candidate path upwards comparing `os.SameFile` against the bundle root
> rather than comparing cleaned strings, falling back to
> `textualPathContains` only when the parent cannot be `Stat`ed. That is
> strictly stronger than the separator-appended prefix check this finding
> praised — it survives symlinks and hardlinked directories that a textual
> comparison would miss — and the sibling-directory case it names
> (`/mnt/bundle-keys` next to `/mnt/bundle`) is still refused. Quote the
> current code, not the snippet above, if you are re-auditing this.

### F9 — Digest checks canonicalise raw bytes from disk, never a re-marshalled struct

`core/manifest/api.go`'s `Load`, and `core/verify/verify.go`'s
`canonicalDigestOfFile` (used for both the lock and the optional snapshot
companion file), both deliberately re-canonicalise the **exact bytes read
from disk**, never `canonical.Marshal(&parsedStruct)`. The code comments in
both places are explicit about why: re-marshalling would silently drop any
field Go's struct doesn't recognise before it ever reaches a digest
comparison, reopening exactly the kind of hole that produces real-world
signature-verification bypasses. Applied consistently everywhere this
pattern is needed in the codebase.

### F10 — Both named path-traversal boundaries are implemented and closed, including a document-driven variant this review specifically checked for and found already handled

`core/snapshot/archive.go`'s `extractTar` validates every tar member name
with `safeArchivePath` before writing. Separately — and this is the part
worth recording, because it was a real, specific attack this review
constructed and then had to disprove by reading further — `verifyExtractedFiles`
and `reapplyRedactions` (also in `archive.go`) read and, in the redaction
case, **write** files at paths taken from `File.ArchivePath` fields in the
*parsed JSON snapshot document*, which is a separate data path from the tar
member names above and is not automatically covered by the tar-extraction
guard. `core/snapshot/validate.go`'s `doValidate` closes this: it validates
every `File.ArchivePath` in the document (including `s.APT.Conf`, via
`Snapshot.Files()`) with the same `safeArchivePath` check, and runs, in
`doOpen`'s sequence, strictly before either of the functions that would
otherwise trust those paths. `core/bundle/tar.go`'s `importTar`/`safeRelPath`
independently implements the equivalent guard for bundle archives, and goes
further than `snapshot`'s tar guard by refusing (hard error) rather than
silently skipping any symlink or hard-link tar entry.

---

## Process observation (not a security finding)

`go.mod` — listed in `docs/dev/contract-brief.md` as a frozen file others
should not edit — grew substantially during the course of this
review: several dependencies were promoted from indirect to direct, and a
full CLI/config dependency stack (`spf13/cobra`, `knadh/koanf` and their
transitive dependencies) was added. Every dependency involved is
functionally consistent with the project's own stated stack and
is licence-clean (`docs/security/dependency-review.md`), so this is flagged
for process awareness, not as a security concern.
