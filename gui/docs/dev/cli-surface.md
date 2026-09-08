# The debark CLI surface, as the GUI must use it

**This document and the real binary outrank `contract-brief.md` where they
disagree.** Everything below was verified against `..` at commit
**`6549f6f`**, and the parts this round changed were re-verified at
**`92ebdc2`** against a binary built from it
(`go build -o debark ./cmd/debark`, `debark version --json` reporting
`commit 92ebdc2…`, `platform windows/amd64`). Where a fact was read out of a
source file the file is named, so the next person can re-check it rather than
trust it.

**Four caveats this document carried are now fixed in the core, and the
sections that recorded them are rewritten rather than deleted** — a consumer
built against the old behaviour needs to know what changed, not just what is
true today. The six core commits between `94612f5` and `6549f6f` changed, in
order of how much they matter to the GUI:

| Was | Now | Core commit |
|---|---|---|
| C9: the container backend was unusable from Windows and macOS | `--self-binary`, `DEBARK_SELF_BINARY`, a `self_binary` config key, and automatic discovery of `bin/debark-linux-<arch>` beside the binary | `a0b61c2`, `6549f6f` |
| C1/C7: `build`, `verify`, `install` and `fetch` failed with an empty stderr | every failure prints, and names what failed | `3ed80bd` |
| C8: `snapshot inspect` on a non-archive exited 4 (`verification`) | structural failures exit 1 (`usage`); content failures still exit 4 | `2d6d5c7` |
| Item 4: no way to force `Install-Recommends` on | `build --recommends`, mutually exclusive with `--no-recommends` | `09d7821` |

Each was re-verified by running the built binary, not by reading the commit;
the transcripts are inline below. §2's `--json` types, §5's event types and
§6's exit-code table were re-checked and are **unchanged**.

**The third round added six more core commits, `6549f6f`..`92ebdc2`, and three
of them change what this document describes.** They are folded into the sections
below rather than listed only here; this table is the index.

| Was | Now | Core commit |
|---|---|---|
| `BuildResult.FetchFailed` was a list of bare URLs, so an unreachable host, a digest mismatch and an untrusted certificate were indistinguishable in the result document | a parallel `fetch_failures[]` carries `{input, reason, detail}` per entry. `fetch_failed[]` is unchanged in shape and contents. See §2.1 | `1c767f7` |
| `verify` on a directory that is not a bundle exited **4** (`verification`) with a report | it exits **1** (`usage`) with **no report at all**, and `--json` prints nothing. A real bundle with its manifest deleted still exits 4. See C10 | `5837d82` |
| `apt.update` carried only `failed` and `entries`, and read as healthy on a machine with no network at all | it also carries `sources_fetched` and `sources_failed`, counted per source rather than per acquire attempt. See §5.1 | `d5f31cb` |
| a vendor URL's password and query token were written verbatim into `evidence.json`, inside the signed bundle | every error leaving `core/fetch`, and the `error` attr of the warn-level `input.external` event, is passed through `fetch.RedactMessage`. **A `detail` string from the core is now safe to render; `fetch_failed` / `FetchFailure.Input` still are not** | `034f709` |
| `--digest` split at the first `=`, so a vendor URL with a query string silently lost its digest | fixed for `fetch`; **still broken for `build`**. See §3 | `92ebdc2` |
| `TestFileURI` failed on Linux, so the core's Linux CI job was red | green; a full `hack/linux-test.sh` passes, 33 packages | `bf51914` |

It is the reference for `internal/cliadapter` and for every
package that renders what the adapter returns.

Read the [Caveats](#caveats) section first. C1–C4 shape the whole adapter and
are all still live. C7, C8 and C9 record behaviour the core has since
**fixed**; they are kept, rewritten, because a consumer written against the
old behaviour has to know what moved, and because in two of the three the
advice outlived the defect.

---

## 1. Global flags

Every command inherits these from the root (`internal/cli/root.go`):

| Flag | Meaning |
|---|---|
| `--json` | print the documented JSON object for this command instead of human text |
| `--json-events PATH` | stream NDJSON evidence events to `PATH`, or `-` for stdout |
| `--no-color` | disable colour (also honours `NO_COLOR` and `TERM=dumb`) |
| `--config PATH` | config file path, overriding XDG discovery |
| `--profile NAME` | named config profile to apply |

**The GUI must always pass at least one of `--json` and `--json-events`.**
Either one suppresses colour, progress rendering and — the reason it is
mandatory — every interactive prompt. A GUI that let `debark` ask a question
on a terminal that is not there would hang forever with nothing on screen.
`cliadapter.BuildArgv` always appends `--json` for exactly this reason.

`--interactive` (on `build` and `snapshot from-base`) must **never** be passed.
It requires a terminal, and it is refused at run time when `--json` is also
given.

---

## 2. Commands, their `--json` type, and whether events flow

Import these types. Never mirror them: a hand-written copy of a published
struct drifts silently, and a typed import cannot.

| Command | `--json` type | Import path | `schema_version` | `--json-events` |
|---|---|---|---|---|
| `snapshot list-bases` | `base.List` (elements `base.ListEntry`) | `core/base` | `debark.baselist/v1` | no-op |
| `snapshot inspect FILE` | `snapshot.Snapshot` | `core/snapshot` | `debark.snapshot/v1` | no-op |
| `snapshot create` | ad-hoc map — see §4 | — | none | no-op |
| `snapshot from-base BASE` | ad-hoc map — see §4 | — | none | **emits** |
| `build [pkg...]` | `buildjob.BuildResult` | `api/buildjob/v1` | `debark.buildjob/v1` | **emits** |
| `verify BUNDLE` | `verify.Report` | `core/verify` | `debark.verifyreport/v1` | no-op |
| `install BUNDLE` | `install.Report` | `core/install` | `debark.installreport/v1` | **emits** |
| `inspect BUNDLE` | CLI-local envelope — see §4 | — | `debark.inspect/v1` | no-op |
| `doctor BUNDLE` \| `--snapshot F` | `doctor.Report` | `core/doctor` | `debark.doctorreport/v1` | no-op |
| `store ls` | `store.Index` | `core/store` | `debark.storeindex/v1` | no-op |
| `store gc [BUNDLE...]` | — | — | — | no-op |
| `version` | `version.Info` | `core/version` | none | no-op |
| `config show` | unimportable — see §4 | — | `debark.config/v1` | no-op |
| `keygen` | ad-hoc map — see §4 | — | none | no-op |

Plumbing commands (`resolve`, `fetch`, `apt-root`, `gen-man-pages`) exist and
`resolve`, `fetch` and `apt-root` do emit events. **The GUI uses none of
them.** They are the container-side and debugging entry points; going through
them instead of `build` would put the GUI on the inside of the engine, which
rule 1 forbids.

Verified: `NewEvidenceSink` is called from exactly `cmd_build.go`,
`cmd_install.go`, `cmd_frombase.go`, `cmd_fetch.go`, `cmd_resolve.go` and
`cmd_aptroot.go`. Every other command ignores `--json-events` entirely — it is
accepted and does nothing.

**Re-verified at `6549f6f`, unchanged.** Every `schema_version` above was read
back off the binary, not off a source file: `snapshot list-bases --json` still
reports `debark.baselist/v1` with fifteen bases for `amd64`,
`snapshot inspect` `debark.snapshot/v1`, `inspect` `debark.inspect/v1`
with `bundle_path`/`signed`/`manifest`/`lock`, `store ls`
`debark.storeindex/v1`, `config show` `debark.config/v1`, and `keygen`
still an unversioned `map[string]string` of `key_id`, `private_key`,
`public_key`. The six evidence-sink call sites are still exactly those six.

### 2.1 `BuildResult` gained a field: `fetch_failures[]`

**New in core `1c767f7`.** `fetch_failed[]` is unchanged — same shape, same
contents, same order — and every consumer written against it keeps working.
Beside it is now:

```go
// api/buildjob/v1
FetchFailures []FetchFailure `json:"fetch_failures,omitempty"`

type FetchFailure struct {
    Input  string             `json:"input"`            // == the fetch_failed entry
    Reason FetchFailureReason `json:"reason"`
    Detail string             `json:"detail,omitempty"`
}
```

The two lists are **one-to-one and in the same order**, and that is a property
of the engine rather than a promise: both are projected from one internal
list in `core/engine`'s `buildResult`. `FetchFailures[i].Input ==
FetchFailed[i]`, always.

`reason` is a closed set of twelve. The GUI's error catalogue rows 3.3, 3.4
and 3.5 were a single row precisely because these could not be told apart:

| `reason` | What happened | What the operator does |
|---|---|---|
| `unreachable` | the host was never contacted — DNS, refused, reset, timeout | network, proxy or the address |
| `tls-untrusted` | reached, and its certificate could not be verified | this machine's trust store — a corporate root, or an intercepting proxy. Retrying cannot help |
| `http-status` | answered with something other than 200 | the URL: wrong, moved, or needs credentials |
| `digest-mismatch` | the bytes arrived and hashed to something else | **the one case where Retry is the wrong instinct** |
| `not-a-deb` | the bytes arrived and are not a Debian package | usually an HTML sign-in page served as 200 |
| `too-large` | over the 8 GiB download limit | |
| `refused-redirect` | redirected to plaintext, or to a scheme debark will not follow | |
| `unreadable` | a **local** `.deb` input could not be read | missing, a directory, or no permission. No network involved |
| `cancelled` | cancelled or timed out mid-download | not a fault in the input |
| `bad-input` | the URL or the supplied digest is malformed | |
| `local-storage` | the builder's own disk or store failed | nothing wrong with the input |
| `other` | unclassified | |

**A consumer must have a default arm.** The reason set is additive: a
thirteenth may appear without a schema bump, and an unrecognised value is to
be treated as `other`. A Go `switch` on `buildjob.FetchFailureReason` with no
`default` is a bug waiting for the next core release.

**`detail` is safe to render; `input` is not.** This asymmetry is deliberate
and documented in the core. `input` is the operator's literal string,
credentials and all, because they typed it and have to recognise it to retry
— it is exactly `fetch_failed`'s value. `detail` can carry a cause the core
did not compose (a `*url.Error` from `net/http` prints the request URL
verbatim), so the core passes it through `fetch.RedactMessage` first. If a
screen shows a vendor URL under a "Copy the command" button, `detail` is the
half that is already redacted and `input` is the half that is not.

**What this does not remove.** The warn-level `input.external` event still
carries the same sentence, and is still the only place a *successful* build's
non-fatal fetch diagnostics appear. What it removes is the need to correlate
two streams by URL to answer "why", which §5.3 below used to require.

One correction while re-checking: `doctor`'s argument is not optional, as an
earlier `doctor [BUNDLE]` in this table implied. Measured — `debark doctor
--json` with no argument exits **1** with `debark: doctor: give exactly one
of BUNDLE or --snapshot FILE`; with a bundle it exits 0 and prints
`{"schema_version":"debark.doctorreport/v1","checked_at":…,"findings":null,
"scanned":3}`. The GUI runs neither form today.

---

## 3. Per-command flags

Only the commands the GUI runs. Verified against `--help` on the built binary.

### `build [pkg...]`

```
--acknowledge-redistribution   suppress the interactive redistribution prompt
--approved-keys string         archive key fingerprints resolution must satisfy
--arch string                  dpkg architecture the --base describes
--backend string               auto, local or container
--base string                  stock release to assume instead of a snapshot
--digest stringToString        expected digest for a URL input (URL=SHA256); repeatable -- SEE THE WARNING BELOW
--embed-binary string          copy a debark binary into the bundle
--image string                 override the container image
--interactive                  NEVER pass this
--list stringArray             packages.txt-style input list; repeatable
--local-dir stringArray        directory of vendor .deb files, scanned; repeatable
--no-prune                     with --update, keep superseded files
--no-recommends                override the target's Install-Recommends and exclude recommended packages
--no-sign                      write an unsigned bundle explicitly
--out string                   write the bundle as a directory (default ./bundle)
--policy string                local policy file evaluated against the plan
--recommends                   override the target's Install-Recommends and include them (default: follow the target)
--sbom                         write sbom.cdx.json (CycloneDX)
--self-binary string           static linux/<arch> debark for the container backend to mount
--sign string                  signing key: a file, gpg:<keyid>, or plugin:<name>
--snapshot string              snapshot of the real target
--tar string                   write the bundle as a <name>.debark.tar.zst archive
--update                       refresh indexes, re-resolve, fetch newer, prune superseded
--upgrades                     add a full-upgrade pass for packages already installed
```

Constraints cobra enforces before `RunE` runs:

- `--snapshot` and `--base` are **mutually exclusive**;
- exactly one of `--snapshot`, `--base`, `--interactive` is **required**;
- `--out` and `--tar` are mutually exclusive;
- `--sign` and `--no-sign` are mutually exclusive;
- **`--recommends` and `--no-recommends` are mutually exclusive** (new).

Measured, since the mutual-exclusion message is what the GUI would have to
render if it let both through:

```
$ debark build --base debian:12/minimal --recommends --no-recommends --json -- apt:jq
exit 1, stderr:
debark: if any flags in the group [recommends no-recommends] are set none of
the others can be; [no-recommends recommends] were all set
(followed by the whole usage block — see C1)
```

**`--recommends` completes a three-state control.** `Options.Recommends` is a
`*bool`, and until `09d7821` only two of its three states were reachable from
a command line. The GUI can now offer *follow the target* (neither flag),
*include* (`--recommends`) and *exclude* (`--no-recommends`), which is what
item 4 below used to say was impossible. `cliadapter.BuildSpec` needs a
three-valued field to carry it — a `*bool` or an enum, not a `bool`, because
a `bool` cannot distinguish "follow the target" from "exclude".

**`--self-binary` is the flag that makes `build --backend container` work off
Linux at all.** Its full `--help` text is the definitive statement of the
search order:

```
--self-binary string   static linux/<arch> debark for the container backend to mount
                       (default: bin/debark-linux-<arch> beside this binary, else this
                       binary; also DEBARK_SELF_BINARY or the self_binary config key)
```

See C9 for the whole story and for the measured transcripts. The GUI does not
normally need to pass it: shipping `bin/debark-linux-<arch>` beside the
`debark` it invokes is the arrangement that needs no flag on any command,
and `internal/readiness`'s `self-binary` check reports whether it is there.

**`build --digest` misbinds a URL that has a query string, and this is not
fixed.** `build` registers the flag as pflag `stringToString`, which splits
each pair at the **first** `=`. A vendor URL's query string has its own `=`
long before the separator, so:

```
--digest 'https://vendor.example/a.deb?ver=1=<sha>'
  -> {"https://vendor.example/a.deb?ver": "1=<sha>"}
--digest 'https://vendor.example/a.deb?ids=1,2=<sha>'
  -> {"https://vendor.example/a.deb?ids": "1", "2": "<sha>"}     (pflag CSV-splits on the comma)
--digest 'https://vendor.example/a.deb?q="x"=<sha>'
  -> error: parse error on line 1, column 35: bare " in non-quoted-field
```

Three of those are **silent**. In every one the digest never binds to the URL
the operator typed, so the download proceeds as `url-unverified` when they
asked for `user-digest` — the strongest provenance claim debark makes about
a file. Core `92ebdc2` fixed this for `debark fetch` (which now splits at
the last `=` and validates the digest at parse time) and left `build` alone,
because `build`'s flag registration is in a file with unrelated uncommitted
work in it.

**Until it lands, `BuildSpec` must not put a query string and a digest into
one `--digest` argument.** A URL with no `=` in it is unaffected. If the GUI
lets an operator supply a digest for a URL that contains `=`, the safe thing
today is to refuse it in `BuildSpec.Validate` with a message saying why,
rather than pass it and have the provenance quietly downgrade.

And one the command enforces itself: `--arch` describes a `--base`, and is
rejected alongside `--snapshot`, because a snapshot already records the
target's own architecture.

`cliadapter.BuildSpec.Validate` enforces all of the above locally so the UI can
say what is wrong without shelling out and rendering a help screen.

**Positional arguments.** `build` takes package names, `https://` URLs and
local `.deb` paths as positionals, classified by shape
(`classifyBuildArg`, `cmd_build.go:310`): `https://`/`http://` is a URL, a
`.deb` suffix is a file, anything else is a package name. The explicit
prefixes `apt:`, `url:` and `file:` override the shape test, and
`cliadapter.BuildArgv` always uses them, behind a `--` separator. Being
explicit is what makes a `BuildSpec` round-trip exactly.

**Two engine behaviours the flags do not show.** `--update` implies
`--upgrades` (`Upgrades: upgrades || update`), and `--no-prune` only means
anything with `--update`.

### `snapshot list-bases`

```
--arch string   dpkg architecture to render the bases for (default: this machine's)
```

The returned `List.Arch` says which architecture the listing was actually
rendered for. Read it rather than assuming — this is how `Probe.HostArch` is
learned, and the GUI's own `runtime.GOARCH` is a fact about the GUI, not about
what `debark` would default to.

The builtin table at the verified commit is fifteen rows: Debian 12 and 13 and
Ubuntu 22.04, 24.04 and 26.04, each in `minimal`, `server` and `desktop`.
`internal/cliadapter/fake` returns exactly this by calling `base.Builtin` and
`base.NewList`, so it cannot drift.

### `snapshot inspect FILE`

No flags of its own.

### `verify BUNDLE`

```
--allow-unsigned        accept a bundle with no valid signature
--gpg-keyring string    gpg keyring FILE holding the expected release key(s)
--key stringArray       operator public key file; repeatable
--keyring stringArray   directory of trusted public keys; repeatable
```

### `keygen`

```
--comment string   operator comment recorded in the key
--out string       path to write the private key to (REQUIRED)
```

`--out` is marked required; cobra refuses the command without it. The public
key is written alongside, with the `.key` suffix replaced by `.pub` (or `.pub`
appended when there was no `.key` suffix) — `core/sign`'s
`PrivateKeyFileSuffix`/`PublicKeyFileSuffix`, which the adapter imports rather
than hard-codes.

### `version`

No flags of its own.

---

## 4. Commands with no importable Go type

Five, not four. Each needs a locally declared struct in the consuming package,
and each such struct must carry a comment saying it is a local mirror of an
ad-hoc CLI map and why.

| Command | Shape |
|---|---|
| `keygen --json` | `map[string]string` with keys `key_id`, `private_key`, `public_key`. No `schema_version`. Mirrored as `cliadapter.KeyInfo`. |
| `snapshot create --json` | `path`, `digest` (**bare hex, no `sha256:` prefix**), `target` (`core/snapshot`.`Target` — importable). No `schema_version`. |
| `snapshot from-base BASE --json` | `path`, `digest`, `backend`, `base` (`core/base`.`Definition`), `origin` (`core/snapshot`.`Origin`), `target` (`core/snapshot`.`Target`). No top-level `schema_version`; the nested payloads are all importable. |
| `inspect BUNDLE --json` | A CLI-local envelope, `schema_version` `debark.inspect/v1`, fields `bundle_path`, `signed`, `manifest` (`core/manifest`.`Manifest`), `lock` (`core/lock`.`Lock`). The nested payloads **are** importable. |
| `config show --json` | `schema_version` `debark.config/v1` plus `path`, `loaded`, `profile`, `profile_names` and the settings. Its type lives in `internal/cli/config`, which Go's `internal/` rule makes **unimportable from this module at all**. |

The GUI currently runs none of `snapshot create`, `snapshot from-base`,
`inspect` or `config show`, so only `KeyInfo` exists today. Add the others
when they are needed, in `internal/cliadapter`, and say in the comment that
they are mirrors.

---

## 5. The event stream

One NDJSON line is one `core/evidence`.`Event`. **Import it.**

```go
type Event struct {
    Schema string         `json:"schema"`   // always "debark.events/v1"
    TS     string         `json:"ts"`       // RFC 3339 UTC, truncated to the second
    Type   string         `json:"type"`
    Level  string         `json:"level,omitempty"`  // info | warn | error; absent means info
    Msg    string         `json:"msg,omitempty"`    // a short human sentence
    Attrs  map[string]any `json:"attrs,omitempty"`  // machines read this, not Msg
}
```

`cliadapter.Event` is an alias for it.

### 5.1 Event types

`attrs` were read from the emitting call sites, not from documentation.

| `type` | Emitted by | `attrs` |
|---|---|---|
| `snapshot.loaded` | engine | `snapshot_digest`, `distro_id`, `version_id`, `codename`, `arch` |
| `backend.selected` | engine, base synthesis | `backend`, `runtime`, `image`, `image_digest`, `platform` — **and on the local backend a first, separate one carrying `backend` plus `reason`, then a second carrying `apt_version` and `dpkg_version` and no `platform`** (observed) |
| `apt.update` | engine | `failed` (bool), `entries` (int), and **new in core `d5f31cb`** `sources_fetched` (int) and `sources_failed` (int) — **`entries` was absent on the local backend** (observed). See the note below the table |
| `apt.resolve` | engine | `selections`, `unresolved` — or `seeds`, `packages` on the base path |
| `fetch.file` | `core/fetch` | `url`, `filename`, `digest`, `size`, `verification` |
| `input.external` | engine | `names` (the external package names). A **warn**-level variant carries `error` plus `path` for a local `.deb` that could not be read, or **`url` for a vendor download that failed** — observed, and see the note under §5.3. |
| `store.hit` | engine | `name`, `version`, `arch`, `sha256`, `size`. **Not only a cache hit:** its `msg` is `ingested <file>.deb`, and on the local backend it is emitted for a package apt had just downloaded into its own root. See §5.3. |
| `policy.finding` | engine | `rule`, `severity`, `packages` |
| `doctor.finding` | engine | `check`, `severity`, `package`, `flag` |
| `repo.indexed` | engine | `package_count`, `pool_bytes` |
| `closed_world.checked` | engine | `backend`, `image`, `image_digest`, `network` — or `result` alone, which was observed on a **passing** local-backend check (`result: ok`), not only a skipped one |
| `manifest.signed` | engine | `signer_kind`, `key_id` |
| `bundle.assembled` | engine | `package_count`, `added`, `removed`, `unreferenced` |
| `bundle.pruned` | engine | `count` |
| `install.plan` | install | `bundle_id`, `bundle_path`, `to_install`, `to_upgrade`, `to_remove`, `already_current`, `ok` (counts, not lists) |
| `install.result` | install | `bundle_id`, `bundle_path`, `ok`, `dpkg_fallback` |
| `warning` | engine, backends | `code`, plus whatever the warning is about |
| `progress` | `core/fetch` | **two shapes — see below** |

**Declared in `core/evidence` but never emitted anywhere:**
`snapshot.created`, `verify.result`, `build.started`, `build.finished`.

**`apt.update`'s `failed` is apt's verdict, and apt exits 0 with no network
at all.** Measured: `docker run --rm --network none ubuntu:24.04 apt-get
update` prints four `Err:` lines, no `E:` line, and exits 0 — it falls back
to whatever indexes were already on disk, which in a fresh container is
nothing. The build then fails at exit 5 naming the base definition's own seed
packages, which the operator never chose. `entries` could not carry the
signal either: it counts acquire *attempts*, and `Acquire::Retries` inflates
it, so four unreachable sources produced sixteen entries and a larger number
looked like more work done.

So **`sources_fetched == 0 && sources_failed > 0` is the predicate for "this
machine has no route to the archive"**, and it is the one to branch on — not
`failed`, and not `entries`. Alongside it the build's `Warnings` now carry
`apt-update.no-index-fetched`, a sentence that says the archive was
unreachable rather than the package missing; that is the string to render.
A partial failure (one mirror down of three) deliberately does **not** raise
it and stays as the per-source `apt-update.fetch-error` warning.

Both attributes are **stripped from the bundle's `evidence.json`** exactly as
`entries` is, because they are facts about this machine's network and
`evidence.json` is hashed by the manifest the signature covers. They are on
the live `--json-events` stream, which is where the GUI reads them, and
nowhere else. §5.4 is the general form of this.
Accept them (they are in the frozen list and a future release may start
emitting them); never wait for one. In particular, **there is no
"build finished" event** — a build ends when the process exits, and
`bundle.assembled` is the last thing a successful build emits.

### 5.2 Progress — exactly two shapes, and no cumulative counter

Both are `type: "progress"`, both from `core/fetch/fetcher.go`.

**Byte progress** — `attrs{url, bytes, total_bytes}`, `msg` empty.

```json
{"schema":"debark.events/v1","ts":"2026-09-06T10:15:11Z","type":"progress",
 "attrs":{"url":"http://archive.ubuntu.com/…/nginx-core_1.24.0-2ubuntu7.3_amd64.deb",
          "bytes":593684,"total_bytes":1484212}}
```

- Throttled to **once per 250 ms per file**, plus one final emit at the end.
  A UI that assumes a faster tick will look stalled against a real build.
- **`total_bytes` is `-1` when the server sent no `Content-Length`.** Multiply
  by it and you get a negative bar width. `cliadapter.ProgressAttrs.Fraction`
  returns `-1` for this case; render an indeterminate bar.
- `bytes` is **this file's** progress, not the run's. Several files download
  concurrently, so their events interleave. `url` is the only correlation key.
- A retry **restarts** the byte count for that file. A UI that only ever moves
  a bar forward will glitch.

**Retry notice** — `attrs{url, attempt, attempts}`, with
`msg = "retrying <url> (attempt N/M)"`. `attempt` and `attempts` are 1-based;
the default is 3 attempts with exponential backoff.

**There is no cumulative counter on the wire.** Nothing says "17 of 240 files"
or "412 MB of 1.1 GB". A consumer that wants those must accumulate them
itself:

- **files done** = the number of `fetch.file` events seen;
- **bytes done** = the sum of `attrs.size` over those events;
- **files total** is not knowable in advance from the stream at all. The
  closest thing is `apt.resolve`'s `selections`, which arrives before any
  download starts and counts *selections*, not *files still to fetch* — a
  store hit consumes a selection and downloads nothing. Treat it as an upper
  bound, or show a count-up rather than a fraction.

This paragraph is the one the build screen's progress UI must read.
`cliadapter.FetchFileOf` and `cliadapter.ProgressOf` do the decoding; the
accumulation is the caller's.

### 5.3 On the local backend there is no download phase on the wire at all

Everything in §5.2 assumes `fetch.file` and `progress` events exist. On the
**local** apt backend — which is the default on the shipping platform, a Linux
builder whose own release matches the target — they do not.

Measured. `hack/demo-gui-e2e.sh` ran a complete, successful, signed
`build --base debian:12/minimal -- apt:jq` inside `debian:bookworm-slim`. Three
packages, 387,468 bytes downloaded from the Debian archive. The whole
`--json-events` stream was fifteen events, and **not one of them was
`fetch.file` or `progress`**:

```
snapshot.loaded  backend.selected x3  apt.update x2  apt.resolve x2
store.hit x3     bundle.assembled     repo.indexed
closed_world.checked                  manifest.signed
```

The reason is architectural rather than a gap in the stream: the local backend
runs apt in its own root, so apt does the downloading and `core/fetch` — the
only emitter of `fetch.file` and `progress` — never sees the files. What the
engine emits instead is one `store.hit` per package, with `msg` `ingested
<file>.deb`, as it moves what apt fetched into the store.

Two consequences the build screen has to live with:

- **The progress bar has nothing to show during the download phase** on the
  path most operators are on. The counters in §5.2 (`files done` from
  `fetch.file`, `bytes done` from its `size`) stay at zero for the whole
  download and then jump. Counting `store.hit` gives the same total after the
  fact and is no more of a denominator.
- `store.hit` cannot be rendered as "already had this one, nothing was
  downloaded". It means "this file is in the store now", which on the local
  backend is exactly the file that was just fetched.

`fetch.file` and `progress` are real and do arrive — for `https://` vendor
inputs, which `core/fetch` always handles, and on the container backend. The
stream simply does not describe apt's own downloading, on any backend.

**And a warn-level `input.external` is the only place the reason a vendor
download failed exists.** Measured, with the same build against a
`debian:bookworm-slim` that has no CA certificates:

```
exit 3, stdout: the whole debark.buildjob/v1 result, stderr: 0 bytes
{"type":"input.external","level":"warn",
 "msg":"download failed: https://…/hello_2.10-3_amd64.deb",
 "attrs":{"url":"https://…/hello_2.10-3_amd64.deb",
          "error":"fetch: …: tls: failed to verify certificate: x509: certificate signed by unknown authority"}}
```

**This paragraph is now superseded twice over, and the history matters because
a consumer written against either earlier state has to know what moved.**

`3ed80bd` meant stderr was no longer 0 bytes: a build that fails this way
writes `build: <bundle>: incomplete: 1 URL that did not download:
https://…/hello_2.10-3_amd64.deb`. That named the URL, not the reason.

`1c767f7` closes the rest of it. `BuildResult.FetchFailures` now carries
`{input, reason, detail}` per failed input, so the difference between an
unreachable host, an untrusted certificate and a wrong SHA-256 is a value in
the result document, and **a screen no longer has to reach into the event log
and correlate by URL to answer "why"**. See §2.1 for the reason set and for
why `detail` is safe to render while `input` is not.

Two things this does not change. The warn-level `input.external` event still
exists and still carries the same sentence — it is the only place a
*successful* build's non-fatal fetch diagnostics appear, since a build that
recovered has nothing in `fetch_failures`. And the **stderr** line still names
the URL rather than the reason, so a screen that reads only stderr is no
better off than before; the result document is the richer source and always
was.

`034f709` is the other half of the same area: until it landed, that event's
`error` attr contained the vendor URL verbatim, credentials and query token
included, and was written into `evidence.json` inside the signed bundle. It is
redacted now. **The practical consequence for the GUI is that a `detail` or an
`input.external` `error` attr coming from core ≥ `034f709` is safe to put on
screen and in a copyable command; `fetch_failed[]` and `FetchFailure.Input`
still are not, deliberately.**

This strengthens the core-repo suggestion in item 7 below rather than replacing
it: a `fetch.plan` event would give the bar a denominator, and something from
the local backend saying "apt is downloading N packages" would give it a
numerator.

### 5.4 The event stream is not a subset of the bundle's evidence.json

Same run: fifteen events on the wire, **thirteen** in the finished bundle's
`evidence.json`. The extra two (plus one extra `backend.selected`) are the
base-synthesis phase — `build --base` turns the base into a throwaway snapshot
first, and that work is not part of what the bundle attests to.

So a details drawer fed by `--json-events` holds events the bundle does not.
That is the right way round, but anyone diffing the two should expect it.

**Core `5d26dac` widens that gap deliberately, and it is now a rule
rather than an accident: `progress` events never reach `evidence.json` at
all.** They are emitted on a 250 ms wall clock, so how many a download
produces is a fact about the network that afternoon. Measured with the real
fetcher against one server serving one fixture `.deb` at two pacings — same
bytes, same digest, same result — the bundle's copy received **2 events on
the fast run and 9 on the slow one**, which meant two different bundle ids
for byte-identical output. Any build with a vendor URL was unreproducible,
and the project's own determinism checks could not see it because the demo
build resolves `apt:jq` and downloads no vendor URL.

**Nothing changes for the GUI**, and that is the point: `--json-events` is
the other arm of the fan-out and still carries every progress event with its
exact `bytes`. The progress bar reads the live stream, which is where §5.2's
two shapes live. Only a consumer diffing the wire against the bundle needs to
know, and it now has one more expected difference.

---

## 6. Exit codes

`core/dferr`, frozen by ADR-012. **The numeric value IS the process exit
code**, and the strings are stable — automation branches on them.

| Code | Name | Description (verbatim from `Class.Description()`) |
|---|---|---|
| 0 | `success` | success; bundle matches the request |
| 1 | `usage` | usage or configuration error |
| 2 | `environment` | environment: no apt, no container runtime, no disk, no permission |
| 3 | `incomplete` | bundle built but incomplete: a URL failed or an external .deb has unsatisfiable dependencies |
| 4 | `verification` | verification failed: signature, digest or repository metadata mismatch |
| 5 | `resolution` | resolution failed: apt could not satisfy the request |
| 6 | `policy` | policy violation (local policy or approved-keys) |
| 7 | `target-mismatch` | target mismatch on install: architecture or release |

**Re-verified at `6549f6f` against `core/dferr`: every value and every
description string above is unchanged**, and no core commit in this round
altered one. `2d6d5c7` moved three *cases* between classes (C8) without
changing what a class means, which is the distinction that matters to a
consumer branching on the number.

Exit 1 is also the fallback for any error that carries no class at all
(`dferr.ClassOf`).

**An exit code outside 0–7 is not exit 1.** A shell that could not find the
binary returns 127; a process killed by a signal reports −1 through
`os/exec`; Windows returns exception statuses. `cliadapter.NewError` maps all
of those to `dferr.Environment`, deliberately departing from `dferr.ClassOf`'s
own fallback: a process that died in a way debark did not choose is a fact
about the machine, not about the operator's flags. The raw number is preserved
in `ExitCode()` and appears in the summary.

---

## Caveats

### C1. Errors are never JSON — but stderr is no longer empty.

`--json` does not change failure output. `dferr.Error` has **no JSON tags** and
is never serialised. A failure goes to **stderr**, as plain text
(`internal/cli/root.go`, `printErr`):

```
debark: <message>
<hint>              ← only when the error carried one
```

followed, for usage-class (exit 1) errors only, by cobra's **entire usage
block** — flags, global flags, the lot. Measured: a bare `debark build
--json` produces about 2 KB of help text on stderr and nothing at all on
stdout.

Consequences the adapter is built around:

- An `*cliadapter.Error` is constructed from exactly three things: **argv,
  exit code, captured stderr**. There is nothing else.
- The summary is the first stderr line with `debark: ` stripped, taken from
  **before** the usage block. Lines after it and before the block are the
  hint. The usage block itself goes in the details drawer, never the summary.
- Captured stderr is capped at `cliadapter.MaxStderrBytes` (64 KiB) and
  truncation is marked.
- **`Error` has no exported exit-code field.** The number is reachable only
  through `ExitCode()`, documented as for logs and tests. `Summary()` is never
  empty — it falls back to the class's own frozen description. This is how the
  "no error path may surface a raw exit code alone" item is enforced
  structurally rather than by review.

**This used to have a large exception and no longer does.** Until `3ed80bd`,
`build`, `verify`, `install` and `fetch` printed their `--json` document to
stdout and then returned a `silentError`, which `Execute` deliberately did not
prefix with `debark: `. A failing build wrote **zero bytes to stderr**; so
did `verify` on exit 4. `silentError` is gone, and each of the four now builds
a line that names the thing that failed.

Measured on the current binary, one run each:

```
$ debark build --base debian:12/minimal --backend local --out /tmp/bundle       --no-sign --json -- apt:jq url:https://a.invalid/x.deb url:https://b.invalid/y.deb
exit 3, stdout 669 bytes (the whole buildjob/v1 result), stderr 263 bytes:
debark: build: /tmp/bundle: incomplete: 2 URLs that did not download:
https://a.invalid/x.deb, https://b.invalid/y.deb
the bundle holds everything that did resolve; supply the missing input as a
local .deb (--local-dir) or drop it from the request, then re-run

$ debark verify ./bundle --json
exit 4, stdout 904 bytes, stderr 312 bytes:
debark: verify: /…/bundle failed verification: 1 problem:
debark.manifest.sig: no signature verifies against a trusted key
the operator public key must reach this machine independently of the bundle's
media: pass it with --key, or list it under verify_keys in the config file

$ debark install /bundle --json
exit 4, stdout 0 bytes, stderr 80 bytes:
debark: install: bundle failed verification
run 'debark verify' for details

$ debark fetch --store … https://a.invalid/pkg.deb https://b.invalid/other.deb --json
exit 3, stdout 1367 bytes, stderr 94 bytes:
debark: fetch: 2 of 2 URL(s) failed: https://a.invalid/pkg.deb, https://b.invalid/other.deb
```

**The `build` failure line has a documented shape**, and the adapter may
depend on it only for display — never parse it, the result document is the
machine channel:

```
build: <bundle path>: <exit class>[: N input(s) apt could not satisfy: …][; N URL(s) that did not download: …]
```

Both lists elide after **three** names through one shared helper
(`internal/cli/errors.go`, `nameSome`, `maxNamedItems = 3`), so every command
elides the same way. The elision reads "**and N more**", not an ellipsis, and
the noun is pluralised. Measured, with five failing URLs and then with two
unsatisfiable local `.deb` inputs plus one failing URL:

```
debark: build: /tmp/b1: incomplete: 5 URLs that did not download:
https://a1.invalid/1.deb, https://a2.invalid/2.deb, https://a3.invalid/3.deb and 2 more

debark: build: /tmp/b2: incomplete: 2 inputs apt could not satisfy:
acme-agent, acme-tools; 1 URL that did not download: https://z.invalid/z.deb
```

A closed-world failure has neither an unresolved input nor a failed URL, so
`build` falls back to the warning sentence carrying its explanation
(`firstWarningAbout(r.Warnings, "closed-world")`), and the `: …` tail is
absent entirely.

**Two rules still follow, unchanged:**

1. `Adapter.Build` returns a **non-nil result and a non-nil error** for an
   incomplete build. Callers must read both. The full lists live in the
   result's `Unresolved` and `FetchFailed`; stderr carries at most three of
   each.
2. `internal/cliadapter/invoke.go` should call `(*Error).WithSummary` with
   something drawn from the result whenever it has one. The generic class
   description is now rarely what is reached, but the result is still the
   richer source and is the one that is not elided.

### C2. `--json-events -` shares stdout with `--json`.

With `--json-events -` the NDJSON lines and the final result object go to the
**same stream**, events first (`Ctx.NewEvidenceSink`, `context.go:181`). The
result object is `json.MarshalIndent`ed and therefore multi-line, so
line-by-line parsing mangles it.

**Decision: the adapter never passes `-`.** It reserves a temporary file path,
passes `--json-events <path>`, tails the file while the process runs, drains
it after exit, and removes it. stdout stays a clean, plain `json.Decoder`
input.

Rejected alternatives, and why:

- **Framing heuristic on a shared stdout.** Event lines are compact and the
  result is indented, so they *can* be told apart — but only by a rule about
  JSON formatting, in the one place where being wrong means the operator's
  build result silently fails to parse.
- **OS pipe on an extra file descriptor** (`cmd.ExtraFiles` plus `/dev/fd/3`).
  The cleanest thing on Linux, and it does not exist on Windows: `ExtraFiles`
  is documented Unix-only and there is no `/dev/fd`. This repository is
  developed on Windows and Wails builds for it.
- **FIFO (`mkfifo`).** Unix-only for the same reason, and worse: opening one
  for reading blocks until a writer appears, so a `debark` that fails before
  it opens the stream deadlocks the reader.

The temp file costs a polling tail — a few page-cache reads a second, invisible
next to a build that downloads packages — and buys a clean stdout, one
mechanism on both platforms, and a file that survives a crash, which is when
its contents matter most. `--json-events` is opened with `os.Create`
(`context.go:167`), so a reserved-but-unwritten path is exactly what it
expects.

**The interface does not leak this.** No `Adapter` method takes or returns a
path, and `Command(spec)` does not include `--json-events` — that path is an
implementation detail and would be meaningless to an operator pasting the
command. `Command` *does* include `--json`, because the GUI really runs with
it and showing a command that would behave differently is a lie.

### C3. Progress is thinner than you would hope.

See §5.2. In one line: two shapes, `total_bytes` can be `-1`, a retry rewinds
the counter, and **there is no cumulative counter — accumulate it yourself
from `fetch.file`.**

### C4. Five commands have no importable Go type.

See §4. `keygen`, `snapshot create` and `snapshot from-base` emit ad-hoc maps;
`inspect` has a CLI-local envelope (its nested payloads are importable);
`config show` has a real versioned type that lives under `internal/` and is
therefore unreachable from this module. Declare local structs, and say in a
comment that they are mirrors of an ad-hoc CLI shape.

### C5. Four event types are declared and never emitted.

`snapshot.created`, `verify.result`, `build.started`, `build.finished`. Accept
them; never wait for one. Notably there is no build-started or build-finished
event: the build's lifetime is the process's lifetime.

### C6. An unknown subcommand of a group exits 0 and prints help to stdout.

**Still true at `6549f6f`, and re-measured**: `debark snapshot bogus` exits
**0**, writes 1,374 bytes of the snapshot group's help text to **stdout**, and
writes **0 bytes to stderr**. Cobra runs the parent command, which has no
`RunE`, so it prints usage and succeeds.

This one is worth re-measuring every time, because `3ed80bd` made three other
commands start writing to stderr and it would be easy to assume this changed
with them. It did not: nothing failed, so nothing printed an error.

**Success is therefore no evidence that a subcommand exists.** This is the
single most important fact for capability detection, and it is why the adapter
decides `Bases` from whether `snapshot list-bases --json` actually returned a
`debark.baselist/v1` document rather than from its exit status. The stale
binary problem is worse than "the command fails": a stale binary returns prose
with a success status.

The same trap applies to parsing `--help` for flags. `build --help`'s prose
mentions `--snapshot`, `--base` and `--list/--local-dir` in sentences, so a
naive substring search finds flags that may not exist. Scope any help parsing
to cobra's `Flags:` and `Available Commands:` blocks.

### C7. FIXED — `verify` no longer fails silently either, but decode the document anyway.

This caveat recorded that an untrusted bundle made `verify` print a complete
`debark.verifyreport/v1` document to stdout, exit **4**, and write **nothing**
to stderr. `3ed80bd` fixed it along with `build`, `install` and `fetch`; the
measured transcript is in C1.

**The advice it gave survives the fix, and is the reason this section is
rewritten rather than deleted.** stderr now names *one* problem and elides
after three; the report names all of them, with per-file detail. So the rule
stands: **any command that prints a `--json` document is worth decoding on the
failure path**, because the document is complete and the stderr line is a
summary of it. The adapter does this for both `build` and `verify`.

What has changed is the failure mode when the adapter does *not*. Before, a
consumer that read only stderr rendered dferr's generic class description and
told the operator nothing. Now it renders a true, specific sentence that is
merely incomplete. That is a much better floor, and it is the reason the
"no error path may surface a raw exit code alone" item is now structurally
satisfied by the CLI as well as by `cliadapter.Error.Summary()`.

### C8. FIXED — `snapshot inspect` now splits "wrong file" (exit 1) from "bad content" (exit 4).

This caveat recorded that `snapshot inspect` on a bare `snapshot.json` exited
**4 (`verification`)** — the class that means "signature, digest or repository
metadata mismatch" — because everything past `os.Stat` in `snapshot.Open` was
classified that way. A file picker makes choosing the wrong file *likely*, and
the honest answer ("that is not a snapshot archive") rendered as an alarming
claim about tampering.

`2d6d5c7` drew the boundary. **It is structural, not a general softening**,
and the two families now mean genuinely different things to a consumer — so
they are recorded separately. Every line below was measured against the built
binary, using `.demo/target.snapshot.tar.zst` from the core repository as the
known-good archive.

**Family 1 — "this is not a snapshot": exit 1, `usage`.** Three checks, and
only three. Each answers "there is no snapshot here at all", which is an
ordinary command-line mistake.

| Input | Exit | stderr (first line) |
|---|---|---|
| a text file | **1** | `debark: snapshot: plain.txt is not a debark snapshot: it does not begin with a zstd frame header` |
| a directory with no `snapshot.json` | **1** | `debark: snapshot: emptydir is not a debark snapshot: it is a directory with no snapshot.json in it` |
| a valid zstd tar with no `snapshot.json` member | **1** | `debark: snapshot: this is not a debark snapshot: the archive contains no snapshot.json` |

All three carry the same hint, which is the sentence that makes the row
actionable:

```
`debark snapshot create` on the target machine writes one;
`debark snapshot from-base` synthesizes one from a stock release
```

**Family 2 — "this is a damaged or tampered snapshot": exit 4,
`verification`.** These are content checks: something *is* a snapshot and its
contents do not hold together. Exit 4 keeps its ADR-012 meaning, and a
consumer that escalates on it is still right to.

| Input | Exit | stderr (first line) |
|---|---|---|
| archive truncated to 2,000 bytes | **4** | `debark: snapshot: decompress trunc.tar.zst: snapshot: zstd decode: unexpected EOF` |
| one captured file altered, document unchanged | **4** | `debark: snapshot: /etc/apt/apt.conf.d/01autoremove: digest mismatch: document says 35b4360d…, archive has b200975b…` |
| a `files/../../escape.txt` member | **4** | `debark: snapshot: archive entry "files/../../escape.txt" escapes the extraction directory` |
| a file the document names, missing from the archive | **4** | `debark: snapshot: /var/lib/dpkg/status: missing from archive: …` |

**The line the GUI must draw is the one above, not the one it drew before.**
The old workaround — recognising "exit 4 from `snapshot inspect`" as "probably
the wrong file" — is now actively wrong: exit 4 from this command now means
what it says. Exit 1 is the file-picker mistake, and its sentence and hint are
already the right thing to put on screen.

**One thing the split does not cover, and it is deliberate.** A member whose
name does not begin with `files/` and is not `snapshot.json` is *ignored*, not
refused: an archive carrying `../escape.txt` alongside a valid document exits
**0** and inspects normally, measured. That is the safer design rather than an
oversight — the extractor only ever writes members under `files/`, so an
unknown member gains a crafted archive nothing — and it is recorded here so
nobody reads exit 0 as "this archive contains only what it should".

### C9. FIXED — the container backend now works from Windows and macOS, given a Linux debark.

This caveat recorded that `build --backend container` was unusable off Linux:
the backend mounts a debark binary into the container and re-execs it
(`core/apt/container.go`, `containerSelfPath`), the CLI always set
`ContainerOptions.SelfPath` to `os.Executable()`, and on Windows that is a PE
binary. There was no flag, no config key and no environment variable pointing
it anywhere else, so the fix the hint asked for was not reachable from a
command line — the hint named a Go struct field.

`a0b61c2` added all three routes plus discovery. **Verified by running the
built binary on this Windows host against Docker Desktop with a `linux/amd64`
server.**

**It works.** With `bin/debark-linux-amd64` beside `debark.exe` and no
flag at all, a container build reached apt inside a Debian 12 container and
resolved:

```
$ debark build --base debian:12/minimal --arch amd64 --backend container \
      --out ...\b1 --no-sign --json -- apt:this-package-does-not-exist-zz
exit 5, stderr:
debark: container: C:\Program Files\Docker\...\docker.exe exited 5:
debark: apt: could not satisfy requested packages:
this-package-does-not-exist-zz (unable to locate package)
```

That is apt's own answer from inside the container. The ELF error is gone.

**The search order**, from `containerSelfPath`'s own doc comment and confirmed
by driving each route:

1. An explicit path — `--self-binary`, `DEBARK_SELF_BINARY`, or the
   `self_binary` config key. **An explicit answer always wins**, and the
   siblings are not consulted, so a wrong `--self-binary` is not rescued by a
   correct sibling. Both the flag and the environment variable were driven at
   the `.exe` with a good sibling present, and both produced:

   ```
   exit 2, stderr:
   debark: container: .../debark.exe does not look like a Linux ELF binary
   (bad magic number '[77 90 144 0]' in record at byte 0x0)
   build a static linux/amd64 debark and point --self-binary at it:
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/debark-linux-amd64 ./cmd/debark
   (bin/debark-linux-amd64 beside the debark you are running is found
   without any flag; DEBARK_SELF_BINARY and the self_binary config key also work)
   ```

   Note the class: **exit 2, `environment`** — not `usage`. A machine that
   cannot run the build is a fact about the machine, not about the flags.

2. `bin/debark-linux-<arch>`, then `debark-linux-<arch>`, **beside the
   running debark binary** — `filepath.Dir(os.Executable())` inside
   debark's own process, not beside whatever invoked it. `bin/` is the same
   convention `--embed-binary` uses. The file is only stat'd at this step;
   whether it is the right kind of binary is decided later, deliberately, so
   a wrongly-built sibling gets the specific ELF diagnosis above rather than
   being skipped in silence.

3. `os.Executable()` itself, which is a correct answer only on Linux.

`self_binary` is a real key in the config document, and `config show --json`
prints it:

```
$ debark config show --config cfg.yaml --json
{ "schema_version": "debark.config/v1", ..., "self_binary": "C:/.../debark.exe" }
```

**What this means for the GUI.** Shipping `bin/debark-linux-<arch>` beside
the `debark` the application invokes is the arrangement that needs no flag
on any command, and it is exactly what `internal/readiness`'s **`self-binary`**
check looks for. Before that check existed, readiness reported `can_build:
true` with docker present and every build then failed; it now reports a
degraded row naming the two paths it searched, and `DeriveBuildEnvironment`
stops counting a container runtime as a way to build while that row is a
problem.

**One new defect was found while verifying this — see item 10 below.** The
container backend's shared archives volume makes the *second* container build
on a Windows or macOS host fail whenever its package set differs from the
first.

---

### C10. FIXED — `verify` on a directory that is not a bundle is exit 1, and prints no report.

**New in core `5837d82`, and it changes an error path the adapter has to
handle.** This is the same correction C8 records for `snapshot inspect`,
applied to `verify`.

Before, `debark verify some-folder` on a directory that had never been near
debark printed a full `debark.verifyreport/v1` document with
`ok: false` and one problem of kind `manifest-missing`, and exited **4**. Exit
4 is `dferr.Verification` — "a signature, digest or metadata check did not
match", the code a script escalates on and an operator reads as *the media may
have been tampered with*. Nothing had been verified. They had picked the wrong
folder.

Measured on `92ebdc2`:

```
$ debark verify /tmp/notabundle --json
exit 1, stdout 0 bytes, stderr:
debark: verify: /tmp/notabundle is not a debark bundle: it has no
debark.manifest.json and none of a bundle's other parts
`debark build` writes a bundle; point verify at the directory it names, not
at the media root or a folder beside it
```

**Note the stdout: zero bytes.** `verify --json` on a non-bundle now prints no
JSON at all, matching what it already did for a path that does not exist and
for a file passed where a directory was meant. `cliadapter.Verify` must not
assume a decodable document on a non-zero exit — the `*cliadapter.Error` path
is the only path here.

**The boundary is structural, not a general softening**, and the GUI must not
flatten it. A directory that carries any of a bundle's parts —
`debark.manifest.json`, `debark.manifest.sig`, `lock.json`,
`snapshot.json`, `repo/` — is still treated as a bundle, so a **real bundle
whose manifest was deleted still exits 4 with `manifest-missing`**, which is
correct: the one file saying what the bundle should contain has been removed,
and that is what tampering looks like. `README.txt` is deliberately not one of
the markers, because every third directory on a USB stick has one.

So on the export/verify screen: exit 1 here means *"that folder is not a
bundle — pick the one `build` wrote"*, and exit 4 still means *"do not
install this"*. They must not share a message.

---

## What the CLI does not give the GUI

Each of these is a genuine gap. Fixing one means a commit **in the core
repository**, with tests, on its own — never a workaround in this repo.

**Four of them are now fixed** (2, 4, 9 and 11) and are struck through rather
than removed: the record of what was asked for and what arrived is the useful
part, and a consumer written against the old behaviour needs to find it here.
**Item 7 is half fixed** — it now has a numerator on the container backend's
*vendor* downloads and still has no denominator anywhere. **Item 10 is still
open and is now the largest gap here.** **Two are new** (12 and 13).

1. **There is no `dferr` class for cancellation.** The operator pressing Stop
   is not a failure of the work, but the taxonomy has no room to say so.
   `cliadapter.NewCanceledError` uses `dferr.Usage` (dferr's own fallback) and
   exposes `Canceled()` so the UI can tell the two apart without matching on a
   string. A dedicated class would be an ADR-012 change and is not obviously
   worth it.

2. ~~**A failing `build` writes nothing to stderr.**~~ **FIXED in `3ed80bd`**,
   by the second of the two suggestions this item made: `silentError` is gone
   and every failure prints. `BuildResult` still has no `message` or `hint`
   field, so the *document* carries no sentence — but stderr now does, and it
   names the unresolved inputs and failed URLs. See C1 for the measured
   shapes. Nothing further is needed here.

3. **`snapshot inspect --json` omits the path it read.** Every other document
   that describes a file on disk carries its path (`verify.Report.BundlePath`,
   the `inspect` envelope's `bundle_path`, `snapshot create`'s `path`). The
   adapter carries it alongside in `cliadapter.SnapshotInfo`, which is why
   that type is a wrapper rather than an alias. *Suggested core change: an
   envelope for `snapshot inspect --json` with `path` and `schema_version`,
   matching `inspect`.*

4. ~~**There is no way to force `Install-Recommends` on.**~~ **FIXED in
   `09d7821`.** `build --recommends` exists and is mutually exclusive with
   `--no-recommends`, so all three states of `Options.Recommends` are now
   reachable from a command line and the GUI *can* offer the three-state
   control this item said it could not. See §3. The consequence for
   `cliadapter.BuildSpec` is that the field carrying it must be three-valued —
   a `*bool` or an enum — because a `bool` cannot distinguish "follow the
   target" from "exclude".

5. **`snapshot create` reports a bare hex digest** where every other document
   in the system uses a `sha256:`-prefixed form in human output. Not a
   blocker; a trap for anyone comparing strings.

6. **`config show --json`'s type is under `internal/`.** The document is
   versioned and public in spirit but unreachable in fact, so any consumer must
   mirror it by hand — the exact drift the "import, never mirror" rule exists
   to prevent. *Suggested core change: move the shown-config type to
   `api/config/v1` (or `core/config`), leaving loading in `internal/cli`.*

7. **`apt.resolve`'s `selections` is not a file count.** There is no event that
   says how many files a build is about to fetch, so an honest progress bar
   cannot have a denominator until the run is over. *Suggested core change: an
   attr on `apt.resolve` for the number of selections that are not already
   store hits, or a `fetch.plan` event.* §5.3 makes this worse than it reads:
   on the local backend there is no numerator either.

8. **Nothing `keygen` writes is anywhere `verify` looks by default.** A bundle
   the GUI has just built and signed does not verify on the machine that built
   it: `verify BUNDLE` with no `--key` and no configured `verify_keys` trusts
   nothing, and reports "no signature verifies against a trusted key" for a
   signature made seconds earlier with the operator's own key. There is no
   default keyring directory — `sign.KeySource` is filled from flags, then
   `config.VerifyKeys`/`VerifyKeyringDirs`, and both default to empty
   (`internal/cli/cmd_verify.go`). The GUI closes this by passing the public
   key beside its own signing key (`cliadapter.KeyedVerifier`), which is the
   right answer for the builder and no answer at all for anyone using the CLI
   directly. *Suggested core change: have `keygen` also place the public key in
   a default operator keyring directory that `verify` and `install` consult, or
   consult `~/.config/debark/keys` unconditionally.*

   A second, smaller thing sits under it: `--key`/`--keyring` **replace** the
   configured `verify_keys` rather than adding to them
   (`firstNonEmptySlice`), so naming one key narrows the trust set to exactly
   that key. A caller who wants "my configured keys, plus this one" cannot say
   so. *Suggested core change: append rather than replace, or add `--also-key`.*

9. ~~**There is no way to give the container backend a Linux build of
   debark.**~~ **FIXED in `a0b61c2`**, and by both halves of what this item
   suggested: `--self-binary`, `DEBARK_SELF_BINARY` and a `self_binary`
   config key, *and* automatic discovery of `bin/debark-linux-<arch>` beside
   the executable. Verified working from Windows — see C9. The readiness half
   is closed too: `internal/readiness` now has a `self-binary` check, so the
   screen no longer says a Windows machine can build when it cannot.

10. **A second container build on a Windows or macOS host fails whenever it
    differs from the first.** Found while verifying C9, reproduced three
    times, and the largest remaining gap here.

    On a non-Linux host the container backend defaults `/archives` to a named
    Docker volume rather than a bind mount, for a measured I/O reason
    (`core/apt/container.go`, `storeVolumeChoice`, citing experiment E5). The
    volume name is a constant, `debark-archives-cache`, and its comment says
    sharing it across builds and targets is safe because "containerReverifyFiles
    only ever trusts the specific filenames named in the resolve envelope's
    files[], so unrelated bytes a previous, different build left behind in this
    same volume are inert clutter, not a correctness risk".

    They are not inert, because the envelope's `files[]` is not a filtered
    list. `scanArchivesDir` (`internal/cli/cmd_resolve.go`) lists **every**
    `.deb` in the directory and puts them all in the envelope, deliberately —
    it is the contract's redundant cross-check against a truncated mount. So
    leftovers from a previous build enter `files[]`, and the envelope
    consistency check then fails the whole build.

    Measured. After one Ubuntu 24.04 build and one Debian 12 build, the shared
    volume held both:

    ```
    $ docker run --rm -v debark-archives-cache:/a:ro alpine ls /a
    hello_2.10-3_amd64.deb
    jq_1.6-2.1+deb12u2_amd64.deb          libjq1_1.6-2.1+deb12u2_amd64.deb
    jq_1.7.1-3ubuntu0.24.04.2_amd64.deb   libjq1_1.7.1-3ubuntu0.24.04.2_amd64.deb
    libonig5_6.9.8-1_amd64.deb            libonig5_6.9.9-1build1_amd64.deb
    ```

    and every subsequent build failed before writing a bundle:

    ```
    $ debark build --base debian:12/minimal --backend container ... -- apt:jq ...
    exit 4
    debark: container: envelope plan/files disagree for jq: plan says
    1.6-2.1+deb12u2 (jq_1.6-2.1+deb12u2_amd64.deb), files says
    1.7.1-3ubuntu0.24.04.2 (jq_1.7.1-3ubuntu0.24.04.2_amd64.deb)

    $ debark build ... -- apt:hello ...
    exit 4
    debark: container: envelope files[] lists jq (jq:amd64) but the plan
    does not select it
    ```

    Two things make this severe for the GUI specifically. The failure is
    **exit 4, `verification`** — the class an operator reads as tampering, for
    what is actually a stale cache. And there is **no way out from a command
    line**: `ContainerOptions.StoreVolume` overrides the shared name, but no
    flag, no config key and no environment variable sets it (`DEBARK_*`
    exposes `APT`, `BACKEND`, `CONFIG`, `DPKG`, `PROFILE` and
    `SELF_BINARY`, and no more). That is the same shape as C9 before it was
    fixed: a struct field an operator cannot reach.

    *Suggested core change: scope the default volume per target — the cache
    key or the snapshot digest is already computed — or have `scanArchivesDir`
    report only the files the plan selects and keep the whole-directory scan
    as a separate, non-fatal observation. Either alone fixes it. Failing both,
    a `--store-volume` flag with a `store_volume` config key would at least
    make it reachable.*

11. ~~**`BuildResult.FetchFailed` carries URLs with no reason**, so an
    unreachable vendor URL, a digest mismatch and an untrusted certificate
    are one indistinguishable message.~~ **FIXED in `1c767f7`**, by the first
    of the two shapes this item suggested, taken additively:
    `fetch_failures[]` of `{input, reason, detail}` sits beside an unchanged
    `fetch_failed[]`. The core's reasoning for not widening the existing
    field is in `docs/formats.md` §0 and §3.6 — a published schema version is
    never mutated, and an array of string becoming an array of object is a
    meaning change that would have forced `debark.buildjob/v2` on every
    consumer for one added attribute. See §2.1 for the twelve reasons and for
    the `input` / `detail` redaction split. **Error-catalogue rows 3.3, 3.4
    and 3.5 can now be three rows.**

12. **The container backend forwards none of the inner process's events.** A
    complete container build emits **11 events** — no `fetch.file`, no
    `progress`, not even `apt.resolve` — because the real work happens in
    `/debark resolve` inside the container and its event stream never
    crosses back out. `execContainer` (`core/apt/container_driver.go`) runs
    the runtime with `cmd.Stdout` and `cmd.Stderr` bound to `bytes.Buffer`s,
    captures the exit code, and reads the plan envelope from a file. Nothing
    else returns. This is why build progress on the container backend is
    honestly indeterminate, and it is a strictly larger gap than item 7:
    item 7 is a missing denominator, this is a missing stream.

    *Suggested core change, and the route is shorter than it looks.*
    `resolveEvidenceSink` (`internal/cli/cmd_resolve.go`) already sends
    `--json-events -` to **stderr**, never stdout, because stdout is reserved
    for the plan envelope — and `execContainer` already captures the
    container's stderr. So the wiring is: add `--json-events -` to
    `containerBuildInnerArgv`, and have the driver scan stderr line by line
    instead of buffering it whole, decoding each line that parses as a
    `debark.events/v1` event and forwarding it to `b.opts.Events` while
    everything else accumulates as the diagnostic text
    `containerLastLines` and `containerClassifyRunResult` already depend on.
    No `/work` file, no stdout multiplexing, no tailing.

    *Three things make it a real piece of work rather than a one-liner.*
    `execContainerFn` is a package var with a fixed signature used by
    `container.go`, `basecontainer.go` and `container_driver.go`, and every
    test substitutes it — threading a sink through changes all of them.
    The inner process's output is **not trusted material**
    (`containerCrossCheckEnvelope` exists precisely because the envelope is
    not), so forwarded events need validating — known `type`, size and count
    caps — before they reach `evidence.json`, which the manifest hashes and
    the signature covers. And `snapshot from-base`'s container path has the
    same gap and wants the same treatment.

    **What it would and would not buy, which matters before anyone starts.**
    The inner command is `debark resolve --backend local`, so what crosses
    back is `backend.selected`, `apt.update` and `apt.resolve` — real phase
    information, and enough to replace "working…" with "updating indexes" and
    "resolving". It is **not** a progress bar. `fetch.file` and `progress`
    come from `core/fetch`, which the container path never runs for archive
    packages: apt does that downloading, inside the container, and emits
    nothing. That is §5.3's problem, it is architectural on both backends, and
    item 6 does not touch it. A plan that promises a determinate progress bar
    out of this will not get one.

13. **`build --digest` misbinds a URL that has a query string.** `fetch` was
    fixed in `92ebdc2`; `build` was not, because its flag registration is in
    a file that had unrelated uncommitted work in it. The measured shapes and
    the consequence — a silent downgrade from `user-digest` to
    `url-unverified` — are under §3. *Suggested core change: replace
    `cmd_build.go`'s `flags.StringToStringVar(&digests, "digest", ...)` with
    `registerDigestFlag(flags, &digests)`, which already exists in
    `internal/cli/digestflag.go` and writes into the same
    `map[string]string`. One line; `buildInputs` does not move.*
